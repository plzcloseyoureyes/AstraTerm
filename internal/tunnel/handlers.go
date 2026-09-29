package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v5"

	"github.com/nexterm/nexterm/internal/app"
	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
)

type handler struct {
	d *app.Deps
	m *Manager
}

func (h *handler) mount() {
	api := h.d.Router.API()
	api.GET("/tunnels", h.list)
	api.POST("/tunnels", h.create)
	api.POST("/tunnels/start-all", h.startAll)
	api.POST("/tunnels/stop-all", h.stopAll)
	api.POST("/tunnels/reorder", h.reorder)
	api.POST("/tunnels/check-bind", h.checkBind)
	api.GET("/tunnels/export", h.export)
	api.POST("/tunnels/import", h.importTunnels)
	api.GET("/tunnels/remote-ports", h.remotePorts)
	api.GET("/tunnels/session-forwards", h.listSessionForwards)
	api.POST("/tunnels/session-forwards", h.addSessionForward)
	api.DELETE("/tunnels/session-forwards/:id", h.removeSessionForward)
	api.GET("/tunnels/:id", h.get)
	api.PATCH("/tunnels/:id", h.update)
	api.DELETE("/tunnels/:id", h.remove)
	api.POST("/tunnels/:id/start", h.start)
	api.POST("/tunnels/:id/stop", h.stop)
	api.POST("/tunnels/:id/restart", h.restart)
	api.POST("/tunnels/:id/duplicate", h.duplicate)
}

// ---- helpers ------------------------------------------------------------------------------------------------------

// own loads a tunnel of the caller (others' tunnels are reported as not found).
func (h *handler) own(ctx context.Context, u *model.User, id string) (*record, error) {
	if !model.ValidID(id) {
		return nil, httpx.ErrNotFound
	}
	rec, err := h.m.repo.get(ctx, id)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, httpx.ErrNotFound
		}
		return nil, err
	}
	if rec.OwnerID != u.ID {
		return nil, httpx.ErrNotFound
	}
	return rec, nil
}

// sshConnection loads an SSH connection visible to u (the tunnel's transport).
func (h *handler) sshConnection(ctx context.Context, u *model.User, id string) (*model.Connection, error) {
	if id == "" {
		return nil, httpx.BadRequest("connectionId is required")
	}
	c, err := h.d.Store.Connections.Get(ctx, id)
	if err != nil || !app.Visible(u, c.OwnerID, c.Shared) {
		return nil, httpx.BadRequest("SSH connection not found")
	}
	switch c.Protocol {
	case model.ProtoSSH, model.ProtoSFTP, model.ProtoMosh:
	default:
		return nil, httpx.BadRequest(fmt.Sprintf("%q is not an SSH connection", c.Name))
	}
	return c, nil
}

func connRef(c *model.Connection) *ConnRef {
	if c == nil {
		return nil
	}
	port := c.Port
	if port == 0 {
		port = model.DefaultPort(c.Protocol)
	}
	return &ConnRef{ID: c.ID, Name: c.Name, Protocol: string(c.Protocol), Host: c.Host, Port: port, Username: c.Username}
}

func (h *handler) view(rec *record, conn *model.Connection) View {
	opts := rec.Options
	d := rec.def()
	_ = normalizeOptions(&opts, d.kind()) // fill defaults for display (stored options were validated on save)
	keys := rec.SecretKeys
	if keys == nil {
		keys = []string{}
	}
	return View{
		ID: rec.ID, Name: rec.Name, Type: rec.Type, ConnectionID: rec.ConnectionID,
		BindHost: rec.BindHost, BindPort: rec.BindPort, DestHost: rec.DestHost, DestPort: rec.DestPort,
		AutoStart: rec.AutoStart, Status: h.m.status(rec.ID), CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt,
		Options: opts, SortOrder: rec.SortOrder, SecretKeys: keys, Connection: connRef(conn),
	}
}

// viewOne builds the view of one record (looking its connection up). Callers are the tunnel's owner, so the
// connection is described only while the owner may see it (app.Visible), like in list.
func (h *handler) viewOne(ctx context.Context, rec *record) View {
	c, err := h.d.Store.Connections.Get(ctx, rec.ConnectionID)
	if err != nil || !app.Visible(&model.User{ID: rec.OwnerID}, c.OwnerID, c.Shared) {
		c = nil
	}
	return h.view(rec, c)
}

