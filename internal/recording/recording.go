// Package recording implements Termstead's recording & sharing module (RESEARCH REC-1, REC-2, REC-4 session events,
// REC-5, REC-7, REC-9, MU-18, MU-19, TERM-30):
//
//   - recordings browser REST over the core `recordings` table the terminal manager fills (asciicast v3 casts and
//     text logs): list with filters, metadata, file download (v3, on-the-fly v2 conversion, plain-text transcript),
//     search inside one recording or across recordings, delete / bulk delete, usage (recordings.go, search.go);
//   - retention policy (global settings section `recording`: maxAgeDays / maxTotalMB) with a periodic cleanup
//     (retention.go);
//   - instant replay of any terminal session from its scrollback plus a per-session timing index kept by term hooks
//     (replay.go);
//   - command audit: one `session.command` audit entry per executed command, from OSC 133 shell-integration marks or,
//     without them, from echo-confirmed input line reconstruction (cmdaudit.go);
//   - live session share links (read-only / interactive), a public share endpoint and WebSocket (share.go);
//   - admin live session monitoring: all sessions with owners, terminate, message (admin.go); shadowing reuses the
//     terminal WebSocket's read-only admin attach;
//   - an in-memory debug log ring behind GET /api/admin/logs (debuglog.go).
package recording

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/core"
	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/store"
	"github.com/termstead/termstead/internal/term"
)

// settingsKey is the global settings section of this module (admin policy; read from the global scope only).
const settingsKey = "recording"

// Policy is the administrator's module policy (global settings section "recording").
type Policy struct {
	// MaxAgeDays deletes finished recordings older than this many days (0 = keep forever).
	MaxAgeDays int `json:"maxAgeDays"`
	// MaxTotalMB deletes the oldest finished recordings while all recordings together use more (0 = unlimited).
	MaxTotalMB int `json:"maxTotalMB"`
	// CommandAudit writes one session.command audit entry per executed command. Default: on in server mode, off in
	// desktop mode.
	CommandAudit bool `json:"commandAudit"`
	// ShareEnabled allows live session share links; ShareWriteEnabled also interactive ones.
	ShareEnabled      bool `json:"shareEnabled"`
	ShareWriteEnabled bool `json:"shareWriteEnabled"`
	// ShareMaxHours bounds the lifetime of a share link (1…720, default 168 = 7 days).
	ShareMaxHours int `json:"shareMaxHours"`
	// DebugLog enables the in-memory debug log capture; DebugLogLevel is its threshold (debug|info|warn|error).
	DebugLog      bool   `json:"debugLog"`
	DebugLogLevel string `json:"debugLogLevel"`
}

func defaultPolicy(serverMode bool) Policy {
	return Policy{CommandAudit: serverMode, ShareEnabled: true, ShareWriteEnabled: true, ShareMaxHours: 168,
		DebugLog: true, DebugLogLevel: "info"}
}

// normalize clamps values into their valid ranges.
func (p *Policy) normalize() {
	p.MaxAgeDays = min(max(p.MaxAgeDays, 0), 36500)
	p.MaxTotalMB = min(max(p.MaxTotalMB, 0), 1<<24)
	if p.ShareMaxHours <= 0 {
		p.ShareMaxHours = 168
	}
	p.ShareMaxHours = min(p.ShareMaxHours, 720)
	if _, ok := parseLevel(p.DebugLogLevel); !ok {
		p.DebugLogLevel = "info"
	}
}

// Service is the module instance created by Mount.
type Service struct {
	d        *app.Deps
	sessions *term.Manager
	log      *slog.Logger

	polMu     sync.RWMutex
	pol       Policy
	polLoaded time.Time

	states *sessionStates
	audit  *commandAuditor
	shares *shareHub
	limits *ipLimiter
	now    func() time.Time
}

// Mount registers the module's routes, hooks, migrations and background loops.
func Mount(d *app.Deps, c *core.Core) error {
	if d == nil || c == nil || c.Sessions == nil {
		return errors.New("recording: missing dependencies")
	}
	_, err := mount(d, c.Sessions)
	return err
}

func mount(d *app.Deps, sessions *term.Manager) (*Service, error) {
	installDebugCapture(d)
	s := &Service{
		d:        d,
		sessions: sessions,
		log:      d.Log.With("module", "recording"),
		states:   newSessionStates(),
		limits:   newIPLimiter(),
		now:      time.Now,
	}
	s.pol = defaultPolicy(d.Cfg != nil && d.Cfg.IsServer())
	// The share meta table is used right away (startup pruning).
	mctx, cancel := context.WithTimeout(d.Ctx, 30*time.Second)
	defer cancel()
	if err := d.Store.Migrate(mctx); err != nil {
		return nil, err
	}
	s.refreshPolicy(mctx)
	s.audit = newCommandAuditor(s)
	s.shares = newShareHub(s)
	s.shares.pruneDead(mctx)

	sessions.AddHooks(term.Hooks{
		OnOutput:         s.onOutput,
		OnInput:          s.onInput,
		OnSensitiveInput: s.onSensitiveInput,
		OnState:          s.onState,
		OnClose:          s.onClose,
	})
	s.routes()
	go s.audit.run(d.Ctx)
	go s.loop(d.Ctx)
	return s, nil
}

