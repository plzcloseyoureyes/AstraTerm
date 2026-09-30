package tunnel

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// Reconnect backoff (1 s … 60 s, ±20 % jitter), as for terminal sessions.
const (
	backoffMin = time.Second
	backoffMax = 60 * time.Second
	pollEvery  = 2 * time.Second
)

// runner supervises one started saved tunnel: it prepares the forward (decrypting proxy credentials, binding the
// listener), owns the pooled SSH link (acquire, watch, re-acquire with backoff) and reports the tunnel status.
type runner struct {
	m         *Manager
	id        string
	owner     *model.User
	rec       *record
	autostart bool // retry failures of the very first start (boot / vault unlock)

	st     *stats
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	idle   atomic.Int64 // on-demand idle timeout (ns), from the spec

	// set once during preparation (under mu), read-only afterwards
	sp *spec
	fw *forward

	mu         sync.Mutex
	state      string
	errMsg     string
	waiting    string
	retryAt    time.Time
	startedAt  time.Time
	reconnects int
	everUp     bool // the tunnel has been fully running at least once
	stopped    bool
	cur        link
	curRel     func()
	gen        uint64
	lastErr    error
	notify     chan struct{} // closed and replaced on every link change
	demand     chan struct{} // a client needs the link (on-demand tunnels)
	warning    string        // see Status.Warning
	probed     bool          // the GatewayPorts probe was started (once per start)
	probeDone  chan struct{} // closed when the GatewayPorts probe finished
}

func newRunner(m *Manager, owner *model.User, rec *record, autostart bool) *runner {
	ctx, cancel := context.WithCancel(m.ctx)
	return &runner{
		m: m, id: rec.ID, owner: owner, rec: rec, autostart: autostart,
		st: &stats{}, ctx: ctx, cancel: cancel, done: make(chan struct{}),
		state: model.TunnelStarting, startedAt: time.Now().UTC(),
		notify: make(chan struct{}), demand: make(chan struct{}, 1), probeDone: make(chan struct{}),
	}
}

// prepare builds the spec and forward and, for listeners on the AstraTerm host, binds the listener. Errors are typed
// API errors (vault locked, address in use, invalid configuration…).
func (r *runner) prepare(ctx context.Context) error {
	sp, err := r.m.specFor(ctx, r.owner, r.rec)
	if err != nil {
		return err
	}
	fw := newForward(r.ctx, sp, r.st, r.m.log.With("tunnel", r.id), r)
	if sp.kind.listensLocally() {
		if err := fw.listenLocal(); err != nil {
			fw.close()
			return err
		}
	}
	r.idle.Store(int64(sp.idle))
	r.mu.Lock()
	r.sp, r.fw = sp, fw
	r.mu.Unlock()
	return nil
}

// status returns a snapshot of the runner's status.
func (r *runner) status() Status {
	r.mu.Lock()
	st := Status{TunnelStatus: model.TunnelStatus{State: r.state, Error: r.errMsg}}
	if r.state == model.TunnelRunning || r.state == model.TunnelStarting {
		t := r.startedAt
		st.StartedAt = &t
		st.Warning = r.warning
	}
	st.Connected = r.cur != nil
	st.Reconnects = r.reconnects
	st.Waiting = r.waiting
	if !r.retryAt.IsZero() {
		t := r.retryAt
		st.RetryAt = &t
	}
	r.mu.Unlock()
	r.st.fill(&st)
	return st
}

func (r *runner) active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.stopped && r.state != model.TunnelError && r.state != model.TunnelStopped
}

// set updates the state and publishes it.
func (r *runner) set(state, msg, waiting string, retryAt time.Time) {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.state, r.errMsg, r.waiting, r.retryAt = state, truncate(msg, 600), waiting, retryAt
	r.mu.Unlock()
	r.m.publish(r, "")
}

// fail ends the runner with an error state.
func (r *runner) fail(msg string) {
	r.set(model.TunnelError, msg, "", time.Time{})
}

// ---- link management ----------------------------------------------------------------------------------------------

func (r *runner) bumpLocked() {
	r.gen++
	close(r.notify)
	r.notify = make(chan struct{})
}

func (r *runner) setLink(l link, rel func()) {
	r.mu.Lock()
	r.cur, r.curRel, r.lastErr = l, rel, nil
	r.bumpLocked()
	r.mu.Unlock()
	r.st.touch()
}

func (r *runner) clearLink() {
	r.mu.Lock()
	rel := r.curRel
	r.cur, r.curRel = nil, nil
	r.bumpLocked()
	r.mu.Unlock()
	if rel != nil {
		rel()
	}
}

func (r *runner) linkFailed(err error) {
	r.mu.Lock()
	r.lastErr = err
	r.bumpLocked()
	r.mu.Unlock()
}

