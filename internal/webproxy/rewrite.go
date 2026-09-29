package webproxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/html"

	"github.com/plzcloseyoureyes/astraterm/internal/netguard"
)

// sandboxPolicy is applied to every path-mode response: the page runs in an opaque origin (no allow-same-origin), so
// it cannot use AstraTerm's cookies, storage or API although it is served from AstraTerm's origin.
const sandboxPolicy = "sandbox allow-scripts allow-forms allow-popups allow-popups-to-escape-sandbox allow-modals " +
	"allow-downloads allow-pointer-lock allow-presentation allow-orientation-lock"

// ---- request ------------------------------------------------------------------------------------------------------

// rewriteRequest points the outgoing request at the upstream, keeping the proxy transparent: Host / Origin / Referer
// are the upstream's, AstraTerm's cookies and headers are removed, and the path-mode prefix is stripped.
func (p *Proxy) rewriteRequest(pr *httputil.ProxyRequest) {
	m := modeOf(pr.In)
	t := p.rt.target
	esc := pr.In.URL.EscapedPath()
	if m.path {
		esc = strings.TrimPrefix(esc, m.prefix)
	}
	if esc == "" {
		esc = "/"
	}
	out := pr.Out
	out.URL.Scheme, out.URL.Host = t.Scheme, t.authority()
	if dec, err := url.PathUnescape(esc); err == nil {
		out.URL.Path, out.URL.RawPath = dec, esc
	} else {
		out.URL.Path, out.URL.RawPath = esc, ""
	}
	out.URL.RawQuery = dropQueryParam(pr.In.URL.RawQuery, tokenParam)
	out.Host = t.authority()

	h := out.Header
	h.Del("X-AstraTerm")
	filterCookies(h)
	if o := h.Get("Origin"); o != "" && (o == "null" || strings.EqualFold(o, m.origin)) {
		h.Set("Origin", t.Origin())
	}
	if ref := h.Get("Referer"); ref != "" {
		base := m.origin + m.prefix
		switch {
		case strings.HasPrefix(ref, base+"/") || ref == base:
			h.Set("Referer", t.Origin()+strings.TrimPrefix(ref, base))
		case strings.HasPrefix(ref, m.origin):
			h.Del("Referer") // a AstraTerm page (path mode) — never leak AstraTerm URLs upstream
		}
	}
	if strings.Contains(h.Get("Accept"), "text/html") && h.Get("Accept-Encoding") != "" {
		h.Set("Accept-Encoding", "gzip") // HTML is rewritten: only encodings we can decode
	}
	if p.rt.auth != "" && h.Get("Authorization") == "" {
		h.Set("Authorization", p.rt.auth)
	}
}

// filterCookies removes AstraTerm's session cookie and the proxy cookie from the Cookie header(s).
func filterCookies(h http.Header) {
	vals := h.Values("Cookie")
	if len(vals) == 0 {
		return
	}
	var keep []string
	for _, v := range vals {
		for _, part := range strings.Split(v, ";") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			name, _, _ := strings.Cut(part, "=")
			if name == cookieName || name == astratermSessionCookie {
				continue
			}
			keep = append(keep, part)
		}
	}
	h.Del("Cookie")
	if len(keep) > 0 {
		h.Set("Cookie", strings.Join(keep, "; "))
	}
}

func dropQueryParam(raw, name string) string {
	if raw == "" || !strings.Contains(raw, name) {
		return raw
	}
	parts := strings.Split(raw, "&")
	out := parts[:0]
	for _, part := range parts {
		k, _, _ := strings.Cut(part, "=")
		if k == name {
			continue
		}
		out = append(out, part)
	}
	return strings.Join(out, "&")
}

// ---- response -----------------------------------------------------------------------------------------------------

