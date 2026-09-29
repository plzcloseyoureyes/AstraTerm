package events

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v5"

	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
)

type testEnv struct {
	hub  *Hub
	jobs *Jobs
	srv  *httptest.Server
}

var users = map[string]*model.User{
	"alice": {ID: "alice", Username: "alice", Role: model.RoleUser},
	"bob":   {ID: "bob", Username: "bob", Role: model.RoleUser},
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	hub := NewHub(ctx, nil)
	r := httpx.NewRouter(httpx.Options{})
	r.SetAuthenticator(httpx.AuthenticatorFunc(func(c *echo.Context) (*model.User, httpx.AuthInfo, error) {
		name := c.Request().Header.Get("X-Test-User")
		return users[name], httpx.AuthInfo{Method: httpx.AuthCookie, SessionID: "s-" + name}, nil
	}))
	r.WS("/ws/events", hub.ServeWS)
	srv := httptest.NewServer(r)
	t.Cleanup(func() {
		cancel()
		srv.Close()
	})
	return &testEnv{hub: hub, jobs: NewJobs(ctx, hub, nil), srv: srv}
}

type wsClient struct {
	t    *testing.T
	conn *websocket.Conn
	id   string
}

func (e *testEnv) dial(t *testing.T, user string) *wsClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws/events"
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"X-Test-User": {user}}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	c := &wsClient{t: t, conn: conn}
	hello := c.expect("hello")
	c.id, _ = hello["clientId"].(string)
	if c.id == "" || hello["user"].(map[string]any)["id"] != user {
		t.Fatalf("bad hello: %v", hello)
	}
	return c
}

func (c *wsClient) read(timeout time.Duration) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := c.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// expect reads until an event of type typ arrives (skipping others) or fails after 5s.
func (c *wsClient) expect(typ string) map[string]any {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m, err := c.read(time.Until(deadline))
		if err != nil {
			c.t.Fatalf("waiting for %q: %v", typ, err)
		}
		if m["type"] == typ {
			return m
		}
	}
	c.t.Fatalf("timeout waiting for %q", typ)
	return nil
}

