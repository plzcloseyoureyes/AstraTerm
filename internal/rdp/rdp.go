// Package rdp implements NexTerm's RDP session type (PROTO-16, GFX-1..9, GFX-11, GFX-18, CORE-16, CC-15; RESEARCH
// §3.10, §3.11; SPEC §6.3). RDP sessions are graphical runtime sessions (kind "rdp") created through
// POST /api/sessions; they have no terminal backend — this module drives them:
//
//   - Path A (default, engine "ironrdp"): the browser runs IronRDP (WASM). POST /api/sessions/{id}/rdp-ticket hands
//     it a one-time token plus the credentials, and /ws/rdp/{id} relays the RDCleanPath handshake (X.224 + TLS
//     terminated here, certificate trust via the prompt broker) and then the TLS plaintext.
//   - Path B (engine "guacd"): /ws/guac/{id} is a Guacamole WebSocket tunnel; the Go side performs the guacd
//     handshake with the vault credentials (which never reach the browser) and relays whole instructions.
//   - guacd status and an optional Docker sidecar (CORE-16), .rdp file export and native client launch (CC-15).
//
// Every attached viewer is counted with term.Manager.TrackClient and the session state follows the connection.
package rdp

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/nexterm/nexterm/internal/app"
	"github.com/nexterm/nexterm/internal/core"
	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/term"
)

type handler struct {
	d   *app.Deps
	c   *core.Core
	log *slog.Logger
	ctx context.Context

	tickets *ticketStore
	viewers *viewerRegistry
	joins   *guacJoins
	pending *pendingSecrets
	guacd   *guacdManager
	native  *nativeLauncher
}

// Mount registers the module's routes, the guacd feature probe and the session hooks.
func Mount(d *app.Deps, c *core.Core) error {
	_, err := mount(d, c)
	return err
}

func mount(d *app.Deps, c *core.Core) (*handler, error) {
	if d == nil || d.Router == nil || c == nil || c.Sessions == nil {
		return nil, errors.New("rdp: missing dependencies")
	}
	h := newHandler(d, c)
	api := d.Router.API()
	api.POST("/sessions/:id/rdp-ticket", h.handleTicket)
	api.POST("/sessions/:id/rdp-state", h.handleState)
	api.GET("/sessions/:id/rdp-file", h.handleSessionFile)
	api.POST("/sessions/:id/launch-native", h.handleLaunchSession)
	api.GET("/connections/:id/rdp-file", h.handleConnectionFile)
	api.POST("/connections/:id/launch-native", h.handleLaunchConnection)
	api.GET("/guacd/status", h.handleGuacdStatus)
	h.mountRecordings(api)
	d.Router.Admin().POST("/guacd/sidecar", h.handleSidecar)
	d.Router.WS("/ws/rdp/:id", h.handleRelay)
	d.Router.WS("/ws/guac/:id", h.handleGuac)

	app.RegisterFeature("guacd", h.guacd.probe)
	remove := c.Sessions.AddHooks(term.Hooks{OnClose: h.onSessionClosed})
	go func() {
		<-h.ctx.Done()
		remove()
		h.viewers.cancelAll()
		h.native.closeAll()
	}()
	return h, nil
}

func newHandler(d *app.Deps, c *core.Core) *handler {
	ctx := d.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	h := &handler{
		d:       d,
		c:       c,
		log:     log.With("module", "rdp"),
		ctx:     ctx,
		tickets: newTicketStore(),
		viewers: newViewerRegistry(),
		joins:   &guacJoins{m: map[string]guacJoin{}},
		pending: newPendingSecrets(),
	}
	h.guacd = newGuacdManager(h)
	h.native = newNativeLauncher(h)
	return h
}

// onSessionClosed ends every viewer of a closed session and forgets its tickets and pending secrets.
func (h *handler) onSessionClosed(s *term.Session) {
	if s.Kind != model.KindRDP {
		return
	}
	h.tickets.revokeSession(s.ID)
	h.pending.drop(s.ID)
	h.joins.drop(s.ID, "")
	h.viewers.cancelSession(s.ID)
}

// ---- lookups & helpers ----------------------------------------------------------------------------------------------

