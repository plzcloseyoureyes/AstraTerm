//go:build windows

package local

import (
	"errors"
	"os/exec"

	"github.com/charmbracelet/x/xpty"
)

// startOnPty starts cmd on the ConPTY.
func startOnPty(p xpty.Pty, cmd *exec.Cmd) error {
	return p.Start(cmd)
}

// signalPty emulates signals on Windows: INT / QUIT become Ctrl+C / Ctrl+Break keystrokes (ConPTY turns them into
// console control events), everything else terminates the process.
func signalPty(p xpty.Pty, cmd *exec.Cmd, name string) error {
	switch name {
	case "INT":
		_, err := p.Write([]byte{0x03})
		return err
	case "QUIT":
		_, err := p.Write([]byte{0x1c})
		return err
	case "TERM", "KILL", "HUP":
		if cmd.Process == nil {
			return errors.New("process not started")
		}
		return cmd.Process.Kill()
	}
	return errors.New("signal " + name + " is not supported on Windows")
}

// hangup terminates the shell (Windows has no SIGHUP).
func hangup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func exitCode(cmd *exec.Cmd, _ error) int {
	if cmd.ProcessState == nil {
		return -1
	}
	return cmd.ProcessState.ExitCode()
}