// modifyResponse adapts upstream responses to the proxy origin: redirects and cookies, framing (only AstraTerm may
// frame the page), path-mode sandboxing and the HTML bridge / link rewriting.
func (p *Proxy) modifyResponse(resp *http.Response) error {
	m := modeOf(resp.Request)
	h := resp.Header
	for _, k := range []string{"Location", "Content-Location"} {
		if v := h.Get(k); v != "" {
			h.Set(k, p.mapURL(v, m, true))
		}
	}
	if v := h.Get("Refresh"); v != "" {
		h.Set("Refresh", p.mapRefresh(v, m))
	}
	if cs := h.Values("Set-Cookie"); len(cs) > 0 {
		h.Del("Set-Cookie")
		for _, c := range cs {
			if nc := rewriteSetCookie(c, m); nc != "" {
				h.Add("Set-Cookie", nc)
			}
		}
	}
	h.Del("X-Frame-Options")
	for _, k := range originScopedHeaders {
		h.Del(k)
	}
	if m.path {
		// Path mode serves the upstream on AstraTerm's own origin: nothing the upstream says may configure that origin.
		for _, k := range pathModeDeniedHeaders {
			h.Del(k)
		}
		if m.nullOrigin {
			// fetch / XHR of the sandboxed page itself (opaque origin → cross-origin to its own URLs).
			h.Set("Access-Control-Allow-Origin", "null")
			h.Set("Access-Control-Expose-Headers", "*")
			h.Add("Vary", "Origin")
		}
	}

	inject := m.bridge && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified &&
		resp.Request.Method != http.MethodHead && isHTML(h.Get("Content-Type"))
	nonce := ""
	if inject {
		nonce = randomToken(18)
	}
	fixCSPHeaders(h, nonce)
	if m.path {
		h.Add("Content-Security-Policy", sandboxPolicy+"; frame-ancestors 'self'")
	} else {
		h.Add("Content-Security-Policy", "frame-ancestors "+frameAncestors(p.origins()))
	}

	if !isHTML(h.Get("Content-Type")) || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified ||
		resp.Request.Method == http.MethodHead || resp.Body == nil || resp.Body == http.NoBody {
		return nil
	}
	body, ok := decodedBody(resp)
	if !ok {
		return nil // an encoding we cannot decode: pass through untouched (no bridge / link rewriting)
	}
	var script string
	if inject {
		script = bridgeScript(bridgeConfig{ID: p.id, Origins: p.origins(), Prefix: m.prefix, Kind: p.kind}, nonce)
	}
	pr, pw := io.Pipe()
	go func() {
		defer body.Close()
		pw.CloseWithError(rewriteHTML(pw, body, htmlRewrite{p: p, m: m, script: script, nonce: nonce}))
	}()
	resp.Body = pr
	resp.ContentLength = -1
	h.Del("Content-Length")
	h.Del("Content-Encoding")
	if et := h.Get("ETag"); et != "" && !strings.HasPrefix(et, "W/") {
		h.Set("ETag", "W/"+et)
	}
	return nil
}

// originScopedHeaders configure the whole origin (not the response) for a long time: an alternative service for the
// proxy origin could point the browser elsewhere, so they never pass.
var originScopedHeaders = []string{"Alt-Svc", "Public-Key-Pins", "Public-Key-Pins-Report-Only", "Expect-CT"}

// pathModeDeniedHeaders would configure AstraTerm's own origin in path mode: wipe its storage, pin HSTS, register
// reporting endpoints that receive reports about AstraTerm's own requests (NEL), service-worker scope, client hints or
// login status.
var pathModeDeniedHeaders = []string{
	"Clear-Site-Data", "Strict-Transport-Security", "Service-Worker-Allowed", "NEL", "Report-To", "Reporting-Endpoints",
	"Accept-CH", "Critical-CH", "Set-Login", "Origin-Agent-Cluster", "Access-Control-Allow-Origin",
	"Access-Control-Allow-Credentials", "Access-Control-Allow-Headers", "Access-Control-Allow-Methods",
	"Access-Control-Expose-Headers", "Access-Control-Allow-Private-Network", "Timing-Allow-Origin", "Set-Cookie2",
}

func frameAncestors(origins []string) string {
	if len(origins) == 0 {
		return "'self'"
	}
	return strings.Join(origins, " ")
}

func isHTML(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	}
	return mt == "text/html" || mt == "application/xhtml+xml"
}

