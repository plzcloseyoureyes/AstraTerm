package auth

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
)

// ---- companion modules --------------------------------------------------------------------------------------------

// PasskeyProvider is implemented by the webauthn module: it lets the login flow offer passkeys as a second factor
// and admins reset them.
type PasskeyProvider interface {
	// CountCredentials returns how many passkeys userID has registered.
	CountCredentials(ctx context.Context, userID string) (int, error)
	// CountAll returns the passkey count per user ID.
	CountAll(ctx context.Context) (map[string]int, error)
	// DeleteAll removes every passkey of userID and returns how many were removed.
	DeleteAll(ctx context.Context, userID string) (int, error)
}

// SSOMethod is one single-sign-on login option shown on the login screen.
type SSOMethod struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// SSOProvider is implemented by the oidc module.
type SSOProvider interface {
	// LoginMethods lists the enabled providers.
	LoginMethods(ctx context.Context) []SSOMethod
	// LinkedProviders returns, per user ID, the names of the providers the user has signed in with.
	LinkedProviders(ctx context.Context) (map[string][]string, error)
}

// SetPasskeyProvider registers the passkey module.
func (s *Service) SetPasskeyProvider(p PasskeyProvider) {
	s.provMu.Lock()
	s.passkeys = p
	s.provMu.Unlock()
}

// SetSSOProvider registers the single-sign-on module.
func (s *Service) SetSSOProvider(p SSOProvider) {
	s.provMu.Lock()
	s.sso = p
	s.provMu.Unlock()
}

func (s *Service) passkeyProvider() PasskeyProvider {
	s.provMu.RLock()
	defer s.provMu.RUnlock()
	return s.passkeys
}

func (s *Service) ssoProvider() SSOProvider {
	s.provMu.RLock()
	defer s.provMu.RUnlock()
	return s.sso
}

// ---- pending second-factor challenges -------------------------------------------------------------------------------

const (
	mfaTTL         = 5 * time.Minute
	mfaMaxAttempts = 5
	maxPendingMFA  = 10000
)

// Second-factor method names.
const (
	MethodTOTP     = "totp"
	MethodWebAuthn = "webauthn"
)

// MFAChallenge is a password-verified login waiting for its second factor (or, with Enroll, for the user to enrol
// TOTP because the policy requires one).
type MFAChallenge struct {
	UserID   string
	Remember bool
	Methods  []string
	Enroll   bool
	expires  time.Time
	failures int
	// enrollSecretEnc is the TOTP secret generated during enrollment (sealed with the system key).
	enrollSecretEnc []byte
}

var (
	errMFAExpired      = httpx.Unauthorized("mfa_expired", "the sign-in attempt expired, please sign in again")
	errReauthRequired  = httpx.NewError(http.StatusForbidden, "reauth_required", "confirm your identity to continue")
	errAccountLocked   = httpx.NewError(http.StatusForbidden, "account_locked", "this account is temporarily locked after too many failed sign-in attempts")
	errLoginNotAllowed = httpx.NewError(http.StatusForbidden, "login_not_allowed", "sign-in is not allowed from your network")
	errPasswordLogin   = httpx.NewError(http.StatusForbidden, "password_login_disabled", "password sign-in is disabled: use single sign-on or a passkey")
)

func (s *Service) newMFAChallenge(ch *MFAChallenge) string {
	tok := randomToken(32)
	ch.expires = time.Now().Add(mfaTTL)
	s.mfaMu.Lock()
	defer s.mfaMu.Unlock()
	if len(s.mfa) >= maxPendingMFA {
		s.sweepMFALocked(time.Now())
		for k := range s.mfa { // still full: drop an arbitrary one rather than grow without bound
			if len(s.mfa) < maxPendingMFA {
				break
			}
			delete(s.mfa, k)
		}
	}
	s.mfa[hashToken(tok)] = ch
	return tok
}

func (s *Service) sweepMFALocked(now time.Time) {
	for k, v := range s.mfa {
		if now.After(v.expires) {
			delete(s.mfa, k)
		}
	}
}

