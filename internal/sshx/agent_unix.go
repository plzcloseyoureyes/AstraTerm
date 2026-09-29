//go:build !windows

package sshx

import (
	"context"
	"io"
	"net"
	"os"

	"golang.org/x/crypto/ssh/agent"
)

// dialHostAgent connects to the agent named by SSH_AUTH_SOCK.
func dialHostAgent(ctx context.Context) (agent.Agent, io.Closer, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, nil, errNoAgent
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, nil, err
	}
	return agent.NewClient(c), c, nil
}