// linkFor implements linkProvider: it returns the live link, waiting while the supervisor (re)connects and waking
// it up for on-demand tunnels. It fails when a connection attempt made during the wait fails.
func (r *runner) linkFor(ctx context.Context) (link, error) {
	r.mu.Lock()
	gen := r.gen
	for {
		if r.cur != nil {
			l := r.cur
			r.mu.Unlock()
			return l, nil
		}
		if r.stopped || r.state == model.TunnelError {
			msg := r.errMsg
			r.mu.Unlock()
			if msg == "" {
				msg = "the tunnel is stopped"
			}
			return nil, errors.New(msg)
		}
		if r.gen != gen && r.lastErr != nil {
			err := r.lastErr
			r.mu.Unlock()
			return nil, err
		}
		ch := r.notify
		onDemand := r.sp != nil && r.sp.onDemand
		r.mu.Unlock()
		if onDemand {
			select {
			case r.demand <- struct{}{}:
			default:
			}
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, fmt.Errorf("the SSH connection is not available: %w", ctx.Err())
		}
		r.mu.Lock()
	}
}

// ---- supervisor ---------------------------------------------------------------------------------------------------

// run is the supervisor goroutine. everUp is only touched by this goroutine.
func (r *runner) run() {
	defer close(r.done)
	defer r.clearLink()
	defer func() {
		// Whatever ended the supervisor (stop, error), the listener must not outlive it.
		r.mu.Lock()
		fw := r.fw
		r.mu.Unlock()
		if fw != nil {
			fw.close()
		}
	}()
	if r.fw == nil && !r.prepareLoop() {
		return
	}
	attempt := 0
	for r.ctx.Err() == nil {
		if r.sp.onDemand {
			r.set(model.TunnelRunning, "", "demand", time.Time{})
			select {
			case <-r.ctx.Done():
				return
			case <-r.demand:
			}
			r.set(model.TunnelRunning, "", "", time.Time{}) // connecting for a waiting client
		}
		l, rel, err := r.m.acquire(r.ctx, r.owner, r.rec.ConnectionID)
		if err != nil {
			if r.ctx.Err() != nil {
				return
			}
			r.linkFailed(err)
			if !r.waitAfterFailure(err, &attempt) {
				return
			}
			continue
		}
		attempt = 0
		r.setLink(l, rel)
		lost, idle := r.serveLink(l)
		r.clearLink()
		if r.fw.proxy != nil {
			r.fw.proxy.dropIdleUpstreams()
		}
		if r.ctx.Err() != nil {
			return
		}
		switch {
		case idle:
			continue // on-demand: disconnected after the idle timeout, wait for the next client
		case lost == nil:
			continue
		}
		if rle, ok := errors.AsType[*errRemoteListen](lost); ok {
			// The remote listener was refused. On the very first start that is a configuration problem; after a
			// reconnect the server may still hold the previous connection's listener for a while.
			r.mu.Lock()
			everUp := r.everUp
			r.mu.Unlock()
			if !everUp || !r.sp.autoReconnect {
				r.fail(rle.Error())
				return
			}
			attempt++
			if !r.backoffWait(attempt, rle.Error()) {
				return
			}
			continue
		}
		if r.sp.onDemand {
			r.st.setLastError("SSH connection lost: " + lost.Error())
			continue
		}
		if !r.sp.autoReconnect {
			r.fail("SSH connection lost: " + lost.Error())
			return
		}
		r.mu.Lock()
		r.reconnects++
		r.mu.Unlock()
		attempt++
		if !r.backoffWait(attempt, "SSH connection lost: "+lost.Error()) {
			return
		}
	}
}

// prepareLoop prepares an autostarted tunnel, waiting for the vault and retrying transient bind failures.
func (r *runner) prepareLoop() bool {
	attempt := 0
	for r.ctx.Err() == nil {
		err := r.prepare(r.ctx)
		if err == nil {
			return true
		}
		switch {
		case errors.Is(err, httpx.ErrLocked):
			r.set(model.TunnelStarting, "the vault is locked: the tunnel starts once it is unlocked", "vault", time.Time{})
			if !r.waitUntil(func() bool { return !r.m.d.Vault.Locked() }) {
				return false
			}
		case isAddrInUse(err) || strings.Contains(err.Error(), "already in use"):
			attempt++
			if !r.backoffWait(attempt, err.Error()) {
				return false
			}
		default:
			r.fail(err.Error())
			return false
		}
	}
	return false
}

// serveLink runs the forward over a live link until the link ends (lost carries the reason), the runner stops, or
// (on-demand) the tunnel was idle long enough (idle).
func (r *runner) serveLink(l link) (lost error, idle bool) {
	if !r.sp.kind.listensLocally() {
		err := r.fw.serveRemote(r.ctx, l, func(bound string) {
			r.mu.Lock()
			r.everUp = true
			probe := !r.probed && r.sp.exposedRemote()
			if probe {
				r.probed = true
			}
			r.mu.Unlock()
			r.set(model.TunnelRunning, "", "", time.Time{})
			if probe {
				go r.probeGatewayPorts(l, bound)
			}
		})
		if err != nil {
			return err, false
		}
		// serveRemote returned: the link is gone (or going), unless the runner is stopping.
		select {
		case <-r.ctx.Done():
			return nil, false
		case <-l.Done():
			return linkErr(l), false
		}
	}
	r.mu.Lock()
	r.everUp = true
	r.mu.Unlock()
	r.set(model.TunnelRunning, "", "", time.Time{})
	var tick <-chan time.Time
	if r.sp.onDemand {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-r.ctx.Done():
			return nil, false
		case <-l.Done():
			return linkErr(l), false
		case <-tick:
			if r.st.active.Load() == 0 && r.st.idleFor() >= time.Duration(r.idle.Load()) {
				return nil, true
			}
		}
	}
}

