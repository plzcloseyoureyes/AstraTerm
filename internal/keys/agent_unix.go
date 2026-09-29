//go:build !windows

package keys

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// agentEndpoint returns the agent's Unix socket path: <data dir>/agent.sock when the data directory is private to
// this user (like ssh-agent, the socket must live in a directory other users cannot enter: its own 0600 mode is not
// enough on every platform, and some do not report peer credentials), else — or when that path does not fit sun_path
// (104 bytes on macOS/BSD, 108 on Linux) — a private 0700 directory in the temp directory.
func agentEndpoint(dataDir string) (string, error) {
	p := filepath.Join(dataDir, "agent.sock")
	if len(p) < 100 && checkPrivateDir(dataDir) == nil {
		return p, nil
	}
	sum := sha256.Sum256([]byte(dataDir))
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("termstead-agent-%d-%s", os.Getuid(), hex.EncodeToString(sum[:5])))
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	if err := checkPrivateDir(dir); err != nil {
		return "", fmt.Errorf("refusing to use the agent directory %s: %w", dir, err)
	}
	p = filepath.Join(dir, "agent.sock")
	if len(p) >= 104 {
		return "", fmt.Errorf("the agent socket path %s is too long", p)
	}
	return p, nil
}

// checkPrivateDir verifies that dir is a directory (not a symlink) owned by this user that group and others cannot
// access.
func checkPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir():
		return errors.New("not a directory")
	case !ok || int(st.Uid) != os.Getuid():
		return errors.New("owned by another user")
	case fi.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("accessible to other users (mode %04o)", fi.Mode().Perm())
	}
	return nil
}

// listenAgent listens on the socket path (replacing a stale socket file) with mode 0600.
func listenAgent(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, 500*time.Millisecond); err == nil {
			c.Close()
			return nil, fmt.Errorf("another agent is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("cannot remove the stale agent socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(true)
	}
	return ln, nil
}

// cleanupEndpoint removes the private temp directory of a fallback socket once it is empty (the listener unlinks the
// socket itself when it closes).
func cleanupEndpoint(path string) {
	dir := filepath.Dir(path)
	if strings.HasPrefix(filepath.Base(dir), "termstead-agent-") && filepath.Dir(dir) == filepath.Clean(os.TempDir()) {
		_ = os.Remove(dir) // fails (harmlessly) while the directory is not empty
	}
}
