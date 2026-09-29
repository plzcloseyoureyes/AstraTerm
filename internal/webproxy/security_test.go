package webproxy_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nexterm/nexterm/internal/server/servertest"
)

// hostileUpstream answers every request with headers that would configure the serving origin.
func hostileUpstream(t *testing.T) *httptest.Server {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Alt-Svc", `h2="evil.example:443"; ma=86400`)
		h.Set("NEL", `{"report_to":"x","max_age":31536000}`)
		h.Set("Report-To", `{"group":"x","max_age":31536000,"endpoints":[{"url":"https://evil.example/r"}]}`)
		h.Set("Clear-Site-Data", `"cookies", "storage"`)
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		h.Set("Access-Control-Allow-Origin", "https://evil.example")
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Add("Set-Cookie", "nexterm_session=attacker; Path=/")
		h.Add("Set-Cookie", "app=1; Path=/")
		h.Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(up.Close)
	return up
}

func TestHostModeOpenRedirect(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	up := newUpstream(t)
	p := create(t, admin, map[string]any{"url": up.URL + "/"})
	b := newBrowser(t, env)
	ck := b.enter(p)
	// A token parameter on a network-path reference with a valid browser session: the clean-up redirect stays on the
	// proxy origin.
	for _, path := range []string{"//evil.example/x", "/\\evil.example/x", "///evil.example/x"} {
		resp, _ := b.get(p.Base+path+"?__nexterm_proxy_token=spent", withCookie(ck))
		loc := resp.Header.Get("Location")
		if resp.StatusCode != http.StatusFound || strings.HasPrefix(loc, "//") || strings.HasPrefix(loc, "/\\") || !strings.HasPrefix(loc, "/") {
			t.Fatalf("%s: %d Location %q", path, resp.StatusCode, loc)
		}
	}
}

func TestOriginConfigHeadersStripped(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	up := hostileUpstream(t)

	// Host mode: the proxy origin is the upstream's own, only origin-wide redirections (Alt-Svc) are removed.
	p := create(t, admin, map[string]any{"url": up.URL + "/"})
	b := newBrowser(t, env)
	ck := b.enter(p)
	resp, _ := b.get(p.Base+"/", withCookie(ck))
	if resp.Header.Get("Alt-Svc") != "" {
		t.Fatal("Alt-Svc passed in host mode")
	}
	for _, c := range resp.Cookies() {
		if c.Name == "nexterm_session" {
			t.Fatal("upstream set NexTerm's session cookie")
		}
	}

	// Path mode: the upstream is served on NexTerm's origin and may not configure it.
	pp := create(t, admin, map[string]any{"url": up.URL + "/", "mode": "path"})
	resp, _ = admin.Do("GET", pp.URL, nil)
	for _, k := range []string{"Alt-Svc", "NEL", "Report-To", "Clear-Site-Data", "Strict-Transport-Security", "Access-Control-Allow-Origin", "Access-Control-Allow-Credentials"} {
		if v := resp.Header.Get(k); v != "" {
			t.Errorf("path mode passed %s: %q", k, v)
		}
	}
	for _, c := range resp.Cookies() {
		if c.Name == "nexterm_session" {
			t.Fatal("upstream overwrote NexTerm's session cookie in path mode")
		}
		if c.Name == "app" && !strings.HasPrefix(c.Path, pp.Base+"/") {
			t.Fatalf("path-mode cookie escapes its prefix: %+v", c)
		}
	}
}

func TestPathModeCORS(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	up := newUpstream(t)
	p := create(t, admin, map[string]any{"url": up.URL + "/", "mode": "path"})

	// The sandboxed page's own fetches (Origin: null) are allowed by CORS, without credentials.
	req, _ := http.NewRequest(http.MethodOptions, env.URL(p.Base+"/echo"), nil)
	req.Header.Set("Origin", "null")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type, x-requested-with")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "null" ||
		resp.Header.Get("Access-Control-Allow-Methods") != "POST" || resp.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
	resp = rawGet(t, env, p.Base+"/echo", "null")
	if resp.Header.Get("Access-Control-Allow-Origin") != "null" {
		t.Fatalf("null-origin fetch: %v", resp.Header)
	}
	// Other origins get nothing; a wrong key gets no preflight answer either.
	resp = rawGet(t, env, p.Base+"/echo", "https://evil.example")
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("foreign origin: %v", resp.Header)
	}
	req, _ = http.NewRequest(http.MethodOptions, env.URL("/proxy/"+p.ID+"-wrong/echo"), nil)
	req.Header.Set("Origin", "null")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("preflight answered for a wrong key")
	}
}

func rawGet(t *testing.T, env *servertest.Env, path, origin string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, env.URL(path), nil)
	req.Header.Set("Origin", origin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}
