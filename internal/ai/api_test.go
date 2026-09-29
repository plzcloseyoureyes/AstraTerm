package ai_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/plzcloseyoureyes/astraterm/internal/ai"
	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/server/servertest"
)

const testKey = "sk-ant-test-0123456789abcdefghijklmnop"

type status struct {
	Available          bool   `json:"available"`
	Enabled            bool   `json:"enabled"`
	Configured         bool   `json:"configured"`
	Reason             string `json:"reason"`
	Provider           string `json:"provider"`
	Model              string `json:"model"`
	CanConfigure       bool   `json:"canConfigure"`
	CanConfigureGlobal bool   `json:"canConfigureGlobal"`
	CanPickModel       bool   `json:"canPickModel"`
	Locked             bool   `json:"locked"`
	Models             []struct {
		ID string `json:"id"`
	} `json:"models"`
}

func chat(t *testing.T, c *servertest.Client, body any) (int, [][2]string, string) {
	t.Helper()
	resp, data := c.Do("POST", "/api/ai/chat", body)
	if resp.StatusCode != 200 {
		return resp.StatusCode, nil, string(data)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	return 200, ai.ParseSSE(string(data)), string(data)
}

func eventData(t *testing.T, evs [][2]string, name string) map[string]any {
	t.Helper()
	for _, e := range evs {
		if e[0] == name {
			var m map[string]any
			if err := json.Unmarshal([]byte(e[1]), &m); err != nil {
				t.Fatal(err)
			}
			return m
		}
	}
	t.Fatalf("no %q event in %v", name, evs)
	return nil
}

func joinDeltas(t *testing.T, evs [][2]string) string {
	var b strings.Builder
	for _, e := range evs {
		if e[0] == "delta" {
			var m map[string]string
			_ = json.Unmarshal([]byte(e[1]), &m)
			b.WriteString(m["text"])
		}
	}
	return b.String()
}

func TestDesktopAnthropicFlow(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", "correct horse battery")
	fake := ai.NewFakeProvider(t, "anthropic")

	var st status
	admin.MustJSON("GET", "/api/ai/status", nil, &st)
	if st.Available || st.Reason != "not_configured" || !st.Enabled || !st.CanConfigureGlobal {
		t.Fatalf("initial status %+v", st)
	}
	if code, c := admin.ErrorCode("POST", "/api/ai/chat", map[string]any{"messages": []any{map[string]string{"role": "user", "content": "hi"}}}); code != 409 || c != "ai_not_configured" {
		t.Fatalf("chat before config: %d %s", code, c)
	}
	if fake.Count() != 0 {
		t.Fatal("network call before configuration")
	}

	// Configure (key is write-only).
	var cfg map[string]any
	admin.MustJSON("PUT", "/api/ai/config", map[string]any{"scope": "global", "provider": "anthropic", "baseUrl": fake.URL + "/",
		"model": "claude-sonnet-5", "apiKey": testKey}, &cfg)
	if cfg["hasKey"] != true || cfg["baseUrl"] != fake.URL {
		t.Fatalf("config %v", cfg)
	}
	for _, path := range []string{"/api/ai/config?scope=global", "/api/settings", "/api/admin/settings", "/api/ai/status"} {
		_, body := admin.Do("GET", path, nil)
		if strings.Contains(string(body), testKey) || strings.Contains(string(body), "0123456789abcdef") {
			t.Fatalf("%s leaks the API key: %s", path, body)
		}
	}
	admin.MustJSON("GET", "/api/ai/status", nil, &st)
	if !st.Available || st.Model != "claude-sonnet-5" || len(st.Models) < 4 {
		t.Fatalf("status %+v", st)
	}
	if fake.Count() != 0 {
		t.Fatal("status must not call the provider")
	}

	// A stored connection secret and a private key in the context must never reach the provider.
	admin.MustJSON("POST", "/api/connections", map[string]any{"name": "web", "protocol": "ssh", "host": "web1", "port": 22, "username": "deploy",
		"secrets": map[string]string{"password": "Sup3r-S3cret-PW"}}, nil)
	fake.Reply = "The disk is **full**."
	code, evs, raw := chat(t, admin, map[string]any{
		"mode":     "explain",
		"messages": []any{map[string]string{"role": "user", "content": "why does my deploy fail? my password is Sup3r-S3cret-PW"}},
		"context": map[string]any{
			"terminalText": "$ ./deploy\nerror: No space left on device\n-----BEGIN OPENSSH PRIVATE KEY-----\nAAAAB3NzaC1yc2EAAAADAQABAAABAQ\n-----END OPENSSH PRIVATE KEY-----\n",
			"selection":    "export API_TOKEN=abc123def456",
			"sessionInfo":  map[string]string{"os": "Ubuntu 24.04", "shell": "bash", "cwd": "/srv/app"},
		},
	})
	if code != 200 {
		t.Fatalf("chat: %d %s", code, raw)
	}
	meta := eventData(t, evs, "meta")
	if meta["redactions"].(float64) < 3 || meta["mode"] != "explain" {
		t.Fatalf("meta %v", meta)
	}
	if got := joinDeltas(t, evs); got != fake.Reply {
		t.Fatalf("deltas %q", got)
	}
	done := eventData(t, evs, "done")
	if done["stopReason"] != "end_turn" {
		t.Fatalf("done %v", done)
	}
	req := fake.Last()
	if req.Header.Get("x-api-key") != testKey || req.Header.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("headers %v", req.Header)
	}
	for _, leak := range []string{"Sup3r-S3cret-PW", "AAAAB3NzaC1yc2EAAAADAQABAAABAQ", "abc123def456"} {
		if strings.Contains(req.Raw, leak) {
			t.Fatalf("secret %q reached the provider: %s", leak, req.Raw)
		}
	}
	if !strings.Contains(req.Raw, "No space left on device") || !strings.Contains(req.Raw, `os=\"Ubuntu 24.04\"`) {
		t.Fatalf("context missing: %s", req.Raw)
	}
	if sys, _ := req.Body["system"].([]any); len(sys) == 0 || !strings.Contains(sys[0].(map[string]any)["text"].(string), "What happened") {
		t.Fatalf("explain system prompt missing: %v", req.Body["system"])
	}

	// Command mode returns a parsed result with a risk rating.
	fake.Reply = `{"command":"du -xh / | sort -h | tail -20","explanation":"Lists the largest directories.","risk":"low"}`
	code, evs, raw = chat(t, admin, map[string]any{"mode": "command", "messages": []any{map[string]string{"role": "user", "content": "what uses my disk"}}})
	if code != 200 {
		t.Fatalf("command: %d %s", code, raw)
	}
	res := eventData(t, evs, "result")
	if res["command"] != "du -xh / | sort -h | tail -20" || res["risk"] != "low" || res["parsed"] != true {
		t.Fatalf("result %v", res)
	}
	if oc, _ := fake.Last().Body["output_config"].(map[string]any); oc["effort"] != "low" {
		t.Fatalf("command mode effort: %v", fake.Last().Body)
	}

	// Model picker: allowed models work, the request names the model.
	code, _, raw = chat(t, admin, map[string]any{"model": "claude-haiku-4-5-20251001", "messages": []any{map[string]string{"role": "user", "content": "hi"}}})
	if code != 200 || fake.Last().Body["model"] != "claude-haiku-4-5-20251001" {
		t.Fatalf("model override: %d %s %v", code, raw, fake.Last().Body["model"])
	}

	// Audit entries carry no content.
	resp, body := admin.Do("GET", "/api/admin/audit?action=ai.request", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "ai.request") {
		t.Fatalf("audit: %d %s", resp.StatusCode, body)
	}
	for _, leak := range []string{"deploy fail", "No space left", "Sup3r", "du -xh"} {
		if strings.Contains(string(body), leak) {
			t.Fatalf("audit contains content %q: %s", leak, body)
		}
	}

	// Test connection and model listing.
	var tr map[string]any
	admin.MustJSON("POST", "/api/ai/test", map[string]any{"scope": "global", "provider": "anthropic", "baseUrl": fake.URL, "model": "claude-sonnet-5"}, &tr)
	if tr["ok"] != true {
		t.Fatalf("test %v", tr)
	}
	var ml map[string]any
	admin.MustJSON("GET", "/api/ai/models?scope=global", nil, &ml)
	if ml["source"] != "provider" || len(ml["models"].([]any)) != 2 {
		t.Fatalf("models %v", ml)
	}

	// Provider errors surface as 424 (failed dependency) with a specific code.
	fake.Set(func(f *ai.FakeProvider) { f.Status = 401 })
	if code, c := admin.ErrorCode("POST", "/api/ai/chat", map[string]any{"messages": []any{map[string]string{"role": "user", "content": "hi"}}}); code != 424 || c != "provider_auth" {
		t.Fatalf("provider 401: %d %s", code, c)
	}
	fake.Set(func(f *ai.FakeProvider) { f.Status = 0 })

	// Validation.
	for _, bad := range []any{
		map[string]any{"messages": []any{}},
		map[string]any{"mode": "agent", "messages": []any{map[string]string{"role": "user", "content": "x"}}},
		map[string]any{"messages": []any{map[string]string{"role": "system", "content": "x"}}},
	} {
		if code, _ := admin.ErrorCode("POST", "/api/ai/chat", bad); code != 400 {
			t.Fatalf("accepted %v (%d)", bad, code)
		}
	}
	if code, _ := admin.ErrorCode("PUT", "/api/ai/config", map[string]any{"scope": "global", "provider": "anthropic", "baseUrl": "file:///etc/passwd"}); code != 400 {
		t.Fatal("bad base URL accepted")
	}

	// Rate limit.
	admin.MustJSON("PUT", "/api/ai/config", map[string]any{"scope": "global", "provider": "anthropic", "baseUrl": fake.URL, "model": "claude-sonnet-5",
		"rateLimitPerMinute": 1}, nil)
	code, c := admin.ErrorCode("POST", "/api/ai/chat", map[string]any{"messages": []any{map[string]string{"role": "user", "content": "hi"}}})
	if code != 429 {
		t.Fatalf("rate limit: %d %s", code, c)
	}

	// Vault lock: the key cannot be read → 423; status reports locked.
	admin.MustJSON("POST", "/api/vault/master-password", map[string]string{"newPassword": "vault master pw"}, nil)
	admin.MustJSON("POST", "/api/vault/lock", nil, nil)
	admin.MustJSON("GET", "/api/ai/status", nil, &st)
	if !st.Locked || st.Reason != "locked" {
		t.Fatalf("locked status %+v", st)
	}
	admin.MustJSON("PUT", "/api/ai/config", map[string]any{"scope": "global", "provider": "anthropic", "baseUrl": fake.URL, "model": "claude-sonnet-5",
		"rateLimitPerMinute": 100}, nil)
	if code, _ := admin.ErrorCode("POST", "/api/ai/chat", map[string]any{"messages": []any{map[string]string{"role": "user", "content": "hi"}}}); code != http.StatusLocked {
		t.Fatalf("locked chat: %d", code)
	}
	if code, _ := admin.ErrorCode("PUT", "/api/ai/config", map[string]any{"scope": "global", "provider": "anthropic", "apiKey": "new-key"}); code != http.StatusLocked {
		t.Fatalf("locked key write: %d", code)
	}

	// Reset removes configuration and key.
	admin.MustJSON("POST", "/api/vault/unlock", map[string]string{"password": "vault master pw"}, nil)
	var after map[string]any
	admin.MustJSON("PUT", "/api/ai/config", map[string]any{"scope": "global", "reset": true}, &after)
	if after["hasKey"] != false || after["provider"] != nil {
		t.Fatalf("reset %v", after)
	}
}

