// Package sshx is AstraTerm's SSH layer (SPEC §4 "SSH pool", RESEARCH §3.2): a ref-counted pool of authenticated
// *ssh.Client connections shared by terminals, SFTP, monitoring and tunnels; the generic Dialer (proxy → SSH jump
// chain → target) used by every network protocol; interactive authentication and host-key verification relayed
// through the events prompt broker; agent and X11 forwarding; and the "ssh" terminal protocol.
package sshx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// DefaultIdleTTL is how long a client stays connected after its last release.
const DefaultIdleTTL = 60 * time.Second

// Pool shares SSH clients between sessions and features.
type Pool struct {
	d        *app.Deps
	sessions *term.Manager
	log      *slog.Logger
	ctx      context.Context

	// IdleTTL overrides DefaultIdleTTL (tests).
	IdleTTL time.Duration

	mu        sync.Mutex
	entries   map[string]*entry
	bySession map[string]*sshBackend
	closed    bool
}

// entry groups the clients of one pool key: the primary plus overflow clients opened when the server's MaxSessions
// limit was reached.
type entry struct {
	sem     chan struct{} // serializes dials of this key (context-aware mutex)
	clients []*Client
}

// New creates the pool and registers the "ssh" terminal protocol.
func New(d *app.Deps, sessions *term.Manager) *Pool {
	ctx := context.Background()
	log := slog.Default()
	if d != nil && d.Ctx != nil {
		ctx = d.Ctx
	}
	if d != nil && d.Log != nil {
		log = d.Log
	}
	p := &Pool{
		d:         d,
		sessions:  sessions,
		log:       log.With("module", "sshx"),
		ctx:       ctx,
		IdleTTL:   DefaultIdleTTL,
		entries:   map[string]*entry{},
		bySession: map[string]*sshBackend{},
	}
	term.RegisterProtocol(string(model.ProtoSSH), p.openTerminal)
	go func() {
		<-ctx.Done()
		p.closeAll()
	}()
	return p
}

func poolKey(user *model.User, conn *model.Connection) string {
	port := conn.Port
	if port == 0 {
		port = 22
	}
	uid := ""
	if user != nil {
		uid = user.ID
	}
	return uid + "\x00" + strings.ToLower(conn.Host) + "\x00" + strconv.Itoa(port) + "\x00" + conn.Username
}

// Get returns a shared, ref-counted client for a saved connection visible to user (dialing through jump hosts and
// proxies, verifying host keys and prompting as needed). Always call release when done.
func (p *Pool) Get(ctx context.Context, user *model.User, connID string) (c *Client, release func(), err error) {
	if p.d == nil {
		return nil, nil, errors.New("sshx: pool has no dependencies")
	}
	conn, secrets, err := p.d.ResolveConnection(ctx, user, connID)
	if err != nil {
		return nil, nil, err
	}
	switch conn.Protocol {
	case model.ProtoSSH, model.ProtoSFTP, model.ProtoMosh:
	default:
		return nil, nil, httpx.BadRequest(fmt.Sprintf("connection %q is not an SSH connection", conn.Name))
	}
	return p.Acquire(ctx, user, conn, secrets)
}

// Acquire dials (or reuses) a client for an ad-hoc connection spec (quick connect, jump hops); clients are keyed by
// user + host + port + username.
func (p *Pool) Acquire(ctx context.Context, user *model.User, conn *model.Connection, secrets map[string]string) (c *Client, release func(), err error) {
	return p.acquire(ctx, user, conn, secrets, dialOpts{session: term.SessionFromContext(ctx)})
}

