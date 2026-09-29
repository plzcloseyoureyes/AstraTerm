package automation

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dop251/goja"
	"github.com/labstack/echo/v5"

	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
)

// Scripts (AUTO-10): JavaScript (goja, ES5.1+ with most of ES2015+) executed inside the backend, sandboxed — no file
// system, network, timers or modules; only the API below. A run is a job (live log as job events, cancellable via
// POST /api/jobs/{id}/cancel) with a timeout, recorded in the run history.
//
//	session                     the bound session (run against a session / connection), else null
//	  .id .title .host .user .protocol .connectionId
//	  .send(text)               type raw text ("\r" = Enter)
//	  .sendLine(text)           text + Enter
//	  .sendSecret(key[, {enter}]) type a stored secret of the connection (never visible to the script)
//	  .expect(pattern|[patterns], timeoutMs=30000) → {index, match, groups, before}; throws TimeoutError
//	  .waitFor(pattern|[patterns], timeoutMs=30000) → same, or null on timeout
//	  .waitIdle(idleMs=500, maxMs=30000)
//	  .waitPrompt(timeoutMs=30000[, pattern]) → boolean
//	  .run(command[, {timeout, prompt}]) → output of the command (sendLine + wait for the prompt)
//	  .screen(lines=24) → the last lines of output (ANSI-stripped)
//	  .exec(command[, timeoutMs]) → {stdout, stderr, code} (SSH sessions: separate exec channel)
//	  .close()
//	sessions.open(connectionIdOrName[, {timeout, keepOpen}]) · sessions.get(sessionId) · sessions.list()
//	connections.list()           saved connections visible to the user
//	vars / variables             run variables (object of strings)
//	log(...) console.log/info/warn/error/debug(...)
//	sleep(ms) · prompt(label[, {secret, default}]) → string|null · confirm(message) → boolean · exit([code])
//
// Patterns are JavaScript RegExp literals or strings, compiled with Go's RE2 (no look-arounds / back-references).

const (
	defaultScriptTimeout = 10 * time.Minute
	maxScriptTimeout     = 24 * time.Hour
	defaultExpectTimeout = 30 * time.Second
	maxScriptSessions    = 32
	perUserScripts       = 8
	totalScripts         = 64
)

// ---- concurrency limits -------------------------------------------------------------------------------------------

type scriptLimiter struct {
	mu         sync.Mutex
	perUser    map[string]int
	total      int
	maxPerUser int
	maxTotal   int
}

func newScriptLimiter() *scriptLimiter { return newCountLimiter(perUserScripts, totalScripts) }

// newCountLimiter bounds concurrent work per user and in total (scripts, batch runs).
func newCountLimiter(perUser, total int) *scriptLimiter {
	return &scriptLimiter{perUser: map[string]int{}, maxPerUser: perUser, maxTotal: total}
}

func (l *scriptLimiter) acquire(user string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= l.maxTotal || l.perUser[user] >= l.maxPerUser {
		return false
	}
	l.total++
	l.perUser[user]++
	return true
}

func (l *scriptLimiter) release(user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	if l.perUser[user]--; l.perUser[user] <= 0 {
		delete(l.perUser, user)
	}
}

// ---- REST ---------------------------------------------------------------------------------------------------------

type scriptInput struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Content     *string `json:"content"`
}

func (in *scriptInput) apply(s *Script) error {
	if in.Name != nil {
		n, err := cleanName(*in.Name, "name")
		if err != nil {
			return err
		}
		s.Name = n
	}
	if in.Description != nil {
		if utf8.RuneCountInString(*in.Description) > maxDescriptionLen {
			return httpx.BadRequest("description is too long")
		}
		s.Description = *in.Description
	}
	if in.Content != nil {
		if len(*in.Content) > maxScriptBytes {
			return httpx.BadRequest("the script is too large")
		}
		if !utf8.ValidString(*in.Content) {
			return httpx.BadRequest("the script must be valid UTF-8")
		}
		s.Content = *in.Content
	}
	return nil
}

func (m *Module) ownScript(ctx context.Context, user *model.User, id string) (*Script, error) {
	if !model.ValidID(id) {
		return nil, httpx.ErrNotFound
	}
	s, err := m.repo.getScript(ctx, id)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, httpx.ErrNotFound
		}
		return nil, err
	}
	if s.OwnerID != user.ID {
		return nil, httpx.ErrNotFound
	}
	return s, nil
}

func (m *Module) listScripts(c *echo.Context) error {
	list, err := m.repo.listScripts(c.Request().Context(), httpx.UserFrom(c).ID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, list)
}

func (m *Module) getScript(c *echo.Context) error {
	s, err := m.ownScript(c.Request().Context(), httpx.UserFrom(c), c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s)
}

