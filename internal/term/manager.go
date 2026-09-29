package term

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/config"
	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/store"
)

// Manager owns every runtime session of the process.
type Manager struct {
	d        *app.Deps
	ctx      context.Context
	log      *slog.Logger
	ringSize int
	ttl      time.Duration

	mu       sync.RWMutex
	sessions map[string]*Session

	// done is closed once the lifecycle loop has exited after closing every session on shutdown (see Wait).
	done chan struct{}

	hooksMu sync.Mutex
	hookSeq uint64
	hookMap map[uint64]*Hooks
	hooks   atomic.Pointer[[]*Hooks] // copy-on-write snapshot for the hot path

	startup atomic.Pointer[StartupHandler] // see SetStartupHandler
}

// StartupHandler may take over sending a session's `startupCommand`, e.g. to type it after the connection's logon
// actions. It is called on every successful (re)connect (before the OnState(connected) hooks) with the resolved
// connection. Returning true means the handler sends it later by calling send (at most once; send is a no-op when the
// session has reconnected or ended in the meantime); false lets term send it at once, as without a handler.
type StartupHandler func(s *Session, conn *model.Connection, send func()) bool

// SetStartupHandler installs (nil removes) the startup handler; there is one per manager.
func (m *Manager) SetStartupHandler(h StartupHandler) {
	if h == nil {
		m.startup.Store(nil)
		return
	}
	m.startup.Store(&h)
}

// Hooks let other modules observe sessions without editing term (logon actions, triggers, recording, audit,
// monitor). Callbacks run synchronously on session goroutines without session locks held: OnOutput and OnInput
// MUST be fast and non-blocking and must not retain data beyond the call unless they copy it.
type Hooks struct {
	OnState  func(s *Session, st model.SessionState)
	OnOutput func(s *Session, data []byte)
	// OnInput sees input as typed. For WriteSensitive input it gets a same-length mask instead (see SensitiveMask),
	// unless OnSensitiveInput is set, which is then called instead with that mask.
	OnInput          func(s *Session, data []byte)
	OnSensitiveInput func(s *Session, masked []byte)
	OnClose          func(s *Session)
}

// New creates the session manager. Sessions are closed when d.Ctx is cancelled.
func New(d *app.Deps) *Manager {
	ctx := context.Background()
	log := slog.Default()
	ringSize := config.DefaultScrollbackBytes
	ttl := config.DefaultDetachedTTL
	if d != nil {
		if d.Ctx != nil {
			ctx = d.Ctx
		}
		if d.Log != nil {
			log = d.Log
		}
		if d.Cfg != nil {
			if d.Cfg.ScrollbackBytes > 0 {
				ringSize = d.Cfg.ScrollbackBytes
			}
			ttl = d.Cfg.DetachedSessionTTL
		}
	}
	// The ring must hold more than the flow-control window so a single client never loses output.
	ringSize = min(max(ringSize, MinScrollback), 256<<20)
	m := &Manager{
		d:        d,
		ctx:      ctx,
		log:      log.With("module", "term"),
		ringSize: ringSize,
		ttl:      ttl,
		sessions: map[string]*Session{},
		done:     make(chan struct{}),
		hookMap:  map[uint64]*Hooks{},
	}
	empty := []*Hooks{}
	m.hooks.Store(&empty)
	go m.run()
	return m
}

