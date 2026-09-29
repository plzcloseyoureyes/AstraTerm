package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/sshx"
)

// ---- SSH transport source -----------------------------------------------------------------------------------------

// sshSource hands out the pooled SSH client a handle works on. Session handles re-acquire through Pool.ForSession
// after the terminal reconnected (a new transport), connection handles through Pool.Get / Acquire. The handle keeps
// one pool reference, released on Close.
type sshSource struct {
	mu      sync.Mutex
	cl      *sshx.Client
	rel     func()
	acquire func(ctx context.Context) (*sshx.Client, func(), error)
	closed  bool
	host    string
}

var errHandleClosed = errors.New("file system handle is closed")

func (s *sshSource) client(ctx context.Context) (*sshx.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errHandleClosed
	}
	if s.cl != nil && s.cl.Alive() {
		return s.cl, nil
	}
	if s.rel != nil {
		s.rel()
		s.cl, s.rel = nil, nil
	}
	cl, rel, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	s.cl, s.rel = cl, rel
	return cl, nil
}

func (s *sshSource) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.rel != nil {
		s.rel()
	}
	s.cl, s.rel = nil, nil
}

// Exec runs cmd in a new session channel of the transport (MaxSessions overflow handled by sshx). Cancelling ctx
// kills the command.
func (s *sshSource) Exec(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) ([]byte, int, error) {
	cl, err := s.client(ctx)
	if err != nil {
		return nil, -1, err
	}
	sess, _, release, err := cl.NewSessionContext(ctx)
	if err != nil {
		return nil, -1, err
	}
	defer release()
	defer sess.Close()
	if stdout == nil {
		stdout = io.Discard
	}
	se := &capWriter{max: 64 << 10}
	sess.Stdin, sess.Stdout, sess.Stderr = stdin, stdout, se
	if err := sess.Start(cmd); err != nil {
		return nil, -1, err
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		return se.Bytes(), -1, ctx.Err()
	}
	var ee *ssh.ExitError
	var em *ssh.ExitMissingError
	switch {
	case err == nil:
		return se.Bytes(), 0, nil
	case errors.As(err, &ee):
		return se.Bytes(), ee.ExitStatus(), nil
	case errors.As(err, &em):
		return se.Bytes(), -1, nil
	}
	return se.Bytes(), -1, err
}

// probeExec reports whether the account can run shell commands (false for sftp-only / chrooted accounts, where the
// exec request runs internal-sftp or is refused).
func (s *sshSource) probeExec(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out := &capWriter{max: 4096}
	_, code, err := s.Exec(ctx, "echo astraterm-exec-ok", nil, out)
	return err == nil && code == 0 && strings.Contains(out.String(), "astraterm-exec-ok")
}

// ---- owner names --------------------------------------------------------------------------------------------------

// ownerCache resolves uid / gid to names with remote getent (per handle, batched per listing).
type ownerCache struct {
	mu     sync.Mutex
	users  map[int]string // "" = unknown name (numeric shown)
	groups map[int]string
	x      Execer // nil when exec is not possible
	failed bool
}

func newOwnerCache(x Execer) *ownerCache {
	return &ownerCache{users: map[int]string{}, groups: map[int]string{}, x: x}
}

