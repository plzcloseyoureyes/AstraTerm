package recording

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/httpx"
)

// Debug log viewer (REC-9): an in-memory ring of recent log records behind GET /api/admin/logs (admin).
//
// The ring is filled by a slog handler that tees records into it. CaptureLogs(h) is the exported hook: the server
// builds its root logger with slog.New(recording.CaptureLogs(handler)) so every record — including the HTTP request
// log and modules that derived loggers before this module mounted — is captured. Without that hook, Mount wraps
// d.Log, which captures the modules mounted afterwards and code logging through d.Log at run time. Secrets are
// redacted (attributes named like passwords / tokens / secrets, share-link tokens and launch/setup tokens in values).
// Admins can switch the capture off or change its level (policy debugLog / debugLogLevel).

const (
	logRingSize    = 5000
	maxLogMsgBytes = 2048
	maxLogAttrs    = 32
	maxLogValBytes = 1024
)

// LogEntry is one captured log record.
type LogEntry struct {
	ID     int64      `json:"id"`
	TS     time.Time  `json:"ts"`
	Level  string     `json:"level"`
	Msg    string     `json:"msg"`
	Module string     `json:"module,omitempty"`
	Attrs  []LogField `json:"attrs,omitempty"`
}

// LogField is one attribute of a LogEntry.
type LogField struct {
	K string `json:"k"`
	V string `json:"v"`
}

type logRingBuf struct {
	mu      sync.Mutex
	buf     []LogEntry
	next    int
	full    bool
	lastID  int64
	dropped int64 // overwritten entries
}

var (
	debugRing    = &logRingBuf{buf: make([]LogEntry, logRingSize)}
	captureOn    atomic.Bool
	captureLevel atomic.Int64
	rootHooked   atomic.Bool // CaptureLogs was installed by the server (full capture)
)

func init() {
	captureOn.Store(true)
	captureLevel.Store(int64(slog.LevelInfo))
}

func (r *logRingBuf) add(e LogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastID++
	e.ID = r.lastID
	if r.full {
		r.dropped++
	}
	r.buf[r.next] = e
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

// entries returns the stored entries with ID > after, oldest first.
func (r *logRingBuf) entries(after int64) []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []LogEntry
	appendFrom := func(part []LogEntry) {
		for _, e := range part {
			if e.ID > after {
				out = append(out, e)
			}
		}
	}
	if r.full {
		appendFrom(r.buf[r.next:])
	}
	appendFrom(r.buf[:r.next])
	return out
}

func (r *logRingBuf) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.buf)
	r.next, r.full, r.dropped = 0, false, 0
}

func (r *logRingBuf) stats() (lastID, dropped int64, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n = r.next
	if r.full {
		n = len(r.buf)
	}
	return r.lastID, r.dropped, n
}

func parseLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, true
	case "info", "":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return 0, false
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	}
	return "debug"
}

func applyCaptureLevel(p Policy) {
	captureOn.Store(p.DebugLog)
	if l, ok := parseLevel(p.DebugLogLevel); ok {
		captureLevel.Store(int64(l))
	}
}

func capturing(l slog.Level) bool { return captureOn.Load() && int64(l) >= captureLevel.Load() }

// captureHandler tees records into the debug ring.
type captureHandler struct {
	base   slog.Handler
	attrs  []LogField
	module string
	groups string // "a.b." prefix for attribute keys
}

// CaptureLogs wraps h so its records are also kept in the debug log ring (idempotent). The server may install it at
// the root of the process logger (see the package note).
func CaptureLogs(h slog.Handler) slog.Handler {
	if ch, ok := h.(*captureHandler); ok {
		return ch
	}
	return &captureHandler{base: h}
}

// MarkRootCapture records that the process logger was built with CaptureLogs (reported by the API).
func MarkRootCapture() { rootHooked.Store(true) }

func installDebugCapture(d *app.Deps) {
	if d.Log == nil {
		d.Log = slog.New(CaptureLogs(slog.DiscardHandler))
	} else if _, ok := d.Log.Handler().(*captureHandler); ok {
		rootHooked.Store(true)
	} else {
		d.Log = slog.New(CaptureLogs(d.Log.Handler()))
	}
	if d.Router != nil {
		// Without the root hook the HTTP request log (the router's own logger) is not captured: record request
		// summaries here instead (skipped as soon as the root hook is installed).
		d.Router.Echo().Pre(httpCapture)
	}
}

