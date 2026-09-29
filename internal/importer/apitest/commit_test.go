package apitest_test

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/termstead/termstead/internal/config"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/server/servertest"
)

type commitFull struct {
	Created, Updated, Skipped, FoldersCreated, KeysImported, KnownHostsAdded int
	KnownHostsConflicts, GatewaysCreated, IdentitiesCreated, SnippetsCreated int
	Warnings                                                                 []string
	ConnectionIDs                                                            []string `json:"connectionIds"`
	FolderIDs                                                                []string `json:"folderIds"`
}

func connsByName(t *testing.T, c *servertest.Client) map[string]model.Connection {
	t.Helper()
	var list []model.Connection
	c.MustJSON("GET", "/api/connections", nil, &list)
	out := map[string]model.Connection{}
	for _, x := range list {
		out[x.Name] = x
	}
	return out
}

// isolateHome points the importer's home-directory lookups (discovery, IdentityFile ~) at a temp dir.
func isolateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}

func TestMobaXtermGatewaysBecomeSavedConnections(t *testing.T) {
	isolateHome(t)
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	content := b64(fixture(t, "mobaxterm_full.mxtsessions"))

	var prev previewResp
	admin.MustJSON("POST", "/api/import/preview", map[string]any{"format": "auto", "content": content, "base64": true}, &prev)
	if prev.Counts.Connections != 16 || prev.Counts.KnownHosts != 3 || prev.Counts.Unsupported != 7 {
		t.Fatalf("preview counts = %+v", prev.Counts)
	}
	var res commitFull
	admin.MustJSON("POST", "/api/import/commit", map[string]any{"format": "auto", "content": content, "base64": true}, &res)
	if res.Created != 16 || res.KnownHostsAdded != 3 || res.GatewaysCreated != 2 {
		t.Fatalf("commit = %+v", res)
	}
	if len(res.ConnectionIDs) != 18 || len(res.FolderIDs) != res.FoldersCreated {
		t.Errorf("ids: %d connections, %d folders (%d created)", len(res.ConnectionIDs), len(res.FolderIDs), res.FoldersCreated)
	}
	byName := connsByName(t, admin)
	gw := byName["jump@gw.example.com:2200 (SSH gateway)"]
	if gw.ID == "" || gw.Protocol != model.ProtoSSH || gw.Host != "gw.example.com" || gw.Port != 2200 || gw.Username != "jump" {
		t.Fatalf("RDP gateway connection: %+v", gw)
	}
	win01, win02 := byName["win01"], byName["win02"]
	if win01.Options.String("sshTunnelVia") != gw.ID || win02.Options.String("sshTunnelVia") != gw.ID {
		t.Errorf("both RDP sessions must use the one saved gateway: %v / %v", win01.Options["sshTunnelVia"], win02.Options["sshTunnelVia"])
	}
	// VNC with a two-hop gateway: the saved gateway is the last hop and carries the first as its own jump chain.
	vgw := byName["u2@hop2.example.com:2022 (SSH gateway)"]
	if byName["console"].Options.String("sshTunnelVia") != vgw.ID || !slices.Equal(vgw.Options.Strings("jumpHosts"), []string{"u1@hop1.example.com"}) {
		t.Errorf("VNC gateway chain: tunnelVia=%v gw=%+v", byName["console"].Options["sshTunnelVia"], vgw)
	}
	// SSH keeps inline hops (the gw2 key file does not exist here → inline + warning).
	if jh := byName["db1"].Options.Strings("jumpHosts"); !slices.Equal(jh, []string{"u1@gw1.example.com", "jumpuser@gw2.example.com:2222"}) {
		t.Errorf("db1 jumpHosts = %v", jh)
	}
	// Keys referenced by MobaXterm paths are unreadable here: key auth falls back to auto.
	if byName["web1"].AuthMethod != model.AuthAuto {
		t.Errorf("web1 auth = %q", byName["web1"].AuthMethod)
	}
	// Re-import: everything is a duplicate, gateways are reused (not duplicated).
	var res2 commitFull
	admin.MustJSON("POST", "/api/import/commit", map[string]any{"format": "auto", "content": content, "base64": true}, &res2)
	if res2.Created != 0 || res2.Skipped != 16 || res2.GatewaysCreated != 0 || res2.KnownHostsAdded != 0 {
		t.Errorf("re-import = %+v", res2)
	}
}

