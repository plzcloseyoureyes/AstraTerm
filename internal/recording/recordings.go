package recording

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// RecordingItem is a recordings row as listed by GET /api/recordings (model.Recording plus derived fields).
type RecordingItem struct {
	model.Recording
	// Live: the session is still writing this recording / log.
	Live bool `json:"live"`
	// Interrupted: never finished (AstraTerm stopped before the session ended); size is the file's current size.
	Interrupted    bool   `json:"interrupted,omitempty"`
	DurationMs     int64  `json:"durationMs,omitempty"`
	ConnectionName string `json:"connectionName,omitempty"`
	OwnerName      string `json:"ownerName,omitempty"`
}

type listFilter struct {
	ownerID      string // "" = everybody (admins)
	connectionID string
	kind         string
	q            string
	from, to     time.Time
	sort         string
	desc         bool
	limit        int
	offset       int
	ids          []string
}

const selectItem = `SELECT r.id, r.owner_id, r.session_id, r.connection_id, r.title, r.kind, r.path, r.size, r.cols, r.rows,
	r.started_at, r.ended_at, COALESCE(c.name, ''), COALESCE(NULLIF(u.display_name, ''), u.username, '')
	FROM recordings r LEFT JOIN connections c ON c.id = r.connection_id LEFT JOIN users u ON u.id = r.owner_id`

func (f listFilter) where() (string, []any) {
	var (
		w    []string
		args []any
	)
	if f.ownerID != "" {
		w, args = append(w, "r.owner_id = ?"), append(args, f.ownerID)
	}
	if f.connectionID != "" {
		w, args = append(w, "r.connection_id = ?"), append(args, f.connectionID)
	}
	if f.kind != "" {
		w, args = append(w, "r.kind = ?"), append(args, f.kind)
	}
	if f.q != "" {
		like := "%" + escapeLike(strings.ToLower(f.q)) + "%"
		w = append(w, `(lower(r.title) LIKE ? ESCAPE '\' OR lower(COALESCE(c.name, '')) LIKE ? ESCAPE '\')`)
		args = append(args, like, like)
	}
	if !f.from.IsZero() {
		w, args = append(w, "r.started_at >= ?"), append(args, f.from.UnixMilli())
	}
	if !f.to.IsZero() {
		w, args = append(w, "r.started_at < ?"), append(args, f.to.UnixMilli())
	}
	if len(f.ids) > 0 {
		w = append(w, "r.id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.ids)), ",")+")")
		for _, id := range f.ids {
			args = append(args, id)
		}
	}
	if len(w) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(w, " AND "), args
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (s *Service) queryItems(ctx context.Context, f listFilter) ([]*RecordingItem, int, error) {
	where, args := f.where()
	var total int
	if err := s.d.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM recordings r LEFT JOIN connections c ON c.id = r.connection_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := "r.started_at"
	switch f.sort {
	case "size":
		order = "r.size"
	case "title":
		order = "lower(r.title)"
	case "duration":
		order = "(COALESCE(r.ended_at, r.started_at) - r.started_at)"
	}
	dir := " DESC"
	if !f.desc {
		dir = " ASC"
	}
	q := selectItem + where + " ORDER BY " + order + dir + ", r.id" + dir
	if f.limit > 0 {
		q += " LIMIT ? OFFSET ?"
		args = append(args, f.limit, f.offset)
	}
	rows, err := s.d.Store.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*RecordingItem{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, 0, err
		}
		s.decorate(it)
		out = append(out, it)
	}
	return out, total, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanItem(sc rowScanner) (*RecordingItem, error) {
	var (
		it      RecordingItem
		started int64
		ended   sql.NullInt64
	)
	if err := sc.Scan(&it.ID, &it.OwnerID, &it.SessionID, &it.ConnectionID, &it.Title, &it.Kind, &it.Path, &it.Size,
		&it.Cols, &it.Rows, &started, &ended, &it.ConnectionName, &it.OwnerName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, httpx.ErrNotFound
		}
		return nil, err
	}
	it.StartedAt = time.UnixMilli(started).UTC()
	if ended.Valid {
		t := time.UnixMilli(ended.Int64).UTC()
		it.EndedAt = &t
	}
	return &it, nil
}

// decorate fills Live / Interrupted / DurationMs (and the current size of unfinished files).
func (s *Service) decorate(it *RecordingItem) {
	if it.EndedAt != nil {
		it.DurationMs = max(it.EndedAt.Sub(it.StartedAt).Milliseconds(), 0)
		return
	}
	it.Live = s.isActive(&it.Recording)
	if fi, err := os.Stat(it.Path); err == nil {
		it.Size = fi.Size()
		if it.Live {
			it.DurationMs = max(s.now().Sub(it.StartedAt).Milliseconds(), 0)
		} else {
			it.DurationMs = max(fi.ModTime().Sub(it.StartedAt).Milliseconds(), 0)
		}
	}
	it.Interrupted = !it.Live
}

