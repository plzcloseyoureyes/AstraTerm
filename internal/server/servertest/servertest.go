// Package servertest spins up a complete in-process Termstead server (temp data dir, httptest listener) for
// integration tests of any module:
//
//	env := servertest.New(t)
//	admin := env.Setup("admin", "correct horse battery")   // first-run setup, returns a logged-in client
//	var conns []model.Connection
//	admin.MustJSON("GET", "/api/connections", nil, &conns)
package servertest

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

	"github.com/termstead/termstead/internal/auth"
	"github.com/termstead/termstead/internal/config"
	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/server"
	"github.com/termstead/termstead/internal/vault"
)

var fastOnce sync.Once

// FastCrypto lowers argon2 costs so tests run quickly (once per process). New calls it automatically.
func FastCrypto() {
	fastOnce.Do(func() {
		auth.PasswordParams.Time, auth.PasswordParams.MemKiB, auth.PasswordParams.Threads = 1, 1024, 1
		vault.DefaultKDF.Time, vault.DefaultKDF.MemKiB, vault.DefaultKDF.Threads = 1, 1024, 1
	})
}

// Env is a running test server.
type Env struct {
	T      testing.TB
	Cfg    *config.Config
	Server *server.Server
	HTTP   *httptest.Server
}

// New starts a server in desktop mode on a temp data dir; mutate may adjust the config before startup.
func New(t testing.TB, mutate ...func(*config.Config)) *Env {
	t.Helper()
	FastCrypto()
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
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = server.NewLogger(cfg, testWriter{t})
	}
	srv, err := server.New(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	hs := httptest.NewServer(srv.Handler())
	env := &Env{T: t, Cfg: cfg, Server: srv, HTTP: hs}
	t.Cleanup(func() {
		hs.CloseClientConnections()
		hs.Close()
		srv.Close()
	})
	return env
}

type testWriter struct{ t testing.TB }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// Client is an HTTP client with its own cookie jar. CSRF controls whether the X-Termstead header is sent; Bearer, when
// set, is sent as an Authorization header.
type Client struct {
	Env    *Env
	HTTP   *http.Client
	CSRF   bool
	Bearer string
	Header http.Header
}

// Client returns a fresh anonymous client that sends the CSRF header.
func (e *Env) Client() *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{Env: e, HTTP: &http.Client{Jar: jar}, CSRF: true, Header: http.Header{}}
}

// URL returns the absolute URL of path.
func (e *Env) URL(path string) string { return e.HTTP.URL + path }

// Do performs a request; body (if not nil) is JSON-encoded unless it is []byte / string. It returns the response
// (body already read) and the body bytes.
func (c *Client) Do(method, path string, body any) (*http.Response, []byte) {
	c.Env.T.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			c.Env.T.Fatalf("marshal: %v", err)
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, c.Env.URL(path), rd)
	if err != nil {
		c.Env.T.Fatalf("new request: %v", err)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.CSRF {
		req.Header.Set(httpx.CSRFHeader, httpx.CSRFHeaderValue)
	}
	if c.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.Bearer)
	}
	for k, v := range c.Header {
		req.Header[k] = v
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		c.Env.T.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

// JSON performs a request and decodes the JSON response into out (when out is non-nil and the status is 2xx). It
// returns the status code.
func (c *Client) JSON(method, path string, body, out any) int {
	c.Env.T.Helper()
	resp, data := c.Do(method, path, body)
	if out != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			c.Env.T.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	return resp.StatusCode
}

// MustJSON is JSON that fails the test unless the status is 2xx.
func (c *Client) MustJSON(method, path string, body, out any) {
	c.Env.T.Helper()
	resp, data := c.Do(method, path, body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.Env.T.Fatalf("%s %s: status %d: %s", method, path, resp.StatusCode, data)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			c.Env.T.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
}

// ErrorCode performs a request and returns the status and the "code" of the JSON error body.
func (c *Client) ErrorCode(method, path string, body any) (int, string) {
	c.Env.T.Helper()
	resp, data := c.Do(method, path, body)
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(data, &e)
	return resp.StatusCode, e.Code
}

// Setup performs first-run setup (sending the setup token server mode requires) and returns a client logged in as
// the new admin.
func (e *Env) Setup(username, password string) *Client {
	e.T.Helper()
	c := e.Client()
	body := map[string]string{"username": username, "password": password}
	if tok := e.Server.Auth.SetupToken(); tok != "" {
		body["setupToken"] = tok
	}
	c.MustJSON("POST", "/api/auth/setup", body, nil)
	return c
}

// Login returns a new client logged in as username.
func (e *Env) Login(username, password string) *Client {
	e.T.Helper()
	c := e.Client()
	c.MustJSON("POST", "/api/auth/login", map[string]any{"username": username, "password": password}, nil)
	return c
}

// CreateUser creates a user through the admin API (admin must be an admin client) and returns a logged-in client.
func (e *Env) CreateUser(admin *Client, username, password, role string) *Client {
	e.T.Helper()
	admin.MustJSON("POST", "/api/admin/users", map[string]string{"username": username, "password": password, "role": role}, nil)
	return e.Login(username, password)
}
