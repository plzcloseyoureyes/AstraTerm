//go:build !windows

package servers

import (
	"os"
	"os/exec"
	"syscall"
)

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
