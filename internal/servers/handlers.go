package servers

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/httpx"
)

const maxConfigBody = 1 << 20

type handler struct {
	d *app.Deps
	m *Manager
}

func (h *handler) mount() {
	api := h.d.Router.API()
	route := func(method, path string, fn echo.HandlerFunc) { api.Add(method, path, fn, h.guard) }
	route(http.MethodGet, "/servers", h.list)
	route(http.MethodGet, "/servers/host", h.host)
	route(http.MethodPost, "/servers/stop-all", h.stopAll)
	route(http.MethodGet, "/servers/syslog/messages", h.syslogMessages)
	route(http.MethodDelete, "/servers/syslog/messages", h.syslogClear)
	route(http.MethodGet, "/servers/syslog/export", h.syslogExport)
	route(http.MethodGet, "/servers/:kind", h.get)
	route(http.MethodPut, "/servers/:kind", h.put)
	route(http.MethodPost, "/servers/:kind/start", h.start)
	route(http.MethodPost, "/servers/:kind/stop", h.stop)
	route(http.MethodPost, "/servers/:kind/restart", h.restart)
	route(http.MethodGet, "/servers/:kind/logs", h.logs)
	route(http.MethodDelete, "/servers/:kind/logs", h.clearLogs)
	route(http.MethodGet, "/servers/:kind/clients", h.clients)
	route(http.MethodDelete, "/servers/:kind/clients/:id", h.kick)
}

// guard enforces SPEC principle 7: embedded servers are admin-only in server mode.
func (h *handler) guard(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if err := h.m.allowed(httpx.UserFrom(c)); err != nil {
			return err
		}
		return next(c)
	}
}

func (h *handler) slot(c *echo.Context) (*slot, error) {
	k, ok := parseKind(c.Param("kind"))
	if !ok {
		return nil, httpx.NotFound("unknown server")
	}
	return h.m.slots[k], nil
}

func (h *handler) list(c *echo.Context) error {
	return c.JSON(http.StatusOK, h.m.statuses())
}

func (h *handler) host(c *echo.Context) error {
	return c.JSON(http.StatusOK, h.m.hostInfo())
}

func (h *handler) get(c *echo.Context) error {
	s, err := h.slot(c)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s.status())
}

func (h *handler) put(c *echo.Context) error {
	s, err := h.slot(c)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, maxConfigBody+1))
	if err != nil {
		return httpx.BadRequest("cannot read the request body")
	}
	if len(body) > maxConfigBody {
		return httpx.NewError(http.StatusRequestEntityTooLarge, "too_large", "configuration too large")
	}
	st, changed, err := s.configure(c.Request().Context(), body)
	if err != nil {
		if isInvalid(err) {
			return httpx.BadRequest(err.Error())
		}
		return err
	}
	if len(changed) > 0 {
		h.d.Audit.Log(c, "server.config", string(s.kind), map[string]any{"changed": changed})
	}
	return c.JSON(http.StatusOK, st)
}

func (h *handler) start(c *echo.Context) error {
	s, err := h.slot(c)
	if err != nil {
		return err
	}
	st, err := s.start(c.Request().Context())
	if err != nil {
		var se *startError
		if errors.As(err, &se) {
			return se.httpError()
		}
		return err
	}
	h.d.Audit.Log(c, "server.start", string(s.kind), map[string]any{"addr": st.Addrs})
	return c.JSON(http.StatusOK, st)
}

func (h *handler) stop(c *echo.Context) error {
	s, err := h.slot(c)
	if err != nil {
		return err
	}
	was := s.status().Running
	st := s.stop("")
	if was {
		h.d.Audit.Log(c, "server.stop", string(s.kind), nil)
	}
	return c.JSON(http.StatusOK, st)
}

func (h *handler) restart(c *echo.Context) error {
	s, err := h.slot(c)
	if err != nil {
		return err
	}
	st, err := s.restart(c.Request().Context())
	if err != nil {
		var se *startError
		if errors.As(err, &se) {
			return se.httpError()
		}
		return err
	}
	h.d.Audit.Log(c, "server.restart", string(s.kind), nil)
	return c.JSON(http.StatusOK, st)
}

func (h *handler) stopAll(c *echo.Context) error {
	var stopped []string
	for _, k := range Kinds {
		s := h.m.slots[k]
		if s.status().Running {
			s.stop("")
			stopped = append(stopped, string(k))
		}
	}
	if len(stopped) > 0 {
		h.d.Audit.Log(c, "server.stop_all", "", map[string]any{"kinds": stopped})
	}
	return c.JSON(http.StatusOK, h.m.statuses())
}

