//go:build windows

package sshx

import (
	"context"
	"io"

	"github.com/Microsoft/go-winio"
	"github.com/davidmz/go-pageant"
	"golang.org/x/crypto/ssh/agent"
)

// openSSHAgentPipe is the named pipe of the Windows OpenSSH agent service (also served by 1Password).
const openSSHAgentPipe = `\\.\pipe\openssh-ssh-agent`

// dialHostAgent connects to the Windows OpenSSH agent pipe, falling back to Pageant.
func dialHostAgent(ctx context.Context) (agent.Agent, io.Closer, error) {
	if c, err := winio.DialPipeContext(ctx, openSSHAgentPipe); err == nil {
		return agent.NewClient(c), c, nil
	}
	if pageant.Available() {
		return pageant.New(), nopCloser{}, nil
	}
	return nil, nil, errNoAgent
}
