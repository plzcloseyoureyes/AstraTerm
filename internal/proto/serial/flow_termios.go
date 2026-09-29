//go:build linux || darwin

package serial

import (
	"fmt"

	goserial "go.bug.st/serial"
	"golang.org/x/sys/unix"
)

// applyFlowControl configures hardware (RTS/CTS, DSR/DTR where supported) or software (XON/XOFF) flow control on an
// open port through termios, then reads the settings back: a driver that silently ignores a mode is reported instead
// of pretending it is active.
func applyFlowControl(p goserial.Port, mode string) error {
	h, err := portHandle(p)
	if err != nil {
		if mode == flowNone {
			return nil // the library opened the port with flow control off
		}
		return err
	}
	fd := int(h)
	t, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return fmt.Errorf("serial: read line settings: %w", err)
	}
	t.Cflag &^= tcCRTSCTS | tcDSRDTR
	t.Iflag &^= unix.IXON | unix.IXOFF | unix.IXANY
	switch mode {
	case flowNone:
	case flowRTSCTS:
		t.Cflag |= tcCRTSCTS
	case flowXONXOFF:
		t.Iflag |= unix.IXON | unix.IXOFF
		t.Cc[unix.VSTART] = 0x11 // ^Q
		t.Cc[unix.VSTOP] = 0x13  // ^S
	case flowDSRDTR:
		if tcDSRDTR == 0 {
			return fmt.Errorf("serial: DSR/DTR flow control is not supported on this platform (use RTS/CTS)")
		}
		t.Cflag |= tcDSRDTR
	default:
		return fmt.Errorf("serial: unknown flow control %q", mode)
	}
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, t); err != nil {
		return fmt.Errorf("serial: set flow control %s: %w", mode, err)
	}
	got, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return fmt.Errorf("serial: read line settings: %w", err)
	}
	want := uint64(t.Cflag) & uint64(tcCRTSCTS|tcDSRDTR)
	if uint64(got.Cflag)&uint64(tcCRTSCTS|tcDSRDTR) != want || got.Iflag&(unix.IXON|unix.IXOFF) != t.Iflag&(unix.IXON|unix.IXOFF) {
		return fmt.Errorf("serial: the device driver does not support %s flow control", flowLabel(mode))
	}
	return nil
}

// releaseFlow switches flow control off so output held back by XOFF / CTS / DSR resumes (both BSD and Linux ttys
// restart a stopped output when IXON is cleared); used before closing so a pending write cannot block the close.
func releaseFlow(p goserial.Port) {
	h, err := portHandle(p)
	if err != nil {
		return
	}
	fd := int(h)
	t, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return
	}
	t.Cflag &^= tcCRTSCTS | tcDSRDTR
	t.Iflag &^= unix.IXON | unix.IXOFF | unix.IXANY
	_ = unix.IoctlSetTermios(fd, ioctlSetTermios, t)
}
