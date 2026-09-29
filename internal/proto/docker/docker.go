// Package docker implements the "docker" terminal protocol and container management endpoints (PROTO-30,
// RESEARCH §3.16): a minimal hand-rolled Docker Engine API client over a unix socket, Windows named pipe, TCP, or an
// SSH-forwarded socket (options.viaConnectionId). It exposes an interactive exec shell and a follow-logs session as
// term backends, plus a container list and start/stop/restart actions.
//
// Endpoints:
//
//	GET  /api/docker/containers?host=&connectionId=&all=   list containers
//	POST /api/docker/containers/:id/start|stop|restart      lifecycle actions (host/connectionId query honoured)
//
// Local and TCP engines run as the NexTerm process (the socket is effectively root), so in server mode they are
// restricted to administrators; reaching an engine through the caller's own SSH connection is allowed for everyone.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/labstack/echo/v5"

	"github.com/nexterm/nexterm/internal/app"
	"github.com/nexterm/nexterm/internal/core"
	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/term"
)

// Mount registers the "docker" terminal protocol and the container-management endpoints.
func Mount(d *app.Deps, c *core.Core) error {
	m := &module{d: d, c: c}
	term.RegisterPolicy(string(model.ProtoDocker), func(_ context.Context, user *model.User, conn *model.Connection) error {
		return m.allowed(user, conn)
	})
	term.RegisterProtocol(string(model.ProtoDocker), func(ctx context.Context, req term.OpenRequest) (term.Backend, error) {
		return m.open(ctx, req)
	})
	api := d.Router.API()
	api.GET("/docker/containers", m.handleList)
	api.POST("/docker/containers/:id/start", m.handleAction("start"))
	api.POST("/docker/containers/:id/stop", m.handleAction("stop"))
	api.POST("/docker/containers/:id/restart", m.handleAction("restart"))
	return nil
}

type module struct {
	d *app.Deps
	c *core.Core
}

// allowed gates local/TCP engines to admins in server mode; SSH-reached engines use the caller's own credentials.
func (m *module) allowed(user *model.User, conn *model.Connection) error {
	if user == nil {
		return httpx.ErrUnauthorized
	}
	via := ""
	if conn != nil {
		via = strings.TrimSpace(conn.Options.String("viaConnectionId", ""))
	}
	if via != "" {
		return nil
	}
	if m.d.Cfg != nil && m.d.Cfg.IsServer() && !user.IsAdmin() {
		return httpx.Forbidden("local and TCP Docker engines are only available to administrators in server mode")
	}
	return nil
}

// resolveEngine builds an engine for the given docker host / SSH connection. The returned engine's close() must be
// called when done (it releases the SSH client for a remote engine).
func (m *module) resolveEngine(ctx context.Context, user *model.User, host, viaConnID string) (*engine, error) {
	if strings.TrimSpace(viaConnID) != "" {
		if m.c == nil || m.c.SSH == nil {
			return nil, errors.New("docker: SSH is not available")
		}
		cl, release, err := m.c.SSH.Get(ctx, user, viaConnID)
		if err != nil {
			return nil, fmt.Errorf("docker: SSH gateway: %w", err)
		}
		sockPath := strings.TrimSpace(host)
		if sockPath == "" || !strings.HasPrefix(sockPath, "unix://") {
			sockPath = "unix:///var/run/docker.sock"
		}
		path := strings.TrimPrefix(sockPath, "unix://")
		eng := newEngine(func(dctx context.Context) (net.Conn, error) {
			return cl.DialContext(dctx, "unix", path)
		})
		eng.closer = release
		return eng, nil
	}
	dial, _, err := newEngineDialer(host)
	if err != nil {
		return nil, err
	}
	return newEngine(dial), nil
}

// ---- REST ---------------------------------------------------------------------------------------------------------

func (m *module) handleList(c *echo.Context) error {
	user := httpx.UserFrom(c)
	host := c.QueryParam("host")
	via := c.QueryParam("connectionId")
	if err := m.allowed(user, &model.Connection{Options: model.Options{"viaConnectionId": via}}); err != nil {
		return err
	}
	eng, err := m.resolveEngine(c.Request().Context(), user, host, via)
	if err != nil {
		return httpx.BadRequest(err.Error())
	}
	defer eng.close()
	all := c.QueryParam("all") == "1" || c.QueryParam("all") == "true"
	list, err := eng.listContainers(c.Request().Context(), all)
	if err != nil {
		return httpx.BadRequest(err.Error())
	}
	return c.JSON(http.StatusOK, list)
}

func (m *module) handleAction(action string) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user := httpx.UserFrom(c)
		host := c.QueryParam("host")
		via := c.QueryParam("connectionId")
		if err := m.allowed(user, &model.Connection{Options: model.Options{"viaConnectionId": via}}); err != nil {
			return err
		}
		id := c.Param("id")
		if strings.TrimSpace(id) == "" {
			return httpx.BadRequest("container id is required")
		}
		eng, err := m.resolveEngine(c.Request().Context(), user, host, via)
		if err != nil {
			return httpx.BadRequest(err.Error())
		}
		defer eng.close()
		if err := eng.containerAction(c.Request().Context(), id, action); err != nil {
			return httpx.BadRequest(err.Error())
		}
		m.d.Audit.Log(c, "docker."+action, id, map[string]any{"host": host, "via": via})
		return httpx.OK(c)
	}
}

// ---- opener -------------------------------------------------------------------------------------------------------

