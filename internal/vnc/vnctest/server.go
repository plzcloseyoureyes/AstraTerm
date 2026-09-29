// Package vnctest provides a scriptable fake VNC (RFB) server for tests of the vnc module: protocol versions 3.3 /
// 3.7 / 3.8, the UltraVNC repeater greeting, security types None, VNC Authentication, Apple Remote Desktop, VeNCrypt
// (Plain, VncAuth/None subtypes, X509* over crypto/tls, anonymous TLS subtypes over anontlstest when AnonTLS is set,
// else answered with a TLS handshake_failure alert) and arbitrary other types (pass-through), then ServerInit and a
// message handler.
package vnctest

import (
	"bytes"
	"crypto/aes"
	"crypto/des"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/nexterm/nexterm/internal/vnc/anontls/anontlstest"
)

// Security types used by the fake server.
const (
	SecNone     = 1
	SecVNCAuth  = 2
	SecTight    = 16
	SecVeNCrypt = 19
	SecARD      = 30

	VcPlain     = 256
	VcTLSNone   = 257
	VcTLSVnc    = 258
	VcTLSPlain  = 259
	VcX509None  = 260
	VcX509Vnc   = 261
	VcX509Plain = 262
	VcTLSSASL   = 263
)

// Server is a fake VNC server. Zero values give RFB 3.8, security None, a 1024x768 desktop named "fake".
type Server struct {
	Version    string // e.g. "RFB 003.008\n" (default), "RFB 003.003\n", "RFB 003.889\n"
	RepeaterID string // non-empty: greet with "RFB 000.000\n" and expect "ID:<RepeaterID>" first

	Types  []byte // security types offered (RFB 3.7+); default [None]
	Type33 uint32 // RFB 3.3: the type the server decides (default None)

	Password    string // VNC Authentication / VeNCrypt Vnc subtypes
	Username    string // Plain / ARD
	ARDPassword string // ARD password (default Password)
	ARDKeyLen   int    // ARD Diffie-Hellman key length in bytes (default 128)

	VeNCryptSubtypes []uint32
	TLS              *tls.Config         // certificate for X509* subtypes
	AnonTLS          *anontlstest.Config // anonymous TLS subtypes (nil: refuse the TLS handshake)

	FailReason string // SecurityResult failure reason sent on bad credentials (3.8)
	RefuseWith string // non-empty: offer no security types, send this reason

	Width, Height int
	Name          string

	// Handle runs after ServerInit (the RFB stream, possibly inside TLS). Nil: echo client bytes back.
	Handle func(c net.Conn)

	mu       sync.Mutex
	Accepted int      // connections served
	Chosen   []uint32 // security type chosen by the client, per connection
	Subtypes []uint32 // VeNCrypt subtype chosen, per connection
	Shared   []byte   // ClientInit flag, per connection
	AuthOK   int      // successful authentications
	AuthFail int
	Errors   []error
	TLSInfo  []*anontlstest.Negotiated // anonymous TLS handshakes
}

// Listen starts the server on 127.0.0.1 and returns its address; it stops when the returned closer is called.
func (s *Server) Listen() (addr string, stop func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	var wg sync.WaitGroup
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.Serve(c)
			}()
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }, nil
}

func (s *Server) record(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
}

// Serve runs one RFB connection (closing c at the end).
func (s *Server) Serve(c net.Conn) {
	defer c.Close()
	s.record(func() { s.Accepted++ })
	if err := s.serve(c); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		s.record(func() { s.Errors = append(s.Errors, err) })
	}
}

// Snapshot returns copies of the recorded fields.
func (s *Server) Snapshot() (accepted int, chosen, subtypes []uint32, shared []byte, authOK, authFail int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Accepted, slices.Clone(s.Chosen), slices.Clone(s.Subtypes), slices.Clone(s.Shared), s.AuthOK, s.AuthFail
}

