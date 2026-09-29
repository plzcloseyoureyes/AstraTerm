package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/termstead/termstead/internal/model"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mkUser(t *testing.T, s *Store, name string, role model.Role) *model.User {
	t.Helper()
	u := &model.User{Username: name, DisplayName: name, Role: role}
	if err := s.Users.Create(context.Background(), u, "hash"); err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	return u
}

func TestMigrations(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	var n int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE module = 'core' AND version = 1`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("core migration not recorded: n=%d err=%v", n, err)
	}
	// Pragmas are active.
	var fk int
	s.DB.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk)
	if fk != 1 {
		t.Fatalf("foreign_keys = %d", fk)
	}
	var mode string
	s.DB.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Fatalf("journal_mode = %q", mode)
	}

	// A module migration registered later is applied by Migrate, exactly once, in version order.
	RegisterMigration("zz_test", 2, `ALTER TABLE zz_items ADD COLUMN extra TEXT NOT NULL DEFAULT 'x';`)
	RegisterMigration("zz_test", 1, `CREATE TABLE zz_items (id TEXT PRIMARY KEY); CREATE INDEX zz_items_id ON zz_items(id);`)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO zz_items (id) VALUES ('a')`); err != nil {
		t.Fatalf("module table missing: %v", err)
	}
	var extra string
	if err := s.DB.QueryRowContext(ctx, `SELECT extra FROM zz_items`).Scan(&extra); err != nil || extra != "x" {
		t.Fatalf("v2 not applied: %q %v", extra, err)
	}

	// Conflicting re-registration is reported (and cleared afterwards so other tests are unaffected).
	RegisterMigration("zz_test", 1, `SELECT 1;`)
	if err := s.Migrate(ctx); err == nil {
		t.Fatal("expected conflicting migration error")
	}
	regMu.Lock()
	regErrs = nil
	regMu.Unlock()
}

