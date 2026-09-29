package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/model"
)

// syncBuffer collects log output from concurrent handlers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newTestRouter(opts Options) (*Router, *syncBuffer) {
	buf := &syncBuffer{}
	opts.Log = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewRouter(opts), buf
}

func serve(h http.Handler, method, target string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

type errBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func decodeErr(t *testing.T, w *httptest.ResponseRecorder) errBody {
	t.Helper()
	var b errBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("error body %q: %v", w.Body.String(), err)
	}
	return b
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{ErrNotFound, 404, "not_found"},
		{fmt.Errorf("conn x: %w", model.ErrNotFound), 404, "not_found"},
		{model.ErrLocked, 423, "locked"},
		{ErrLocked, 423, "locked"},
		{BadRequest("bad"), 400, "bad_request"},
		{Conflict("dup"), 409, "conflict"},
		{TooManyRequests("slow down", 3), 429, "too_many_requests"},
		{Unauthorized("totp_required", "need code"), 401, "totp_required"},
		{errors.New("db exploded: secret detail"), 500, "internal"},
		{Internal(errors.New("secret detail")), 500, "internal"},
		// echo's own errors (router, middleware) speak the same vocabulary
		{echo.ErrNotFound, 404, "not_found"},
		{echo.ErrMethodNotAllowed, 405, "method_not_allowed"},
		{echo.ErrStatusRequestEntityTooLarge, 413, "too_large"},
		{&http.MaxBytesError{Limit: 1}, 413, "too_large"},
		{echo.NewHTTPError(http.StatusBadRequest, "custom message"), 400, "bad_request"},
		{echo.ErrServiceUnavailable.Wrap(errors.New("secret detail")), 503, "service_unavailable"},
		{fmt.Errorf("op: %w", context.DeadlineExceeded), 504, "timeout"},
	}
	r, _ := newTestRouter(Options{})
	var current error
	r.Public().GET("/err", func(c *echo.Context) error { return current })
	for _, c := range cases {
		current = c.err
		w := serve(r, "GET", "/api/err", nil, "")
		body := decodeErr(t, w)
		if w.Code != c.status || body.Code != c.code {
			t.Fatalf("%v: got %d %q", c.err, w.Code, body.Code)
		}
		if strings.Contains(body.Error, "secret detail") {
			t.Fatalf("internal detail leaked: %q", body.Error)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%v: error response headers %v", c.err, w.Header())
		}
	}
	current = echo.NewHTTPError(http.StatusBadRequest, "custom message")
	if b := decodeErr(t, serve(r, "GET", "/api/err", nil, "")); b.Error != "custom message" {
		t.Fatalf("echo.HTTPError message: %+v", b)
	}
	current = echo.ErrMethodNotAllowed
	if b := decodeErr(t, serve(r, "GET", "/api/err", nil, "")); b.Error != "method not allowed" {
		t.Fatalf("echo error message: %+v", b)
	}
	current = TooManyRequests("x", 7)
	if w := serve(r, "GET", "/api/err", nil, ""); w.Header().Get("Retry-After") != "7" {
		t.Fatal("Retry-After missing")
	}
	if !errors.Is(model.ErrLocked, ErrLocked) || !errors.Is(ErrLocked, model.ErrLocked) {
		t.Fatal("model/httpx locked errors should match both ways")
	}
	if echo.StatusCode(ErrLocked) != http.StatusLocked {
		t.Fatal("HTTPError must implement echo.HTTPStatusCoder")
	}
}

func TestErrorAfterCommitIsNotRendered(t *testing.T) {
	r, _ := newTestRouter(Options{})
	r.Public().GET("/stream", func(c *echo.Context) error {
		_ = c.String(http.StatusOK, "partial")
		return errors.New("stream broke")
	})
	w := serve(r, "GET", "/api/stream", nil, "")
	if w.Code != 200 || w.Body.String() != "partial" {
		t.Fatalf("committed response was modified: %d %q", w.Code, w.Body.String())
	}
}

