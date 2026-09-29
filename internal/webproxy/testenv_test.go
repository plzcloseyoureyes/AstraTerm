package webproxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/plzcloseyoureyes/astraterm/internal/server/servertest"
)

// Integration tests against the shared Docker test environment (ASTRATERM_TESTENV=1): ssh1 on 127.0.0.1:22022
// (test/test) reaches the internal nginx "web:80". ASTRATERM_TEST_XPRA=host:port points at an SSH server with xpra
// (test/test) for the Xpra test.

func testenv(t *testing.T) {
	if os.Getenv("ASTRATERM_TESTENV") != "1" {
		t.Skip("set ASTRATERM_TESTENV=1 to run against the Docker test environment")
	}
}

// acceptPrompts answers host-key prompts (trust) and password prompts ("test") on the user's events socket.
func acceptPrompts(t *testing.T, env *servertest.Env, c *servertest.Client) {
	t.Helper()
	ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(env.URL("/ws/events"), "http"),
		&websocket.DialOptions{HTTPClient: c.HTTP})
	if err != nil {
		t.Fatalf("events socket: %v", err)
	}
	ws.SetReadLimit(1 << 24)
	t.Cleanup(func() { ws.CloseNow() })
	hello := make(chan struct{})
	var once sync.Once
	go func() {
		for {
			_, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			switch m["type"] {
			case "hello":
				once.Do(func() { close(hello) })
			case "prompt":
				p, _ := m["prompt"].(map[string]any)
				resp := map[string]any{"type": "prompt.response", "id": p["id"], "accept": true, "save": true}
				if p["kind"] != "hostkey" {
					resp["values"], resp["save"] = []string{"test"}, false
				}
				b, _ := json.Marshal(resp)
				_ = ws.Write(context.Background(), websocket.MessageText, b)
			}
		}
	}()
	select {
	case <-hello:
	case <-time.After(10 * time.Second):
		t.Fatal("no hello on the events socket")
	}
}

func sshConnection(t *testing.T, c *servertest.Client, name, host string, port int) string {
	t.Helper()
	var conn struct {
		ID string `json:"id"`
	}
	c.MustJSON("POST", "/api/connections", map[string]any{
		"name": name, "protocol": "ssh", "host": host, "port": port, "username": "test", "authMethod": "password",
		"secrets": map[string]string{"password": "test"}, "options": map[string]any{},
	}, &conn)
	return conn.ID
}

