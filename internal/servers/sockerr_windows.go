//go:build windows

package servers

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

func isAddrInUse(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, syscall.EADDRINUSE)
}

func isAddrNotAvail(err error) bool {
	return errors.Is(err, windows.WSAEADDRNOTAVAIL) || errors.Is(err, syscall.EADDRNOTAVAIL)
}

func isAccessDenied(err error) bool {
	return errors.Is(err, windows.WSAEACCES) || errors.Is(err, syscall.EACCES) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
