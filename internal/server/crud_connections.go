package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/nexterm/nexterm/internal/app"
	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/store"
)

const (
	maxOptionsBytes = 256 << 10
	maxSecretBytes  = 64 << 10
	maxBulkItems    = 5000
)

func (s *Server) mountConnections() {
	api := s.Deps.Router.API()
	api.GET("/connections", s.listConnections)
	api.POST("/connections", s.createConnection)
	api.POST("/connections/reorder", s.reorderConnections)
	api.POST("/connections/bulk-delete", s.bulkDeleteConnections)
	api.GET("/connections/:id", s.getConnection)
	api.PATCH("/connections/:id", s.updateConnection)
	api.DELETE("/connections/:id", s.deleteConnection)
	api.POST("/connections/:id/duplicate", s.duplicateConnection)
}

// visibleConnection loads a connection the user may see (404 otherwise).
func (s *Server) visibleConnection(ctx context.Context, u *model.User, id string) (*model.Connection, error) {
	c, err := s.Deps.Store.Connections.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !app.Visible(u, c.OwnerID, c.Shared) {
		return nil, httpx.ErrNotFound
	}
	return c, nil
}

// modifiableConnection loads a visible connection the user may modify (owner, or admin for shared rows).
func (s *Server) modifiableConnection(ctx context.Context, u *model.User, id string) (*model.Connection, error) {
	c, err := s.visibleConnection(ctx, u, id)
	if err != nil {
		return nil, err
	}
	if !app.CanModify(u, c.OwnerID) {
		return nil, httpx.Forbidden("shared connections are read-only")
	}
	return c, nil
}

func (s *Server) listConnections(c *echo.Context) error {
	list, err := s.Deps.Store.Connections.ListVisible(c.Request().Context(), httpx.UserFrom(c).ID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, list)
}

func (s *Server) getConnection(c *echo.Context) error {
	conn, err := s.visibleConnection(c.Request().Context(), httpx.UserFrom(c), c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, conn)
}

func (s *Server) createConnection(c *echo.Context) error {
	var in model.Connection
	if err := httpx.BindLimit(c, &in, 1<<20); err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	conn := &model.Connection{
		FolderID: in.FolderID, Name: in.Name, Protocol: in.Protocol, Host: in.Host, Port: in.Port, Username: in.Username,
		IdentityID: in.IdentityID, KeyID: in.KeyID, AuthMethod: in.AuthMethod, Color: in.Color, Icon: in.Icon,
		Tags: in.Tags, Notes: in.Notes, Favorite: in.Favorite, SortOrder: in.SortOrder, Options: in.Options,
		Shared: in.Shared, OwnerID: u.ID,
	}
	if conn.Shared && !u.IsAdmin() {
		return httpx.Forbidden("only administrators can share connections")
	}
	if err := s.validateConnection(ctx, u, conn); err != nil {
		return err
	}
	if err := s.applySecrets(&conn.SecretsEnc, &conn.SecretKeys, in.Secrets); err != nil {
		return err
	}
	if err := s.Deps.Store.Connections.Create(ctx, conn); err != nil {
		return err
	}
	s.Deps.Audit.Log(c, "connection.create", conn.ID, map[string]any{"name": conn.Name, "protocol": conn.Protocol, "host": conn.Host})
	return c.JSON(http.StatusCreated, conn)
}

