package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/studio-b12/gowebdav"
)

// webdavFS is the WebDAV driver (PROTO-27) on studio-b12/gowebdav. WebDAV has no partial writes, so Create stages the
// data in a local temporary file and PUTs it with a known length on Close (resumable browser uploads are staged by the
// upload handler).
type webdavFS struct {
	c       *gowebdav.Client
	tmpDir  string
	home    string
	closeFn func()
}

func newWebDAVFS(c *gowebdav.Client, tmpDir, home string, closeFn func()) *webdavFS {
	if home == "" {
		home = "/"
	}
	return &webdavFS{c: c, tmpDir: tmpDir, home: home, closeFn: closeFn}
}

func (w *webdavFS) Close() error {
	if w.closeFn != nil {
		w.closeFn()
	}
	return nil
}

// davErr maps gowebdav status errors to fs errors.
func davErr(err error, p string) error {
	if err == nil {
		return nil
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		var se gowebdav.StatusError
		if errors.As(pe.Err, &se) {
			switch se.Status {
			case http.StatusNotFound, http.StatusGone:
				return &os.PathError{Op: pe.Op, Path: p, Err: fs.ErrNotExist}
			case http.StatusUnauthorized, http.StatusForbidden:
				return &os.PathError{Op: pe.Op, Path: p, Err: fs.ErrPermission}
			case http.StatusMethodNotAllowed, http.StatusPreconditionFailed:
				return &os.PathError{Op: pe.Op, Path: p, Err: fs.ErrExist}
			case http.StatusConflict:
				return &os.PathError{Op: pe.Op, Path: p, Err: fs.ErrNotExist} // missing parent collection
			case http.StatusInsufficientStorage:
				return fmt.Errorf("webdav: insufficient storage")
			}
			return fmt.Errorf("webdav: %s failed with HTTP %d", pe.Op, se.Status)
		}
	}
	return err
}

func davEntry(p string, fi os.FileInfo) *Entry {
	e := &Entry{Name: baseName(p), Path: p, Size: fi.Size(), Mtime: fi.ModTime().UTC()}
	if fi.IsDir() {
		e.Type, e.Mode, e.Size = "dir", sIFDIR|0o755, 0
	} else {
		e.Type, e.Mode = "file", sIFREG|0o644
	}
	return e
}

func (w *webdavFS) List(ctx context.Context, dir string) ([]*Entry, error) {
	fis, err := w.c.ReadDir(dir)
	if err != nil {
		return nil, davErr(err, dir)
	}
	out := make([]*Entry, 0, len(fis))
	for _, fi := range fis {
		name := strings.TrimSuffix(fi.Name(), "/")
		if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
			continue
		}
		out = append(out, davEntry(joinPath(dir, name), fi))
	}
	return out, nil
}

func (w *webdavFS) Stat(ctx context.Context, p string) (*Entry, error) {
	if p == "/" {
		return &Entry{Name: "/", Path: "/", Type: "dir", Mode: sIFDIR | 0o755}, nil
	}
	fi, err := w.c.Stat(p)
	if err != nil {
		return nil, davErr(err, p)
	}
	return davEntry(p, fi), nil
}

func (w *webdavFS) Lstat(ctx context.Context, p string) (*Entry, error) { return w.Stat(ctx, p) }

func (w *webdavFS) Open(ctx context.Context, p string, offset int64) (io.ReadCloser, error) {
	var r io.ReadCloser
	var err error
	if offset > 0 {
		// An explicit (huge) length: with length 0 gowebdav would return an empty reader when the server ignores
		// Range and answers 200 (it then skips offset bytes itself and limits the rest to length).
		r, err = w.c.ReadStreamRange(p, offset, int64(math.MaxInt)-offset)
	} else {
		r, err = w.c.ReadStream(p)
	}
	if err != nil {
		return nil, davErr(err, p)
	}
	return r, nil
}

