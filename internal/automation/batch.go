package automation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v5"

	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/term"
)

// Batch runs (AUTO-11): run a command, a snippet or a script on many saved connections with a parallelism limit,
// optional stop-on-error and a per-host timeout. SSH connections run commands on an exec channel (clean output and
// exit code); other protocols — or mode "session" — open a terminal session, type the command, capture the output
// until the prompt returns and close the session. Scripts get each connection's session bound as `session`.
// Per-host results stream as job events and are stored in the run history (also used by scheduled tasks).

const (
	maxBatchParallel   = 32
	defaultHostTimeout = 60 * time.Second
	maxHostTimeout     = 6 * time.Hour
	maxBatchHosts      = 500
	perUserBatches     = 4  // concurrent batch runs (and scheduled tasks) per user
	totalBatches       = 32 // … and in total
)

var errTooManyBatches = httpx.TooManyRequests("too many batch runs are in progress; wait for one to finish", 10)

// batchEvent is the job data of batch runs.
type batchEvent struct {
	Kind    string      `json:"kind"` // host | summary | log
	Result  *HostResult `json:"result,omitempty"`
	Summary *RunSummary `json:"summary,omitempty"`
	Level   string      `json:"level,omitempty"`
	Text    string      `json:"text,omitempty"`
	Host    string      `json:"host,omitempty"`
	TS      time.Time   `json:"ts"`
}

type batchRequest struct {
	Name             string   `json:"name"`
	ConnectionIDs    []string `json:"connectionIds"`
	ConfirmDangerous bool     `json:"confirmDangerous"`
	JobAction
}

// preparedAction is a validated JobAction with the loaded snippet / script.
type preparedAction struct {
	JobAction
	snippet  *model.Snippet
	script   *Script
	timeout  time.Duration
	parallel int
}

func (m *Module) prepareAction(ctx context.Context, user *model.User, a JobAction) (*preparedAction, error) {
	p := &preparedAction{JobAction: a}
	switch a.Kind {
	case KindCommand:
		if strings.TrimSpace(a.Command) == "" {
			return nil, httpx.BadRequest("command is required")
		}
		if len(a.Command) > maxSnippetBytes || !utf8.ValidString(a.Command) {
			return nil, httpx.BadRequest("command is too large or not valid UTF-8")
		}
	case KindSnippet:
		s, err := m.ownSnippet(ctx, user, a.SnippetID)
		if err != nil {
			return nil, httpx.BadRequest("snippet not found")
		}
		p.snippet = s
	case KindScript:
		s, err := m.ownScript(ctx, user, a.ScriptID)
		if err != nil {
			return nil, httpx.BadRequest("script not found")
		}
		if !m.scriptsAllowed(ctx, user) {
			return nil, errScriptsForbidden
		}
		if _, err := compileScript(s.Name, s.Content); err != nil {
			return nil, err
		}
		p.script = s
	default:
		return nil, httpx.BadRequest("kind must be command, snippet or script")
	}
	switch a.Mode {
	case "", ModeAuto:
		p.Mode = ModeAuto
	case ModeExec, ModeSession:
	default:
		return nil, httpx.BadRequest("mode must be auto, exec or session")
	}
	if len(a.Variables) > 200 {
		return nil, httpx.BadRequest("too many variables")
	}
	p.timeout = defaultHostTimeout
	if a.TimeoutSec > 0 {
		p.timeout = min(time.Duration(a.TimeoutSec)*time.Second, maxHostTimeout)
	}
	p.parallel = clampInt(a.Parallel, 1, maxBatchParallel)
	if a.Parallel == 0 {
		p.parallel = 4
	}
	return p, nil
}

// text returns the command text of a command / snippet action (placeholders rendered for conn).
func (p *preparedAction) text(conn *model.Connection, t time.Time) (string, []string) {
	switch p.Kind {
	case KindCommand:
		return RenderTemplate(p.Command, p.Variables, connectionBuiltins(conn, t))
	case KindSnippet:
		return RenderTemplate(p.snippet.Content, p.Variables, connectionBuiltins(conn, t))
	}
	return "", nil
}

// checkActionDangerous applies the guard to a command / snippet action (placeholders rendered with defaults).
func (m *Module) checkActionDangerous(ctx context.Context, user *model.User, p *preparedAction) []DangerMatch {
	if p.Kind == KindScript {
		return nil
	}
	var src string
	if p.Kind == KindCommand {
		src = p.Command
	} else {
		src = p.snippet.Content
	}
	text, _ := RenderTemplate(src, p.Variables, nil)
	return checkDangerous(text, loadGuardConfig(ctx, m.d.Store, user.ID))
}

