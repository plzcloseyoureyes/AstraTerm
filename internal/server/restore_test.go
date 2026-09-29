package server_test

import (
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexterm/nexterm/internal/config"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/server/servertest"
)

// A restore staged through the admin API (importer, IMP-4) is applied by the next start, before the database opens.
func TestStagedRestoreAppliedOnRestart(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", pass)
	admin.MustJSON("POST", "/api/connections", map[string]any{"name": "before", "protocol": "ssh", "host": "a.example.com", "port": 22}, nil)
	resp, backup := admin.Do("GET", "/api/admin/backup", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("backup: %d", resp.StatusCode)
	}
	admin.MustJSON("POST", "/api/connections", map[string]any{"name": "after", "protocol": "ssh", "host": "b.example.com", "port": 22}, nil)

	// Staging and discarding.
	admin.MustJSON("POST", "/api/admin/restore", map[string]any{"content": base64.StdEncoding.EncodeToString(backup)}, nil)
	var st struct {
		Pending bool `json:"pending"`
	}
	admin.MustJSON("GET", "/api/admin/restore", nil, &st)
	if !st.Pending {
		t.Fatal("restore not pending after staging")
	}
	admin.MustJSON("DELETE", "/api/admin/restore", nil, nil)
	admin.MustJSON("GET", "/api/admin/restore", nil, &st)
	if st.Pending {
		t.Fatal("restore still pending after discarding")
	}

	var staged struct {
		ApplyOnRestart bool `json:"applyOnRestart"`
	}
	admin.MustJSON("POST", "/api/admin/restore", map[string]any{"content": base64.StdEncoding.EncodeToString(backup)}, &staged)
	if !staged.ApplyOnRestart {
		t.Fatalf("staged restore = %+v", staged)
	}
	dataDir := env.Cfg.DataDir
	env.HTTP.CloseClientConnections()
	env.HTTP.Close()
	if err := env.Server.Close(); err != nil {
		t.Fatal(err)
	}

	env2 := servertest.New(t, func(c *config.Config) { c.DataDir = dataDir })
	admin2 := env2.Login("admin", pass)
	var conns []model.Connection
	admin2.MustJSON("GET", "/api/connections", nil, &conns)
	if len(conns) != 1 || conns[0].Name != "before" {
		t.Fatalf("connections after restore = %+v, want only \"before\"", conns)
	}
	admin2.MustJSON("GET", "/api/admin/restore", nil, &st)
	if st.Pending {
		t.Fatal("restore still pending after it was applied")
	}
	prev, _ := filepath.Glob(filepath.Join(dataDir, "restore", "previous-*", "nexterm.db"))
	if len(prev) != 1 {
		t.Fatalf("previous database not kept: %v", prev)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "restore", "nexterm.db")); !os.IsNotExist(err) {
		t.Fatalf("staged database still present (err %v)", err)
	}
}
