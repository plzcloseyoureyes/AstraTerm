package auth

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
)

// extraRoutes registers the endpoints added by the security module (SPEC §9 "security").
func (s *Service) extraRoutes() {
	pub, api, admin := s.d.Router.Public(), s.d.Router.API(), s.d.Router.Admin()

	pub.POST("/auth/mfa/totp", s.handleMFATOTP)
	pub.POST("/auth/mfa/enroll/totp/setup", s.handleEnrollTOTPSetup)
	pub.POST("/auth/mfa/enroll/totp/enable", s.handleEnrollTOTPEnable)

	api.GET("/auth/me", s.handleMe)
	api.PATCH("/auth/me", s.handleUpdateMe)
	api.POST("/auth/sessions/revoke-others", s.handleRevokeOtherSessions)

	admin.GET("/admin/auth/policy", s.handleGetPolicy)
	admin.PUT("/admin/auth/policy", s.handlePutPolicy)
	admin.POST("/admin/users/:id/unlock", s.handleUnlockUser)
	admin.POST("/admin/users/:id/reset-mfa", s.handleResetMFA)
	admin.POST("/admin/users/:id/revoke-sessions", s.handleRevokeUserSessions)
	admin.GET("/admin/system", s.handleSystemInfo)
	admin.GET("/admin/audit/search", s.handleAuditSearch)
	admin.GET("/admin/audit/export", s.handleAuditExport)
}

// CookieSecure reports whether cookies set on this request must carry the Secure attribute (TLS or a trusted
// proxy that terminated TLS).
func (s *Service) CookieSecure(c *echo.Context) bool { return s.secureCookies(c) }

// ---- current account ------------------------------------------------------------------------------------------------

type meResponse struct {
	User              *model.User `json:"user"`
	HasPassword       bool        `json:"hasPassword"`
	PasswordChangedAt *time.Time  `json:"passwordChangedAt,omitempty"`
	Passkeys          int         `json:"passkeys"`
	RecoveryCodesLeft int         `json:"recoveryCodesLeft"`
	MFARequired       bool        `json:"mfaRequired"`
	// ReauthFresh: sensitive changes are authorized without asking again (recent sign-in or re-verification).
	ReauthFresh bool   `json:"reauthFresh"`
	AuthMethod  string `json:"authMethod"`
}

func (s *Service) handleMe(c *echo.Context) error {
	ctx := c.Request().Context()
	u := httpx.UserFrom(c)
	ua, err := s.d.Store.Users.GetAuth(ctx, u.ID)
	if err != nil {
		return err
	}
	m, err := s.userMeta(ctx, u.ID)
	if err != nil {
		return err
	}
	resp := meResponse{User: ua.User, HasPassword: m.PasswordSet, MFARequired: s.Policy(ctx).mfaRequiredFor(ua.User),
		AuthMethod: httpx.AuthInfoFrom(c).Method}
	if !m.PasswordChangedAt.IsZero() {
		t := m.PasswordChangedAt
		resp.PasswordChangedAt = &t
	}
	if ua.User.TOTPEnabled {
		resp.RecoveryCodesLeft = len(ua.RecoveryHashes)
	}
	if p := s.passkeyProvider(); p != nil {
		if resp.Passkeys, err = p.CountCredentials(ctx, u.ID); err != nil {
			return err
		}
	}
	resp.ReauthFresh = s.RequireRecentAuth(c, "") == nil
	return c.JSON(http.StatusOK, resp)
}

