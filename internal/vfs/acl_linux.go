package vfs

import (
	"bytes"

	"golang.org/x/sys/unix"
)

// hasPosixACL reports whether a host file carries a POSIX access ACL (lost by a rename-based save).
func hasPosixACL(p string) bool {
	buf := make([]byte, 4096)
	n, err := unix.Llistxattr(p, buf)
	if err != nil || n <= 0 {
		return false
	}
	for _, name := range bytes.Split(buf[:n], []byte{0}) {
		if string(name) == "system.posix_acl_access" {
			return true
		}
	}
	return false
}
