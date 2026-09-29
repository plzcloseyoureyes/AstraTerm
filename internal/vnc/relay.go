package vnc

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
)

// ---- browser side of the RFB handshake ----------------------------------------------------------------------------

// presentNone performs the server side of the RFB handshake with the browser after NexTerm authenticated upstream:
// RFB 3.8, security type None, SecurityResult OK, then (after the viewer's ClientInit, whose shared flag NexTerm has
// already applied upstream) the VNC server's ServerInit. Older client versions are handled per the specification.
func presentNone(r io.Reader, w io.Writer, serverInit []byte) error {
	if _, err := w.Write(rfb38.line()); err != nil {
		return err
	}
	cv, err := readClientVersion(r)
	if err != nil {
		return err
	}
	switch cv {
	case rfb33:
		if _, err := w.Write(u32(secNone)); err != nil { // the server decides in 3.3
			return err
		}
	default:
		if _, err := w.Write([]byte{1, secNone}); err != nil {
			return err
		}
		choice, err := readU8(r)
		if err != nil {
			return err
		}
		if choice != secNone {
			return fmt.Errorf("the viewer chose security type %d", choice)
		}
		if cv >= rfb38 {
			if _, err := w.Write(u32(0)); err != nil { // SecurityResult OK
				return err
			}
		}
	}
	if _, err := readU8(r); err != nil { // ClientInit
		return err
	}
	_, err = w.Write(serverInit)
	return err
}

// presentPassthrough replays the server's version (as negotiated by NexTerm) and security types to the browser,
// which then authenticates itself; everything after flows through the relay unchanged.
func presentPassthrough(r io.Reader, w io.Writer, p *passthrough) error {
	if _, err := w.Write(p.version.line()); err != nil {
		return err
	}
	b, err := readFull(r, versionLen)
	if err != nil {
		return err
	}
	if !bytes.Equal(b, p.version.line()) {
		return fmt.Errorf("the viewer answered %q to RFB %s", printable(b), p.version)
	}
	_, err = w.Write(p.security)
	return err
}

func readClientVersion(r io.Reader) (rfbVersion, error) {
	b, err := readFull(r, versionLen)
	if err != nil {
		return 0, err
	}
	v, repeater, err := parseServerVersion(b) // same syntax
	if err != nil || repeater {
		return 0, fmt.Errorf("invalid viewer version %q", printable(b))
	}
	return v, nil
}

// ---- relay --------------------------------------------------------------------------------------------------------

type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// relay copies both directions until one ends; who is "server" when the VNC server side ended first, "browser"
// otherwise. Read-only viewers' input events are filtered out.
func (v *viewer) relay(browser io.Reader, nc net.Conn, up net.Conn) (who string, err error) {
	type result struct {
		who string
		err error
	}
	done := make(chan result, 2)
	go func() {
		buf := make([]byte, relayBuffer)
		_, err := io.CopyBuffer(countingWriter{w: nc, n: &v.bytesDown}, up, buf)
		done <- result{"server", err}
	}()
	go func() {
		var err error
		dst := countingWriter{w: up, n: &v.bytesUp}
		if f := (clientFilter{dropInput: v.readOnly, dropClipboard: !v.clip.toRemote}); f.active() {
			err = forwardClientMessages(bufio.NewReaderSize(browser, 32<<10), dst, f)
		} else {
			_, err = io.Copy(dst, browser)
		}
		done <- result{"browser", err}
	}()
	first := <-done
	if first.who == "browser" && first.err == nil {
		first.err = io.EOF
	}
	// Unblock the other direction: closing the upstream ends its reads; the viewer's socket is closed by the caller
	// (or already gone).
	_ = up.Close()
	if first.who == "server" {
		v.finishSocketAfterServer(first.err)
	} else {
		v.cancel()
	}
	<-done
	return first.who, first.err
}

// finishSocketAfterServer records the close code for a server-side end before the socket is closed.
func (v *viewer) finishSocketAfterServer(err error) {
	if err == nil || errors.Is(err, io.EOF) {
		v.setClose(CloseServerEnded, "The VNC server closed the connection")
	} else if !isClosedErr(err) {
		v.setClose(CloseConnectFailed, "Connection to the VNC server lost: "+friendlyNetError(err).Error())
	}
	v.finishSocket()
}

// maxCutText bounds a single ClientCutText message (the browser's clipboard) in filtered mode.
const maxCutText = 64 << 20

// pseudoEncodingExtendedClipboard lets a server request and receive the viewer's clipboard (as ClientCutText).
const pseudoEncodingExtendedClipboard = int32(-1063131698) // 0xC0A1E5CE

// clientFilter selects what forwardClientMessages drops.
type clientFilter struct {
	dropInput     bool // read-only viewer: key and pointer events, clipboard, desktop resize, power (XVP), QEMU keys
	dropClipboard bool // clipboard policy without local→remote: ClientCutText and the Extended Clipboard encoding
}

