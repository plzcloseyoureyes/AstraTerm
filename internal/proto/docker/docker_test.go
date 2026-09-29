package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/term"
)

func TestShellCommand(t *testing.T) {
	def := shellCommand("")
	if len(def) != 3 || def[0] != "/bin/sh" || def[1] != "-c" {
		t.Fatalf("default = %v", def)
	}
	if got := shellCommand("/bin/bash"); !reflect.DeepEqual(got, []string{"/bin/bash"}) {
		t.Fatalf("explicit shell = %v", got)
	}
	if got := shellCommand("bash -l"); !reflect.DeepEqual(got, []string{"/bin/sh", "-c", "bash -l"}) {
		t.Fatalf("command line = %v", got)
	}
}

func TestNewEngineDialerParsing(t *testing.T) {
	if _, label, err := newEngineDialer("unix:///tmp/foo.sock"); err != nil || label != "unix:///tmp/foo.sock" {
		t.Fatalf("unix parse: label=%q err=%v", label, err)
	}
	if _, label, err := newEngineDialer("tcp://1.2.3.4:2375"); err != nil || label != "tcp://1.2.3.4:2375" {
		t.Fatalf("tcp parse: label=%q err=%v", label, err)
	}
	if _, _, err := newEngineDialer("ftp://nope"); err == nil {
		t.Fatal("expected error for unsupported scheme")
	}
}

func TestDemuxReader(t *testing.T) {
	// Build a multiplexed stream: stdout "out", stderr "err".
	var raw bytes.Buffer
	frame := func(stream byte, payload string) {
		var hdr [8]byte
		hdr[0] = stream
		binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
		raw.Write(hdr[:])
		raw.WriteString(payload)
	}
	frame(1, "out")
	frame(2, "err")
	d := newDemuxReader(&raw)
	got, _ := io.ReadAll(d)
	if !bytes.Contains(got, []byte("out")) {
		t.Errorf("missing stdout in %q", got)
	}
	// stderr should be wrapped in a red SGR.
	if !bytes.Contains(got, []byte("\x1b[31merr\x1b[0m")) {
		t.Errorf("stderr not colorized in %q", got)
	}
}

