package oidc_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/server/servertest"
)

const adminPass = "correct horse battery"

// fakeIdP is a minimal OpenID provider: discovery + JWKS (oidctest), an authorization endpoint that immediately
// redirects back with a code, and a token endpoint that checks the client secret and the PKCE verifier and issues
// a signed ID token with the configured claims.
type fakeIdP struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	claims map[string]any // next login's claims (sub, preferred_username, groups…)
	codes  map[string]codeGrant
	// badNonce makes the next ID token carry a wrong nonce.
	badNonce bool
	sawAuth  url.Values
}

type codeGrant struct {
	challenge, nonce, redirect string
}

func newIdP(t *testing.T) *fakeIdP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{t: t, key: key, codes: map[string]codeGrant{}}
	disc := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: key.Public(), KeyID: "k1", Algorithm: gooidc.RS256}}}
	mux := http.NewServeMux()
	mux.Handle("/.well-known/openid-configuration", disc)
	mux.Handle("/keys", disc)
	mux.HandleFunc("/auth", f.authorize)
	mux.HandleFunc("/token", f.token)
	f.srv = httptest.NewServer(mux)
	disc.SetIssuer(f.srv.URL)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	f.sawAuth = q
	code := "code-" + q.Get("state")
	f.codes[code] = codeGrant{challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), redirect: q.Get("redirect_uri")}
	f.mu.Unlock()
	if q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" || q.Get("client_id") != "astraterm" {
		http.Error(w, "bad authorization request", 400)
		return
	}
	u, _ := url.Parse(q.Get("redirect_uri"))
	v := u.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	u.RawQuery = v.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (f *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != "astraterm" || secret != "s3cret" {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	g, ok := f.codes[r.PostForm.Get("code")]
	delete(f.codes, r.PostForm.Get("code"))
	claims := map[string]any{}
	maps.Copy(claims, f.claims)
	bad := f.badNonce
	f.badNonce = false
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge || r.PostForm.Get("redirect_uri") != g.redirect {
		http.Error(w, `{"error":"invalid_grant"}`, 400)
		return
	}
	claims["iss"], claims["aud"], claims["exp"], claims["iat"], claims["nonce"] = f.srv.URL, "astraterm", time.Now().Add(time.Hour).Unix(), time.Now().Unix(), g.nonce
	if bad {
		claims["nonce"] = "something-else"
	}
	raw, _ := json.Marshal(claims)
	tok := oidctest.SignIDToken(f.key, "k1", gooidc.RS256, string(raw))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": tok})
}

func (f *fakeIdP) next(claims map[string]any) {
	f.mu.Lock()
	f.claims = claims
	f.mu.Unlock()
}

// ssoLogin runs the browser side of the flow and returns the final URL.
func ssoLogin(t *testing.T, c *servertest.Client, path string) *url.URL {
	t.Helper()
	resp, _ := c.Do("GET", path, nil)
	return resp.Request.URL
}

func setupProvider(t *testing.T, admin *servertest.Client, idp *fakeIdP, extra map[string]any) {
	body := map[string]any{"name": "Corp SSO", "id": "corp", "enabled": true, "issuer": idp.srv.URL, "clientId": "astraterm",
		"clientSecret": "s3cret", "autoProvision": true, "adminGroups": []string{"ops"}, "syncRole": true}
	maps.Copy(body, extra)
	var p map[string]any
	if st := admin.JSON("POST", "/api/admin/oidc/providers", body, &p); st != 201 {
		t.Fatalf("create provider: %d", st)
	}
	if p["hasClientSecret"] != true || p["clientSecretEnc"] != nil || p["clientSecret"] != nil {
		t.Fatalf("provider view leaks or lacks secret state: %v", p)
	}
}

func TestOIDCLoginProvisioningAndRoles(t *testing.T) {
	idp := newIdP(t)
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	setupProvider(t, admin, idp, nil)

	// Discovery test.
	var tr struct {
		OK          bool     `json:"ok"`
		Issuer      string   `json:"issuer"`
		RedirectURI string   `json:"redirectUri"`
		Error       string   `json:"error"`
		PKCE        []string `json:"pkceMethods"`
	}
	admin.MustJSON("POST", "/api/admin/oidc/test", map[string]string{"id": "corp"}, &tr)
	if !tr.OK || tr.Issuer != idp.srv.URL || !strings.HasSuffix(tr.RedirectURI, "/api/auth/oidc/callback") {
		t.Fatalf("test: %+v", tr)
	}
	admin.MustJSON("POST", "/api/admin/oidc/test", map[string]string{"issuer": "http://127.0.0.1:1"}, &tr)
	if tr.OK || tr.Error == "" {
		t.Fatalf("unreachable issuer test: %+v", tr)
	}

	// Login methods on the sign-in screen.
	var st struct {
		Authenticated bool        `json:"authenticated"`
		User          *model.User `json:"user"`
		LoginMethods  struct {
			SSO []struct{ ID, Name string } `json:"sso"`
		} `json:"loginMethods"`
	}
	env.Client().MustJSON("GET", "/api/auth/state", nil, &st)
	if len(st.LoginMethods.SSO) != 1 || st.LoginMethods.SSO[0].ID != "corp" {
		t.Fatalf("login methods: %+v", st.LoginMethods)
	}

	// First login provisions an admin (group "ops").
	idp.next(map[string]any{"sub": "u-1", "preferred_username": "Alice Smith!", "name": "Alice Smith", "email": "alice@corp", "groups": []string{"ops"}})
	c := env.Client()
	final := ssoLogin(t, c, "/api/auth/oidc/login?provider=corp&remember=1")
	if final.Path != "/" || final.Query().Get("sso_error") != "" {
		t.Fatalf("final URL: %s", final)
	}
	if idp.sawAuth.Get("nonce") == "" || idp.sawAuth.Get("state") == "" || idp.sawAuth.Get("code_challenge") == "" {
		t.Fatalf("authorization request: %v", idp.sawAuth)
	}
	c.MustJSON("GET", "/api/auth/state", nil, &st)
	if !st.Authenticated || st.User.Username != "AliceSmith" || st.User.Role != model.RoleAdmin || st.User.DisplayName != "Alice Smith" {
		t.Fatalf("provisioned user: %+v", st.User)
	}
	// The SSO account has no password: /api/auth/me says so, and the admin list shows the provider.
	var me struct {
		HasPassword bool `json:"hasPassword"`
	}
	c.MustJSON("GET", "/api/auth/me", nil, &me)
	if me.HasPassword {
		t.Fatal("SSO-provisioned account reports a password")
	}
	var users []struct {
		Username    string   `json:"username"`
		SSO         []string `json:"sso"`
		HasPassword bool     `json:"hasPassword"`
	}
	admin.MustJSON("GET", "/api/admin/users", nil, &users)
	found := false
	for _, u := range users {
		if u.Username == "AliceSmith" {
			found = len(u.SSO) == 1 && u.SSO[0] == "Corp SSO" && !u.HasPassword
		}
	}
	if !found {
		t.Fatalf("admin list: %+v", users)
	}
	// It may set a first password right after signing in (fresh login), without a current one.
	c.MustJSON("POST", "/api/auth/password", map[string]string{"newPassword": "alice password 1"}, nil)
	env.Login("AliceSmith", "alice password 1")

	// Second login: same subject → same account; role synced from groups (demoted).
	idp.next(map[string]any{"sub": "u-1", "preferred_username": "alice", "groups": []string{"dev"}})
	c2 := env.Client()
	ssoLogin(t, c2, "/api/auth/oidc/login?provider=corp")
	c2.MustJSON("GET", "/api/auth/state", nil, &st)
	if !st.Authenticated || st.User.Username != "AliceSmith" || st.User.Role != model.RoleUser {
		t.Fatalf("second login: %+v", st.User)
	}

	// A wrong nonce in the ID token is rejected.
	idp.mu.Lock()
	idp.badNonce = true
	idp.mu.Unlock()
	c3 := env.Client()
	final = ssoLogin(t, c3, "/api/auth/oidc/login?provider=corp")
	if final.Query().Get("sso_error") != "verification_failed" {
		t.Fatalf("bad nonce: %s", final)
	}

	// The callback is bound to the browser that started the flow (login CSRF).
	idp.next(map[string]any{"sub": "u-1"})
	attacker := env.Client()
	attacker.HTTP.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if strings.Contains(req.URL.Path, "/api/auth/oidc/callback") {
			return http.ErrUseLastResponse
		}
		return nil
	}
	resp, _ := attacker.Do("GET", "/api/auth/oidc/login?provider=corp", nil)
	callback := resp.Header.Get("Location")
	victim := env.Client()
	vresp, _ := victim.HTTP.Get(callback)
	vresp.Body.Close()
	if vresp.Request.URL.Query().Get("sso_error") != "invalid_state" {
		t.Fatalf("foreign callback: %s", vresp.Request.URL)
	}
	victim.MustJSON("GET", "/api/auth/state", nil, &st)
	if st.Authenticated {
		t.Fatal("login CSRF: victim signed in with the attacker's code")
	}

	// Disabled users cannot sign in with SSO.
	var list []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	admin.MustJSON("GET", "/api/admin/users", nil, &list)
	for _, u := range list {
		if u.Username == "AliceSmith" {
			admin.MustJSON("PATCH", "/api/admin/users/"+u.ID, map[string]any{"disabled": true}, nil)
		}
	}
	idp.next(map[string]any{"sub": "u-1"})
	final = ssoLogin(t, env.Client(), "/api/auth/oidc/login?provider=corp")
	if final.Query().Get("sso_error") != "account_disabled" {
		t.Fatalf("disabled SSO user: %s", final)
	}
}