func (c *wsClient) send(v any) {
	c.t.Helper()
	b, _ := json.Marshal(v)
	if err := c.conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type promptResult struct {
	resp model.PromptResponse
	err  error
}

func (e *testEnv) promptAsync(ctx context.Context, user string, p model.Prompt) chan promptResult {
	ch := make(chan promptResult, 1)
	go func() {
		r, err := e.hub.Prompt(ctx, user, p)
		ch <- promptResult{r, err}
	}()
	return ch
}

func TestPromptNoClient(t *testing.T) {
	e := newEnv(t)
	_, err := e.hub.Prompt(context.Background(), "alice", model.Prompt{Kind: model.PromptPassword, Title: "pw"})
	if !errors.Is(err, ErrNoInteractiveClient) {
		t.Fatalf("err = %v", err)
	}
}

func TestPromptAnswerFirstWins(t *testing.T) {
	e := newEnv(t)
	a1 := e.dial(t, "alice")
	a2 := e.dial(t, "alice")
	b := e.dial(t, "bob")
	waitFor(t, func() bool { return e.hub.ClientCount() == 3 })

	res := e.promptAsync(context.Background(), "alice", model.Prompt{Kind: model.PromptPassword, Title: "Password",
		Fields: []model.PromptField{{Label: "Password", Echo: false}}, AllowSave: true})
	p1 := a1.expect("prompt")["prompt"].(map[string]any)
	p2 := a2.expect("prompt")["prompt"].(map[string]any)
	id := p1["id"].(string)
	if id == "" || p2["id"] != id || p1["allowSave"] != true || p1["kind"] != "password" {
		t.Fatalf("prompt payloads: %v / %v", p1, p2)
	}

	// Another user's answer is ignored.
	b.send(map[string]any{"type": "prompt.response", "id": id, "accept": true, "values": []string{"evil"}})
	a2.send(map[string]any{"type": "prompt.response", "id": id, "accept": true, "values": []string{"hunter2"}, "save": true})
	r := <-res
	if r.err != nil || !r.resp.Accept || len(r.resp.Values) != 1 || r.resp.Values[0] != "hunter2" || !r.resp.Save {
		t.Fatalf("result: %+v", r)
	}
	// The other socket of the user is told to close the prompt.
	if c := a1.expect("prompt.cancel"); c["id"] != id {
		t.Fatalf("cancel: %v", c)
	}
	// A late second answer is harmless.
	a1.send(map[string]any{"type": "prompt.response", "id": id, "accept": false})
	if e.hub.PendingPrompts() != 0 {
		t.Fatal("prompt still pending")
	}
}

func TestPromptTimeoutAndCancel(t *testing.T) {
	e := newEnv(t)
	e.hub.PromptTimeout = 150 * time.Millisecond
	a := e.dial(t, "alice")
	waitFor(t, func() bool { return e.hub.HasClient("alice") })

	res := e.promptAsync(context.Background(), "alice", model.Prompt{Kind: model.PromptConfirm, Title: "ok?"})
	id := a.expect("prompt")["prompt"].(map[string]any)["id"]
	r := <-res
	if !errors.Is(r.err, ErrPromptTimeout) {
		t.Fatalf("timeout err = %v", r.err)
	}
	if c := a.expect("prompt.cancel"); c["id"] != id {
		t.Fatalf("cancel after timeout: %v", c)
	}

	e.hub.PromptTimeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	res = e.promptAsync(ctx, "alice", model.Prompt{Kind: model.PromptConfirm, Title: "ok?"})
	a.expect("prompt")
	cancel()
	if r := <-res; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("ctx cancel err = %v", r.err)
	}
	a.expect("prompt.cancel")
}

func TestPromptReplayOnReconnect(t *testing.T) {
	e := newEnv(t)
	a := e.dial(t, "alice")
	waitFor(t, func() bool { return e.hub.HasClient("alice") })
	res := e.promptAsync(context.Background(), "alice", model.Prompt{Kind: model.PromptHostKey, Title: "Trust host?",
		HostKey: &model.HostKeyInfo{Host: "h", Port: 22, KeyType: "ssh-ed25519", Fingerprint: "SHA256:x", Status: model.HostKeyUnknown}})
	a.expect("prompt")
	a.conn.Close(websocket.StatusNormalClosure, "reload")
	waitFor(t, func() bool { return !e.hub.HasClient("alice") })

	a2 := e.dial(t, "alice") // "page reload"
	p := a2.expect("prompt")["prompt"].(map[string]any)
	if p["hostKey"].(map[string]any)["fingerprint"] != "SHA256:x" {
		t.Fatalf("replayed prompt: %v", p)
	}
	a2.send(map[string]any{"type": "prompt.response", "id": p["id"], "accept": true})
	if r := <-res; r.err != nil || !r.resp.Accept {
		t.Fatalf("result: %+v", r)
	}
}

