package automation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/termstead/termstead/internal/model"
)

// repo gives typed access to the module tables (schema.go). Times are stored as unix milliseconds (SPEC §5.1).
type repo struct{ db *sql.DB }

func now() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

func toMs(t time.Time) int64 { return t.UnixMilli() }

func fromMs(v int64) time.Time { return time.UnixMilli(v).UTC() }

func nullMs(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func fromNullMs(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMs(v.Int64)
	return &t
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func dbErr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return model.ErrNotFound
	}
	return err
}

func expectRow(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return model.ErrNotFound
	}
	return nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

type rowScanner interface{ Scan(dest ...any) error }

// ---- macros (core table "macros", SPEC §5.1) ----------------------------------------------------------------------
//
// The core store decodes steps into model.MacroStep {data, delayMs}; this module owns the macros REST and keeps the
// extended step fields (waitFor, timeoutMs, secret) in the same JSON column, so it reads and writes the rows itself.

const macroCols = `id, owner_id, name, steps, created_at, updated_at`

func scanMacro(sc rowScanner) (*Macro, error) {
	var (
		mc               Macro
		steps            string
		created, updated int64
	)
	if err := sc.Scan(&mc.ID, &mc.OwnerID, &mc.Name, &steps, &created, &updated); err != nil {
		return nil, dbErr(err)
	}
	_ = json.Unmarshal([]byte(steps), &mc.Steps)
	if mc.Steps == nil {
		mc.Steps = []MacroStep{}
	}
	mc.CreatedAt, mc.UpdatedAt = fromMs(created), fromMs(updated)
	return &mc, nil
}

func (r *repo) listMacros(ctx context.Context, owner string) ([]*Macro, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+macroCols+` FROM macros WHERE owner_id = ? ORDER BY name COLLATE NOCASE`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Macro{}
	for rows.Next() {
		mc, err := scanMacro(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, mc)
	}
	return out, rows.Err()
}

func (r *repo) getMacro(ctx context.Context, id string) (*Macro, error) {
	return scanMacro(r.db.QueryRowContext(ctx, `SELECT `+macroCols+` FROM macros WHERE id = ?`, id))
}

func (r *repo) createMacro(ctx context.Context, mc *Macro) error {
	mc.ID = model.NewID()
	if mc.Steps == nil {
		mc.Steps = []MacroStep{}
	}
	t := now()
	mc.CreatedAt, mc.UpdatedAt = t, t
	_, err := r.db.ExecContext(ctx, `INSERT INTO macros (`+macroCols+`) VALUES (?, ?, ?, ?, ?, ?)`,
		mc.ID, mc.OwnerID, mc.Name, mustJSON(mc.Steps), toMs(t), toMs(t))
	return err
}

func (r *repo) updateMacro(ctx context.Context, mc *Macro) error {
	if mc.Steps == nil {
		mc.Steps = []MacroStep{}
	}
	mc.UpdatedAt = now()
	return expectRow(r.db.ExecContext(ctx, `UPDATE macros SET name = ?, steps = ?, updated_at = ? WHERE id = ?`,
		mc.Name, mustJSON(mc.Steps), toMs(mc.UpdatedAt), mc.ID))
}

func (r *repo) deleteMacro(ctx context.Context, id string) error {
	return expectRow(r.db.ExecContext(ctx, `DELETE FROM macros WHERE id = ?`, id))
}

// ---- scripts ------------------------------------------------------------------------------------------------------

const scriptCols = `id, owner_id, name, description, content, created_at, updated_at`

func scanScript(sc rowScanner) (*Script, error) {
	var s Script
	var created, updated int64
	if err := sc.Scan(&s.ID, &s.OwnerID, &s.Name, &s.Description, &s.Content, &created, &updated); err != nil {
		return nil, dbErr(err)
	}
	s.CreatedAt, s.UpdatedAt = fromMs(created), fromMs(updated)
	return &s, nil
}

