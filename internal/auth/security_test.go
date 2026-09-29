package auth_test

import (
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/server/servertest"
)

var totpOpts = totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}

type challenge struct {
	Code     string   `json:"code"`
	Methods  []string `json:"methods"`
	MFAToken string   `json:"mfaToken"`
}

func loginChallenge(t *testing.T, c *servertest.Client, user, pass string) (int, challenge) {
	t.Helper()
	resp, body := c.Do("POST", "/api/auth/login", map[string]any{"username": user, "password": pass})
	var ch challenge
	json.Unmarshal(body, &ch)
	return resp.StatusCode, ch
}

func enableTOTP(t *testing.T, c *servertest.Client) (secret string, recovery []string) {
	t.Helper()
	var setup struct {
		Secret string `json:"secret"`
	}
	c.MustJSON("POST", "/api/auth/totp/setup", nil, &setup)
	code, _ := totp.GenerateCodeCustom(setup.Secret, time.Now().Add(-30*time.Second), totpOpts)
	var out struct {
		RecoveryCodes []string `json:"recoveryCodes"`
	}
	c.MustJSON("POST", "/api/auth/totp/enable", map[string]string{"code": code}, &out)
	return setup.Secret, out.RecoveryCodes
}

func TestMFAStepEndpoint(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	secret, recovery := enableTOTP(t, admin)

	c := env.Client()
	st, ch := loginChallenge(t, c, "admin", adminPass)
	if st != 401 || ch.Code != "totp_required" || ch.MFAToken == "" || len(ch.Methods) != 1 || ch.Methods[0] != "totp" {
		t.Fatalf("challenge: %d %+v", st, ch)
	}
	if code, ec := c.ErrorCode("POST", "/api/auth/mfa/totp", map[string]string{"mfaToken": ch.MFAToken, "code": "000000"}); code != 401 || ec != "totp_invalid" {
		t.Fatalf("bad code: %d %s", code, ec)
	}
	good, _ := totp.GenerateCodeCustom(secret, time.Now().Add(30*time.Second), totpOpts)
	var out struct {
		User model.User `json:"user"`
	}
	c.MustJSON("POST", "/api/auth/mfa/totp", map[string]string{"mfaToken": ch.MFAToken, "code": good}, &out)
	if out.User.Username != "admin" || !getState(t, c).Authenticated {
		t.Fatal("mfa step did not sign in")
	}
	// Single use.
	if code, ec := env.Client().ErrorCode("POST", "/api/auth/mfa/totp", map[string]string{"mfaToken": ch.MFAToken, "code": good}); code != 401 || ec != "mfa_expired" {
		t.Fatalf("reused token: %d %s", code, ec)
	}
	// Recovery codes work through the step as well; too many failures drop the challenge.
	c2 := env.Client()
	_, ch = loginChallenge(t, c2, "admin", adminPass)
	c2.MustJSON("POST", "/api/auth/mfa/totp", map[string]string{"mfaToken": ch.MFAToken, "code": recovery[0]}, nil)
	c3 := env.Client()
	_, ch = loginChallenge(t, c3, "admin", adminPass)
	for range 5 {
		c3.ErrorCode("POST", "/api/auth/mfa/totp", map[string]string{"mfaToken": ch.MFAToken, "code": "zzzzz-zzzzz"})
	}
	if code, ec := c3.ErrorCode("POST", "/api/auth/mfa/totp", map[string]string{"mfaToken": ch.MFAToken, "code": recovery[1]}); !(code == 401 && ec == "mfa_expired") && code != 429 {
		t.Fatalf("after 5 failures: %d %s", code, ec)
	}

	// /api/auth/me reports the second factor state.
	var me struct {
		HasPassword       bool `json:"hasPassword"`
		RecoveryCodesLeft int  `json:"recoveryCodesLeft"`
		ReauthFresh       bool `json:"reauthFresh"`
	}
	c.MustJSON("GET", "/api/auth/me", nil, &me)
	if !me.HasPassword || me.RecoveryCodesLeft != 9 || !me.ReauthFresh {
		t.Fatalf("me: %+v", me)
	}
}

