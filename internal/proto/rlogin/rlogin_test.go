package rlogin

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

func TestRoutedDetection(t *testing.T) {
	if routed(&model.Connection{}) {
		t.Error("plain connection should be direct")
	}
	if !routed(&model.Connection{Options: model.Options{"sshTunnelVia": "abc"}}) {
		t.Error("sshTunnelVia should be routed")
	}
	if !routed(&model.Connection{Options: model.Options{"jumpHosts": []any{"h"}}}) {
		t.Error("jumpHosts should be routed")
	}
	if !routed(&model.Connection{Options: model.Options{"proxy": map[string]any{"type": "socks5", "host": "h", "port": 1}}}) {
		t.Error("proxy should be routed")
	}
}

func TestLoginMatchOnce(t *testing.T) {
	st := loginState{enabled: true}
	if got := st.match("", "hunter2", []byte("Password: ")); got != "hunter2\r" {
		t.Fatalf("password match got %q", got)
	}
	if got := st.match("", "hunter2", []byte("Password: ")); got != "" {
		t.Fatalf("password must be sent once, got %q", got)
	}
}

func TestWindowSizeMessage(t *testing.T) {
	got := windowSizeMessage(100, 40)
	want := []byte{0xff, 0xff, 's', 's', 0, 40, 0, 100, 0, 0, 0, 0}
	if !bytes.Equal(got, want) {
		t.Fatalf("window size message = % x, want % x", got, want)
	}
}

// collector reads a backend with one goroutine.
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

func (c *collector) until(timeout time.Duration, pred func([]byte) bool) []byte {
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		if pred(c.buf) || c.err != nil || time.Now().After(deadline) {
			out := c.buf
			c.buf = nil
			c.mu.Unlock()
			return out
		}
		c.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
}

func (c *collector) error() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func readNulString(r *bufio.Reader) (string, error) {
	s, err := r.ReadString(0)
	return strings.TrimSuffix(s, "\x00"), err
}

// fakeRlogind emulates rlogind: it reads the handshake, confirms with NUL, records the first window-size message,
// prompts for a password, and then echoes input lines.
type fakeRlogind struct {
	ln net.Listener

	mu        sync.Mutex
	handshake []string
	winsize   []byte
	password  string
	input     bytes.Buffer
}

func startFakeRlogind(t *testing.T, refuse string) *fakeRlogind {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRlogind{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		if b, _ := br.ReadByte(); b != 0 {
			return
		}
		var hs []string
		for i := 0; i < 3; i++ {
			s, err := readNulString(br)
			if err != nil {
				return
			}
			hs = append(hs, s)
		}
		f.mu.Lock()
		f.handshake = hs
		f.mu.Unlock()
		if refuse != "" {
			_, _ = c.Write([]byte("\x01rlogind: " + refuse + "\r\n"))
			return
		}
		_, _ = c.Write([]byte{0})
		ws := make([]byte, 12)
		if _, err := io.ReadFull(br, ws); err != nil {
			return
		}
		f.mu.Lock()
		f.winsize = ws
		f.mu.Unlock()
		_, _ = c.Write([]byte("Password: "))
		line, err := br.ReadString('\r')
		if err != nil {
			return
		}
		f.mu.Lock()
		f.password = strings.TrimSuffix(line, "\r")
		f.mu.Unlock()
		_, _ = c.Write([]byte("\r\nLast login: never\r\n$ "))
		buf := make([]byte, 256)
		for {
			n, err := br.Read(buf)
			if n > 0 {
				f.mu.Lock()
				f.input.Write(buf[:n])
				f.mu.Unlock()
				_, _ = c.Write(bytes.ReplaceAll(buf[:n], []byte("\r"), []byte("\r\n$ ")))
			}
			if err != nil {
				return
			}
		}
	}()
	return f
}

