package importer

import (
	"errors"
	"io"
	"os"
	"os/user"
	"strings"
)

// userHomeDir returns the current OS user's home directory ("" when unknown).
func userHomeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// localUserName returns the OS login name (without a Windows domain prefix), for ssh_config's %u token.
func localUserName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		name := u.Username
		if i := strings.LastIndexByte(name, '\\'); i >= 0 {
			name = name[i+1:]
		}
		return name
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return os.Getenv("USERNAME")
}

// isTruthy reports whether a query-string flag means "on".
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

var errNotRegular = errors.New("not a regular file")

// readFileLimited reads a regular file (symlinks are followed — dotfile managers symlink ~/.ssh/config) of at most
// limit bytes. Devices, FIFOs and directories are refused before opening (opening a FIFO would block), and the opened
// handle is checked to be the very file that was inspected.
func readFileLimited(path string, limit int64) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errNotRegular
	}
	if fi.Size() > limit {
		return nil, errors.New("file is too large")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ofi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !ofi.Mode().IsRegular() || !os.SameFile(fi, ofi) {
		return nil, errNotRegular
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file is too large")
	}
	return data, nil
}
