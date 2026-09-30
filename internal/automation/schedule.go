package automation

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/robfig/cron/v3"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
)

// Scheduled tasks (AUTO-12): a standard 5-field cron expression (minute hour day-of-month month day-of-week, names
// allowed) or a descriptor (@hourly, @daily, @weekly, @monthly, @yearly, @every 15m), optionally prefixed with
// CRON_TZ=<zone>, runs a command / snippet / script on connections as a batch run owned by the schedule's owner.
// Tasks run only while AstraTerm runs; a run still in progress when the next one is due is skipped. Secrets need an
// unlocked vault and interactive prompts (host keys, 2FA) fail the host because nobody may be watching.

const minScheduleInterval = time.Minute

type scheduler struct {
	m *Module

	mu      sync.Mutex
	cron    *cron.Cron
	entries map[string]cron.EntryID
	running map[string]bool
}

func newScheduler(m *Module) *scheduler {
	return &scheduler{m: m, entries: map[string]cron.EntryID{}, running: map[string]bool{}}
}

type slogCron struct{ s *scheduler }

func (l slogCron) Info(string, ...any) {}
func (l slogCron) Error(err error, msg string, kv ...any) {
	l.s.m.log.Warn("scheduler: "+msg, append([]any{"err", err}, kv...)...)
}

func (s *scheduler) start() {
	s.mu.Lock()
	s.cron = cron.New(cron.WithLocation(time.Local), cron.WithLogger(slogCron{s}), cron.WithChain(cron.Recover(slogCron{s})))
	s.mu.Unlock()
	list, err := s.m.repo.listSchedules(s.m.ctx, "")
	if err != nil {
		s.m.log.Warn("cannot load schedules", "err", err)
	}
	for _, sch := range list {
		s.upsert(sch)
	}
	s.cron.Start()
	go func() {
		<-s.m.ctx.Done()
		s.mu.Lock()
		c := s.cron
		s.mu.Unlock()
		c.Stop()
	}()
}

// parseSpec validates a cron spec and enforces the minimum interval.
func parseSpec(spec string) (cron.Schedule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, errors.New("the schedule is empty")
	}
	if len(spec) > 200 {
		return nil, errors.New("the schedule is too long")
	}
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return nil, err
	}
	t := time.Now()
	a := sched.Next(t)
	b := sched.Next(a)
	c := sched.Next(b)
	if a.IsZero() {
		return nil, errors.New("the schedule never runs")
	}
	if !b.IsZero() && (b.Sub(a) < minScheduleInterval || (!c.IsZero() && c.Sub(b) < minScheduleInterval)) {
		return nil, errors.New("tasks may run at most once a minute")
	}
	return sched, nil
}

// upsert (re)registers a schedule with the cron runner (disabled ones are removed).
func (s *scheduler) upsert(sch *Schedule) {
	s.remove(sch.ID)
	if !sch.Enabled {
		return
	}
	parsed, err := parseSpec(sch.Spec)
	if err != nil {
		s.m.log.Warn("schedule has an invalid spec", "schedule", sch.ID, "err", err)
		return
	}
	id := sch.ID
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron == nil {
		return
	}
	s.entries[id] = s.cron.Schedule(parsed, cron.FuncJob(func() { s.fire(id) }))
}

func (s *scheduler) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[id]; ok {
		if s.cron != nil {
			s.cron.Remove(e)
		}
		delete(s.entries, id)
	}
}

func (s *scheduler) next(id string) *time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok || s.cron == nil {
		return nil
	}
	n := s.cron.Entry(e).Next
	if n.IsZero() {
		return nil
	}
	n = n.UTC()
	return &n
}

// fire is the cron callback: run the schedule unless its previous run is still going.
func (s *scheduler) fire(id string) {
	s.mu.Lock()
	if s.running[id] {
		s.mu.Unlock()
		s.m.log.Info("scheduled task skipped: previous run still in progress", "schedule", id)
		return
	}
	s.running[id] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, id)
		s.mu.Unlock()
	}()
	if _, err := s.m.runSchedule(s.m.ctx, id, OriginSchedule, nil); err != nil {
		s.m.log.Debug("scheduled task failed", "schedule", id, "err", err)
	}
}