// httpCapture records one "http" entry per API / WebSocket request in the debug ring, at the level the router's
// request log uses (5xx error, 4xx warn, other mutations info, reads debug). Share tokens are redacted.
func httpCapture(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if rootHooked.Load() || !captureOn.Load() {
			return next(c)
		}
		start := time.Now()
		err := next(c)
		req := c.Request()
		p := req.URL.Path
		if !strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/ws/") {
			return err
		}
		status := 0
		if resp, uerr := echo.UnwrapResponse(c.Response()); uerr == nil {
			status = resp.Status
		}
		if err != nil {
			status = http.StatusInternalServerError
			var sc interface{ StatusCode() int }
			if errors.As(err, &sc) {
				status = sc.StatusCode()
			}
		}
		level := slog.LevelDebug
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		case req.Method != http.MethodGet && req.Method != http.MethodHead:
			level = slog.LevelInfo
		}
		if !capturing(level) {
			return err
		}
		e := LogEntry{TS: start.UTC(), Level: levelName(level), Msg: "http", Module: "http", Attrs: []LogField{
			{K: "method", V: req.Method},
			{K: "path", V: redactValue(p)},
			{K: "status", V: strconv.Itoa(status)},
			{K: "dur", V: time.Since(start).Round(time.Microsecond).String()},
			{K: "ip", V: httpx.ClientIP(c)},
		}}
		if u := httpx.UserFrom(c); u != nil {
			e.Attrs = append(e.Attrs, LogField{K: "user", V: u.Username})
		}
		if err != nil && status >= 500 {
			e.Attrs = append(e.Attrs, LogField{K: "err", V: truncBytes(redactValue(err.Error()), maxLogValBytes)})
		}
		debugRing.add(e)
		return err
	}
}

func (h *captureHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.base.Enabled(ctx, l) || capturing(l)
}

func (h *captureHandler) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if h.base.Enabled(ctx, r.Level) {
		err = h.base.Handle(ctx, r)
	}
	if capturing(r.Level) {
		debugRing.add(h.entry(r))
	}
	return err
}

func (h *captureHandler) WithAttrs(as []slog.Attr) slog.Handler {
	nh := &captureHandler{base: h.base.WithAttrs(as), module: h.module, groups: h.groups,
		attrs: append([]LogField(nil), h.attrs...)}
	for _, a := range as {
		if h.groups == "" && a.Key == "module" {
			nh.module = a.Value.String()
			continue
		}
		nh.attrs = appendAttr(nh.attrs, h.groups, a)
	}
	return nh
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &captureHandler{base: h.base.WithGroup(name), attrs: h.attrs, module: h.module, groups: h.groups + name + "."}
}

func (h *captureHandler) entry(r slog.Record) LogEntry {
	e := LogEntry{TS: r.Time.UTC(), Level: levelName(r.Level), Msg: truncBytes(redactValue(r.Message), maxLogMsgBytes),
		Module: h.module, Attrs: append([]LogField(nil), h.attrs...)}
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	r.Attrs(func(a slog.Attr) bool {
		if h.groups == "" && a.Key == "module" && e.Module == "" {
			e.Module = a.Value.String()
			return true
		}
		e.Attrs = appendAttr(e.Attrs, h.groups, a)
		return len(e.Attrs) < maxLogAttrs
	})
	return e
}

var secretKey = regexp.MustCompile(`(?i)(pass(word|wd|phrase)?|secret|token|cookie|authorization|credential|private|otp|totp|session_?key|api_?key|access_?key|signature|^auth$|^key$|^pin$)`)

