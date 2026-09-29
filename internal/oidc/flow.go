package oidc

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/labstack/echo/v5"
	"golang.org/x/oauth2"

	"github.com/termstead/termstead/internal/auth"
	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/store"
)

// flow is a pending authorization-code flow, bound to the browser that started it by a cookie.
type flow struct {
	providerID  string
	verifier    string // PKCE
	nonce       string
	redirectURI string
	binding     [32]byte // sha256 of the browser-binding cookie value
	remember    bool
	linkUserID  string // "link" flows: the signed-in user to attach the identity to
	returnTo    string
	expires     time.Time
}

// ssoError sends the browser back to the app with a short error code the login screen explains.
func ssoError(c *echo.Context, code string) error {
	return c.Redirect(http.StatusSeeOther, "/?sso_error="+url.QueryEscape(code))
}

func (s *Service) redirectURI(c *echo.Context) string {
	scheme := "http"
	if s.auth.CookieSecure(c) {
		scheme = "https"
	}
	return scheme + "://" + c.Request().Host + callbackPath
}

// safeReturn keeps only same-origin absolute paths ("/…", not "//…"). Control characters and white space are
// refused outright: browsers strip tabs / newlines from URLs, so "/\t/evil.example" would become "//evil.example".
func safeReturn(p string) string {
	if p == "" || len(p) > 2048 || !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\") ||
		strings.IndexFunc(p, func(r rune) bool { return r <= 0x20 || r == 0x7f || unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "/"
	}
	if strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/ws/") {
		return "/"
	}
	if u, err := url.Parse(p); err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	return p
}

// start begins a flow for provider id and redirects the browser to the identity provider.
func (s *Service) start(c *echo.Context, providerID string, remember bool, linkUserID string) error {
	ctx := c.Request().Context()
	p, err := s.provider(ctx, providerID)
	if err != nil || !p.Enabled {
		return ssoError(c, "unknown_provider")
	}
	prov, err := s.discover(ctx, p, false)
	if err != nil {
		s.log.Warn("identity provider discovery failed", "provider", p.ID, "err", err)
		return ssoError(c, "provider_unreachable")
	}
	secret, err := s.clientSecret(p)
	if err != nil {
		s.log.Error("open client secret", "provider", p.ID, "err", err)
		return ssoError(c, "provider_misconfigured")
	}
	f := &flow{providerID: p.ID, verifier: oauth2.GenerateVerifier(), nonce: randomString(24), redirectURI: s.redirectURI(c),
		remember: remember, linkUserID: linkUserID, returnTo: safeReturn(c.QueryParam("returnTo")), expires: time.Now().Add(flowTTL)}
	bindValue := randomString(24)
	f.binding = sha256.Sum256([]byte(bindValue))
	state := randomString(24)
	s.flowMu.Lock()
	if len(s.flows) >= maxFlows {
		s.flowMu.Unlock()
		return ssoError(c, "busy")
	}
	s.flows[state] = f
	s.flowMu.Unlock()

	// Lax (not Strict): the callback is a top-level navigation coming from the identity provider's site.
	c.SetCookie(&http.Cookie{Name: fmt.Sprintf(flowCookieFmt, state[:12]), Value: bindValue, Path: "/api/auth/oidc",
		MaxAge: int(flowTTL / time.Second), HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.auth.CookieSecure(c)})

	conf := oauth2.Config{ClientID: p.ClientID, ClientSecret: secret, Endpoint: prov.Endpoint(), RedirectURL: f.redirectURI, Scopes: p.Scopes}
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(f.verifier), gooidc.Nonce(f.nonce)}
	if p.Prompt != "" {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", p.Prompt))
	}
	return c.Redirect(http.StatusFound, conf.AuthCodeURL(state, opts...))
}

// handleLogin: GET /api/auth/oidc/login?provider=<id>[&remember=1][&returnTo=/…] (first enabled provider when omitted).
func (s *Service) handleLogin(c *echo.Context) error {
	if !s.allowIP(auth.RateKeyIP(httpx.ClientIP(c))) {
		return ssoError(c, "busy")
	}
	id := c.QueryParam("provider")
	if id == "" {
		if m := s.LoginMethods(c.Request().Context()); len(m) > 0 {
			id = m[0].ID
		}
	}
	remember := c.QueryParam("remember") == "1" || c.QueryParam("remember") == "true"
	return s.start(c, id, remember, "")
}

// handleLink: GET /api/auth/oidc/link?provider=<id> — attach an identity to the signed-in account.
func (s *Service) handleLink(c *echo.Context) error {
	u := httpx.UserFrom(c)
	if u == nil {
		return ssoError(c, "not_signed_in")
	}
	// Linking adds a way to sign in: like adding a passkey it needs a recent sign-in or re-verification, so a
	// hijacked session cannot attach the attacker's identity-provider account.
	if err := s.auth.RequireRecentAuth(c, ""); err != nil {
		return ssoError(c, "reauth_required")
	}
	return s.start(c, c.QueryParam("provider"), false, u.ID)
}