func TestOpenAICompatible(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", "correct horse battery")
	fake := ai.NewFakeProvider(t, "openai")
	admin.MustJSON("PUT", "/api/ai/config", map[string]any{"scope": "global", "provider": "openai", "preset": "ollama", "baseUrl": fake.URL + "/v1",
		"model": "llama3.2"}, nil)
	var st status
	admin.MustJSON("GET", "/api/ai/status", nil, &st)
	if !st.Available || st.Provider != "openai" {
		t.Fatalf("status %+v", st)
	}
	fake.Reply = "Try `journalctl -u nginx`."
	code, evs, raw := chat(t, admin, map[string]any{"messages": []any{
		map[string]string{"role": "user", "content": "nginx fails"},
		map[string]string{"role": "assistant", "content": "What error?"},
		map[string]string{"role": "user", "content": "502"},
	}})
	if code != 200 || joinDeltas(t, evs) != fake.Reply {
		t.Fatalf("chat %d %s", code, raw)
	}
	req := fake.Last()
	if req.Path != "/v1/chat/completions" || req.Header.Get("Authorization") != "" {
		t.Fatalf("req %s %v", req.Path, req.Header)
	}
	if msgs := req.Body["messages"].([]any); len(msgs) != 4 {
		t.Fatalf("messages %v", msgs)
	}
	var ml map[string]any
	admin.MustJSON("GET", "/api/ai/models", nil, &ml)
	if ml["source"] != "provider" {
		t.Fatalf("models %v", ml)
	}

	// A stream that breaks mid-answer ends with an error event (the HTTP status was already 200).
	fake.Set(func(f *ai.FakeProvider) { f.Drop = true })
	code, evs, raw = chat(t, admin, map[string]any{"messages": []any{map[string]string{"role": "user", "content": "hi"}}})
	if code != 200 {
		t.Fatalf("drop: %d %s", code, raw)
	}
	if e := eventData(t, evs, "error"); e["code"] != "provider_unreachable" {
		t.Fatalf("drop error %v", e)
	}
	for _, e := range evs {
		if e[0] == "done" {
			t.Fatal("done after a broken stream")
		}
	}
}

