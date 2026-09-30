package anontls

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"testing/cryptotest"
	"time"
)

// Fuzzing (go test -fuzz=FuzzServerStream ./internal/vnc/anontls, likewise FuzzRecordOpen and FuzzPaddingLen).
//
// FuzzServerStream replays a server's complete byte stream to a client whose randomness is deterministic
// (testing/cryptotest.SetGlobalRandom), so a recorded conversation replays exactly: the Finished messages verify and
// the encrypted records after the handshake decrypt. The seed corpus in testdata/fuzz/FuzzServerStream holds
// conversations with the test server (every suite, with and without EMS / encrypt-then-MAC, followed by application
// data, empty records, warnings, HelloRequests, close_notify) and with real servers (OpenSSL s_server, TigerVNC's
// GnuTLS), so the fuzzer mutates deep inside the handshake and the record layer. TestWriteFuzzCorpus regenerates it.

const fuzzSeed = 0x5eed

// replayConn is a net.Conn that reads a fixed server stream and discards writes.
type replayConn struct{ r io.Reader }

func (c *replayConn) Read(b []byte) (int, error)       { return c.r.Read(b) }
func (c *replayConn) Write(b []byte) (int, error)      { return len(b), nil }
func (c *replayConn) Close() error                     { return nil }
func (c *replayConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *replayConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *replayConn) SetDeadline(time.Time) error      { return nil }
func (c *replayConn) SetReadDeadline(time.Time) error  { return nil }
func (c *replayConn) SetWriteDeadline(time.Time) error { return nil }

// replay runs a deterministic client against a recorded server stream; it reports whether the handshake completed
// and the application data received.
func replay(t *testing.T, stream []byte) (bool, []byte) {
	cryptotest.SetGlobalRandom(t, fuzzSeed)
	c := Client(&replayConn{r: bytes.NewReader(stream)}, nil)
	if err := c.Handshake(); err != nil {
		return false, nil
	}
	var got []byte
	buf := make([]byte, 4096)
	for range 4096 {
		n, err := c.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	_, _ = c.Write([]byte("bye"))
	_ = c.Close()
	return true, got
}

func FuzzServerStream(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{recordHandshake, 3, 3, 0, 4, handshakeServerHello, 0, 0, 0})
	f.Add([]byte{recordAlert, 3, 3, 0, 2, alertLevelFatal, alertHandshakeFailure})
	f.Fuzz(func(t *testing.T, stream []byte) {
		ok, data := replay(t, stream)
		if !ok && len(data) != 0 {
			t.Fatal("application data without a handshake")
		}
	})
}

// TestFuzzCorpusReplays keeps the corpus meaningful: the recorded conversations must still complete their handshake
// and deliver their application data (if Go changes how crypto/ecdh consumes randomness, the ECDH seeds stop
// replaying and TestWriteFuzzCorpus has to be rerun).
func TestFuzzCorpusReplays(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("testdata", "fuzz", "FuzzServerStream", "*"))
	if len(files) == 0 {
		t.Skip("no corpus")
	}
	complete := 0
	for _, name := range files {
		stream, err := readCorpusFile(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ok, data := replay(t, stream)
		if ok && len(data) > 0 {
			complete++
		} else if filepath.Base(name) != "truncated" {
			t.Errorf("%s: handshake %v, %d bytes of data", filepath.Base(name), ok, len(data))
		}
	}
	t.Logf("%d of %d seeds complete the handshake and deliver data", complete, len(files))
}

