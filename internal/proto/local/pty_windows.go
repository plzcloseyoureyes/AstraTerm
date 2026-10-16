//go:build windows

package local

import (
	"errors"
	"os/exec"

	"github.com/charmbracelet/x/xpty"
)

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
