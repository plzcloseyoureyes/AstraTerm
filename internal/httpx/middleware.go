package httpx

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"

	"github.com/nexterm/nexterm/internal/model"
)

// ---- Pre (before routing) -----------------------------------------------------------------------------------------

// requestLogger logs one "http" record per request (method, path with share tokens redacted, status, dur, ip, and
// err for 5xx) at a level derived from the status, and stores the resolved client IP in the request context. The
// error handler runs first (HandleError), so the logged status is the one sent to the client.
func (r *Router) requestLogger() echo.MiddlewareFunc {
	return middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		HandleError: true,
		LogLatency:  true,
		LogMethod:   true,
		LogStatus:   true,
		BeforeNextFunc: func(c *echo.Context) {
			req := c.Request()
			c.SetRequest(req.WithContext(WithClientIP(req.Context(), c.RealIP())))
		},
		LogValuesFunc: func(c *echo.Context, v middleware.RequestLoggerValues) error {
			p := safePath(c.Request().URL.Path)
			level := slog.LevelInfo
			switch {
			case v.Status >= 500:
				level = slog.LevelError
			case v.Status >= 400:
				level = slog.LevelWarn
			case !strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/ws/"):
				level = slog.LevelDebug // static assets
			case v.Method == http.MethodGet && v.Status < 300:
				level = slog.LevelDebug
			}
			attrs := []any{"method", v.Method, "path", p, "status", v.Status,
				"dur", v.Latency.Round(time.Microsecond), "ip", ClientIP(c)}
			if v.Error != nil && v.Status >= 500 {
				attrs = append(attrs, "err", v.Error.Error())
			}
			r.log.Log(c.Request().Context(), level, "http", attrs...)
			return nil
		},
	})
}

// safePath redacts bearer-like path segments from logs: share tokens, and the key of web-proxy path-mode URLs
// (/proxy/<id>-<key>/… → /proxy/<id>-…/…; the id stays so requests can still be told apart).
func safePath(p string) string {
	for _, pre := range []string{"/api/share/", "/ws/share/"} {
		if strings.HasPrefix(p, pre) {
			return pre + "…"
		}
	}
	if rest, ok := strings.CutPrefix(p, "/proxy/"); ok {
		seg, tail, hasTail := strings.Cut(rest, "/")
		id, _, keyed := strings.Cut(seg, "-")
		if !keyed {
			id = ""
		}
		out := "/proxy/" + id + "-…"
		if hasTail {
			out += "/" + tail
		}
		return out
	}
	return p
}

// recoverer turns a panic into a logged (with stack) 500 that never exposes the panic value to the client.
func (r *Router) recoverer(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				req := c.Request()
				r.log.Error("panic in handler", "method", req.Method, "path", safePath(req.URL.Path),
					"panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
				err = ErrInternal
			}
		}()
		return next(c)
	}
}

// secureHeaders sets the static security headers (nosniff, same-origin framing, no referrer).
func secureHeaders() echo.MiddlewareFunc {
	return middleware.SecureWithConfig(middleware.SecureConfig{
		ContentTypeNosniff: "nosniff",
		XFrameOptions:      "SAMEORIGIN",
		ReferrerPolicy:     "no-referrer",
	})
}

// securityHeaders sets the per-request Content-Security-Policy (its connect-src names the request host), the
// cross-origin isolation and permissions policies, and HSTS when serving TLS.
func (r *Router) securityHeaders(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		h := c.Response().Header()
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), geolocation=(), payment=(), usb=()")
		h.Set(echo.HeaderContentSecurityPolicy, contentSecurityPolicy(c.Request().Host))
		if r.opts.TLS {
			h.Set(echo.HeaderStrictTransportSecurity, "max-age=31536000")
		}
		return next(c)
	}
}

// contentSecurityPolicy allows only same-origin resources plus what the SPA needs: WASM compilation (IronRDP,
// asciinema-player: 'wasm-unsafe-eval'), inline styles (CodeMirror, xterm, dockview), blob:/data: images, fonts and
// media, blob: workers, and ws/wss to this host.
func contentSecurityPolicy(host string) string {
	connect := "'self'"
	if validHostHeader(host) {
		connect += " ws://" + host + " wss://" + host
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' 'wasm-unsafe-eval'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob:",
		"font-src 'self' data:",
		"media-src 'self' data: blob:",
		"connect-src " + connect,
		"worker-src 'self' blob:",
		"frame-src 'self' blob:",
		"object-src 'none'",
		"base-uri 'self'",
		"form-action 'self'",
		"frame-ancestors 'self'",
	}, "; ")
}

func validHostHeader(h string) bool {
	if h == "" || len(h) > 255 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(".-:[]_", c) >= 0) {
			return false
		}
	}
	return true
}

var errInvalidHost = NewError(http.StatusMisdirectedRequest, "invalid_host", "invalid Host header")

// hostGuard rejects requests whose Host header is not a loopback name (or one of Options.AllowedHosts) when the server
// only listens on loopback. This defeats DNS-rebinding attacks against the local instance (SEC-6).
func (r *Router) hostGuard(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if r.opts.LoopbackOnly && !r.allowedLoopbackHost(c.Request().Host) {
			return errInvalidHost
		}
		return next(c)
	}
}