func (s *Server) serve(c net.Conn) error {
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	if s.RepeaterID != "" {
		if _, err := c.Write([]byte("RFB 000.000\n")); err != nil {
			return err
		}
		buf := make([]byte, 250)
		if _, err := io.ReadFull(c, buf); err != nil {
			return err
		}
		id := string(bytes.TrimRight(buf, "\x00"))
		if id != "ID:"+s.RepeaterID {
			return fmt.Errorf("repeater: unexpected id %q", id)
		}
	}
	version := s.Version
	if version == "" {
		version = "RFB 003.008\n"
	}
	if _, err := c.Write([]byte(version)); err != nil {
		return err
	}
	cv := make([]byte, 12)
	if _, err := io.ReadFull(c, cv); err != nil {
		return err
	}
	minor := 8
	switch string(cv) {
	case "RFB 003.003\n":
		minor = 3
	case "RFB 003.007\n":
		minor = 7
	case "RFB 003.008\n":
	default:
		return fmt.Errorf("unexpected client version %q", cv)
	}

	var rw net.Conn = c
	var chosen uint32
	if minor == 3 {
		chosen = s.Type33
		if chosen == 0 {
			chosen = SecNone
		}
		if _, err := c.Write(be32(chosen)); err != nil {
			return err
		}
	} else {
		if s.RefuseWith != "" {
			msg := append([]byte{0}, be32(uint32(len(s.RefuseWith)))...)
			_, err := c.Write(append(msg, s.RefuseWith...))
			return err
		}
		types := s.Types
		if len(types) == 0 {
			types = []byte{SecNone}
		}
		if _, err := c.Write(append([]byte{byte(len(types))}, types...)); err != nil {
			return err
		}
		b := make([]byte, 1)
		if _, err := io.ReadFull(c, b); err != nil {
			return err
		}
		if !slices.Contains(types, b[0]) {
			return fmt.Errorf("client chose unoffered type %d", b[0])
		}
		chosen = uint32(b[0])
	}
	s.record(func() { s.Chosen = append(s.Chosen, chosen) })

	var ok bool
	var err error
	sendResult := true
	switch chosen {
	case SecNone:
		ok, sendResult = true, minor >= 8
	case SecVNCAuth:
		ok, err = vncAuth(rw, s.Password)
	case SecARD:
		ok, err = s.ard(rw)
	case SecVeNCrypt:
		rw, ok, err = s.veNCrypt(c)
	default:
		return fmt.Errorf("fake server cannot serve type %d", chosen)
	}
	if err != nil {
		return err
	}
	if sendResult {
		if ok {
			s.record(func() { s.AuthOK++ })
			if _, err := rw.Write(be32(0)); err != nil {
				return err
			}
		} else {
			s.record(func() { s.AuthFail++ })
			msg := be32(1)
			if minor >= 8 {
				reason := s.FailReason
				if reason == "" {
					reason = "Authentication failure"
				}
				msg = append(append(msg, be32(uint32(len(reason)))...), reason...)
			}
			_, _ = rw.Write(msg)
			return nil
		}
	}
	shared := make([]byte, 1)
	if _, err := io.ReadFull(rw, shared); err != nil {
		return err
	}
	s.record(func() { s.Shared = append(s.Shared, shared[0]) })
	w, h := s.Width, s.Height
	if w == 0 {
		w, h = 1024, 768
	}
	name := s.Name
	if name == "" {
		name = "fake"
	}
	init := make([]byte, 24)
	binary.BigEndian.PutUint16(init[0:], uint16(w))
	binary.BigEndian.PutUint16(init[2:], uint16(h))
	copy(init[4:], []byte{32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0})
	binary.BigEndian.PutUint32(init[20:], uint32(len(name)))
	if _, err := rw.Write(append(init, name...)); err != nil {
		return err
	}
	_ = c.SetDeadline(time.Time{})
	if s.Handle != nil {
		s.Handle(rw)
		return nil
	}
	_, err = io.Copy(rw, rw)
	return err
}

func vncAuth(rw io.ReadWriter, password string) (bool, error) {
	challenge := make([]byte, 16)
	_, _ = rand.Read(challenge)
	if _, err := rw.Write(challenge); err != nil {
		return false, err
	}
	resp := make([]byte, 16)
	if _, err := io.ReadFull(rw, resp); err != nil {
		return false, err
	}
	return bytes.Equal(resp, DESResponse(password, challenge)), nil
}