func (m *module) open(ctx context.Context, req term.OpenRequest) (term.Backend, error) {
	conn := req.Connection
	if conn == nil {
		return nil, term.Permanent(errors.New("docker: missing connection"))
	}
	if err := m.allowed(req.User, conn); err != nil {
		return nil, term.Permanent(err)
	}
	o := conn.Options
	container := strings.TrimSpace(o.String("container", ""))
	if container == "" {
		return nil, term.Permanent(errors.New("docker: no container configured"))
	}
	via := strings.TrimSpace(o.String("viaConnectionId", ""))
	eng, err := m.resolveEngine(ctx, req.User, o.String("dockerHost", ""), via)
	if err != nil {
		if via != "" {
			return nil, err // SSH errors carry their own retry semantics (auth failures are permanent)
		}
		return nil, term.Permanent(err)
	}

	if strings.EqualFold(o.String("dockerMode", ""), "logs") {
		return newLogsBackend(ctx, eng, container, min(max(o.Int("logTail", 200), 0), 100000))
	}
	return newExecBackend(ctx, eng, req, container)
}

// ---- exec backend -------------------------------------------------------------------------------------------------

type execBackend struct {
	eng    *engine
	execID string
	conn   net.Conn
	rd     io.Reader

	closeOnce sync.Once
	done      chan struct{}
	code      atomic.Int64
}

func newExecBackend(ctx context.Context, eng *engine, req term.OpenRequest, container string) (term.Backend, error) {
	o := req.Connection.Options
	cmd := shellCommand(o.String("shell", ""))
	cfg := execConfig{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
		Cmd:          cmd,
		User:         strings.TrimSpace(o.String("user", "")),
		// docker exec does not set TERM; without it shells in minimal images fall back to a dumb terminal.
		Env: []string{"TERM=" + o.String("term", "xterm-256color")},
	}
	execID, err := eng.execCreate(ctx, container, cfg)
	if err != nil {
		eng.close()
		return nil, term.Permanent(fmt.Errorf("docker: create exec in %q: %w", container, err))
	}
	conn, br, err := eng.execStart(ctx, execID, true)
	if err != nil {
		eng.close()
		return nil, fmt.Errorf("docker: start exec: %w", err)
	}
	b := &execBackend{eng: eng, execID: execID, conn: conn, rd: br, done: make(chan struct{})}
	b.code.Store(-1)
	// Apply the initial size (the session applies later changes through Resize).
	if req.Session != nil {
		cols, rows := req.Session.Size()
		rctx, cancel := context.WithTimeout(ctx, apiTimeout)
		_ = eng.execResize(rctx, execID, rows, cols)
		cancel()
	}
	return b, nil
}

// shellCommand returns the exec command: the configured shell, or an auto-detecting bash→sh fallback.
func shellCommand(shell string) []string {
	shell = strings.TrimSpace(shell)
	switch {
	case shell == "":
		return []string{"/bin/sh", "-c", "command -v bash >/dev/null 2>&1 && exec bash || exec sh"}
	case strings.ContainsAny(shell, " "):
		// A command line: run it through sh -c.
		return []string{"/bin/sh", "-c", shell}
	default:
		return []string{shell}
	}
}

func (b *execBackend) Read(p []byte) (int, error) {
	n, err := b.rd.Read(p)
	if err == nil {
		return n, nil
	}
	select {
	case <-b.done:
		return n, io.EOF // closed by the session
	default:
	}
	if errors.Is(err, io.EOF) {
		// The process ended: fetch its exit status for the "Session ended (exit code N)" notice.
		ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
		if code := b.eng.execExitCode(ctx, b.execID); code >= 0 {
			b.code.Store(int64(code))
		}
		cancel()
		return n, io.EOF
	}
	return n, err
}

func (b *execBackend) Write(p []byte) (int, error) { return b.conn.Write(p) }

// Resize runs from the session's input loop, so it must not hang on a stuck engine.
func (b *execBackend) Resize(cols, rows int) error {
	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()
	return b.eng.execResize(ctx, b.execID, rows, cols)
}

func (b *execBackend) ExitCode() int { return int(b.code.Load()) }

func (b *execBackend) Close() error {
	b.closeOnce.Do(func() {
		close(b.done)
		if b.conn != nil {
			b.conn.Close()
		}
		b.eng.close()
	})
	return nil
}

// ---- logs backend -------------------------------------------------------------------------------------------------

type logsBackend struct {
	eng  *engine
	body io.ReadCloser
	rd   io.Reader
	name string

	closeOnce sync.Once
	done      chan struct{}
}

func newLogsBackend(ctx context.Context, eng *engine, container string, tail int) (term.Backend, error) {
	body, tty, err := eng.logs(ctx, container, tail)
	if err != nil {
		eng.close()
		return nil, term.Permanent(fmt.Errorf("docker: logs for %q: %w", container, err))
	}
	b := &logsBackend{eng: eng, body: body, name: container, done: make(chan struct{})}
	if tty {
		b.rd = body
	} else {
		b.rd = newDemuxReader(body)
	}
	return b, nil
}

func (b *logsBackend) Read(p []byte) (int, error) {
	n, err := b.rd.Read(p)
	if err == nil {
		return n, nil
	}
	select {
	case <-b.done:
		return n, io.EOF
	default:
	}
	if errors.Is(err, io.EOF) {
		return n, io.EOF // the container stopped: the log stream ended
	}
	return n, err // connection to the engine lost (autoReconnect may resume following)
}

// Write is ignored: a logs view is read-only.
func (b *logsBackend) Write(p []byte) (int, error) { return len(p), nil }

func (b *logsBackend) Resize(int, int) error { return nil }

func (b *logsBackend) Close() error {
	b.closeOnce.Do(func() {
		close(b.done)
		if b.body != nil {
			b.body.Close()
		}
		b.eng.close()
	})
	return nil
}
