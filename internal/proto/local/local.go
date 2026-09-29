// Package local implements the "local" terminal protocol: a shell on the machine running Termstead, on a Unix PTY or a
// Windows ConPTY (github.com/charmbracelet/x/xpty), plus shell detection for GET /api/local/shells (PROTO-14/15,
// RESEARCH §3.9). In server mode local shells are restricted to administrators.
package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/xpty"
	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/core"
	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/term"
)

// Mount registers the "local" protocol and GET /api/local/shells.
func Mount(d *app.Deps, c *core.Core) error {
	allowed := func(user *model.User) error {
		if user == nil {
			return httpx.ErrUnauthorized
		}
		if d.Cfg != nil && d.Cfg.IsServer() && !user.IsAdmin() {
			return httpx.Forbidden("local shells are only available to administrators in server mode")
		}
		return nil
	}
	term.RegisterPolicy(string(model.ProtoLocal), func(_ context.Context, user *model.User, _ *model.Connection) error {
		return allowed(user)
	})
	term.RegisterProtocol(string(model.ProtoLocal), func(ctx context.Context, req term.OpenRequest) (term.Backend, error) {
		if err := allowed(req.User); err != nil {
			return nil, term.Permanent(err)
		}
		return Open(ctx, req)
	})
	d.Router.API().GET("/local/shells", func(c *echo.Context) error {
		if err := allowed(httpx.UserFrom(c)); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, Shells(c.Request().Context()))
	})
	return nil
}

// Shell is one entry of GET /api/local/shells.
type Shell struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Path string   `json:"path"`
	Args []string `json:"args"`
}

var shellCache struct {
	sync.Mutex
	at     time.Time
	shells []Shell
}

// Shells returns the shells available on this host (cached for 30 s; WSL enumeration is slow).
func Shells(ctx context.Context) []Shell {
	shellCache.Lock()
	defer shellCache.Unlock()
	if shellCache.shells != nil && time.Since(shellCache.at) < 30*time.Second {
		return append([]Shell(nil), shellCache.shells...)
	}
	list := detectShells(ctx)
	for i := range list {
		if list[i].Args == nil {
			list[i].Args = []string{}
		}
	}
	shellCache.shells, shellCache.at = list, time.Now()
	return append([]Shell(nil), list...)
}

// resolveShell maps options.shell (an ID from Shells, a path, or a command name; empty = default) to a program.
func resolveShell(ctx context.Context, spec string) (Shell, error) {
	spec = strings.TrimSpace(spec)
	shells := Shells(ctx)
	if spec == "" {
		if len(shells) > 0 {
			return shells[0], nil
		}
		return Shell{}, errors.New("no shell found on this host")
	}
	for _, s := range shells {
		if s.ID == spec || strings.EqualFold(s.Path, spec) {
			return s, nil
		}
	}
	path := spec
	if !filepath.IsAbs(path) {
		p, err := exec.LookPath(path)
		if err != nil {
			return Shell{}, fmt.Errorf("shell %q not found", spec)
		}
		path = p
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return Shell{}, fmt.Errorf("shell %q not found", spec)
	}
	return Shell{ID: path, Name: filepath.Base(path), Path: path, Args: []string{}}, nil
}

// Open starts the local shell described by req.Connection.Options (shell, args, cwd, env, loginShell).
func Open(ctx context.Context, req term.OpenRequest) (term.Backend, error) {
	o := model.Options{}
	if req.Connection != nil {
		o = req.Connection.Options
	}
	sh, err := resolveShell(ctx, o.String("shell", ""))
	if err != nil {
		return nil, term.Permanent(err)
	}
	args := sh.Args
	if o.Has("args") {
		args = o.Strings("args")
	}
	dir, err := workingDir(o.String("cwd", ""))
	if err != nil {
		return nil, term.Permanent(err)
	}
	cols, rows := 80, 24
	if req.Session != nil {
		cols, rows = req.Session.Size()
	}
	cmd := exec.Command(sh.Path, args...)
	cmd.Dir = dir
	cmd.Env = buildEnv(o)
	if runtime.GOOS != "windows" && o.Bool("loginShell", true) && len(args) == 0 {
		// Login shell by convention: argv[0] starts with '-' (works for every POSIX shell).
		cmd.Args[0] = "-" + filepath.Base(sh.Path)
	}
	p, err := xpty.NewPty(cols, rows)
	if err != nil {
		return nil, fmt.Errorf("create pseudo-terminal: %w", err)
	}
	if err := startOnPty(p, cmd); err != nil {
		p.Close()
		return nil, fmt.Errorf("start %s: %w", sh.Name, err)
	}
	b := &backend{pty: p, cmd: cmd, done: make(chan struct{})}
	b.code.Store(-1)
	go b.wait()
	return b, nil
}