func TestSSHConfigProxyJumpAndKeyFile(t *testing.T) {
	home := isolateHome(t)
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)

	// A real (unencrypted) key file referenced by IdentityFile.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	blk, err := ssh.MarshalPrivateKey(priv, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_test"), pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := "Host bastion\n  HostName bastion.example.com\n  User jump\n  IdentityFile ~/.ssh/id_test\n\nHost app\n  HostName app.internal\n  ProxyJump bastion,ops@edge.example.com:2022\n"
	var res commitFull
	admin.MustJSON("POST", "/api/import/commit", map[string]any{"format": "ssh_config", "content": b64([]byte(cfg)), "base64": true}, &res)
	if res.Created != 2 || res.KeysImported != 1 {
		t.Fatalf("commit = %+v", res)
	}
	byName := connsByName(t, admin)
	b, app := byName["bastion"], byName["app"]
	if b.KeyID == "" || b.AuthMethod != model.AuthKey {
		t.Errorf("bastion key: %+v", b)
	}
	if jh := app.Options.Strings("jumpHosts"); !slices.Equal(jh, []string{b.ID, "ops@edge.example.com:2022"}) {
		t.Errorf("app jumpHosts = %v (bastion %s)", jh, b.ID)
	}
	// Deselecting the bastion: its address is used for the hop; since it has a readable key, the hop becomes a saved
	// gateway connection holding that (de-duplicated) key.
	admin.MustJSON("POST", "/api/connections/bulk-delete", map[string]any{"ids": []string{b.ID, app.ID}}, nil)
	var prev struct {
		Connections []struct {
			ID, Name string
		} `json:"connections"`
	}
	admin.MustJSON("POST", "/api/import/preview", map[string]any{"format": "ssh_config", "content": b64([]byte(cfg)), "base64": true}, &prev)
	var appID string
	for _, c := range prev.Connections {
		if c.Name == "app" {
			appID = c.ID
		}
	}
	admin.MustJSON("POST", "/api/import/commit", map[string]any{"format": "ssh_config", "content": b64([]byte(cfg)), "base64": true, "selectedIds": []string{appID}}, &res)
	after := connsByName(t, admin)
	jh := after["app"].Options.Strings("jumpHosts")
	gw := after["jump@bastion.example.com (SSH gateway)"]
	if len(jh) != 2 || jh[0] != gw.ID || gw.Host != "bastion.example.com" || gw.KeyID != b.KeyID || jh[1] != "ops@edge.example.com:2022" {
		t.Errorf("deselected keyed jump host: jumpHosts=%v gateway=%+v", jh, gw)
	}
	if _, ok := after["bastion"]; ok {
		t.Errorf("the deselected bastion itself must not be imported")
	}
}

func TestKnownHostsOnlyImportAndConflicts(t *testing.T) {
	isolateHome(t)
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	kh := fixture(t, "known_hosts")
	var res commitFull
	admin.MustJSON("POST", "/api/import/commit", map[string]any{"format": "known_hosts", "content": b64(kh), "base64": true, "selectedIds": []string{}}, &res)
	if res.KnownHostsAdded != 2 || res.Created != 0 {
		t.Fatalf("known hosts import = %+v", res)
	}
	admin.MustJSON("POST", "/api/import/commit", map[string]any{"format": "known_hosts", "content": b64(kh), "base64": true}, &res)
	if res.KnownHostsAdded != 0 || res.KnownHostsConflicts != 0 {
		t.Errorf("re-import = %+v", res)
	}
	// A different key for an already-trusted host is never added silently.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	pub, _ := ssh.NewPublicKey(other.Public())
	evil := "github.example.com " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + "\n"
	var prev struct {
		KnownHosts []struct {
			Conflict, Duplicate bool
		} `json:"knownHosts"`
	}
	admin.MustJSON("POST", "/api/import/preview", map[string]any{"format": "known_hosts", "content": b64([]byte(evil)), "base64": true}, &prev)
	if len(prev.KnownHosts) != 1 || !prev.KnownHosts[0].Conflict {
		t.Errorf("preview must flag the conflict: %+v", prev.KnownHosts)
	}
	admin.MustJSON("POST", "/api/import/commit", map[string]any{"format": "known_hosts", "content": b64([]byte(evil)), "base64": true}, &res)
	if res.KnownHostsAdded != 0 || res.KnownHostsConflicts != 1 {
		t.Errorf("conflicting key = %+v", res)
	}
}

