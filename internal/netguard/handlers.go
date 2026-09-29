package netguard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
)

// Mount registers the administrator endpoints (SPEC §9 "netguard"):
//
//	GET  /api/admin/network-policy       → PolicyView
//	PUT  /api/admin/network-policy       {Policy keys; omitted keys keep their value} → PolicyView (audited)
//	POST /api/admin/network-policy/test  {host, port, userId?, policy?} → TestResult
func Mount(d *app.Deps) error {
	m := For(d)
	h := &handler{m: m}
	admin := d.Router.Admin()
	admin.GET("/admin/network-policy", h.get)
	admin.PUT("/admin/network-policy", h.put)
	admin.POST("/admin/network-policy/test", h.test)
	return nil
}

type handler struct{ m *Manager }

// PolicyView is the GET / PUT reply.
type PolicyView struct {
	Policy        Policy        `json:"policy"`
	Default       Policy        `json:"default"`
	Mode          string        `json:"mode"`
	Enforced      bool          `json:"enforced"` // false in desktop mode: nothing is restricted
	Listen        string        `json:"listen"`
	Builtin       []builtinRule `json:"builtin"`       // always-refused ranges (exceptions possible via allow)
	PrivateRanges []string      `json:"privateRanges"` // refused when allowPrivate is false
	HostAddresses []string      `json:"hostAddresses"` // refused when blockHostAddresses is true
}

func (h *handler) view() PolicyView {
	v := PolicyView{Policy: h.m.Policy(), Default: DefaultPolicy(), Enforced: h.m.Enforced(), Builtin: BuiltinRules(),
		PrivateRanges: append([]string(nil), privateRanges...)}
	if cfg := h.m.d.Cfg; cfg != nil {
		v.Mode, v.Listen = cfg.Mode, cfg.Listen
	}
	for a := range hostAddrs() {
		v.HostAddresses = append(v.HostAddresses, a.String())
	}
	sort.Strings(v.HostAddresses)
	return v
}

func (h *handler) get(c *echo.Context) error { return c.JSON(http.StatusOK, h.view()) }

func (h *handler) put(c *echo.Context) error {
	var raw json.RawMessage
	if err := httpx.Bind(c, &raw); err != nil {
		return err
	}
	old := h.m.Policy()
	p := clonePolicy(old)
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return httpx.BadRequest("invalid network policy: " + strings.TrimPrefix(err.Error(), "json: "))
	}
	if p.Deny == nil {
		p.Deny = []string{}
	}
	if p.Allow == nil {
		p.Allow = []string{}
	}
	saved, err := h.m.SetPolicy(c.Request().Context(), p)
	if err != nil {
		return err
	}
	if h.m.d.Audit != nil {
		h.m.d.Audit.Log(c, "netguard.policy.update", SettingsKey, map[string]any{"old": old, "new": saved})
	}
	return c.JSON(http.StatusOK, h.view())
}

// TestRequest is the dry-run body.
type TestRequest struct {
	Host   string  `json:"host"`
	Port   int     `json:"port"`
	UserID string  `json:"userId,omitempty"` // evaluate for this user (default: an ordinary user)
	Policy *Policy `json:"policy,omitempty"` // evaluate an unsaved draft instead of the saved policy
}

// TestResult is the dry-run reply.
type TestResult struct {
	Host       string     `json:"host"`
	Port       int        `json:"port"`
	Decision   string     `json:"decision"` // allow | deny | unrestricted | error
	Allowed    bool       `json:"allowed"`
	Restricted bool       `json:"restricted"` // whether the policy applies to the user
	Reason     string     `json:"reason"`
	User       *TestUser  `json:"user,omitempty"`
	Addresses  []Decision `json:"addresses"`
	Error      string     `json:"error,omitempty"` // resolution failure
}

// TestUser identifies the user a dry run was evaluated for.
type TestUser struct {
	ID       string     `json:"id"`
	Username string     `json:"username"`
	Role     model.Role `json:"role"`
}

func (h *handler) test(c *echo.Context) error {
	var req TestRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	req.Host = strings.TrimSpace(req.Host)
	if req.Host == "" || len(req.Host) > 255 || strings.ContainsAny(req.Host, " \t/\\@") {
		return httpx.BadRequest("a host name or IP address is required")
	}
	if req.Port < 0 || req.Port > 65535 {
		return httpx.BadRequest("port must be 0 (any) or 1-65535")
	}
	ctx := c.Request().Context()
	user := &model.User{ID: "", Username: "(ordinary user)", Role: model.RoleUser}
	res := TestResult{Host: req.Host, Port: req.Port, Addresses: []Decision{}}
	if req.UserID != "" {
		u, err := h.m.d.Store.Users.Get(ctx, req.UserID)
		if errors.Is(err, model.ErrNotFound) {
			return httpx.NotFound("no such user")
		}
		if err != nil {
			return err
		}
		user = u
		res.User = &TestUser{ID: u.ID, Username: u.Username, Role: u.Role}
	}
	pol := h.m.Policy()
	if req.Policy != nil {
		pol = *req.Policy
		if pol.Deny == nil {
			pol.Deny = []string{}
		}
		if pol.Allow == nil {
			pol.Allow = []string{}
		}
	}
	g, err := h.m.withPolicy(pol)
	if err != nil {
		return httpx.BadRequest(err.Error())
	}
	switch {
	case !h.m.Enforced():
		res.Decision, res.Allowed, res.Reason = "unrestricted", true, "desktop mode: connections are not restricted"
		return c.JSON(http.StatusOK, res)
	case user.IsAdmin() && !pol.ApplyToAdmins:
		res.Decision, res.Allowed, res.Reason = "unrestricted", true, "administrators are not restricted (applyToAdmins is off)"
		return c.JSON(http.StatusOK, res)
	}
	res.Restricted = true

	var addrs []netip.Addr
	switch a, ok := ParseHostIP(req.Host); {
	case ok:
		addrs = []netip.Addr{a}
	case isLocalhostName(req.Host):
		addrs = []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.IPv6Loopback()}
	default:
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		ips, err := resolver.LookupNetIP(rctx, "ip", req.Host)
		cancel()
		if err != nil {
			res.Decision, res.Reason, res.Error = "error", "the host name could not be resolved", err.Error()
			return c.JSON(http.StatusOK, res)
		}
		addrs = ips
	}
	res.Allowed = true
	for _, a := range addrs {
		d := g.Check(a, req.Port)
		res.Addresses = append(res.Addresses, d)
		if !d.Allowed && res.Allowed {
			res.Allowed, res.Reason = false, d.IP+" is "+d.Reason
			if d.Class == ClassPort {
				res.Reason = d.Reason
			}
		}
	}
	if res.Allowed {
		res.Decision, res.Reason = "allow", "every address is allowed"
		if len(res.Addresses) == 1 {
			res.Reason = res.Addresses[0].IP + " is " + res.Addresses[0].Reason
			if res.Addresses[0].Class == ClassPublic {
				res.Reason = res.Addresses[0].IP + " is allowed"
			}
		}
	} else {
		res.Decision = "deny"
	}
	return c.JSON(http.StatusOK, res)
}
