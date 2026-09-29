package sshx

import (
	"context"
	"errors"
	"io"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/nexterm/nexterm/internal/model"
)

// Built-in agent integration (SSH-11/12). The keys module runs NexTerm's own SSH agent ("MobAgent") and registers it
// with Pool.SetBuiltinAgent:
//
//   - authentication: while the built-in agent runs for the user, its keys are offered after the connection's own key,
//     merged with the host agent's keys (duplicates removed). Its signatures may ask for a passphrase or a
//     confirmation, so they pause the handshake deadline;
//   - agent forwarding (desktop mode / admins): the running built-in agent, read-only, merged with the host agent;
//   - agent forwarding otherwise (no host agent, or server mode): the module's policy-aware keyring of the user's stored
//     keys — only the keys selected for the agent, optional confirm-before-use naming the requesting host — instead of
//     the static keyring of agent.go.

// ErrAgentKeyNotFound is returned by built-in agent views asked to sign with a key they do not hold, so that a merged
// agent tries the next agent.
var ErrAgentKeyNotFound = errors.New("agent: key not found")

// BuiltinAgent is implemented by the keys module.
type BuiltinAgent interface {
	// Agent returns a view of the user's running built-in agent, or nil when it is not running for user. ctx bounds
	// interactive prompts. conn is the connection being authenticated (its own stored key is left out) or, with
	// forwarding, the connection whose server uses the forwarded agent (such views refuse modifications).
	Agent(ctx context.Context, user *model.User, conn *model.Connection, forwarding bool) agent.ExtendedAgent
	// Keyring returns the read-only agent of user's stored keys served through agent forwarding when neither the
	// running built-in agent nor the host agent is used (nil: the static keyring of agent.go is used).
	Keyring(ctx context.Context, user *model.User, conn *model.Connection) agent.Agent
}

// SetBuiltinAgent registers the pool's built-in agent (nil unregisters it).
func (p *Pool) SetBuiltinAgent(b BuiltinAgent) {
	p.setProvider(func(pr *providers) { pr.agent = b })
}

// dialAgent returns the agent whose keys authFlow.signers offers: the running built-in agent merged with the host
// agent (either may be absent).
func (a *authFlow) dialAgent() (agent.Agent, io.Closer, error) {
	var b agent.ExtendedAgent
	if ba := a.p.provider().agent; ba != nil {
		b = ba.Agent(a.ctx, a.user, a.conn, false)
	}
	host, closer, err := dialHostAgent(a.ctx)
	if b == nil {
		return host, closer, err
	}
	pb := &pausingAgent{ExtendedAgent: b, dc: a.dc}
	if err != nil {
		return pb, nopCloser{}, nil
	}
	return &mergedAgent{agents: []agent.Agent{pb, host}}, closer, nil
}

// dialForwardAgent returns the agent served on a forwarded-agent channel when the host agent may be used: the running
// built-in agent (read-only) merged with the host agent. dialCtx only bounds dialing the host agent.
func (c *Client) dialForwardAgent(dialCtx context.Context) (agent.Agent, io.Closer, error) {
	var b agent.ExtendedAgent
	var stop context.CancelFunc = func() {}
	if ba := c.pool.provider().agent; ba != nil {
		ctx, cancel := c.channelContext()
		if b = ba.Agent(ctx, c.user, c.Conn, true); b == nil {
			cancel()
		} else {
			stop = cancel
		}
	}
	host, closer, err := dialHostAgent(dialCtx)
	switch {
	case b == nil:
		return host, closer, err
	case err != nil:
		return b, closerFunc(stop), nil
	}
	return &mergedAgent{agents: []agent.Agent{b, readOnlyHost(host)}}, closerFunc(func() { stop(); _ = closer.Close() }), nil
}

// readOnlyHost is readOnlyAgent keeping the host agent's signature flags (rsa-sha2-256/512) and extensions (e.g.
// session-bind@openssh.com) when it supports them.
func readOnlyHost(a agent.Agent) agent.Agent {
	if ea, ok := a.(agent.ExtendedAgent); ok {
		return readOnlyExtendedAgent{ea}
	}
	return readOnlyAgent{a}
}

type readOnlyExtendedAgent struct{ agent.ExtendedAgent }

func (readOnlyExtendedAgent) Add(agent.AddedKey) error       { return errReadOnlyAgent }
func (readOnlyExtendedAgent) Remove(ssh.PublicKey) error     { return errReadOnlyAgent }
func (readOnlyExtendedAgent) RemoveAll() error               { return errReadOnlyAgent }
func (readOnlyExtendedAgent) Lock(passphrase []byte) error   { return errReadOnlyAgent }
func (readOnlyExtendedAgent) Unlock(passphrase []byte) error { return errReadOnlyAgent }

// fallbackKeyring is served for agent forwarding when the host agent is not used.
func (c *Client) fallbackKeyring() (agent.Agent, func()) {
	if ba := c.pool.provider().agent; ba != nil {
		ctx, cancel := c.channelContext()
		if k := ba.Keyring(ctx, c.user, c.Conn); k != nil {
			return k, cancel
		}
		cancel()
	}
	return c.userKeyring(), func() {}
}

