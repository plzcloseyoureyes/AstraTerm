// Package automation is AstraTerm's automation module (SPEC §10.3 "automation"; RESEARCH AUTO-1, AUTO-4..12, TERM-15,
// TERM-17 pacing, TERM-33, SEC-21):
//
//   - snippets & macros REST (core tables), server-side snippet runs, macro replays (with timing) and paced sends
//   - logon actions (connection.options.logonActions: expect/send steps run after every (re)connect)
//   - secret injection (POST /api/sessions/{id}/inject-secret) — stored secrets typed into a session, never sent to
//     the browser
//   - triggers evaluated on the ANSI-stripped output of every session of their owner, with or without a browser
//   - JavaScript scripting (goja) with session.expect(), batch runs across connections, cron schedules, run history
//   - the dangerous-command guard enforced on every server-side send
//
// The browser side lives in web/src/features/automation.
package automation

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/core"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// Module holds the automation runtime.
type Module struct {
	d    *app.Deps
	c    *core.Core
	ctx  context.Context
	log  *slog.Logger
	repo *repo

	taps     *tapHub
	logon    *logonManager
	triggers *triggerEngine
	sched    *scheduler
	scripts  *scriptLimiter
	batches  *scriptLimiter
}

// Mount registers the module's routes, session hooks and scheduler (SPEC §10.1).
func Mount(d *app.Deps, c *core.Core) error {
	m, err := newModule(d, c)
	if err != nil {
		return err
	}
	m.routes()
	m.start()
	return nil
}