func checkName(name string) (string, error) {
	name = strings.TrimSpace(name)
	n := utf8.RuneCountInString(name)
	if n == 0 {
		return "", httpx.BadRequest("name is required")
	}
	if n > maxNameLen || strings.ContainsFunc(name, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return "", httpx.BadRequest(fmt.Sprintf("name must be at most %d characters without control characters", maxNameLen))
	}
	return name, nil
}

// validate normalizes a record in place (definition, options, name, connection) and checks the run-mode policy.
func (h *handler) validate(ctx context.Context, u *model.User, rec *record) error {
	name, err := checkName(rec.Name)
	if err != nil {
		return err
	}
	rec.Name = name
	d := rec.def()
	if err := d.normalize(); err != nil {
		return err
	}
	rec.Type, rec.BindHost, rec.BindPort, rec.DestHost, rec.DestPort = d.Type, d.BindHost, d.BindPort, d.DestHost, d.DestPort
	rec.Options.Reverse, rec.Options.BindSocket, rec.Options.DestSocket = d.Reverse, d.BindSocket, d.DestSocket
	if err := normalizeOptions(&rec.Options, d.kind()); err != nil {
		return err
	}
	if _, err := h.sshConnection(ctx, u, rec.ConnectionID); err != nil {
		return err
	}
	if rec.Options.SocksUsername != "" && !hasKey(rec.SecretKeys, secretSocksPassword) {
		return httpx.BadRequest("a SOCKS password is required with a SOCKS username")
	}
	// Policy / proxy exposure checks on the would-be runtime spec (the password value does not matter here).
	fake := map[string]string{}
	if rec.Options.SocksUsername != "" {
		fake[secretSocksPassword] = "x"
	}
	sp, err := buildSpec(d, rec.Options, fake)
	if err != nil {
		return err
	}
	return h.m.policy(u, sp)
}

func hasKey(keys []string, k string) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

// applySecrets merges a write-only secrets patch (omitted = unchanged, "" = delete) into the sealed secrets.
func (h *handler) applySecrets(rec *record, patch map[string]string) error {
	if len(patch) == 0 {
		return nil
	}
	if err := checkSecrets(patch); err != nil {
		return err
	}
	cur, err := h.d.Vault.OpenJSON(rec.SecretsEnc)
	if err != nil {
		if errors.Is(err, model.ErrLocked) {
			return httpx.ErrLocked
		}
		return err
	}
	for k, v := range patch {
		if v == "" {
			delete(cur, k)
		} else {
			cur[k] = v
		}
	}
	enc, err := h.d.Vault.SealJSON(cur)
	if err != nil {
		if errors.Is(err, model.ErrLocked) {
			return httpx.ErrLocked
		}
		return err
	}
	rec.SecretsEnc, rec.SecretKeys = enc, model.SortedKeys(cur)
	return nil
}

// startTunnel pre-checks the connection (so a locked vault is reported as 423) and starts the tunnel. Failures other
// than a locked vault are also kept as the tunnel's error status.
func (h *handler) startTunnel(ctx context.Context, u *model.User, rec *record) error {
	if _, _, err := h.d.ResolveConnection(ctx, u, rec.ConnectionID); err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			err = httpx.BadRequest("the tunnel's SSH connection no longer exists")
		}
		if !errors.Is(err, httpx.ErrLocked) {
			h.m.recordFailure(u, rec, err)
		}
		return err
	}
	if err := h.m.start(ctx, u, rec, false); err != nil {
		if !errors.Is(err, httpx.ErrLocked) {
			h.m.recordFailure(u, rec, err)
		}
		return err
	}
	return nil
}

func auditDetails(rec *record) map[string]any {
	d := map[string]any{"name": rec.Name, "type": rec.Type, "connectionId": rec.ConnectionID}
	sp := &spec{kind: rec.def().kind(), bindHost: rec.BindHost, bindPort: rec.BindPort, bindSocket: rec.Options.BindSocket,
		destHost: rec.DestHost, destPort: rec.DestPort, destSocket: rec.Options.DestSocket}
	d["forward"] = sp.label()
	return d
}

