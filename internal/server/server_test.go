package server_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/server/servertest"
)

const pass = "correct horse battery"

func TestConnectionSecretsWriteOnly(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", pass)

	body := map[string]any{"name": "web01", "protocol": "ssh", "host": "10.0.0.5", "username": "root",
		"tags": []string{"prod", " prod ", "web"}, "options": map[string]any{"term": "xterm", "custom": map[string]any{"a": 1}},
		"secrets": map[string]string{"password": "hunter2-secret", "passphrase": "pp-secret", "empty": ""}}
	resp, raw := admin.Do("POST", "/api/connections", body)
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), "hunter2-secret") || strings.Contains(string(raw), `"secrets"`) {
		t.Fatalf("secrets leaked in response: %s", raw)
	}
	var c model.Connection
	json.Unmarshal(raw, &c)
	if c.Port != 22 || c.AuthMethod != "auto" || len(c.Tags) != 2 || c.OwnerID == "" || c.ID == "" {
		t.Fatalf("normalized connection: %+v", c)
	}
	if strings.Join(c.SecretKeys, ",") != "passphrase,password" {
		t.Fatalf("secretKeys: %v", c.SecretKeys)
	}

	// List + get never contain secret values.
	_, raw = admin.Do("GET", "/api/connections", nil)
	if strings.Contains(string(raw), "hunter2-secret") || strings.Contains(string(raw), "pp-secret") {
		t.Fatalf("secrets leaked in list: %s", raw)
	}
	var list []model.Connection
	json.Unmarshal(raw, &list)
	if len(list) != 1 || len(list[0].SecretKeys) != 2 || list[0].Options.String("term") != "xterm" {
		t.Fatalf("list: %+v", list)
	}

	// PATCH: omitted secrets unchanged; "" deletes; new keys merge.
	admin.MustJSON("PATCH", "/api/connections/"+c.ID, map[string]any{"name": "web01b"}, &c)
	if c.Name != "web01b" || len(c.SecretKeys) != 2 {
		t.Fatalf("patch without secrets: %+v", c)
	}
	admin.MustJSON("PATCH", "/api/connections/"+c.ID, map[string]any{"secrets": map[string]string{"passphrase": "", "sudoPassword": "sudo-secret"}}, &c)
	if strings.Join(c.SecretKeys, ",") != "password,sudoPassword" {
		t.Fatalf("secret merge: %v", c.SecretKeys)
	}
	if st, _ := admin.ErrorCode("PATCH", "/api/connections/"+c.ID, map[string]any{"secrets": map[string]string{"bad key!": "x"}}); st != 400 {
		t.Fatalf("invalid secret name: %d", st)
	}

	// Server-side resolution decrypts the merged secrets.
	d := env.Server.Deps
	u, _ := d.Store.Users.GetByUsername(context.Background(), "admin")
	conn, secrets, err := d.ResolveConnection(context.Background(), u, c.ID)
	if err != nil || secrets["password"] != "hunter2-secret" || secrets["sudoPassword"] != "sudo-secret" || len(secrets) != 2 || conn.Secrets != nil {
		t.Fatalf("resolve: %v %v", secrets, err)
	}

	// Validation errors.
	for _, bad := range []map[string]any{
		{"name": "", "protocol": "ssh"},
		{"name": "x", "protocol": "SSH!"},
		{"name": "x", "protocol": "ssh", "port": 70000},
		{"name": "x", "protocol": "ssh", "authMethod": "magic"},
		{"name": "x", "protocol": "ssh", "folderId": "missing"},
		{"name": "x", "protocol": "ssh", "keyId": "missing"},
	} {
		if st, _ := admin.ErrorCode("POST", "/api/connections", bad); st != 400 {
			t.Fatalf("expected 400 for %v, got %d", bad, st)
		}
	}
	if st, _ := admin.ErrorCode("POST", "/api/connections", `{"name":`); st != 400 {
		t.Fatalf("malformed JSON: %d", st)
	}

	// Duplicate keeps secrets for the owner; bulk delete removes both.
	var dup model.Connection
	if st := admin.JSON("POST", "/api/connections/"+c.ID+"/duplicate", nil, &dup); st != 201 {
		t.Fatalf("duplicate: %d", st)
	}
	if dup.ID == c.ID || dup.Name != "web01b (copy)" || len(dup.SecretKeys) != 2 {
		t.Fatalf("dup: %+v", dup)
	}
	var del struct {
		Deleted int `json:"deleted"`
	}
	admin.MustJSON("POST", "/api/connections/bulk-delete", map[string]any{"ids": []string{c.ID, dup.ID, "gone"}}, &del)
	if del.Deleted != 2 {
		t.Fatalf("bulk delete: %+v", del)
	}
	if st, _ := admin.ErrorCode("GET", "/api/connections/"+c.ID, nil); st != 404 {
		t.Fatalf("deleted connection: %d", st)
	}
}

