//go:build !linux && !darwin && !windows

package serial

import (
	"fmt"

	goserial "go.bug.st/serial"
)

// applyFlowControl: flow control is only implemented for Linux, macOS and Windows hosts.
func applyFlowControl(_ goserial.Port, mode string) error {
	if mode == flowNone {
		return nil
	}
	return fmt.Errorf("serial: %s flow control is not supported on this platform", flowLabel(mode))
}

// releaseFlow: nothing to release where flow control is not supported.
func releaseFlow(goserial.Port) {}