func TestServerModePolicy(t *testing.T) {
	env := servertest.New(t, func(c *config.Config) { c.Mode = config.ModeServer })
	admin := env.Setup("admin", "correct horse battery")
	bob := env.CreateUser(admin, "bob", "bob password 123", "user")
	fake := ai.NewFakeProvider(t, "anthropic")
	msg := map[string]any{"messages": []any{map[string]string{"role": "user", "content": "hi"}}}

	var st status
	bob.MustJSON("GET", "/api/ai/status", nil, &st)
	if st.Enabled || st.Reason != "disabled" || st.CanConfigure || st.CanConfigureGlobal {
		t.Fatalf("server default %+v", st)
	}
	if code, _ := bob.ErrorCode("GET", "/api/ai/config?scope=global", nil); code != 403 {
		t.Fatal("user read global config")
	}
	if code, _ := bob.ErrorCode("PUT", "/api/ai/config", map[string]any{"scope": "global", "provider": "anthropic"}); code != 403 {
		t.Fatal("user wrote global config")
	}
	admin.MustJSON("PUT", "/api/ai/config", map[string]any{"scope": "global", "provider": "anthropic", "baseUrl": fake.URL, "apiKey": testKey,
		"models": []string{"claude-sonnet-5"}}, nil)
	if code, c := bob.ErrorCode("POST", "/api/ai/chat", msg); code != 403 || c != "ai_disabled" {
		t.Fatalf("disabled: %d %s", code, c)
	}
	admin.MustJSON("PUT", "/api/ai/config", map[string]any{"scope": "global", "enabled": true, "provider": "anthropic", "baseUrl": fake.URL,
		"models": []string{"claude-sonnet-5"}}, nil)
	bob.MustJSON("GET", "/api/ai/status", nil, &st)
	if !st.Available || st.CanConfigure || len(st.Models) != 1 {
		t.Fatalf("enabled status %+v", st)
	}
	if code, _, raw := chat(t, bob, msg); code != 200 {
		t.Fatalf("bob chat %d %s", code, raw)
	}
	// Models outside the admin's list are refused.
	if code, c := bob.ErrorCode("POST", "/api/ai/chat", map[string]any{"model": "claude-opus-5-5", "messages": msg["messages"]}); code != 403 || c != "model_not_allowed" {
		t.Fatalf("model policy: %d %s", code, c)
	}
	// Users cannot bring their own endpoint unless allowed…
	if code, _ := bob.ErrorCode("PUT", "/api/ai/config", map[string]any{"scope": "user", "provider": "openai", "baseUrl": fake.URL}); code != 403 {
		t.Fatal("user configured a provider without permission")
	}
	// … and a personal "off" switch is always possible.
	bob.MustJSON("PUT", "/api/ai/config", map[string]any{"scope": "user", "enabled": false}, nil)
	bob.MustJSON("GET", "/api/ai/status", nil, &st)
	if st.Available {
		t.Fatal("personal off ignored")
	}
	bob.MustJSON("PUT", "/api/ai/config", map[string]any{"scope": "user"}, nil)

	// With allowUserConfig, a personal endpoint is used — through the network guard (loopback is refused for users).
	admin.MustJSON("PUT", "/api/ai/config", map[string]any{"scope": "global", "enabled": true, "provider": "anthropic", "baseUrl": fake.URL,
		"allowUserConfig": true}, nil)
	before := fake.Count()
	bob.MustJSON("PUT", "/api/ai/config", map[string]any{"scope": "user", "provider": "anthropic", "baseUrl": fake.URL, "apiKey": "bob-own-key-123"}, nil)
	code, c := bob.ErrorCode("POST", "/api/ai/chat", msg)
	if code != 424 || c != "provider_unreachable" || fake.Count() != before {
		t.Fatalf("guarded personal endpoint: %d %s (requests %d→%d)", code, c, before, fake.Count())
	}
	// The admin's own requests still use the organisation endpoint.
	if code, _, raw := chat(t, admin, msg); code != 200 || fake.Last().Header.Get("x-api-key") != testKey {
		t.Fatalf("admin chat %d %s", code, raw)
	}
}