func TestBind(t *testing.T) {
	var v struct {
		A int `json:"a"`
	}
	r, _ := newTestRouter(Options{})
	e := r.Echo()
	ctx := func(body string) *echo.Context {
		return e.NewContext(httptest.NewRequest("POST", "/", strings.NewReader(body)), httptest.NewRecorder())
	}
	if err := Bind(ctx(`{"a":1,"unknown":2}`), &v); err != nil || v.A != 1 {
		t.Fatalf("decode: %v %v", err, v)
	}
	for _, bad := range []string{``, `{"a":`, `{"a":"x"}`, `{"a":1} trailing`, `[1]`} {
		var he *HTTPError
		if err := Bind(ctx(bad), &v); !errors.As(err, &he) || he.Status != 400 {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if err := BindOptional(ctx(``), &v); err != nil {
		t.Fatalf("optional empty: %v", err)
	}
	if err := BindOptional(ctx(`{"a":`), &v); err == nil {
		t.Fatal("optional must still reject malformed JSON")
	}
	big := `{"a":1,"pad":"` + strings.Repeat("x", 100) + `"}`
	var mbe *http.MaxBytesError
	if err := BindLimit(ctx(big), &v, 50); !errors.As(err, &mbe) {
		t.Fatalf("limit: %v", err)
	}
	v.A = 0
	if err := ctx(`{"a":5}`).Bind(&v); err != nil || v.A != 5 {
		t.Fatalf("c.Bind must use Bind semantics: %v %v", err, v)
	}
}

func TestBodyLimits(t *testing.T) {
	r, _ := newTestRouter(Options{})
	raw := func(c *echo.Context) error {
		b, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]int{"n": len(b)})
	}
	bind := func(limit int64) echo.HandlerFunc {
		return func(c *echo.Context) error {
			var v map[string]string
			if err := BindLimit(c, &v, limit); err != nil {
				return err
			}
			return OK(c)
		}
	}
	r.Public().POST("/raw", raw)
	r.Public().POST("/raw-unlimited", raw, BodyLimit(0))
	r.Public().POST("/raw-small", raw, BodyLimit(10))
	r.Public().POST("/bind-4m", bind(4<<20))
	r.Public().POST("/bind-4m-capped", bind(4<<20), BodyLimit(100))
	csrfHdr := map[string]string{CSRFHeader: "1"}
	three := `{"k":"` + strings.Repeat("x", 3<<20) + `"}`
	checks := []struct {
		path, body string
		want       int
	}{
		{"/api/raw", three, 413},           // default cap for raw readers
		{"/api/raw-unlimited", three, 200}, // route lifted the cap
		{"/api/raw-small", strings.Repeat("y", 11), 413},
		{"/api/bind-4m", three, 200},        // BindLimit replaces the default cap
		{"/api/bind-4m-capped", three, 413}, // …but not a route's explicit cap
		{"/api/bind-4m", `{"k":"` + strings.Repeat("x", 5<<20) + `"}`, 413},
	}
	for _, c := range checks {
		w := serve(r, "POST", c.path, csrfHdr, c.body)
		if w.Code != c.want {
			t.Fatalf("%s: got %d want %d (%s)", c.path, w.Code, c.want, w.Body.String())
		}
		if c.want == 413 && decodeErr(t, w).Code != "too_large" {
			t.Fatalf("%s: %s", c.path, w.Body.String())
		}
	}
}

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	mk := func(remote, xff, xri string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		if xri != "" {
			r.Header.Set("X-Real-IP", xri)
		}
		return r
	}
	cases := []struct{ remote, xff, xri, want string }{
		{"203.0.113.5:1234", "1.2.3.4", "", "203.0.113.5"},             // untrusted peer: headers ignored
		{"10.0.0.2:1234", "1.2.3.4, 10.0.0.9", "", "1.2.3.4"},          // skip trusted hops
		{"10.0.0.2:1234", "6.6.6.6, 1.2.3.4, 10.1.1.1", "", "1.2.3.4"}, // rightmost untrusted wins (no spoofing)
		{"10.0.0.2:1234", "", "5.5.5.5", "5.5.5.5"},
		{"10.0.0.2:1234", "garbage", "", "10.0.0.2"},
		{"[::ffff:10.0.0.2]:1", "", "", "10.0.0.2"},
	}
	for _, c := range cases {
		if got := resolveClientIP(mk(c.remote, c.xff, c.xri), trusted); got != c.want {
			t.Fatalf("%+v: got %s", c, got)
		}
	}

	// Through the router: ClientIP(c) and the request context (for lower layers) agree.
	r, _ := newTestRouter(Options{TrustedProxies: trusted})
	r.Public().GET("/ip", func(c *echo.Context) error {
		return c.String(http.StatusOK, ClientIP(c)+"|"+ClientIPFrom(c.Request().Context())+"|"+c.RealIP())
	})
	req := mk("10.0.0.2:1234", "1.2.3.4, 10.0.0.9", "")
	req.URL.Path = "/api/ip"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "1.2.3.4|1.2.3.4|1.2.3.4" {
		t.Fatalf("router client IP: %q", w.Body.String())
	}
}

