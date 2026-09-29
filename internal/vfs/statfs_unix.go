//go:build linux || darwin || freebsd

package vfs

import "golang.org/x/sys/unix"

// diskSpace reports the capacity of the file system holding p.
func diskSpace(p string) (Space, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(p, &st); err != nil {
		return Space{}, err
	}
	bs := int64(st.Bsize)
	return Space{
		Total: int64(st.Blocks) * bs,
		Free:  int64(st.Bfree) * bs,
		Avail: int64(st.Bavail) * bs,
	}, nil
}