// session returns the RDP session named in the path. Owners may do everything; with viewOnly, administrators may
// also read other users' sessions. Everyone else gets 404.
func (h *handler) session(c *echo.Context, viewOnly bool) (*term.Session, *model.User, error) {
	u := httpx.UserFrom(c)
	if u == nil {
		return nil, nil, httpx.ErrUnauthorized
	}
	id := c.Param("id")
	if !model.ValidID(id) {
		return nil, nil, httpx.ErrNotFound
	}
	s := h.c.Sessions.Get(id)
	if s == nil {
		return nil, nil, httpx.ErrNotFound
	}
	if s.OwnerID != u.ID {
		if !(viewOnly && u.IsAdmin()) {
			return nil, nil, httpx.ErrNotFound
		}
	}
	if s.Kind != model.KindRDP {
		return nil, nil, httpx.BadRequest("not an RDP session")
	}
	if s.Closed() {
		return nil, nil, httpx.NewError(410, "gone", "the session has been closed")
	}
	return s, u, nil
}

// setState reports a session state (graphical sessions have no backend: the module drives them).
func (h *handler) setState(sessionID string, st model.SessionState, msg string) {
	h.c.Sessions.SetState(sessionID, st, msg)
}

// failState reports a viewer's failure in the session state — unless another viewer of the owner keeps the session
// running (a second tab failing must not mark a working desktop as failed).
func (h *handler) failState(s *term.Session, msg string) {
	if h.viewers.count(s.ID) > 1 {
		return
	}
	h.setState(s.ID, model.StateError, cleanMessage(msg))
}

// endState moves a session to "disconnected" (or "error") when its last (owner's) viewer left, unless it already ended.
func (h *handler) endState(s *term.Session, st model.SessionState, msg string) {
	if h.viewers.count(s.ID) > 0 {
		return
	}
	cur, _ := s.State()
	if cur == model.StateClosed || cur == model.StateError && st != model.StateError {
		return
	}
	if cur == model.StateDisconnected && st == model.StateDisconnected {
		return
	}
	h.setState(s.ID, st, msg)
}

func (h *handler) auditUser(ctx context.Context, user *model.User, action, target string, details any) {
	if h.d.Audit == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.d.Audit.LogUser(context.WithoutCancel(ctx), user, action, target, details)
}

// isDesktop reports desktop mode.
func (h *handler) isDesktop() bool { return h.d.Cfg != nil && h.d.Cfg.IsDesktop() }

// allowGlobalTrust reports whether user may replace a trusted certificate (a change affecting every user): desktop
// mode or administrators.
func (h *handler) allowGlobalTrust(u *model.User) bool { return h.isDesktop() || u.IsAdmin() }

// ---- viewers --------------------------------------------------------------------------------------------------------

// viewer is one attached relay (/ws/rdp) or tunnel (/ws/guac).
type viewer struct {
	sessionID string
	engine    string
	shadow    bool // an administrator's read-only view: never drives the session state
	cancel    context.CancelFunc
}

type viewerRegistry struct {
	mu        sync.Mutex
	bySession map[string]map[*viewer]struct{}
}

func newViewerRegistry() *viewerRegistry {
	return &viewerRegistry{bySession: map[string]map[*viewer]struct{}{}}
}

func (r *viewerRegistry) add(v *viewer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.bySession[v.sessionID]
	if m == nil {
		m = map[*viewer]struct{}{}
		r.bySession[v.sessionID] = m
	}
	m[v] = struct{}{}
}

// remove unregisters v and returns the number of viewers left on its session.
func (r *viewerRegistry) remove(v *viewer) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.bySession[v.sessionID]
	delete(m, v)
	if len(m) == 0 {
		delete(r.bySession, v.sessionID)
	}
	return len(m)
}

// count returns the number of the owner's viewers of a session (read-only shadows excluded).
func (r *viewerRegistry) count(sessionID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for v := range r.bySession[sessionID] {
		if !v.shadow {
			n++
		}
	}
	return n
}

func (r *viewerRegistry) cancelSession(sessionID string) {
	r.mu.Lock()
	var list []*viewer
	for v := range r.bySession[sessionID] {
		list = append(list, v)
	}
	r.mu.Unlock()
	for _, v := range list {
		v.cancel()
	}
}

func (r *viewerRegistry) cancelAll() {
	r.mu.Lock()
	var list []*viewer
	for _, m := range r.bySession {
		for v := range m {
			list = append(list, v)
		}
	}
	r.mu.Unlock()
	for _, v := range list {
		v.cancel()
	}
}