func (m *Module) startBatch(c *echo.Context) error {
	var req batchRequest
	if err := httpx.BindLimit(c, &req, maxSnippetBytes+256<<10); err != nil {
		return err
	}
	ctx, user := c.Request().Context(), httpx.UserFrom(c)
	ids, err := uniqueIDs(req.ConnectionIDs, maxBatchHosts, "connection")
	if err != nil {
		return err
	}
	p, err := m.prepareAction(ctx, user, req.JobAction)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, _, err := m.d.ResolveConnection(ctx, user, id); err != nil {
			if errors.Is(err, httpx.ErrLocked) {
				return err
			}
			return httpx.BadRequest("connection not found: " + id)
		}
	}
	if !req.ConfirmDangerous {
		if hits := m.checkActionDangerous(ctx, user, p); len(hits) > 0 {
			return refuseDangerous(c, hits)
		}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = batchName(p)
	}
	jobID, runID, err := m.startBatchJob(user, truncateUTF8(name, maxNameLen), ids, p, OriginManual, "")
	if err != nil {
		return err
	}
	m.audit(c, nil, "automation.batch.run", runID, map[string]any{"name": name, "kind": p.Kind, "connections": len(ids),
		"dangerousConfirmed": req.ConfirmDangerous})
	return c.JSON(http.StatusOK, jobStarted{JobID: jobID, RunID: runID})
}

func batchName(p *preparedAction) string {
	switch p.Kind {
	case KindSnippet:
		return "Snippet: " + p.snippet.Name
	case KindScript:
		return "Script: " + p.script.Name
	}
	line := strings.TrimSpace(strings.SplitN(p.Command, "\n", 2)[0])
	return "Command: " + truncateUTF8(line, 80)
}

// startBatchJob records a run and executes it as a job.
func (m *Module) startBatchJob(user *model.User, name string, ids []string, p *preparedAction, origin, refID string) (string, string, error) {
	run := &Run{ID: model.NewID(), OwnerID: user.ID, Kind: RunBatch, RefID: refID, Name: name, Origin: origin,
		Status: StatusRunning, Target: countOf(len(ids), "connection"), StartedAt: now()}
	if p.Kind == KindScript && refID == "" {
		run.RefID = p.script.ID
	}
	if !m.batches.acquire(user.ID) {
		return "", "", errTooManyBatches
	}
	if err := m.repo.insertRun(m.ctx, run); err != nil {
		m.batches.release(user.ID)
		return "", "", err
	}
	ready := make(chan struct{})
	jobID := m.d.Jobs.Start(user, "batch: "+name, func(ctx context.Context, emit func(any)) error {
		<-ready
		defer m.batches.release(user.ID)
		return m.executeBatch(ctx, user, run, ids, p, emit)
	})
	run.JobID = jobID
	_ = m.repo.setRunJob(m.ctx, run.ID, jobID)
	m.publishRun(run)
	close(ready)
	return jobID, run.ID, nil
}