func TestLoginPolicy(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	var pol map[string]any
	admin.MustJSON("GET", "/api/admin/auth/policy", nil, &pol)
	if pol["passwordMinLength"] != float64(8) || pol["rememberDays"] != float64(30) || pol["requireMfa"] != "off" {
		t.Fatalf("defaults: %v", pol)
	}
	// Validation and self-lockout guard.
	if code, _ := admin.ErrorCode("PUT", "/api/admin/auth/policy", map[string]any{"passwordMinLength": 4}); code != 400 {
		t.Fatalf("min length 4 accepted: %d", code)
	}
	if code, _ := admin.ErrorCode("PUT", "/api/admin/auth/policy", map[string]any{"allowedNetworks": []string{"nonsense"}}); code != 400 {
		t.Fatalf("bad network accepted: %d", code)
	}
	// Password rules apply to new passwords.
	admin.MustJSON("PUT", "/api/admin/auth/policy", map[string]any{"passwordMinLength": 12, "passwordRequireClasses": 3, "passwordDisallowUsername": true}, &pol)
	if pol["passwordMinLength"] != float64(12) || pol["rememberDays"] != float64(30) {
		t.Fatalf("partial update: %v", pol)
	}
	if code, _ := admin.ErrorCode("POST", "/api/admin/users", map[string]string{"username": "bob", "password": "short one1A"}); code != 400 {
		t.Fatalf("short password accepted: %d", code)
	}
	if code, _ := admin.ErrorCode("POST", "/api/admin/users", map[string]string{"username": "bob", "password": "alllowercaseletters"}); code != 400 {
		t.Fatalf("single-class password accepted: %d", code)
	}
	if code, _ := admin.ErrorCode("POST", "/api/admin/users", map[string]string{"username": "bobby", "password": "Bobby-Secret-99"}); code != 400 {
		t.Fatalf("password containing the user name accepted: %d", code)
	}
	admin.MustJSON("POST", "/api/admin/users", map[string]string{"username": "bob", "password": "Correct-Horse-9"}, nil)
	var state struct {
		PasswordPolicy struct {
			MinLength int `json:"minLength"`
		} `json:"passwordPolicy"`
		LoginMethods struct {
			Password bool `json:"password"`
			Remember bool `json:"remember"`
		} `json:"loginMethods"`
	}
	env.Client().MustJSON("GET", "/api/auth/state", nil, &state)
	if state.PasswordPolicy.MinLength != 12 || !state.LoginMethods.Password || !state.LoginMethods.Remember {
		t.Fatalf("state: %+v", state)
	}

	// Remembering disabled: login cookies become browser-session cookies.
	admin.MustJSON("PUT", "/api/admin/auth/policy", map[string]any{"rememberDays": 0}, nil)
	resp, _ := env.Client().Do("POST", "/api/auth/login", map[string]any{"username": "bob", "password": "Correct-Horse-9", "remember": true})
	if ck := findCookie(resp, "nexterm_session"); ck == nil || ck.MaxAge != 0 {
		t.Fatalf("remember disabled, cookie: %+v", ck)
	}

	// SSO/passkey-only for non-admins.
	admin.MustJSON("PUT", "/api/admin/auth/policy", map[string]any{"passwordLogin": false}, nil)
	if code, ec := env.Client().ErrorCode("POST", "/api/auth/login", map[string]string{"username": "bob", "password": "Correct-Horse-9"}); code != 403 || ec != "password_login_disabled" {
		t.Fatalf("password login disabled: %d %s", code, ec)
	}
	env.Login("admin", adminPass) // admins keep the break-glass path
	admin.MustJSON("PUT", "/api/admin/auth/policy", map[string]any{"passwordLogin": true}, nil)

	// Allowed networks: loopback always passes, so the test client still signs in; a policy excluding it is refused
	// for the admin editing it only when their own address would be locked out (loopback never is).
	admin.MustJSON("PUT", "/api/admin/auth/policy", map[string]any{"allowedNetworks": []string{"10.0.0.0/8", "192.168.1.7"}}, &pol)
	if nets, _ := pol["allowedNetworks"].([]any); len(nets) != 2 || nets[1] != "192.168.1.7/32" {
		t.Fatalf("networks: %v", pol["allowedNetworks"])
	}
	env.Login("bob", "Correct-Horse-9")
}

