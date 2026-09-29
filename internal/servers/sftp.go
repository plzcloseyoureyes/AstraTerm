package servers

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

// sftpHandler serves one SFTP session over the jailed root (pkg/sftp request server handlers).
type sftpHandler struct {
	fs   *rootFS
	s    *sshService
	cl   *client
	user string
	addr string
}

var (
	_ sftp.FileReader           = (*sftpHandler)(nil)
	_ sftp.OpenFileWriter       = (*sftpHandler)(nil)
	_ sftp.PosixRenameFileCmder = (*sftpHandler)(nil)
	_ sftp.LstatFileLister      = (*sftpHandler)(nil)
	_ sftp.RealPathFileLister   = (*sftpHandler)(nil)
	_ sftp.ReadlinkFileLister   = (*sftpHandler)(nil)
)

// sftpErr maps file system errors to SFTP status codes (escaping the root reads as "permission denied").
func sftpErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return os.ErrNotExist
	case errors.Is(err, fs.ErrPermission), strings.Contains(err.Error(), "escapes"):
		return sftp.ErrSSHFxPermissionDenied
	}
	return err
}

func (h *sftpHandler) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	f, err := h.fs.OpenRegular(r.Filepath)
	if err != nil {
		if errors.Is(err, errNotRegular) {
			return nil, sftp.ErrSSHFxFailure
		}
		return nil, sftpErr(err)
	}
	return h.track(f, r.Filepath, false, false)
}

func (h *sftpHandler) Filewrite(r *sftp.Request) (io.WriterAt, error) { return h.open(r, false) }

func (h *sftpHandler) OpenFile(r *sftp.Request) (sftp.WriterAtReaderAt, error) {
	return h.open(r, true)
}

func (h *sftpHandler) open(r *sftp.Request, read bool) (*transferFile, error) {
	pf := r.Pflags()
	flag := os.O_WRONLY
	if read || pf.Read {
		flag = os.O_RDWR
	}
	if pf.Creat {
		flag |= os.O_CREATE
	}
	if pf.Trunc {
		flag |= os.O_TRUNC
	}
	if pf.Excl {
		flag |= os.O_EXCL
	}
	perm := os.FileMode(0o644)
	if r.AttrFlags().Permissions {
		if p := r.Attributes().FileMode().Perm(); p != 0 {
			perm = p
		}
	}
	if err := h.fs.checkUploadTarget(r.Filepath); err != nil {
		return nil, sftp.ErrSSHFxFailure
	}
	f, err := h.fs.OpenFile(r.Filepath, flag, perm)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			h.s.in.logf(levelWarn, h.addr, h.user, "Upload of %s refused (read-only)", r.Filepath)
		}
		return nil, sftpErr(err)
	}
	return h.track(f, r.Filepath, true, pf.Append)
}

func (h *sftpHandler) track(f *os.File, name string, upload, appendMode bool) (*transferFile, error) {
	tf := &transferFile{File: f, name: name, upload: upload}
	if appendMode {
		tf.appendMu = &sync.Mutex{}
	}
	tf.done = func(t *transferFile) {
		h.s.files.remove(t)
		if h.cl != nil {
			h.cl.setActivity("SFTP")
		}
		n := t.bytes()
		switch {
		case t.failed.Load():
			h.s.in.logf(levelWarn, h.addr, h.user, "Transfer of %s interrupted (%s)", t.name, humanBytes(n))
		case t.upload:
			h.s.in.stats.transfers.Add(1)
			h.s.in.logf(levelInfo, h.addr, h.user, "Uploaded %s (%s)", t.name, humanBytes(n))
		case n > 0:
			h.s.in.stats.transfers.Add(1)
			h.s.in.logf(levelInfo, h.addr, h.user, "Downloaded %s (%s)", t.name, humanBytes(n))
		}
	}
	if !h.s.files.add(tf) {
		_ = f.Close()
		return nil, sftp.ErrSSHFxConnectionLost
	}
	if h.cl != nil {
		if upload {
			h.cl.setActivity("Uploading " + name)
		} else {
			h.cl.setActivity("Downloading " + name)
		}
	}
	return tf, nil
}

