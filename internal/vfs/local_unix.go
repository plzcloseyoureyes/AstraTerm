//go:build unix

package vfs

import (
	"io/fs"
	"os/user"
	"strconv"
	"syscall"
)

// sysStat extracts the raw POSIX mode and owner ids of a host file.
func sysStat(fi fs.FileInfo) (mode uint32, uid, gid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, 0, 0, false
	}
	return uint32(st.Mode), int(st.Uid), int(st.Gid), true
}

// lookupOwnerName resolves a host uid / gid to a name ("" → the numeric id is shown).
func lookupOwnerName(isUser bool, id int) string {
	if isUser {
		if u, err := user.LookupId(strconv.Itoa(id)); err == nil {
			return u.Username
		}
		return ""
	}
	if g, err := user.LookupGroupId(strconv.Itoa(id)); err == nil {
		return g.Name
	}
	return ""
}

// localLookupName maps a host user / group name to its id (-1 when unknown).
func localLookupName(isUser bool, name string) int {
	if isUser {
		if u, err := user.Lookup(name); err == nil {
			if n, err := strconv.Atoi(u.Uid); err == nil {
				return n
			}
		}
		return -1
	}
	if g, err := user.LookupGroup(name); err == nil {
		if n, err := strconv.Atoi(g.Gid); err == nil {
			return n
		}
	}
	return -1
}

// sysNlink returns the hard link count of a host file (0 when unknown).
func sysNlink(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st != nil {
		return uint64(st.Nlink)
	}
	return 0
}

// localSymlinks reports whether the host supports creating symlinks without privileges.
const localSymlinks = true
