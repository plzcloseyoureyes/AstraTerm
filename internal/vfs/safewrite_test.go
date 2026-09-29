//go:build unix

package vfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// flakyFS wraps a file system and simulates a connection that drops at a chosen step of a save.
type flakyFS struct {
	FS
	dropWriteAfter int  // Create of a temporary file: fail after this many bytes (-1: never)
	dropClose      bool // Close of a temporary file fails
	dropRename     bool // Rename fails
	lieUID         *int // Lstat/Stat of the target reports this owner
	chownDenied    bool
	removed        []string
}

var errDrop = errors.New("ssh: connection lost")

func isTmp(p string) bool { return strings.Contains(p, tmpSuffix) }

func (f *flakyFS) Create(ctx context.Context, p string, off int64) (io.WriteCloser, error) {
	w, err := f.FS.Create(ctx, p, off)
	if err != nil || !isTmp(p) || (f.dropWriteAfter < 0 && !f.dropClose) {
		return w, err
	}
	return &flakyWriter{w: w, left: f.dropWriteAfter, dropClose: f.dropClose}, nil
}

func (f *flakyFS) Rename(ctx context.Context, from, to string) error {
	if f.dropRename {
		return errDrop
	}
	return f.FS.Rename(ctx, from, to)
}

func (f *flakyFS) Remove(ctx context.Context, p string) error {
	f.removed = append(f.removed, p)
	return f.FS.Remove(ctx, p)
}

func (f *flakyFS) Lstat(ctx context.Context, p string) (*Entry, error) {
	e, err := f.FS.Lstat(ctx, p)
	if err == nil && f.lieUID != nil && !isTmp(p) {
		e.UID = intPtr(*f.lieUID)
	}
	return e, err
}

func (f *flakyFS) Chown(ctx context.Context, p string, uid, gid int) error {
	if f.chownDenied {
		return &os.PathError{Op: "chown", Path: p, Err: os.ErrPermission}
	}
	return f.FS.Chown(ctx, p, uid, gid)
}

// flakyFS keeps the safe-write hooks of the wrapped local FS.
func (f *flakyFS) atomicReplace(ctx context.Context) bool {
	return f.FS.(safeReplacer).atomicReplace(ctx)
}
func (f *flakyFS) keepInPlace(ctx context.Context, p string) string {
	return f.FS.(safeReplacer).keepInPlace(ctx, p)
}

type flakyWriter struct {
	w         io.WriteCloser
	left      int
	dropClose bool
}

func (w *flakyWriter) Write(p []byte) (int, error) {
	if w.left >= 0 && len(p) > w.left {
		n, _ := w.w.Write(p[:w.left])
		w.left = 0
		return n, errDrop
	}
	if w.left >= 0 {
		w.left -= len(p)
	}
	return w.w.Write(p)
}

func (w *flakyWriter) Close() error {
	err := w.w.Close()
	if w.dropClose {
		return errDrop
	}
	return err
}

func inode(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

func noTemps(t *testing.T, dir string) {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if isTmp(e.Name()) {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

func unrestricted(t *testing.T) *localFS {
	t.Helper()
	l, err := newLocalFS("")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestSafeWriteAtomicKeepsModeAndReplacesInode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX")
	}
	l := unrestricted(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "conf.txt")
	os.WriteFile(p, []byte("old content"), 0o640)
	os.Chmod(p, 0o640)
	before := inode(t, p)
	strategy, err := writeFileSafe(bg, l, p, []byte("new content, longer"))
	if err != nil || strategy != writeAtomic {
		t.Fatalf("strategy %q err %v", strategy, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "new content, longer" {
		t.Fatalf("content %q", b)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode not kept: %v", fi.Mode())
	}
	if inode(t, p) == before {
		t.Fatal("expected a rename-based replacement (new inode)")
	}
	noTemps(t, dir)
	// A new file is written atomically too.
	np := filepath.Join(dir, "new.txt")
	if s, err := writeFileSafe(bg, l, np, []byte("fresh")); err != nil || s != writeAtomic {
		t.Fatalf("new file: %q %v", s, err)
	}
	if b, _ := os.ReadFile(np); string(b) != "fresh" {
		t.Fatalf("new file content %q", b)
	}
	noTemps(t, dir)
}

func TestSafeWriteHardLinkStaysInPlace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX")
	}
	l := unrestricted(t)
	dir := t.TempDir()
	p, other := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")
	os.WriteFile(p, []byte("shared"), 0o644)
	if err := os.Link(p, other); err != nil {
		t.Skip("hard links unsupported:", err)
	}
	before := inode(t, p)
	strategy, err := writeFileSafe(bg, l, p, []byte("updated through a"))
	if err != nil || strategy != writeInPlace {
		t.Fatalf("strategy %q err %v", strategy, err)
	}
	if inode(t, p) != before {
		t.Fatal("a hard-linked file must keep its inode")
	}
	if b, _ := os.ReadFile(other); string(b) != "updated through a" {
		t.Fatalf("the other link does not see the change: %q", b)
	}
	noTemps(t, dir)
}

func TestSafeWriteFollowsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX")
	}
	l := unrestricted(t)
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	os.Mkdir(real, 0o755)
	target := filepath.Join(real, "t.txt")
	os.WriteFile(target, []byte("v1"), 0o600)
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	strategy, err := writeFileSafe(bg, l, link, []byte("v2 via link"))
	if err != nil || strategy != writeAtomic {
		t.Fatalf("strategy %q err %v", strategy, err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a file")
	}
	if b, _ := os.ReadFile(target); string(b) != "v2 via link" {
		t.Fatalf("target content %q", b)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o600 {
		t.Fatalf("target mode %v", fi.Mode())
	}
	noTemps(t, dir)
	noTemps(t, real)
}

// A connection drop at any step of the save must leave the original file untouched and no temporary file behind.
func TestSafeWriteDropKeepsOriginal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX")
	}
	for _, tc := range []struct {
		name string
		fs   func(FS) *flakyFS
	}{
		{"mid-write", func(inner FS) *flakyFS { return &flakyFS{FS: inner, dropWriteAfter: 5} }},
		{"at close", func(inner FS) *flakyFS { return &flakyFS{FS: inner, dropWriteAfter: -1, dropClose: true} }},
		{"at rename", func(inner FS) *flakyFS { return &flakyFS{FS: inner, dropWriteAfter: -1, dropRename: true} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "precious.txt")
			orig := strings.Repeat("original line\n", 1000)
			os.WriteFile(p, []byte(orig), 0o644)
			f := tc.fs(unrestricted(t))
			_, err := writeFileSafe(bg, f, p, []byte(strings.Repeat("new line\n", 5000)))
			if !errors.Is(err, errDrop) {
				t.Fatalf("err = %v, want the connection error", err)
			}
			if b, _ := os.ReadFile(p); string(b) != orig {
				t.Fatalf("original damaged: %d bytes", len(b))
			}
			noTemps(t, dir)
			if len(f.removed) == 0 || !isTmp(f.removed[0]) {
				t.Fatalf("temporary file not cleaned up: %v", f.removed)
			}
		})
	}
}