func TestAccountLockout(t *testing.T) {
	env := servertest.New(t, trustLoopbackProxy)
	admin := env.Setup("admin", adminPass)
	admin.MustJSON("PUT", "/api/admin/auth/policy", map[string]any{"accountLockThreshold": 3, "accountLockMinutes": 5, "lockoutThreshold": 50}, nil)
	admin.MustJSON("POST", "/api/admin/users", map[string]string{"username": "bob", "password": "bob password"}, nil)
	c := remoteClient(env, "203.0.113.7")
	for range 3 {
		if code, _ := c.ErrorCode("POST", "/api/auth/login", map[string]string{"username": "bob", "password": "wrong password"}); code != 401 {
			t.Fatalf("bad password: %d", code)
		}
	}
	// Locked: even the right password is refused (wrong ones still look like wrong passwords).
	if code, ec := c.ErrorCode("POST", "/api/auth/login", map[string]string{"username": "bob", "password": "bob password"}); code != 403 || ec != "account_locked" {
		t.Fatalf("locked login: %d %s", code, ec)
	}
	if code, ec := c.ErrorCode("POST", "/api/auth/login", map[string]string{"username": "bob", "password": "still wrong"}); code != 401 || ec != "invalid_credentials" {
		t.Fatalf("locked, wrong password: %d %s", code, ec)
	}
	var users []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Locked   bool   `json:"locked"`
	}
	admin.MustJSON("GET", "/api/admin/users", nil, &users)
	var bobID string
	for _, u := range users {
		if u.Username == "bob" {
			bobID = u.ID
			if !u.Locked {
				t.Fatalf("admin list does not show the lock: %+v", u)
			}
		}
	}
	// The lock never applies to clients on the NexTerm host itself (break-glass: no permanent remote lock-out).
	env.Login("bob", "bob password")
	admin.MustJSON("POST", "/api/admin/users/"+bobID+"/unlock", nil, nil)
	remoteClient(env, "203.0.113.7").MustJSON("POST", "/api/auth/login", map[string]string{"username": "bob", "password": "bob password"}, nil)
	bob := env.Login("bob", "bob password")

	// Admin can end all of bob's sessions.
	admin.MustJSON("POST", "/api/admin/users/"+bobID+"/revoke-sessions", nil, nil)
	if getState(t, bob).Authenticated {
		t.Fatal("session survived revoke-sessions")
	}
}

