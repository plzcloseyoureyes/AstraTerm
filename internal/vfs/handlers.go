package vfs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/core"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

type handler struct {
	d   *app.Deps
	c   *core.Core
	reg *Registry
}

// MountRegistry registers the files API (SPEC §6.0 "Files" + §9 files-backend) and the FILE-2 shell integration, and
// returns the handle registry (the transfer manager works on its handles).
func MountRegistry(d *app.Deps, c *core.Core) (*Registry, error) {
	reg := NewRegistry(d, c)
	h := &handler{d: d, c: c, reg: reg}
	api := d.Router.API()
	api.POST("/fs", h.open)
	api.GET("/fs", h.listHandles)
	api.POST("/fs/compare", h.compare)
	api.GET("/fs/:id", h.info)
	api.DELETE("/fs/:id", h.close)
	api.GET("/fs/:id/list", h.list)
	api.GET("/fs/:id/stat", h.stat)
	api.GET("/fs/:id/realpath", h.realpath)
	api.GET("/fs/:id/download", h.download)
	api.GET("/fs/:id/upload", h.uploadStatus)
	api.PUT("/fs/:id/upload", h.upload, httpx.BodyLimit(0))
	api.GET("/fs/:id/read", h.read)
	api.PUT("/fs/:id/write", h.write, httpx.BodyLimit(maxWriteBody))
	api.POST("/fs/:id/mkdir", h.mkdir)
	api.POST("/fs/:id/rename", h.rename)
	api.POST("/fs/:id/delete", h.delete)
	api.POST("/fs/:id/chmod", h.chmod)
	api.POST("/fs/:id/chown", h.chown)
	api.POST("/fs/:id/symlink", h.symlink)
	api.POST("/fs/:id/link", h.hardlink)
	api.POST("/fs/:id/touch", h.touch)
	api.POST("/fs/:id/copy", h.copy)
	api.POST("/fs/:id/checksum", h.checksum)
	api.POST("/fs/:id/search", h.search)
	api.POST("/fs/:id/archive", h.archive)
	api.POST("/fs/:id/extract", h.extract)
	api.POST("/fs/:id/presign", h.presign)
	api.GET("/fs/:id/space", h.space)
	api.GET("/fs/:id/cwd", h.cwd)

	if c != nil && c.Sessions != nil {
		c.Sessions.AddHooks(term.Hooks{OnClose: func(s *term.Session) { reg.closeSession(s.ID) }})
		startFollowCwd(d, c)
	}
	return reg, nil
}

// ---- helpers ------------------------------------------------------------------------------------------------------

// acquire pins the handle of the request (404 fs_not_found for unknown / foreign handles).
func (h *handler) acquire(c *echo.Context) (*Handle, func(), error) {
	return h.reg.Acquire(httpx.UserFrom(c), c.Param("id"))
}

// pathArg cleans a path argument against the handle's home.
func pathArg(hd *Handle, raw string) (string, error) {
	p, err := cleanPath(raw, hd.Home)
	if err != nil {
		return "", httpx.BadRequest(err.Error())
	}
	return p, nil
}

func requirePath(hd *Handle, raw, field string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", httpx.BadRequest(field + " is required")
	}
	return pathArg(hd, raw)
}

func (h *handler) audit(c *echo.Context, action, target string, details any) {
	if h.d != nil && h.d.Audit != nil {
		h.d.Audit.Log(c, action, target, details)
	}
}

// entry stats p (Lstat) and completes it for the response.
func entryFor(ctx context.Context, hd *Handle, p string) (*Entry, error) {
	e, err := hd.FS.Lstat(ctx, p)
	if err != nil {
		return nil, fsError(err, p)
	}
	finishEntries(ctx, hd.FS, []*Entry{e})
	return e, nil
}

// ---- handles ------------------------------------------------------------------------------------------------------

func (h *handler) open(c *echo.Context) error {
	var req OpenRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	hd, err := h.reg.Open(c.Request().Context(), httpx.UserFrom(c), req)
	if err != nil {
		return fsError(err, "")
	}
	return c.JSON(http.StatusCreated, hd)
}

