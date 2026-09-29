package vfs_test

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nexterm/nexterm/internal/config"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/server/servertest"
	"github.com/nexterm/nexterm/internal/transfer"
	"github.com/nexterm/nexterm/internal/vfs"
)

const pw = "correct horse battery staple"

type fsClient struct {
	t  *testing.T
	c  *servertest.Client
	id string
}

// handleView is the JSON reply of POST /api/fs (vfs.Handle itself holds a mutex).
type handleView struct {
	ID, Kind, Driver, Home, UserHome, Root, Label, SessionID, ConnectionID string
	Capabilities                                                           vfs.Capabilities
}

func openLocal(t *testing.T, c *servertest.Client) *fsClient {
	t.Helper()
	var h handleView
	c.MustJSON("POST", "/api/fs", map[string]any{"local": true}, &h)
	if h.ID == "" || h.Kind != "local" || h.Driver != "local" || !h.Capabilities.Checksum {
		t.Fatalf("handle %+v", h)
	}
	return &fsClient{t: t, c: c, id: h.ID}
}

func (f *fsClient) url(op string, q url.Values) string {
	u := "/api/fs/" + f.id + "/" + op
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

func (f *fsClient) must(method, op string, q url.Values, body, out any) {
	f.t.Helper()
	f.c.MustJSON(method, f.url(op, q), body, out)
}

func (f *fsClient) code(method, op string, q url.Values, body any) (int, string) {
	f.t.Helper()
	return f.c.ErrorCode(method, f.url(op, q), body)
}

func q(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Add(kv[i], kv[i+1])
	}
	return v
}

func tempDir(t *testing.T) string {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(d)
}

