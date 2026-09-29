package recording_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/termstead/termstead/internal/recording"
)

// shareViewer is a test client of /ws/share/{token}.
type shareViewer struct {
	t    *testing.T
	ws   *websocket.Conn
	ctx  context.Context
	text []string
	out  strings.Builder
}

func dialShare(t *testing.T, e *testEnv, token string) *shareViewer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	ws, _, err := websocket.Dial(ctx, strings.Replace(e.URL("/ws/share/"+token), "http", "ws", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	return &shareViewer{t: t, ws: ws, ctx: ctx}
}

// until reads until cond holds for a received frame; it returns the read error that ended the stream (nil = cond).
func (v *shareViewer) until(cond func(text bool, data string) bool) error {
	v.t.Helper()
	for {
		typ, data, err := v.ws.Read(v.ctx)
		if err != nil {
			return err
		}
		if typ == websocket.MessageText {
			v.text = append(v.text, string(data))
		} else {
			v.out.Write(data)
		}
		if cond(typ == websocket.MessageText, string(data)) {
			return nil
		}
	}
}

func (v *shareViewer) send(data string) {
	_ = v.ws.Write(v.ctx, websocket.MessageBinary, []byte(data))
}

func TestShareRelayPolicy(t *testing.T) {
	e := newEnv(t)
	c := e.admin
	c.MustJSON("PUT", "/api/admin/recordings/policy", map[string]any{"commandAudit": true}, nil)
	s := e.openSession(t, c, map[string]any{})
	e.input(t, c, s.ID, "cd\r") // the owner's cwd (OSC 7) must not reach viewers

	// Read-only viewer: sees output, never the cwd, cannot resize or type.
	var ro recording.ShareView
	c.MustJSON("POST", "/api/sessions/"+s.ID+"/share", map[string]any{"mode": "read", "expiresInSec": 600}, &ro)
	v := dialShare(t, e, ro.Token)
	if err := v.until(func(text bool, d string) bool { return !text && strings.Contains(v.out.String(), "Welcome") }); err != nil {
		t.Fatal(err)
	}
	_ = v.ws.Write(v.ctx, websocket.MessageText, []byte(`{"type":"resize","cols":33,"rows":11}`))
	v.send("touch pwned\r")
	if err := v.until(func(text bool, d string) bool { return text && strings.Contains(d, "read-only") }); err != nil {
		t.Fatal(err)
	}
	var sess struct {
		Cols int `json:"cols"`
	}
	c.MustJSON("GET", "/api/sessions/"+s.ID, nil, &sess)
	if sess.Cols != 80 {
		t.Fatalf("a read-only viewer resized the session to %d columns", sess.Cols)
	}
	for _, m := range v.text {
		if strings.Contains(m, `"cwd"`) || strings.Contains(m, "secret-project") {
			t.Fatalf("cwd leaked to a viewer: %s", m)
		}
	}

	// Revocation ends the viewer's stream with the "share ended" close code at once.
	c.MustJSON("DELETE", "/api/shares/"+ro.ID, nil, nil)
	err := v.until(func(bool, string) bool { return false })
	if code := websocket.CloseStatus(err); code != 4410 {
		t.Fatalf("revoked: close %v (%v)", code, err)
	}

	// Interactive link created paused: input is refused until the owner allows it; then the guest's command is audited
	// with the guest's identity.
	var rw recording.ShareView
	c.MustJSON("POST", "/api/sessions/"+s.ID+"/share", map[string]any{"mode": "write", "expiresInSec": 600, "inputPaused": true, "label": "for dana"}, &rw)
	if !rw.InputPaused {
		t.Fatalf("share not paused: %+v", rw)
	}
	g := dialShare(t, e, rw.Token)
	if err := g.until(func(text bool, d string) bool { return text && strings.Contains(d, `"type":"readonly","value":true`) }); err != nil {
		t.Fatal(err)
	}
	g.send("echo paused\r")
	if err := g.until(func(text bool, d string) bool { return text && strings.Contains(d, "paused guest input") }); err != nil {
		t.Fatal(err)
	}
	var upd recording.ShareView
	c.MustJSON("PUT", "/api/shares/"+rw.ID+"/input", map[string]any{"paused": false}, &upd)
	if upd.InputPaused {
		t.Fatal("still paused")
	}
	if err := g.until(func(text bool, d string) bool { return text && strings.Contains(d, `"type":"readonly","value":false`) }); err != nil {
		t.Fatal(err)
	}
	g.send("whoami-guest\r")
	e.input(t, c, s.ID, "owner-cmd\r")
	type auditEntry struct {
		Details map[string]any `json:"details"`
	}
	var entries []auditEntry
	waitFor(t, "guest command audited", func() bool {
		c.MustJSON("GET", "/api/admin/audit?action=session.command&target="+s.ID, nil, &entries)
		n := 0
		for _, en := range entries {
			if en.Details["command"] == "whoami-guest" || en.Details["command"] == "owner-cmd" {
				n++
			}
		}
		return n == 2
	})
	for _, en := range entries {
		guest, _ := en.Details["guest"].(map[string]any)
		switch en.Details["command"] {
		case "whoami-guest":
			if guest == nil || guest["shareId"] != rw.ID || guest["label"] != "for dana" || guest["ip"] == "" {
				t.Fatalf("guest command not attributed: %+v", en.Details)
			}
		case "owner-cmd", "cd":
			if guest != nil {
				t.Fatalf("owner command attributed to a guest: %+v", en.Details)
			}
		case "echo paused":
			t.Fatal("input typed while paused reached the session")
		}
	}
	// Guests never resize the owner's terminal either.
	_ = g.ws.Write(g.ctx, websocket.MessageText, []byte(`{"type":"resize","cols":40,"rows":10}`))
	time.Sleep(100 * time.Millisecond)
	c.MustJSON("GET", "/api/sessions/"+s.ID, nil, &sess)
	if sess.Cols != 80 {
		t.Fatalf("a guest resized the session to %d columns", sess.Cols)
	}
	// Pausing again takes effect on the open socket.
	c.MustJSON("PUT", "/api/shares/"+rw.ID+"/input", map[string]any{"paused": true}, nil)
	if err := g.until(func(text bool, d string) bool { return text && strings.Contains(d, `"type":"readonly","value":true`) }); err != nil {
		t.Fatal(err)
	}
	// Read links cannot be unpaused; other users cannot touch the link.
	if st, _ := c.ErrorCode("PUT", "/api/shares/"+ro.ID+"/input", map[string]any{"paused": false}); st != 404 {
		t.Fatalf("revoked link input: %d", st)
	}
	bob := e.CreateUser(c, "bob", "bob password 123", "user")
	if st, _ := bob.ErrorCode("PUT", "/api/shares/"+rw.ID+"/input", map[string]any{"paused": false}); st != 404 {
		t.Fatalf("bob changed the link: %d", st)
	}
	// The session ends: the viewer gets a normal closure (final, no reconnect).
	c.MustJSON("DELETE", "/api/sessions/"+s.ID, nil, nil)
	err = g.until(func(bool, string) bool { return false })
	if code := websocket.CloseStatus(err); code != websocket.StatusNormalClosure {
		t.Fatalf("session end: close %v (%v)", code, err)
	}
}

func TestShareViewerLimit(t *testing.T) {
	e := newEnv(t)
	c := e.admin
	s := e.openSession(t, c, map[string]any{})
	var sh recording.ShareView
	c.MustJSON("POST", "/api/sessions/"+s.ID+"/share", map[string]any{"mode": "read", "expiresInSec": 600, "maxViewers": 1}, &sh)
	v := dialShare(t, e, sh.Token)
	if err := v.until(func(text bool, d string) bool { return !text }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, strings.Replace(e.URL("/ws/share/"+sh.Token), "http", "ws", 1), nil)
	if err == nil || resp == nil || resp.StatusCode != 409 {
		t.Fatalf("second viewer admitted: %v %v", err, resp)
	}
}
