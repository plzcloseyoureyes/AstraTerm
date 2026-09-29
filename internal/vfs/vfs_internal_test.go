package vfs

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nexterm/nexterm/internal/model"
)

var bg = context.Background()

func TestCleanPath(t *testing.T) {
	cases := []struct{ in, home, want string }{
		{"", "/home/u", "/home/u"},
		{"~", "/home/u", "/home/u"},
		{"~/a/../b", "/home/u", "/home/u/b"},
		{"docs", "/home/u", "/home/u/docs"},
		{"/a/./b//c/..", "/", "/a/b"},
		{"/../..", "/", "/"},
		{"/", "/x", "/"},
	}
	for _, c := range cases {
		got, err := cleanPath(c.in, c.home)
		if err != nil || got != c.want {
			t.Errorf("cleanPath(%q,%q) = %q, %v; want %q", c.in, c.home, got, err, c.want)
		}
	}
	for _, bad := range []string{"a\x00b", string([]byte{0xff, 0xfe}), strings.Repeat("a", maxPathLen+1)} {
		if _, err := cleanPath(bad, "/"); err == nil {
			t.Errorf("cleanPath(%q) should fail", bad)
		}
	}
}

func TestPermStringAndModes(t *testing.T) {
	cases := map[uint32]string{
		sIFDIR | 0o755:  "drwxr-xr-x",
		sIFREG | 0o644:  "-rw-r--r--",
		sIFREG | 0o4755: "-rwsr-xr-x",
		sIFREG | 0o2644: "-rw-r-Sr--",
		sIFDIR | 0o1777: "drwxrwxrwt",
		sIFDIR | 0o1770: "drwxrwx--T",
		sIFLNK | 0o777:  "lrwxrwxrwx",
		sIFIFO | 0o600:  "prw-------",
	}
	for m, want := range cases {
		if got := permString(m); got != want {
			t.Errorf("permString(%o) = %s, want %s", m, got, want)
		}
		if back, ok := parsePermString(want); !ok || back != m {
			t.Errorf("parsePermString(%s) = %o, want %o", want, back, m)
		}
	}
	type tc struct {
		mode  string
		cur   uint32
		dir   bool
		want  uint32
		isErr bool
	}
	for _, c := range []tc{
		{"755", 0o644, false, 0o755, false},
		{"04755", 0, false, 0o4755, false},
		{"u+x", 0o644, false, 0o744, false},
		{"go-w", 0o666, false, 0o644, false},
		{"a=rX", 0o700, true, 0o555, false},
		{"a=rX", 0o600, false, 0o444, false},
		{"a+X", 0o644, false, 0o644, false},
		{"a+X", 0o744, false, 0o755, false},
		{"u=rwx,g=rx,o=", 0o777, false, 0o750, false},
		{"g=u", 0o740, false, 0o770, false},
		{"u+s,+t", 0o755, true, 0o5755, false},
		{"+x", 0o600, false, 0o711, false},
		{"8", 0, false, 0, true},
		{"u?x", 0, false, 0, true},
		{"", 0, false, 0, true},
	} {
		spec, err := parseMode(c.mode)
		if c.isErr {
			if err == nil {
				t.Errorf("parseMode(%q) should fail", c.mode)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseMode(%q): %v", c.mode, err)
		}
		if got := spec.apply(c.cur, c.dir); got != c.want {
			t.Errorf("%q applied to %o (dir %v) = %o, want %o", c.mode, c.cur, c.dir, got, c.want)
		}
	}
}

// TestShellQuote checks shq against a real POSIX shell: arbitrary strings must reach the command verbatim.
func TestShellQuote(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, s := range []string{"plain", "with space", "it's", `"double"`, "$(touch /tmp/pwned)", "`id`", "a;b|c&d",
		"new\nline", "\\back\\", "-rf", "*", "", "tab\there", "ünï©ødé", "'", "''", "$HOME"} {
		out, err := exec.Command(sh, "-c", "printf %s "+shq(s)).Output()
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if string(out) != s {
			t.Errorf("shq(%q) round-trip gave %q", s, out)
		}
	}
}

func TestParseStatAndLS(t *testing.T) {
	e, err := parseStatLine("a1ff/13/1790522685/1000/1000/test/users//tmp/weird/name")
	if err != nil {
		t.Fatal(err)
	}
	if e.Type != "symlink" || e.Size != 13 || e.Path != "/tmp/weird/name" || e.Name != "name" || e.Owner != "test" ||
		e.Group != "users" || *e.UID != 1000 || e.Mtime.Unix() != 1790522685 {
		t.Fatalf("stat line parsed as %+v", e)
	}
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		line, name, typ, target string
		size                    int64
		mode                    uint32
	}{
		{"-rw-r--r--    1 1000     1000            4 Sep 27 07:27 sshd.pid", "sshd.pid", "file", "", 4, sIFREG | 0o644},
		{"drwxr-xr-x  6 0  0  4096 Jan  3  2025 my dir", "my dir", "dir", "", 4096, sIFDIR | 0o755},
		{"lrwxrwxrwx 1 0 0 9 Sep 19 16:36 stat -> coreutils", "stat", "symlink", "coreutils", 9, sIFLNK | 0o777},
		{"crw-rw-rw- 1 0 0 1, 3 Sep 19 16:36 null", "null", "other", "", 0, sIFCHR | 0o666},
		{"-rwsr-x--T 1 0 0 1 Sep 19 16:36 x", "x", "file", "", 1, sIFREG | 0o5750},
	} {
		e, ok := parseLSLine(c.line, now)
		if !ok {
			t.Fatalf("cannot parse %q", c.line)
		}
		if e.Name != c.name || e.Type != c.typ || e.LinkTarget != c.target || e.Size != c.size || e.Mode != c.mode {
			t.Errorf("%q → %+v", c.line, e)
		}
	}
	if _, ok := parseLSLine("total 12", now); ok {
		t.Error("total line must be skipped")
	}
}