func TestFilesAPILocal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	env := servertest.New(t)
	admin := env.Setup("admin", pw)
	f := openLocal(t, admin)
	dir := tempDir(t)

	// mkdir / touch / list / stat
	var e model.FileEntry
	f.must("POST", "mkdir", nil, map[string]any{"path": dir + "/a/b", "parents": true}, &e)
	if e.Type != "dir" || e.Perm[0] != 'd' {
		t.Fatalf("mkdir entry %+v", e)
	}
	if st, code := f.code("POST", "mkdir", nil, map[string]any{"path": dir + "/a"}); st != 409 || code != "exists" {
		t.Fatalf("mkdir existing: %d %s", st, code)
	}
	f.must("POST", "touch", nil, map[string]any{"path": dir + "/a/.hidden"}, &e)
	if !e.Hidden || e.Type != "file" {
		t.Fatalf("touch %+v", e)
	}
	os.WriteFile(dir+"/a/z.txt", []byte("zzz"), 0o644)
	var list struct {
		Path, Parent string
		Entries      []model.FileEntry
	}
	f.must("GET", "list", q("path", dir+"/a"), nil, &list)
	if list.Path != dir+"/a" || list.Parent != dir || len(list.Entries) != 3 || list.Entries[0].Name != "b" {
		t.Fatalf("list %+v", list)
	}
	for _, e := range list.Entries {
		if e.Owner == "" || e.UID == nil || e.Perm == "" || e.Mode&0o170000 == 0 {
			t.Fatalf("entry fields missing: %+v", e)
		}
	}
	if st, code := f.code("GET", "list", q("path", dir+"/missing"), nil); st != 404 || code != "not_found" {
		t.Fatalf("missing dir: %d %s", st, code)
	}

	// upload: two chunks, resume status, offset mismatch, final rename, mtime
	target := dir + "/a/up.txt"
	resp, _ := admin.Do("PUT", f.url("upload", q("path", target, "offset", "0")), []byte("hello "))
	if resp.StatusCode != 200 {
		t.Fatalf("chunk 1: %d", resp.StatusCode)
	}
	var st struct{ Size, Offset int64 }
	f.must("GET", "upload", q("path", target), nil, &st)
	if st.Size != 6 || st.Offset != 6 {
		t.Fatalf("upload status %+v", st)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("target must not exist before final")
	}
	r2, body := admin.Do("PUT", f.url("upload", q("path", target, "offset", "9")), []byte("x"))
	var mm struct {
		Code string
		Size int64
	}
	json.Unmarshal(body, &mm)
	if r2.StatusCode != 409 || mm.Code != "offset_mismatch" || mm.Size != 6 || r2.Header.Get("Upload-Offset") != "6" {
		t.Fatalf("mismatch: %d %s", r2.StatusCode, body)
	}
	resp, body = admin.Do("PUT", f.url("upload", q("path", target, "offset", "6", "final", "1", "mtime", "1600000000000")), []byte("world"))
	var ur struct {
		Size  int64
		Final bool
		Entry *model.FileEntry
	}
	json.Unmarshal(body, &ur)
	if resp.StatusCode != 200 || ur.Size != 11 || !ur.Final || ur.Entry == nil || ur.Entry.Mtime.Unix() != 1600000000 {
		t.Fatalf("final chunk: %d %s", resp.StatusCode, body)
	}
	if b, _ := os.ReadFile(target); string(b) != "hello world" {
		t.Fatalf("uploaded %q", b)
	}
	if _, err := os.Stat(target + vfs.PartSuffix); !os.IsNotExist(err) {
		t.Fatal("part file left behind")
	}

	// download: full, range, inline, zip
	resp, body = admin.Do("GET", f.url("download", q("path", target)), nil)
	if resp.StatusCode != 200 || string(body) != "hello world" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") ||
		resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("download: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	admin.Header.Set("Range", "bytes=6-")
	resp, body = admin.Do("GET", f.url("download", q("path", target)), nil)
	admin.Header.Del("Range")
	if resp.StatusCode != 206 || string(body) != "world" || resp.Header.Get("Content-Range") != "bytes 6-10/11" {
		t.Fatalf("range: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	admin.Header.Set("Range", "bytes=50-")
	resp, _ = admin.Do("GET", f.url("download", q("path", target)), nil)
	admin.Header.Del("Range")
	if resp.StatusCode != 416 {
		t.Fatalf("unsatisfiable range: %d", resp.StatusCode)
	}
	os.WriteFile(dir+"/a/page.html", []byte("<script>alert(1)</script>"), 0o644)
	resp, _ = admin.Do("GET", f.url("download", q("path", dir+"/a/page.html", "inline", "1")), nil)
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("inline html: %v", resp.Header)
	}
	resp, body = admin.Do("GET", f.url("download", q("path", dir+"/a", "name", "bundle")), nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "bundle.zip") {
		t.Fatalf("zip download: %d %v", resp.StatusCode, resp.Header)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, zf := range zr.File {
		names[zf.Name] = true
	}
	if !names["a/up.txt"] || !names["a/b/"] || !names["a/.hidden"] {
		t.Fatalf("zip content %v", names)
	}
	resp, body = admin.Do("GET", f.url("download", url.Values{"paths": {target, dir + "/a/z.txt"}}), nil)
	if zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body))); err != nil || len(zr.File) != 2 || resp.StatusCode != 200 {
		t.Fatalf("multi-path zip: %v", err)
	}

	// read / write with conflict detection
	var rd struct {
		Content, Encoding string
		Size              int64
		Mtime             time.Time
	}
	f.must("GET", "read", q("path", target), nil, &rd)
	if rd.Content != "hello world" || rd.Encoding != "utf-8" || rd.Size != 11 {
		t.Fatalf("read %+v", rd)
	}
	os.WriteFile(dir+"/a/bin", []byte{0, 1, 2, 0xff}, 0o644)
	f.must("GET", "read", q("path", dir+"/a/bin"), nil, &rd)
	if rd.Encoding != "base64" || rd.Content != base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 0xff}) {
		t.Fatalf("binary read %+v", rd)
	}
	if st, code := f.code("GET", "read", q("path", target, "maxBytes", "4"), nil); st != 413 || code != "too_large" {
		t.Fatalf("too large: %d %s", st, code)
	}
	f.must("PUT", "write", nil, map[string]any{"path": target, "content": "v2", "encoding": "utf-8", "expectMtime": time.Unix(1600000000, 0)}, &e)
	if e.Size != 2 {
		t.Fatalf("write entry %+v", e)
	}
	if st, code := f.code("PUT", "write", nil, map[string]any{"path": target, "content": "v3", "expectMtime": time.Unix(1600000000, 0)}); st != 409 || code != "conflict" {
		t.Fatalf("stale write: %d %s", st, code)
	}
	// Saving through a symlink: the reply describes the target, so its mtime is what the next conflict check uses.
	os.WriteFile(dir+"/a/real.conf", []byte("x=1\n"), 0o644)
	os.Symlink("real.conf", dir+"/a/link.conf")
	old := time.Unix(1500000000, 0)
	os.Chtimes(dir+"/a/real.conf", old, old)
	var rl struct{ Mtime time.Time }
	f.must("GET", "read", q("path", dir+"/a/link.conf"), nil, &rl)
	f.must("PUT", "write", nil, map[string]any{"path": dir + "/a/link.conf", "content": "x=2\n", "expectMtime": rl.Mtime}, &e)
	if e.Type != "file" || e.Path != dir+"/a/link.conf" || e.Size != 4 {
		t.Fatalf("write through symlink entry %+v", e)
	}
	f.must("PUT", "write", nil, map[string]any{"path": dir + "/a/link.conf", "content": "x=3\n", "expectMtime": e.Mtime}, &e)
	if lt, _ := os.Readlink(dir + "/a/link.conf"); lt != "real.conf" {
		t.Fatal("the symlink must stay a symlink")
	}
	if b, _ := os.ReadFile(dir + "/a/real.conf"); string(b) != "x=3\n" {
		t.Fatalf("target content %q", b)
	}
	f.must("PUT", "write", nil, map[string]any{"path": dir + "/a/new.bin", "content": base64.StdEncoding.EncodeToString([]byte{9, 8}), "encoding": "base64"}, &e)
	if b, _ := os.ReadFile(dir + "/a/new.bin"); !bytes.Equal(b, []byte{9, 8}) {
		t.Fatal("base64 write")
	}

	// rename / copy / chmod / symlink / checksum / search / archive / extract / delete
	f.must("POST", "rename", nil, map[string]any{"from": target, "to": "renamed.txt"}, &e)
	if e.Path != dir+"/a/renamed.txt" {
		t.Fatalf("rename %+v", e)
	}
	if st, code := f.code("POST", "rename", nil, map[string]any{"from": dir + "/a/renamed.txt", "to": dir + "/a/z.txt"}); st != 409 || code != "exists" {
		t.Fatalf("rename onto existing: %d %s", st, code)
	}
	var cp struct{ Entries []model.FileEntry }
	f.must("POST", "copy", nil, map[string]any{"from": []string{dir + "/a/z.txt"}, "toDir": dir + "/a"}, &cp)
	if len(cp.Entries) != 1 || cp.Entries[0].Name != "z (copy).txt" {
		t.Fatalf("copy %+v", cp)
	}
	f.must("POST", "chmod", nil, map[string]any{"paths": []string{dir + "/a/z.txt"}, "mode": "u+x,go-r"}, nil)
	if fi, _ := os.Stat(dir + "/a/z.txt"); fi.Mode().Perm() != 0o700 {
		t.Fatalf("symbolic chmod: %v", fi.Mode())
	}
	f.must("POST", "chmod", nil, map[string]any{"paths": []string{dir + "/a/b"}, "mode": 0o750, "recursive": true}, nil)
	f.must("POST", "symlink", nil, map[string]any{"target": "z.txt", "link": dir + "/a/zl"}, &e)
	if e.Type != "symlink" || e.LinkTarget != "z.txt" || e.LinkType != "file" {
		t.Fatalf("symlink %+v", e)
	}
	var sum struct{ Algo, Hash string }
	f.must("POST", "checksum", nil, map[string]any{"path": dir + "/a/z.txt", "algo": "sha256"}, &sum)
	if sum.Hash != "17f165d5a5ba695f27c023a83aa2b3463e23810e360b7517127e90161eebabda" || sum.Algo != "sha256" {
		t.Fatalf("checksum %+v", sum)
	}
	var sr struct {
		Entries   []model.FileEntry
		Truncated bool
	}
	f.must("POST", "search", nil, map[string]any{"path": dir, "pattern": "z", "maxResults": 10}, &sr)
	if len(sr.Entries) != 3 {
		t.Fatalf("search %+v", sr)
	}
	f.must("POST", "archive", nil, map[string]any{"paths": []string{dir + "/a/z.txt", dir + "/a/b"}, "dest": "pack.tar.gz", "format": "tar.gz"}, &e)
	if e.Name != "pack.tar.gz" || e.Size == 0 {
		t.Fatalf("archive %+v", e)
	}
	f.must("POST", "extract", nil, map[string]any{"path": dir + "/a/pack.tar.gz", "destDir": dir + "/x"}, nil)
	if _, err := os.Stat(dir + "/x/z.txt"); err != nil {
		t.Fatal("extract", err)
	}
	var sp vfs.Space
	f.must("GET", "space", q("path", dir), nil, &sp)
	if sp.Total <= 0 || sp.Avail <= 0 {
		t.Fatalf("space %+v", sp)
	}
	if st, code := f.code("POST", "presign", nil, map[string]any{"path": dir + "/a/z.txt"}); st != 400 || code != "not_supported" {
		t.Fatalf("presign on local: %d %s", st, code)
	}
	var rp struct{ Path string }
	f.must("GET", "realpath", q("path", dir+"/a/../a/zl"), nil, &rp)
	if rp.Path != dir+"/a/z.txt" {
		t.Fatalf("realpath %q", rp.Path)
	}
	var cwd struct{ Path string }
	f.must("GET", "cwd", nil, nil, &cwd)
	if cwd.Path != "" {
		t.Fatal("cwd of a handle without session")
	}
	if st, code := f.code("POST", "delete", nil, map[string]any{"paths": []string{dir + "/a"}}); st != 409 || code != "not_empty" {
		t.Fatalf("non-recursive delete of a folder: %d %s", st, code)
	}
	if st, _ := f.code("POST", "delete", nil, map[string]any{"paths": []string{"/"}, "recursive": true}); st != 400 {
		t.Fatalf("delete / must be refused: %d", st)
	}
	f.must("POST", "delete", nil, map[string]any{"paths": []string{dir + "/a", dir + "/x"}, "recursive": true}, nil)
	if _, err := os.Stat(dir + "/a"); !os.IsNotExist(err) {
		t.Fatal("recursive delete")
	}

	// compare
	os.MkdirAll(dir+"/L", 0o755)
	os.MkdirAll(dir+"/R", 0o755)
	os.WriteFile(dir+"/L/f", []byte("1"), 0o644)
	var cmpRes vfs.CompareResult
	admin.MustJSON("POST", "/api/fs/compare", map[string]any{"left": map[string]string{"fsId": f.id, "path": dir + "/L"},
		"right": map[string]string{"fsId": f.id, "path": dir + "/R"}, "recursive": true}, &cmpRes)
	if len(cmpRes.Items) != 1 || cmpRes.Items[0].Status != "left-only" {
		t.Fatalf("compare %+v", cmpRes)
	}

	// ownership: another user cannot use the handle
	bob := env.CreateUser(admin, "bob", pw, "user")
	if st, code := bob.ErrorCode("GET", f.url("list", q("path", dir)), nil); st != 404 || code != "fs_not_found" {
		t.Fatalf("foreign handle: %d %s", st, code)
	}
	if st, code := bob.ErrorCode("DELETE", "/api/fs/"+f.id, nil); st != 404 || code != "fs_not_found" {
		t.Fatalf("foreign close: %d %s", st, code)
	}
	var handles []handleView
	bob.MustJSON("GET", "/api/fs", nil, &handles)
	if len(handles) != 0 {
		t.Fatal("bob sees admin's handles")
	}
	admin.MustJSON("DELETE", "/api/fs/"+f.id, nil, nil)
	if st, code := f.code("GET", "list", q("path", dir), nil); st != 404 || code != "fs_not_found" {
		t.Fatalf("closed handle: %d %s", st, code)
	}
}

