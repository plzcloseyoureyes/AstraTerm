package auth_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/server/servertest"
)

const adminPass = "correct horse battery"

type state struct {
	SetupRequired          bool            `json:"setupRequired"`
	Authenticated          bool            `json:"authenticated"`
	User                   *model.User     `json:"user"`
	Mode                   string          `json:"mode"`
	VaultLocked            bool            `json:"vaultLocked"`
	VaultHasMasterPassword bool            `json:"vaultHasMasterPassword"`
	Version                string          `json:"version"`
	Features               map[string]bool `json:"features"`
}

func getState(t *testing.T, c *servertest.Client) state {
	t.Helper()
	var s state
	c.MustJSON("GET", "/api/auth/state", nil, &s)
	return s
}

func TestSetupLoginStateLogout(t *testing.T) {
	env := servertest.New(t)
	anon := env.Client()

	s := getState(t, anon)
	if !s.SetupRequired || s.Authenticated || s.Mode != "desktop" || s.Version != "test" {
		t.Fatalf("initial state: %+v", s)
	}
	for _, f := range []string{"guacd", "docker", "kubectl", "mosh", "wsl"} {
		if _, ok := s.Features[f]; !ok {
			t.Fatalf("feature %q missing: %v", f, s.Features)
		}
	}
	// Protected routes need a login.
	if st, code := anon.ErrorCode("GET", "/api/connections", nil); st != 401 || code != "unauthorized" {
		t.Fatalf("anonymous: %d %s", st, code)
	}
	// Validation.
	if st, _ := anon.ErrorCode("POST", "/api/auth/setup", map[string]string{"username": "admin", "password": "short"}); st != 400 {
		t.Fatalf("weak password accepted: %d", st)
	}
	if st, _ := anon.ErrorCode("POST", "/api/auth/setup", map[string]string{"username": "bad name!", "password": adminPass}); st != 400 {
		t.Fatalf("bad username accepted: %d", st)
	}

	admin := env.Setup("admin", adminPass)
	s = getState(t, admin)
	if s.SetupRequired || !s.Authenticated || s.User == nil || s.User.Role != model.RoleAdmin || s.User.Username != "admin" {
		t.Fatalf("after setup: %+v", s)
	}
	if st, _ := anon.ErrorCode("POST", "/api/auth/setup", map[string]string{"username": "evil", "password": adminPass}); st != 409 {
		t.Fatalf("second setup: %d", st)
	}

	// Cookie attributes.
	resp, _ := env.Client().Do("POST", "/api/auth/login", map[string]any{"username": "admin", "password": adminPass, "remember": true})
	ck := findCookie(resp, "astraterm_session")
	if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.MaxAge != 30*24*3600 || ck.Path != "/" {
		t.Fatalf("cookie: %+v", ck)
	}

	if st, code := anon.ErrorCode("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "wrong password"}); st != 401 || code != "invalid_credentials" {
		t.Fatalf("wrong password: %d %s", st, code)
	}
	if st, code := anon.ErrorCode("POST", "/api/auth/login", map[string]string{"username": "nobody", "password": "wrong password"}); st != 401 || code != "invalid_credentials" {
		t.Fatalf("unknown user: %d %s", st, code)
	}
	user := env.Login("ADMIN", adminPass) // case-insensitive username
	if !getState(t, user).Authenticated {
		t.Fatal("login did not authenticate")
	}

	admin.MustJSON("POST", "/api/auth/logout", nil, nil)
	if getState(t, admin).Authenticated {
		t.Fatal("still authenticated after logout")
	}
	if st, _ := admin.ErrorCode("GET", "/api/connections", nil); st != 401 {
		t.Fatalf("after logout: %d", st)
	}
	if !getState(t, user).Authenticated {
		t.Fatal("logout of one session ended another")
	}

	// Login sessions list + revoke.
	var sessions []model.AuthSession
	user.MustJSON("GET", "/api/auth/sessions", nil, &sessions)
	var current, other string
	for _, se := range sessions {
		if se.Current {
			current = se.ID
		} else if other == "" {
			other = se.ID
		}
	}
	if current == "" || other == "" {
		t.Fatalf("sessions: %+v", sessions)
	}
	user.MustJSON("DELETE", "/api/auth/sessions/"+other, nil, nil)
	if st, _ := user.ErrorCode("DELETE", "/api/auth/sessions/"+other, nil); st != 404 {
		t.Fatalf("double revoke: %d", st)
	}

	// Audit trail recorded the login activity.
	var entries []model.AuditEntry
	user.MustJSON("GET", "/api/audit/me?action=auth.", nil, &entries)
	if len(entries) < 3 {
		t.Fatalf("audit entries: %d", len(entries))
	}
}