// DESResponse is the VNC Authentication response (independent implementation for tests).
func DESResponse(password string, challenge []byte) []byte {
	var key [8]byte
	copy(key[:], password)
	for i, b := range key {
		var r byte
		for j := 0; j < 8; j++ {
			if b&(1<<j) != 0 {
				r |= 1 << (7 - j)
			}
		}
		key[i] = r
	}
	block, _ := des.NewCipher(key[:])
	out := make([]byte, 16)
	block.Encrypt(out[:8], challenge[:8])
	block.Encrypt(out[8:], challenge[8:])
	return out
}

// ard serves Apple Remote Desktop authentication (macOS uses 128-byte keys; this uses the RFC 5114 1024-bit prime).
func (s *Server) ard(rw io.ReadWriter) (bool, error) {
	p := anontlstest.RFC5114P1024
	g := big.NewInt(2)
	keyLen := 128
	if s.ARDKeyLen != 0 {
		keyLen = s.ARDKeyLen
		p = new(big.Int).SetBytes(bytes.Repeat([]byte{0xff}, keyLen)) // odd, not prime: only for rejection tests
	}
	priv, _ := rand.Int(rand.Reader, new(big.Int).Sub(p, big.NewInt(3)))
	priv.Add(priv, big.NewInt(2))
	pub := new(big.Int).Exp(g, priv, p)
	msg := []byte{0, 2, byte(keyLen >> 8), byte(keyLen)}
	msg = append(msg, pad(p.Bytes(), keyLen)...)
	msg = append(msg, pad(pub.Bytes(), keyLen)...)
	if _, err := rw.Write(msg); err != nil {
		return false, err
	}
	resp := make([]byte, 128+keyLen)
	if _, err := io.ReadFull(rw, resp); err != nil {
		return false, err
	}
	clientPub := new(big.Int).SetBytes(resp[128:])
	shared := new(big.Int).Exp(clientPub, priv, p)
	key := md5.Sum(pad(shared.Bytes(), keyLen))
	block, _ := aes.NewCipher(key[:])
	plain := make([]byte, 128)
	for i := 0; i < 128; i += 16 {
		block.Decrypt(plain[i:i+16], resp[i:i+16])
	}
	user := string(plain[:bytes.IndexByte(plain[:64], 0)])
	pass := string(plain[64 : 64+bytes.IndexByte(plain[64:], 0)])
	want := s.ARDPassword
	if want == "" {
		want = s.Password
	}
	return user == s.Username && pass == want, nil
}

