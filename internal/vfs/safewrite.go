package vfs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/termstead/termstead/internal/model"
)

// Safe editor saves (PUT /api/fs/{id}/write). Rewriting a file in place (truncate, then write) leaves a truncated
// file behind when the connection drops mid-write. Where the protocol allows it, the new content is written to a
// hidden temporary file in the same folder (".<name>.termstead-tmp-<random>"), flushed (fsync when the server
// offers it), its size verified, the original's permission bits and owner / group applied, and it then atomically
// replaces the file with a rename — a drop at any point leaves the original untouched (plus, at worst, the
// temporary file). A symlink is followed: the file it points to is replaced, the link stays.
//
// The file is rewritten in place (the previous behaviour, keeping inode, owner, ACLs and hard links) when the rename
// would change what the file is:
//   - it has several hard links, or POSIX ACLs (checked where the driver can: local host, SSH with shell access);
//   - the owner, group or permission bits cannot be given to the temporary file (e.g. another user's file in a
//     writable folder), or the folder is not writable (no temporary file possible);
//   - the symlink cannot be resolved, or the target is not a regular file;
//   - the protocol cannot replace a file atomically or keep its permissions: FTP (no chmod, rename semantics vary),
//     SMB (rename cannot replace; the replacement would get the folder's ACL), SFTP servers without
//     posix-rename@openssh.com, the local host on Windows (ACLs), and root browsing (sudo-sftp: SELinux labels,
//     ACLs and xattrs of system files must survive). "Save with sudo" (sudo tee) always writes in place.
// S3 and WebDAV replace objects atomically with one PUT, so their direct write is already safe. Every strategy
// verifies the final size.

// Write strategies (audit detail "strategy").
const (
	writeAtomic  = "atomic"   // temporary file + atomic rename
	writeInPlace = "in-place" // truncate and rewrite
	writePut     = "put"      // the protocol replaces the object atomically itself
)

// safeReplacer is implemented by drivers that can replace a file atomically with a rename.
type safeReplacer interface {
	// atomicReplace reports whether Rename(tmp, target) atomically replaces an existing target (and the driver
	// keeps permissions / owners meaningful).
	atomicReplace(ctx context.Context) bool
	// keepInPlace returns why the existing regular file p must be rewritten in place (hard links, ACLs), or ""
	// (also when the driver cannot tell).
	keepInPlace(ctx context.Context, p string) string
}

// syncer is a writer that can flush the data to stable storage before Close.
type syncer interface{ Sync() error }

// writeFileSafe replaces the content of p with data (see above) and reports the strategy used.
func writeFileSafe(ctx context.Context, fsys FS, p string, data []byte) (string, error) {
	switch fsys.(type) {
	case *s3FS, *webdavFS:
		return writePut, writeDirect(ctx, fsys, p, data)
	}
	sr, ok := fsys.(safeReplacer)
	if !ok {
		return writeInPlace, writeDirect(ctx, fsys, p, data)
	}
	target, cur, err := resolveWriteTarget(ctx, fsys, p)
	if err != nil {
		return "", err
	}
	if target == "" {
		return writeInPlace, writeDirect(ctx, fsys, p, data) // unresolvable symlink, not a regular file
	}
	if cur != nil {
		if !sr.atomicReplace(ctx) || sr.keepInPlace(ctx, target) != "" {
			return writeInPlace, writeDirect(ctx, fsys, p, data)
		}
	}
	done, err := replaceAtomically(ctx, fsys, target, cur, data)
	if !done && err == nil {
		return writeInPlace, writeDirect(ctx, fsys, p, data)
	}
	return writeAtomic, err
}

// resolveWriteTarget returns the file a write of p changes (a symlink resolved) and its entry (nil for a new
// file). target "" means "write p directly" (broken / unresolvable link, a device...).
func resolveWriteTarget(ctx context.Context, fsys FS, p string) (string, *Entry, error) {
	e, err := fsys.Lstat(ctx, p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return p, nil, nil
	case err != nil:
		return "", nil, err
	}
	switch e.Type {
	case model.FileTypeFile:
		return p, e, nil
	case model.FileTypeDir:
		return "", nil, &pathErr{p, fmt.Errorf("is a directory: %w", fs.ErrExist)}
	case model.FileTypeSymlink:
		rp, err := fsys.Realpath(ctx, p)
		if err != nil || rp == p || rp == "" {
			return "", nil, nil
		}
		if rp, err = cleanPath(rp, "/"); err != nil {
			return "", nil, nil
		}
		te, err := fsys.Lstat(ctx, rp)
		if err != nil || te.Type != model.FileTypeFile {
			return "", nil, nil
		}
		return rp, te, nil
	}
	return "", nil, nil
}

type pathErr struct {
	p   string
	err error
}

func (e *pathErr) Error() string { return e.err.Error() + ": " + e.p }
func (e *pathErr) Unwrap() error { return e.err }

// writeDirect rewrites p in place (or creates it) and verifies the size.
func writeDirect(ctx context.Context, fsys FS, p string, data []byte) error {
	w, err := fsys.Create(ctx, p, 0)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return checkSize(ctx, fsys, p, int64(len(data)))
}

