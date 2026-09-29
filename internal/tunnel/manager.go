package tunnel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/netguard"
	"github.com/termstead/termstead/internal/sshx"
	"github.com/termstead/termstead/internal/term"
)

// Per-user limits.
const (
	maxTunnelsPerUser = 1000
	maxRunningPerUser = 256
)

// Manager owns every running tunnel of the process (TUN-1): one supervised runner per started tunnel, the
// session-integrated forwards (TUN-7) and the status event stream.
type Manager struct {
	d        *app.Deps
	pool     *sshx.Pool
	sessions *term.Manager
	repo     *repo
	log      *slog.Logger
	ctx      context.Context

	// acquire returns a pooled SSH link for a saved connection (sshx.Pool.Get; replaced in tests).
	acquire func(ctx context.Context, user *model.User, connID string) (link, func(), error)
	// forSession returns the SSH link of a live runtime session (sshx.Pool.ForSession; replaced in tests).
	forSession func(ctx context.Context, user *model.User, sessionID string) (link, func(), error)

	mu      sync.Mutex
	runners map[string]*runner
	locks   map[string]*keyLock // serializes start / stop of one tunnel

	sf *sessionForwards

	pwMu    sync.Mutex
	watches map[string]*portWatch // listening-port watchers by session id ("tunnel.ports" topic)
}

// keyLock is a reference-counted per-tunnel mutex.
type keyLock struct {
	sync.Mutex
	refs int
}

// lockTunnel serializes lifecycle operations (start, stop, restart) of one tunnel; call the returned func to unlock.
func (m *Manager) lockTunnel(id string) func() {
	m.mu.Lock()
	l := m.locks[id]
	if l == nil {
		l = &keyLock{}
		m.locks[id] = l
	}
	l.refs++
	m.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		m.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(m.locks, id)
		}
		m.mu.Unlock()
	}
}

// newManager creates the manager; its goroutines end with d.Ctx.
func newManager(d *app.Deps, pool *sshx.Pool, sessions *term.Manager) *Manager {
	ctx := d.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{
		d: d, pool: pool, sessions: sessions, repo: &repo{st: d.Store}, log: log.With("module", "tunnel"), ctx: ctx,
		runners: map[string]*runner{}, locks: map[string]*keyLock{}, watches: map[string]*portWatch{},
	}
	if pool != nil {
		m.acquire = func(ctx context.Context, user *model.User, connID string) (link, func(), error) {
			c, rel, err := pool.Get(ctx, user, connID)
			if err != nil {
				return nil, nil, err
			}
			return c, rel, nil
		}
		m.forSession = func(ctx context.Context, user *model.User, sessionID string) (link, func(), error) {
			c, rel, err := pool.ForSession(ctx, user, sessionID)
			if err != nil {
				return nil, nil, err
			}
			return c, rel, nil
		}
	}
	m.sf = newSessionForwards(m)
	return m
}

// run publishes throughput / counter changes (at most once a second per tunnel) and reconciles running tunnels
// with the database (deleted tunnels or owners, disabled accounts) until shutdown.
func (m *Manager) run() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastReconcile := time.Now()
	for {
		select {
		case <-m.ctx.Done():
			m.stopAll()
			return
		case now := <-tick.C:
			for _, r := range m.snapshot() {
				r.st.sampleRates(now)
				if r.st.dirty.Swap(false) {
					m.publish(r, "")
				}
			}
			m.sf.tick(now)
			if now.Sub(lastReconcile) >= 30*time.Second {
				lastReconcile = now
				m.reconcile()
			}
		}
	}
}

func (m *Manager) snapshot() []*runner {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*runner, 0, len(m.runners))
	for _, r := range m.runners {
		out = append(out, r)
	}
	return out
}