type fakeExec struct {
	out  string
	cmds []string
}

func (f *fakeExec) Exec(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) ([]byte, int, error) {
	f.cmds = append(f.cmds, cmd)
	if stdout != nil {
		io.WriteString(stdout, f.out)
	}
	return nil, 0, nil
}

func TestOwnerNamesAndChecksumParsing(t *testing.T) {
	fx := &fakeExec{out: "root:x:0:0:root:/root:/bin/sh\ntest:x:1000:1000::/config:/bin/bash\n::nexterm-groups::\nroot:x:0:root\nusers:x:1000:games,test\n"}
	oc := newOwnerCache(fx)
	entries := []*Entry{{UID: intPtr(0), GID: intPtr(0)}, {UID: intPtr(1000), GID: intPtr(1000)}, {UID: intPtr(4242), GID: intPtr(4242)}}
	oc.nameOwners(bg, entries)
	if entries[0].Owner != "root" || entries[1].Owner != "test" || entries[1].Group != "users" || entries[2].Owner != "" {
		t.Fatalf("names: %+v %+v %+v", entries[0], entries[1], entries[2])
	}
	if !strings.Contains(fx.cmds[0], "getent passwd 0 1000 4242") {
		t.Fatalf("unexpected command %q", fx.cmds[0])
	}
	// Cached: no second exec.
	oc.nameOwners(bg, []*Entry{{UID: intPtr(1000)}})
	if len(fx.cmds) != 1 {
		t.Fatal("owner names must be cached")
	}
	if oc.lookupName(true, "test") != 1000 || oc.lookupName(false, "users") != 1000 {
		t.Fatal("reverse lookup")
	}
	h, err := parseHashOutput([]byte(`\e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  a\nb`+"\n"), "sha256")
	if err != nil || h != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("hash %q %v", h, err)
	}
	if _, err := parseHashOutput([]byte("nope"), "md5"); err == nil {
		t.Fatal("bad hash output must fail")
	}
}

