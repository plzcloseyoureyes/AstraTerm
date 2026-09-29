package webproxy_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/server/servertest"
)

const adminPass = "correct horse battery staple"

type proxyInfo struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	URL    string `json:"url"`
	Mode   string `json:"mode"`
	Base   string `json:"base"`
	Title  string `json:"title"`
	Target struct {
		Scheme string `json:"scheme"`
		Host   string `json:"host"`
		Port   int    `json:"port"`
		Path   string `json:"path"`
	} `json:"target"`
	Via struct {
		Kind  string `json:"kind"`
		Label string `json:"label"`
	} `json:"via"`
}

// upstream is a test web application recording what it received.
type upstream struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
}

func (u *upstream) last() *http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		return nil
	}
	return u.reqs[len(u.reqs)-1]
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "script-src 'self'; frame-ancestors 'none'")
		http.SetCookie(w, &http.Cookie{Name: "app", Value: "1", Path: "/", Domain: "127.0.0.1", SameSite: http.SameSiteLaxMode})
		fmt.Fprintf(w, `<!doctype html><html><head><title>Upstream</title></head><body><a href="%s/next">next</a><a href="/abs">abs</a></body></html>`, u.URL)
	})
	mux.HandleFunc("/gz", func(w http.ResponseWriter, r *http.Request) {
		// A large gzip-encoded document (the rewriter decodes, injects the bridge and streams it back).
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		fmt.Fprint(zw, "<!doctype html><html><head><title>Big</title></head><body>")
		for i := 0; i < 5000; i++ {
			fmt.Fprintf(zw, "<p>line %d</p>", i)
		}
		fmt.Fprint(zw, "</body></html>")
		zw.Close()
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, u.URL+"/target?x=1", http.StatusFound)
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"host": r.Host, "path": r.URL.RequestURI(), "cookie": r.Header.Get("Cookie"),
			"origin": r.Header.Get("Origin"), "referer": r.Header.Get("Referer"), "xastraterm": r.Header.Get("X-AstraTerm")})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			typ, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), typ, append([]byte("echo:"), data...)); err != nil {
				return
			}
		}
	})
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.reqs = append(u.reqs, r.Clone(context.Background()))
		u.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

// browser talks to a proxy origin: it connects to the test server but sends the proxy's Host header, with its own
// cookie jar keyed by the proxy origin (as a browser would).
type browser struct {
	t      *testing.T
	env    *servertest.Env
	client *http.Client
}

