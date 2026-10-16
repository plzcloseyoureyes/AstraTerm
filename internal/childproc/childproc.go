// Package childproc starts and ends child processes the way a terminal does: on a PTY as a session leader, hung up as
// a process group, with shell-style exit statuses. Local shells, the embedded servers' shells and kubectl share it.
package childproc

import "os/exec"

// ExitCode returns cmd's exit status once it exited: 128+n when signal n ended it (as shells report it), -1 before.
func ExitCode(cmd *exec.Cmd) int {
	ps := cmd.ProcessState
	if ps == nil {
		return -1
	}
	if n := signalNumber(ps); n > 0 {
		return 128 + n
	}
	return ps.ExitCode()
}