// LookupMFA returns a copy of the pending challenge for token (401 mfa_expired when unknown or expired).
func (s *Service) LookupMFA(token string) (MFAChallenge, error) {
	if token == "" || len(token) > maxCookieLen {
		return MFAChallenge{}, errMFAExpired
	}
	s.mfaMu.Lock()
	defer s.mfaMu.Unlock()
	ch := s.mfa[hashToken(token)]
	if ch == nil || time.Now().After(ch.expires) {
		delete(s.mfa, hashToken(token))
		return MFAChallenge{}, errMFAExpired
	}
	out := *ch
	out.Methods = slices.Clone(ch.Methods)
	return out, nil
}

// MFAFailed records a failed second-factor attempt; the challenge is dropped after too many.
func (s *Service) MFAFailed(token string) {
	s.mfaMu.Lock()
	defer s.mfaMu.Unlock()
	k := hashToken(token)
	if ch := s.mfa[k]; ch != nil {
		ch.failures++
		if ch.failures >= mfaMaxAttempts {
			delete(s.mfa, k)
		}
	}
}

// takeMFA removes and returns the challenge (single use).
func (s *Service) takeMFA(token string) (*MFAChallenge, error) {
	s.mfaMu.Lock()
	defer s.mfaMu.Unlock()
	k := hashToken(token)
	ch := s.mfa[k]
	delete(s.mfa, k)
	if ch == nil || time.Now().After(ch.expires) {
		return nil, errMFAExpired
	}
	return ch, nil
}

// FinishMFA completes a pending login whose second factor (method) the caller verified.
func (s *Service) FinishMFA(c *echo.Context, token, method string) (*model.User, error) {
	ch, err := s.takeMFA(token)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(ch.Methods, method) {
		return nil, errMFAExpired
	}
	return s.CompleteLogin(c, ch.UserID, LoginOptions{Remember: ch.Remember, Method: "password+" + method, MFA: true})
}

// mfaMethods returns the second factors u has configured (TOTP, passkeys).
func (s *Service) mfaMethods(ctx context.Context, u *model.User) ([]string, error) {
	var m []string
	if u.TOTPEnabled {
		m = append(m, MethodTOTP)
	}
	if p := s.passkeyProvider(); p != nil {
		n, err := p.CountCredentials(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			m = append(m, MethodWebAuthn)
		}
	}
	return m, nil
}

// mfaChallengeResponse answers a password-verified login that needs a second factor: 401 with the usual error body
// plus {methods, mfaToken, expiresIn}. The code stays "totp_required" for TOTP-only accounts (older clients).
func (s *Service) mfaChallengeResponse(c *echo.Context, userID string, remember bool, methods []string, enroll bool) error {
	tok := s.newMFAChallenge(&MFAChallenge{UserID: userID, Remember: remember, Methods: methods, Enroll: enroll})
	code, msg := "mfa_required", "a second authentication factor is required"
	switch {
	case enroll:
		code, msg = "mfa_enrollment_required", "two-factor authentication must be set up for this account"
	case len(methods) == 1 && methods[0] == MethodTOTP:
		code, msg = "totp_required", "two-factor authentication code required"
	}
	return c.JSON(http.StatusUnauthorized, map[string]any{
		"error": msg, "code": code, "methods": methods, "mfaToken": tok, "expiresIn": int(mfaTTL / time.Second),
	})
}

// ---- completing logins ----------------------------------------------------------------------------------------------

// LoginOptions describes how a login was authenticated (CompleteLogin).
type LoginOptions struct {
	Remember bool
	// Method is recorded in the audit log ("passkey", "oidc:<provider>", "password+totp", …).
	Method string
	// MFA: the login used a second factor (or a factor that counts as multi-factor, like a user-verified passkey).
	MFA bool
	// External: the identity provider is responsible for MFA (SSO).
	External bool
	Details  map[string]any
}

