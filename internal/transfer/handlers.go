package transfer

import (
	"net/http"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/vfs"
)

// Mount creates the transfer manager on the vfs registry and registers /api/transfers:
//
//	GET    /api/transfers              → Info[] (own, oldest first)
//	POST   /api/transfers              → 201 Info (queued; progress arrives as {type:'transfer'} events)
//	GET    /api/transfers/:id          → Info
//	POST   /api/transfers/:id/cancel   → {ok}
//	POST   /api/transfers/:id/retry    → 201 Info (resume an interrupted / failed transfer; replaces the old one)
//	DELETE /api/transfers/:id          → {ok} (cancels a running transfer, forgets it)
//	DELETE /api/transfers              → {removed} (forget every finished transfer)
func Mount(d *app.Deps, reg *vfs.Registry) (*Manager, error) {
	m := New(d, reg)
	api := d.Router.API()
	api.GET("/transfers", func(c *echo.Context) error {
		return c.JSON(http.StatusOK, m.List(httpx.UserFrom(c)))
	})
	api.POST("/transfers", func(c *echo.Context) error {
		var req Request
		if err := httpx.Bind(c, &req); err != nil {
			return err
		}
		in, err := m.Create(c.Request().Context(), httpx.UserFrom(c), req)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, in)
	})
	api.DELETE("/transfers", func(c *echo.Context) error {
		return c.JSON(http.StatusOK, map[string]int{"removed": m.RemoveFinished(httpx.UserFrom(c))})
	})
	api.GET("/transfers/:id", func(c *echo.Context) error {
		in, err := m.Get(httpx.UserFrom(c), c.Param("id"))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, in)
	})
	api.POST("/transfers/:id/cancel", func(c *echo.Context) error {
		if err := m.Cancel(httpx.UserFrom(c), c.Param("id")); err != nil {
			return err
		}
		return httpx.OK(c)
	})
	api.POST("/transfers/:id/retry", func(c *echo.Context) error {
		in, err := m.Retry(c.Request().Context(), httpx.UserFrom(c), c.Param("id"))
		if err != nil {
			return vfs.FSError(err, "")
		}
		return c.JSON(http.StatusCreated, in)
	})
	api.DELETE("/transfers/:id", func(c *echo.Context) error {
		if err := m.Remove(httpx.UserFrom(c), c.Param("id")); err != nil {
			return err
		}
		return httpx.OK(c)
	})
	return m, nil
}