func TestRequireMFAEnrollment(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	admin.MustJSON("PUT", "/api/admin/auth/policy", map[string]any{"requireMfa": "all"}, nil)
	admin.MustJSON("POST", "/api/admin/users", map[string]string{"username": "bob", "password": "bob password"}, nil)

	c := env.Client()
	st, ch := loginChallenge(t, c, "bob", "bob password")
	if st != 401 || ch.Code != "mfa_enrollment_required" || ch.MFAToken == "" {
		t.Fatalf("enrollment challenge: %d %+v", st, ch)
	}
	if code, _ := c.ErrorCode("POST", "/api/auth/mfa/totp", map[string]string{"mfaToken": ch.MFAToken, "code": "123456"}); code != 400 {
		t.Fatalf("totp step during enrollment: %d", code)
	}
	var setup struct {
		Secret string `json:"secret"`
	}
	c.MustJSON("POST", "/api/auth/mfa/enroll/totp/setup", map[string]string{"mfaToken": ch.MFAToken}, &setup)
	code, _ := totp.GenerateCodeCustom(setup.Secret, time.Now(), totpOpts)
	var out struct {
		User          model.User `json:"user"`
		RecoveryCodes []string   `json:"recoveryCodes"`
	}
	c.MustJSON("POST", "/api/auth/mfa/enroll/totp/enable", map[string]string{"mfaToken": ch.MFAToken, "code": code}, &out)
	if !out.User.TOTPEnabled || len(out.RecoveryCodes) != 10 || !getState(t, c).Authenticated {
		t.Fatalf("enrolled: %+v", out)
	}
	// Next login asks for the code.
	if st, ch := loginChallenge(t, env.Client(), "bob", "bob password"); st != 401 || ch.Code != "totp_required" {
		t.Fatalf("after enrollment: %d %+v", st, ch)
	}
	// Turning the only factor off is refused while the policy requires one.
	if code, ec := c.ErrorCode("POST", "/api/auth/totp/disable", map[string]string{"password": "bob password"}); code != 409 || ec != "mfa_required" {
		t.Fatalf("disable under policy: %d %s", code, ec)
	}
}

func TestAccountEndpointsAuditAndSystem(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	other := env.Login("admin", adminPass)
	third := env.Login("admin", adminPass)

	var u model.User
	admin.MustJSON("PATCH", "/api/auth/me", map[string]string{"displayName": "  The Admin  "}, &u)
	if u.DisplayName != "The Admin" {
		t.Fatalf("display name: %q", u.DisplayName)
	}
	var rv struct {
		Revoked int `json:"revoked"`
	}
	admin.MustJSON("POST", "/api/auth/sessions/revoke-others", nil, &rv)
	if rv.Revoked != 2 || getState(t, other).Authenticated || getState(t, third).Authenticated || !getState(t, admin).Authenticated {
		t.Fatalf("revoke others: %+v", rv)
	}

	for range 3 {
		env.Client().ErrorCode("POST", "/api/auth/login", map[string]string{"username": "ghost", "password": "no such user"})
	}
	var res struct {
		Entries []model.AuditEntry `json:"entries"`
	}
	admin.MustJSON("GET", "/api/admin/audit/search?q=GHOST&limit=2", nil, &res)
	if len(res.Entries) != 2 || !strings.Contains(string(res.Entries[0].Details), "ghost") {
		t.Fatalf("search: %+v", res.Entries)
	}
	admin.MustJSON("GET", "/api/admin/audit/search?action=auth.profile.update", nil, &res)
	if len(res.Entries) != 1 {
		t.Fatalf("action filter: %d", len(res.Entries))
	}
	resp, body := admin.Do("GET", "/api/admin/audit/export?q=ghost", nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("export: %d %v", resp.StatusCode, resp.Header)
	}
	rows, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	if err != nil || len(rows) != 4 || rows[0][4] != "action" {
		t.Fatalf("csv: %v %q", err, body)
	}

	var sys struct {
		Mode    string `json:"mode"`
		Version string `json:"version"`
		DataDir string `json:"dataDir"`
		Users   int    `json:"users"`
		DBSize  int64  `json:"dbSizeBytes"`
	}
	admin.MustJSON("GET", "/api/admin/system", nil, &sys)
	if sys.Mode != "desktop" || sys.Version != "test" || sys.DataDir == "" || sys.Users != 1 || sys.DBSize <= 0 {
		t.Fatalf("system: %+v", sys)
	}
	bob := env.CreateUser(admin, "bob", "bob password", "user")
	if code, _ := bob.ErrorCode("GET", "/api/admin/system", nil); code != 403 {
		t.Fatalf("non-admin system info: %d", code)
	}
	if code, _ := bob.ErrorCode("GET", "/api/admin/audit/export", nil); code != 403 {
		t.Fatalf("non-admin export: %d", code)
	}
}