func checkSize(ctx context.Context, fsys FS, p string, want int64) error {
	st, err := fsys.Stat(ctx, p)
	if err != nil {
		return err
	}
	if st.Size != want {
		return fmt.Errorf("the saved file has %d bytes instead of %d", st.Size, want)
	}
	return nil
}

// tmpSuffix marks the temporary files of safe writes.
const tmpSuffix = ".termstead-tmp-"

// tempName returns a hidden temporary name next to target (the name is shortened to stay below NAME_MAX).
func tempName(target string) string {
	name := baseName(target)
	if len(name) > 200 {
		name = strings.ToValidUTF8(name[:200], "")
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return joinPath(parentDir(target), "."+name+tmpSuffix+hex.EncodeToString(b))
}

// replaceAtomically writes data to a temporary file next to target, gives it cur's permissions and owner and
// renames it over target. done=false with a nil error means "not possible here, write in place instead" (nothing
// was changed); any other failure leaves target untouched and removes the temporary file.
func replaceAtomically(ctx context.Context, fsys FS, target string, cur *Entry, data []byte) (done bool, err error) {
	tmp := tempName(target)
	w, err := fsys.Create(ctx, tmp, 0)
	if err != nil {
		if isDisconnect(err) || ctx.Err() != nil {
			return false, err
		}
		return false, nil // e.g. the folder is not writable: in place
	}
	cleanup := func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_ = fsys.Remove(cctx, tmp)
	}
	fail := func(err error) (bool, error) {
		cleanup()
		return true, err
	}
	if _, err := w.Write(data); err != nil {
		w.Close()
		return fail(err)
	}
	if s, ok := w.(syncer); ok {
		_ = s.Sync() // best effort (fsync@openssh.com is optional)
	}
	if err := w.Close(); err != nil {
		return fail(err)
	}
	st, err := fsys.Stat(ctx, tmp)
	if err != nil {
		return fail(err)
	}
	if st.Size != int64(len(data)) {
		return fail(fmt.Errorf("the temporary file has %d bytes instead of %d", st.Size, len(data)))
	}
	if cur != nil {
		ok, err := adoptAttributes(ctx, fsys, tmp, st, cur)
		if err != nil {
			return fail(err)
		}
		if !ok {
			cleanup()
			return false, nil // owner / group / mode cannot be kept: in place
		}
	}
	if err := fsys.Rename(ctx, tmp, target); err != nil {
		if isDisconnect(err) || ctx.Err() != nil {
			return fail(err)
		}
		cleanup()
		return false, nil
	}
	if err := checkSize(ctx, fsys, target, int64(len(data))); err != nil {
		return true, err
	}
	return true, nil
}

// adoptAttributes gives tmp (entry st) the permission bits, owner and group of cur. ok=false: they cannot be kept.
func adoptAttributes(ctx context.Context, fsys FS, tmp string, st, cur *Entry) (bool, error) {
	wantMode := cur.Mode & 0o7777
	if st.Mode&0o7777 != wantMode {
		if err := fsys.Chmod(ctx, tmp, wantMode); err != nil {
			if isDisconnect(err) || ctx.Err() != nil {
				return false, err
			}
			return false, nil
		}
	}
	if cur.UID != nil && cur.GID != nil && st.UID != nil && st.GID != nil && (*cur.UID != *st.UID || *cur.GID != *st.GID) {
		uid, gid := -1, -1
		if *cur.UID != *st.UID {
			uid = *cur.UID
		}
		if *cur.GID != *st.GID {
			gid = *cur.GID
		}
		if err := fsys.Chown(ctx, tmp, uid, gid); err != nil {
			if isDisconnect(err) || ctx.Err() != nil {
				return false, err
			}
			return false, nil
		}
	}
	// Verify: chmod may silently drop set-id bits, chown may be ignored.
	got, err := fsys.Stat(ctx, tmp)
	if err != nil {
		return false, err
	}
	if got.Mode&0o7777 != wantMode {
		return false, nil
	}
	if cur.UID != nil && got.UID != nil && *cur.UID != *got.UID || cur.GID != nil && got.GID != nil && *cur.GID != *got.GID {
		return false, nil
	}
	return true, nil
}

// execKeepInPlace asks an exec-capable SSH server whether p has several hard links or an ACL ("+" after the
// permission string of ls -l). Unknown answers are "".
func execKeepInPlace(ctx context.Context, x Execer, p string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	q := shq(p)
	out, err := runShell(ctx, x, "{ stat -c %h -- "+q+" 2>/dev/null || stat -f %l -- "+q+" 2>/dev/null; }; LC_ALL=C ls -ld -- "+q, nil)
	if err != nil {
		return ""
	}
	return keepInPlaceReason(string(out))
}

// keepInPlaceReason parses the output of execKeepInPlace: the link count, then the `ls -ld` line.
func keepInPlaceReason(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return ""
	}
	if n, err := strconv.Atoi(strings.TrimSpace(lines[0])); err == nil && n > 1 {
		return "hard links"
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) > 0 && len(f[0]) == 11 && f[0][10] == '+' {
		return "ACL"
	}
	return ""
}
