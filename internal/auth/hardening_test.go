package auth_test

import (
	"net/netip"
	"net/url"
	"strconv"
	"testing"

	"github.com/nexterm/nexterm/internal/auth"
	"github.com/nexterm/nexterm/internal/config"
	"github.com/nexterm/nexterm/internal/server/servertest"
)

// trustLoopbackProxy makes the test server honour X-Forwarded-For from the (loopback) test client, so tests can
// simulate remote clients.
func trustLoopbackProxy(c *config.Config) {
	c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
}

// remoteClient returns an anonymous client that appears to connect from ip (needs trustLoopbackProxy).
func remoteClient(env *servertest.Env, ip string) *servertest.Client {
	c := env.Client()
	c.Header.Set("X-Forwarded-For", ip)
	return c
}

// makeStale moves every login session's creation time back an hour: sensitive changes then need re-verification.
func makeStale(t *testing.T, env *servertest.Env) {
	t.Helper()
	if _, err := env.Server.Deps.Store.DB.Exec(`UPDATE auth_sessions SET created_at = created_at - 3600000`); err != nil {
		t.Fatal(err)
	}
}

func TestRateKeyIP(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.9":                 "203.0.113.9",
		"::ffff:203.0.113.9":          "203.0.113.9",
		"2001:db8:1:2:aaaa::1":        "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff:ffff:0:99": "2001:db8:1:2::/64",
		"not-an-ip":                   "not-an-ip",
	} {
		if got := auth.RateKeyIP(in); got != want {
			t.Errorf("RateKeyIP(%q) = %q, want %q", in, got, want)
		}
	}
}

