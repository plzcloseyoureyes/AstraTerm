//go:build windows

package kube

import "os/exec"

// configureCmd is a no-op on Windows (ConPTY handles the child process group).
func configureCmd(*exec.Cmd) {}

// killCmd terminates kubectl (Windows has no process groups via signals).
func killCmd(cmd *exec.Cmd) {
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
