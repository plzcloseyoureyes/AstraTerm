package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/audit"
	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/core"
	"github.com/plzcloseyoureyes/astraterm/internal/events"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/proto/local"
	"github.com/plzcloseyoureyes/astraterm/internal/sshx"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
	"github.com/plzcloseyoureyes/astraterm/internal/vault"
)

// harness is an in-process AstraTerm built from the foundation packages only (so the tests do not depend on other
// feature modules): store, vault, events, jobs, audit, router, term + sshx + local shells and this module. Requests
// are authenticated by an X-Test-User header carrying a user ID.
type harness struct {
	t    *testing.T
	d    *app.Deps
	core *core.Core
	m    *Module
	srv  *httptest.Server
}

func newHarness(t *testing.T, mutate ...func(*config.Config)) *harness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell tests")
	}
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	cfg := &config.Config{Listen: "127.0.0.1:0", DataDir: dir, Mode: config.ModeDesktop, DetachedSessionTTL: time.Hour,
		ScrollbackBytes: config.DefaultScrollbackBytes, Version: "test"}
	for _, f := range mutate {
		f(cfg)
	}
	if err := config.EnsureDataDir(dir); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(dir, "astraterm.db"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := httpx.NewRouter(httpx.Options{Log: log})
	router.SetAuthenticator(httpx.AuthenticatorFunc(func(c *echo.Context) (*model.User, httpx.AuthInfo, error) {
		id := c.Request().Header.Get("X-Test-User")
		if id == "" {
			return nil, httpx.AuthInfo{}, nil
		}
		u, err := st.Users.Get(c.Request().Context(), id)
		if err != nil {
			return nil, httpx.AuthInfo{}, nil
		}
		return u, httpx.AuthInfo{Method: httpx.AuthCookie}, nil
	}))
	hub := events.NewHub(ctx, log)
	d := &app.Deps{Ctx: ctx, Cfg: cfg, Log: log, Router: router, Store: st, Vault: v, Events: hub,
		Jobs: events.NewJobs(ctx, hub, log), Audit: audit.New(st, log)}
	router.WS("/ws/events", hub.ServeWS)
	router.API().POST("/jobs/:id/cancel", func(c *echo.Context) error {
		if err := d.Jobs.Cancel(httpx.UserFrom(c), c.Param("id")); err != nil {
			return err
		}
		return httpx.OK(c)
	})
	sessions := term.New(d)
	if err := term.Mount(d, sessions); err != nil {
		t.Fatal(err)
	}
	pool := sshx.New(d, sessions)
	if err := sshx.Mount(d, pool); err != nil {
		t.Fatal(err)
	}
	cc := &core.Core{Sessions: sessions, SSH: pool}
	if err := local.Mount(d, cc); err != nil {
		t.Fatal(err)
	}
	m, err := newModule(d, cc)
	if err != nil {
		t.Fatal(err)
	}
	m.routes()
	m.start()
	srv := httptest.NewServer(router)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		cancel()
		wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = sessions.Wait(wctx)
		wcancel()
		st.Close()
	})
	return &harness{t: t, d: d, core: cc, m: m, srv: srv}
}

func (h *harness) user(name string, role model.Role) *model.User {
	h.t.Helper()
	u := &model.User{Username: name, DisplayName: name, Role: role}
	if err := h.d.Store.Users.Create(context.Background(), u, "x"); err != nil {
		h.t.Fatal(err)
	}
	return u
}

