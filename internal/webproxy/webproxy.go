// Package webproxy implements NexTerm's per-user HTTP(S) reverse proxy (PROTO-28 "Browser" sessions, TUN-8 "open a
// forwarded web service in the browser") and remote X11 applications in the browser through Xpra (PROTO-20).
//
// A proxy reaches one upstream origin (scheme://host:port) directly from the NexTerm host (vetted by internal/netguard
// in server mode), through a saved or live SSH connection (direct-tcpip channels) or along a saved "web" connection's
// route (sshTunnelVia / jumpHosts / proxy through sshx.Pool.Dialer). Proxies are served in one of two modes:
//
//   - host mode: every proxy is its own origin "p-<id>.localhost:<port>" (browsers resolve *.localhost to loopback)
//     or "p-<id>.<hostSuffix>" when an administrator configured a wildcard domain. The browser authenticates with a
//     one-time token (query parameter) exchanged for a host-only cookie; NexTerm's own cookies never reach that origin.
//   - path mode (server-mode fallback): "/proxy/<id>-<key>/…" on NexTerm's origin. The capability key authorizes the
//     requests and every response carries a CSP sandbox (no allow-same-origin), so the proxied page runs in an opaque
//     origin and cannot use NexTerm's session.
//
// Requests are dispatched by a pre-routing Echo middleware installed in Mount (see dispatch.go), in front of the API
// routes and the SPA fallback.
package webproxy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/nexterm/nexterm/internal/app"
	"github.com/nexterm/nexterm/internal/core"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/store"
	"github.com/nexterm/nexterm/internal/term"
)

// Limits and timings.
const (
	maxProxiesPerUser  = 32
	tokenTTL           = 2 * time.Minute
	maxTokensPerProxy  = 16
	cookieSessionTTL   = 12 * time.Hour
	maxCookieSessions  = 16
	userRecheck        = 30 * time.Second
	defaultIdleMinutes = 30
	janitorInterval    = 30 * time.Second
	settingsTTL        = 5 * time.Second
	dialTimeout        = 20 * time.Second
)

// SettingsKey is the global settings key of the module's administrator configuration.
const SettingsKey = "webproxy"

// Settings is the administrator configuration (global settings key "webproxy").
type Settings struct {
	// HostSuffix enables host mode for UIs not served from a loopback host: proxies are served as
	// "p-<id>.<HostSuffix>" (wildcard DNS and, with TLS, a wildcard certificate pointing at NexTerm are required).
	HostSuffix string `json:"hostSuffix,omitempty"`
	// PathMode allows the "/proxy/<id>-<key>/" fallback (default true).
	PathMode *bool `json:"pathMode,omitempty"`
	// IdleMinutes closes proxies without traffic after this many minutes (default 30, 1…1440).
	IdleMinutes int `json:"idleMinutes,omitempty"`
}

func (s Settings) pathModeAllowed() bool { return s.PathMode == nil || *s.PathMode }

func (s Settings) idle() time.Duration {
	m := s.IdleMinutes
	if m <= 0 {
		m = defaultIdleMinutes
	}
	if m > 1440 {
		m = 1440
	}
	return time.Duration(m) * time.Minute
}

// Service owns every live proxy of the instance.
type Service struct {
	d   *app.Deps
	c   *core.Core
	log *slog.Logger

	mu      sync.Mutex
	proxies map[string]*Proxy // by id

	setMu    sync.Mutex
	settings Settings
	setAt    time.Time

	// now is time.Now (tests override it).
	now func() time.Time
}

// services maps Deps to their Service so tests and other packages can reach it (Lookup).
var (
	svcMu    sync.Mutex
	services = map[*app.Deps]*Service{}
)

// Lookup returns the webproxy service mounted on d (nil when not mounted).
func Lookup(d *app.Deps) *Service {
	svcMu.Lock()
	defer svcMu.Unlock()
	return services[d]
}

// Mount registers the REST API, the Xpra launcher, the request dispatcher (host / path routing in front of the SPA)
// and the session-close hook.
func Mount(d *app.Deps, c *core.Core) error {
	s := newService(d, c)
	svcMu.Lock()
	services[d] = s
	svcMu.Unlock()

	api := d.Router.API()
	api.GET("/webproxy", s.handleList)
	api.POST("/webproxy", s.handleCreate)
	api.GET("/webproxy/:id", s.handleGet)
	api.DELETE("/webproxy/:id", s.handleDelete)
	api.POST("/webproxy/:id/url", s.handleURL)
	api.GET("/xpra/check", s.handleXpraCheck)
	api.POST("/xpra/start", s.handleXpraStart)

	// Proxied requests must be handled before routing: their paths belong to the upstream application (e.g. its own
	// /api/…), they carry no CSRF header, and NexTerm's authentication / body limits do not apply to them.
	d.Router.Echo().Pre(s.dispatch)

	if c != nil && c.Sessions != nil {
		c.Sessions.AddHooks(term.Hooks{OnClose: func(sess *term.Session) {
			s.closeWhere(func(p *Proxy) bool { return p.spec.SessionID == sess.ID }, "the SSH session was closed")
		}})
	}
	go s.janitor()
	context.AfterFunc(d.Ctx, func() {
		s.closeWhere(func(*Proxy) bool { return true }, "NexTerm is shutting down")
		svcMu.Lock()
		if services[d] == s {
			delete(services, d)
		}
		svcMu.Unlock()
	})
	return nil
}

func newService(d *app.Deps, c *core.Core) *Service {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{d: d, c: c, log: log.With("module", "webproxy"), proxies: map[string]*Proxy{}, now: time.Now}
}

