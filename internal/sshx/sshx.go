package sshx

import (
	"net/http"

	"github.com/labstack/echo/v5"

	"github.com/nexterm/nexterm/internal/app"
	"github.com/nexterm/nexterm/internal/httpx"
)

// Mount registers the SSH-specific REST endpoints (extensions, see SPEC §9 B1 notes):
//
//	GET /api/sessions/:id/ssh-info → model.SSHConnInfo of the session's SSH transport (SSH-38)
//	GET /api/ssh/algorithms        → AlgorithmCatalog for the algorithm editor (SSH-23)
//	GET /api/ssh/agent             → {available} whether a host SSH agent can be used by the caller
func Mount(d *app.Deps, p *Pool) error {
	api := d.Router.API()
	api.GET("/sessions/:id/ssh-info", func(c *echo.Context) error {
		cl, release, err := p.ForSession(c.Request().Context(), httpx.UserFrom(c), c.Param("id"))
		if err != nil {
			return err
		}
		defer release()
		return c.JSON(http.StatusOK, cl.Info())
	})
	api.GET("/ssh/algorithms", func(c *echo.Context) error {
		return c.JSON(http.StatusOK, Catalog())
	})
	api.GET("/ssh/agent", func(c *echo.Context) error {
		available := p.allowLocalExec(httpx.UserFrom(c)) && HostAgentAvailable()
		return c.JSON(http.StatusOK, map[string]bool{"available": available})
	})
	return nil
}