var usernameStrip = regexp.MustCompile(`[^A-Za-z0-9._@-]+`)

// sanitizeUsername maps a claim value onto Termstead's user-name rules.
func sanitizeUsername(v string) string {
	v = usernameStrip.ReplaceAllString(strings.TrimSpace(v), "")
	v = strings.TrimLeft(v, "._@-")
	if len(v) > 64 {
		v = v[:64]
	}
	return v
}

func claimString(claims map[string]any, name string) string {
	switch v := claims[name].(type) {
	case string:
		return v
	case float64:
		return fmt.Sprint(v)
	}
	return ""
}

func claimStrings(claims map[string]any, name string) []string {
	switch v := claims[name].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	case string:
		if v == "" {
			return nil
		}
		return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
	}
	return nil
}

func intersects(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}

// handleCallback completes the flow: code exchange with the PKCE verifier, ID token verification (issuer, audience,
// expiry, nonce), account resolution, then a login session (or an account link).
func (s *Service) handleCallback(c *echo.Context) error {
	ctx := c.Request().Context()
	state := c.QueryParam("state")
	if len(state) < 12 || len(state) > 64 {
		return ssoError(c, "invalid_state")
	}
	s.flowMu.Lock()
	f := s.flows[state]
	delete(s.flows, state)
	s.flowMu.Unlock()
	cookieName := fmt.Sprintf(flowCookieFmt, state[:12])
	c.SetCookie(&http.Cookie{Name: cookieName, Value: "", Path: "/api/auth/oidc", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.auth.CookieSecure(c)})
	if f == nil || time.Now().After(f.expires) {
		return ssoError(c, "expired")
	}
	ck, err := c.Cookie(cookieName)
	if err != nil {
		return ssoError(c, "invalid_state")
	}
	got := sha256.Sum256([]byte(ck.Value))
	if subtle.ConstantTimeCompare(got[:], f.binding[:]) != 1 {
		return ssoError(c, "invalid_state")
	}
	if e := c.QueryParam("error"); e != "" {
		s.log.Info("identity provider returned an error", "provider", f.providerID, "error", e, "description", c.QueryParam("error_description"))
		if e == "access_denied" {
			return ssoError(c, "access_denied")
		}
		return ssoError(c, "idp_error")
	}
	code := c.QueryParam("code")
	if code == "" {
		return ssoError(c, "idp_error")
	}
	p, err := s.provider(ctx, f.providerID)
	if err != nil || !p.Enabled {
		return ssoError(c, "unknown_provider")
	}
	claims, subject, err := s.exchange(ctx, p, f, code)
	if err != nil {
		s.log.Warn("SSO sign-in failed", "provider", p.ID, "err", err)
		s.d.Audit.LogUser(c, nil, "auth.login.failed", "", map[string]string{"reason": "sso", "provider": p.ID})
		return ssoError(c, "verification_failed")
	}
	email := claimString(claims, p.EmailClaim)
	emailVerified := email != "" && claimBool(claims, "email_verified")
	if p.RequireVerifiedEmail && !emailVerified {
		return ssoError(c, "email_not_verified")
	}
	groups := claimStrings(claims, p.GroupsClaim)
	if len(p.AllowedGroups) > 0 && !intersects(groups, p.AllowedGroups) {
		s.d.Audit.LogUser(c, nil, "auth.login.failed", "", map[string]any{"reason": "sso group not allowed", "provider": p.ID, "subject": subject})
		return ssoError(c, "not_allowed")
	}
	usernameClaim := claimString(claims, p.UsernameClaim)
	preferred := usernameClaim
	if preferred == "" && email != "" {
		preferred = strings.SplitN(email, "@", 2)[0] // only a suggestion for new accounts, never used for linking
	}

	existing, err := s.userForSubject(ctx, p.ID, subject)
	if err != nil {
		return err
	}
	// Linking an identity to the signed-in account.
	if f.linkUserID != "" {
		if existing != "" && existing != f.linkUserID {
			return ssoError(c, "already_linked")
		}
		if existing == "" {
			if err := s.insertIdentity(ctx, p.ID, subject, f.linkUserID, email, preferred); err != nil {
				return err
			}
		}
		u, _ := s.d.Store.Users.Get(ctx, f.linkUserID)
		s.d.Audit.LogUser(c, u, "auth.sso.link", f.linkUserID, map[string]string{"provider": p.ID, "subject": subject})
		return c.Redirect(http.StatusSeeOther, "/?sso_linked="+url.QueryEscape(p.Name))
	}

	userID := existing
	provisioned := false
	if userID == "" && p.LinkByUsername {
		uid, err := s.linkCandidate(ctx, p, usernameClaim, email, emailVerified)
		if err != nil {
			return err
		}
		if uid != "" {
			userID = uid
			s.d.Audit.LogUser(c, nil, "auth.sso.link", uid, map[string]string{"provider": p.ID, "subject": subject, "by": "username"})
		}
	}
	if userID == "" {
		if !p.AutoProvision {
			s.d.Audit.LogUser(c, nil, "auth.login.failed", "", map[string]any{"reason": "sso account not provisioned", "provider": p.ID,
				"subject": subject, "username": preferred})
			return ssoError(c, "not_provisioned")
		}
		role := model.RoleUser
		if intersects(groups, p.AdminGroups) {
			role = model.RoleAdmin
		}
		u, err := s.provision(ctx, preferred, claimString(claims, p.DisplayNameClaim), role)
		if err != nil {
			s.log.Warn("SSO provisioning failed", "provider", p.ID, "err", err)
			return ssoError(c, "provisioning_failed")
		}
		userID, provisioned = u.ID, true
		s.d.Audit.LogUser(c, u, "auth.sso.provision", u.ID, map[string]string{"provider": p.ID, "username": u.Username, "role": string(role)})
	}
	if existing == "" {
		if err := s.insertIdentity(ctx, p.ID, subject, userID, email, preferred); err != nil {
			return err
		}
	} else {
		_, _ = s.d.Store.DB.ExecContext(ctx, `UPDATE oidc_identities SET email = ?, username = ?, last_login_at = ? WHERE provider_id = ? AND subject = ?`,
			email, preferred, store.Now().UnixMilli(), p.ID, subject)
	}
	if p.SyncRole && !provisioned {
		u, err := s.d.Store.Users.Get(ctx, userID)
		if err != nil {
			return err
		}
		role := model.RoleUser
		if intersects(groups, p.AdminGroups) {
			role = model.RoleAdmin
		}
		if changed, err := s.auth.SyncRole(ctx, u, role); err != nil {
			return err
		} else if changed {
			s.d.Audit.LogUser(c, u, "auth.sso.role", u.ID, map[string]string{"provider": p.ID, "role": string(role)})
		}
	}
	_, err = s.auth.CompleteLogin(c, userID, auth.LoginOptions{Remember: f.remember, Method: "oidc:" + p.ID, External: true,
		Details: map[string]any{"provider": p.Name}})
	if err != nil {
		var he *httpx.HTTPError
		if errors.As(err, &he) && he.Status < 500 {
			return ssoError(c, he.Code)
		}
		return err
	}
	return c.Redirect(http.StatusSeeOther, f.returnTo)
}

