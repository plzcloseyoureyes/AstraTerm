package vfs

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/termstead/termstead/internal/model"
)

// Generic implementations of higher-level operations on top of the FS primitives. Drivers with native (exec, server
// side) variants are preferred by the callers; these are the portable fallbacks.

// ---- listings -----------------------------------------------------------------------------------------------------

// entryNameOK reports whether a name returned by a server is usable as ONE path element. Listings come from remote
// servers (SFTP, FTP, S3, WebDAV, SMB, parsed `ls` / `stat` output) that may be malicious or broken: an entry named
// "..", "a/../../x" (a valid S3 key segment) or containing NUL must never make Termstead address a path outside the
// folder that was listed — a recursive transfer would otherwise write anywhere on the destination (zip-slip).
// Invalid UTF-8 is allowed (POSIX names are bytes).
func entryNameOK(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

// ListDir lists dir and sanitizes the result (every caller must use it instead of FS.List): entries with unusable
// names are dropped, a name listed twice (e.g. once as a symlink and once as a folder) keeps its first occurrence,
// and every entry's path is exactly dir + "/" + name.
func ListDir(ctx context.Context, fsys FS, dir string) ([]*Entry, error) {
	entries, err := fsys.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	return sanitizeListing(dir, entries), nil
}

func sanitizeListing(dir string, entries []*Entry) []*Entry {
	seen := make(map[string]struct{}, len(entries))
	out := entries[:0]
	for _, e := range entries {
		if e == nil || !entryNameOK(e.Name) {
			continue
		}
		if _, dup := seen[e.Name]; dup {
			continue
		}
		seen[e.Name] = struct{}{}
		e.Path = joinPath(dir, e.Name)
		out = append(out, e)
	}
	clear(entries[len(out):]) // drop references to filtered entries
	return out
}

// withinResults keeps the entries whose path lies strictly below dir (search results parsed from command output or
// built from object keys must not point elsewhere).
func withinResults(dir string, entries []*Entry) []*Entry {
	out := entries[:0]
	for _, e := range entries {
		if e != nil && e.Path != dir && isWithin(e.Path, dir) && e.Path == path.Clean(e.Path) {
			out = append(out, e)
		}
	}
	return out
}

// ---- entries ------------------------------------------------------------------------------------------------------

// finishEntries completes driver entries: permission string, hidden flag, owner / group names (numeric fallback) and
// symlink target types.
func finishEntries(ctx context.Context, fsys FS, entries []*Entry) {
	if on, ok := fsys.(ownerNamer); ok {
		on.nameOwners(ctx, entries)
	}
	var links []*Entry
	for _, e := range entries {
		if e.Mode&sIFMT == 0 {
			e.Mode |= modeForType(e.Type)
		}
		if e.Type == "" {
			e.Type = typeFromMode(e.Mode)
		}
		e.Perm = permString(e.Mode)
		e.Hidden = strings.HasPrefix(e.Name, ".") && e.Name != "." && e.Name != ".."
		if e.Owner == "" && e.UID != nil {
			e.Owner = strconv.Itoa(*e.UID)
		}
		if e.Group == "" && e.GID != nil {
			e.Group = strconv.Itoa(*e.GID)
		}
		if e.Type == model.FileTypeSymlink && e.LinkType == "" {
			links = append(links, e)
		}
	}
	resolveLinkTypes(ctx, fsys, links)
}

// resolveLinkTypes stats symlink targets (bounded concurrency) to fill LinkType (file | dir | broken).
func resolveLinkTypes(ctx context.Context, fsys FS, links []*Entry) {
	if len(links) == 0 {
		return
	}
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, e := range links {
		wg.Add(1)
		sem <- struct{}{}
		go func(e *Entry) {
			defer wg.Done()
			defer func() { <-sem }()
			t, err := fsys.Stat(ctx, e.Path)
			switch {
			case err != nil:
				e.LinkType = "broken"
			case t.Type == model.FileTypeDir:
				e.LinkType = "dir"
			default:
				e.LinkType = "file"
			}
			if e.LinkTarget == "" {
				if lt, err := fsys.Readlink(ctx, e.Path); err == nil {
					e.LinkTarget = lt
				}
			}
		}(e)
	}
	wg.Wait()
}

// sortEntries orders a listing: directories (and symlinks to directories) first, then by name (case-insensitive,
// natural ties broken by the exact name).
func sortEntries(entries []*Entry) {
	isDir := func(e *Entry) bool { return e.Type == "dir" || (e.Type == "symlink" && e.LinkType == "dir") }
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		da, db := isDir(a), isDir(b)
		if da != db {
			return da
		}
		la, lb := strings.ToLower(a.Name), strings.ToLower(b.Name)
		if la != lb {
			return la < lb
		}
		return a.Name < b.Name
	})
}