func TestRouterPolicies(t *testing.T) {
	r, logs := newTestRouter(Options{LoopbackOnly: true, Dev: true})
	admin := &model.User{ID: "a", Role: model.RoleAdmin}
	user := &model.User{ID: "u", Role: model.RoleUser}
	r.SetAuthenticator(AuthenticatorFunc(func(c *echo.Context) (*model.User, AuthInfo, error) {
		switch c.Request().Header.Get("X-As") {
		case "admin":
			return admin, AuthInfo{Method: AuthCookie}, nil
		case "user":
			return user, AuthInfo{Method: AuthCookie}, nil
		case "token":
			return user, AuthInfo{Method: AuthToken}, nil
		case "boom":
			return nil, AuthInfo{}, errors.New("db down")
		}
		return nil, AuthInfo{}, nil
	}))
	ok := func(c *echo.Context) error { return OK(c) }
	r.Public().GET("/pub", ok)
	r.API().POST("/priv", ok)
	r.Admin().GET("/adm", ok)
	r.WS("/ws/x", ok)
	r.PublicWS("/ws/pub", ok)
	r.API().GET("/panic", func(c *echo.Context) error { panic("boom") })
	r.Public().GET("/pub", ok) // duplicate registration must not crash

	do := func(method, path, as string, hdr map[string]string) int {
		h := map[string]string{"Host": "127.0.0.1:7822"}
		if as != "" {
			h["X-As"] = as
		}
		for k, v := range hdr {
			h[k] = v
		}
		return serve(r, method, path, h, "").Code
	}
	csrf := map[string]string{CSRFHeader: "1"}
	checks := []struct {
		got, want int
		name      string
	}{
		{do("GET", "/api/pub", "", nil), 200, "public anon"},
		{do("GET", "/api/pub", "user", nil), 200, "public user"},
		{do("POST", "/api/priv", "", csrf), 401, "private anon"},
		{do("POST", "/api/priv", "user", nil), 403, "missing csrf"},
		{do("POST", "/api/priv", "user", csrf), 200, "private user"},
		{do("POST", "/api/priv", "token", nil), 200, "bearer skips csrf"},
		{do("GET", "/api/adm", "", nil), 401, "admin anon"},
		{do("GET", "/api/adm", "user", nil), 403, "admin as user"},
		{do("GET", "/api/adm", "admin", nil), 200, "admin"},
		{do("GET", "/ws/x", "user", map[string]string{"Origin": "http://evil.example"}), 403, "ws cross-origin"},
		{do("GET", "/ws/x", "", map[string]string{"Origin": "http://evil.example"}), 403, "ws cross-origin before auth"},
		{do("GET", "/ws/x", "", nil), 401, "ws anon"},
		{do("GET", "/ws/x", "user", map[string]string{"Origin": "http://localhost:5173"}), 200, "ws dev origin"},
		{do("GET", "/ws/x", "user", map[string]string{"Origin": "http://127.0.0.1:7822"}), 200, "ws same origin"},
		{do("GET", "/ws/x", "user", map[string]string{"Origin": "::bad"}), 403, "ws malformed origin"},
		{do("GET", "/ws/pub", "", nil), 200, "public ws anon"},
		{do("GET", "/ws/pub", "", map[string]string{"Origin": "http://evil.example"}), 403, "public ws cross-origin"},
		{do("GET", "/api/panic", "user", nil), 500, "panic recovered"},
		{do("GET", "/api/pub", "boom", nil), 500, "authenticator error"},
		{do("HEAD", "/api/pub", "", nil), 200, "GET routes answer HEAD"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Fatalf("%s: got %d want %d", c.name, c.got, c.want)
		}
	}
	for _, host := range []string{"attacker.example:7822", "", "EVIL.example"} {
		w := serve(r, "GET", "/api/pub", map[string]string{"Host": host}, "")
		if w.Code != http.StatusMisdirectedRequest || decodeErr(t, w).Code != "invalid_host" {
			t.Fatalf("rebinding host %q: %d", host, w.Code)
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "'wasm-unsafe-eval'") {
			t.Fatal("CSP missing")
		}
	}
	for _, host := range []string{"localhost", "localhost:1", "[::1]:5", "127.0.0.2", "app.localhost"} {
		if w := serve(r, "GET", "/api/pub", map[string]string{"Host": host}, ""); w.Code != 200 {
			t.Fatalf("loopback host %q rejected: %d", host, w.Code)
		}
	}
	out := logs.String()
	for _, want := range []string{`msg="panic in handler"`, `panic=boom`, `msg=http method=GET path=/api/panic status=500`,
		`err="internal server error"`, `msg="route registration failed" method=GET path=/api/pub`, `status=421`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log lacks %q:\n%s", want, out)
		}
	}
}