// exchange redeems the code and verifies the ID token; it returns the merged claims (ID token + userinfo when the
// token lacks the username / email / groups claims) and the subject.
func (s *Service) exchange(ctx context.Context, p *ProviderConfig, f *flow, code string) (map[string]any, string, error) {
	prov, err := s.discover(ctx, p, false)
	if err != nil {
		return nil, "", err
	}
	secret, err := s.clientSecret(p)
	if err != nil {
		return nil, "", err
	}
	hctx, cancel := context.WithTimeout(context.WithValue(ctx, oauth2.HTTPClient, s.http), httpTimeout)
	defer cancel()
	conf := oauth2.Config{ClientID: p.ClientID, ClientSecret: secret, Endpoint: prov.Endpoint(), RedirectURL: f.redirectURI, Scopes: p.Scopes}
	tok, err := conf.Exchange(hctx, code, oauth2.VerifierOption(f.verifier))
	if err != nil {
		return nil, "", fmt.Errorf("token exchange: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return nil, "", errors.New("the provider returned no id_token")
	}
	idt, err := prov.VerifierContext(gooidc.ClientContext(hctx, s.http), &gooidc.Config{ClientID: p.ClientID}).Verify(hctx, raw)
	if err != nil {
		return nil, "", fmt.Errorf("id token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(f.nonce)) != 1 {
		return nil, "", errors.New("id token nonce mismatch")
	}
	claims := map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		return nil, "", err
	}
	if prov.UserInfoEndpoint() != "" && (claims[p.UsernameClaim] == nil || claims[p.EmailClaim] == nil || claims[p.GroupsClaim] == nil) {
		if ui, err := prov.UserInfo(gooidc.ClientContext(hctx, s.http), oauth2.StaticTokenSource(tok)); err == nil && ui.Subject == idt.Subject {
			extra := map[string]any{}
			if ui.Claims(&extra) == nil {
				for k, v := range extra {
					if _, ok := claims[k]; !ok {
						claims[k] = v
					}
				}
			}
		}
	}
	if idt.Subject == "" {
		return nil, "", errors.New("id token without subject")
	}
	return claims, idt.Subject, nil
}

// claimBool reads a boolean claim; some providers send "true" / "false" strings.
func claimBool(claims map[string]any, name string) bool {
	switch v := claims[name].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	}
	return false
}

