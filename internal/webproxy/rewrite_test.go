package webproxy

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// farFuture keeps a test Service's cached settings valid.
var farFuture = time.Now().Add(time.Hour)

func testProxy(t Target) *Proxy {
	return &Proxy{rt: &route{target: t}}
}

func TestParseURL(t *testing.T) {
	cases := []struct {
		in   string
		want Target
		err  bool
	}{
		{"http://web/", Target{Scheme: "http", Host: "web", Port: 80, Path: "/"}, false},
		{"web:8080/grafana?x=1#h", Target{Scheme: "http", Host: "web", Port: 8080, Path: "/grafana?x=1#h"}, false},
		{"10.0.0.1:443", Target{Scheme: "https", Host: "10.0.0.1", Port: 443, Path: "/"}, false},
		{"https://[::1]:8443/a%20b", Target{Scheme: "https", Host: "::1", Port: 8443, Path: "/a%20b"}, false},
		{"HTTPS://Example.com", Target{Scheme: "https", Host: "Example.com", Port: 443, Path: "/"}, false},
		{"ftp://host/", Target{}, true},
		{"http://user:pw@host/", Target{}, true},
		{"", Target{}, true},
		{"http://bad host/", Target{}, true},
	}
	for _, c := range cases {
		got, err := parseURL(c.in)
		if err == nil {
			err = got.normalize()
		}
		if (err != nil) != c.err {
			t.Errorf("%q: err = %v, want error %v", c.in, err, c.err)
			continue
		}
		if !c.err && got != c.want {
			t.Errorf("%q: got %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestTargetOrigin(t *testing.T) {
	if o := (Target{Scheme: "https", Host: "h", Port: 443}).Origin(); o != "https://h" {
		t.Fatal(o)
	}
	if o := (Target{Scheme: "http", Host: "::1", Port: 8080}).Origin(); o != "http://[::1]:8080" {
		t.Fatal(o)
	}
	if o := (Target{Scheme: "http", Host: "::1", Port: 80}).Origin(); o != "http://[::1]" {
		t.Fatal(o)
	}
}

func TestMapURL(t *testing.T) {
	p := testProxy(Target{Scheme: "http", Host: "web", Port: 8080})
	host := reqMode{origin: "http://p-x.localhost:7822"}
	path := reqMode{path: true, prefix: "/proxy/id-key", origin: "https://nx.example.com"}
	cases := []struct {
		in   string
		m    reqMode
		want string
	}{
		{"http://web:8080/login?next=/", host, "/login?next=/"},
		{"ws://web:8080/socket", host, "/socket"},
		{"//web:8080/x", host, "/x"},
		{"http://web/other", host, "http://web/other"}, // other port = other origin
		{"https://web:8080/", host, "https://web:8080/"},
		{"/abs", host, "/abs"},
		{"rel/x", host, "rel/x"},
		{"/abs", path, "/proxy/id-key/abs"},
		{"/proxy/id-key/already", path, "/proxy/id-key/already"},
		{"http://web:8080/y#f", path, "/proxy/id-key/y#f"},
		{"https://elsewhere.example/", path, "https://elsewhere.example/"},
		{"mailto:a@b", path, "mailto:a@b"},
	}
	for _, c := range cases {
		if got := p.mapURL(c.in, c.m, true); got != c.want {
			t.Errorf("mapURL(%q, path=%v) = %q, want %q", c.in, c.m.path, got, c.want)
		}
	}
	if got := p.mapRefresh("0; url=/next", path); got != "0; url=/proxy/id-key/next" {
		t.Errorf("mapRefresh = %q", got)
	}
}

func TestRewriteSetCookie(t *testing.T) {
	secure := reqMode{secure: true}
	plain := reqMode{}
	path := reqMode{path: true, prefix: "/proxy/i-k", secure: true}
	cases := []struct {
		in   string
		m    reqMode
		want string
	}{
		{"sid=abc; Path=/; Domain=web.local; HttpOnly; SameSite=Lax", secure, "sid=abc; Path=/; HttpOnly; Secure; SameSite=None; Partitioned"},
		{"sid=abc; Path=/; Secure; SameSite=Strict", plain, "sid=abc; Path=/; SameSite=Strict"},
		{"sid=abc; SameSite=None; Secure", plain, "sid=abc"},
		{"a=1; Path=/app; Max-Age=60", path, "a=1; Path=/proxy/i-k/app; Max-Age=60; Secure; SameSite=None; Partitioned"},
		{"astraterm_session=stolen; Path=/", secure, ""},
		{cookieName + "=x", secure, ""},
		{"=novalue", secure, ""},
	}
	for _, c := range cases {
		if got := rewriteSetCookie(c.in, c.m); got != c.want {
			t.Errorf("rewriteSetCookie(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFixCSP(t *testing.T) {
	cases := []struct {
		in, nonce, want string
	}{
		{"default-src 'self'; frame-ancestors 'none'", "", "default-src 'self'"},
		{"script-src 'self'; frame-ancestors 'self'", "N", "script-src 'self' 'nonce-N'"},
		{"script-src 'self' 'unsafe-inline'", "N", "script-src 'self' 'unsafe-inline'"},                           // already allowed
		{"script-src 'unsafe-inline' 'nonce-abc'", "N", "script-src 'unsafe-inline' 'nonce-abc' 'nonce-N'"},       // keyed
		{"script-src 'strict-dynamic' 'sha256-x'", "N", "script-src 'strict-dynamic' 'sha256-x' 'nonce-N'"},       //
		{"default-src 'none'", "N", "default-src 'nonce-N'"},                                                      // 'none' replaced
		{"script-src-elem 'self'; script-src 'none'", "N", "script-src-elem 'self' 'nonce-N'; script-src 'none'"}, // elem first
		{"img-src *", "N", "img-src *"}, // no script directive: inline allowed
		{"frame-ancestors 'none', script-src 'self'", "N", "script-src 'self' 'nonce-N'"},
	}
	for _, c := range cases {
		if got := fixCSP(c.in, c.nonce, true); got != c.want {
			t.Errorf("fixCSP(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFilterCookiesAndQuery(t *testing.T) {
	h := http.Header{}
	h.Add("Cookie", "a=1; astraterm_session=secret; "+cookieName+"=p")
	h.Add("Cookie", "b=2")
	filterCookies(h)
	if got := h.Get("Cookie"); got != "a=1; b=2" {
		t.Fatalf("cookies = %q", got)
	}
	h = http.Header{"Cookie": {"astraterm_session=x"}}
	filterCookies(h)
	if _, ok := h["Cookie"]; ok {
		t.Fatal("empty Cookie header kept")
	}
	if got := dropQueryParam("a=1&"+tokenParam+"=t&b=2", tokenParam); got != "a=1&b=2" {
		t.Fatalf("query = %q", got)
	}
	if got := withQuery("/a?x=1#frag", "t=2"); got != "/a?x=1&t=2#frag" {
		t.Fatalf("withQuery = %q", got)
	}
}

func TestRewriteHTML(t *testing.T) {
	p := testProxy(Target{Scheme: "http", Host: "web", Port: 80})
	in := `<!doctype html><html><head><meta charset="utf-8"><base href="/"><title>T</title>` +
		`<meta http-equiv="Content-Security-Policy" content="script-src 'self'"></head>` +
		`<body><a href="http://web/page">x</a><img srcset="/a.png 1x, http://web/b.png 2x">` +
		`<script>var s = "<a href='/not-rewritten'>";</script><form action="/post"></form></body></html>`
	var out strings.Builder
	err := rewriteHTML(&out, strings.NewReader(in), htmlRewrite{p: p, m: reqMode{path: true, prefix: "/proxy/i-k"},
		script: `<script nonce="N">BRIDGE</script>`, nonce: "N"})
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		`<head><script nonce="N">BRIDGE</script><meta charset="utf-8">`,
		`<base href="/proxy/i-k/">`,
		`href="/proxy/i-k/page"`,
		`srcset="/proxy/i-k/a.png 1x, /proxy/i-k/b.png 2x"`,
		`var s = "<a href='/not-rewritten'>";`,
		`action="/proxy/i-k/post"`,
		`content="script-src &#39;self&#39; &#39;nonce-N&#39;"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	if strings.Count(s, "BRIDGE") != 1 {
		t.Errorf("bridge injected %d times", strings.Count(s, "BRIDGE"))
	}

	// No <head>: injected before the first body-level element; host mode leaves absolute paths alone.
	out.Reset()
	if err := rewriteHTML(&out, strings.NewReader(`<p><a href="/x">x</a></p>`), htmlRewrite{p: p, m: reqMode{}, script: "<S>"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != `<S><p><a href="/x">x</a></p>` {
		t.Errorf("got %q", got)
	}
	// Empty document still gets the bridge.
	out.Reset()
	_ = rewriteHTML(&out, strings.NewReader(``), htmlRewrite{p: p, script: "<S>"})
	if out.String() != "<S>" {
		t.Errorf("empty doc: %q", out.String())
	}
}

func TestBridgeScriptEscaping(t *testing.T) {
	s := bridgeScript(bridgeConfig{ID: "x", Origins: []string{"http://a</script><script>alert(1)//"}}, "n")
	if strings.Contains(s, "</script><script>alert") {
		t.Fatal("config not escaped for a script context")
	}
	if !strings.HasPrefix(s, `<script nonce="n">`) {
		t.Fatal(s[:40])
	}
}

func TestExtendFrameSrcAndMatchHost(t *testing.T) {
	s := &Service{settings: Settings{HostSuffix: "apps.example.com"}}
	s.setAt = farFuture
	h := http.Header{}
	h.Set("Content-Security-Policy", "default-src 'self'; frame-src 'self' blob:; object-src 'none'")
	s.extendFrameSrc(h)
	got := h.Get("Content-Security-Policy")
	if !strings.Contains(got, "frame-src 'self' blob: http://*.localhost:* https://*.localhost:*") ||
		!strings.Contains(got, "https://*.apps.example.com") || !strings.Contains(got, "object-src 'none'") {
		t.Fatalf("csp = %q", got)
	}
	id := "abcdefghijklmnopqrst"
	for host, ok := range map[string]bool{
		"p-" + id + ".localhost:7822":        true,
		"P-" + id + ".LOCALHOST":             true,
		"p-" + id + ".apps.example.com":      true,
		"p-" + id + ".evil.com":              false,
		"p-short.localhost":                  false,
		"x-" + id + ".localhost":             false,
		"p-" + id + "0.localhost":            false,
		"p-abcdefghijklmnopqrs1.localhost:1": false, // '1' is not base32
	} {
		if _, got := s.matchHost(host); got != ok {
			t.Errorf("matchHost(%q) = %v, want %v", host, got, ok)
		}
	}
}

func TestXpraScriptQuoting(t *testing.T) {
	s := xpraScript("/usr/bin/xpra", "seamless", `xterm -T 'it''s' -e "echo $HOME"`, 23456)
	if !strings.HasPrefix(s, "sh -c '") {
		t.Fatal(s)
	}
	for _, want := range []string{"--bind-ws=127.0.0.1:23456", "start ", "--start-child="} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q: %s", want, s)
		}
	}
	if !strings.Contains(xpraScript("xpra", "desktop", "startxfce4", 1), " start-desktop ") {
		t.Error("desktop mode")
	}
	if validCommand("a\nb") == nil || validCommand(" ") == nil || validCommand("xeyes") != nil {
		t.Error("validCommand")
	}
}