// decodedBody returns the identity-encoded body (gzip / deflate are decoded). Closing it closes the upstream body as
// it is now (the caller replaces resp.Body afterwards).
func decodedBody(resp *http.Response) (io.ReadCloser, bool) {
	orig := resp.Body
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "", "identity":
		return orig, true
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(orig)
		if err != nil {
			return nil, false
		}
		return readCloser{Reader: zr, close: func() error { zr.Close(); return orig.Close() }}, true
	case "deflate":
		fr := flate.NewReader(orig)
		return readCloser{Reader: fr, close: func() error { fr.Close(); return orig.Close() }}, true
	}
	return nil, false
}

type readCloser struct {
	io.Reader
	close func() error
}

func (r readCloser) Close() error { return r.close() }

// mapURL maps an upstream URL to the proxy: absolute URLs of the upstream origin become proxy paths, and in path mode
// absolute paths get the prefix. Other URLs are returned unchanged. header selects Location semantics (relative
// references stay relative either way).
func (p *Proxy) mapURL(raw string, m reqMode, header bool) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return raw
	}
	t := p.rt.target
	switch {
	case strings.HasPrefix(s, "//"):
		if u, err := url.Parse(t.Scheme + ":" + s); err == nil && sameOrigin(u, t) {
			return m.prefix + requestURI(u)
		}
		return raw
	case strings.HasPrefix(s, "/"):
		if m.path && !strings.HasPrefix(s, m.prefix+"/") {
			return m.prefix + s
		}
		return raw
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" {
		return raw
	}
	if (u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "ws" || u.Scheme == "wss") && sameOrigin(u, t) {
		return m.prefix + requestURI(u)
	}
	_ = header
	return raw
}

func requestURI(u *url.URL) string {
	s := u.EscapedPath()
	if s == "" {
		s = "/"
	}
	if u.RawQuery != "" || u.ForceQuery {
		s += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		s += "#" + u.EscapedFragment()
	}
	return s
}

// sameOrigin reports whether u points at the upstream origin (ws/wss count as http/https).
func sameOrigin(u *url.URL, t Target) bool {
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	}
	if scheme != t.Scheme || !strings.EqualFold(strings.Trim(u.Hostname(), "[]"), t.Host) {
		return false
	}
	port := defaultPortFor(scheme)
	if ps := u.Port(); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil {
			return false
		}
		port = n
	}
	return port == t.Port
}

// mapRefresh rewrites the URL of a Refresh header / meta refresh ("5; url=/x").
func (p *Proxy) mapRefresh(v string, m reqMode) string {
	i := strings.Index(strings.ToLower(v), "url=")
	if i < 0 {
		return v
	}
	u := strings.Trim(v[i+4:], `'" `)
	return v[:i+4] + p.mapURL(u, m, true)
}

// rewriteSetCookie adapts an upstream cookie to the proxy origin: Domain is dropped (host-only), path-mode paths get
// the prefix, and on secure contexts cookies become SameSite=None; Secure; Partitioned so the application's session
// works inside AstraTerm's iframe. AstraTerm's own cookie names are refused.
func rewriteSetCookie(v string, m reqMode) string {
	parts := strings.Split(v, ";")
	nameVal := strings.TrimSpace(parts[0])
	name, _, ok := strings.Cut(nameVal, "=")
	name = strings.TrimSpace(name)
	if !ok || name == "" || name == cookieName || name == astratermSessionCookie {
		return ""
	}
	out := []string{nameVal}
	sameSite := ""
	for _, a := range parts[1:] {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		k, val, _ := strings.Cut(a, "=")
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "domain", "secure", "partitioned":
			continue
		case "samesite":
			sameSite = strings.TrimSpace(val)
			continue
		case "path":
			if m.path {
				pv := strings.TrimSpace(val)
				if !strings.HasPrefix(pv, "/") {
					pv = "/"
				}
				a = "Path=" + m.prefix + pv
			}
		}
		out = append(out, a)
	}
	if m.secure {
		out = append(out, "Secure", "SameSite=None", "Partitioned")
	} else if sameSite != "" && !strings.EqualFold(sameSite, "none") {
		out = append(out, "SameSite="+sameSite)
	}
	return strings.Join(out, "; ")
}