func TestLocalServerModeJail(t *testing.T) {
	root := tempDir(t)
	t.Setenv("NEXTERM_LOCAL_FS_ROOT", root)
	env := servertest.New(t, func(c *config.Config) { c.Mode = config.ModeServer })
	admin := env.Setup("admin", pw)
	bob := env.CreateUser(admin, "bob", pw, "user")
	if st, _ := bob.ErrorCode("POST", "/api/fs", map[string]any{"local": true}); st != 403 {
		t.Fatalf("non-admin local fs in server mode: %d", st)
	}
	f := openLocal(t, admin)
	f.must("POST", "mkdir", nil, map[string]any{"path": "/inside"}, nil)
	if _, err := os.Stat(filepath.Join(root, "inside")); err != nil {
		t.Fatal("jail root not used")
	}
	var list struct{ Entries []model.FileEntry }
	f.must("GET", "list", q("path", "/../../.."), nil, &list)
	if len(list.Entries) != 1 || list.Entries[0].Name != "inside" {
		t.Fatalf("escaped the jail: %+v", list)
	}
	os.Symlink("/etc", filepath.Join(root, "etc"))
	if st, _ := f.code("GET", "list", q("path", "/etc"), nil); st < 400 {
		t.Fatal("symlink escaped the jail")
	}
}

