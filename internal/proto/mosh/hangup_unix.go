//go:build !windows

package mosh

import (
	"os/exec"
	"syscall"
)

// hangup asks mosh-client to exit (on SIGHUP it tells the server to shut down first).
func hangup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGHUP)
	}
}