func (h *handler) listHandles(c *echo.Context) error {
	list := h.reg.List(httpx.UserFrom(c))
	if list == nil {
		list = []*Handle{}
	}
	return c.JSON(http.StatusOK, list)
}

func (h *handler) info(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	return c.JSON(http.StatusOK, hd)
}

func (h *handler) close(c *echo.Context) error {
	if err := h.reg.Close(httpx.UserFrom(c), c.Param("id")); err != nil {
		return err
	}
	return httpx.OK(c)
}

// ---- reading ------------------------------------------------------------------------------------------------------

type listResult struct {
	Path    string   `json:"path"`
	Parent  string   `json:"parent"`
	Entries []*Entry `json:"entries"`
}

func (h *handler) list(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	p, err := pathArg(hd, c.QueryParam("path"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	entries, err := ListDir(ctx, hd.FS, p)
	if err != nil {
		return fsError(err, p)
	}
	finishEntries(ctx, hd.FS, entries)
	sortEntries(entries)
	if entries == nil {
		entries = []*Entry{}
	}
	return c.JSON(http.StatusOK, listResult{Path: p, Parent: parentDir(p), Entries: entries})
}

func (h *handler) stat(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	p, err := pathArg(hd, c.QueryParam("path"))
	if err != nil {
		return err
	}
	e, err := entryFor(c.Request().Context(), hd, p)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, e)
}

func (h *handler) realpath(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	p, err := pathArg(hd, c.QueryParam("path"))
	if err != nil {
		return err
	}
	rp, err := hd.FS.Realpath(c.Request().Context(), p)
	if err != nil {
		return fsError(err, p)
	}
	return c.JSON(http.StatusOK, map[string]string{"path": rp})
}

// Editor reads (FILE-10).
const (
	defaultReadMax = 5 << 20
	maxReadMax     = 64 << 20
	maxWriteBody   = 96 << 20
)

type readResult struct {
	Path     string    `json:"path"`
	Content  string    `json:"content"`
	Encoding string    `json:"encoding"`
	Size     int64     `json:"size"`
	Mtime    time.Time `json:"mtime"`
	Mode     uint32    `json:"mode"`
}

func tooLarge(c *echo.Context, size int64, limit int64) error {
	return c.JSON(http.StatusRequestEntityTooLarge, map[string]any{"error": fmt.Sprintf("file is too large (%d bytes, limit %d)", size, limit),
		"code": codeTooLarge, "size": size})
}

func (h *handler) read(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	p, err := requirePath(hd, c.QueryParam("path"), "path")
	if err != nil {
		return err
	}
	limit := int64(defaultReadMax)
	if v := c.QueryParam("maxBytes"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return httpx.BadRequest("invalid maxBytes")
		}
		limit = min(max(n, 1), maxReadMax)
	}
	ctx := c.Request().Context()
	st, err := hd.FS.Stat(ctx, p)
	if err != nil {
		return fsError(err, p)
	}
	if err := readable(st, p); err != nil {
		return err
	}
	if st.Size > limit {
		return tooLarge(c, st.Size, limit)
	}
	r, err := hd.FS.Open(ctx, p, 0)
	if err != nil {
		return fsError(err, p)
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	r.Close()
	if err != nil {
		return fsError(err, p)
	}
	if int64(len(data)) > limit {
		return tooLarge(c, int64(len(data)), limit)
	}
	res := readResult{Path: p, Size: int64(len(data)), Mtime: st.Mtime, Mode: st.Mode}
	if utf8.Valid(data) && !containsNUL(data) {
		res.Content, res.Encoding = string(data), "utf-8"
	} else {
		res.Content, res.Encoding = base64.StdEncoding.EncodeToString(data), "base64"
	}
	return c.JSON(http.StatusOK, res)
}

func containsNUL(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}

type writeRequest struct {
	Path        string     `json:"path"`
	Content     string     `json:"content"`
	Encoding    string     `json:"encoding"`
	ExpectMtime *time.Time `json:"expectMtime,omitempty"`
	ExpectSize  *int64     `json:"expectSize,omitempty"`
	Sudo        bool       `json:"sudo,omitempty"`
}

func (h *handler) write(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	var req writeRequest
	if err := httpx.BindLimit(c, &req, maxWriteBody); err != nil {
		return err
	}
	p, err := requirePath(hd, req.Path, "path")
	if err != nil {
		return err
	}
	var data []byte
	switch strings.ToLower(req.Encoding) {
	case "", "utf-8", "utf8", "text":
		data = []byte(req.Content)
	case "base64":
		if data, err = base64.StdEncoding.DecodeString(req.Content); err != nil {
			return httpx.BadRequest("invalid base64 content")
		}
	default:
		return httpx.BadRequest("encoding must be utf-8 or base64")
	}
	ctx := c.Request().Context()
	if req.ExpectMtime != nil || req.ExpectSize != nil {
		cur, err := hd.FS.Stat(ctx, p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return c.JSON(http.StatusConflict, map[string]any{"error": "the file was deleted on the server", "code": model.CodeConflict})
		case err != nil:
			return fsError(err, p)
		}
		changed := req.ExpectMtime != nil && !cur.Mtime.Truncate(time.Second).Equal(req.ExpectMtime.Truncate(time.Second))
		if req.ExpectSize != nil && cur.Size != *req.ExpectSize {
			changed = true
		}
		if changed {
			return c.JSON(http.StatusConflict, map[string]any{"error": "the file was changed on the server",
				"code": model.CodeConflict, "mtime": cur.Mtime, "size": cur.Size})
		}
	}
	strategy, err := writeFileSafe(ctx, hd.FS, p, data)
	usedSudo := false
	if err != nil && errors.Is(err, fs.ErrPermission) && req.Sudo {
		sw, ok := hd.FS.(SudoWriter)
		if !ok {
			return httpx.NewError(http.StatusBadRequest, codeNotSupported, "sudo is not available on this file system")
		}
		// sudo tee rewrites in place: inode, owner, mode, ACLs and labels of system files stay.
		err, usedSudo, strategy = sw.SudoWrite(ctx, p, data), true, writeInPlace
		if err == nil {
			// The unprivileged view may not be allowed to stat it; only a visible size mismatch is an error.
			if st, serr := hd.FS.Stat(ctx, p); serr == nil && st.Size != int64(len(data)) {
				err = fmt.Errorf("the saved file has %d bytes instead of %d", st.Size, len(data))
			}
		}
	}
	if err != nil {
		return fsError(err, p)
	}
	h.audit(c, "fs.write", p, map[string]any{"fs": hd.ID, "host": hd.Host, "size": len(data), "sudo": usedSudo,
		"strategy": strategy})
	// Like read and the expectMtime check, the reply describes the file actually written (a symlink is followed):
	// its mtime is what the next conflict check compares against.
	e, err := targetEntryFor(ctx, hd, p)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, e)
}

