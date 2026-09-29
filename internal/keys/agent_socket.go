package keys

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh/agent"

	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
)

// The agent socket exposes the owner's keyring to local programs (git, ssh, IDEs): a Unix socket in the data
// directory (mode 0600 inside the 0700 data directory, peers must run as the same OS user) or, on Windows, a named
// pipe restricted to the current user. Desktop mode only: in server mode the host's processes are not the user's.

const maxAgentClients = 64

type agentSocket struct {
	svc      *agentService
	owner    *model.User
	endpoint string
	ln       net.Listener
	started  time.Time
	ctx      context.Context
	cancel   context.CancelFunc

	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	clients int
	wg      sync.WaitGroup
}

// peerInfo describes the local process connected to the agent socket (best effort).
type peerInfo struct {
	pid  int
	name string
}

func (p peerInfo) label() string {
	switch {
	case p.name != "" && p.pid > 0:
		return fmt.Sprintf("%s (pid %d)", p.name, p.pid)
	case p.name != "":
		return p.name
	case p.pid > 0:
		return fmt.Sprintf("process %d", p.pid)
	}
	return "a local program"
}

var errAgentUnsupported = errors.New("the built-in SSH agent is only available in desktop mode")

// start opens the agent socket for user (idempotent for the same user).
func (s *agentService) start(user *model.User) error {
	d := s.h.d
	if d.Cfg == nil || !d.Cfg.IsDesktop() {
		return httpx.Forbidden(errAgentUnsupported.Error())
	}
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if cur := s.current(); cur != nil {
		if cur.owner.ID == user.ID {
			return nil
		}
		return httpx.Conflict("the built-in agent is running for another user")
	}
	if d.Ctx != nil && d.Ctx.Err() != nil {
		return httpx.Conflict("NexTerm is shutting down")
	}
	endpoint, err := agentEndpoint(d.Cfg.DataDir)
	if err != nil {
		return httpx.NewError(409, "agent_unavailable", err.Error())
	}
	ln, err := listenAgent(endpoint)
	if err != nil {
		return httpx.NewError(409, "agent_unavailable", err.Error())
	}
	parent := d.Ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	sock := &agentSocket{svc: s, owner: user, endpoint: endpoint, ln: ln, started: time.Now(), ctx: ctx, cancel: cancel,
		conns: map[net.Conn]struct{}{}}
	s.mu.Lock()
	s.sock = sock
	s.mu.Unlock()
	sock.wg.Add(1)
	go sock.acceptLoop()
	context.AfterFunc(ctx, func() { _ = ln.Close() })
	s.log.Info("built-in SSH agent started", "endpoint", endpoint, "user", user.Username)
	return nil
}

// stop closes the agent socket; it reports whether it was running. The owner's keyring forgets added keys,
// decrypted keys, locks and decisions (like killing ssh-agent).
func (s *agentService) stop() bool {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	s.mu.Lock()
	sock := s.sock
	s.sock = nil
	s.mu.Unlock()
	if sock == nil {
		return false
	}
	sock.close()
	s.ring(sock.owner.ID).reset()
	s.log.Info("built-in SSH agent stopped")
	return true
}

func (a *agentSocket) acceptLoop() {
	defer a.wg.Done()
	for {
		c, err := a.ln.Accept()
		if err != nil {
			if a.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-a.ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		peer, err := checkPeer(c)
		if err != nil {
			a.svc.log.Warn("agent: rejected a connection", "err", err)
			_ = c.Close()
			continue
		}
		a.mu.Lock()
		if len(a.conns) >= maxAgentClients {
			a.mu.Unlock()
			_ = c.Close()
			continue
		}
		a.conns[c] = struct{}{}
		a.clients++
		a.mu.Unlock()
		a.wg.Add(1)
		go a.serve(c, peer)
	}
}

func (a *agentSocket) serve(c net.Conn, peer peerInfo) {
	defer a.wg.Done()
	defer func() {
		_ = c.Close()
		a.mu.Lock()
		delete(a.conns, c)
		a.mu.Unlock()
	}()
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	view := a.svc.ring(a.owner.ID).view(ctx, localOrigin(peer), false, "")
	_ = agent.ServeAgent(view, c)
}

func (a *agentSocket) close() {
	a.cancel()
	_ = a.ln.Close()
	a.mu.Lock()
	for c := range a.conns {
		_ = c.Close()
	}
	a.mu.Unlock()
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		a.svc.log.Warn("agent: connections did not close in time")
	}
	cleanupEndpoint(a.endpoint)
}

// current returns the running socket (nil when stopped).
func (s *agentService) current() *agentSocket {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sock
}

// open reports the number of connected clients.
func (a *agentSocket) open() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.conns)
}
