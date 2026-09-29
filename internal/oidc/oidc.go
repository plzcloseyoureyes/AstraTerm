// Package oidc implements OpenID Connect single sign-on (RESEARCH MU-7): administrator-configured providers (issuer,
// client id, sealed client secret, scopes, claim mapping, group → role mapping, just-in-time provisioning, allowed
// groups), the authorization-code flow with PKCE (S256), state and nonce, account linking from the Security tab, and
// the login options shown on the sign-in screen. Built on github.com/coreos/go-oidc/v3 and golang.org/x/oauth2.
package oidc

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/time/rate"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/auth"
	"github.com/termstead/termstead/internal/core"
	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/store"
)

func init() {
	store.RegisterMigration("oidc", 1, `
CREATE TABLE oidc_identities (
	id            TEXT PRIMARY KEY,
	provider_id   TEXT NOT NULL,
	subject       TEXT NOT NULL,
	user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	email         TEXT NOT NULL DEFAULT '',
	username      TEXT NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL,
	last_login_at INTEGER,
	UNIQUE (provider_id, subject)
);
CREATE INDEX oidc_identities_user ON oidc_identities(user_id);`)
}

const (
	configScope   = "oidc"
	configKey     = "providers"
	callbackPath  = "/api/auth/oidc/callback"
	flowTTL       = 10 * time.Minute
	maxFlows      = 5000
	maxProviders  = 16
	discoveryTTL  = time.Hour
	httpTimeout   = 15 * time.Second
	flowCookieFmt = "termstead_oidc_%s"
)

// ProviderConfig is one identity provider (stored in the module-private settings scope "oidc", key "providers").
type ProviderConfig struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Enabled  bool     `json:"enabled"`
	Issuer   string   `json:"issuer"`
	ClientID string   `json:"clientId"`
	Scopes   []string `json:"scopes"`
	// Claim names (defaults preferred_username / email / name / groups).
	UsernameClaim    string `json:"usernameClaim"`
	EmailClaim       string `json:"emailClaim"`
	DisplayNameClaim string `json:"displayNameClaim"`
	GroupsClaim      string `json:"groupsClaim"`
	// AdminGroups: members get the admin role (when provisioned, and on every login with SyncRole).
	AdminGroups []string `json:"adminGroups"`
	// AllowedGroups: when set, only members may sign in.
	AllowedGroups []string `json:"allowedGroups"`
	SyncRole      bool     `json:"syncRole"`
	AutoProvision bool     `json:"autoProvision"`
	// LinkByUsername links the first login to an existing local account with the same user name. Only enable it for
	// providers whose user names cannot be chosen by users.
	LinkByUsername       bool      `json:"linkByUsername"`
	RequireVerifiedEmail bool      `json:"requireVerifiedEmail"`
	Prompt               string    `json:"prompt"` // "", login, consent, select_account
	CreatedAt            time.Time `json:"createdAt"`
	UpdatedAt            time.Time `json:"updatedAt"`
	// ClientSecretEnc is the client secret sealed with the vault's system key (usable while the vault is locked).
	ClientSecretEnc string `json:"clientSecretEnc,omitempty"`
}

// ProviderView is a provider as the admin API returns it (secret write-only).
type ProviderView struct {
	ProviderConfig
	ClientSecretEnc string `json:"clientSecretEnc,omitempty"` // shadowed: never sent
	HasClientSecret bool   `json:"hasClientSecret"`
}

func (p ProviderConfig) view() ProviderView {
	v := ProviderView{ProviderConfig: p, HasClientSecret: p.ClientSecretEnc != ""}
	v.ProviderConfig.ClientSecretEnc = ""
	return v
}

func (p *ProviderConfig) withDefaults() {
	if p.UsernameClaim == "" {
		p.UsernameClaim = "preferred_username"
	}
	if p.EmailClaim == "" {
		p.EmailClaim = "email"
	}
	if p.DisplayNameClaim == "" {
		p.DisplayNameClaim = "name"
	}
	if p.GroupsClaim == "" {
		p.GroupsClaim = "groups"
	}
	if len(p.Scopes) == 0 {
		p.Scopes = []string{gooidc.ScopeOpenID, "profile", "email"}
	}
	for _, l := range []*[]string{&p.AdminGroups, &p.AllowedGroups} {
		if *l == nil {
			*l = []string{}
		}
	}
}