func findCookie(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestCSRFAndBearerTokens(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)

	noCSRF := *admin
	noCSRF.CSRF = false
	if st, code := noCSRF.ErrorCode("POST", "/api/folders", map[string]string{"name": "x"}); st != 403 || code != "csrf" {
		t.Fatalf("missing CSRF header: %d %s", st, code)
	}
	if st, code := env.Client().ErrorCode("GET", "/api/auth/state", nil); st != 200 || code != "" {
		t.Fatalf("GET without CSRF: %d", st)
	}
	anon := env.Client()
	anon.CSRF = false
	if st, code := anon.ErrorCode("POST", "/api/auth/login", map[string]string{"username": "admin", "password": adminPass}); st != 403 || code != "csrf" {
		t.Fatalf("login without CSRF header: %d %s", st, code)
	}

	var created struct {
		model.APIToken
		Token string `json:"token"`
	}
	if st := admin.JSON("POST", "/api/auth/tokens", map[string]any{"name": "ci", "expiresInDays": 30}, &created); st != 201 {
		t.Fatalf("create token: %d", st)
	}
	if !strings.HasPrefix(created.Token, "nxt_") || created.ID == "" || created.ExpiresAt == nil {
		t.Fatalf("token response: %+v", created)
	}
	var tokens []model.APIToken
	admin.MustJSON("GET", "/api/auth/tokens", nil, &tokens)
	if len(tokens) != 1 || tokens[0].Name != "ci" {
		t.Fatalf("tokens: %+v", tokens)
	}
	raw, _ := json.Marshal(tokens)
	if strings.Contains(string(raw), created.Token) || strings.Contains(string(raw), "tokenHash") {
		t.Fatalf("token material leaked in list: %s", raw)
	}

	// Bearer auth works without cookies and without the CSRF header.
	bot := env.Client()
	bot.CSRF, bot.Bearer = false, created.Token
	s := getState(t, bot)
	if !s.Authenticated || s.User.Username != "admin" {
		t.Fatalf("bearer state: %+v", s)
	}
	if st := bot.JSON("POST", "/api/folders", map[string]string{"name": "from-token"}, nil); st != 201 {
		t.Fatalf("bearer mutation: %d", st)
	}
	bad := env.Client()
	bad.Bearer = "nxt_notavalidtoken"
	if st, _ := bad.ErrorCode("GET", "/api/folders", nil); st != 401 {
		t.Fatalf("invalid bearer: %d", st)
	}

	admin.MustJSON("DELETE", "/api/auth/tokens/"+created.ID, nil, nil)
	if st, _ := bot.ErrorCode("GET", "/api/folders", nil); st != 401 {
		t.Fatalf("revoked token still works: %d", st)
	}
}