func TestServerModeKnownHostsAdminOnly(t *testing.T) {
	isolateHome(t)
	env := servertest.New(t, func(c *config.Config) { c.Mode = config.ModeServer })
	admin := env.Setup("admin", adminPass)
	bob := env.CreateUser(admin, "bob", "bob correct horse staple", "user")
	var res commitFull
	bob.MustJSON("POST", "/api/import/commit", map[string]any{"format": "known_hosts", "content": b64(fixture(t, "known_hosts")), "base64": true}, &res)
	if res.KnownHostsAdded != 0 || len(res.Warnings) == 0 {
		t.Errorf("non-admin known hosts import in server mode = %+v", res)
	}
	if code, _ := bob.ErrorCode("POST", "/api/import/preview", map[string]any{"format": "ssh_config", "path": "/etc/passwd"}); code != http.StatusForbidden {
		t.Errorf("path imports are desktop-only: %d", code)
	}
	var disc struct {
		Supported bool `json:"supported"`
	}
	bob.MustJSON("GET", "/api/import/discover", nil, &disc)
	if disc.Supported {
		t.Errorf("discover must be unsupported in server mode")
	}
}

func TestExportPostAndCrossUserReferenceRemap(t *testing.T) {
	isolateHome(t)
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	var bastion, app, desk model.Connection
	admin.MustJSON("POST", "/api/connections", map[string]any{"name": "bastion", "protocol": "ssh", "host": "b.example.com", "port": 22,
		"secrets": map[string]string{"password": "bastion-pw"}}, &bastion)
	admin.MustJSON("POST", "/api/connections", map[string]any{"name": "app", "protocol": "ssh", "host": "app.internal", "port": 22,
		"options": map[string]any{"jumpHosts": []string{bastion.ID}}}, &app)
	admin.MustJSON("POST", "/api/connections", map[string]any{"name": "desk", "protocol": "vnc", "host": "desk.internal", "port": 5900,
		"options": map[string]any{"sshTunnelVia": bastion.ID}}, &desk)

	// POST keeps the passphrase out of the URL; short passphrases are refused.
	if code, _ := admin.ErrorCode("POST", "/api/export", map[string]any{"format": "json", "includeSecrets": true, "passphrase": "short"}); code != http.StatusBadRequest {
		t.Errorf("short passphrase: %d", code)
	}
	resp, data := admin.Do("POST", "/api/export", map[string]any{"format": "json", "includeSecrets": true, "passphrase": "export passphrase"})
	if resp.StatusCode != http.StatusOK || !isEncrypted(data) {
		t.Fatalf("POST export: %d %s", resp.StatusCode, firstBytes(data))
	}
	if !strings.Contains(resp.Header.Get("Content-Disposition"), ".secret.json") {
		t.Errorf("disposition %q", resp.Header.Get("Content-Disposition"))
	}

	bob := env.CreateUser(admin, "bob", "bob correct horse staple", "user")
	if code, ec := bob.ErrorCode("POST", "/api/import/preview", map[string]any{"format": "json", "content": b64(data), "base64": true,
		"options": map[string]any{"passphrase": "wrong passphrase"}}); code != http.StatusForbidden || ec != "wrong_password" {
		t.Errorf("wrong passphrase = %d %s", code, ec)
	}
	var res commitFull
	bob.MustJSON("POST", "/api/import/commit", map[string]any{"format": "json", "content": b64(data), "base64": true,
		"options": map[string]any{"passphrase": "export passphrase"}}, &res)
	if res.Created != 3 {
		t.Fatalf("bob import = %+v", res)
	}
	byName := connsByName(t, bob)
	bb := byName["bastion"]
	if bb.ID == "" || bb.ID == bastion.ID {
		t.Fatalf("bob must get his own copy: %+v", bb)
	}
	if jh := byName["app"].Options.Strings("jumpHosts"); !slices.Equal(jh, []string{bb.ID}) {
		t.Errorf("jumpHosts not remapped to bob's bastion: %v (want %s)", jh, bb.ID)
	}
	if v := byName["desk"].Options.String("sshTunnelVia"); v != bb.ID {
		t.Errorf("sshTunnelVia not remapped: %v", v)
	}
	if !slices.Contains(bb.SecretKeys, "password") {
		t.Errorf("encrypted export secrets not imported: %v", bb.SecretKeys)
	}

	// ssh_config export names the jump alias.
	_, cfg := admin.Do("POST", "/api/export", map[string]any{"format": "ssh_config"})
	if !strings.Contains(string(cfg), "ProxyJump bastion") {
		t.Errorf("ssh_config export:\n%s", cfg)
	}
}