func TestOIDCNoProvisioningGroupsAndLinking(t *testing.T) {
	idp := newIdP(t)
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	setupProvider(t, admin, idp, map[string]any{"autoProvision": false, "allowedGroups": []string{"staff"}})

	idp.next(map[string]any{"sub": "x", "preferred_username": "bob", "groups": []string{"staff"}})
	if f := ssoLogin(t, env.Client(), "/api/auth/oidc/login?provider=corp"); f.Query().Get("sso_error") != "not_provisioned" {
		t.Fatalf("unprovisioned: %s", f)
	}
	idp.next(map[string]any{"sub": "x", "preferred_username": "bob", "groups": []string{"guests"}})
	if f := ssoLogin(t, env.Client(), "/api/auth/oidc/login?provider=corp"); f.Query().Get("sso_error") != "not_allowed" {
		t.Fatalf("group not allowed: %s", f)
	}

	// An existing user links the identity from the Security tab, then signs in with it.
	bob := env.CreateUser(admin, "bob", "bob password", "user")
	idp.next(map[string]any{"sub": "x", "preferred_username": "someone-else", "groups": []string{"staff"}})
	f := ssoLogin(t, bob, "/api/auth/oidc/link?provider=corp")
	if f.Query().Get("sso_linked") != "Corp SSO" {
		t.Fatalf("link: %s", f)
	}
	var ids []struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerId"`
		Subject    string `json:"subject"`
	}
	bob.MustJSON("GET", "/api/auth/oidc/identities", nil, &ids)
	if len(ids) != 1 || ids[0].Subject != "x" {
		t.Fatalf("identities: %+v", ids)
	}
	c := env.Client()
	idp.next(map[string]any{"sub": "x", "groups": []string{"staff"}})
	ssoLogin(t, c, "/api/auth/oidc/login?provider=corp")
	var st struct {
		Authenticated bool        `json:"authenticated"`
		User          *model.User `json:"user"`
	}
	c.MustJSON("GET", "/api/auth/state", nil, &st)
	if !st.Authenticated || st.User.Username != "bob" {
		t.Fatalf("linked login: %+v", st)
	}
	// Another account cannot claim the same identity.
	carol := env.CreateUser(admin, "carol", "carol password", "user")
	idp.next(map[string]any{"sub": "x", "groups": []string{"staff"}})
	if f := ssoLogin(t, carol, "/api/auth/oidc/link?provider=corp"); f.Query().Get("sso_error") != "already_linked" {
		t.Fatalf("second link: %s", f)
	}
	// Unlink (bob still has a password).
	bob.MustJSON("DELETE", "/api/auth/oidc/identities/"+ids[0].ID, nil, nil)

	// Deleting the provider removes it from the sign-in screen.
	admin.MustJSON("DELETE", "/api/admin/oidc/providers/corp", nil, nil)
	if f := ssoLogin(t, env.Client(), "/api/auth/oidc/login?provider=corp"); f.Query().Get("sso_error") != "unknown_provider" {
		t.Fatalf("deleted provider: %s", f)
	}
}

