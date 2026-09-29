package webproxy

import (
	"context"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
)

// termsteadSessionCookie is internal/auth.CookieName (not imported: webproxy must not depend on auth). It is removed
// from every proxied request.
const termsteadSessionCookie = "termstead_session"

// reqMode describes how the current proxied request reached Termstead.
type reqMode struct {
	path   bool   // path mode (/proxy/<id>-<key>/…)
	prefix string // path-mode prefix ("" in host mode)
	origin string // browser-visible origin of the proxied content (scheme://host[:port])
	secure bool   // the proxied origin is a secure context (https or *.localhost)
	bridge bool   // inject the navigation bridge into HTML documents
	// nullOrigin: a path-mode request from the sandboxed (opaque-origin) page itself — fetch / XHR from it are
	// cross-origin requests that need CORS.
	nullOrigin bool
}

type reqModeKey struct{}

func modeOf(r *http.Request) reqMode {
	m, _ := r.Context().Value(reqModeKey{}).(reqMode)
	return m
}

// Security headers Termstead's pre-routing middleware sets on every response; proxied responses carry the upstream's.
var termsteadHeaders = []string{
	"Content-Security-Policy", "X-Frame-Options", "Cross-Origin-Opener-Policy", "Cross-Origin-Resource-Policy",
	"Cross-Origin-Embedder-Policy", "Permissions-Policy", "Referrer-Policy", "Strict-Transport-Security",
	"X-Xss-Protection", "X-Content-Type-Options",
}

// dispatch is the pre-routing middleware: requests for a proxy origin (host mode) or /proxy/… (path mode) are proxied;
// every other request continues through Termstead's router, with the SPA's frame-src extended to the proxy origins.
func (s *Service) dispatch(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		r := c.Request()
		if id, ok := s.matchHost(r.Host); ok {
			s.serveHost(c.Response(), r, id)
			return nil
		}
		if strings.HasPrefix(r.URL.Path, "/proxy/") {
			s.servePath(c.Response(), r)
			return nil
		}
		s.extendFrameSrc(c.Response().Header())
		return next(c)
	}
}

// matchHost extracts the proxy id from "p-<id>.localhost[:port]" or "p-<id>.<hostSuffix>[:port]".
func (s *Service) matchHost(hostport string) (string, bool) {
	host, _ := splitHostPortLoose(hostport)
	if !strings.HasPrefix(host, "p-") {
		return "", false
	}
	dot := strings.IndexByte(host, '.')
	if dot < 0 {
		return "", false
	}
	id, domain := host[2:dot], host[dot+1:]
	if !validID(id) {
		return "", false
	}
	if domain == "localhost" {
		return id, true
	}
	if suf := s.currentSettings().HostSuffix; suf != "" && domain == suf {
		return id, true
	}
	return "", false
}

