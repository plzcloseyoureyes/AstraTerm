package vnc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// RFB protocol constants (RFC 6143 and the IANA "RFB Security Types" registry / rfbproto).

// Security types (u8 on the wire in RFB 3.7+, u32 in RFB 3.3).
const (
	secInvalid   = 0
	secNone      = 1
	secVNCAuth   = 2
	secRA2       = 5
	secRA2ne     = 6
	secTight     = 16
	secUltra     = 17
	secTLS       = 18
	secVeNCrypt  = 19
	secSASL      = 20
	secXVP       = 22
	secARD       = 30
	secMSLogonII = 113
)

// VeNCrypt (version 0.2) subtypes (u32).
const (
	vcPlain     = 256
	vcTLSNone   = 257
	vcTLSVnc    = 258
	vcTLSPlain  = 259
	vcX509None  = 260
	vcX509Vnc   = 261
	vcX509Plain = 262
	vcTLSSASL   = 263
	vcX509SASL  = 264
)

// Limits applied to untrusted server input.
const (
	maxReasonLen   = 4 << 10  // SecurityResult / connection-failed reason strings
	maxDesktopName = 64 << 10 // ServerInit name
	versionLen     = 12
)

// rfbVersion is a negotiated protocol version (3.3, 3.7 or 3.8).
type rfbVersion int

const (
	rfb33 rfbVersion = 33
	rfb37 rfbVersion = 37
	rfb38 rfbVersion = 38
)

func (v rfbVersion) String() string {
	switch v {
	case rfb33:
		return "3.3"
	case rfb37:
		return "3.7"
	}
	return "3.8"
}

// line returns the 12-byte ProtocolVersion message for v.
func (v rfbVersion) line() []byte {
	switch v {
	case rfb33:
		return []byte("RFB 003.003\n")
	case rfb37:
		return []byte("RFB 003.007\n")
	}
	return []byte("RFB 003.008\n")
}

// errRepeater reports a VNC repeater greeting ("RFB 000.000") when no repeater ID is configured.
var errRepeater = errors.New("the server is a VNC repeater (RFB 000.000): set a repeater ID for this connection")

// parseServerVersion validates a server ProtocolVersion message and maps it to the version a client speaks, exactly
// like noVNC does (so pass-through mode presents a version noVNC accepts): 3.3 for 003.003-003.006 (UltraVNC uses
// 003.006, some servers 003.005), 3.7, and 3.8 for 003.008 and every later version (Apple 003.889, RealVNC 004.x and
// 005.x, Intel AMT 004.000). It reports repeater=true for the UltraVNC repeater greeting "RFB 000.000".
func parseServerVersion(b []byte) (v rfbVersion, repeater bool, err error) {
	if len(b) != versionLen || !strings.HasPrefix(string(b), "RFB ") || b[7] != '.' || b[11] != '\n' {
		return 0, false, fmt.Errorf("not a VNC server (unexpected greeting %q)", printable(b))
	}
	major, err1 := strconv.Atoi(string(b[4:7]))
	minor, err2 := strconv.Atoi(string(b[8:11]))
	if err1 != nil || err2 != nil {
		return 0, false, fmt.Errorf("not a VNC server (unexpected greeting %q)", printable(b))
	}
	switch {
	case major == 0 && minor == 0:
		return 0, true, nil
	case major < 3:
		return 0, false, fmt.Errorf("unsupported RFB version %d.%d", major, minor)
	case major == 3 && minor < 7:
		return rfb33, false, nil
	case major == 3 && minor == 7:
		return rfb37, false, nil
	}
	return rfb38, false, nil
}

// printable renders untrusted bytes for error messages.
func printable(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			sb.WriteByte(c)
		} else {
			fmt.Fprintf(&sb, "\\x%02x", c)
		}
	}
	return sb.String()
}

// cleanReason sanitizes a server-provided reason string (control characters stripped, bounded length).
func cleanReason(b []byte) string {
	s := strings.ToValidUTF8(string(b), "?")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = strings.ToValidUTF8(s[:300], "") + "…"
	}
	return s
}

func readFull(r io.Reader, n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

func readU8(r io.Reader) (byte, error) {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

func readU16(r io.Reader) (uint16, error) {
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b[:]), nil
}

func readU32(r io.Reader) (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b[:]), nil
}

// readReason reads a u32-length-prefixed reason string (bounded).
func readReason(r io.Reader) (string, error) {
	n, err := readU32(r)
	if err != nil {
		return "", err
	}
	if n > maxReasonLen {
		return "", fmt.Errorf("server sent an oversized reason (%d bytes)", n)
	}
	b, err := readFull(r, int(n))
	if err != nil {
		return "", err
	}
	return cleanReason(b), nil
}

func u32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

// securityTypeName returns a human-readable name of a security type or VeNCrypt subtype.
func securityTypeName(t uint32) string {
	switch t {
	case secNone:
		return "None"
	case secVNCAuth:
		return "VNC Authentication"
	case secRA2:
		return "RA2"
	case secRA2ne:
		return "RA2ne"
	case secTight:
		return "Tight"
	case secUltra:
		return "Ultra"
	case secTLS:
		return "TLS"
	case secVeNCrypt:
		return "VeNCrypt"
	case secSASL:
		return "SASL"
	case secXVP:
		return "XVP"
	case secARD:
		return "Apple Remote Desktop"
	case secMSLogonII:
		return "MS-Logon II"
	case vcPlain:
		return "Plain"
	case vcTLSNone:
		return "TLSNone"
	case vcTLSVnc:
		return "TLSVnc"
	case vcTLSPlain:
		return "TLSPlain"
	case vcX509None:
		return "X509None"
	case vcX509Vnc:
		return "X509Vnc"
	case vcX509Plain:
		return "X509Plain"
	case vcTLSSASL:
		return "TLSSASL"
	case vcX509SASL:
		return "X509SASL"
	}
	return "type " + strconv.FormatUint(uint64(t), 10)
}

func typeList(types []byte) string {
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = securityTypeName(uint32(t))
	}
	return strings.Join(names, ", ")
}

// serverInit is the parsed ServerInit message (the raw bytes are replayed to the browser).
type serverInit struct {
	raw           []byte
	width, height int
	name          string
}

// readServerInit reads a ServerInit message: framebuffer size, pixel format, desktop name.
func readServerInit(r io.Reader) (*serverInit, error) {
	hdr, err := readFull(r, 24)
	if err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[20:24])
	if n > maxDesktopName {
		return nil, fmt.Errorf("server sent an oversized desktop name (%d bytes)", n)
	}
	name, err := readFull(r, int(n))
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 0, 24+len(name))
	raw = append(append(raw, hdr...), name...)
	return &serverInit{
		raw:    raw,
		width:  int(binary.BigEndian.Uint16(hdr[0:2])),
		height: int(binary.BigEndian.Uint16(hdr[2:4])),
		name:   cleanReason(name),
	}, nil
}