func (f *fakeRlogind) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func TestRloginSessionAgainstFakeServer(t *testing.T) {
	f := startFakeRlogind(t, "")
	conn := &model.Connection{Protocol: model.ProtoRlogin, Host: "127.0.0.1", Port: f.port(), Username: "alice",
		Options: model.Options{"term": "vt220", "localUser": "astraterm"}}
	nc, err := dial(context.Background(), nil, nil, term.OpenRequest{Connection: conn}, f.port())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	b, err := startRlogin(nc, conn, map[string]string{model.SecretPassword: "s3cret"}, 132, 43)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer b.Close()
	col := collect(b)
	if out := col.until(3*time.Second, func(acc []byte) bool { return bytes.Contains(acc, []byte("$ ")) }); !bytes.Contains(out, []byte("Last login")) {
		t.Fatalf("no shell after auto-login: %q", out)
	}
	f.mu.Lock()
	hs, ws, pw := f.handshake, f.winsize, f.password
	f.mu.Unlock()
	if strings.Join(hs, "|") != "astraterm|alice|vt220/38400" {
		t.Errorf("handshake = %q", hs)
	}
	if want := []byte{0xff, 0xff, 's', 's', 0, 43, 0, 132, 0, 0, 0, 0}; !bytes.Equal(ws, want) {
		t.Errorf("window size = % x, want % x", ws, want)
	}
	if pw != "s3cret" {
		t.Errorf("auto-login password = %q", pw)
	}
	// Enter sends CR by default (a CR LF would reach the remote pty as two newlines).
	if _, err := b.Write([]byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	col.until(2*time.Second, func(acc []byte) bool { return bytes.Contains(acc, []byte("ls")) })
	f.mu.Lock()
	in := f.input.String()
	f.mu.Unlock()
	if in != "ls\r" {
		t.Errorf("remote received %q, want %q", in, "ls\r")
	}
}

func TestRloginRefusalIsPermanent(t *testing.T) {
	f := startFakeRlogind(t, "Permission denied.")
	conn := &model.Connection{Protocol: model.ProtoRlogin, Host: "127.0.0.1", Port: f.port(), Username: "bob"}
	nc, err := net.Dial("tcp", f.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	_, err = startRlogin(nc, conn, nil, 80, 24)
	if err == nil || !term.IsPermanent(err) || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("err = %v (permanent=%v)", err, term.IsPermanent(err))
	}
}

// TestDialReservedFailsFastOnRefusal: an unreachable port must fail once, not once per reserved port.
func TestDialReservedFailsFastOnRefusal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	start := time.Now()
	_, err = dialReserved(context.Background(), addr, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, errNoReservedPort) && time.Since(start) > 2*time.Second {
		t.Fatalf("refused dial took %v", time.Since(start))
	}
}