func TestSafeWriteOwnerNotKeptFallsBackInPlace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "theirs.txt")
	os.WriteFile(p, []byte("owned by someone else"), 0o666)
	before := inode(t, p)
	other := os.Getuid() + 4242
	f := &flakyFS{FS: unrestricted(t), dropWriteAfter: -1, lieUID: &other, chownDenied: true}
	strategy, err := writeFileSafe(bg, f, p, []byte("edited"))
	if err != nil || strategy != writeInPlace {
		t.Fatalf("strategy %q err %v", strategy, err)
	}
	if inode(t, p) != before {
		t.Fatal("must be rewritten in place when the owner cannot be kept")
	}
	if b, _ := os.ReadFile(p); string(b) != "edited" {
		t.Fatalf("content %q", b)
	}
	noTemps(t, dir)
}

func TestSafeWriteReadOnlyFolderFallsBackInPlace(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions as a normal user")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	os.WriteFile(p, []byte("before"), 0o644)
	os.Chmod(dir, 0o555)
	defer os.Chmod(dir, 0o755)
	strategy, err := writeFileSafe(bg, unrestricted(t), p, []byte("after"))
	if err != nil || strategy != writeInPlace {
		t.Fatalf("strategy %q err %v", strategy, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "after" {
		t.Fatalf("content %q", b)
	}
}

func TestSafeWriteJailAndProtocols(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX")
	}
	l, dir := newJail(t)
	mustWrite(t, l, "/doc.txt", "one")
	if s, err := writeFileSafe(bg, l, "/doc.txt", []byte("two")); err != nil || s != writeAtomic {
		t.Fatalf("jail: %q %v", s, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "doc.txt")); string(b) != "two" {
		t.Fatalf("jail content %q", b)
	}
	noTemps(t, dir)
	// Drivers without atomic replacement write in place (FTP, SMB); S3 / WebDAV PUT atomically.
	if s, _ := writeFileSafe(bg, &plainFS{l}, "/doc.txt", []byte("three")); s != writeInPlace {
		t.Fatalf("plain driver strategy %q", s)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "doc.txt")); string(b) != "three" {
		t.Fatalf("plain content %q", b)
	}
	if _, err := writeFileSafe(bg, l, "/", []byte("x")); err == nil {
		t.Fatal("writing a folder must fail")
	}
}

// plainFS hides the optional interfaces of the wrapped FS (a driver without safe replacement).
type plainFS struct{ FS }

func TestKeepInPlaceReason(t *testing.T) {
	cases := map[string]string{
		"1\n-rw-r--r-- 1 u g 5 Jan  1 00:00 /x\n":  "",
		"2\n-rw-r--r-- 2 u g 5 Jan  1 00:00 /x\n":  "hard links",
		"1\n-rw-rw-r--+ 1 u g 5 Jan  1 00:00 /x\n": "ACL",
		"1\n-rw-r--r--. 1 u g 5 Jan  1 00:00 /x\n": "", // SELinux context marker: not an ACL
		"": "",
	}
	for out, want := range cases {
		if got := keepInPlaceReason(out); got != want {
			t.Errorf("keepInPlaceReason(%q) = %q, want %q", out, got, want)
		}
	}
	if n := tempName("/d/" + strings.Repeat("é", 200)); len(baseName(n)) > 255 || !isTmp(n) || !strings.HasPrefix(baseName(n), ".") {
		t.Fatalf("temp name %q", n)
	}
}
