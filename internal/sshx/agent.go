package sshx

import (
	"context"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/nexterm/nexterm/internal/model"
)

// Agent support (SSH-9/10/12). The host agent (SSH_AUTH_SOCK on Unix; the Windows OpenSSH agent pipe or Pageant on
// Windows, see agent_*.go) is used for authentication and forwarding in desktop mode, or by administrators in server
// mode (there the host's agent is not the user's). Without a usable host agent, forwarding serves a read-only
// in-memory keyring of the user's stored keys that can be decrypted without prompting.

var errNoAgent = errors.New("no SSH agent is available on this host")

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// HostAgentAvailable reports whether a host agent can be reached (for UI hints).
func HostAgentAvailable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, c, err := dialHostAgent(ctx)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// forwardAgent registers the auth-agent@openssh.com handler on the connection (once) and requests agent forwarding
// for sess. It must be called before Shell / Start.
func (c *Client) forwardAgent(sess *ssh.Session) error {
	c.agentOnce.Do(func() {
		chans := c.Client.HandleChannelOpen(agentChannelType)
		if chans == nil {
			c.agentErr = errors.New("agent forwarding is already handled on this connection")
			return
		}
		go func() {
			for nc := range chans {
				go c.serveAgentChannel(nc)
			}
		}()
	})
	if c.agentErr != nil {
		return c.agentErr
	}
	return agent.RequestAgentForwarding(sess)
}

const agentChannelType = "auth-agent@openssh.com"

func (c *Client) serveAgentChannel(nc ssh.NewChannel) {
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	defer ch.Close()
	if c.pool != nil && c.pool.allowLocalExec(c.user) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ag, closer, err := c.dialForwardAgent(ctx) // host agent (+ running built-in agent, agent_builtin.go)
		cancel()
		if err == nil {
			defer closer.Close()
			_ = agent.ServeAgent(ag, ch)
			return
		}
	}
	kr, done := c.fallbackKeyring() // built-in keyring (agent_builtin.go), else userKeyring
	defer done()
	_ = agent.ServeAgent(kr, ch)
}

// userKeyring lazily builds the read-only keyring of the user's stored keys.
func (c *Client) userKeyring() agent.Agent {
	c.keyringOnce.Do(func() {
		kr := agent.NewKeyring()
		if c.pool != nil && c.pool.d != nil && c.pool.d.Store != nil && c.user != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			keys, err := c.pool.d.Store.Keys.ListByOwner(ctx, c.user.ID)
			if err == nil {
				for _, k := range keys {
					addStoredKey(ctx, c.pool, c.user, k, kr)
				}
			}
		}
		c.keyring = readOnlyAgent{kr}
	})
	return c.keyring
}

func addStoredKey(ctx context.Context, p *Pool, user *model.User, k *model.SSHKey, kr agent.Agent) {
	pemBytes, pass, err := p.d.KeyMaterial(ctx, user, k.ID)
	if err != nil {
		return
	}
	raw, err := parseRawPrivateKey(pemBytes, pass)
	if err != nil {
		return // encrypted without a stored passphrase, or unsupported
	}
	added := agent.AddedKey{PrivateKey: raw, Comment: k.Name}
	if cert := strings.TrimSpace(k.Certificate); cert != "" {
		if pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(cert)); err == nil {
			if cc, ok := pk.(*ssh.Certificate); ok {
				added.Certificate = cc
			}
		}
	}
	_ = kr.Add(added)
}

// readOnlyAgent refuses modifications from the remote side.
type readOnlyAgent struct{ agent.Agent }

var errReadOnlyAgent = errors.New("agent: read-only")

func (readOnlyAgent) Add(agent.AddedKey) error       { return errReadOnlyAgent }
func (readOnlyAgent) Remove(ssh.PublicKey) error     { return errReadOnlyAgent }
func (readOnlyAgent) RemoveAll() error               { return errReadOnlyAgent }
func (readOnlyAgent) Lock(passphrase []byte) error   { return errReadOnlyAgent }
func (readOnlyAgent) Unlock(passphrase []byte) error { return errReadOnlyAgent }
