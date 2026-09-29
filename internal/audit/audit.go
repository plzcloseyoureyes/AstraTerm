// Package audit records security-relevant actions in the append-only audit_log table and serves them over REST
// (GET /api/admin/audit, GET /api/audit/me).
package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
)

// Logger writes audit entries.
type Logger struct {
	st  *store.Store
	log *slog.Logger
}

// New creates an audit logger.
func New(st *store.Store, log *slog.Logger) *Logger {
	if log == nil {
		log = slog.Default()
	}
	return &Logger{st: st, log: log}
}

// Log records action on target. src is the *echo.Context, *http.Request or context.Context of the operation: the
// acting user and client IP are taken from it. details is any JSON-marshalable value (nil for none) and must never
// contain secrets.
func (l *Logger) Log(src any, action, target string, details any) {
	ctx := contextOf(src)
	l.LogUser(ctx, httpx.UserFromContext(ctx), action, target, details)
}

// LogUser records an action performed by (or on behalf of) an explicit user, e.g. a login before the user is attached
// to the request context. user may be nil for anonymous events (failed logins).
func (l *Logger) LogUser(src any, user *model.User, action, target string, details any) {
	ctx := contextOf(src)
	e := &model.AuditEntry{Action: action, Target: target, IP: httpx.ClientIPFrom(ctx)}
	if user != nil {
		e.UserID, e.Username = user.ID, user.Username
	}
	if details != nil {
		b, err := json.Marshal(details)
		if err != nil {
			l.log.Warn("audit: cannot encode details", "action", action, "err", err)
		} else {
			e.Details = b
		}
	}
	// Persist even if the request context is already cancelled (client went away mid-request).
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := l.st.Audit.Insert(wctx, e); err != nil {
		l.log.Error("audit: write failed", "action", action, "err", err)
	}
}

func contextOf(src any) context.Context {
	switch s := src.(type) {
	case *echo.Context:
		if s != nil {
			return s.Request().Context()
		}
	case *http.Request:
		if s != nil {
			return s.Context()
		}
	case context.Context:
		if s != nil {
			return s
		}
	}
	return context.Background()
}

// Mount registers the audit REST endpoints.
func Mount(r *httpx.Router, st *store.Store) {
	r.Admin().GET("/admin/audit", func(c *echo.Context) error {
		f, err := parseFilter(c)
		if err != nil {
			return err
		}
		f.UserID = c.QueryParam("userId")
		return list(c, st, f)
	})
	r.API().GET("/audit/me", func(c *echo.Context) error {
		f, err := parseFilter(c)
		if err != nil {
			return err
		}
		f.UserID = httpx.UserFrom(c).ID
		return list(c, st, f)
	})
}

func list(c *echo.Context, st *store.Store, f store.AuditFilter) error {
	entries, err := st.Audit.List(c.Request().Context(), f)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, entries)
}

// parseFilter reads limit, before (entry ID cursor), action, target, since and until (RFC3339) query parameters.
func parseFilter(c *echo.Context) (store.AuditFilter, error) {
	f := store.AuditFilter{Limit: 100, Action: c.QueryParam("action"), Target: c.QueryParam("target")}
	if v := c.QueryParam("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			return f, httpx.BadRequest("limit must be between 1 and 1000")
		}
		f.Limit = n
	}
	if v := c.QueryParam("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return f, httpx.BadRequest("before must be an entry id")
		}
		f.Before = n
	}
	for name, dst := range map[string]*time.Time{"since": &f.Since, "until": &f.Until} {
		if v := c.QueryParam(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return f, httpx.BadRequest(name + " must be an RFC3339 timestamp")
			}
			*dst = t
		}
	}
	return f, nil
}
