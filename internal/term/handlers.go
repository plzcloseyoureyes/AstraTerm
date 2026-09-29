package term

import (
	"context"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
)

// Mount registers the runtime session REST API (SPEC §6.0 "Runtime sessions") and /ws/terminal/:id (§6.2).
func Mount(d *app.Deps, m *Manager) error {
	api := d.Router.API()
	api.GET("/sessions", m.handleList)
	api.POST("/sessions", m.handleCreate)
	api.GET("/sessions/:id", m.handleGet)
	api.PATCH("/sessions/:id", m.handlePatch)
	api.DELETE("/sessions/:id", m.handleDelete)
	api.POST("/sessions/:id/reconnect", m.handleReconnect)
	api.POST("/sessions/:id/input", m.handleInput)
	api.POST("/sessions/:id/resize", m.handleResize)
	api.POST("/sessions/:id/signal", m.handleSignal)
	api.POST("/sessions/:id/break", m.handleBreak)
	api.POST("/sessions/:id/record", m.handleRecord)
	api.POST("/sessions/:id/log", m.handleLog)
	api.GET("/sessions/:id/scrollback", m.handleScrollback)
	d.Router.WS("/ws/terminal/:id", m.handleWS)
	return nil
}

// access levels for session lookups.
const (
	accessView  = iota // owner or admin (read-only for admins)
	accessClose        // owner or admin (force-disconnect)
	accessOwner        // owner only
)

// lookup returns the session named in the path if the current user may access it. Sessions of other users are
// reported as not found.
func (m *Manager) lookup(c *echo.Context, level int) (*Session, error) {
	u := httpx.UserFrom(c)
	if u == nil {
		return nil, httpx.ErrUnauthorized
	}
	s := m.Get(c.Param("id"))
	if s == nil {
		return nil, httpx.ErrNotFound
	}
	if s.OwnerID == u.ID {
		return s, nil
	}
	if u.IsAdmin() && (level == accessView || level == accessClose) {
		return s, nil
	}
	if u.IsAdmin() {
		return nil, httpx.Forbidden("only the session owner can do this")
	}
	return nil, httpx.ErrNotFound
}

func (m *Manager) handleList(c *echo.Context) error {
	all := c.QueryParam("all")
	sessions := m.List(httpx.UserFrom(c), all == "1" || all == "true")
	out := make([]model.RuntimeSession, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.Info())
	}
	return c.JSON(http.StatusOK, out)
}

func (m *Manager) handleCreate(c *echo.Context) error {
	var req CreateRequest
	if err := httpx.BindLimit(c, &req, 1<<20); err != nil {
		return err
	}
	req.Connection, req.Secrets = nil, nil
	s, err := m.Create(c.Request().Context(), httpx.UserFrom(c), req)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, s.Info())
}

func (m *Manager) handleGet(c *echo.Context) error {
	s, err := m.lookup(c, accessView)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s.Info())
}

func (m *Manager) handlePatch(c *echo.Context) error {
	s, err := m.lookup(c, accessOwner)
	if err != nil {
		return err
	}
	var body struct {
		Title *string `json:"title"`
	}
	if err := httpx.Bind(c, &body); err != nil {
		return err
	}
	if body.Title != nil {
		if utf8.RuneCountInString(*body.Title) > maxTitleRunes*4 {
			return httpx.BadRequest("title is too long")
		}
		if err := m.Rename(s.ID, *body.Title); err != nil {
			return err
		}
	}
	return c.JSON(http.StatusOK, s.Info())
}

func (m *Manager) handleDelete(c *echo.Context) error {
	s, err := m.lookup(c, accessClose)
	if err != nil {
		return err
	}
	m.closeSession(c.Request().Context(), s, httpx.UserFrom(c), "closed by user")
	return httpx.OK(c)
}

func (m *Manager) handleReconnect(c *echo.Context) error {
	s, err := m.lookup(c, accessOwner)
	if err != nil {
		return err
	}
	if err := s.reconnect(); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s.Info())
}

