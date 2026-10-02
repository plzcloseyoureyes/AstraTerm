package recording_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/server/servertest"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// fakeShell is a deterministic terminal backend: it echoes typed characters (except while a password is asked),
// runs a line on Enter ("sudo" asks for a password, "exit" ends the session, anything else prints "ran: <line>")
// and prints a prompt, optionally wrapped in OSC 133 shell-integration marks.
type fakeShell struct {
	marks  bool
	out    chan []byte
	mu     sync.Mutex
	line   []byte
	noEcho bool
	closed chan struct{}
	once   sync.Once
}

func newFakeShell(marks bool) *fakeShell {
	f := &fakeShell{marks: marks, out: make(chan []byte, 256), closed: make(chan struct{})}
	f.emit("Welcome to the fake shell\r\n")
	f.prompt(0, false)
	return f
}

func (f *fakeShell) emit(s string) {
	select {
	case f.out <- []byte(s):
	case <-f.closed:
	}
}

func (f *fakeShell) prompt(code int, finished bool) {
	if !f.marks {
		f.emit("user@fake:~$ ")
		return
	}
	d := ""
	if finished {
		d = "\x1b]133;D;" + itoa(code) + "\x07"
	}
	f.emit(d + "\x1b]133;A\x07user@fake:~$ \x1b]133;B\x07")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func (f *fakeShell) Read(p []byte) (int, error) {
	select {
	case b := <-f.out:
		if b == nil {
			return 0, io.EOF
		}
		return copy(p, b), nil
	case <-f.closed:
		return 0, io.EOF
	}
}

func (f *fakeShell) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range p {
		switch {
		case b == '\r':
			f.emit("\r\n")
			f.run(string(f.line))
			f.line = f.line[:0]
		case b == 0x7f:
			if len(f.line) > 0 {
				f.line = f.line[:len(f.line)-1]
				if !f.noEcho {
					f.emit("\b \b")
				}
			}
		case b >= 0x20:
			f.line = append(f.line, b)
			if !f.noEcho {
				f.emit(string(rune(b)))
			}
		}
	}
	return len(p), nil
}

func (f *fakeShell) run(line string) {
	if f.noEcho {
		f.noEcho = false
		f.emit("authenticated\r\n")
		f.prompt(0, true)
		return
	}
	if f.marks {
		f.emit("\x1b]133;C\x07")
	}
	switch strings.TrimSpace(line) {
	case "":
		f.prompt(0, false)
	case "sudo":
		f.noEcho = true
		f.emit("[sudo] password for user: ")
	case "exit":
		f.once.Do(func() { close(f.closed) })
	case "cd":
		f.emit("\x1b]7;file://fake/srv/secret-project\x07")
		f.prompt(0, true)
	case "false":
		f.prompt(1, true)
	default:
		f.emit("ran: " + line + "\r\n")
		f.prompt(0, true)
	}
}

func (f *fakeShell) Resize(cols, rows int) error { return nil }
func (f *fakeShell) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

// ---- environment --------------------------------------------------------------------------------------------------

type testEnv struct {
	*servertest.Env
	admin *servertest.Client
}

const fakeProto = "rlogin" // taken over by the fake shell inside this test binary

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	env := servertest.New(t)
	// Registered after the server mounted the real rlogin module, so the fake wins.
	term.RegisterProtocol(fakeProto, func(ctx context.Context, req term.OpenRequest) (term.Backend, error) {
		return newFakeShell(req.Connection.Options.Bool("marks")), nil
	})
	admin := env.Setup("admin", "correct horse battery staple")
	return &testEnv{Env: env, admin: admin}
}

// openSession starts a fake shell session for c.
func (e *testEnv) openSession(t *testing.T, c *servertest.Client, opts map[string]any) model.RuntimeSession {
	t.Helper()
	var s model.RuntimeSession
	c.MustJSON("POST", "/api/sessions", map[string]any{
		"cols": 80, "rows": 24, "title": "fake box",
		"quick": map[string]any{"protocol": fakeProto, "host": "fake", "options": opts},
	}, &s)
	e.waitState(t, c, s.ID, model.StateConnected)
	return s
}

func (e *testEnv) waitState(t *testing.T, c *servertest.Client, id string, want model.SessionState) {
	t.Helper()
	waitFor(t, "session "+string(want), func() bool {
		var s model.RuntimeSession
		return c.JSON("GET", "/api/sessions/"+id, nil, &s) == http.StatusOK && s.State == want
	})
}

func (e *testEnv) input(t *testing.T, c *servertest.Client, id, data string) {
	t.Helper()
	c.MustJSON("POST", "/api/sessions/"+id+"/input", map[string]string{"data": data}, nil)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second) // generous: Windows CI runners under the race detector are slow
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
