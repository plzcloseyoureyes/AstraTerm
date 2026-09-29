//go:build unix

package vfs_test

import (
	"archive/zip"
	"bytes"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/nexterm/nexterm/internal/config"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/server/servertest"
)

// Regression tests of the files-backend review (security).

// Server mode: an ordinary user's WebDAV / FTP / S3 / SMB connections must not reach the NexTerm host's loopback
// (or link-local / metadata addresses) — SSRF through a file browser.
func TestFilesSSRFGuard(t *testing.T) {
	env := servertest.New(t, func(c *config.Config) { c.Mode = config.ModeServer })
	admin := env.Setup("admin", pw)
	user := env.CreateUser(admin, "bob", pw, "user")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("internal secret"))
	}))
	defer target.Close()
	u, _ := url.Parse(target.URL)
	port, _ := strconv.Atoi(u.Port())
	for _, spec := range []map[string]any{
		{"protocol": "webdav", "host": "127.0.0.1", "port": port, "options": map[string]any{"https": false}},
		{"protocol": "ftp", "host": "127.0.0.1", "port": port},
		{"protocol": "webdav", "host": "169.254.169.254", "port": 80, "options": map[string]any{"https": false}},
		{"protocol": "s3", "options": map[string]any{"endpoint": "http://127.0.0.1:" + u.Port(), "accessKeyId": "k"},
			"secrets": map[string]string{"secretAccessKey": "s"}},
	} {
		st, code := user.ErrorCode("POST", "/api/fs", map[string]any{"quick": spec})
		if st != 403 || code != "destination_blocked" {
			t.Errorf("%v: %d %s, want 403 destination_blocked", spec, st, code)
		}
	}
}

func TestFilesReadRefusesSpecialFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFOs")
	}
	env := servertest.New(t)
	admin := env.Setup("admin", pw)
	f := openLocal(t, admin)
	dir := tempDir(t)
	fifo := dir + "/pipe"
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skip(err)
	}
	// Opening a FIFO would block the request (and, over SFTP, the whole sftp-server) until a writer appears.
	for _, op := range []struct{ method, op string }{{"GET", "read"}, {"GET", "download"}} {
		if st, code := f.code(op.method, op.op, q("path", fifo), nil); st != 400 || code != "fs_error" {
			t.Errorf("%s of a FIFO: %d %s", op.op, st, code)
		}
	}
	if st, _ := f.code("POST", "checksum", nil, map[string]any{"path": fifo}); st != 400 {
		t.Errorf("checksum of a FIFO: %d", st)
	}
}

func TestFilesDownloadHeaders(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX names")
	}
	env := servertest.New(t)
	admin := env.Setup("admin", pw)
	f := openLocal(t, admin)
	dir := tempDir(t)
	name := "a\"b;c=d\r\nX-Evil: 1 é.svg"
	os.WriteFile(dir+"/"+name, []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), 0o644)
	for _, inline := range []string{"", "1"} {
		resp, _ := admin.Do("GET", f.url("download", q("path", dir+"/"+name, "inline", inline)), nil)
		if resp.StatusCode != 200 {
			t.Fatalf("download: %d", resp.StatusCode)
		}
		cd := resp.Header.Get("Content-Disposition")
		if strings.ContainsAny(cd, "\r\n") || resp.Header.Get("X-Evil") != "" || !strings.Contains(cd, "filename*=UTF-8''a%22b%3Bc%3Dd%0D%0AX-Evil%3A%201%20%C3%A9.svg") {
			t.Fatalf("Content-Disposition %q", cd)
		}
		csp := resp.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "sandbox") || !strings.Contains(csp, "frame-ancestors 'self'") {
			t.Fatalf("active content must be sandboxed: %q", csp)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("nosniff missing")
		}
	}
}

// Extraction must not write through a symlink that already exists in the destination, nor expand a zip bomb.
func TestFilesExtractHardening(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	env := servertest.New(t)
	admin := env.Setup("admin", pw)
	f := openLocal(t, admin)
	dir, outside := tempDir(t), tempDir(t)
	os.MkdirAll(dir+"/dest", 0o755)
	if err := os.Symlink(outside, dir+"/dest/link"); err != nil {
		t.Fatal(err)
	}
	os.Symlink(outside+"/victim.txt", dir+"/dest/file.txt")
	os.WriteFile(outside+"/victim.txt", []byte("keep me"), 0o644)
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for _, n := range []string{"link/evil.txt", "file.txt"} {
		w, _ := zw.Create(n)
		w.Write([]byte("evil"))
	}
	zw.Close()
	os.WriteFile(dir+"/a.zip", zb.Bytes(), 0o644)
	st, _ := f.code("POST", "extract", nil, map[string]any{"path": dir + "/a.zip", "destDir": dir + "/dest"})
	if _, err := os.Stat(outside + "/evil.txt"); err == nil {
		t.Fatal("extraction wrote through a symlink in the destination")
	}
	if b, _ := os.ReadFile(outside + "/victim.txt"); string(b) != "keep me" {
		t.Fatalf("symlinked target overwritten: %q (status %d)", b, st)
	}
	if st == 200 {
		t.Fatal("an archive member below a symlink must fail the extraction")
	}

	// A zip bomb: small archive, declared sizes far beyond the expansion limit.
	zb.Reset()
	zw = zip.NewWriter(&zb)
	data := []byte("zero")
	h := &zip.FileHeader{Name: "big.bin", Method: zip.Store, CRC32: crc32.ChecksumIEEE(data),
		CompressedSize64: uint64(len(data)), UncompressedSize64: 3 << 30}
	w, _ := zw.CreateRaw(h)
	w.Write(data)
	zw.Close()
	os.WriteFile(dir+"/bomb.zip", zb.Bytes(), 0o644)
	if st, _ := f.code("POST", "extract", nil, map[string]any{"path": dir + "/bomb.zip", "destDir": dir + "/bomb"}); st < 400 {
		t.Fatalf("zip bomb extracted: %d", st)
	}
	if _, err := os.Stat(dir + "/bomb/big.bin"); err == nil {
		t.Fatal("zip bomb member written")
	}
	_ = model.FileTypeDir
}