func TestPlaintextJSONWithSecretsIsNotTrusted(t *testing.T) {
	isolateHome(t)
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	doc := `{"format":"termstead-export","version":1,"connections":[{"id":"x","name":"planted","protocol":"ssh","host":"p.example.com","secrets":{"password":"planted-pw"}}]}`
	var res commitFull
	admin.MustJSON("POST", "/api/import/commit", map[string]any{"format": "json", "content": b64([]byte(doc)), "base64": true}, &res)
	if res.Created != 1 {
		t.Fatalf("commit = %+v", res)
	}
	if c := connsByName(t, admin)["planted"]; len(c.SecretKeys) != 0 {
		t.Errorf("plaintext secrets were stored: %v", c.SecretKeys)
	}
}

func TestSyncToggleAdminOnlyAndBackupPost(t *testing.T) {
	isolateHome(t)
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	bob := env.CreateUser(admin, "bob", "bob correct horse staple", "user")
	if code, _ := bob.ErrorCode("POST", "/api/import/sync", map[string]any{"enabled": true}); code != http.StatusForbidden {
		t.Errorf("non-admin sync toggle = %d", code)
	}
	resp, arch := admin.Do("POST", "/api/admin/backup", map[string]any{"includeSystemKey": true, "passphrase": "backup passphrase"})
	if resp.StatusCode != http.StatusOK || !isEncrypted(arch) {
		t.Fatalf("POST backup: %d", resp.StatusCode)
	}
	if code, _ := admin.ErrorCode("POST", "/api/admin/backup", map[string]any{"includeSystemKey": true}); code != http.StatusBadRequest {
		t.Errorf("system key without passphrase = %d", code)
	}
	if code, _ := bob.ErrorCode("POST", "/api/admin/backup", map[string]any{}); code != http.StatusForbidden {
		t.Errorf("non-admin backup = %d", code)
	}
	// Encrypted restore: passphrase required, wrong one refused, right one stages db + key.
	if code, ec := admin.ErrorCode("POST", "/api/admin/restore", map[string]any{"content": b64(arch)}); code != http.StatusUnprocessableEntity || ec != "passphrase_required" {
		t.Errorf("restore without passphrase = %d %s", code, ec)
	}
	var rr restoreResp
	admin.MustJSON("POST", "/api/admin/restore", map[string]any{"content": b64(arch), "passphrase": "backup passphrase"}, &rr)
	if !rr.Staged {
		t.Fatalf("restore = %+v", rr)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(rr.StagedDBPath), "system.key")); err != nil {
		t.Errorf("staged system.key missing: %v", err)
	}
	// Restore hardening: a zip bomb / foreign entries are refused.
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, _ := zw.Create("termstead.db")
	_, _ = w.Write([]byte("SQLite format 3\x00"))
	w2, _ := zw.Create("../escape.txt")
	_, _ = w2.Write([]byte("x"))
	_ = zw.Close()
	if code, _ := admin.ErrorCode("POST", "/api/admin/restore", map[string]any{"content": b64(zb.Bytes())}); code != http.StatusBadRequest {
		t.Errorf("foreign zip entry = %d", code)
	}
	if code, _ := admin.ErrorCode("POST", "/api/admin/restore", map[string]any{"content": b64([]byte("SQLite format 3\x00garbage"))}); code != http.StatusBadRequest {
		t.Errorf("corrupt database = %d", code)
	}
}