func TestHostGuardAllowedHosts(t *testing.T) {
	r, _ := newTestRouter(Options{LoopbackOnly: true, AllowedHosts: []string{"termstead.example.com", "10.1.2.3"}})
	r.Public().GET("/pub", func(c *echo.Context) error { return OK(c) })
	for host, want := range map[string]int{
		"termstead.example.com":      200,
		"TERMSTEAD.example.com:443":  200, // case and port do not matter
		"termstead.example.com.":     200, // fully qualified form
		"10.1.2.3:8443":              200,
		"localhost:7822":             200, // loopback names still pass
		"example.com":                421,
		"evil.termstead.example.com": 421, // exact names only, no suffix match
		"termstead.example.com.evil": 421,
	} {
		if got := serve(r, "GET", "/api/pub", map[string]string{"Host": host}, "").Code; got != want {
			t.Fatalf("host %q: got %d want %d", host, got, want)
		}
	}
	// The option only extends the loopback guard; without LoopbackOnly any Host passes as before.
	open, _ := newTestRouter(Options{AllowedHosts: []string{"termstead.example.com"}})
	open.Public().GET("/pub", func(c *echo.Context) error { return OK(c) })
	if got := serve(open, "GET", "/api/pub", map[string]string{"Host": "other.example"}, "").Code; got != 200 {
		t.Fatalf("network listener: %d", got)
	}
}

func TestSecurityHeaders(t *testing.T) {
	for _, tls := range []bool{false, true} {
		r, _ := newTestRouter(Options{TLS: tls})
		r.Public().GET("/x", func(c *echo.Context) error { return OK(c) })
		w := serve(r, "GET", "/api/x", map[string]string{"Host": "termstead.test:7822"}, "")
		h := w.Header()
		want := map[string]string{
			"X-Content-Type-Options":       "nosniff",
			"X-Frame-Options":              "SAMEORIGIN",
			"Referrer-Policy":              "no-referrer",
			"Cross-Origin-Opener-Policy":   "same-origin",
			"Cross-Origin-Resource-Policy": "same-origin",
			"Permissions-Policy":           "camera=(), geolocation=(), payment=(), usb=()",
			"Content-Security-Policy": "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; " +
				"img-src 'self' data: blob:; font-src 'self' data:; media-src 'self' data: blob:; " +
				"connect-src 'self' ws://termstead.test:7822 wss://termstead.test:7822; worker-src 'self' blob:; frame-src 'self' blob:; " +
				"object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'self'",
		}
		for k, v := range want {
			if h.Get(k) != v {
				t.Fatalf("tls=%v %s = %q, want %q", tls, k, h.Get(k), v)
			}
		}
		if got := h.Get("Strict-Transport-Security"); (got == "max-age=31536000") != tls || (!tls && got != "") {
			t.Fatalf("tls=%v HSTS %q", tls, got)
		}
		if h.Get("X-XSS-Protection") != "" {
			t.Fatal("unexpected X-XSS-Protection")
		}
	}
}