// ---- CRUD ---------------------------------------------------------------------------------------------------------

func (h *handler) list(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	recs, err := h.m.repo.listByOwner(ctx, u.ID)
	if err != nil {
		return err
	}
	conns := map[string]*model.Connection{}
	if len(recs) > 0 {
		visible, err := h.d.Store.Connections.ListVisible(ctx, u.ID)
		if err != nil {
			return err
		}
		for _, c := range visible {
			conns[c.ID] = c
		}
	}
	out := make([]View, 0, len(recs))
	for _, rec := range recs {
		out = append(out, h.view(rec, conns[rec.ConnectionID]))
	}
	return c.JSON(http.StatusOK, out)
}

func (h *handler) get(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	rec, err := h.own(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, h.viewOne(ctx, rec))
}

func (h *handler) create(c *echo.Context) error {
	var in Input
	if err := httpx.BindLimit(c, &in, 256<<10); err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	n, err := h.m.repo.countByOwner(ctx, u.ID)
	if err != nil {
		return err
	}
	if n >= maxTunnelsPerUser {
		return httpx.Conflict(fmt.Sprintf("at most %d tunnels per user", maxTunnelsPerUser))
	}
	rec := &record{
		Tunnel: &model.Tunnel{Name: in.Name, Type: in.Type, ConnectionID: in.ConnectionID, BindHost: in.BindHost,
			BindPort: in.BindPort, DestHost: in.DestHost, DestPort: in.DestPort, AutoStart: in.AutoStart, OwnerID: u.ID},
		meta: meta{Options: in.Options, SortOrder: in.SortOrder, SecretKeys: []string{}},
	}
	if err := h.applySecrets(rec, in.Secrets); err != nil {
		return err
	}
	if err := h.validate(ctx, u, rec); err != nil {
		return err
	}
	if in.SortOrder == 0 {
		if mx, err := h.m.repo.maxSortOrder(ctx, u.ID); err == nil {
			rec.SortOrder = mx + 1
		}
	}
	if err := h.m.repo.create(ctx, rec); err != nil {
		return err
	}
	h.d.Audit.Log(c, "tunnel.create", rec.ID, auditDetails(rec))
	h.m.publishChange(u.ID, rec.ID, "created")
	if in.Start {
		// The tunnel exists either way: a failed start is reported in its status (a locked vault leaves it stopped).
		if err := h.startTunnel(ctx, u, rec); err == nil {
			h.d.Audit.Log(c, "tunnel.start", rec.ID, auditDetails(rec))
		}
	}
	return c.JSON(http.StatusCreated, h.viewOne(ctx, rec))
}