// CompleteLogin finishes a login authenticated by a companion module (passkey, SSO) or a second-factor step: it
// re-checks the account (exists, enabled, network policy, MFA policy), starts the session, resets the failure
// counters and audits auth.login. The response is up to the caller.
func (s *Service) CompleteLogin(c *echo.Context, userID string, opt LoginOptions) (*model.User, error) {
	ctx := c.Request().Context()
	u, err := s.d.Store.Users.Get(ctx, userID)
	if errors.Is(err, model.ErrNotFound) {
		return nil, errInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	pol := s.Policy(ctx)
	if !pol.loginAllowedFrom(httpx.ClientIP(c)) {
		s.d.Audit.LogUser(c, u, "auth.login.failed", "", map[string]string{"username": u.Username, "reason": "network", "method": opt.Method})
		return nil, errLoginNotAllowed
	}
	if u.Disabled {
		s.d.Audit.LogUser(c, u, "auth.login.failed", "", map[string]string{"username": u.Username, "reason": "disabled", "method": opt.Method})
		return nil, errAccountDisabled
	}
	if !opt.MFA && !opt.External && pol.mfaRequiredFor(u) {
		return nil, httpx.NewError(http.StatusForbidden, "mfa_required", "this account must sign in with a second factor")
	}
	if err := s.startSession(c, u, opt.Remember); err != nil {
		return nil, err
	}
	if err := s.clearLoginFailures(ctx, u.ID, false); err != nil {
		s.log.Warn("reset login failures", "err", err)
	}
	details := map[string]any{"remember": opt.Remember, "method": opt.Method}
	maps.Copy(details, opt.Details)
	s.d.Audit.LogUser(c, u, "auth.login", u.ID, details)
	return u, nil
}

// CreateExternalUser creates an account provisioned by an identity provider: it has no usable local password until
// the user sets one (fresh login required).
func (s *Service) CreateExternalUser(ctx context.Context, username, displayName string, role model.Role) (*model.User, error) {
	if err := ValidateUsername(username); err != nil {
		return nil, httpx.BadRequest(err.Error())
	}
	if !role.Valid() {
		role = model.RoleUser
	}
	hash, err := HashPassword(randomToken(32)) // unusable: nobody knows it
	if err != nil {
		return nil, err
	}
	u := &model.User{Username: username, DisplayName: cleanDisplayName(displayName, username), Role: role}
	if err := s.d.Store.Users.Create(ctx, u, hash); err != nil {
		return nil, err
	}
	if err := s.setPasswordState(ctx, u.ID, false); err != nil {
		return nil, err
	}
	return u, nil
}

// SyncRole sets u's role as mapped by an identity provider, refusing to demote the last active administrator.
func (s *Service) SyncRole(ctx context.Context, u *model.User, role model.Role) (bool, error) {
	if !role.Valid() || u.Role == role {
		return false, nil
	}
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	if u.Role == model.RoleAdmin && !u.Disabled {
		n, err := s.d.Store.Users.CountActiveAdmins(ctx)
		if err != nil {
			return false, err
		}
		if n <= 1 {
			return false, nil
		}
	}
	u.Role = role
	if err := s.d.Store.Users.Update(ctx, u); err != nil {
		return false, err
	}
	return true, nil
}

// ---- recent authentication ("sudo mode") ------------------------------------------------------------------------------

// reauthWindow is how long a fresh login or re-verification authorizes sensitive account changes (adding passkeys,
// setting a first password…) without asking again.
const reauthWindow = 10 * time.Minute

// MarkReauthenticated records that the current login session just proved its identity again.
func (s *Service) MarkReauthenticated(c *echo.Context) {
	info := httpx.AuthInfoFrom(c)
	if info.Method != httpx.AuthCookie || info.SessionID == "" {
		return
	}
	s.reauthMu.Lock()
	s.reauthAt[info.SessionID] = time.Now()
	s.reauthMu.Unlock()
}

// RequireRecentAuth authorizes a sensitive change: a correct password (when given), or a login session that signed
// in / re-verified within the last 10 minutes. Otherwise 403 reauth_required (or invalid_password).
func (s *Service) RequireRecentAuth(c *echo.Context, password string) error {
	u := httpx.UserFrom(c)
	if u == nil {
		return httpx.ErrUnauthorized
	}
	if password != "" {
		if err := s.checkPassword(c, u, password); err != nil {
			return err
		}
		s.MarkReauthenticated(c)
		return nil
	}
	info := httpx.AuthInfoFrom(c)
	if info.Method != httpx.AuthCookie || info.SessionID == "" {
		return errReauthRequired
	}
	s.reauthMu.Lock()
	at, ok := s.reauthAt[info.SessionID]
	s.reauthMu.Unlock()
	if ok && time.Since(at) < reauthWindow {
		return nil
	}
	sess, err := s.d.Store.AuthSessions.Get(c.Request().Context(), info.SessionID)
	if err == nil && store.Now().Sub(sess.CreatedAt) < reauthWindow {
		return nil
	}
	return errReauthRequired
}

// reauth authorizes a change that traditionally asks for the password: accounts without a local password (SSO)
// use the recent-authentication rule instead.
func (s *Service) reauth(c *echo.Context, u *model.User, password string) error {
	m, err := s.userMeta(c.Request().Context(), u.ID)
	if err != nil {
		return err
	}
	if !m.PasswordSet && password == "" {
		return s.RequireRecentAuth(c, "")
	}
	return s.checkPassword(c, u, password)
}

func (s *Service) sweepReauth() {
	s.reauthMu.Lock()
	for k, t := range s.reauthAt {
		if time.Since(t) > reauthWindow {
			delete(s.reauthAt, k)
		}
	}
	s.reauthMu.Unlock()
	s.mfaMu.Lock()
	s.sweepMFALocked(time.Now())
	s.mfaMu.Unlock()
}

// ---- second-factor step endpoints ------------------------------------------------------------------------------------

type mfaTOTPRequest struct {
	MFAToken string `json:"mfaToken"`
	Code     string `json:"code"`
}

// handleMFATOTP completes a pending login with a TOTP or recovery code (POST /api/auth/mfa/totp).
func (s *Service) handleMFATOTP(c *echo.Context) error {
	var req mfaTOTPRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	ch, err := s.LookupMFA(req.MFAToken)
	if err != nil {
		return err
	}
	if !slices.Contains(ch.Methods, MethodTOTP) || ch.Enroll {
		return httpx.BadRequest("this sign-in does not accept an authenticator code")
	}
	key := "mfa:" + ch.UserID
	if d := s.limiter.retryAfter(key); d > 0 {
		return tooMany(d)
	}
	ctx := c.Request().Context()
	ua, err := s.d.Store.Users.GetAuth(ctx, ch.UserID)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return errMFAExpired
		}
		return err
	}
	if strings.TrimSpace(req.Code) == "" {
		return errTOTPRequired
	}
	ok, err := s.checkSecondFactor(ctx, ua, req.Code)
	if err != nil {
		return err
	}
	if !ok {
		s.MFAFailed(req.MFAToken)
		s.limiter.fail(key, passwordFailures)
		s.d.Audit.LogUser(c, ua.User, "auth.login.failed", "", map[string]string{"username": ua.User.Username, "reason": "bad totp"})
		s.noteLoginFailure(c, ua.User, s.Policy(ctx))
		return errTOTPInvalid
	}
	s.limiter.reset(key)
	u, err := s.FinishMFA(c, req.MFAToken, MethodTOTP)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, userResponse{User: u})
}