func TestSharedVisibility(t *testing.T) {
	env := servertest.New(t, func(c *config.Config) { c.Mode = config.ModeServer })
	admin := env.Setup("admin", pass)
	bob := env.CreateUser(admin, "bob", "bob password", "user")

	var shared, private model.Connection
	admin.MustJSON("POST", "/api/connections", map[string]any{"name": "shared", "protocol": "ssh", "host": "h1",
		"shared": true, "secrets": map[string]string{"password": "admin-pw"}}, &shared)
	admin.MustJSON("POST", "/api/connections", map[string]any{"name": "private", "protocol": "ssh", "host": "h2"}, &private)

	var list []model.Connection
	bob.MustJSON("GET", "/api/connections", nil, &list)
	if len(list) != 1 || list[0].ID != shared.ID || len(list[0].SecretKeys) != 1 {
		t.Fatalf("bob list: %+v", list)
	}
	if st, _ := bob.ErrorCode("GET", "/api/connections/"+private.ID, nil); st != 404 {
		t.Fatalf("bob sees private: %d", st)
	}
	if st, _ := bob.ErrorCode("PATCH", "/api/connections/"+shared.ID, map[string]any{"host": "attacker.example"}); st != 403 {
		t.Fatalf("bob modified shared: %d", st)
	}
	if st, _ := bob.ErrorCode("DELETE", "/api/connections/"+shared.ID, nil); st != 403 {
		t.Fatalf("bob deleted shared: %d", st)
	}
	if st, _ := bob.ErrorCode("PATCH", "/api/connections/"+private.ID, map[string]any{"name": "x"}); st != 404 {
		t.Fatalf("bob patched private: %d", st)
	}
	if st, _ := bob.ErrorCode("POST", "/api/connections/bulk-delete", map[string]any{"ids": []string{shared.ID}}); st != 403 {
		t.Fatalf("bob bulk-deleted shared: %d", st)
	}
	// Bob can use the shared connection's secrets server-side...
	d := env.Server.Deps
	bu, _ := d.Store.Users.GetByUsername(context.Background(), "bob")
	if _, sec, err := d.ResolveConnection(context.Background(), bu, shared.ID); err != nil || sec["password"] != "admin-pw" {
		t.Fatalf("bob resolve shared: %v %v", sec, err)
	}
	if _, _, err := d.ResolveConnection(context.Background(), bu, private.ID); err == nil {
		t.Fatal("bob resolved a private connection")
	}
	// ...but a copy of it carries no secrets (so they cannot be redirected to another host).
	var dup model.Connection
	bob.MustJSON("POST", "/api/connections/"+shared.ID+"/duplicate", nil, &dup)
	if dup.OwnerID == shared.OwnerID || len(dup.SecretKeys) != 0 || dup.Shared {
		t.Fatalf("bob's copy: %+v", dup)
	}
	// Non-admins cannot publish shared connections or folders.
	if st, _ := bob.ErrorCode("POST", "/api/connections", map[string]any{"name": "s", "protocol": "ssh", "shared": true}); st != 403 {
		t.Fatalf("bob shared a connection: %d", st)
	}
	if st, _ := bob.ErrorCode("PATCH", "/api/connections/"+dup.ID, map[string]any{"shared": true}); st != 403 {
		t.Fatalf("bob shared via patch: %d", st)
	}
	if st, _ := bob.ErrorCode("POST", "/api/folders", map[string]any{"name": "f", "shared": true}); st != 403 {
		t.Fatalf("bob shared a folder: %d", st)
	}
	// Admin may edit shared rows.
	admin.MustJSON("PATCH", "/api/connections/"+shared.ID, map[string]any{"notes": "hello"}, nil)
}