func TestDiscoverAndPathImport(t *testing.T) {
	home := isolateHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte("Host h1\n  HostName h1.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "Downloads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "Downloads", "team.mxtsessions"), fixture(t, "mobaxterm.mxtsessions"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	var out struct {
		Supported bool `json:"supported"`
		Files     []struct {
			Path, Format, Label string
		} `json:"files"`
	}
	admin.MustJSON("GET", "/api/import/discover", nil, &out)
	if !out.Supported || len(out.Files) < 2 || out.Files[0].Format != "ssh_config" {
		t.Fatalf("discover = %+v", out)
	}
	var moba string
	for _, f := range out.Files {
		if f.Format == "mobaxterm" {
			moba = f.Path
		}
	}
	var prev previewResp
	admin.MustJSON("POST", "/api/import/preview", map[string]any{"format": "auto", "path": moba}, &prev)
	if prev.Counts.Connections < 8 {
		t.Errorf("path preview = %+v", prev.Counts)
	}
	if code, _ := admin.ErrorCode("POST", "/api/import/preview", map[string]any{"format": "auto", "path": filepath.Join(home, ".ssh", "..", ".ssh", "id_rsa")}); code != http.StatusBadRequest {
		t.Errorf("undiscovered path = %d", code)
	}
}

func TestCommitIDsAndEmptySelection(t *testing.T) {
	isolateHome(t)
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	content := b64(fixture(t, "ssh_config"))
	var res commitFull
	admin.MustJSON("POST", "/api/import/commit", map[string]any{"format": "ssh_config", "content": content, "base64": true, "selectedIds": []string{}}, &res)
	if res.Created != 0 || len(res.ConnectionIDs) != 0 {
		t.Errorf("an empty selection imports nothing: %+v", res)
	}
	raw, _ := json.Marshal(res)
	if !strings.Contains(string(raw), `"connectionIds":[]`) {
		t.Errorf("connectionIds must be an array: %s", raw)
	}
}

func TestSSHConfigLiveSync(t *testing.T) {
	home := isolateHome(t)
	cfgPath := filepath.Join(home, ".ssh", "config")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(cfgPath, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("Host a\n  HostName a1.example.com\n  User alice\n\nHost b\n  HostName b.example.com\n  ProxyJump a\n")
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)

	type status struct {
		Enabled   bool   `json:"enabled"`
		FolderID  string `json:"folderId"`
		Synced    int    `json:"synced"`
		LastError string `json:"lastError"`
	}
	var st status
	admin.MustJSON("POST", "/api/import/sync", map[string]any{"enabled": true}, &st)
	if !st.Enabled || st.Synced != 2 || st.FolderID == "" || st.LastError != "" {
		t.Fatalf("sync status = %+v", st)
	}
	byName := connsByName(t, admin)
	a, b := byName["a"], byName["b"]
	if a.FolderID != st.FolderID || !slices.Equal(b.Options.Strings("jumpHosts"), []string{a.ID}) {
		t.Fatalf("synced a=%+v b=%+v", a, b)
	}
	// The user renames and decorates a synced connection and adds an own connection to the folder.
	admin.MustJSON("PATCH", "/api/connections/"+a.ID, map[string]any{"name": "Alpha", "color": "#22c55e", "favorite": true}, nil)
	var own model.Connection
	admin.MustJSON("POST", "/api/connections", map[string]any{"name": "mine", "protocol": "ssh", "host": "m.example.com", "port": 22, "folderId": st.FolderID}, &own)

	// The file changes: a's HostName moves, b disappears.
	write("Host a\n  HostName a2.example.com\n  User alice\n")
	admin.MustJSON("POST", "/api/import/sync", map[string]any{"enabled": true}, &st)
	byName = connsByName(t, admin)
	alpha := byName["Alpha"]
	if alpha.ID != a.ID || alpha.Host != "a2.example.com" || alpha.Color != "#22c55e" || !alpha.Favorite {
		t.Errorf("renamed synced connection must be updated in place, keeping the user's edits: %+v", alpha)
	}
	if _, ok := byName["a"]; ok {
		t.Errorf("a renamed connection must not be re-created")
	}
	if _, ok := byName["b"]; ok {
		t.Errorf("b vanished from the file and must be pruned")
	}
	if _, ok := byName["mine"]; !ok {
		t.Errorf("the user's own connection in the sync folder must never be touched")
	}
	// An emptied file removes every synced host.
	write("# nothing left\n")
	admin.MustJSON("POST", "/api/import/sync", map[string]any{"enabled": true}, &st)
	if _, ok := connsByName(t, admin)["Alpha"]; ok || st.Synced != 0 {
		t.Errorf("empty config must prune synced hosts: %+v", st)
	}
	admin.MustJSON("POST", "/api/import/sync", map[string]any{"enabled": false}, &st)
	if st.Enabled {
		t.Errorf("disable failed")
	}
}