// executeBatch runs p on every connection and finishes the run record. emit may be nil.
func (m *Module) executeBatch(ctx context.Context, user *model.User, run *Run, ids []string, p *preparedAction, emit func(any)) error {
	if emit == nil {
		emit = func(any) {}
	}
	var (
		mu      sync.Mutex
		results = make([]HostResult, len(ids))
		log     logBuffer
		stopped bool
	)
	for i, id := range ids {
		results[i] = HostResult{ConnectionID: id, Name: id, Status: StatusPending}
		if conn, err := m.d.Store.Connections.Get(ctx, id); err == nil {
			results[i].Name, results[i].Host = conn.Name, conn.Host
		}
	}
	th := newLogThrottle()
	runCtx, cancelRest := context.WithCancel(ctx)
	defer cancelRest()
	sem := make(chan struct{}, p.parallel)
	var wg sync.WaitGroup
	for i, id := range ids {
		select {
		case sem <- struct{}{}:
		case <-runCtx.Done():
		}
		if runCtx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			mu.Lock()
			res := results[i]
			mu.Unlock()
			res.Status = StatusRunning
			emit(batchEvent{Kind: "host", Result: &res, TS: time.Now().UTC()})
			logf := func(level, text string) {
				ts := time.Now().UTC()
				log.add(ts, level, res.Name, text)
				if ok, notice := th.allow(); ok {
					emit(batchEvent{Kind: "log", Level: level, Text: text, Host: res.Name, TS: ts})
				} else if notice != "" {
					emit(batchEvent{Kind: "log", Level: "warn", Text: notice, TS: ts})
				}
			}
			m.runOnHost(runCtx, user, id, p, &res, logf)
			mu.Lock()
			results[i] = res
			if res.Status == StatusError && p.StopOnError && !stopped {
				stopped = true
				cancelRest()
			}
			mu.Unlock()
			emit(batchEvent{Kind: "host", Result: &res, TS: time.Now().UTC()})
		}()
	}
	wg.Wait()

	var sum RunSummary
	sum.Total = len(ids)
	for i := range results {
		switch results[i].Status {
		case StatusOK:
			sum.OK++
		case StatusError:
			sum.Failed++
		default:
			if results[i].Status == StatusPending || results[i].Status == StatusRunning {
				results[i].Status = StatusSkipped
				if results[i].Error == "" {
					results[i].Error = "not run"
				}
			}
			sum.Skipped++
		}
	}
	emit(batchEvent{Kind: "summary", Summary: &sum, TS: time.Now().UTC()})
	fin := now()
	run.FinishedAt = &fin
	run.Results, run.Summary, run.Log = results, sum, log.String()
	var err error
	switch {
	case ctx.Err() != nil:
		run.Status, run.Error = StatusCanceled, "canceled"
		err = ctx.Err()
	case sum.Failed > 0:
		run.Status = StatusError
		run.Error = fmt.Sprintf("%d of %d connections failed", sum.Failed, sum.Total)
		if stopped {
			run.Error += " (stopped on the first error)"
		}
		err = errors.New(run.Error)
	default:
		run.Status = StatusOK
	}
	if ferr := m.repo.finishRun(context.WithoutCancel(ctx), run); ferr != nil {
		m.log.Debug("cannot record batch run", "err", ferr)
	}
	m.publishRun(run)
	return err
}

// runOnHost executes the action on one connection and fills res.
func (m *Module) runOnHost(ctx context.Context, user *model.User, id string, p *preparedAction, res *HostResult, logf func(string, string)) {
	start := time.Now()
	defer func() { res.DurationMs = time.Since(start).Milliseconds() }()
	fail := func(err error) {
		res.Status, res.Error = StatusError, errorText(err)
	}
	conn, _, err := m.d.ResolveConnection(ctx, user, id)
	if err != nil {
		fail(err)
		return
	}
	res.Name, res.Host = conn.Name, conn.Host
	hctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	if p.Kind == KindScript {
		res.Mode = KindScript
		m.scriptOnHost(hctx, user, conn, p, res, logf)
		return
	}
	text, missing := p.text(conn, time.Now())
	if len(missing) > 0 {
		fail(fmt.Errorf("missing values for: %s", strings.Join(missing, ", ")))
		return
	}
	isSSH := conn.Protocol == model.ProtoSSH || conn.Protocol == model.ProtoSFTP
	mode := p.Mode
	if mode == ModeAuto {
		mode = ModeSession
		if isSSH {
			mode = ModeExec
		}
	}
	res.Mode = mode
	if mode == ModeExec {
		if !isSSH || m.c == nil || m.c.SSH == nil {
			fail(errors.New("exec mode needs an SSH connection"))
			return
		}
		cl, release, err := m.c.SSH.Get(hctx, user, id)
		if err != nil {
			fail(err)
			return
		}
		defer release()
		so, se, code, err := cl.Exec(hctx, strings.ReplaceAll(text, "\r\n", "\n"))
		out := string(so)
		if len(se) > 0 {
			if out != "" && !strings.HasSuffix(out, "\n") {
				out += "\n"
			}
			out += string(se)
		}
		res.Output = tailUTF8(out, maxHostOutput)
		if code >= 0 {
			c := code
			res.ExitCode = &c
		}
		switch {
		case err != nil:
			fail(err)
		case code != 0:
			res.Status, res.Error = StatusError, fmt.Sprintf("exit code %d", code)
		default:
			res.Status = StatusOK
		}
		return
	}
	out, exit, err := m.sessionCommand(hctx, user, conn, text)
	res.Output = tailUTF8(out, maxHostOutput)
	res.ExitCode = exit
	switch {
	case err != nil:
		fail(err)
	case exit != nil && *exit != 0:
		res.Status, res.Error = StatusError, fmt.Sprintf("exit code %d", *exit)
	default:
		res.Status = StatusOK
	}
}

