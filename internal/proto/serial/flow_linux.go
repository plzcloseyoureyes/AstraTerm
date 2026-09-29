//go:build linux

package serial

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
	tcCRTSCTS       = unix.CRTSCTS
	tcDSRDTR        = 0 // Linux serial drivers have no DSR/DTR flow control
)
