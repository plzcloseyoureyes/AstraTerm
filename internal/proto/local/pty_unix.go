//go:build !windows

package local

import (
	"errors"
	"os/exec"
	"syscall"

	"github.com/charmbracelet/x/xpty"
	"golang.org/x/sys/unix"
)

var signals = map[string]syscall.Signal{
	"INT": syscall.SIGINT, "TERM": syscall.SIGTERM, "KILL": syscall.SIGKILL, "HUP": syscall.SIGHUP,
	"QUIT": syscall.SIGQUIT, "USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2, "STOP": syscall.SIGSTOP,
	"CONT": syscall.SIGCONT, "WINCH": syscall.SIGWINCH,
}

// signalPty signals the terminal's foreground process group (what a keyboard signal would reach), falling back to
// the shell's process group.
func signalPty(p xpty.Pty, cmd *exec.Cmd, name string) error {
	sig, ok := signals[name]
	if !ok {
		return errors.New("unsupported signal " + name)
	}
	if cmd.Process == nil {
		return errors.New("process not started")
	}
	pgrp := 0
	if up, ok := p.(*xpty.UnixPty); ok {
		_ = up.Control(func(fd uintptr) {
			if g, err := unix.IoctlGetInt(int(fd), unix.TIOCGPGRP); err == nil && g > 0 {
				pgrp = g
			}
		})
	}
	if pgrp == 0 {
		pgrp = cmd.Process.Pid // the shell is a session (and group) leader
	}
	return syscall.Kill(-pgrp, sig)
}
