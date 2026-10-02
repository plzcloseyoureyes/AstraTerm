package vnc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/audit"
	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/core"
	"github.com/plzcloseyoureyes/astraterm/internal/events"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/sshx"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
	"github.com/plzcloseyoureyes/astraterm/internal/vault"
	"github.com/plzcloseyoureyes/astraterm/internal/vnc"
)

// harness is a minimal AstraTerm server built from the foundation packages only (store, vault, events, audit, term,
// sshx) plus this module, so the tests do not depend on other feature modules. Requests authenticate with the
// X-Test-User header (user ID) as Bearer-like token logins (no CSRF header needed).
type harness struct {
	t    *testing.T
	d    *app.Deps
	core *core.Core
	http *httptest.Server

	mu    sync.Mutex
	users map[string]*model.User
}

func newHarness(t *testing.T, mode string) *harness {
	t.Helper()
	// Own temp dir (not t.TempDir): hijacked WebSocket handlers are not awaited by httptest.Server.Close, so a late
	// write may race with the removal; retry instead of failing the test.
	dataDir, err := os.MkdirTemp("", "astraterm-vnc-test-")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Listen:             "127.0.0.1:0",
		DataDir:            dataDir,
		Mode:               mode,
		LogLevel:           "error",
		DetachedSessionTTL: config.DefaultDetachedTTL,
		ScrollbackBytes:    config.DefaultScrollbackBytes,
		Version:            "test",
	}
	if err := config.EnsureDataDir(cfg.DataDir); err != nil {
		t.Fatal(err)
	}
	var log *slog.Logger
	if testing.Verbose() && os.Getenv("ASTRATERM_TEST_LOG") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	} else {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
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
	router := httpx.NewRouter(httpx.Options{Log: log, LoopbackOnly: true})
	hub := events.NewHub(ctx, log)
	d := &app.Deps{Ctx: ctx, Cfg: cfg, Log: log, Router: router, Store: st, Vault: v, Events: hub,
		Jobs: events.NewJobs(ctx, hub, log), Audit: audit.New(st, log)}
	h := &harness{t: t, d: d, users: map[string]*model.User{}}
	router.SetAuthenticator(httpx.AuthenticatorFunc(func(c *echo.Context) (*model.User, httpx.AuthInfo, error) {
		h.mu.Lock()
		u := h.users[c.Request().Header.Get("X-Test-User")]
		h.mu.Unlock()
		if u == nil {
			return nil, httpx.AuthInfo{}, nil
		}
		cp := *u
		return &cp, httpx.AuthInfo{Method: httpx.AuthToken, TokenID: "test"}, nil
	}))
	router.WS("/ws/events", hub.ServeWS)
	audit.Mount(router, st)
	sessions := term.New(d)
	h.core = &core.Core{Sessions: sessions, SSH: sshx.New(d, sessions)}
	if err := term.Mount(d, sessions); err != nil {
		t.Fatal(err)
	}
	if err := vnc.Mount(d, h.core); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	h.d = d
	h.http = httptest.NewServer(router)
	t.Cleanup(func() {
		h.http.CloseClientConnections()
		h.http.Close()
		cancel()
		wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer wcancel()
		_ = sessions.Wait(wctx)
		time.Sleep(100 * time.Millisecond) // let viewers finish their last store writes
		st.Close()
		for range 20 {
			if err := os.RemoveAll(dataDir); err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Logf("could not remove %s", dataDir)
	})
	return h
}

// user creates a user and returns a client acting as it.
func (h *harness) user(name string, role model.Role) *client {
	h.t.Helper()
	u := &model.User{Username: name, DisplayName: name, Role: role}
	if err := h.d.Store.Users.Create(context.Background(), u, "x"); err != nil {
		h.t.Fatal(err)
	}
	h.mu.Lock()
	h.users[u.ID] = u
	h.mu.Unlock()
	return &client{h: h, user: u}
}

// connection stores a saved connection (with vault-encrypted secrets) owned by c's user.
func (h *harness) connection(c *client, conn *model.Connection, secrets map[string]string) string {
	h.t.Helper()
	conn.OwnerID = c.user.ID
	if conn.Protocol == "" {
		conn.Protocol = model.ProtoVNC
	}
	if len(secrets) > 0 {
		enc, err := h.d.Vault.SealJSON(secrets)
		if err != nil {
			h.t.Fatal(err)
		}
		conn.SecretsEnc = enc
		for k := range secrets {
			conn.SecretKeys = append(conn.SecretKeys, k)
		}
	}
	conn.Normalize()
	if err := h.d.Store.Connections.Create(context.Background(), conn); err != nil {
		h.t.Fatal(err)
	}
	return conn.ID
}

func (h *harness) wsURL(path string) string {
	return strings.Replace(h.http.URL, "http", "ws", 1) + path
}

type client struct {
	h    *harness
	user *model.User
}

func (c *client) header() http.Header {
	return http.Header{"X-Test-User": []string{c.user.ID}}
}

// JSON performs a request, decodes a 2xx body into out and returns the status (and the error code of 4xx/5xx).
func (c *client) JSON(method, path string, body, out any) (int, string) {
	c.h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.h.http.URL+path, rd)
	req.Header = c.header()
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(data, &e)
		return resp.StatusCode, e.Code
	}
	if out != nil && len(data) > 0 {
		// A zeroed target: decoding into a value from an earlier call would keep the fields this response omits.
		reflect.ValueOf(out).Elem().SetZero()
		if err := json.Unmarshal(data, out); err != nil {
			c.h.t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	return resp.StatusCode, ""
}

func (c *client) MustJSON(method, path string, body, out any) {
	c.h.t.Helper()
	if st, code := c.JSON(method, path, body, out); st >= 300 {
		c.h.t.Fatalf("%s %s: %d %s", method, path, st, code)
	}
}
