// Package auth implements accounts and authentication (SPEC §6.0 "Auth & account"): first-run setup, password login
// with argon2id and brute-force backoff, TOTP 2FA with recovery codes, opaque server-side session cookies, desktop
// launch tokens, API tokens (Bearer nxt_…), login-session management and admin user management. Mount installs the
// request Authenticator into the router.
package auth

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/store"
)

// Session cookie and lifetimes.
const (
	CookieName        = "termstead_session"
	SessionTTL        = 7 * 24 * time.Hour  // sliding
	RememberTTL       = 30 * 24 * time.Hour // sliding, persistent cookie
	touchInterval     = time.Minute
	cookieRefresh     = time.Hour
	maxCookieLen      = 256
	loginUserFailures = 5  // per IP+username before backoff
	loginIPFailures   = 20 // per IP before backoff
	passwordFailures  = 5  // per user for re-auth endpoints
	featureCacheTTL   = 10 * time.Second
)

// Service holds auth state. Create it with Mount.
type Service struct {
	d       *app.Deps
	log     *slog.Logger
	limiter *limiter

	launchMu    sync.Mutex
	launchToken string

	// setupToken guards first-run setup where the UI may be reachable by others (server mode or a non-loopback
	// bind): POST /api/auth/setup must present it. "" when not needed or once setup has completed.
	setupMu    sync.Mutex
	setupToken string

	adminMu sync.Mutex // serializes last-admin checks in user management

	featMu   sync.Mutex
	featAt   time.Time
	featVals map[string]bool

	policyMu sync.Mutex
	policy   *Policy // cached login policy (nil = reload)

	provMu   sync.RWMutex
	passkeys PasskeyProvider
	sso      SSOProvider

	mfaMu sync.Mutex
	mfa   map[string]*MFAChallenge // pending second-factor logins by token hash

	reauthMu sync.Mutex
	reauthAt map[string]time.Time // login session ID → last re-verification

	startedAt time.Time
}

// Mount creates the auth service, installs it as the router's Authenticator and registers all auth routes. In
// desktop mode a one-time launch token is generated (see LaunchToken).
func Mount(d *app.Deps) (*Service, error) {
	s := &Service{d: d, log: d.Log.With("module", "auth"), limiter: newLimiter(), mfa: map[string]*MFAChallenge{},
		reauthAt: map[string]time.Time{}, startedAt: time.Now()}
	if d.Cfg.IsDesktop() {
		s.launchToken = randomToken(32)
	}
	if d.Cfg.IsServer() || !d.Cfg.ListenIsLoopback() {
		// Whoever completes setup becomes the administrator: on an instance other people can reach, only the operator
		// (who sees the startup banner) may do it.
		n, err := d.Store.Users.Count(d.Ctx)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			s.setupToken = randomToken(32)
		}
	}
	d.Router.SetAuthenticator(s)
	s.routes()
	services.Store(d, s)
	go func() {
		<-d.Ctx.Done()
		services.Delete(d)
	}()
	go s.janitor()
	return s, nil
}

// LaunchToken returns the unused desktop launch token ("" in server mode or once consumed).
func (s *Service) LaunchToken() string {
	s.launchMu.Lock()
	defer s.launchMu.Unlock()
	return s.launchToken
}

// SetupToken returns the one-time token that first-run setup requires in server mode (and on non-loopback binds);
// "" when setup needs no token or has completed. The startup banner prints it as the ?setup= link.
func (s *Service) SetupToken() string {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	return s.setupToken
}

func (s *Service) routes() {
	pub, api, admin := s.d.Router.Public(), s.d.Router.API(), s.d.Router.Admin()
	pub.GET("/auth/state", s.handleState)
	pub.POST("/auth/setup", s.handleSetup)
	pub.POST("/auth/login", s.handleLogin)
	pub.POST("/auth/logout", s.handleLogout)
	pub.POST("/auth/launch", s.handleLaunch)
	api.POST("/auth/password", s.handlePassword)
	api.POST("/auth/verify-password", s.handleVerifyPassword)

	api.POST("/auth/totp/setup", s.handleTOTPSetup)
	api.POST("/auth/totp/enable", s.handleTOTPEnable)
	api.POST("/auth/totp/disable", s.handleTOTPDisable)
	api.POST("/auth/totp/recovery-codes", s.handleTOTPRecoveryCodes)

	api.GET("/auth/tokens", s.handleListTokens)
	api.POST("/auth/tokens", s.handleCreateToken)
	api.DELETE("/auth/tokens/:id", s.handleDeleteToken)

	api.GET("/auth/sessions", s.handleListSessions)
	api.DELETE("/auth/sessions/:id", s.handleDeleteSession)

	admin.GET("/admin/users", s.handleListUsers)
	admin.POST("/admin/users", s.handleCreateUser)
	admin.PATCH("/admin/users/:id", s.handleUpdateUser)
	admin.DELETE("/admin/users/:id", s.handleDeleteUser)
	admin.POST("/admin/users/:id/reset-password", s.handleResetPassword)

	s.extraRoutes()
}

