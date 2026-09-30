package webproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/sshx"
)

// Xpra (PROTO-20): an X11 application started on the remote host under `xpra start --bind-ws=127.0.0.1:<port>
// --html=on`, reached through the SSH connection and shown in a web tab through xpra's own HTML5 client.

const (
	xpraReadyTimeout = 45 * time.Second
	xpraOutputTail   = 16 << 10
)

type xpraTarget struct {
	ConnectionID string `json:"connectionId,omitempty"`
	SessionID    string `json:"sessionId,omitempty"`
}

// XpraCheck is the answer of GET /api/xpra/check.
type XpraCheck struct {
	Installed bool   `json:"installed"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
	HTML5     bool   `json:"html5"`
	Message   string `json:"message,omitempty"`
}

// xpraProbe finds xpra and its HTML5 client on the remote host (login-shell independent: runs under sh).
const xpraProbe = `sh -c 'x=$(command -v xpra 2>/dev/null); if [ -z "$x" ]; then for p in /usr/bin/xpra /usr/local/bin/xpra /opt/homebrew/bin/xpra /snap/bin/xpra; do [ -x "$p" ] && x=$p && break; done; fi; [ -n "$x" ] || exit 3; echo "path=$x"; echo "version=$("$x" --version 2>/dev/null | head -n 1)"; for d in /usr/share/xpra/www /usr/local/share/xpra/www /opt/homebrew/share/xpra/www /usr/share/xpra-html5 /usr/local/share/xpra-html5; do [ -f "$d/index.html" ] && echo "html5=$d" && break; done; exit 0'`

// sshFor returns the SSH client of a saved connection or live session of user (release when done).
func (s *Service) sshFor(ctx context.Context, user *model.User, t xpraTarget) (*sshx.Client, func(), string, error) {
	pool := s.pool()
	if pool == nil {
		return nil, nil, "", httpx.Conflict("SSH is not available")
	}
	switch {
	case t.SessionID != "" && t.ConnectionID != "":
		return nil, nil, "", httpx.BadRequest("give connectionId or sessionId, not both")
	case t.SessionID != "":
		meta := s.sessionInfo(t.SessionID, user)
		if meta == nil {
			return nil, nil, "", httpx.NotFound("SSH session not found")
		}
		c, rel, err := pool.ForSession(ctx, user, t.SessionID)
		if err != nil {
			return nil, nil, "", httpx.Conflict("the SSH session is not connected")
		}
		return c, rel, meta.Title, nil
	case t.ConnectionID != "":
		conn, _, err := s.d.ResolveConnection(ctx, user, t.ConnectionID)
		if err != nil {
			return nil, nil, "", err
		}
		c, rel, err := pool.Get(ctx, user, t.ConnectionID)
		if err != nil {
			return nil, nil, "", sshError(err)
		}
		return c, rel, conn.Name, nil
	}
	return nil, nil, "", httpx.BadRequest("choose an SSH connection or session")
}

// sshError keeps typed errors and turns connection failures into a 422 with their message.
func sshError(err error) error {
	if _, ok := errors.AsType[*httpx.HTTPError](err); ok {
		return err
	}
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		return err
	}
	return httpx.NewError(http.StatusUnprocessableEntity, "connect_failed", err.Error())
}

func (s *Service) xpraCheck(ctx context.Context, c *sshx.Client) (XpraCheck, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, _, code, err := c.Exec(ctx, xpraProbe)
	if err != nil {
		return XpraCheck{}, httpx.NewError(http.StatusUnprocessableEntity, "exec_failed", "cannot run commands on the host: "+err.Error())
	}
	var res XpraCheck
	if code == 3 {
		res.Message = "Xpra is not installed on this host. Install it (for example `sudo apt install xpra xpra-html5`, " +
			"or the packages from xpra.org) to run X11 applications in the browser."
		return res, nil
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "path":
			res.Path, res.Installed = v, v != ""
		case "version":
			res.Version = strings.TrimSpace(strings.TrimPrefix(v, "xpra "))
		case "html5":
			res.HTML5 = v != ""
		}
	}
	if !res.Installed {
		res.Message = "Xpra could not be found on this host."
	} else if !res.HTML5 {
		res.Message = "Xpra is installed but its HTML5 client was not found (package xpra-html5 on Debian / Ubuntu)."
	}
	return res, nil
}

func (s *Service) handleXpraCheck(c *echo.Context) error {
	t := xpraTarget{ConnectionID: c.QueryParam("connectionId"), SessionID: c.QueryParam("sessionId")}
	cl, rel, _, err := s.sshFor(c.Request().Context(), httpx.UserFrom(c), t)
	if err != nil {
		return err
	}
	defer rel()
	res, err := s.xpraCheck(c.Request().Context(), cl)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, res)
}

type xpraStartRequest struct {
	xpraTarget
	Command string `json:"command"`
	Mode    string `json:"mode"` // seamless | desktop
	Title   string `json:"title,omitempty"`
}

func validCommand(cmd string) error {
	if strings.TrimSpace(cmd) == "" {
		return httpx.BadRequest("enter the command of the application to start (e.g. xterm)")
	}
	if len(cmd) > 2048 {
		return httpx.BadRequest("command is too long")
	}
	if strings.ContainsAny(cmd, "\x00\r\n") {
		return httpx.BadRequest("command must be a single line")
	}
	return nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// xpraScript runs xpra in the foreground under a stdin watchdog: when AstraTerm closes the channel (proxy closed, tab
// closed, shutdown), stdin reaches EOF and xpra is terminated (non-PTY execs get no SIGHUP).
func xpraScript(bin, mode, cmd string, port int) string {
	sub := "start"
	if mode == "desktop" {
		sub = "start-desktop"
	}
	args := []string{shellQuote(bin), sub,
		"--bind-ws=127.0.0.1:" + strconv.Itoa(port), "--html=on", "--daemon=no",
		"--start-child=" + shellQuote(cmd), "--exit-with-children=yes", "--terminate-children=yes",
		"--mdns=no", "--webcam=no", "--printing=no", "--notifications=no", "--systemd-run=no",
	}
	// Background jobs of a non-interactive shell read /dev/null unless redirected explicitly, so the channel's stdin
	// is handed to the watchdog through fd 3.
	inner := `exec 3<&0; ` + strings.Join(args, " ") + ` </dev/null & p=$!; ` +
		`(cat <&3 >/dev/null 2>&1; kill $p 2>/dev/null; sleep 5; kill -9 $p 2>/dev/null) >/dev/null 2>&1 & exec 3<&-; wait $p`
	return "sh -c " + shellQuote(inner)
}

// tailBuffer keeps the last n bytes written.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	n   int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.n {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.n:]...)
	}
	t.mu.Unlock()
	return len(p), nil
}

// lastLines returns up to n non-empty trailing lines.
func (t *tailBuffer) lastLines(n int) string {
	t.mu.Lock()
	s := string(t.buf)
	t.mu.Unlock()
	var out []string
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			out = append([]string{l}, out...)
		}
	}
	return strings.Join(out, "\n")
}

func (s *Service) handleXpraStart(c *echo.Context) error {
	var req xpraStartRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if err := validCommand(req.Command); err != nil {
		return err
	}
	switch req.Mode {
	case "":
		req.Mode = "seamless"
	case "seamless", "desktop":
	default:
		return httpx.BadRequest("mode must be seamless or desktop")
	}
	ctx := c.Request().Context()
	user := httpx.UserFrom(c)
	cl, rel, _, err := s.sshFor(ctx, user, req.xpraTarget)
	if err != nil {
		return err
	}
	chk, err := s.xpraCheck(ctx, cl)
	if err != nil {
		rel()
		return err
	}
	if !chk.Installed {
		rel()
		return httpx.NewError(http.StatusUnprocessableEntity, "xpra_not_installed", chk.Message)
	}
	if !chk.HTML5 {
		rel()
		return httpx.NewError(http.StatusUnprocessableEntity, "xpra_html5_missing", chk.Message)
	}
	run, err := s.startXpra(ctx, cl, chk.Path, req.Mode, req.Command)
	if err != nil {
		rel()
		return err
	}
	title := req.Title
	if title == "" {
		title = firstWord(req.Command)
	}
	sp := Spec{ConnectionID: req.ConnectionID, SessionID: req.SessionID, Scheme: "http", Host: "127.0.0.1",
		Port: run.port, Path: "/", Title: title}
	p, err := s.open(ctx, user, "xpra", sp, false)
	if err != nil {
		run.stop()
		rel()
		return err
	}
	p.setExtra("command", req.Command)
	p.setExtra("mode", req.Mode)
	if chk.Version != "" {
		p.setExtra("xpraVersion", chk.Version)
	}
	p.addCloser(func() {
		run.stop()
		rel()
	})
	go func() {
		<-run.done
		if !p.isClosed() {
			reason := "the application exited"
			if tail := run.out.lastLines(3); run.exitCode != 0 && tail != "" {
				reason += ": " + tail
			}
			s.remove(p, reason)
		}
	}()
	info, err := s.entryInfo(c, p, "/", "")
	if err != nil {
		s.remove(p, "no usable proxy mode")
		return err
	}
	s.d.Audit.Log(c, "xpra.start", p.id, map[string]any{"command": req.Command, "mode": req.Mode,
		"connectionId": req.ConnectionID, "sessionId": req.SessionID, "port": run.port})
	return c.JSON(http.StatusCreated, info)
}

func firstWord(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return "X11 app"
	}
	w := f[0]
	if i := strings.LastIndexByte(w, '/'); i >= 0 {
		w = w[i+1:]
	}
	return w
}

// xpraRun is a running xpra server.
type xpraRun struct {
	port     int
	sess     *ssh.Session
	stdin    io.WriteCloser
	release  func()
	out      *tailBuffer
	done     chan struct{}
	exitCode int
	stopOnce sync.Once
}

func (r *xpraRun) stop() {
	r.stopOnce.Do(func() {
		r.stdin.Close() // watchdog: EOF → kill xpra
		select {
		case <-r.done:
		case <-time.After(3 * time.Second):
		}
		r.sess.Close()
		r.release()
	})
}

// startXpra picks a free loopback port on the host, starts xpra and waits until its WebSocket/HTTP port accepts
// connections.
func (s *Service) startXpra(ctx context.Context, cl *sshx.Client, bin, mode, cmd string) (*xpraRun, error) {
	port := 0
	for i := 0; i < 8 && port == 0; i++ {
		cand := 20000 + rand.IntN(40000)
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		c, err := cl.DialContext(dctx, "tcp", "127.0.0.1:"+strconv.Itoa(cand))
		cancel()
		if err != nil {
			port = cand // nothing listens there
		} else {
			c.Close()
		}
	}
	if port == 0 {
		return nil, httpx.NewError(http.StatusUnprocessableEntity, "xpra_failed", "no free port found on the host")
	}
	sess, _, rel, err := cl.NewSessionContext(ctx)
	if err != nil {
		return nil, httpx.NewError(http.StatusUnprocessableEntity, "exec_failed", "cannot open an SSH channel: "+err.Error())
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		rel()
		return nil, err
	}
	run := &xpraRun{port: port, sess: sess, stdin: stdin, release: rel, out: &tailBuffer{n: xpraOutputTail}, done: make(chan struct{})}
	sess.Stdout, sess.Stderr = run.out, run.out
	if err := sess.Start(xpraScript(bin, mode, cmd, port)); err != nil {
		sess.Close()
		rel()
		return nil, httpx.NewError(http.StatusUnprocessableEntity, "xpra_failed", "cannot start xpra: "+err.Error())
	}
	go func() {
		err := sess.Wait()
		var ee *ssh.ExitError
		switch {
		case err == nil:
		case errors.As(err, &ee):
			run.exitCode = ee.ExitStatus()
		default:
			run.exitCode = -1
		}
		close(run.done)
	}()

	deadline := time.Now().Add(xpraReadyTimeout)
	for {
		select {
		case <-run.done:
			tail := run.out.lastLines(6)
			if tail == "" {
				tail = fmt.Sprintf("exit status %d", run.exitCode)
			}
			run.stop()
			return nil, httpx.NewError(http.StatusUnprocessableEntity, "xpra_failed", "xpra stopped before it was ready:\n"+tail)
		case <-ctx.Done():
			run.stop()
			return nil, ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
		dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		c, err := cl.DialContext(dctx, "tcp", "127.0.0.1:"+strconv.Itoa(port))
		cancel()
		if err == nil {
			c.Close()
			return run, nil
		}
		if time.Now().After(deadline) {
			tail := run.out.lastLines(6)
			run.stop()
			msg := "xpra did not start within " + xpraReadyTimeout.String()
			if tail != "" {
				msg += ":\n" + tail
			}
			return nil, httpx.NewError(http.StatusUnprocessableEntity, "xpra_failed", msg)
		}
	}
}
