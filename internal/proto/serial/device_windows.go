//go:build windows

package serial

import (
	"fmt"
	"regexp"
)

// Serial devices are named like COM3, or \\.\COM12 / \\.\CNCA0 (virtual port drivers); paths are refused so the
// endpoints cannot open arbitrary files of the AstraTerm host.
var windowsPortRe = regexp.MustCompile(`^(?:\\\\\.\\)?[A-Za-z][A-Za-z0-9_.-]{0,63}$`)

// stopBits15Supported: the Windows DCB supports ONE5STOPBITS.
const stopBits15Supported = true

func validateDevice(dev string) error {
	if !windowsPortRe.MatchString(dev) {
		return fmt.Errorf("serial: %q is not a serial port name (e.g. COM3)", dev)
	}
	return nil
}
