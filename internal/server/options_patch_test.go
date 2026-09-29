package server_test

import (
	"testing"

	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/server/servertest"
)

// PATCH replaces the whole options object (SPEC §9): keys missing from the patch are removed, so editors can reset an
// option to its default by omitting it. Regression test: json.Unmarshal into the existing map used to merge instead.
func TestPatchConnectionOptionsReplaces(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", pass)

	var c model.Connection
	admin.MustJSON("POST", "/api/connections", map[string]any{
		"name": "web", "protocol": "ssh", "host": "10.0.0.7", "username": "root",
		"options": map[string]any{"sshBrowser": "none", "followCwd": false, "term": "xterm"},
	}, &c)
	if c.Options["sshBrowser"] != "none" || c.Options["followCwd"] != false {
		t.Fatalf("create options not stored: %#v", c.Options)
	}

	// Omit sshBrowser/followCwd (reset to defaults) and change term. Decode every response into a FRESH value:
	// json.Unmarshal into an existing map merges, which would hide the very bug this test guards against.
	var patched model.Connection
	admin.MustJSON("PATCH", "/api/connections/"+c.ID, map[string]any{"options": map[string]any{"term": "xterm-256color"}}, &patched)
	if _, ok := patched.Options["sshBrowser"]; ok {
		t.Fatalf("sshBrowser should have been removed, got %#v", patched.Options)
	}
	if _, ok := patched.Options["followCwd"]; ok {
		t.Fatalf("followCwd should have been removed, got %#v", patched.Options)
	}
	if patched.Options["term"] != "xterm-256color" {
		t.Fatalf("term not updated: %#v", patched.Options)
	}

	// The stored row agrees with the PATCH response.
	var got model.Connection
	admin.MustJSON("GET", "/api/connections/"+c.ID, nil, &got)
	if _, ok := got.Options["sshBrowser"]; ok || got.Options["term"] != "xterm-256color" {
		t.Fatalf("stored options wrong: %#v", got.Options)
	}

	// A patch without "options" leaves them untouched; null clears them.
	var renamed model.Connection
	admin.MustJSON("PATCH", "/api/connections/"+c.ID, map[string]any{"name": "web2"}, &renamed)
	if renamed.Options["term"] != "xterm-256color" {
		t.Fatalf("options changed by unrelated patch: %#v", renamed.Options)
	}
	var cleared model.Connection
	admin.MustJSON("PATCH", "/api/connections/"+c.ID, map[string]any{"options": nil}, &cleared)
	if _, ok := cleared.Options["term"]; ok {
		t.Fatalf("null options should clear stored options (defaults may be rendered), got %#v", cleared.Options)
	}
}