// readable refuses to open anything but a regular file for reading: a folder, and devices, FIFOs and sockets (opening
// a FIFO blocks until a writer appears — on SFTP it would stall the whole single-threaded sftp-server —, /dev/zero
// never ends).
func readable(e *Entry, p string) error {
	switch e.Type {
	case model.FileTypeDir:
		return httpx.NewError(http.StatusBadRequest, codeFSError, "is a directory: "+p)
	case model.FileTypeOther:
		return httpx.NewError(http.StatusBadRequest, codeFSError, "not a regular file (device, FIFO or socket): "+p)
	}
	return nil
}

// targetEntryFor stats p following symlinks (the entry keeps the requested path and name).
func targetEntryFor(ctx context.Context, hd *Handle, p string) (*Entry, error) {
	e, err := hd.FS.Stat(ctx, p)
	if err != nil {
		return nil, fsError(err, p)
	}
	e.Path, e.Name = p, baseName(p)
	finishEntries(ctx, hd.FS, []*Entry{e})
	return e, nil
}

// ---- mutations ----------------------------------------------------------------------------------------------------

func (h *handler) mkdir(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	var req struct {
		Path    string `json:"path"`
		Parents bool   `json:"parents"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	p, err := requirePath(hd, req.Path, "path")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if req.Parents {
		err = hd.FS.MkdirAll(ctx, p)
	} else {
		err = hd.FS.Mkdir(ctx, p)
	}
	if err != nil {
		return fsError(err, p)
	}
	h.audit(c, "fs.mkdir", p, map[string]any{"fs": hd.ID, "host": hd.Host})
	e, err := entryFor(ctx, hd, p)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, e)
}

func (h *handler) rename(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	var req struct {
		From      string `json:"from"`
		To        string `json:"to"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	from, err := requirePath(hd, req.From, "from")
	if err != nil {
		return err
	}
	to := strings.TrimSpace(req.To)
	if to == "" {
		return httpx.BadRequest("to is required")
	}
	if !strings.HasPrefix(to, "/") && !strings.HasPrefix(to, "~") {
		if !validName(to) {
			return httpx.BadRequest("invalid name")
		}
		to = joinPath(parentDir(from), to)
	}
	if to, err = pathArg(hd, to); err != nil {
		return err
	}
	if from == "/" || to == "/" {
		return httpx.BadRequest("cannot rename the root directory")
	}
	if isWithin(to, from) && to != from {
		return httpx.BadRequest("cannot move a folder into itself")
	}
	ctx := c.Request().Context()
	if to != from {
		if te, err := hd.FS.Lstat(ctx, to); err == nil {
			if !req.Overwrite || te.Type == model.FileTypeDir {
				return httpx.NewError(http.StatusConflict, codeExists, "file already exists: "+to)
			}
		}
		if err := hd.FS.Rename(ctx, from, to); err != nil {
			return fsError(err, from)
		}
		h.audit(c, "fs.rename", from, map[string]any{"fs": hd.ID, "host": hd.Host, "to": to})
	}
	e, err := entryFor(ctx, hd, to)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, e)
}

