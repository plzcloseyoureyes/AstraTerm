package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

// Helpers shared with the transfer manager (internal/transfer).

// PartSuffix is appended to files while they are being written (atomic rename on completion).
const PartSuffix = partSuffix

// CleanPath validates an API path and makes it absolute and clean (relative paths resolve against home).
func CleanPath(p, home string) (string, error) { return cleanPath(p, home) }

// HostToAPI converts an absolute path of the AstraTerm host to the files API form ("/home/x", "/C:/Users/x").
func HostToAPI(p string) string { return hostToAPI(p) }

// JoinPath joins a clean absolute directory and a name.
func JoinPath(dir, name string) string { return joinPath(dir, name) }

// BaseName returns the last element of a clean absolute path.
func BaseName(p string) string { return baseName(p) }

// ParentDir returns the parent of a clean absolute path.
func ParentDir(p string) string { return parentDir(p) }

// IsWithin reports whether p equals dir or lies below it.
func IsWithin(p, dir string) bool { return isWithin(p, dir) }

// FSError maps a driver error to the HTTP error the files API would return.
func FSError(err error, p string) error { return fsError(err, p) }

// Checksum hashes p (native tools first, streaming fallback). algo: md5, sha1, sha256, sha512.
func Checksum(ctx context.Context, fsys FS, p, algo string) (string, error) {
	return checksum(ctx, fsys, p, algo)
}

// NumberedName returns a free name in dir derived from name: "a (1).txt", "a (2).txt", ...
func NumberedName(ctx context.Context, fsys FS, dir, name string) (string, error) {
	stem, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		stem, ext = name[:i], name[i:]
	}
	for n := 1; n < 10000; n++ {
		cand := fmt.Sprintf("%s (%d)%s", stem, n, ext)
		if _, err := fsys.Lstat(ctx, joinPath(dir, cand)); errors.Is(err, fs.ErrNotExist) {
			return cand, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("no free name for %q", name)
}

// SupportsOffsetWrites reports whether Create(p, offset>0) works on the driver (resume in place).
func SupportsOffsetWrites(fsys FS) bool {
	switch fsys.(type) {
	case *s3FS, *webdavFS:
		return false
	}
	return true
}

// SupportsSymlinks reports whether the driver can create symlinks.
func SupportsSymlinks(fsys FS) bool {
	switch fsys.(type) {
	case *sftpFS, *shellFS:
		return true
	case *localFS:
		return localSymlinks
	}
	return false
}

// SizedCreate opens p for writing exactly size bytes when the driver benefits from knowing the size (SCP),
// else it behaves like Create(p, 0).
func SizedCreate(ctx context.Context, fsys FS, p string, size int64, perm uint32) (io.WriteCloser, error) {
	if sc, ok := fsys.(sizedCreator); ok && size >= 0 {
		return sc.CreateSized(ctx, p, size, perm)
	}
	return fsys.Create(ctx, p, 0)
}