// runSchedule executes a schedule synchronously (cron) and records the run; the returned run ID is set once the run
// exists. started, when not nil, receives the run and job IDs as soon as the job has been started (run now).
func (m *Module) runSchedule(ctx context.Context, id, origin string, started func(runID, jobID string)) (string, error) {
	sch, err := m.repo.getSchedule(ctx, id)
	if err != nil {
		return "", err
	}
	user, err := m.d.Store.Users.Get(ctx, sch.OwnerID)
	if err != nil {
		return "", err
	}
	if user.Disabled {
		return "", errors.New("the owner's account is disabled")
	}
	p, err := m.prepareAction(ctx, user, sch.Action)
	if err != nil {
		m.finishScheduleWithoutRun(ctx, sch, user, errorText(err))
		return "", err
	}
	ids := sch.ConnectionIDs
	if p.Kind != KindScript && len(ids) == 0 {
		err := errors.New("the task has no connections")
		m.finishScheduleWithoutRun(ctx, sch, user, err.Error())
		return "", err
	}
	name := sch.Name
	var run *Run
	var runErr error
	done := make(chan struct{})
	if p.Kind == KindScript && len(ids) == 0 {
		// A script without connections runs once (it may open sessions itself).
		if !m.scripts.acquire(user.ID) {
			err := errors.New("too many scripts are running")
			m.finishScheduleWithoutRun(ctx, sch, user, err.Error())
			return "", err
		}
		// Keyed by the schedule (not the script) so the task's history ("show runs") lists it.
		run = &Run{ID: model.NewID(), OwnerID: user.ID, Kind: RunScript, RefID: sch.ID, Name: name, Origin: origin,
			Status: StatusRunning, StartedAt: store.Now()}
		if err := m.repo.insertRun(ctx, run); err != nil {
			m.scripts.release(user.ID)
			return "", err
		}
		prg, err := compileScript(p.script.Name, p.script.Content)
		if err != nil {
			m.scripts.release(user.ID)
			return "", err
		}
		ready := make(chan struct{})
		jobID := m.d.Jobs.Start(user, "schedule: "+name, func(jctx context.Context, emit func(any)) error {
			<-ready
			defer close(done)
			defer m.scripts.release(user.ID)
			var log logBuffer
			th := newLogThrottle()
			runErr = m.execScript(jctx, user, scriptParams{name: name, program: prg, vars: p.Variables, timeout: p.timeout,
				logf: func(level, text string) {
					ts := time.Now().UTC()
					log.add(ts, level, "", text)
					if ok, notice := th.allow(); ok {
						emit(scriptLogEvent{Kind: "log", Level: level, Text: text, TS: ts})
					} else if notice != "" {
						emit(scriptLogEvent{Kind: "log", Level: "warn", Text: notice, TS: ts})
					}
				}})
			fin := store.Now()
			run.FinishedAt, run.Log = &fin, log.String()
			switch {
			case runErr == nil:
				run.Status = StatusOK
			case jctx.Err() != nil:
				run.Status, run.Error = StatusCanceled, "canceled"
			default:
				run.Status, run.Error = StatusError, errorText(runErr)
			}
			if err := m.repo.finishRun(context.WithoutCancel(jctx), run); err != nil {
				m.log.Debug("cannot record run", "err", err)
			}
			m.publishRun(run)
			return runErr
		})
		run.JobID = jobID
		_ = m.repo.setRunJob(ctx, run.ID, jobID)
		m.publishRun(run)
		close(ready)
		if started != nil {
			started(run.ID, jobID)
		}
	} else {
		if !m.batches.acquire(user.ID) {
			m.finishScheduleWithoutRun(ctx, sch, user, errorText(errTooManyBatches))
			return "", errTooManyBatches
		}
		run = &Run{ID: model.NewID(), OwnerID: user.ID, Kind: RunBatch, RefID: sch.ID, Name: name, Origin: origin,
			Status: StatusRunning, Target: countOf(len(ids), "connection"), StartedAt: store.Now()}
		if err := m.repo.insertRun(ctx, run); err != nil {
			m.batches.release(user.ID)
			return "", err
		}
		ready := make(chan struct{})
		jobID := m.d.Jobs.Start(user, "schedule: "+name, func(jctx context.Context, emit func(any)) error {
			<-ready
			defer close(done)
			defer m.batches.release(user.ID)
			runErr = m.executeBatch(jctx, user, run, ids, p, emit)
			return runErr
		})
		run.JobID = jobID
		_ = m.repo.setRunJob(ctx, run.ID, jobID)
		m.publishRun(run)
		close(ready)
		if started != nil {
			started(run.ID, jobID)
		}
	}
	select {
	case <-done:
	case <-ctx.Done():
		return run.ID, ctx.Err()
	}
	status := run.Status
	if err := m.repo.setScheduleResult(context.WithoutCancel(ctx), sch.ID, store.Now(), status, run.ID); err != nil {
		m.log.Debug("cannot record schedule result", "err", err)
	}
	m.notifySchedule(sch, run)
	return run.ID, runErr
}

