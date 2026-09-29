package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/labstack/echo/v5"

	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
)

// Policy is the administrator's login policy (SEC-20, MU-1, MU-5). It lives in the module-private settings scope
// "auth" (key "policy"), not in the global scope that GET /api/settings merges into every user's settings.
type Policy struct {
	// Password rules (password changes, admin-created users, resets; the first-run setup uses the defaults).
	PasswordMinLength        int  `json:"passwordMinLength"`        // 8..128
	PasswordRequireClasses   int  `json:"passwordRequireClasses"`   // 0..4 of lower / upper / digit / symbol
	PasswordDisallowUsername bool `json:"passwordDisallowUsername"` // reject passwords containing the user name
	// Brute-force protection: after LockoutThreshold failures per client IP + user name, further attempts back off
	// exponentially (1 s doubling) up to LockoutMaxMinutes.
	LockoutThreshold  int `json:"lockoutThreshold"`  // 1..100
	LockoutMaxMinutes int `json:"lockoutMaxMinutes"` // 1..1440
	// Account lockout (independent of the IP): AccountLockThreshold consecutive failures lock the account for
	// AccountLockMinutes (0 = off). A locked account cannot sign in with its password even when it is correct.
	AccountLockThreshold int `json:"accountLockThreshold"` // 0..100
	AccountLockMinutes   int `json:"accountLockMinutes"`   // 1..10080
	// Session lifetime: SessionIdleHours of inactivity end a normal session, RememberDays a "keep me signed in" one
	// (0 disables remembering); SessionMaxDays caps any session regardless of activity (0 = no cap).
	SessionIdleHours int `json:"sessionIdleHours"` // 1..8760
	RememberDays     int `json:"rememberDays"`     // 0..365
	SessionMaxDays   int `json:"sessionMaxDays"`   // 0..365
	// RequireMFA: "off" | "admins" | "all" — password logins of matching accounts without a second factor must
	// enrol TOTP during sign-in. SSO logins leave MFA to the identity provider.
	RequireMFA string `json:"requireMfa"`
	// PasswordLogin: false = SSO / passkeys only for non-administrators (administrators keep password logins as the
	// break-glass path).
	PasswordLogin bool `json:"passwordLogin"`
	// AllowedNetworks: CIDRs / IPs allowed to sign in (empty = anywhere). Loopback clients are always allowed.
	AllowedNetworks []string `json:"allowedNetworks"`
}

// DefaultPolicy is the policy of a fresh instance (it matches the behaviour before policies existed).
func DefaultPolicy() Policy {
	return Policy{
		PasswordMinLength: MinPasswordLen, LockoutThreshold: loginUserFailures, LockoutMaxMinutes: 15,
		AccountLockMinutes: 15, SessionIdleHours: int(SessionTTL / time.Hour), RememberDays: int(RememberTTL / (24 * time.Hour)),
		RequireMFA: "off", PasswordLogin: true, AllowedNetworks: []string{},
	}
}

const (
	policyScope = "auth"
	policyKey   = "policy"
)

// normalize validates p and fills missing values.
func (p *Policy) normalize() error {
	bad := func(field string, lo, hi int) error { return fmt.Errorf("%s must be between %d and %d", field, lo, hi) }
	switch {
	case p.PasswordMinLength < MinPasswordLen || p.PasswordMinLength > 128:
		return bad("passwordMinLength", MinPasswordLen, 128)
	case p.PasswordRequireClasses < 0 || p.PasswordRequireClasses > 4:
		return bad("passwordRequireClasses", 0, 4)
	case p.LockoutThreshold < 1 || p.LockoutThreshold > 100:
		return bad("lockoutThreshold", 1, 100)
	case p.LockoutMaxMinutes < 1 || p.LockoutMaxMinutes > 1440:
		return bad("lockoutMaxMinutes", 1, 1440)
	case p.AccountLockThreshold < 0 || p.AccountLockThreshold > 100:
		return bad("accountLockThreshold", 0, 100)
	case p.AccountLockMinutes < 1 || p.AccountLockMinutes > 10080:
		return bad("accountLockMinutes", 1, 10080)
	case p.SessionIdleHours < 1 || p.SessionIdleHours > 8760:
		return bad("sessionIdleHours", 1, 8760)
	case p.RememberDays < 0 || p.RememberDays > 365:
		return bad("rememberDays", 0, 365)
	case p.SessionMaxDays < 0 || p.SessionMaxDays > 365:
		return bad("sessionMaxDays", 0, 365)
	}
	switch p.RequireMFA {
	case "":
		p.RequireMFA = "off"
	case "off", "admins", "all":
	default:
		return fmt.Errorf("requireMfa must be off, admins or all")
	}
	if len(p.AllowedNetworks) > 256 {
		return fmt.Errorf("allowedNetworks: at most 256 entries")
	}
	nets := make([]string, 0, len(p.AllowedNetworks))
	for _, n := range p.AllowedNetworks {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		pfx, err := parseNetwork(n)
		if err != nil {
			return fmt.Errorf("allowedNetworks: %q is not an IP address or CIDR", n)
		}
		nets = append(nets, pfx.String())
	}
	p.AllowedNetworks = nets
	return nil
}

