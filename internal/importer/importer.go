// Package importer implements Termstead's import/export module (SPEC §6 "Import / export", RESEARCH IMP-1..4, SSH-36):
// importing saved sessions from MobaXterm, PuTTY/KiTTY, OpenSSH ssh_config, Termius, mRemoteNG, Remmina, FileZilla,
// WinSCP, SecureCRT, generic CSV, Termstead JSON and OpenSSH known_hosts; exporting the user's sessions; and admin
// backup/restore. See model.go for the security posture (no credential extraction from third-party files).
package importer

import (
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/core"
	"github.com/termstead/termstead/internal/httpx"
)

// maxImportBody bounds an import request body (file content is JSON-embedded; base64 inflates it ~33%).
const maxImportBody = 48 << 20

type handler struct {
	d    *app.Deps
	c    *core.Core
	sync *syncManager
}

// Mount registers the import/export routes and starts the optional ssh_config watcher.
func Mount(d *app.Deps, c *core.Core) error {
	h := &handler{d: d, c: c, sync: newSyncManager(d)}
	api := d.Router.API()
	api.POST("/import/preview", h.preview, httpx.BodyLimit(maxImportBody))
	api.POST("/import/commit", h.commit, httpx.BodyLimit(maxImportBody))
	api.GET("/import/discover", h.discover)
	api.GET("/import/sync", h.syncStatus)
	// Exports: GET (query parameters, SPEC §6) or POST with a JSON body — the UI uses POST so a passphrase never
	// appears in a URL (proxy / browser logs).
	api.GET("/export", h.exportGet)
	api.POST("/export", h.exportPost)

	admin := d.Router.Admin()
	admin.POST("/import/sync", h.syncSet) // changes the first admin's library (desktop mode)
	admin.GET("/admin/backup", h.backupGet)
	admin.POST("/admin/backup", h.backupPost)
	admin.POST("/admin/restore", h.restore, httpx.BodyLimit(maxRestoreBytes+(maxRestoreBytes/2)))
	admin.GET("/admin/restore", h.restoreStatus)
	admin.DELETE("/admin/restore", h.restoreDiscard)

	h.sync.start()
	return nil
}

// loadContent resolves the import source: an explicit Path (desktop, discovered files), base64, or raw text. It
// returns the bytes and the effective format (a discovered path knows its own format).
func (h *handler) loadContent(format, content string, isBase64 bool, path string) ([]byte, string, error) {
	if strings.TrimSpace(path) != "" {
		if h.d.Cfg == nil || !h.d.Cfg.IsDesktop() {
			return nil, "", httpx.Forbidden("reading local files is only available in desktop mode")
		}
		data, detected, err := resolveDiscoverPath(path)
		if err != nil {
			return nil, "", err
		}
		f := format
		if f == "" || f == fmtAuto {
			f = detected
		}
		return data, f, nil
	}
	if isBase64 {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(content))
		if err != nil {
			// Tolerate URL-safe / unpadded base64.
			if b2, e2 := base64.RawStdEncoding.DecodeString(strings.TrimSpace(content)); e2 == nil {
				b = b2
			} else {
				return nil, "", httpx.BadRequest("invalid base64 content")
			}
		}
		return b, format, nil
	}
	if strings.TrimSpace(content) == "" {
		return nil, "", httpx.BadRequest("no content to import")
	}
	return []byte(content), format, nil
}

