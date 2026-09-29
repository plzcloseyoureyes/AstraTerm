package rdp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/nexterm/nexterm/internal/app"
	"github.com/nexterm/nexterm/internal/audit"
	"github.com/nexterm/nexterm/internal/auth"
	"github.com/nexterm/nexterm/internal/config"
	"github.com/nexterm/nexterm/internal/core"
	"github.com/nexterm/nexterm/internal/events"
	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/sshx"
	"github.com/nexterm/nexterm/internal/store"
	"github.com/nexterm/nexterm/internal/term"
	"github.com/nexterm/nexterm/internal/vault"
)

// A self-contained in-process server with the foundation services and this module only (the full servertest
// harness mounts every module, some of which are being built concurrently).

var fastOnce sync.Once

type testEnv struct {
	t    *testing.T
	auth *auth.Service
	cfg  *config.Config
	d    *app.Deps
	core *core.Core
	h    *handler
	srv  *httptest.Server
}

func testEnvEnabled() bool { return os.Getenv("NEXTERM_TESTENV") == "1" }

func newTestEnv(t *testing.T, mutate ...func(*config.Config)) *testEnv {
	t.Helper()
	fastOnce.Do(func() {
		auth.PasswordParams.Time, auth.PasswordParams.MemKiB, auth.PasswordParams.Threads = 1, 1024, 1
		vault.DefaultKDF.Time, vault.DefaultKDF.MemKiB, vault.DefaultKDF.Threads = 1, 1024, 1
	})
	cfg := &config.Config{
		Listen:             "127.0.0.1:0",
		DataDir:            t.TempDir(),
		Mode:               config.ModeDesktop,
		LogLevel:           "error",
		DetachedSessionTTL: config.DefaultDetachedTTL,
		ScrollbackBytes:    config.DefaultScrollbackBytes,
		Version:            "test",
	}
	for _, m := range mutate {
		m(cfg)
	}
	if err := config.EnsureDataDir(cfg.DataDir); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.OpenConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open(ctx, st, cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	var log *slog.Logger
	if testing.Verbose() && os.Getenv("RDP_TEST_LOG") == "1" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	} else {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	router := httpx.NewRouter(httpx.Options{Log: log, LoopbackOnly: true})
	hub := events.NewHub(ctx, log)
	d := &app.Deps{Ctx: ctx, Cfg: cfg, Log: log, Router: router, Store: st, Vault: v, Events: hub,
		Jobs: events.NewJobs(ctx, hub, log), Audit: audit.New(st, log)}
	authSvc, err := auth.Mount(d)
	if err != nil {
		t.Fatal(err)
	}
	router.WS("/ws/events", hub.ServeWS)
	audit.Mount(router, st)
	sessions := term.New(d)
	if err := term.Mount(d, sessions); err != nil {
		t.Fatal(err)
	}
	c := &core.Core{Sessions: sessions, SSH: sshx.New(d, sessions)}
	h, err := mount(d, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(router)
	env := &testEnv{t: t, auth: authSvc, cfg: cfg, d: d, core: c, h: h, srv: srv}
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		cancel()
		wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = sessions.Wait(wctx)
		wcancel()
		_ = st.Close()
	})
	return env
}

type testClient struct {
	env  *testEnv
	http *http.Client
	user *model.User
}

func (e *testEnv) client() *testClient {
	jar, _ := cookiejar.New(nil)
	return &testClient{env: e, http: &http.Client{Jar: jar, Timeout: 60 * time.Second}}
}

// setup creates the first admin and returns its logged-in client.
func (e *testEnv) setup() *testClient {
	c := e.client()
	var resp struct {
		User model.User `json:"user"`
	}
	body := map[string]string{"username": "admin", "password": "correct horse battery"}
	if tok := e.auth.SetupToken(); tok != "" {
		body["setupToken"] = tok
	}
	c.must("POST", "/api/auth/setup", body, &resp)
	c.user = &resp.User
	if c.user.ID == "" {
		var st struct {
			User *model.User `json:"user"`
		}
		c.must("GET", "/api/auth/state", nil, &st)
		c.user = st.User
	}
	return c
}

// createUser creates a regular user (through the admin API) and logs it in.
func (e *testEnv) createUser(admin *testClient, name string) *testClient {
	admin.must("POST", "/api/admin/users", map[string]any{"username": name, "password": "another good password", "role": "user"}, nil)
	c := e.client()
	var resp struct {
		User model.User `json:"user"`
	}
	c.must("POST", "/api/auth/login", map[string]any{"username": name, "password": "another good password"}, &resp)
	c.user = &resp.User
	return c
}