type mfaTokenRequest struct {
	MFAToken string `json:"mfaToken"`
}

// handleEnrollTOTPSetup starts TOTP enrollment during a login that the policy requires a second factor for.
func (s *Service) handleEnrollTOTPSetup(c *echo.Context) error {
	var req mfaTokenRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	ch, err := s.LookupMFA(req.MFAToken)
	if err != nil {
		return err
	}
	if !ch.Enroll {
		return httpx.BadRequest("this sign-in does not need enrollment")
	}
	u, err := s.d.Store.Users.Get(c.Request().Context(), ch.UserID)
	if err != nil {
		return errMFAExpired
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: totpIssuer, AccountName: u.Username, Period: totpPeriod,
		SecretSize: 20, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		return httpx.Internal(err)
	}
	sealed, err := s.d.Vault.SystemSeal([]byte(key.Secret()))
	if err != nil {
		return httpx.Internal(err)
	}
	s.mfaMu.Lock()
	if p := s.mfa[hashToken(req.MFAToken)]; p != nil {
		p.enrollSecretEnc = sealed
	}
	s.mfaMu.Unlock()
	return c.JSON(http.StatusOK, totpSetupResponse{Secret: key.Secret(), OtpauthURL: key.URL()})
}

type enrollEnableRequest struct {
	MFAToken string `json:"mfaToken"`
	Code     string `json:"code"`
}

