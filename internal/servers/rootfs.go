package servers

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/afero"
)

// rootFS is a jailed view of a shared folder. Every path is resolved inside the root with os.Root, so neither ".."
// nor symbolic links (absolute, or relative ones leading outside) can escape it.
type rootFS struct {
	root     *os.Root
	readOnly bool
}

func openRootFS(dir string, readOnly bool) (*rootFS, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &rootFS{root: r, readOnly: readOnly}, nil
}

func (r *rootFS) Close() error { return r.root.Close() }

// withReadOnly returns a view sharing the same root with another write permission.
func (r *rootFS) withReadOnly(ro bool) *rootFS {
	if ro == r.readOnly {
		return r
	}
	return &rootFS{root: r.root, readOnly: ro}
}

// relName converts a client path ("/a/b", "a/b", "", "/") into a root-relative name ("a/b", "."). Clients always use
// "/" separators; on Windows backslashes are separators too.
func relName(p string) string {
	if runtime.GOOS == "windows" {
		p = strings.ReplaceAll(p, `\`, "/")
	}
	p = strings.TrimPrefix(path.Clean("/"+p), "/")
	if p == "" {
		return "."
	}
	return filepath.FromSlash(p)
}

func errPerm(op, name string) error {
	return &os.PathError{Op: op, Path: name, Err: os.ErrPermission}
}

func writeFlags(flag int) bool {
	return flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0
}

func (r *rootFS) Open(name string) (*os.File, error) { return r.root.Open(relName(name)) }

// errNotRegular refuses transfers of anything but regular files (FIFOs, devices, sockets).
var errNotRegular = errors.New("not a regular file")

// OpenRegular opens a regular file for reading. The type is checked before opening: opening a FIFO (or some device)
// blocks until a writer appears, which would hang the transfer goroutine and a later server stop.
func (r *rootFS) OpenRegular(name string) (*os.File, error) {
	st, err := r.Stat(name)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, &os.PathError{Op: "open", Path: name, Err: errNotRegular}
	}
	f, err := r.Open(name)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, &os.PathError{Op: "open", Path: name, Err: errNotRegular}
	}
	return f, nil
}

// checkUploadTarget refuses writing to an existing name that is not a regular file (opening a FIFO for writing
// blocks; directories and devices are no upload targets).
func (r *rootFS) checkUploadTarget(name string) error {
	st, err := r.Stat(name)
	if err != nil {
		return nil // missing (created by the open) or unreachable (the open reports it)
	}
	if !st.Mode().IsRegular() {
		return &os.PathError{Op: "open", Path: name, Err: errNotRegular}
	}
	return nil
}

func (r *rootFS) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	if r.readOnly && writeFlags(flag) {
		return nil, errPerm("open", name)
	}
	return r.root.OpenFile(relName(name), flag, perm)
}

func (r *rootFS) Stat(name string) (os.FileInfo, error)  { return r.root.Stat(relName(name)) }
func (r *rootFS) Lstat(name string) (os.FileInfo, error) { return r.root.Lstat(relName(name)) }

func (r *rootFS) Mkdir(name string, perm os.FileMode) error {
	if r.readOnly {
		return errPerm("mkdir", name)
	}
	return r.root.Mkdir(relName(name), perm)
}

func (r *rootFS) MkdirAll(name string, perm os.FileMode) error {
	if r.readOnly {
		return errPerm("mkdir", name)
	}
	return r.root.MkdirAll(relName(name), perm)
}

func (r *rootFS) Remove(name string) error {
	if r.readOnly {
		return errPerm("remove", name)
	}
	rel := relName(name)
	if rel == "." {
		return errPerm("remove", name)
	}
	return r.root.Remove(rel)
}

func (r *rootFS) RemoveAll(name string) error {
	if r.readOnly {
		return errPerm("remove", name)
	}
	rel := relName(name)
	if rel == "." {
		return errPerm("remove", name)
	}
	return r.root.RemoveAll(rel)
}

func (r *rootFS) Rename(from, to string) error {
	if r.readOnly {
		return errPerm("rename", from)
	}
	a, b := relName(from), relName(to)
	if a == "." || b == "." {
		return errPerm("rename", from)
	}
	return r.root.Rename(a, b)
}

func (r *rootFS) Chmod(name string, mode os.FileMode) error {
	rel := relName(name)
	if r.readOnly || rel == "." {
		// The shared folder itself keeps its permissions (a client could lock everyone out or open it to all).
		return errPerm("chmod", name)
	}
	return r.root.Chmod(rel, mode&fs.ModePerm)
}

func (r *rootFS) Chtimes(name string, atime, mtime time.Time) error {
	if r.readOnly {
		return errPerm("chtimes", name)
	}
	return r.root.Chtimes(relName(name), atime, mtime)
}

func (r *rootFS) Readlink(name string) (string, error) { return r.root.Readlink(relName(name)) }

// Symlink creates link → target. The target must be relative and must not contain "..": a lexical check of
// "dir/../x" is not enough, because the link's parent may itself be a symbolic link (e.g. "a" → ".") so that the
// real location of the link is shallower than its name suggests and the target would point outside the shared folder
// for every program that does not resolve paths through os.Root. Without "..", a link always resolves below its real
// parent, which lies inside the root.
func (r *rootFS) Symlink(target, link string) error {
	if r.readOnly {
		return errPerm("symlink", link)
	}
	l := relName(link)
	t := strings.ReplaceAll(target, `\`, "/")
	if t == "" || l == "." || path.IsAbs(t) || filepath.IsAbs(target) || !filepath.IsLocal(filepath.FromSlash(t)) {
		return errPerm("symlink", link)
	}
	for _, seg := range strings.Split(t, "/") {
		if seg == ".." {
			return errPerm("symlink", link)
		}
	}
	return r.root.Symlink(filepath.FromSlash(t), l)
}

// Link creates a hard link (both names inside the root).
func (r *rootFS) Link(oldname, newname string) error {
	if r.readOnly {
		return errPerm("link", newname)
	}
	return r.root.Link(relName(oldname), relName(newname))
}

// ReadDir lists a directory (sorted by name).
func (r *rootFS) ReadDir(name string) ([]os.FileInfo, error) {
	f, err := r.root.Open(relName(name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	list, err := f.Readdir(-1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name() < list[j].Name() })
	return list, nil
}

// ---- afero adapter (FTP) ------------------------------------------------------------------------------------------

// aferoFS adapts rootFS to afero.Fs for ftpserverlib.
type aferoFS struct{ r *rootFS }

var _ afero.Fs = aferoFS{}

func (a aferoFS) Create(name string) (afero.File, error) {
	return a.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
}
func (a aferoFS) Mkdir(name string, perm os.FileMode) error    { return a.r.Mkdir(name, perm) }
func (a aferoFS) MkdirAll(name string, perm os.FileMode) error { return a.r.MkdirAll(name, perm) }
func (a aferoFS) Open(name string) (afero.File, error) {
	f, err := a.r.Open(name)
	if err != nil {
		return nil, err
	}
	return f, nil
}
func (a aferoFS) OpenFile(name string, flag int, perm os.FileMode) (afero.File, error) {
	f, err := a.r.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return f, nil
}
func (a aferoFS) Remove(name string) error              { return a.r.Remove(name) }
func (a aferoFS) RemoveAll(name string) error           { return a.r.RemoveAll(name) }
func (a aferoFS) Rename(from, to string) error          { return a.r.Rename(from, to) }
func (a aferoFS) Stat(name string) (os.FileInfo, error) { return a.r.Stat(name) }
func (a aferoFS) Name() string                          { return "nexterm-root" }
func (a aferoFS) Chmod(name string, mode os.FileMode) error {
	return a.r.Chmod(name, mode)
}
func (a aferoFS) Chown(name string, _, _ int) error { return errPerm("chown", name) }
func (a aferoFS) Chtimes(name string, atime, mtime time.Time) error {
	return a.r.Chtimes(name, atime, mtime)
}

// LstatIfPossible implements afero.Lstater.
func (a aferoFS) LstatIfPossible(name string) (os.FileInfo, bool, error) {
	fi, err := a.r.Lstat(name)
	return fi, true, err
}

// ---- transfer accounting ------------------------------------------------------------------------------------------

// transferFile wraps an open file of a transfer: it counts bytes (server stats and client), records the client's
// current activity and logs the completed transfer once on Close.
type transferFile struct {
	*os.File
	name    string
	upload  bool
	read    atomic.Int64
	written atomic.Int64
	once    sync.Once
	failed  atomic.Bool
	done    func(t *transferFile)
	// appendMu is set for SFTP appends: writes go to the end of the file whatever their offset.
	appendMu *sync.Mutex
}

func (t *transferFile) Read(p []byte) (int, error) {
	n, err := t.File.Read(p)
	t.read.Add(int64(n))
	return n, err
}

func (t *transferFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := t.File.ReadAt(p, off)
	t.read.Add(int64(n))
	return n, err
}

func (t *transferFile) Write(p []byte) (int, error) {
	n, err := t.File.Write(p)
	t.written.Add(int64(n))
	return n, err
}

func (t *transferFile) WriteAt(p []byte, off int64) (int, error) {
	if t.appendMu != nil {
		t.appendMu.Lock()
		defer t.appendMu.Unlock()
		end, err := t.File.Seek(0, io.SeekEnd)
		if err != nil {
			return 0, err
		}
		off = end
	}
	n, err := t.File.WriteAt(p, off)
	t.written.Add(int64(n))
	return n, err
}

// ReadFrom and WriteTo hide *os.File's fast paths (copy_file_range / sendfile), which would bypass the byte
// accounting and the abort-on-stop behavior of Read / Write.
func (t *transferFile) ReadFrom(r io.Reader) (int64, error) { return io.Copy(writerOnly{t}, r) }

func (t *transferFile) WriteTo(w io.Writer) (int64, error) { return io.Copy(w, readerOnly{t}) }

type writerOnly struct{ io.Writer }

type readerOnly struct{ io.Reader }

// TransferError implements the ftpserverlib / pkg/sftp interruption hooks.
func (t *transferFile) TransferError(error) { t.failed.Store(true) }

func (t *transferFile) Close() error {
	err := t.File.Close()
	t.once.Do(func() {
		if t.done != nil {
			t.done(t)
		}
	})
	return err
}

// bytes returns the payload size moved through the file.
func (t *transferFile) bytes() int64 {
	if t.upload {
		return t.written.Load()
	}
	return t.read.Load()
}

// openFiles tracks the files of running transfers so stopping a server aborts them.
type openFiles struct {
	mu     sync.Mutex
	m      map[*transferFile]struct{}
	closed bool
}

func (o *openFiles) add(t *transferFile) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return false
	}
	if o.m == nil {
		o.m = map[*transferFile]struct{}{}
	}
	o.m[t] = struct{}{}
	return true
}

func (o *openFiles) remove(t *transferFile) {
	o.mu.Lock()
	delete(o.m, t)
	o.mu.Unlock()
}

// closeAll closes every open transfer file (their copies fail and the transfers abort).
func (o *openFiles) closeAll() {
	o.mu.Lock()
	o.closed = true
	list := make([]*transferFile, 0, len(o.m))
	for t := range o.m {
		list = append(list, t)
	}
	o.mu.Unlock()
	for _, t := range list {
		t.failed.Store(true)
		_ = t.File.Close()
	}
}
