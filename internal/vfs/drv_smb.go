package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hirochachacha/go-smb2"
)

// smbFS is the SMB2/3 driver (PROTO-27) on hirochachacha/go-smb2 over Termstead's Dialer. With a configured share the
// share root is "/"; without one "/" lists the shares and paths are /share/dir/file. Broken sessions are re-dialed
// on the next operation.
type smbFS struct {
	host, share string
	user, pass  string
	domain      string
	home        string
	dial        func(ctx context.Context) (net.Conn, error)
	closeFn     func()

	mu     sync.Mutex
	conn   net.Conn
	sess   *smb2.Session
	mounts map[string]*smb2.Share
	closed bool
}

func newSMBFS(ctx context.Context, host, share, user, pass, domain, home string, dial func(ctx context.Context) (net.Conn, error)) (*smbFS, error) {
	s := &smbFS{host: host, share: strings.Trim(share, `/\`), user: user, pass: pass, domain: domain, dial: dial,
		mounts: map[string]*smb2.Share{}, home: home}
	if s.home == "" {
		s.home = "/"
	}
	if _, err := s.session(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *smbFS) session(ctx context.Context) (*smb2.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errHandleClosed
	}
	if s.sess != nil {
		return s.sess, nil
	}
	conn, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: s.user, Password: s.pass, Domain: s.domain}}
	sess, err := d.DialContext(ctx, conn)
	if err != nil {
		conn.Close()
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "logon") || strings.Contains(msg, "access") || strings.Contains(msg, "password") {
			return nil, fmt.Errorf("SMB login failed: %w", fs.ErrPermission)
		}
		return nil, fmt.Errorf("SMB connection failed: %w", err)
	}
	s.conn, s.sess = conn, sess
	return sess, nil
}

// reset drops a broken session so the next call re-dials.
func (s *smbFS) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.mounts {
		_ = m.Umount()
	}
	s.mounts = map[string]*smb2.Share{}
	if s.sess != nil {
		_ = s.sess.Logoff()
	}
	if s.conn != nil {
		s.conn.Close()
	}
	s.sess, s.conn = nil, nil
}

func (s *smbFS) Close() error {
	s.reset()
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	if s.closeFn != nil {
		s.closeFn()
	}
	return nil
}

// resolve maps an API path to (share, path inside the share).
func (s *smbFS) resolve(ctx context.Context, p string) (*smb2.Share, string, error) {
	rest := strings.TrimPrefix(p, "/")
	share := s.share
	if share == "" {
		share, rest, _ = strings.Cut(rest, "/")
		if share == "" {
			return nil, "", nil // virtual root
		}
	}
	// SMB separates with "\": a "\" inside an API path element (a legal POSIX name, e.g. from a transfer source)
	// would address another folder, and a share name is one plain element of \\host\share.
	if strings.ContainsAny(rest, `\:`+"\x00") || strings.ContainsAny(share, `\/:`+"\x00") {
		return nil, "", &os.PathError{Op: "smb", Path: p, Err: fmt.Errorf("invalid SMB file name: %w", fs.ErrInvalid)}
	}
	sess, err := s.session(ctx)
	if err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	m := s.mounts[share]
	s.mu.Unlock()
	if m == nil {
		m, err = sess.Mount(`\\` + s.host + `\` + share)
		if err != nil {
			if isDisconnect(err) {
				s.reset()
			}
			return nil, "", smbErr(err, p)
		}
		s.mu.Lock()
		s.mounts[share] = m
		s.mu.Unlock()
	}
	return m.WithContext(ctx), strings.ReplaceAll(rest, "/", `\`), nil
}

func smbErr(err error, p string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission), errors.Is(err, fs.ErrExist):
		return err
	case strings.Contains(msg, "OBJECT_NAME_NOT_FOUND"), strings.Contains(msg, "OBJECT_PATH_NOT_FOUND"),
		strings.Contains(msg, "BAD_NETWORK_NAME"), strings.Contains(msg, "NO_SUCH_FILE"):
		return &os.PathError{Op: "smb", Path: p, Err: fs.ErrNotExist}
	case strings.Contains(msg, "ACCESS_DENIED"):
		return &os.PathError{Op: "smb", Path: p, Err: fs.ErrPermission}
	case strings.Contains(msg, "OBJECT_NAME_COLLISION"):
		return &os.PathError{Op: "smb", Path: p, Err: fs.ErrExist}
	case strings.Contains(msg, "DIRECTORY_NOT_EMPTY"):
		return errNotEmpty
	}
	return err
}

// op runs fn and drops the session when the transport failed.
func (s *smbFS) op(err error, p string) error {
	var te *smb2.TransportError
	if errors.As(err, &te) || isDisconnect(err) {
		s.reset()
	}
	return smbErr(err, p)
}

func smbEntry(p string, fi os.FileInfo) *Entry {
	e := &Entry{Name: baseName(p), Path: p, Size: fi.Size(), Mtime: fi.ModTime().UTC()}
	e.Mode = goModeToPOSIX(fi.Mode())
	e.Type = typeFromMode(e.Mode)
	if e.Type == "dir" {
		e.Size = 0
	}
	return e
}

func (s *smbFS) List(ctx context.Context, dir string) ([]*Entry, error) {
	sh, rp, err := s.resolve(ctx, dir)
	if err != nil {
		return nil, err
	}
	if sh == nil {
		sess, err := s.session(ctx)
		if err != nil {
			return nil, err
		}
		names, err := sess.ListSharenames()
		if err != nil {
			return nil, s.op(err, dir)
		}
		sort.Strings(names)
		var out []*Entry
		for _, n := range names {
			if strings.HasSuffix(n, "$") {
				continue // administrative / IPC shares
			}
			out = append(out, &Entry{Name: n, Path: "/" + n, Type: "dir", Mode: sIFDIR | 0o755})
		}
		return out, nil
	}
	fis, err := sh.ReadDir(rp)
	if err != nil {
		return nil, s.op(err, dir)
	}
	out := make([]*Entry, 0, len(fis))
	for _, fi := range fis {
		if n := fi.Name(); n == "." || n == ".." {
			continue
		}
		out = append(out, smbEntry(joinPath(dir, fi.Name()), fi))
	}
	return out, nil
}

func (s *smbFS) Stat(ctx context.Context, p string) (*Entry, error) {
	sh, rp, err := s.resolve(ctx, p)
	if err != nil {
		return nil, err
	}
	if sh == nil {
		return &Entry{Name: "/", Path: "/", Type: "dir", Mode: sIFDIR | 0o755}, nil
	}
	if rp == "" {
		return &Entry{Name: baseName(p), Path: p, Type: "dir", Mode: sIFDIR | 0o755}, nil
	}
	fi, err := sh.Stat(rp)
	if err != nil {
		return nil, s.op(err, p)
	}
	return smbEntry(p, fi), nil
}

func (s *smbFS) Lstat(ctx context.Context, p string) (*Entry, error) { return s.Stat(ctx, p) }

func (s *smbFS) Open(ctx context.Context, p string, offset int64) (io.ReadCloser, error) {
	sh, rp, err := s.resolve(ctx, p)
	if err != nil {
		return nil, err
	}
	if sh == nil || rp == "" {
		return nil, &os.PathError{Op: "open", Path: p, Err: errIsDir}
	}
	f, err := sh.Open(rp)
	if err != nil {
		return nil, s.op(err, p)
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return nil, s.op(err, p)
		}
	}
	return f, nil
}

func (s *smbFS) Create(ctx context.Context, p string, offset int64) (io.WriteCloser, error) {
	sh, rp, err := s.resolve(ctx, p)
	if err != nil {
		return nil, err
	}
	if sh == nil || rp == "" {
		return nil, &os.PathError{Op: "create", Path: p, Err: fs.ErrInvalid}
	}
	flag := os.O_WRONLY | os.O_CREATE
	if offset <= 0 {
		flag |= os.O_TRUNC
	}
	f, err := sh.OpenFile(rp, flag, 0o644)
	if err != nil {
		return nil, s.op(err, p)
	}
	if offset > 0 {
		if err := f.Truncate(offset); err != nil {
			f.Close()
			return nil, s.op(err, p)
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return nil, s.op(err, p)
		}
	}
	return f, nil
}

func (s *smbFS) Mkdir(ctx context.Context, p string) error {
	sh, rp, err := s.resolve(ctx, p)
	if err != nil {
		return err
	}
	if sh == nil || rp == "" {
		return &os.PathError{Op: "mkdir", Path: p, Err: fs.ErrExist}
	}
	return s.op(sh.Mkdir(rp, 0o755), p)
}

func (s *smbFS) MkdirAll(ctx context.Context, p string) error {
	sh, rp, err := s.resolve(ctx, p)
	if err != nil {
		return err
	}
	if sh == nil || rp == "" {
		return nil
	}
	return s.op(sh.MkdirAll(rp, 0o755), p)
}

func (s *smbFS) Remove(ctx context.Context, p string) error {
	sh, rp, err := s.resolve(ctx, p)
	if err != nil {
		return err
	}
	if sh == nil || rp == "" {
		return fmt.Errorf("refusing to delete a share")
	}
	return s.op(sh.Remove(rp), p)
}

func (s *smbFS) RemoveAll(ctx context.Context, p string) error { return removeAllGeneric(ctx, s, p) }

func (s *smbFS) Rename(ctx context.Context, from, to string) error {
	sh, rf, err := s.resolve(ctx, from)
	if err != nil {
		return err
	}
	sh2, rt, err := s.resolve(ctx, to)
	if err != nil {
		return err
	}
	if sh == nil || sh2 == nil || rf == "" || rt == "" {
		return ErrNotSupported
	}
	if s.share == "" && strings.SplitN(strings.TrimPrefix(from, "/"), "/", 2)[0] != strings.SplitN(strings.TrimPrefix(to, "/"), "/", 2)[0] {
		return fmt.Errorf("cannot move between shares: %w", ErrNotSupported)
	}
	if err := sh.Rename(rf, rt); err != nil {
		// SMB rename does not replace: remove a file target and retry.
		if fi, serr := sh.Stat(rt); serr == nil && !fi.IsDir() {
			if rerr := sh.Remove(rt); rerr == nil {
				return s.op(sh.Rename(rf, rt), from)
			}
		}
		return s.op(err, from)
	}
	return nil
}

func (s *smbFS) Chtimes(ctx context.Context, p string, atime, mtime time.Time) error {
	sh, rp, err := s.resolve(ctx, p)
	if err != nil {
		return err
	}
	if sh == nil || rp == "" {
		return ErrNotSupported
	}
	return s.op(sh.Chtimes(rp, atime, mtime), p)
}

func (s *smbFS) Chmod(ctx context.Context, p string, perm uint32) error  { return ErrNotSupported }
func (s *smbFS) Chown(ctx context.Context, p string, uid, gid int) error { return ErrNotSupported }
func (s *smbFS) Symlink(ctx context.Context, target, link string) error  { return ErrNotSupported }
func (s *smbFS) Readlink(ctx context.Context, p string) (string, error)  { return "", ErrNotSupported }
func (s *smbFS) Home(ctx context.Context) (string, error)                { return s.home, nil }

func (s *smbFS) Realpath(ctx context.Context, p string) (string, error) {
	if _, err := s.Stat(ctx, p); err != nil {
		return "", err
	}
	return p, nil
}

func (s *smbFS) Space(ctx context.Context, p string) (Space, error) {
	sh, rp, err := s.resolve(ctx, p)
	if err != nil {
		return Space{}, err
	}
	if sh == nil {
		return Space{}, ErrNotSupported
	}
	st, err := sh.Statfs(rp)
	if err != nil {
		return Space{}, s.op(err, p)
	}
	bs := int64(st.BlockSize())
	return Space{Total: int64(st.TotalBlockCount()) * bs, Free: int64(st.FreeBlockCount()) * bs,
		Avail: int64(st.AvailableBlockCount()) * bs}, nil
}
