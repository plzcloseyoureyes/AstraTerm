package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// localFS is the file system of the machine running NexTerm (PROTO-29, FILE-13). Unrestricted (desktop mode) it
// exposes absolute host paths (on Windows "/C:/Users/..." with a virtual "/" listing the drives); jailed (server
// mode) every operation goes through an os.Root so nothing outside the configured root is reachable, not even via
// symlinks or "..".
type localFS struct {
	root    *os.Root // nil = unrestricted
	rootDir string   // host directory of the jail
	home    string   // API path of the start folder

	ownMu  sync.Mutex
	users  map[int]string
	groups map[int]string
}

// newLocalFS opens the host file system; rootDir != "" jails it.
func newLocalFS(rootDir string) (*localFS, error) {
	l := &localFS{users: map[int]string{}, groups: map[int]string{}}
	if rootDir == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			home = string(filepath.Separator)
		}
		l.home = hostToAPI(home)
		return l, nil
	}
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create local root: %w", err)
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("open local root: %w", err)
	}
	l.root, l.rootDir, l.home = r, abs, "/"
	return l, nil
}

func (l *localFS) Close() error {
	if l.root != nil {
		return l.root.Close()
	}
	return nil
}

// ---- path mapping -------------------------------------------------------------------------------------------------

// hostToAPI converts an absolute host path to the API form ("/home/x", "/C:/Users/x").
func hostToAPI(p string) string {
	p = filepath.ToSlash(filepath.Clean(p))
	if runtime.GOOS == "windows" {
		if len(p) >= 2 && p[1] == ':' {
			return path.Clean("/" + strings.ToUpper(p[:1]) + p[1:])
		}
		if strings.HasPrefix(p, "//") { // UNC: not exposed
			return "/"
		}
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

// isWinVirtualRoot reports whether p is the virtual "/" of unrestricted Windows access (list of drives).
func (l *localFS) isWinVirtualRoot(p string) bool {
	return l.root == nil && runtime.GOOS == "windows" && p == "/"
}

// hostPath maps an API path to a host path (unrestricted mode).
func (l *localFS) hostPath(p string) (string, error) {
	if runtime.GOOS == "windows" {
		return winHostPath(p)
	}
	return p, nil
}

// winHostPath maps "/C:/dir/file" to `C:\dir\file`. Everything after the drive must be a plain local path: a name
// containing "\" or ":" (legal in a remote POSIX listing, e.g. `..\..\x`), drive-relative forms ("/C:x"), UNC and
// device paths and reserved device names (CON, NUL, COM1...) are refused, so no name coming from a remote server can
// address anything but the file it names.
func winHostPath(p string) (string, error) {
	s := strings.TrimPrefix(p, "/")
	if len(s) < 2 || s[1] != ':' || !('a' <= s[0] && s[0] <= 'z' || 'A' <= s[0] && s[0] <= 'Z') {
		return "", &os.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	drive, rest := strings.ToUpper(s[:1])+`:\`, s[2:]
	if rest == "" || rest == "/" {
		return drive, nil
	}
	if rest[0] != '/' {
		return "", &os.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	loc, err := filepath.Localize(rest[1:])
	if err != nil {
		return "", &os.PathError{Op: "open", Path: p, Err: fmt.Errorf("invalid Windows file name: %w", fs.ErrInvalid)}
	}
	return drive + loc, nil
}

// rel maps an API path to a root-relative path (jailed mode).
func rel(p string) string {
	if p == "/" {
		return "."
	}
	return strings.TrimPrefix(p, "/")
}

// ---- primitive wrappers -------------------------------------------------------------------------------------------

func (l *localFS) lstat(p string) (os.FileInfo, error) {
	if l.root != nil {
		return l.root.Lstat(rel(p))
	}
	hp, err := l.hostPath(p)
	if err != nil {
		return nil, err
	}
	return os.Lstat(hp)
}

func (l *localFS) stat(p string) (os.FileInfo, error) {
	if l.root != nil {
		return l.root.Stat(rel(p))
	}
	hp, err := l.hostPath(p)
	if err != nil {
		return nil, err
	}
	return os.Stat(hp)
}

func (l *localFS) openFile(p string, flag int, perm os.FileMode) (*os.File, error) {
	if l.root != nil {
		return l.root.OpenFile(rel(p), flag, perm)
	}
	hp, err := l.hostPath(p)
	if err != nil {
		return nil, err
	}
	return os.OpenFile(hp, flag, perm)
}

// ---- FS implementation --------------------------------------------------------------------------------------------

func (l *localFS) entryFromInfo(p string, fi os.FileInfo) *Entry {
	e := &Entry{Name: baseName(p), Path: p, Size: fi.Size(), Mtime: fi.ModTime().UTC()}
	if p == "/" {
		e.Name = "/"
	}
	mode, uid, gid, ok := sysStat(fi)
	if !ok {
		mode = goModeToPOSIX(fi.Mode())
	} else {
		e.UID, e.GID = intPtr(uid), intPtr(gid)
	}
	e.Mode = mode
	e.Type = typeFromMode(mode)
	if e.Type == "dir" {
		e.Size = 0
	}
	return e
}

func (l *localFS) finishLink(p string, e *Entry) {
	if e.Type != "symlink" {
		return
	}
	if t, err := l.Readlink(context.Background(), p); err == nil {
		e.LinkTarget = t
	}
}

func (l *localFS) List(ctx context.Context, dir string) ([]*Entry, error) {
	if l.isWinVirtualRoot(dir) {
		return winDrives(), nil
	}
	var f *os.File
	var err error
	if l.root != nil {
		f, err = l.root.Open(rel(dir))
	} else {
		var hp string
		if hp, err = l.hostPath(dir); err == nil {
			f, err = os.Open(hp)
		}
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	des, err := f.ReadDir(-1)
	if err != nil && len(des) == 0 {
		return nil, err
	}
	out := make([]*Entry, 0, len(des))
	for _, de := range des {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		p := joinPath(dir, de.Name())
		fi, err := de.Info()
		if err != nil {
			continue // vanished meanwhile
		}
		e := l.entryFromInfo(p, fi)
		l.finishLink(p, e)
		out = append(out, e)
	}
	return out, nil
}

func (l *localFS) Stat(ctx context.Context, p string) (*Entry, error) {
	if l.isWinVirtualRoot(p) {
		return &Entry{Name: "/", Path: "/", Type: "dir", Mode: sIFDIR | 0o555}, nil
	}
	fi, err := l.stat(p)
	if err != nil {
		return nil, err
	}
	return l.entryFromInfo(p, fi), nil
}

func (l *localFS) Lstat(ctx context.Context, p string) (*Entry, error) {
	if l.isWinVirtualRoot(p) {
		return l.Stat(ctx, p)
	}
	fi, err := l.lstat(p)
	if err != nil {
		return nil, err
	}
	e := l.entryFromInfo(p, fi)
	l.finishLink(p, e)
	return e, nil
}

func (l *localFS) Open(ctx context.Context, p string, offset int64) (io.ReadCloser, error) {
	f, err := l.openFile(p, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	if fi, err := f.Stat(); err == nil && fi.IsDir() {
		f.Close()
		return nil, &os.PathError{Op: "open", Path: p, Err: errIsDir}
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

var errIsDir = errors.New("is a directory")

func (l *localFS) Create(ctx context.Context, p string, offset int64) (io.WriteCloser, error) {
	flag := os.O_WRONLY | os.O_CREATE
	if offset <= 0 {
		flag |= os.O_TRUNC
	}
	f, err := l.openFile(p, flag, 0o644)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if err := f.Truncate(offset); err != nil {
			f.Close()
			return nil, err
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

func (l *localFS) Mkdir(ctx context.Context, p string) error {
	if l.root != nil {
		return l.root.Mkdir(rel(p), 0o755)
	}
	hp, err := l.hostPath(p)
	if err != nil {
		return err
	}
	return os.Mkdir(hp, 0o755)
}

func (l *localFS) MkdirAll(ctx context.Context, p string) error {
	if l.root != nil {
		if p == "/" {
			return nil
		}
		return l.root.MkdirAll(rel(p), 0o755)
	}
	hp, err := l.hostPath(p)
	if err != nil {
		return err
	}
	return os.MkdirAll(hp, 0o755)
}

func (l *localFS) Remove(ctx context.Context, p string) error {
	if l.root != nil {
		return l.root.Remove(rel(p))
	}
	hp, err := l.hostPath(p)
	if err != nil {
		return err
	}
	return os.Remove(hp)
}

func (l *localFS) RemoveAll(ctx context.Context, p string) error {
	return removeAllGeneric(ctx, l, p)
}

func (l *localFS) Rename(ctx context.Context, from, to string) error {
	if l.root != nil {
		return l.root.Rename(rel(from), rel(to))
	}
	hf, err := l.hostPath(from)
	if err != nil {
		return err
	}
	ht, err := l.hostPath(to)
	if err != nil {
		return err
	}
	return os.Rename(hf, ht)
}

func (l *localFS) Chmod(ctx context.Context, p string, perm uint32) error {
	mode := posixPermToGo(perm)
	if l.root != nil {
		return l.root.Chmod(rel(p), mode)
	}
	hp, err := l.hostPath(p)
	if err != nil {
		return err
	}
	return os.Chmod(hp, mode)
}

func (l *localFS) Chown(ctx context.Context, p string, uid, gid int) error {
	if runtime.GOOS == "windows" {
		return ErrNotSupported
	}
	if l.root != nil {
		return l.root.Lchown(rel(p), uid, gid)
	}
	return os.Lchown(p, uid, gid)
}

func (l *localFS) Chtimes(ctx context.Context, p string, atime, mtime time.Time) error {
	if l.root != nil {
		return l.root.Chtimes(rel(p), atime, mtime)
	}
	hp, err := l.hostPath(p)
	if err != nil {
		return err
	}
	return os.Chtimes(hp, atime, mtime)
}

func (l *localFS) Symlink(ctx context.Context, target, link string) error {
	if l.root != nil {
		return l.root.Symlink(target, rel(link))
	}
	hp, err := l.hostPath(link)
	if err != nil {
		return err
	}
	return os.Symlink(target, hp)
}

func (l *localFS) Link(ctx context.Context, oldname, newname string) error {
	if l.root != nil {
		return l.root.Link(rel(oldname), rel(newname))
	}
	ho, err := l.hostPath(oldname)
	if err != nil {
		return err
	}
	hn, err := l.hostPath(newname)
	if err != nil {
		return err
	}
	return os.Link(ho, hn)
}

func (l *localFS) Readlink(ctx context.Context, p string) (string, error) {
	if l.root != nil {
		return l.root.Readlink(rel(p))
	}
	hp, err := l.hostPath(p)
	if err != nil {
		return "", err
	}
	t, err := os.Readlink(hp)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" && filepath.IsAbs(t) {
		return hostToAPI(t), nil
	}
	return filepath.ToSlash(t), nil
}

func (l *localFS) Realpath(ctx context.Context, p string) (string, error) {
	if l.isWinVirtualRoot(p) {
		return "/", nil
	}
	if l.root != nil {
		// Inside the jail symlinks are resolved by os.Root itself; report the cleaned path of an existing entry.
		if _, err := l.root.Stat(rel(p)); err != nil {
			return "", err
		}
		return p, nil
	}
	hp, err := l.hostPath(p)
	if err != nil {
		return "", err
	}
	r, err := filepath.EvalSymlinks(hp)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(r)
	if err != nil {
		return "", err
	}
	return hostToAPI(abs), nil
}

func (l *localFS) Home(ctx context.Context) (string, error) { return l.home, nil }

// atomicReplace: rename(2) replaces atomically; on Windows saves stay in place (a replacement would get the folder's
// inherited ACL instead of the file's own).
func (l *localFS) atomicReplace(ctx context.Context) bool { return runtime.GOOS != "windows" }

// keepInPlace reports hard links and (Linux) POSIX ACLs of p.
func (l *localFS) keepInPlace(ctx context.Context, p string) string {
	fi, err := l.lstat(p)
	if err != nil {
		return ""
	}
	if n := sysNlink(fi); n > 1 {
		return "hard links"
	}
	if l.root == nil && hasPosixACL(p) {
		return "ACL"
	}
	if l.root != nil && hasPosixACL(filepath.Join(l.rootDir, filepath.FromSlash(rel(p)))) {
		return "ACL"
	}
	return ""
}

func (l *localFS) Space(ctx context.Context, p string) (Space, error) {
	hp := p
	if l.root != nil {
		hp = filepath.Join(l.rootDir, filepath.FromSlash(rel(p)))
	} else {
		var err error
		if hp, err = l.hostPath(p); err != nil {
			return Space{}, err
		}
	}
	return diskSpace(hp)
}

// nameOwners fills owner / group names from the host user database (cached).
func (l *localFS) nameOwners(ctx context.Context, entries []*Entry) {
	for _, e := range entries {
		if e.UID != nil && e.Owner == "" {
			e.Owner = l.lookup(true, *e.UID)
		}
		if e.GID != nil && e.Group == "" {
			e.Group = l.lookup(false, *e.GID)
		}
	}
}

func (l *localFS) lookup(user bool, id int) string {
	l.ownMu.Lock()
	m := l.groups
	if user {
		m = l.users
	}
	name, ok := m[id]
	l.ownMu.Unlock()
	if ok {
		return name
	}
	name = lookupOwnerName(user, id)
	l.ownMu.Lock()
	m[id] = name
	l.ownMu.Unlock()
	return name
}

// goModeToPOSIX converts a Go FileMode to st_mode (type bits + permission bits).
func goModeToPOSIX(m fs.FileMode) uint32 {
	mode := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		mode |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		mode |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		mode |= 0o1000
	}
	switch {
	case m.IsDir():
		mode |= sIFDIR
	case m&fs.ModeSymlink != 0:
		mode |= sIFLNK
	case m&fs.ModeNamedPipe != 0:
		mode |= sIFIFO
	case m&fs.ModeSocket != 0:
		mode |= sIFSOCK
	case m&fs.ModeCharDevice != 0:
		mode |= sIFCHR
	case m&fs.ModeDevice != 0:
		mode |= sIFBLK
	default:
		mode |= sIFREG
	}
	return mode
}

// posixPermToGo converts permission bits (0o7777) to a Go FileMode.
func posixPermToGo(perm uint32) fs.FileMode {
	m := fs.FileMode(perm & 0o777)
	if perm&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if perm&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if perm&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m
}

// winDrives lists the existing drive letters as directory entries of the virtual Windows root.
func winDrives() []*Entry {
	var out []*Entry
	for c := 'A'; c <= 'Z'; c++ {
		d := string(c) + ":\\"
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, &Entry{Name: string(c) + ":", Path: "/" + string(c) + ":", Type: "dir",
				Mode: sIFDIR | 0o755, Mtime: fi.ModTime().UTC()})
		}
	}
	return out
}