// ForSession returns the SSH client used by a live runtime session (quick-connect sessions included), so the SFTP
// side panel, monitoring and "open tunnel from session" reuse the terminal's connection.
func (p *Pool) ForSession(ctx context.Context, user *model.User, sessionID string) (c *Client, release func(), err error) {
	if user == nil {
		return nil, nil, httpx.ErrUnauthorized
	}
	if p.sessions != nil {
		s := p.sessions.Get(sessionID)
		if s == nil || s.OwnerID != user.ID {
			return nil, nil, httpx.ErrNotFound
		}
		if s.Protocol != model.ProtoSSH {
			return nil, nil, httpx.BadRequest("not an SSH session")
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.bySession[sessionID]
	if b == nil || !b.client.Alive() {
		return nil, nil, term.ErrNotConnected
	}
	c = b.client
	p.refLocked(c)
	return c, p.releaser(c), nil
}

// acquire implements Acquire with dial options. The route (jump hops, proxy) is resolved before the key's dial lock is
// taken, so no lock is held while other keys are acquired (no lock-order cycles between chains).
func (p *Pool) acquire(ctx context.Context, user *model.User, conn *model.Connection, secrets map[string]string, o dialOpts) (*Client, func(), error) {
	if user == nil {
		return nil, nil, httpx.ErrUnauthorized
	}
	if conn == nil || conn.Host == "" {
		return nil, nil, httpx.BadRequest("host is required")
	}
	key := poolKey(user, conn)
	if c, rel := p.lookup(key); c != nil {
		return c, rel, nil
	}

	rt, err := p.routeFor(ctx, user, conn, secrets, o)
	if err != nil {
		return nil, nil, err
	}
	defer rt.release() // no-op once handed over to the new client

	e, err := p.entry(key)
	if err != nil {
		return nil, nil, err
	}
	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		p.gcEntry(key, e)
		return nil, nil, ctx.Err()
	}
	defer func() {
		<-e.sem
		p.gcEntry(key, e)
	}()
	if c, rel := p.lookup(key); c != nil {
		return c, rel, nil // dialed concurrently by someone else
	}

	c, err := p.dial(ctx, user, conn, secrets, rt, o)
	if err != nil {
		return nil, nil, err
	}
	c.key = key
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		c.Close()
		return nil, nil, errors.New("sshx: shutting down")
	}
	e = p.entries[key]
	if e == nil {
		e = &entry{sem: make(chan struct{}, 1)}
		p.entries[key] = e
	}
	e.clients = append(e.clients, c)
	c.refs = 1
	p.mu.Unlock()
	go p.watch(c)
	return c, p.releaser(c), nil
}

// lookup returns a live primary client for key with a reference taken.
func (p *Pool) lookup(key string) (*Client, func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.entries[key]; e != nil {
		for _, c := range e.clients {
			if !c.overflow && c.Alive() {
				p.refLocked(c)
				return c, p.releaser(c)
			}
		}
	}
	return nil, nil
}

func (p *Pool) entry(key string) (*entry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("sshx: shutting down")
	}
	e := p.entries[key]
	if e == nil {
		e = &entry{sem: make(chan struct{}, 1)}
		p.entries[key] = e
	}
	return e, nil
}

// gcEntry drops an entry that holds no client and no pending dial (e.g. after a failed dial).
func (p *Pool) gcEntry(key string, e *entry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries[key] == e && len(e.clients) == 0 && len(e.sem) == 0 {
		delete(p.entries, key)
	}
}

// routeFor returns the route a new client for conn is dialed along: an explicit route, a gateway client (a new
// reference is taken so the gateway outlives the chain that created it), or conn's own jump chain / proxy.
func (p *Pool) routeFor(ctx context.Context, user *model.User, conn *model.Connection, secrets map[string]string, o dialOpts) (*route, error) {
	switch {
	case o.route != nil:
		return o.route, nil
	case o.via != nil:
		p.mu.Lock()
		p.refLocked(o.via)
		p.mu.Unlock()
		return &route{via: o.via, rel: p.releaser(o.via), timeout: connectTimeout(conn.Options)}, nil
	}
	return p.buildRoute(ctx, user, conn, secrets, o.session, true)
}

// overflowCandidates returns the live overflow clients of c's pool key, each with a reference taken (MaxSessions
// exhaustion fallback, SSH-35).
func (p *Pool) overflowCandidates(c *Client) []*Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[c.key]
	if e == nil {
		return nil
	}
	var out []*Client
	for _, oc := range e.clients {
		if oc != c && oc.overflow && oc.Alive() {
			p.refLocked(oc)
			out = append(out, oc)
		}
	}
	return out
}

