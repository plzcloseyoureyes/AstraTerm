package servers

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
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/audit"
	"github.com/plzcloseyoureyes/astraterm/internal/auth"
	cfgpkg "github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/events"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
	"github.com/plzcloseyoureyes/astraterm/internal/vault"
)

func TestMain(m *testing.M) {
	auth.PasswordParams.Time, auth.PasswordParams.MemKiB, auth.PasswordParams.Threads = 1, 1024, 1
	vault.DefaultKDF.Time, vault.DefaultKDF.MemKiB, vault.DefaultKDF.Threads = 1, 1024, 1
	rsaHostKeyBits = 2048
	os.Exit(m.Run())
}

// harness is a minimal AstraTerm backend (store, vault, events, router with a header-based test authenticator) with the
// servers module mounted (no autostart).
type harness struct {
	t      *testing.T
	ctx    context.Context
	cancel context.CancelFunc
	cfg    *cfgpkg.Config
	d      *app.Deps
	m      *Manager
	api    *httptest.Server
	dir    string

	mu    sync.Mutex
	users map[string]*model.User
}

func newHarness(t *testing.T, mutate ...func(*cfgpkg.Config)) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &cfgpkg.Config{Mode: cfgpkg.ModeDesktop, Listen: "127.0.0.1:7822", DataDir: dataDir, LogLevel: "error"}
	for _, f := range mutate {
		f(cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() && os.Getenv("SERVERS_TEST_LOG") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	st, err := store.Open(ctx, filepath.Join(dataDir, "astraterm.db"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open(ctx, st, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	hub := events.NewHub(ctx, log)
	router := httpx.NewRouter(httpx.Options{Log: log})
	d := &app.Deps{Ctx: ctx, Cfg: cfg, Log: log, Router: router, Store: st, Vault: v, Events: hub,
		Jobs: events.NewJobs(ctx, hub, log), Audit: audit.New(st, log)}
	h := &harness{t: t, ctx: ctx, cancel: cancel, cfg: cfg, d: d, dir: dir, users: map[string]*model.User{}}
	router.SetAuthenticator(httpx.AuthenticatorFunc(func(c *echo.Context) (*model.User, httpx.AuthInfo, error) {
		h.mu.Lock()
		u := h.users[c.Request().Header.Get("X-Test-User")]
		h.mu.Unlock()
		if u == nil {
			return nil, httpx.AuthInfo{}, nil
		}
		return u, httpx.AuthInfo{Method: httpx.AuthToken}, nil
	}))
	router.WS("/ws/events", hub.ServeWS)
	m, err := mount(d, false)
	if err != nil {
		t.Fatal(err)
	}
	h.m = m
	h.api = httptest.NewServer(router)
	t.Cleanup(func() {
		h.api.CloseClientConnections()
		h.api.Close()
		cancel()
		wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = m.Wait(wctx)
		wcancel()
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

// call performs an API request as user and decodes a 2xx JSON response into out.
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
		// A zeroed target: decoding into a value from an earlier call would keep the fields this response omits.
		reflect.ValueOf(out).Elem().SetZero()
		if err := json.Unmarshal(data, out); err != nil {
			h.t.Fatalf("%s %s: decode %s: %v", method, path, data, err)
		}
	}
	return resp.StatusCode, data
}

// must is call that fails the test unless the status equals want.
func (h *harness) must(u *model.User, method, path string, body, out any, want int) []byte {
	h.t.Helper()
	code, data := h.call(u, method, path, body, out)
	if code != want {
		h.t.Fatalf("%s %s: status %d (want %d): %s", method, path, code, want, data)
	}
	return data
}

// configure saves a configuration (PUT) and returns the status.
func (h *harness) configure(u *model.User, kind Kind, cfg map[string]any) Status {
	h.t.Helper()
	var st Status
	h.must(u, http.MethodPut, "/api/servers/"+string(kind), cfg, &st, http.StatusOK)
	return st
}

// start starts a server and returns its status.
func (h *harness) start(u *model.User, kind Kind) Status {
	h.t.Helper()
	var st Status
	h.must(u, http.MethodPost, "/api/servers/"+string(kind)+"/start", nil, &st, http.StatusOK)
	if !st.Running || st.State != stateRunning {
		h.t.Fatalf("%s not running after start: %+v", kind, st)
	}
	return st
}

func (h *harness) stop(u *model.User, kind Kind) Status {
	h.t.Helper()
	var st Status
	h.must(u, http.MethodPost, "/api/servers/"+string(kind)+"/stop", nil, &st, http.StatusOK)
	return st
}

// logs returns the log messages of a server.
func (h *harness) logs(u *model.User, kind Kind) []LogEntry {
	h.t.Helper()
	var r logsReply
	h.must(u, http.MethodGet, "/api/servers/"+string(kind)+"/logs", nil, &r, http.StatusOK)
	return r.Entries
}

// waitLog waits until a log message of kind contains substr and returns the newest such entry.
func (h *harness) waitLog(u *model.User, kind Kind, substr string) LogEntry {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries := h.logs(u, kind)
		for _, entrie := range slices.Backward(entries) { // newest match
			if strings.Contains(entrie.Message, substr) {
				return entrie
			}
		}
		if time.Now().After(deadline) {
			var msgs []string
			for _, e := range h.logs(u, kind) {
				msgs = append(msgs, e.Message)
			}
			h.t.Fatalf("no %s log entry containing %q; log: %q", kind, substr, msgs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// shareDir creates a shared folder with a few files.
func (h *harness) shareDir() string {
	h.t.Helper()
	root := filepath.Join(h.dir, "share")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		h.t.Fatal(err)
	}
	for name, content := range map[string]string{"hello.txt": "hello world\n", "sub/nested.txt": "nested\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	// A secret outside the share, reachable only through escapes.
	if err := os.WriteFile(filepath.Join(h.dir, "secret.txt"), []byte("top secret"), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return root
}

// freePort returns a currently free port on 127.0.0.1 for network ("tcp", "udp" or "both").
func freePort(t *testing.T, network string) int {
	t.Helper()
	for range 50 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		if network == "tcp" {
			ln.Close()
			return port
		}
		pc, err := net.ListenPacket("udp", "127.0.0.1:"+strconv.Itoa(port))
		ln.Close()
		if err != nil {
			continue
		}
		pc.Close()
		return port
	}
	t.Fatal("no free port")
	return 0
}

// eventsClient is a /ws/events connection of a user.
type eventsClient struct {
	t    *testing.T
	conn *websocket.Conn
	ch   chan map[string]any
}

func (h *harness) events(u *model.User) *eventsClient {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("X-Test-User", u.Username)
	url := "ws" + strings.TrimPrefix(h.api.URL, "http") + "/ws/events"
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		h.t.Fatal(err)
	}
	ec := &eventsClient{t: h.t, conn: conn, ch: make(chan map[string]any, 256)}
	go func() {
		for {
			_, data, err := conn.Read(h.ctx)
			if err != nil {
				close(ec.ch)
				return
			}
			var m map[string]any
			if json.Unmarshal(data, &m) == nil {
				ec.ch <- m
			}
		}
	}()
	h.t.Cleanup(func() { conn.CloseNow() })
	ec.wait(func(m map[string]any) bool { return m["type"] == "hello" })
	return ec
}

func (ec *eventsClient) send(v any) {
	ec.t.Helper()
	b, _ := json.Marshal(v)
	if err := ec.conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		ec.t.Fatal(err)
	}
}

// wait returns the first event matching pred (fails after 5 s).
func (ec *eventsClient) wait(pred func(map[string]any) bool) map[string]any {
	ec.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-ec.ch:
			if !ok {
				ec.t.Fatal("events socket closed")
			}
			if pred(m) {
				return m
			}
		case <-timeout:
			ec.t.Fatal("timed out waiting for an event")
		}
	}
}