// ---- local driver -------------------------------------------------------------------------------------------------

func mustWrite(t *testing.T, fsys FS, p, content string) {
	t.Helper()
	w, err := fsys.Create(bg, p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, fsys FS, p string, off int64) string {
	t.Helper()
	r, err := fsys.Open(bg, p, off)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// newJail returns a jailed local FS over a temp dir.
func newJail(t *testing.T) (*localFS, string) {
	t.Helper()
	dir := t.TempDir()
	l, err := newLocalFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, dir
}

func TestLocalFSOperations(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX semantics")
	}
	l, dir := newJail(t)
	if h, _ := l.Home(bg); h != "/" {
		t.Fatalf("jailed home %q", h)
	}
	if err := l.MkdirAll(bg, "/a/b/c"); err != nil {
		t.Fatal(err)
	}
	if err := l.Mkdir(bg, "/a"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("mkdir existing: %v", err)
	}
	mustWrite(t, l, "/a/f.txt", "hello world")
	if got := mustRead(t, l, "/a/f.txt", 6); got != "world" {
		t.Fatalf("offset read %q", got)
	}
	// Create at an offset truncates there and continues.
	w, err := l.Create(bg, "/a/f.txt", 5)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "!")
	w.Close()
	if got := mustRead(t, l, "/a/f.txt", 0); got != "hello!" {
		t.Fatalf("offset write %q", got)
	}
	e, err := l.Stat(bg, "/a/f.txt")
	if err != nil || e.Type != "file" || e.Size != 6 || e.Name != "f.txt" {
		t.Fatalf("stat %+v %v", e, err)
	}
	if err := l.Chmod(bg, "/a/f.txt", 0o4640); err != nil {
		t.Fatal(err)
	}
	e, _ = l.Stat(bg, "/a/f.txt")
	if e.Mode&0o7777 != 0o4640 {
		t.Fatalf("chmod: %o", e.Mode)
	}
	mt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := l.Chtimes(bg, "/a/f.txt", mt, mt); err != nil {
		t.Fatal(err)
	}
	if e, _ = l.Stat(bg, "/a/f.txt"); !e.Mtime.Equal(mt) {
		t.Fatalf("mtime %v", e.Mtime)
	}
	if err := l.Symlink(bg, "f.txt", "/a/link"); err != nil {
		t.Fatal(err)
	}
	if tgt, err := l.Readlink(bg, "/a/link"); err != nil || tgt != "f.txt" {
		t.Fatalf("readlink %q %v", tgt, err)
	}
	entries, err := l.List(bg, "/a")
	if err != nil {
		t.Fatal(err)
	}
	finishEntries(bg, l, entries)
	sortEntries(entries)
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name+":"+e.Type+":"+e.LinkType)
	}
	if strings.Join(names, ",") != "b:dir:,f.txt:file:,link:symlink:file" {
		t.Fatalf("listing %v", names)
	}
	if err := l.Rename(bg, "/a/f.txt", "/a/g.txt"); err != nil {
		t.Fatal(err)
	}
	if err := l.Remove(bg, "/a/b"); err == nil {
		t.Fatal("removing a non-empty directory must fail")
	}
	if err := l.RemoveAll(bg, "/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
		t.Fatal("tree not removed")
	}
	if err := l.RemoveAll(bg, "/missing"); err != nil {
		t.Fatalf("RemoveAll of a missing path: %v", err)
	}
	if err := l.RemoveAll(bg, "/"); err == nil {
		t.Fatal("RemoveAll(/) must be refused")
	}
}

