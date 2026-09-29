package ai_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexterm/nexterm/internal/ai"
	"github.com/nexterm/nexterm/internal/server/servertest"
)

func userMsg(text string) map[string]any {
	return map[string]any{"messages": []any{map[string]string{"role": "user", "content": text}}}
}

// Secrets saved a moment ago (connection password, SSH key passphrase) are redacted on the very next request, and
// suggested commands never carry control characters.
func TestReviewFreshSecretsAndCommandSanitizing(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", "correct horse battery")
	fake := ai.NewFakeProvider(t, "anthropic")
	admin.MustJSON("PUT", "/api/ai/config", map[string]any{"scope": "global", "provider": "anthropic", "baseUrl": fake.URL, "apiKey": testKey}, nil)

	// Prime the secret cache, then store new secrets.
	if code, _, raw := chat(t, admin, userMsg("hello")); code != 200 {
		t.Fatalf("chat %d %s", code, raw)
	}
	admin.MustJSON("POST", "/api/connections", map[string]any{"name": "db", "protocol": "ssh", "host": "db1", "port": 22, "username": "ops",
		"secrets": map[string]string{"password": "Fresh-Conn-Pw-9"}}, nil)
	if st := admin.JSON("POST", "/api/keys/generate", map[string]any{"name": "k", "type": "ed25519", "passphrase": "Key-Passphrase-77"}, nil); st != 201 {
		t.Logf("keys module unavailable (%d): skipping the key passphrase check", st)
	}
	code, _, raw := chat(t, admin, map[string]any{
		"messages": []any{map[string]string{"role": "user", "content": "why? pw Fresh-Conn-Pw-9 / Key-Passphrase-77"}},
		"context":  map[string]any{"terminalText": "$ echo 'Fresh-Conn-Pw-9' | sudo -S ls\n"},
	})
	if code != 200 {
		t.Fatalf("chat %d %s", code, raw)
	}
	for _, leak := range []string{"Fresh-Conn-Pw-9", "Key-Passphrase-77"} {
		if strings.Contains(fake.Last().Raw, leak) {
			t.Fatalf("%q reached the provider: %s", leak, fake.Last().Raw)
		}
	}

	// Control characters in a suggested command (escape sequences, bidi overrides) are stripped server-side.
	fake.Reply = "{\"command\":\"ls\\u001b[201~; rm -rf ~\\u202e\",\"explanation\":\"x\",\"risk\":\"low\"}"
	code, evs, raw := chat(t, admin, map[string]any{"mode": "command", "messages": userMsg("list")["messages"]})
	if code != 200 {
		t.Fatalf("command %d %s", code, raw)
	}
	res := eventData(t, evs, "result")
	if res["command"] != "ls[201~; rm -rf ~" || res["risk"] != "high" {
		t.Fatalf("result %q risk %v", res["command"], res["risk"])
	}
}

// The provider client never follows redirects (Go would forward x-api-key to the new host and replay the body).
func TestReviewNoRedirects(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	env := servertest.New(t)
	admin := env.Setup("admin", "correct horse battery")
	admin.MustJSON("PUT", "/api/ai/config", map[string]any{"scope": "global", "provider": "anthropic", "baseUrl": redirector.URL, "apiKey": testKey}, nil)
	code, c := admin.ErrorCode("POST", "/api/ai/chat", userMsg("hi"))
	if code != 424 || hits.Load() != 0 {
		t.Fatalf("redirect: %d %s, target hits %d", code, c, hits.Load())
	}
	if code, _ := admin.ErrorCode("POST", "/api/ai/test", map[string]any{"scope": "global", "provider": "anthropic", "baseUrl": redirector.URL}); code != 424 || hits.Load() != 0 {
		t.Fatalf("test redirect: %d, target hits %d", code, hits.Load())
	}
}

// Provider error texts that echo the key are scrubbed before they reach the browser.
func TestReviewProviderErrorScrubbed(t *testing.T) {
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		fmt.Fprintf(w, `{"error":{"type":"authentication_error","message":"bad key %s"}}`, r.Header.Get("x-api-key"))
	}))
	defer echo.Close()
	env := servertest.New(t)
	admin := env.Setup("admin", "correct horse battery")
	admin.MustJSON("PUT", "/api/ai/config", map[string]any{"scope": "global", "provider": "anthropic", "baseUrl": echo.URL, "apiKey": testKey}, nil)
	for _, path := range []string{"/api/ai/chat", "/api/ai/test"} {
		body := any(userMsg("hi"))
		if path == "/api/ai/test" {
			body = map[string]any{"scope": "global", "provider": "anthropic", "baseUrl": echo.URL}
		}
		resp, data := admin.Do("POST", path, body)
		if resp.StatusCode != 424 || strings.Contains(string(data), "0123456789abcdef") {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, data)
		}
	}
	var ml map[string]any
	admin.MustJSON("GET", "/api/ai/models", nil, &ml)
	if s := fmt.Sprint(ml); strings.Contains(s, "0123456789abcdef") {
		t.Fatalf("models leak: %s", s)
	}
}

// A user cannot hold more than a few provider streams at once; cancelling a stream frees its slot.
func TestReviewConcurrentStreams(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", "correct horse battery")
	fake := ai.NewFakeProvider(t, "openai")
	fake.Hang = true
	admin.MustJSON("PUT", "/api/ai/config", map[string]any{"scope": "global", "provider": "openai", "baseUrl": fake.URL, "model": "m",
		"rateLimitPerMinute": -1}, nil)

	type open struct {
		resp *http.Response
		stop func()
	}
	var mu sync.Mutex
	var streams []open
	start := func() int {
		req, _ := http.NewRequest("POST", env.URL("/api/ai/chat"), strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-NexTerm", "1")
		for k, v := range admin.Header {
			req.Header[k] = v
		}
		client := &http.Client{Jar: admin.HTTP.Jar}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == 200 {
			mu.Lock()
			streams = append(streams, open{resp, func() { _ = resp.Body.Close() }})
			mu.Unlock()
		} else {
			_ = resp.Body.Close()
		}
		return resp.StatusCode
	}
	for i := 0; i < 4; i++ {
		if st := start(); st != 200 {
			t.Fatalf("stream %d: %d", i, st)
		}
	}
	if st := start(); st != 429 {
		t.Fatalf("5th stream: %d", st)
	}
	streams[0].stop()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := start()
		if st == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot not released: %d", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, s := range streams {
		s.stop()
	}
}