type enrollEnableResponse struct {
	User          *model.User `json:"user"`
	RecoveryCodes []string    `json:"recoveryCodes"`
}

// handleEnrollTOTPEnable verifies the first code, enables TOTP and completes the login.
func (s *Service) handleEnrollTOTPEnable(c *echo.Context) error {
	var req enrollEnableRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	s.mfaMu.Lock()
	var secretEnc []byte
	var userID string
	if p := s.mfa[hashToken(req.MFAToken)]; p != nil && p.Enroll && time.Now().Before(p.expires) {
		secretEnc, userID = p.enrollSecretEnc, p.UserID
	}
	s.mfaMu.Unlock()
	if userID == "" {
		return errMFAExpired
	}
	if len(secretEnc) == 0 {
		return httpx.BadRequest("start the authenticator setup first")
	}
	key := "totp-enable:" + userID
	if d := s.limiter.retryAfter(key); d > 0 {
		return tooMany(d)
	}
	secret, err := s.d.Vault.SystemOpen(secretEnc)
	if err != nil {
		return httpx.Internal(err)
	}
	step, ok := verifyTOTPCode(string(secret), normalizeCode(req.Code), time.Now())
	if !ok {
		s.limiter.fail(key, passwordFailures)
		s.MFAFailed(req.MFAToken)
		return httpx.NewError(http.StatusBadRequest, "totp_invalid", "invalid code — check the authenticator app and the device clock")
	}
	s.limiter.reset(key)
	ch, err := s.takeMFA(req.MFAToken)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	codes, hashes := newRecoveryCodes()
	if err := s.d.Store.Users.SetTOTP(ctx, ch.UserID, secretEnc, true, hashes); err != nil {
		return err
	}
	if _, err := s.d.Store.Users.AdvanceTOTPStep(ctx, ch.UserID, step); err != nil {
		return err
	}
	u, err := s.CompleteLogin(c, ch.UserID, LoginOptions{Remember: ch.Remember, Method: "password+totp", MFA: true,
		Details: map[string]any{"enrolled": true}})
	if err != nil {
		return err
	}
	s.d.Audit.LogUser(c, u, "auth.totp.enable", u.ID, map[string]any{"duringLogin": true})
	return c.JSON(http.StatusOK, enrollEnableResponse{User: u, RecoveryCodes: codes})
}

// HasPassword reports whether userID has a usable local password (accounts provisioned by SSO have none).
func (s *Service) HasPassword(ctx context.Context, userID string) (bool, error) {
	m, err := s.userMeta(ctx, userID)
	return m.PasswordSet, err
}

// PasskeyCount returns how many passkeys userID has (0 when the passkey module is not mounted).
func (s *Service) PasskeyCount(ctx context.Context, userID string) (int, error) {
	if p := s.passkeyProvider(); p != nil {
		return p.CountCredentials(ctx, userID)
	}
	return 0, nil
}