// ---- recursive operations -----------------------------------------------------------------------------------------

// walkFunc is called for every entry below (and including) root in pre-order; returning errSkipDir skips a directory.
type walkFunc func(e *Entry) error

var errSkipDir = errors.New("skip this directory")

// walk visits root and everything below it with Lstat semantics (symlinks are reported, never followed).
func walk(ctx context.Context, fsys FS, root string, fn walkFunc) error {
	e, err := fsys.Lstat(ctx, root)
	if err != nil {
		return err
	}
	return walkEntry(ctx, fsys, e, fn)
}

func walkEntry(ctx context.Context, fsys FS, e *Entry, fn walkFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fn(e); err != nil {
		if errors.Is(err, errSkipDir) {
			return nil
		}
		return err
	}
	if e.Type != model.FileTypeDir {
		return nil
	}
	children, err := ListDir(ctx, fsys, e.Path)
	if err != nil {
		return err
	}
	sortEntries(children)
	for _, c := range children {
		if err := walkEntry(ctx, fsys, c, fn); err != nil {
			return err
		}
	}
	return nil
}

// removeAllGeneric deletes p recursively in post-order using Lstat, so symlinks (including a symlink passed as p)
// are removed themselves and never followed. A missing p is not an error.
func removeAllGeneric(ctx context.Context, fsys FS, p string) error {
	if p == "/" {
		return fmt.Errorf("refusing to delete the root directory")
	}
	e, err := fsys.Lstat(ctx, p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return removeEntry(ctx, fsys, e)
}

func removeEntry(ctx context.Context, fsys FS, e *Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.Type == model.FileTypeDir {
		children, err := ListDir(ctx, fsys, e.Path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		// Bounded parallelism: deletes over high-latency links are dominated by round trips.
		sem := make(chan struct{}, 8)
		var wg sync.WaitGroup
		var mu sync.Mutex
		var first error
		fail := func(err error) {
			mu.Lock()
			if first == nil {
				first = err
			}
			mu.Unlock()
		}
		failed := func() bool {
			mu.Lock()
			defer mu.Unlock()
			return first != nil
		}
		for _, c := range children {
			if failed() || ctx.Err() != nil {
				break
			}
			if c.Type == model.FileTypeDir {
				if err := removeEntry(ctx, fsys, c); err != nil {
					fail(err)
				}
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(c *Entry) {
				defer wg.Done()
				defer func() { <-sem }()
				if err := fsys.Remove(ctx, c.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
					fail(err)
				}
			}(c)
		}
		// Never return while deletes of this folder are still running.
		wg.Wait()
		if first != nil {
			return first
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if err := fsys.Remove(ctx, e.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// chmodTree applies spec to p (and, when recursive, everything below it; symlinks are skipped). Recursive changes
// run as one `chmod -R` when the file system can execute commands.
func chmodTree(ctx context.Context, fsys FS, p string, spec modeSpec, recursive bool) error {
	if x, ok := fsys.(Execer); ok && recursive {
		if arg := spec.chmodArg(); arg != "" {
			_, err := runShell(ctx, x, "chmod -R "+shq(arg)+" -- "+shq(p), nil)
			if err == nil || !errors.Is(err, ErrNotSupported) {
				return err
			}
		}
	}
	apply := func(e *Entry) error {
		if e.Type == model.FileTypeSymlink {
			return nil
		}
		perm := spec.apply(e.Mode, e.Type == model.FileTypeDir)
		if perm == e.Mode&0o7777 && !spec.abs {
			return nil
		}
		return fsys.Chmod(ctx, e.Path, perm)
	}
	if !recursive {
		e, err := fsys.Stat(ctx, p)
		if err != nil {
			return err
		}
		e.Path = p
		return apply(e)
	}
	return walk(ctx, fsys, p, apply)
}

// chownTree changes owner / group of p (recursively without following symlinks when asked).
func chownTree(ctx context.Context, fsys FS, p string, uid, gid int, recursive bool) error {
	if !recursive {
		return fsys.Chown(ctx, p, uid, gid)
	}
	if x, ok := fsys.(Execer); ok {
		spec := ""
		switch {
		case uid >= 0 && gid >= 0:
			spec = fmt.Sprintf("%d:%d", uid, gid)
		case uid >= 0:
			spec = fmt.Sprint(uid)
		case gid >= 0:
			spec = fmt.Sprintf(":%d", gid)
		}
		if spec != "" {
			_, err := runShell(ctx, x, "chown -R -h "+spec+" -- "+shq(p), nil)
			if err == nil || !errors.Is(err, ErrNotSupported) {
				return err
			}
		}
	}
	return walk(ctx, fsys, p, func(e *Entry) error { return fsys.Chown(ctx, e.Path, uid, gid) })
}

// ---- hashing ------------------------------------------------------------------------------------------------------

func newHash(algo string) (hash.Hash, error) {
	switch algo {
	case "md5":
		return md5.New(), nil
	case "sha1":
		return sha1.New(), nil
	case "sha256", "":
		return sha256.New(), nil
	case "sha512":
		return sha512.New(), nil
	}
	return nil, fmt.Errorf("unsupported algorithm %q (md5, sha1, sha256, sha512)", algo)
}

// checksum hashes p: natively when the driver can (remote sha256sum...), else by streaming the content.
func checksum(ctx context.Context, fsys FS, p, algo string) (string, error) {
	if algo == "" {
		algo = "sha256"
	}
	if _, err := newHash(algo); err != nil {
		return "", err
	}
	if c, ok := fsys.(Checksummer); ok {
		if h, err := c.Checksum(ctx, p, algo); err == nil {
			return h, nil
		} else if !errors.Is(err, ErrNotSupported) {
			return "", err
		}
	}
	return streamChecksum(ctx, fsys, p, algo)
}

func streamChecksum(ctx context.Context, fsys FS, p, algo string) (string, error) {
	h, err := newHash(algo)
	if err != nil {
		return "", err
	}
	r, err := fsys.Open(ctx, p, 0)
	if err != nil {
		return "", err
	}
	defer r.Close()
	if _, err := copyBuffer(ctx, h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyBuffer copies with a large buffer (so remote readers issue big, pipelined reads) and honors ctx.
func copyBuffer(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 1<<20)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		m, rerr := src.Read(buf)
		if m > 0 {
			w, werr := dst.Write(buf[:m])
			n += int64(w)
			if werr != nil {
				return n, werr
			}
			if w < m {
				return n, io.ErrShortWrite
			}
		}
		if rerr == io.EOF {
			return n, nil
		}
		if rerr != nil {
			return n, rerr
		}
	}
}

// ---- search -------------------------------------------------------------------------------------------------------

// matchName reports whether name matches the case-insensitive glob (see globPattern).
func matchName(glob, name string) bool {
	ok, err := path.Match(strings.ToLower(glob), strings.ToLower(name))
	return err == nil && ok
}

// search finds entries under dir whose name matches pattern (and content when given). Native searchers (find over
// exec) are preferred; the fallback walks the tree.
func search(ctx context.Context, fsys FS, dir, pattern, content string, max int) ([]*Entry, bool, error) {
	if max <= 0 || max > 10000 {
		max = 1000
	}
	if s, ok := fsys.(Searcher); ok {
		res, more, err := s.Search(ctx, dir, pattern, content, max)
		if err == nil || !errors.Is(err, ErrNotSupported) {
			return withinResults(dir, res), more, err
		}
	}
	glob := globPattern(pattern)
	var out []*Entry
	more := false
	errStop := errors.New("stop")
	err := walk(ctx, fsys, dir, func(e *Entry) error {
		if e.Path == dir {
			return nil
		}
		if !matchName(glob, e.Name) {
			return nil
		}
		if content != "" {
			if e.Type != model.FileTypeFile || e.Size > 64<<20 {
				return nil
			}
			found, err := fileContains(ctx, fsys, e.Path, content)
			if err != nil || !found {
				return nil
			}
		}
		if len(out) >= max {
			more = true
			return errStop
		}
		out = append(out, e)
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		if len(out) == 0 {
			return nil, false, err
		}
	}
	return out, more, nil
}

// fileContains reports whether the file contains needle (case-insensitive, ASCII folding).
func fileContains(ctx context.Context, fsys FS, p, needle string) (bool, error) {
	r, err := fsys.Open(ctx, p, 0)
	if err != nil {
		return false, err
	}
	defer r.Close()
	n := []byte(strings.ToLower(needle))
	buf := make([]byte, 256<<10)
	var carry []byte
	for {
		m, rerr := r.Read(buf)
		if m > 0 {
			chunk := append(carry, buf[:m]...)
			if containsFold(chunk, n) {
				return true, nil
			}
			if len(chunk) > len(n) {
				carry = append([]byte(nil), chunk[len(chunk)-len(n)+1:]...)
			} else {
				carry = append([]byte(nil), chunk...)
			}
		}
		if rerr != nil {
			return false, nil
		}
	}
}

func containsFold(hay, lowerNeedle []byte) bool {
	if len(lowerNeedle) == 0 {
		return true
	}
	for i := 0; i+len(lowerNeedle) <= len(hay); i++ {
		ok := true
		for j, c := range lowerNeedle {
			h := hay[i+j]
			if h >= 'A' && h <= 'Z' {
				h += 'a' - 'A'
			}
			if h != c {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// ---- copies -------------------------------------------------------------------------------------------------------

// uniqueName returns a free name in dir derived from name ("a.txt" → "a (copy).txt", "a (copy 2).txt"...).
func uniqueName(ctx context.Context, fsys FS, dir, name, tag string) (string, error) {
	stem, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		stem, ext = name[:i], name[i:]
	}
	for n := 1; n < 1000; n++ {
		var cand string
		if n == 1 {
			cand = fmt.Sprintf("%s (%s)%s", stem, tag, ext)
		} else {
			cand = fmt.Sprintf("%s (%s %d)%s", stem, tag, n, ext)
		}
		if _, err := fsys.Lstat(ctx, joinPath(dir, cand)); errors.Is(err, fs.ErrNotExist) {
			return cand, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("no free name for %q", name)
}

// copyTree copies src (file, symlink or directory tree) to dst on the same file system by streaming (fallback of
// ServerSideCopier). Attributes (mode, mtime) are preserved where the driver allows.
func copyTree(ctx context.Context, fsys FS, src, dst string) error {
	if isWithin(dst, src) {
		return fmt.Errorf("cannot copy a folder into itself")
	}
	e, err := fsys.Lstat(ctx, src)
	if err != nil {
		return err
	}
	return copyEntry(ctx, fsys, e, dst)
}

func copyEntry(ctx context.Context, fsys FS, e *Entry, dst string) error {
	switch e.Type {
	case model.FileTypeSymlink:
		t, err := fsys.Readlink(ctx, e.Path)
		if err != nil {
			return err
		}
		return fsys.Symlink(ctx, t, dst)
	case model.FileTypeDir:
		if err := fsys.Mkdir(ctx, dst); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		children, err := ListDir(ctx, fsys, e.Path)
		if err != nil {
			return err
		}
		for _, c := range children {
			if err := copyEntry(ctx, fsys, c, joinPath(dst, c.Name)); err != nil {
				return err
			}
		}
		_ = fsys.Chmod(ctx, dst, e.Mode&0o7777)
		_ = fsys.Chtimes(ctx, dst, e.Mtime, e.Mtime)
		return nil
	case model.FileTypeFile:
		if err := copyFileData(ctx, fsys, e.Path, fsys, dst); err != nil {
			return err
		}
		_ = fsys.Chmod(ctx, dst, e.Mode&0o7777)
		_ = fsys.Chtimes(ctx, dst, e.Mtime, e.Mtime)
		return nil
	}
	return nil // sockets, devices: skipped
}

// copyFileData streams the content of src (on sfs) into dst (on dfs).
func copyFileData(ctx context.Context, sfs FS, src string, dfs FS, dst string) error {
	r, err := sfs.Open(ctx, src, 0)
	if err != nil {
		return err
	}
	defer r.Close()
	w, err := dfs.Create(ctx, dst, 0)
	if err != nil {
		return err
	}
	if _, err := CopyStream(ctx, w, r, -1); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

// CopyStream copies r into w efficiently: a writer's ReaderFrom (SFTP concurrent writes) gets a size-bounded reader so
// it can pipeline, a reader's WriterTo (SFTP concurrent reads) is used otherwise, else a 1 MiB buffer. size is the
// expected length (-1 unknown). ctx cancellation aborts between buffers.
func CopyStream(ctx context.Context, w io.Writer, r io.Reader, size int64) (int64, error) {
	cr := &ctxReader{ctx: ctx, r: r}
	if rf, ok := w.(io.ReaderFrom); ok {
		// A big read buffer makes remote sources (SFTP) issue large, pipelined reads while the writer pipelines too.
		var src io.Reader = bufio.NewReaderSize(cr, 1<<20)
		if size >= 0 {
			src = &io.LimitedReader{R: src, N: size}
		} else {
			src = struct{ io.Reader }{src}
		}
		return rf.ReadFrom(src)
	}
	if wt, ok := r.(io.WriterTo); ok {
		if size < 0 {
			return wt.WriteTo(&ctxWriter{ctx: ctx, w: w})
		}
		// WriterTo copies to EOF: bound it so a Range / known-size copy never writes past size.
		lw := &limitWriter{w: &ctxWriter{ctx: ctx, w: w}, left: size}
		n, err := wt.WriteTo(lw)
		if errors.Is(err, errLimitReached) {
			err = nil
		}
		return n - lw.dropped, err
	}
	if size >= 0 {
		r = io.LimitReader(r, size)
	}
	return copyBuffer(ctx, w, r)
}

var errLimitReached = errors.New("copy limit reached")

// limitWriter passes at most left bytes and then fails with errLimitReached.
type limitWriter struct {
	w       io.Writer
	left    int64
	dropped int64 // bytes accepted from the caller but not written (reported as written to satisfy WriteTo)
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.left <= 0 {
		return 0, errLimitReached
	}
	if int64(len(p)) > l.left {
		n, err := l.w.Write(p[:l.left])
		l.left -= int64(n)
		if err != nil {
			return n, err
		}
		return n, errLimitReached
	}
	n, err := l.w.Write(p)
	l.left -= int64(n)
	return n, err
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

type ctxWriter struct {
	ctx context.Context
	w   io.Writer
}

func (c *ctxWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.w.Write(p)
}