func TestLocalJail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privileges")
	}
	l, dir := newJail(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("s3cr3t"), 0o600)
	// Symlinks pointing outside (absolute or relative) cannot be followed; ".." never escapes.
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "abs"))
	rel, _ := filepath.Rel(dir, filepath.Join(outside, "secret"))
	os.Symlink(rel, filepath.Join(dir, "rel"))
	for _, p := range []string{"/abs", "/rel"} {
		if _, err := l.Open(bg, p, 0); err == nil {
			t.Fatalf("%s: escaped the jail", p)
		}
		if e, err := l.Lstat(bg, p); err != nil || e.Type != "symlink" {
			t.Fatalf("%s must still be listed as a symlink: %+v %v", p, e, err)
		}
	}
	if p, _ := cleanPath("/../../etc/passwd", "/"); p != "/etc/passwd" {
		t.Fatal(p)
	}
	if _, err := l.Stat(bg, "/etc/passwd"); err == nil {
		t.Fatal("jail must map /etc/passwd inside the root")
	}
	// Deleting a symlink to a directory removes the link, never the target.
	os.MkdirAll(filepath.Join(outside, "keep"), 0o755)
	os.WriteFile(filepath.Join(outside, "keep", "x"), []byte("x"), 0o600)
	os.Symlink(filepath.Join(outside, "keep"), filepath.Join(dir, "dirlink"))
	if err := l.RemoveAll(bg, "/dirlink"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep", "x")); err != nil {
		t.Fatal("RemoveAll followed a symlink")
	}
}

func TestRemoveAllGenericNeverFollowsLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	l, dir := newJail(t)
	os.MkdirAll(filepath.Join(dir, "tree", "sub"), 0o755)
	os.MkdirAll(filepath.Join(dir, "victim"), 0o755)
	os.WriteFile(filepath.Join(dir, "victim", "precious"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, "tree", "sub", "f"), []byte("x"), 0o600)
	os.Symlink("../../victim", filepath.Join(dir, "tree", "sub", "link"))
	if err := removeAllGeneric(bg, l, "/tree"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "victim", "precious")); err != nil {
		t.Fatal("recursive delete followed a symlink")
	}
	if _, err := os.Lstat(filepath.Join(dir, "tree")); !os.IsNotExist(err) {
		t.Fatal("tree not deleted")
	}
}

func TestCopyChecksumSearch(t *testing.T) {
	l, _ := newJail(t)
	l.MkdirAll(bg, "/src/d")
	mustWrite(t, l, "/src/a.txt", "alpha content")
	mustWrite(t, l, "/src/d/b.log", "Needle in here")
	l.Chmod(bg, "/src/a.txt", 0o600)
	if err := copyTree(bg, l, "/src", "/dst"); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, l, "/dst/d/b.log", 0); got != "Needle in here" {
		t.Fatalf("copy content %q", got)
	}
	if e, _ := l.Stat(bg, "/dst/a.txt"); e.Mode&0o777 != 0o600 {
		t.Fatalf("copy mode %o", e.Mode)
	}
	if err := copyTree(bg, l, "/src", "/src/inner"); err == nil {
		t.Fatal("copy into itself must fail")
	}
	sum, err := checksum(bg, l, "/src/a.txt", "sha256")
	if err != nil || sum != "d6d382edd8065bd0bbaa26f4a118d32d3be05597be1968c51080971f95d90b23" {
		t.Fatalf("sha256 %q %v", sum, err)
	}
	if md5sum, _ := checksum(bg, l, "/src/a.txt", "md5"); len(md5sum) != 32 {
		t.Fatalf("md5 %q", md5sum)
	}
	if _, err := checksum(bg, l, "/src/a.txt", "crc"); err == nil {
		t.Fatal("unknown algorithm")
	}
	res, more, err := search(bg, l, "/", "*.log", "", 10)
	if err != nil || more || len(res) != 2 {
		t.Fatalf("search: %d %v %v", len(res), more, err)
	}
	res, _, _ = search(bg, l, "/src", "b", "needle", 10)
	if len(res) != 1 || res[0].Path != "/src/d/b.log" {
		t.Fatalf("content search %v", res)
	}
	res, more, _ = search(bg, l, "/", "", "", 2)
	if len(res) != 2 || !more {
		t.Fatalf("limit: %d %v", len(res), more)
	}
	name, err := uniqueName(bg, l, "/src", "a.txt", "copy")
	if err != nil || name != "a (copy).txt" {
		t.Fatalf("uniqueName %q %v", name, err)
	}
}