func (r *repo) listScripts(ctx context.Context, owner string) ([]*Script, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+scriptCols+` FROM automation_scripts WHERE owner_id = ? ORDER BY name COLLATE NOCASE`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Script{}
	for rows.Next() {
		s, err := scanScript(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *repo) countScripts(ctx context.Context, owner string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM automation_scripts WHERE owner_id = ?`, owner).Scan(&n)
	return n, err
}

func (r *repo) getScript(ctx context.Context, id string) (*Script, error) {
	return scanScript(r.db.QueryRowContext(ctx, `SELECT `+scriptCols+` FROM automation_scripts WHERE id = ?`, id))
}

func (r *repo) createScript(ctx context.Context, s *Script) error {
	s.ID = model.NewID()
	t := now()
	s.CreatedAt, s.UpdatedAt = t, t
	_, err := r.db.ExecContext(ctx, `INSERT INTO automation_scripts (`+scriptCols+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.OwnerID, s.Name, s.Description, s.Content, toMs(t), toMs(t))
	return err
}

func (r *repo) updateScript(ctx context.Context, s *Script) error {
	s.UpdatedAt = now()
	return expectRow(r.db.ExecContext(ctx, `UPDATE automation_scripts SET name = ?, description = ?, content = ?, updated_at = ? WHERE id = ?`,
		s.Name, s.Description, s.Content, toMs(s.UpdatedAt), s.ID))
}

func (r *repo) deleteScript(ctx context.Context, id string) error {
	return expectRow(r.db.ExecContext(ctx, `DELETE FROM automation_scripts WHERE id = ?`, id))
}

// ---- triggers -----------------------------------------------------------------------------------------------------

const triggerCols = `id, owner_id, name, enabled, pattern, case_sensitive, scope, actions, cooldown_ms, once, sort_order, created_at, updated_at, event, event_opts`

// triggerEventOpts is the event_opts column.
type triggerEventOpts struct {
	Exit        string `json:"exit,omitempty"`
	MinDuration int    `json:"minDurationSec,omitempty"`
}

func scanTrigger(sc rowScanner) (*Trigger, error) {
	var (
		t                 Trigger
		enabled, cs, once int
		scope, actions    string
		created, updated  int64
		eventOpts         string
	)
	if err := sc.Scan(&t.ID, &t.OwnerID, &t.Name, &enabled, &t.Pattern, &cs, &scope, &actions, &t.CooldownMs, &once,
		&t.SortOrder, &created, &updated, &t.Event, &eventOpts); err != nil {
		return nil, dbErr(err)
	}
	var eo triggerEventOpts
	_ = json.Unmarshal([]byte(eventOpts), &eo)
	t.Exit, t.MinDuration = eo.Exit, eo.MinDuration
	if t.Event == "" {
		t.Event = EventOutput
	}
	t.Enabled, t.CaseSensitive, t.Once = enabled != 0, cs != 0, once != 0
	_ = json.Unmarshal([]byte(scope), &t.Scope)
	_ = json.Unmarshal([]byte(actions), &t.Actions)
	if t.Actions == nil {
		t.Actions = []TriggerAction{}
	}
	t.CreatedAt, t.UpdatedAt = fromMs(created), fromMs(updated)
	return &t, nil
}

func (r *repo) listTriggers(ctx context.Context, owner string) ([]*Trigger, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+triggerCols+` FROM automation_triggers WHERE owner_id = ?
		ORDER BY sort_order, name COLLATE NOCASE`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Trigger{}
	for rows.Next() {
		t, err := scanTrigger(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *repo) getTrigger(ctx context.Context, id string) (*Trigger, error) {
	return scanTrigger(r.db.QueryRowContext(ctx, `SELECT `+triggerCols+` FROM automation_triggers WHERE id = ?`, id))
}

func (r *repo) createTrigger(ctx context.Context, t *Trigger) error {
	t.ID = model.NewID()
	ts := now()
	t.CreatedAt, t.UpdatedAt = ts, ts
	_, err := r.db.ExecContext(ctx, `INSERT INTO automation_triggers (`+triggerCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.OwnerID, t.Name, b2i(t.Enabled), t.Pattern, b2i(t.CaseSensitive), mustJSON(t.Scope), mustJSON(t.Actions),
		t.CooldownMs, b2i(t.Once), t.SortOrder, toMs(ts), toMs(ts), t.Event, mustJSON(triggerEventOpts{Exit: t.Exit, MinDuration: t.MinDuration}))
	return err
}

func (r *repo) updateTrigger(ctx context.Context, t *Trigger) error {
	t.UpdatedAt = now()
	return expectRow(r.db.ExecContext(ctx, `UPDATE automation_triggers SET name = ?, enabled = ?, pattern = ?, case_sensitive = ?,
		scope = ?, actions = ?, cooldown_ms = ?, once = ?, sort_order = ?, updated_at = ?, event = ?, event_opts = ? WHERE id = ?`,
		t.Name, b2i(t.Enabled), t.Pattern, b2i(t.CaseSensitive), mustJSON(t.Scope), mustJSON(t.Actions), t.CooldownMs,
		b2i(t.Once), t.SortOrder, toMs(t.UpdatedAt), t.Event, mustJSON(triggerEventOpts{Exit: t.Exit, MinDuration: t.MinDuration}), t.ID))
}

func (r *repo) deleteTrigger(ctx context.Context, id string) error {
	return expectRow(r.db.ExecContext(ctx, `DELETE FROM automation_triggers WHERE id = ?`, id))
}

func (r *repo) insertTriggerLog(ctx context.Context, owner string, e *TriggerLogEntry) error {
	res, err := r.db.ExecContext(ctx, `INSERT INTO automation_trigger_log (owner_id, trigger_id, trigger_name, session_id, session_title,
		connection_id, line, ts) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		owner, e.TriggerID, e.TriggerName, e.SessionID, e.SessionTitle, e.ConnectionID, e.Line, toMs(e.TS))
	if err != nil {
		return err
	}
	e.ID, _ = res.LastInsertId()
	// Keep the most recent maxTriggerLog lines per user.
	_, err = r.db.ExecContext(ctx, `DELETE FROM automation_trigger_log WHERE owner_id = ? AND id <= (
		SELECT id FROM automation_trigger_log WHERE owner_id = ? ORDER BY id DESC LIMIT 1 OFFSET ?)`, owner, owner, maxTriggerLog)
	return err
}

func (r *repo) listTriggerLog(ctx context.Context, owner, triggerID string, limit int) ([]TriggerLogEntry, error) {
	q := `SELECT id, trigger_id, trigger_name, session_id, session_title, connection_id, line, ts FROM automation_trigger_log WHERE owner_id = ?`
	args := []any{owner}
	if triggerID != "" {
		q += ` AND trigger_id = ?`
		args = append(args, triggerID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TriggerLogEntry{}
	for rows.Next() {
		var e TriggerLogEntry
		var ts int64
		if err := rows.Scan(&e.ID, &e.TriggerID, &e.TriggerName, &e.SessionID, &e.SessionTitle, &e.ConnectionID, &e.Line, &ts); err != nil {
			return nil, err
		}
		e.TS = fromMs(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *repo) clearTriggerLog(ctx context.Context, owner string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM automation_trigger_log WHERE owner_id = ?`, owner)
	return err
}

// ---- schedules ----------------------------------------------------------------------------------------------------

const scheduleCols = `id, owner_id, name, enabled, spec, action, connection_ids, notify, last_run_at, last_status, last_run_id, created_at, updated_at`

func scanSchedule(sc rowScanner) (*Schedule, error) {
	var (
		s                Schedule
		enabled          int
		action, conns    string
		lastRun          sql.NullInt64
		created, updated int64
	)
	if err := sc.Scan(&s.ID, &s.OwnerID, &s.Name, &enabled, &s.Spec, &action, &conns, &s.Notify, &lastRun, &s.LastStatus,
		&s.LastRunID, &created, &updated); err != nil {
		return nil, dbErr(err)
	}
	s.Enabled = enabled != 0
	_ = json.Unmarshal([]byte(action), &s.Action)
	_ = json.Unmarshal([]byte(conns), &s.ConnectionIDs)
	if s.ConnectionIDs == nil {
		s.ConnectionIDs = []string{}
	}
	s.LastRunAt = fromNullMs(lastRun)
	s.CreatedAt, s.UpdatedAt = fromMs(created), fromMs(updated)
	return &s, nil
}

func (r *repo) listSchedules(ctx context.Context, owner string) ([]*Schedule, error) {
	q := `SELECT ` + scheduleCols + ` FROM automation_schedules`
	var args []any
	if owner != "" {
		q += ` WHERE owner_id = ?`
		args = append(args, owner)
	}
	q += ` ORDER BY name COLLATE NOCASE`
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Schedule{}
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *repo) getSchedule(ctx context.Context, id string) (*Schedule, error) {
	return scanSchedule(r.db.QueryRowContext(ctx, `SELECT `+scheduleCols+` FROM automation_schedules WHERE id = ?`, id))
}

func (r *repo) countSchedules(ctx context.Context, owner string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM automation_schedules WHERE owner_id = ?`, owner).Scan(&n)
	return n, err
}

func (r *repo) createSchedule(ctx context.Context, s *Schedule) error {
	s.ID = model.NewID()
	t := now()
	s.CreatedAt, s.UpdatedAt = t, t
	_, err := r.db.ExecContext(ctx, `INSERT INTO automation_schedules (`+scheduleCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.OwnerID, s.Name, b2i(s.Enabled), s.Spec, mustJSON(s.Action), mustJSON(s.ConnectionIDs), s.Notify,
		nullMs(s.LastRunAt), s.LastStatus, s.LastRunID, toMs(t), toMs(t))
	return err
}

func (r *repo) updateSchedule(ctx context.Context, s *Schedule) error {
	s.UpdatedAt = now()
	return expectRow(r.db.ExecContext(ctx, `UPDATE automation_schedules SET name = ?, enabled = ?, spec = ?, action = ?,
		connection_ids = ?, notify = ?, updated_at = ? WHERE id = ?`,
		s.Name, b2i(s.Enabled), s.Spec, mustJSON(s.Action), mustJSON(s.ConnectionIDs), s.Notify, toMs(s.UpdatedAt), s.ID))
}

func (r *repo) setScheduleResult(ctx context.Context, id string, at time.Time, status, runID string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE automation_schedules SET last_run_at = ?, last_status = ?, last_run_id = ? WHERE id = ?`,
		toMs(at), status, runID, id)
	return err
}

func (r *repo) deleteSchedule(ctx context.Context, id string) error {
	return expectRow(r.db.ExecContext(ctx, `DELETE FROM automation_schedules WHERE id = ?`, id))
}

// ---- runs ---------------------------------------------------------------------------------------------------------

const runListCols = `id, owner_id, kind, ref_id, name, origin, target, status, job_id, error, summary, started_at, finished_at`

func scanRun(sc rowScanner, full bool) (*Run, error) {
	var (
		r        Run
		summary  string
		started  int64
		finished sql.NullInt64
		logText  string
		results  string
	)
	dest := []any{&r.ID, &r.OwnerID, &r.Kind, &r.RefID, &r.Name, &r.Origin, &r.Target, &r.Status, &r.JobID, &r.Error,
		&summary, &started, &finished}
	if full {
		dest = append(dest, &logText, &results)
	}
	if err := sc.Scan(dest...); err != nil {
		return nil, dbErr(err)
	}
	_ = json.Unmarshal([]byte(summary), &r.Summary)
	r.StartedAt, r.FinishedAt = fromMs(started), fromNullMs(finished)
	if full {
		r.Log = logText
		_ = json.Unmarshal([]byte(results), &r.Results)
	}
	return &r, nil
}

func (r *repo) insertRun(ctx context.Context, run *Run) error {
	if run.ID == "" {
		run.ID = model.NewID()
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = now()
	}
	results := run.Results
	if results == nil {
		results = []HostResult{}
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO automation_runs (id, owner_id, kind, ref_id, name, origin, target, status, job_id,
		error, log, results, summary, started_at, finished_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.OwnerID, run.Kind, run.RefID, run.Name, run.Origin, run.Target, run.Status, run.JobID, run.Error,
		run.Log, mustJSON(results), mustJSON(run.Summary), toMs(run.StartedAt), nullMs(run.FinishedAt))
	if err != nil {
		return err
	}
	// Keep the most recent maxRunsPerUser runs (never deleting running ones).
	_, err = r.db.ExecContext(ctx, `DELETE FROM automation_runs WHERE owner_id = ? AND status != 'running' AND id IN (
		SELECT id FROM automation_runs WHERE owner_id = ? ORDER BY started_at DESC LIMIT -1 OFFSET ?)`,
		run.OwnerID, run.OwnerID, maxRunsPerUser)
	return err
}

func (r *repo) finishRun(ctx context.Context, run *Run) error {
	results := run.Results
	if results == nil {
		results = []HostResult{}
	}
	return expectRow(r.db.ExecContext(ctx, `UPDATE automation_runs SET status = ?, error = ?, log = ?, results = ?, summary = ?,
		finished_at = ?, job_id = ?, target = ? WHERE id = ?`,
		run.Status, run.Error, run.Log, mustJSON(results), mustJSON(run.Summary), nullMs(run.FinishedAt), run.JobID, run.Target, run.ID))
}

func (r *repo) setRunJob(ctx context.Context, id, jobID string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE automation_runs SET job_id = ? WHERE id = ?`, jobID, id)
	return err
}

type runFilter struct {
	Owner  string
	Kind   string
	RefID  string
	Origin string
	Before int64 // started_at (ms) exclusive
	Limit  int
}

func (r *repo) listRuns(ctx context.Context, f runFilter) ([]*Run, error) {
	var where []string
	args := []any{}
	where = append(where, `owner_id = ?`)
	args = append(args, f.Owner)
	if f.Kind != "" {
		where = append(where, `kind = ?`)
		args = append(args, f.Kind)
	}
	if f.RefID != "" {
		where = append(where, `ref_id = ?`)
		args = append(args, f.RefID)
	}
	if f.Origin != "" {
		where = append(where, `origin = ?`)
		args = append(args, f.Origin)
	}
	if f.Before > 0 {
		where = append(where, `started_at < ?`)
		args = append(args, f.Before)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, `SELECT `+runListCols+` FROM automation_runs WHERE `+strings.Join(where, " AND ")+
		` ORDER BY started_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Run{}
	for rows.Next() {
		run, err := scanRun(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (r *repo) getRun(ctx context.Context, id string) (*Run, error) {
	return scanRun(r.db.QueryRowContext(ctx, `SELECT `+runListCols+`, log, results FROM automation_runs WHERE id = ?`, id), true)
}

func (r *repo) deleteRun(ctx context.Context, id string) error {
	return expectRow(r.db.ExecContext(ctx, `DELETE FROM automation_runs WHERE id = ?`, id))
}

func (r *repo) clearRuns(ctx context.Context, owner string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM automation_runs WHERE owner_id = ? AND status != 'running'`, owner)
	return err
}

// interruptRuns marks runs left "running" by a previous process as failed (called at startup).
func (r *repo) interruptRuns(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `UPDATE automation_runs SET status = 'error', error = 'interrupted (Termstead was restarted)',
		finished_at = ? WHERE status = 'running'`, toMs(now()))
	return err
}