// janitor purges expired sessions / share links and idle limiter entries.
func (s *Service) janitor() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.d.Ctx.Done():
			return
		case <-t.C:
			now := store.Now()
			if _, err := s.d.Store.AuthSessions.DeleteExpired(s.d.Ctx, now); err != nil && s.d.Ctx.Err() == nil {
				s.log.Warn("purge expired sessions", "err", err)
			}
			if _, err := s.d.Store.ShareLinks.DeleteExpired(s.d.Ctx, now); err != nil && s.d.Ctx.Err() == nil {
				s.log.Warn("purge expired share links", "err", err)
			}
			s.limiter.sweep()
			s.sweepReauth()
		}
	}
}

// ---- authentication -----------------------------------------------------------------------------------------------

// Authenticate implements httpx.Authenticator: Bearer API tokens first, then the session cookie (sliding expiry).
func (s *Service) Authenticate(c *echo.Context) (*model.User, httpx.AuthInfo, error) {
	r := c.Request()
	ctx := r.Context()
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return s.authToken(ctx, strings.TrimSpace(h[7:]))
	}
	ck, err := c.Cookie(CookieName)
	if err != nil || ck.Value == "" || len(ck.Value) > maxCookieLen {
		return nil, httpx.AuthInfo{}, nil
	}
	id := hashToken(ck.Value)
	sess, err := s.d.Store.AuthSessions.Get(ctx, id)
	if errors.Is(err, model.ErrNotFound) {
		return nil, httpx.AuthInfo{}, nil
	}
	if err != nil {
		return nil, httpx.AuthInfo{}, err
	}
	now := store.Now()
	pol := s.Policy(ctx)
	if !now.Before(sess.ExpiresAt) || (pol.SessionMaxDays > 0 && now.Sub(sess.CreatedAt) >= time.Duration(pol.SessionMaxDays)*24*time.Hour) {
		_ = s.d.Store.AuthSessions.Delete(ctx, id)
		return nil, httpx.AuthInfo{}, nil
	}
	u, err := s.d.Store.Users.Get(ctx, sess.UserID)
	if errors.Is(err, model.ErrNotFound) {
		return nil, httpx.AuthInfo{}, nil
	}
	if err != nil {
		return nil, httpx.AuthInfo{}, err
	}
	if u.Disabled {
		return nil, httpx.AuthInfo{}, nil
	}
	if now.Sub(sess.LastSeenAt) >= touchInterval {
		exp := now.Add(pol.idleTTL())
		if sess.Remember && pol.RememberDays > 0 {
			exp = now.Add(pol.rememberTTL())
		}
		if pol.SessionMaxDays > 0 {
			if hard := sess.CreatedAt.Add(time.Duration(pol.SessionMaxDays) * 24 * time.Hour); exp.After(hard) {
				exp = hard
			}
		}
		if err := s.d.Store.AuthSessions.Touch(ctx, id, now, exp, httpx.ClientIP(c)); err != nil {
			s.log.Warn("touch session", "err", err)
		}
		if sess.Remember && pol.RememberDays > 0 && now.Sub(sess.LastSeenAt) >= cookieRefresh {
			s.setCookie(c, ck.Value, true, pol.rememberTTL())
		}
	}
	return u, httpx.AuthInfo{Method: httpx.AuthCookie, SessionID: id}, nil
}

func (s *Service) authToken(ctx context.Context, tok string) (*model.User, httpx.AuthInfo, error) {
	if !strings.HasPrefix(tok, APITokenPrefix) || len(tok) > maxCookieLen {
		return nil, httpx.AuthInfo{}, nil
	}
	t, err := s.d.Store.APITokens.GetByHash(ctx, hashToken(tok))
	if errors.Is(err, model.ErrNotFound) {
		return nil, httpx.AuthInfo{}, nil
	}
	if err != nil {
		return nil, httpx.AuthInfo{}, err
	}
	now := store.Now()
	if t.ExpiresAt != nil && !now.Before(*t.ExpiresAt) {
		return nil, httpx.AuthInfo{}, nil
	}
	u, err := s.d.Store.Users.Get(ctx, t.UserID)
	if errors.Is(err, model.ErrNotFound) {
		return nil, httpx.AuthInfo{}, nil
	}
	if err != nil {
		return nil, httpx.AuthInfo{}, err
	}
	if u.Disabled {
		return nil, httpx.AuthInfo{}, nil
	}
	if t.LastUsedAt == nil || now.Sub(*t.LastUsedAt) >= touchInterval {
		if err := s.d.Store.APITokens.Touch(ctx, t.ID, now); err != nil {
			s.log.Warn("touch api token", "err", err)
		}
	}
	return u, httpx.AuthInfo{Method: httpx.AuthToken, TokenID: t.ID}, nil
}