func (oc *ownerCache) nameOwners(ctx context.Context, entries []*Entry) {
	oc.mu.Lock()
	var uids, gids []int
	seenU, seenG := map[int]bool{}, map[int]bool{}
	for _, e := range entries {
		if e.UID != nil && e.Owner == "" {
			if _, ok := oc.users[*e.UID]; !ok && !seenU[*e.UID] {
				seenU[*e.UID] = true
				uids = append(uids, *e.UID)
			}
		}
		if e.GID != nil && e.Group == "" {
			if _, ok := oc.groups[*e.GID]; !ok && !seenG[*e.GID] {
				seenG[*e.GID] = true
				gids = append(gids, *e.GID)
			}
		}
	}
	x, failed := oc.x, oc.failed
	oc.mu.Unlock()

	if (len(uids) > 0 || len(gids) > 0) && x != nil && !failed {
		if len(uids) > 256 {
			uids = uids[:256]
		}
		if len(gids) > 256 {
			gids = gids[:256]
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		users, groups, err := execOwnerNames(cctx, x, uids, gids)
		cancel()
		oc.mu.Lock()
		if err != nil {
			oc.failed = errors.Is(err, ErrNotSupported)
		}
		for _, id := range uids {
			oc.users[id] = users[id]
		}
		for _, id := range gids {
			oc.groups[id] = groups[id]
		}
		oc.mu.Unlock()
	}
	oc.mu.Lock()
	defer oc.mu.Unlock()
	for _, e := range entries {
		if e.UID != nil && e.Owner == "" {
			e.Owner = oc.users[*e.UID]
		}
		if e.GID != nil && e.Group == "" {
			e.Group = oc.groups[*e.GID]
		}
	}
}

// lookupName maps a user / group name to its id using the cache (reverse lookup), or -1.
func (oc *ownerCache) lookupName(user bool, name string) int {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	m := oc.groups
	if user {
		m = oc.users
	}
	for id, n := range m {
		if n == name {
			return id
		}
	}
	return -1
}

// ---- SFTP driver --------------------------------------------------------------------------------------------------

// sftpFS is the SSH-browser driver (FILE-1..11): pkg/sftp over the pooled transport (shared client) or over a private
// "sudo sftp-server" channel (FILE-11), with exec-powered extras (checksums, search, archives, cp -a, sudo tee).
type sftpFS struct {
	src    *sshSource
	getSC  func(ctx context.Context) (*sftp.Client, error)
	closer func()
	execOK bool
	root   bool // browse as root (sudo-sftp): saves always rewrite in place
	owners *ownerCache
	sudo   *sudoHelper // nil when sudo writes are unavailable

	extOnce  sync.Once
	posixRen bool
	hardlink bool
	statvfs  bool
}

func newSFTPFS(src *sshSource, execOK bool, sudo *sudoHelper) *sftpFS {
	f := &sftpFS{src: src, execOK: execOK, sudo: sudo}
	f.getSC = func(ctx context.Context) (*sftp.Client, error) {
		cl, err := src.client(ctx)
		if err != nil {
			return nil, err
		}
		return cl.SFTP()
	}
	if execOK {
		f.owners = newOwnerCache(src)
	} else {
		f.owners = newOwnerCache(nil)
	}
	return f
}

func (f *sftpFS) sc(ctx context.Context) (*sftp.Client, error) {
	c, err := f.getSC(ctx)
	if err != nil {
		return nil, err
	}
	f.extOnce.Do(func() {
		_, f.posixRen = c.HasExtension("posix-rename@openssh.com")
		_, f.hardlink = c.HasExtension("hardlink@openssh.com")
		_, f.statvfs = c.HasExtension("statvfs@openssh.com")
	})
	return c, nil
}

func (f *sftpFS) Close() error {
	if f.closer != nil {
		f.closer()
	}
	f.src.close()
	return nil
}

func sftpEntry(p string, fi os.FileInfo) *Entry {
	e := &Entry{Name: fi.Name(), Path: p, Size: fi.Size(), Mtime: fi.ModTime().UTC()}
	if st, ok := fi.Sys().(*sftp.FileStat); ok && st != nil {
		e.Mode = st.Mode
		e.UID, e.GID = intPtr(int(st.UID)), intPtr(int(st.GID))
		e.Mtime = time.Unix(int64(st.Mtime), 0).UTC()
	} else {
		e.Mode = goModeToPOSIX(fi.Mode())
	}
	if e.Mode&sIFMT == 0 {
		e.Mode |= goModeToPOSIX(fi.Mode()) & sIFMT
	}
	e.Type = typeFromMode(e.Mode)
	if p == "/" {
		e.Name = "/"
	}
	return e
}

func (f *sftpFS) List(ctx context.Context, dir string) ([]*Entry, error) {
	c, err := f.sc(ctx)
	if err != nil {
		return nil, err
	}
	fis, err := c.ReadDirContext(ctx, dir)
	if err != nil {
		return nil, err
	}
	out := make([]*Entry, 0, len(fis))
	for _, fi := range fis {
		if n := fi.Name(); n == "." || n == ".." {
			continue
		}
		out = append(out, sftpEntry(joinPath(dir, fi.Name()), fi))
	}
	return out, nil
}

func (f *sftpFS) Stat(ctx context.Context, p string) (*Entry, error) {
	c, err := f.sc(ctx)
	if err != nil {
		return nil, err
	}
	fi, err := c.Stat(p)
	if err != nil {
		return nil, err
	}
	e := sftpEntry(p, fi)
	e.Name = baseName(p)
	return e, nil
}

func (f *sftpFS) Lstat(ctx context.Context, p string) (*Entry, error) {
	c, err := f.sc(ctx)
	if err != nil {
		return nil, err
	}
	fi, err := c.Lstat(p)
	if err != nil {
		return nil, err
	}
	e := sftpEntry(p, fi)
	e.Name = baseName(p)
	return e, nil
}

func (f *sftpFS) Open(ctx context.Context, p string, offset int64) (io.ReadCloser, error) {
	c, err := f.sc(ctx)
	if err != nil {
		return nil, err
	}
	file, err := c.Open(p)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			file.Close()
			return nil, err
		}
	}
	return file, nil
}