func waitTransfer(t *testing.T, c *servertest.Client, id string) transfer.Info {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var in transfer.Info
		c.MustJSON("GET", "/api/transfers/"+id, nil, &in)
		if in.FinishedAt != nil {
			return in
		}
		if time.Now().After(deadline) {
			t.Fatalf("transfer did not finish: %+v", in)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTransfersLocal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	env := servertest.New(t)
	admin := env.Setup("admin", pw)
	f := openLocal(t, admin)
	src, dst := tempDir(t), tempDir(t)
	os.MkdirAll(src+"/tree/sub", 0o755)
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	os.WriteFile(src+"/tree/big.bin", big, 0o640)
	os.WriteFile(src+"/tree/sub/small.txt", []byte("small"), 0o600)
	os.Symlink("small.txt", src+"/tree/sub/link")
	mt := time.Unix(1500000000, 0)
	os.Chtimes(src+"/tree/big.bin", mt, mt)

	var in transfer.Info
	admin.MustJSON("POST", "/api/transfers", map[string]any{"srcFs": f.id, "srcPaths": []string{src + "/tree"}, "dstFs": f.id,
		"dstDir": dst, "overwrite": "overwrite", "verify": true}, &in)
	if in.State != "queued" && in.State != "running" {
		t.Fatalf("created %+v", in)
	}
	in = waitTransfer(t, admin, in.ID)
	if in.State != "done" || in.DoneFiles != 3 || in.TotalFiles != 3 || in.DoneBytes != in.TotalBytes || in.TotalBytes != int64(len(big)+5) {
		t.Fatalf("transfer %+v", in)
	}
	if b, _ := os.ReadFile(dst + "/tree/big.bin"); !bytes.Equal(b, big) {
		t.Fatal("content")
	}
	fi, _ := os.Stat(dst + "/tree/big.bin")
	if !fi.ModTime().Equal(mt) || fi.Mode().Perm() != 0o640 {
		t.Fatalf("attributes not preserved: %v %v", fi.ModTime(), fi.Mode())
	}
	if l, _ := os.Readlink(dst + "/tree/sub/link"); l != "small.txt" {
		t.Fatalf("symlink not recreated: %q", l)
	}

	// skip: nothing rewritten; rename: numbered copy; ask without an events socket: skipped
	os.WriteFile(dst+"/tree/sub/small.txt", []byte("changed"), 0o600)
	for _, pol := range []string{"skip", "ask"} {
		admin.MustJSON("POST", "/api/transfers", map[string]any{"srcFs": f.id, "srcPaths": []string{src + "/tree/sub/small.txt"},
			"dstFs": f.id, "dstDir": dst + "/tree/sub", "overwrite": pol}, &in)
		in = waitTransfer(t, admin, in.ID)
		if in.State != "done" || in.SkippedFiles != 1 {
			t.Fatalf("%s: %+v", pol, in)
		}
	}
	if b, _ := os.ReadFile(dst + "/tree/sub/small.txt"); string(b) != "changed" {
		t.Fatal("skip overwrote")
	}
	admin.MustJSON("POST", "/api/transfers", map[string]any{"srcFs": f.id, "srcPaths": []string{src + "/tree/sub/small.txt"},
		"dstFs": f.id, "dstDir": dst + "/tree/sub", "overwrite": "rename"}, &in)
	waitTransfer(t, admin, in.ID)
	if b, _ := os.ReadFile(dst + "/tree/sub/small (1).txt"); string(b) != "small" {
		t.Fatal("rename policy")
	}

	// resume: a partial destination is completed
	os.WriteFile(dst+"/tree/big.bin", big[:300000], 0o640)
	admin.MustJSON("POST", "/api/transfers", map[string]any{"srcFs": f.id, "srcPaths": []string{src + "/tree/big.bin"},
		"dstFs": f.id, "dstDir": dst + "/tree", "overwrite": "resume"}, &in)
	in = waitTransfer(t, admin, in.ID)
	if b, _ := os.ReadFile(dst + "/tree/big.bin"); !bytes.Equal(b, big) || in.State != "done" {
		t.Fatalf("resume: %+v", in)
	}

	// move within one file system: rename
	admin.MustJSON("POST", "/api/transfers", map[string]any{"srcFs": f.id, "srcPaths": []string{dst + "/tree"}, "dstFs": f.id,
		"dstDir": src, "overwrite": "rename", "move": true}, &in)
	in = waitTransfer(t, admin, in.ID)
	if _, err := os.Stat(dst + "/tree"); !os.IsNotExist(err) || in.State != "done" {
		t.Fatalf("move: %+v", in)
	}
	if _, err := os.Stat(src + "/tree (1)/big.bin"); err != nil {
		t.Fatal("moved tree missing", err)
	}

	// errors and listing
	if st, _ := admin.ErrorCode("POST", "/api/transfers", map[string]any{"srcFs": f.id, "srcPaths": []string{src},
		"dstFs": f.id, "dstDir": src + "/tree"}); st != 400 {
		t.Fatalf("copy into itself: %d", st)
	}
	if st, code := admin.ErrorCode("POST", "/api/transfers", map[string]any{"srcFs": "nope", "srcPaths": []string{src},
		"dstFs": f.id, "dstDir": dst}); st != 404 || code != "fs_not_found" {
		t.Fatalf("unknown handle: %d %s", st, code)
	}
	var all []transfer.Info
	admin.MustJSON("GET", "/api/transfers", nil, &all)
	if len(all) != 6 {
		t.Fatalf("list: %d", len(all))
	}
	bob := env.CreateUser(admin, "bob", pw, "user")
	var none []transfer.Info
	bob.MustJSON("GET", "/api/transfers", nil, &none)
	if len(none) != 0 {
		t.Fatal("transfers leak across users")
	}
	if st, _ := bob.ErrorCode("DELETE", "/api/transfers/"+all[0].ID, nil); st != 404 {
		t.Fatal("foreign delete")
	}
	admin.MustJSON("DELETE", "/api/transfers/"+all[0].ID, nil, nil)
	var removed struct{ Removed int }
	admin.MustJSON("DELETE", "/api/transfers", nil, &removed)
	if removed.Removed != 5 {
		t.Fatalf("clear finished: %+v", removed)
	}
}

func TestTransferCancel(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", pw)
	f := openLocal(t, admin)
	src, dst := tempDir(t), tempDir(t)
	for i := 0; i < 200; i++ {
		os.WriteFile(fmt.Sprintf("%s/f%03d", src, i), bytes.Repeat([]byte{byte(i)}, 256<<10), 0o644)
	}
	var in transfer.Info
	admin.MustJSON("POST", "/api/transfers", map[string]any{"srcFs": f.id, "srcPaths": []string{src}, "dstFs": f.id,
		"dstDir": dst, "overwrite": "overwrite"}, &in)
	admin.MustJSON("POST", "/api/transfers/"+in.ID+"/cancel", nil, nil)
	in = waitTransfer(t, admin, in.ID)
	if in.State != "canceled" && in.State != "done" {
		t.Fatalf("cancel: %+v", in)
	}
	entries, _ := os.ReadDir(filepath.Join(dst, filepath.Base(src)))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), vfs.PartSuffix) {
			t.Fatal("canceled transfer left a part file")
		}
	}
	_ = io.Discard
	_ = http.StatusOK
}