func TestUnmatchedRoutesAndFallback(t *testing.T) {
	r, _ := newTestRouter(Options{})
	r.Public().GET("/pub", func(c *echo.Context) error { return OK(c) })
	csrf := map[string]string{CSRFHeader: "1"}

	// Without a fallback, unknown paths are JSON 404s.
	if w := serve(r, "GET", "/some/page", nil, ""); w.Code != 404 || decodeErr(t, w).Code != "not_found" {
		t.Fatalf("no fallback: %d %s", w.Code, w.Body.String())
	}

	r.SetFallback(func(c *echo.Context) error { return c.String(http.StatusOK, "spa:"+c.Request().Method) })
	for _, p := range []string{"/api", "/api/", "/api/nope", "/ws", "/ws/", "/ws/nope", "/api/pub/"} {
		w := serve(r, "GET", p, nil, "")
		if b := decodeErr(t, w); w.Code != 404 || b.Code != "not_found" || b.Error != "no such endpoint" {
			t.Fatalf("GET %s: %d %+v", p, w.Code, b)
		}
	}
	for _, m := range []string{"POST", "DELETE", "PATCH", "OPTIONS"} {
		w := serve(r, m, "/api/pub", csrf, "")
		if w.Code != 404 || decodeErr(t, w).Error != "no such endpoint" || w.Header().Get("Allow") != "" {
			t.Fatalf("%s on a GET route: %d %s %v", m, w.Code, w.Body.String(), w.Header())
		}
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "OPTIONS"} {
		w := serve(r, m, "/", csrf, "")
		if b := decodeErr(t, w); w.Code != 405 || b.Code != "method_not_allowed" || w.Header().Get("Allow") != "GET, HEAD" {
			t.Fatalf("%s /: %d %+v %v", m, w.Code, b, w.Header())
		}
	}
	if w := serve(r, "POST", "/", nil, ""); w.Code != 403 || decodeErr(t, w).Code != "csrf" {
		t.Fatalf("POST / without CSRF header: %d", w.Code)
	}
	for _, m := range []string{"GET", "HEAD"} {
		w := serve(r, m, "/deep/client/route", nil, "")
		if w.Code != 200 || (m == "GET" && w.Body.String() != "spa:GET") {
			t.Fatalf("%s fallback: %d %q", m, w.Code, w.Body.String())
		}
	}
	if w := serve(r, "GET", "/API/pub", nil, ""); w.Body.String() != "spa:GET" {
		t.Fatalf("paths are case-sensitive: %q", w.Body.String())
	}
}

func TestCanonicalPathRedirect(t *testing.T) {
	r, _ := newTestRouter(Options{})
	r.Public().GET("/pub", func(c *echo.Context) error { return OK(c) })
	r.API().POST("/priv", func(c *echo.Context) error { return OK(c) })
	cases := []struct{ method, target, location string }{
		{"GET", "//api/pub", "/api/pub"},
		{"GET", "/api/./pub", "/api/pub"},
		{"GET", "/api/x/../pub?q=1", "/api/pub?q=1"},
		{"GET", "/assets/../index.html", "/index.html"},
		{"POST", "//api/priv", "/api/priv"},
	}
	for _, c := range cases {
		w := serve(r, c.method, c.target, map[string]string{CSRFHeader: "1"}, "")
		if w.Code != http.StatusTemporaryRedirect || w.Header().Get("Location") != c.location {
			t.Fatalf("%s %s: %d %q", c.method, c.target, w.Code, w.Header().Get("Location"))
		}
	}
	// The CSRF check runs first.
	if w := serve(r, "POST", "//api/priv", nil, ""); w.Code != 403 {
		t.Fatalf("CSRF before redirect: %d", w.Code)
	}
}

func TestPathParamsAndDuplicates(t *testing.T) {
	r, logs := newTestRouter(Options{})
	r.Public().GET("/items/:id", func(c *echo.Context) error { return c.String(http.StatusOK, "first:"+c.Param("id")) })
	r.Public().GET("/items/:other", func(c *echo.Context) error { return c.String(http.StatusOK, "second") })
	r.Public().GET("/items/:id/sub", func(c *echo.Context) error { return c.String(http.StatusOK, "sub:"+c.Param("id")) })
	cases := map[string]string{
		"/api/items/abc":        "first:abc",
		"/api/items/a%2Fb":      "first:a/b", // an escaped slash stays inside the parameter
		"/api/items/%2541":      "first:%41", // decoded exactly once
		"/api/items/a%20b":      "first:a b",
		"/api/it%65ms/x":        "first:x", // literal segments match regardless of optional escaping
		"/api/items/a%2Fb/sub":  "sub:a/b",
		"/api/items/caf%C3%A9":  "first:café",
		"/api/items/semi%3Bcol": "first:semi;col",
	}
	for target, want := range cases {
		if w := serve(r, "GET", target, nil, ""); w.Code != 200 || w.Body.String() != want {
			t.Fatalf("%s: %d %q, want %q", target, w.Code, w.Body.String(), want)
		}
	}
	if !strings.Contains(logs.String(), `msg="route registration failed" method=GET path=/api/items/:other`) {
		t.Fatalf("conflicting registration not logged:\n%s", logs.String())
	}
}