func TestFoldersIdentitiesReorder(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", pass)

	var root, child model.Folder
	admin.MustJSON("POST", "/api/folders", map[string]any{"name": "Root", "color": "#f00"}, &root)
	admin.MustJSON("POST", "/api/folders", map[string]any{"name": "Child", "parentId": root.ID}, &child)
	if child.ParentID != root.ID {
		t.Fatalf("child: %+v", child)
	}
	if st, _ := admin.ErrorCode("PATCH", "/api/folders/"+root.ID, map[string]any{"parentId": child.ID}); st != 400 {
		t.Fatalf("cycle accepted: %d", st)
	}
	childID := child.ID
	child = model.Folder{} // decode into a fresh value: omitted fields must not survive from the previous response
	admin.MustJSON("PATCH", "/api/folders/"+childID, map[string]any{"parentId": nil, "name": "Moved"}, &child)
	if child.ParentID != "" || child.Name != "Moved" {
		t.Fatalf("move to root: %+v", child)
	}
	admin.MustJSON("PATCH", "/api/folders/"+child.ID, map[string]any{"parentId": root.ID}, &child)

	var c1, c2 model.Connection
	admin.MustJSON("POST", "/api/connections", map[string]any{"name": "c1", "protocol": "telnet", "folderId": child.ID}, &c1)
	admin.MustJSON("POST", "/api/connections", map[string]any{"name": "c2", "protocol": "vnc"}, &c2)
	if c1.Port != 23 || c2.Port != 5900 {
		t.Fatalf("default ports: %d %d", c1.Port, c2.Port)
	}
	admin.MustJSON("POST", "/api/connections/reorder", map[string]any{"items": []map[string]any{
		{"id": c2.ID, "folderId": root.ID, "sortOrder": 1}, {"id": c1.ID, "folderId": nil, "sortOrder": 2}}}, nil)
	c1, c2 = refetch(t, admin, c1.ID), refetch(t, admin, c2.ID)
	if c2.FolderID != root.ID || c2.SortOrder != 1 || c1.FolderID != "" {
		t.Fatalf("reorder: c1=%+v c2=%+v", c1, c2)
	}

	// Non-recursive delete moves children up; recursive delete removes the subtree with its connections.
	admin.MustJSON("DELETE", "/api/folders/"+child.ID, nil, nil)
	var folders []model.Folder
	admin.MustJSON("GET", "/api/folders", nil, &folders)
	if len(folders) != 1 || folders[0].ID != root.ID {
		t.Fatalf("folders: %+v", folders)
	}
	admin.MustJSON("DELETE", "/api/folders/"+root.ID+"?recursive=1", nil, nil)
	if st, _ := admin.ErrorCode("GET", "/api/connections/"+c2.ID, nil); st != 404 {
		t.Fatalf("connection in deleted subtree: %d", st)
	}

	// Identities: secrets write-only, merged into connections at resolve time.
	var ident model.Identity
	resp, raw := admin.Do("POST", "/api/identities", map[string]any{"name": "ops", "username": "deploy",
		"secrets": map[string]string{"password": "ident-pw", "passphrase": "ident-pp"}})
	if resp.StatusCode != 201 || strings.Contains(string(raw), "ident-pw") {
		t.Fatalf("identity create: %d %s", resp.StatusCode, raw)
	}
	json.Unmarshal(raw, &ident)
	var c3 model.Connection
	admin.MustJSON("POST", "/api/connections", map[string]any{"name": "c3", "protocol": "ssh", "host": "h",
		"identityId": ident.ID, "secrets": map[string]string{"password": "conn-pw"}}, &c3)
	d := env.Server.Deps
	u, _ := d.Store.Users.GetByUsername(context.Background(), "admin")
	conn, sec, err := d.ResolveConnection(context.Background(), u, c3.ID)
	if err != nil || conn.Username != "deploy" || sec["password"] != "conn-pw" || sec["passphrase"] != "ident-pp" {
		t.Fatalf("identity merge: %+v %v %v", conn, sec, err)
	}
	admin.MustJSON("PATCH", "/api/identities/"+ident.ID, map[string]any{"secrets": map[string]string{"password": ""}}, &ident)
	if strings.Join(ident.SecretKeys, ",") != "passphrase" {
		t.Fatalf("identity secrets: %v", ident.SecretKeys)
	}
	var idents []model.Identity
	admin.MustJSON("GET", "/api/identities", nil, &idents)
	if len(idents) != 1 {
		t.Fatalf("identities: %d", len(idents))
	}
	admin.MustJSON("DELETE", "/api/identities/"+ident.ID, nil, nil)
	if c3 = refetch(t, admin, c3.ID); c3.IdentityID != "" {
		t.Fatalf("identity not unlinked: %+v", c3)
	}
}

