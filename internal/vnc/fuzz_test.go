package vnc

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/vnc/anontls/anontlstest"
	"github.com/plzcloseyoureyes/astraterm/internal/vnc/vnctest"
)

// Fuzzing of the parsers that consume data from the VNC server or the browser:
//
//	go test -fuzz=FuzzRFBHandshake ./internal/vnc        server → AstraTerm: version, security types, VeNCrypt,
//	                                                      ARD parameters, reasons, ServerInit (TLS: see anontls)
//	go test -fuzz=FuzzForwardClientMessages ./internal/vnc   browser → server filter (read-only / clipboard policy)
//
// The seeds are recorded from the fake server (vnctest) at test time.

// replayConn is a net.Conn that reads a fixed stream and discards writes.
type replayConn struct{ r io.Reader }

func (c *replayConn) Read(b []byte) (int, error)       { return c.r.Read(b) }
func (c *replayConn) Write(b []byte) (int, error)      { return len(b), nil }
func (c *replayConn) Close() error                     { return nil }
func (c *replayConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *replayConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *replayConn) SetDeadline(time.Time) error      { return nil }
func (c *replayConn) SetReadDeadline(time.Time) error  { return nil }
func (c *replayConn) SetWriteDeadline(time.Time) error { return nil }

type recordReads struct {
	net.Conn
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *recordReads) Read(b []byte) (int, error) {
	n, err := r.Conn.Read(b)
	r.mu.Lock()
	r.buf.Write(b[:n])
	r.mu.Unlock()
	return n, err
}

// recordServer runs a handshake against srv and returns what the server sent.
func recordServer(tb testing.TB, srv *vnctest.Server, cfg *handshakeConfig) []byte {
	tb.Helper()
	addr, stop, err := srv.Listen()
	if err != nil {
		tb.Fatal(err)
	}
	defer stop()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		tb.Fatal(err)
	}
	defer c.Close()
	rec := &recordReads{Conn: c}
	cfg.host, cfg.timeout = "127.0.0.1", 5*time.Second
	if cfg.exclude == nil {
		cfg.exclude = map[uint32]bool{}
	}
	if cfg.trust == nil {
		cfg.trust = &testTrust{}
	}
	res, err := clientHandshake(context.Background(), rec, cfg)
	if err == nil && res.init != nil {
		// Let a little of the post-ServerInit stream in too.
		_ = res.conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		_, _ = res.conn.Read(make([]byte, 64))
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]byte(nil), rec.buf.Bytes()...)
}

func fuzzHandshakeConfig() *handshakeConfig {
	return &handshakeConfig{
		host: "fuzz.example", port: 5900, repeaterID: "42", allowPassthrough: true, exclude: map[uint32]bool{},
		creds: &testCreds{user: "user", pass: "password"}, trust: &testTrust{}, timeout: time.Second,
	}
}