// isActive reports whether a live session is still writing rec.
func (s *Service) isActive(rec *model.Recording) bool {
	if rec.EndedAt != nil {
		return false
	}
	sess := s.sessions.Get(rec.SessionID)
	if sess == nil || sess.Closed() {
		return false
	}
	info := sess.Info()
	switch rec.Kind {
	case model.RecordingAsciicast:
		return info.RecordingID == rec.ID
	case model.RecordingLog:
		return info.Logging
	}
	return false
}

// item loads one recording the caller may access (owner, or any for admins; 404 otherwise).
func (s *Service) item(c *echo.Context) (*RecordingItem, *model.User, error) {
	u := httpx.UserFrom(c)
	if u == nil {
		return nil, nil, httpx.ErrUnauthorized
	}
	id := c.Param("id")
	if !model.ValidID(id) {
		return nil, nil, httpx.ErrNotFound
	}
	it, err := scanItem(s.d.Store.DB.QueryRowContext(c.Request().Context(), selectItem+" WHERE r.id = ?", id))
	if err != nil {
		return nil, nil, err
	}
	if it.OwnerID != u.ID && !u.IsAdmin() {
		return nil, nil, httpx.ErrNotFound
	}
	s.decorate(it)
	return it, u, nil
}

// ---- list / get ---------------------------------------------------------------------------------------------------