func (h *handler) update(c *echo.Context) error {
	var p map[string]json.RawMessage
	if err := httpx.BindLimit(c, &p, 256<<10); err != nil {
		return err
	}
	if p == nil {
		return httpx.BadRequest("request body must be a JSON object")
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	rec, err := h.own(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	before := *rec.Tunnel
	beforeOpts, _ := json.Marshal(rec.Options)
	var secrets map[string]string
	fields := map[string]any{
		"name": &rec.Name, "type": &rec.Type, "connectionId": &rec.ConnectionID, "bindHost": &rec.BindHost,
		"bindPort": &rec.BindPort, "destHost": &rec.DestHost, "destPort": &rec.DestPort, "autoStart": &rec.AutoStart,
		"options": &rec.Options, "sortOrder": &rec.SortOrder, "secrets": &secrets,
	}
	for k, raw := range p {
		dst, ok := fields[k]
		if !ok {
			continue // read-only / unknown fields (id, status, createdAt…) are ignored
		}
		if strings.TrimSpace(string(raw)) == "null" {
			switch v := dst.(type) {
			case *Options:
				*v = Options{}
			case *map[string]string:
				*v = nil
			case *string:
				*v = ""
			default:
				return httpx.BadRequest(fmt.Sprintf("field %q must not be null", k))
			}
			continue
		}
		if k == "options" {
			rec.Options = Options{} // options are replaced as a whole
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return httpx.BadRequest(fmt.Sprintf("invalid value for field %q", k))
		}
	}
	if err := h.applySecrets(rec, secrets); err != nil {
		return err
	}
	if err := h.validate(ctx, u, rec); err != nil {
		return err
	}
	if err := h.m.repo.update(ctx, rec); err != nil {
		return err
	}
	afterOpts, _ := json.Marshal(rec.Options)
	changed := before.Type != rec.Type || before.ConnectionID != rec.ConnectionID || before.BindHost != rec.BindHost ||
		before.BindPort != rec.BindPort || before.DestHost != rec.DestHost || before.DestPort != rec.DestPort ||
		string(beforeOpts) != string(afterOpts) || len(secrets) > 0
	keys := make([]string, 0, len(p))
	for k := range p {
		if _, ok := fields[k]; ok {
			keys = append(keys, k)
		}
	}
	details := auditDetails(rec)
	details["fields"] = keys
	h.d.Audit.Log(c, "tunnel.update", rec.ID, details)
	// A running tunnel whose forwarding definition changed is restarted with the new settings.
	if changed && h.m.running(rec.ID) {
		h.m.stop(rec.ID)
		_ = h.startTunnel(ctx, u, rec) // failures are reported in the tunnel's status
	}
	h.m.publishChange(u.ID, rec.ID, "updated")
	return c.JSON(http.StatusOK, h.viewOne(ctx, rec))
}

func (h *handler) remove(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	rec, err := h.own(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	h.m.stop(rec.ID)
	if err := h.m.repo.delete(ctx, rec.ID); err != nil {
		return err
	}
	// Stop again: a start that passed its ownership check before the row was deleted may have registered a runner
	// meanwhile (forgetting it instead would leave its listener running unsupervised). A start that registers even
	// later is stopped by the reconciler, which finds the row gone.
	h.m.stop(rec.ID)
	h.d.Audit.Log(c, "tunnel.delete", rec.ID, auditDetails(rec))
	h.d.Events.Publish(u.ID, Event{Type: model.EvTunnel, ID: rec.ID, Status: stoppedStatus(), Change: "deleted"})
	return httpx.OK(c)
}

func (h *handler) duplicate(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	src, err := h.own(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	n, err := h.m.repo.countByOwner(ctx, u.ID)
	if err != nil {
		return err
	}
	if n >= maxTunnelsPerUser {
		return httpx.Conflict(fmt.Sprintf("at most %d tunnels per user", maxTunnelsPerUser))
	}
	t := *src.Tunnel
	t.ID, t.AutoStart = "", false
	t.Name = truncateRunes(src.Name+" (copy)", maxNameLen)
	rec := &record{Tunnel: &t, meta: src.meta}
	rec.SecretKeys = append([]string{}, src.SecretKeys...)
	if mx, err := h.m.repo.maxSortOrder(ctx, u.ID); err == nil {
		rec.SortOrder = mx + 1
	}
	if err := h.m.repo.create(ctx, rec); err != nil {
		return err
	}
	h.d.Audit.Log(c, "tunnel.duplicate", rec.ID, map[string]any{"source": src.ID, "name": rec.Name})
	h.m.publishChange(u.ID, rec.ID, "created")
	return c.JSON(http.StatusCreated, h.viewOne(ctx, rec))
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// ---- runtime ------------------------------------------------------------------------------------------------------

func (h *handler) start(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	rec, err := h.own(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	if err := h.startTunnel(ctx, u, rec); err != nil {
		return err
	}
	h.d.Audit.Log(c, "tunnel.start", rec.ID, auditDetails(rec))
	return c.JSON(http.StatusOK, h.viewOne(ctx, rec))
}

func (h *handler) stop(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	rec, err := h.own(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	was := h.m.running(rec.ID)
	h.m.stop(rec.ID)
	if was {
		h.d.Audit.Log(c, "tunnel.stop", rec.ID, auditDetails(rec))
	}
	return c.JSON(http.StatusOK, h.viewOne(ctx, rec))
}

func (h *handler) restart(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	rec, err := h.own(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	h.m.stop(rec.ID)
	if err := h.startTunnel(ctx, u, rec); err != nil {
		return err
	}
	h.d.Audit.Log(c, "tunnel.restart", rec.ID, auditDetails(rec))
	return c.JSON(http.StatusOK, h.viewOne(ctx, rec))
}

type idsRequest struct {
	IDs []string `json:"ids"`
}

type bulkFailure struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Error string `json:"error"`
}

// startAll starts the caller's tunnels (all, or the given ids) that are not running.
func (h *handler) startAll(c *echo.Context) error {
	var req idsRequest
	if err := httpx.BindOptional(c, &req); err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	recs, err := h.selectOwn(ctx, u, req.IDs)
	if err != nil {
		return err
	}
	started := 0
	failed := []bulkFailure{}
	for _, rec := range recs {
		if h.m.running(rec.ID) {
			continue
		}
		if err := h.startTunnel(ctx, u, rec); err != nil {
			if errors.Is(err, httpx.ErrLocked) {
				return err // the UI unlocks the vault and repeats the request (started tunnels are skipped)
			}
			_, _, msg := errorText(err)
			failed = append(failed, bulkFailure{ID: rec.ID, Name: rec.Name, Error: msg})
			continue
		}
		started++
	}
	if started > 0 {
		h.d.Audit.Log(c, "tunnel.start_all", "", map[string]any{"started": started, "failed": len(failed)})
	}
	return c.JSON(http.StatusOK, map[string]any{"started": started, "failed": failed})
}

// stopAll stops the caller's running tunnels (all, or the given ids).
func (h *handler) stopAll(c *echo.Context) error {
	var req idsRequest
	if err := httpx.BindOptional(c, &req); err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	recs, err := h.selectOwn(ctx, u, req.IDs)
	if err != nil {
		return err
	}
	stopped := 0
	for _, rec := range recs {
		if h.m.running(rec.ID) {
			stopped++
		}
		h.m.stop(rec.ID)
	}
	if stopped > 0 {
		h.d.Audit.Log(c, "tunnel.stop_all", "", map[string]any{"stopped": stopped})
	}
	return c.JSON(http.StatusOK, map[string]any{"stopped": stopped})
}

func (h *handler) selectOwn(ctx context.Context, u *model.User, ids []string) ([]*record, error) {
	recs, err := h.m.repo.listByOwner(ctx, u.ID)
	if err != nil || len(ids) == 0 {
		return recs, err
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	out := recs[:0]
	for _, r := range recs {
		if want[r.ID] {
			out = append(out, r)
		}
	}
	return out, nil
}

// errorText renders an error the way the router would (status, code, message).
func errorText(err error) (int, string, string) {
	var he *httpx.HTTPError
	if errors.As(err, &he) {
		if he.Status >= 500 {
			return he.Status, he.Code, "internal error"
		}
		return he.Status, he.Code, he.Message
	}
	return http.StatusInternalServerError, model.CodeInternal, err.Error()
}

// recordFailure keeps a failed start visible as the tunnel's error status.
func (m *Manager) recordFailure(owner *model.User, rec *record, err error) {
	_, _, msg := errorText(err)
	m.mu.Lock()
	if r := m.runners[rec.ID]; r != nil && r.active() {
		m.mu.Unlock()
		return
	}
	r := newRunner(m, owner, rec, false)
	r.cancel()
	close(r.done)
	r.state, r.errMsg = model.TunnelError, msg
	m.runners[rec.ID] = r
	m.mu.Unlock()
	m.publish(r, "")
}

func (h *handler) reorder(c *echo.Context) error {
	var req struct {
		Items []struct {
			ID        string `json:"id"`
			SortOrder int    `json:"sortOrder"`
		} `json:"items"`
	}
	if err := httpx.BindLimit(c, &req, 1<<20); err != nil {
		return err
	}
	if len(req.Items) > maxTunnelsPerUser {
		return httpx.BadRequest("too many items")
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	orders := make(map[string]int, len(req.Items))
	for _, it := range req.Items {
		if !model.ValidID(it.ID) {
			return httpx.BadRequest("invalid id")
		}
		orders[it.ID] = it.SortOrder
	}
	if err := h.m.repo.setSortOrders(ctx, u.ID, orders); err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return httpx.ErrNotFound
		}
		return err
	}
	for id := range orders {
		h.m.publishChange(u.ID, id, "updated")
	}
	return httpx.OK(c)
}

// checkBind reports whether a listen address on the NexTerm host is usable (editor pre-flight check, TUN-1/TUN-2).
func (h *handler) checkBind(c *echo.Context) error {
	var req struct {
		BindHost   string `json:"bindHost"`
		BindPort   int    `json:"bindPort"`
		BindSocket string `json:"bindSocket"`
		ID         string `json:"id"` // the tunnel being edited (its own running listener does not count)
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	u := httpx.UserFrom(c)
	type result struct {
		Available  bool   `json:"available"`
		Error      string `json:"error,omitempty"`
		Suggestion int    `json:"suggestion,omitempty"`
	}
	d := def{Type: model.TunnelLocal, BindHost: req.BindHost, BindPort: req.BindPort, BindSocket: req.BindSocket,
		DestHost: "localhost", DestPort: 1}
	if err := d.normalize(); err != nil {
		_, _, msg := errorText(err)
		return c.JSON(http.StatusOK, result{Error: msg})
	}
	sp := &spec{kind: kindLocal, bindHost: d.BindHost, bindPort: d.BindPort, bindSocket: d.BindSocket}
	if err := h.m.policy(u, sp); err != nil {
		_, _, msg := errorText(err)
		return c.JSON(http.StatusOK, result{Error: msg})
	}
	if d.BindSocket == "" && d.BindPort == 0 {
		return c.JSON(http.StatusOK, result{Available: true})
	}
	// A listener of the caller's own tunnels / session forwards: fine when it is the tunnel being edited, otherwise
	// name it (more helpful than "in use by another program").
	if d.BindSocket == "" {
		if holder, id := h.m.holderOf(u, d.BindHost, d.BindPort); holder != "" {
			if id != "" && id == req.ID {
				return c.JSON(http.StatusOK, result{Available: true})
			}
			res := result{Error: fmt.Sprintf("port %d is used by %s", d.BindPort, holder)}
			res.Suggestion = h.suggestPort(u, sp)
			return c.JSON(http.StatusOK, res)
		}
	}
	if d.BindSocket != "" {
		if uc, err := net.DialTimeout("unix", d.BindSocket, time.Second); err == nil {
			uc.Close()
			return c.JSON(http.StatusOK, result{Error: d.BindSocket + " is in use"})
		}
		return c.JSON(http.StatusOK, result{Available: true})
	}
	if err := probeListen(sp); err != nil {
		return c.JSON(http.StatusOK, result{Error: err.Error(), Suggestion: h.suggestPort(u, sp)})
	}
	return c.JSON(http.StatusOK, result{Available: true})
}

// suggestPort finds the next usable port after sp's (free on this machine and not held by the user's tunnels).
func (h *handler) suggestPort(u *model.User, sp *spec) int {
	for p := sp.bindPort + 1; p <= min(sp.bindPort+50, 65535); p++ {
		alt := *sp
		alt.bindPort = p
		if holder, _ := h.m.holderOf(u, alt.bindHost, p); holder != "" {
			continue
		}
		if h.m.policy(u, &alt) == nil && probeListen(&alt) == nil {
			return p
		}
	}
	return 0
}

// probeListen tries to bind a TCP listener briefly.
func probeListen(sp *spec) error {
	network, addr := sp.localListenAddr()
	ln, err := net.Listen(network, addr)
	if err != nil {
		_, _, msg := errorText(listenError(sp.bindLabel(), err))
		return errors.New(msg)
	}
	return ln.Close()
}

// ---- import / export ----------------------------------------------------------------------------------------------

func (h *handler) export(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	recs, err := h.m.repo.listByOwner(ctx, u.ID)
	if err != nil {
		return err
	}
	conns := map[string]*model.Connection{}
	visible, err := h.d.Store.Connections.ListVisible(ctx, u.ID)
	if err != nil {
		return err
	}
	for _, c := range visible {
		conns[c.ID] = c
	}
	file := ExportFile{Format: exportFormat, Version: 1, ExportedAt: time.Now().UTC(), Tunnels: []ExportTunnel{}}
	for _, rec := range recs {
		opts := rec.Options
		_ = normalizeOptions(&opts, rec.def().kind())
		et := ExportTunnel{Name: rec.Name, Type: rec.Type, BindHost: rec.BindHost, BindPort: rec.BindPort,
			DestHost: rec.DestHost, DestPort: rec.DestPort, AutoStart: rec.AutoStart, Options: opts, SortOrder: rec.SortOrder}
		if cr := connRef(conns[rec.ConnectionID]); cr != nil {
			et.Connection = *cr
		} else {
			et.Connection = ConnRef{ID: rec.ConnectionID}
		}
		file.Tunnels = append(file.Tunnels, et)
	}
	name := "nexterm-tunnels-" + time.Now().Format("20060102") + ".json"
	c.Response().Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	return c.JSON(http.StatusOK, file)
}

type importRequest struct {
	File ExportFile `json:"file"`
	// DefaultConnectionID is used for tunnels whose connection cannot be matched.
	DefaultConnectionID string `json:"defaultConnectionId"`
	DryRun              bool   `json:"dryRun"`
}

type importPlanned struct {
	Name           string `json:"name"`
	ConnectionID   string `json:"connectionId"`
	ConnectionName string `json:"connectionName"`
	Matched        string `json:"matched"` // id | name+address | address | name | default
	// Warning flags a tunnel that listens on a non-loopback address (reachable from other machines), so importing
	// someone else's file cannot silently expose a listener.
	Warning string `json:"warning,omitempty"`
}

// exposureWarning describes a definition whose listener accepts clients from other machines ("" when it does not).
func exposureWarning(rec *record) string {
	if rec.Options.BindSocket != "" || isLoopbackHost(rec.BindHost) {
		return ""
	}
	bind := hostPortLabel(rec.BindHost, strconv.Itoa(rec.BindPort))
	if rec.def().kind().listensLocally() {
		return "listens on " + bind + ": reachable from other machines"
	}
	return "asks the SSH server to listen on " + bind + ": reachable from its network"
}

type importSkipped struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// importTunnels creates tunnels from an export file, mapping each one to a visible SSH connection (same id, else
// same name and address, same address, same name, else the default connection). Secrets are never imported.
func (h *handler) importTunnels(c *echo.Context) error {
	var req importRequest
	if err := httpx.BindLimit(c, &req, 8<<20); err != nil {
		return err
	}
	if req.File.Format != exportFormat {
		return httpx.BadRequest("not a NexTerm tunnels export file")
	}
	if len(req.File.Tunnels) > maxTunnelsPerUser {
		return httpx.BadRequest(fmt.Sprintf("at most %d tunnels per import", maxTunnelsPerUser))
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	visible, err := h.d.Store.Connections.ListVisible(ctx, u.ID)
	if err != nil {
		return err
	}
	var sshConns []*model.Connection
	for _, cn := range visible {
		switch cn.Protocol {
		case model.ProtoSSH, model.ProtoSFTP, model.ProtoMosh:
			sshConns = append(sshConns, cn)
		}
	}
	var defConn *model.Connection
	if req.DefaultConnectionID != "" {
		if defConn, err = h.sshConnection(ctx, u, req.DefaultConnectionID); err != nil {
			return err
		}
	}
	count, err := h.m.repo.countByOwner(ctx, u.ID)
	if err != nil {
		return err
	}
	maxOrder, _ := h.m.repo.maxSortOrder(ctx, u.ID)
	planned := []importPlanned{}
	skipped := []importSkipped{}
	created := []View{}
	for i, et := range req.File.Tunnels {
		name := strings.TrimSpace(et.Name)
		if name == "" {
			name = fmt.Sprintf("Tunnel %d", i+1)
		}
		conn, how := matchConnection(sshConns, et.Connection, defConn)
		if conn == nil {
			skipped = append(skipped, importSkipped{Name: name, Reason: "no matching SSH connection (choose a default connection)"})
			continue
		}
		opts := et.Options
		opts.SocksUsername = "" // no secrets are imported, so proxy authentication cannot be restored
		rec := &record{
			Tunnel: &model.Tunnel{Name: name, Type: et.Type, ConnectionID: conn.ID, BindHost: et.BindHost, BindPort: et.BindPort,
				DestHost: et.DestHost, DestPort: et.DestPort, AutoStart: false, OwnerID: u.ID},
			meta: meta{Options: opts, SortOrder: maxOrder + 1 + i, SecretKeys: []string{}},
		}
		if err := h.validate(ctx, u, rec); err != nil {
			_, _, msg := errorText(err)
			skipped = append(skipped, importSkipped{Name: name, Reason: msg})
			continue
		}
		plan := importPlanned{Name: name, ConnectionID: conn.ID, ConnectionName: conn.Name, Matched: how, Warning: exposureWarning(rec)}
		if req.DryRun {
			planned = append(planned, plan)
			continue
		}
		if count >= maxTunnelsPerUser {
			skipped = append(skipped, importSkipped{Name: name, Reason: "tunnel limit reached"})
			continue
		}
		if err := h.m.repo.create(ctx, rec); err != nil {
			return err
		}
		count++
		created = append(created, h.view(rec, conn))
		planned = append(planned, plan)
	}
	if !req.DryRun && len(created) > 0 {
		h.d.Audit.Log(c, "tunnel.import", "", map[string]any{"created": len(created), "skipped": len(skipped)})
		for _, v := range created {
			h.m.publishChange(u.ID, v.ID, "created")
		}
	}
	return c.JSON(http.StatusOK, map[string]any{"created": created, "planned": planned, "skipped": skipped})
}

// matchConnection maps an exported connection reference to one of the user's SSH connections.
func matchConnection(conns []*model.Connection, ref ConnRef, fallback *model.Connection) (*model.Connection, string) {
	sameAddr := func(c *model.Connection) bool {
		port := c.Port
		if port == 0 {
			port = 22
		}
		rp := ref.Port
		if rp == 0 {
			rp = 22
		}
		return ref.Host != "" && strings.EqualFold(c.Host, ref.Host) && port == rp &&
			(ref.Username == "" || c.Username == ref.Username)
	}
	if ref.ID != "" {
		for _, c := range conns {
			if c.ID == ref.ID {
				return c, "id"
			}
		}
	}
	for _, c := range conns {
		if ref.Name != "" && c.Name == ref.Name && sameAddr(c) {
			return c, "name+address"
		}
	}
	for _, c := range conns {
		if sameAddr(c) {
			return c, "address"
		}
	}
	if ref.Name != "" {
		for _, c := range conns {
			if strings.EqualFold(c.Name, ref.Name) {
				return c, "name"
			}
		}
	}
	if fallback != nil {
		return fallback, "default"
	}
	return nil, ""
}

// ---- remote ports & session forwards ------------------------------------------------------------------------------

func (h *handler) remotePorts(c *echo.Context) error {
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	res, err := h.m.remotePorts(ctx, u, c.QueryParam("sessionId"), c.QueryParam("connectionId"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, res)
}

func (h *handler) listSessionForwards(c *echo.Context) error {
	return c.JSON(http.StatusOK, h.m.sf.list(httpx.UserFrom(c), c.QueryParam("sessionId")))
}

func (h *handler) addSessionForward(c *echo.Context) error {
	var req struct {
		SessionID string `json:"sessionId"`
		ForwardSpec
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	ctx, u := c.Request().Context(), httpx.UserFrom(c)
	if h.m.forSession == nil {
		return httpx.Conflict("SSH is not available")
	}
	v, err := h.m.sf.add(ctx, u, req.SessionID, req.ForwardSpec)
	if err != nil {
		return err
	}
	h.d.Audit.Log(c, "tunnel.session_forward.add", req.SessionID, map[string]any{"forward": describeForwardSpec(v.Spec)})
	return c.JSON(http.StatusCreated, v)
}

func (h *handler) removeSessionForward(c *echo.Context) error {
	u := httpx.UserFrom(c)
	if err := h.m.sf.remove(u, c.Param("id")); err != nil {
		return err
	}
	h.d.Audit.Log(c, "tunnel.session_forward.remove", c.Param("id"), nil)
	return httpx.OK(c)
}