var secretValue = []struct {
	re   *regexp.Regexp
	repl string
}{
	// share links, and the capability key of path-mode web proxies (/proxy/<id>-<key>/)
	{regexp.MustCompile(`(/share/|/ws/share/|/api/share/)[A-Za-z0-9_\-]{16,}`), "${1}[redacted]"},
	{regexp.MustCompile(`(/proxy/[a-z2-7]{20}-)[A-Za-z0-9_\-]+`), "${1}[redacted]"},
	// query parameters carrying credentials (?launch=, ?token=, ?__termstead_proxy_token=, ?api_key=, ?password=…)
	{regexp.MustCompile(`(?i)([?&][A-Za-z0-9_.\-]*(?:launch|setup|token|password|passwd|secret|api_?key|access_?key|signature|sig)=)[^&\s"]+`), "${1}[redacted]"},
	{regexp.MustCompile(`(?i)((?:bearer|basic)\s+)[A-Za-z0-9._~+/\-]+=*`), "${1}[redacted]"},
	// user:password@ in URLs
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^/\s:@]+:)[^/\s@]+@`), "${1}[redacted]@"},
}

func redactValue(v string) string {
	for _, r := range secretValue {
		v = r.re.ReplaceAllString(v, r.repl)
	}
	return v
}

func appendAttr(dst []LogField, prefix string, a slog.Attr) []LogField {
	if len(dst) >= maxLogAttrs {
		return dst
	}
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, g := range a.Value.Group() {
			dst = appendAttr(dst, p, g)
		}
		return dst
	}
	if a.Key == "" {
		return dst
	}
	key := prefix + a.Key
	val := logValueString(a.Value)
	if secretKey.MatchString(a.Key) {
		val = "[redacted]"
	} else {
		val = redactValue(val)
	}
	return append(dst, LogField{K: key, V: truncBytes(val, maxLogValBytes)})
}

// logValueString renders an attribute value for the ring. Arbitrary Go values (structs, maps, pointers — e.g. a
// connection with its secrets logged by mistake) are never formatted field by field: only their type is shown, unless
// they are errors, Stringers or simple values.
func logValueString(v slog.Value) string {
	if v.Kind() != slog.KindAny {
		return v.String()
	}
	switch x := v.Any().(type) {
	case nil:
		return "<nil>"
	case error:
		return x.Error()
	case fmt.Stringer:
		return x.String()
	case string:
		return x
	case []byte:
		return fmt.Sprintf("[%d bytes]", len(x))
	case []string:
		return strings.Join(x, ",")
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return fmt.Sprint(x)
	default:
		return fmt.Sprintf("<%T>", x)
	}
}

func truncBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// ---- REST ---------------------------------------------------------------------------------------------------------

type logsResponse struct {
	Entries  []LogEntry `json:"entries"`
	LastID   int64      `json:"lastId"`
	Stored   int        `json:"stored"`
	Capacity int        `json:"capacity"`
	Dropped  int64      `json:"dropped"`
	Enabled  bool       `json:"enabled"`
	Level    string     `json:"level"`
	// Source: "root" = every log record is captured; "partial" = only loggers derived after this module mounted.
	Source string `json:"source"`
}

func filterLogs(c *echo.Context) (func(LogEntry) bool, error) {
	minLevel := slog.LevelDebug
	if v := c.QueryParam("level"); v != "" {
		l, ok := parseLevel(v)
		if !ok {
			return nil, httpx.BadRequest("level must be debug, info, warn or error")
		}
		minLevel = l
	}
	q := strings.ToLower(strings.TrimSpace(c.QueryParam("q")))
	module := c.QueryParam("module")
	return func(e LogEntry) bool {
		if l, _ := parseLevel(e.Level); l < minLevel {
			return false
		}
		if module != "" && e.Module != module {
			return false
		}
		if q == "" {
			return true
		}
		if strings.Contains(strings.ToLower(e.Msg), q) || strings.Contains(strings.ToLower(e.Module), q) {
			return true
		}
		for _, a := range e.Attrs {
			if strings.Contains(strings.ToLower(a.K+"="+a.V), q) {
				return true
			}
		}
		return false
	}, nil
}

func (s *Service) handleLogs(c *echo.Context) error {
	keep, err := filterLogs(c)
	if err != nil {
		return err
	}
	var after int64
	if v := c.QueryParam("after"); v != "" {
		if after, err = strconv.ParseInt(v, 10, 64); err != nil || after < 0 {
			return httpx.BadRequest("after must be an entry id")
		}
	}
	limit := 1000
	if v := c.QueryParam("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > logRingSize {
			return httpx.BadRequest("limit must be between 1 and 5000")
		}
		limit = n
	}
	out := []LogEntry{}
	for _, e := range debugRing.entries(after) {
		if keep(e) {
			out = append(out, e)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	lastID, dropped, n := debugRing.stats()
	src := "partial"
	if rootHooked.Load() {
		src = "root"
	}
	return c.JSON(http.StatusOK, logsResponse{Entries: out, LastID: lastID, Stored: n, Capacity: logRingSize,
		Dropped: dropped, Enabled: captureOn.Load(), Level: levelName(slog.Level(captureLevel.Load())), Source: src})
}

func (s *Service) handleLogsExport(c *echo.Context) error {
	keep, err := filterLogs(c)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Termstead debug log export %s (version %s, %s mode); secrets redacted\n",
		time.Now().UTC().Format(time.RFC3339), versionOf(s), modeOf(s.d))
	for _, e := range debugRing.entries(0) {
		if !keep(e) {
			continue
		}
		fmt.Fprintf(&b, "%s %-5s", e.TS.Format("2006-01-02T15:04:05.000Z"), strings.ToUpper(e.Level))
		if e.Module != "" {
			fmt.Fprintf(&b, " [%s]", e.Module)
		}
		b.WriteString(" " + e.Msg)
		for _, a := range e.Attrs {
			v := a.V
			if strings.ContainsAny(v, " \t\"=") || v == "" {
				v = strconv.Quote(v)
			}
			b.WriteString(" " + a.K + "=" + v)
		}
		b.WriteByte('\n')
	}
	s.d.Audit.Log(c, "admin.logs.export", "", nil)
	h := c.Response().Header()
	h.Set(echo.HeaderContentDisposition, `attachment; filename="termstead-debug-`+time.Now().Format("20060102-150405")+`.log"`)
	h.Set(echo.HeaderCacheControl, "no-store")
	return c.Blob(http.StatusOK, "text/plain; charset=utf-8", []byte(b.String()))
}

func versionOf(s *Service) string {
	if s.d.Cfg != nil && s.d.Cfg.Version != "" {
		return s.d.Cfg.Version
	}
	return "dev"
}

func (s *Service) handleLogsClear(c *echo.Context) error {
	debugRing.clear()
	s.d.Audit.Log(c, "admin.logs.clear", "", nil)
	return httpx.OK(c)
}