func TestArchiveRoundTrip(t *testing.T) {
	l, _ := newJail(t)
	l.MkdirAll(bg, "/p/dir/sub")
	mustWrite(t, l, "/p/dir/one.txt", "one")
	mustWrite(t, l, "/p/dir/sub/two.bin", strings.Repeat("\x00\x01", 1000))
	mustWrite(t, l, "/p/other.txt", "other")
	for _, format := range []string{"zip", "tar.gz"} {
		dest := "/p/out." + format
		if err := createArchive(bg, l, []string{"/p/dir", "/p/other.txt"}, dest, format); err != nil {
			t.Fatal(format, err)
		}
		if err := extractArchive(bg, l, dest, "/x-"+format, t.TempDir()); err != nil {
			t.Fatal(format, err)
		}
		if got := mustRead(t, l, "/x-"+format+"/dir/sub/two.bin", 0); got != strings.Repeat("\x00\x01", 1000) {
			t.Fatalf("%s: content mismatch", format)
		}
		if got := mustRead(t, l, "/x-"+format+"/other.txt", 0); got != "other" {
			t.Fatalf("%s: %q", format, got)
		}
	}
}

func TestExtractRejectsZipSlip(t *testing.T) {
	l, dir := newJail(t)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range []string{"../../escape.txt", "/abs.txt", "ok/../../up.txt", "fine/ok.txt"} {
		w, _ := zw.Create(n)
		io.WriteString(w, "x")
	}
	zw.Close()
	w, _ := l.Create(bg, "/evil.zip", 0)
	w.Write(buf.Bytes())
	w.Close()
	if err := extractArchive(bg, l, "/evil.zip", "/out", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	var found []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if !d.IsDir() {
			r, _ := filepath.Rel(dir, p)
			found = append(found, r)
		}
		return nil
	})
	sort.Strings(found)
	if strings.Join(found, ",") != "evil.zip,out/fine/ok.txt" {
		t.Fatalf("extracted %v", found)
	}
}