func TestOIDCProviderValidation(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	for _, body := range []map[string]any{
		{"name": "x", "issuer": "ftp://nope", "clientId": "a"},
		{"name": "x", "issuer": "https://idp.example", "clientId": ""},
		{"name": "", "issuer": "https://idp.example", "clientId": "a"},
		{"name": "x", "issuer": "https://idp.example", "clientId": "a", "usernameClaim": "bad claim"},
	} {
		if code, _ := admin.ErrorCode("POST", "/api/admin/oidc/providers", body); code != 400 {
			t.Fatalf("invalid provider %v accepted: %d", body, code)
		}
	}
	var p map[string]any
	admin.MustJSON("POST", "/api/admin/oidc/providers", map[string]any{"name": "Okta Prod", "issuer": "https://idp.example/", "clientId": "a"}, &p)
	if p["id"] != "okta-prod" || p["issuer"] != "https://idp.example" || p["hasClientSecret"] != false {
		t.Fatalf("created: %v", p)
	}
	admin.MustJSON("PATCH", "/api/admin/oidc/providers/okta-prod", map[string]any{"clientSecret": "x", "enabled": true}, &p)
	if p["hasClientSecret"] != true || p["enabled"] != true || p["name"] != "Okta Prod" {
		t.Fatalf("patched: %v", p)
	}
	bob := env.CreateUser(admin, "bob", "bob password", "user")
	if code, _ := bob.ErrorCode("GET", "/api/admin/oidc/providers", nil); code != 403 {
		t.Fatalf("non-admin provider list: %d", code)
	}
}
