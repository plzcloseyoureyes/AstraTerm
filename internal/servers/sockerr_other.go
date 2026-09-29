//go:build !windows

package servers

import (
	"errors"
	"syscall"
)

func isAddrInUse(err error) bool    { return errors.Is(err, syscall.EADDRINUSE) }
func isAddrNotAvail(err error) bool { return errors.Is(err, syscall.EADDRNOTAVAIL) }
func isAccessDenied(err error) bool {
	return errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM)
}
