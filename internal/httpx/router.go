// Package httpx is Termstead's HTTP layer, built on Echo v5 (SPEC §3, §4 "Router helpers"): a Router wrapping an
// *echo.Echo with the middleware chain (request log, recover, security headers, Host guard, authentication, CSRF, body
// limit), route groups for public / authenticated / admin API routes, Origin-checked WebSocket routes, the SPA
// fallback, a JSON error handler for typed errors ({error, code}) and request helpers (UserFrom, ClientIP, Bind,
// AcceptWS).
package httpx

import (
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/model"
)

// CSRFHeader must accompany every mutating request that is not authenticated by a Bearer token.
const (
	CSRFHeader      = "X-Termstead"
	CSRFHeaderValue = "1"
)

// DevOrigins are the Vite dev-server origins accepted in --dev mode.
var DevOrigins = []string{"localhost:5173", "127.0.0.1:5173"}

// Authenticator resolves the user of a request. It returns (nil, AuthInfo{}, nil) for anonymous requests and may set
// cookies on c.Response() (sliding sessions). It is installed by the auth module.
type Authenticator interface {
	Authenticate(c *echo.Context) (*model.User, AuthInfo, error)
}

// AuthenticatorFunc adapts a function to Authenticator.
type AuthenticatorFunc func(c *echo.Context) (*model.User, AuthInfo, error)

// Authenticate implements Authenticator.
func (f AuthenticatorFunc) Authenticate(c *echo.Context) (*model.User, AuthInfo, error) { return f(c) }

// Options configures a Router.
type Options struct {
	Log *slog.Logger
	// TrustedProxies whose forwarding headers are honored for the client IP.
	TrustedProxies []netip.Prefix
	// Dev allows the Vite dev server origins for WebSockets.
	Dev bool
	// LoopbackOnly enables the DNS-rebinding guard: only loopback / localhost Host headers (and the machine's own
	// hostname) are accepted. Set when the server listens on a loopback address.
	LoopbackOnly bool
	// AllowedHosts are extra Host names (lower case, no port) the guard accepts, e.g. the public name of a reverse
	// proxy on the same machine that forwards the original Host header.
	AllowedHosts []string
	// TLS adds HSTS.
	TLS bool
}

// Router is the application's HTTP handler: an *echo.Echo with Termstead's middleware chain, error handler and route
// groups. Middleware order, outermost first:
//
//	Pre:   request log → recover → security headers → Host guard      (every request, before routing)
//	Use:   authentication → CSRF → canonical-path redirect → body limit (every request, after routing)
//	route: WS Origin check, RequireUser (API group, WS) / RequireAdmin (Admin group)
//
// Requests that match no route get a JSON 404 under /api and /ws, a JSON 405 for other methods than GET/HEAD, and
// the fallback handler (the SPA) otherwise.
type Router struct {
	e    *echo.Echo
	opts Options
	log  *slog.Logger

	mu       sync.RWMutex
	auth     Authenticator
	fallback echo.HandlerFunc

	hostname string
}

// NewRouter builds a router with the full middleware chain.
func NewRouter(opts Options) *Router {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	r := &Router{opts: opts, log: opts.Log}
	if h, err := os.Hostname(); err == nil {
		r.hostname = strings.ToLower(h)
	}
	trusted := opts.TrustedProxies
	e := echo.NewWithConfig(echo.Config{
		Logger:           opts.Log,
		HTTPErrorHandler: r.handleError,
		Router: echo.NewConcurrentRouter(&muxRouter{
			DefaultRouter: echo.NewRouter(echo.RouterConfig{
				NotFoundHandler:         r.unmatched,
				MethodNotAllowedHandler: r.unmatched,
				OptionsMethodHandler:    r.unmatched,
				UnescapePathParamValues: true, // see muxRouter.Route
				AutoHandleHEAD:          true, // GET routes answer HEAD, as with net/http.ServeMux
			}),
			log:  opts.Log,
			seen: map[string]bool{},
		}),
		Binder:         jsonBinder{},
		JSONSerializer: jsonSerializer{},
		IPExtractor:    func(req *http.Request) string { return resolveClientIP(req, trusted) },
		// Group middleware (RequireUser / RequireAdmin) runs only for matched routes; unknown paths get the JSON 404.
		NoGroupAutoRegister404Routes: true,
	})
	e.Pre(r.requestLogger(), r.recoverer, secureHeaders(), r.securityHeaders, r.hostGuard)
	e.Use(r.authenticate, csrf, canonicalPath, bodyLimit)
	r.e = e
	return r
}

// Echo returns the underlying Echo instance (for routes outside /api and /ws; they get the global middleware but no
// authentication requirement).
func (r *Router) Echo() *echo.Echo { return r.e }

// Public returns an "/api" group for routes that need no authentication (the user is still resolved when present).
// Each call returns a new group, so Use or sub-groups of one module never affect another's routes.
func (r *Router) Public() *echo.Group { return r.e.Group("/api") }

// API returns an "/api" group for routes that require an authenticated user (401 otherwise).
func (r *Router) API() *echo.Group { return r.e.Group("/api", RequireUser) }

// Admin returns an "/api" group for routes that require an administrator (401 anonymous, 403 non-admin).
func (r *Router) Admin() *echo.Group { return r.e.Group("/api", RequireAdmin) }