func (s *Server) veNCrypt(c net.Conn) (net.Conn, bool, error) {
	if _, err := c.Write([]byte{0, 2}); err != nil {
		return nil, false, err
	}
	v := make([]byte, 2)
	if _, err := io.ReadFull(c, v); err != nil {
		return nil, false, err
	}
	if v[0] != 0 || v[1] != 2 {
		_, _ = c.Write([]byte{1})
		return nil, false, fmt.Errorf("client VeNCrypt version %d.%d", v[0], v[1])
	}
	if _, err := c.Write([]byte{0}); err != nil {
		return nil, false, err
	}
	subs := s.VeNCryptSubtypes
	msg := []byte{byte(len(subs))}
	for _, t := range subs {
		msg = append(msg, be32(t)...)
	}
	if _, err := c.Write(msg); err != nil {
		return nil, false, err
	}
	b := make([]byte, 4)
	if _, err := io.ReadFull(c, b); err != nil {
		return nil, false, err
	}
	sub := binary.BigEndian.Uint32(b)
	if !slices.Contains(subs, sub) {
		return nil, false, fmt.Errorf("client chose unoffered subtype %d", sub)
	}
	s.record(func() { s.Subtypes = append(s.Subtypes, sub) })
	var rw net.Conn = c
	switch sub {
	case VcTLSNone, VcTLSVnc, VcTLSPlain:
		if _, err := c.Write([]byte{1}); err != nil {
			return nil, false, err
		}
		if s.AnonTLS != nil {
			tc, neg, err := anontlstest.Serve(c, s.AnonTLS)
			s.record(func() { s.TLSInfo = append(s.TLSInfo, neg) })
			if err != nil {
				return nil, false, err
			}
			rw = tc
			break
		}
		// Without AnonTLS: fail the handshake like a server whose priorities share nothing with the client (fatal
		// handshake_failure alert).
		hdr := make([]byte, 5)
		if _, err := io.ReadFull(c, hdr); err != nil {
			return nil, false, err
		}
		n := int(binary.BigEndian.Uint16(hdr[3:]))
		if _, err := io.CopyN(io.Discard, c, int64(n)); err != nil {
			return nil, false, err
		}
		_, _ = c.Write([]byte{21, 3, 3, 0, 2, 2, 40})
		return nil, false, errors.New("anonymous TLS rejected (as scripted)")
	case VcX509None, VcX509Vnc, VcX509Plain:
		if s.TLS == nil {
			return nil, false, errors.New("no TLS config")
		}
		if _, err := c.Write([]byte{1}); err != nil {
			return nil, false, err
		}
		tc := tls.Server(c, s.TLS)
		if err := tc.Handshake(); err != nil {
			return nil, false, err
		}
		rw = tc
	}
	switch sub {
	case VcX509None, SecNone, VcTLSNone:
		return rw, true, nil
	case VcX509Vnc, VcTLSVnc, SecVNCAuth:
		ok, err := vncAuth(rw, s.Password)
		return rw, ok, err
	case VcX509Plain, VcTLSPlain, VcPlain:
		hdr := make([]byte, 8)
		if _, err := io.ReadFull(rw, hdr); err != nil {
			return nil, false, err
		}
		ul, pl := binary.BigEndian.Uint32(hdr), binary.BigEndian.Uint32(hdr[4:])
		if ul > 4096 || pl > 4096 {
			return nil, false, errors.New("plain credentials too long")
		}
		cred := make([]byte, ul+pl)
		if _, err := io.ReadFull(rw, cred); err != nil {
			return nil, false, err
		}
		return rw, string(cred[:ul]) == s.Username && string(cred[ul:]) == s.Password, nil
	}
	return nil, false, fmt.Errorf("unsupported subtype %d", sub)
}

func be32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func pad(b []byte, n int) []byte {
	if len(b) >= n {
		return b
	}
	out := make([]byte, n)
	copy(out[n-len(b):], b)
	return out
}

// Viewer drives the browser side of the RFB handshake the way noVNC does (RFB 3.8, choosing the first offered type
// when it is None) and returns the ServerInit bytes. rw is the WebSocket byte stream.
func Viewer(rw io.ReadWriter) (serverInit []byte, err error) {
	v := make([]byte, 12)
	if _, err := io.ReadFull(rw, v); err != nil {
		return nil, err
	}
	if string(v) != "RFB 003.008\n" {
		return nil, fmt.Errorf("unexpected version %q", v)
	}
	if _, err := rw.Write(v); err != nil {
		return nil, err
	}
	n := make([]byte, 1)
	if _, err := io.ReadFull(rw, n); err != nil {
		return nil, err
	}
	types := make([]byte, n[0])
	if _, err := io.ReadFull(rw, types); err != nil {
		return nil, err
	}
	if len(types) != 1 || types[0] != SecNone {
		return nil, fmt.Errorf("unexpected security types %v", types)
	}
	if _, err := rw.Write([]byte{SecNone}); err != nil {
		return nil, err
	}
	res := make([]byte, 4)
	if _, err := io.ReadFull(rw, res); err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint32(res) != 0 {
		return nil, errors.New("security result failed")
	}
	if _, err := rw.Write([]byte{1}); err != nil {
		return nil, err
	}
	hdr := make([]byte, 24)
	if _, err := io.ReadFull(rw, hdr); err != nil {
		return nil, err
	}
	name := make([]byte, binary.BigEndian.Uint32(hdr[20:]))
	if _, err := io.ReadFull(rw, name); err != nil {
		return nil, err
	}
	return append(hdr, name...), nil
}
