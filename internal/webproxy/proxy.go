package webproxy

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Reserved names on proxied origins.
const (
	tokenParam   = "__astraterm_proxy_token"
	cookieName   = "__astraterm_proxy"
	reservedPath = "/__astraterm/"
	bridgePath   = reservedPath + "bridge.js"
)

// Proxy is one live reverse proxy to an upstream origin.
type Proxy struct {
	s         *Service
	id        string
	kind      string // web | xpra
	spec      Spec
	rt        *route
	createdAt time.Time

	transport *http.Transport
	rp        *httputil.ReverseProxy

	last   atomic.Int64 // unix nanos of the last request
	active atomic.Int32 // in-flight requests (upgraded WebSockets stay in flight)

	mu            sync.Mutex
	owner         *model.User
	ownerErrV     error
	userCheckedAt time.Time
	uiOrigins     []string
	tokens        map[string]time.Time // one-time entry tokens → expiry
	cookies       map[string]time.Time // browser sessions (cookie values) → expiry
	pathKey       string               // capability of /proxy/<id>-<key>/ (created when first handed out)
	conns         map[net.Conn]struct{}
	onClose       []func()
	closed        bool
	extra         map[string]any // kind-specific info (xpra display, command)
}

// Info is the JSON view of a proxy.
type Info struct {
	ID           string         `json:"id"`
	Kind         string         `json:"kind"`
	Title        string         `json:"title"`
	Target       Target         `json:"target"`
	Via          Via            `json:"via"`
	InsecureTLS  bool           `json:"insecureTls"`
	ConnectionID string         `json:"connectionId,omitempty"`
	SessionID    string         `json:"sessionId,omitempty"`
	TunnelID     string         `json:"tunnelId,omitempty"`
	Spec         Spec           `json:"spec"`
	CreatedAt    time.Time      `json:"createdAt"`
	LastUsedAt   time.Time      `json:"lastUsedAt"`
	Active       int            `json:"active"`
	Extra        map[string]any `json:"extra,omitempty"`
	// Entry fields (create / url responses only).
	URL  string `json:"url,omitempty"`
	Mode string `json:"mode,omitempty"` // host | path
	Base string `json:"base,omitempty"` // proxied origin (host mode) or path prefix (path mode)
}

func newProxy(s *Service, user *model.User, kind string, sp Spec, rt *route) *Proxy {
	p := &Proxy{
		s: s, id: model.NewID(), kind: kind, spec: sp, rt: rt, createdAt: s.now(),
		owner: user, userCheckedAt: time.Now(), tokens: map[string]time.Time{}, cookies: map[string]time.Time{},
		conns: map[net.Conn]struct{}{},
	}
	p.last.Store(s.now().UnixNano())
	p.transport = p.newTransport()
	p.rp = &httputil.ReverseProxy{
		Rewrite:        p.rewriteRequest,
		Transport:      p.transport,
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.errorHandler,
		FlushInterval:  -1, // stream (SSE, long polls, chunked progress) without buffering
		ErrorLog:       slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	return p
}

func (p *Proxy) newTransport() *http.Transport {
	t := p.rt.target
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if net.ParseIP(t.Host) == nil {
		tc.ServerName = t.Host
	}
	if p.rt.insecure {
		// The user explicitly accepted an unverified certificate (self-signed appliances); allow legacy TLS too.
		tc.InsecureSkipVerify = true //nolint:gosec // explicit per-proxy user choice
		tc.MinVersion = tls.VersionTLS10
	}
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			c, err := p.rt.dial(context.WithValue(ctx, proxyCtxKey{}, p))
			if err != nil {
				return nil, err
			}
			return p.track(c), nil
		},
		TLSClientConfig:       tc,
		TLSHandshakeTimeout:   20 * time.Second,
		DisableCompression:    true, // pass Accept-Encoding through untouched
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		Proxy:                 nil,
	}
}

// ---- state --------------------------------------------------------------------------------------------------------

func (p *Proxy) lastUsed() time.Time { return time.Unix(0, p.last.Load()) }
func (p *Proxy) inflight() int32     { return p.active.Load() }
func (p *Proxy) touch()              { p.last.Store(p.s.now().UnixNano()) }

func (p *Proxy) currentOwner() *model.User {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.owner
}