func newModule(d *app.Deps, c *core.Core) (*Module, error) {
	ctx := d.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	// The module's tables are needed during Mount (scheduler, trigger cache).
	if err := d.Store.Migrate(ctx); err != nil {
		return nil, err
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	m := &Module{
		d:       d,
		c:       c,
		ctx:     ctx,
		log:     log.With("module", "automation"),
		repo:    &repo{db: d.Store.DB},
		taps:    newTapHub(),
		scripts: newScriptLimiter(),
		batches: newCountLimiter(perUserBatches, totalBatches),
	}
	m.logon = newLogonManager(m)
	m.triggers = newTriggerEngine(m)
	m.sched = newScheduler(m)
	return m, nil
}

func (m *Module) start() {
	if err := m.repo.interruptRuns(m.ctx); err != nil {
		m.log.Warn("cannot mark interrupted runs", "err", err)
	}
	if err := m.triggers.loadAll(m.ctx); err != nil {
		m.log.Warn("cannot load triggers", "err", err)
	}
	if m.c != nil && m.c.Sessions != nil {
		remove := m.c.Sessions.AddHooks(term.Hooks{
			OnOutput: m.onOutput,
			OnState:  m.onState,
			OnClose:  m.onClose,
		})
		// Connections with logon actions get their start-up command after the last step.
		m.c.Sessions.SetStartupHandler(m.logon.claimStartup)
		go func() {
			<-m.ctx.Done()
			remove()
			m.c.Sessions.SetStartupHandler(nil)
		}()
	}
	m.sched.start()
}

func (m *Module) routes() {
	api := m.d.Router.API()

	// snippets & macros (SPEC §6.0)
	api.GET("/snippets", m.listSnippets)
	api.POST("/snippets", m.createSnippet)
	api.GET("/snippets/:id", m.getSnippet)
	api.PATCH("/snippets/:id", m.patchSnippet)
	api.DELETE("/snippets/:id", m.deleteSnippet)
	api.POST("/snippets/:id/run", m.runSnippet)
	api.GET("/macros", m.listMacros)
	api.POST("/macros", m.createMacro)
	api.GET("/macros/:id", m.getMacro)
	api.PATCH("/macros/:id", m.patchMacro)
	api.DELETE("/macros/:id", m.deleteMacro)
	api.POST("/macros/:id/run", m.runMacro)

	// secret injection (AUTO-9, TERM-33)
	api.POST("/sessions/:id/inject-secret", m.injectSecret)
	api.GET("/sessions/:id/secret-keys", m.secretKeys)

	// paced sends (TERM-17 / AUTO-6), guard check, helpers
	api.POST("/automation/send", m.pacedSend)
	api.POST("/automation/guard/check", m.guardCheck)
	api.POST("/automation/regex/test", m.regexTest)
	api.POST("/automation/template/vars", m.templateVars)
	api.GET("/automation/capabilities", m.capabilities)

	// triggers (AUTO-7)
	api.GET("/automation/triggers", m.listTriggers)
	api.POST("/automation/triggers", m.createTrigger)
	api.PATCH("/automation/triggers/:id", m.patchTrigger)
	api.DELETE("/automation/triggers/:id", m.deleteTrigger)
	api.GET("/automation/trigger-log", m.listTriggerLog)
	api.DELETE("/automation/trigger-log", m.clearTriggerLog)

	// scripts (AUTO-10)
	api.GET("/scripts", m.listScripts)
	api.POST("/scripts", m.createScript)
	api.POST("/scripts/run", m.runAdhocScript)
	api.GET("/scripts/:id", m.getScript)
	api.PATCH("/scripts/:id", m.patchScript)
	api.DELETE("/scripts/:id", m.deleteScript)
	api.POST("/scripts/:id/run", m.runScript)

	// batch runs (AUTO-11), schedules (AUTO-12), history
	api.POST("/automation/batch", m.startBatch)
	api.GET("/automation/schedules", m.listSchedules)
	api.POST("/automation/schedules", m.createSchedule)
	api.GET("/automation/schedules/preview", m.previewSchedule)
	api.PATCH("/automation/schedules/:id", m.patchSchedule)
	api.DELETE("/automation/schedules/:id", m.deleteSchedule)
	api.POST("/automation/schedules/:id/run", m.runScheduleNow)
	api.GET("/automation/runs", m.listRuns)
	api.DELETE("/automation/runs", m.clearRuns)
	api.GET("/automation/runs/:id", m.getRun)
	api.DELETE("/automation/runs/:id", m.deleteRun)
}

// ---- session hooks ------------------------------------------------------------------------------------------------

func (m *Module) onOutput(s *term.Session, data []byte) {
	t := m.taps.get(s.ID)
	if t == nil {
		return
	}
	_, head := s.Offsets()
	t.push(head-int64(len(data)), data)
}

func (m *Module) onState(s *term.Session, st model.SessionState) {
	if s.Kind != model.KindTerminal {
		return
	}
	if t := m.taps.get(s.ID); t != nil {
		t.setState(st)
	}
	if st == model.StateConnected {
		// Triggers first: they take their tap reference before the logon capture may release its own.
		m.triggers.onState(s, st)
		m.logon.onState(s, st)
		return
	}
	m.logon.onState(s, st)
	m.triggers.onState(s, st)
}

func (m *Module) onClose(s *term.Session) {
	if t := m.taps.get(s.ID); t != nil {
		t.setState(model.StateClosed)
	}
	m.logon.onClose(s)
	m.triggers.onClose(s)
}

// ---- helpers ------------------------------------------------------------------------------------------------------

// sessions returns the runtime session manager (nil in unit tests without one).
func (m *Module) sessions() *term.Manager {
	if m.c == nil {
		return nil
	}
	return m.c.Sessions
}

// ownSession returns a live session owned by user; other users' sessions are reported as not found.
func (m *Module) ownSession(user *model.User, id string) (*term.Session, error) {
	mgr := m.sessions()
	if mgr == nil || user == nil || !model.ValidID(id) {
		return nil, httpx.NotFound("session not found")
	}
	s := mgr.Get(id)
	if s == nil || s.OwnerID != user.ID {
		return nil, httpx.NotFound("session not found")
	}
	return s, nil
}

// acquireTap returns the session's output tap (created and seeded on demand).
func (m *Module) acquireTap(s *term.Session) *tap {
	st, _ := s.State()
	return m.taps.acquire(s.ID, s, st)
}

// write types data into a session.
func (m *Module) write(id, data string) error {
	mgr := m.sessions()
	if mgr == nil {
		return httpx.NotFound("session not found")
	}
	return mgr.Write(id, []byte(data))
}

// writeSecret types a stored secret (and Enter when enter is set) into a session through term's WriteSensitive: the
// plaintext never reaches the asciicast input track, and input hooks (command audit, …) only see a mask.
func (m *Module) writeSecret(id, secret string, enter bool) error {
	mgr := m.sessions()
	if mgr == nil {
		return httpx.NotFound("session not found")
	}
	if enter {
		secret += "\r"
	}
	return mgr.WriteSensitive(id, []byte(secret))
}

// publish sends an event to every socket of a user.
func (m *Module) publish(userID string, ev any) {
	if m.d.Events != nil {
		m.d.Events.Publish(userID, ev)
	}
}

func (m *Module) notify(userID, level, title, message string) {
	m.publish(userID, model.Notify(level, title, message))
}

func (m *Module) audit(ctx any, user *model.User, action, target string, details any) {
	if m.d.Audit == nil {
		return
	}
	if user != nil {
		m.d.Audit.LogUser(ctx, user, action, target, details)
		return
	}
	m.d.Audit.Log(ctx, action, target, details)
}

// isServerMode reports whether AstraTerm runs in multi-user server mode.
func (m *Module) isServerMode() bool { return m.d.Cfg != nil && m.d.Cfg.IsServer() }

// globalAutomationSettings returns the admin's global "automation" settings section.
func (m *Module) globalAutomationSettings(ctx context.Context) map[string]json.RawMessage {
	var sec map[string]json.RawMessage
	if ok, err := m.d.Store.Settings.GetJSON(ctx, store.ScopeGlobal, "automation", &sec); err != nil || !ok {
		return nil
	}
	return sec
}

// scriptsAllowed: scripts run inside the AstraTerm process, so in server mode they are admin-only unless an admin
// enabled them for everyone (global settings automation.userScripts = true). Desktop mode allows them.
func (m *Module) scriptsAllowed(ctx context.Context, user *model.User) bool {
	if user == nil {
		return false
	}
	if !m.isServerMode() || user.IsAdmin() {
		return true
	}
	var on bool
	if raw, ok := m.globalAutomationSettings(ctx)["userScripts"]; ok && json.Unmarshal(raw, &on) == nil {
		return on
	}
	return false
}

var errScriptsForbidden = httpx.Forbidden("scripts are restricted to administrators on this server")

// secretAccess reports whether user may have stored secrets of the session's connection typed into it: always for
// quick-connect sessions (the user supplied them), otherwise only for the connection's owner or an administrator —
// a shared connection's secrets are used server-side but must never become readable by other users.
func secretAccess(user *model.User, s *term.Session) bool {
	if user == nil || s == nil {
		return false
	}
	info := s.Info()
	if info.ConnectionID == "" {
		return true
	}
	conn := s.Connection()
	return conn != nil && app.CanModify(user, conn.OwnerID)
}

// uniqueIDs de-duplicates and validates a list of IDs.
func uniqueIDs(ids []string, max int, what string) ([]string, error) {
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		if !model.ValidID(id) {
			return nil, httpx.BadRequest("invalid " + what + " id")
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, httpx.BadRequest("no " + what + "s given")
	}
	if len(out) > max {
		return nil, httpx.BadRequest("too many " + what + "s")
	}
	return out, nil
}

// errorText renders an error for a JSON result without leaking internals.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	var he *httpx.HTTPError
	if errors.As(err, &he) {
		if he.Status >= 500 {
			return "internal error"
		}
		return he.Message
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	msg := err.Error()
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	return msg
}

type capabilitiesResponse struct {
	Scripts        bool   `json:"scripts"`
	Mode           string `json:"mode"`
	MaxTargets     int    `json:"maxTargets"`
	MaxParallel    int    `json:"maxParallel"`
	DefaultTimeout int    `json:"defaultTimeoutSec"`
}

func (m *Module) capabilities(c *echo.Context) error {
	mode := model.ModeDesktop
	if m.isServerMode() {
		mode = model.ModeServer
	}
	return c.JSON(http.StatusOK, capabilitiesResponse{
		Scripts:        m.scriptsAllowed(c.Request().Context(), httpx.UserFrom(c)),
		Mode:           mode,
		MaxTargets:     maxTargets,
		MaxParallel:    maxBatchParallel,
		DefaultTimeout: int(defaultHostTimeout / time.Second),
	})
}