func (s *Server) updateConnection(c *echo.Context) error {
	p, err := decodePatch(c)
	if err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	conn, err := s.modifiableConnection(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	wasShared := conn.Shared
	var secrets map[string]string
	if err := p.apply(map[string]any{
		"folderId": &conn.FolderID, "name": &conn.Name, "protocol": &conn.Protocol, "host": &conn.Host, "port": &conn.Port,
		"username": &conn.Username, "identityId": &conn.IdentityID, "keyId": &conn.KeyID, "authMethod": &conn.AuthMethod,
		"color": &conn.Color, "icon": &conn.Icon, "tags": &conn.Tags, "notes": &conn.Notes, "favorite": &conn.Favorite,
		"sortOrder": &conn.SortOrder, "options": &conn.Options, "shared": &conn.Shared, "secrets": &secrets,
	}); err != nil {
		return err
	}
	if conn.Shared && !wasShared && !u.IsAdmin() {
		return httpx.Forbidden("only administrators can share connections")
	}
	if err := s.validateConnection(ctx, u, conn); err != nil {
		return err
	}
	if p.has("secrets") && !p.isNull("secrets") {
		if err := s.applySecrets(&conn.SecretsEnc, &conn.SecretKeys, secrets); err != nil {
			return err
		}
	}
	if err := s.Deps.Store.Connections.Update(ctx, conn); err != nil {
		return err
	}
	changed := make([]string, 0, len(p))
	for k := range p {
		changed = append(changed, k)
	}
	s.Deps.Audit.Log(c, "connection.update", conn.ID, map[string]any{"name": conn.Name, "fields": changed})
	return c.JSON(http.StatusOK, conn)
}

func (s *Server) deleteConnection(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	conn, err := s.modifiableConnection(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	if err := s.Deps.Store.Connections.Delete(ctx, conn.ID); err != nil {
		return err
	}
	s.Deps.Audit.Log(c, "connection.delete", conn.ID, map[string]any{"name": conn.Name})
	return httpx.OK(c)
}

// duplicateConnection copies a visible connection into the caller's library. Copies of other users' (shared)
// connections do not carry secrets, identity or key references, so credentials can never be redirected to a host
// chosen by the copier.
func (s *Server) duplicateConnection(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	src, err := s.visibleConnection(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	conn := src.Clone()
	conn.ID, conn.OwnerID, conn.Shared, conn.LastUsedAt, conn.Secrets = "", u.ID, false, nil, nil
	conn.Name = truncateRunes(src.Name+" (copy)", 200)
	if src.OwnerID != u.ID {
		conn.SecretsEnc, conn.SecretKeys, conn.IdentityID, conn.KeyID = nil, []string{}, "", ""
		if conn.FolderID != "" {
			if f, err := s.Deps.Store.Folders.Get(ctx, conn.FolderID); err != nil || !app.Visible(u, f.OwnerID, f.Shared) {
				conn.FolderID = ""
			}
		}
	}
	if err := s.Deps.Store.Connections.Create(ctx, conn); err != nil {
		return err
	}
	s.Deps.Audit.Log(c, "connection.duplicate", conn.ID, map[string]any{"source": src.ID, "name": conn.Name})
	return c.JSON(http.StatusCreated, conn)
}

type reorderRequest struct {
	Items []struct {
		ID        string `json:"id"`
		FolderID  string `json:"folderId"`
		SortOrder int    `json:"sortOrder"`
	} `json:"items"`
}

func (s *Server) reorderConnections(c *echo.Context) error {
	var req reorderRequest
	if err := httpx.BindLimit(c, &req, 4<<20); err != nil {
		return err
	}
	if len(req.Items) > maxBulkItems {
		return httpx.BadRequest(fmt.Sprintf("at most %d items per request", maxBulkItems))
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	folders := map[string]bool{}
	items := make([]store.ReorderItem, 0, len(req.Items))
	for _, it := range req.Items {
		if _, err := s.modifiableConnection(ctx, u, it.ID); err != nil {
			return err
		}
		if it.FolderID != "" && !folders[it.FolderID] {
			if err := s.checkFolderVisible(ctx, u, it.FolderID); err != nil {
				return err
			}
			folders[it.FolderID] = true
		}
		items = append(items, store.ReorderItem{ID: it.ID, FolderID: it.FolderID, SortOrder: it.SortOrder})
	}
	if err := s.Deps.Store.Connections.Reorder(ctx, items); err != nil {
		return err
	}
	return httpx.OK(c)
}

type bulkDeleteRequest struct {
	IDs []string `json:"ids"`
}

func (s *Server) bulkDeleteConnections(c *echo.Context) error {
	var req bulkDeleteRequest
	if err := httpx.BindLimit(c, &req, 4<<20); err != nil {
		return err
	}
	if len(req.IDs) > maxBulkItems {
		return httpx.BadRequest(fmt.Sprintf("at most %d ids per request", maxBulkItems))
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	ids := make([]string, 0, len(req.IDs))
	for _, id := range req.IDs {
		conn, err := s.modifiableConnection(ctx, u, id)
		if errors.Is(err, model.ErrNotFound) {
			continue // already gone
		}
		if err != nil {
			return err
		}
		ids = append(ids, conn.ID)
	}
	n, err := s.Deps.Store.Connections.DeleteMany(ctx, ids)
	if err != nil {
		return err
	}
	s.Deps.Audit.Log(c, "connection.bulk_delete", "", map[string]any{"ids": ids})
	return c.JSON(http.StatusOK, map[string]int{"deleted": n})
}

// validateConnection normalizes and validates user-editable fields and references (folder visible to the user;
// identity and key owned by the connection owner).
func (s *Server) validateConnection(ctx context.Context, u *model.User, c *model.Connection) error {
	c.Name = strings.TrimSpace(c.Name)
	c.Host = strings.TrimSpace(c.Host)
	c.Username = strings.TrimSpace(c.Username)
	c.Protocol = model.Protocol(strings.ToLower(strings.TrimSpace(string(c.Protocol))))
	if err := checkLen("name", c.Name, 1, 200); err != nil {
		return err
	}
	if !model.ValidProtocol(c.Protocol) {
		return httpx.BadRequest("invalid protocol")
	}
	if err := checkLen("host", c.Host, 0, 255); err != nil {
		return err
	}
	if err := checkLen("username", c.Username, 0, 255); err != nil {
		return err
	}
	if err := checkLen("notes", c.Notes, 0, 64<<10); err != nil {
		return err
	}
	if err := checkLen("color", c.Color, 0, 64); err != nil {
		return err
	}
	if err := checkLen("icon", c.Icon, 0, 64<<10); err != nil {
		return err
	}
	if c.Port < 0 || c.Port > 65535 {
		return httpx.BadRequest("port must be between 0 and 65535")
	}
	if c.Port == 0 {
		c.Port = model.DefaultPort(c.Protocol)
	}
	if c.AuthMethod == "" {
		c.AuthMethod = model.AuthAuto
	}
	if !model.ValidAuthMethod(c.AuthMethod) {
		return httpx.BadRequest("invalid authMethod")
	}
	tags, err := cleanTags(c.Tags)
	if err != nil {
		return err
	}
	c.Tags = tags
	if c.Options == nil {
		c.Options = model.Options{}
	}
	if b, err := json.Marshal(c.Options); err != nil || len(b) > maxOptionsBytes {
		return httpx.BadRequest("options are invalid or too large")
	}
	if c.FolderID != "" {
		if err := s.checkFolderVisible(ctx, u, c.FolderID); err != nil {
			return err
		}
	}
	if c.IdentityID != "" {
		ident, err := s.Deps.Store.Identities.Get(ctx, c.IdentityID)
		if err != nil || ident.OwnerID != c.OwnerID {
			return httpx.BadRequest("identity not found")
		}
	}
	if c.KeyID != "" {
		k, err := s.Deps.Store.Keys.Get(ctx, c.KeyID)
		if err != nil || k.OwnerID != c.OwnerID {
			return httpx.BadRequest("key not found")
		}
	}
	c.Normalize()
	return nil
}

// applySecrets merges a write-only secrets patch into sealed storage: omitted keys are unchanged, "" deletes a key.
// The vault must be unlocked whenever existing secrets have to be decrypted or new ones sealed.
func (s *Server) applySecrets(enc *[]byte, keys *[]string, patch map[string]string) error {
	if len(patch) == 0 {
		if *keys == nil {
			*keys = []string{}
		}
		return nil
	}
	for k, v := range patch {
		if !secretKeyRE.MatchString(k) {
			return httpx.BadRequest(fmt.Sprintf("invalid secret name %q", k))
		}
		if len(v) > maxSecretBytes {
			return httpx.BadRequest(fmt.Sprintf("secret %q is too large", k))
		}
	}
	current, err := s.Deps.Vault.OpenJSON(*enc)
	if err != nil {
		if errors.Is(err, model.ErrLocked) {
			return httpx.ErrLocked
		}
		return httpx.Internal(err)
	}
	for k, v := range patch {
		if v == "" {
			delete(current, k)
		} else {
			current[k] = v
		}
	}
	sealed, err := s.Deps.Vault.SealJSON(current)
	if err != nil {
		if errors.Is(err, model.ErrLocked) {
			return httpx.ErrLocked
		}
		return httpx.Internal(err)
	}
	*enc, *keys = sealed, model.SortedKeys(current)
	return nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
