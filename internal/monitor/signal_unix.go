//go:build !windows

package monitor

import (
	"errors"
	"os"
	"syscall"
)

var unixSignals = map[string]syscall.Signal{
	"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT, "KILL": syscall.SIGKILL,
	"TERM": syscall.SIGTERM, "STOP": syscall.SIGSTOP, "CONT": syscall.SIGCONT, "USR1": syscall.SIGUSR1,
	"USR2": syscall.SIGUSR2,
}

// signalLocal delivers a signal to a process of the Termstead host (sig is a validated name such as "TERM").
func signalLocal(pid int, sig string) error {
	s, ok := unixSignals[sig]
	if !ok {
		return errBadSignal
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return errNoProcess
	}
	switch err := p.Signal(s); {
	case err == nil:
		return nil
	case errors.Is(err, syscall.ESRCH), errors.Is(err, os.ErrProcessDone):
		return errNoProcess
	case errors.Is(err, syscall.EPERM):
		return errPermission("Operation not permitted: the process belongs to another user (retry with sudo)")
	default:
		return err
	}
}