func (m *Module) finishScheduleWithoutRun(ctx context.Context, sch *Schedule, user *model.User, msg string) {
	_ = m.repo.setScheduleResult(context.WithoutCancel(ctx), sch.ID, store.Now(), StatusError, "")
	if sch.Notify != NotifyNever {
		m.notify(user.ID, "error", "Scheduled task failed: "+sch.Name, msg)
	}
}

func (m *Module) notifySchedule(sch *Schedule, run *Run) {
	switch {
	case run.Status == StatusOK && sch.Notify == NotifyAlways:
		m.notify(sch.OwnerID, "success", "Scheduled task finished: "+sch.Name, "")
	case run.Status != StatusOK && sch.Notify != NotifyNever:
		msg := run.Error
		if msg == "" {
			msg = run.Status
		}
		m.notify(sch.OwnerID, "error", "Scheduled task failed: "+sch.Name, msg)
	}
}

// ---- REST ---------------------------------------------------------------------------------------------------------

type scheduleInput struct {
	Name             *string    `json:"name"`
	Enabled          *bool      `json:"enabled"`
	Spec             *string    `json:"spec"`
	Action           *JobAction `json:"action"`
	ConnectionIDs    *[]string  `json:"connectionIds"`
	Notify           *string    `json:"notify"`
	ConfirmDangerous bool       `json:"confirmDangerous"`
}

func (m *Module) applySchedule(c *echo.Context, in *scheduleInput, sch *Schedule) error {
	ctx, user := c.Request().Context(), httpx.UserFrom(c)
	if in.Name != nil {
		n, err := cleanName(*in.Name, "name")
		if err != nil {
			return err
		}
		sch.Name = n
	}
	if in.Enabled != nil {
		sch.Enabled = *in.Enabled
	}
	if in.Spec != nil {
		if _, err := parseSpec(*in.Spec); err != nil {
			return httpx.BadRequest("invalid schedule: " + err.Error())
		}
		sch.Spec = strings.TrimSpace(*in.Spec)
	}
	if in.ConnectionIDs != nil {
		ids := []string{}
		if len(*in.ConnectionIDs) > 0 {
			var err error
			if ids, err = uniqueIDs(*in.ConnectionIDs, maxBatchHosts, "connection"); err != nil {
				return err
			}
		}
		for _, id := range ids {
			conn, err := m.d.Store.Connections.Get(ctx, id)
			if err != nil || (conn.OwnerID != user.ID && !conn.Shared) {
				return httpx.BadRequest("connection not found: " + id)
			}
		}
		sch.ConnectionIDs = ids
	}
	if in.Notify != nil {
		switch *in.Notify {
		case NotifyNever, NotifyFailure, NotifyAlways:
			sch.Notify = *in.Notify
		default:
			return httpx.BadRequest("notify must be never, failure or always")
		}
	}
	if in.Action != nil {
		sch.Action = *in.Action
	}
	p, err := m.prepareAction(ctx, user, sch.Action)
	if err != nil {
		return err
	}
	if p.Kind != KindScript && len(sch.ConnectionIDs) == 0 {
		return httpx.BadRequest("choose at least one connection")
	}
	if in.Action != nil && !in.ConfirmDangerous {
		if hits := m.checkActionDangerous(ctx, user, p); len(hits) > 0 {
			return &dangerError{matches: hits}
		}
	}
	return nil
}

