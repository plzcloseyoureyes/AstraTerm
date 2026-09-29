package keys

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
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

// A self-contained test environment: the foundation services (store, vault, router, auth, events, term, sshx) plus
// this module — without the other feature modules, so these tests do not depend on their state.

var fastCrypto sync.Once

type testEnv struct {
	t     *testing.T
	d     *app.Deps
	core  *core.Core
	auth  *auth.Service
	h     *handler
	http  *httptest.Server
	admin *model.User
	c     *tclient // logged in as admin
}

func newTestEnv(t *testing.T, mode string) *testEnv {
	t.Helper()
	fastCrypto.Do(func() {
		auth.PasswordParams.Time, auth.PasswordParams.MemKiB, auth.PasswordParams.Threads = 1, 1024, 1
		vault.DefaultKDF.Time, vault.DefaultKDF.MemKiB, vault.DefaultKDF.Threads = 1, 1024, 1
	})
	dir := t.TempDir()
	cfg := &config.Config{Listen: "127.0.0.1:0", DataDir: dir, Mode: mode, LogLevel: "error",
		DetachedSessionTTL: time.Hour, ScrollbackBytes: 1 << 20, Version: "test"}
	if err := config.EnsureDataDir(dir); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.OpenConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(testLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
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
	sessions := term.New(d)
	pool := sshx.New(d, sessions)
	pool.IdleTTL = 200 * time.Millisecond
	c := &core.Core{Sessions: sessions, SSH: pool}
	if err := Mount(d, c); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(router)
	env := &testEnv{t: t, d: d, core: c, auth: authSvc, http: hs}
	t.Cleanup(func() {
		hs.CloseClientConnections()
		hs.Close()
		cancel()
		wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = sessions.Wait(wctx)
		wcancel()
		time.Sleep(20 * time.Millisecond) // let the agent service stop
		st.Close()
	})
	body := map[string]string{"username": "admin", "password": "correct horse battery staple"}
	if tok := authSvc.SetupToken(); tok != "" {
		body["setupToken"] = tok
	}
	env.c = env.client()
	env.c.must("POST", "/api/auth/setup", body, nil)
	if env.admin, err = st.Users.GetByUsername(ctx, "admin"); err != nil {
		t.Fatal(err)
	}
	env.h = env.handler()
	return env
}

// handler returns the module's handler of this environment (white-box assertions).
func (e *testEnv) handler() *handler {
	h, ok := mounted.Load(e.d)
	if !ok {
		e.t.Fatal("keys module not mounted")
	}
	return h.(*handler)
}

type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// user creates a non-admin user and returns a logged-in client.
func (e *testEnv) user(name string) (*tclient, *model.User) {
	e.t.Helper()
	e.c.must("POST", "/api/admin/users", map[string]string{"username": name, "password": "another good password", "role": "user"}, nil)
	c := e.client()
	c.must("POST", "/api/auth/login", map[string]string{"username": name, "password": "another good password"}, nil)
	u, err := e.d.Store.Users.GetByUsername(context.Background(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return c, u
}

type tclient struct {
	e  *testEnv
	hc *http.Client
}

func (e *testEnv) client() *tclient {
	jar, _ := cookiejar.New(nil)
	return &tclient{e: e, hc: &http.Client{Jar: jar, Timeout: 60 * time.Second}}
}

func (c *tclient) do(method, path string, body any) (int, []byte, http.Header) {
	c.e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.e.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.e.http.URL+path, rd)
	if err != nil {
		c.e.t.Fatal(err)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(httpx.CSRFHeader, httpx.CSRFHeaderValue)
	resp, err := c.hc.Do(req)
	if err != nil {
		c.e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, resp.Header
}

// json performs a request, decoding a 2xx body into out; it returns the status.
func (c *tclient) json(method, path string, body, out any) int {
	c.e.t.Helper()
	st, data, _ := c.do(method, path, body)
	if out != nil && st >= 200 && st < 300 && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			c.e.t.Fatalf("%s %s: decode %s: %v", method, path, data, err)
		}
	}
	return st
}

func (c *tclient) must(method, path string, body, out any) {
	c.e.t.Helper()
	st, data, _ := c.do(method, path, body)
	if st < 200 || st >= 300 {
		c.e.t.Fatalf("%s %s: %d %s", method, path, st, data)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			c.e.t.Fatalf("%s %s: decode %s: %v", method, path, data, err)
		}
	}
}

// code performs a request and returns the status and the error code.
func (c *tclient) code(method, path string, body any) (int, string) {
	c.e.t.Helper()
	st, data, _ := c.do(method, path, body)
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(data, &e)
	return st, e.Code
}

// prompts connects the client's events socket and answers prompt-broker questions with answer.
type prompter struct {
	mu     sync.Mutex
	seen   []model.Prompt
	answer func(model.Prompt) model.PromptResponse
}

func (p *prompter) set(fn func(model.Prompt) model.PromptResponse) {
	p.mu.Lock()
	p.answer, p.seen = fn, nil
	p.mu.Unlock()
}

func (p *prompter) prompts() []model.Prompt {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]model.Prompt(nil), p.seen...)
}

func (c *tclient) prompter(t *testing.T) *prompter {
	t.Helper()
	p := &prompter{answer: func(model.Prompt) model.PromptResponse { return model.PromptResponse{} }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := strings.Replace(c.e.http.URL, "http", "ws", 1) + "/ws/events"
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: c.hc})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	hello := make(chan struct{})
	go func() {
		var once sync.Once
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
			switch ev.Type {
			case "hello":
				once.Do(func() { close(hello) })
			case "prompt":
				p.mu.Lock()
				p.seen = append(p.seen, ev.Prompt)
				fn := p.answer
				p.mu.Unlock()
				r := fn(ev.Prompt)
				b, _ := json.Marshal(map[string]any{"type": "prompt.response", "id": ev.Prompt.ID, "accept": r.Accept,
					"values": r.Values, "save": r.Save})
				_ = ws.Write(context.Background(), websocket.MessageText, b)
			}
		}
	}()
	select {
	case <-hello:
	case <-time.After(5 * time.Second):
		t.Fatal("no hello on the events socket")
	}
	return p
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
