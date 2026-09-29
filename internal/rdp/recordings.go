package rdp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/store"
)

// Recordings of guacd sessions live in the module table rdp_recordings: the core recordings table only admits the
// terminal kinds (asciicast, log). GET /api/rdp/recordings lists them (own; administrators ?all=1),
// GET /api/rdp/recordings/{id}/file serves the Guacamole protocol stream (play it with Guacamole.SessionRecording),
// DELETE /api/rdp/recordings/{id} removes one. Owners and administrators only; everyone else gets 404.

func init() {
	store.RegisterMigration("rdp", 1, `CREATE TABLE IF NOT EXISTS rdp_recordings (
		id            TEXT PRIMARY KEY,
		owner_id      TEXT NOT NULL,
		session_id    TEXT NOT NULL,
		connection_id TEXT NOT NULL DEFAULT '',
		title         TEXT NOT NULL DEFAULT '',
		path          TEXT NOT NULL,
		size          INTEGER NOT NULL DEFAULT 0,
		width         INTEGER NOT NULL DEFAULT 0,
		height        INTEGER NOT NULL DEFAULT 0,
		started_at    INTEGER NOT NULL,
		ended_at      INTEGER
	);
	CREATE INDEX IF NOT EXISTS rdp_recordings_owner ON rdp_recordings (owner_id, started_at)`)
}

// rdpRecording is a recorded guacd session (JSON: web/src/features/rdp/types.ts RdpRecording).
type rdpRecording struct {
	ID           string     `json:"id"`
	OwnerID      string     `json:"ownerId"`
	SessionID    string     `json:"sessionId"`
	ConnectionID string     `json:"connectionId,omitempty"`
	Title        string     `json:"title"`
	Kind         string     `json:"kind"` // "guac"
	Size         int64      `json:"size"`
	Width        int        `json:"width"`
	Height       int        `json:"height"`
	StartedAt    time.Time  `json:"startedAt"`
	EndedAt      *time.Time `json:"endedAt,omitempty"`
	Path         string     `json:"-"`
}

const recordingCols = `id, owner_id, session_id, connection_id, title, path, size, width, height, started_at, ended_at`

type recordingStore struct{ db *sql.DB }

func (h *handler) recordings() (*recordingStore, error) {
	if h.d.Store == nil || h.d.Store.DB == nil {
		return nil, errors.New("recordings are not available")
	}
	return &recordingStore{db: h.d.Store.DB}, nil
}

func (r *recordingStore) create(ctx context.Context, rec *rdpRecording) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO rdp_recordings (`+recordingCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.OwnerID, rec.SessionID, rec.ConnectionID, rec.Title, rec.Path, rec.Size, rec.Width, rec.Height,
		rec.StartedAt.UnixMilli(), nullMillis(rec.EndedAt))
	return err
}

func (r *recordingStore) finish(ctx context.Context, rec *rdpRecording) error {
	_, err := r.db.ExecContext(ctx, `UPDATE rdp_recordings SET size = ?, width = ?, height = ?, ended_at = ? WHERE id = ?`,
		rec.Size, rec.Width, rec.Height, nullMillis(rec.EndedAt), rec.ID)
	return err
}

func (r *recordingStore) get(ctx context.Context, id string) (*rdpRecording, error) {
	list, err := r.query(ctx, `SELECT `+recordingCols+` FROM rdp_recordings WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, httpx.ErrNotFound
	}
	return list[0], nil
}

// list returns recordings newest first: ownerID's, or everybody's when ownerID is "".
func (r *recordingStore) list(ctx context.Context, ownerID string) ([]*rdpRecording, error) {
	if ownerID == "" {
		return r.query(ctx, `SELECT `+recordingCols+` FROM rdp_recordings ORDER BY started_at DESC LIMIT 1000`)
	}
	return r.query(ctx, `SELECT `+recordingCols+` FROM rdp_recordings WHERE owner_id = ? ORDER BY started_at DESC LIMIT 1000`, ownerID)
}

func (r *recordingStore) listBySession(ctx context.Context, sessionID string) ([]*rdpRecording, error) {
	return r.query(ctx, `SELECT `+recordingCols+` FROM rdp_recordings WHERE session_id = ? ORDER BY started_at`, sessionID)
}