func (m *Manager) handleInput(c *echo.Context) error {
	s, err := m.lookup(c, accessOwner)
	if err != nil {
		return err
	}
	var body struct {
		Data string `json:"data"`
	}
	if err := httpx.BindLimit(c, &body, maxQueuedInput); err != nil {
		return err
	}
	if err := s.writeInput([]byte(body.Data), nil); err != nil {
		return err
	}
	return httpx.OK(c)
}

func (m *Manager) handleResize(c *echo.Context) error {
	s, err := m.lookup(c, accessOwner)
	if err != nil {
		return err
	}
	var body struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}
	if err := httpx.Bind(c, &body); err != nil {
		return err
	}
	if body.Cols <= 0 || body.Rows <= 0 {
		return httpx.BadRequest("cols and rows are required")
	}
	s.resize(body.Cols, body.Rows, nil)
	return c.JSON(http.StatusOK, s.Info())
}

func (m *Manager) handleSignal(c *echo.Context) error {
	s, err := m.lookup(c, accessOwner)
	if err != nil {
		return err
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := httpx.Bind(c, &body); err != nil {
		return err
	}
	if err := s.Signal(body.Name); err != nil {
		return err
	}
	return httpx.OK(c)
}

func (m *Manager) handleBreak(c *echo.Context) error {
	s, err := m.lookup(c, accessOwner)
	if err != nil {
		return err
	}
	if err := s.Break(); err != nil {
		return err
	}
	return httpx.OK(c)
}

func (m *Manager) handleRecord(c *echo.Context) error { return m.handleToggle(c, m.SetRecording) }

func (m *Manager) handleLog(c *echo.Context) error { return m.handleToggle(c, m.SetLogging) }

func (m *Manager) handleToggle(c *echo.Context, set func(ctx context.Context, id string, enabled bool) error) error {
	s, err := m.lookup(c, accessOwner)
	if err != nil {
		return err
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := httpx.Bind(c, &body); err != nil {
		return err
	}
	if body.Enabled == nil {
		return httpx.BadRequest("enabled is required")
	}
	if err := set(c.Request().Context(), s.ID, *body.Enabled); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s.Info())
}

// handleScrollback returns the ring buffer as text/plain: raw bytes by default (or ?raw=1), ANSI-stripped text with
// ?raw=0.
func (m *Manager) handleScrollback(c *echo.Context) error {
	s, err := m.lookup(c, accessView)
	if err != nil {
		return err
	}
	if s.Kind != model.KindTerminal {
		return httpx.BadRequest("not a terminal session")
	}
	if u := httpx.UserFrom(c); s.OwnerID != u.ID {
		m.audit(c.Request().Context(), u, "session.scrollback", s.ID, map[string]any{"ownerId": s.OwnerID})
	}
	data := s.Scrollback()
	switch c.QueryParam("raw") {
	case "0", "false", "no":
		data = []byte(StripText(data))
	}
	h := c.Response().Header()
	h.Set(echo.HeaderCacheControl, "no-store")
	h.Set(echo.HeaderContentLength, strconv.Itoa(len(data)))
	return c.Blob(http.StatusOK, "text/plain; charset=utf-8", data)
}

// handleWS serves /ws/terminal/:id?offset=<n>. The owner attaches read-write; admins may attach to other users'
// sessions read-only (shadowing, audited).
func (m *Manager) handleWS(c *echo.Context) error {
	s, err := m.lookup(c, accessView)
	if err != nil {
		return err
	}
	if s.Kind != model.KindTerminal {
		return httpx.BadRequest("not a terminal session")
	}
	var offset int64
	if v := c.QueryParam("offset"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return httpx.BadRequest("invalid offset")
		}
		offset = n
	}
	user := httpx.UserFrom(c)
	readOnly := s.OwnerID != user.ID
	ws, err := httpx.AcceptWS(c, nil)
	if err != nil {
		m.log.Debug("term: websocket accept failed", "err", err)
		return nil // the handshake error response has been written
	}
	ctx := c.Request().Context()
	if readOnly {
		m.audit(ctx, user, "session.shadow", s.ID, map[string]any{"ownerId": s.OwnerID})
	}
	_ = m.Attach(ctx, s.ID, ws, AttachOptions{Offset: offset, ReadOnly: readOnly, User: user, Shadow: readOnly})
	return nil
}
