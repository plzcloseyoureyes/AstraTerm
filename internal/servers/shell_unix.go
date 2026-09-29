//go:build !windows

package servers

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"github.com/charmbracelet/x/xpty"
)

// startOnPTY starts cmd as a session leader with the PTY as its controlling terminal (job control works) and closes
// the parent's copy of the slave, so reads end once the shell and its children are gone.
func startOnPTY(p xpty.Pty, cmd *exec.Cmd) error {
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

// hangup sends SIGHUP to the shell's process group.
func hangup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
	}
}

// signalStatus returns 128+signal for a process killed by a signal, else -1.
func signalStatus(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return -1
}

// setProcessGroup puts a pipe-mode command in its own process group so stopping the server can end it and its
// children.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills a pipe-mode command and its process group.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
