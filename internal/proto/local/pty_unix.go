//go:build !windows

package local

import (
	"errors"
	"os/exec"
	"syscall"

	"github.com/charmbracelet/x/xpty"
	"golang.org/x/sys/unix"
)

// startOnPty starts cmd as a session leader with the PTY as its controlling terminal (job control works), then
// closes the parent's copy of the slave so reads return EOF once the shell exits.
func startOnPty(p xpty.Pty, cmd *exec.Cmd) error {
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

// hangup sends SIGHUP to the shell's process group.
func hangup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
	}
}

// exitCode maps the wait result to an exit status (128+n for a signal, like shells report it).
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