// do performs a JSON request as user and decodes a 2xx response into out; it returns the status and raw body.
func (h *harness) do(u *model.User, method, path string, body, out any) (int, []byte) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(httpx.CSRFHeader, httpx.CSRFHeaderValue)
	if u != nil {
		req.Header.Set("X-Test-User", u.ID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode/100 == 2 && len(data) > 0 {
		// A zeroed target: decoding into a value from an earlier call would keep the fields this response omits.
		reflect.ValueOf(out).Elem().SetZero()
		if err := json.Unmarshal(data, out); err != nil {
			h.t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	return resp.StatusCode, data
}

func (h *harness) must(u *model.User, method, path string, body, out any) {
	h.t.Helper()
	if st, data := h.do(u, method, path, body, out); st/100 != 2 {
		h.t.Fatalf("%s %s: status %d: %s", method, path, st, data)
	}
}

func (h *harness) code(u *model.User, method, path string, body any) (int, string) {
	h.t.Helper()
	st, data := h.do(u, method, path, body, nil)
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(data, &e)
	return st, e.Code
}

var shellOpts = map[string]any{"shell": "/bin/sh", "loginShell": false, "env": map[string]any{"PS1": "nx$ "}}

// quickShell opens a local /bin/sh session (prompt "nx$ ") and waits until it shows its prompt.
func (h *harness) quickShell(u *model.User, extra map[string]any) model.RuntimeSession {
	h.t.Helper()
	quick := map[string]any{"protocol": "local", "options": shellOpts}
	maps.Copy(quick, extra)
	var rs model.RuntimeSession
	h.must(u, "POST", "/api/sessions", map[string]any{"quick": quick, "cols": 120, "rows": 30}, &rs)
	h.waitOutput(u, rs.ID, "nx$")
	return rs
}

func (h *harness) savedShell(u *model.User, name string, options map[string]any, secrets map[string]string) *model.Connection {
	h.t.Helper()
	opts := model.Options{}
	maps.Copy(opts, shellOpts)
	maps.Copy(opts, options)
	c := &model.Connection{OwnerID: u.ID, Name: name, Protocol: model.ProtoLocal, Options: opts, Tags: []string{}}
	if len(secrets) > 0 {
		enc, err := h.d.Vault.SealJSON(secrets)
		if err != nil {
			h.t.Fatal(err)
		}
		c.SecretsEnc, c.SecretKeys = enc, model.SortedKeys(secrets)
	}
	c.Normalize()
	if err := h.d.Store.Connections.Create(context.Background(), c); err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) scrollback(u *model.User, id string) string { return h.scrollbackAs(u, id, "0") }

// scrollbackAs reads the session's output as text (raw "0") or with its escape sequences (raw "1").
func (h *harness) scrollbackAs(u *model.User, id, raw string) string {
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/sessions/"+id+"/scrollback?raw="+raw, nil)
	req.Header.Set("X-Test-User", u.ID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func (h *harness) waitOutput(u *model.User, id, want string) string {
	h.t.Helper()
	var sb string
	waitFor(h.t, "output "+want, 15*time.Second, func() bool {
		sb = h.scrollback(u, id)
		return strings.Contains(sb, want)
	})
	return sb
}

func (h *harness) input(u *model.User, id, data string) {
	h.t.Helper()
	h.must(u, "POST", "/api/sessions/"+id+"/input", map[string]string{"data": data}, nil)
}

func (h *harness) waitRun(u *model.User, id string) *Run {
	h.t.Helper()
	var run Run
	waitFor(h.t, "run "+id, 30*time.Second, func() bool {
		h.must(u, "GET", "/api/automation/runs/"+id, nil, &run)
		return run.Status != StatusRunning
	})
	return &run
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// eventLog collects the events of one user's /ws/events socket.
type eventLog struct {
	mu     sync.Mutex
	events []map[string]any
}

func (h *harness) listen(u *model.User) *eventLog {
	h.t.Helper()
	url := strings.Replace(h.srv.URL, "http", "ws", 1) + "/ws/events"
	ws, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{HTTPHeader: http.Header{"X-Test-User": {u.ID}}})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { ws.CloseNow() })
	el := &eventLog{}
	go func() {
		for {
			_, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var ev map[string]any
			if json.Unmarshal(data, &ev) == nil {
				el.mu.Lock()
				el.events = append(el.events, ev)
				el.mu.Unlock()
			}
		}
	}()
	waitFor(h.t, "events hello", 5*time.Second, func() bool { return el.find(func(ev map[string]any) bool { return ev["type"] == "hello" }) != nil })
	return el
}

func (el *eventLog) find(pred func(map[string]any) bool) map[string]any {
	el.mu.Lock()
	defer el.mu.Unlock()
	for _, ev := range el.events {
		if pred(ev) {
			return ev
		}
	}
	return nil
}

func storeAuditFilter(action string) store.AuditFilter {
	return store.AuditFilter{Action: action, Limit: 10}
}
