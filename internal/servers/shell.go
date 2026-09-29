package servers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/xpty"
)

// shellSpec describes a shell started for an SSH or Telnet client (as the AstraTerm OS user).
type shellSpec struct {
	command string   // configured shell program ("" = default)
	dir     string   // working directory ("" = home)
	env     []string // extra KEY=VALUE entries (override the inherited environment)
	term    string
	cols    int
	rows    int
	exec    string // run this command through the shell instead of an interactive session
}

// defaultShell returns the host's default interactive shell.
func defaultShell() string {
	if runtime.GOOS == "windows" {
		for _, c := range []string{"pwsh.exe", "powershell.exe"} {
			if p, err := exec.LookPath(c); err == nil {
				return p
			}
		}
		if c := os.Getenv("COMSPEC"); c != "" {
			return c
		}
		return `C:\Windows\System32\cmd.exe`
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		if st, err := os.Stat(sh); err == nil && !st.IsDir() {
			return sh
		}
	}
	for _, c := range []string{"/bin/bash", "/bin/zsh", "/bin/sh"} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return "/bin/sh"
}

// buildShellCmd prepares the command for spec (not started).
func buildShellCmd(spec shellSpec) (*exec.Cmd, error) {
	sh := spec.command
	if sh == "" {
		sh = defaultShell()
	}
	path := sh
	if !filepath.IsAbs(path) {
		p, err := exec.LookPath(sh)
		if err != nil {
			return nil, fmt.Errorf("shell %q not found", sh)
		}
		path = p
	}
	base := strings.ToLower(filepath.Base(path))
	var args []string
	if spec.exec != "" {
		switch {
		case runtime.GOOS == "windows" && (base == "cmd.exe" || base == "cmd"):
			args = []string{"/c", spec.exec}
		case strings.HasPrefix(base, "pwsh") || strings.HasPrefix(base, "powershell"):
			args = []string{"-NoProfile", "-Command", spec.exec}
		default:
			args = []string{"-c", spec.exec}
		}
	}
	cmd := exec.Command(path, args...)
	if spec.exec == "" && runtime.GOOS != "windows" {
		cmd.Args[0] = "-" + filepath.Base(path) // login shell
	}
	dir := spec.dir
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	if dir != "" {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			cmd.Dir = dir
		}
	}
	cmd.Env = shellEnv(spec)
	return cmd, nil
}

// shellEnv inherits AstraTerm's environment (without ASTRATERM_* variables) and applies the terminal settings.
func shellEnv(spec shellSpec) []string {
	vars := map[string]string{}
	var order []string
	set := func(kv string) {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return
		}
		key := k
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(k)
		}
		if _, seen := vars[key]; !seen {
			order = append(order, key)
		}
		vars[key] = kv
	}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "ASTRATERM_") {
			set(kv)
		}
	}
	if spec.term != "" {
		set("TERM=" + spec.term)
	} else if runtime.GOOS != "windows" {
		set("TERM=xterm-256color")
	}
	if runtime.GOOS != "windows" && os.Getenv("LANG") == "" && os.Getenv("LC_ALL") == "" {
		set("LANG=en_US.UTF-8")
	}
	for _, kv := range spec.env {
		set(kv)
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, vars[k])
	}
	return out
}

// ptyShell is a shell running on a pseudo-terminal.
type ptyShell struct {
	pty       xpty.Pty
	cmd       *exec.Cmd
	done      chan struct{}
	code      int
	closeOnce sync.Once
	ptyOnce   sync.Once
}

// startPTYShell starts spec on a new PTY (Unix PTY or Windows ConPTY).
func startPTYShell(spec shellSpec) (*ptyShell, error) {
	cmd, err := buildShellCmd(spec)
	if err != nil {
		return nil, err
	}
	cols, rows := spec.cols, spec.rows
	if cols <= 0 || cols > 1000 {
		cols = 80
	}
	if rows <= 0 || rows > 1000 {
		rows = 24
	}
	p, err := xpty.NewPty(cols, rows)
	if err != nil {
		return nil, fmt.Errorf("create pseudo-terminal: %w", err)
	}
	if err := startOnPTY(p, cmd); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("start %s: %w", filepath.Base(cmd.Path), err)
	}
	s := &ptyShell{pty: p, cmd: cmd, done: make(chan struct{}), code: -1}
	go s.wait()
	return s, nil
}

func (s *ptyShell) wait() {
	err := xpty.WaitProcess(context.Background(), s.cmd)
	s.code = exitStatus(s.cmd, err)
	close(s.done)
	// Let the last output drain, then close the PTY (background jobs may keep the slave open forever).
	time.Sleep(300 * time.Millisecond)
	s.closePTY()
}

func (s *ptyShell) closePTY() { s.ptyOnce.Do(func() { _ = s.pty.Close() }) }

// Read returns output; io.EOF once the shell has exited and the PTY is closed.
func (s *ptyShell) Read(p []byte) (int, error) {
	n, err := s.pty.Read(p)
	if err != nil {
		if n > 0 {
			return n, nil
		}
		return 0, io.EOF
	}
	return n, nil
}

func (s *ptyShell) Write(p []byte) (int, error) { return s.pty.Write(p) }

func (s *ptyShell) Resize(cols, rows int) {
	if cols > 0 && rows > 0 && cols <= 1000 && rows <= 1000 {
		_ = s.pty.Resize(cols, rows)
	}
}

// Done is closed when the shell process has exited.
func (s *ptyShell) Done() <-chan struct{} { return s.done }

// ExitCode is valid after Done.
func (s *ptyShell) ExitCode() int { return s.code }

// Close hangs up the shell (SIGHUP to its process group, a kill after a grace period) and closes the PTY.
func (s *ptyShell) Close() {
	s.closeOnce.Do(func() {
		select {
		case <-s.done:
		default:
			hangup(s.cmd)
			go func() {
				select {
				case <-s.done:
				case <-time.After(3 * time.Second):
					if s.cmd.Process != nil {
						_ = s.cmd.Process.Kill()
					}
				}
			}()
		}
		s.closePTY()
	})
}

// exitStatus maps a wait result to a shell-style exit status (128+n for a signal).
func exitStatus(cmd *exec.Cmd, err error) int {
	if ps := cmd.ProcessState; ps != nil {
		if code := signalStatus(ps); code >= 0 {
			return code
		}
		return ps.ExitCode()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return 255
	}
	return 0
}