// refetch GETs a connection into a fresh value (decoding into an existing struct would keep omitted fields).
func refetch(t *testing.T, c *servertest.Client, id string) model.Connection {
	t.Helper()
	var out model.Connection
	c.MustJSON("GET", "/api/connections/"+id, nil, &out)
	return out
}

func TestVaultLockedFlow(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", pass)

	var st struct {
		Locked            bool `json:"locked"`
		HasMasterPassword bool `json:"hasMasterPassword"`
	}
	if code, _ := admin.ErrorCode("POST", "/api/vault/lock", nil); code != 409 {
		t.Fatalf("lock without master password: %d", code)
	}
	admin.MustJSON("POST", "/api/vault/master-password", map[string]string{"newPassword": "vault password"}, &st)
	if !st.HasMasterPassword || st.Locked {
		t.Fatalf("after set: %+v", st)
	}
	var c model.Connection
	admin.MustJSON("POST", "/api/connections", map[string]any{"name": "c", "protocol": "ssh",
		"secrets": map[string]string{"password": "pw"}}, &c)
	admin.MustJSON("POST", "/api/vault/lock", nil, &st)
	if !st.Locked {
		t.Fatalf("after lock: %+v", st)
	}
	// Locked: sealing new secrets and resolving stored ones fail with 423; secret-less writes still work.
	if code, errc := admin.ErrorCode("POST", "/api/connections", map[string]any{"name": "d", "protocol": "ssh",
		"secrets": map[string]string{"password": "x"}}); code != 423 || errc != "locked" {
		t.Fatalf("create with secrets while locked: %d %s", code, errc)
	}
	if code := admin.JSON("POST", "/api/connections", map[string]any{"name": "e", "protocol": "ssh"}, nil); code != 201 {
		t.Fatalf("create without secrets while locked: %d", code)
	}
	if code := admin.JSON("PATCH", "/api/connections/"+c.ID, map[string]any{"name": "renamed"}, nil); code != 200 {
		t.Fatalf("patch without secrets while locked: %d", code)
	}
	d := env.Server.Deps
	u, _ := d.Store.Users.GetByUsername(context.Background(), "admin")
	if _, _, err := d.ResolveConnection(context.Background(), u, c.ID); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("resolve while locked: %v", err)
	}
	if code, errc := admin.ErrorCode("POST", "/api/vault/unlock", map[string]string{"password": "wrong!!!"}); code != 403 || errc != "wrong_password" {
		t.Fatalf("wrong unlock: %d %s", code, errc)
	}
	admin.MustJSON("POST", "/api/vault/unlock", map[string]string{"password": "vault password"}, &st)
	if st.Locked {
		t.Fatal("still locked")
	}
	if _, sec, err := d.ResolveConnection(context.Background(), u, c.ID); err != nil || sec["password"] != "pw" {
		t.Fatalf("resolve after unlock: %v %v", sec, err)
	}
	admin.MustJSON("GET", "/api/vault/status", nil, &st)
	admin.MustJSON("POST", "/api/vault/master-password", map[string]string{"currentPassword": "vault password", "newPassword": ""}, &st)
	if st.HasMasterPassword {
		t.Fatal("master password not removed")
	}
}

