package apitest_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/server/servertest"
	"github.com/termstead/termstead/internal/vault"
)

const adminPass = "correct horse battery staple"

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// isEncrypted reports whether data is a Termstead passphrase-encrypted envelope (matches importer/crypto.go).
func isEncrypted(data []byte) bool {
	return bytes.Contains(data, []byte(`"envelope"`)) && bytes.Contains(data, []byte("termstead-encrypted"))
}

type previewCounts struct {
	Folders, Connections, Keys, KnownHosts, Duplicates, Unsupported int
}
type previewResp struct {
	Counts      previewCounts    `json:"counts"`
	Connections []map[string]any `json:"connections"`
	Warnings    []string         `json:"warnings"`
	Duplicates  []map[string]any `json:"duplicates"`
}
type commitResp struct {
	Created, Updated, Skipped, FoldersCreated, KeysImported, KnownHostsAdded int
	Warnings                                                                 []string
}
type restoreResp struct {
	Staged       bool     `json:"staged"`
	StagedDBPath string   `json:"stagedDbPath"`
	Instructions []string `json:"instructions"`
	Warnings     []string `json:"warnings"`
}

func TestPreviewCommitMobaXterm(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	content := b64(fixture(t, "mobaxterm.mxtsessions"))

	var prev previewResp
	admin.MustJSON("POST", "/api/import/preview", map[string]any{"format": "mobaxterm", "content": content, "base64": true}, &prev)
	if prev.Counts.Connections < 8 {
		t.Fatalf("expected >=8 connections, got %d", prev.Counts.Connections)
	}
	if prev.Counts.Unsupported == 0 {
		t.Errorf("expected unsupported sessions to be reported")
	}

	var res commitResp
	admin.MustJSON("POST", "/api/import/commit", map[string]any{"format": "mobaxterm", "content": content, "base64": true}, &res)
	if res.Created != prev.Counts.Connections || res.FoldersCreated == 0 {
		t.Fatalf("commit mismatch: created=%d folders=%d (want created=%d)", res.Created, res.FoldersCreated, prev.Counts.Connections)
	}

	var conns []model.Connection
	admin.MustJSON("GET", "/api/connections", nil, &conns)
	web := findConn(t, conns, "web1")
	if web.Host != "web1.example.com" || web.Port != 2222 {
		t.Errorf("web1 mismatch: %+v", web)
	}
	var folders []model.Folder
	admin.MustJSON("GET", "/api/folders", nil, &folders)
	if pathOfFolder(folders, web.FolderID) != "Production" {
		t.Errorf("web1 folder = %q", pathOfFolder(folders, web.FolderID))
	}
	if countFolders(folders, "Network") != 1 {
		t.Errorf("expected exactly one Network folder")
	}

	// Idempotent re-commit.
	var res2 commitResp
	admin.MustJSON("POST", "/api/import/commit", map[string]any{"format": "mobaxterm", "content": content, "base64": true}, &res2)
	if res2.Created != 0 || res2.Skipped == 0 {
		t.Errorf("re-commit not idempotent: created=%d skipped=%d", res2.Created, res2.Skipped)
	}
}

func TestExportImportEncryptedRoundTrip(t *testing.T) {
	vault.DefaultKDF.Time, vault.DefaultKDF.MemKiB, vault.DefaultKDF.Threads = 1, 1024, 1
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)

	var created model.Connection
	admin.MustJSON("POST", "/api/connections", map[string]any{
		"name": "secret-host", "protocol": "ssh", "host": "s.example.com", "port": 22, "username": "root",
		"secrets": map[string]string{"password": "s3cr3t-pw"},
	}, &created)

	resp, data := admin.Do("GET", "/api/export?format=json&includeSecrets=1&passphrase=export-pass", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export status %d: %s", resp.StatusCode, data)
	}
	if !isEncrypted(data) {
		t.Fatalf("export is not encrypted: %s", firstBytes(data))
	}

	if code, ec := admin.ErrorCode("POST", "/api/import/preview", map[string]any{"format": "json", "content": b64(data), "base64": true}); code != 422 || ec != "passphrase_required" {
		t.Fatalf("expected 422 passphrase_required, got %d %s", code, ec)
	}

	var res commitResp
	admin.MustJSON("POST", "/api/import/commit", map[string]any{
		"format": "json", "content": b64(data), "base64": true, "dedupe": "duplicate",
		"options": map[string]any{"passphrase": "export-pass"},
	}, &res)
	if res.Created == 0 {
		t.Fatalf("nothing imported: %+v", res)
	}

	var conns []model.Connection
	admin.MustJSON("GET", "/api/connections", nil, &conns)
	var imported *model.Connection
	for i := range conns {
		if conns[i].ID != created.ID && conns[i].Host == "s.example.com" {
			imported = &conns[i]
		}
	}
	if imported == nil {
		t.Fatalf("imported copy not found")
	}
	if !contains(imported.SecretKeys, "password") {
		t.Fatalf("imported connection missing password secret: %v", imported.SecretKeys)
	}
	_, secrets, err := env.Server.Deps.ResolveConnection(context.Background(), userOf(t, admin), imported.ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if secrets["password"] != "s3cr3t-pw" {
		t.Fatalf("secret round-trip failed: %q", secrets["password"])
	}
}

