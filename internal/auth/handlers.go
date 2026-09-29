package auth

import (
	"errors"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
)

// Error responses specific to auth.
var (
	errInvalidCredentials = httpx.Unauthorized("invalid_credentials", "invalid username or password")
	errTOTPRequired       = httpx.Unauthorized("totp_required", "two-factor authentication code required")
	errTOTPInvalid        = httpx.Unauthorized("totp_invalid", "invalid two-factor authentication code")
	errInvalidPassword    = httpx.NewError(http.StatusForbidden, "invalid_password", "incorrect password")
	errAccountDisabled    = httpx.NewError(http.StatusForbidden, "account_disabled", "account is disabled")
	errSetupRequired      = httpx.NewError(http.StatusConflict, "setup_required", "initial setup has not been completed")
	errInvalidLaunch      = httpx.Unauthorized("invalid_token", "invalid or already used launch token")
	errSetupToken         = httpx.NewError(http.StatusForbidden, "setup_token_required",
		"a valid setup token is required: open the setup link printed in the server's startup output")
)

func tooMany(d time.Duration) error {
	sec := int(math.Ceil(d.Seconds()))
	if sec < 1 {
		sec = 1
	}
	return httpx.TooManyRequests("too many failed attempts, try again later", sec)
}

type userResponse struct {
	User *model.User `json:"user"`
}

// ---- state --------------------------------------------------------------------------------------------------------

type stateResponse struct {
	SetupRequired bool `json:"setupRequired"`
	// SetupTokenRequired tells the setup wizard to send the one-time token from the startup banner (server mode).
	SetupTokenRequired     bool            `json:"setupTokenRequired,omitempty"`
	Authenticated          bool            `json:"authenticated"`
	User                   *model.User     `json:"user,omitempty"`
	Mode                   string          `json:"mode"`
	VaultLocked            bool            `json:"vaultLocked"`
	VaultHasMasterPassword bool            `json:"vaultHasMasterPassword"`
	Version                string          `json:"version"`
	Features               map[string]bool `json:"features"`
	// LoginMethods tells the login screen which sign-in options to offer.
	LoginMethods loginMethods `json:"loginMethods"`
	// PasswordPolicy lets password forms validate before submitting.
	PasswordPolicy passwordPolicyView `json:"passwordPolicy"`
}

type loginMethods struct {
	Password bool        `json:"password"`
	Passkey  bool        `json:"passkey"`
	SSO      []SSOMethod `json:"sso"`
	Remember bool        `json:"remember"`
}

type passwordPolicyView struct {
	MinLength        int  `json:"minLength"`
	RequireClasses   int  `json:"requireClasses"`
	DisallowUsername bool `json:"disallowUsername"`
}

func (s *Service) handleState(c *echo.Context) error {
	ctx := c.Request().Context()
	n, err := s.d.Store.Users.Count(ctx)
	if err != nil {
		return err
	}
	u := httpx.UserFrom(c)
	pol := s.Policy(ctx)
	methods := loginMethods{Password: true, Passkey: s.passkeyProvider() != nil, SSO: []SSOMethod{}, Remember: pol.RememberDays > 0}
	if p := s.ssoProvider(); p != nil {
		if m := p.LoginMethods(ctx); m != nil {
			methods.SSO = m
		}
	}
	return c.JSON(http.StatusOK, stateResponse{
		LoginMethods: methods,
		PasswordPolicy: passwordPolicyView{MinLength: pol.PasswordMinLength, RequireClasses: pol.PasswordRequireClasses,
			DisallowUsername: pol.PasswordDisallowUsername},
		SetupRequired:          n == 0,
		SetupTokenRequired:     n == 0 && s.SetupToken() != "",
		Authenticated:          u != nil,
		User:                   u,
		Mode:                   s.d.Cfg.Mode,
		VaultLocked:            s.d.Vault.Locked(),
		VaultHasMasterPassword: s.d.Vault.HasMasterPassword(),
		Version:                s.d.Cfg.Version,
		Features:               s.features(ctx),
	})
}

// ---- setup / login / logout / launch ------------------------------------------------------------------------------

type setupRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName"`
	// SetupToken is required while Service.SetupToken() is set (server mode, non-loopback binds).
	SetupToken string `json:"setupToken"`
}