// openAutomationSession creates a session for conn and waits until it is connected. Close it with mgr.Close.
func (m *Module) openAutomationSession(ctx context.Context, user *model.User, conn *model.Connection, title string) (*term.Session, *tap, error) {
	mgr := m.sessions()
	if mgr == nil {
		return nil, nil, errors.New("sessions are not available")
	}
	s, err := mgr.Create(ctx, user, term.CreateRequest{ConnectionID: conn.ID, Cols: 200, Rows: 50, Title: title})
	if err != nil {
		return nil, nil, err
	}
	t := m.acquireTap(s)
	if err := waitConnected(ctx, t, s, 60*time.Second); err != nil {
		t.release()
		_ = mgr.Close(s.ID)
		return nil, nil, err
	}
	return s, t, nil
}

// sessionCommand types text into a fresh session of conn and captures the output until the prompt returns.
func (m *Module) sessionCommand(ctx context.Context, user *model.User, conn *model.Connection, text string) (string, *int, error) {
	s, t, err := m.openAutomationSession(ctx, user, conn, "Batch · "+conn.Name)
	if err != nil {
		return "", nil, err
	}
	defer func() {
		t.release()
		if mgr := m.sessions(); mgr != nil {
			_ = mgr.Close(s.ID)
		}
	}()
	r := t.newReader(false)
	// Wait for the login banner / first prompt to settle.
	if err := r.waitIdle(ctx, 700*time.Millisecond, 20*time.Second); err != nil {
		return "", nil, err
	}
	seq := t.promptCount()
	mark := r.mark()
	t.mu.Lock()
	t.lastExit = nil
	t.mu.Unlock()
	for _, line := range splitLines(text) {
		if err := m.write(s.ID, line+"\r"); err != nil {
			return "", nil, err
		}
	}
	_ = sleepCtx(ctx, 100*time.Millisecond)
	ok, err := r.waitPrompt(ctx, seq, defaultPromptRe, 800*time.Millisecond, time.Until(deadlineOf(ctx)))
	out := cleanCommandOutput(r.textSince(mark), text, defaultPromptRe)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return out, nil, errors.New("timed out waiting for the command to finish")
		}
		return out, nil, err
	}
	if !ok {
		return out, nil, errors.New("timed out waiting for the command to finish")
	}
	t.mu.Lock()
	exit := t.lastExit
	t.mu.Unlock()
	return out, exit, nil
}

func deadlineOf(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Now().Add(defaultHostTimeout)
}

// scriptOnHost runs the batch script with a fresh session of conn bound as `session`.
func (m *Module) scriptOnHost(ctx context.Context, user *model.User, conn *model.Connection, p *preparedAction, res *HostResult, logf func(string, string)) {
	// Wait for a script slot (a batch may run more hosts in parallel than scripts may run per user).
	for !m.scripts.acquire(user.ID) {
		if err := sleepCtx(ctx, 250*time.Millisecond); err != nil {
			res.Status, res.Error = StatusError, "too many scripts are running"
			return
		}
	}
	defer m.scripts.release(user.ID)
	prg, err := compileScript(p.script.Name, p.script.Content)
	if err != nil {
		res.Status, res.Error = StatusError, errorText(err)
		return
	}
	s, t, err := m.openAutomationSession(ctx, user, conn, "Script · "+conn.Name)
	if err != nil {
		res.Status, res.Error = StatusError, errorText(err)
		return
	}
	t.release() // the script environment takes its own reference
	defer func() {
		if mgr := m.sessions(); mgr != nil {
			_ = mgr.Close(s.ID)
		}
	}()
	var out strings.Builder
	hostLog := func(level, text string) {
		if out.Len() < maxHostOutput {
			out.WriteString(text)
			out.WriteByte('\n')
		}
		logf(level, text)
	}
	vars := map[string]string{}
	for k, v := range p.Variables {
		vars[k] = v
	}
	vars["connectionId"], vars["connectionName"], vars["host"] = conn.ID, conn.Name, conn.Host
	err = m.execScript(ctx, user, scriptParams{name: p.script.Name, program: prg, vars: vars, session: s,
		timeout: time.Until(deadlineOf(ctx)), logf: hostLog})
	res.Output = tailUTF8(out.String(), maxHostOutput)
	if err != nil {
		res.Status, res.Error = StatusError, errorText(err)
		return
	}
	res.Status = StatusOK
}