func parseNetwork(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// Policy returns the current login policy (cached; invalidated by the admin endpoint).
func (s *Service) Policy(ctx context.Context) Policy {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	if s.policy != nil {
		return clonePolicy(*s.policy)
	}
	p := DefaultPolicy()
	if ok, err := s.d.Store.Settings.GetJSON(ctx, policyScope, policyKey, &p); err != nil {
		s.log.Warn("load login policy", "err", err)
		return DefaultPolicy() // not cached: retry next time
	} else if ok {
		if err := p.normalize(); err != nil {
			s.log.Warn("stored login policy is invalid; using defaults", "err", err)
			p = DefaultPolicy()
		}
	}
	s.policy = &p
	s.limiter.setMaxLock(time.Duration(p.LockoutMaxMinutes) * time.Minute)
	return clonePolicy(p)
}

func clonePolicy(p Policy) Policy {
	p.AllowedNetworks = append([]string{}, p.AllowedNetworks...)
	return p
}

// ValidatePasswordFor checks password against the login policy for username.
func (p Policy) ValidatePasswordFor(password, username string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	if utf8.RuneCountInString(password) < p.PasswordMinLength {
		return fmt.Errorf("password must be at least %d characters", p.PasswordMinLength)
	}
	if p.PasswordRequireClasses > 0 {
		var lower, upper, digit, other bool
		for _, r := range password {
			switch {
			case unicode.IsLower(r):
				lower = true
			case unicode.IsUpper(r):
				upper = true
			case unicode.IsDigit(r):
				digit = true
			default:
				other = true
			}
		}
		n := b2i(lower) + b2i(upper) + b2i(digit) + b2i(other)
		if n < p.PasswordRequireClasses {
			return fmt.Errorf("password must mix at least %d of: lowercase letters, uppercase letters, digits, symbols", p.PasswordRequireClasses)
		}
	}
	if p.PasswordDisallowUsername && len(username) >= 3 && strings.Contains(strings.ToLower(password), strings.ToLower(username)) {
		return fmt.Errorf("password must not contain the user name")
	}
	return nil
}

// mfaRequiredFor reports whether the policy requires a second factor for u.
func (p Policy) mfaRequiredFor(u *model.User) bool {
	switch p.RequireMFA {
	case "all":
		return true
	case "admins":
		return u.IsAdmin()
	}
	return false
}

// loginAllowedFrom reports whether a client at ip may sign in.
func (p Policy) loginAllowedFrom(ip string) bool {
	if len(p.AllowedNetworks) == 0 {
		return true
	}
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	if a.IsLoopback() {
		return true // break-glass on the host itself
	}
	for _, n := range p.AllowedNetworks {
		if pfx, err := netip.ParsePrefix(n); err == nil && pfx.Contains(a) {
			return true
		}
	}
	return false
}

func (p Policy) idleTTL() time.Duration { return time.Duration(p.SessionIdleHours) * time.Hour }
func (p Policy) rememberTTL() time.Duration {
	return time.Duration(p.RememberDays) * 24 * time.Hour
}

// ---- admin endpoints ----------------------------------------------------------------------------------------------

type policyResponse struct {
	Policy
	Defaults Policy `json:"defaults"`
}

func (s *Service) handleGetPolicy(c *echo.Context) error {
	return c.JSON(http.StatusOK, policyResponse{Policy: s.Policy(c.Request().Context()), Defaults: DefaultPolicy()})
}

// handlePutPolicy applies a partial update (omitted keys keep their value).
func (s *Service) handlePutPolicy(c *echo.Context) error {
	ctx := c.Request().Context()
	cur := s.Policy(ctx)
	next := clonePolicy(cur)
	if err := httpx.Bind(c, &next); err != nil {
		return err
	}
	if next.AllowedNetworks == nil {
		next.AllowedNetworks = []string{}
	}
	if err := next.normalize(); err != nil {
		return httpx.BadRequest(err.Error())
	}
	if err := s.RequireAdminSudo(c); err != nil {
		return err
	}
	// Do not lock the administrator out: their own client must stay allowed.
	if ip := httpx.ClientIP(c); !next.loginAllowedFrom(ip) {
		return httpx.NewError(http.StatusConflict, "self_lockout",
			"the allowed networks would not include your own address ("+ip+"); add it first")
	}
	if err := s.d.Store.Settings.SetJSON(ctx, policyScope, policyKey, next); err != nil {
		return err
	}
	s.policyMu.Lock()
	s.policy = nil
	s.policyMu.Unlock()
	s.d.Audit.Log(c, "admin.auth.policy", "", policyDiff(cur, next))
	return c.JSON(http.StatusOK, policyResponse{Policy: s.Policy(ctx), Defaults: DefaultPolicy()})
}

// policyDiff lists the changed keys (old → new) for the audit log.
func policyDiff(a, b Policy) map[string]any {
	out := map[string]any{}
	add := func(k string, x, y any) {
		if fmt.Sprint(x) != fmt.Sprint(y) {
			out[k] = map[string]any{"from": x, "to": y}
		}
	}
	add("passwordMinLength", a.PasswordMinLength, b.PasswordMinLength)
	add("passwordRequireClasses", a.PasswordRequireClasses, b.PasswordRequireClasses)
	add("passwordDisallowUsername", a.PasswordDisallowUsername, b.PasswordDisallowUsername)
	add("lockoutThreshold", a.LockoutThreshold, b.LockoutThreshold)
	add("lockoutMaxMinutes", a.LockoutMaxMinutes, b.LockoutMaxMinutes)
	add("accountLockThreshold", a.AccountLockThreshold, b.AccountLockThreshold)
	add("accountLockMinutes", a.AccountLockMinutes, b.AccountLockMinutes)
	add("sessionIdleHours", a.SessionIdleHours, b.SessionIdleHours)
	add("rememberDays", a.RememberDays, b.RememberDays)
	add("sessionMaxDays", a.SessionMaxDays, b.SessionMaxDays)
	add("requireMfa", a.RequireMFA, b.RequireMFA)
	add("passwordLogin", a.PasswordLogin, b.PasswordLogin)
	add("allowedNetworks", a.AllowedNetworks, b.AllowedNetworks)
	return out
}