func (s *Service) routes() {
	api, admin, public := s.d.Router.API(), s.d.Router.Admin(), s.d.Router.Public()

	api.GET("/recordings", s.handleList)
	api.GET("/recordings/usage", s.handleUsage)
	api.GET("/recordings/policy", s.handleGetPolicy)
	api.GET("/recordings/search", s.handleSearchAll)
	api.POST("/recordings/bulk-delete", s.handleBulkDelete)
	api.GET("/recordings/:id", s.handleGet)
	api.GET("/recordings/:id/file", s.handleFile)
	api.GET("/recordings/:id/search", s.handleSearch)
	api.DELETE("/recordings/:id", s.handleDelete)
	admin.PUT("/admin/recordings/policy", s.handlePutPolicy)
	admin.POST("/admin/recordings/cleanup", s.handleCleanup)

	api.GET("/sessions/:id/replay", s.handleReplay)
	api.GET("/sessions/:id/commands", s.handleSessionCommands)

	api.POST("/sessions/:id/share", s.handleCreateShare)
	api.GET("/sessions/:id/shares", s.handleListShares)
	api.DELETE("/sessions/:id/shares", s.handleRevokeSessionShares)
	api.GET("/shares", s.handleListMyShares)
	api.DELETE("/shares/:id", s.handleRevokeShare)
	api.PUT("/shares/:id/input", s.handleShareInput)
	public.GET("/share/:token", s.handleShareInfo)
	s.d.Router.PublicWS("/ws/share/:token", s.handleShareWS)

	admin.GET("/admin/sessions", s.handleAdminSessions)
	admin.POST("/admin/sessions/:id/terminate", s.handleAdminTerminate)
	admin.POST("/admin/sessions/:id/message", s.handleAdminMessage)

	admin.GET("/admin/logs", s.handleLogs)
	admin.GET("/admin/logs/export", s.handleLogsExport)
	admin.DELETE("/admin/logs", s.handleLogsClear)
}

// loop runs the periodic maintenance: policy refresh, retention, share pruning and limiter cleanup.
func (s *Service) loop(ctx context.Context) {
	policyTick := time.NewTicker(15 * time.Second)
	defer policyTick.Stop()
	maint := time.NewTicker(10 * time.Minute)
	defer maint.Stop()
	first := time.NewTimer(45 * time.Second)
	defer first.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-policyTick.C:
			s.refreshPolicy(ctx)
			s.limits.gc(s.now())
		case <-first.C:
			s.maintenance(ctx)
		case <-maint.C:
			s.maintenance(ctx)
		}
	}
}

func (s *Service) maintenance(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if _, err := s.applyRetention(ctx, false); err != nil && ctx.Err() == nil {
		s.log.Warn("retention cleanup failed", "err", err)
	}
	s.shares.pruneDead(ctx)
}

// policy returns the cached administrator policy.
func (s *Service) policy() Policy {
	s.polMu.RLock()
	defer s.polMu.RUnlock()
	return s.pol
}

// refreshPolicy re-reads the global settings section (defaults for missing keys).
func (s *Service) refreshPolicy(ctx context.Context) {
	p, err := s.loadPolicy(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Debug("reading the recording policy failed", "err", err)
		}
		return
	}
	s.polMu.Lock()
	s.pol, s.polLoaded = p, s.now()
	s.polMu.Unlock()
	applyCaptureLevel(p)
}

func (s *Service) loadPolicy(ctx context.Context) (Policy, error) {
	p := defaultPolicy(s.d.Cfg != nil && s.d.Cfg.IsServer())
	if _, err := s.d.Store.Settings.GetJSON(ctx, store.ScopeGlobal, settingsKey, &p); err != nil {
		return p, err
	}
	p.normalize()
	return p, nil
}

// ---- policy endpoints ---------------------------------------------------------------------------------------------

type policyView struct {
	Policy
	Mode string `json:"mode"`
}

func (s *Service) handleGetPolicy(c *echo.Context) error {
	s.refreshPolicy(c.Request().Context())
	mode := "desktop"
	if s.d.Cfg != nil && s.d.Cfg.IsServer() {
		mode = "server"
	}
	return c.JSON(http.StatusOK, policyView{Policy: s.policy(), Mode: mode})
}

