package telnet

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

func TestEncodeOutbound(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		binary bool
		mode   returnMode
		want   []byte
	}{
		{"cr-nul default", "a\r", false, returnCRNUL, []byte{'a', '\r', 0}},
		{"crlf", "a\r", false, returnCRLF, []byte{'a', '\r', '\n'}},
		{"lf", "a\r", false, returnLF, []byte{'a', '\n'}},
		{"cr", "a\r", false, returnCR, []byte{'a', '\r'}},
		{"binary keeps cr", "a\r", true, returnCRNUL, []byte{'a', '\r'}},
		{"iac doubled", "\xff", false, returnCRNUL, []byte{iacIAC, iacIAC}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := encodeOutbound([]byte(tc.in), tc.binary, tc.mode)
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("encodeOutbound(%q) = % x, want % x", tc.in, got, tc.want)
			}
		})
	}
}

// recConn is a net.Conn whose writes are recorded (reads are unused by parser tests).
type recConn struct {
	mu      sync.Mutex
	written bytes.Buffer
}

func (c *recConn) Read([]byte) (int, error) { return 0, io.EOF }
func (c *recConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.written.Write(p)
}
func (c *recConn) Close() error                     { return nil }
func (c *recConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *recConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *recConn) SetDeadline(time.Time) error      { return nil }
func (c *recConn) SetReadDeadline(time.Time) error  { return nil }
func (c *recConn) SetWriteDeadline(time.Time) error { return nil }
func (c *recConn) wrote() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.written.Bytes()...)
}
func (c *recConn) reset() {
	c.mu.Lock()
	c.written.Reset()
	c.mu.Unlock()
}

func newTestBackend(s settings) (*backend, *recConn) {
	rc := &recConn{}
	if s.cols == 0 {
		s.cols, s.rows = 80, 24
	}
	return newBackend(rc, s), rc
}

// serverStream is a representative server output: negotiation, a TTYPE request, a subnegotiation carrying an
// escaped IAC, data with an escaped 0xFF, a CR NUL (bare CR) and a CR LF.
func serverStream() []byte {
	var s []byte
	s = append(s, iacIAC, iacDO, optECHO)
	s = append(s, iacIAC, iacDO, optNAWS)
	s = append(s, iacIAC, iacWILL, optECHO)
	s = append(s, iacIAC, iacWILL, optSGA)
	s = append(s, 'h', 'i', iacIAC, iacIAC, '!')
	s = append(s, iacIAC, iacSB, optTTYPE, subSEND, iacIAC, iacSE)
	s = append(s, iacIAC, iacSB, 99, 1, iacIAC, iacIAC, 2, iacIAC, iacSE) // unknown option with an escaped IAC
	s = append(s, 'a', '\r', 0, 'b', '\r', '\n')
	s = append(s, iacIAC, iacNOP, iacIAC, iacGA, 'z')
	return s
}

const wantApp = "hi\xff!a\rb\r\nz"

func TestParserWholeStream(t *testing.T) {
	b, rc := newTestBackend(settings{negotiate: true, termType: "xterm", cols: 90, rows: 30})
	out := b.parser.feed(b, serverStream())
	if string(out) != wantApp {
		t.Fatalf("application data = %q, want %q", out, wantApp)
	}
	w := rc.wrote()
	for _, want := range [][]byte{
		{iacIAC, iacWONT, optECHO},                            // we never echo toward the server
		{iacIAC, iacWILL, optNAWS},                            // accept NAWS…
		{iacIAC, iacSB, optNAWS, 0, 90, 0, 30, iacIAC, iacSE}, // …and send the size
		{iacIAC, iacDO, optECHO},                              // let the server echo
		{iacIAC, iacDO, optSGA},                               // accept SGA
		append(append([]byte{iacIAC, iacSB, optTTYPE, subIS}, "XTERM"...), iacIAC, iacSE), // TTYPE IS
	} {
		if !bytes.Contains(w, want) {
			t.Errorf("missing % x in % x", want, w)
		}
	}
	if !b.serverEchoes() {
		t.Error("server echo should be on after WILL ECHO")
	}
}

