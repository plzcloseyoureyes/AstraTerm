package servers_test

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/termstead/termstead/internal/config"
	"github.com/termstead/termstead/internal/server/servertest"
	"github.com/termstead/termstead/internal/servers"
)

// Full-stack checks through the real Termstead server (cookie auth, CSRF, module wiring in internal/server).

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestServersThroughTermstead(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", "correct horse battery")

	var list []servers.Status
	admin.MustJSON(http.MethodGet, "/api/servers", nil, &list)
	if len(list) != len(servers.Kinds) {
		t.Fatalf("%d servers", len(list))
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<h1>shared</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	port := freeTCPPort(t)
	cfg := map[string]any{"root": root, "port": port}

	// Mutations need the CSRF header.
	noCSRF := *admin
	noCSRF.CSRF = false
	if code, errCode := noCSRF.ErrorCode(http.MethodPut, "/api/servers/http", cfg); code != http.StatusForbidden || errCode != "csrf" {
		t.Fatalf("PUT without CSRF header: %d %s", code, errCode)
	}
	var st servers.Status
	admin.MustJSON(http.MethodPut, "/api/servers/http", cfg, &st)
	admin.MustJSON(http.MethodPost, "/api/servers/http/start", nil, &st)
	if !st.Running {
		t.Fatalf("not running: %+v", st)
	}
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "<h1>shared</h1>" {
		t.Fatalf("index.html not served: %q", b)
	}
	// Audit trail.
	var audit []map[string]any
	admin.MustJSON(http.MethodGet, "/api/admin/audit?action=server.", nil, &audit)
	actions := map[string]bool{}
	for _, e := range audit {
		actions[e["action"].(string)] = true
	}
	if !actions["server.start"] || !actions["server.config"] {
		t.Fatalf("audit actions %v", actions)
	}
	admin.MustJSON(http.MethodPost, "/api/servers/http/stop", nil, &st)
	if st.Running {
		t.Fatal("still running")
	}
}

func TestServersAdminOnlyInServerMode(t *testing.T) {
	env := servertest.New(t, func(c *config.Config) { c.Mode = config.ModeServer })
	admin := env.Setup("admin", "correct horse battery")
	user := env.CreateUser(admin, "bob", "another good password", "user")
	admin.MustJSON(http.MethodGet, "/api/servers", nil, nil)
	if code, _ := user.ErrorCode(http.MethodGet, "/api/servers", nil); code != http.StatusForbidden {
		t.Fatalf("non-admin list: %d", code)
	}
	if code, _ := user.ErrorCode(http.MethodPost, "/api/servers/sftp/start", nil); code != http.StatusForbidden {
		t.Fatalf("non-admin start: %d", code)
	}
	// Server configurations never appear in anyone's settings (module-private scope).
	var settings map[string]any
	user.MustJSON(http.MethodGet, "/api/settings", nil, &settings)
	admin.MustJSON(http.MethodPut, "/api/servers/ftp", map[string]any{"users": []map[string]any{
		{"username": "alice", "password": "alice-pass"}}}, nil)
	user.MustJSON(http.MethodGet, "/api/settings", nil, &settings)
	for k := range settings {
		if len(k) >= 8 && k[:8] == "servers." {
			t.Fatalf("server configuration visible in user settings: %s", k)
		}
	}
	var global map[string]any
	admin.MustJSON(http.MethodGet, "/api/admin/settings", nil, &global)
	if _, ok := global["servers.ftp"]; ok {
		t.Fatal("server configuration stored in the global settings")
	}
}