// ---- Content-Security-Policy --------------------------------------------------------------------------------------

// fixCSPHeaders removes frame-ancestors from the upstream policies (AstraTerm adds its own) and, when nonce is set,
// allows the injected bridge script.
func fixCSPHeaders(h http.Header, nonce string) {
	vals := h.Values("Content-Security-Policy")
	if len(vals) == 0 {
		return
	}
	h.Del("Content-Security-Policy")
	for _, v := range vals {
		if nv := fixCSP(v, nonce, true); nv != "" {
			h.Add("Content-Security-Policy", nv)
		}
	}
}

// fixCSP rewrites one header value (possibly several comma-separated policies).
func fixCSP(v, nonce string, dropFrameAncestors bool) string {
	var policies []string
	for _, pol := range strings.Split(v, ",") {
		var dirs []string
		for _, d := range strings.Split(pol, ";") {
			f := strings.Fields(d)
			if len(f) == 0 {
				continue
			}
			if dropFrameAncestors && strings.EqualFold(f[0], "frame-ancestors") {
				continue
			}
			dirs = append(dirs, strings.Join(f, " "))
		}
		if nonce != "" {
			dirs = allowNonce(dirs, nonce)
		}
		if len(dirs) > 0 {
			policies = append(policies, strings.Join(dirs, "; "))
		}
	}
	return strings.Join(policies, ", ")
}

// allowNonce makes the directive governing inline <script> elements accept 'nonce-<nonce>' without widening it: a
// nonce is added unless the directive already allows every inline script ('unsafe-inline' without nonces / hashes,
// where adding one would disable 'unsafe-inline').
func allowNonce(dirs []string, nonce string) []string {
	idx := -1
	for _, name := range []string{"script-src-elem", "script-src", "default-src"} {
		for i, d := range dirs {
			if strings.EqualFold(strings.Fields(d)[0], name) {
				idx = i
				break
			}
		}
		if idx >= 0 {
			break
		}
	}
	if idx < 0 {
		return dirs
	}
	f := strings.Fields(dirs[idx])
	unsafeInline, keyed := false, false
	var kept []string
	for _, tok := range f[1:] {
		lt := strings.ToLower(tok)
		switch {
		case lt == "'unsafe-inline'":
			unsafeInline = true
		case strings.HasPrefix(lt, "'nonce-"), strings.HasPrefix(lt, "'sha256-"), strings.HasPrefix(lt, "'sha384-"),
			strings.HasPrefix(lt, "'sha512-"), lt == "'strict-dynamic'":
			keyed = true
		}
		if lt != "'none'" {
			kept = append(kept, tok)
		}
	}
	if unsafeInline && !keyed {
		return dirs
	}
	dirs[idx] = f[0] + " " + strings.TrimSpace(strings.Join(kept, " ")+" 'nonce-"+nonce+"'")
	return dirs
}

// ---- HTML ---------------------------------------------------------------------------------------------------------

type htmlRewrite struct {
	p      *Proxy
	m      reqMode
	script string // bridge <script> element ("" = no injection)
	nonce  string
}

// urlAttrs are attributes holding one URL (checked for every element).
var urlAttrs = map[string]bool{"href": true, "src": true, "action": true, "formaction": true, "poster": true, "background": true}

// rewriteHTML streams src to w, injecting the bridge script at the start of <head> (or before the first body-level
// element) and mapping URL attributes (upstream-origin URLs → proxy paths; path mode also prefixes absolute paths).
func rewriteHTML(w io.Writer, src io.Reader, rw htmlRewrite) error {
	z := html.NewTokenizer(src)
	injected := rw.script == ""
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if err := z.Err(); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if !injected {
				_, err := io.WriteString(w, rw.script)
				return err
			}
			return nil
		}
		raw := z.Raw()
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			rawCopy := append([]byte(nil), raw...)
			tok := z.Token()
			name := tok.Data
			if !injected && name != "html" && name != "head" {
				if _, err := io.WriteString(w, rw.script); err != nil {
					return err
				}
				injected = true
			}
			if rw.rewriteTag(&tok) {
				if _, err := io.WriteString(w, tok.String()); err != nil {
					return err
				}
			} else if _, err := w.Write(rawCopy); err != nil {
				return err
			}
			if !injected && name == "head" {
				if _, err := io.WriteString(w, rw.script); err != nil {
					return err
				}
				injected = true
			}
		default:
			if _, err := w.Write(raw); err != nil {
				return err
			}
		}
	}
}