func (h *handler) preview(c *echo.Context) error {
	var req previewRequest
	if err := httpx.BindLimit(c, &req, maxImportBody); err != nil {
		return err
	}
	data, format, err := h.loadContent(req.Format, req.Content, req.Base64, req.Path)
	if err != nil {
		return err
	}
	p, err := parseSource(format, data, req.Options)
	if err != nil {
		return err
	}
	resp, err := buildPreview(c.Request().Context(), h.d, httpx.UserFrom(c), p)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

func (h *handler) commit(c *echo.Context) error {
	var req commitRequest
	if err := httpx.BindLimit(c, &req, maxImportBody); err != nil {
		return err
	}
	data, format, err := h.loadContent(req.Format, req.Content, req.Base64, req.Path)
	if err != nil {
		return err
	}
	p, err := parseSource(format, data, req.Options)
	if err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	res, err := commitImport(ctx, h.d, u, p, req)
	if err != nil {
		return err
	}
	h.d.Audit.Log(c, "import.commit", "", map[string]any{
		"format": p.format, "created": res.Created, "updated": res.Updated, "skipped": res.Skipped,
		"folders": res.FoldersCreated, "keys": res.KeysImported, "knownHosts": res.KnownHostsAdded,
	})
	return c.JSON(http.StatusOK, res)
}

func (h *handler) discover(c *echo.Context) error {
	if h.d.Cfg == nil || !h.d.Cfg.IsDesktop() {
		// Not an error: the wizard hides the local-scan buttons when nothing is available.
		return c.JSON(http.StatusOK, map[string]any{"supported": false, "files": []discoverEntry{}})
	}
	files := discoverLocalFiles()
	if files == nil {
		files = []discoverEntry{}
	}
	return c.JSON(http.StatusOK, map[string]any{"supported": true, "files": files})
}

func (h *handler) syncStatus(c *echo.Context) error {
	return c.JSON(http.StatusOK, h.sync.status())
}

func (h *handler) syncSet(c *echo.Context) error {
	if h.d.Cfg == nil || !h.d.Cfg.IsDesktop() {
		return httpx.Forbidden("live sync is only available in desktop mode")
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if err := h.sync.setEnabled(req.Enabled); err != nil {
		return err
	}
	h.d.Audit.Log(c, "import.ssh_config.sync.toggle", "", map[string]any{"enabled": req.Enabled})
	return c.JSON(http.StatusOK, h.sync.status())
}

// exportRequest is the JSON body of POST /api/export (the query parameters of GET /api/export).
type exportRequest struct {
	Format         string   `json:"format"`
	IncludeSecrets bool     `json:"includeSecrets,omitempty"`
	Passphrase     string   `json:"passphrase,omitempty"`
	FolderID       string   `json:"folderId,omitempty"`
	ConnectionIDs  []string `json:"connectionIds,omitempty"`
}

func (h *handler) exportGet(c *echo.Context) error {
	req := exportRequest{
		Format:         c.QueryParam("format"),
		IncludeSecrets: isTruthy(c.QueryParam("includeSecrets")),
		Passphrase:     c.QueryParam("passphrase"),
		FolderID:       c.QueryParam("folderId"),
	}
	if ids := c.QueryParam("connectionIds"); ids != "" {
		req.ConnectionIDs = splitList(ids, ",")
	}
	return h.export(c, req)
}

func (h *handler) exportPost(c *echo.Context) error {
	var req exportRequest
	if err := httpx.BindLimit(c, &req, 1<<20); err != nil {
		return err
	}
	return h.export(c, req)
}

func (h *handler) export(c *echo.Context, req exportRequest) error {
	pr := exportParams{
		format:         strings.ToLower(strings.TrimSpace(req.Format)),
		includeSecrets: req.IncludeSecrets,
		passphrase:     req.Passphrase,
		folderID:       strings.TrimSpace(req.FolderID),
		connectionIDs:  req.ConnectionIDs,
	}
	if pr.format == "" {
		pr.format = exportJSON
	}
	if len(pr.connectionIDs) > maxImportItems {
		return httpx.BadRequest("too many connection ids")
	}
	res, err := runExport(c.Request().Context(), h.d, httpx.UserFrom(c), pr)
	if err != nil {
		return err
	}
	h.d.Audit.Log(c, "export", "", map[string]any{"format": pr.format, "includeSecrets": pr.includeSecrets,
		"folderId": pr.folderID, "connections": len(pr.connectionIDs)})
	return sendFile(c, res)
}

// sendFile writes an export/backup as a download.
func sendFile(c *echo.Context, res exportResult) error {
	c.Response().Header().Set("Content-Disposition", "attachment; filename=\""+res.Filename+"\"")
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	return c.Blob(http.StatusOK, res.ContentType, res.Data)
}

// backupRequest is the JSON body of POST /api/admin/backup (the query parameters of GET /api/admin/backup).
type backupRequest struct {
	IncludeSystemKey bool   `json:"includeSystemKey,omitempty"`
	Passphrase       string `json:"passphrase,omitempty"`
}

func (h *handler) backupGet(c *echo.Context) error {
	return h.backup(c, backupRequest{IncludeSystemKey: isTruthy(c.QueryParam("includeSystemKey")), Passphrase: c.QueryParam("passphrase")})
}

func (h *handler) backupPost(c *echo.Context) error {
	var req backupRequest
	if err := httpx.BindOptional(c, &req); err != nil {
		return err
	}
	return h.backup(c, req)
}

func (h *handler) backup(c *echo.Context, req backupRequest) error {
	res, err := buildBackup(c.Request().Context(), h.d, req.IncludeSystemKey, req.Passphrase)
	if err != nil {
		return err
	}
	h.d.Audit.Log(c, "admin.backup", "", map[string]any{"encrypted": req.Passphrase != "", "systemKey": req.IncludeSystemKey})
	return sendFile(c, res)
}

func (h *handler) restore(c *echo.Context) error {
	var req struct {
		Content    string `json:"content"` // base64 backup
		Passphrase string `json:"passphrase,omitempty"`
	}
	if err := httpx.BindLimit(c, &req, maxRestoreBytes+(maxRestoreBytes/2)); err != nil {
		return err
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(req.Content))
	if err != nil {
		return httpx.BadRequest("invalid base64 backup content")
	}
	res, err := stageRestore(c.Request().Context(), h.d, data, req.Passphrase, httpx.UserFrom(c).Username)
	if err != nil {
		return err
	}
	h.d.Audit.Log(c, "admin.restore.staged", res.StagedDBPath, map[string]any{"warnings": len(res.Warnings)})
	return c.JSON(http.StatusOK, res)
}

// restoreStatus reports a staged restore waiting for the next start: {pending, stagedAt?, stagedBy?, systemKey}.
func (h *handler) restoreStatus(c *echo.Context) error {
	m, pending := readRestoreMarker(h.d.Cfg)
	out := map[string]any{"pending": pending, "systemKey": pending && m.SystemKey}
	if pending {
		out["stagedAt"] = m.StagedAt
		out["stagedBy"] = m.StagedBy
	}
	return c.JSON(http.StatusOK, out)
}

// restoreDiscard cancels a staged restore (the staged files are deleted).
func (h *handler) restoreDiscard(c *echo.Context) error {
	pending, err := discardStagedRestore(h.d.Cfg)
	if err != nil {
		return err
	}
	if pending {
		h.d.Audit.Log(c, "admin.restore.discarded", "", nil)
	}
	return httpx.OK(c)
}
