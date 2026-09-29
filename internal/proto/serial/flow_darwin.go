//go:build darwin

package serial

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
	tcCRTSCTS       = unix.CRTSCTS // CCTS_OFLOW | CRTS_IFLOW
	// CDTR_IFLOW | CDSR_OFLOW from <sys/termios.h> (not exported by x/sys/unix).
	tcDSRDTR = 0x00040000 | 0x00080000
)
