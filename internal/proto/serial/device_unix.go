//go:build !windows

package serial

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// stopBits15Supported: POSIX termios has no 1.5 stop bits (go.bug.st/serial rejects it on unix).
const stopBits15Supported = false

// validateDevice accepts only character devices under /dev (symlinks such as /dev/serial/by-id/... are resolved).
// A device that does not exist (yet) is accepted: opening it reports "not found", and a replug is awaited on
// reconnect. This keeps the serial endpoints from opening arbitrary files of the AstraTerm host.
func validateDevice(dev string) error {
	if dev == "" || !filepath.IsAbs(dev) || strings.ContainsRune(dev, 0) {
		return fmt.Errorf("serial: %q is not a device path (e.g. /dev/ttyUSB0)", dev)
	}
	real, err := filepath.EvalSymlinks(dev)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if !strings.HasPrefix(filepath.Clean(dev), "/dev/") {
				return fmt.Errorf("serial: %q is not under /dev", dev)
			}
			return nil
		}
		return fmt.Errorf("serial: %s: %w", dev, err)
	}
	if !strings.HasPrefix(real, "/dev/") {
		return fmt.Errorf("serial: %q is not a device under /dev", dev)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return fmt.Errorf("serial: %s: %w", dev, err)
	}
	if fi.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("serial: %q is not a character device", dev)
	}
	return nil
}