// WS registers a WebSocket endpoint (GET, full path such as "/ws/terminal/:id") that requires an authenticated user
// and a same-origin (or dev) Origin. The handler upgrades the connection itself with AcceptWS.
func (r *Router) WS(path string, h echo.HandlerFunc) {
	r.e.GET(path, h, r.checkOrigin, RequireUser)
}

// PublicWS registers an unauthenticated WebSocket endpoint (e.g. "/ws/share/:token") with the Origin check.
func (r *Router) PublicWS(path string, h echo.HandlerFunc) {
	r.e.GET(path, h, r.checkOrigin)
}

// SetAuthenticator installs the authenticator (called once by the auth module).
func (r *Router) SetAuthenticator(a Authenticator) {
	r.mu.Lock()
	r.auth = a
	r.mu.Unlock()
}

// SetFallback installs the handler for GET/HEAD requests outside /api and /ws that match no route (the SPA).
func (r *Router) SetFallback(h echo.HandlerFunc) {
	r.mu.Lock()
	r.fallback = h
	r.mu.Unlock()
}

// ServeHTTP implements http.Handler.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) { r.e.ServeHTTP(w, req) }

func (r *Router) authenticator() Authenticator {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.auth
}

var (
	errNoEndpoint       = NotFound("no such endpoint")
	errMethodNotAllowed = NewError(http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
)

// unmatched handles requests no route matches (echo's not-found, method-not-allowed and OPTIONS handlers): JSON 404
// under /api and /ws, 405 (Allow: GET, HEAD) for other methods, the fallback (SPA) for everything else.
func (r *Router) unmatched(c *echo.Context) error {
	req := c.Request()
	if isReservedPath(req.URL.Path) {
		return errNoEndpoint
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		c.Response().Header().Set(echo.HeaderAllow, "GET, HEAD")
		return errMethodNotAllowed
	}
	r.mu.RLock()
	fb := r.fallback
	r.mu.RUnlock()
	if fb == nil {
		return ErrNotFound
	}
	return fb(c)
}

// isReservedPath reports whether p belongs to the API or WebSocket namespaces (never served by the fallback).
func isReservedPath(p string) bool {
	return p == "/api" || p == "/ws" || strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/ws/")
}

// ---- router adapter -----------------------------------------------------------------------------------------------

// muxRouter adapts echo's DefaultRouter to the conventions the API had with net/http.ServeMux:
//   - Registering a route whose method and path (ignoring parameter names) are already taken logs the conflict and
//     keeps the first route, instead of panicking or silently replacing it.
//   - Requests are matched on canonically escaped path segments: literal segments match regardless of optional
//     percent-encoding, and path parameters are decoded exactly once ("/api/connections/a%2Fb" → id "a/b").
//
// It runs inside echo.NewConcurrentRouter, which serializes Add against Route.
type muxRouter struct {
	*echo.DefaultRouter
	log  *slog.Logger
	seen map[string]bool
}

// Add implements echo.Router.
func (m *muxRouter) Add(route echo.Route) (echo.RouteInfo, error) {
	key := route.Method + " " + routeShape(route.Path)
	if m.seen[key] {
		m.log.Error("route registration failed", "method", route.Method, "path", route.Path,
			"err", "a route with this method and path is already registered")
		return echo.RouteInfo{Method: route.Method, Path: route.Path, Name: route.Name}, nil
	}
	ri, err := m.DefaultRouter.Add(route)
	if err != nil {
		m.log.Error("route registration failed", "method", route.Method, "path", route.Path, "err", err)
		return echo.RouteInfo{Method: route.Method, Path: route.Path, Name: route.Name}, nil
	}
	m.seen[key] = true
	return ri, nil
}

// Remove implements echo.Router.
func (m *muxRouter) Remove(method, path string) error {
	if err := m.DefaultRouter.Remove(method, path); err != nil {
		return err
	}
	delete(m.seen, method+" "+routeShape(path))
	return nil
}

// Route implements echo.Router.
func (m *muxRouter) Route(c *echo.Context) echo.HandlerFunc {
	u := c.Request().URL
	raw := u.RawPath
	u.RawPath = routingPath(u.EscapedPath()) // the default router matches on RawPath when it is set
	h := m.DefaultRouter.Route(c)
	u.RawPath = raw
	return h
}

// routeShape normalizes a route path for conflict detection: parameter names do not distinguish routes.
func routeShape(p string) string {
	if !strings.Contains(p, ":") {
		return p
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, ":") {
			segs[i] = ":"
		}
	}
	return strings.Join(segs, "/")
}

// routingPath re-escapes every segment of an escaped path canonically (url.PathEscape of the unescaped segment).
func routingPath(escaped string) string {
	simple := true
	for i := 0; i < len(escaped); i++ {
		c := escaped[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("/-_.~", c) >= 0) {
			simple = false
			break
		}
	}
	if simple {
		return escaped
	}
	segs := strings.Split(escaped, "/")
	for i, s := range segs {
		if u, err := url.PathUnescape(s); err == nil {
			segs[i] = url.PathEscape(u)
		}
	}
	return strings.Join(segs, "/")
}