// TestDockerExecAgainstTestEnv execs a command in the shared test container over the local Docker socket.
func TestDockerExecAgainstTestEnv(t *testing.T) {
	if os.Getenv("TERMSTEAD_TESTENV") != "1" {
		t.Skip("set TERMSTEAD_TESTENV=1 to run against the docker test environment")
	}
	dial, _, err := newEngineDialer("")
	if err != nil {
		t.Skipf("no docker engine: %v", err)
	}
	eng := newEngine(dial)
	defer eng.close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	list, err := eng.listContainers(ctx, true)
	if err != nil {
		t.Skipf("cannot list containers: %v", err)
	}
	target := ""
	for _, c := range list {
		if c.Name == "termstead-testenv-web-1" {
			target = c.ID
		}
	}
	if target == "" {
		t.Skip("termstead-testenv-web-1 not found")
	}
	execID, err := eng.execCreate(ctx, target, execConfig{
		AttachStdout: true, AttachStderr: true, Tty: true,
		Cmd: []string{"/bin/sh", "-c", "echo TERMSTEAD_DOCKER_OK"},
	})
	if err != nil {
		t.Fatalf("execCreate: %v", err)
	}
	conn, br, err := eng.execStart(ctx, execID, true)
	if err != nil {
		t.Fatalf("execStart: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	out, _ := io.ReadAll(br)
	if !bytes.Contains(out, []byte("TERMSTEAD_DOCKER_OK")) {
		t.Fatalf("exec output missing marker: %q", out)
	}
}

// TestDockerSessionBackendsAgainstTestEnv drives the real opener (exec and logs modes) against the shared test
// container: TERM, the initial size, resize, the exit status and a clean end.
func TestDockerSessionBackendsAgainstTestEnv(t *testing.T) {
	if os.Getenv("TERMSTEAD_TESTENV") != "1" {
		t.Skip("set TERMSTEAD_TESTENV=1 to run against the docker test environment")
	}
	if _, _, err := newEngineDialer(""); err != nil {
		t.Skipf("no docker engine: %v", err)
	}
	m := &module{d: &app.Deps{}}
	user := &model.User{ID: "u1", Role: model.RoleAdmin}
	conn := &model.Connection{Protocol: model.ProtoDocker, Options: model.Options{"container": "termstead-testenv-web-1"}}
	be, err := m.open(context.Background(), term.OpenRequest{Connection: conn, User: user})
	if err != nil {
		t.Fatalf("open exec: %v", err)
	}
	col := newCollector(be)
	_ = be.Resize(80, 24)
	if _, err := be.Write([]byte("echo T=$TERM; stty size\r")); err != nil {
		t.Fatal(err)
	}
	out := col.until(10*time.Second, func(b []byte) bool { return bytes.Contains(b, []byte("24 80")) })
	if !bytes.Contains(out, []byte("T=xterm-256color")) || !bytes.Contains(out, []byte("24 80")) {
		t.Fatalf("exec output: %q", out)
	}
	if err := be.Resize(100, 30); err != nil {
		t.Fatalf("resize: %v", err)
	}
	_, _ = be.Write([]byte("stty size\r"))
	if out := col.until(10*time.Second, func(b []byte) bool { return bytes.Contains(b, []byte("30 100")) }); !bytes.Contains(out, []byte("30 100")) {
		t.Fatalf("resize not applied: %q", out)
	}
	_, _ = be.Write([]byte("exit 3\r"))
	col.until(10*time.Second, func([]byte) bool { return false })
	if err := col.error(); err != io.EOF {
		t.Fatalf("exec end = %v, want EOF", err)
	}
	if ec, ok := be.(term.ExitCoder); !ok || ec.ExitCode() != 3 {
		t.Fatalf("exit code = %v", be.(term.ExitCoder).ExitCode())
	}
	be.Close()

	logsConn := &model.Connection{Protocol: model.ProtoDocker, Options: model.Options{"container": "termstead-testenv-web-1", "dockerMode": "logs", "logTail": 5}}
	lb, err := m.open(context.Background(), term.OpenRequest{Connection: logsConn, User: user})
	if err != nil {
		t.Fatalf("open logs: %v", err)
	}
	lcol := newCollector(lb)
	time.Sleep(500 * time.Millisecond)
	lb.Close()
	lcol.until(3*time.Second, func([]byte) bool { return false })
	if err := lcol.error(); err != io.EOF {
		t.Fatalf("logs end after Close = %v, want EOF", err)
	}

	// Unknown containers fail permanently with the engine's message.
	bad := &model.Connection{Protocol: model.ProtoDocker, Options: model.Options{"container": "termstead-no-such-container"}}
	if _, err := m.open(context.Background(), term.OpenRequest{Connection: bad, User: user}); err == nil || !term.IsPermanent(err) || !strings.Contains(err.Error(), "No such container") {
		t.Fatalf("missing container: %v", err)
	}
}

type collector struct {
	mu  sync.Mutex
	buf []byte
	err error
}

func newCollector(r io.Reader) *collector {
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

func (c *collector) until(d time.Duration, pred func([]byte) bool) []byte {
	deadline := time.Now().Add(d)
	for {
		c.mu.Lock()
		if pred(c.buf) || c.err != nil || time.Now().After(deadline) {
			out := c.buf
			c.buf = nil
			c.mu.Unlock()
			return out
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
}

func (c *collector) error() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func TestFriendlyDialErrors(t *testing.T) {
	for in, want := range map[string]string{
		"dial unix /var/run/docker.sock: connect: no such file or directory": "cannot reach the Docker engine",
		"dial unix /var/run/docker.sock: connect: permission denied":         "docker group",
		"ssh: rejected: connect failed (open failed)":                        "refused to open the Docker socket",
	} {
		if got := friendlyDialError(errors.New(in)).Error(); !strings.Contains(got, want) {
			t.Errorf("%q → %q, want it to mention %q", in, got, want)
		}
	}
}

func TestWindowsDefaultEngine(t *testing.T) {
	if runtime.GOOS != "windows" {
		if _, label, err := newEngineDialer("npipe:////./pipe/docker_engine"); err == nil {
			t.Fatalf("npipe accepted on %s (%s)", runtime.GOOS, label)
		}
		return
	}
	_, label, err := newEngineDialer("")
	if err != nil || label != `npipe://\\.\pipe\docker_engine` {
		t.Fatalf("default engine on Windows: %q %v", label, err)
	}
}