// dialOverflow opens an additional connection with c's credentials and adds it to c's pool entry.
func (p *Pool) dialOverflow(ctx context.Context, c *Client) (*Client, func(), error) {
	o := dialOpts{session: term.SessionFromContext(ctx)}
	rt, err := p.routeFor(ctx, c.user, c.Conn, c.secrets, o)
	if err != nil {
		return nil, nil, err
	}
	nc, err := p.dial(ctx, c.user, c.Conn, c.secrets, rt, o)
	rt.release()
	if err != nil {
		return nil, nil, err
	}
	nc.key, nc.overflow = c.key, true
	p.mu.Lock()
	e := p.entries[c.key]
	if e == nil || p.closed {
		p.mu.Unlock()
		nc.Close()
		return nil, nil, errors.New("sshx: connection pool entry is gone")
	}
	e.clients = append(e.clients, nc)
	nc.refs = 1
	p.mu.Unlock()
	go p.watch(nc)
	return nc, p.releaser(nc), nil
}

func (p *Pool) refLocked(c *Client) {
	c.refs++
	if c.idle != nil {
		c.idle.Stop()
		c.idle = nil
	}
}

func (p *Pool) releaser(c *Client) func() {
	return sync.OnceFunc(func() { p.release(c) })
}

// release drops one reference; the client is closed after IdleTTL without references (immediately when dead).
func (p *Pool) release(c *Client) {
	p.mu.Lock()
	c.refs--
	if c.refs > 0 {
		p.mu.Unlock()
		return
	}
	if !c.Alive() || p.closed {
		p.removeLocked(c)
		p.mu.Unlock()
		c.Close()
		return
	}
	ttl := p.IdleTTL
	if ttl <= 0 {
		ttl = DefaultIdleTTL
	}
	if c.idle != nil {
		c.idle.Stop()
	}
	c.idle = time.AfterFunc(ttl, func() {
		p.mu.Lock()
		if c.refs > 0 {
			p.mu.Unlock()
			return
		}
		p.removeLocked(c)
		p.mu.Unlock()
		c.Close()
	})
	p.mu.Unlock()
}

func (p *Pool) removeLocked(c *Client) {
	e := p.entries[c.key]
	if e == nil {
		return
	}
	for i, x := range e.clients {
		if x == c {
			e.clients = append(e.clients[:i], e.clients[i+1:]...)
			break
		}
	}
	if len(e.clients) == 0 && len(e.sem) == 0 {
		delete(p.entries, c.key)
	}
	for id, b := range p.bySession {
		if b.client == c {
			delete(p.bySession, id)
		}
	}
}

// watch removes a client from the pool as soon as its transport ends so new acquirers dial afresh.
func (p *Pool) watch(c *Client) {
	<-c.done
	p.mu.Lock()
	p.removeLocked(c)
	p.mu.Unlock()
	if err := c.Err(); err != nil {
		p.log.Debug("ssh connection ended", "host", c.Conn.Host, "err", err)
	}
}

func (p *Pool) trackSession(sessionID string, b *sshBackend) {
	p.mu.Lock()
	p.bySession[sessionID] = b
	p.mu.Unlock()
}

// untrackSession forgets b as the backend of sessionID unless a newer backend (reconnect) replaced it.
func (p *Pool) untrackSession(sessionID string, b *sshBackend) {
	p.mu.Lock()
	if p.bySession[sessionID] == b {
		delete(p.bySession, sessionID)
	}
	p.mu.Unlock()
}

func (p *Pool) closeAll() {
	p.mu.Lock()
	p.closed = true
	var all []*Client
	for _, e := range p.entries {
		all = append(all, e.clients...)
	}
	p.entries = map[string]*entry{}
	p.bySession = map[string]*sshBackend{}
	p.mu.Unlock()
	for _, c := range all {
		c.Close()
	}
}

// Stats reports the number of pooled clients (diagnostics/tests).
func (p *Pool) Stats() (clients int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		clients += len(e.clients)
	}
	return clients
}