func (f *sftpFS) Create(ctx context.Context, p string, offset int64) (io.WriteCloser, error) {
	c, err := f.sc(ctx)
	if err != nil {
		return nil, err
	}
	flags := os.O_WRONLY | os.O_CREATE
	if offset <= 0 {
		flags |= os.O_TRUNC
	}
	file, err := c.OpenFile(p, flags)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if err := file.Truncate(offset); err != nil {
			file.Close()
			return nil, err
		}
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			file.Close()
			return nil, err
		}
	}
	return &sftpWriter{f: file}, nil
}

// sftpWriter pipelines writes (concurrent requests) and, after a failed ReadFrom, truncates the file to the last
// offset known to be written completely so the upload can resume from there.
type sftpWriter struct{ f *sftp.File }

func (w *sftpWriter) Write(p []byte) (int, error) { return w.f.Write(p) }

func (w *sftpWriter) ReadFrom(r io.Reader) (int64, error) {
	n, err := w.f.ReadFromWithConcurrency(r, 0)
	if err != nil {
		if off, serr := w.f.Seek(0, io.SeekCurrent); serr == nil {
			_ = w.f.Truncate(off)
		}
	}
	return n, err
}

func (w *sftpWriter) Close() error { return w.f.Close() }

// Sync flushes the file on the server (fsync@openssh.com; an error when the server lacks the extension).
func (w *sftpWriter) Sync() error { return w.f.Sync() }

// atomicReplace: posix-rename@openssh.com replaces atomically; plain SFTP v3 rename cannot replace. Root browsing
// keeps in-place saves (labels / ACLs / xattrs of system files).
func (f *sftpFS) atomicReplace(ctx context.Context) bool {
	if f.root {
		return false
	}
	if _, err := f.sc(ctx); err != nil {
		return false
	}
	return f.posixRen
}

func (f *sftpFS) keepInPlace(ctx context.Context, p string) string {
	if !f.execOK {
		return "" // cannot tell (SFTP has no link count)
	}
	return execKeepInPlace(ctx, f.src, p)
}

func (f *sftpFS) Mkdir(ctx context.Context, p string) error {
	c, err := f.sc(ctx)
	if err != nil {
		return err
	}
	if err := c.Mkdir(p); err != nil {
		// SFTP v3 reports "exists" as a generic failure.
		if _, serr := c.Lstat(p); serr == nil {
			return &os.PathError{Op: "mkdir", Path: p, Err: fs.ErrExist}
		}
		return err
	}
	return nil
}

