//go:build windows

package servers

import (
	"os"
	"os/exec"

	"github.com/charmbracelet/x/xpty"
)

// startOnPTY starts cmd on the ConPTY.
func startOnPTY(p xpty.Pty, cmd *exec.Cmd) error { return p.Start(cmd) }

// hangup terminates the shell (Windows has no SIGHUP).
func hangup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func signalStatus(*os.ProcessState) int { return -1 }

func setProcessGroup(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
