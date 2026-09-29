package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/termstead/termstead/internal/model"
)

// ---- settings -----------------------------------------------------------------------------------------------------

// ScopeGlobal is the settings scope for server-wide settings; user settings use the user ID as scope.
const ScopeGlobal = "global"

// Settings is the (scope, key) → JSON value repository.
type Settings struct{ db *sql.DB }

// All returns every key of scope.
func (r *Settings) All(ctx context.Context, scope string) (map[string]json.RawMessage, error) {
	return allSettings(ctx, r.db, scope)
}

func allSettings(ctx context.Context, q execer, scope string) (map[string]json.RawMessage, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, value FROM settings WHERE scope = ?`, scope)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = json.RawMessage(v)
	}
	return out, rows.Err()
}

// Get returns one value (model.ErrNotFound when missing).
func (r *Settings) Get(ctx context.Context, scope, key string) (json.RawMessage, error) {
	var v string
	if err := r.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE scope = ? AND key = ?`, scope, key).Scan(&v); err != nil {
		return nil, mapErr(err)
	}
	return json.RawMessage(v), nil
}

// GetJSON decodes a value into v; it returns false (and leaves v untouched) when the key is missing.
func (r *Settings) GetJSON(ctx context.Context, scope, key string, v any) (bool, error) {
	raw, err := r.Get(ctx, scope, key)
	if errors.Is(err, model.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return false, fmt.Errorf("store: decode setting %s/%s: %w", scope, key, err)
	}
	return true, nil
}

// Set stores one raw JSON value.
func (r *Settings) Set(ctx context.Context, scope, key string, value json.RawMessage) error {
	return r.Apply(ctx, scope, map[string]json.RawMessage{key: value}, nil)
}

// SetJSON marshals v and stores it.
func (r *Settings) SetJSON(ctx context.Context, scope, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return r.Set(ctx, scope, key, b)
}

// Apply upserts set and removes del atomically.
func (r *Settings) Apply(ctx context.Context, scope string, set map[string]json.RawMessage, del []string) error {
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		return applySettings(ctx, tx, scope, set, del)
	})
}

// Update is an atomic read-modify-write of scope: fn gets the current values and returns the keys to upsert and to
// delete (an error aborts without changes). The transaction takes SQLite's write lock at BEGIN (IMMEDIATE), so
// concurrent updates of the same scope — e.g. two browser tabs saving settings — are serialized instead of one
// silently overwriting the other.
func (r *Settings) Update(ctx context.Context, scope string, fn func(current map[string]json.RawMessage) (set map[string]json.RawMessage, del []string, err error)) error {
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		cur, err := allSettings(ctx, tx, scope)
		if err != nil {
			return err
		}
		set, del, err := fn(cur)
		if err != nil {
			return err
		}
		return applySettings(ctx, tx, scope, set, del)
	})
}