// Wait blocks until the manager has shut down — d.Ctx was cancelled and every session has been closed, with its
// recording, log and audit rows written — or until ctx ends. The server calls it before closing the database.
func (m *Manager) Wait(ctx context.Context) error {
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---- lifecycle loop -----------------------------------------------------------------------------------------------

func (m *Manager) run() {
	defer close(m.done)
	flush := time.NewTicker(time.Second)
	defer flush.Stop()
	reapEvery := 30 * time.Second
	if m.ttl > 0 {
		reapEvery = min(max(m.ttl/4, time.Second), 30*time.Second)
	}
	lastReap := time.Now()
	for {
		select {
		case <-m.ctx.Done():
			m.closeAll("server shutting down")
			return
		case <-flush.C:
			m.flushWriters()
			if time.Since(lastReap) >= reapEvery {
				lastReap = time.Now()
				m.reap()
			}
		}
	}
}

func (m *Manager) snapshot() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

func (m *Manager) flushWriters() {
	for _, s := range m.snapshot() {
		s.mu.Lock()
		rec, lg := s.recorder, s.logger
		s.mu.Unlock()
		if rec != nil {
			rec.Flush()
		}
		if lg != nil {
			lg.Flush()
		}
	}
}

// reap closes sessions that have had no attached client for longer than Cfg.DetachedSessionTTL (0 = never).
func (m *Manager) reap() {
	if m.ttl <= 0 {
		return
	}
	now := time.Now()
	for _, s := range m.snapshot() {
		s.mu.Lock()
		idle := !s.closed && len(s.clients)+s.extClients == 0 && !s.detachedAt.IsZero() && now.Sub(s.detachedAt) >= m.ttl
		s.mu.Unlock()
		if idle {
			m.log.Info("closing detached session", "session", s.ID, "ttl", m.ttl)
			m.closeSession(context.Background(), s, nil, "detached session timeout")
		}
	}
}

func (m *Manager) closeAll(reason string) {
	for _, s := range m.snapshot() {
		m.closeSession(context.Background(), s, nil, reason)
	}
}

// ---- creation -----------------------------------------------------------------------------------------------------

// CreateRequest is the body of POST /api/sessions.
type CreateRequest struct {
	ConnectionID string           `json:"connectionId,omitempty"`
	Quick        *QuickConnection `json:"quick,omitempty"`
	Cols         int              `json:"cols"`
	Rows         int              `json:"rows"`
	Title        string           `json:"title,omitempty"`

	// Connection and Secrets let Go callers open an already resolved connection (never settable through JSON).
	// Connection.ID empty means "ad-hoc" (no saved connection is touched).
	Connection *model.Connection `json:"-"`
	Secrets    map[string]string `json:"-"`
}

// QuickConnection is an ad-hoc connection spec (Partial<Connection> & {password?}).
type QuickConnection struct {
	model.Connection
	Password string `json:"password,omitempty"`
}

// Create starts a new runtime session for user. Terminal sessions connect asynchronously (state "connecting");
// graphical sessions (vnc, rdp) are created without a backend — their module drives them through SetState.
func (m *Manager) Create(ctx context.Context, user *model.User, req CreateRequest) (*Session, error) {
	if user == nil {
		return nil, httpx.ErrUnauthorized
	}
	conn, secrets, quick, err := m.resolveRequest(ctx, user, req)
	if err != nil {
		return nil, err
	}
	kind := model.KindForProtocol(conn.Protocol)
	if kind == model.KindTerminal && lookupOpener(string(conn.Protocol)) == nil {
		return nil, httpx.BadRequest(fmt.Sprintf("protocol %q cannot be opened as a terminal session", conn.Protocol))
	}
	if needsHost(conn.Protocol) && conn.Host == "" {
		return nil, httpx.BadRequest("host is required")
	}
	if p := lookupPolicy(string(conn.Protocol)); p != nil {
		if err := p(ctx, user, conn); err != nil {
			return nil, err
		}
	}
	var enc encoding.Encoding
	if kind == model.KindTerminal {
		if enc, err = LookupEncoding(conn.Options.String("encoding")); err != nil {
			return nil, httpx.BadRequest(err.Error())
		}
	}
	title := cleanTitle(req.Title)
	if title == "" {
		title = defaultTitle(conn)
	}
	owner := *user
	now := time.Now().UTC()
	if !quick {
		// Saved connections are re-resolved on every connect; only prompt answers are kept in memory.
		secrets = map[string]string{}
	}
	s := &Session{
		m:              m,
		ID:             model.NewID(),
		Kind:           kind,
		Protocol:       conn.Protocol,
		OwnerID:        user.ID,
		CreatedAt:      now,
		owner:          &owner,
		inq:            newInputQueue(),
		conn:           conn,
		secrets:        secrets,
		quick:          quick,
		title:          title,
		state:          model.StateConnecting,
		cols:           ClampSize(req.Cols, 80),
		rows:           ClampSize(req.Rows, 24),
		ring:           newRing(m.ringSize),
		clients:        map[*client]struct{}{},
		detachedAt:     now,
		enc:            enc,
		encoder:        newEncoder(enc),
		backspaceCtrlH: strings.EqualFold(conn.Options.String("backspace"), "ctrl-h"),
		termType:       conn.Options.String("term"),
		autoReconnect:  conn.Options.Bool("autoReconnect"),
	}
	if kind != model.KindTerminal {
		s.ring = newRing(1024)
	}
	s.cond = sync.NewCond(&s.mu)

	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()

	if !quick && conn.ID != "" && m.d != nil && m.d.Store != nil {
		if err := m.d.Store.Connections.TouchUsed(ctx, conn.ID, store.Now()); err != nil {
			m.log.Debug("term: touch connection failed", "connection", conn.ID, "err", err)
		}
	}
	m.audit(ctx, user, "session.open", s.ID, map[string]any{"protocol": conn.Protocol, "host": conn.Host,
		"connectionId": s.Info().ConnectionID, "title": title})

	if kind == model.KindTerminal {
		if m.flag(ctx, s, conn, "record") {
			if err := m.startRecording(ctx, s); err != nil {
				m.log.Warn("term: cannot start recording", "session", s.ID, "err", err)
				s.Notice("Recording could not be started: " + err.Error())
			}
		}
		if m.flag(ctx, s, conn, "log") {
			if err := m.startLogging(ctx, s); err != nil {
				m.log.Warn("term: cannot start text log", "session", s.ID, "err", err)
				s.Notice("Logging could not be started: " + err.Error())
			}
		}
		s.mu.Lock()
		s.startConnectLocked("")
		s.mu.Unlock()
	}
	m.publish(s)
	return s, nil
}

func needsHost(p model.Protocol) bool {
	switch p {
	case model.ProtoSSH, model.ProtoTelnet, model.ProtoRlogin, model.ProtoRaw, model.ProtoMosh, model.ProtoVNC,
		model.ProtoRDP, model.ProtoWinRM, model.ProtoIPMI:
		return true
	}
	return false
}

func (m *Manager) resolveRequest(ctx context.Context, user *model.User, req CreateRequest) (*model.Connection, map[string]string, bool, error) {
	switch {
	case req.Connection != nil:
		c := req.Connection.Clone()
		c.Secrets = nil
		c.Normalize()
		if c.Port == 0 {
			c.Port = model.DefaultPort(c.Protocol)
		}
		if c.OwnerID == "" {
			c.OwnerID = user.ID
		}
		secrets := make(map[string]string, len(req.Secrets))
		for k, v := range req.Secrets {
			secrets[k] = v
		}
		return c, secrets, c.ID == "", nil
	case req.ConnectionID != "":
		if !model.ValidID(req.ConnectionID) {
			return nil, nil, false, httpx.ErrNotFound
		}
		if m.d == nil {
			return nil, nil, false, httpx.ErrNotFound
		}
		conn, secrets, err := m.d.ResolveConnection(ctx, user, req.ConnectionID)
		if err != nil {
			return nil, nil, false, err
		}
		if conn.Port == 0 {
			conn.Port = model.DefaultPort(conn.Protocol)
		}
		return conn, secrets, false, nil
	case req.Quick != nil:
		conn, secrets, err := m.quickConnection(ctx, user, req.Quick)
		return conn, secrets, true, err
	}
	return nil, nil, false, httpx.BadRequest("connectionId or quick is required")
}

// quickConnection validates and synthesizes an ad-hoc connection owned by user.
func (m *Manager) quickConnection(ctx context.Context, user *model.User, q *QuickConnection) (*model.Connection, map[string]string, error) {
	c := &model.Connection{
		Name:       strings.TrimSpace(q.Name),
		Protocol:   q.Protocol,
		Host:       strings.TrimSpace(q.Host),
		Port:       q.Port,
		Username:   strings.TrimSpace(q.Username),
		IdentityID: q.IdentityID,
		KeyID:      q.KeyID,
		AuthMethod: q.AuthMethod,
		Options:    q.Options.Clone(),
		OwnerID:    user.ID,
	}
	if c.Protocol == "" {
		c.Protocol = model.ProtoSSH
	}
	if !model.ValidProtocol(c.Protocol) {
		return nil, nil, httpx.BadRequest("invalid protocol")
	}
	if c.AuthMethod == "" {
		c.AuthMethod = model.AuthAuto
	}
	if !model.ValidAuthMethod(c.AuthMethod) {
		return nil, nil, httpx.BadRequest("invalid authMethod")
	}
	if c.Port < 0 || c.Port > 65535 {
		return nil, nil, httpx.BadRequest("port must be between 1 and 65535")
	}
	if c.Port == 0 {
		c.Port = model.DefaultPort(c.Protocol)
	}
	if len(c.Host) > 255 || strings.ContainsFunc(c.Host, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return nil, nil, httpx.BadRequest("invalid host")
	}
	if len(c.Username) > 256 || strings.ContainsFunc(c.Username, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return nil, nil, httpx.BadRequest("invalid username")
	}
	if c.KeyID != "" && !model.ValidID(c.KeyID) {
		return nil, nil, httpx.BadRequest("invalid keyId")
	}
	secrets := map[string]string{}
	for k, v := range q.Secrets {
		if v != "" && k != "" {
			secrets[k] = v
		}
	}
	if q.Password != "" {
		secrets[model.SecretPassword] = q.Password
	}
	if c.IdentityID != "" {
		if m.d == nil || m.d.Store == nil || !model.ValidID(c.IdentityID) {
			return nil, nil, httpx.NotFound("identity not found")
		}
		ident, err := m.d.Store.Identities.Get(ctx, c.IdentityID)
		if err != nil || ident.OwnerID != user.ID {
			return nil, nil, httpx.NotFound("identity not found")
		}
		if c.Username == "" {
			c.Username = ident.Username
		}
		if c.KeyID == "" {
			c.KeyID = ident.KeyID
		}
		is, err := m.d.Vault.OpenJSON(ident.SecretsEnc)
		if err != nil {
			if errors.Is(err, model.ErrLocked) {
				return nil, nil, httpx.ErrLocked
			}
			return nil, nil, fmt.Errorf("decrypt identity secrets: %w", err)
		}
		for k, v := range is {
			if _, ok := secrets[k]; !ok {
				secrets[k] = v
			}
		}
	}
	c.Normalize()
	return c, secrets, nil
}

func defaultTitle(c *model.Connection) string {
	if c.Name != "" {
		return cleanTitle(c.Name)
	}
	if c.Protocol == model.ProtoLocal {
		if sh := c.Options.String("shell", ""); sh != "" {
			return cleanTitle(filepath.Base(strings.ReplaceAll(sh, `\`, "/")))
		}
		return "Local shell"
	}
	if c.Host != "" {
		if c.Username != "" {
			return cleanTitle(c.Username + "@" + c.Host)
		}
		return cleanTitle(c.Host)
	}
	return string(c.Protocol)
}

func cleanTitle(s string) string {
	s = sanitizeTitle(s)
	if utf8.RuneCountInString(s) > maxTitleRunes {
		s = string([]rune(s)[:maxTitleRunes])
	}
	return s
}

// ---- lookup -------------------------------------------------------------------------------------------------------

// Get returns a live session, or nil.
func (m *Manager) Get(id string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

// List returns user's sessions (all sessions when all is set and user is an admin), oldest first.
func (m *Manager) List(user *model.User, all bool) []*Session {
	if user == nil {
		return nil
	}
	all = all && user.IsAdmin()
	m.mu.RLock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if all || s.OwnerID == user.ID {
			out = append(out, s)
		}
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// ---- operations ---------------------------------------------------------------------------------------------------

// Write injects input into a connected session (snippets, multi-exec, logon actions, macros).
func (m *Manager) Write(id string, data []byte) error {
	s := m.Get(id)
	if s == nil {
		return model.ErrNotFound
	}
	return s.writeInput(data, nil)
}

// WriteSensitive injects a secret (inject-secret, logon / macro / trigger secret steps) into a connected session like
// Write, but the bytes never reach the asciicast input track, and input hooks only see SensitiveMask(data) — so
// recordings with recordInput, command audit and other observers never get the plaintext.
func (m *Manager) WriteSensitive(id string, data []byte) error {
	s := m.Get(id)
	if s == nil {
		return model.ErrNotFound
	}
	return s.writeInputOpts(data, nil, true)
}

// SensitiveMask returns a same-length stand-in for secret input: every byte becomes '*' except trailing CR / LF, so
// observers still see where a line ends (e.g. that Enter was pressed) but nothing of the secret.
func SensitiveMask(data []byte) []byte {
	out := make([]byte, len(data))
	end := len(data)
	for end > 0 && (data[end-1] == '\r' || data[end-1] == '\n') {
		end--
	}
	for i := range data {
		if i < end {
			out[i] = '*'
		} else {
			out[i] = data[i]
		}
	}
	return out
}

// Resize changes the terminal size of a session (clamped to [2..1000]).
func (m *Manager) Resize(id string, cols, rows int) error {
	s := m.Get(id)
	if s == nil {
		return model.ErrNotFound
	}
	s.resize(cols, rows, nil)
	return nil
}

// Reconnect re-runs the opener on the same session id.
func (m *Manager) Reconnect(id string) error {
	s := m.Get(id)
	if s == nil {
		return model.ErrNotFound
	}
	return s.reconnect()
}

// Rename sets the session title (an empty title restores the default).
func (m *Manager) Rename(id, title string) error {
	s := m.Get(id)
	if s == nil {
		return model.ErrNotFound
	}
	title = cleanTitle(title)
	s.mu.Lock()
	if title == "" {
		title = defaultTitle(s.conn)
	}
	s.title = title
	s.mu.Unlock()
	m.publish(s)
	return nil
}

// Close closes a session (backend, recordings, attached sockets) and forgets it.
func (m *Manager) Close(id string) error {
	return m.CloseContext(context.Background(), id)
}

// CloseContext is Close with the acting request context (audited as the request's user).
func (m *Manager) CloseContext(ctx context.Context, id string) error {
	s := m.Get(id)
	if s == nil {
		return model.ErrNotFound
	}
	m.closeSession(ctx, s, httpx.UserFromContext(ctx), "closed")
	return nil
}

func (m *Manager) closeSession(ctx context.Context, s *Session, by *model.User, reason string) {
	m.mu.Lock()
	if m.sessions[s.ID] == s {
		delete(m.sessions, s.ID)
	}
	m.mu.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.gen++
	if s.cancel != nil {
		s.cancel()
	}
	if s.reconnectTimer != nil {
		s.reconnectTimer.Stop()
		s.reconnectTimer = nil
	}
	be := s.backend
	s.backend = nil
	s.state, s.stateMsg = model.StateClosed, ""
	s.queueStateLocked()
	for c := range s.clients {
		c.closing = true
		c.wake()
		time.AfterFunc(wsCloseGrace, c.cancel)
	}
	rec, recMeta := s.recorder, s.recording
	lg, logMeta := s.logger, s.logRec
	s.recorder, s.recording, s.logger, s.logRec = nil, nil, nil, nil
	s.cond.Broadcast()
	s.mu.Unlock()

	if be != nil {
		go be.Close()
	}
	if lg != nil {
		lg.Note("--- session closed (" + reason + ")")
	}
	m.finishRecording(rec, recMeta)
	m.finishRecording(lg, logMeta)

	s.pubMu.Lock()
	if s.pubTimer != nil {
		s.pubTimer.Stop()
		s.pubTimer = nil
	}
	s.pubMu.Unlock()
	if m.d != nil && m.d.Events != nil {
		m.d.Events.Publish(s.OwnerID, sessionClosedEvent{Type: model.EvSessionClosed, ID: s.ID})
	}
	if by == nil {
		by = s.owner
	}
	m.audit(ctx, by, "session.close", s.ID, map[string]any{"protocol": s.Protocol, "reason": reason})
	m.runStateHooks(s, model.StateClosed)
	for _, h := range *m.hooks.Load() {
		if h.OnClose != nil {
			m.safeHook(func() { h.OnClose(s) })
		}
	}
}

// SetState reports the state of a session; graphical modules (vnc, rdp) use it to drive their sessions.
// StateClosed closes the session.
func (m *Manager) SetState(id string, st model.SessionState, msg string) {
	s := m.Get(id)
	if s == nil {
		return
	}
	if st == model.StateClosed {
		m.closeSession(context.Background(), s, nil, "closed")
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.state, s.stateMsg = st, cleanMessage(msg)
	if st == model.StateConnected && s.connectedAt == nil {
		now := time.Now().UTC()
		s.connectedAt = &now
	}
	s.queueStateLocked()
	s.mu.Unlock()
	m.runStateHooks(s, st)
	m.publish(s)
}

// TrackClient counts a viewer of a (graphical) session so it shows in RuntimeSession.clients and is not reaped
// while attached. Call release when the viewer disconnects.
func (m *Manager) TrackClient(id string) (release func(), err error) {
	s := m.Get(id)
	if s == nil {
		return nil, model.ErrNotFound
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	s.extClients++
	s.detachedAt = time.Time{}
	s.mu.Unlock()
	m.publish(s)
	return sync.OnceFunc(func() {
		s.mu.Lock()
		s.extClients--
		if !s.closed && len(s.clients)+s.extClients == 0 {
			s.detachedAt = time.Now()
		}
		s.mu.Unlock()
		m.publish(s)
	}), nil
}

// ---- recording & logging ------------------------------------------------------------------------------------------

type finisher interface {
	Close() (int64, error)
}

// SetRecording starts or stops the asciicast recording of a session.
func (m *Manager) SetRecording(ctx context.Context, id string, enabled bool) error {
	s := m.Get(id)
	if s == nil {
		return model.ErrNotFound
	}
	if s.Kind != model.KindTerminal {
		return ErrUnsupported
	}
	if enabled {
		return m.startRecording(ctx, s)
	}
	s.mu.Lock()
	rec, meta := s.recorder, s.recording
	s.recorder, s.recording = nil, nil
	s.mu.Unlock()
	if rec != nil {
		m.finishRecording(rec, meta)
		m.audit(ctx, httpx.UserFromContext(ctx), "session.record.stop", s.ID, map[string]any{"recordingId": meta.ID})
		m.publish(s)
	}
	return nil
}

// SetLogging starts or stops the plain-text log of a session.
func (m *Manager) SetLogging(ctx context.Context, id string, enabled bool) error {
	s := m.Get(id)
	if s == nil {
		return model.ErrNotFound
	}
	if s.Kind != model.KindTerminal {
		return ErrUnsupported
	}
	if enabled {
		return m.startLogging(ctx, s)
	}
	s.mu.Lock()
	lg, meta := s.logger, s.logRec
	s.logger, s.logRec = nil, nil
	s.mu.Unlock()
	if lg != nil {
		lg.Note("--- logging stopped")
		m.finishRecording(lg, meta)
		m.audit(ctx, httpx.UserFromContext(ctx), "session.log.stop", s.ID, map[string]any{"recordingId": meta.ID})
		m.publish(s)
	}
	return nil
}

func (m *Manager) startRecording(ctx context.Context, s *Session) error {
	if m.d == nil || m.d.Cfg == nil || m.d.Store == nil {
		return errors.New("recording is not available")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	if s.recorder != nil {
		s.mu.Unlock()
		return nil
	}
	meta := m.newRecordingMeta(s, model.RecordingAsciicast)
	cols, rows, termType := s.cols, s.rows, s.termType
	conn := s.conn
	s.mu.Unlock()

	dir := m.d.Cfg.RecordingsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create recordings directory: %w", err)
	}
	meta.Path = filepath.Join(dir, meta.ID+".cast")
	rec, err := newCastRecorder(meta.Path, cols, rows, termType, meta.Title, m.flag(ctx, s, conn, "recordInput"))
	if err != nil {
		return fmt.Errorf("create recording file: %w", err)
	}
	if err := m.d.Store.Recordings.Create(ctx, meta); err != nil {
		rec.Close()
		os.Remove(meta.Path)
		return fmt.Errorf("save recording: %w", err)
	}
	s.mu.Lock()
	if s.closed || s.recorder != nil {
		s.mu.Unlock()
		m.finishRecording(rec, meta)
		return nil
	}
	s.recorder, s.recording = rec, meta
	s.mu.Unlock()
	m.audit(ctx, httpx.UserFromContext(ctx), "session.record.start", s.ID, map[string]any{"recordingId": meta.ID})
	m.publish(s)
	return nil
}

func (m *Manager) startLogging(ctx context.Context, s *Session) error {
	if m.d == nil || m.d.Cfg == nil || m.d.Store == nil {
		return errors.New("logging is not available")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	if s.logger != nil {
		s.mu.Unlock()
		return nil
	}
	meta := m.newRecordingMeta(s, model.RecordingLog)
	conn := s.conn
	header := fmt.Sprintf("=== Termstead session log: %s | %s | session %s | started %s ===", meta.Title,
		describeTarget(conn), s.ID, time.Now().Format(logTimeFormat))
	s.mu.Unlock()

	dir := m.d.Cfg.LogsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create logs directory: %w", err)
	}
	name := fmt.Sprintf("%s_%s_%s.log", time.Now().Format("2006-01-02"), safeFileName(meta.Title), meta.ID)
	meta.Path = filepath.Join(dir, name)
	lg, err := newTextLogger(meta.Path, header, m.flag(ctx, s, conn, "logTimestamps"))
	if err != nil {
		return fmt.Errorf("create log file: %w", err)
	}
	if err := m.d.Store.Recordings.Create(ctx, meta); err != nil {
		lg.Close()
		os.Remove(meta.Path)
		return fmt.Errorf("save log: %w", err)
	}
	s.mu.Lock()
	if s.closed || s.logger != nil {
		s.mu.Unlock()
		m.finishRecording(lg, meta)
		return nil
	}
	s.logger, s.logRec = lg, meta
	s.mu.Unlock()
	m.audit(ctx, httpx.UserFromContext(ctx), "session.log.start", s.ID, map[string]any{"recordingId": meta.ID})
	m.publish(s)
	return nil
}

// newRecordingMeta builds the recordings row for s. s.mu held.
func (m *Manager) newRecordingMeta(s *Session, kind string) *model.Recording {
	meta := &model.Recording{
		ID:        model.NewID(),
		OwnerID:   s.OwnerID,
		SessionID: s.ID,
		Title:     s.title,
		Kind:      kind,
		Cols:      s.cols,
		Rows:      s.rows,
		StartedAt: store.Now(),
	}
	if s.conn != nil && !s.quick {
		meta.ConnectionID = s.conn.ID
	}
	return meta
}

func (m *Manager) finishRecording(f finisher, meta *model.Recording) {
	if f == nil || meta == nil {
		return
	}
	size, err := f.Close()
	if err != nil {
		m.log.Warn("term: closing recording failed", "recording", meta.ID, "err", err)
	}
	now := store.Now()
	meta.Size, meta.EndedAt = size, &now
	if m.d == nil || m.d.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.d.Store.Recordings.Update(ctx, meta); err != nil && !errors.Is(err, model.ErrNotFound) {
		m.log.Debug("term: updating recording failed", "recording", meta.ID, "err", err)
	}
}

func describeTarget(c *model.Connection) string {
	if c == nil {
		return ""
	}
	if c.Host == "" {
		return string(c.Protocol)
	}
	t := c.Host
	if c.Username != "" {
		t = c.Username + "@" + t
	}
	if c.Port != 0 && c.Port != model.DefaultPort(c.Protocol) {
		t = fmt.Sprintf("%s:%d", t, c.Port)
	}
	return string(c.Protocol) + "://" + t
}

// safeFileName keeps a short, portable file name component.
func safeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 48 {
			break
		}
	}
	out := strings.Trim(b.String(), "._")
	if out == "" {
		out = "session"
	}
	return out
}

// flag resolves a boolean session option: connection option, else the owner's "terminal" settings section, else
// the global one.
func (m *Manager) flag(ctx context.Context, s *Session, conn *model.Connection, key string) bool {
	if conn != nil && conn.Options.Has(key) {
		return conn.Options.Bool(key)
	}
	if m.d == nil || m.d.Store == nil {
		return false
	}
	for _, scope := range []string{s.OwnerID, store.ScopeGlobal} {
		var sec map[string]any
		ok, err := m.d.Store.Settings.GetJSON(ctx, scope, "terminal", &sec)
		if err != nil || !ok {
			continue
		}
		if v, present := sec[key]; present {
			if b, isBool := v.(bool); isBool {
				return b
			}
		}
	}
	return false
}

// ---- events, audit, hooks -----------------------------------------------------------------------------------------

type sessionUpdatedEvent struct {
	Type    string               `json:"type"`
	Session model.RuntimeSession `json:"session"`
}

type sessionClosedEvent struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// publishInterval throttles session.updated to at most 4 events per second per session.
const publishInterval = 250 * time.Millisecond

// publish emits session.updated for s, coalescing bursts (≤ 4/s). Never call it with s.mu held.
func (m *Manager) publish(s *Session) {
	if m.d == nil || m.d.Events == nil {
		return
	}
	s.pubMu.Lock()
	if s.pubTimer != nil {
		s.pubMu.Unlock()
		return // a flush is already scheduled and will carry the latest state
	}
	wait := publishInterval - time.Since(s.lastPub)
	if wait > 0 {
		s.pubTimer = time.AfterFunc(wait, func() {
			s.pubMu.Lock()
			s.pubTimer = nil
			s.pubMu.Unlock()
			m.sendUpdate(s)
		})
		s.pubMu.Unlock()
		return
	}
	s.pubMu.Unlock()
	m.sendUpdate(s)
}

func (m *Manager) sendUpdate(s *Session) {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	s.mu.Lock()
	closed := s.closed
	info := s.infoLocked()
	s.mu.Unlock()
	if closed {
		return // session.closed was (or is about to be) published instead
	}
	s.lastPub = time.Now()
	m.d.Events.Publish(s.OwnerID, sessionUpdatedEvent{Type: model.EvSessionUpdated, Session: info})
}

func (m *Manager) audit(ctx context.Context, user *model.User, action, target string, details any) {
	if m.d == nil || m.d.Audit == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.d.Audit.LogUser(ctx, user, action, target, details)
}

// AddHooks registers session observers; call remove to unregister.
func (m *Manager) AddHooks(h Hooks) (remove func()) {
	m.hooksMu.Lock()
	m.hookSeq++
	id := m.hookSeq
	hh := h
	m.hookMap[id] = &hh
	m.rebuildHooksLocked()
	m.hooksMu.Unlock()
	return sync.OnceFunc(func() {
		m.hooksMu.Lock()
		delete(m.hookMap, id)
		m.rebuildHooksLocked()
		m.hooksMu.Unlock()
	})
}

func (m *Manager) rebuildHooksLocked() {
	ids := make([]uint64, 0, len(m.hookMap))
	for id := range m.hookMap {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	list := make([]*Hooks, 0, len(ids))
	for _, id := range ids {
		list = append(list, m.hookMap[id])
	}
	m.hooks.Store(&list)
}

func (m *Manager) safeHook(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			m.log.Error("term: session hook panicked", "panic", r)
		}
	}()
	fn()
}

func (m *Manager) runOutputHooks(s *Session, data []byte) {
	for _, h := range *m.hooks.Load() {
		if h.OnOutput != nil {
			m.safeHook(func() { h.OnOutput(s, data) })
		}
	}
}

func (m *Manager) runInputHooks(s *Session, data []byte, sensitive bool) {
	var masked []byte
	if sensitive {
		masked = SensitiveMask(data)
	}
	for _, h := range *m.hooks.Load() {
		switch {
		case sensitive && h.OnSensitiveInput != nil:
			m.safeHook(func() { h.OnSensitiveInput(s, masked) })
		case sensitive && h.OnInput != nil:
			m.safeHook(func() { h.OnInput(s, masked) })
		case h.OnInput != nil:
			m.safeHook(func() { h.OnInput(s, data) })
		}
	}
}

// claimStartup asks the startup handler whether it sends the startup command itself (a panic counts as "no").
func (m *Manager) claimStartup(h StartupHandler, s *Session, conn *model.Connection, send func()) (claimed bool) {
	m.safeHook(func() { claimed = h(s, conn, send) })
	return claimed
}

func (m *Manager) runStateHooks(s *Session, st model.SessionState) {
	for _, h := range *m.hooks.Load() {
		if h.OnState != nil {
			m.safeHook(func() { h.OnState(s, st) })
		}
	}
}
