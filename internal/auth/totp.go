package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/store"
)

const (
	totpIssuer        = "Termstead"
	totpPeriod        = 30
	recoveryCodeCount = 10
	recoveryAlphabet  = "abcdefghjkmnpqrstuvwxyz23456789" // no 0/o/1/l/i
)

var totpOpts = totp.ValidateOpts{Period: totpPeriod, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}

// verifyTOTPCode checks a 6-digit code against secret allowing ±1 step of clock skew and returns the matched step.
func verifyTOTPCode(secret, code string, now time.Time) (int64, bool) {
	if len(code) != 6 || strings.Trim(code, "0123456789") != "" {
		return 0, false
	}
	cur := now.Unix() / totpPeriod
	matched, found := int64(0), false
	for _, d := range []int64{-1, 0, 1} { // constant work: check every window
		st := cur + d
		c, err := totp.GenerateCodeCustom(secret, time.Unix(st*totpPeriod, 0), totpOpts)
		if err == nil && constantTimeEqual(c, code) && !found {
			matched, found = st, true
		}
	}
	return matched, found
}

func normalizeCode(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	return strings.NewReplacer("-", "", " ", "").Replace(code)
}

func hashRecoveryCode(normalized string) string {
	h := sha256.Sum256([]byte("termstead-recovery:" + normalized))
	return hex.EncodeToString(h[:])
}

// newRecoveryCodes returns display codes (xxxxx-xxxxx) and their hashes.
func newRecoveryCodes() (codes, hashes []string) {
	for range recoveryCodeCount {
		b := make([]byte, 10)
		if _, err := rand.Read(b); err != nil {
			panic("auth: crypto/rand failed: " + err.Error())
		}
		for i := range b {
			b[i] = recoveryAlphabet[int(b[i])%len(recoveryAlphabet)]
		}
		codes = append(codes, string(b[:5])+"-"+string(b[5:]))
		hashes = append(hashes, hashRecoveryCode(string(b)))
	}
	return codes, hashes
}

// checkSecondFactor validates a TOTP code (with replay guard) or consumes a recovery code.
func (s *Service) checkSecondFactor(ctx context.Context, ua *store.UserAuth, code string) (bool, error) {
	c := normalizeCode(code)
	if len(c) == 6 {
		secret, err := s.d.Vault.SystemOpen(ua.TOTPSecretEnc)
		if err != nil {
			return false, httpx.Internal(err)
		}
		step, ok := verifyTOTPCode(string(secret), c, time.Now())
		if !ok {
			return false, nil
		}
		return s.d.Store.Users.AdvanceTOTPStep(ctx, ua.User.ID, step)
	}
	if len(c) == 10 {
		ok, err := s.d.Store.Users.ConsumeRecoveryCode(ctx, ua.User.ID, hashRecoveryCode(c))
		if ok {
			s.d.Audit.LogUser(ctx, ua.User, "auth.totp.recovery_used", ua.User.ID, nil)
		}
		return ok, err
	}
	return false, nil
}

type totpSetupResponse struct {
	Secret     string `json:"secret"`
	OtpauthURL string `json:"otpauthUrl"`
}

func (s *Service) handleTOTPSetup(c *echo.Context) error {
	u := httpx.UserFrom(c)
	if u.TOTPEnabled {
		return httpx.Conflict("two-factor authentication is already enabled; disable it first")
	}
	if err := s.RequireRecentAuth(c, ""); err != nil {
		return err
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
	if err := s.d.Store.Users.SetTOTP(c.Request().Context(), u.ID, sealed, false, nil); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, totpSetupResponse{Secret: key.Secret(), OtpauthURL: key.URL()})
}

type codeRequest struct {
	Code string `json:"code"`
}

type recoveryCodesResponse struct {
	RecoveryCodes []string `json:"recoveryCodes"`
}

func (s *Service) handleTOTPEnable(c *echo.Context) error {
	var req codeRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	u := httpx.UserFrom(c)
	ctx := c.Request().Context()
	ua, err := s.d.Store.Users.GetAuth(ctx, u.ID)
	if err != nil {
		return err
	}
	if ua.User.TOTPEnabled {
		return httpx.Conflict("two-factor authentication is already enabled")
	}
	if len(ua.TOTPSecretEnc) == 0 {
		return httpx.BadRequest("call /api/auth/totp/setup first")
	}
	key := "totp-enable:" + u.ID
	if d := s.limiter.retryAfter(key); d > 0 {
		return tooMany(d)
	}
	secret, err := s.d.Vault.SystemOpen(ua.TOTPSecretEnc)
	if err != nil {
		return httpx.Internal(err)
	}
	step, ok := verifyTOTPCode(string(secret), normalizeCode(req.Code), time.Now())
	if !ok {
		s.limiter.fail(key, passwordFailures)
		return httpx.NewError(http.StatusBadRequest, "totp_invalid", "invalid code — check the authenticator app and the device clock")
	}
	s.limiter.reset(key)
	codes, hashes := newRecoveryCodes()
	if err := s.d.Store.Users.SetTOTP(ctx, u.ID, ua.TOTPSecretEnc, true, hashes); err != nil {
		return err
	}
	if _, err := s.d.Store.Users.AdvanceTOTPStep(ctx, u.ID, step); err != nil {
		return err
	}
	s.d.Audit.Log(c, "auth.totp.enable", u.ID, nil)
	return c.JSON(http.StatusOK, recoveryCodesResponse{RecoveryCodes: codes})
}

type passwordOnlyRequest struct {
	Password string `json:"password"`
}

func (s *Service) handleTOTPDisable(c *echo.Context) error {
	var req passwordOnlyRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	u := httpx.UserFrom(c)
	if err := s.reauth(c, u, req.Password); err != nil {
		return err
	}
	if s.Policy(c.Request().Context()).mfaRequiredFor(u) {
		if m, err := s.mfaMethods(c.Request().Context(), u); err == nil && len(m) == 1 && m[0] == MethodTOTP {
			return httpx.NewError(http.StatusConflict, "mfa_required",
				"the login policy requires a second factor: register a passkey before turning off the authenticator app")
		}
	}
	if err := s.d.Store.Users.SetTOTP(c.Request().Context(), u.ID, nil, false, nil); err != nil {
		return err
	}
	s.d.Audit.Log(c, "auth.totp.disable", u.ID, nil)
	return httpx.OK(c)
}

// handleTOTPRecoveryCodes regenerates the recovery codes (invalidating the old ones); requires the password.
func (s *Service) handleTOTPRecoveryCodes(c *echo.Context) error {
	var req passwordOnlyRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	u := httpx.UserFrom(c)
	if !u.TOTPEnabled {
		return httpx.Conflict("two-factor authentication is not enabled")
	}
	if err := s.reauth(c, u, req.Password); err != nil {
		return err
	}
	codes, hashes := newRecoveryCodes()
	if err := s.d.Store.Users.SetRecoveryCodes(c.Request().Context(), u.ID, hashes); err != nil {
		return err
	}
	s.d.Audit.Log(c, "auth.totp.recovery_regenerate", u.ID, nil)
	return c.JSON(http.StatusOK, recoveryCodesResponse{RecoveryCodes: codes})
}