func (f clientFilter) active() bool { return f.dropInput || f.dropClipboard }

// forwardClientMessages parses the client-to-server RFB message stream (the messages noVNC 1.7 sends) and forwards
// it, dropping what the filter excludes. Unknown message types end the stream (their length cannot be known).
func forwardClientMessages(r *bufio.Reader, w io.Writer, f clientFilter) error {
	for {
		t, err := r.ReadByte()
		if err != nil {
			return err
		}
		var hdr []byte // the rest of the fixed part after the type byte
		var tail int64 // variable part
		drop := false
		switch t {
		case 0: // SetPixelFormat
			hdr, err = readFull(r, 19)
		case 2: // SetEncodings: pad, u16 count, count × s32
			if hdr, err = readFull(r, 3); err == nil {
				count := int(binary.BigEndian.Uint16(hdr[1:3]))
				if f.dropClipboard {
					var list []byte
					if list, err = readFull(r, 4*count); err == nil {
						hdr = append(hdr, stripEncoding(list, pseudoEncodingExtendedClipboard)...)
						binary.BigEndian.PutUint16(hdr[1:3], uint16((len(hdr)-3)/4))
					}
				} else {
					tail = 4 * int64(count)
				}
			}
		case 3: // FramebufferUpdateRequest
			hdr, err = readFull(r, 9)
		case 4: // KeyEvent
			hdr, err = readFull(r, 7)
			drop = f.dropInput
		case 5: // PointerEvent (ExtendedMouseButtons: marker bit 7 of the mask → one more byte)
			if hdr, err = readFull(r, 5); err == nil && hdr[0]&0x80 != 0 {
				tail = 1
			}
			drop = f.dropInput
		case 6: // ClientCutText: 3 pad, s32 length (negative: extended clipboard, |length| bytes)
			if hdr, err = readFull(r, 7); err == nil {
				n := int64(int32(binary.BigEndian.Uint32(hdr[3:7])))
				if n < 0 {
					n = -n
				}
				if n > maxCutText {
					return fmt.Errorf("clipboard message too large (%d bytes)", n)
				}
				tail = n
			}
			drop = f.dropInput || f.dropClipboard
		case 150: // EnableContinuousUpdates
			hdr, err = readFull(r, 9)
		case 248: // ClientFence: 3 pad, u32 flags, u8 length, payload
			if hdr, err = readFull(r, 8); err == nil {
				tail = int64(hdr[7])
			}
		case 250: // XVP (power control)
			hdr, err = readFull(r, 3)
			drop = f.dropInput
		case 251: // SetDesktopSize: pad, u16 w, u16 h, u8 screens, pad, screens × 16
			if hdr, err = readFull(r, 7); err == nil {
				tail = 16 * int64(hdr[5])
			}
			drop = f.dropInput
		case 255: // QEMU client message
			sub, err2 := r.ReadByte()
			if err2 != nil {
				return err2
			}
			switch sub {
			case 0: // extended key event: u16 down, u32 keysym, u32 keycode
				rest, err3 := readFull(r, 10)
				hdr, err, drop = append([]byte{sub}, rest...), err3, f.dropInput
			case 1: // audio: u16 operation (2 = set format: u8, u8, u32)
				op, err3 := readFull(r, 2)
				hdr, err = append([]byte{sub}, op...), err3
				if err == nil && binary.BigEndian.Uint16(op) == 2 {
					tail = 6
				}
			default:
				return fmt.Errorf("unsupported QEMU client message %d", sub)
			}
		default:
			return fmt.Errorf("unsupported client message type %d", t)
		}
		if err != nil {
			return err
		}
		if drop {
			if tail > 0 {
				if _, err := io.CopyN(io.Discard, r, tail); err != nil {
					return err
				}
			}
			continue
		}
		msg := make([]byte, 0, 1+len(hdr))
		msg = append(append(msg, t), hdr...)
		if tail > 0 && tail <= 64<<10 {
			rest, err := readFull(r, int(tail))
			if err != nil {
				return err
			}
			msg = append(msg, rest...)
			tail = 0
		}
		if _, err := w.Write(msg); err != nil {
			return err
		}
		if tail > 0 {
			if _, err := io.CopyN(w, r, tail); err != nil {
				return err
			}
		}
	}
}

// stripEncoding removes an encoding from a SetEncodings list (big-endian s32 values).
func stripEncoding(list []byte, enc int32) []byte {
	out := list[:0:0]
	for i := 0; i+4 <= len(list); i += 4 {
		if int32(binary.BigEndian.Uint32(list[i:])) != enc {
			out = append(out, list[i:i+4]...)
		}
	}
	return out
}