func (s *Service) handleSetup(c *echo.Context) error {
	var req setupRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if want := s.SetupToken(); want != "" {
		key := "setup:" + RateKeyIP(httpx.ClientIP(c))
		if d := s.limiter.retryAfter(key); d > 0 {
			return tooMany(d)
		}
		if req.SetupToken == "" || !constantTimeEqual(want, req.SetupToken) {
			if req.SetupToken != "" {
				s.limiter.fail(key, loginUserFailures)
			}
			return errSetupToken
		}
	}
	req.Username = strings.TrimSpace(req.Username)
	if err := ValidateUsername(req.Username); err != nil {
		return httpx.BadRequest(err.Error())
	}
	if err := ValidatePassword(req.Password); err != nil {
		return httpx.BadRequest(err.Error())
	}
	ctx := c.Request().Context()
	if n, err := s.d.Store.Users.Count(ctx); err != nil {
		return err
	} else if n > 0 {
		return httpx.Conflict("setup has already been completed")
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		return httpx.Internal(err)
	}
	u := &model.User{Username: req.Username, DisplayName: cleanDisplayName(req.DisplayName, req.Username), Role: model.RoleAdmin}
	if err := s.d.Store.Users.CreateFirstAdmin(ctx, u, hash); err != nil {
		if errors.Is(err, model.ErrConflict) {
			return httpx.Conflict("setup has already been completed")
		}
		return err
	}
	s.setupMu.Lock()
	s.setupToken = "" // one-time
	s.setupMu.Unlock()
	if err := s.startSession(c, u, s.d.Cfg.IsDesktop()); err != nil {
		return err
	}
	s.d.Audit.LogUser(c, u, "auth.setup", u.ID, nil)
	return c.JSON(http.StatusOK, userResponse{User: u})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	TOTP     string `json:"totp"`
	Remember bool   `json:"remember"`
}

func (s *Service) handleLogin(c *echo.Context) error {
	var req loginRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	ctx := c.Request().Context()
	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" || len(username) > 128 {
		return httpx.BadRequest("username and password are required")
	}
	pol := s.Policy(ctx)
	ip := httpx.ClientIP(c)
	if !pol.loginAllowedFrom(ip) {
		s.d.Audit.LogUser(c, nil, "auth.login.failed", "", map[string]string{"username": username, "reason": "network"})
		return errLoginNotAllowed
	}
	// Backoff keys: per client (IPv6 by /64) + user name, per client, and — for remote clients — per user name across
	// all clients, so a password spray from many addresses still slows down. Loopback clients (the AstraTerm host
	// itself) are exempt from the cross-client key: they are the break-glass path.
	rip, lname := RateKeyIP(ip), strings.ToLower(username)
	userKey, ipKey, nameKey := "login:"+rip+"|"+lname, "login-ip:"+rip, "login-user:"+lname
	loopback := isLoopbackIP(ip)
	keys := []string{userKey, ipKey}
	if !loopback {
		keys = append(keys, nameKey)
	}
	if d := s.limiter.retryAfter(keys...); d > 0 {
		return tooMany(d)
	}
	ipThreshold := loginIPFailures
	if t := 4 * pol.LockoutThreshold; t > ipThreshold {
		ipThreshold = t
	}
	failed := func(u *model.User, reason string, err error) error {
		s.limiter.fail(userKey, pol.LockoutThreshold)
		s.limiter.fail(ipKey, ipThreshold)
		if !loopback {
			s.limiter.fail(nameKey, ipThreshold)
		}
		s.d.Audit.LogUser(c, u, "auth.login.failed", "", map[string]string{"username": username, "reason": reason})
		s.noteLoginFailure(c, u, pol)
		return err
	}

	ua, err := s.d.Store.Users.GetAuthByUsername(ctx, username)
	if errors.Is(err, model.ErrNotFound) {
		burnPasswordCheck(req.Password)
		return failed(nil, "unknown user", errInvalidCredentials)
	}
	if err != nil {
		return err
	}
	if !VerifyPassword(ua.PasswordHash, req.Password) {
		return failed(ua.User, "bad password", errInvalidCredentials)
	}
	// A locked account is refused even with the right password (the lock is only revealed to someone who knows it).
	// Clients on the AstraTerm host itself are exempt, so remote failures can never lock the owner out for good.
	meta, err := s.userMeta(ctx, ua.User.ID)
	if err != nil {
		return err
	}
	if meta.locked(store.Now()) && !loopback {
		s.d.Audit.LogUser(c, ua.User, "auth.login.failed", "", map[string]string{"username": username, "reason": "locked"})
		return errAccountLocked
	}
	if ua.User.Disabled {
		s.d.Audit.LogUser(c, ua.User, "auth.login.failed", "", map[string]string{"username": username, "reason": "disabled"})
		return errAccountDisabled
	}
	if !pol.PasswordLogin && !ua.User.IsAdmin() {
		s.d.Audit.LogUser(c, ua.User, "auth.login.failed", "", map[string]string{"username": username, "reason": "password login disabled"})
		return errPasswordLogin
	}
	methods, err := s.mfaMethods(ctx, ua.User)
	if err != nil {
		return err
	}
	usedTOTP := false
	switch {
	case len(methods) > 0 && strings.TrimSpace(req.TOTP) != "" && slices.Contains(methods, MethodTOTP):
		// Single-request form (older clients): password + TOTP / recovery code.
		ok, err := s.checkSecondFactor(ctx, ua, req.TOTP)
		if err != nil {
			return err
		}
		if !ok {
			return failed(ua.User, "bad totp", errTOTPInvalid)
		}
		usedTOTP = true
	case len(methods) > 0:
		s.limiter.reset(userKey)
		return s.mfaChallengeResponse(c, ua.User.ID, req.Remember, methods, false)
	case pol.mfaRequiredFor(ua.User):
		s.limiter.reset(userKey)
		return s.mfaChallengeResponse(c, ua.User.ID, req.Remember, []string{MethodTOTP}, true)
	}
	s.limiter.reset(userKey)
	s.limiter.reset(nameKey)
	if err := s.startSession(c, ua.User, req.Remember); err != nil {
		return err
	}
	if err := s.clearLoginFailures(ctx, ua.User.ID, false); err != nil {
		s.log.Warn("reset login failures", "err", err)
	}
	method := "password"
	if usedTOTP {
		method = "password+totp"
	}
	s.d.Audit.LogUser(c, ua.User, "auth.login", ua.User.ID, map[string]any{"remember": req.Remember, "totp": usedTOTP, "method": method})
	return c.JSON(http.StatusOK, userResponse{User: ua.User})
}

