package server_test

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"

	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/server/servertest"
	"github.com/nexterm/nexterm/internal/store"
)

// Closing the server with live sessions must let the session manager finish its shutdown sweep (audit rows,
// recording finalization) before the database is closed. Regression: the sweep raced Store.Close and logged
// "audit: write failed … sql: database is closed".
func TestCloseFinishesSessionShutdownBeforeClosingTheDatabase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX local shell")
	}
	env := servertest.New(t)
	admin := env.Setup("admin", pass)

	var s model.RuntimeSession
	admin.MustJSON("POST", "/api/sessions", map[string]any{"quick": map[string]any{"protocol": "local"}, "cols": 80, "rows": 24}, &s)
	deadline := time.Now().Add(15 * time.Second)
	for {
		var cur model.RuntimeSession
		admin.MustJSON("GET", "/api/sessions/"+s.ID, nil, &cur)
		if cur.State == model.StateConnected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("local session did not connect (state %s: %s)", cur.State, cur.StateMessage)
		}
		time.Sleep(20 * time.Millisecond)
	}

	env.HTTP.CloseClientConnections()
	env.HTTP.Close()
	if err := env.Server.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st, err := store.OpenConfig(context.Background(), env.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	entries, err := st.Audit.List(context.Background(), store.AuditFilter{Action: "session.close", Target: s.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 session.close audit entry written during shutdown, got %d", len(entries))
	}
	var details struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(entries[0].Details, &details)
	if details.Reason != "server shutting down" {
		t.Fatalf("unexpected close reason %q", details.Reason)
	}
}
