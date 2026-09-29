package monitor

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func inhibitMethod() string { return "caffeinate" }

func inhibitSupported() bool {
	_, err := exec.LookPath("caffeinate")
	return err == nil
}

// startInhibit runs `caffeinate -dimsu -w <nexterm pid>`: display, idle, disk and system sleep are prevented, and the
// helper exits by itself when NexTerm does.
func startInhibit() (inhibitor, error) {
	path, err := exec.LookPath("caffeinate")
	if err != nil {
		return nil, errors.New("caffeinate is not available")
	}
	cmd := exec.Command(path, "-dimsu", "-w", strconv.Itoa(os.Getpid()))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return startProc(cmd)
}

func helperError(cmd *exec.Cmd) error {
	if b, ok := cmd.Stderr.(*strings.Builder); ok && strings.TrimSpace(b.String()) != "" {
		return errors.New("caffeinate: " + clip(strings.TrimSpace(b.String()), 300))
	}
	return errors.New("caffeinate exited immediately")
}