func (s *Service) handleLogout(c *echo.Context) error {
	info := httpx.AuthInfoFrom(c)
	if info.Method == httpx.AuthCookie && info.SessionID != "" {
		if err := s.d.Store.AuthSessions.Delete(c.Request().Context(), info.SessionID); err != nil {
			return err
		}
		s.d.Events.CloseAuthSession(info.SessionID)
	}
	if u := httpx.UserFrom(c); u != nil {
		s.d.Audit.Log(c, "auth.logout", u.ID, nil)
	}
	s.clearCookie(c)
	return httpx.OK(c)
}

type launchRequest struct {
	Token string `json:"token"`
}

// handleLaunch exchanges the desktop-mode one-time launch token for a session of the first admin. While setup is
// still required it answers 409 {code:'setup_required'} without consuming the token (the setup call logs in anyway).
func (s *Service) handleLaunch(c *echo.Context) error {
	if !s.d.Cfg.IsDesktop() {
		return httpx.NotFound("launch tokens are only available in desktop mode")
	}
	var req launchRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	key := "launch:" + RateKeyIP(httpx.ClientIP(c))
	if d := s.limiter.retryAfter(key); d > 0 {
		return tooMany(d)
	}
	s.launchMu.Lock()
	valid := s.launchToken != "" && req.Token != "" && constantTimeEqual(s.launchToken, req.Token)
	s.launchMu.Unlock()
	if !valid {
		s.limiter.fail(key, loginUserFailures)
		return errInvalidLaunch
	}
	ctx := c.Request().Context()
	n, err := s.d.Store.Users.Count(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return errSetupRequired
	}
	admin, err := s.d.Store.Users.FirstAdmin(ctx)
	if errors.Is(err, model.ErrNotFound) {
		return httpx.Conflict("no active administrator account exists")
	}
	if err != nil {
		return err
	}
	// Consume (single use); a concurrent exchange may have won the race.
	s.launchMu.Lock()
	if s.launchToken == "" || !constantTimeEqual(s.launchToken, req.Token) {
		s.launchMu.Unlock()
		return errInvalidLaunch
	}
	s.launchToken = ""
	s.launchMu.Unlock()
	s.limiter.reset(key)

	if err := s.startSession(c, admin, true); err != nil {
		return err
	}
	s.d.Audit.LogUser(c, admin, "auth.launch", admin.ID, nil)
	return c.JSON(http.StatusOK, userResponse{User: admin})
}

// ---- password -----------------------------------------------------------------------------------------------------

// checkPassword verifies the current user's password with per-user backoff.
func (s *Service) checkPassword(c *echo.Context, u *model.User, password string) error {
	key := "password:" + u.ID
	if d := s.limiter.retryAfter(key); d > 0 {
		return tooMany(d)
	}
	ua, err := s.d.Store.Users.GetAuth(c.Request().Context(), u.ID)
	if err != nil {
		return err
	}
	if password == "" || !VerifyPassword(ua.PasswordHash, password) {
		s.limiter.fail(key, passwordFailures)
		s.d.Audit.Log(c, "auth.reauth.failed", u.ID, nil)
		return errInvalidPassword
	}
	s.limiter.reset(key)
	return nil
}

type passwordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

