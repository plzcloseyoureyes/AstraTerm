package oidc_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/server/servertest"
)

func whoAmI(t *testing.T, c *servertest.Client) *model.User {
	t.Helper()
	var st struct {
		Authenticated bool        `json:"authenticated"`
		User          *model.User `json:"user"`
	}
	c.MustJSON("GET", "/api/auth/state", nil, &st)
	if !st.Authenticated {
		return nil
	}
	return st.User
}

func staleSessions(t *testing.T, env *servertest.Env) {
	t.Helper()
	if _, err := env.Server.Deps.Store.DB.Exec(`UPDATE auth_sessions SET created_at = created_at - 3600000`); err != nil {
		t.Fatal(err)
	}
}

// LinkByUsername must never link through an e-mail address the provider did not verify, nor through a name derived
// from the e-mail's local part, nor a second subject to an account that already has an identity at the provider.
func TestOIDCLinkByUsernameIsStrict(t *testing.T) {
	idp := newIdP(t)
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	env.CreateUser(admin, "bob", "bob password", "user")
	setupProvider(t, admin, idp, map[string]any{"linkByUsername": true, "adminGroups": []string{}})

	// No user-name claim; the e-mail's local part "admin" matches the administrator: must not link.
	c := env.Client()
	idp.next(map[string]any{"sub": "e1", "email": "admin@evil.example", "email_verified": false})
	ssoLogin(t, c, "/api/auth/oidc/login?provider=corp")
	if u := whoAmI(t, c); u == nil || u.Username == "admin" || u.Role == model.RoleAdmin {
		t.Fatalf("e-mail local part linked to an existing account: %+v", u)
	}

	// A user-name claim that is an unverified e-mail address is not trusted either.
	env.CreateUser(admin, "carol@corp.example", "carol password", "user")
	c = env.Client()
	idp.next(map[string]any{"sub": "e2", "preferred_username": "carol@corp.example", "email": "carol@corp.example", "email_verified": false})
	ssoLogin(t, c, "/api/auth/oidc/login?provider=corp")
	if u := whoAmI(t, c); u == nil || u.Username == "carol@corp.example" {
		t.Fatalf("unverified e-mail user name linked: %+v", u)
	}
	// …but a verified one is.
	c = env.Client()
	idp.next(map[string]any{"sub": "e3", "preferred_username": "carol@corp.example", "email": "carol@corp.example", "email_verified": true})
	ssoLogin(t, c, "/api/auth/oidc/login?provider=corp")
	if u := whoAmI(t, c); u == nil || u.Username != "carol@corp.example" {
		t.Fatalf("verified e-mail user name not linked: %+v", u)
	}

	// A plain user-name claim links once…
	c = env.Client()
	idp.next(map[string]any{"sub": "b1", "preferred_username": "bob"})
	ssoLogin(t, c, "/api/auth/oidc/login?provider=corp")
	if u := whoAmI(t, c); u == nil || u.Username != "bob" {
		t.Fatalf("user name link: %+v", u)
	}
	// …but a different subject claiming the same name gets its own account.
	c = env.Client()
	idp.next(map[string]any{"sub": "b2", "preferred_username": "bob"})
	ssoLogin(t, c, "/api/auth/oidc/login?provider=corp")
	if u := whoAmI(t, c); u == nil || u.Username == "bob" {
		t.Fatalf("second subject took over bob: %+v", u)
	}
}

// Linking an identity adds a way to sign in: it needs a recent sign-in.
func TestOIDCLinkNeedsRecentAuth(t *testing.T) {
	idp := newIdP(t)
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	setupProvider(t, admin, idp, map[string]any{"autoProvision": false})
	bob := env.CreateUser(admin, "bob", "bob password", "user")
	staleSessions(t, env)

	idp.next(map[string]any{"sub": "attacker"})
	if f := ssoLogin(t, bob, "/api/auth/oidc/link?provider=corp"); f.Query().Get("sso_error") != "reauth_required" {
		t.Fatalf("stale link: %s", f)
	}
	bob.MustJSON("POST", "/api/auth/verify-password", map[string]string{"password": "bob password"}, nil)
	idp.next(map[string]any{"sub": "bob-sub"})
	if f := ssoLogin(t, bob, "/api/auth/oidc/link?provider=corp"); f.Query().Get("sso_linked") == "" {
		t.Fatalf("fresh link: %s", f)
	}
}

// Changing a provider's issuer drops the identities linked through the old one (subjects are per issuer).
func TestOIDCIssuerChangeUnlinks(t *testing.T) {
	idp, other := newIdP(t), newIdP(t)
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	setupProvider(t, admin, idp, map[string]any{"autoProvision": false})
	bob := env.CreateUser(admin, "bob", "bob password", "user")
	idp.next(map[string]any{"sub": "42"})
	if f := ssoLogin(t, bob, "/api/auth/oidc/link?provider=corp"); f.Query().Get("sso_linked") == "" {
		t.Fatalf("link: %s", f)
	}
	admin.MustJSON("PATCH", "/api/admin/oidc/providers/corp", map[string]any{"issuer": other.srv.URL}, nil)
	var ids []map[string]any
	bob.MustJSON("GET", "/api/auth/oidc/identities", nil, &ids)
	if len(ids) != 0 {
		t.Fatalf("identities survived the issuer change: %v", ids)
	}
	other.next(map[string]any{"sub": "42"})
	c := env.Client()
	if f := ssoLogin(t, c, "/api/auth/oidc/login?provider=corp"); f.Query().Get("sso_error") != "not_provisioned" {
		t.Fatalf("same subject at another issuer: %s", f)
	}
}

// returnTo only accepts same-origin paths, whatever browsers would strip or normalise.
func TestOIDCReturnToIsSameOrigin(t *testing.T) {
	idp := newIdP(t)
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	setupProvider(t, admin, idp, nil)
	base, _ := url.Parse(env.URL("/"))
	for i, rt := range []string{"/\t/evil.example", "//evil.example", "/\\evil.example", "https://evil.example", "/ /evil.example", "/%0a/evil.example"} {
		c := env.Client()
		idp.next(map[string]any{"sub": "u" + strings.Repeat("x", i+1), "preferred_username": "u" + strings.Repeat("x", i+1)})
		f := ssoLogin(t, c, "/api/auth/oidc/login?provider=corp&returnTo="+url.QueryEscape(rt))
		if f.Host != base.Host || (rt != "/%0a/evil.example" && f.Path != "/") { // an encoded %0a stays in the path
			t.Fatalf("returnTo %q led to %s", rt, f)
		}
	}
	c := env.Client()
	idp.next(map[string]any{"sub": "ok", "preferred_username": "ok"})
	if f := ssoLogin(t, c, "/api/auth/oidc/login?provider=corp&returnTo="+url.QueryEscape("/?tab=files")); f.Host != base.Host || f.RawQuery != "tab=files" {
		t.Fatalf("legit returnTo: %s", f)
	}
}