// reconcile stops tunnels whose row, owner or owner account is gone or disabled.
func (m *Manager) reconcile() {
	ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
	defer cancel()
	for _, r := range m.snapshot() {
		if !r.active() {
			continue
		}
		_, err := m.d.Store.Tunnels.Get(ctx, r.id)
		if errors.Is(err, model.ErrNotFound) {
			m.forget(r.id)
			r.stop()
			m.d.Events.Publish(r.owner.ID, Event{Type: model.EvTunnel, ID: r.id, Status: stoppedStatus(), Change: "deleted"})
			continue
		}
		u, err := m.d.Store.Users.Get(ctx, r.owner.ID)
		reason := ""
		switch {
		case errors.Is(err, model.ErrNotFound) || (err == nil && u.Disabled):
			reason = "the owner's account is disabled"
		case err == nil && u.Role != r.owner.Role:
			// The owner's role changed (e.g. an administrator was demoted in server mode): re-apply the policy.
			r.mu.Lock()
			sp := r.sp
			r.mu.Unlock()
			if sp != nil {
				if perr := m.policy(u, sp); perr != nil {
					reason = "no longer allowed: " + perr.Error()
				}
			}
		}
		if reason == "" && err == nil {
			// The SSH connection must still be visible to the owner: a pooled client outlives the access check made
			// when it was dialed, so a shared connection that stopped being shared would otherwise keep carrying
			// the tunnel until the link drops.
			c, cerr := m.d.Store.Connections.Get(ctx, r.rec.ConnectionID)
			switch {
			case errors.Is(cerr, model.ErrNotFound):
				reason = "the tunnel's SSH connection no longer exists"
			case cerr == nil && !app.Visible(u, c.OwnerID, c.Shared):
				reason = "the SSH connection is no longer shared with you"
			}
		}
		if reason != "" {
			r.stop()
			r.mu.Lock()
			r.state, r.errMsg = model.TunnelError, reason
			r.mu.Unlock()
			m.publish(r, "")
		}
	}
}

func (m *Manager) forget(id string) {
	m.mu.Lock()
	delete(m.runners, id)
	m.mu.Unlock()
}

// stopAll stops every tunnel (shutdown). No database writes happen here.
func (m *Manager) stopAll() {
	var wg sync.WaitGroup
	for _, r := range m.snapshot() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.stop()
		}()
	}
	wg.Wait()
	m.sf.stopAll()
}

// publish sends a tunnel's status to its owner's windows.
func (m *Manager) publish(r *runner, change string) {
	if m.d.Events == nil {
		return
	}
	m.d.Events.Publish(r.owner.ID, Event{Type: model.EvTunnel, ID: r.id, Status: r.status(), Change: change})
}

// publishChange announces a definition change (created / updated / deleted) with the current status.
func (m *Manager) publishChange(ownerID, id, change string) {
	if m.d.Events == nil {
		return
	}
	m.d.Events.Publish(ownerID, Event{Type: model.EvTunnel, ID: id, Status: m.status(id), Change: change})
}

// status returns the runtime status of a tunnel (stopped when it never ran).
func (m *Manager) status(id string) Status {
	m.mu.Lock()
	r := m.runners[id]
	m.mu.Unlock()
	if r == nil {
		return stoppedStatus()
	}
	return r.status()
}

// running reports whether a tunnel is started (starting, running or waiting to reconnect).
func (m *Manager) running(id string) bool {
	m.mu.Lock()
	r := m.runners[id]
	m.mu.Unlock()
	return r != nil && r.active()
}

func (m *Manager) runningCount(ownerID string) int {
	n := 0
	for _, r := range m.snapshot() {
		if r.owner.ID == ownerID && r.active() {
			n++
		}
	}
	return n
}

// specFor checks policy and builds the runtime spec of a saved tunnel (decrypting its proxy password).
func (m *Manager) specFor(ctx context.Context, owner *model.User, rec *record) (*spec, error) {
	d := rec.def()
	if err := d.normalize(); err != nil {
		return nil, err
	}
	opts := rec.Options
	if err := normalizeOptions(&opts, d.kind()); err != nil {
		return nil, err
	}
	secrets := map[string]string{}
	if d.kind().isProxy() && opts.SocksUsername != "" && len(rec.SecretsEnc) > 0 {
		s, err := m.d.Vault.OpenJSON(rec.SecretsEnc)
		if err != nil {
			if errors.Is(err, model.ErrLocked) {
				return nil, httpx.ErrLocked
			}
			return nil, fmt.Errorf("decrypt tunnel secrets: %w", err)
		}
		secrets = s
	}
	sp, err := buildSpec(d, opts, secrets)
	if err != nil {
		return nil, err
	}
	if err := m.policy(owner, sp); err != nil {
		return nil, err
	}
	m.finishSpec(owner, sp)
	return sp, nil
}

