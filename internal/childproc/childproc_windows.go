//go:build windows

package childproc

import (
	"os"
	"os/exec"

	"github.com/charmbracelet/x/xpty"
)

// StartOnPTY starts cmd on the ConPTY.
func StartOnPTY(p xpty.Pty, cmd *exec.Cmd) error { return p.Start(cmd) }

// Hangup terminates cmd (Windows has no SIGHUP).
func Hangup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func signalNumber(*os.ProcessState) int { return 0 }