func (h *sftpHandler) Filecmd(r *sftp.Request) error {
	p := r.Filepath
	switch r.Method {
	case "Setstat":
		return sftpErr(h.setstat(r))
	case "Rename":
		// SFTP v3 rename never replaces an existing target (posix-rename@openssh.com does).
		if _, err := h.fs.Lstat(r.Target); err == nil {
			return sftp.ErrSSHFxFailure
		}
		return h.logged(h.fs.Rename(p, r.Target), "Renamed %s → %s", p, r.Target)
	case "Rmdir":
		st, err := h.fs.Lstat(p)
		if err != nil {
			return sftpErr(err)
		}
		if !st.IsDir() {
			return sftp.ErrSSHFxFailure
		}
		return h.logged(h.fs.Remove(p), "Removed folder %s", p)
	case "Remove":
		st, err := h.fs.Lstat(p)
		if err != nil {
			return sftpErr(err)
		}
		if st.IsDir() {
			return sftp.ErrSSHFxFailure
		}
		return h.logged(h.fs.Remove(p), "Deleted %s", p)
	case "Mkdir":
		return h.logged(h.fs.Mkdir(p, 0o755), "Created folder %s", p)
	case "Link":
		return h.logged(h.fs.Link(p, r.Target), "Linked %s → %s", r.Target, p)
	case "Symlink":
		// Filepath is the link target (not cleaned), Target the new link.
		return h.logged(h.fs.Symlink(p, r.Target), "Symlink %s → %s", r.Target, p)
	}
	return sftp.ErrSSHFxOpUnsupported
}

// PosixRename replaces the target (posix-rename@openssh.com).
func (h *sftpHandler) PosixRename(r *sftp.Request) error {
	return h.logged(h.fs.Rename(r.Filepath, r.Target), "Renamed %s → %s", r.Filepath, r.Target)
}

func (h *sftpHandler) logged(err error, format string, args ...any) error {
	if err != nil {
		return sftpErr(err)
	}
	h.s.in.logf(levelInfo, h.addr, h.user, format, args...)
	return nil
}

func (h *sftpHandler) setstat(r *sftp.Request) error {
	flags, a := r.AttrFlags(), r.Attributes()
	p := r.Filepath
	if flags.Size {
		if h.fs.readOnly {
			return errPerm("truncate", p)
		}
		f, err := h.fs.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		err = f.Truncate(int64(a.Size))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	if flags.Permissions {
		if err := h.fs.Chmod(p, a.FileMode()); err != nil {
			return err
		}
	}
	if flags.Acmodtime {
		if err := h.fs.Chtimes(p, time.Unix(int64(a.Atime), 0), time.Unix(int64(a.Mtime), 0)); err != nil {
			return err
		}
	}
	// Ownership changes (UidGid) are ignored: files belong to the AstraTerm OS user.
	return nil
}

func (h *sftpHandler) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	switch r.Method {
	case "List":
		list, err := h.fs.ReadDir(r.Filepath)
		if err != nil {
			return nil, sftpErr(err)
		}
		return listerAt(list), nil
	case "Stat":
		fi, err := h.fs.Stat(r.Filepath)
		if err != nil {
			return nil, sftpErr(err)
		}
		return listerAt{renamedInfo{fi, path.Base(r.Filepath)}}, nil
	case "Readlink":
		t, err := h.fs.Readlink(r.Filepath)
		if err != nil {
			return nil, sftpErr(err)
		}
		return listerAt{linkInfo(t)}, nil
	}
	return nil, sftp.ErrSSHFxOpUnsupported
}

func (h *sftpHandler) Lstat(r *sftp.Request) (sftp.ListerAt, error) {
	fi, err := h.fs.Lstat(r.Filepath)
	if err != nil {
		return nil, sftpErr(err)
	}
	return listerAt{renamedInfo{fi, path.Base(r.Filepath)}}, nil
}

// RealPath canonicalizes a client path inside the jail.
func (h *sftpHandler) RealPath(p string) (string, error) { return path.Clean("/" + p), nil }

func (h *sftpHandler) Readlink(p string) (string, error) {
	t, err := h.fs.Readlink(p)
	return t, sftpErr(err)
}

type listerAt []os.FileInfo

func (l listerAt) ListAt(ls []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(ls, l[offset:])
	if n < len(ls) {
		return n, io.EOF
	}
	return n, nil
}

// renamedInfo reports another base name (the root's own Stat name is ".").
type renamedInfo struct {
	os.FileInfo
	name string
}

func (r renamedInfo) Name() string { return r.name }

// linkInfo carries a Readlink result through the lister fallback.
type linkInfo string

func (l linkInfo) Name() string       { return string(l) }
func (l linkInfo) Size() int64        { return 0 }
func (l linkInfo) Mode() os.FileMode  { return os.ModeSymlink | 0o777 }
func (l linkInfo) ModTime() time.Time { return time.Time{} }
func (l linkInfo) IsDir() bool        { return false }
func (l linkInfo) Sys() any           { return nil }