func (m *Module) ownSchedule(ctx context.Context, user *model.User, id string) (*Schedule, error) {
	if !model.ValidID(id) {
		return nil, httpx.ErrNotFound
	}
	sch, err := m.repo.getSchedule(ctx, id)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, httpx.ErrNotFound
		}
		return nil, err
	}
	if sch.OwnerID != user.ID {
		return nil, httpx.ErrNotFound
	}
	return sch, nil
}

func (m *Module) withNext(sch *Schedule) *Schedule {
	sch.NextRunAt = m.sched.next(sch.ID)
	return sch
}

func (m *Module) listSchedules(c *echo.Context) error {
	list, err := m.repo.listSchedules(c.Request().Context(), httpx.UserFrom(c).ID)
	if err != nil {
		return err
	}
	for _, s := range list {
		m.withNext(s)
	}
	return c.JSON(http.StatusOK, list)
}

func (m *Module) createSchedule(c *echo.Context) error {
	var in scheduleInput
	if err := httpx.BindLimit(c, &in, maxSnippetBytes+256<<10); err != nil {
		return err
	}
	ctx, user := c.Request().Context(), httpx.UserFrom(c)
	if in.Name == nil || in.Spec == nil || in.Action == nil {
		return httpx.BadRequest("name, spec and action are required")
	}
	if n, err := m.repo.countSchedules(ctx, user.ID); err != nil {
		return err
	} else if n >= maxSchedules {
		return httpx.Conflict("too many scheduled tasks")
	}
	sch := &Schedule{OwnerID: user.ID, Enabled: true, Notify: NotifyFailure, ConnectionIDs: []string{}}
	if err := m.applySchedule(c, &in, sch); err != nil {
		_, err = asDanger(c, err)
		return err
	}
	if err := m.repo.createSchedule(ctx, sch); err != nil {
		return err
	}
	m.sched.upsert(sch)
	m.audit(c, nil, "automation.schedule.create", sch.ID, map[string]any{"name": sch.Name, "spec": sch.Spec, "kind": sch.Action.Kind})
	return c.JSON(http.StatusCreated, m.withNext(sch))
}

func (m *Module) patchSchedule(c *echo.Context) error {
	var in scheduleInput
	if err := httpx.BindLimit(c, &in, maxSnippetBytes+256<<10); err != nil {
		return err
	}
	ctx, user := c.Request().Context(), httpx.UserFrom(c)
	sch, err := m.ownSchedule(ctx, user, c.Param("id"))
	if err != nil {
		return err
	}
	if err := m.applySchedule(c, &in, sch); err != nil {
		_, err = asDanger(c, err)
		return err
	}
	if err := m.repo.updateSchedule(ctx, sch); err != nil {
		return err
	}
	m.sched.upsert(sch)
	m.audit(c, nil, "automation.schedule.update", sch.ID, map[string]any{"name": sch.Name, "enabled": sch.Enabled})
	return c.JSON(http.StatusOK, m.withNext(sch))
}

func (m *Module) deleteSchedule(c *echo.Context) error {
	ctx, user := c.Request().Context(), httpx.UserFrom(c)
	sch, err := m.ownSchedule(ctx, user, c.Param("id"))
	if err != nil {
		return err
	}
	if err := m.repo.deleteSchedule(ctx, sch.ID); err != nil {
		return err
	}
	m.sched.remove(sch.ID)
	m.audit(c, nil, "automation.schedule.delete", sch.ID, map[string]any{"name": sch.Name})
	return httpx.OK(c)
}