func applySettings(ctx context.Context, q execer, scope string, set map[string]json.RawMessage, del []string) error {
	now := ms(Now())
	for k, v := range set {
		if !json.Valid(v) {
			return fmt.Errorf("%w: invalid JSON for setting %q", model.ErrConflict, k)
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO settings (scope, key, value, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(scope, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			scope, k, string(v), now); err != nil {
			return err
		}
	}
	for _, k := range del {
		if _, err := q.ExecContext(ctx, `DELETE FROM settings WHERE scope = ? AND key = ?`, scope, k); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes one key.
func (r *Settings) Delete(ctx context.Context, scope, key string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM settings WHERE scope = ? AND key = ?`, scope, key)
	return mapErr(err)
}

// ---- audit --------------------------------------------------------------------------------------------------------

// Audit is the append-only audit log repository.
type Audit struct{ db *sql.DB }

// AuditFilter selects audit entries. Results are ordered newest first; Before is an exclusive ID cursor.
type AuditFilter struct {
	Limit  int
	Before int64
	UserID string
	// Action matches exactly, or as a prefix when it ends with "*" or "." (e.g. "auth." or "auth.*").
	Action string
	Target string
	Since  time.Time
	Until  time.Time
}

// Insert appends e (e.ID and e.TS are filled).
func (r *Audit) Insert(ctx context.Context, e *model.AuditEntry) error {
	if e.TS.IsZero() {
		e.TS = Now()
	}
	var details any
	if len(e.Details) > 0 && string(e.Details) != "null" {
		details = string(e.Details)
	}
	res, err := r.db.ExecContext(ctx, `INSERT INTO audit_log (ts, user_id, username, action, target, details, ip) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ms(e.TS), e.UserID, e.Username, e.Action, e.Target, details, e.IP)
	if err != nil {
		return mapErr(err)
	}
	e.ID, err = res.LastInsertId()
	return err
}

// List returns entries matching f.
func (r *Audit) List(ctx context.Context, f AuditFilter) ([]model.AuditEntry, error) {
	var (
		where []string
		args  []any
	)
	if f.Before > 0 {
		where, args = append(where, "id < ?"), append(args, f.Before)
	}
	if f.UserID != "" {
		where, args = append(where, "user_id = ?"), append(args, f.UserID)
	}
	if a := f.Action; a != "" {
		if strings.HasSuffix(a, "*") || strings.HasSuffix(a, ".") {
			prefix := strings.TrimSuffix(a, "*")
			where, args = append(where, "substr(action, 1, ?) = ?"), append(args, len(prefix), prefix)
		} else {
			where, args = append(where, "action = ?"), append(args, a)
		}
	}
	if f.Target != "" {
		where, args = append(where, "target = ?"), append(args, f.Target)
	}
	if !f.Since.IsZero() {
		where, args = append(where, "ts >= ?"), append(args, ms(f.Since))
	}
	if !f.Until.IsZero() {
		where, args = append(where, "ts < ?"), append(args, ms(f.Until))
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := `SELECT id, ts, user_id, username, action, target, details, ip FROM audit_log`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []model.AuditEntry{}
	for rows.Next() {
		var (
			e       model.AuditEntry
			ts      int64
			details sql.NullString
		)
		if err := rows.Scan(&e.ID, &ts, &e.UserID, &e.Username, &e.Action, &e.Target, &details, &e.IP); err != nil {
			return nil, err
		}
		e.TS = fromMs(ts)
		if details.Valid && details.String != "" {
			e.Details = json.RawMessage(details.String)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Prune deletes entries older than before (retention).
func (r *Audit) Prune(ctx context.Context, before time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM audit_log WHERE ts < ?`, ms(before))
	if err != nil {
		return 0, mapErr(err)
	}
	return res.RowsAffected()
}

// ---- recordings ---------------------------------------------------------------------------------------------------

// Recordings is the recording metadata repository.
type Recordings struct{ db *sql.DB }

const recordingCols = `id, owner_id, session_id, connection_id, title, kind, path, size, cols, rows, started_at, ended_at`

func scanRecording(sc scanner) (*model.Recording, error) {
	var (
		rec     model.Recording
		started int64
		ended   sql.NullInt64
	)
	if err := sc.Scan(&rec.ID, &rec.OwnerID, &rec.SessionID, &rec.ConnectionID, &rec.Title, &rec.Kind, &rec.Path, &rec.Size,
		&rec.Cols, &rec.Rows, &started, &ended); err != nil {
		return nil, mapErr(err)
	}
	rec.StartedAt, rec.EndedAt = fromMs(started), fromNullMs(ended)
	return &rec, nil
}

func collectRecordings(rows *sql.Rows, err error) ([]*model.Recording, error) {
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*model.Recording{}
	for rows.Next() {
		rec, err := scanRecording(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Create inserts rec.
func (r *Recordings) Create(ctx context.Context, rec *model.Recording) error {
	if rec.ID == "" {
		rec.ID = model.NewID()
	}
	if rec.StartedAt.IsZero() {
		rec.StartedAt = Now()
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO recordings (`+recordingCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.OwnerID, rec.SessionID, rec.ConnectionID, rec.Title, rec.Kind, rec.Path, rec.Size, rec.Cols, rec.Rows,
		ms(rec.StartedAt), nullMs(rec.EndedAt))
	return mapErr(err)
}

// Get returns a recording by ID.
func (r *Recordings) Get(ctx context.Context, id string) (*model.Recording, error) {
	return scanRecording(r.db.QueryRowContext(ctx, `SELECT `+recordingCols+` FROM recordings WHERE id = ?`, id))
}

// List returns recordings newest first: all of them when ownerID is "", otherwise ownerID's.
func (r *Recordings) List(ctx context.Context, ownerID string) ([]*model.Recording, error) {
	if ownerID == "" {
		return collectRecordings(r.db.QueryContext(ctx, `SELECT `+recordingCols+` FROM recordings ORDER BY started_at DESC`))
	}
	return collectRecordings(r.db.QueryContext(ctx, `SELECT `+recordingCols+` FROM recordings WHERE owner_id = ? ORDER BY started_at DESC`, ownerID))
}

// ListBySession returns the recordings of a runtime session.
func (r *Recordings) ListBySession(ctx context.Context, sessionID string) ([]*model.Recording, error) {
	return collectRecordings(r.db.QueryContext(ctx, `SELECT `+recordingCols+` FROM recordings WHERE session_id = ? ORDER BY started_at`, sessionID))
}

// Update saves title, size, geometry and end time.
func (r *Recordings) Update(ctx context.Context, rec *model.Recording) error {
	return expectOne(r.db.ExecContext(ctx, `UPDATE recordings SET title = ?, size = ?, cols = ?, rows = ?, ended_at = ? WHERE id = ?`,
		rec.Title, rec.Size, rec.Cols, rec.Rows, nullMs(rec.EndedAt), rec.ID))
}

// Delete removes the metadata row (the caller deletes the file).
func (r *Recordings) Delete(ctx context.Context, id string) error {
	return expectOne(r.db.ExecContext(ctx, `DELETE FROM recordings WHERE id = ?`, id))
}

// ---- share links --------------------------------------------------------------------------------------------------

// ShareLinks is the live-session share link repository.
type ShareLinks struct{ db *sql.DB }

const shareCols = `id, token_hash, session_id, owner_id, mode, created_at, expires_at`

func scanShare(sc scanner) (*model.ShareLink, error) {
	var (
		s                model.ShareLink
		created, expires int64
	)
	if err := sc.Scan(&s.ID, &s.TokenHash, &s.SessionID, &s.OwnerID, &s.Mode, &created, &expires); err != nil {
		return nil, mapErr(err)
	}
	s.CreatedAt, s.ExpiresAt = fromMs(created), fromMs(expires)
	return &s, nil
}

// Create inserts s (TokenHash must be set).
func (r *ShareLinks) Create(ctx context.Context, s *model.ShareLink) error {
	if s.ID == "" {
		s.ID = model.NewID()
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = Now()
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO share_links (`+shareCols+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.TokenHash, s.SessionID, s.OwnerID, s.Mode, ms(s.CreatedAt), ms(s.ExpiresAt))
	return mapErr(err)
}

// GetByTokenHash returns a non-expired link by token hash.
func (r *ShareLinks) GetByTokenHash(ctx context.Context, hash string) (*model.ShareLink, error) {
	return scanShare(r.db.QueryRowContext(ctx, `SELECT `+shareCols+` FROM share_links WHERE token_hash = ? AND expires_at > ?`,
		hash, ms(Now())))
}

// Get returns a link by ID.
func (r *ShareLinks) Get(ctx context.Context, id string) (*model.ShareLink, error) {
	return scanShare(r.db.QueryRowContext(ctx, `SELECT `+shareCols+` FROM share_links WHERE id = ?`, id))
}

// ListBySession returns the non-expired links of a session.
func (r *ShareLinks) ListBySession(ctx context.Context, sessionID string) ([]*model.ShareLink, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+shareCols+` FROM share_links WHERE session_id = ? AND expires_at > ? ORDER BY created_at`,
		sessionID, ms(Now()))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*model.ShareLink{}
	for rows.Next() {
		s, err := scanShare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Delete removes a link.
func (r *ShareLinks) Delete(ctx context.Context, id string) error {
	return expectOne(r.db.ExecContext(ctx, `DELETE FROM share_links WHERE id = ?`, id))
}

// DeleteBySession removes all links of a session.
func (r *ShareLinks) DeleteBySession(ctx context.Context, sessionID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM share_links WHERE session_id = ?`, sessionID)
	return mapErr(err)
}

// DeleteExpired purges expired links.
func (r *ShareLinks) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM share_links WHERE expires_at <= ?`, ms(now))
	if err != nil {
		return 0, mapErr(err)
	}
	return res.RowsAffected()
}

// ---- vault meta ---------------------------------------------------------------------------------------------------

// VaultMeta stores the vault's wrapped keys and KDF parameters.
type VaultMeta struct{ db *sql.DB }

// Get returns one value (model.ErrNotFound when missing).
func (r *VaultMeta) Get(ctx context.Context, key string) ([]byte, error) {
	var v []byte
	if err := r.db.QueryRowContext(ctx, `SELECT value FROM vault_meta WHERE key = ?`, key).Scan(&v); err != nil {
		return nil, mapErr(err)
	}
	return v, nil
}

// All returns every entry.
func (r *VaultMeta) All(ctx context.Context) (map[string][]byte, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT key, value FROM vault_meta`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var (
			k string
			v []byte
		)
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// Apply upserts set and deletes del atomically.
func (r *VaultMeta) Apply(ctx context.Context, set map[string][]byte, del []string) error {
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		for k, v := range set {
			if _, err := tx.ExecContext(ctx, `INSERT INTO vault_meta (key, value) VALUES (?, ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
				return err
			}
		}
		for _, k := range del {
			if _, err := tx.ExecContext(ctx, `DELETE FROM vault_meta WHERE key = ?`, k); err != nil {
				return err
			}
		}
		return nil
	})
}
