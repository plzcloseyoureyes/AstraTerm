package oidc

import (
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/store"
)

type providersResponse struct {
	Providers []ProviderView `json:"providers"`
	// RedirectURI is the callback to register at the identity provider (for the address the admin uses).
	RedirectURI string `json:"redirectUri"`
}

func (s *Service) handleListProviders(c *echo.Context) error {
	list, err := s.loadProviders(c.Request().Context())
	if err != nil {
		return err
	}
	out := providersResponse{Providers: []ProviderView{}, RedirectURI: s.redirectURI(c)}
	for _, p := range list {
		out.Providers = append(out.Providers, p.view())
	}
	return c.JSON(http.StatusOK, out)
}

// providerInput is a provider plus its write-only client secret (nil = unchanged, "" = remove).
type providerInput struct {
	ProviderConfig
	ClientSecret *string `json:"clientSecret"`
}

func (s *Service) handleCreateProvider(c *echo.Context) error {
	var in providerInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	if err := s.auth.RequireAdminSudo(c); err != nil {
		return err
	}
	ctx := c.Request().Context()
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	list, err := s.loadProviders(ctx)
	if err != nil {
		return err
	}
	if len(list) >= maxProviders {
		return httpx.Conflict("too many identity providers")
	}
	p := in.ProviderConfig
	p.ID = strings.ToLower(strings.TrimSpace(p.ID))
	if p.ID == "" {
		p.ID = strings.Trim(strings.ToLower(usernameStrip.ReplaceAllString(strings.ReplaceAll(p.Name, " ", "-"), "")), "-._@")
		if len(p.ID) > 32 {
			p.ID = p.ID[:32]
		}
	}
	if err := p.validate(); err != nil {
		return httpx.BadRequest(err.Error())
	}
	for _, x := range list {
		if x.ID == p.ID {
			return httpx.Conflict("an identity provider with this id already exists")
		}
	}
	p.ClientSecretEnc = ""
	if in.ClientSecret != nil {
		if p.ClientSecretEnc, err = s.sealSecret(*in.ClientSecret); err != nil {
			return httpx.Internal(err)
		}
	}
	p.CreatedAt, p.UpdatedAt = store.Now(), store.Now()
	if err := s.saveProviders(ctx, append(list, p)); err != nil {
		return err
	}
	s.d.Audit.Log(c, "admin.sso.provider.create", p.ID, map[string]any{"name": p.Name, "issuer": p.Issuer, "enabled": p.Enabled})
	return c.JSON(http.StatusCreated, p.view())
}

