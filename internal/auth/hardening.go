package auth

import (
	"context"
	"net/netip"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// RateKeyIP returns the rate-limiting identity of a client address: IPv4 addresses as they are, IPv6 addresses by
// their /64 prefix (one subscriber usually controls a whole /64, so per-address keys would be trivial to rotate).
func RateKeyIP(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap()
	if a.Is6() {
		if p, err := a.Prefix(64); err == nil {
			return p.String()
		}
	}
	return a.String()
}

// isLoopbackIP reports whether ip is a loopback address (a client on the AstraTerm host itself).
func isLoopbackIP(ip string) bool {
	a, err := netip.ParseAddr(ip)
	return err == nil && a.Unmap().IsLoopback()
}

// RequireAdminSudo authorizes an administrator action that could take over other accounts or change how everyone
// signs in (password / 2FA resets, granting the admin role, login policy, identity providers, passkey relying party).
// Cookie sessions need a recent sign-in or re-verification (403 reauth_required otherwise); API tokens are accepted
// as they are, because creating one already required a recent authentication.
func (s *Service) RequireAdminSudo(c *echo.Context) error {
	if httpx.AuthInfoFrom(c).Method == httpx.AuthToken {
		return nil
	}
	return s.RequireRecentAuth(c, "")
}

// MFARequiredFor reports whether the login policy requires a second factor for u.
func (s *Service) MFARequiredFor(ctx context.Context, u *model.User) bool {
	return s.Policy(ctx).mfaRequiredFor(u)
}

// noteLoginFailure counts a failed sign-in step of u towards the account lockout (and audits the lock).
func (s *Service) noteLoginFailure(c *echo.Context, u *model.User, pol Policy) {
	if u == nil {
		return
	}
	until, err := s.recordLoginFailure(c.Request().Context(), u.ID, pol.AccountLockThreshold, time.Duration(pol.AccountLockMinutes)*time.Minute)
	if err != nil {
		s.log.Warn("record login failure", "err", err)
	} else if !until.IsZero() {
		s.d.Audit.LogUser(c, u, "auth.account.locked", u.ID, map[string]any{"until": until, "failures": pol.AccountLockThreshold})
	}
}

// HasSSOIdentity reports whether userID has signed in with (or linked) a single-sign-on identity.
func (s *Service) HasSSOIdentity(ctx context.Context, userID string) (bool, error) {
	p := s.ssoProvider()
	if p == nil {
		return false, nil
	}
	m, err := p.LinkedProviders(ctx)
	if err != nil {
		return false, err
	}
	return len(m[userID]) > 0, nil
}
