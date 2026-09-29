//go:build !linux && !darwin && !freebsd

package vfs

func diskSpace(p string) (Space, error) { return Space{}, ErrNotSupported }