var (
	idRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	claimRE = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,128}$`)
	scopeRE = regexp.MustCompile(`^[\x21\x23-\x5B\x5D-\x7E]{1,128}$`)
)

func (p *ProviderConfig) validate() error {
	p.Name = strings.TrimSpace(p.Name)
	p.Issuer = strings.TrimRight(strings.TrimSpace(p.Issuer), "/")
	p.ClientID = strings.TrimSpace(p.ClientID)
	if !idRE.MatchString(p.ID) {
		return errors.New("id must be 1-32 lowercase letters, digits or '-'")
	}
	if p.Name == "" || len(p.Name) > 64 {
		return errors.New("name must be 1-64 characters")
	}
	u, err := url.Parse(p.Issuer)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("issuer must be an http(s) URL such as https://accounts.example.com")
	}
	if p.ClientID == "" || len(p.ClientID) > 512 {
		return errors.New("client id is required")
	}
	p.withDefaults()
	hasOpenID := false
	for _, sc := range p.Scopes {
		if !scopeRE.MatchString(sc) {
			return fmt.Errorf("invalid scope %q", sc)
		}
		hasOpenID = hasOpenID || sc == gooidc.ScopeOpenID
	}
	if !hasOpenID {
		p.Scopes = append([]string{gooidc.ScopeOpenID}, p.Scopes...)
	}
	for _, cl := range []string{p.UsernameClaim, p.EmailClaim, p.DisplayNameClaim, p.GroupsClaim} {
		if !claimRE.MatchString(cl) {
			return fmt.Errorf("invalid claim name %q", cl)
		}
	}
	for _, l := range []*[]string{&p.AdminGroups, &p.AllowedGroups} {
		clean := []string{}
		for _, g := range *l {
			if g = strings.TrimSpace(g); g != "" && !slices.Contains(clean, g) {
				if len(g) > 256 {
					return errors.New("group names are limited to 256 characters")
				}
				clean = append(clean, g)
			}
		}
		*l = clean
	}
	switch p.Prompt {
	case "", "login", "consent", "select_account", "none":
	default:
		return errors.New("prompt must be empty, login, consent or select_account")
	}
	return nil
}

// Service is the SSO module.
type Service struct {
	d    *app.Deps
	auth *auth.Service
	log  *slog.Logger
	http *http.Client

	cfgMu sync.Mutex // serializes provider config writes

	provMu    sync.Mutex
	providers map[string]*cachedProvider // discovery documents by provider id

	flowMu   sync.Mutex
	flows    map[string]*flow // by state
	ipLimits map[string]*ipLimit
}

type ipLimit struct {
	lim  *rate.Limiter
	seen time.Time
}

// allowIP rate-limits unauthenticated sign-in starts per client (each creates a pending flow).
func (s *Service) allowIP(key string) bool {
	s.flowMu.Lock()
	defer s.flowMu.Unlock()
	l := s.ipLimits[key]
	if l == nil {
		l = &ipLimit{lim: rate.NewLimiter(rate.Every(2*time.Second), 20)}
		s.ipLimits[key] = l
	}
	l.seen = time.Now()
	return l.lim.Allow()
}

type cachedProvider struct {
	issuer  string
	p       *gooidc.Provider
	fetched time.Time
}

// Mount registers the SSO endpoints and plugs the providers into the login screen.
func Mount(d *app.Deps, _ *core.Core) error {
	a := auth.ServiceFor(d)
	if a == nil {
		return errors.New("oidc: the auth module is not mounted")
	}
	if err := d.Store.Migrate(d.Ctx); err != nil {
		return err
	}
	s := &Service{d: d, auth: a, log: d.Log.With("module", "oidc"), http: &http.Client{Timeout: httpTimeout},
		providers: map[string]*cachedProvider{}, flows: map[string]*flow{}, ipLimits: map[string]*ipLimit{}}
	a.SetSSOProvider(s)

	pub, api, admin := d.Router.Public(), d.Router.API(), d.Router.Admin()
	pub.GET("/auth/oidc/login", s.handleLogin)
	pub.GET("/auth/oidc/link", s.handleLink)
	pub.GET("/auth/oidc/callback", s.handleCallback)
	api.GET("/auth/oidc/identities", s.handleMyIdentities)
	api.DELETE("/auth/oidc/identities/:id", s.handleUnlink)
	admin.GET("/admin/oidc/providers", s.handleListProviders)
	admin.POST("/admin/oidc/providers", s.handleCreateProvider)
	admin.PATCH("/admin/oidc/providers/:id", s.handleUpdateProvider)
	admin.DELETE("/admin/oidc/providers/:id", s.handleDeleteProvider)
	admin.POST("/admin/oidc/test", s.handleTest)

	go s.janitor()
	return nil
}

func (s *Service) janitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.d.Ctx.Done():
			return
		case now := <-t.C:
			s.flowMu.Lock()
			for k, f := range s.flows {
				if now.After(f.expires) {
					delete(s.flows, k)
				}
			}
			for k, l := range s.ipLimits {
				if now.Sub(l.seen) > 10*time.Minute {
					delete(s.ipLimits, k)
				}
			}
			s.flowMu.Unlock()
		}
	}
}

// ---- configuration --------------------------------------------------------------------------------------------------

func (s *Service) loadProviders(ctx context.Context) ([]ProviderConfig, error) {
	var list []ProviderConfig
	if _, err := s.d.Store.Settings.GetJSON(ctx, configScope, configKey, &list); err != nil {
		return nil, err
	}
	for i := range list {
		list[i].withDefaults()
	}
	return list, nil
}

func (s *Service) saveProviders(ctx context.Context, list []ProviderConfig) error {
	if list == nil {
		list = []ProviderConfig{}
	}
	return s.d.Store.Settings.SetJSON(ctx, configScope, configKey, list)
}

func (s *Service) provider(ctx context.Context, id string) (*ProviderConfig, error) {
	list, err := s.loadProviders(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == id {
			return &list[i], nil
		}
	}
	return nil, httpx.NotFound("unknown identity provider")
}

func (s *Service) clientSecret(p *ProviderConfig) (string, error) {
	if p.ClientSecretEnc == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(p.ClientSecretEnc)
	if err != nil {
		return "", err
	}
	b, err := s.d.Vault.SystemOpen(raw)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *Service) sealSecret(secret string) (string, error) {
	if secret == "" {
		return "", nil
	}
	b, err := s.d.Vault.SystemSeal([]byte(secret))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// discover returns the (cached) discovery document of p.
func (s *Service) discover(ctx context.Context, p *ProviderConfig, fresh bool) (*gooidc.Provider, error) {
	s.provMu.Lock()
	cp := s.providers[p.ID]
	s.provMu.Unlock()
	if !fresh && cp != nil && cp.issuer == p.Issuer && time.Since(cp.fetched) < discoveryTTL {
		return cp.p, nil
	}
	dctx, cancel := context.WithTimeout(gooidc.ClientContext(ctx, s.http), httpTimeout)
	defer cancel()
	// The discovery context also serves later key fetches: keep the client, drop the deadline.
	prov, err := gooidc.NewProvider(gooidc.ClientContext(context.WithoutCancel(dctx), s.http), p.Issuer)
	if err != nil {
		return nil, err
	}
	s.provMu.Lock()
	s.providers[p.ID] = &cachedProvider{issuer: p.Issuer, p: prov, fetched: time.Now()}
	s.provMu.Unlock()
	return prov, nil
}

func (s *Service) forget(id string) {
	s.provMu.Lock()
	delete(s.providers, id)
	s.provMu.Unlock()
}

// ---- auth.SSOProvider -------------------------------------------------------------------------------------------------

// LoginMethods implements auth.SSOProvider: the enabled providers.
func (s *Service) LoginMethods(ctx context.Context) []auth.SSOMethod {
	list, err := s.loadProviders(ctx)
	if err != nil {
		s.log.Warn("load identity providers", "err", err)
		return nil
	}
	out := []auth.SSOMethod{}
	for _, p := range list {
		if p.Enabled {
			out = append(out, auth.SSOMethod{ID: p.ID, Name: p.Name})
		}
	}
	return out
}

// LinkedProviders implements auth.SSOProvider.
func (s *Service) LinkedProviders(ctx context.Context) (map[string][]string, error) {
	list, err := s.loadProviders(ctx)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, p := range list {
		names[p.ID] = p.Name
	}
	rows, err := s.d.Store.DB.QueryContext(ctx, `SELECT user_id, provider_id FROM oidc_identities ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var uid, pid string
		if err := rows.Scan(&uid, &pid); err != nil {
			return nil, err
		}
		n := names[pid]
		if n == "" {
			n = pid
		}
		if !slices.Contains(out[uid], n) {
			out[uid] = append(out[uid], n)
		}
	}
	return out, rows.Err()
}