func (f *sftpFS) MkdirAll(ctx context.Context, p string) error {
	c, err := f.sc(ctx)
	if err != nil {
		return err
	}
	return c.MkdirAll(p)
}

func (f *sftpFS) Remove(ctx context.Context, p string) error {
	c, err := f.sc(ctx)
	if err != nil {
		return err
	}
	e, err := f.Lstat(ctx, p)
	if err != nil {
		return err
	}
	if e.Type == "dir" {
		if err := c.RemoveDirectory(p); err != nil {
			if children, lerr := c.ReadDir(p); lerr == nil && len(children) > 0 {
				return errNotEmpty
			}
			return err
		}
		return nil
	}
	return c.Remove(p)
}

func (f *sftpFS) RemoveAll(ctx context.Context, p string) error {
	if p == "/" {
		return fmt.Errorf("refusing to delete the root directory")
	}
	if f.execOK {
		// rm -rf never follows symlinks and runs server side (orders of magnitude faster than SFTP round trips).
		if _, err := runShell(ctx, f.src, "rm -rf -- "+shq(p), nil); err == nil {
			return nil
		} else if errors.Is(err, fs.ErrPermission) {
			return err
		}
	}
	return removeAllGeneric(ctx, f, p)
}

func (f *sftpFS) Rename(ctx context.Context, from, to string) error {
	c, err := f.sc(ctx)
	if err != nil {
		return err
	}
	if f.posixRen {
		return c.PosixRename(from, to)
	}
	err = c.Rename(from, to)
	if err == nil {
		return nil
	}
	// SFTP v3 rename refuses to replace: remove a file target, then retry.
	if te, lerr := c.Lstat(to); lerr == nil && !te.IsDir() {
		if rerr := c.Remove(to); rerr == nil {
			return c.Rename(from, to)
		}
	}
	return err
}

func (f *sftpFS) Chmod(ctx context.Context, p string, perm uint32) error {
	c, err := f.sc(ctx)
	if err != nil {
		return err
	}
	return c.Chmod(p, posixPermToGo(perm))
}

func (f *sftpFS) Chown(ctx context.Context, p string, uid, gid int) error {
	c, err := f.sc(ctx)
	if err != nil {
		return err
	}
	if uid < 0 || gid < 0 {
		e, err := f.Lstat(ctx, p)
		if err != nil {
			return err
		}
		if uid < 0 && e.UID != nil {
			uid = *e.UID
		}
		if gid < 0 && e.GID != nil {
			gid = *e.GID
		}
	}
	return c.Chown(p, uid, gid)
}

func (f *sftpFS) Chtimes(ctx context.Context, p string, atime, mtime time.Time) error {
	c, err := f.sc(ctx)
	if err != nil {
		return err
	}
	return c.Chtimes(p, atime, mtime)
}

func (f *sftpFS) Symlink(ctx context.Context, target, link string) error {
	c, err := f.sc(ctx)
	if err != nil {
		return err
	}
	return c.Symlink(target, link)
}

func (f *sftpFS) Link(ctx context.Context, oldname, newname string) error {
	c, err := f.sc(ctx)
	if err != nil {
		return err
	}
	if !f.hardlink {
		return ErrNotSupported
	}
	return c.Link(oldname, newname)
}

func (f *sftpFS) Readlink(ctx context.Context, p string) (string, error) {
	c, err := f.sc(ctx)
	if err != nil {
		return "", err
	}
	return c.ReadLink(p)
}

func (f *sftpFS) Realpath(ctx context.Context, p string) (string, error) {
	c, err := f.sc(ctx)
	if err != nil {
		return "", err
	}
	return c.RealPath(p)
}

func (f *sftpFS) Home(ctx context.Context) (string, error) {
	c, err := f.sc(ctx)
	if err != nil {
		return "", err
	}
	return c.Getwd()
}