// finishSpec attaches the host-side context of a checked spec: Termstead's own port and the owner's destination
// guard.
func (m *Manager) finishSpec(owner *model.User, sp *spec) {
	sp.selfPort = m.selfPort()
	nm := netguard.For(m.d)
	sp.guard = func() *netguard.Guard { return nm.ForUser(owner) }
}

// selfPort returns Termstead's own listen port (0 when unknown).
func (m *Manager) selfPort() int {
	if m.d.Cfg == nil {
		return 0
	}
	_, p, err := net.SplitHostPort(m.d.Cfg.Listen)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

// policy applies the run-mode restrictions (SPEC principle 7): in server mode, listeners on the server host and
// connections made from it are powerful, so non-admins may only open loopback listeners on unprivileged ports and
// no remote (-R) forwards. Termstead's own port is never available.
func (m *Manager) policy(user *model.User, sp *spec) error {
	if sp.bindSocket == "" && sp.kind.listensLocally() && sp.bindPort != 0 && m.d.Cfg != nil {
		if _, p, err := net.SplitHostPort(m.d.Cfg.Listen); err == nil && p == strconv.Itoa(sp.bindPort) {
			return httpx.Conflict(fmt.Sprintf("port %d is Termstead's own port", sp.bindPort))
		}
	}
	if m.d.Cfg == nil || m.d.Cfg.IsDesktop() {
		return nil
	}
	if user.IsAdmin() {
		return m.destinationPolicy(user, sp)
	}
	switch {
	case !sp.kind.listensLocally():
		return httpx.Forbidden("in server mode, remote port forwarding is reserved for administrators")
	case sp.bindSocket != "":
		return httpx.Forbidden("in server mode, Unix socket listeners are reserved for administrators")
	case !isLoopbackHost(sp.bindHost):
		return httpx.Forbidden("in server mode, only administrators can listen on non-loopback addresses")
	case sp.bindPort > 0 && sp.bindPort < 1024:
		return httpx.Forbidden("in server mode, ports below 1024 are reserved for administrators")
	}
	return m.destinationPolicy(user, sp)
}

// destinationPolicy is the early (advisory) part of the destination guard for forwards whose connections are made
// from the Termstead host: a remote forward to a refused IP literal / localhost name, or to a Unix socket of the host,
// is rejected when it is saved or started. The authoritative check runs on every connection (forward.hostDial).
func (m *Manager) destinationPolicy(user *model.User, sp *spec) error {
	if sp.kind != kindRemote {
		return nil
	}
	g := netguard.For(m.d).ForUser(user)
	if g == nil {
		return nil
	}
	if sp.destSocket != "" {
		return httpx.Forbidden("the network policy does not allow connecting to local Unix sockets of the Termstead host")
	}
	return g.CheckLiteral(sp.destHost, sp.destPort)
}

// start starts a saved tunnel for its owner. Manual starts (autostart=false) prepare synchronously so the caller
// gets immediate errors (vault locked, port in use…); autostarts prepare in the background, waiting for the vault
// and retrying failures. Starting a running tunnel is a no-op.
func (m *Manager) start(ctx context.Context, owner *model.User, rec *record, autostart bool) error {
	if m.acquire == nil {
		return httpx.Conflict("SSH is not available")
	}
	defer m.lockTunnel(rec.ID)()
	m.mu.Lock()
	if r := m.runners[rec.ID]; r != nil && r.active() {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	if m.runningCount(owner.ID) >= maxRunningPerUser {
		return httpx.Conflict(fmt.Sprintf("at most %d tunnels can run at the same time", maxRunningPerUser))
	}
	r := newRunner(m, owner, rec, autostart)
	if !autostart {
		if err := r.prepare(ctx); err != nil {
			r.cancel()
			return err
		}
	}
	m.mu.Lock()
	if old := m.runners[rec.ID]; old != nil && old.active() {
		m.mu.Unlock()
		r.cancel()
		if r.fw != nil {
			r.fw.close()
		}
		return nil // started concurrently
	}
	m.runners[rec.ID] = r
	m.mu.Unlock()
	m.publish(r, "")
	go r.run()
	return nil
}

// stop stops a tunnel (no-op when stopped) and publishes the stopped status.
func (m *Manager) stop(id string) {
	defer m.lockTunnel(id)()
	m.mu.Lock()
	r := m.runners[id]
	m.mu.Unlock()
	if r == nil {
		return
	}
	r.stop()
	m.mu.Lock()
	if m.runners[id] == r {
		delete(m.runners, id)
	}
	m.mu.Unlock()
	if m.d.Events != nil {
		m.d.Events.Publish(r.owner.ID, Event{Type: model.EvTunnel, ID: id, Status: stoppedStatus()})
	}
}

// autostartAll starts every autostart tunnel (all users) in the background, shortly after the server started.
func (m *Manager) autostartAll() {
	select {
	case <-m.ctx.Done():
		return
	case <-time.After(500 * time.Millisecond):
	}
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	list, err := m.d.Store.Tunnels.ListAutoStart(ctx)
	if err != nil {
		m.log.Error("tunnel autostart: cannot list tunnels", "err", err)
		return
	}
	users := map[string]*model.User{}
	for _, t := range list {
		u, ok := users[t.OwnerID]
		if !ok {
			u, err = m.d.Store.Users.Get(ctx, t.OwnerID)
			if err != nil {
				u = nil
			}
			users[t.OwnerID] = u
		}
		if u == nil || u.Disabled {
			continue
		}
		mt, err := m.repo.getMeta(ctx, t.ID)
		if err != nil {
			m.log.Warn("tunnel autostart: cannot load tunnel", "tunnel", t.ID, "err", err)
			continue
		}
		rec := &record{Tunnel: t, meta: mt}
		if err := m.start(m.ctx, u, rec, true); err != nil {
			m.log.Warn("tunnel autostart failed", "tunnel", t.ID, "name", t.Name, "err", err)
			continue
		}
		m.log.Info("tunnel autostarted", "tunnel", t.ID, "name", t.Name)
	}
}

// holderOf names the caller's running tunnel (and returns its id) or session forward listening on host:port of this
// machine, if any.
func (m *Manager) holderOf(u *model.User, host string, port int) (string, string) {
	if port <= 0 {
		return "", ""
	}
	for _, r := range m.snapshot() {
		if r.owner.ID != u.ID || !r.active() {
			continue
		}
		if sameListener(r.status().LocalAddr, host, port) {
			return fmt.Sprintf("your tunnel “%s”", r.rec.Name), r.id
		}
	}
	for _, f := range m.sf.list(u, "") {
		if f.Status.State == model.TunnelRunning && sameListener(f.Status.LocalAddr, host, port) {
			return fmt.Sprintf("a port forward of the session “%s”", f.SessionTitle), ""
		}
	}
	return "", ""
}

// sameListener reports whether a bound address ("127.0.0.1:8080", "*:8080", "[::1]:8080") overlaps host:port.
func sameListener(bound, host string, port int) bool {
	bh, bp, err := net.SplitHostPort(bound)
	if err != nil || bp != strconv.Itoa(port) {
		return false
	}
	wild := func(h string) bool { return h == "*" || h == "" || h == "0.0.0.0" || h == "::" }
	if wild(bh) || wild(host) {
		return true
	}
	norm := func(h string) string {
		if strings.EqualFold(h, "localhost") {
			return "127.0.0.1"
		}
		return h
	}
	return norm(bh) == norm(host)
}