// startSession creates a login session for u and sets the cookie.
func (s *Service) startSession(c *echo.Context, u *model.User, remember bool) error {
	r := c.Request()
	tok := randomToken(32)
	now := store.Now()
	pol := s.Policy(r.Context())
	if pol.RememberDays == 0 {
		remember = false
	}
	ttl := pol.idleTTL()
	if remember {
		ttl = pol.rememberTTL()
	}
	if pol.SessionMaxDays > 0 && ttl > time.Duration(pol.SessionMaxDays)*24*time.Hour {
		ttl = time.Duration(pol.SessionMaxDays) * 24 * time.Hour
	}
	ua := r.UserAgent()
	if len(ua) > 512 {
		ua = ua[:512]
	}
	sess := &model.AuthSession{ID: hashToken(tok), UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(ttl), LastSeenAt: now,
		IP: httpx.ClientIP(c), UserAgent: ua, Remember: remember}
	if err := s.d.Store.AuthSessions.Create(r.Context(), sess); err != nil {
		return err
	}
	// Signing in replaces whatever session this browser presented (another account's, or a planted one): the old
	// server-side session ends instead of lingering until it expires.
	if old, err := c.Cookie(CookieName); err == nil && old.Value != "" && len(old.Value) <= maxCookieLen {
		oldID := hashToken(old.Value)
		if err := s.d.Store.AuthSessions.Delete(r.Context(), oldID); err != nil && !errors.Is(err, model.ErrNotFound) {
			s.log.Warn("end previous session", "err", err)
		}
		s.d.Events.CloseAuthSession(oldID)
	}
	if err := s.d.Store.Users.TouchLogin(r.Context(), u.ID, now); err != nil {
		s.log.Warn("record login time", "err", err)
	}
	u.LastLoginAt = &now
	s.setCookie(c, tok, remember, ttl)
	return nil
}

func (s *Service) secureCookies(c *echo.Context) bool {
	if s.d.Cfg.TLSEnabled() || c.IsTLS() {
		return true
	}
	return len(s.d.Cfg.TrustedProxies) > 0 && strings.EqualFold(c.Request().Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Service) setCookie(c *echo.Context, token string, remember bool, ttl time.Duration) {
	ck := &http.Cookie{Name: CookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: s.secureCookies(c)}
	if remember {
		ck.MaxAge = int(ttl / time.Second)
	}
	c.SetCookie(ck)
}

func (s *Service) clearCookie(c *echo.Context) {
	c.SetCookie(&http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: s.secureCookies(c)})
}

// ---- feature detection --------------------------------------------------------------------------------------------

func (s *Service) features(ctx context.Context) map[string]bool {
	s.featMu.Lock()
	defer s.featMu.Unlock()
	if s.featVals != nil && time.Since(s.featAt) < featureCacheTTL {
		return s.featVals
	}
	vals := map[string]bool{
		"guacd":   probeGuacd(ctx, s.d.Cfg.Guacd),
		"docker":  probeDocker(),
		"kubectl": hasCommand("kubectl"),
		"mosh":    hasCommand("mosh-client") || hasCommand("mosh"),
		"wsl":     runtime.GOOS == "windows" && hasCommand("wsl.exe"),
	}
	for _, p := range app.FeatureProbes() {
		vals[p.Name] = safeProbe(ctx, p.Probe)
	}
	s.featVals, s.featAt = vals, time.Now()
	return vals
}

func safeProbe(ctx context.Context, p app.FeatureProbe) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	pctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return p(pctx)
}

func probeGuacd(ctx context.Context, addr string) bool {
	if addr == "" {
		return false
	}
	dctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(dctx, "tcp", addr)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func probeDocker() bool {
	if os.Getenv("DOCKER_HOST") != "" {
		return true
	}
	var candidates []string
	if runtime.GOOS == "windows" {
		candidates = []string{`\\.\pipe\docker_engine`, `\\.\pipe\dockerDesktopLinuxEngine`}
	} else {
		candidates = []string{"/var/run/docker.sock", "/run/docker.sock"}
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates,
				filepath.Join(home, ".docker", "run", "docker.sock"),
				filepath.Join(home, ".colima", "default", "docker.sock"),
				filepath.Join(home, ".orbstack", "run", "docker.sock"),
				filepath.Join(home, ".rd", "docker.sock"))
		}
		if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
			candidates = append(candidates, filepath.Join(xdg, "docker.sock"))
		}
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

func hasCommand(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
