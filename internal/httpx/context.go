package httpx

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/nexterm/nexterm/internal/model"
)

// The authenticated user, its AuthInfo and the client IP travel in the request's context.Context (not only in the
// echo.Context), so code below the HTTP layer (audit, term, sshx…) reads them from any ctx derived from the request.

type ctxKey int

const (
	userKey ctxKey = iota
	authInfoKey
	clientIPKey
	originPatternsKey
)

// Authentication methods reported in AuthInfo.Method.
const (
	AuthCookie = "cookie"
	AuthToken  = "token"
)

// AuthInfo describes how the current request was authenticated.
type AuthInfo struct {
	Method    string // AuthCookie | AuthToken | "" (anonymous)
	SessionID string // auth_sessions.id for cookie logins
	TokenID   string // api_tokens.id for Bearer tokens
}

// UserFrom returns the authenticated user of the request, or nil when anonymous.
func UserFrom(c *echo.Context) *model.User { return UserFromContext(c.Request().Context()) }

// UserFromContext returns the user carried by ctx (a request context or one derived from it), or nil.
func UserFromContext(ctx context.Context) *model.User {
	u, _ := ctx.Value(userKey).(*model.User)
	return u
}

// WithUser returns ctx carrying user.
func WithUser(ctx context.Context, u *model.User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// AuthInfoFrom returns how the request was authenticated.
func AuthInfoFrom(c *echo.Context) AuthInfo { return AuthInfoFromContext(c.Request().Context()) }

// AuthInfoFromContext returns the AuthInfo carried by ctx.
func AuthInfoFromContext(ctx context.Context) AuthInfo {
	info, _ := ctx.Value(authInfoKey).(AuthInfo)
	return info
}

// WithAuthInfo returns ctx carrying info.
func WithAuthInfo(ctx context.Context, info AuthInfo) context.Context {
	return context.WithValue(ctx, authInfoKey, info)
}

// ClientIP returns the client IP of the request, honoring trusted proxies (see resolveClientIP).
func ClientIP(c *echo.Context) string {
	if ip := ClientIPFrom(c.Request().Context()); ip != "" {
		return ip
	}
	return c.RealIP()
}

// ClientIPFrom returns the client IP stored in ctx by the router ("" if unknown).
func ClientIPFrom(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey).(string)
	return ip
}

// WithClientIP returns ctx carrying the resolved client IP.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey, ip)
}

func remoteIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

// resolveClientIP implements trusted-proxy aware client IP detection (the router's echo.IPExtractor):
// X-Forwarded-For is walked right-to-left, skipping trusted proxies; X-Real-IP is used as a fallback. Untrusted
// peers' headers are ignored.
func resolveClientIP(r *http.Request, trusted []netip.Prefix) string {
	peer := remoteIP(r)
	if !peer.IsValid() {
		return r.RemoteAddr
	}
	if !inPrefixes(peer, trusted) {
		return peer.String()
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		a = a.Unmap()
		if !inPrefixes(a, trusted) || i == 0 {
			return a.String()
		}
	}
	for _, h := range []string{"X-Real-IP", "CF-Connecting-IP"} {
		if a, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get(h))); err == nil {
			return a.Unmap().String()
		}
	}
	return peer.String()
}

func inPrefixes(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