func (p *Proxy) setOwner(u *model.User) {
	p.mu.Lock()
	p.owner, p.ownerErrV = u, nil
	p.mu.Unlock()
}

func (p *Proxy) setOwnerErr(err error) {
	p.mu.Lock()
	p.ownerErrV = err
	p.mu.Unlock()
}

func (p *Proxy) ownerErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ownerErrV
}

func (p *Proxy) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// addCloser registers fn to run when the proxy closes (runs at once when already closed).
func (p *Proxy) addCloser(fn func()) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		fn()
		return
	}
	p.onClose = append(p.onClose, fn)
	p.mu.Unlock()
}

func (p *Proxy) setExtra(key string, v any) {
	p.mu.Lock()
	if p.extra == nil {
		p.extra = map[string]any{}
	}
	p.extra[key] = v
	p.mu.Unlock()
}

// shutdown closes upstream streams (ending proxied WebSockets), idle connections and runs the close hooks.
func (p *Proxy) shutdown() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	conns := p.conns
	p.conns = map[net.Conn]struct{}{}
	hooks := p.onClose
	p.onClose = nil
	p.tokens, p.cookies = map[string]time.Time{}, map[string]time.Time{}
	p.mu.Unlock()
	for c := range conns {
		c.Close()
	}
	p.transport.CloseIdleConnections()
	for _, fn := range hooks {
		fn()
	}
}

type trackedConn struct {
	net.Conn
	p    *Proxy
	once sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() {
		c.p.mu.Lock()
		delete(c.p.conns, c.Conn)
		c.p.mu.Unlock()
	})
	return c.Conn.Close()
}

func (c *trackedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (p *Proxy) track(c net.Conn) net.Conn {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		c.Close()
		return c
	}
	p.conns[c] = struct{}{}
	p.mu.Unlock()
	return &trackedConn{Conn: c, p: p}
}

func (p *Proxy) info() Info {
	p.mu.Lock()
	var extra map[string]any
	if len(p.extra) > 0 {
		extra = make(map[string]any, len(p.extra))
		for k, v := range p.extra {
			extra[k] = v
		}
	}
	p.mu.Unlock()
	return Info{
		ID: p.id, Kind: p.kind, Title: p.rt.title, Target: p.rt.target, Via: p.rt.via, InsecureTLS: p.rt.insecure,
		ConnectionID: p.spec.ConnectionID, SessionID: p.spec.SessionID, TunnelID: p.spec.TunnelID, Spec: p.spec,
		CreatedAt: p.createdAt, LastUsedAt: p.lastUsed(), Active: int(p.inflight()), Extra: extra,
	}
}

// ---- credentials --------------------------------------------------------------------------------------------------

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// newToken issues a one-time entry token.
func (p *Proxy) newToken() string {
	tok := randomToken(32)
	now := time.Now()
	p.mu.Lock()
	for k, exp := range p.tokens {
		if now.After(exp) {
			delete(p.tokens, k)
		}
	}
	if len(p.tokens) >= maxTokensPerProxy {
		var oldest string
		var oldestExp time.Time
		for k, exp := range p.tokens {
			if oldest == "" || exp.Before(oldestExp) {
				oldest, oldestExp = k, exp
			}
		}
		delete(p.tokens, oldest)
	}
	p.tokens[tok] = now.Add(tokenTTL)
	p.mu.Unlock()
	return tok
}

// consumeToken validates and burns a one-time token.
func (p *Proxy) consumeToken(tok string) bool {
	if tok == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	exp, ok := p.tokens[tok]
	if !ok {
		return false
	}
	delete(p.tokens, tok)
	return time.Now().Before(exp)
}

// newCookie starts a browser session and returns its secret.
func (p *Proxy) newCookie() string {
	v := randomToken(32)
	now := time.Now()
	p.mu.Lock()
	for k, exp := range p.cookies {
		if now.After(exp) {
			delete(p.cookies, k)
		}
	}
	if len(p.cookies) >= maxCookieSessions {
		var oldest string
		var oldestExp time.Time
		for k, exp := range p.cookies {
			if oldest == "" || exp.Before(oldestExp) {
				oldest, oldestExp = k, exp
			}
		}
		delete(p.cookies, oldest)
	}
	p.cookies[v] = now.Add(cookieSessionTTL)
	p.mu.Unlock()
	return v
}