func TestGuardHandshakeTimesOutAndCancels(t *testing.T) {
	cl, srv := net.Pipe()
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := guardHandshake(ctx, cl, func() error {
		_, err := cl.Read(make([]byte, 1)) // blocks until the watchdog closes cl
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// fakeRshd emulates rshd: it connects the stderr back-channel, confirms, writes stdout and stderr, then echoes the
// command's stdin until EOF.
func startFakeRshd(t *testing.T) (port int, got chan []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan []string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		portStr, err := readNulString(br)
		if err != nil {
			return
		}
		var stderr net.Conn
		if p, _ := strconv.Atoi(portStr); p != 0 {
			stderr, err = net.Dial("tcp", net.JoinHostPort("127.0.0.1", portStr))
			if err != nil {
				return
			}
			defer stderr.Close()
		}
		var fields []string
		for i := 0; i < 3; i++ {
			s, err := readNulString(br)
			if err != nil {
				return
			}
			fields = append(fields, s)
		}
		_, _ = c.Write([]byte{0})
		_, _ = c.Write([]byte("out\n"))
		if stderr != nil {
			_, _ = stderr.Write([]byte("err\n"))
		}
		stdin, _ := io.ReadAll(br) // until the client closes stdin (Ctrl+D)
		_, _ = c.Write([]byte("stdin=" + strconv.Quote(string(stdin)) + "\n"))
		got <- append([]string{portStr}, fields...)
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}

func TestRshSessionAgainstFakeServer(t *testing.T) {
	port, got := startFakeRshd(t)
	conn := &model.Connection{Protocol: model.ProtoRlogin, Host: "127.0.0.1", Port: port, Username: "carol",
		Options: model.Options{"variant": "rsh", "command": "cat"}}
	nc, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := startRsh(nc, conn, "cat", true)
	if err != nil {
		t.Fatalf("rsh handshake: %v", err)
	}
	defer b.Close()
	col := collect(b)
	// Typed input goes to stdin with LF line endings, is echoed locally; Ctrl+D ends stdin.
	if _, err := b.Write([]byte("hello\r\x04")); err != nil {
		t.Fatal(err)
	}
	// The server's output may land between (or before) the local echoes: wait for all of them, check each alone.
	out := col.until(3*time.Second, func(acc []byte) bool {
		return bytes.Contains(acc, []byte("stdin=")) && bytes.Contains(acc, []byte("^D\r\n"))
	})
	fields := <-got
	if fields[1] != "carol" || fields[2] != "carol" || fields[3] != "cat" {
		t.Errorf("handshake fields = %q", fields)
	}
	for _, want := range []string{"out\n", "hello\r\n", "^D\r\n", `stdin="hello\n"`} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
	// stderr arrives on the back-channel when a reserved port could be bound (root / macOS), else it is merged.
	if fields[0] != "0" {
		if rest := col.until(2*time.Second, func(acc []byte) bool { return bytes.Contains(append(out, acc...), []byte("err")) }); !bytes.Contains(append(out, rest...), []byte("\x1b[31merr\n\x1b[0m")) {
			t.Errorf("stderr not shown in red: %q", append(out, rest...))
		}
	}
	col.until(3*time.Second, func([]byte) bool { return false })
	if err := col.error(); !errors.Is(err, io.EOF) {
		t.Errorf("session end = %v, want EOF", err)
	}
}

// TestRshStderrBackChannel exercises the stderr back-channel with an unprivileged listener (rshd itself would insist
// on a reserved port, which needs root).
func TestRshStderrBackChannel(t *testing.T) {
	prev := listenBackChannel
	listenBackChannel = func(local net.Addr) *net.TCPListener {
		ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: addrIP(local)})
		if err != nil {
			return nil
		}
		return ln
	}
	defer func() { listenBackChannel = prev }()

	port, got := startFakeRshd(t)
	conn := &model.Connection{Protocol: model.ProtoRlogin, Host: "127.0.0.1", Port: port, Username: "dave",
		Options: model.Options{"variant": "rsh", "command": "cat", "localEcho": false}}
	nc, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := startRsh(nc, conn, "cat", true)
	if err != nil {
		t.Fatalf("rsh handshake: %v", err)
	}
	defer b.Close()
	col := collect(b)
	if _, err := b.Write([]byte("x\x04")); err != nil {
		t.Fatal(err)
	}
	out := col.until(3*time.Second, func([]byte) bool { return false }) // until EOF
	if !bytes.Contains(out, []byte("\x1b[31merr\n\x1b[0m")) || !bytes.Contains(out, []byte(`stdin="x"`)) {
		t.Errorf("output = %q", out)
	}
	if err := col.error(); !errors.Is(err, io.EOF) {
		t.Errorf("session end = %v, want EOF", err)
	}
	fields := <-got
	if fields[0] == "0" {
		t.Fatal("no back-channel port was sent")
	}
	b.mu.Lock()
	hadStderr := b.stderr != nil
	b.mu.Unlock()
	if !hadStderr {
		t.Fatal("stderr connection not accepted")
	}
}

// TestServeFakeRlogind is a manual end-to-end harness: with ASTRATERM_SERVE_RLOGIND=host:port it serves the fake rlogind
// (password prompt, then an echoing "$ " prompt) for any number of connections until the process is killed.
func TestServeFakeRlogind(t *testing.T) {
	addr := os.Getenv("ASTRATERM_SERVE_RLOGIND")
	if addr == "" {
		t.Skip("set ASTRATERM_SERVE_RLOGIND=host:port to serve a fake rlogind for manual end-to-end tests")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("fake rlogind listening on", ln.Addr())
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			br := bufio.NewReader(c)
			if b, _ := br.ReadByte(); b != 0 {
				return
			}
			var hs []string
			for i := 0; i < 3; i++ {
				s, err := readNulString(br)
				if err != nil {
					return
				}
				hs = append(hs, s)
			}
			_, _ = c.Write([]byte{0})
			ws := make([]byte, 12)
			if _, err := io.ReadFull(br, ws); err != nil {
				return
			}
			_, _ = fmt.Fprintf(c, "handshake=%s rows=%d cols=%d\r\nPassword: ", strings.Join(hs, "|"), int(ws[4])<<8|int(ws[5]), int(ws[6])<<8|int(ws[7]))
			// Like rlogind, strip window-size messages (FF FF s s + 8 bytes) wherever they appear in the input.
			var in []byte
			stripped := func() []byte {
				for {
					i := bytes.Index(in, []byte{0xff, 0xff, 's', 's'})
					if i < 0 || len(in) < i+12 {
						return in
					}
					_, _ = fmt.Fprintf(c, "[winsize rows=%d cols=%d]", int(in[i+4])<<8|int(in[i+5]), int(in[i+6])<<8|int(in[i+7]))
					in = append(in[:i], in[i+12:]...)
				}
			}
			buf := make([]byte, 256)
			loggedIn := false
			for {
				n, err := br.Read(buf)
				in = append(in, buf[:n]...)
				data := stripped()
				if !loggedIn {
					if i := bytes.IndexByte(data, '\r'); i >= 0 {
						_, _ = fmt.Fprintf(c, "\r\nlogged in with %q\r\n$ ", data[:i])
						in = append([]byte(nil), data[i+1:]...)
						loggedIn = true
					}
				} else if len(data) > 0 {
					_, _ = c.Write(bytes.ReplaceAll(data, []byte("\r"), []byte("\r\n$ ")))
					in = in[:0]
				}
				if err != nil {
					return
				}
			}
		}()
	}
}