func TestTestenvProxyThroughSSH(t *testing.T) {
	testenv(t)
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	acceptPrompts(t, env, admin)
	ssh1 := sshConnection(t, admin, "ssh1", "127.0.0.1", 22022)

	// Through the saved SSH connection: "web" is resolved by ssh1.
	p := create(t, admin, map[string]any{"connectionId": ssh1, "url": "http://web/"})
	if p.Via.Kind != "ssh" || p.Via.Label != "ssh1" || p.Target.Host != "web" || p.Target.Port != 80 {
		t.Fatalf("unexpected proxy: %+v", p)
	}
	b := newBrowser(t, env)
	ck := b.enter(p)
	resp, body := b.get(p.Base+"/", http.Header{"Cookie": {"__astraterm_proxy=" + ck}, "Sec-Fetch-Dest": {"iframe"}})
	if resp.StatusCode != 200 || !strings.Contains(body, "Welcome to nginx") {
		t.Fatalf("nginx page: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "astraterm-webproxy") {
		t.Fatal("bridge not injected")
	}
	// A missing page is the upstream's 404, not AstraTerm's.
	if resp, body := b.get(p.Base+"/nope", http.Header{"Cookie": {"__astraterm_proxy=" + ck}}); resp.StatusCode != 404 || !strings.Contains(body, "nginx") {
		t.Fatalf("404: %d %s", resp.StatusCode, body)
	}

	// A saved "web" session reaching web:80 through ssh1 as its SSH gateway (sshTunnelVia).
	var web struct {
		ID string `json:"id"`
	}
	admin.MustJSON("POST", "/api/connections", map[string]any{
		"name": "Intranet", "protocol": "web", "host": "", "port": 0,
		"options": map[string]any{"url": "http://web/", "sshTunnelVia": ssh1},
	}, &web)
	p2 := create(t, admin, map[string]any{"connectionId": web.ID})
	if p2.Via.Kind != "web" || p2.Via.Label != "ssh1" || p2.Title != "Intranet" {
		t.Fatalf("web session proxy: %+v", p2)
	}
	ck2 := b.enter(p2)
	if resp, body := b.get(p2.Base+"/", http.Header{"Cookie": {"__astraterm_proxy=" + ck2}}); resp.StatusCode != 200 || !strings.Contains(body, "Welcome to nginx") {
		t.Fatalf("web session: %d %s", resp.StatusCode, body)
	}

	// A port nothing listens on: the SSH server refuses the channel → clear 422.
	st, code := admin.ErrorCode("POST", "/api/webproxy", map[string]any{"connectionId": ssh1, "host": "localhost", "port": 1})
	if st != 422 || code != "upstream_unreachable" {
		t.Fatalf("closed port: %d %s", st, code)
	}

	// ssh1 has no xpra: a helpful message instead of a failure deep inside.
	var chk struct {
		Installed bool   `json:"installed"`
		Message   string `json:"message"`
	}
	admin.MustJSON("GET", "/api/xpra/check?connectionId="+url.QueryEscape(ssh1), nil, &chk)
	if chk.Installed || !strings.Contains(chk.Message, "not installed") {
		t.Fatalf("xpra check: %+v", chk)
	}
	st, code = admin.ErrorCode("POST", "/api/xpra/start", map[string]any{"connectionId": ssh1, "command": "xterm"})
	if st != 422 || code != "xpra_not_installed" {
		t.Fatalf("xpra start without xpra: %d %s", st, code)
	}
}

func TestTestenvXpra(t *testing.T) {
	testenv(t)
	addr := os.Getenv("ASTRATERM_TEST_XPRA")
	if addr == "" {
		t.Skip("set ASTRATERM_TEST_XPRA=host:port (SSH server with xpra, user test/test)")
	}
	host, portStr, _ := strings.Cut(addr, ":")
	port := 22
	if portStr != "" {
		_ = json.Unmarshal([]byte(portStr), &port)
	}
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	acceptPrompts(t, env, admin)
	conn := sshConnection(t, admin, "xpra-host", host, port)

	var chk struct {
		Installed bool   `json:"installed"`
		HTML5     bool   `json:"html5"`
		Version   string `json:"version"`
	}
	admin.MustJSON("GET", "/api/xpra/check?connectionId="+conn, nil, &chk)
	if !chk.Installed || !chk.HTML5 {
		t.Fatalf("xpra check: %+v", chk)
	}
	var p proxyInfo
	admin.MustJSON("POST", "/api/xpra/start", map[string]any{"connectionId": conn, "command": "xterm", "mode": "seamless"}, &p)
	if p.Kind != "xpra" || p.Target.Host != "127.0.0.1" || p.Title != "xterm" {
		t.Fatalf("xpra proxy: %+v", p)
	}
	b := newBrowser(t, env)
	ck := b.enter(p)
	resp, body := b.get(p.Base+"/", http.Header{"Cookie": {"__astraterm_proxy=" + ck}, "Sec-Fetch-Dest": {"iframe"}})
	if resp.StatusCode != 200 || !strings.Contains(strings.ToLower(body), "xpra") {
		t.Fatalf("xpra html5 client: %d %.300s", resp.StatusCode, body)
	}
	// The HTML5 client's WebSocket reaches xpra through the proxy.
	pu, _ := url.Parse(p.Base)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(env.URL("/"), "http"), &websocket.DialOptions{
		Host: pu.Host, HTTPHeader: http.Header{"Cookie": {"__astraterm_proxy=" + ck}, "Origin": {p.Base}},
		Subprotocols: []string{"binary"},
	})
	if err != nil {
		t.Fatalf("xpra websocket: %v", err)
	}
	ws.CloseNow()

	// Closing the proxy stops xpra (and the application) on the host.
	admin.MustJSON("DELETE", "/api/webproxy/"+p.ID, nil, nil)
	deadline := time.Now().Add(15 * time.Second)
	for {
		st, _ := admin.ErrorCode("POST", "/api/webproxy", map[string]any{"connectionId": conn, "host": "127.0.0.1", "port": p.Target.Port})
		if st == 422 {
			break // nothing listens any more
		}
		if time.Now().After(deadline) {
			t.Fatalf("xpra still listening on %d after the proxy closed (status %d)", p.Target.Port, st)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