func TestGroupsAreIsolated(t *testing.T) {
	r, _ := newTestRouter(Options{})
	mine := r.Public()
	mine.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error { return Forbidden("module A only") }
	})
	mine.GET("/a", func(c *echo.Context) error { return OK(c) })
	r.Public().GET("/b", func(c *echo.Context) error { return OK(c) }) // another module
	if w := serve(r, "GET", "/api/a", nil, ""); w.Code != 403 {
		t.Fatalf("group middleware not applied: %d", w.Code)
	}
	if w := serve(r, "GET", "/api/b", nil, ""); w.Code != 200 {
		t.Fatalf("one module's group middleware leaked into another's routes: %d", w.Code)
	}
}

func TestJSONResponses(t *testing.T) {
	r, _ := newTestRouter(Options{})
	r.Public().GET("/json", func(c *echo.Context) error {
		return c.JSON(http.StatusCreated, map[string]string{"html": "<a&b>"})
	})
	r.Public().GET("/cached", func(c *echo.Context) error {
		c.Response().Header().Set("Cache-Control", "max-age=5")
		return c.JSON(http.StatusOK, []int{1})
	})
	r.Public().POST("/ok", OK)
	w := serve(r, "GET", "/api/json", nil, "")
	if w.Code != 201 || w.Body.String() != "{\"html\":\"<a&b>\"}\n" || w.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
		w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("json: %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	if w := serve(r, "GET", "/api/cached", nil, ""); w.Header().Get("Cache-Control") != "max-age=5" {
		t.Fatalf("handler cache-control overridden: %v", w.Header())
	}
	if w := serve(r, "POST", "/api/ok", map[string]string{CSRFHeader: "1"}, ""); w.Code != 200 || w.Body.String() != "{\"ok\":true}\n" {
		t.Fatalf("OK: %d %q", w.Code, w.Body.String())
	}
}

func TestRequestContextCarriesUser(t *testing.T) {
	r, logs := newTestRouter(Options{})
	u := &model.User{ID: "u1", Role: model.RoleUser}
	r.SetAuthenticator(AuthenticatorFunc(func(c *echo.Context) (*model.User, AuthInfo, error) {
		return u, AuthInfo{Method: AuthToken, TokenID: "t1"}, nil
	}))
	r.API().GET("/me", func(c *echo.Context) error {
		ctx := c.Request().Context()
		if UserFrom(c) != u || UserFromContext(ctx) != u || AuthInfoFrom(c).TokenID != "t1" ||
			AuthInfoFromContext(ctx).Method != AuthToken || ClientIPFrom(ctx) == "" {
			return errors.New("context not propagated")
		}
		return OK(c)
	})
	r.PublicWS("/ws/share/:token", func(c *echo.Context) error { return NotFound("gone") })
	if w := serve(r, "GET", "/api/me", nil, ""); w.Code != 200 {
		t.Fatalf("context propagation: %d %s", w.Code, w.Body.String())
	}
	serve(r, "GET", "/ws/share/secret-token", nil, "")
	if out := logs.String(); strings.Contains(out, "secret-token") || !strings.Contains(out, "path=/ws/share/…") {
		t.Fatalf("share token not redacted:\n%s", out)
	}
}

func TestSafePathRedactsTokens(t *testing.T) {
	for in, want := range map[string]string{
		"/api/share/abcdef":                   "/api/share/…",
		"/ws/share/abcdef":                    "/ws/share/…",
		"/proxy/p1a2b3-SECRETKEY/app/x.js":    "/proxy/p1a2b3-…/app/x.js",
		"/proxy/p1a2b3-SECRETKEY":             "/proxy/p1a2b3-…",
		"/proxy/SECRETWITHOUTDASH/index.html": "/proxy/-…/index.html",
		"/api/connections/x":                  "/api/connections/x",
	} {
		if got := safePath(in); got != want {
			t.Errorf("safePath(%q) = %q, want %q", in, got, want)
		}
	}
}