func newBrowser(t *testing.T, env *servertest.Env) *browser {
	return &browser{t: t, env: env, client: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

var cookieRe = func(resp *http.Response, name string) string {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// get requests rawURL (an http://p-<id>.localhost:<port>/… URL) through the test server.
func (b *browser) get(rawURL string, hdr http.Header) (*http.Response, string) {
	b.t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		b.t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", b.env.URL(u.RequestURI()), nil)
	req.Host = u.Host
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func create(t *testing.T, c *servertest.Client, body map[string]any) proxyInfo {
	t.Helper()
	var p proxyInfo
	c.MustJSON("POST", "/api/webproxy", body, &p)
	return p
}

// enter exchanges the entry URL's token and returns the proxy cookie.
func (b *browser) enter(p proxyInfo) string {
	b.t.Helper()
	resp, _ := b.get(p.URL, nil)
	if resp.StatusCode != http.StatusFound {
		b.t.Fatalf("token exchange: status %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); strings.Contains(loc, "__astraterm_proxy_token") {
		b.t.Fatalf("redirect keeps the token: %s", loc)
	}
	ck := cookieRe(resp, "__astraterm_proxy")
	if ck == "" {
		b.t.Fatal("no proxy cookie")
	}
	for _, c := range resp.Cookies() {
		if c.Name == "__astraterm_proxy" && (!c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteNoneMode || !c.Partitioned) {
			b.t.Fatalf("cookie attributes: %+v", c)
		}
	}
	return ck
}

func withCookie(v string) http.Header {
	return http.Header{"Cookie": {"__astraterm_proxy=" + v + "; astraterm_session=must-not-leak"}}
}

func TestHostModeProxy(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	up := newUpstream(t)

	p := create(t, admin, map[string]any{"url": up.URL + "/"})
	if p.Mode != "host" || !strings.HasPrefix(p.URL, "http://p-"+p.ID+".localhost:") || p.Via.Kind != "direct" {
		t.Fatalf("unexpected proxy: %+v", p)
	}
	b := newBrowser(t, env)

	// Without the cookie: the auth page, never the upstream.
	resp, body := b.get(p.Base+"/echo", nil)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "Open this page from AstraTerm") {
		t.Fatalf("anonymous: %d %s", resp.StatusCode, body)
	}
	ck := b.enter(p)
	// The token is single-use: another browser cannot reuse it.
	if resp, _ := newBrowser(t, env).get(p.URL, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token reuse: %d", resp.StatusCode)
	}

	// Transparent request: upstream Host / Origin / Referer, no AstraTerm cookies or headers.
	origin := p.Base
	resp, body = b.get(p.Base+"/echo?q=1", http.Header{
		"Cookie": {"__astraterm_proxy=" + ck + "; astraterm_session=must-not-leak; app=1"}, "Origin": {origin},
		"Referer": {origin + "/page"}, "X-AstraTerm": {"1"},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("echo: %d %s", resp.StatusCode, body)
	}
	var echo map[string]string
	_ = json.Unmarshal([]byte(body), &echo)
	upHost := strings.TrimPrefix(up.URL, "http://")
	if echo["host"] != upHost || echo["path"] != "/echo?q=1" || echo["cookie"] != "app=1" ||
		echo["origin"] != up.URL || echo["referer"] != up.URL+"/page" || echo["xastraterm"] != "" {
		t.Fatalf("upstream saw %+v", echo)
	}
	if resp.Header.Get("X-Frame-Options") != "" {
		t.Fatal("X-Frame-Options not removed")
	}

	// HTML: bridge injected with a nonce allowed by the (rewritten) upstream CSP; framing limited to the UI origin.
	resp, body = b.get(p.Base+"/", http.Header{"Cookie": {"__astraterm_proxy=" + ck}, "Sec-Fetch-Dest": {"iframe"}})
	csp := strings.Join(resp.Header.Values("Content-Security-Policy"), " | ")
	if !strings.Contains(body, "astraterm-webproxy") || !strings.Contains(csp, "'nonce-") || strings.Contains(csp, "frame-ancestors 'none'") ||
		!strings.Contains(csp, "frame-ancestors http://127.0.0.1") {
		t.Fatalf("html: csp=%q body=%s", csp, body)
	}
	if !strings.Contains(body, `href="/next"`) || !strings.Contains(body, `href="/abs"`) {
		t.Fatalf("links not mapped: %s", body)
	}
	if sc := resp.Header.Get("Set-Cookie"); !strings.Contains(sc, "app=1") || strings.Contains(sc, "Domain") || !strings.Contains(sc, "SameSite=None") {
		t.Fatalf("set-cookie = %q", sc)
	}
	// A gzip-encoded document is decoded, given the bridge and delivered completely.
	resp, body = b.get(p.Base+"/gz", http.Header{"Cookie": {"__astraterm_proxy=" + ck}, "Sec-Fetch-Dest": {"document"},
		"Accept": {"text/html"}, "Accept-Encoding": {"gzip, deflate, br"}})
	if resp.StatusCode != 200 || !strings.HasSuffix(body, "</body></html>") || !strings.Contains(body, "line 4999") ||
		!strings.Contains(body, "astraterm-webproxy") || resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("gzip document: %d enc=%q len=%d tail=%q", resp.StatusCode, resp.Header.Get("Content-Encoding"), len(body), body[max(0, len(body)-40):])
	}
	if got := up.last().Header.Get("Accept-Encoding"); got != "gzip" {
		t.Fatalf("upstream Accept-Encoding for documents = %q", got)
	}
	// XHR HTML fragments are not given the bridge.
	_, body = b.get(p.Base+"/", http.Header{"Cookie": {"__astraterm_proxy=" + ck}, "Sec-Fetch-Dest": {"empty"}})
	if strings.Contains(body, "astraterm-webproxy") {
		t.Fatal("bridge injected into a fetch")
	}

	// Redirects to the upstream origin stay on the proxy origin.
	resp, _ = b.get(p.Base+"/redirect", withCookie(ck))
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/target?x=1" {
		t.Fatalf("redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// A fresh entry URL (reload / new window).
	var fresh proxyInfo
	admin.MustJSON("POST", "/api/webproxy/"+p.ID+"/url", map[string]any{"path": "/echo"}, &fresh)
	if !strings.Contains(fresh.URL, "/echo?__astraterm_proxy_token=") {
		t.Fatalf("fresh url %q", fresh.URL)
	}

	// The SPA may frame proxy origins.
	resp, _ = admin.Do("GET", "/", nil)
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "http://*.localhost:*") {
		t.Fatalf("SPA frame-src not extended: %q", csp)
	}

	// Close: the origin answers "closed".
	admin.MustJSON("DELETE", "/api/webproxy/"+p.ID, nil, nil)
	if resp, _ := b.get(p.Base+"/echo", withCookie(ck)); resp.StatusCode != http.StatusGone {
		t.Fatalf("after close: %d", resp.StatusCode)
	}
}

func TestWebSocketPassthrough(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	up := newUpstream(t)
	p := create(t, admin, map[string]any{"url": up.URL})
	b := newBrowser(t, env)
	ck := b.enter(p)

	hdr := http.Header{"Cookie": {"__astraterm_proxy=" + ck}, "Origin": {p.Base}}
	pu, _ := url.Parse(p.Base)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(env.URL("/ws"), "http"), &websocket.DialOptions{
		HTTPHeader: hdr, Host: pu.Host,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()
	for _, msg := range []string{"hello", "world"} {
		if err := ws.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatal(err)
		}
		_, data, err := ws.Read(ctx)
		if err != nil || string(data) != "echo:"+msg {
			t.Fatalf("read %q %v", data, err)
		}
	}
	// The upstream saw its own origin (apps such as Jupyter check it).
	if o := up.last().Header.Get("Origin"); o != up.URL {
		t.Fatalf("upstream Origin %q", o)
	}
	// An open socket keeps the proxy active; closing the proxy ends it.
	var info struct {
		Active int `json:"active"`
	}
	admin.MustJSON("GET", "/api/webproxy/"+p.ID, nil, &info)
	if info.Active < 1 {
		t.Fatalf("active = %d", info.Active)
	}
	admin.MustJSON("DELETE", "/api/webproxy/"+p.ID, nil, nil)
	if _, _, err := ws.Read(ctx); err == nil {
		t.Fatal("socket survived the proxy")
	}
}

func TestPathModeProxy(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	up := newUpstream(t)
	p := create(t, admin, map[string]any{"url": up.URL + "/", "mode": "path"})
	if p.Mode != "path" || !strings.HasPrefix(p.URL, "/proxy/"+p.ID+"-") {
		t.Fatalf("unexpected: %+v", p)
	}
	anon := env.Client()
	resp, body := anon.Do("GET", p.URL, nil)
	if resp.StatusCode != 200 || !strings.Contains(body2(body), "Upstream") {
		t.Fatalf("path mode: %d %s", resp.StatusCode, body)
	}
	csp := strings.Join(resp.Header.Values("Content-Security-Policy"), " | ")
	if !strings.Contains(csp, "sandbox allow-scripts") || strings.Contains(csp, "allow-same-origin") {
		t.Fatalf("path mode must sandbox: %q", csp)
	}
	if !strings.Contains(body2(body), `href="`+p.Base+`/abs"`) || !strings.Contains(body2(body), `href="`+p.Base+`/next"`) {
		t.Fatalf("links not prefixed: %s", body)
	}
	// Redirects keep the prefix; AstraTerm's session cookie never reaches the upstream.
	resp, _ = admin.Do("GET", p.Base+"/echo", nil)
	if resp.StatusCode != 200 || strings.Contains(up.last().Header.Get("Cookie"), "astraterm_session") {
		t.Fatalf("echo: %d cookie=%q", resp.StatusCode, up.last().Header.Get("Cookie"))
	}
	// A wrong key is indistinguishable from a closed proxy.
	resp, _ = anon.Do("GET", "/proxy/"+p.ID+"-wrong/", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong key: %d", resp.StatusCode)
	}
}

func body2(b []byte) string { return string(b) }

func TestOwnershipAndErrors(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	up := newUpstream(t)
	p := create(t, admin, map[string]any{"url": up.URL})
	bob := env.CreateUser(admin, "bob", adminPass, "user")
	if st, _ := bob.ErrorCode("GET", "/api/webproxy/"+p.ID, nil); st != 404 {
		t.Fatalf("foreign get: %d", st)
	}
	if st, _ := bob.ErrorCode("DELETE", "/api/webproxy/"+p.ID, nil); st != 404 {
		t.Fatalf("foreign delete: %d", st)
	}
	if st, _ := bob.ErrorCode("POST", "/api/webproxy/"+p.ID+"/url", nil); st != 404 {
		t.Fatalf("foreign url: %d", st)
	}
	var list []proxyInfo
	bob.MustJSON("GET", "/api/webproxy", nil, &list)
	if len(list) != 0 {
		t.Fatalf("bob sees %d proxies", len(list))
	}
	// Unreachable upstream → 422 with a message.
	up.Close()
	st, code := admin.ErrorCode("POST", "/api/webproxy", map[string]any{"url": up.URL})
	if st != 422 || code != "upstream_refused" {
		t.Fatalf("refused: %d %s", st, code)
	}
	for _, bad := range []map[string]any{{"url": "ftp://x/"}, {"host": "a b", "port": 80}, {"url": "http://x/", "port": 70000},
		{"sessionId": "nope", "port": 80}, {"connectionId": "nope", "port": 80}, {"tunnelId": "x", "sessionId": "y"}} {
		if st, _ := admin.ErrorCode("POST", "/api/webproxy", bad); st != 400 && st != 404 {
			t.Errorf("%v: status %d", bad, st)
		}
	}
}

func TestServerModeGuard(t *testing.T) {
	env := servertest.New(t, func(c *config.Config) { c.Mode = config.ModeServer })
	admin := env.Setup("admin", adminPass)
	up := newUpstream(t)
	user := env.CreateUser(admin, "carol", adminPass, "user")
	// Non-admins may not reach the AstraTerm host's loopback directly…
	st, code := user.ErrorCode("POST", "/api/webproxy", map[string]any{"url": up.URL})
	if st != 403 || code != "destination_blocked" {
		t.Fatalf("user → loopback: %d %s", st, code)
	}
	st, code = user.ErrorCode("POST", "/api/webproxy", map[string]any{"url": "http://169.254.169.254/latest/meta-data/"})
	if st != 403 || code != "destination_blocked" {
		t.Fatalf("user → metadata: %d %s", st, code)
	}
	// …administrators may.
	p := create(t, admin, map[string]any{"url": up.URL})
	if p.Mode != "host" {
		t.Fatalf("mode %q", p.Mode)
	}
}

func TestTLSUpstream(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "secure hello")
	}))
	t.Cleanup(up.Close)
	b := newBrowser(t, env)

	// Self-signed: the page explains the certificate problem (and the bridge reports code "tls").
	p := create(t, admin, map[string]any{"url": up.URL + "/"})
	ck := b.enter(p)
	resp, body := b.get(p.Base+"/", withCookie(ck))
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "Certificate not trusted") || !strings.Contains(body, `"code":"tls"`) {
		t.Fatalf("untrusted certificate: %d %.400s", resp.StatusCode, body)
	}
	// The user accepts the certificate: a proxy with insecureTls reaches the site.
	p2 := create(t, admin, map[string]any{"url": up.URL + "/", "insecureTls": true})
	ck2 := b.enter(p2)
	if resp, body := b.get(p2.Base+"/", withCookie(ck2)); resp.StatusCode != 200 || body != "secure hello" {
		t.Fatalf("insecureTls: %d %q", resp.StatusCode, body)
	}
}