// validCookie checks (and slides) a browser session.
func (p *Proxy) validCookie(v string) bool {
	if v == "" {
		return false
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	exp, ok := p.cookies[v]
	if !ok || now.After(exp) {
		delete(p.cookies, v)
		return false
	}
	p.cookies[v] = now.Add(cookieSessionTTL)
	return true
}

// ensurePathKey returns the path-mode capability key, creating it on first use.
func (p *Proxy) ensurePathKey() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pathKey == "" {
		p.pathKey = randomToken(24)
	}
	return p.pathKey
}

func (p *Proxy) checkPathKey(k string) bool {
	p.mu.Lock()
	key := p.pathKey
	p.mu.Unlock()
	return key != "" && subtle.ConstantTimeCompare([]byte(k), []byte(key)) == 1
}

// addUIOrigin records an origin of the AstraTerm UI allowed to frame the proxy and talk to its bridge.
func (p *Proxy) addUIOrigin(o string) {
	if o == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, x := range p.uiOrigins {
		if x == o {
			return
		}
	}
	if len(p.uiOrigins) >= 8 {
		p.uiOrigins = p.uiOrigins[1:]
	}
	p.uiOrigins = append(p.uiOrigins, o)
}

func (p *Proxy) origins() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.uiOrigins...)
}

// ---- entry URLs ---------------------------------------------------------------------------------------------------

// entry describes how the requesting browser reaches a proxy.
type entry struct {
	Mode string // host | path
	Base string // host: scheme://p-<id>.<domain>[:port]; path: /proxy/<id>-<key>
	URL  string // entry URL (host mode: with a one-time token)
}

// errNoMode is returned when neither host nor path mode is available for the requesting UI.
var errNoMode = errors.New("no proxy mode")

// entryFor builds the entry URL for a browser whose UI request was r. mode "" = automatic, "path" forces path mode.
func (s *Service) entryFor(p *Proxy, r *http.Request, scheme, path, mode string) (entry, error) {
	if path == "" {
		path = p.rt.target.Path
	}
	path, err := cleanTargetPath(path)
	if err != nil {
		return entry{}, err
	}
	st := s.currentSettings()
	host, port := splitHostPortLoose(r.Host)
	if mode != "path" {
		domain := ""
		switch {
		case isLoopbackHost(host):
			domain = "localhost"
		case st.HostSuffix != "":
			domain = st.HostSuffix
		}
		if domain != "" {
			base := scheme + "://p-" + p.id + "." + domain
			if port != "" {
				base += ":" + port
			}
			return entry{Mode: "host", Base: base, URL: base + withQuery(path, tokenParam+"="+p.newToken())}, nil
		}
	}
	if !st.pathModeAllowed() {
		return entry{}, errNoMode
	}
	base := "/proxy/" + p.id + "-" + p.ensurePathKey()
	return entry{Mode: "path", Base: base, URL: base + path}, nil
}

// withQuery appends a query parameter to a path (before any fragment).
func withQuery(path, kv string) string {
	frag := ""
	if i := strings.IndexByte(path, '#'); i >= 0 {
		path, frag = path[:i], path[i:]
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + kv + frag
}

func splitHostPortLoose(hostport string) (host, port string) {
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(strings.Trim(h, "[]")), p
	}
	return strings.ToLower(strings.Trim(hostport, "[]")), ""
}

// isLoopbackHost reports whether a browser-facing host name is local: localhost, *.localhost or a loopback IP.
func isLoopbackHost(h string) bool {
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Unmap().IsLoopback()
	}
	return false
}

// uiOrigin returns the origin of the AstraTerm UI that sent an API request (the Origin header when it is a plausible
// web origin, else scheme://Host).
func uiOrigin(r *http.Request, scheme string) string {
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		if u, err := url.Parse(o); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.Path == "" {
			return u.Scheme + "://" + strings.ToLower(u.Host)
		}
	}
	return scheme + "://" + strings.ToLower(r.Host)
}

func normalizeSuffix(s string) string {
	s = strings.Trim(strings.ToLower(strings.TrimSpace(s)), ".")
	if s == "" || validHost(s) != nil || !strings.Contains(s, ".") && s != "localhost" {
		return ""
	}
	return s
}