func FuzzRFBHandshake(f *testing.F) {
	creds := func() *testCreds { return &testCreds{user: "alice", pass: "pw"} }
	tlsCfg := selfSignedTLS(f, "fuzz.example")
	for _, sc := range []struct {
		srv *vnctest.Server
		cfg *handshakeConfig
	}{
		{&vnctest.Server{Version: "RFB 003.003\n"}, &handshakeConfig{creds: creds()}},
		{&vnctest.Server{Version: "RFB 003.007\n", Types: []byte{vnctest.SecNone}}, &handshakeConfig{creds: creds()}},
		{&vnctest.Server{Types: []byte{vnctest.SecVNCAuth}, Password: "pw", Name: "desk"}, &handshakeConfig{creds: creds()}},
		{&vnctest.Server{Types: []byte{vnctest.SecVNCAuth}, Password: "other", FailReason: "Authentication failure"}, &handshakeConfig{creds: creds()}},
		{&vnctest.Server{Types: []byte{vnctest.SecARD}, Username: "alice", Password: "pw"}, &handshakeConfig{creds: creds()}},
		{&vnctest.Server{RepeaterID: "42"}, &handshakeConfig{creds: creds(), repeaterID: "42"}},
		{&vnctest.Server{RefuseWith: "Too many security failures"}, &handshakeConfig{creds: creds()}},
		{&vnctest.Server{Types: []byte{vnctest.SecTight, 113}}, &handshakeConfig{creds: creds(), allowPassthrough: true}},
		{&vnctest.Server{Types: []byte{vnctest.SecVeNCrypt}, VeNCryptSubtypes: []uint32{vnctest.VcPlain, vnctest.SecVNCAuth},
			Username: "alice", Password: "pw"}, &handshakeConfig{creds: creds(), policy: encAllowUnencrypted}},
		{&vnctest.Server{Types: []byte{vnctest.SecVeNCrypt}, VeNCryptSubtypes: []uint32{vnctest.VcX509Vnc}, TLS: tlsCfg,
			Password: "pw"}, &handshakeConfig{creds: creds()}},
		{&vnctest.Server{Types: []byte{vnctest.SecVeNCrypt}, VeNCryptSubtypes: []uint32{vnctest.VcTLSVnc}, Password: "pw",
			AnonTLS: &anontlstest.Config{}}, &handshakeConfig{creds: creds()}},
	} {
		f.Add(recordServer(f, sc.srv, sc.cfg))
	}
	f.Add([]byte("RFB 003.008\n\x00\x00\x00\x00\x05hello"))
	f.Add([]byte("RFB 000.000\nRFB 003.008\n\x01\x01\x00\x00\x00\x00"))
	f.Fuzz(func(t *testing.T, stream []byte) {
		for _, policy := range []encryptionPolicy{encPrefer, encRequire, encAllowUnencrypted} {
			cfg := fuzzHandshakeConfig()
			cfg.policy = policy
			res, err := clientHandshake(context.Background(), &replayConn{r: bytes.NewReader(stream)}, cfg)
			if err == nil {
				if res.pass == nil && res.init == nil {
					t.Fatal("success without ServerInit or pass-through")
				}
				if policy == encRequire && !res.encrypted() {
					t.Fatalf("policy require but connected without encryption (%s)", res.securityName())
				}
				if policy != encAllowUnencrypted && res.subType == vcPlain {
					t.Fatal("clear-text Plain login without confirmation")
				}
			}
		}
	})
}

func FuzzForwardClientMessages(f *testing.F) {
	f.Add([]byte{3, 1, 0, 0, 0, 0, 4, 0, 3, 0})
	f.Add(append([]byte{6, 0, 0, 0, 0, 0, 0, 3}, "abc"...))
	f.Add([]byte{2, 0, 0, 2, 0xc0, 0xa1, 0xe5, 0xce, 0, 0, 0, 7})
	f.Add([]byte{255, 0, 0, 1, 0, 0, 0, 0x61, 0, 0, 0, 0x1e, 5, 0x81, 0, 1, 0, 1, 1})
	f.Add(append([]byte{251, 0, 4, 0, 3, 0, 1, 0}, make([]byte, 16)...))
	f.Fuzz(func(t *testing.T, in []byte) {
		for _, filter := range []clientFilter{{}, {dropInput: true}, {dropClipboard: true}, {dropInput: true, dropClipboard: true}} {
			var out bytes.Buffer
			err := forwardClientMessages(bufio.NewReader(bytes.NewReader(in)), &out, filter)
			if err == nil {
				t.Fatal("forwardClientMessages returned without an error")
			}
			if !filter.active() && !bytes.HasPrefix(in, out.Bytes()) {
				t.Fatalf("unfiltered output is not a prefix of the input:\n in %x\nout %x", in, out.Bytes())
			}
			if out.Len() > len(in) {
				t.Fatalf("filtered output (%d bytes) longer than the input (%d)", out.Len(), len(in))
			}
			if filter.dropClipboard && containsClientCutText(out.Bytes()) {
				t.Fatalf("ClientCutText passed the clipboard filter: %x", out.Bytes())
			}
			if errors.Is(err, io.ErrShortWrite) {
				t.Fatal(err)
			}
		}
	})
}

// containsClientCutText re-parses a (filtered, hence well-formed) output stream looking for ClientCutText.
func containsClientCutText(b []byte) bool {
	found := false
	w := writerFunc(func(p []byte) (int, error) {
		if len(p) > 0 && p[0] == 6 {
			found = true
		}
		return len(p), nil
	})
	_ = forwardClientMessages(bufio.NewReader(bytes.NewReader(b)), w, clientFilter{})
	return found
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
