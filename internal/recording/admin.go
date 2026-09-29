package recording

import (
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
)

// Admin live session monitoring (MU-19):
//
//	GET  /api/admin/sessions                      every user's runtime sessions with owner, traffic and viewers
//	POST /api/admin/sessions/{id}/terminate {reason?}   force-close (notice in the terminal, owner notified, audited)
//	POST /api/admin/sessions/{id}/message {text}  a notice in the session's terminal + a notification to the owner
//
// Shadowing uses the terminal WebSocket's read-only admin attach (/ws/terminal/{id}, audited `session.shadow` by term).

// AdminSession is one row of the admin live sessions table.
type AdminSession struct {
	model.RuntimeSession
	Owner        AdminOwner `json:"owner"`
	BytesOut     int64      `json:"bytesOut"`
	BytesIn      int64      `json:"bytesIn"`
	DurationMs   int64      `json:"durationMs"`
	LastOutputAt *time.Time `json:"lastOutputAt,omitempty"`
	Shares       int        `json:"shares"`
	ShareViewers int        `json:"shareViewers"`
}

// AdminOwner identifies a session owner.
type AdminOwner struct {
	ID          string     `json:"id"`
	Username    string     `json:"username"`
	DisplayName string     `json:"displayName,omitempty"`
	Role        model.Role `json:"role,omitempty"`
}

func (s *Service) handleAdminSessions(c *echo.Context) error {
	ctx := c.Request().Context()
	users := map[string]*model.User{}
	if list, err := s.d.Store.Users.List(ctx); err == nil {
		for _, u := range list {
			users[u.ID] = u
		}
	}
	shareCount := map[string]int{}
	if rows, err := s.d.Store.DB.QueryContext(ctx, `SELECT session_id, COUNT(*) FROM share_links WHERE expires_at > ? GROUP BY session_id`,
		time.Now().UnixMilli()); err == nil {
		for rows.Next() {
			var (
				id string
				n  int
			)
			if rows.Scan(&id, &n) == nil {
				shareCount[id] = n
			}
		}
		rows.Close()
	}
	now := s.now()
	out := []AdminSession{}
	for _, sess := range s.sessions.List(httpx.UserFrom(c), true) {
		info := sess.Info()
		if info.State == model.StateClosed {
			continue
		}
		row := AdminSession{RuntimeSession: info, Owner: AdminOwner{ID: info.OwnerID}}
		if u := users[info.OwnerID]; u != nil {
			row.Owner.Username, row.Owner.DisplayName, row.Owner.Role = u.Username, u.DisplayName, u.Role
		} else if o := sess.Owner(); o != nil {
			row.Owner.Username = o.Username
		}
		if sess.Kind == model.KindTerminal {
			_, row.BytesOut = sess.Offsets()
		}
		if st := s.states.lookup(sess.ID); st != nil {
			row.BytesIn = st.inputBytes()
			st.mu.Lock()
			if !st.lastSeen.IsZero() {
				t := st.lastSeen.UTC()
				row.LastOutputAt = &t
			}
			st.mu.Unlock()
		}
		row.DurationMs = max(now.Sub(info.CreatedAt).Milliseconds(), 0)
		row.Shares = shareCount[sess.ID]
		row.ShareViewers = s.shares.sessionViewers(sess.ID)
		out = append(out, row)
	}
	return c.JSON(http.StatusOK, out)
}

type notifyEvent struct {
	Type    string `json:"type"`
	Level   string `json:"level"`
	Title   string `json:"title"`
	Message string `json:"message,omitempty"`
}

func cleanText(s string, maxRunes int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s))
	if utf8.RuneCountInString(s) > maxRunes {
		s = string([]rune(s)[:maxRunes])
	}
	return s
}

func (s *Service) handleAdminTerminate(c *echo.Context) error {
	var body struct {
		Reason string `json:"reason"`
	}
	if err := httpx.BindOptional(c, &body); err != nil {
		return err
	}
	id := c.Param("id")
	if !model.ValidID(id) {
		return httpx.ErrNotFound
	}
	sess := s.sessions.Get(id)
	if sess == nil || sess.Closed() {
		return httpx.ErrNotFound
	}
	admin := httpx.UserFrom(c)
	reason := cleanText(body.Reason, 300)
	title := sess.Title()
	note := "Session terminated by an administrator"
	if reason != "" {
		note += ": " + reason
	}
	if sess.Kind == model.KindTerminal {
		sess.Notice(note)
	}
	s.d.Audit.Log(c, "admin.session.terminate", sess.ID, map[string]any{"ownerId": sess.OwnerID, "reason": reason,
		"protocol": sess.Protocol, "title": title})
	if err := s.sessions.CloseContext(c.Request().Context(), sess.ID); err != nil {
		return err
	}
	if s.d.Events != nil && sess.OwnerID != admin.ID {
		msg := "Closed by " + admin.Username
		if reason != "" {
			msg += ": " + reason
		}
		s.d.Events.Publish(sess.OwnerID, notifyEvent{Type: model.EvNotify, Level: "warning", Title: "Session “" + title + "” was terminated", Message: msg})
	}
	return httpx.OK(c)
}

func (s *Service) handleAdminMessage(c *echo.Context) error {
	var body struct {
		Text string `json:"text"`
	}
	if err := httpx.Bind(c, &body); err != nil {
		return err
	}
	text := cleanText(body.Text, 500)
	if text == "" {
		return httpx.BadRequest("text is required")
	}
	id := c.Param("id")
	if !model.ValidID(id) {
		return httpx.ErrNotFound
	}
	sess := s.sessions.Get(id)
	if sess == nil || sess.Closed() {
		return httpx.ErrNotFound
	}
	admin := httpx.UserFrom(c)
	if sess.Kind == model.KindTerminal {
		sess.Notice("Message from administrator " + admin.Username + ": " + text)
	}
	if s.d.Events != nil {
		s.d.Events.Publish(sess.OwnerID, notifyEvent{Type: model.EvNotify, Level: "info",
			Title: "Message from administrator " + admin.Username, Message: text})
	}
	s.d.Audit.Log(c, "admin.session.message", sess.ID, map[string]any{"ownerId": sess.OwnerID, "text": text})
	return httpx.OK(c)
}