// TestParserSplitAnywhere feeds the same stream in random pieces (including one byte at a time): sequences split
// across reads must produce exactly the same data and replies.
func TestParserSplitAnywhere(t *testing.T) {
	ref, refConn := newTestBackend(settings{negotiate: true, termType: "xterm", cols: 90, rows: 30})
	wantOut := ref.parser.feed(ref, serverStream())
	wantReplies := refConn.wrote()

	rng := rand.New(rand.NewPCG(1, 2))
	stream := serverStream()
	for trial := 0; trial < 200; trial++ {
		b, rc := newTestBackend(settings{negotiate: true, termType: "xterm", cols: 90, rows: 30})
		var out []byte
		for rest := stream; len(rest) > 0; {
			n := 1
			if trial > 0 {
				n = 1 + rng.IntN(len(rest))
			}
			out = append(out, b.parser.feed(b, rest[:n])...)
			rest = rest[n:]
		}
		if !bytes.Equal(out, wantOut) {
			t.Fatalf("trial %d: data %q, want %q", trial, out, wantOut)
		}
		if !bytes.Equal(rc.wrote(), wantReplies) {
			t.Fatalf("trial %d: replies % x, want % x", trial, rc.wrote(), wantReplies)
		}
	}
}

func TestNegotiationNoLoops(t *testing.T) {
	b, rc := newTestBackend(settings{negotiate: true, termType: "xterm", cols: 80, rows: 24})
	b.sendInitial()
	initial := rc.wrote()
	for _, want := range [][]byte{{iacIAC, iacWILL, optTTYPE}, {iacIAC, iacWILL, optNAWS}, {iacIAC, iacDO, optECHO}} {
		if !bytes.Contains(initial, want) {
			t.Fatalf("initial offers % x lack % x", initial, want)
		}
	}
	rc.reset()

	// The server confirms our requests: no further replies, except the NAWS size report.
	b.parser.feed(b, []byte{iacIAC, iacDO, optTTYPE, iacIAC, iacWILL, optECHO, iacIAC, iacWILL, optSGA, iacIAC, iacDO, optSGA})
	if w := rc.wrote(); len(w) != 0 {
		t.Fatalf("confirmations must not be answered, wrote % x", w)
	}
	b.parser.feed(b, []byte{iacIAC, iacDO, optNAWS})
	if w := rc.wrote(); !bytes.Equal(w, []byte{iacIAC, iacSB, optNAWS, 0, 80, 0, 24, iacIAC, iacSE}) {
		t.Fatalf("DO NAWS (confirming our WILL) should only send the size, wrote % x", w)
	}
	rc.reset()

	// Repeated requests for the current state are ignored (RFC 1143: no loops).
	b.parser.feed(b, []byte{iacIAC, iacWILL, optECHO, iacIAC, iacDO, optNAWS, iacIAC, iacDO, optTTYPE})
	if w := rc.wrote(); len(w) != 0 {
		t.Fatalf("repeated requests answered: % x", w)
	}

	// Turning off an active option is acknowledged exactly once (RFC 1143: YES + WONT → send DONT).
	b.parser.feed(b, []byte{iacIAC, iacWONT, optECHO, iacIAC, iacWONT, optECHO})
	if w := rc.wrote(); !bytes.Equal(w, []byte{iacIAC, iacDONT, optECHO}) || b.serverEchoes() {
		t.Fatalf("WONT ECHO after WILL: wrote % x, echo=%v", w, b.serverEchoes())
	}
	rc.reset()
	// An unsupported option is refused.
	b.parser.feed(b, []byte{iacIAC, iacDO, 34 /* LINEMODE */})
	if w := rc.wrote(); !bytes.Equal(w, []byte{iacIAC, iacWONT, 34}) {
		t.Fatalf("LINEMODE refusal = % x", w)
	}
	rc.reset()

	// DONT for an active option is acknowledged once with WONT, and NAWS stops on resize.
	b.parser.feed(b, []byte{iacIAC, iacDONT, optNAWS})
	if w := rc.wrote(); !bytes.Equal(w, []byte{iacIAC, iacWONT, optNAWS}) {
		t.Fatalf("DONT NAWS ack = % x", w)
	}
	rc.reset()
	_ = b.Resize(100, 40)
	if w := rc.wrote(); len(w) != 0 {
		t.Fatalf("NAWS sent after DONT NAWS: % x", w)
	}
}

func TestRefusedRequestNotAnswered(t *testing.T) {
	b, rc := newTestBackend(settings{negotiate: true, cols: 80, rows: 24})
	b.sendInitial()
	rc.reset()
	// The server refuses our DO ECHO and WILL NEW-ENVIRON: WANTYES + WONT/DONT → NO, no reply.
	b.parser.feed(b, []byte{iacIAC, iacWONT, optECHO, iacIAC, iacDONT, optNEWENV})
	if w := rc.wrote(); len(w) != 0 {
		t.Fatalf("refusals answered: % x", w)
	}
	// Later the server offers ECHO itself: accepted once.
	b.parser.feed(b, []byte{iacIAC, iacWILL, optECHO, iacIAC, iacWILL, optECHO})
	if w := rc.wrote(); !bytes.Equal(w, []byte{iacIAC, iacDO, optECHO}) || !b.serverEchoes() {
		t.Fatalf("late WILL ECHO: wrote % x echo=%v", w, b.serverEchoes())
	}
}

