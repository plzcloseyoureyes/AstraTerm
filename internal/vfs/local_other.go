//go:build !unix

package vfs

import "io/fs"

func sysStat(fi fs.FileInfo) (mode uint32, uid, gid int, ok bool) { return 0, 0, 0, false }

func lookupOwnerName(isUser bool, id int) string { return "" }

func localLookupName(isUser bool, name string) int { return -1 }

func sysNlink(fi fs.FileInfo) uint64 { return 0 }

func hasPosixACL(p string) bool { return false }

// localSymlinks: creating symlinks on Windows needs privileges / developer mode.
const localSymlinks = false