func TestTOTPFlow(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)

	var setup struct {
		Secret     string `json:"secret"`
		OtpauthURL string `json:"otpauthUrl"`
	}
	admin.MustJSON("POST", "/api/auth/totp/setup", nil, &setup)
	if setup.Secret == "" || !strings.HasPrefix(setup.OtpauthURL, "otpauth://totp/") {
		t.Fatalf("setup: %+v", setup)
	}
	if st, code := admin.ErrorCode("POST", "/api/auth/totp/enable", map[string]string{"code": "000000"}); st != 400 || code != "totp_invalid" {
		t.Fatalf("enable with bad code: %d %s", st, code)
	}
	now := time.Now()
	code, _ := totp.GenerateCodeCustom(setup.Secret, now, totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	var enabled struct {
		RecoveryCodes []string `json:"recoveryCodes"`
	}
	admin.MustJSON("POST", "/api/auth/totp/enable", map[string]string{"code": code}, &enabled)
	if len(enabled.RecoveryCodes) != 10 {
		t.Fatalf("recovery codes: %v", enabled.RecoveryCodes)
	}
	if !getState(t, admin).User.TOTPEnabled {
		t.Fatal("totpEnabled not reported")
	}

	c := env.Client()
	login := func(totpCode string) (int, string) {
		return c.ErrorCode("POST", "/api/auth/login", map[string]string{"username": "admin", "password": adminPass, "totp": totpCode})
	}
	if st, code := login(""); st != 401 || code != "totp_required" {
		t.Fatalf("missing totp: %d %s", st, code)
	}
	// The code used for enabling was consumed (replay guard): the same step is rejected.
	if st, c2 := login(code); st != 401 || c2 != "totp_invalid" {
		t.Fatalf("replayed code: %d %s", st, c2)
	}
	next, _ := totp.GenerateCodeCustom(setup.Secret, now.Add(30*time.Second), totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if st, _ := login(next); st != 200 {
		t.Fatalf("valid totp login: %d", st)
	}
	if st, _ := login(next); st != 401 {
		t.Fatalf("same code accepted twice: %d", st)
	}
	// Recovery codes work exactly once (formatting-insensitive).
	rc := strings.ToUpper(enabled.RecoveryCodes[0])
	if st, _ := login(rc); st != 200 {
		t.Fatalf("recovery code login: %d", st)
	}
	if st, _ := login(rc); st != 401 {
		t.Fatalf("recovery code reused: %d", st)
	}

	// Regenerate requires the password; disable requires the password.
	if st, code := admin.ErrorCode("POST", "/api/auth/totp/recovery-codes", map[string]string{"password": "nope nope"}); st != 403 || code != "invalid_password" {
		t.Fatalf("regenerate with wrong password: %d %s", st, code)
	}
	var regen struct {
		RecoveryCodes []string `json:"recoveryCodes"`
	}
	admin.MustJSON("POST", "/api/auth/totp/recovery-codes", map[string]string{"password": adminPass}, &regen)
	if len(regen.RecoveryCodes) != 10 || regen.RecoveryCodes[0] == enabled.RecoveryCodes[0] {
		t.Fatalf("regenerated: %v", regen.RecoveryCodes)
	}
	if st, _ := login(enabled.RecoveryCodes[1]); st != 401 {
		t.Fatal("old recovery code still valid after regeneration")
	}
	admin.MustJSON("POST", "/api/auth/totp/disable", map[string]string{"password": adminPass}, nil)
	if st, _ := login(""); st != 200 {
		t.Fatalf("login after disabling totp: %d", st)
	}
}

func TestLoginRateLimit(t *testing.T) {
	env := servertest.New(t)
	env.Setup("admin", adminPass)
	c := env.Client()
	for i := range 5 {
		if st, _ := c.ErrorCode("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "wrong password"}); st != 401 {
			t.Fatalf("attempt %d: %d", i, st)
		}
	}
	resp, _ := c.Do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": adminPass})
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("expected 429 with Retry-After, got %d %v", resp.StatusCode, resp.Header)
	}
	// Other usernames from the same IP are not locked by the per-user limit.
	if st, _ := c.ErrorCode("POST", "/api/auth/login", map[string]string{"username": "someone", "password": "wrong password"}); st != 401 {
		t.Fatalf("other username: %d", st)
	}
}

// The desktop app (config.LocalAccount): no first-run setup. The administrator exists from the first start, the launch
// token signs in, and the account has no usable password.
func TestLocalAccount(t *testing.T) {
	env := servertest.New(t, func(c *config.Config) { c.LocalAccount = true })
	c := env.Client()
	if st := getState(t, c); st.SetupRequired || st.Authenticated {
		t.Fatalf("state before launch: %+v", st)
	}
	var out struct {
		User model.User `json:"user"`
	}
	c.MustJSON("POST", "/api/auth/launch", map[string]string{"token": env.Server.Auth.LaunchToken()}, &out)
	if out.User.Role != model.RoleAdmin || out.User.Username == "" || !getState(t, c).Authenticated {
		t.Fatalf("launch login: %+v", out)
	}
	var me struct {
		HasPassword bool `json:"hasPassword"`
	}
	c.MustJSON("GET", "/api/auth/me", nil, &me)
	if me.HasPassword {
		t.Fatal("the local account must not have a usable password")
	}
	if st, _ := env.Client().ErrorCode("POST", "/api/auth/login", map[string]string{"username": out.User.Username, "password": ""}); st == 200 {
		t.Fatal("password login must not work")
	}
	// Setting the first password needs no current one; afterwards a browser can sign in with it.
	c.MustJSON("POST", "/api/auth/password", map[string]string{"currentPassword": "", "newPassword": adminPass}, nil)
	env.Login(out.User.Username, adminPass)
}

func TestLaunchToken(t *testing.T) {
	env := servertest.New(t)
	tok := env.Server.Auth.LaunchToken()
	if tok == "" {
		t.Fatal("no launch token in desktop mode")
	}
	c := env.Client()
	if st, code := c.ErrorCode("POST", "/api/auth/launch", map[string]string{"token": tok}); st != 409 || code != "setup_required" {
		t.Fatalf("launch before setup: %d %s", st, code)
	}
	env.Setup("owner", adminPass)
	if st, code := c.ErrorCode("POST", "/api/auth/launch", map[string]string{"token": "wrong"}); st != 401 || code != "invalid_token" {
		t.Fatalf("wrong token: %d %s", st, code)
	}
	var out struct {
		User model.User `json:"user"`
	}
	c.MustJSON("POST", "/api/auth/launch", map[string]string{"token": tok}, &out)
	if out.User.Username != "owner" || !getState(t, c).Authenticated {
		t.Fatalf("launch login: %+v", out)
	}
	if st, _ := env.Client().ErrorCode("POST", "/api/auth/launch", map[string]string{"token": tok}); st != 401 {
		t.Fatalf("launch token reused: %d", st)
	}
	if env.Server.Auth.LaunchToken() != "" {
		t.Fatal("token not consumed")
	}

	srv := servertest.New(t, func(c *config.Config) { c.Mode = config.ModeServer })
	if srv.Server.Auth.LaunchToken() != "" {
		t.Fatal("launch token generated in server mode")
	}
	if st, _ := srv.Client().ErrorCode("POST", "/api/auth/launch", map[string]string{"token": "x"}); st != 404 {
		t.Fatalf("launch in server mode: %d", st)
	}
}

