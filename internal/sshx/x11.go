package sshx

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// X11 forwarding to the host's X server (PROTO-19, RESEARCH §3.13 mode 3). Desktop mode only: remote X clients show
// up on the display of the machine running Termstead (XQuartz, VcXsrv, Xorg/XWayland, WSLg). The server is given a
// random fake MIT-MAGIC-COOKIE-1; every forwarded connection's setup packet is checked against it and rewritten with
// the real cookie from `xauth list $DISPLAY` (or no authentication when the local server needs none).

const x11AuthProto = "MIT-MAGIC-COOKIE-1"

type x11Forwarder struct {
	display  string
	fake     []byte
	realName []byte
	realData []byte

	mu     sync.Mutex
	conns  map[io.Closer]struct{}
	closed bool
}

// enableX11 registers the "x11" channel handler on the connection (once).
func (c *Client) enableX11() (*x11Forwarder, error) {
	c.x11Once.Do(func() {
		display := os.Getenv("DISPLAY")
		if display == "" && runtime.GOOS == "windows" {
			display = "localhost:0.0" // VcXsrv / X410 / Xming default
		}
		if display == "" {
			c.x11Err = errors.New("no local X server: DISPLAY is not set")
			return
		}
		fake := make([]byte, 16)
		if _, err := rand.Read(fake); err != nil {
			c.x11Err = err
			return
		}
		f := &x11Forwarder{display: display, fake: fake, conns: map[io.Closer]struct{}{}}
		f.realName, f.realData = xauthCookie(display)
		chans := c.Client.HandleChannelOpen("x11")
		if chans == nil {
			c.x11Err = errors.New("X11 forwarding is already handled on this connection")
			return
		}
		go func() {
			for nc := range chans {
				go f.handle(nc)
			}
		}()
		c.x11 = f
	})
	return c.x11, c.x11Err
}

// request sends x11-req for sess (before Shell / Start).
func (f *x11Forwarder) request(sess *ssh.Session, screen int) error {
	payload := ssh.Marshal(struct {
		SingleConnection bool
		AuthProtocol     string
		AuthCookie       string
		ScreenNumber     uint32
	}{false, x11AuthProto, hex.EncodeToString(f.fake), uint32(screen)})
	ok, err := sess.SendRequest("x11-req", true, payload)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("the server refused X11 forwarding")
	}
	return nil
}

func (f *x11Forwarder) track(c io.Closer) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.conns[c] = struct{}{}
	return true
}

func (f *x11Forwarder) untrack(c io.Closer) {
	f.mu.Lock()
	delete(f.conns, c)
	f.mu.Unlock()
}

func (f *x11Forwarder) close() {
	f.mu.Lock()
	f.closed = true
	conns := f.conns
	f.conns = map[io.Closer]struct{}{}
	f.mu.Unlock()
	for c := range conns {
		c.Close()
	}
}

func (f *x11Forwarder) handle(nc ssh.NewChannel) {
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	if !f.track(ch) {
		ch.Close()
		return
	}
	defer f.untrack(ch)
	defer ch.Close()

	setup, err := f.rewriteSetup(ch)
	if err != nil {
		return
	}
	local, err := dialDisplay(f.display)
	if err != nil {
		return
	}
	if !f.track(local) {
		local.Close()
		return
	}
	defer f.untrack(local)
	defer local.Close()
	if _, err := local.Write(setup); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(local, ch); closeWrite(local); done <- struct{}{} }()
	go func() { _, _ = io.Copy(ch, local); _ = ch.CloseWrite(); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// rewriteSetup reads the X11 connection setup packet from the remote client, verifies the fake cookie and returns the
// packet re-encoded with the real credentials.
func (f *x11Forwarder) rewriteSetup(r io.Reader) ([]byte, error) {
	hdr := make([]byte, 12)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	var bo binary.ByteOrder
	switch hdr[0] {
	case 'B':
		bo = binary.BigEndian
	case 'l':
		bo = binary.LittleEndian
	default:
		return nil, errors.New("x11: invalid byte order")
	}
	nameLen, dataLen := int(bo.Uint16(hdr[6:8])), int(bo.Uint16(hdr[8:10]))
	body := make([]byte, nameLen+pad4(nameLen)+dataLen+pad4(dataLen))
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	name := body[:nameLen]
	data := body[nameLen+pad4(nameLen) : nameLen+pad4(nameLen)+dataLen]
	if string(name) != x11AuthProto || subtle.ConstantTimeCompare(data, f.fake) != 1 {
		return nil, errors.New("x11: client presented a wrong authentication cookie")
	}
	out := make([]byte, 12, 12+len(f.realName)+pad4(len(f.realName))+len(f.realData)+pad4(len(f.realData)))
	copy(out, hdr)
	bo.PutUint16(out[6:8], uint16(len(f.realName)))
	bo.PutUint16(out[8:10], uint16(len(f.realData)))
	out = append(out, f.realName...)
	out = append(out, make([]byte, pad4(len(f.realName)))...)
	out = append(out, f.realData...)
	out = append(out, make([]byte, pad4(len(f.realData)))...)
	return out, nil
}

func pad4(n int) int { return (4 - n%4) % 4 }

// parseDisplay splits DISPLAY into (host or socket path, display number).
func parseDisplay(display string) (host string, num int, err error) {
	i := strings.LastIndex(display, ":")
	if i < 0 {
		return "", 0, fmt.Errorf("invalid DISPLAY %q", display)
	}
	host, rest := display[:i], display[i+1:]
	if dot := strings.IndexByte(rest, '.'); dot >= 0 {
		rest = rest[:dot]
	}
	num, err = strconv.Atoi(rest)
	if err != nil || num < 0 || num > 1000 {
		return "", 0, fmt.Errorf("invalid DISPLAY %q", display)
	}
	return host, num, nil
}

// dialDisplay connects to the local X server named by DISPLAY.
func dialDisplay(display string) (net.Conn, error) {
	host, num, err := parseDisplay(display)
	if err != nil {
		return nil, err
	}
	timeout := 5 * time.Second
	switch {
	case strings.HasPrefix(host, "/"):
		// XQuartz: DISPLAY=/private/tmp/com.apple.launchd.XXXX/org.xquartz:0 is the socket path itself.
		return net.DialTimeout("unix", host+":"+strconv.Itoa(num), timeout)
	case host == "" || host == "unix":
		if runtime.GOOS != "windows" {
			if c, err := net.DialTimeout("unix", fmt.Sprintf("/tmp/.X11-unix/X%d", num), timeout); err == nil {
				return c, nil
			}
		}
		host = "127.0.0.1"
	}
	return net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(6000+num)), timeout)
}

// xauthCookie returns the MIT-MAGIC-COOKIE-1 of display from `xauth list` (empty when unavailable, i.e. the local
// server accepts unauthenticated local connections).
func xauthCookie(display string) (name, data []byte) {
	path, err := exec.LookPath("xauth")
	if err != nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "list", display).Output()
	if err != nil {
		return nil, nil
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 3 && f[1] == x11AuthProto {
			if cookie, err := hex.DecodeString(f[2]); err == nil {
				return []byte(x11AuthProto), cookie
			}
		}
	}
	return nil, nil
}