// linkCandidate returns the existing local account a first SSO login may be linked to by user name (LinkByUsername),
// or "". Only the provider's user-name claim is used — never a name derived from the e-mail address — and a claim
// that is an e-mail address (or the e-mail claim itself) must be verified by the provider. An account that already
// has an identity at this provider is never linked to a second subject.
func (s *Service) linkCandidate(ctx context.Context, p *ProviderConfig, usernameClaim, email string, emailVerified bool) (string, error) {
	if usernameClaim == "" {
		return "", nil
	}
	if (p.UsernameClaim == p.EmailClaim || strings.Contains(usernameClaim, "@")) && !(emailVerified && strings.EqualFold(usernameClaim, email)) {
		return "", nil
	}
	name := sanitizeUsername(usernameClaim)
	if name == "" || name != strings.TrimSpace(usernameClaim) {
		return "", nil // the claim is not a valid Termstead user name as is: no fuzzy matches
	}
	u, err := s.d.Store.Users.GetByUsername(ctx, name)
	if errors.Is(err, model.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var n int
	if err := s.d.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM oidc_identities WHERE provider_id = ? AND user_id = ?`, p.ID, u.ID).Scan(&n); err != nil {
		return "", err
	}
	if n > 0 {
		return "", nil
	}
	return u.ID, nil
}

func (s *Service) insertIdentity(ctx context.Context, providerID, subject, userID, email, username string) error {
	now := store.Now().UnixMilli()
	_, err := s.d.Store.DB.ExecContext(ctx, `INSERT INTO oidc_identities (id, provider_id, subject, user_id, email, username, created_at, last_login_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, model.NewID(), providerID, subject, userID, email, username, now, now)
	return err
}

// provision creates the account for a first SSO login, choosing a free user name derived from preferred.
func (s *Service) provision(ctx context.Context, preferred, displayName string, role model.Role) (*model.User, error) {
	base := sanitizeUsername(preferred)
	if base == "" || auth.ValidateUsername(base) != nil {
		base = "user"
	}
	for i := 1; i <= 50; i++ {
		name := base
		if i > 1 {
			suffix := fmt.Sprintf("-%d", i)
			if len(name)+len(suffix) > 64 {
				name = name[:64-len(suffix)]
			}
			name += suffix
		}
		u, err := s.auth.CreateExternalUser(ctx, name, displayName, role)
		if errors.Is(err, model.ErrConflict) {
			continue
		}
		return u, err
	}
	return nil, errors.New("no free user name")
}

// ---- own identities -----------------------------------------------------------------------------------------------------

func (s *Service) handleMyIdentities(c *echo.Context) error {
	ctx := c.Request().Context()
	ids, err := s.identitiesOf(ctx, httpx.UserFrom(c).ID)
	if err != nil {
		return err
	}
	list, _ := s.loadProviders(ctx)
	for i := range ids {
		ids[i].ProviderName = ids[i].ProviderID
		for _, p := range list {
			if p.ID == ids[i].ProviderID {
				ids[i].ProviderName = p.Name
			}
		}
	}
	return c.JSON(http.StatusOK, ids)
}

// handleUnlink removes a linked identity; the last way to sign in cannot be removed.
func (s *Service) handleUnlink(c *echo.Context) error {
	ctx := c.Request().Context()
	u := httpx.UserFrom(c)
	ids, err := s.identitiesOf(ctx, u.ID)
	if err != nil {
		return err
	}
	var target *Identity
	for i := range ids {
		if ids[i].ID == c.Param("id") {
			target = &ids[i]
		}
	}
	if target == nil {
		return httpx.ErrNotFound
	}
	hasPw, err := s.auth.HasPassword(ctx, u.ID)
	if err != nil {
		return err
	}
	pk, err := s.auth.PasskeyCount(ctx, u.ID)
	if err != nil {
		return err
	}
	if len(ids) == 1 && !hasPw && pk == 0 {
		return httpx.Conflict("this is your only way to sign in: set a password or add a passkey first")
	}
	if _, err := s.d.Store.DB.ExecContext(ctx, `DELETE FROM oidc_identities WHERE id = ? AND user_id = ?`, target.ID, u.ID); err != nil {
		return err
	}
	s.d.Audit.Log(c, "auth.sso.unlink", u.ID, map[string]string{"provider": target.ProviderID})
	return httpx.OK(c)
}