func TestPasswordChangeAndAdminUsers(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	other := env.Login("admin", adminPass)

	if st, code := admin.ErrorCode("POST", "/api/auth/verify-password", map[string]string{"password": "wrong password"}); st != 403 || code != "invalid_password" {
		t.Fatalf("verify wrong: %d %s", st, code)
	}
	admin.MustJSON("POST", "/api/auth/verify-password", map[string]string{"password": adminPass}, nil)
	if st, _ := admin.ErrorCode("POST", "/api/auth/password", map[string]string{"currentPassword": "wrong password", "newPassword": "new password 1"}); st != 403 {
		t.Fatalf("change with wrong current: %d", st)
	}
	admin.MustJSON("POST", "/api/auth/password", map[string]string{"currentPassword": adminPass, "newPassword": "new password 1"}, nil)
	if !getState(t, admin).Authenticated {
		t.Fatal("current session dropped by password change")
	}
	if getState(t, other).Authenticated {
		t.Fatal("other session survived password change")
	}
	env.Login("admin", "new password 1")

	// Admin user management.
	var u model.User
	if st := admin.JSON("POST", "/api/admin/users", map[string]string{"username": "bob", "password": "bob password", "role": "user"}, &u); st != 201 {
		t.Fatalf("create user: %d", st)
	}
	if st, _ := admin.ErrorCode("POST", "/api/admin/users", map[string]string{"username": "BOB", "password": "bob password"}); st != 409 {
		t.Fatalf("duplicate user: %d", st)
	}
	bob := env.Login("bob", "bob password")
	if st, _ := bob.ErrorCode("GET", "/api/admin/users", nil); st != 403 {
		t.Fatalf("non-admin on admin route: %d", st)
	}
	var self model.User
	json.Unmarshal(mustBody(t, admin, "GET", "/api/auth/state"), &struct {
		User *model.User `json:"user"`
	}{&self})
	if st, _ := admin.ErrorCode("DELETE", "/api/admin/users/"+self.ID, nil); st != 409 {
		t.Fatalf("self delete: %d", st)
	}
	if st, _ := admin.ErrorCode("PATCH", "/api/admin/users/"+self.ID, map[string]string{"role": "user"}); st != 409 {
		t.Fatalf("demote last admin: %d", st)
	}
	admin.MustJSON("PATCH", "/api/admin/users/"+u.ID, map[string]any{"disabled": true, "displayName": "Bobby"}, &u)
	if !u.Disabled || u.DisplayName != "Bobby" {
		t.Fatalf("patched user: %+v", u)
	}
	if getState(t, bob).Authenticated {
		t.Fatal("disabled user still authenticated")
	}
	if st, code := env.Client().ErrorCode("POST", "/api/auth/login", map[string]string{"username": "bob", "password": "bob password"}); st != 403 || code != "account_disabled" {
		t.Fatalf("disabled login: %d %s", st, code)
	}
	admin.MustJSON("PATCH", "/api/admin/users/"+u.ID, map[string]any{"disabled": false, "role": "admin"}, nil)
	admin.MustJSON("POST", "/api/admin/users/"+u.ID+"/reset-password", map[string]string{"password": "fresh password"}, nil)
	env.Login("bob", "fresh password")
	admin.MustJSON("DELETE", "/api/admin/users/"+u.ID, nil, nil)
	var users []model.User
	admin.MustJSON("GET", "/api/admin/users", nil, &users)
	if len(users) != 1 {
		t.Fatalf("users after delete: %d", len(users))
	}
}

func TestHostHeaderGuard(t *testing.T) {
	env := servertest.New(t)
	req, _ := http.NewRequest("GET", env.URL("/api/auth/state"), nil)
	req.Host = "evil.example:7822"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("DNS-rebinding host accepted: %d", resp.StatusCode)
	}
	req.Host = "localhost"
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("localhost host rejected: %d", resp.StatusCode)
	}
}

func mustBody(t *testing.T, c *servertest.Client, method, path string) []byte {
	t.Helper()
	resp, b := c.Do(method, path, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%s %s: %d", method, path, resp.StatusCode)
	}
	return b
}
