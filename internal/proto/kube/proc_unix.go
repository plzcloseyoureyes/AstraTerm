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