func TestPlaintextExportOmitsSecrets(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	admin.MustJSON("POST", "/api/connections", map[string]any{
		"name": "h", "protocol": "ssh", "host": "h.example.com", "port": 22, "username": "u",
		"secrets": map[string]string{"password": "leaky"},
	}, nil)
	_, data := admin.Do("GET", "/api/export?format=json&includeSecrets=0", nil)
	if strings.Contains(string(data), "leaky") {
		t.Fatalf("plaintext export leaked a secret")
	}
	if code, _ := admin.ErrorCode("GET", "/api/export?format=json&includeSecrets=1", nil); code != http.StatusBadRequest {
		t.Fatalf("expected 400 without passphrase, got %d", code)
	}
}

func TestBackupRestore(t *testing.T) {
	vault.DefaultKDF.Time, vault.DefaultKDF.MemKiB, vault.DefaultKDF.Threads = 1, 1024, 1
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	admin.MustJSON("POST", "/api/connections", map[string]any{"name": "bk", "protocol": "ssh", "host": "bk.example.com", "port": 22}, nil)

	resp, dbData := admin.Do("GET", "/api/admin/backup", nil)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(dbData), "SQLite format 3\x00") {
		t.Fatalf("plain backup failed: status=%d", resp.StatusCode)
	}
	resp2, arch := admin.Do("GET", "/api/admin/backup?includeSystemKey=1&passphrase=bk-passphrase", nil)
	if resp2.StatusCode != http.StatusOK || !isEncrypted(arch) {
		t.Fatalf("encrypted backup failed: status=%d encrypted=%v", resp2.StatusCode, isEncrypted(arch))
	}

	var rr restoreResp
	admin.MustJSON("POST", "/api/admin/restore", map[string]any{"content": b64(dbData)}, &rr)
	if !rr.Staged || rr.StagedDBPath == "" {
		t.Fatalf("restore not staged: %+v", rr)
	}
	if _, err := os.Stat(rr.StagedDBPath); err != nil {
		t.Fatalf("staged db missing: %v", err)
	}

	user := env.CreateUser(admin, "bob", "bob correct horse staple", "user")
	if code, _ := user.ErrorCode("GET", "/api/admin/backup", nil); code != http.StatusForbidden {
		t.Errorf("non-admin backup should be 403, got %d", code)
	}
}

// TestLargeImportNotTruncated posts a body well over the default 2 MiB JSON bind cap to confirm preview/commit use
// the route's larger body limit (a real MobaXterm.ini or backup can exceed 2 MiB).
func TestLargeImportNotTruncated(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)

	var sb strings.Builder
	sb.WriteString("[Bookmarks]\r\nSubRep=\r\nImgNum=42\r\n")
	const n = 12000 // ~2.5 MiB of session lines
	for i := 0; i < n; i++ {
		sb.WriteString("host")
		sb.WriteString(itoa(i))
		sb.WriteString("=#109#0%h")
		sb.WriteString(itoa(i))
		sb.WriteString(".example.com%22%user%%-1%-1%%%%%0%0%0%%%-1%0%0%0%%1080%%0%0%1#MobaFont%10%0%0%-1%15%236,236,236%30,30,30%180,180,192%0%-1%0%%xterm%-1%0%_Std_Colors_0_%80%24%0%1%-1%<none>%%0#0##-1\r\n")
	}
	content := b64([]byte(sb.String()))
	if len(content) <= 2<<20 {
		t.Fatalf("test body is only %d bytes; expected > 2 MiB", len(content))
	}
	var prev previewResp
	admin.MustJSON("POST", "/api/import/preview", map[string]any{"format": "mobaxterm", "content": content, "base64": true}, &prev)
	if prev.Counts.Connections != n {
		t.Fatalf("expected %d connections, got %d (body truncated?)", n, prev.Counts.Connections)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestDiscoverDesktop(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	var out struct {
		Supported bool `json:"supported"`
	}
	admin.MustJSON("GET", "/api/import/discover", nil, &out)
	if !out.Supported {
		t.Fatalf("discover should be supported in desktop mode")
	}
}

// ---- helpers ------------------------------------------------------------------------------------------------------

func findConn(t *testing.T, conns []model.Connection, name string) model.Connection {
	t.Helper()
	for _, c := range conns {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("connection %q not found", name)
	return model.Connection{}
}

func pathOfFolder(folders []model.Folder, id string) string {
	byID := map[string]model.Folder{}
	for _, f := range folders {
		byID[f.ID] = f
	}
	var parts []string
	for id != "" {
		f, ok := byID[id]
		if !ok {
			break
		}
		parts = append([]string{f.Name}, parts...)
		id = f.ParentID
	}
	return strings.Join(parts, "/")
}

func countFolders(folders []model.Folder, name string) int {
	n := 0
	for _, f := range folders {
		if f.Name == name {
			n++
		}
	}
	return n
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func userOf(t *testing.T, c *servertest.Client) *model.User {
	t.Helper()
	var st struct {
		User *model.User `json:"user"`
	}
	c.MustJSON("GET", "/api/auth/state", nil, &st)
	if st.User == nil {
		t.Fatalf("no authenticated user")
	}
	return st.User
}

func firstBytes(b []byte) string {
	if len(b) > 120 {
		return string(b[:120])
	}
	return string(b)
}