// handlePutPolicy merges the given keys into the global policy (admin), validates and stores it.
func (s *Service) handlePutPolicy(c *echo.Context) error {
	var patch struct {
		MaxAgeDays        *int    `json:"maxAgeDays"`
		MaxTotalMB        *int    `json:"maxTotalMB"`
		CommandAudit      *bool   `json:"commandAudit"`
		ShareEnabled      *bool   `json:"shareEnabled"`
		ShareWriteEnabled *bool   `json:"shareWriteEnabled"`
		ShareMaxHours     *int    `json:"shareMaxHours"`
		DebugLog          *bool   `json:"debugLog"`
		DebugLogLevel     *string `json:"debugLogLevel"`
	}
	if err := httpx.Bind(c, &patch); err != nil {
		return err
	}
	ctx := c.Request().Context()
	p, err := s.loadPolicy(ctx)
	if err != nil {
		return err
	}
	changed := map[string]any{}
	if v := patch.MaxAgeDays; v != nil {
		if *v < 0 || *v > 36500 {
			return httpx.BadRequest("maxAgeDays must be between 0 and 36500")
		}
		p.MaxAgeDays, changed["maxAgeDays"] = *v, *v
	}
	if v := patch.MaxTotalMB; v != nil {
		if *v < 0 || *v > 1<<24 {
			return httpx.BadRequest("maxTotalMB must be between 0 and 16777216")
		}
		p.MaxTotalMB, changed["maxTotalMB"] = *v, *v
	}
	if v := patch.CommandAudit; v != nil {
		p.CommandAudit, changed["commandAudit"] = *v, *v
	}
	if v := patch.ShareEnabled; v != nil {
		p.ShareEnabled, changed["shareEnabled"] = *v, *v
	}
	if v := patch.ShareWriteEnabled; v != nil {
		p.ShareWriteEnabled, changed["shareWriteEnabled"] = *v, *v
	}
	if v := patch.ShareMaxHours; v != nil {
		if *v < 1 || *v > 720 {
			return httpx.BadRequest("shareMaxHours must be between 1 and 720")
		}
		p.ShareMaxHours, changed["shareMaxHours"] = *v, *v
	}
	if v := patch.DebugLog; v != nil {
		p.DebugLog, changed["debugLog"] = *v, *v
	}
	if v := patch.DebugLogLevel; v != nil {
		if _, ok := parseLevel(*v); !ok {
			return httpx.BadRequest("debugLogLevel must be debug, info, warn or error")
		}
		p.DebugLogLevel, changed["debugLogLevel"] = *v, *v
	}
	p.normalize()
	if err := s.d.Store.Settings.SetJSON(ctx, store.ScopeGlobal, settingsKey, p); err != nil {
		return err
	}
	s.polMu.Lock()
	s.pol, s.polLoaded = p, s.now()
	s.polMu.Unlock()
	applyCaptureLevel(p)
	s.d.Audit.Log(c, "recording.policy.update", settingsKey, changed)
	if !p.ShareEnabled || !p.ShareWriteEnabled {
		s.shares.enforcePolicy(ctx, p)
	}
	return c.JSON(http.StatusOK, policyView{Policy: p, Mode: modeOf(s.d)})
}

func modeOf(d *app.Deps) string {
	if d.Cfg != nil && d.Cfg.IsServer() {
		return "server"
	}
	return "desktop"
}

// ---- hooks --------------------------------------------------------------------------------------------------------

func (s *Service) onOutput(sess *term.Session, data []byte) {
	if sess.Kind != model.KindTerminal || len(data) == 0 {
		return
	}
	st := s.states.get(sess, true)
	st.output(sess, data, s.now(), s.audit)
}

func (s *Service) onInput(sess *term.Session, data []byte) {
	if sess.Kind != model.KindTerminal || len(data) == 0 {
		return
	}
	st := s.states.get(sess, true)
	st.input(sess, data, s.now(), s.audit, false)
}

// onSensitiveInput sees injected secrets (term.Manager.WriteSensitive) as a same-length mask: they count as input,
// and the line they are typed on is never command-audited.
func (s *Service) onSensitiveInput(sess *term.Session, masked []byte) {
	if sess.Kind != model.KindTerminal || len(masked) == 0 {
		return
	}
	st := s.states.get(sess, true)
	st.input(sess, masked, s.now(), s.audit, true)
}

func (s *Service) onState(sess *term.Session, st model.SessionState) {
	if sess.Kind != model.KindTerminal {
		return
	}
	if st == model.StateConnecting {
		if ss := s.states.get(sess, false); ss != nil {
			ss.resetCommands()
		}
	}
}

func (s *Service) onClose(sess *term.Session) {
	s.states.drop(sess.ID)
	s.shares.sessionClosed(sess.ID)
}

// ---- access helpers -----------------------------------------------------------------------------------------------

// sessionFor returns a live session the caller owns (owner=true) or may view (owner or admin).
func (s *Service) sessionFor(c *echo.Context, ownerOnly bool) (*term.Session, *model.User, error) {
	u := httpx.UserFrom(c)
	if u == nil {
		return nil, nil, httpx.ErrUnauthorized
	}
	id := c.Param("id")
	if !model.ValidID(id) {
		return nil, nil, httpx.ErrNotFound
	}
	sess := s.sessions.Get(id)
	if sess == nil || sess.Closed() {
		return nil, nil, httpx.ErrNotFound
	}
	if sess.OwnerID == u.ID {
		return sess, u, nil
	}
	if u.IsAdmin() {
		if ownerOnly {
			return nil, nil, httpx.Forbidden("only the session owner can do this")
		}
		return sess, u, nil
	}
	return nil, nil, httpx.ErrNotFound
}