// channelContext lives until the connection ends or the returned cancel is called (prompts raised for a forwarded
// agent are withdrawn when the SSH connection goes away).
func (c *Client) channelContext() (context.Context, context.CancelFunc) {
	parent := context.Background()
	if c.pool != nil && c.pool.ctx != nil {
		parent = c.pool.ctx
	}
	ctx, cancel := context.WithCancel(parent)
	if c.done != nil {
		go func() {
			select {
			case <-c.done:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	return ctx, cancel
}

type closerFunc func()

func (f closerFunc) Close() error {
	f()
	return nil
}

// ---- merged agent ----------------------------------------------------------------------------------------------

// mergedAgent combines agents (built-in first): keys are listed once, a signature is made by the first agent holding
// the key. It is read-only.
type mergedAgent struct{ agents []agent.Agent }

func (m *mergedAgent) List() ([]*agent.Key, error) {
	var (
		out      []*agent.Key
		firstErr error
	)
	seen := map[string]bool{}
	for _, a := range m.agents {
		keys, err := a.List()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, k := range keys {
			if !seen[string(k.Blob)] {
				seen[string(k.Blob)] = true
				out = append(out, k)
			}
		}
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func (m *mergedAgent) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	return m.SignWithFlags(key, data, 0)
}

func (m *mergedAgent) SignWithFlags(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	for i, a := range m.agents {
		var (
			sig *ssh.Signature
			err error
		)
		if ea, ok := a.(agent.ExtendedAgent); ok {
			sig, err = ea.SignWithFlags(key, data, flags)
		} else if flags == 0 {
			sig, err = a.Sign(key, data)
		} else {
			continue
		}
		if err == nil {
			return sig, nil
		}
		if !errors.Is(err, ErrAgentKeyNotFound) || i == len(m.agents)-1 {
			return nil, err
		}
	}
	return nil, ErrAgentKeyNotFound
}

func (m *mergedAgent) Signers() ([]ssh.Signer, error) {
	var (
		out      []ssh.Signer
		firstErr error
	)
	seen := map[string]bool{}
	for _, a := range m.agents {
		ss, err := a.Signers()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, s := range ss {
			if b := string(s.PublicKey().Marshal()); !seen[b] {
				seen[b] = true
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// Extension passes extension requests (e.g. session-bind@openssh.com) to every agent; it succeeds when one does.
func (m *mergedAgent) Extension(extensionType string, contents []byte) ([]byte, error) {
	var (
		res []byte
		ok  bool
	)
	for _, a := range m.agents {
		if ea, is := a.(agent.ExtendedAgent); is {
			if r, err := ea.Extension(extensionType, contents); err == nil && !ok {
				res, ok = r, true
			}
		}
	}
	if !ok {
		return nil, agent.ErrExtensionUnsupported
	}
	return res, nil
}

func (m *mergedAgent) Add(agent.AddedKey) error       { return errReadOnlyAgent }
func (m *mergedAgent) Remove(ssh.PublicKey) error     { return errReadOnlyAgent }
func (m *mergedAgent) RemoveAll() error               { return errReadOnlyAgent }
func (m *mergedAgent) Lock(passphrase []byte) error   { return errReadOnlyAgent }
func (m *mergedAgent) Unlock(passphrase []byte) error { return errReadOnlyAgent }

// ---- deadline pausing ------------------------------------------------------------------------------------------

// pausingAgent pauses the handshake deadline while its signers sign (they may wait for the user).
type pausingAgent struct {
	agent.ExtendedAgent
	dc *deadlineConn
}

func (p *pausingAgent) Signers() ([]ssh.Signer, error) {
	ss, err := p.ExtendedAgent.Signers()
	if err != nil || p.dc == nil {
		return ss, err
	}
	out := make([]ssh.Signer, len(ss))
	for i, s := range ss {
		out[i] = pauseSigner(s, p.dc)
	}
	return out, nil
}

// pauseSigner wraps s so that its signatures pause the handshake deadline. The wrapper implements ssh.AlgorithmSigner
// only when s does: x/crypto assumes an AlgorithmSigner supports every signature algorithm of its key format.
func pauseSigner(s ssh.Signer, dc *deadlineConn) ssh.Signer {
	ps := &pausingSigner{Signer: s, dc: dc}
	if as, ok := s.(ssh.AlgorithmSigner); ok {
		return &pausingAlgorithmSigner{pausingSigner: ps, as: as}
	}
	return ps
}

type pausingSigner struct {
	ssh.Signer
	dc *deadlineConn
}

func (s *pausingSigner) Sign(rand io.Reader, data []byte) (*ssh.Signature, error) {
	s.dc.pause()
	defer s.dc.resume()
	return s.Signer.Sign(rand, data)
}

type pausingAlgorithmSigner struct {
	*pausingSigner
	as ssh.AlgorithmSigner
}

func (s *pausingAlgorithmSigner) SignWithAlgorithm(rand io.Reader, data []byte, algorithm string) (*ssh.Signature, error) {
	s.dc.pause()
	defer s.dc.resume()
	return s.as.SignWithAlgorithm(rand, data, algorithm)
}