// ---- guacd connections administrators can join ----------------------------------------------------------------------

// guacJoin identifies the guacd connection an owner's tunnel drives: administrators join it read-only (shadowing).
type guacJoin struct {
	id            string // guacd connection id ("$…")
	width, height int
}

type guacJoins struct {
	mu sync.Mutex
	m  map[string]guacJoin // by session
}

func (j *guacJoins) set(sessionID string, v guacJoin) {
	j.mu.Lock()
	j.m[sessionID] = v
	j.mu.Unlock()
}

func (j *guacJoins) get(sessionID string) (guacJoin, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	v, ok := j.m[sessionID]
	return v, ok
}

// drop forgets the session's guacd connection (only if it is still id, when id is set).
func (j *guacJoins) drop(sessionID, id string) {
	j.mu.Lock()
	if cur, ok := j.m[sessionID]; ok && (id == "" || cur.id == id) {
		delete(j.m, sessionID)
	}
	j.mu.Unlock()
}

// ---- secrets answered at prompts, saved once the connection proved them right ---------------------------------------

type pendingSecret struct {
	connID  string
	userID  string
	values  map[string]string
	expires time.Time
}

type pendingSecrets struct {
	mu sync.Mutex
	m  map[string]*pendingSecret // by session
}

func newPendingSecrets() *pendingSecrets { return &pendingSecrets{m: map[string]*pendingSecret{}} }

const pendingSecretTTL = 10 * time.Minute

func (p *pendingSecrets) put(sessionID, connID, userID string, values map[string]string) {
	if connID == "" || len(values) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.m[sessionID]
	if cur == nil || cur.connID != connID {
		cur = &pendingSecret{connID: connID, userID: userID, values: map[string]string{}}
		p.m[sessionID] = cur
	}
	for k, v := range values {
		cur.values[k] = v
	}
	cur.expires = time.Now().Add(pendingSecretTTL)
}

// take removes and returns the pending secrets of a session (nil when none or expired).
func (p *pendingSecrets) take(sessionID string) *pendingSecret {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.m[sessionID]
	delete(p.m, sessionID)
	if cur == nil || time.Now().After(cur.expires) {
		return nil
	}
	return cur
}

func (p *pendingSecrets) drop(sessionID string) {
	p.mu.Lock()
	delete(p.m, sessionID)
	p.mu.Unlock()
}

// commitPending saves the secrets answered (with "save") at prompts of a session that just connected successfully.
func (h *handler) commitPending(ctx context.Context, s *term.Session, user *model.User) {
	ps := h.pending.take(s.ID)
	if ps == nil || user == nil || ps.userID != user.ID {
		return
	}
	for k, v := range ps.values {
		if err := h.saveConnectionSecret(ctx, user, ps.connID, k, v); err != nil {
			h.log.Warn("rdp: saving the answered credential failed", "session", s.ID, "key", k, "err", err)
		}
	}
}

// saveConnectionSecret stores a prompt answer into the owner's saved connection (vault-encrypted).
func (h *handler) saveConnectionSecret(ctx context.Context, user *model.User, connID, key, value string) error {
	if h.d.Store == nil || h.d.Vault == nil || connID == "" || user == nil {
		return errors.New("secrets cannot be saved for this connection")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	c, err := h.d.Store.Connections.Get(ctx, connID)
	if err != nil {
		return err
	}
	if c.OwnerID != user.ID {
		return errors.New("only the owner can save secrets of this connection")
	}
	secrets, err := h.d.Vault.OpenJSON(c.SecretsEnc)
	if err != nil {
		return err
	}
	secrets[key] = value
	enc, err := h.d.Vault.SealJSON(secrets)
	if err != nil {
		return err
	}
	c.SecretsEnc = enc
	c.SecretKeys = c.SecretKeys[:0]
	for _, k := range model.SortedKeys(secrets) {
		if secrets[k] != "" {
			c.SecretKeys = append(c.SecretKeys, k)
		}
	}
	if err := h.d.Store.Connections.Update(ctx, c); err != nil {
		return err
	}
	h.auditUser(ctx, user, "connection.secret.save", c.ID, map[string]any{"key": key})
	return nil
}

// cleanMessage shortens a state message for the session list.
func cleanMessage(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	return clipRunes(s, 480)
}