func TestPublishPingAndTopics(t *testing.T) {
	e := newEnv(t)
	var unsubs atomic.Int32
	e.hub.RegisterTopic("monitor", func(ctx context.Context, user *model.User, clientID string, params json.RawMessage) (func(), error) {
		var p struct {
			SessionID string `json:"sessionId"`
		}
		json.Unmarshal(params, &p)
		if p.SessionID == "bad" {
			return nil, errors.New("no such session")
		}
		e.hub.PublishClient(clientID, map[string]any{"type": "monitor", "sessionId": p.SessionID, "user": user.ID})
		return func() { unsubs.Add(1) }, nil
	})
	a := e.dial(t, "alice")
	b := e.dial(t, "bob")
	waitFor(t, func() bool { return e.hub.ClientCount() == 2 })

	e.hub.Publish("alice", model.Notify("info", "hi", ""))
	if n := a.expect("notify"); n["title"] != "hi" {
		t.Fatalf("notify: %v", n)
	}
	e.hub.Broadcast(map[string]any{"type": "vault", "locked": true})
	a.expect("vault")
	b.expect("vault")

	a.send(map[string]string{"type": "ping"})
	a.expect("pong")

	a.send(map[string]string{"type": "subscribe", "topic": "monitor", "sessionId": "s1"})
	if m := a.expect("monitor"); m["sessionId"] != "s1" || m["user"] != "alice" {
		t.Fatalf("monitor: %v", m)
	}
	a.send(map[string]string{"type": "subscribe", "topic": "monitor", "sessionId": "bad"})
	if m := a.expect("subscribe.error"); m["error"] != "no such session" {
		t.Fatalf("subscribe error: %v", m)
	}
	a.send(map[string]string{"type": "subscribe", "topic": "nope"})
	a.expect("subscribe.error")

	a.send(map[string]string{"type": "unsubscribe", "topic": "monitor", "sessionId": "s1"})
	waitFor(t, func() bool { return unsubs.Load() == 1 })

	// Disconnect stops remaining subscriptions.
	a.send(map[string]string{"type": "subscribe", "topic": "monitor", "sessionId": "s2"})
	a.expect("monitor")
	a.conn.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return unsubs.Load() == 2 })

	// Closing a user's sessions disconnects them.
	e.hub.CloseAuthSession("s-bob")
	if _, err := b.read(5 * time.Second); err == nil {
		// drain any queued event, the next read must fail
		if _, err := b.read(5 * time.Second); err == nil {
			t.Fatal("bob's socket still open")
		}
	}
}

func TestJobs(t *testing.T) {
	e := newEnv(t)
	a := e.dial(t, "alice")
	waitFor(t, func() bool { return e.hub.HasClient("alice") })
	alice, bob := users["alice"], users["bob"]

	id := e.jobs.Start(alice, "count", func(ctx context.Context, emit func(any)) error {
		for i := range 3 {
			emit(map[string]int{"n": i})
		}
		return nil
	})
	for i := range 3 {
		m := a.expect("job")
		if m["jobId"] != id || m["event"] != "data" || m["data"].(map[string]any)["n"] != float64(i) {
			t.Fatalf("data %d: %v", i, m)
		}
	}
	if m := a.expect("job"); m["event"] != "done" {
		t.Fatalf("done: %v", m)
	}

	started := make(chan struct{})
	id = e.jobs.Start(alice, "wait", func(ctx context.Context, emit func(any)) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	<-started
	if len(e.jobs.List(alice)) != 1 {
		t.Fatal("job not listed")
	}
	if err := e.jobs.Cancel(bob, id); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("foreign cancel: %v", err)
	}
	if err := e.jobs.Cancel(alice, id); err != nil {
		t.Fatal(err)
	}
	if m := a.expect("job"); m["event"] != "error" || m["error"] != "canceled" {
		t.Fatalf("cancel event: %v", m)
	}

	e.jobs.Start(alice, "fail", func(ctx context.Context, emit func(any)) error { return errors.New("boom") })
	if m := a.expect("job"); m["event"] != "error" || m["error"] != "boom" {
		t.Fatalf("error event: %v", m)
	}
	e.jobs.Start(alice, "panic", func(ctx context.Context, emit func(any)) error { panic("oops") })
	if m := a.expect("job"); m["event"] != "error" {
		t.Fatalf("panic event: %v", m)
	}
}

func TestOriginRejected(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws/events"
	_, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{
		"X-Test-User": {"alice"}, "Origin": {"https://evil.example"}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin upgrade: err=%v resp=%v", err, resp)
	}
	_, resp, err = websocket.Dial(ctx, url, nil) // anonymous
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous upgrade: err=%v resp=%v", err, resp)
	}
}