func (f *sftpFS) Space(ctx context.Context, p string) (Space, error) {
	c, err := f.sc(ctx)
	if err != nil {
		return Space{}, err
	}
	if f.statvfs {
		if st, err := c.StatVFS(p); err == nil {
			bs := int64(st.Frsize)
			if bs == 0 {
				bs = int64(st.Bsize)
			}
			return Space{Total: int64(st.Blocks) * bs, Free: int64(st.Bfree) * bs, Avail: int64(st.Bavail) * bs}, nil
		}
	}
	if f.execOK {
		return execSpace(ctx, f.src, p)
	}
	return Space{}, ErrNotSupported
}

func (f *sftpFS) nameOwners(ctx context.Context, entries []*Entry) { f.owners.nameOwners(ctx, entries) }

// ---- exec-powered capabilities ------------------------------------------------------------------------------------

func (f *sftpFS) Exec(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) ([]byte, int, error) {
	if !f.execOK {
		return nil, -1, ErrNotSupported
	}
	return f.src.Exec(ctx, cmd, stdin, stdout)
}

func (f *sftpFS) Checksum(ctx context.Context, p, algo string) (string, error) {
	if !f.execOK {
		return "", ErrNotSupported
	}
	return execChecksum(ctx, f.src, p, algo)
}

func (f *sftpFS) Search(ctx context.Context, dir, pattern, content string, max int) ([]*Entry, bool, error) {
	if !f.execOK {
		return nil, false, ErrNotSupported
	}
	paths, more, err := execSearch(ctx, f.src, dir, pattern, content, max)
	if err != nil {
		return nil, false, err
	}
	return lstatMany(ctx, f, paths), more, nil
}

func (f *sftpFS) Archive(ctx context.Context, paths []string, dest, format string) error {
	if !f.execOK {
		return ErrNotSupported
	}
	return execArchive(ctx, f.src, paths, dest, format)
}

func (f *sftpFS) Extract(ctx context.Context, archive, destDir string) error {
	if !f.execOK {
		return ErrNotSupported
	}
	return execExtract(ctx, f.src, archive, destDir)
}

func (f *sftpFS) CopyServerSide(ctx context.Context, src, dst string) error {
	if !f.execOK {
		return ErrNotSupported
	}
	_, err := runShell(ctx, f.src, "cp -a -- "+shq(src)+" "+shq(dst), nil)
	return err
}

func (f *sftpFS) SudoWrite(ctx context.Context, p string, data []byte) error {
	if !f.execOK || f.sudo == nil {
		return ErrNotSupported
	}
	return f.sudo.tee(ctx, p, data)
}

// lstatMany stats paths concurrently (SFTP pipelines requests) and drops the ones that vanished.
func lstatMany(ctx context.Context, fsys FS, paths []string) []*Entry {
	out := make([]*Entry, len(paths))
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, p string) {
			defer wg.Done()
			defer func() { <-sem }()
			if e, err := fsys.Lstat(ctx, p); err == nil {
				out[i] = e
			}
		}(i, p)
	}
	wg.Wait()
	res := out[:0]
	for _, e := range out {
		if e != nil {
			res = append(res, e)
		}
	}
	return res
}

// execSpace parses `df -Pk` (POSIX output, 1024-byte blocks).
func execSpace(ctx context.Context, x Execer, p string) (Space, error) {
	out, err := runShell(ctx, x, "df -Pk -- "+shq(p), nil)
	if err != nil {
		return Space{}, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return Space{}, fmt.Errorf("unexpected df output")
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return Space{}, fmt.Errorf("unexpected df output")
	}
	var total, used, avail int64
	fmt.Sscan(f[1], &total)
	fmt.Sscan(f[2], &used)
	fmt.Sscan(f[3], &avail)
	return Space{Total: total * 1024, Free: (total - used) * 1024, Avail: avail * 1024}, nil
}

// isSubsystemError reports whether the SFTP subsystem is unavailable on the server (SCP/shell fallback).
func isSubsystemError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "subsystem")
}