func (c *testClient) do(method, path string, body any) (int, []byte, http.Header) {
	c.env.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.env.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.env.srv.URL+path, rd)
	if err != nil {
		c.env.t.Fatal(err)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(httpx.CSRFHeader, httpx.CSRFHeaderValue)
	resp, err := c.http.Do(req)
	if err != nil {
		c.env.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, resp.Header
}

func (c *testClient) must(method, path string, body, out any) {
	c.env.t.Helper()
	st, data, _ := c.do(method, path, body)
	if st < 200 || st >= 300 {
		c.env.t.Fatalf("%s %s: %d %s", method, path, st, data)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			c.env.t.Fatalf("%s %s: decode %s: %v", method, path, data, err)
		}
	}
}

// errorCode performs a request and returns the status and the JSON error code.
func (c *testClient) errorCode(method, path string, body any) (int, string) {
	c.env.t.Helper()
	st, data, _ := c.do(method, path, body)
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(data, &e)
	return st, e.Code
}

// ws dials a WebSocket of the test server with the client's cookies.
func (c *testClient) ws(path string, subprotocols ...string) (*websocket.Conn, *http.Response, error) {
	url := strings.Replace(c.env.srv.URL, "http", "ws", 1) + path
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: c.http, Subprotocols: subprotocols})
	if err == nil {
		ws.SetReadLimit(64 << 20)
	}
	return ws, resp, err
}

// promptAnswerer answers prompts of a user's events socket.
type promptAnswerer struct {
	mu      sync.Mutex
	prompts []model.Prompt
}

func (p *promptAnswerer) seen() []model.Prompt {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]model.Prompt(nil), p.prompts...)
}

// answerPrompts connects the client's events socket and answers every prompt with fn.
func (c *testClient) answerPrompts(fn func(model.Prompt) model.PromptResponse) *promptAnswerer {
	c.env.t.Helper()
	ws, _, err := c.ws("/ws/events")
	if err != nil {
		c.env.t.Fatal(err)
	}
	c.env.t.Cleanup(func() { ws.CloseNow() })
	pa := &promptAnswerer{}
	ready := make(chan struct{})
	go func() {
		once := sync.Once{}
		for {
			_, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var ev struct {
				Type   string       `json:"type"`
				Prompt model.Prompt `json:"prompt"`
			}
			if json.Unmarshal(data, &ev) != nil {
				continue
			}
			if ev.Type == "hello" {
				once.Do(func() { close(ready) })
			}
			if ev.Type != "prompt" {
				continue
			}
			pa.mu.Lock()
			pa.prompts = append(pa.prompts, ev.Prompt)
			pa.mu.Unlock()
			r := fn(ev.Prompt)
			msg, _ := json.Marshal(map[string]any{"type": "prompt.response", "id": ev.Prompt.ID, "accept": r.Accept,
				"values": r.Values, "save": r.Save})
			_ = ws.Write(context.Background(), websocket.MessageText, msg)
		}
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		c.env.t.Fatal("events socket did not say hello")
	}
	return pa
}

// createConnection stores an RDP connection owned by owner (secrets sealed with the vault).
func (e *testEnv) createConnection(owner *model.User, host string, port int, username string, secrets map[string]string, opts model.Options) *model.Connection {
	e.t.Helper()
	c := &model.Connection{
		ID: model.NewID(), Name: "rdp " + host, Protocol: model.ProtoRDP, Host: host, Port: port, Username: username,
		AuthMethod: model.AuthPassword, Options: opts, OwnerID: owner.ID,
	}
	if len(secrets) > 0 {
		enc, err := e.d.Vault.SealJSON(secrets)
		if err != nil {
			e.t.Fatal(err)
		}
		c.SecretsEnc = enc
		c.SecretKeys = model.SortedKeys(secrets)
	}
	c.Normalize()
	if err := e.d.Store.Connections.Create(context.Background(), c); err != nil {
		e.t.Fatal(err)
	}
	return c
}

// openSession creates an RDP runtime session for a saved connection.
func (c *testClient) openSession(connID string) model.RuntimeSession {
	c.env.t.Helper()
	var rs model.RuntimeSession
	c.must("POST", "/api/sessions", map[string]any{"connectionId": connID, "cols": 80, "rows": 24}, &rs)
	if rs.Kind != model.KindRDP {
		c.env.t.Fatalf("session kind %s", rs.Kind)
	}
	return rs
}

// waitFor polls cond until it holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *testEnv) sessionState(id string) (model.SessionState, string) {
	s := e.core.Sessions.Get(id)
	if s == nil {
		return model.StateClosed, ""
	}
	return s.State()
}
