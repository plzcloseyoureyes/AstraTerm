//go:build !darwin && !linux && !windows

package monitor

import "errors"

func inhibitMethod() string { return "" }

func inhibitSupported() bool { return false }

func startInhibit() (inhibitor, error) {
	return nil, errors.New("preventing sleep is not supported on this system")
}
