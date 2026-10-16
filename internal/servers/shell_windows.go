//go:build windows

package servers

import (
	"os"
	"os/exec"
)

func signalStatus(*os.ProcessState) int { return -1 }

func setProcessGroup(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