func (r *recordingStore) delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM rdp_recordings WHERE id = ?`, id)
	return err
}

func (r *recordingStore) query(ctx context.Context, q string, args ...any) ([]*rdpRecording, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*rdpRecording{}
	for rows.Next() {
		var (
			rec     rdpRecording
			started int64
			ended   sql.NullInt64
		)
		if err := rows.Scan(&rec.ID, &rec.OwnerID, &rec.SessionID, &rec.ConnectionID, &rec.Title, &rec.Path, &rec.Size,
			&rec.Width, &rec.Height, &started, &ended); err != nil {
			return nil, err
		}
		rec.Kind = recordingKindGuac
		rec.StartedAt = time.UnixMilli(started).UTC()
		if ended.Valid {
			t := time.UnixMilli(ended.Int64).UTC()
			rec.EndedAt = &t
		}
		out = append(out, &rec)
	}
	return out, rows.Err()
}

func nullMillis(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

// ---- handlers ------------------------------------------------------------------------------------------------------

// recording returns a recording the caller may access (owner or administrator; 404 otherwise).
func (h *handler) recording(c *echo.Context) (*rdpRecording, *model.User, error) {
	u := httpx.UserFrom(c)
	if u == nil {
		return nil, nil, httpx.ErrUnauthorized
	}
	id := c.Param("id")
	if !model.ValidID(id) {
		return nil, nil, httpx.ErrNotFound
	}
	rs, err := h.recordings()
	if err != nil {
		return nil, nil, err
	}
	rec, err := rs.get(c.Request().Context(), id)
	if err != nil {
		return nil, nil, err
	}
	if rec.OwnerID != u.ID && !u.IsAdmin() {
		return nil, nil, httpx.ErrNotFound
	}
	return rec, u, nil
}

func (h *handler) handleListRecordings(c *echo.Context) error {
	u := httpx.UserFrom(c)
	rs, err := h.recordings()
	if err != nil {
		return err
	}
	owner := u.ID
	if c.QueryParam("all") == "1" && u.IsAdmin() {
		owner = ""
	}
	list, err := rs.list(c.Request().Context(), owner)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, list)
}

func (h *handler) handleRecordingFile(c *echo.Context) error {
	rec, u, err := h.recording(c)
	if err != nil {
		return err
	}
	f, err := os.Open(rec.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return httpx.NewError(http.StatusGone, "gone", "the recording file no longer exists")
		}
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if rec.OwnerID != u.ID {
		h.auditUser(c.Request().Context(), u, "rdp.recording.view", rec.ID, map[string]any{"ownerId": rec.OwnerID})
	}
	name := rdpFileName(rec.Title, "remote-desktop")
	name = strings.TrimSuffix(name, ".rdp") + rec.StartedAt.Format("-2006-01-02-150405") + ".guac"
	hdr := c.Response().Header()
	hdr.Set(echo.HeaderContentDisposition, fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
		asciiFileName(name), url.PathEscape(name)))
	hdr.Set(echo.HeaderContentType, "application/octet-stream")
	hdr.Set(echo.HeaderCacheControl, "no-store")
	http.ServeContent(c.Response(), c.Request(), "", fi.ModTime(), f)
	return nil
}

func (h *handler) handleDeleteRecording(c *echo.Context) error {
	rec, u, err := h.recording(c)
	if err != nil {
		return err
	}
	rs, err := h.recordings()
	if err != nil {
		return err
	}
	if rec.EndedAt == nil {
		return httpx.Conflict("the recording is still running")
	}
	if err := os.Remove(rec.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := rs.delete(c.Request().Context(), rec.ID); err != nil {
		return err
	}
	h.auditUser(c.Request().Context(), u, "rdp.recording.delete", rec.ID, map[string]any{"ownerId": rec.OwnerID, "title": rec.Title})
	return httpx.OK(c)
}

// mountRecordings registers the recording endpoints.
func (h *handler) mountRecordings(api *echo.Group) {
	api.GET("/rdp/recordings", h.handleListRecordings)
	api.GET("/rdp/recordings/:id/file", h.handleRecordingFile)
	api.DELETE("/rdp/recordings/:id", h.handleDeleteRecording)
}