func (s *Service) handlePassword(c *echo.Context) error {
	var req passwordRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	u := httpx.UserFrom(c)
	ctx := c.Request().Context()
	// Accounts provisioned by single sign-on have no password yet: a fresh login authorizes setting one.
	if err := s.reauth(c, u, req.CurrentPassword); err != nil {
		return err
	}
	if err := s.Policy(ctx).ValidatePasswordFor(req.NewPassword, u.Username); err != nil {
		return httpx.BadRequest(err.Error())
	}
	hash, err := HashPassword(req.NewPassword)
	if err != nil {
		return httpx.Internal(err)
	}
	if err := s.d.Store.Users.SetPassword(ctx, u.ID, hash); err != nil {
		return err
	}
	if err := s.setPasswordState(ctx, u.ID, true); err != nil {
		return err
	}
	// Invalidate every other login session of the user.
	current := httpx.AuthInfoFrom(c).SessionID
	if err := s.d.Store.AuthSessions.DeleteAllForUser(ctx, u.ID, current); err != nil {
		return err
	}
	s.closeOtherSockets(u.ID, current)
	s.d.Audit.Log(c, "auth.password.change", u.ID, nil)
	return httpx.OK(c)
}

// closeOtherSockets disconnects event sockets opened with any login session of userID other than keep.
func (s *Service) closeOtherSockets(userID, keep string) {
	s.d.Events.CloseUserSessionsExcept(userID, keep)
}

type verifyRequest struct {
	Password string `json:"password"`
}

func (s *Service) handleVerifyPassword(c *echo.Context) error {
	var req verifyRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if err := s.checkPassword(c, httpx.UserFrom(c), req.Password); err != nil {
		return err
	}
	s.MarkReauthenticated(c)
	return httpx.OK(c)
}

// ---- API tokens ---------------------------------------------------------------------------------------------------

func (s *Service) handleListTokens(c *echo.Context) error {
	list, err := s.d.Store.APITokens.ListByUser(c.Request().Context(), httpx.UserFrom(c).ID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, list)
}

type createTokenRequest struct {
	Name          string `json:"name"`
	ExpiresInDays int    `json:"expiresInDays"`
	// Password re-verifies the user when the session is not fresh (optional; see RequireRecentAuth).
	Password string `json:"password"`
}

type createTokenResponse struct {
	*model.APIToken
	Token string `json:"token"`
}

func (s *Service) handleCreateToken(c *echo.Context) error {
	var req createTokenRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 100 {
		return httpx.BadRequest("name must be 1-100 characters")
	}
	if req.ExpiresInDays < 0 || req.ExpiresInDays > 3650 {
		return httpx.BadRequest("expiresInDays must be between 0 (never) and 3650")
	}
	if err := s.RequireRecentAuth(c, req.Password); err != nil {
		return err
	}
	u := httpx.UserFrom(c)
	raw := APITokenPrefix + randomToken(32)
	t := &model.APIToken{Name: req.Name, UserID: u.ID, TokenHash: hashToken(raw), CreatedAt: store.Now()}
	if req.ExpiresInDays > 0 {
		exp := t.CreatedAt.Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour)
		t.ExpiresAt = &exp
	}
	if err := s.d.Store.APITokens.Create(c.Request().Context(), t); err != nil {
		return err
	}
	s.d.Audit.Log(c, "auth.token.create", t.ID, map[string]string{"name": t.Name})
	return c.JSON(http.StatusCreated, createTokenResponse{APIToken: t, Token: raw})
}

func (s *Service) handleDeleteToken(c *echo.Context) error {
	u := httpx.UserFrom(c)
	id := c.Param("id")
	if err := s.d.Store.APITokens.Delete(c.Request().Context(), u.ID, id); err != nil {
		return err
	}
	s.d.Audit.Log(c, "auth.token.delete", id, nil)
	return httpx.OK(c)
}

// ---- login sessions -----------------------------------------------------------------------------------------------

func (s *Service) handleListSessions(c *echo.Context) error {
	u := httpx.UserFrom(c)
	list, err := s.d.Store.AuthSessions.ListByUser(c.Request().Context(), u.ID)
	if err != nil {
		return err
	}
	current := httpx.AuthInfoFrom(c).SessionID
	for _, se := range list {
		se.Current = se.ID == current
	}
	return c.JSON(http.StatusOK, list)
}

func (s *Service) handleDeleteSession(c *echo.Context) error {
	u := httpx.UserFrom(c)
	id := c.Param("id")
	if err := s.d.Store.AuthSessions.DeleteForUser(c.Request().Context(), u.ID, id); err != nil {
		return err
	}
	s.d.Events.CloseAuthSession(id)
	if id == httpx.AuthInfoFrom(c).SessionID {
		s.clearCookie(c)
	}
	s.d.Audit.Log(c, "auth.session.revoke", u.ID, nil)
	return httpx.OK(c)
}

func cleanDisplayName(name, fallback string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return fallback
	}
	if len(name) > 128 {
		name = name[:128]
	}
	return name
}