// A password spray from many addresses of one IPv6 /64 — or from many unrelated addresses — still backs off.
func TestDistributedLoginBackoff(t *testing.T) {
	env := servertest.New(t, trustLoopbackProxy)
	env.Setup("admin", adminPass)
	bad := map[string]string{"username": "admin", "password": "wrong password"}

	// Rotating the interface identifier inside one /64 shares the per-client key (threshold 5).
	throttled := false
	for i := range 8 {
		code, _ := remoteClient(env, "2001:db8:5:6::"+strconv.Itoa(i+1)).ErrorCode("POST", "/api/auth/login", bad)
		if code == 429 {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Fatal("rotating addresses within one /64 was never throttled")
	}

	// Distinct addresses: the per-user-name key (threshold 20) kicks in.
	throttled = false
	for i := range 30 {
		ip := "198.51.100." + strconv.Itoa(i+1)
		if code, _ := remoteClient(env, ip).ErrorCode("POST", "/api/auth/login", bad); code == 429 {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Fatal("password spray over many addresses was never throttled")
	}
	// The host itself is the break-glass path.
	env.Login("admin", adminPass)
}

// Bad codes at the second-factor step count towards the account lock like bad passwords.
func TestMFAStepFailuresLockAccount(t *testing.T) {
	env := servertest.New(t, trustLoopbackProxy)
	admin := env.Setup("admin", adminPass)
	admin.MustJSON("PUT", "/api/admin/auth/policy", map[string]any{"accountLockThreshold": 3, "accountLockMinutes": 5}, nil)
	bob := env.CreateUser(admin, "bob", "bob password", "user")
	enableTOTP(t, bob)

	c := remoteClient(env, "203.0.113.20")
	for range 3 {
		st, ch := loginChallenge(t, c, "bob", "bob password")
		if st != 401 || ch.MFAToken == "" {
			t.Fatalf("challenge: %d %+v", st, ch)
		}
		c.ErrorCode("POST", "/api/auth/mfa/totp", map[string]string{"mfaToken": ch.MFAToken, "code": "000000"})
	}
	if code, ec := remoteClient(env, "203.0.113.21").ErrorCode("POST", "/api/auth/login", map[string]string{"username": "bob", "password": "bob password"}); code != 403 || ec != "account_locked" {
		t.Fatalf("after 3 bad codes: %d %s", code, ec)
	}
}

// Signing in ends the session the browser presented before (no lingering / planted sessions).
func TestLoginReplacesPresentedSession(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	env.CreateUser(admin, "bob", "bob password", "user")
	c := env.Login("bob", "bob password")
	old := c.HTTP.Jar.Cookies(mustURL(t, env.URL("/")))
	count := func() (n int) {
		env.Server.Deps.Store.DB.QueryRow(`SELECT COUNT(*) FROM auth_sessions WHERE user_id = (SELECT id FROM users WHERE username = 'bob')`).Scan(&n)
		return n
	}
	before := count()
	c.MustJSON("POST", "/api/auth/login", map[string]string{"username": "bob", "password": "bob password"}, nil)
	if n := count(); n != before {
		t.Fatalf("sessions after re-login: %d (the previous one should have ended)", n)
	}
	// The old cookie no longer works.
	stale := env.Client()
	stale.HTTP.Jar.SetCookies(mustURL(t, env.URL("/")), old)
	if getState(t, stale).Authenticated {
		t.Fatal("previous session still valid")
	}
}

// Sensitive account and admin changes need a recent sign-in (or the password / a re-verification).
func TestRecentAuthGates(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	bob := env.CreateUser(admin, "bob", "bob password", "user")
	var users []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	admin.MustJSON("GET", "/api/admin/users", nil, &users)
	var bobID string
	for _, u := range users {
		if u.Username == "bob" {
			bobID = u.ID
		}
	}
	makeStale(t, env)

	expectReauth := func(c *servertest.Client, method, path string, body any) {
		t.Helper()
		if code, ec := c.ErrorCode(method, path, body); code != 403 || ec != "reauth_required" {
			t.Fatalf("%s %s on a stale session: %d %s", method, path, code, ec)
		}
	}
	expectReauth(bob, "POST", "/api/auth/tokens", map[string]any{"name": "ci"})
	expectReauth(bob, "POST", "/api/auth/totp/setup", nil)
	expectReauth(admin, "POST", "/api/admin/users/"+bobID+"/reset-password", map[string]string{"password": "new bob password"})
	expectReauth(admin, "POST", "/api/admin/users/"+bobID+"/reset-mfa", nil)
	expectReauth(admin, "PATCH", "/api/admin/users/"+bobID, map[string]string{"role": "admin"})
	expectReauth(admin, "POST", "/api/admin/users", map[string]string{"username": "eve", "password": "eve password", "role": "admin"})
	expectReauth(admin, "PUT", "/api/admin/auth/policy", map[string]any{"requireMfa": "all"})
	expectReauth(admin, "POST", "/api/admin/oidc/providers", map[string]any{"name": "Corp", "issuer": "https://idp.example.com", "clientId": "x"})
	expectReauth(admin, "PUT", "/api/admin/webauthn/config", map[string]any{"rpId": "", "origins": []string{}})
	// Harmless admin changes stay one click.
	admin.MustJSON("PATCH", "/api/admin/users/"+bobID, map[string]string{"displayName": "Bob"}, nil)
	admin.MustJSON("POST", "/api/admin/users", map[string]string{"username": "carol", "password": "carol password"}, nil)

	// The password in the body (tokens) or a re-verification authorizes.
	var tok struct {
		Token string `json:"token"`
	}
	bob.MustJSON("POST", "/api/auth/tokens", map[string]any{"name": "ci", "password": "bob password"}, &tok)
	bob.MustJSON("POST", "/api/auth/totp/setup", nil, nil) // re-verified by the password above (10 minutes)
	if code, ec := bob.ErrorCode("POST", "/api/auth/tokens", map[string]any{"name": "ci2", "password": "wrong password"}); code != 403 || ec != "invalid_password" {
		t.Fatalf("wrong password: %d %s", code, ec)
	}
	admin.MustJSON("POST", "/api/auth/verify-password", map[string]string{"password": adminPass}, nil)
	admin.MustJSON("PATCH", "/api/admin/users/"+bobID, map[string]string{"role": "admin"}, nil)

	// API tokens: admin actions accepted (creating the token needed a recent sign-in), account changes are not.
	var at struct {
		Token string `json:"token"`
	}
	admin.MustJSON("POST", "/api/auth/tokens", map[string]any{"name": "automation"}, &at)
	api := env.Client()
	api.CSRF = false
	api.Bearer = at.Token
	api.MustJSON("PATCH", "/api/admin/users/"+bobID, map[string]string{"role": "user"}, nil)
	expectReauth(api, "POST", "/api/auth/tokens", map[string]any{"name": "more"})
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