// workingDir expands and validates options.cwd (default: the user's home directory).
func workingDir(cwd string) (string, error) {
	home, _ := os.UserHomeDir()
	switch {
	case cwd == "" || cwd == "~":
		cwd = home
	case strings.HasPrefix(cwd, "~/") || strings.HasPrefix(cwd, `~\`):
		cwd = filepath.Join(home, cwd[2:])
	}
	if cwd == "" {
		return "", nil
	}
	st, err := os.Stat(cwd)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("working directory %q does not exist", cwd)
	}
	return cwd, nil
}

// buildEnv inherits the Termstead process environment and applies terminal variables and options.env.
func buildEnv(o model.Options) []string {
	env := map[string]string{}
	var order []string
	set := func(k, v string) {
		key := k
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(k)
		}
		if _, ok := env[key]; !ok {
			order = append(order, k)
		}
		env[key] = k + "=" + v
	}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || strings.HasPrefix(k, "TERMSTEAD_") {
			continue
		}
		set(k, v)
	}
	set("TERM", o.String("term"))
	set("COLORTERM", "truecolor")
	set("TERM_PROGRAM", "Termstead")
	if runtime.GOOS != "windows" && os.Getenv("LANG") == "" && os.Getenv("LC_ALL") == "" && os.Getenv("LC_CTYPE") == "" {
		set("LANG", "en_US.UTF-8")
	}
	for k, v := range o.StringMap("env") {
		if k != "" && !strings.ContainsAny(k, "=\x00") && !strings.Contains(v, "\x00") {
			set(k, v)
		}
	}
	out := make([]string, 0, len(order))
	seen := map[string]bool{}
	for _, k := range order {
		key := k
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(k)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, env[key])
	}
	return out
}

// backend is a running local shell.
type backend struct {
	pty       xpty.Pty
	cmd       *exec.Cmd
	done      chan struct{}
	code      atomic.Int64
	closeOnce sync.Once
	ptyOnce   sync.Once
}

// drainGrace is how long output may still be read after the shell exited before the PTY is closed (background
// processes can keep the terminal open indefinitely).
const drainGrace = 500 * time.Millisecond

func (b *backend) wait() {
	err := xpty.WaitProcess(context.Background(), b.cmd)
	b.code.Store(int64(exitCode(b.cmd, err)))
	close(b.done)
	time.Sleep(drainGrace)
	b.closePty()
}

func (b *backend) closePty() {
	b.ptyOnce.Do(func() { _ = b.pty.Close() })
}

// Read returns shell output and io.EOF once the shell has exited.
func (b *backend) Read(p []byte) (int, error) {
	n, err := b.pty.Read(p)
	if err == nil {
		return n, nil
	}
	select {
	case <-b.done:
	case <-time.After(2 * time.Second):
	}
	return n, io.EOF
}

func (b *backend) Write(p []byte) (int, error) { return b.pty.Write(p) }

func (b *backend) Resize(cols, rows int) error { return b.pty.Resize(cols, rows) }

// Signal delivers INT, TERM, KILL, HUP, QUIT, ... to the terminal's foreground process group.
func (b *backend) Signal(name string) error { return signalPty(b.pty, b.cmd, name) }

func (b *backend) ExitCode() int { return int(b.code.Load()) }

// Close hangs up the shell (SIGHUP to its process group, SIGKILL after a grace period) and closes the PTY.
func (b *backend) Close() error {
	b.closeOnce.Do(func() {
		select {
		case <-b.done:
		default:
			hangup(b.cmd)
			go func() {
				select {
				case <-b.done:
				case <-time.After(3 * time.Second):
					if b.cmd.Process != nil {
						_ = b.cmd.Process.Kill()
					}
				}
			}()
		}
		b.closePty()
	})
	return nil
}
