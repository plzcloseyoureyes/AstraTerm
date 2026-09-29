//go:build windows

package mosh

import "os/exec"

// hangup terminates mosh-client (Windows has no SIGHUP).
func hangup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
