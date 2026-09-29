package monitor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func inhibitMethod() string { return "systemd-inhibit" }

func inhibitSupported() bool {
	_, err := exec.LookPath("systemd-inhibit")
	return err == nil
}

// startInhibit takes a logind idle:sleep block lock with systemd-inhibit. The inhibited command polls Termstead's PID,
// so the lock is released even if Termstead is killed.
func startInhibit() (inhibitor, error) {
	path, err := exec.LookPath("systemd-inhibit")
	if err != nil {
		return nil, errors.New("systemd-inhibit is not available")
	}
	watch := fmt.Sprintf("while kill -0 %d 2>/dev/null; do sleep 5; done", os.Getpid())
	cmd := exec.Command(path, "--what=idle:sleep", "--who=Termstead", "--why=Caffeine mode is on", "--mode=block",
		"/bin/sh", "-c", watch)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	// No Pdeathsig: Linux delivers it when the OS *thread* that forked the helper exits, which the Go runtime may do
	// long before Termstead does — Caffeine would switch itself off. The PID watcher above covers crashes (≤ 5 s).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return startProc(cmd)
}

func helperError(cmd *exec.Cmd) error {
	if b, ok := cmd.Stderr.(*strings.Builder); ok && strings.TrimSpace(b.String()) != "" {
		return errors.New("systemd-inhibit: " + clip(strings.TrimSpace(b.String()), 300))
	}
	return errors.New("systemd-inhibit exited immediately (is systemd-logind running?)")
}