func (w *webdavFS) Create(ctx context.Context, p string, offset int64) (io.WriteCloser, error) {
	if offset > 0 {
		return nil, fmt.Errorf("webdav cannot write at an offset: %w", ErrNotSupported)
	}
	if w.tmpDir != "" {
		_ = os.MkdirAll(w.tmpDir, 0o700)
	}
	f, err := os.CreateTemp(w.tmpDir, "webdav-*.part")
	if err != nil {
		return nil, err
	}
	return &davWriter{w: w, p: p, f: f}, nil
}

type davWriter struct {
	w       *webdavFS
	p       string
	f       *os.File
	closed  bool
	err     error
	n       int64 // bytes staged
	checked int64 // n at the last free-space check
}

func (d *davWriter) Write(b []byte) (int, error) {
	if d.n-d.checked >= 64<<20 || d.checked == 0 {
		// The whole file is staged on the AstraTerm host: keep its disk from filling up.
		if err := stagingRoom(d.w.tmpDir, 64<<20); err != nil {
			return 0, err
		}
		d.checked = max(d.n, 1)
	}
	n, err := d.f.Write(b)
	d.n += int64(n)
	return n, err
}

func (d *davWriter) Close() error {
	if d.closed {
		return d.err
	}
	d.closed = true
	defer os.Remove(d.f.Name())
	defer d.f.Close()
	size, err := d.f.Seek(0, io.SeekCurrent)
	if err == nil {
		_, err = d.f.Seek(0, io.SeekStart)
	}
	if err == nil {
		err = davErr(d.w.c.WriteStreamWithLength(d.p, d.f, size, 0o644), d.p)
	}
	d.err = err
	return err
}

func (w *webdavFS) Mkdir(ctx context.Context, p string) error {
	if _, err := w.c.Stat(p); err == nil {
		return &os.PathError{Op: "mkdir", Path: p, Err: fs.ErrExist}
	}
	return davErr(w.c.Mkdir(p, 0o755), p)
}

func (w *webdavFS) MkdirAll(ctx context.Context, p string) error {
	return davErr(w.c.MkdirAll(p, 0o755), p)
}

func (w *webdavFS) Remove(ctx context.Context, p string) error {
	e, err := w.Stat(ctx, p)
	if err != nil {
		return err
	}
	if e.Type == "dir" {
		children, err := w.List(ctx, p)
		if err != nil {
			return err
		}
		if len(children) > 0 {
			return errNotEmpty
		}
	}
	return davErr(w.c.Remove(p), p)
}

func (w *webdavFS) RemoveAll(ctx context.Context, p string) error {
	if p == "/" {
		return fmt.Errorf("refusing to delete the root directory")
	}
	err := davErr(w.c.RemoveAll(p), p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (w *webdavFS) Rename(ctx context.Context, from, to string) error {
	return davErr(w.c.Rename(from, to, true), from)
}

func (w *webdavFS) CopyServerSide(ctx context.Context, src, dst string) error {
	return davErr(w.c.Copy(src, dst, false), src)
}

func (w *webdavFS) Chmod(ctx context.Context, p string, perm uint32) error  { return ErrNotSupported }
func (w *webdavFS) Chown(ctx context.Context, p string, uid, gid int) error { return ErrNotSupported }
func (w *webdavFS) Chtimes(ctx context.Context, p string, a, m time.Time) error {
	return ErrNotSupported
}
func (w *webdavFS) Symlink(ctx context.Context, target, link string) error { return ErrNotSupported }
func (w *webdavFS) Readlink(ctx context.Context, p string) (string, error) {
	return "", ErrNotSupported
}
func (w *webdavFS) Home(ctx context.Context) (string, error) { return w.home, nil }

func (w *webdavFS) Realpath(ctx context.Context, p string) (string, error) {
	if _, err := w.Stat(ctx, p); err != nil {
		return "", err
	}
	return p, nil
}
