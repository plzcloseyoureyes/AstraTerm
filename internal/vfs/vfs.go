// Package vfs is AstraTerm's virtual file system layer (SPEC §6.0 "Files", RESEARCH FILE-*, PROTO-23..27, §3.3, §3.14,
// §3.15): one FS interface implemented by drivers — SFTP over the terminal's pooled SSH transport, an SCP / shell
// fallback, sudo-SFTP, the local host (jailed with os.Root in server mode), FTP/FTPS, S3, WebDAV and SMB — a per-user
// handle registry, and the /api/fs REST endpoints (sorted listings, streaming downloads with Range and zip, resumable
// uploads, editor read/write with conflict detection, file operations, checksums, search, archives, folder compare).
// It also installs the "follow terminal folder" shell integration (FILE-2) for SSH sessions.
package vfs

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Entry is one file of a listing (the JSON contract type of SPEC §6.0). Drivers fill Name, Path, Type, Size, Mode (POSIX
// st_mode), Mtime, UID/GID and LinkTarget; the handle layer completes Perm, Hidden, Owner/Group names and LinkType.
type Entry = model.FileEntry

// FS is a virtual file system. Every path is absolute, slash-separated and clean ("/", "/home/x"); drivers never see
// relative paths. Implementations must be safe for concurrent use and honor ctx for blocking calls where they can.
//
// Errors should wrap the io/fs sentinels (fs.ErrNotExist, fs.ErrPermission, fs.ErrExist) or ErrNotSupported so the
// HTTP layer can map them; anything else is reported as a remote failure with its message.
type FS interface {
	// List returns the entries of dir (without "." and ".."), unsorted, with Lstat semantics.
	List(ctx context.Context, dir string) ([]*Entry, error)
	// Stat follows symlinks; Lstat does not.
	Stat(ctx context.Context, p string) (*Entry, error)
	Lstat(ctx context.Context, p string) (*Entry, error)
	// Open reads p starting at offset. The reader may implement io.WriterTo / io.ReaderAt for faster copies.
	Open(ctx context.Context, p string, offset int64) (io.ReadCloser, error)
	// Create opens p for writing at offset: the file is created when missing and truncated to offset (0 = empty).
	// Close must be called and reports the final error. The writer may implement io.ReaderFrom.
	Create(ctx context.Context, p string, offset int64) (io.WriteCloser, error)
	Mkdir(ctx context.Context, p string) error
	MkdirAll(ctx context.Context, p string) error
	// Remove deletes a file, a symlink (never its target) or an empty directory.
	Remove(ctx context.Context, p string) error
	// RemoveAll deletes p recursively without following symlinks (Lstat based). Missing p is not an error.
	RemoveAll(ctx context.Context, p string) error
	// Rename moves from onto to, replacing an existing file at to where the protocol allows it.
	Rename(ctx context.Context, from, to string) error
	// Chmod sets the POSIX permission bits (0o7777) of p.
	Chmod(ctx context.Context, p string, perm uint32) error
	// Chown sets numeric owner and group; -1 keeps the current value.
	Chown(ctx context.Context, p string, uid, gid int) error
	Chtimes(ctx context.Context, p string, atime, mtime time.Time) error
	// Symlink creates link pointing to target (target is stored verbatim).
	Symlink(ctx context.Context, target, link string) error
	Readlink(ctx context.Context, p string) (string, error)
	// Realpath resolves p to an absolute canonical path (symlinks resolved where the protocol can).
	Realpath(ctx context.Context, p string) (string, error)
	// Home is the login / start folder.
	Home(ctx context.Context) (string, error)
	Close() error
}

// ---- optional capabilities ----------------------------------------------------------------------------------------

// Execer runs shell commands on the machine that serves the file system (SSH drivers). stdout receives the command's
// standard output (nil discards it); stderr is captured (bounded).
type Execer interface {
	Exec(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) (stderr []byte, code int, err error)
}

// Checksummer computes a checksum natively (hex). algo is md5, sha1, sha256 or sha512.
type Checksummer interface {
	Checksum(ctx context.Context, p, algo string) (string, error)
}

// Searcher finds entries under dir whose name matches pattern (case-insensitive glob) and, when content is not empty,
// whose content contains it. It returns at most max entries and whether more existed.
type Searcher interface {
	Search(ctx context.Context, dir, pattern, content string, max int) ([]*Entry, bool, error)
}

// Archiver creates an archive (format zip or tar.gz) at dest from paths natively (e.g. tar over exec).
type Archiver interface {
	Archive(ctx context.Context, paths []string, dest, format string) error
}

// Extractor unpacks an archive into destDir natively.
type Extractor interface {
	Extract(ctx context.Context, archive, destDir string) error
}

// ServerSideCopier copies src (file or tree, attributes preserved) to dst on the server itself.
type ServerSideCopier interface {
	CopyServerSide(ctx context.Context, src, dst string) error
}

// Presigner returns a time-limited URL that downloads p without AstraTerm (S3).
type Presigner interface {
	Presign(ctx context.Context, p string, expires time.Duration) (string, error)
}

// Linker creates hard links.
type Linker interface {
	Link(ctx context.Context, oldname, newname string) error
}

// SpaceInfo reports the capacity of the file system holding p (bytes).
type SpaceInfo interface {
	Space(ctx context.Context, p string) (Space, error)
}

// Space is the reply of GET /api/fs/{id}/space.
type Space struct {
	Total int64 `json:"total"`
	Free  int64 `json:"free"`
	Avail int64 `json:"avail"`
}

// NativeUploader is implemented by file systems with their own resumable upload mechanism (S3 multipart) instead of
// the generic "<target>.astraterm-part + rename" scheme.
type NativeUploader interface {
	// UploadChunk appends body (length n, -1 = unknown) at offset of the pending upload for target; final completes
	// it atomically. It returns the number of bytes stored so far.
	UploadChunk(ctx context.Context, target string, offset int64, body io.Reader, n int64, final bool) (int64, error)
	// UploadOffset reports the bytes stored for a pending upload of target (0, false when none).
	UploadOffset(ctx context.Context, target string) (int64, bool)
}

// SudoWriter writes a file with elevated rights (sudo tee over exec).
type SudoWriter interface {
	SudoWrite(ctx context.Context, p string, data []byte) error
}

// ownerNamer resolves numeric owner / group ids of entries to names (cached).
type ownerNamer interface {
	nameOwners(ctx context.Context, entries []*Entry)
}

// Capabilities advertises what a handle supports (POST /api/fs reply).
type Capabilities struct {
	Chmod    bool `json:"chmod"`
	Chown    bool `json:"chown"`
	Symlink  bool `json:"symlink"`
	Exec     bool `json:"exec"`
	Checksum bool `json:"checksum"`
	Search   bool `json:"search"`
	Archive  bool `json:"archive"`
	Extract  bool `json:"extract"`
	Copy     bool `json:"copy"`
	Presign  bool `json:"presign"`
	Sudo     bool `json:"sudo"`
	Resume   bool `json:"resume"`
	Mtime    bool `json:"mtime"`
	Hardlink bool `json:"hardlink"`
	Space    bool `json:"space"`
}

// ---- errors -------------------------------------------------------------------------------------------------------

// ErrNotSupported is returned by drivers for operations their protocol cannot do.
var ErrNotSupported = errors.New("operation not supported by this file system")

// errNotEmpty marks "directory not empty" failures.
var errNotEmpty = errors.New("directory not empty")

// errDisconnected marks transport failures (the SSH / FTP connection went away).
var errDisconnected = errors.New("connection lost")

// POSIX file type bits of st_mode.
const (
	sIFMT   = 0o170000
	sIFSOCK = 0o140000
	sIFLNK  = 0o120000
	sIFREG  = 0o100000
	sIFBLK  = 0o060000
	sIFDIR  = 0o040000
	sIFCHR  = 0o020000
	sIFIFO  = 0o010000
)

// typeFromMode maps st_mode type bits to the FileEntry type.
func typeFromMode(mode uint32) string {
	switch mode & sIFMT {
	case sIFDIR:
		return model.FileTypeDir
	case sIFREG:
		return model.FileTypeFile
	case sIFLNK:
		return model.FileTypeSymlink
	case 0:
		return model.FileTypeFile
	}
	return model.FileTypeOther
}

// modeForType returns st_mode type bits for an entry type (used by drivers without POSIX modes).
func modeForType(t string) uint32 {
	switch t {
	case model.FileTypeDir:
		return sIFDIR
	case model.FileTypeSymlink:
		return sIFLNK
	case model.FileTypeOther:
		return 0
	}
	return sIFREG
}

// permString renders st_mode like ls -l ("drwxr-xr-x", "-rwsr-x--T").
func permString(mode uint32) string {
	b := []byte("----------")
	switch mode & sIFMT {
	case sIFDIR:
		b[0] = 'd'
	case sIFLNK:
		b[0] = 'l'
	case sIFSOCK:
		b[0] = 's'
	case sIFBLK:
		b[0] = 'b'
	case sIFCHR:
		b[0] = 'c'
	case sIFIFO:
		b[0] = 'p'
	}
	const rwx = "rwxrwxrwx"
	for i := range 9 {
		if mode&(1<<uint(8-i)) != 0 {
			b[i+1] = rwx[i]
		}
	}
	special := func(pos int, bit uint32, set, unset byte) {
		if mode&bit == 0 {
			return
		}
		if b[pos] == '-' {
			b[pos] = unset
		} else {
			b[pos] = set
		}
	}
	special(3, 0o4000, 's', 'S')
	special(6, 0o2000, 's', 'S')
	special(9, 0o1000, 't', 'T')
	return string(b)
}