// currentSettings returns the administrator configuration (cached for settingsTTL).
func (s *Service) currentSettings() Settings {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	if !s.setAt.IsZero() && time.Since(s.setAt) < settingsTTL {
		return s.settings
	}
	s.setAt = time.Now()
	var st Settings
	if s.d.Store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := s.d.Store.Settings.GetJSON(ctx, store.ScopeGlobal, SettingsKey, &st)
		cancel()
		if err != nil && !errors.Is(err, model.ErrNotFound) {
			s.log.Warn("cannot read the web proxy settings", "err", err)
			return s.settings
		}
	}
	st.HostSuffix = normalizeSuffix(st.HostSuffix)
	s.settings = st
	return st
}

// ---- registry -----------------------------------------------------------------------------------------------------

func (s *Service) get(id string) *Proxy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proxies[id]
}

// owned returns the proxy id of user (nil when unknown or foreign).
func (s *Service) owned(user *model.User, id string) *Proxy {
	p := s.get(id)
	if p == nil || user == nil || p.owner.ID != user.ID {
		return nil
	}
	return p
}

// add registers p, evicting the owner's least recently used proxy beyond maxProxiesPerUser.
func (s *Service) add(p *Proxy) {
	var evict *Proxy
	s.mu.Lock()
	s.proxies[p.id] = p
	var mine []*Proxy
	for _, q := range s.proxies {
		if q.owner.ID == p.owner.ID {
			mine = append(mine, q)
		}
	}
	if len(mine) > maxProxiesPerUser {
		sort.Slice(mine, func(i, j int) bool { return mine[i].lastUsed().Before(mine[j].lastUsed()) })
		for _, q := range mine {
			if q != p && q.inflight() == 0 {
				evict = q
				break
			}
		}
		if evict != nil {
			delete(s.proxies, evict.id)
		}
	}
	s.mu.Unlock()
	if evict != nil {
		evict.shutdown()
		s.publish(evict, "closed", "too many open web proxies (closed the least recently used one)")
	}
	s.publish(p, "created", "")
}

// remove closes and unregisters p (idempotent).
func (s *Service) remove(p *Proxy, reason string) {
	s.mu.Lock()
	cur := s.proxies[p.id]
	if cur == p {
		delete(s.proxies, p.id)
	}
	s.mu.Unlock()
	if cur != p {
		return
	}
	p.shutdown()
	s.publish(p, "closed", reason)
}

func (s *Service) closeWhere(match func(*Proxy) bool, reason string) {
	s.mu.Lock()
	var list []*Proxy
	for _, p := range s.proxies {
		if match(p) {
			list = append(list, p)
		}
	}
	s.mu.Unlock()
	for _, p := range list {
		s.remove(p, reason)
	}
}

func (s *Service) list(user *model.User) []*Proxy {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Proxy
	for _, p := range s.proxies {
		if p.owner.ID == user.ID {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].createdAt.Before(out[j].createdAt) })
	return out
}

// janitor closes idle proxies and proxies whose owner was disabled or deleted.
func (s *Service) janitor() {
	t := time.NewTicker(janitorInterval)
	defer t.Stop()
	for {
		select {
		case <-s.d.Ctx.Done():
			return
		case <-t.C:
		}
		idle := s.currentSettings().idle()
		now := s.now()
		s.closeWhere(func(p *Proxy) bool {
			return p.inflight() == 0 && now.Sub(p.lastUsed()) > idle
		}, "closed after being idle")
		s.mu.Lock()
		var all []*Proxy
		for _, p := range s.proxies {
			all = append(all, p)
		}
		s.mu.Unlock()
		for _, p := range all {
			if err := s.checkOwner(p); err != nil {
				s.remove(p, "the owner's account is no longer active")
			}
		}
	}
}

// checkOwner re-reads the owner's account (at most every userRecheck) and returns an error when it is disabled or
// gone. The refreshed record (role changes) is used for later dials.
func (s *Service) checkOwner(p *Proxy) error {
	if s.d.Store == nil {
		return nil
	}
	p.mu.Lock()
	due := time.Since(p.userCheckedAt) >= userRecheck
	if due {
		p.userCheckedAt = time.Now()
	}
	p.mu.Unlock()
	if !due {
		return p.ownerErr()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	u, err := s.d.Store.Users.Get(ctx, p.owner.ID)
	switch {
	case errors.Is(err, model.ErrNotFound):
		p.setOwnerErr(errOwnerGone)
	case err != nil:
		return p.ownerErr() // transient: keep the previous verdict
	case u.Disabled:
		p.setOwnerErr(errOwnerGone)
	default:
		p.setOwner(u)
	}
	return p.ownerErr()
}

var errOwnerGone = errors.New("account disabled or deleted")

// ---- events -------------------------------------------------------------------------------------------------------

// Event is pushed to the owner's sockets as {type:'webproxy', change, proxy, reason?}.
type Event struct {
	Type   string `json:"type"`
	Change string `json:"change"` // created | closed | updated
	Proxy  Info   `json:"proxy"`
	Reason string `json:"reason,omitempty"`
}

func (s *Service) publish(p *Proxy, change, reason string) {
	if s.d.Events == nil {
		return
	}
	s.d.Events.Publish(p.owner.ID, Event{Type: "webproxy", Change: change, Proxy: p.info(), Reason: reason})
}

// marshalJSON is json.Marshal for small config blobs embedded in scripts (never fails for the types used).
func marshalJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