func (m *Module) createScript(c *echo.Context) error {
	var in scriptInput
	if err := httpx.BindLimit(c, &in, maxScriptBytes+64<<10); err != nil {
		return err
	}
	ctx, user := c.Request().Context(), httpx.UserFrom(c)
	if in.Name == nil {
		return httpx.BadRequest("name is required")
	}
	if n, err := m.repo.countScripts(ctx, user.ID); err != nil {
		return err
	} else if n >= maxScripts {
		return httpx.Conflict("too many scripts")
	}
	s := &Script{OwnerID: user.ID}
	if err := in.apply(s); err != nil {
		return err
	}
	if err := m.repo.createScript(ctx, s); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, s)
}

func (m *Module) patchScript(c *echo.Context) error {
	var in scriptInput
	if err := httpx.BindLimit(c, &in, maxScriptBytes+64<<10); err != nil {
		return err
	}
	ctx := c.Request().Context()
	s, err := m.ownScript(ctx, httpx.UserFrom(c), c.Param("id"))
	if err != nil {
		return err
	}
	if err := in.apply(s); err != nil {
		return err
	}
	if err := m.repo.updateScript(ctx, s); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s)
}

func (m *Module) deleteScript(c *echo.Context) error {
	ctx := c.Request().Context()
	s, err := m.ownScript(ctx, httpx.UserFrom(c), c.Param("id"))
	if err != nil {
		return err
	}
	if err := m.repo.deleteScript(ctx, s.ID); err != nil {
		return err
	}
	return httpx.OK(c)
}

type scriptRunRequest struct {
	Content      string            `json:"content"`
	Name         string            `json:"name"`
	SessionID    string            `json:"sessionId"`
	ConnectionID string            `json:"connectionId"`
	Variables    map[string]string `json:"variables"`
	TimeoutSec   int               `json:"timeoutSec"`
}

func (m *Module) runScript(c *echo.Context) error {
	var req scriptRunRequest
	if err := httpx.BindOptional(c, &req); err != nil {
		return err
	}
	ctx, user := c.Request().Context(), httpx.UserFrom(c)
	if !m.scriptsAllowed(ctx, user) {
		return errScriptsForbidden
	}
	s, err := m.ownScript(ctx, user, c.Param("id"))
	if err != nil {
		return err
	}
	content := s.Content
	if req.Content != "" {
		content = req.Content // run the editor's unsaved text
	}
	return m.startScriptRequest(c, user, scriptStart{script: s, content: content, name: s.Name}, req)
}

func (m *Module) runAdhocScript(c *echo.Context) error {
	var req scriptRunRequest
	if err := httpx.BindLimit(c, &req, maxScriptBytes+64<<10); err != nil {
		return err
	}
	ctx, user := c.Request().Context(), httpx.UserFrom(c)
	if !m.scriptsAllowed(ctx, user) {
		return errScriptsForbidden
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "Untitled script"
	}
	return m.startScriptRequest(c, user, scriptStart{content: req.Content, name: truncateUTF8(name, maxNameLen)}, req)
}

func (m *Module) startScriptRequest(c *echo.Context, user *model.User, st scriptStart, req scriptRunRequest) error {
	if strings.TrimSpace(st.content) == "" {
		return httpx.BadRequest("the script is empty")
	}
	if len(st.content) > maxScriptBytes {
		return httpx.BadRequest("the script is too large")
	}
	if req.SessionID != "" && req.ConnectionID != "" {
		return httpx.BadRequest("give either sessionId or connectionId")
	}
	if req.SessionID != "" {
		if _, err := m.ownSession(user, req.SessionID); err != nil {
			return err
		}
	}
	if req.ConnectionID != "" {
		if _, _, err := m.d.ResolveConnection(c.Request().Context(), user, req.ConnectionID); err != nil {
			return err
		}
	}
	if len(req.Variables) > 200 {
		return httpx.BadRequest("too many variables")
	}
	st.sessionID, st.connectionID, st.variables = req.SessionID, req.ConnectionID, req.Variables
	st.origin = OriginManual
	if req.TimeoutSec > 0 {
		st.timeout = min(time.Duration(req.TimeoutSec)*time.Second, maxScriptTimeout)
	}
	jobID, runID, err := m.startScript(user, st)
	if err != nil {
		return err
	}
	name := st.name
	id := ""
	if st.script != nil {
		id = st.script.ID
	}
	m.audit(c, nil, "automation.script.run", id, map[string]any{"name": name, "sessionId": req.SessionID,
		"connectionId": req.ConnectionID})
	return c.JSON(http.StatusOK, jobStarted{JobID: jobID, RunID: runID})
}

// ---- runs ---------------------------------------------------------------------------------------------------------

type scriptStart struct {
	script       *Script
	content      string
	name         string
	sessionID    string
	connectionID string
	variables    map[string]string
	timeout      time.Duration
	origin       string
	onDone       func()
}