// validID reports whether id looks like model.NewID() (20 lowercase base32 characters).
func validID(id string) bool {
	if len(id) != 20 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

// extendFrameSrc lets the SPA frame proxy origins: Termstead's CSP allows `frame-src 'self' blob:` only.
func (s *Service) extendFrameSrc(h http.Header) {
	csp := h.Get("Content-Security-Policy")
	if csp == "" || !strings.Contains(csp, "frame-src") {
		return
	}
	extra := " http://*.localhost:* https://*.localhost:*"
	if suf := s.currentSettings().HostSuffix; suf != "" {
		extra += " http://*." + suf + ":* https://*." + suf + ":* http://*." + suf + " https://*." + suf
	}
	parts := strings.Split(csp, ";")
	for i, d := range parts {
		if f := strings.Fields(d); len(f) > 0 && strings.EqualFold(f[0], "frame-src") {
			parts[i] = strings.TrimRight(d, " ") + extra
		}
	}
	h.Set("Content-Security-Policy", strings.Join(parts, ";"))
}

func clearTermsteadHeaders(h http.Header, keepNosniff bool) {
	for _, k := range termsteadHeaders {
		if keepNosniff && k == "X-Content-Type-Options" {
			continue
		}
		h.Del(k)
	}
}

func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// serveHost handles a request for a host-mode proxy origin.
func (s *Service) serveHost(w http.ResponseWriter, r *http.Request, id string) {
	clearTermsteadHeaders(w.Header(), false)
	scheme := requestScheme(r)
	origin := scheme + "://" + strings.ToLower(r.Host)
	host, _ := splitHostPortLoose(r.Host)
	secure := scheme == "https" || isLoopbackHost(host)
	p := s.get(id)
	if p == nil {
		writePage(w, r, http.StatusGone, page{
			Code: "gone", Title: "This web page is closed",
			Message: "The Termstead web proxy for this page was closed (idle, or Termstead restarted). Reopen it from Termstead.",
			Origins: []string{"*"},
		})
		return
	}
	if err := s.checkOwner(p); err != nil {
		s.remove(p, "the owner's account is no longer active")
		writePage(w, r, http.StatusGone, page{Code: "gone", Title: "This web page is closed", Message: "The proxy was closed.", Origins: []string{"*"}})
		return
	}

	// One-time token → browser session cookie, then a redirect to the clean URL.
	if q := r.URL.Query(); q.Has(tokenParam) && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		tok := q.Get(tokenParam)
		ok := p.consumeToken(tok)
		if !ok {
			if ck, err := r.Cookie(cookieName); err == nil && p.validCookie(ck.Value) {
				ok = true // a reload of an already used entry URL in the same browser
			}
		} else {
			http.SetCookie(w, proxyCookie(p.newCookie(), secure))
		}
		if ok {
			q.Del(tokenParam)
			u := *r.URL
			u.RawQuery = q.Encode()
			if u.RawQuery == "" {
				u.ForceQuery = false
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
			http.Redirect(w, r, localRedirect(u.RequestURI()), http.StatusFound)
			return
		}
		p.authFailed(w, r)
		return
	}
	if ck, err := r.Cookie(cookieName); err != nil || !p.validCookie(ck.Value) {
		p.authFailed(w, r)
		return
	}
	p.serve(w, r, reqMode{origin: origin, secure: secure})
}

// localRedirect keeps a redirect target on the current origin: "//host/…" and "/\host/…" are network-path references
// browsers would follow to another host (open redirect), so leading slashes / backslashes collapse to one "/".
func localRedirect(uri string) string {
	if !strings.HasPrefix(uri, "/") {
		return "/" + uri
	}
	return "/" + strings.TrimLeft(uri, "/\\")
}

// proxyCookie is the host-only browser-session cookie of a host-mode proxy. On a secure context it is SameSite=None
// + Partitioned so it works inside Termstead's (cross-site) iframe, even where third-party cookies are blocked.
func proxyCookie(v string, secure bool) *http.Cookie {
	ck := &http.Cookie{Name: cookieName, Value: v, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode}
	if secure {
		ck.Secure, ck.SameSite, ck.Partitioned = true, http.SameSiteNoneMode, true
	}
	return ck
}

func (p *Proxy) authFailed(w http.ResponseWriter, r *http.Request) {
	writePage(w, r, http.StatusUnauthorized, page{
		Code: "auth", Title: "Open this page from Termstead",
		Message: "This web proxy link is only valid for the Termstead tab that opened it. If you see this inside Termstead, " +
			"your browser may block the proxy's cookie in embedded pages: use \"Open in new window\".",
		Origins: p.origins(), Frame: p.origins(),
	})
}

// servePath handles /proxy/<id>-<key>/… (path mode).
func (s *Service) servePath(w http.ResponseWriter, r *http.Request) {
	clearTermsteadHeaders(w.Header(), true)
	rest := strings.TrimPrefix(r.URL.EscapedPath(), "/proxy/")
	capSeg, tail, hasSlash := strings.Cut(rest, "/")
	id, key, _ := strings.Cut(capSeg, "-")
	var p *Proxy
	if validID(id) {
		p = s.get(id)
	}
	if p == nil || !p.checkPathKey(key) || !s.currentSettings().pathModeAllowed() {
		writePage(w, r, http.StatusNotFound, page{
			Code: "gone", Title: "This web page is closed", Sandbox: true, Origins: []string{"*"},
			Message: "The Termstead web proxy for this page is closed or the link is invalid. Reopen it from Termstead.",
		})
		return
	}
	if err := s.checkOwner(p); err != nil {
		s.remove(p, "the owner's account is no longer active")
		writePage(w, r, http.StatusGone, page{Code: "gone", Title: "This web page is closed", Sandbox: true, Origins: []string{"*"}})
		return
	}
	prefix := "/proxy/" + capSeg
	if !hasSlash {
		u := prefix + "/"
		if r.URL.RawQuery != "" {
			u += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, u, http.StatusFound)
		return
	}
	_ = tail
	scheme := requestScheme(r)
	host, _ := splitHostPortLoose(r.Host)
	p.serve(w, r, reqMode{path: true, prefix: prefix, origin: scheme + "://" + strings.ToLower(r.Host),
		secure: scheme == "https" || isLoopbackHost(host)})
}

// serve proxies one request.
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, m reqMode) {
	if p.isClosed() {
		writePage(w, r, http.StatusGone, page{Code: "gone", Title: "This web page is closed", Sandbox: m.path, Origins: []string{"*"}})
		return
	}
	p.active.Add(1)
	p.touch()
	defer func() {
		p.touch()
		p.active.Add(-1)
	}()
	m.bridge = wantsBridge(r)
	if m.path && r.Header.Get("Origin") == "null" {
		m.nullOrigin = true
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			writePreflight(w, r)
			return
		}
	}
	ctx := context.WithValue(r.Context(), reqModeKey{}, m)
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

// writePreflight answers a CORS preflight of the sandboxed path-mode page for its own proxy URLs (the capability key
// in the path is the credential; no cookies are involved, so credentials are not allowed).
func writePreflight(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "null")
	if m := r.Header.Get("Access-Control-Request-Method"); isToken(m) {
		h.Set("Access-Control-Allow-Methods", m)
	}
	if hs := r.Header.Get("Access-Control-Request-Headers"); hs != "" && len(hs) <= 2048 && isHeaderList(hs) {
		h.Set("Access-Control-Allow-Headers", hs)
	}
	h.Set("Access-Control-Max-Age", "600")
	h.Add("Vary", "Origin")
	w.WriteHeader(http.StatusNoContent)
}

func isToken(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return false
		}
	}
	return true
}

func isHeaderList(s string) bool {
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" && !isToken(f) {
			return false
		}
	}
	return true
}

// wantsBridge reports whether the request is a document navigation whose HTML gets the navigation bridge (never XHR
// fragments).
func wantsBridge(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Dest") {
	case "document", "iframe", "frame":
		return true
	case "":
		return r.Header.Get("X-Requested-With") == "" && strings.Contains(r.Header.Get("Accept"), "text/html")
	}
	return false
}