func TestCompareTrees(t *testing.T) {
	l, _ := newJail(t)
	for _, p := range []string{"/L/sub", "/R/sub", "/L/onlyleftdir"} {
		l.MkdirAll(bg, p)
	}
	mustWrite(t, l, "/L/same.txt", "same")
	mustWrite(t, l, "/R/same.txt", "same")
	mustWrite(t, l, "/L/size.txt", "short")
	mustWrite(t, l, "/R/size.txt", "longer!")
	mustWrite(t, l, "/L/sub/hash.txt", "aaaa")
	mustWrite(t, l, "/R/sub/hash.txt", "bbbb")
	mustWrite(t, l, "/L/left.txt", "l")
	mustWrite(t, l, "/R/right.txt", "r")
	mustWrite(t, l, "/L/kind", "file")
	l.MkdirAll(bg, "/R/kind")
	mt := time.Unix(1700000000, 0)
	for _, p := range []string{"/L/same.txt", "/R/same.txt", "/L/sub/hash.txt", "/R/sub/hash.txt"} {
		l.Chtimes(bg, p, mt, mt)
	}
	res, err := compareTrees(bg, l, "/L", l, "/R", compareOptions{Recursive: true, Mode: "size-mtime"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, it := range res.Items {
		got[it.Path] = it.Status + "/" + it.Reason
	}
	want := map[string]string{"same.txt": "same/", "size.txt": "different/size", "sub/hash.txt": "same/",
		"left.txt": "left-only/", "right.txt": "right-only/", "kind": "type-mismatch/", "onlyleftdir": "left-only/"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	res, _ = compareTrees(bg, l, "/L", l, "/R", compareOptions{Recursive: true, Mode: "checksum"})
	for _, it := range res.Items {
		if it.Path == "sub/hash.txt" && (it.Status != "different" || it.Reason != "checksum") {
			t.Fatalf("checksum mode: %+v", it)
		}
	}
	if res.Summary.LeftOnly != 2 || res.Summary.RightOnly != 1 || res.Summary.TypeMismatch != 1 {
		t.Fatalf("summary %+v", res.Summary)
	}
	res, _ = compareTrees(bg, l, "/L", l, "/R", compareOptions{Recursive: true, Excludes: []string{"*.txt"}})
	for _, it := range res.Items {
		if strings.HasSuffix(it.Path, ".txt") {
			t.Fatalf("excluded item reported: %s", it.Path)
		}
	}
}

func TestUploadPartFlow(t *testing.T) {
	l, _ := newJail(t)
	if _, err := writePart(bg, l, "/up.bin", 5, strings.NewReader("x"), 1, false); !errors.As(err, new(errOffsetMismatch)) {
		t.Fatalf("resume without a part: %v", err)
	}
	n, err := writePart(bg, l, "/up.bin", 0, strings.NewReader("hello "), 6, false)
	if err != nil || n != 6 {
		t.Fatal(n, err)
	}
	var om errOffsetMismatch
	if _, err := writePart(bg, l, "/up.bin", 9, strings.NewReader("x"), 1, false); !errors.As(err, &om) || om.size != 6 {
		t.Fatalf("gap must be refused with the stored size: %v", err)
	}
	// Retry of a partially stored chunk: offset below the part size truncates.
	if n, err = writePart(bg, l, "/up.bin", 4, strings.NewReader("o world"), 7, true); err != nil || n != 11 {
		t.Fatal(n, err)
	}
	mustWrite(t, l, "/up.bin", "old")
	l.Chmod(bg, "/up.bin", 0o600)
	if err := commitPart(bg, l, "/up.bin"); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, l, "/up.bin", 0); got != "hello world" {
		t.Fatalf("committed %q", got)
	}
	if e, _ := l.Stat(bg, "/up.bin"); e.Mode&0o777 != 0o600 {
		t.Fatalf("replaced file must keep its mode, got %o", e.Mode)
	}
	if _, err := l.Stat(bg, "/up.bin"+partSuffix); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("part file must be gone")
	}
	if _, err := writePart(bg, l, "/short.bin", 0, strings.NewReader("abc"), 10, true); err == nil {
		t.Fatal("a short body must fail")
	}
	// Folder uploads: missing parents are created on the first chunk.
	if _, err := writePart(bg, l, "/new/deep/dir/f.txt", 0, strings.NewReader("x"), 1, true); err != nil {
		t.Fatal(err)
	}
	if err := commitPart(bg, l, "/new/deep/dir/f.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestParseRangeAndPreview(t *testing.T) {
	for _, c := range []struct {
		h         string
		size      int64
		s, e      int64
		ok, unsat bool
	}{
		{"bytes=0-9", 100, 0, 9, true, false},
		{"bytes=90-", 100, 90, 99, true, false},
		{"bytes=-10", 100, 90, 99, true, false},
		{"bytes=50-500", 100, 50, 99, true, false},
		{"bytes=100-", 100, 0, 0, false, true},
		{"bytes=0-1,5-6", 100, 0, 0, false, false},
		{"items=0-1", 100, 0, 0, false, false},
		{"bytes=5-2", 100, 0, 0, false, false},
	} {
		s, e, ok, unsat := parseRange(c.h, c.size)
		if ok != c.ok || unsat != c.unsat || (ok && (s != c.s || e != c.e)) {
			t.Errorf("parseRange(%q) = %d %d %v %v", c.h, s, e, ok, unsat)
		}
	}
	for name, want := range map[string]string{"a.png": "image/png", "a.html": "text/plain; charset=utf-8",
		"a.svg": "image/svg+xml", "a.pdf": "application/pdf", "notes": "text/plain; charset=utf-8"} {
		ct, _, _ := previewType(name, []byte("plain text"))
		if ct != want {
			t.Errorf("previewType(%s) = %s, want %s", name, ct, want)
		}
	}
	if _, sandbox, _ := previewType("x.html", []byte("<script>")); !sandbox {
		t.Error("html must be sandboxed")
	}
	if cd := contentDisposition("attachment", `ré"sumé.txt`); !strings.Contains(cd, `filename="r__sum_.txt"`) ||
		!strings.Contains(cd, "filename*=UTF-8''r%C3%A9%22sum%C3%A9.txt") {
		t.Errorf("content disposition %s", cd)
	}
}

func TestParseMtime(t *testing.T) {
	if tm, ok := parseMtime("1700000000"); !ok || tm.Unix() != 1700000000 {
		t.Fatal("seconds")
	}
	if tm, ok := parseMtime("1700000000123"); !ok || tm.UnixMilli() != 1700000000123 {
		t.Fatal("milliseconds")
	}
	if _, ok := parseMtime("garbage"); ok {
		t.Fatal("garbage")
	}
}

type closeCountFS struct {
	*localFS
	closed int
}

func (c *closeCountFS) Close() error { c.closed++; return nil }

func TestRegistryLifecycle(t *testing.T) {
	l, _ := newJail(t)
	reg := &Registry{handles: map[string]*Handle{}, IdleTimeout: time.Minute, log: slogDiscard()}
	alice := &modelUser{ID: "alice"}
	bob := &modelUser{ID: "bob"}
	fsA := &closeCountFS{localFS: l}
	h := reg.Register(alice.user(), &Handle{Kind: "local", FS: fsA})
	if _, _, err := reg.Acquire(bob.user(), h.ID); err == nil {
		t.Fatal("foreign acquire")
	}
	got, release, err := reg.Acquire(alice.user(), h.ID)
	if err != nil || got != h {
		t.Fatal(err)
	}
	// Pinned handles survive the idle reaper even when old.
	h.lastUsed = time.Now().Add(-time.Hour)
	reg.reapIdle()
	if fsA.closed != 0 {
		t.Fatal("pinned handle reaped")
	}
	// Closing while pinned defers the FS close to the last release; new acquires fail at once.
	if err := reg.Close(alice.user(), h.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.Acquire(alice.user(), h.ID); err == nil {
		t.Fatal("closed handle acquired")
	}
	if fsA.closed != 0 {
		t.Fatal("closed while pinned")
	}
	release()
	release() // idempotent
	if fsA.closed != 1 {
		t.Fatalf("closed %d times", fsA.closed)
	}
	// Idle handles are reaped.
	fsB := &closeCountFS{localFS: l}
	h2 := reg.Register(alice.user(), &Handle{Kind: "local", FS: fsB})
	h2.lastUsed = time.Now().Add(-2 * time.Minute)
	reg.reapIdle()
	if fsB.closed != 1 {
		t.Fatal("idle handle not reaped")
	}
	if _, _, err := reg.Acquire(alice.user(), h2.ID); err == nil {
		t.Fatal("reaped handle still usable")
	}
	// Session-bound handles close with their session.
	fsC := &closeCountFS{localFS: l}
	h3 := reg.Register(alice.user(), &Handle{Kind: "sftp", FS: fsC, SessionID: "s1"})
	reg.closeSession("s1")
	if fsC.closed != 1 || len(reg.List(alice.user())) != 0 {
		t.Fatalf("session close: %d, %d handles", fsC.closed, len(reg.List(alice.user())))
	}
	_ = h3
}

type modelUser struct{ ID string }

func (m *modelUser) user() *model.User { return &model.User{ID: m.ID} }

func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