func (r *Router) allowedLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if slices.Contains(r.opts.AllowedHosts, strings.TrimSuffix(host, ".")) {
		return true
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Unmap().IsLoopback()
	}
	return r.hostname != "" && (host == r.hostname || strings.TrimSuffix(host, ".local") == strings.TrimSuffix(r.hostname, ".local"))
}

// ---- Use (after routing) ------------------------------------------------------------------------------------------

// authenticate resolves the user (session cookie or Bearer token) and stores it with its AuthInfo in the request
// context. Anonymous requests continue; RequireUser / RequireAdmin enforce authentication per route.
func (r *Router) authenticate(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if a := r.authenticator(); a != nil {
			u, info, err := a.Authenticate(c)
			if err != nil {
				return err
			}
			if u != nil {
				req := c.Request()
				c.SetRequest(req.WithContext(WithAuthInfo(WithUser(req.Context(), u), info)))
			}
		}
		if r.opts.Dev {
			req := c.Request()
			c.SetRequest(req.WithContext(withOriginPatterns(req.Context(), DevOrigins)))
		}
		return next(c)
	}
}

var errCSRF = NewError(http.StatusForbidden, "csrf", "missing "+CSRFHeader+" header")

// csrf requires the X-NexTerm: 1 header on every mutating request that was not authenticated with a Bearer token.
// Browsers cannot attach custom headers cross-origin without a CORS preflight (which we never grant).
func csrf(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		req := c.Request()
		switch req.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if AuthInfoFromContext(req.Context()).Method != AuthToken && req.Header.Get(CSRFHeader) != CSRFHeaderValue {
				return errCSRF
			}
		}
		return next(c)
	}
}

// canonicalPath redirects (307) requests whose path contains "." / ".." elements or repeated slashes to the cleaned
// path, as net/http.ServeMux does, so no handler ever sees a non-canonical path.
func canonicalPath(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		req := c.Request()
		if req.Method != http.MethodConnect {
			escaped := req.URL.EscapedPath()
			if p := cleanPath(escaped); p != escaped {
				u := &url.URL{Path: p, RawQuery: req.URL.RawQuery}
				http.Redirect(c.Response(), req, u.String(), http.StatusTemporaryRedirect)
				return nil
			}
		}
		return next(c)
	}
}

// cleanPath returns the canonical path for p, eliminating . and .. elements and repeated slashes (keeping a trailing
// slash), like net/http.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	if p[len(p)-1] == '/' && np != "/" {
		if len(p) == len(np)+1 && strings.HasPrefix(p, np) {
			np = p
		} else {
			np += "/"
		}
	}
	return np
}

// limitedBody is a request body capped by bodyLimit / BodyLimit; orig is the uncapped body.
type limitedBody struct {
	io.ReadCloser
	orig     io.ReadCloser
	explicit bool // set by a route's BodyLimit (honored by Bind), not by the default cap
}

// bodyLimit caps every request body at DefaultBodyLimit bytes; reading beyond fails with *http.MaxBytesError (→ 413
// too_large). The cap applies lazily, only to handlers that read the body: Bind / BindLimit apply their own limit
// instead, and a route can replace the cap with BodyLimit.
func bodyLimit(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		setBodyLimit(c.Request(), DefaultBodyLimit, false)
		return next(c)
	}
}

// BodyLimit returns route middleware replacing the default request body cap (DefaultBodyLimit) with n bytes; n <= 0
// removes it (streaming uploads). Bind / BindLimit also honor a smaller route cap.
func BodyLimit(n int64) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			setBodyLimit(c.Request(), n, true)
			return next(c)
		}
	}
}

func setBodyLimit(req *http.Request, n int64, explicit bool) {
	if req.Body == nil || req.Body == http.NoBody {
		return
	}
	orig := req.Body
	if lb, ok := orig.(*limitedBody); ok {
		orig = lb.orig
	}
	if n <= 0 {
		req.Body = orig
		return
	}
	req.Body = &limitedBody{ReadCloser: http.MaxBytesReader(nil, orig, n), orig: orig, explicit: explicit}
}

// ---- route level --------------------------------------------------------------------------------------------------

// RequireUser is route middleware that answers 401 unless the request is authenticated (used by the API group).
func RequireUser(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if UserFrom(c) == nil {
			return ErrUnauthorized
		}
		return next(c)
	}
}

// RequireAdmin is route middleware that answers 401 for anonymous and 403 for non-admin requests (Admin group).
func RequireAdmin(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		u := UserFrom(c)
		if u == nil {
			return ErrUnauthorized
		}
		if !u.IsAdmin() {
			return Forbidden("administrator privileges required")
		}
		return next(c)
	}
}

var errCrossOriginWS = NewError(http.StatusForbidden, model.CodeForbidden, "cross-origin WebSocket rejected")

// checkOrigin enforces a same-host Origin (or a dev origin) on WebSocket upgrades (cross-site WebSocket hijacking).
func (r *Router) checkOrigin(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if !originAllowed(c.Request(), r.opts.Dev) {
			return errCrossOriginWS
		}
		return next(c)
	}
}

func originAllowed(req *http.Request, dev bool) bool {
	origin := req.Header.Get(echo.HeaderOrigin)
	if origin == "" {
		return true // non-browser client
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, req.Host) {
		return true
	}
	if dev {
		for _, o := range DevOrigins {
			if strings.EqualFold(u.Host, o) {
				return true
			}
		}
	}
	return false
}