// ---- identities -----------------------------------------------------------------------------------------------------

// Identity links a provider subject to a Termstead user.
type Identity struct {
	ID           string     `json:"id"`
	ProviderID   string     `json:"providerId"`
	ProviderName string     `json:"providerName"`
	Subject      string     `json:"subject"`
	Email        string     `json:"email,omitempty"`
	Username     string     `json:"username,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	LastLoginAt  *time.Time `json:"lastLoginAt,omitempty"`
}

func (s *Service) identitiesOf(ctx context.Context, userID string) ([]Identity, error) {
	rows, err := s.d.Store.DB.QueryContext(ctx, `SELECT id, provider_id, subject, email, username, created_at, last_login_at
		FROM oidc_identities WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Identity{}
	for rows.Next() {
		var (
			id      Identity
			created int64
			last    sql.NullInt64
		)
		if err := rows.Scan(&id.ID, &id.ProviderID, &id.Subject, &id.Email, &id.Username, &created, &last); err != nil {
			return nil, err
		}
		id.CreatedAt = time.UnixMilli(created).UTC()
		if last.Valid {
			t := time.UnixMilli(last.Int64).UTC()
			id.LastLoginAt = &t
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Service) userForSubject(ctx context.Context, providerID, subject string) (string, error) {
	var uid string
	err := s.d.Store.DB.QueryRowContext(ctx, `SELECT user_id FROM oidc_identities WHERE provider_id = ? AND subject = ?`, providerID, subject).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return uid, err
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("oidc: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
