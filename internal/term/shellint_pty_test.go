//go:build unix

package term

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/xpty"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Real shells behind a PTY: the injected line must be invisible, the folder reported and followed, keystrokes
// typed during the injection executed, and full-screen programs never typed into.

const ptyProto = "shellpty-test"

var ptyOnce sync.Once

type ptyBackend struct {
	p   xpty.Pty
	cmd *exec.Cmd
}

func (b *ptyBackend) Read(p []byte) (int, error) {
	n, err := b.p.Read(p)
	if err != nil && n == 0 {
		return 0, io.EOF // EIO when the shell exits
	}
	return n, nil
}
func (b *ptyBackend) Write(p []byte) (int, error) { return b.p.Write(p) }
func (b *ptyBackend) Resize(c, r int) error       { return b.p.Resize(c, r) }
func (b *ptyBackend) Close() error {
	if b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
	}
	return b.p.Close()
}

func registerPtyProto() {
	ptyOnce.Do(func() {
		RegisterProtocol(ptyProto, func(ctx context.Context, req OpenRequest) (Backend, error) {
			argv := req.Connection.Options.Strings("argv")
			p, err := xpty.NewPty(100, 30)
			if err != nil {
				return nil, err
			}
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Dir = req.Connection.Options.String("dir", "/")
			// A session leader with the PTY as controlling terminal, like an SSH login (job control, /dev/tty).
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
			cmd.Env = []string{"TERM=xterm-256color", "PS1=nx-test$ ", "PROMPT=nx-test$ ", "HISTFILE=/dev/null",
				"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + req.Connection.Options.String("dir", "/"), "LANG=C"}
			if err := p.Start(cmd); err != nil {
				p.Close()
				return nil, err
			}
			return &ptyBackend{p: p, cmd: cmd}, nil
		})
	})
}

func startShell(t *testing.T, h *harness, argv ...string) *Session {
	t.Helper()
	registerPtyProto()
	s, err := h.m.Create(context.Background(), h.user, CreateRequest{
		Connection: &model.Connection{Protocol: ptyProto, Options: model.Options{"argv": argv, "dir": "/tmp"}},
		Cols:       100, Rows: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.waitState(s, model.StateConnected)
	return s
}

func text(s *Session) string { return StripText(s.Scrollback()) }

// waitQuietPrompt waits until the output ends with the prompt and stayed unchanged for a moment.
func waitQuietPrompt(t *testing.T, s *Session, prompt string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last, since := int64(-1), time.Now()
	for time.Now().Before(deadline) {
		_, head := s.Offsets()
		if head != last {
			last, since = head, time.Now()
		} else if time.Since(since) > 400*time.Millisecond &&
			strings.HasSuffix(strings.TrimRight(cursorLine([]byte(lastLine(s.Scrollback()))), " "), strings.TrimSpace(prompt)) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no quiet prompt; output:\n%s", text(s))
}

func lastLine(b []byte) string {
	if i := strings.LastIndexByte(string(b), '\n'); i >= 0 {
		return string(b[i+1:])
	}
	return string(b)
}

func TestShellIntegrationRealShells(t *testing.T) {
	host, _ := exec.Command("uname", "-n").Output()
	shells := []struct {
		argv   []string
		kind   string
		follow bool
	}{
		{[]string{"/bin/bash", "--norc", "--noprofile", "-i"}, "bash", true},
		{[]string{"/bin/zsh", "-f", "-i"}, "zsh", true},
		{[]string{"/bin/ksh", "-i"}, "ksh", true},
		{[]string{"/bin/dash", "-i"}, "posix", false},
		{[]string{"/bin/sh", "-i"}, "posix", true}, // bash (or dash) as sh: the all-variants line
	}
	for _, sh := range shells {
		if _, err := os.Stat(sh.argv[0]); err != nil {
			continue
		}
		t.Run(strings.TrimPrefix(sh.argv[0], "/bin/"), func(t *testing.T) {
			h := newHarness(t, 0)
			s := startShell(t, h, sh.argv...)
			defer h.m.Close(s.ID)
			waitQuietPrompt(t, s, "nx-test$")
			before := text(s)
			line := ShellIntegrationLineFor(sh.kind, strings.TrimSpace(string(host)))
			if err := h.m.InjectHidden(s.ID, line, InjectOptions{}); err != nil {
				t.Fatalf("inject: %v\n%s", err, text(s))
			}
			// The user types while the hidden command runs.
			if err := h.m.Write(s.ID, []byte("echo typed-$((40+2))\r")); err != nil {
				t.Fatal(err)
			}
			siWait(t, "cwd", func() bool { return strings.HasSuffix(s.Cwd(), "/tmp") })
			siWait(t, "typed command", func() bool { return strings.Contains(text(s), "typed-42") })
			siWait(t, "filter done", func() bool { return filterDone(s) })
			if sh.follow {
				_ = h.m.Write(s.ID, []byte("cd /usr\r"))
				siWait(t, "cwd follows cd", func() bool { return s.Cwd() == "/usr" })
			}
			waitQuietPrompt(t, s, "nx-test$")
			after := text(s)
			for _, leak := range []string{"nx7", "OPTIND", "PROMPT_COMMAND", "uname"} {
				if strings.Contains(after, leak) {
					t.Fatalf("injected line visible (%q):\n%s", leak, after)
				}
			}
			if b := strings.TrimSuffix(before, "nx-test$\n"); !strings.HasPrefix(after, b+"nx-test$ echo typed") {
				t.Fatalf("output before the injection changed:\nbefore %q\nafter  %q", before, after)
			}
			t.Logf("%s terminal:\n%s", sh.argv[0], after)
		})
	}
}

// Full-screen programs (alternate screen) and pagers are never typed into, however long they run.
func TestShellIntegrationNeverTypesIntoFullScreenPrograms(t *testing.T) {
	for _, prog := range []string{"vim -u NONE -N /tmp/astraterm-si-test.txt", "less /etc/hosts"} {
		bin := strings.Fields(prog)[0]
		if _, err := exec.LookPath(bin); err != nil {
			continue
		}
		t.Run(bin, func(t *testing.T) {
			h := newHarness(t, 0)
			s := startShell(t, h, "/bin/bash", "--norc", "--noprofile", "-i")
			defer h.m.Close(s.ID)
			waitQuietPrompt(t, s, "nx-test$")
			_ = h.m.Write(s.ID, []byte(prog+"\r"))
			siWait(t, "alternate screen", func() bool { s.mu.Lock(); g := s.gen; s.mu.Unlock(); return s.shell().altActive(g) })
			time.Sleep(500 * time.Millisecond)
			err := h.m.InjectHidden(s.ID, ShellIntegrationLine("bash"), InjectOptions{})
			if !errors.Is(err, ErrNotAtPrompt) {
				t.Fatalf("injection into %s: %v", bin, err)
			}
			if bin == "vim" {
				_ = h.m.Write(s.ID, []byte("\x1b:q!\r"))
			} else {
				_ = h.m.Write(s.ID, []byte("q"))
			}
			waitQuietPrompt(t, s, "nx-test$")
			if err := h.m.InjectHidden(s.ID, ShellIntegrationLine("bash"), InjectOptions{}); err != nil {
				t.Fatalf("after quitting %s: %v", bin, err)
			}
			siWait(t, "cwd", func() bool { return s.Cwd() != "" })
			siWait(t, "filter done", func() bool { return filterDone(s) })
		})
	}
	os.Remove("/tmp/astraterm-si-test.txt")
}