// rewriteTag maps URL attributes and meta refresh / CSP; it reports whether tok changed.
func (rw htmlRewrite) rewriteTag(tok *html.Token) bool {
	changed := false
	isMeta := tok.Data == "meta"
	var equiv string
	if isMeta {
		for _, a := range tok.Attr {
			if strings.EqualFold(a.Key, "http-equiv") {
				equiv = strings.ToLower(strings.TrimSpace(a.Val))
			}
		}
	}
	for i, a := range tok.Attr {
		if a.Namespace != "" {
			continue
		}
		key := strings.ToLower(a.Key)
		var nv string
		switch {
		case urlAttrs[key], key == "data" && tok.Data == "object":
			nv = rw.p.mapURL(a.Val, rw.m, false)
		case key == "srcset" || key == "imagesrcset":
			nv = rw.mapSrcset(a.Val)
		case isMeta && key == "content" && equiv == "refresh":
			nv = rw.p.mapRefresh(a.Val, rw.m)
		case isMeta && key == "content" && equiv == "content-security-policy":
			nv = fixCSP(a.Val, rw.nonce, false)
		default:
			continue
		}
		if nv != a.Val {
			tok.Attr[i].Val = nv
			changed = true
		}
	}
	return changed
}

func (rw htmlRewrite) mapSrcset(v string) string {
	items := strings.Split(v, ",")
	var b bytes.Buffer
	for i, it := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		trimmed := strings.TrimSpace(it)
		u, desc, _ := strings.Cut(trimmed, " ")
		lead := it[:len(it)-len(strings.TrimLeft(it, " \t\n"))]
		b.WriteString(lead)
		b.WriteString(rw.p.mapURL(u, rw.m, false))
		if desc != "" {
			b.WriteByte(' ')
			b.WriteString(desc)
		}
	}
	return b.String()
}

// ---- errors -------------------------------------------------------------------------------------------------------

// errorHandler renders upstream failures as a page inside the proxied origin (with the bridge reporting the error to
// the AstraTerm tab).
func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		return // the browser went away
	}
	m := modeOf(r)
	status := http.StatusBadGateway
	code, msg := classifyDialErr(err, p.rt.target)
	if _, ok := netguard.IsBlocked(err); ok {
		status = http.StatusForbidden
	} else if code == "upstream_timeout" {
		status = http.StatusGatewayTimeout
	}
	if isTLSError(err) {
		code = "tls"
		msg = "The site's certificate could not be verified: " + tlsReason(err)
	}
	p.s.log.Debug("proxy request failed", "proxy", p.id, "target", p.rt.target.HostPort(), "err", err)
	writePage(w, r, status, page{
		Code: code, Title: errorTitle(code), Message: msg, Target: p.rt.target.String(), Via: p.rt.via.Label,
		Origins: p.origins(), Frame: p.origins(), Sandbox: m.path, Prefix: m.prefix, ProxyID: p.id,
	})
}

func errorTitle(code string) string {
	switch code {
	case "tls":
		return "Certificate not trusted"
	case "upstream_refused":
		return "Connection refused"
	case "upstream_timeout":
		return "The site did not answer"
	case netguard.CodeBlocked:
		return "Blocked by the network policy"
	case "session_gone", "session_disconnected":
		return "SSH session not available"
	case "locked":
		return "AstraTerm is locked"
	}
	return "Cannot reach the site"
}

func isTLSError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "x509:") || strings.Contains(s, "tls:") || strings.Contains(s, "certificate")
}

func tlsReason(err error) string {
	s := err.Error()
	if i := strings.Index(s, "x509: "); i >= 0 {
		return s[i+len("x509: "):]
	}
	if i := strings.Index(s, "tls: "); i >= 0 {
		return s[i+len("tls: "):]
	}
	return s
}