func TestUsers(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	admin := &model.User{Username: "Alice", DisplayName: "Alice"}
	if err := s.Users.CreateFirstAdmin(ctx, admin, "h1"); err != nil {
		t.Fatal(err)
	}
	if admin.Role != model.RoleAdmin {
		t.Fatalf("first admin role = %s", admin.Role)
	}
	if err := s.Users.CreateFirstAdmin(ctx, &model.User{Username: "bob"}, "h"); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("second first-admin: %v", err)
	}
	// Usernames are case-insensitive and unique.
	if err := s.Users.Create(ctx, &model.User{Username: "alice"}, "h"); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("duplicate username: %v", err)
	}
	got, err := s.Users.GetByUsername(ctx, "ALICE")
	if err != nil || got.ID != admin.ID {
		t.Fatalf("get by username: %v %v", got, err)
	}
	ua, err := s.Users.GetAuthByUsername(ctx, "alice")
	if err != nil || ua.PasswordHash != "h1" || len(ua.RecoveryHashes) != 0 {
		t.Fatalf("get auth: %+v %v", ua, err)
	}

	// TOTP state, replay guard, recovery codes.
	if err := s.Users.SetTOTP(ctx, admin.ID, []byte{1, 2, 3}, true, []string{"r1", "r2"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Users.AdvanceTOTPStep(ctx, admin.ID, 100); !ok {
		t.Fatal("advance to 100 failed")
	}
	if ok, _ := s.Users.AdvanceTOTPStep(ctx, admin.ID, 100); ok {
		t.Fatal("replayed step accepted")
	}
	if ok, _ := s.Users.ConsumeRecoveryCode(ctx, admin.ID, "r2"); !ok {
		t.Fatal("consume r2 failed")
	}
	if ok, _ := s.Users.ConsumeRecoveryCode(ctx, admin.ID, "r2"); ok {
		t.Fatal("r2 consumed twice")
	}
	ua, _ = s.Users.GetAuth(ctx, admin.ID)
	if !ua.User.TOTPEnabled || len(ua.RecoveryHashes) != 1 || ua.RecoveryHashes[0] != "r1" {
		t.Fatalf("totp state: %+v", ua)
	}

	bob := mkUser(t, s, "bob", model.RoleUser)
	if n, _ := s.Users.CountActiveAdmins(ctx); n != 1 {
		t.Fatalf("admins = %d", n)
	}
	fa, err := s.Users.FirstAdmin(ctx)
	if err != nil || fa.ID != admin.ID {
		t.Fatalf("first admin: %v %v", fa, err)
	}

	// Sessions and tokens cascade on user deletion.
	now := Now()
	if err := s.AuthSessions.Create(ctx, &model.AuthSession{ID: "sess1", UserID: bob.ID, CreatedAt: now,
		ExpiresAt: now.Add(time.Hour), LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.APITokens.Create(ctx, &model.APIToken{UserID: bob.ID, Name: "t", TokenHash: "th"}); err != nil {
		t.Fatal(err)
	}
	s.Settings.SetJSON(ctx, bob.ID, "theme", "dark")
	if err := s.Users.Delete(ctx, bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthSessions.Get(ctx, "sess1"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("session survived user delete: %v", err)
	}
	if _, err := s.APITokens.GetByHash(ctx, "th"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("token survived user delete: %v", err)
	}
	if all, _ := s.Settings.All(ctx, bob.ID); len(all) != 0 {
		t.Fatalf("settings survived user delete: %v", all)
	}
	if err := s.Users.Delete(ctx, bob.ID); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("delete missing user: %v", err)
	}
}

func TestConnectionsVisibilityAndCRUD(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	alice := mkUser(t, s, "alice", model.RoleAdmin)
	bob := mkUser(t, s, "bob", model.RoleUser)

	priv := &model.Connection{Name: "private", Protocol: model.ProtoSSH, Host: "h1", Port: 22, OwnerID: alice.ID,
		Options: model.Options{"jumpHosts": []any{"x"}, "unknownKey": 1.5}, Tags: []string{"prod"},
		SecretsEnc: []byte("cipher"), SecretKeys: []string{"password"}}
	shared := &model.Connection{Name: "shared", Protocol: model.ProtoTelnet, Host: "h2", OwnerID: alice.ID, Shared: true}
	own := &model.Connection{Name: "bob's", Protocol: model.ProtoSSH, Host: "h3", OwnerID: bob.ID}
	for _, c := range []*model.Connection{priv, shared, own} {
		if err := s.Connections.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	names := func(list []*model.Connection) map[string]bool {
		m := map[string]bool{}
		for _, c := range list {
			m[c.Name] = true
		}
		return m
	}
	bobList, _ := s.Connections.ListVisible(ctx, bob.ID)
	if n := names(bobList); len(n) != 2 || !n["shared"] || !n["bob's"] {
		t.Fatalf("bob sees %v", n)
	}
	aliceList, _ := s.Connections.ListVisible(ctx, alice.ID)
	if n := names(aliceList); len(n) != 2 || !n["shared"] || !n["private"] {
		t.Fatalf("alice sees %v", n)
	}

	got, err := s.Connections.Get(ctx, priv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.SecretsEnc) != "cipher" || len(got.SecretKeys) != 1 || got.Secrets != nil {
		t.Fatalf("secrets columns: %+v", got)
	}
	if got.Options.Strings("jumpHosts")[0] != "x" || got.Options.Float("unknownKey") != 1.5 {
		t.Fatalf("options round-trip: %v", got.Options)
	}
	b, _ := json.Marshal(got)
	var m map[string]any
	json.Unmarshal(b, &m)
	if _, ok := m["secrets"]; ok {
		t.Fatalf("secrets serialized: %s", b)
	}
	if _, ok := m["secretKeys"]; !ok {
		t.Fatalf("secretKeys missing: %s", b)
	}

	got.Name, got.SecretsEnc, got.SecretKeys = "renamed", nil, nil
	if err := s.Connections.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Connections.Get(ctx, priv.ID)
	if got.Name != "renamed" || got.SecretsEnc != nil || len(got.SecretKeys) != 0 {
		t.Fatalf("update: %+v", got)
	}
	if err := s.Connections.TouchUsed(ctx, priv.ID, Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Connections.Get(ctx, priv.ID); got.LastUsedAt == nil {
		t.Fatal("lastUsedAt not set")
	}

	// Reorder into a folder; unknown IDs abort the whole batch.
	f := &model.Folder{Name: "F", OwnerID: alice.ID}
	s.Folders.Create(ctx, f)
	if err := s.Connections.Reorder(ctx, []ReorderItem{{ID: priv.ID, FolderID: f.ID, SortOrder: 5}}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Connections.Get(ctx, priv.ID); got.FolderID != f.ID || got.SortOrder != 5 {
		t.Fatalf("reorder: %+v", got)
	}
	if err := s.Connections.Reorder(ctx, []ReorderItem{{ID: shared.ID, SortOrder: 9}, {ID: "nope"}}); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("reorder unknown: %v", err)
	}
	if got, _ = s.Connections.Get(ctx, shared.ID); got.SortOrder == 9 {
		t.Fatal("reorder was not atomic")
	}

	n, err := s.Connections.DeleteMany(ctx, []string{priv.ID, shared.ID, "missing"})
	if err != nil || n != 2 {
		t.Fatalf("delete many: %d %v", n, err)
	}
}

func TestIdentityAndKeyReferences(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	alice := mkUser(t, s, "alice", model.RoleUser)
	key := &model.SSHKey{Name: "k", Type: "ssh-ed25519", OwnerID: alice.ID, PrivateKeyEnc: []byte("pk"), HasPassphrase: true}
	if err := s.Keys.Create(ctx, key); err != nil {
		t.Fatal(err)
	}
	ident := &model.Identity{Name: "id", Username: "root", KeyID: key.ID, OwnerID: alice.ID, SecretsEnc: []byte("s"), SecretKeys: []string{"password"}}
	if err := s.Identities.Create(ctx, ident); err != nil {
		t.Fatal(err)
	}
	c := &model.Connection{Name: "c", Protocol: model.ProtoSSH, OwnerID: alice.ID, IdentityID: ident.ID}
	if err := s.Connections.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	if used, _ := s.Keys.UsedBySharedConnection(ctx, key.ID); used {
		t.Fatal("key reported shared while connection is private")
	}
	c.Shared = true
	s.Connections.Update(ctx, c)
	if used, _ := s.Keys.UsedBySharedConnection(ctx, key.ID); !used {
		t.Fatal("key used via identity of a shared connection not detected")
	}
	// Deleting the identity unlinks it (ON DELETE SET NULL).
	if err := s.Identities.Delete(ctx, ident.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Connections.Get(ctx, c.ID)
	if got.IdentityID != "" {
		t.Fatalf("identity not unlinked: %q", got.IdentityID)
	}
	k, _ := s.Keys.Get(ctx, key.ID)
	if !k.HasPassphrase || string(k.PrivateKeyEnc) != "pk" {
		t.Fatalf("key: %+v", k)
	}
	// Referencing a missing key violates the foreign key.
	bad := &model.Connection{Name: "bad", Protocol: model.ProtoSSH, OwnerID: alice.ID, KeyID: "missing"}
	if err := s.Connections.Create(ctx, bad); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("fk violation: %v", err)
	}
}

func TestFoldersDelete(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	alice := mkUser(t, s, "alice", model.RoleUser)
	bob := mkUser(t, s, "bob", model.RoleUser)

	root := &model.Folder{Name: "root", OwnerID: alice.ID, Shared: true}
	s.Folders.Create(ctx, root)
	child := &model.Folder{Name: "child", OwnerID: alice.ID, ParentID: root.ID}
	s.Folders.Create(ctx, child)
	grand := &model.Folder{Name: "grand", OwnerID: alice.ID, ParentID: child.ID}
	s.Folders.Create(ctx, grand)
	c1 := &model.Connection{Name: "c1", Protocol: "ssh", OwnerID: alice.ID, FolderID: child.ID}
	c2 := &model.Connection{Name: "c2", Protocol: "ssh", OwnerID: alice.ID, FolderID: grand.ID}
	s.Connections.Create(ctx, c1)
	s.Connections.Create(ctx, c2)

	if in, _ := s.Folders.IsInSubtree(ctx, root.ID, grand.ID); !in {
		t.Fatal("grand not in root subtree")
	}
	if in, _ := s.Folders.IsInSubtree(ctx, grand.ID, root.ID); in {
		t.Fatal("root reported inside grand")
	}
	if n, _ := s.Folders.ForeignItemsInSubtree(ctx, root.ID, alice.ID); n != 0 {
		t.Fatalf("foreign items = %d", n)
	}
	bc := &model.Connection{Name: "bob-in-shared", Protocol: "ssh", OwnerID: bob.ID, FolderID: grand.ID}
	s.Connections.Create(ctx, bc)
	if n, _ := s.Folders.ForeignItemsInSubtree(ctx, root.ID, alice.ID); n != 1 {
		t.Fatalf("foreign items = %d, want 1", n)
	}
	s.Connections.Delete(ctx, bc.ID)

	// Non-recursive: children move up to the parent.
	if _, err := s.Folders.Delete(ctx, child.ID, false); err != nil {
		t.Fatal(err)
	}
	g, _ := s.Folders.Get(ctx, grand.ID)
	c, _ := s.Connections.Get(ctx, c1.ID)
	if g.ParentID != root.ID || c.FolderID != root.ID {
		t.Fatalf("children not moved up: grand.parent=%q c1.folder=%q", g.ParentID, c.FolderID)
	}
	// Recursive: whole subtree and its connections are removed.
	deleted, err := s.Folders.Delete(ctx, root.ID, true)
	if err != nil || len(deleted) != 2 {
		t.Fatalf("recursive delete: %v %v", deleted, err)
	}
	if list, _ := s.Folders.ListVisible(ctx, alice.ID); len(list) != 0 {
		t.Fatalf("folders left: %d", len(list))
	}
	if list, _ := s.Connections.ListVisible(ctx, alice.ID); len(list) != 0 {
		t.Fatalf("connections left: %d", len(list))
	}
}

func TestSettingsAuditMisc(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	u := mkUser(t, s, "alice", model.RoleUser)

	if err := s.Settings.Apply(ctx, ScopeGlobal, map[string]json.RawMessage{"a": []byte(`1`), "b": []byte(`{"x":1}`)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Settings.Apply(ctx, ScopeGlobal, map[string]json.RawMessage{"a": []byte(`2`)}, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Settings.All(ctx, ScopeGlobal)
	if len(all) != 1 || string(all["a"]) != "2" {
		t.Fatalf("settings: %v", all)
	}
	var v int
	if ok, err := s.Settings.GetJSON(ctx, ScopeGlobal, "a", &v); !ok || err != nil || v != 2 {
		t.Fatalf("getjson: %v %v %d", ok, err, v)
	}
	if ok, _ := s.Settings.GetJSON(ctx, ScopeGlobal, "missing", &v); ok {
		t.Fatal("missing key reported present")
	}
	if err := s.Settings.Set(ctx, ScopeGlobal, "bad", []byte(`{`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}

	for i, a := range []string{"auth.login", "auth.logout", "connection.create", "auth.login"} {
		e := &model.AuditEntry{UserID: u.ID, Username: u.Username, Action: a, Target: "t", IP: "127.0.0.1"}
		if i == 2 {
			e.Details = json.RawMessage(`{"name":"x"}`)
		}
		if err := s.Audit.Insert(ctx, e); err != nil || e.ID == 0 {
			t.Fatalf("audit insert: %v", err)
		}
	}
	list, _ := s.Audit.List(ctx, AuditFilter{Action: "auth."})
	if len(list) != 3 {
		t.Fatalf("prefix filter: %d", len(list))
	}
	page1, _ := s.Audit.List(ctx, AuditFilter{Limit: 2})
	page2, _ := s.Audit.List(ctx, AuditFilter{Limit: 2, Before: page1[1].ID})
	if len(page1) != 2 || len(page2) != 2 || page2[0].ID >= page1[1].ID {
		t.Fatalf("paging: %v %v", page1, page2)
	}
	exact, _ := s.Audit.List(ctx, AuditFilter{Action: "connection.create"})
	if len(exact) != 1 || string(exact[0].Details) != `{"name":"x"}` {
		t.Fatalf("exact filter: %+v", exact)
	}

	// Vault meta, share links, recordings, known hosts, tunnels, snippets, macros.
	if err := s.VaultMeta.Apply(ctx, map[string][]byte{"k": {1, 2}}, nil); err != nil {
		t.Fatal(err)
	}
	if b, err := s.VaultMeta.Get(ctx, "k"); err != nil || len(b) != 2 {
		t.Fatalf("vault meta: %v %v", b, err)
	}
	sl := &model.ShareLink{TokenHash: "h", SessionID: "s", OwnerID: u.ID, Mode: model.ShareRead, ExpiresAt: Now().Add(time.Hour)}
	if err := s.ShareLinks.Create(ctx, sl); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ShareLinks.GetByTokenHash(ctx, "h"); err != nil || got.ID != sl.ID {
		t.Fatalf("share: %v %v", got, err)
	}
	rec := &model.Recording{OwnerID: u.ID, SessionID: "s", Kind: model.RecordingAsciicast, Path: "/tmp/x.cast"}
	if err := s.Recordings.Create(ctx, rec); err != nil {
		t.Fatal(err)
	}
	end := Now()
	rec.EndedAt, rec.Size = &end, 42
	if err := s.Recordings.Update(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if recs, _ := s.Recordings.List(ctx, u.ID); len(recs) != 1 || recs[0].Size != 42 || recs[0].EndedAt == nil {
		t.Fatalf("recordings: %+v", recs)
	}
	kh := &model.KnownHost{Host: "Example.com", Port: 22, KeyType: "ssh-ed25519", PublicKey: "AAA"}
	s.KnownHosts.Add(ctx, kh)
	s.KnownHosts.Replace(ctx, &model.KnownHost{Host: "example.com", Port: 22, KeyType: "ssh-ed25519", PublicKey: "BBB"})
	if hosts, _ := s.KnownHosts.Find(ctx, "EXAMPLE.com", 22); len(hosts) != 1 || hosts[0].PublicKey != "BBB" {
		t.Fatalf("known hosts: %+v", hosts)
	}
	conn := &model.Connection{Name: "c", Protocol: "ssh", OwnerID: u.ID}
	s.Connections.Create(ctx, conn)
	tun := &model.Tunnel{Name: "t", Type: model.TunnelLocal, ConnectionID: conn.ID, BindHost: "127.0.0.1", BindPort: 8080, AutoStart: true, OwnerID: u.ID}
	if err := s.Tunnels.Create(ctx, tun); err != nil {
		t.Fatal(err)
	}
	if auto, _ := s.Tunnels.ListAutoStart(ctx); len(auto) != 1 || auto[0].Status.State != model.TunnelStopped {
		t.Fatalf("tunnels: %+v", auto)
	}
	s.Connections.Delete(ctx, conn.ID) // cascades to the tunnel
	if _, err := s.Tunnels.Get(ctx, tun.ID); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("tunnel survived connection delete: %v", err)
	}
	sn := &model.Snippet{Name: "s", Content: "ls", OwnerID: u.ID, Tags: []string{"a"}}
	if err := s.Snippets.Create(ctx, sn); err != nil || sn.SendMode != model.SendModePaste {
		t.Fatalf("snippet: %v", err)
	}
	mc := &model.Macro{Name: "m", OwnerID: u.ID, Steps: []model.MacroStep{{Data: "x", DelayMs: 5}}}
	s.Macros.Create(ctx, mc)
	if got, _ := s.Macros.Get(ctx, mc.ID); len(got.Steps) != 1 || got.Steps[0].DelayMs != 5 {
		t.Fatalf("macro: %+v", got)
	}
}