type logsReply struct {
	Entries []LogEntry `json:"entries"`
	LastID  int64      `json:"lastId"`
}

func (h *handler) logs(c *echo.Context) error {
	s, err := h.slot(c)
	if err != nil {
		return err
	}
	after, _ := strconv.ParseInt(c.QueryParam("after"), 10, 64)
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	entries, last := s.logs.since(after, limit)
	return c.JSON(http.StatusOK, logsReply{Entries: entries, LastID: last})
}

func (h *handler) clearLogs(c *echo.Context) error {
	s, err := h.slot(c)
	if err != nil {
		return err
	}
	s.logs.clear()
	return httpx.OK(c)
}

func (h *handler) clients(c *echo.Context) error {
	s, err := h.slot(c)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s.clientList())
}

func (h *handler) kick(c *echo.Context) error {
	s, err := h.slot(c)
	if err != nil {
		return err
	}
	if !s.kick(c.Param("id")) {
		return httpx.NotFound("no such client")
	}
	h.d.Audit.Log(c, "server.client.disconnect", string(s.kind), map[string]any{"client": c.Param("id")})
	return httpx.OK(c)
}

// ---- syslog -------------------------------------------------------------------------------------------------------

func parseSyslogQuery(c *echo.Context) (syslogQuery, error) {
	q := syslogQuery{maxSev: -1, facility: -1}
	q.text = strings.TrimSpace(c.QueryParam("q"))
	if q.text == "" {
		q.text = strings.TrimSpace(c.QueryParam("filter")) // alias
	}
	if len(q.text) > 512 {
		return q, httpx.BadRequest("the search text is too long")
	}
	if c.QueryParam("regex") == "1" && q.text != "" {
		re, err := regexp.Compile("(?i)" + q.text)
		if err != nil {
			return q, httpx.BadRequest("invalid regular expression: " + err.Error())
		}
		q.re = re
	}
	if v := c.QueryParam("severity"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 7 {
			return q, httpx.BadRequest("severity must be 0-7")
		}
		q.maxSev = n
	}
	if v := c.QueryParam("facility"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 23 {
			return q, httpx.BadRequest("facility must be 0-23")
		}
		q.facility = n
	}
	q.host = strings.TrimSpace(c.QueryParam("host"))
	q.app = strings.TrimSpace(c.QueryParam("app"))
	q.after, _ = strconv.ParseInt(c.QueryParam("after"), 10, 64)
	q.before, _ = strconv.ParseInt(c.QueryParam("before"), 10, 64)
	q.limit, _ = strconv.Atoi(c.QueryParam("limit"))
	return q, nil
}

func (h *handler) syslogMessages(c *echo.Context) error {
	q, err := parseSyslogQuery(c)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, h.m.syslog.query(q))
}

func (h *handler) syslogClear(c *echo.Context) error {
	h.m.syslog.clear()
	h.d.Audit.Log(c, "server.syslog.clear", string(KindSyslog), nil)
	h.m.slots[KindSyslog].changed()
	return httpx.OK(c)
}

func (h *handler) syslogExport(c *echo.Context) error {
	q, err := parseSyslogQuery(c)
	if err != nil {
		return err
	}
	q.limit = maxMsgLimit
	var all []SyslogMessage
	// Page backwards through the buffer (newest first) so exports are not capped at one page.
	for {
		page := h.m.syslog.query(q)
		all = append(page.Messages, all...)
		if !page.HasMore || len(page.Messages) == 0 || len(all) >= maxBufferSize {
			break
		}
		q.before = page.Messages[0].ID
	}
	w := c.Response()
	name := "syslog-" + time.Now().Format("20060102-150405") + ".log"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	bw := bufio.NewWriterSize(w, 64<<10)
	for i := range all {
		_, _ = bw.WriteString(formatSyslogLine(&all[i]))
		_ = bw.WriteByte('\n')
	}
	return bw.Flush()
}

// ---- slot helpers used by the handlers ----------------------------------------------------------------------------

func (s *slot) clientList() []ClientInfo {
	s.mu.Lock()
	in := s.inst
	s.mu.Unlock()
	if s.kind == KindSyslog {
		return s.m.syslog.recentSources()
	}
	if in == nil {
		return []ClientInfo{}
	}
	if cl, ok := in.svc.(clientLister); ok {
		return cl.clientList()
	}
	return in.clients.list()
}

func (s *slot) kick(id string) bool {
	s.mu.Lock()
	in := s.inst
	s.mu.Unlock()
	if in == nil {
		return false
	}
	return in.clients.kick(id)
}
