//go:build !windows

package kube

import (
	"os/exec"
	"syscall"
)

// configureCmd puts kubectl in its own session so the whole process group can be signalled/killed together.
func configureCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// killCmd sends SIGTERM to kubectl's process group.
func killCmd(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

// exitCode reports the process exit status (128+signal when signalled).
func exitCode(cmd *exec.Cmd, _ error) int {
	ps := cmd.ProcessState
	if ps == nil {
		return -1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}