type listResponse struct {
	Items  []*RecordingItem `json:"items"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

func (s *Service) parseFilter(c *echo.Context) (listFilter, error) {
	u := httpx.UserFrom(c)
	f := listFilter{ownerID: u.ID, limit: 200, desc: true}
	if u.IsAdmin() && (c.QueryParam("all") == "1" || c.QueryParam("all") == "true") {
		f.ownerID = ""
		if v := c.QueryParam("userId"); v != "" {
			f.ownerID = v
		}
	}
	f.connectionID = c.QueryParam("connectionId")
	switch k := c.QueryParam("kind"); k {
	case "", "all":
	case model.RecordingAsciicast, model.RecordingLog:
		f.kind = k
	default:
		return f, httpx.BadRequest("kind must be asciicast or log")
	}
	f.q = strings.TrimSpace(c.QueryParam("q"))
	if len(f.q) > 200 {
		return f, httpx.BadRequest("q is too long")
	}
	var err error
	if f.from, err = parseTimeParam(c.QueryParam("from"), false); err != nil {
		return f, httpx.BadRequest("from: " + err.Error())
	}
	if f.to, err = parseTimeParam(c.QueryParam("to"), true); err != nil {
		return f, httpx.BadRequest("to: " + err.Error())
	}
	switch sortKey := c.QueryParam("sort"); sortKey {
	case "", "started":
	case "size", "title", "duration":
		f.sort = sortKey
	default:
		return f, httpx.BadRequest("sort must be started, size, title or duration")
	}
	if c.QueryParam("order") == "asc" {
		f.desc = false
	}
	if v := c.QueryParam("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			return f, httpx.BadRequest("limit must be between 1 and 1000")
		}
		f.limit = n
	}
	if v := c.QueryParam("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return f, httpx.BadRequest("invalid offset")
		}
		f.offset = n
	}
	return f, nil
}

// parseTimeParam accepts RFC 3339 or a date (YYYY-MM-DD, local midnight; the end of the day when endOfDay).
func parseTimeParam(v string, endOfDay bool) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	t, err := time.ParseInLocation("2006-01-02", v, time.Local)
	if err != nil {
		return time.Time{}, errors.New("expected RFC 3339 or YYYY-MM-DD")
	}
	if endOfDay {
		t = t.AddDate(0, 0, 1)
	}
	return t, nil
}

func (s *Service) handleList(c *echo.Context) error {
	f, err := s.parseFilter(c)
	if err != nil {
		return err
	}
	items, total, err := s.queryItems(c.Request().Context(), f)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, listResponse{Items: items, Total: total, Limit: f.limit, Offset: f.offset})
}

func (s *Service) handleGet(c *echo.Context) error {
	it, _, err := s.item(c)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, it)
}

// ---- usage --------------------------------------------------------------------------------------------------------

type kindUsage struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
}

type usageResponse struct {
	Count      int                  `json:"count"`
	Bytes      int64                `json:"bytes"`
	ByKind     map[string]kindUsage `json:"byKind"`
	Oldest     *time.Time           `json:"oldest,omitempty"`
	Scope      string               `json:"scope"` // "own" | "all"
	MaxAgeDays int                  `json:"maxAgeDays"`
	MaxTotalMB int                  `json:"maxTotalMB"`
}

func (s *Service) handleUsage(c *echo.Context) error {
	u := httpx.UserFrom(c)
	q := `SELECT kind, COUNT(*), COALESCE(SUM(size), 0), MIN(started_at) FROM recordings`
	var args []any
	scope := "all"
	if !(u.IsAdmin() && c.QueryParam("all") == "1") {
		q += " WHERE owner_id = ?"
		args = append(args, u.ID)
		scope = "own"
	}
	rows, err := s.d.Store.DB.QueryContext(c.Request().Context(), q+" GROUP BY kind", args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	p := s.policy()
	out := usageResponse{ByKind: map[string]kindUsage{}, Scope: scope, MaxAgeDays: p.MaxAgeDays, MaxTotalMB: p.MaxTotalMB}
	for rows.Next() {
		var (
			kind   string
			n      int
			b      int64
			oldest sql.NullInt64
		)
		if err := rows.Scan(&kind, &n, &b, &oldest); err != nil {
			return err
		}
		out.ByKind[kind] = kindUsage{Count: n, Bytes: b}
		out.Count += n
		out.Bytes += b
		if oldest.Valid {
			t := time.UnixMilli(oldest.Int64).UTC()
			if out.Oldest == nil || t.Before(*out.Oldest) {
				out.Oldest = &t
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, out)
}

// ---- file ---------------------------------------------------------------------------------------------------------

// safePath verifies that a recording path lies inside AstraTerm's recordings or logs directory and is a regular file.
func (s *Service) safePath(p string) (string, error) {
	if s.d.Cfg == nil || p == "" {
		return "", errGone
	}
	clean := filepath.Clean(p)
	inside := false
	for _, dir := range []string{s.d.Cfg.RecordingsDir(), s.d.Cfg.LogsDir()} {
		if rel, err := filepath.Rel(filepath.Clean(dir), clean); err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			inside = true
			break
		}
	}
	if !inside {
		return "", errGone
	}
	fi, err := os.Lstat(clean)
	if err != nil || !fi.Mode().IsRegular() {
		return "", errGone
	}
	return clean, nil
}

var errGone = httpx.NewError(http.StatusGone, "gone", "the recording file no longer exists")

// handleFile serves a recording: ?format=v3 (default; the file as recorded), v2 (converted on the fly), txt (plain
// text transcript of a cast); logs are served as text. ?download=1 makes it an attachment. For a recording still being
// written the response ends at its last complete line.
func (s *Service) handleFile(c *echo.Context) error {
	it, u, err := s.item(c)
	if err != nil {
		return err
	}
	path, err := s.safePath(it.Path)
	if err != nil {
		return err
	}
	format := c.QueryParam("format")
	switch format {
	case "", "v3", "v2", "txt", "raw":
	default:
		return httpx.BadRequest("format must be v3, v2 or txt")
	}
	if it.Kind == model.RecordingLog && (format == "v2" || format == "v3") {
		return httpx.BadRequest("text logs have no asciicast format")
	}
	f, err := os.Open(path)
	if err != nil {
		return errGone
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()
	if it.Live || it.Interrupted {
		size = completeLines(f, size)
	}
	if it.OwnerID != u.ID {
		s.d.Audit.Log(c, "recording.view", it.ID, map[string]any{"ownerId": it.OwnerID, "kind": it.Kind, "format": format})
	}
	download := c.QueryParam("download") == "1"
	base := downloadName(it)
	h := c.Response().Header()
	h.Set(echo.HeaderCacheControl, "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	src := io.NewSectionReader(f, 0, size)

	switch {
	case it.Kind == model.RecordingLog:
		setDisposition(h, download, base+".log")
		h.Set(echo.HeaderContentType, "text/plain; charset=utf-8")
		http.ServeContent(c.Response(), c.Request(), "", fi.ModTime(), src)
		return nil
	case it.Kind != model.RecordingAsciicast:
		setDisposition(h, true, base+"."+safeExt(it.Kind))
		h.Set(echo.HeaderContentType, "application/octet-stream")
		http.ServeContent(c.Response(), c.Request(), "", fi.ModTime(), src)
		return nil
	case format == "v2":
		setDisposition(h, download, base+".v2.cast")
		h.Set(echo.HeaderContentType, "application/x-asciicast; charset=utf-8")
		c.Response().WriteHeader(http.StatusOK)
		if err := convertCast(c.Response(), src, 2); err != nil && !errors.Is(err, errNotCast) {
			s.log.Debug("cast conversion failed", "recording", it.ID, "err", err)
		}
		return nil
	case format == "txt":
		setDisposition(h, download, base+".txt")
		h.Set(echo.HeaderContentType, "text/plain; charset=utf-8")
		c.Response().WriteHeader(http.StatusOK)
		bw := bufio.NewWriterSize(c.Response(), 64<<10)
		_, err := castTranscript(src, func(l transcriptLine) error {
			_, err := bw.WriteString(l.Text + "\n")
			return err
		})
		if err != nil && !errors.Is(err, errNotCast) {
			s.log.Debug("transcript failed", "recording", it.ID, "err", err)
		}
		return bw.Flush()
	default:
		setDisposition(h, download, base+".cast")
		h.Set(echo.HeaderContentType, "application/x-asciicast; charset=utf-8")
		http.ServeContent(c.Response(), c.Request(), "", fi.ModTime(), src)
		return nil
	}
}

// completeLines returns the length of f's prefix that ends with a newline (at most size).
func completeLines(f *os.File, size int64) int64 {
	const window = 256 << 10
	buf := make([]byte, window)
	for end := size; end > 0; {
		start := max(end-window, 0)
		n, err := f.ReadAt(buf[:end-start], start)
		if n <= 0 && err != nil {
			return 0
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return start + int64(i) + 1
		}
		end = start
	}
	return 0
}

func setDisposition(h http.Header, download bool, name string) {
	kind := "inline"
	if download {
		kind = "attachment"
	}
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	h.Set(echo.HeaderContentDisposition, fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, kind, ascii, url.PathEscape(name)))
}

func downloadName(it *RecordingItem) string {
	title := strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r == 0x7f, strings.ContainsRune(`/\:*?"<>|`, r):
			return '_'
		}
		return r
	}, strings.TrimSpace(it.Title))
	if r := []rune(title); len(r) > 60 {
		title = string(r[:60])
	}
	if title == "" {
		title = "session"
	}
	return title + "-" + it.StartedAt.Local().Format("20060102-150405")
}