// handleUpdateProvider applies a partial update (omitted keys keep their value; the id cannot change).
func (s *Service) handleUpdateProvider(c *echo.Context) error {
	ctx := c.Request().Context()
	if err := s.auth.RequireAdminSudo(c); err != nil {
		return err
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	list, err := s.loadProviders(ctx)
	if err != nil {
		return err
	}
	idx := -1
	for i := range list {
		if list[i].ID == c.Param("id") {
			idx = i
		}
	}
	if idx < 0 {
		return httpx.ErrNotFound
	}
	list0Issuer := list[idx].Issuer
	in := providerInput{ProviderConfig: list[idx]}
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	p := in.ProviderConfig
	p.ID, p.CreatedAt, p.ClientSecretEnc = list[idx].ID, list[idx].CreatedAt, list[idx].ClientSecretEnc
	if err := p.validate(); err != nil {
		return httpx.BadRequest(err.Error())
	}
	if in.ClientSecret != nil {
		if p.ClientSecretEnc, err = s.sealSecret(*in.ClientSecret); err != nil {
			return httpx.Internal(err)
		}
	}
	p.UpdatedAt = store.Now()
	list[idx] = p
	if err := s.saveProviders(ctx, list); err != nil {
		return err
	}
	s.forget(p.ID)
	// Subjects are only unique per issuer: identities linked through the old issuer must not sign in to the same
	// accounts through a different one.
	var unlinked int64
	if !strings.EqualFold(p.Issuer, list0Issuer) {
		res, err := s.d.Store.DB.ExecContext(ctx, `DELETE FROM oidc_identities WHERE provider_id = ?`, p.ID)
		if err != nil {
			return err
		}
		unlinked, _ = res.RowsAffected()
	}
	s.d.Audit.Log(c, "admin.sso.provider.update", p.ID, map[string]any{"name": p.Name, "issuer": p.Issuer, "enabled": p.Enabled,
		"secretChanged": in.ClientSecret != nil, "unlinkedIdentities": unlinked})
	return c.JSON(http.StatusOK, p.view())
}

func (s *Service) handleDeleteProvider(c *echo.Context) error {
	ctx := c.Request().Context()
	if err := s.auth.RequireAdminSudo(c); err != nil {
		return err
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	list, err := s.loadProviders(ctx)
	if err != nil {
		return err
	}
	id := c.Param("id")
	out := list[:0]
	found := false
	for _, p := range list {
		if p.ID == id {
			found = true
			continue
		}
		out = append(out, p)
	}
	if !found {
		return httpx.ErrNotFound
	}
	if err := s.saveProviders(ctx, out); err != nil {
		return err
	}
	res, err := s.d.Store.DB.ExecContext(ctx, `DELETE FROM oidc_identities WHERE provider_id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	s.forget(id)
	s.d.Audit.Log(c, "admin.sso.provider.delete", id, map[string]any{"unlinkedIdentities": n})
	return httpx.OK(c)
}

type testRequest struct {
	ID     string `json:"id"`
	Issuer string `json:"issuer"`
}

type testResponse struct {
	OK                    bool     `json:"ok"`
	Issuer                string   `json:"issuer,omitempty"`
	AuthorizationEndpoint string   `json:"authorizationEndpoint,omitempty"`
	TokenEndpoint         string   `json:"tokenEndpoint,omitempty"`
	UserinfoEndpoint      string   `json:"userinfoEndpoint,omitempty"`
	ScopesSupported       []string `json:"scopesSupported,omitempty"`
	ClaimsSupported       []string `json:"claimsSupported,omitempty"`
	PKCEMethods           []string `json:"pkceMethods,omitempty"`
	Error                 string   `json:"error,omitempty"`
	LatencyMs             int64    `json:"latencyMs"`
	RedirectURI           string   `json:"redirectUri"`
}

// handleTest fetches the discovery document of a saved provider (id) or of an issuer URL being edited.
func (s *Service) handleTest(c *echo.Context) error {
	var req testRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	ctx := c.Request().Context()
	var p ProviderConfig
	if req.ID != "" && req.Issuer == "" {
		saved, err := s.provider(ctx, req.ID)
		if err != nil {
			return err
		}
		p = *saved
	} else {
		p = ProviderConfig{ID: "test", Name: "test", Issuer: req.Issuer, ClientID: "test"}
		if err := p.validate(); err != nil {
			return httpx.BadRequest(err.Error())
		}
	}
	started := time.Now()
	out := testResponse{RedirectURI: s.redirectURI(c)}
	prov, err := s.discover(ctx, &p, true)
	out.LatencyMs = time.Since(started).Milliseconds()
	if req.ID == "" || req.Issuer != "" {
		s.forget("test")
	}
	if err != nil {
		out.Error = err.Error()
		return c.JSON(http.StatusOK, out)
	}
	var meta struct {
		Issuer   string   `json:"issuer"`
		Auth     string   `json:"authorization_endpoint"`
		Token    string   `json:"token_endpoint"`
		UserInfo string   `json:"userinfo_endpoint"`
		Scopes   []string `json:"scopes_supported"`
		Claims   []string `json:"claims_supported"`
		PKCE     []string `json:"code_challenge_methods_supported"`
	}
	_ = prov.Claims(&meta)
	out.OK, out.Issuer, out.AuthorizationEndpoint, out.TokenEndpoint, out.UserinfoEndpoint = true, meta.Issuer, meta.Auth, meta.Token, meta.UserInfo
	out.ScopesSupported, out.ClaimsSupported, out.PKCEMethods = meta.Scopes, meta.Claims, meta.PKCE
	return c.JSON(http.StatusOK, out)
}
