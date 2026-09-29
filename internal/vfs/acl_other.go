//go:build unix && !linux

package vfs

// hasPosixACL: POSIX ACLs are only checked on Linux (macOS / BSD ACLs are not exposed as xattrs).
func hasPosixACL(p string) bool { return false }