// runScheduleNow starts a schedule immediately (in the background) and returns its job / run IDs.
func (m *Module) runScheduleNow(c *echo.Context) error {
	ctx, user := c.Request().Context(), httpx.UserFrom(c)
	sch, err := m.ownSchedule(ctx, user, c.Param("id"))
	if err != nil {
		return err
	}
	type startedIDs struct{ run, job string }
	ch := make(chan startedIDs, 1)
	errCh := make(chan error, 1)
	go func() {
		m.sched.mu.Lock()
		if m.sched.running[sch.ID] {
			m.sched.mu.Unlock()
			errCh <- httpx.Conflict("the task is already running")
			return
		}
		m.sched.running[sch.ID] = true
		m.sched.mu.Unlock()
		defer func() {
			m.sched.mu.Lock()
			delete(m.sched.running, sch.ID)
			m.sched.mu.Unlock()
		}()
		_, err := m.runSchedule(m.ctx, sch.ID, OriginManual, func(runID, jobID string) { ch <- startedIDs{runID, jobID} })
		if err != nil {
			errCh <- err
		}
	}()
	select {
	case ids := <-ch:
		m.audit(c, nil, "automation.schedule.run", sch.ID, map[string]any{"name": sch.Name})
		return c.JSON(http.StatusOK, jobStarted{JobID: ids.job, RunID: ids.run})
	case err := <-errCh:
		if _, ok := errors.AsType[*httpx.HTTPError](err); ok {
			return err
		}
		return httpx.BadRequest(errorText(err))
	case <-time.After(30 * time.Second):
		return httpx.Conflict("the task did not start")
	}
}

type previewResponse struct {
	Valid bool        `json:"valid"`
	Error string      `json:"error,omitempty"`
	Next  []time.Time `json:"next"`
}

// previewSchedule validates a cron spec and lists its next activations (the UI's cron helper).
func (m *Module) previewSchedule(c *echo.Context) error {
	count, _ := strconv.Atoi(c.QueryParam("count"))
	count = min(max(count, 1), 20)
	if c.QueryParam("count") == "" {
		count = 5
	}
	resp := previewResponse{Next: []time.Time{}}
	sched, err := parseSpec(c.QueryParam("spec"))
	if err != nil {
		resp.Error = err.Error()
		return c.JSON(http.StatusOK, resp)
	}
	resp.Valid = true
	t := time.Now()
	for i := 0; i < count; i++ {
		t = sched.Next(t)
		if t.IsZero() {
			break
		}
		resp.Next = append(resp.Next, t.UTC())
	}
	return c.JSON(http.StatusOK, resp)
}

// ---- run history --------------------------------------------------------------------------------------------------

func (m *Module) listRuns(c *echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	var before int64
	if b := c.QueryParam("before"); b != "" {
		t, err := time.Parse(time.RFC3339Nano, b)
		if err != nil {
			return httpx.BadRequest("before must be an RFC 3339 time")
		}
		before = t.UnixMilli()
	}
	f := runFilter{Owner: httpx.UserFrom(c).ID, Kind: c.QueryParam("kind"), RefID: c.QueryParam("refId"),
		Origin: c.QueryParam("origin"), Before: before, Limit: limit}
	list, err := m.repo.listRuns(c.Request().Context(), f)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, list)
}

func (m *Module) ownRun(ctx context.Context, user *model.User, id string) (*Run, error) {
	if !model.ValidID(id) {
		return nil, httpx.ErrNotFound
	}
	run, err := m.repo.getRun(ctx, id)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, httpx.ErrNotFound
		}
		return nil, err
	}
	if run.OwnerID != user.ID {
		return nil, httpx.ErrNotFound
	}
	return run, nil
}

func (m *Module) getRun(c *echo.Context) error {
	run, err := m.ownRun(c.Request().Context(), httpx.UserFrom(c), c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, run)
}

func (m *Module) deleteRun(c *echo.Context) error {
	ctx := c.Request().Context()
	run, err := m.ownRun(ctx, httpx.UserFrom(c), c.Param("id"))
	if err != nil {
		return err
	}
	if run.Status == StatusRunning {
		return httpx.Conflict("the run is still in progress")
	}
	if err := m.repo.deleteRun(ctx, run.ID); err != nil {
		return err
	}
	return httpx.OK(c)
}

func (m *Module) clearRuns(c *echo.Context) error {
	if err := m.repo.clearRuns(c.Request().Context(), httpx.UserFrom(c).ID); err != nil {
		return err
	}
	return httpx.OK(c)
}
