package webproxy

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

type createRequest struct {
	Spec
	// Mode "path" forces path mode (e.g. a browser that cannot resolve *.localhost); "" = automatic.
	Mode string `json:"mode,omitempty"`
	// Check dials the upstream once before answering (default true), so unreachable targets fail with a message.
	Check *bool `json:"check,omitempty"`
}

var errUnavailable = httpx.NewError(http.StatusConflict, "webproxy_unavailable",
	"the web proxy needs AstraTerm to be opened on a loopback address (localhost), a wildcard domain configured by an "+
		"administrator (Settings → Web proxy), or path mode enabled")

// open resolves spec, optionally checks the upstream, and registers a proxy for user.
func (s *Service) open(ctx context.Context, user *model.User, kind string, sp Spec, check bool) (*Proxy, error) {
	rt, err := s.resolve(ctx, user, sp)
	if err != nil {
		return nil, err
	}
	if check {
		if err := s.preflight(ctx, rt); err != nil {
			return nil, err
		}
	}
	p := newProxy(s, user, kind, sp, rt)
	s.add(p)
	return p, nil
}

func (s *Service) handleCreate(c *echo.Context) error {
	var req createRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if req.Mode != "" && req.Mode != "path" && req.Mode != "auto" {
		return httpx.BadRequest("mode must be auto or path")
	}
	user := httpx.UserFrom(c)
	p, err := s.open(c.Request().Context(), user, "web", req.Spec, req.Check == nil || *req.Check)
	if err != nil {
		return err
	}
	info, err := s.entryInfo(c, p, "", req.Mode)
	if err != nil {
		s.remove(p, "no usable proxy mode")
		return err
	}
	s.d.Audit.Log(c, "webproxy.open", p.id, map[string]any{"target": p.rt.target.String(), "via": p.rt.via.Kind,
		"viaId": p.rt.via.ID, "mode": info.Mode, "insecureTls": p.rt.insecure})
	return c.JSON(http.StatusCreated, info)
}

// entryInfo returns the proxy's Info with an entry URL for the requesting browser.
func (s *Service) entryInfo(c *echo.Context, p *Proxy, path, mode string) (Info, error) {
	r := c.Request()
	scheme := requestScheme(r)
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	p.addUIOrigin(uiOrigin(r, scheme))
	e, err := s.entryFor(p, r, scheme, path, mode)
	if errors.Is(err, errNoMode) {
		return Info{}, errUnavailable
	}
	if err != nil {
		return Info{}, err
	}
	info := p.info()
	info.URL, info.Mode, info.Base = e.URL, e.Mode, e.Base
	return info, nil
}

func (s *Service) handleList(c *echo.Context) error {
	out := []Info{}
	for _, p := range s.list(httpx.UserFrom(c)) {
		out = append(out, p.info())
	}
	return c.JSON(http.StatusOK, out)
}

func (s *Service) handleGet(c *echo.Context) error {
	p := s.owned(httpx.UserFrom(c), c.Param("id"))
	if p == nil {
		return httpx.NotFound("web proxy not found (closed or expired)")
	}
	return c.JSON(http.StatusOK, p.info())
}

func (s *Service) handleDelete(c *echo.Context) error {
	p := s.owned(httpx.UserFrom(c), c.Param("id"))
	if p == nil {
		return httpx.NotFound("web proxy not found (closed or expired)")
	}
	s.remove(p, "closed")
	s.d.Audit.Log(c, "webproxy.close", p.id, map[string]any{"target": p.rt.target.String()})
	return httpx.OK(c)
}

// handleURL issues a fresh entry URL (one-time token) — for reloading a tab, reopening it after a page reload, or
// "open in new window".
func (s *Service) handleURL(c *echo.Context) error {
	var req struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
	}
	if err := httpx.BindOptional(c, &req); err != nil {
		return err
	}
	p := s.owned(httpx.UserFrom(c), c.Param("id"))
	if p == nil {
		return httpx.NotFound("web proxy not found (closed or expired)")
	}
	p.touch()
	info, err := s.entryInfo(c, p, req.Path, req.Mode)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, info)
}