func TestPassiveModeStillReportsSize(t *testing.T) {
	b, rc := newTestBackend(settings{negotiate: false, cols: 80, rows: 24})
	b.parser.feed(b, []byte{iacIAC, iacDO, optNAWS})
	rc.reset()
	_ = b.Resize(132, 43)
	if w := rc.wrote(); !bytes.Equal(w, []byte{iacIAC, iacSB, optNAWS, 0, 132, 0, 43, iacIAC, iacSE}) {
		t.Fatalf("resize in passive mode sent % x", w)
	}
	rc.reset()
	_ = b.Resize(255, 43) // 255 must be doubled inside the subnegotiation
	if w := rc.wrote(); !bytes.Equal(w, []byte{iacIAC, iacSB, optNAWS, 0, iacIAC, iacIAC, 0, 43, iacIAC, iacSE}) {
		t.Fatalf("NAWS with 0xFF = % x", w)
	}
}

func TestNewEnvironEscaping(t *testing.T) {
	got := newEnvReply("a\x01b\xffc")
	want := []byte{iacIAC, iacSB, optNEWENV, subIS, envVAR, 'U', 'S', 'E', 'R', envVALUE, 'a', envESC, 1, 'b', iacIAC, iacIAC, 'c', iacIAC, iacSE}
	if !bytes.Equal(got, want) {
		t.Fatalf("NEW-ENVIRON = % x, want % x", got, want)
	}
	if got := newEnvReply(""); !bytes.Equal(got, []byte{iacIAC, iacSB, optNEWENV, subIS, iacIAC, iacSE}) {
		t.Fatalf("empty NEW-ENVIRON = % x", got)
	}
}

func TestOversizedSubnegotiationIsDropped(t *testing.T) {
	b, rc := newTestBackend(settings{termType: "xterm"})
	stream := []byte{iacIAC, iacSB, optTTYPE, subSEND}
	stream = append(stream, bytes.Repeat([]byte{'x'}, maxSubneg+10)...)
	stream = append(stream, iacIAC, iacSE, 'o', 'k')
	if out := b.parser.feed(b, stream); string(out) != "ok" {
		t.Fatalf("data after oversized SB = %q", out)
	}
	if w := rc.wrote(); len(w) != 0 {
		t.Fatalf("oversized SB must be ignored, wrote % x", w)
	}
}

func TestAutoLoginExpect(t *testing.T) {
	b, rc := newTestBackend(settings{username: "test", password: "secret"})
	b.runLogin([]byte("Welcome\r\nlogin: "))
	if got := rc.wrote(); !bytes.Equal(got, []byte("test\r\x00")) {
		t.Fatalf("username reply = %q", got)
	}
	rc.reset()
	b.runLogin([]byte("Pass"))
	b.runLogin([]byte("word: "))
	if got := rc.wrote(); !bytes.Equal(got, []byte("secret\r\x00")) {
		t.Fatalf("password reply = %q", got)
	}
	rc.reset()
	// Prompts are answered once only (a failed login must not loop on stored credentials).
	b.runLogin([]byte("Login incorrect\r\nlogin: "))
	if got := rc.wrote(); len(got) != 0 {
		t.Fatalf("second login prompt answered: %q", got)
	}
}

// tcpPair returns both ends of a loopback TCP connection.
func tcpPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acc := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		acc <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-acc
	if server == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

// collector reads a backend with a single goroutine (several concurrent readers would steal each other's data).
type collector struct {
	mu  sync.Mutex
	buf []byte
	err error
}

