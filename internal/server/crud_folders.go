package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/nexterm/nexterm/internal/app"
	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
)

func (s *Server) mountFolders() {
	api := s.Deps.Router.API()
	api.GET("/folders", s.listFolders)
	api.POST("/folders", s.createFolder)
	api.PATCH("/folders/:id", s.updateFolder)
	api.DELETE("/folders/:id", s.deleteFolder)
}

// checkFolderVisible verifies that folderID exists and is visible to u (400 otherwise: it is an invalid reference).
func (s *Server) checkFolderVisible(ctx context.Context, u *model.User, folderID string) error {
	f, err := s.Deps.Store.Folders.Get(ctx, folderID)
	if errors.Is(err, model.ErrNotFound) || (err == nil && !app.Visible(u, f.OwnerID, f.Shared)) {
		return httpx.BadRequest("folder not found")
	}
	return err
}

func (s *Server) modifiableFolder(ctx context.Context, u *model.User, id string) (*model.Folder, error) {
	f, err := s.Deps.Store.Folders.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !app.Visible(u, f.OwnerID, f.Shared) {
		return nil, httpx.ErrNotFound
	}
	if !app.CanModify(u, f.OwnerID) {
		return nil, httpx.Forbidden("shared folders are read-only")
	}
	return f, nil
}

func (s *Server) listFolders(c *echo.Context) error {
	list, err := s.Deps.Store.Folders.ListVisible(c.Request().Context(), httpx.UserFrom(c).ID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, list)
}

func (s *Server) validateFolder(f *model.Folder) error {
	f.Name = strings.TrimSpace(f.Name)
	if err := checkLen("name", f.Name, 1, 200); err != nil {
		return err
	}
	if err := checkLen("color", f.Color, 0, 64); err != nil {
		return err
	}
	return checkLen("icon", f.Icon, 0, 64<<10)
}

func (s *Server) createFolder(c *echo.Context) error {
	var in model.Folder
	if err := httpx.BindLimit(c, &in, 256<<10); err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	f := &model.Folder{ParentID: in.ParentID, Name: in.Name, Color: in.Color, Icon: in.Icon, SortOrder: in.SortOrder,
		Shared: in.Shared, OwnerID: u.ID}
	if f.Shared && !u.IsAdmin() {
		return httpx.Forbidden("only administrators can share folders")
	}
	if err := s.validateFolder(f); err != nil {
		return err
	}
	if f.ParentID != "" {
		if err := s.checkFolderVisible(ctx, u, f.ParentID); err != nil {
			return err
		}
	}
	if err := s.Deps.Store.Folders.Create(ctx, f); err != nil {
		return err
	}
	s.Deps.Audit.Log(c, "folder.create", f.ID, map[string]string{"name": f.Name})
	return c.JSON(http.StatusCreated, f)
}

func (s *Server) updateFolder(c *echo.Context) error {
	p, err := decodePatch(c)
	if err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	f, err := s.modifiableFolder(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	wasShared, oldParent := f.Shared, f.ParentID
	if err := p.apply(map[string]any{"parentId": &f.ParentID, "name": &f.Name, "color": &f.Color, "icon": &f.Icon,
		"sortOrder": &f.SortOrder, "shared": &f.Shared}); err != nil {
		return err
	}
	if f.Shared && !wasShared && !u.IsAdmin() {
		return httpx.Forbidden("only administrators can share folders")
	}
	if err := s.validateFolder(f); err != nil {
		return err
	}
	if f.ParentID != "" && f.ParentID != oldParent {
		if err := s.checkFolderVisible(ctx, u, f.ParentID); err != nil {
			return err
		}
		cycle, err := s.Deps.Store.Folders.IsInSubtree(ctx, f.ID, f.ParentID)
		if err != nil {
			return err
		}
		if cycle {
			return httpx.BadRequest("a folder cannot be moved into itself or one of its subfolders")
		}
	}
	if err := s.Deps.Store.Folders.Update(ctx, f); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, f)
}

// deleteFolder deletes a folder. With ?recursive=1 its whole subtree (subfolders and connections) is deleted, which
// requires that the caller may modify every item in it; otherwise children move up to the folder's parent.
func (s *Server) deleteFolder(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	f, err := s.modifiableFolder(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	recursive := isTruthy(c.QueryParam("recursive"))
	if recursive && !u.IsAdmin() {
		n, err := s.Deps.Store.Folders.ForeignItemsInSubtree(ctx, f.ID, u.ID)
		if err != nil {
			return err
		}
		if n > 0 {
			return httpx.Forbidden("the folder contains items owned by other users")
		}
	}
	deleted, err := s.Deps.Store.Folders.Delete(ctx, f.ID, recursive)
	if err != nil {
		return err
	}
	s.Deps.Audit.Log(c, "folder.delete", f.ID, map[string]any{"name": f.Name, "recursive": recursive, "deletedConnections": len(deleted)})
	return httpx.OK(c)
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