// ---- delete -------------------------------------------------------------------------------------------------------

var errActive = httpx.NewError(http.StatusConflict, "recording_active", "the session is still writing this recording; stop it first")

// deleteRecording removes the file and the row of a finished recording.
func (s *Service) deleteRecording(ctx context.Context, rec *model.Recording) error {
	if s.isActive(rec) {
		return errActive
	}
	if p, err := s.safePath(rec.Path); err == nil {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	err := s.d.Store.Recordings.Delete(ctx, rec.ID)
	if errors.Is(err, model.ErrNotFound) {
		return nil
	}
	return err
}

func (s *Service) handleDelete(c *echo.Context) error {
	it, _, err := s.item(c)
	if err != nil {
		return err
	}
	if err := s.deleteRecording(c.Request().Context(), &it.Recording); err != nil {
		return err
	}
	s.d.Audit.Log(c, "recording.delete", it.ID, map[string]any{"ownerId": it.OwnerID, "kind": it.Kind, "title": it.Title, "size": it.Size})
	return httpx.OK(c)
}

type bulkFailure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

func (s *Service) handleBulkDelete(c *echo.Context) error {
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := httpx.Bind(c, &body); err != nil {
		return err
	}
	if len(body.IDs) == 0 {
		return httpx.BadRequest("ids is required")
	}
	if len(body.IDs) > 1000 {
		return httpx.BadRequest("at most 1000 ids")
	}
	u := httpx.UserFrom(c)
	ctx := c.Request().Context()
	seen := map[string]bool{}
	var ids []string
	for _, id := range body.IDs {
		if model.ValidID(id) && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	f := listFilter{ids: ids}
	if !u.IsAdmin() {
		f.ownerID = u.ID
	}
	var items []*RecordingItem
	if len(ids) > 0 {
		var err error
		if items, _, err = s.queryItems(ctx, f); err != nil {
			return err
		}
	}
	found := map[string]bool{}
	failed := []bulkFailure{}
	deleted := []string{}
	var bytesFreed int64
	for _, it := range items {
		found[it.ID] = true
		if err := s.deleteRecording(ctx, &it.Recording); err != nil {
			fe := bulkFailure{ID: it.ID, Error: err.Error()}
			if he, ok := err.(*httpx.HTTPError); ok {
				fe.Error, fe.Code = he.Message, he.Code
			}
			failed = append(failed, fe)
			continue
		}
		deleted = append(deleted, it.ID)
		bytesFreed += it.Size
	}
	for _, id := range body.IDs {
		if !found[id] {
			failed = append(failed, bulkFailure{ID: id, Error: "not found", Code: model.CodeNotFound})
		}
	}
	if len(deleted) > 0 {
		s.d.Audit.Log(c, "recording.bulk_delete", "", map[string]any{"ids": deleted, "count": len(deleted), "bytes": bytesFreed})
	}
	return c.JSON(http.StatusOK, map[string]any{"deleted": deleted, "failed": failed, "bytes": bytesFreed})
}

func safeExt(kind string) string {
	out := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, strings.ToLower(kind))
	if out == "" || len(out) > 10 {
		return "bin"
	}
	return out
}
