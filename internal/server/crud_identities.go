package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
)

func (s *Server) mountIdentities() {
	api := s.Deps.Router.API()
	api.GET("/identities", s.listIdentities)
	api.POST("/identities", s.createIdentity)
	api.GET("/identities/:id", s.getIdentity)
	api.PATCH("/identities/:id", s.updateIdentity)
	api.DELETE("/identities/:id", s.deleteIdentity)
}

// ownIdentity loads an identity owned by u (identities are private; 404 otherwise).
func (s *Server) ownIdentity(ctx context.Context, u *model.User, id string) (*model.Identity, error) {
	i, err := s.Deps.Store.Identities.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if i.OwnerID != u.ID {
		return nil, httpx.ErrNotFound
	}
	return i, nil
}

func (s *Server) validateIdentity(ctx context.Context, i *model.Identity) error {
	i.Name = strings.TrimSpace(i.Name)
	i.Username = strings.TrimSpace(i.Username)
	if err := checkLen("name", i.Name, 1, 200); err != nil {
		return err
	}
	if err := checkLen("username", i.Username, 0, 255); err != nil {
		return err
	}
	if i.KeyID != "" {
		k, err := s.Deps.Store.Keys.Get(ctx, i.KeyID)
		if err != nil || k.OwnerID != i.OwnerID {
			return httpx.BadRequest("key not found")
		}
	}
	i.Normalize()
	return nil
}

func (s *Server) listIdentities(c *echo.Context) error {
	list, err := s.Deps.Store.Identities.ListByOwner(c.Request().Context(), httpx.UserFrom(c).ID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, list)
}

func (s *Server) getIdentity(c *echo.Context) error {
	i, err := s.ownIdentity(c.Request().Context(), httpx.UserFrom(c), c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, i)
}

func (s *Server) createIdentity(c *echo.Context) error {
	var in model.Identity
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	i := &model.Identity{Name: in.Name, Username: in.Username, KeyID: in.KeyID, OwnerID: u.ID}
	if err := s.validateIdentity(ctx, i); err != nil {
		return err
	}
	if err := s.applySecrets(&i.SecretsEnc, &i.SecretKeys, in.Secrets); err != nil {
		return err
	}
	if err := s.Deps.Store.Identities.Create(ctx, i); err != nil {
		return err
	}
	s.Deps.Audit.Log(c, "identity.create", i.ID, map[string]string{"name": i.Name})
	return c.JSON(http.StatusCreated, i)
}

func (s *Server) updateIdentity(c *echo.Context) error {
	p, err := decodePatch(c)
	if err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	i, err := s.ownIdentity(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	var secrets map[string]string
	if err := p.apply(map[string]any{"name": &i.Name, "username": &i.Username, "keyId": &i.KeyID, "secrets": &secrets}); err != nil {
		return err
	}
	if err := s.validateIdentity(ctx, i); err != nil {
		return err
	}
	if err := s.applySecrets(&i.SecretsEnc, &i.SecretKeys, secrets); err != nil {
		return err
	}
	if err := s.Deps.Store.Identities.Update(ctx, i); err != nil {
		return err
	}
	s.Deps.Audit.Log(c, "identity.update", i.ID, map[string]string{"name": i.Name})
	return c.JSON(http.StatusOK, i)
}

func (s *Server) deleteIdentity(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	i, err := s.ownIdentity(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	if err := s.Deps.Store.Identities.Delete(ctx, i.ID); err != nil {
		return err
	}
	s.Deps.Audit.Log(c, "identity.delete", i.ID, map[string]string{"name": i.Name})
	return httpx.OK(c)
}
