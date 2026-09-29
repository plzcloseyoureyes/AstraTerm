//go:build darwin || linux

package monitor

import (
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// procInhibitor is a helper process holding the inhibition (caffeinate, systemd-inhibit); it runs in its own process
// group so releasing it also ends its children.
type procInhibitor struct {
	cmd  *exec.Cmd
	done chan struct{}
	once sync.Once
}

func startProc(cmd *exec.Cmd) (*procInhibitor, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &procInhibitor{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	// A helper that cannot take the lock (no logind, no D-Bus...) exits right away.
	select {
	case <-p.done:
		return nil, helperError(cmd)
	case <-time.After(300 * time.Millisecond):
	}
	return p, nil
}

func (p *procInhibitor) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *procInhibitor) release() {
	p.once.Do(func() {
		if p.cmd.Process == nil {
			return
		}
		pgid := p.cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			<-p.done
		}
	})
}
