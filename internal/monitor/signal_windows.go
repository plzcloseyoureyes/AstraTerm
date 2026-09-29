//go:build windows

package monitor

import (
	"os"
	"strings"
)

// signalLocal ends a process of the Termstead host; Windows has no signals, every one terminates the process.
func signalLocal(pid int, sig string) error {
	if sig == "STOP" || sig == "CONT" || sig == "USR1" || sig == "USR2" {
		return errBadSignal
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return errNoProcess
	}
	defer p.Release()
	if err := p.Kill(); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "access is denied") {
			return errPermission("Access is denied: the process belongs to another user or is protected")
		}
		return err
	}
	return nil
}