// scriptLogEvent is the job data of script runs.
type scriptLogEvent struct {
	Kind  string    `json:"kind"` // "log"
	Level string    `json:"level"`
	Text  string    `json:"text"`
	TS    time.Time `json:"ts"`
	Host  string    `json:"host,omitempty"`
}

// runEvent announces run history changes to the owner ({type:'automation.run'}).
type runEvent struct {
	Type string `json:"type"`
	Run  *Run   `json:"run"`
}

func (m *Module) publishRun(run *Run) {
	cp := *run
	cp.Log, cp.Results = "", nil
	m.publish(run.OwnerID, runEvent{Type: "automation.run", Run: &cp})
}

// compileScript checks the syntax up front so API callers get a 400 instead of a failed job.
func compileScript(name, content string) (*goja.Program, error) {
	prg, err := goja.Compile(name, content, false)
	if err != nil {
		msg := err.Error()
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return nil, httpx.BadRequest("syntax error: " + msg)
	}
	return prg, nil
}

// startScript starts a script run as a job owned by user. It returns the job and run IDs.
func (m *Module) startScript(user *model.User, st scriptStart) (string, string, error) {
	if st.script != nil && st.name == "" {
		st.name = st.script.Name
	}
	prg, err := compileScript(st.name, st.content)
	if err != nil {
		return "", "", err
	}
	if !m.scripts.acquire(user.ID) {
		return "", "", httpx.TooManyRequests("too many scripts are running", 5)
	}
	timeout := st.timeout
	if timeout <= 0 {
		timeout = defaultScriptTimeout
	}
	run := &Run{ID: model.NewID(), OwnerID: user.ID, Kind: RunScript, Name: st.name, Origin: st.origin, Status: StatusRunning,
		StartedAt: now()}
	if st.script != nil {
		run.RefID = st.script.ID
	}
	if mgr := m.sessions(); st.sessionID != "" && mgr != nil {
		if s := mgr.Get(st.sessionID); s != nil {
			run.Target = s.Info().Title
		}
	} else if st.connectionID != "" {
		if conn, err := m.d.Store.Connections.Get(m.ctx, st.connectionID); err == nil {
			run.Target = conn.Name
		}
	}
	if err := m.repo.insertRun(m.ctx, run); err != nil {
		m.scripts.release(user.ID)
		return "", "", err
	}
	var log logBuffer
	ready := make(chan struct{}) // the job starts once run.JobID is set (no shared-write race)
	jobID := m.d.Jobs.Start(user, "script: "+st.name, func(ctx context.Context, emit func(any)) error {
		<-ready
		defer m.scripts.release(user.ID)
		if st.onDone != nil {
			defer st.onDone()
		}
		th := newLogThrottle()
		logf := func(level, text string) {
			ts := time.Now().UTC()
			log.add(ts, level, "", text)
			if ok, notice := th.allow(); ok {
				emit(scriptLogEvent{Kind: "log", Level: level, Text: text, TS: ts})
			} else if notice != "" {
				emit(scriptLogEvent{Kind: "log", Level: "warn", Text: notice, TS: ts})
			}
		}
		runErr := m.execScript(ctx, user, scriptParams{name: st.name, program: prg, vars: st.variables,
			sessionID: st.sessionID, connectionID: st.connectionID, timeout: timeout, logf: logf})
		fin := now()
		run.FinishedAt = &fin
		run.Log = log.String()
		switch {
		case runErr == nil:
			run.Status = StatusOK
		case ctx.Err() != nil:
			run.Status, run.Error = StatusCanceled, "canceled"
		default:
			run.Status, run.Error = StatusError, errorText(runErr)
		}
		if err := m.repo.finishRun(context.WithoutCancel(ctx), run); err != nil {
			m.log.Debug("cannot record script run", "err", err)
		}
		m.publishRun(run)
		return runErr
	})
	run.JobID = jobID
	_ = m.repo.setRunJob(m.ctx, run.ID, jobID)
	m.publishRun(run)
	close(ready)
	return jobID, run.ID, nil
}

// logBuffer keeps the tail of a run log (maxRunLog bytes).
type logBuffer struct {
	mu        sync.Mutex
	b         strings.Builder
	truncated bool
}

func (l *logBuffer) add(ts time.Time, level, host, text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	line := ts.Local().Format("15:04:05.000") + " "
	if level != "info" && level != "log" {
		line += strings.ToUpper(level) + " "
	}
	if host != "" {
		line += "[" + host + "] "
	}
	line += text + "\n"
	l.b.WriteString(line)
	if l.b.Len() > maxRunLog+maxRunLog/4 {
		s := tailUTF8(l.b.String(), maxRunLog)
		l.b.Reset()
		l.b.WriteString(s)
		l.truncated = true
	}
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, truncated := l.b.String(), l.truncated
	if len(s) > maxRunLog {
		s, truncated = tailUTF8(s, maxRunLog), true
	}
	if truncated {
		return "…(earlier output truncated)\n" + s
	}
	return s
}