func TestSettingsMerge(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", pass)
	bob := env.CreateUser(admin, "bob", "bob password", "user")

	var global map[string]any
	admin.MustJSON("PUT", "/api/admin/settings", map[string]any{"terminal": map[string]any{"fontSize": 13, "theme": "dark"}, "lang": "en"}, &global)
	if st, _ := bob.ErrorCode("PUT", "/api/admin/settings", map[string]any{"x": 1}); st != 403 {
		t.Fatalf("non-admin global settings: %d", st)
	}
	var eff map[string]any
	bob.MustJSON("PUT", "/api/settings", map[string]any{"terminal": map[string]any{"fontSize": 15}}, &eff)
	term := eff["terminal"].(map[string]any)
	if term["fontSize"] != float64(15) || term["theme"] != "dark" || eff["lang"] != "en" {
		t.Fatalf("effective: %v", eff)
	}
	bob.MustJSON("PUT", "/api/settings", map[string]any{"terminal": map[string]any{"cursor": "bar"}, "layout": []int{1, 2}}, &eff)
	term = eff["terminal"].(map[string]any)
	if term["fontSize"] != float64(15) || term["cursor"] != "bar" {
		t.Fatalf("deep merge: %v", eff)
	}
	eff = nil // fresh map: json.Unmarshal merges into an existing one
	bob.MustJSON("PUT", "/api/settings", map[string]any{"terminal": map[string]any{"fontSize": nil}, "layout": nil}, &eff)
	term = eff["terminal"].(map[string]any)
	if term["fontSize"] != float64(13) || eff["layout"] != nil {
		t.Fatalf("null deletes: %v", eff)
	}
	eff = nil
	admin.MustJSON("GET", "/api/settings", nil, &eff)
	if eff["terminal"].(map[string]any)["cursor"] != nil {
		t.Fatalf("user settings leaked to another user: %v", eff)
	}
	if st, _ := bob.ErrorCode("PUT", "/api/settings", `[1,2]`); st != 400 {
		t.Fatalf("non-object settings: %d", st)
	}
}

func TestSPAAndHeaders(t *testing.T) {
	env := servertest.New(t)
	c := env.Client()
	resp, body := c.Do("GET", "/", nil)
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(strings.ToLower(string(body)), "<html") {
		t.Fatalf("index: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("index cache-control: %q", resp.Header.Get("Cache-Control"))
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"'wasm-unsafe-eval'", "img-src 'self' data: blob:", "style-src 'self' 'unsafe-inline'", "ws://"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP missing %q: %s", want, csp)
		}
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("security headers: %v", resp.Header)
	}
	resp, _ = c.Do("GET", "/some/client/route", nil)
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("SPA fallback: %d", resp.StatusCode)
	}
	resp, _ = c.Do("GET", "/assets/definitely-missing.js", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("missing asset: %d", resp.StatusCode)
	}
	if st, code := c.ErrorCode("GET", "/api/definitely-missing", nil); st != 404 || code != "not_found" {
		t.Fatalf("unknown API: %d %s", st, code)
	}
	if st, _ := c.ErrorCode("DELETE", "/", nil); st != 405 {
		t.Fatalf("DELETE /: %d", st)
	}
}