func (s *Service) handleUpdateMe(c *echo.Context) error {
	var req struct {
		DisplayName *string `json:"displayName"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	ctx := c.Request().Context()
	u, err := s.d.Store.Users.Get(ctx, httpx.UserFrom(c).ID)
	if err != nil {
		return err
	}
	if req.DisplayName != nil {
		u.DisplayName = cleanDisplayName(*req.DisplayName, u.Username)
		if err := s.d.Store.Users.Update(ctx, u); err != nil {
			return err
		}
		s.d.Audit.Log(c, "auth.profile.update", u.ID, map[string]string{"displayName": u.DisplayName})
	}
	return c.JSON(http.StatusOK, u)
}

func (s *Service) handleRevokeOtherSessions(c *echo.Context) error {
	u := httpx.UserFrom(c)
	ctx := c.Request().Context()
	current := httpx.AuthInfoFrom(c).SessionID
	list, err := s.d.Store.AuthSessions.ListByUser(ctx, u.ID)
	if err != nil {
		return err
	}
	n := 0
	for _, se := range list {
		if se.ID != current {
			n++
		}
	}
	if err := s.d.Store.AuthSessions.DeleteAllForUser(ctx, u.ID, current); err != nil {
		return err
	}
	s.closeOtherSockets(u.ID, current)
	s.d.Audit.Log(c, "auth.session.revoke_others", u.ID, map[string]int{"revoked": n})
	return c.JSON(http.StatusOK, map[string]int{"revoked": n})
}

// ---- admin: account actions -----------------------------------------------------------------------------------------

func (s *Service) handleUnlockUser(c *echo.Context) error {
	id := c.Param("id")
	ctx := c.Request().Context()
	if _, err := s.d.Store.Users.Get(ctx, id); err != nil {
		return err
	}
	if err := s.clearLoginFailures(ctx, id, true); err != nil {
		return err
	}
	s.d.Audit.Log(c, "admin.user.unlock", id, nil)
	return httpx.OK(c)
}

type resetMFARequest struct {
	TOTP     *bool `json:"totp"`
	Passkeys *bool `json:"passkeys"`
}

// handleResetMFA removes a user's second factors (lost phone / security key): TOTP and/or passkeys (both by default).
func (s *Service) handleResetMFA(c *echo.Context) error {
	var req resetMFARequest
	if err := httpx.BindOptional(c, &req); err != nil {
		return err
	}
	id := c.Param("id")
	ctx := c.Request().Context()
	u, err := s.d.Store.Users.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.RequireAdminSudo(c); err != nil {
		return err
	}
	out := map[string]any{"totpReset": false, "passkeysRemoved": 0}
	if req.TOTP == nil || *req.TOTP {
		if u.TOTPEnabled {
			if err := s.d.Store.Users.SetTOTP(ctx, id, nil, false, nil); err != nil {
				return err
			}
			out["totpReset"] = true
		}
	}
	if req.Passkeys == nil || *req.Passkeys {
		if p := s.passkeyProvider(); p != nil {
			n, err := p.DeleteAll(ctx, id)
			if err != nil {
				return err
			}
			out["passkeysRemoved"] = n
		}
	}
	s.d.Audit.Log(c, "admin.user.reset_mfa", id, out)
	return c.JSON(http.StatusOK, out)
}

func (s *Service) handleRevokeUserSessions(c *echo.Context) error {
	id := c.Param("id")
	ctx := c.Request().Context()
	if _, err := s.d.Store.Users.Get(ctx, id); err != nil {
		return err
	}
	keep := ""
	if id == httpx.UserFrom(c).ID {
		keep = httpx.AuthInfoFrom(c).SessionID
	}
	if err := s.d.Store.AuthSessions.DeleteAllForUser(ctx, id, keep); err != nil {
		return err
	}
	if keep == "" {
		s.d.Events.CloseUser(id)
	} else {
		s.closeOtherSockets(id, keep)
	}
	s.d.Audit.Log(c, "admin.user.revoke_sessions", id, nil)
	return httpx.OK(c)
}

// ---- admin: system information --------------------------------------------------------------------------------------

type systemInfo struct {
	Version         string          `json:"version"`
	Mode            string          `json:"mode"`
	DataDir         string          `json:"dataDir"`
	Listen          string          `json:"listen"`
	TLS             bool            `json:"tls"`
	TLSSelfSigned   bool            `json:"tlsSelfSigned"`
	InsecureHTTP    bool            `json:"insecureHttp"`
	TrustedProxies  []string        `json:"trustedProxies"`
	Guacd           string          `json:"guacd"`
	GoVersion       string          `json:"goVersion"`
	OS              string          `json:"os"`
	Arch            string          `json:"arch"`
	CPUs            int             `json:"cpus"`
	PID             int             `json:"pid"`
	Hostname        string          `json:"hostname"`
	StartedAt       time.Time       `json:"startedAt"`
	UptimeSec       int64           `json:"uptimeSec"`
	DBSizeBytes     int64           `json:"dbSizeBytes"`
	Users           int             `json:"users"`
	Admins          int             `json:"admins"`
	LoginSessions   int             `json:"loginSessions"`
	EventClients    int             `json:"eventClients"`
	VaultLocked     bool            `json:"vaultLocked"`
	VaultMasterPass bool            `json:"vaultHasMasterPassword"`
	DetachedTTLSec  int64           `json:"detachedTtlSec"`
	ScrollbackBytes int             `json:"scrollbackBytes"`
	Features        map[string]bool `json:"features"`
}

func (s *Service) handleSystemInfo(c *echo.Context) error {
	ctx := c.Request().Context()
	cfg := s.d.Cfg
	info := systemInfo{
		Version: cfg.Version, Mode: cfg.Mode, DataDir: cfg.DataDir, Listen: cfg.Listen, TLS: cfg.TLSEnabled(),
		TLSSelfSigned: cfg.TLSSelfSigned, InsecureHTTP: cfg.InsecureHTTP, TrustedProxies: []string{}, Guacd: cfg.Guacd,
		GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), PID: os.Getpid(),
		StartedAt: s.startedAt.UTC(), UptimeSec: int64(time.Since(s.startedAt).Seconds()),
		VaultLocked: s.d.Vault.Locked(), VaultMasterPass: s.d.Vault.HasMasterPassword(),
		DetachedTTLSec: int64(cfg.DetachedSessionTTL / time.Second), ScrollbackBytes: cfg.ScrollbackBytes,
		EventClients: s.d.Events.ClientCount(), Features: s.features(ctx),
	}
	info.Hostname, _ = os.Hostname()
	for _, p := range cfg.TrustedProxies {
		info.TrustedProxies = append(info.TrustedProxies, p.String())
	}
	for _, f := range []string{cfg.DBPath(), cfg.DBPath() + "-wal"} {
		if st, err := os.Stat(f); err == nil {
			info.DBSizeBytes += st.Size()
		}
	}
	var err error
	if info.Users, err = s.d.Store.Users.Count(ctx); err != nil {
		return err
	}
	if info.Admins, err = s.d.Store.Users.CountActiveAdmins(ctx); err != nil {
		return err
	}
	counts, err := s.countSessions(ctx)
	if err != nil {
		return err
	}
	for _, n := range counts {
		info.LoginSessions += n
	}
	return c.JSON(http.StatusOK, info)
}

// ---- admin: audit search & export (REC-4) -----------------------------------------------------------------------------

const (
	auditPage        = 500
	auditScanLimit   = 50000  // rows examined per search request
	auditExportLimit = 200000 // rows written per export
)

// parseAuditFilter reads the same filters as GET /api/admin/audit (limit, before, userId, action, target, since,
// until) plus q, a case-insensitive text search over action, target, user name, IP and details.
func parseAuditFilter(c *echo.Context) (store.AuditFilter, string, error) {
	f := store.AuditFilter{Limit: 100, Action: c.QueryParam("action"), Target: c.QueryParam("target"), UserID: c.QueryParam("userId")}
	if v := c.QueryParam("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			return f, "", httpx.BadRequest("limit must be between 1 and 1000")
		}
		f.Limit = n
	}
	if v := c.QueryParam("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return f, "", httpx.BadRequest("before must be an entry id")
		}
		f.Before = n
	}
	for name, dst := range map[string]*time.Time{"since": &f.Since, "until": &f.Until} {
		if v := c.QueryParam(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return f, "", httpx.BadRequest(name + " must be an RFC3339 timestamp")
			}
			*dst = t
		}
	}
	q := strings.ToLower(strings.TrimSpace(c.QueryParam("q")))
	if len(q) > 200 {
		return f, "", httpx.BadRequest("q is too long")
	}
	return f, q, nil
}

func auditMatches(e *model.AuditEntry, q string) bool {
	if q == "" {
		return true
	}
	for _, v := range []string{e.Action, e.Target, e.Username, e.UserID, e.IP, string(e.Details)} {
		if strings.Contains(strings.ToLower(v), q) {
			return true
		}
	}
	return false
}

// scanAudit walks entries newest first (in pages) calling fn for every entry matching q until fn returns false or
// maxRows were examined. It returns the id of the last examined entry and whether older entries remain.
func (s *Service) scanAudit(ctx context.Context, f store.AuditFilter, q string, maxRows int, fn func(e *model.AuditEntry) bool) (int64, bool, error) {
	scanned := 0
	var last int64
	for {
		page := f
		page.Limit = auditPage
		entries, err := s.d.Store.Audit.List(ctx, page)
		if err != nil {
			return last, false, err
		}
		for i := range entries {
			e := &entries[i]
			last = e.ID
			scanned++
			if auditMatches(e, q) && !fn(e) {
				return last, true, nil
			}
			if scanned >= maxRows {
				return last, i < len(entries)-1 || len(entries) == auditPage, nil
			}
		}
		if len(entries) < auditPage {
			return last, false, nil
		}
		f.Before = last
		if err := ctx.Err(); err != nil {
			return last, false, err
		}
	}
}

type auditSearchResponse struct {
	Entries []model.AuditEntry `json:"entries"`
	// NextBefore continues the search (the id of the last entry examined); absent when nothing older remains.
	NextBefore int64 `json:"nextBefore,omitempty"`
}

func (s *Service) handleAuditSearch(c *echo.Context) error {
	f, q, err := parseAuditFilter(c)
	if err != nil {
		return err
	}
	limit := f.Limit
	out := auditSearchResponse{Entries: []model.AuditEntry{}}
	last, more, err := s.scanAudit(c.Request().Context(), f, q, auditScanLimit, func(e *model.AuditEntry) bool {
		out.Entries = append(out.Entries, *e)
		return len(out.Entries) < limit
	})
	if err != nil {
		return err
	}
	if more {
		out.NextBefore = last
	}
	return c.JSON(http.StatusOK, out)
}

// csvCell neutralizes spreadsheet formula injection (OWASP "CSV injection"): a cell whose first character — or first
// character after leading white space — is =, +, -, @ (or their full-width forms), tab, CR or LF is prefixed with a
// single quote so spreadsheet applications show it as text instead of evaluating it.
func csvCell(v string) string {
	if v == "" {
		return v
	}
	if strings.ContainsRune("\t\r\n", rune(v[0])) {
		return "'" + v
	}
	t := strings.TrimLeft(v, " \t\r\n\u00a0\u3000")
	if t == "" {
		return v
	}
	if r, _ := utf8.DecodeRuneInString(t); strings.ContainsRune("=+-@\uff1d\uff0b\uff0d\uff20", r) {
		return "'" + v
	}
	return v
}

func (s *Service) handleAuditExport(c *echo.Context) error {
	f, q, err := parseAuditFilter(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"id", "time", "user", "user_id", "action", "target", "ip", "details"})
	rows := 0
	_, _, err = s.scanAudit(ctx, f, q, auditExportLimit, func(e *model.AuditEntry) bool {
		details := ""
		if len(e.Details) > 0 {
			var compact bytes.Buffer
			if json.Compact(&compact, e.Details) == nil {
				details = compact.String()
			}
		}
		_ = w.Write([]string{strconv.FormatInt(e.ID, 10), e.TS.UTC().Format(time.RFC3339), csvCell(e.Username), csvCell(e.UserID),
			csvCell(e.Action), csvCell(e.Target), csvCell(e.IP), csvCell(details)})
		rows++
		return true
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	w.Flush()
	name := "astraterm-audit-" + time.Now().UTC().Format("20060102-150405") + ".csv"
	h := c.Response().Header()
	h.Set(echo.HeaderContentDisposition, `attachment; filename="`+name+`"`)
	h.Set(echo.HeaderCacheControl, "no-store")
	h.Set("X-Audit-Rows", strconv.Itoa(rows))
	s.d.Audit.Log(c, "admin.audit.export", "", map[string]any{"rows": rows, "q": q, "action": f.Action, "userId": f.UserID})
	return c.Blob(http.StatusOK, "text/csv; charset=utf-8", buf.Bytes())
}