func collect(r io.Reader) *collector {
	c := &collector{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			c.mu.Lock()
			c.buf = append(c.buf, buf[:n]...)
			if err != nil {
				c.err = err
			}
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return c
}

// until waits until pred holds for the unconsumed data (or the source ended, or the timeout passed), then consumes
// and returns that data.
func (c *collector) until(timeout time.Duration, pred func([]byte) bool) []byte {
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		done := pred(c.buf) || c.err != nil || time.Now().After(deadline)
		if done {
			out := c.buf
			c.buf = nil
			c.mu.Unlock()
			return out
		}
		c.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
}

func (c *collector) failed() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func TestLocalEchoIsImmediate(t *testing.T) {
	cl, srv := tcpPair(t)
	b := newBackend(cl, settings{localEcho: true, cols: 80, rows: 24})
	defer b.Close()
	// The server never sends anything: the echo must still arrive (not wait for the next server output).
	if _, err := b.Write([]byte("ls\x7f\x7fok\x1b[A\r")); err != nil {
		t.Fatal(err)
	}
	got := collect(b).until(2*time.Second, func(acc []byte) bool { return bytes.HasSuffix(acc, []byte("\r\n")) })
	if want := "ls\b \b\b \bok\r\n"; string(got) != want {
		t.Fatalf("echo = %q, want %q", got, want)
	}
	buf := make([]byte, 64)
	_ = srv.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := io.ReadAtLeast(srv, buf, len("ls\x7f\x7fok\x1b[A\r\x00"))
	if want := "ls\x7f\x7fok\x1b[A\r\x00"; string(buf[:n]) != want {
		t.Fatalf("server received %q, want %q", buf[:n], want)
	}
}

func TestLocalEchoOffWhileServerEchoes(t *testing.T) {
	cl, srv := tcpPair(t)
	b := newBackend(cl, settings{localEcho: true, negotiate: true, cols: 80, rows: 24})
	defer b.Close()
	b.sendInitial()
	col := collect(b)
	if _, err := srv.Write([]byte{iacIAC, iacWILL, optECHO, 'p', 'r', 'o', 'm', 'p', 't'}); err != nil {
		t.Fatal(err)
	}
	if got := col.until(2*time.Second, func(acc []byte) bool { return bytes.Contains(acc, []byte("prompt")) }); string(got) != "prompt" {
		t.Fatalf("got %q", got)
	}
	if _, err := b.Write([]byte("secret\r")); err != nil {
		t.Fatal(err)
	}
	if got := col.until(300*time.Millisecond, func(acc []byte) bool { return len(acc) > 0 }); len(got) != 0 {
		t.Fatalf("input echoed locally while the server echoes: %q", got)
	}
}

func TestCloseUnblocksRead(t *testing.T) {
	cl, _ := tcpPair(t)
	b := newBackend(cl, settings{cols: 80, rows: 24})
	errc := make(chan error, 1)
	go func() {
		_, err := b.Read(make([]byte, 16))
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	b.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("read after close = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock Read")
	}
}

func TestServerCloseIsEOF(t *testing.T) {
	cl, srv := tcpPair(t)
	b := newBackend(cl, settings{cols: 80, rows: 24})
	defer b.Close()
	_, _ = srv.Write([]byte("bye"))
	srv.Close()
	col := collect(b)
	got := col.until(2*time.Second, func([]byte) bool { return false })
	if string(got) != "bye" {
		t.Fatalf("got %q", got)
	}
	if err := col.failed(); !errors.Is(err, io.EOF) {
		t.Fatalf("read after server close = %v, want EOF", err)
	}
}

// TestTelnetAgainstTestServer connects to the busybox telnetd in the shared docker test environment.
func TestTelnetAgainstTestServer(t *testing.T) {
	if os.Getenv("ASTRATERM_TESTENV") != "1" {
		t.Skip("set ASTRATERM_TESTENV=1 to run against the docker test environment")
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:22023", 5*time.Second)
	if err != nil {
		t.Skipf("telnet test server unreachable: %v", err)
	}
	b := newBackend(conn, settings{negotiate: true, termType: "xterm-256color", cols: 80, rows: 24})
	b.sendInitial()
	defer b.Close()

	col := collect(b)
	got := col.until(8*time.Second, func(acc []byte) bool { return bytes.Contains(acc, []byte("#")) || bytes.Contains(acc, []byte("$")) })
	if len(got) == 0 {
		t.Fatal("no output from telnet server")
	}
	// The window size reached the remote pty through NAWS.
	if _, err := b.Write([]byte("stty size; echo ASTRATERM_$((40+2))\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := col.until(8*time.Second, func(acc []byte) bool { return bytes.Contains(acc, []byte("ASTRATERM_42")) })
	if !bytes.Contains(out, []byte("ASTRATERM_42")) {
		t.Fatalf("did not see command output; got %q", out)
	}
	if !bytes.Contains(out, []byte("24 80")) {
		t.Errorf("remote pty size not 24x80 (NAWS): %q", out)
	}
	_ = b.Resize(100, 30)
	if _, err := b.Write([]byte("stty size\r")); err != nil {
		t.Fatal(err)
	}
	if out := col.until(8*time.Second, func(acc []byte) bool { return bytes.Contains(acc, []byte("30 100")) }); !bytes.Contains(out, []byte("30 100")) {
		t.Errorf("resize did not reach the remote pty: %q", out)
	}
}