// linkErr describes why a link ended.
func linkErr(l link) error {
	if e, ok := l.(interface{ Err() error }); ok {
		if err := e.Err(); err != nil {
			return err
		}
	}
	return errors.New("the SSH connection closed")
}

// probeGatewayPorts runs the GatewayPorts check (bindWarning) of an exposed remote forward, once per start.
func (r *runner) probeGatewayPorts(l link, bound string) {
	defer close(r.probeDone)
	msg := bindWarning(r.ctx, l, bound)
	if msg == "" {
		return
	}
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.warning = msg
	r.mu.Unlock()
	r.m.publish(r, "")
}

// waitAfterFailure handles a failed connection attempt: it waits for the vault / an interactive client, backs off,
// or ends the runner (permanent errors, first manual start). It returns false when the runner must stop.
func (r *runner) waitAfterFailure(err error, attempt *int) bool {
	msg := err.Error()
	switch {
	case errors.Is(err, httpx.ErrLocked):
		r.set(model.TunnelStarting, "the vault is locked: the tunnel connects once it is unlocked", "vault", time.Time{})
		return r.waitUntil(func() bool { return !r.m.d.Vault.Locked() })
	case strings.Contains(msg, "no AstraTerm window is connected"):
		r.set(model.TunnelStarting, "a login prompt needs an answer: waiting for a AstraTerm window", "client", time.Time{})
		return r.waitUntil(func() bool { return r.m.d.Events.HasClient(r.owner.ID) })
	case permanent(err):
		r.fail(msg)
		return false
	}
	r.mu.Lock()
	everUp := r.everUp
	r.mu.Unlock()
	if r.sp.onDemand {
		// Waiting clients got the error; the next client triggers a new attempt (after a short pause).
		r.st.setLastError(msg)
		*attempt++
		return r.backoffWait(*attempt, "")
	}
	if !everUp && !r.autostart {
		r.fail(msg)
		return false
	}
	if everUp && !r.sp.autoReconnect {
		r.fail(msg)
		return false
	}
	*attempt++
	return r.backoffWait(*attempt, msg)
}

// permanent reports errors that retrying cannot fix: authentication / host key failures, cancelled prompts,
// unanswered prompts, invalid or deleted connections, policy refusals.
func permanent(err error) bool {
	if term.IsPermanent(err) {
		return true
	}
	if strings.Contains(err.Error(), "no answer to the") {
		return true
	}
	if he, ok := errors.AsType[*httpx.HTTPError](err); ok {
		return he.Status >= 400 && he.Status < 500 && he.Status != http.StatusLocked && he.Status != http.StatusConflict
	}
	if me, ok := errors.AsType[*model.Error](err); ok {
		return me.Code == model.CodeNotFound || me.Code == model.CodeForbidden || me.Code == model.CodeBadRequest
	}
	return false
}

// backoffWait sleeps before reconnect attempt n (state "starting" with msg and the retry time). It returns false when
// the runner was stopped meanwhile.
func (r *runner) backoffWait(n int, msg string) bool {
	d := backoff(n)
	at := time.Now().Add(d)
	if msg != "" {
		msg = fmt.Sprintf("%s — retrying in %s", msg, d.Round(time.Second))
		r.set(model.TunnelStarting, msg, "", at.UTC())
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-r.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func backoff(n int) time.Duration {
	d := backoffMin << min(max(n-1, 0), 6)
	d = min(d, backoffMax)
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

// waitUntil polls cond until it holds (true) or the runner stops (false).
func (r *runner) waitUntil(cond func() bool) bool {
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for !cond() {
		select {
		case <-r.ctx.Done():
			return false
		case <-t.C:
		}
	}
	return true
}

// stop shuts the runner down (idempotent) and waits for it.
func (r *runner) stop() {
	r.mu.Lock()
	r.stopped = true
	fw := r.fw
	r.bumpLocked() // wake waiting clients
	r.mu.Unlock()
	r.cancel()
	if fw != nil {
		fw.close()
	}
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		r.m.log.Warn("tunnel supervisor did not stop in time", "tunnel", r.id)
	}
	r.mu.Lock()
	if r.state != model.TunnelError {
		r.state, r.errMsg = model.TunnelStopped, ""
	}
	r.waiting, r.retryAt = "", time.Time{}
	r.mu.Unlock()
}