// checkRanges verifies Range / If-Range / HEAD handling of a download URL against the expected content.
func checkRanges(t *testing.T, c *servertest.Client, u string, data []byte) {
	t.Helper()
	size := len(data)
	get := func(method string, hdr map[string]string) (*http.Response, []byte) {
		for k, v := range hdr {
			c.Header.Set(k, v)
		}
		defer func() {
			for k := range hdr {
				c.Header.Del(k)
			}
		}()
		return c.Do(method, u, nil)
	}
	full, _ := get("GET", nil)
	etag := full.Header.Get("ETag")
	if full.StatusCode != 200 || etag == "" || full.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("full: %d %v", full.StatusCode, full.Header)
	}
	cases := []struct {
		rng       string
		ifRange   string
		status    int
		from, to  int // expected body data[from:to]
		contRange string
	}{
		{"bytes=0-0", "", 206, 0, 1, fmt.Sprintf("bytes 0-0/%d", size)},
		{"bytes=5-104", "", 206, 5, 105, fmt.Sprintf("bytes 5-104/%d", size)},
		{fmt.Sprintf("bytes=%d-", size-7), "", 206, size - 7, size, fmt.Sprintf("bytes %d-%d/%d", size-7, size-1, size)},
		{"bytes=-9", "", 206, size - 9, size, fmt.Sprintf("bytes %d-%d/%d", size-9, size-1, size)},
		{fmt.Sprintf("bytes=10-%d", size+500), "", 206, 10, size, fmt.Sprintf("bytes 10-%d/%d", size-1, size)},
		{"bytes=50-10", "", 200, 0, size, ""},
		{"bytes=0-1,5-6", "", 200, 0, size, ""},
		{"bytes=5-104", etag, 206, 5, 105, fmt.Sprintf("bytes 5-104/%d", size)},
		{"bytes=5-104", `"stale"`, 200, 0, size, ""},
		{fmt.Sprintf("bytes=%d-", size), "", 416, 0, 0, fmt.Sprintf("bytes */%d", size)},
		{fmt.Sprintf("bytes=%d-", size), `"stale"`, 200, 0, size, ""},
	}
	for _, cs := range cases {
		hdr := map[string]string{"Range": cs.rng}
		if cs.ifRange != "" {
			hdr["If-Range"] = cs.ifRange
		}
		resp, body := get("GET", hdr)
		if resp.StatusCode != cs.status {
			t.Fatalf("%s (If-Range %q): status %d, want %d", cs.rng, cs.ifRange, resp.StatusCode, cs.status)
		}
		if resp.Header.Get("Content-Range") != cs.contRange {
			t.Fatalf("%s: Content-Range %q, want %q", cs.rng, resp.Header.Get("Content-Range"), cs.contRange)
		}
		if cs.status == 416 {
			continue
		}
		if !bytes.Equal(body, data[cs.from:cs.to]) {
			t.Fatalf("%s: body %d bytes, want data[%d:%d]", cs.rng, len(body), cs.from, cs.to)
		}
		if cl := resp.Header.Get("Content-Length"); cl != fmt.Sprint(cs.to-cs.from) {
			t.Fatalf("%s: Content-Length %s, want %d", cs.rng, cl, cs.to-cs.from)
		}
	}
	resp, body := get("HEAD", map[string]string{"Range": "bytes=5-104"})
	if resp.StatusCode != 206 || len(body) != 0 || resp.Header.Get("Content-Length") != "100" {
		t.Fatalf("HEAD range: %d, %d bytes, CL %s", resp.StatusCode, len(body), resp.Header.Get("Content-Length"))
	}
	// Inline previews honor Range too (text previews read the first bytes this way).
	resp, body = get("GET", map[string]string{"Range": "bytes=0-99"})
	if resp.StatusCode != 206 || !bytes.Equal(body, data[:100]) {
		t.Fatalf("preview range: %d", resp.StatusCode)
	}
}

func TestDownloadRanges(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", pw)
	f := openLocal(t, admin)
	dir := tempDir(t)
	data := make([]byte, 300000)
	for i := range data {
		data[i] = byte(i * 7)
	}
	os.WriteFile(dir+"/r.bin", data, 0o644)
	checkRanges(t, admin, f.url("download", q("path", dir+"/r.bin")), data)
	checkRanges(t, admin, f.url("download", q("path", dir+"/r.bin", "inline", "1")), data)
}