func FuzzRecordOpen(f *testing.F) {
	for i := range suites {
		f.Add(uint8(i), false, []byte{})
		f.Add(uint8(i), true, bytes.Repeat([]byte{0x41}, 64))
	}
	macKey := bytes.Repeat([]byte{1}, 32)
	key := bytes.Repeat([]byte{2}, 32)
	iv := bytes.Repeat([]byte{3}, 4)
	// Valid records as seeds.
	for i, s := range suites {
		for _, etm := range []bool{false, true} {
			w, _ := s.newHalfConn(macKey[:s.macKeyLen()], key[:s.keyLen], iv[:s.fixedIVLen()], etm)
			for _, n := range []int{0, 1, 31, 100} {
				frag, _ := w.seal(recordApplicationData, versionTLS12, bytes.Repeat([]byte{'p'}, n), rand.Reader)
				f.Add(uint8(i), etm, frag)
				w.seq = 0
			}
		}
	}
	f.Fuzz(func(t *testing.T, idx uint8, etm bool, frag []byte) {
		s := suites[int(idx)%len(suites)]
		r, err := s.newHalfConn(macKey[:s.macKeyLen()], key[:s.keyLen], iv[:s.fixedIVLen()], etm)
		if err != nil {
			t.Fatal(err)
		}
		in := append([]byte(nil), frag...)
		pt, err := r.open(recordApplicationData, versionTLS12, in)
		if err != nil {
			if !errors.Is(err, errBadRecordMAC) {
				t.Fatalf("unexpected error type: %v", err)
			}
			if r.seq != 0 {
				t.Fatal("sequence number advanced on a rejected record")
			}
			return
		}
		if len(pt) > len(frag) || r.seq != 1 {
			t.Fatalf("plaintext %d bytes from a %d-byte fragment, seq %d", len(pt), len(frag), r.seq)
		}
		// Whatever decrypted must be exactly what the sender protects: re-sealing it with the same keys must open.
		w, _ := s.newHalfConn(macKey[:s.macKeyLen()], key[:s.keyLen], iv[:s.fixedIVLen()], etm)
		again, err := w.seal(recordApplicationData, versionTLS12, pt, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		r2, _ := s.newHalfConn(macKey[:s.macKeyLen()], key[:s.keyLen], iv[:s.fixedIVLen()], etm)
		if pt2, err := r2.open(recordApplicationData, versionTLS12, again); err != nil || !bytes.Equal(pt, pt2) {
			t.Fatalf("round trip of accepted plaintext failed: %v", err)
		}
	})
}

func FuzzPaddingLen(f *testing.F) {
	f.Add([]byte{0})
	f.Add([]byte{1, 1})
	f.Add(bytes.Repeat([]byte{15}, 16))
	f.Add(bytes.Repeat([]byte{255}, 256))
	f.Add(bytes.Repeat([]byte{255}, 300))
	f.Fuzz(func(t *testing.T, p []byte) {
		if len(p) == 0 {
			return
		}
		checkPadding(t, p)
	})
}

// ---- corpus generation ----------------------------------------------------------------------------------------------

func writeCorpusFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	body := fmt.Sprintf("go test fuzz v1\n[]byte(%q)\n", string(data))
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readCorpusFile(name string) ([]byte, error) {
	b, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	const prefix = "go test fuzz v1\n[]byte("
	s := string(b)
	if len(s) < len(prefix)+2 || s[:len(prefix)] != prefix {
		return nil, errors.New("not a []byte fuzz corpus file")
	}
	var out string
	if _, err := fmt.Sscanf(s[len(prefix):], "%q", &out); err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// recordConversation runs the deterministic client against dial() and returns what the server sent.
func recordConversation(t *testing.T, conn net.Conn, send []byte) []byte {
	t.Helper()
	cryptotest.SetGlobalRandom(t, fuzzSeed)
	rec := &recordingReadConn{Conn: conn}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	c := Client(rec, nil)
	if err := c.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if send != nil {
		if _, err := c.Write(send); err != nil {
			t.Fatal(err)
		}
	}
	buf := make([]byte, 4096)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
		if _, err := c.Read(buf); err != nil {
			break
		}
	}
	return rec.buf.Bytes()
}

type recordingReadConn struct {
	net.Conn
	buf bytes.Buffer
}

func (r *recordingReadConn) Read(b []byte) (int, error) {
	n, err := r.Conn.Read(b)
	r.buf.Write(b[:n])
	return n, err
}

// TestWriteFuzzCorpus regenerates testdata/fuzz/FuzzServerStream (ASTRATERM_ANONTLS_CORPUS=1; the OpenSSL and
// TigerVNC seeds also need ASTRATERM_TESTENV=1).
func TestWriteFuzzCorpus(t *testing.T) {
	if os.Getenv("ASTRATERM_ANONTLS_CORPUS") != "1" {
		t.Skip("set ASTRATERM_ANONTLS_CORPUS=1 to regenerate the fuzz corpus")
	}
	dir := filepath.Join("testdata", "fuzz", "FuzzServerStream")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := func(s *serverConn) error {
		_ = s.writeRecord(recordApplicationData, []byte("RFB security result and ServerInit"))
		_ = s.writeRecord(recordApplicationData, nil)
		_ = s.writeRecord(recordAlert, []byte{alertLevelWarning, alertUserCanceled})
		_ = s.writeRecord(recordHandshake, []byte{handshakeHelloRequest, 0, 0, 0})
		_ = s.writeRecord(recordApplicationData, bytes.Repeat([]byte("framebuffer "), 100))
		return s.writeRecord(recordAlert, []byte{alertLevelWarning, alertCloseNotify})
	}
	for _, s := range suites {
		for _, v := range []struct {
			name       string
			noETM, ems bool
		}{{"etm-ems", false, true}, {"mte-ems", true, true}, {"etm-noems", false, false}} {
			if s.aead && v.noETM {
				continue
			}
			ts := &testServer{suite: s.id, noETM: v.noETM, noEMS: !v.ems, after: script, splitFirst: v.name == "etm-noems"}
			if s.kx == kxDH && v.name == "mte-ems" {
				ts.dhGroup = "ffdhe3072"
			}
			cc, sc := tcpPair(t)
			go func() { _ = ts.serve(sc); sc.Close() }()
			stream := recordConversation(t, cc, nil)
			writeCorpusFile(t, dir, fmt.Sprintf("mirror-%s-%s", s.name, v.name), stream)
		}
	}
	// A non-standard group (primality test path) and a truncated conversation.
	{
		ts := &testServer{suite: TLS_DH_anon_WITH_AES_128_GCM_SHA256, dhP: mustBig(rfc5114p2048), dhG: mustBig(rfc5114g2048), after: script}
		cc, sc := tcpPair(t)
		go func() { _ = ts.serve(sc); sc.Close() }()
		stream := recordConversation(t, cc, nil)
		writeCorpusFile(t, dir, "mirror-rfc5114-2048", stream)
		writeCorpusFile(t, dir, "truncated", stream[:len(stream)/2])
	}
	if os.Getenv("ASTRATERM_TESTENV") != "1" {
		t.Log("ASTRATERM_TESTENV not set: skipping OpenSSL / TigerVNC seeds")
		return
	}
	if bin, err := exec.LookPath("openssl"); err == nil {
		dhparam := filepath.Join(t.TempDir(), "ffdhe2048.pem")
		if out, err := exec.Command(bin, "genpkey", "-genparam", "-algorithm", "DH", "-pkeyopt", "group:ffdhe2048", "-out", dhparam).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		for _, v := range []struct {
			name, cipher string
			extra        []string
		}{
			{"openssl-dh-gcm", "ADH-AES256-GCM-SHA384", []string{"-dhparam", dhparam}},
			{"openssl-ecdh-cbc-etm", "AECDH-AES128-SHA", nil},
			{"openssl-ecdh-cbc-mte", "AECDH-AES256-SHA", []string{"-no_etm"}},
			{"openssl-dh-cbc-sha256-etm", "ADH-AES128-SHA256", []string{"-dhparam", dhparam}},
		} {
			addr := startOpenSSL(t, bin, v.cipher, true, true, v.extra...)
			raw, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			stream := recordConversation(t, raw, []byte("hello openssl\n"))
			raw.Close()
			writeCorpusFile(t, dir, v.name, stream)
		}
	}
	if raw, err := net.DialTimeout("tcp", envOr("ASTRATERM_TEST_VNC", "127.0.0.1:22059"), 5*time.Second); err == nil {
		// RFB up to the VeNCrypt TLSVnc subtype, then TigerVNC's TLS conversation: handshake, the encrypted VNC
		// challenge, our (correct) response and the SecurityResult. Authenticating matters: TigerVNC blacklists
		// clients after repeated security failures, and the test server is shared.
		defer raw.Close()
		step := func(w io.ReadWriter, write []byte, n int) []byte {
			if write != nil {
				if _, err := w.Write(write); err != nil {
					t.Fatal(err)
				}
			}
			b := make([]byte, n)
			if _, err := io.ReadFull(w, b); err != nil {
				t.Fatal(err)
			}
			return b
		}
		_ = raw.SetDeadline(time.Now().Add(20 * time.Second))
		step(raw, nil, 12)
		n := step(raw, []byte("RFB 003.008\n"), 1)
		if n[0] == 0 {
			t.Fatal("TigerVNC refuses connections (blacklisted?)")
		}
		step(raw, nil, int(n[0]))
		step(raw, []byte{19}, 2)
		step(raw, []byte{0, 2}, 1)
		cnt := step(raw, nil, 1)
		step(raw, nil, 4*int(cnt[0]))
		if ok := step(raw, []byte{0, 0, 1, 2}, 1); ok[0] != 1 {
			t.Fatal("TLSVnc refused")
		}
		cryptotest.SetGlobalRandom(t, fuzzSeed)
		rec := &recordingReadConn{Conn: raw}
		c := Client(rec, nil)
		challenge := step(c, nil, 16)
		if res := step(c, desResponse(envOr("ASTRATERM_TEST_VNC_PASSWORD", "vncpassword"), challenge), 4); !bytes.Equal(res, []byte{0, 0, 0, 0}) {
			t.Fatalf("VNC authentication failed: %x", res)
		}
		writeCorpusFile(t, dir, "tigervnc-tlsvnc", rec.buf.Bytes())
	}
}