func (h *handler) delete(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	var req struct {
		Paths     []string `json:"paths"`
		Path      string   `json:"path"`
		Recursive bool     `json:"recursive"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if req.Path != "" {
		req.Paths = append(req.Paths, req.Path)
	}
	if len(req.Paths) == 0 {
		return httpx.BadRequest("paths is required")
	}
	ctx := c.Request().Context()
	var done []string
	for _, raw := range req.Paths {
		p, err := requirePath(hd, raw, "path")
		if err != nil {
			return err
		}
		if p == "/" {
			return httpx.BadRequest("refusing to delete the root directory")
		}
		e, err := hd.FS.Lstat(ctx, p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fsError(err, p)
		}
		if e.Type == model.FileTypeDir && req.Recursive {
			err = hd.FS.RemoveAll(ctx, p)
		} else {
			err = hd.FS.Remove(ctx, p)
		}
		if err != nil {
			if len(done) > 0 {
				h.audit(c, "fs.delete", done[0], map[string]any{"fs": hd.ID, "host": hd.Host, "paths": done, "recursive": req.Recursive})
			}
			return fsError(err, p)
		}
		done = append(done, p)
	}
	if len(done) > 0 {
		h.audit(c, "fs.delete", done[0], map[string]any{"fs": hd.ID, "host": hd.Host, "paths": done, "recursive": req.Recursive})
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "deleted": len(done)})
}

// modeArg accepts a JSON number (permission bits) or a string (octal or symbolic).
type modeArg struct {
	spec modeSpec
	set  bool
}

func (m *modeArg) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err == nil {
		v, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil || v < 0 || v > 0o7777 {
			return fmt.Errorf("mode must be between 0 and 0o7777")
		}
		m.spec, m.set = modeSpec{abs: true, perm: uint32(v)}, true
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("mode must be a number or a string")
	}
	spec, err := parseMode(s)
	if err != nil {
		return err
	}
	m.spec, m.set = spec, true
	return nil
}

func (h *handler) chmod(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	var req struct {
		Paths     []string `json:"paths"`
		Path      string   `json:"path"`
		Mode      modeArg  `json:"mode"`
		Recursive bool     `json:"recursive"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if req.Path != "" {
		req.Paths = append(req.Paths, req.Path)
	}
	if len(req.Paths) == 0 || !req.Mode.set {
		return httpx.BadRequest("paths and mode are required")
	}
	ctx := c.Request().Context()
	for _, raw := range req.Paths {
		p, err := requirePath(hd, raw, "path")
		if err != nil {
			return err
		}
		if err := chmodTree(ctx, hd.FS, p, req.Mode.spec, req.Recursive); err != nil {
			return fsError(err, p)
		}
	}
	h.audit(c, "fs.chmod", req.Paths[0], map[string]any{"fs": hd.ID, "host": hd.Host, "paths": req.Paths, "recursive": req.Recursive})
	return httpx.OK(c)
}

func (h *handler) chown(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	var req struct {
		Paths     []string `json:"paths"`
		Path      string   `json:"path"`
		UID       *int     `json:"uid"`
		GID       *int     `json:"gid"`
		Owner     string   `json:"owner"`
		Group     string   `json:"group"`
		Recursive bool     `json:"recursive"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if req.Path != "" {
		req.Paths = append(req.Paths, req.Path)
	}
	if len(req.Paths) == 0 {
		return httpx.BadRequest("paths is required")
	}
	ctx := c.Request().Context()
	uid, gid := -1, -1
	if req.UID != nil {
		uid = *req.UID
	}
	if req.GID != nil {
		gid = *req.GID
	}
	if req.Owner != "" {
		if uid, err = resolveID(ctx, hd.FS, true, req.Owner); err != nil {
			return err
		}
	}
	if req.Group != "" {
		if gid, err = resolveID(ctx, hd.FS, false, req.Group); err != nil {
			return err
		}
	}
	if uid < 0 && gid < 0 {
		return httpx.BadRequest("uid, gid, owner or group is required")
	}
	for _, raw := range req.Paths {
		p, err := requirePath(hd, raw, "path")
		if err != nil {
			return err
		}
		if err := chownTree(ctx, hd.FS, p, uid, gid, req.Recursive); err != nil {
			return fsError(err, p)
		}
	}
	h.audit(c, "fs.chown", req.Paths[0], map[string]any{"fs": hd.ID, "host": hd.Host, "paths": req.Paths, "uid": uid, "gid": gid,
		"recursive": req.Recursive})
	return httpx.OK(c)
}

// resolveID maps a user / group name (or numeric string) to its id on the file system's host.
func resolveID(ctx context.Context, fsys FS, user bool, name string) (int, error) {
	name = strings.TrimSpace(name)
	if n, err := strconv.Atoi(name); err == nil && n >= 0 {
		return n, nil
	}
	kind := "group"
	if user {
		kind = "user"
	}
	if strings.ContainsAny(name, ":/\n\x00 ") || len(name) > 64 {
		return -1, httpx.BadRequest("invalid " + kind + " name")
	}
	var oc *ownerCache
	switch f := fsys.(type) {
	case *sftpFS:
		oc = f.owners
	case *shellFS:
		oc = f.owners
	case *localFS:
		if id := localLookupName(user, name); id >= 0 {
			return id, nil
		}
		return -1, httpx.BadRequest("unknown " + kind + " " + name)
	}
	if oc != nil {
		if id := oc.lookupName(user, name); id >= 0 {
			return id, nil
		}
	}
	x, ok := fsys.(Execer)
	if !ok {
		return -1, httpx.BadRequest("names cannot be resolved on this file system; use numeric ids")
	}
	db := "group"
	if user {
		db = "passwd"
	}
	out, err := runShell(ctx, x, "getent "+db+" "+shq(name)+" 2>/dev/null || grep -E "+shq("^"+regexpQuote(name)+":")+" /etc/"+db, nil)
	if err != nil {
		if errors.Is(err, ErrNotSupported) {
			return -1, httpx.BadRequest("names cannot be resolved on this server; use numeric ids")
		}
		return -1, httpx.BadRequest("unknown " + kind + " " + name)
	}
	f := strings.Split(strings.TrimSpace(firstLine(string(out))), ":")
	if len(f) >= 3 {
		if n, err := strconv.Atoi(f[2]); err == nil {
			return n, nil
		}
	}
	return -1, httpx.BadRequest("unknown " + kind + " " + name)
}

func regexpQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (h *handler) symlink(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	var req struct {
		Target string `json:"target"`
		Link   string `json:"link"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if req.Target == "" || strings.IndexByte(req.Target, 0) >= 0 || len(req.Target) > maxPathLen {
		return httpx.BadRequest("target is required")
	}
	link, err := requirePath(hd, req.Link, "link")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := hd.FS.Symlink(ctx, req.Target, link); err != nil {
		return fsError(err, link)
	}
	h.audit(c, "fs.symlink", link, map[string]any{"fs": hd.ID, "host": hd.Host, "target": req.Target})
	e, err := entryFor(ctx, hd, link)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, e)
}

func (h *handler) hardlink(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	var req struct {
		Target string `json:"target"`
		Link   string `json:"link"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	target, err := requirePath(hd, req.Target, "target")
	if err != nil {
		return err
	}
	link, err := requirePath(hd, req.Link, "link")
	if err != nil {
		return err
	}
	l, ok := hd.FS.(Linker)
	if !ok {
		return fsError(ErrNotSupported, link)
	}
	ctx := c.Request().Context()
	if err := l.Link(ctx, target, link); err != nil {
		return fsError(err, link)
	}
	h.audit(c, "fs.link", link, map[string]any{"fs": hd.ID, "host": hd.Host, "target": target})
	e, err := entryFor(ctx, hd, link)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, e)
}

func (h *handler) touch(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	var req struct {
		Path  string     `json:"path"`
		Mtime *time.Time `json:"mtime"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	p, err := requirePath(hd, req.Path, "path")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	now := time.Now()
	if req.Mtime != nil {
		now = *req.Mtime
	}
	if _, err := hd.FS.Lstat(ctx, p); errors.Is(err, fs.ErrNotExist) {
		w, err := hd.FS.Create(ctx, p, 0)
		if err != nil {
			return fsError(err, p)
		}
		if err := w.Close(); err != nil {
			return fsError(err, p)
		}
		h.audit(c, "fs.create", p, map[string]any{"fs": hd.ID, "host": hd.Host})
		if req.Mtime != nil {
			_ = hd.FS.Chtimes(ctx, p, now, now)
		}
	} else if err != nil {
		return fsError(err, p)
	} else if err := hd.FS.Chtimes(ctx, p, now, now); err != nil && !errors.Is(err, ErrNotSupported) {
		return fsError(err, p)
	}
	e, err := entryFor(ctx, hd, p)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, e)
}

func (h *handler) copy(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	var req struct {
		From      []string `json:"from"`
		ToDir     string   `json:"toDir"`
		Overwrite bool     `json:"overwrite"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if len(req.From) == 0 {
		return httpx.BadRequest("from is required")
	}
	toDir, err := requirePath(hd, req.ToDir, "toDir")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if d, err := hd.FS.Stat(ctx, toDir); err != nil {
		return fsError(err, toDir)
	} else if d.Type != model.FileTypeDir {
		return httpx.BadRequest("toDir is not a folder")
	}
	var out []*Entry
	for _, raw := range req.From {
		src, err := requirePath(hd, raw, "from")
		if err != nil {
			return err
		}
		if src == "/" {
			return httpx.BadRequest("cannot copy the root directory")
		}
		se, err := hd.FS.Lstat(ctx, src)
		if err != nil {
			return fsError(err, src)
		}
		name := baseName(src)
		dst := joinPath(toDir, name)
		if isWithin(toDir, src) && se.Type == model.FileTypeDir {
			return httpx.BadRequest("cannot copy a folder into itself")
		}
		if de, err := hd.FS.Lstat(ctx, dst); err == nil {
			switch {
			case dst == src || parentDir(src) == toDir:
				if name, err = uniqueName(ctx, hd.FS, toDir, name, "copy"); err != nil {
					return fsError(err, dst)
				}
				dst = joinPath(toDir, name)
			case !req.Overwrite || de.Type == model.FileTypeDir || se.Type == model.FileTypeDir:
				return httpx.NewError(http.StatusConflict, codeExists, "file already exists: "+dst)
			}
		}
		if sc, ok := hd.FS.(ServerSideCopier); ok {
			err = sc.CopyServerSide(ctx, src, dst)
			if errors.Is(err, ErrNotSupported) {
				err = copyTree(ctx, hd.FS, src, dst)
			}
		} else {
			err = copyTree(ctx, hd.FS, src, dst)
		}
		if err != nil {
			return fsError(err, src)
		}
		h.audit(c, "fs.copy", src, map[string]any{"fs": hd.ID, "host": hd.Host, "to": dst})
		if e, err := entryFor(ctx, hd, dst); err == nil {
			out = append(out, e)
		}
	}
	if out == nil {
		out = []*Entry{}
	}
	return c.JSON(http.StatusOK, map[string]any{"entries": out})
}

func (h *handler) checksum(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	var req struct {
		Path string `json:"path"`
		Algo string `json:"algo"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	p, err := requirePath(hd, req.Path, "path")
	if err != nil {
		return err
	}
	algo := strings.ToLower(strings.ReplaceAll(req.Algo, "-", ""))
	if algo == "" {
		algo = "sha256"
	}
	if _, err := newHash(algo); err != nil {
		return httpx.BadRequest(err.Error())
	}
	ctx := c.Request().Context()
	if e, err := hd.FS.Stat(ctx, p); err != nil {
		return fsError(err, p)
	} else if err := readable(e, p); err != nil {
		return err
	}
	sum, err := checksum(ctx, hd.FS, p, algo)
	if err != nil {
		return fsError(err, p)
	}
	return c.JSON(http.StatusOK, map[string]string{"path": p, "algo": algo, "hash": sum})
}

func (h *handler) search(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	var req struct {
		Path       string `json:"path"`
		Pattern    string `json:"pattern"`
		Content    string `json:"content"`
		MaxResults int    `json:"maxResults"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	p, err := pathArg(hd, req.Path)
	if err != nil {
		return err
	}
	if len(req.Pattern) > 1024 || len(req.Content) > 1024 {
		return httpx.BadRequest("pattern too long")
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Minute)
	defer cancel()
	entries, more, err := search(ctx, hd.FS, p, req.Pattern, req.Content, req.MaxResults)
	if err != nil {
		return fsError(err, p)
	}
	finishEntries(ctx, hd.FS, entries)
	if entries == nil {
		entries = []*Entry{}
	}
	return c.JSON(http.StatusOK, map[string]any{"entries": entries, "truncated": more})
}

func (h *handler) archive(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	var req struct {
		Paths     []string `json:"paths"`
		Dest      string   `json:"dest"`
		Format    string   `json:"format"`
		Overwrite bool     `json:"overwrite"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if len(req.Paths) == 0 {
		return httpx.BadRequest("paths is required")
	}
	paths := make([]string, 0, len(req.Paths))
	for _, raw := range req.Paths {
		p, err := requirePath(hd, raw, "paths")
		if err != nil {
			return err
		}
		if p == "/" {
			return httpx.BadRequest("cannot archive the root directory")
		}
		paths = append(paths, p)
	}
	format := strings.ToLower(req.Format)
	if format == "" {
		format = "zip"
	}
	dest := strings.TrimSpace(req.Dest)
	if dest == "" {
		ext := ".zip"
		if format != "zip" {
			ext = ".tar.gz"
		}
		name := baseName(paths[0])
		if len(paths) > 1 {
			name = "archive"
		}
		dest = joinPath(parentDir(paths[0]), name+ext)
	} else if !strings.HasPrefix(dest, "/") && !strings.HasPrefix(dest, "~") {
		dest = joinPath(parentDir(paths[0]), dest)
	}
	if dest, err = pathArg(hd, dest); err != nil {
		return err
	}
	ctx := c.Request().Context()
	if _, err := hd.FS.Lstat(ctx, dest); err == nil && !req.Overwrite {
		return httpx.NewError(http.StatusConflict, codeExists, "file already exists: "+dest)
	}
	if err := createArchive(ctx, hd.FS, paths, dest, format); err != nil {
		return fsError(err, dest)
	}
	h.audit(c, "fs.archive", dest, map[string]any{"fs": hd.ID, "host": hd.Host, "paths": paths, "format": format})
	e, err := entryFor(ctx, hd, dest)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, e)
}

func (h *handler) extract(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	var req struct {
		Path    string `json:"path"`
		DestDir string `json:"destDir"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	p, err := requirePath(hd, req.Path, "path")
	if err != nil {
		return err
	}
	dest := parentDir(p)
	if strings.TrimSpace(req.DestDir) != "" {
		if dest, err = pathArg(hd, req.DestDir); err != nil {
			return err
		}
	}
	if archiveKind(p) == "" {
		return httpx.BadRequest("unknown archive type (zip, tar, tar.gz, tgz, tar.bz2, tar.xz, gz, 7z, rar)")
	}
	ctx := c.Request().Context()
	if err := extractArchive(ctx, hd.FS, p, dest, h.reg.tmpDir()); err != nil {
		return fsError(err, p)
	}
	h.audit(c, "fs.extract", p, map[string]any{"fs": hd.ID, "host": hd.Host, "destDir": dest})
	return httpx.OK(c)
}

func (h *handler) presign(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	var req struct {
		Path       string `json:"path"`
		ExpiresSec int    `json:"expiresSec"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	p, err := requirePath(hd, req.Path, "path")
	if err != nil {
		return err
	}
	ps, ok := hd.FS.(Presigner)
	if !ok {
		return fsError(ErrNotSupported, p)
	}
	exp := time.Duration(req.ExpiresSec) * time.Second
	if exp <= 0 {
		exp = time.Hour
	}
	exp = min(exp, 7*24*time.Hour)
	u, err := ps.Presign(c.Request().Context(), p, exp)
	if err != nil {
		return fsError(err, p)
	}
	h.audit(c, "fs.presign", p, map[string]any{"fs": hd.ID, "host": hd.Host, "expiresSec": int(exp.Seconds())})
	return c.JSON(http.StatusOK, map[string]any{"url": u, "expiresAt": time.Now().Add(exp).UTC()})
}

func (h *handler) space(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	p, err := pathArg(hd, c.QueryParam("path"))
	if err != nil {
		return err
	}
	si, ok := hd.FS.(SpaceInfo)
	if !ok {
		return fsError(ErrNotSupported, p)
	}
	sp, err := si.Space(c.Request().Context(), p)
	if err != nil {
		return fsError(err, p)
	}
	return c.JSON(http.StatusOK, sp)
}

// cwd reports the working directory of the handle's (or another own) terminal session (FILE-2 polling fallback).
func (h *handler) cwd(c *echo.Context) error {
	hd, release, err := h.acquire(c)
	if err != nil {
		return err
	}
	defer release()
	sid := c.QueryParam("sessionId")
	if sid == "" {
		sid = hd.SessionID
	}
	if sid == "" || h.c == nil || h.c.Sessions == nil {
		return c.JSON(http.StatusOK, map[string]string{"path": ""})
	}
	s := h.c.Sessions.Get(sid)
	if s == nil || s.OwnerID != httpx.UserFrom(c).ID {
		return httpx.NotFound("session not found")
	}
	return c.JSON(http.StatusOK, map[string]string{"path": s.Cwd(), "sessionId": sid})
}
