//go:build !windows

package childproc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"github.com/charmbracelet/x/xpty"
)

// StartOnPTY starts cmd as a session leader with the PTY as its controlling terminal (job control works) and closes
// the parent's copy of the slave, so reads end once the shell and its children are gone.
func StartOnPTY(p xpty.Pty, cmd *exec.Cmd) error {
	up, ok := p.(*xpty.UnixPty)
	if !ok {
		return errors.New("unexpected pty type")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := up.Start(cmd); err != nil {
		return err
	}
	_ = up.Slave().Close()
	return nil
}

// Hangup sends SIGHUP to cmd's process group, as a closed terminal does.
func Hangup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
	}
}

func signalNumber(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return int(ws.Signal())
	}
	return 0
}
