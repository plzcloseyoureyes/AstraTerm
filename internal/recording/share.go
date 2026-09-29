package recording

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v5"
	"golang.org/x/time/rate"

	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/store"
	"github.com/nexterm/nexterm/internal/term"
)

// Live session sharing (MU-18). A share link maps a random 256-bit token (base64url; only its SHA-256 is stored in
// the core share_links table, plus the token sealed with the vault system key in recording_share_meta so the owner can
// copy the link again) to a live terminal session in read-only or interactive mode, with an expiry, an optional
// "viewers must be signed in to NexTerm" flag and a viewer cap. Viewers attach anonymously through the public
// /ws/share/{token} (the terminal WebSocket protocol of SPEC §6.2); read-only viewers can never send input or resize.
// Links die with their session (runtime sessions do not survive a restart), when revoked, or at expiry (connected
// viewers are disconnected then). Public endpoints are rate-limited per client IP.

func init() {
	store.RegisterMigration("recording", 1, `
CREATE TABLE IF NOT EXISTS recording_share_meta (
	share_id      TEXT PRIMARY KEY REFERENCES share_links(id) ON DELETE CASCADE,
	token_enc     BLOB,
	require_login INTEGER NOT NULL DEFAULT 0,
	max_viewers   INTEGER NOT NULL DEFAULT 0,
	label         TEXT NOT NULL DEFAULT '',
	uses          INTEGER NOT NULL DEFAULT 0,
	last_used_at  INTEGER
);`)
}

const (
	shareTokenBytes    = 32
	defaultMaxViewers  = 20
	maxViewersCap      = 100
	maxSessionViewers  = 50
	minShareSeconds    = 60
	maxShareLabelRunes = 80
)

// ShareView is a share link as returned to its owner.
type ShareView struct {
	ID           string       `json:"id"`
	SessionID    string       `json:"sessionId"`
	SessionTitle string       `json:"sessionTitle,omitempty"`
	OwnerID      string       `json:"ownerId"`
	Mode         string       `json:"mode"`
	CreatedAt    time.Time    `json:"createdAt"`
	ExpiresAt    time.Time    `json:"expiresAt"`
	RequireLogin bool         `json:"requireLogin"`
	MaxViewers   int          `json:"maxViewers"`
	Label        string       `json:"label,omitempty"`
	Uses         int          `json:"uses"`
	LastUsedAt   *time.Time   `json:"lastUsedAt,omitempty"`
	URL          string       `json:"url,omitempty"`
	Token        string       `json:"token,omitempty"`
	Viewers      []ViewerInfo `json:"viewers"`
	// InputPaused: the owner paused guest input of this interactive link (viewers see it read-only meanwhile).
	InputPaused bool `json:"inputPaused"`
}

// ViewerInfo describes one connected share viewer.
type ViewerInfo struct {
	ID       string    `json:"id"`
	IP       string    `json:"ip"`
	Username string    `json:"username,omitempty"`
	Since    time.Time `json:"since"`
	Mode     string    `json:"mode"`
}

type viewer struct {
	info    ViewerInfo
	shareID string
	write   bool // the link is interactive
	ref     *GuestRef
	cancel  context.CancelCauseFunc
	// push sends a control message to the viewer's socket (set once the socket is open).
	push func(msg []byte)
}

// Causes a viewer's stream ends with (context.Cause of its context).
var (
	errShareRevoked      = errors.New("the owner stopped sharing this session")
	errShareExpired      = errors.New("this share link has expired")
	errShareSessionEnded = errors.New("the shared session has ended")
)

// closeShareEnded is the WebSocket close code telling a share viewer that the link is gone for good (revoked,
// expired, disabled by policy): no reconnect.
const closeShareEnded = websocket.StatusCode(4410)

type shareHub struct {
	s  *Service
	mu sync.Mutex
	// viewers by share ID, and the session each share belongs to.
	viewers map[string]map[*viewer]struct{}
	session map[string]string
	// paused holds the interactive links whose guest input the owner paused.
	paused map[string]bool
}

func newShareHub(s *Service) *shareHub {
	return &shareHub{s: s, viewers: map[string]map[*viewer]struct{}{}, session: map[string]string{}, paused: map[string]bool{}}
}

func (h *shareHub) isPaused(shareID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.paused[shareID]
}

// canType reports whether v may send input right now.
func (v *viewer) canType(h *shareHub) bool { return v.write && !h.isPaused(v.shareID) }

// readonlyMsg is the viewer's current read-only state as a terminal protocol message.
func (v *viewer) readonlyMsg(h *shareHub) []byte {
	if v.canType(h) {
		return []byte(`{"type":"readonly","value":false}`)
	}
	return []byte(`{"type":"readonly","value":true}`)
}

