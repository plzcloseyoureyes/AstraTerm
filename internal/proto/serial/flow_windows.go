//go:build windows

package serial

import (
	"fmt"

	goserial "go.bug.st/serial"
	"golang.org/x/sys/windows"
)

// DCB flag bits (winbase.h).
const (
	dcbOutxCtsFlow     = 0x00000004
	dcbOutxDsrFlow     = 0x00000008
	dcbDtrControlMask  = 0x00000030
	dcbDsrSensitivity  = 0x00000040
	dcbTXContinueOnXof = 0x00000080
	dcbOutX            = 0x00000100
	dcbInX             = 0x00000200
	dcbRtsControlMask  = 0x00003000
)

// applyFlowControl configures the port's DCB for hardware (RTS/CTS, DSR/DTR) or software (XON/XOFF) flow control.
func applyFlowControl(p goserial.Port, mode string) error {
	h, err := portHandle(p)
	if err != nil {
		if mode == flowNone {
			return nil
		}
		return err
	}
	handle := windows.Handle(h)
	var dcb windows.DCB
	if err := windows.GetCommState(handle, &dcb); err != nil {
		return fmt.Errorf("serial: read line settings: %w", err)
	}
	dcb.Flags &^= dcbOutxCtsFlow | dcbOutxDsrFlow | dcbDsrSensitivity | dcbOutX | dcbInX
	dcb.Flags |= dcbTXContinueOnXof
	// Keep DTR/RTS asserted unless a handshake drives them.
	if dcb.Flags&dcbRtsControlMask == windows.RTS_CONTROL_HANDSHAKE {
		dcb.Flags = dcb.Flags&^dcbRtsControlMask | windows.RTS_CONTROL_ENABLE
	}
	if dcb.Flags&dcbDtrControlMask == windows.DTR_CONTROL_HANDSHAKE {
		dcb.Flags = dcb.Flags&^dcbDtrControlMask | windows.DTR_CONTROL_ENABLE
	}
	switch mode {
	case flowNone:
	case flowRTSCTS:
		dcb.Flags |= dcbOutxCtsFlow
		dcb.Flags = dcb.Flags&^dcbRtsControlMask | windows.RTS_CONTROL_HANDSHAKE
	case flowDSRDTR:
		dcb.Flags |= dcbOutxDsrFlow
		dcb.Flags = dcb.Flags&^dcbDtrControlMask | windows.DTR_CONTROL_HANDSHAKE
	case flowXONXOFF:
		dcb.Flags |= dcbOutX | dcbInX
		dcb.Flags &^= dcbTXContinueOnXof
		dcb.XonChar, dcb.XoffChar = 0x11, 0x13
		if dcb.XonLim == 0 || dcb.XoffLim == 0 {
			dcb.XonLim, dcb.XoffLim = 2048, 512
		}
	default:
		return fmt.Errorf("serial: unknown flow control %q", mode)
	}
	if err := windows.SetCommState(handle, &dcb); err != nil {
		return fmt.Errorf("serial: set %s flow control: %w", flowLabel(mode), err)
	}
	return nil
}

// releaseFlow switches flow control off so output held back by XOFF / CTS / DSR resumes before the port closes.
func releaseFlow(p goserial.Port) {
	_ = applyFlowControl(p, flowNone)
}
