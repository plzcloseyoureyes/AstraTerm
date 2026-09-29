package mosh

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/termstead/termstead/internal/model"
)

func fakeAddr(s string) net.Addr {
	a, _ := net.ResolveTCPAddr("tcp", s)
	return a
}

// Live tests against a real mosh-server reached over SSH. They need direct UDP reachability, so they are driven by
// TERMSTEAD_TEST_MOSH=user:password@host:port (see scripts in the protocols review: run the test binary inside the
// network namespace of a container that has sshd + mosh). TERMSTEAD_TEST_MOSH_PORTS sets moshPorts.

type sshExec struct{ c *ssh.Client }

func (s sshExec) Exec(_ context.Context, cmd string) ([]byte, []byte, int, error) {
	sess, err := s.c.NewSession()
	if err != nil {
		return nil, nil, -1, err
	}
	defer sess.Close()
	var out, errb bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errb
	err = sess.Run(cmd)
	code := 0
	if ee, ok := err.(*ssh.ExitError); ok {
		code, err = ee.ExitStatus(), nil
	}
	return out.Bytes(), errb.Bytes(), code, err
}

func liveSSH(t *testing.T) (*ssh.Client, model.Options) {
	t.Helper()
	spec := os.Getenv("TERMSTEAD_TEST_MOSH")
	if spec == "" {
		t.Skip("set TERMSTEAD_TEST_MOSH=user:password@host:port to run against a real mosh-server")
	}
	cred, addr, _ := strings.Cut(spec, "@")
	user, pass, _ := strings.Cut(cred, ":")
	c, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.Password(pass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("ssh: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	o := model.Options{}
	if p := os.Getenv("TERMSTEAD_TEST_MOSH_PORTS"); p != "" {
		o["moshPorts"] = p
	}
	return c, o
}

func countServers(t *testing.T, c *ssh.Client) int {
	t.Helper()
	out, _, _, _ := sshExec{c}.Exec(context.Background(), "pgrep -c mosh-server || true")
	n := 0
	for _, ch := range strings.TrimSpace(string(out)) {
		if ch >= '0' && ch <= '9' {
			n = n*10 + int(ch-'0')
		}
	}
	return n
}

type liveCollector struct {
	mu  sync.Mutex
	buf []byte
	err error
}

func collectLive(r io.Reader) *liveCollector {
	c := &liveCollector{}
	go func() {
		buf := make([]byte, 8192)
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

func (c *liveCollector) until(d time.Duration, pred func([]byte) bool) ([]byte, error) {
	deadline := time.Now().Add(d)
	for {
		c.mu.Lock()
		if pred(c.buf) || c.err != nil || time.Now().After(deadline) {
			out, err := c.buf, c.err
			c.buf = nil
			c.mu.Unlock()
			return out, err
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
}

func dialLive(t *testing.T, c *ssh.Client, o model.Options) *goBackend {
	t.Helper()
	port, key, err := bootstrap(context.Background(), sshExec{c}, o)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
	ip, err := udpTarget(context.Background(), host, c.RemoteAddr())
	if err != nil {
		t.Fatal(err)
	}
	b, err := dialBuiltin(context.Background(), nil, &net.UDPAddr{IP: ip, Port: port}, key, 80, 24)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return b
}

// TestLiveMoshSession: output, input, resize, and the remote shell's exit ending the session with EOF.
func TestLiveMoshSession(t *testing.T) {
	c, o := liveSSH(t)
	b := dialLive(t, c, o)
	defer b.Close()
	col := collectLive(b)
	if _, err := b.Write([]byte("echo MOSH_$((6*7)); stty size\r")); err != nil {
		t.Fatal(err)
	}
	out, err := col.until(10*time.Second, func(acc []byte) bool {
		return bytes.Contains(acc, []byte("MOSH_42")) && bytes.Contains(acc, []byte("24 80"))
	})
	if !bytes.Contains(out, []byte("MOSH_42")) || !bytes.Contains(out, []byte("24 80")) {
		t.Fatalf("no command output / size (err %v): %q", err, out)
	}
	_ = b.Resize(120, 40)
	time.Sleep(200 * time.Millisecond)
	_, _ = b.Write([]byte("stty size\r"))
	if out, _ := col.until(10*time.Second, func(acc []byte) bool { return bytes.Contains(acc, []byte("40 120")) }); !bytes.Contains(out, []byte("40 120")) {
		t.Fatalf("resize not applied: %q", out)
	}
	_, _ = b.Write([]byte("exit\r"))
	if _, err := col.until(15*time.Second, func([]byte) bool { return false }); err != io.EOF {
		t.Fatalf("shell exit should end the session with EOF, got %v", err)
	}
}

// TestLiveMoshCloseStopsServer: closing the session sends the shutdown; the remote mosh-server must exit.
func TestLiveMoshCloseStopsServer(t *testing.T) {
	c, o := liveSSH(t)
	before := countServers(t, c)
	b := dialLive(t, c, o)
	col := collectLive(b)
	_, _ = b.Write([]byte("echo READY\r"))
	if out, _ := col.until(10*time.Second, func(acc []byte) bool { return bytes.Contains(acc, []byte("READY")) }); !bytes.Contains(out, []byte("READY")) {
		t.Fatalf("session not ready: %q", out)
	}
	if n := countServers(t, c); n != before+1 {
		t.Fatalf("mosh-server count = %d, want %d", n, before+1)
	}
	b.Close()
	deadline := time.Now().Add(10 * time.Second)
	for countServers(t, c) > before && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if n := countServers(t, c); n > before {
		t.Fatalf("mosh-server still running after Close (%d > %d)", n, before)
	}
}

// TestLiveMoshManyStates types more than 1024 separately acknowledged keystrokes (the C server's receive queue limit)
// and checks the session still reacts promptly. Opt-in (slow): TERMSTEAD_TEST_MOSH_LONG=1.
func TestLiveMoshManyStates(t *testing.T) {
	if os.Getenv("TERMSTEAD_TEST_MOSH_LONG") != "1" {
		t.Skip("set TERMSTEAD_TEST_MOSH_LONG=1")
	}
	c, o := liveSSH(t)
	b := dialLive(t, c, o)
	defer b.Close()
	col := collectLive(b)
	sc := b.client
	// Input is acknowledged when the server has a state containing every typed action (they leave the send buffer).
	allAcked := func() bool {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		return !sc.dirty && len(sc.actions) == 0
	}
	slow := 0
	for i := 0; i < 1100; i++ {
		_, _ = b.Write([]byte("x"))
		start := time.Now()
		time.Sleep(time.Millisecond)
		for time.Since(start) < 20*time.Second && !allAcked() {
			time.Sleep(time.Millisecond)
		}
		if el := time.Since(start); el > 3*time.Second {
			slow++
			t.Logf("keystroke %d took %v to be acknowledged", i, el)
			if slow > 3 {
				t.Fatalf("session degraded after %d keystroke states", i)
			}
		}
		if i%100 == 99 {
			col.until(0, func([]byte) bool { return true })
		}
	}
	sc.mu.Lock()
	t.Logf("client states sent: %d, base %d", sc.nextNum-1, sc.baseNum)
	sc.mu.Unlock()
	_, _ = b.Write([]byte("\x15echo LONG_$((6*7))\r"))
	if out, _ := col.until(15*time.Second, func(acc []byte) bool { return bytes.Contains(acc, []byte("LONG_42")) }); !bytes.Contains(out, []byte("LONG_42")) {
		t.Fatalf("no response after 1100 states: %q", out)
	}
}

// TestLiveMoshRemoteCommandAndEnv: options.env and options.remoteCommand reach the remote session; the session ends
// when the command exits.
func TestLiveMoshRemoteCommandAndEnv(t *testing.T) {
	c, o := liveSSH(t)
	o["env"] = map[string]any{"NX_GREETING": "it's here"}
	o["remoteCommand"] = `echo "CMD:$NX_GREETING"; sleep 1`
	b := dialLive(t, c, o)
	defer b.Close()
	col := collectLive(b)
	out, err := col.until(15*time.Second, func([]byte) bool { return false })
	if !bytes.Contains(out, []byte("CMD:it's here")) {
		t.Fatalf("remote command output = %q (err %v)", out, err)
	}
	if err != io.EOF {
		t.Fatalf("session end = %v, want EOF", err)
	}
}
