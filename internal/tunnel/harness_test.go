package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"golang.org/x/crypto/ssh"

	"github.com/nexterm/nexterm/internal/app"
	"github.com/nexterm/nexterm/internal/audit"
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

// harness is a minimal NexTerm backend (store, vault, events, router with a header-based test authenticator, term
// manager, SSH pool) with the tunnel module mounted.
type harness struct {
	t        *testing.T
	ctx      context.Context
	cancel   context.CancelFunc
	cfg      *config.Config
	d        *app.Deps
	m        *Manager
	pool     *sshx.Pool
	sessions *term.Manager
	api      *httptest.Server

	mu    sync.Mutex
	users map[string]*model.User
}

func newHarness(t *testing.T, mutate ...func(*config.Config)) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	cfg := &config.Config{Mode: config.ModeDesktop, Listen: "127.0.0.1:7822", DataDir: dir, LogLevel: "error",
		DetachedSessionTTL: time.Hour, ScrollbackBytes: 4 << 20}
	for _, f := range mutate {
		f(cfg)
	}
	var log *slog.Logger
	if testing.Verbose() && os.Getenv("TUNNEL_TEST_LOG") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	} else {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	st, err := store.Open(ctx, filepath.Join(dir, "nexterm.db"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	hub := events.NewHub(ctx, log)
	router := httpx.NewRouter(httpx.Options{Log: log})
	d := &app.Deps{Ctx: ctx, Cfg: cfg, Log: log, Router: router, Store: st, Vault: v, Events: hub,
		Jobs: events.NewJobs(ctx, hub, log), Audit: audit.New(st, log)}
	h := &harness{t: t, ctx: ctx, cancel: cancel, cfg: cfg, d: d, users: map[string]*model.User{}}
	router.SetAuthenticator(httpx.AuthenticatorFunc(func(c *echo.Context) (*model.User, httpx.AuthInfo, error) {
		h.mu.Lock()
		u := h.users[c.Request().Header.Get("X-Test-User")]
		h.mu.Unlock()
		if u == nil {
			return nil, httpx.AuthInfo{}, nil
		}
		return u, httpx.AuthInfo{Method: httpx.AuthToken}, nil
	}))
	h.sessions = term.New(d)
	h.pool = sshx.New(d, h.sessions)
	h.pool.IdleTTL = 500 * time.Millisecond
	m, err := mount(d, &core.Core{Sessions: h.sessions, SSH: h.pool}, false)
	if err != nil {
		t.Fatal(err)
	}
	h.m = m
	h.api = httptest.NewServer(router)
	t.Cleanup(func() {
		h.api.Close()
		cancel()
		_ = h.sessions.Wait(context.Background())
		time.Sleep(50 * time.Millisecond)
		st.Close()
	})
	return h
}

// user creates a user (admin or not).
func (h *harness) user(name string, admin bool) *model.User {
	h.t.Helper()
	role := model.RoleUser
	if admin {
		role = model.RoleAdmin
	}
	u := &model.User{ID: model.NewID(), Username: name, DisplayName: name, Role: role}
	if err := h.d.Store.Users.Create(h.ctx, u, "x"); err != nil {
		h.t.Fatal(err)
	}
	h.mu.Lock()
	h.users[name] = u
	h.mu.Unlock()
	return u
}

// sshConn saves an SSH connection to srv (password auth, host key trusted) owned by u.
func (h *harness) sshConn(u *model.User, srv *testSSHServer, opts model.Options) *model.Connection {
	h.t.Helper()
	enc, err := h.d.Vault.SealJSON(map[string]string{"password": srv.Password})
	if err != nil {
		h.t.Fatal(err)
	}
	if opts == nil {
		opts = model.Options{}
	}
	c := &model.Connection{Name: "srv", Protocol: model.ProtoSSH, Host: srv.Host, Port: srv.Port, Username: srv.User,
		AuthMethod: model.AuthPassword, Options: opts, SecretsEnc: enc, SecretKeys: []string{"password"}, OwnerID: u.ID}
	c.Normalize()
	if err := h.d.Store.Connections.Create(h.ctx, c); err != nil {
		h.t.Fatal(err)
	}
	h.trust(srv.Host, srv.Port, srv.HostKey.PublicKey())
	return c
}

func (h *harness) trust(host string, port int, key ssh.PublicKey) {
	h.t.Helper()
	existing, _ := h.d.Store.KnownHosts.Find(h.ctx, host, port)
	if len(existing) > 0 {
		return
	}
	kh := &model.KnownHost{Host: host, Port: port, KeyType: key.Type(), PublicKey: sshx.FormatKnownHostKey(key),
		Fingerprint: ssh.FingerprintSHA256(key)}
	if err := h.d.Store.KnownHosts.Add(h.ctx, kh); err != nil {
		h.t.Fatal(err)
	}
}

// call performs an API request as user (nil user = anonymous) and decodes a 2xx JSON response into out. It returns
// the status code and the raw body.
func (h *harness) call(u *model.User, method, path string, body, out any) (int, []byte) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.api.URL+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(httpx.CSRFHeader, httpx.CSRFHeaderValue)
	if u != nil {
		req.Header.Set("X-Test-User", u.Username)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(data, out); err != nil {
			h.t.Fatalf("%s %s: decode %s: %v", method, path, data, err)
		}
	}
	return resp.StatusCode, data
}

// must is call that fails the test unless the status equals want.
func (h *harness) must(u *model.User, method, path string, body, out any, want int) {
	h.t.Helper()
	code, data := h.call(u, method, path, body, out)
	if code != want {
		h.t.Fatalf("%s %s: status %d (want %d): %s", method, path, code, want, data)
	}
}

// createTunnel creates a tunnel through the API.
func (h *harness) createTunnel(u *model.User, in Input) View {
	h.t.Helper()
	var v View
	h.must(u, "POST", "/api/tunnels", in, &v, http.StatusCreated)
	return v
}

// startTunnel starts a tunnel and waits until it is running and connected.
func (h *harness) startTunnel(u *model.User, id string) Status {
	h.t.Helper()
	h.must(u, "POST", "/api/tunnels/"+id+"/start", nil, nil, http.StatusOK)
	return h.waitStatus(id, func(s Status) bool { return s.State == model.TunnelRunning && s.Connected })
}

// waitStatus polls a tunnel's status until cond holds (10 s).
func (h *harness) waitStatus(id string, cond func(Status) bool) Status {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var st Status
	for time.Now().Before(deadline) {
		st = h.m.status(id)
		if cond(st) {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("tunnel %s: condition not met, last status %+v", id, st)
	return st
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// webServer starts an HTTP server answering "hello <path>".
func webServer(t *testing.T) (*httptest.Server, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello "+r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv, srv.Listener.Addr().(*net.TCPAddr).Port
}

// httpGet fetches http://addr/path with a short timeout.
func httpGet(t *testing.T, addr, path string) string {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get("http://" + addr + path)
	if err != nil {
		t.Fatalf("GET http://%s%s: %v", addr, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func itoa(n int) string { return strconv.Itoa(n) }

// shortTempDir returns a short temporary directory (Unix socket paths are limited to ~104 bytes).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "nxt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