// setPaused pauses or resumes guest input of an interactive link and tells its connected viewers.
func (h *shareHub) setPaused(shareID string, paused bool) {
	h.mu.Lock()
	if paused {
		h.paused[shareID] = true
	} else {
		delete(h.paused, shareID)
	}
	type target struct {
		v    *viewer
		push func([]byte)
	}
	var ts []target
	for v := range h.viewers[shareID] {
		if v.push != nil {
			ts = append(ts, target{v, v.push})
		}
	}
	h.mu.Unlock()
	for _, t := range ts {
		t.push(t.v.readonlyMsg(h))
	}
}

func ctrlError(msg string) []byte {
	b, _ := json.Marshal(map[string]string{"type": "error", "message": msg})
	return b
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func newToken() (string, error) {
	b := make([]byte, shareTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// validTokenShape rejects obviously invalid tokens before any database work.
func validTokenShape(tok string) bool {
	if len(tok) != base64.RawURLEncoding.EncodedLen(shareTokenBytes) {
		return false
	}
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func shareURL(tok string) string { return "/share/" + tok }

type shareMeta struct {
	tokenEnc     []byte
	requireLogin bool
	maxViewers   int
	label        string
	uses         int
	lastUsedAt   *time.Time
}

func (h *shareHub) loadMeta(ctx context.Context, id string) (shareMeta, error) {
	var (
		m    shareMeta
		req  int
		last sql.NullInt64
	)
	err := h.s.d.Store.DB.QueryRowContext(ctx, `SELECT token_enc, require_login, max_viewers, label, uses, last_used_at
		FROM recording_share_meta WHERE share_id = ?`, id).Scan(&m.tokenEnc, &req, &m.maxViewers, &m.label, &m.uses, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return shareMeta{maxViewers: defaultMaxViewers}, nil
	}
	if err != nil {
		return m, err
	}
	m.requireLogin = req != 0
	if m.maxViewers <= 0 {
		m.maxViewers = defaultMaxViewers
	}
	if last.Valid {
		t := time.UnixMilli(last.Int64).UTC()
		m.lastUsedAt = &t
	}
	return m, nil
}

// view builds the owner's view of a link (token recovered from its sealed copy when possible).
func (h *shareHub) view(ctx context.Context, l *model.ShareLink) ShareView {
	v := ShareView{ID: l.ID, SessionID: l.SessionID, OwnerID: l.OwnerID, Mode: l.Mode, CreatedAt: l.CreatedAt,
		ExpiresAt: l.ExpiresAt, Viewers: h.viewersOf(l.ID), InputPaused: l.Mode == model.ShareWrite && h.isPaused(l.ID)}
	if m, err := h.loadMeta(ctx, l.ID); err == nil {
		v.RequireLogin, v.MaxViewers, v.Label, v.Uses, v.LastUsedAt = m.requireLogin, m.maxViewers, m.label, m.uses, m.lastUsedAt
		if len(m.tokenEnc) > 0 {
			if tok, err := h.s.d.Vault.SystemOpen(m.tokenEnc); err == nil && hashToken(string(tok)) == l.TokenHash {
				v.Token, v.URL = string(tok), shareURL(string(tok))
			}
		}
	}
	if sess := h.s.sessions.Get(l.SessionID); sess != nil {
		v.SessionTitle = sess.Title()
	}
	return v
}

func (h *shareHub) viewersOf(shareID string) []ViewerInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []ViewerInfo{}
	for v := range h.viewers[shareID] {
		out = append(out, v.info)
	}
	return out
}

func (h *shareHub) sessionViewers(sessionID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessionViewersLocked(sessionID)
}

func (h *shareHub) sessionViewersLocked(sessionID string) int {
	n := 0
	for id, sid := range h.session {
		if sid == sessionID {
			n += len(h.viewers[id])
		}
	}
	return n
}

// disconnect ends the stream of every viewer of a share (cause: why).
func (h *shareHub) disconnect(shareID string, why error) {
	h.mu.Lock()
	vs := h.viewers[shareID]
	delete(h.viewers, shareID)
	delete(h.session, shareID)
	delete(h.paused, shareID)
	h.mu.Unlock()
	for v := range vs {
		v.cancel(why)
	}
}

// admit registers v as a viewer of link l on sess unless a viewer limit is reached.
func (h *shareHub) admit(v *viewer, l *model.ShareLink, sessionID string, maxViewers int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.viewers[l.ID]) >= maxViewers || h.sessionViewersLocked(sessionID) >= maxSessionViewers {
		return false
	}
	if h.viewers[l.ID] == nil {
		h.viewers[l.ID] = map[*viewer]struct{}{}
	}
	h.viewers[l.ID][v] = struct{}{}
	h.session[l.ID] = sessionID
	return true
}

// leave unregisters v.
func (h *shareHub) leave(v *viewer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if vs := h.viewers[v.shareID]; vs != nil {
		delete(vs, v)
		if len(vs) == 0 {
			delete(h.viewers, v.shareID)
			delete(h.session, v.shareID)
		}
	}
}

// revoke deletes a link and disconnects its viewers.
func (h *shareHub) revoke(ctx context.Context, l *model.ShareLink) error {
	if err := h.s.d.Store.ShareLinks.Delete(ctx, l.ID); err != nil && !errors.Is(err, model.ErrNotFound) {
		return err
	}
	h.disconnect(l.ID, errShareRevoked)
	h.publishChanged(l.OwnerID, l.SessionID)
	return nil
}

// sessionClosed forgets the links of a closed session.
func (h *shareHub) sessionClosed(sessionID string) {
	h.mu.Lock()
	var ids []string
	for id, sid := range h.session {
		if sid == sessionID {
			ids = append(ids, id)
		}
	}
	h.mu.Unlock()
	for _, id := range ids {
		h.disconnect(id, errShareSessionEnded)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.s.d.Store.ShareLinks.DeleteBySession(ctx, sessionID); err != nil && h.s.d.Ctx.Err() == nil {
		h.s.log.Debug("deleting share links failed", "session", sessionID, "err", err)
	}
}

// pruneDead removes expired links and links whose session no longer runs.
func (h *shareHub) pruneDead(ctx context.Context) {
	if _, err := h.s.d.Store.ShareLinks.DeleteExpired(ctx, store.Now()); err != nil && ctx.Err() == nil {
		h.s.log.Debug("pruning expired share links failed", "err", err)
	}
	rows, err := h.s.d.Store.DB.QueryContext(ctx, `SELECT id, session_id FROM share_links`)
	if err != nil {
		return
	}
	var dead []string
	for rows.Next() {
		var id, sid string
		if rows.Scan(&id, &sid) == nil {
			if sess := h.s.sessions.Get(sid); sess == nil || sess.Closed() {
				dead = append(dead, id)
			}
		}
	}
	rows.Close()
	for _, id := range dead {
		_ = h.s.d.Store.ShareLinks.Delete(ctx, id)
		h.disconnect(id, errShareSessionEnded)
	}
}

// enforcePolicy revokes the links the administrator's new policy no longer allows.
func (h *shareHub) enforcePolicy(ctx context.Context, p Policy) {
	rows, err := h.s.d.Store.DB.QueryContext(ctx, `SELECT id, session_id, owner_id, mode FROM share_links`)
	if err != nil {
		return
	}
	var drop []model.ShareLink
	for rows.Next() {
		var l model.ShareLink
		if rows.Scan(&l.ID, &l.SessionID, &l.OwnerID, &l.Mode) == nil {
			if !p.ShareEnabled || (!p.ShareWriteEnabled && l.Mode == model.ShareWrite) {
				drop = append(drop, l)
			}
		}
	}
	rows.Close()
	for i := range drop {
		_ = h.revoke(ctx, &drop[i])
	}
}

type shareChangedEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	ShareID   string `json:"shareId,omitempty"`
	Viewers   int    `json:"viewers"`
}

// publishChanged tells the owner's windows that the shares of a session changed (dialogs refresh).
func (h *shareHub) publishChanged(ownerID, sessionID string) {
	if h.s.d.Events == nil {
		return
	}
	h.s.d.Events.Publish(ownerID, shareChangedEvent{Type: "share.changed", SessionID: sessionID, Viewers: h.sessionViewers(sessionID)})
}

// ---- owner endpoints ----------------------------------------------------------------------------------------------

func (s *Service) handleCreateShare(c *echo.Context) error {
	sess, u, err := s.sessionFor(c, true)
	if err != nil {
		return err
	}
	if sess.Kind != model.KindTerminal {
		return httpx.BadRequest("only terminal sessions can be shared")
	}
	var body struct {
		Mode         string `json:"mode"`
		ExpiresInSec int64  `json:"expiresInSec"`
		RequireLogin bool   `json:"requireLogin"`
		MaxViewers   int    `json:"maxViewers"`
		Label        string `json:"label"`
		// InputPaused starts an interactive link with guest input paused: guests watch until the owner allows it.
		InputPaused bool `json:"inputPaused"`
	}
	if err := httpx.Bind(c, &body); err != nil {
		return err
	}
	p := s.policy()
	if !p.ShareEnabled {
		return httpx.Forbidden("session sharing is disabled by the administrator")
	}
	switch body.Mode {
	case model.ShareRead:
	case model.ShareWrite:
		if !p.ShareWriteEnabled {
			return httpx.Forbidden("interactive share links are disabled by the administrator")
		}
	default:
		return httpx.BadRequest("mode must be read or write")
	}
	maxSec := int64(p.ShareMaxHours) * 3600
	if body.ExpiresInSec == 0 {
		body.ExpiresInSec = min(3600, maxSec)
	}
	if body.ExpiresInSec < minShareSeconds || body.ExpiresInSec > maxSec {
		return httpx.BadRequest("expiresInSec must be between 60 and " + strconv.FormatInt(maxSec, 10))
	}
	if body.MaxViewers < 0 || body.MaxViewers > maxViewersCap {
		return httpx.BadRequest("maxViewers must be between 0 and 100")
	}
	label := strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, body.Label))
	if r := []rune(label); len(r) > maxShareLabelRunes {
		label = string(r[:maxShareLabelRunes])
	}
	tok, err := newToken()
	if err != nil {
		return err
	}
	sealed, err := s.d.Vault.SystemSeal([]byte(tok))
	if err != nil {
		sealed = nil // the link still works; it just cannot be shown again
	}
	now := store.Now()
	link := &model.ShareLink{ID: model.NewID(), TokenHash: hashToken(tok), SessionID: sess.ID, OwnerID: u.ID,
		Mode: body.Mode, CreatedAt: now, ExpiresAt: now.Add(time.Duration(body.ExpiresInSec) * time.Second)}
	ctx := c.Request().Context()
	maxViewers := body.MaxViewers
	if maxViewers == 0 {
		maxViewers = defaultMaxViewers
	}
	err = s.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO share_links (id, token_hash, session_id, owner_id, mode, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, link.ID, link.TokenHash, link.SessionID, link.OwnerID, link.Mode,
			link.CreatedAt.UnixMilli(), link.ExpiresAt.UnixMilli()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO recording_share_meta (share_id, token_enc, require_login, max_viewers, label)
			VALUES (?, ?, ?, ?, ?)`, link.ID, sealed, b2i(body.RequireLogin), maxViewers, label)
		return err
	})
	if err != nil {
		return err
	}
	if sess.Closed() { // closed while we were creating the link
		_ = s.d.Store.ShareLinks.Delete(ctx, link.ID)
		return httpx.ErrNotFound
	}
	if link.Mode == model.ShareWrite && body.InputPaused {
		s.shares.setPaused(link.ID, true)
	}
	s.d.Audit.Log(c, "share.create", link.ID, map[string]any{"sessionId": sess.ID, "mode": link.Mode,
		"expiresAt": link.ExpiresAt, "requireLogin": body.RequireLogin, "maxViewers": maxViewers,
		"inputPaused": link.Mode == model.ShareWrite && body.InputPaused})
	s.shares.publishChanged(u.ID, sess.ID)
	v := s.shares.view(ctx, link)
	v.Token, v.URL = tok, shareURL(tok)
	return c.JSON(http.StatusCreated, v)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Service) handleListShares(c *echo.Context) error {
	sess, _, err := s.sessionFor(c, false)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	links, err := s.d.Store.ShareLinks.ListBySession(ctx, sess.ID)
	if err != nil {
		return err
	}
	u := httpx.UserFrom(c)
	out := []ShareView{}
	for _, l := range links {
		v := s.shares.view(ctx, l)
		if l.OwnerID != u.ID { // admins see the links of other users' sessions, not their tokens
			v.Token, v.URL = "", ""
		}
		out = append(out, v)
	}
	return c.JSON(http.StatusOK, out)
}

// handleListMyShares: GET /api/shares → the caller's active links over all sessions (admins: ?all=1).
func (s *Service) handleListMyShares(c *echo.Context) error {
	u := httpx.UserFrom(c)
	ctx := c.Request().Context()
	q := `SELECT id, token_hash, session_id, owner_id, mode, created_at, expires_at FROM share_links WHERE expires_at > ?`
	args := []any{store.Now().UnixMilli()}
	all := u.IsAdmin() && c.QueryParam("all") == "1"
	if !all {
		q += " AND owner_id = ?"
		args = append(args, u.ID)
	}
	rows, err := s.d.Store.DB.QueryContext(ctx, q+" ORDER BY created_at", args...)
	if err != nil {
		return err
	}
	var links []model.ShareLink
	for rows.Next() {
		var (
			l                model.ShareLink
			created, expires int64
		)
		if err := rows.Scan(&l.ID, &l.TokenHash, &l.SessionID, &l.OwnerID, &l.Mode, &created, &expires); err != nil {
			rows.Close()
			return err
		}
		l.CreatedAt, l.ExpiresAt = time.UnixMilli(created).UTC(), time.UnixMilli(expires).UTC()
		links = append(links, l)
	}
	rows.Close()
	out := []ShareView{}
	for i := range links {
		if sess := s.sessions.Get(links[i].SessionID); sess == nil || sess.Closed() {
			continue
		}
		v := s.shares.view(ctx, &links[i])
		if links[i].OwnerID != u.ID {
			v.Token, v.URL = "", ""
		}
		out = append(out, v)
	}
	return c.JSON(http.StatusOK, out)
}

func (s *Service) handleRevokeShare(c *echo.Context) error {
	u := httpx.UserFrom(c)
	id := c.Param("id")
	if !model.ValidID(id) {
		return httpx.ErrNotFound
	}
	ctx := c.Request().Context()
	l, err := s.d.Store.ShareLinks.Get(ctx, id)
	if err != nil {
		return err
	}
	if l.OwnerID != u.ID && !u.IsAdmin() {
		return httpx.ErrNotFound
	}
	if err := s.shares.revoke(ctx, l); err != nil {
		return err
	}
	s.d.Audit.Log(c, "share.revoke", l.ID, map[string]any{"sessionId": l.SessionID, "ownerId": l.OwnerID})
	return httpx.OK(c)
}

// handleShareInput: PUT /api/shares/{id}/input {paused} pauses or resumes guest input of an interactive link (owner or
// admin). Connected guests switch to read-only (and back) at once.
func (s *Service) handleShareInput(c *echo.Context) error {
	var body struct {
		Paused *bool `json:"paused"`
	}
	if err := httpx.Bind(c, &body); err != nil {
		return err
	}
	if body.Paused == nil {
		return httpx.BadRequest("paused is required")
	}
	u := httpx.UserFrom(c)
	id := c.Param("id")
	if !model.ValidID(id) {
		return httpx.ErrNotFound
	}
	ctx := c.Request().Context()
	l, err := s.d.Store.ShareLinks.Get(ctx, id)
	if err != nil {
		return err
	}
	if l.OwnerID != u.ID && !u.IsAdmin() {
		return httpx.ErrNotFound
	}
	if l.Mode != model.ShareWrite {
		return httpx.BadRequest("only interactive links accept guest input")
	}
	s.shares.setPaused(l.ID, *body.Paused)
	s.d.Audit.Log(c, "share.input", l.ID, map[string]any{"sessionId": l.SessionID, "ownerId": l.OwnerID, "paused": *body.Paused})
	s.shares.publishChanged(l.OwnerID, l.SessionID)
	return c.JSON(http.StatusOK, s.shares.view(ctx, l))
}

func (s *Service) handleRevokeSessionShares(c *echo.Context) error {
	sess, _, err := s.sessionFor(c, false)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	links, err := s.d.Store.ShareLinks.ListBySession(ctx, sess.ID)
	if err != nil {
		return err
	}
	for _, l := range links {
		if err := s.shares.revoke(ctx, l); err != nil {
			return err
		}
	}
	if len(links) > 0 {
		s.d.Audit.Log(c, "share.revoke_all", sess.ID, map[string]any{"count": len(links), "ownerId": sess.OwnerID})
	}
	return c.JSON(http.StatusOK, map[string]any{"revoked": len(links)})
}

// ---- public endpoints ---------------------------------------------------------------------------------------------

// ShareInfo is the public description of a share link (no session id, owner or host details).
type ShareInfo struct {
	Title        string             `json:"title"`
	State        model.SessionState `json:"state"`
	Cols         int                `json:"cols"`
	Rows         int                `json:"rows"`
	Mode         string             `json:"mode"`
	ExpiresAt    time.Time          `json:"expiresAt"`
	RequireLogin bool               `json:"requireLogin"`
	Label        string             `json:"label,omitempty"`
}

var (
	errShareNotFound = httpx.NewError(http.StatusNotFound, "share_not_found", "this share link is invalid, expired or was revoked")
	errLoginRequired = httpx.Unauthorized("login_required", "sign in to NexTerm to open this shared session")
	errTooManyViews  = httpx.NewError(http.StatusConflict, "too_many_viewers", "this shared session has reached its viewer limit")
)

// resolveShare checks the rate limits and returns the link, its meta and live session for a token.
func (s *Service) resolveShare(c *echo.Context) (*model.ShareLink, shareMeta, *term.Session, error) {
	ip := httpx.ClientIP(c)
	if err := s.limits.allow(ip, s.now()); err != nil {
		return nil, shareMeta{}, nil, err
	}
	tok := c.Param("token")
	fail := func() (*model.ShareLink, shareMeta, *term.Session, error) {
		s.limits.failure(ip, s.now())
		return nil, shareMeta{}, nil, errShareNotFound
	}
	if !validTokenShape(tok) || !s.policy().ShareEnabled {
		return fail()
	}
	ctx := c.Request().Context()
	l, err := s.d.Store.ShareLinks.GetByTokenHash(ctx, hashToken(tok))
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return fail()
		}
		return nil, shareMeta{}, nil, err
	}
	sess := s.sessions.Get(l.SessionID)
	if sess == nil || sess.Closed() || sess.Kind != model.KindTerminal || sess.OwnerID != l.OwnerID {
		return nil, shareMeta{}, nil, errShareNotFound
	}
	m, err := s.shares.loadMeta(ctx, l.ID)
	if err != nil {
		return nil, shareMeta{}, nil, err
	}
	if m.requireLogin && httpx.UserFrom(c) == nil {
		return nil, shareMeta{}, nil, errLoginRequired
	}
	if l.Mode == model.ShareWrite && !s.policy().ShareWriteEnabled {
		l.Mode = model.ShareRead
	}
	return l, m, sess, nil
}

func (s *Service) handleShareInfo(c *echo.Context) error {
	l, m, sess, err := s.resolveShare(c)
	if err != nil {
		return err
	}
	info := sess.Info()
	c.Response().Header().Set("Referrer-Policy", "no-referrer")
	return c.JSON(http.StatusOK, ShareInfo{Title: info.Title, State: info.State, Cols: info.Cols, Rows: info.Rows,
		Mode: l.Mode, ExpiresAt: l.ExpiresAt, RequireLogin: m.requireLogin, Label: m.label})
}

type joinEvent struct {
	Type    string `json:"type"`
	Level   string `json:"level"`
	Title   string `json:"title"`
	Message string `json:"message,omitempty"`
}

// handleShareWS serves /ws/share/{token}?offset=: the session's terminal stream (SPEC §6.2) through the share relay
// (relayShare): read-only unless the link is interactive and its guest input is not paused.
func (s *Service) handleShareWS(c *echo.Context) error {
	l, m, sess, err := s.resolveShare(c)
	if err != nil {
		return err
	}
	var offset int64
	if v := c.QueryParam("offset"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return httpx.BadRequest("invalid offset")
		}
		offset = n
	}
	h := s.shares
	user := httpx.UserFrom(c)
	ip := httpx.ClientIP(c)
	ctx, cancel := context.WithCancelCause(c.Request().Context())
	defer cancel(nil)
	ctx, dcancel := context.WithDeadlineCause(ctx, l.ExpiresAt, errShareExpired)
	defer dcancel()
	v := &viewer{info: ViewerInfo{ID: model.NewID(), IP: ip, Since: s.now().UTC(), Mode: l.Mode}, shareID: l.ID,
		write: l.Mode == model.ShareWrite, cancel: cancel}
	if user != nil {
		v.info.Username = user.Username
	}
	v.ref = &GuestRef{ShareID: l.ID, ViewerID: v.info.ID, Username: v.info.Username, IP: ip, Label: m.label}
	// Admission is atomic with the registration (limits hold under concurrent joins).
	if !h.admit(v, l, sess.ID, m.maxViewers) {
		return errTooManyViews
	}
	ws, err := httpx.AcceptWS(c, nil)
	if err != nil {
		h.leave(v)
		return nil
	}
	defer func() {
		h.leave(v)
		h.publishChanged(l.OwnerID, sess.ID)
	}()
	// Revoked between the lookup and the registration? (revoke deletes the row, then disconnects registered viewers.)
	if _, err := s.d.Store.ShareLinks.Get(ctx, l.ID); err != nil {
		closeWS(ws, closeShareEnded, errShareRevoked.Error())
		return nil
	}

	actx, acancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	_, _ = s.d.Store.DB.ExecContext(actx, `UPDATE recording_share_meta SET uses = uses + 1, last_used_at = ? WHERE share_id = ?`,
		s.now().UnixMilli(), l.ID)
	acancel()
	s.d.Audit.LogUser(ctx, user, "share.join", l.ID, map[string]any{"sessionId": sess.ID, "ownerId": l.OwnerID,
		"mode": l.Mode, "viewerId": v.info.ID, "ip": ip})
	who := "An anonymous viewer"
	if user != nil {
		who = user.Username
	}
	mode := "read-only"
	if v.write {
		mode = "interactive"
		if h.isPaused(l.ID) {
			mode += " (guest input paused)"
		}
	}
	if s.d.Events != nil {
		s.d.Events.Publish(l.OwnerID, joinEvent{Type: model.EvNotify, Level: "info", Title: "Viewer joined " + sess.Title(),
			Message: who + " (" + ip + ") opened your " + mode + " share link."})
	}
	h.publishChanged(l.OwnerID, sess.ID)

	s.relayShare(ctx, ws, sess, l, v, user, offset)
	return nil
}

// closeWS closes a socket with a close frame, bounded (the peer may not answer).
func closeWS(ws *websocket.Conn, code websocket.StatusCode, reason string) {
	t := time.AfterFunc(2*time.Second, func() { _ = ws.CloseNow() })
	_ = ws.Close(code, reason)
	t.Stop()
}

const (
	shareGuestReadLimit = 1 << 20
	shareWriteTimeout   = 30 * time.Second
)

// relayShare connects a viewer's socket to the session through an in-memory WebSocket pair attached read-only to the
// terminal manager, so every frame passes this module's policy:
//
//   - downstream (session → viewer): output and control messages, minus metadata viewers do not need (cwd, state
//     error details); the read-only flag reflects the link mode and the owner's input pause;
//   - upstream (viewer → session): acks and pings pass; input, signals, breaks and reconnects only for interactive
//     links whose input is not paused (input is written with Manager.Write and attributed to the guest in the command
//     audit); resizes are ignored (viewers follow the owner's terminal size).
//
// It returns once the viewer's socket is closed: with closeShareEnded when the link was revoked or expired, with the
// terminal manager's close code when the session ended, abruptly when the viewer left.
func (s *Service) relayShare(ctx context.Context, ws *websocket.Conn, sess *term.Session, l *model.ShareLink, v *viewer,
	user *model.User, offset int64) {
	ws.SetReadLimit(shareGuestReadLimit)
	// Cancelling a context passed to a coder/websocket read or write closes the socket without a close frame, so the
	// viewer's socket only ever sees contexts that end with the relay (end), never the share's context itself.
	sockCtx, sockCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer sockCancel()
	// The relay's own context ends only through end (below), so the reason the share ended decides the close code.
	rctx, rcancel := context.WithCancel(context.WithoutCancel(ctx))
	defer rcancel()

	var (
		endOnce sync.Once
		ended   atomic.Bool
	)
	end := func(code websocket.StatusCode, reason string) {
		endOnce.Do(func() {
			ended.Store(true)
			rcancel()
			if code >= 0 {
				closeWS(ws, code, reason)
			} else {
				_ = ws.CloseNow()
			}
		})
	}
	push := func(msg []byte) {
		if ended.Load() {
			return
		}
		go func() {
			wctx, cancel := context.WithTimeout(sockCtx, shareWriteTimeout)
			defer cancel()
			_ = ws.Write(wctx, websocket.MessageText, msg)
		}()
	}

	srv, cli, err := wsPair(rctx)
	if err != nil {
		s.log.Warn("share relay failed", "err", err)
		end(websocket.StatusInternalError, "internal error")
		return
	}
	defer cli.CloseNow()
	s.shares.mu.Lock()
	v.push = push
	s.shares.mu.Unlock()

	attached := make(chan struct{})
	go func() {
		defer close(attached)
		// Returns (and closes srv) when rctx ends or the session closes.
		_ = s.sessions.Attach(rctx, sess.ID, srv, term.AttachOptions{Offset: offset, ReadOnly: true, User: user,
			Label: "share:" + l.ID})
	}()
	defer func() { <-attached }()

	// The share ends: revoked, expired, session closed, server shutting down.
	go func() {
		select {
		case <-ctx.Done():
			switch cause := context.Cause(ctx); {
			case errors.Is(cause, errShareRevoked), errors.Is(cause, errShareExpired):
				end(closeShareEnded, cause.Error())
			case errors.Is(cause, errShareSessionEnded):
				end(websocket.StatusNormalClosure, "session closed")
			default:
				end(websocket.StatusGoingAway, "server shutting down")
			}
		case <-rctx.Done():
		}
	}()

	// downstream
	downDone := make(chan struct{})
	go func() {
		defer close(downDone)
		for {
			typ, data, err := cli.Read(rctx)
			if err != nil {
				code := websocket.CloseStatus(err)
				var ce websocket.CloseError
				reason := ""
				if errors.As(err, &ce) {
					reason = ce.Reason
				}
				end(code, reason) // the terminal manager ended the stream (session closed / gone), or the relay ends
				return
			}
			if typ == websocket.MessageText {
				if data = s.filterShareCtrl(data, v); data == nil {
					continue
				}
			}
			wctx, cancel := context.WithTimeout(sockCtx, shareWriteTimeout)
			err = ws.Write(wctx, typ, data)
			cancel()
			if err != nil {
				end(-1, "")
				return
			}
		}
	}()

	// upstream (this goroutine)
	warned := false
	warn := func(msg string) {
		if !warned {
			warned = true
			push(ctrlError(msg))
		}
	}
read:
	for {
		typ, data, err := ws.Read(sockCtx)
		if err != nil {
			break
		}
		if typ == websocket.MessageBinary {
			if !v.canType(s.shares) {
				if v.write {
					warn("The owner paused guest input.")
				} else {
					warn("this view is read-only")
				}
				continue
			}
			warned = false
			if err := s.guestInput(sess, v, data); err != nil && !errors.Is(err, term.ErrNotConnected) {
				push(ctrlError(err.Error()))
			}
			continue
		}
		var m struct {
			Type   string `json:"type"`
			Offset int64  `json:"offset"`
			Name   string `json:"name"`
		}
		if json.Unmarshal(data, &m) != nil {
			push(ctrlError("invalid control message"))
			continue
		}
		switch m.Type {
		case "ack":
			msg, _ := json.Marshal(map[string]any{"type": "ack", "offset": m.Offset})
			if !writeCtx(rctx, cli, msg) {
				break read
			}
		case "ping":
			if !writeCtx(rctx, cli, []byte(`{"type":"ping"}`)) {
				break read
			}
		case "resize":
			// Viewers follow the owner's terminal size.
		case "signal", "break", "reconnect":
			if !v.canType(s.shares) {
				warn("this view is read-only")
				continue
			}
			var err error
			switch m.Type {
			case "signal":
				err = sess.Signal(m.Name)
			case "break":
				err = sess.Break()
			default:
				err = s.sessions.Reconnect(sess.ID)
			}
			if err != nil {
				push(ctrlError(err.Error()))
			}
		default:
			push(ctrlError("unknown message type"))
		}
	}
	end(-1, "") // the viewer left (no-op when the relay already ended)
	<-downDone
}

func writeCtx(ctx context.Context, c *websocket.Conn, msg []byte) bool {
	wctx, cancel := context.WithTimeout(ctx, shareWriteTimeout)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, msg) == nil
}

// filterShareCtrl adapts one terminal control message for a share viewer (nil = drop it).
func (s *Service) filterShareCtrl(data []byte, v *viewer) []byte {
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &head) != nil {
		return nil
	}
	switch head.Type {
	case "cwd":
		return nil // the owner's working directory is not part of what is shared
	case "readonly":
		return v.readonlyMsg(s.shares)
	case "state":
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			return nil
		}
		delete(m, "message") // connection error details (addresses, host names) stay with the owner
		out, err := json.Marshal(m)
		if err != nil {
			return nil
		}
		return out
	}
	return data
}

// guestInput writes a guest's keystrokes into the session, attributed to the guest in the command audit.
func (s *Service) guestInput(sess *term.Session, v *viewer, data []byte) error {
	st := s.states.get(sess, true)
	g := st.expectGuest(data, v.ref)
	err := s.sessions.Write(sess.ID, data)
	st.forgetGuest(g)
	return err
}

// ---- rate limiting ------------------------------------------------------------------------------------------------

// ipLimiter throttles the public share endpoints per client IP: a general request rate and a much lower budget for
// invalid tokens.
type ipLimiter struct {
	mu sync.Mutex
	m  map[string]*ipLimit
}

type ipLimit struct {
	req, fail *rate.Limiter
	seen      time.Time
}

func newIPLimiter() *ipLimiter { return &ipLimiter{m: map[string]*ipLimit{}} }

func (l *ipLimiter) get(ip string, now time.Time) *ipLimit {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[ip]
	if e == nil {
		if len(l.m) > 100000 {
			clear(l.m) // memory bound under a flood of addresses
		}
		e = &ipLimit{req: rate.NewLimiter(rate.Limit(1), 30), fail: rate.NewLimiter(rate.Every(30*time.Second), 10)}
		l.m[ip] = e
	}
	e.seen = now
	return e
}

func (l *ipLimiter) allow(ip string, now time.Time) error {
	e := l.get(ip, now)
	if e.fail.TokensAt(now) < 1 {
		return httpx.TooManyRequests("too many invalid share links from this address", 30)
	}
	if !e.req.AllowN(now, 1) {
		return httpx.TooManyRequests("too many requests", 5)
	}
	return nil
}

func (l *ipLimiter) failure(ip string, now time.Time) { l.get(ip, now).fail.AllowN(now, 1) }

func (l *ipLimiter) gc(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, e := range l.m {
		if now.Sub(e.seen) > 15*time.Minute {
			delete(l.m, ip)
		}
	}
}
