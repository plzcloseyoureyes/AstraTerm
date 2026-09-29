package vnc_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
	"github.com/plzcloseyoureyes/astraterm/internal/vnc"
	"github.com/plzcloseyoureyes/astraterm/internal/vnc/anontls/anontlstest"
	"github.com/plzcloseyoureyes/astraterm/internal/vnc/vnctest"
)

// ---- helpers ------------------------------------------------------------------------------------------------------

// promptAnswerer answers prompt-broker requests of one user over /ws/events.
type promptAnswerer struct {
	mu      sync.Mutex
	prompts []model.Prompt
	events  []map[string]any
	answer  func(p model.Prompt) (accept bool, values []string, save bool)
}

func (a *promptAnswerer) got() []model.Prompt {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]model.Prompt(nil), a.prompts...)
}

func (a *promptAnswerer) seen(typ string) []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []map[string]any
	for _, e := range a.events {
		if e["type"] == typ {
			out = append(out, e)
		}
	}
	return out
}

func answerPrompts(t *testing.T, c *client, answer func(p model.Prompt) (bool, []string, bool)) *promptAnswerer {
	t.Helper()
	ws, _, err := websocket.Dial(context.Background(), c.h.wsURL("/ws/events"), &websocket.DialOptions{HTTPHeader: c.header()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	a := &promptAnswerer{answer: answer}
	hello := make(chan struct{})
	go func() {
		once := sync.Once{}
		for {
			_, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var ev map[string]any
			if json.Unmarshal(data, &ev) != nil {
				continue
			}
			a.mu.Lock()
			a.events = append(a.events, ev)
			a.mu.Unlock()
			switch ev["type"] {
			case "hello":
				once.Do(func() { close(hello) })
			case "prompt":
				var p struct {
					Prompt model.Prompt `json:"prompt"`
				}
				_ = json.Unmarshal(data, &p)
				a.mu.Lock()
				a.prompts = append(a.prompts, p.Prompt)
				a.mu.Unlock()
				accept, values, save := a.answer(p.Prompt)
				resp, _ := json.Marshal(map[string]any{"type": "prompt.response", "id": p.Prompt.ID, "accept": accept,
					"values": values, "save": save})
				_ = ws.Write(context.Background(), websocket.MessageText, resp)
			}
		}
	}()
	select {
	case <-hello:
	case <-time.After(5 * time.Second):
		t.Fatal("no hello on the events socket")
	}
	return a
}

type vncConn struct {
	ws *websocket.Conn
	nc net.Conn
}

func dialVNC(t *testing.T, c *client, sessionID string) *vncConn {
	t.Helper()
	ws, _, err := websocket.Dial(context.Background(), c.h.wsURL("/ws/vnc/"+sessionID), &websocket.DialOptions{HTTPHeader: c.header()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	return &vncConn{ws: ws, nc: websocket.NetConn(context.Background(), ws, websocket.MessageBinary)}
}

// closeStatus waits for the server to close the socket and returns the close code and reason.
func (v *vncConn) closeStatus(t *testing.T) (websocket.StatusCode, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		_, _, err := v.ws.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			if errors.As(err, &ce) {
				return ce.Code, ce.Reason
			}
			t.Fatalf("socket ended without a close frame: %v", err)
		}
	}
}

func createVNCConnection(t *testing.T, c *client, name, addr string, secrets map[string]string, opts map[string]any) string {
	t.Helper()
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	return c.h.connection(c, &model.Connection{Name: name, Host: host, Port: port, Options: opts}, secrets)
}

func openSession(t *testing.T, c *client, body map[string]any) model.RuntimeSession {
	t.Helper()
	var s model.RuntimeSession
	c.MustJSON("POST", "/api/sessions", body, &s)
	if s.Kind != model.KindVNC {
		t.Fatalf("kind %q", s.Kind)
	}
	return s
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func sessionState(c *client, id string) (model.RuntimeSession, int) {
	var s model.RuntimeSession
	st, _ := c.JSON("GET", "/api/sessions/"+id, nil, &s)
	return s, st
}

// ---- tests --------------------------------------------------------------------------------------------------------

func TestVNCSessionEndToEnd(t *testing.T) {
	h := newHarness(t, config.ModeDesktop)
	admin := h.user("admin", model.RoleAdmin)
	answerPrompts(t, admin, func(model.Prompt) (bool, []string, bool) { return false, nil, false })

	srv := &vnctest.Server{Types: []byte{vnctest.SecVNCAuth}, Password: "vncpw", Name: "Test Desktop", Width: 640, Height: 480}
	addr, stop, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	connID := createVNCConnection(t, admin, "fake vnc", addr, map[string]string{"vncPassword": "vncpw"}, nil)
	s := openSession(t, admin, map[string]any{"connectionId": connID, "cols": 80, "rows": 24})

	v := dialVNC(t, admin, s.ID)
	init, err := vnctest.Viewer(v.nc)
	if err != nil {
		t.Fatal(err)
	}
	if name := string(init[24:]); name != "Test Desktop" || binary.BigEndian.Uint16(init[0:]) != 640 {
		t.Fatalf("ServerInit %q", init)
	}
	// The fake server echoes: data flows both ways through the relay.
	if _, err := v.nc.Write([]byte{3, 0, 0, 0, 0, 0, 1, 0, 1, 0}); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 10)
	if _, err := io.ReadFull(v.nc, echo); err != nil || echo[0] != 3 {
		t.Fatalf("echo %v %v", echo, err)
	}
	waitFor(t, "connected state", func() bool {
		cur, _ := sessionState(admin, s.ID)
		return cur.State == model.StateConnected && cur.Clients == 1 && cur.ConnectedAt != nil
	})
	var info vnc.Info
	admin.MustJSON("GET", "/api/sessions/"+s.ID+"/vnc-info", nil, &info)
	if !info.Connected || info.Security != "VNC Authentication" || info.DesktopName != "Test Desktop" || info.Width != 640 ||
		info.Encrypted || info.Route != "direct" || info.Viewers != 1 || info.Protocol != "3.8" {
		t.Fatalf("info %+v", info)
	}

	// Viewer goes away → disconnected, session kept.
	v.ws.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "disconnected state", func() bool {
		cur, _ := sessionState(admin, s.ID)
		return cur.State == model.StateDisconnected && cur.Clients == 0
	})
	// Re-attach works (a new upstream connection; the server keeps the desktop).
	v2 := dialVNC(t, admin, s.ID)
	if _, err := vnctest.Viewer(v2.nc); err != nil {
		t.Fatal(err)
	}
	accepted, _, _, _, ok, _ := srv.Snapshot()
	if accepted != 2 || ok != 2 {
		t.Fatalf("accepted %d authOK %d", accepted, ok)
	}
	// Closing the session ends the viewer with 4410.
	admin.MustJSON("DELETE", "/api/sessions/"+s.ID, nil, nil)
	if code, _ := v2.closeStatus(t); code != vnc.CloseSessionClosed {
		t.Fatalf("close code %d", code)
	}
	waitFor(t, "audit entries", func() bool {
		var entries []model.AuditEntry
		admin.MustJSON("GET", "/api/admin/audit?action=vnc.", nil, &entries)
		var conn, disc int
		for _, e := range entries {
			switch e.Action {
			case "vnc.connect":
				conn++
			case "vnc.disconnect":
				disc++
			}
		}
		return conn == 2 && disc == 2
	})
}

func TestVNCPasswordPromptAndSave(t *testing.T) {
	h := newHarness(t, config.ModeDesktop)
	admin := h.user("admin", model.RoleAdmin)
	srv := &vnctest.Server{Types: []byte{vnctest.SecVNCAuth}, Password: "right", FailReason: "Authentication failure"}
	addr, stop, _ := srv.Listen()
	defer stop()
	// A stored but wrong password: rejected, then the user is asked and saves the right one.
	connID := createVNCConnection(t, admin, "prompted", addr, map[string]string{"vncPassword": "stale"}, nil)
	pa := answerPrompts(t, admin, func(p model.Prompt) (bool, []string, bool) {
		if p.Kind != model.PromptPassword {
			return false, nil, false
		}
		return true, []string{"right"}, true
	})
	s := openSession(t, admin, map[string]any{"connectionId": connID})
	v := dialVNC(t, admin, s.ID)
	if _, err := vnctest.Viewer(v.nc); err != nil {
		t.Fatal(err)
	}
	prompts := pa.got()
	if len(prompts) != 1 || !prompts[0].AllowSave || prompts[0].SessionID != s.ID || prompts[0].ConnectionID != connID ||
		!strings.Contains(prompts[0].Message, "saved password was rejected") {
		t.Fatalf("prompts %+v", prompts)
	}
	conn, err := h.d.Store.Connections.Get(context.Background(), connID)
	if err != nil || !containsStr(conn.SecretKeys, "vncPassword") {
		t.Fatalf("secret keys %v", conn.SecretKeys)
	}
	// The saved password now works without prompting.
	v.ws.Close(websocket.StatusNormalClosure, "")
	s2 := openSession(t, admin, map[string]any{"connectionId": connID})
	v2 := dialVNC(t, admin, s2.ID)
	if _, err := vnctest.Viewer(v2.nc); err != nil {
		t.Fatal(err)
	}
	if n := len(pa.got()); n != 1 {
		t.Fatalf("prompted again (%d prompts)", n)
	}
}

func TestVNCPromptCanceledAndErrors(t *testing.T) {
	h := newHarness(t, config.ModeDesktop)
	admin := h.user("admin", model.RoleAdmin)
	answerPrompts(t, admin, func(model.Prompt) (bool, []string, bool) { return false, nil, false })
	srv := &vnctest.Server{Types: []byte{vnctest.SecVNCAuth}, Password: "pw"}
	addr, stop, _ := srv.Listen()
	defer stop()

	// Canceled prompt → 4499, session in error.
	s := openSession(t, admin, map[string]any{"quick": map[string]any{"protocol": "vnc", "host": "127.0.0.1",
		"port": portOf(addr)}})
	v := dialVNC(t, admin, s.ID)
	if code, reason := v.closeStatus(t); code != vnc.CloseCanceled || !strings.Contains(reason, "canceled") {
		t.Fatalf("close %d %q", code, reason)
	}
	waitFor(t, "error state", func() bool {
		cur, _ := sessionState(admin, s.ID)
		return cur.State == model.StateError
	})

	// Quick-connect password (secrets.password) is used for VNC authentication.
	s = openSession(t, admin, map[string]any{"quick": map[string]any{"protocol": "vnc", "host": "127.0.0.1",
		"port": portOf(addr), "password": "pw"}})
	v = dialVNC(t, admin, s.ID)
	if _, err := vnctest.Viewer(v.nc); err != nil {
		t.Fatal(err)
	}

	// Nothing listening → 4502 with a message.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := l.Addr().String()
	l.Close()
	s = openSession(t, admin, map[string]any{"quick": map[string]any{"protocol": "vnc", "host": "127.0.0.1",
		"port": portOf(dead)}})
	v = dialVNC(t, admin, s.ID)
	if code, reason := v.closeStatus(t); code != vnc.CloseConnectFailed || reason == "" {
		t.Fatalf("close %d %q", code, reason)
	}

	// Unknown session → 4404; other users' sessions are invisible.
	v = dialVNC(t, admin, "doesnotexist0000000")
	if code, _ := v.closeStatus(t); code != vnc.CloseNotFound {
		t.Fatalf("close %d", code)
	}
	bob := h.user("bob", model.RoleUser)
	v = dialVNC(t, bob, s.ID)
	if code, _ := v.closeStatus(t); code != vnc.CloseNotFound {
		t.Fatalf("other user's session: close %d", code)
	}
	if st, _ := bob.JSON("GET", "/api/sessions/"+s.ID+"/vnc-info", nil, nil); st != 404 {
		t.Fatalf("vnc-info of another user's session: %d", st)
	}
}

func TestVNCAdminShadowIsReadOnly(t *testing.T) {
	h := newHarness(t, config.ModeDesktop)
	admin := h.user("admin", model.RoleAdmin)
	alice := h.user("alice", model.RoleUser)
	received := make(chan []byte, 64)
	srv := &vnctest.Server{Handle: func(c net.Conn) {
		br := bufio.NewReader(c)
		for {
			b := make([]byte, 64)
			n, err := br.Read(b)
			if err != nil {
				return
			}
			received <- b[:n]
		}
	}}
	addr, stop, _ := srv.Listen()
	defer stop()
	s := openSession(t, alice, map[string]any{"quick": map[string]any{"protocol": "vnc", "host": "127.0.0.1",
		"port": portOf(addr), "options": map[string]any{"shared": false}}})

	v := dialVNC(t, admin, s.ID)
	if _, err := vnctest.Viewer(v.nc); err != nil {
		t.Fatal(err)
	}
	// Key event + pointer event are dropped; the update request passes.
	if _, err := v.nc.Write([]byte{4, 1, 0, 0, 0, 0, 0, 0x61}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.nc.Write([]byte{5, 1, 0, 1, 0, 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.nc.Write([]byte{3, 1, 0, 0, 0, 0, 0, 10, 0, 10}); err != nil {
		t.Fatal(err)
	}
	var got []byte
	deadline := time.After(5 * time.Second)
	for len(got) < 10 {
		select {
		case b := <-received:
			got = append(got, b...)
		case <-deadline:
			t.Fatalf("server received %x", got)
		}
	}
	if got[0] != 3 || len(got) != 10 {
		t.Fatalf("server received %x (input not filtered)", got)
	}
	_, _, _, shared, _, _ := srv.Snapshot()
	if len(shared) != 1 || shared[0] != 1 {
		t.Fatalf("shadow viewers must connect shared: %v", shared)
	}
	waitFor(t, "shadow audit", func() bool {
		var entries []model.AuditEntry
		admin.MustJSON("GET", "/api/admin/audit?action=session.shadow", nil, &entries)
		return len(entries) == 1 && entries[0].Target == s.ID
	})
}

func TestVNCReverseListener(t *testing.T) {
	h := newHarness(t, config.ModeDesktop)
	admin := h.user("admin", model.RoleAdmin)
	pa := answerPrompts(t, admin, func(model.Prompt) (bool, []string, bool) { return false, nil, false })
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	var li vnc.ListenerInfo
	admin.MustJSON("POST", "/api/vnc/listen", map[string]any{"port": port, "bindHost": "127.0.0.1", "password": "revpw"}, &li)
	if li.Port != port || !li.HasPassword || li.ID == "" {
		t.Fatalf("listener %+v", li)
	}
	if st, code := admin.JSON("POST", "/api/vnc/listen", map[string]any{"port": port, "bindHost": "127.0.0.1"}, nil); st != 409 {
		t.Fatalf("duplicate listener: %d %s", st, code)
	}

	// A VNC server connects to the viewer (x11vnc -connect).
	srv := &vnctest.Server{Types: []byte{vnctest.SecVNCAuth}, Password: "revpw", Name: "reverse"}
	c, err := net.Dial("tcp", li.Address)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(c)
	var sessionID string
	waitFor(t, "vnc.incoming event", func() bool {
		evs := pa.seen("vnc.incoming")
		if len(evs) == 1 {
			sessionID, _ = evs[0]["sessionId"].(string)
		}
		return sessionID != ""
	})
	v := dialVNC(t, admin, sessionID)
	init, err := vnctest.Viewer(v.nc)
	if err != nil {
		t.Fatal(err)
	}
	if string(init[24:]) != "reverse" {
		t.Fatalf("ServerInit %q", init)
	}
	var info vnc.Info
	admin.MustJSON("GET", "/api/sessions/"+sessionID+"/vnc-info", nil, &info)
	if !info.Reverse || info.Route != "incoming connection" {
		t.Fatalf("info %+v", info)
	}
	// A reverse connection serves one viewer.
	v2 := dialVNC(t, admin, sessionID)
	if code, _ := v2.closeStatus(t); code != vnc.CloseUnavailable {
		t.Fatalf("second viewer: close %d", code)
	}
	var list []vnc.ListenerInfo
	admin.MustJSON("GET", "/api/vnc/listen", nil, &list)
	if len(list) != 1 || list[0].Accepted != 1 {
		t.Fatalf("listeners %+v", list)
	}
	admin.MustJSON("DELETE", "/api/vnc/listen/"+li.ID, nil, nil)
	admin.MustJSON("GET", "/api/vnc/listen", nil, &list)
	if len(list) != 0 {
		t.Fatalf("listener not stopped: %+v", list)
	}
	if _, err := net.DialTimeout("tcp", li.Address, time.Second); err == nil {
		t.Fatal("port still open after stopping the listener")
	}
}

func TestVNCListenGatedInServerMode(t *testing.T) {
	h := newHarness(t, config.ModeServer)
	user := h.user("carol", model.RoleUser)
	if st, _ := user.JSON("POST", "/api/vnc/listen", map[string]any{"port": 0, "bindHost": "127.0.0.1"}, nil); st != 403 {
		t.Fatalf("non-admin listen in server mode: %d", st)
	}
	if st, _ := user.JSON("DELETE", "/api/vnc/certs/abc", nil, nil); st != 403 {
		t.Fatalf("non-admin cert delete in server mode: %d", st)
	}
	var certs []vnc.TrustedCert
	user.MustJSON("GET", "/api/vnc/certs", nil, &certs)
	if len(certs) != 0 {
		t.Fatalf("certs %+v", certs)
	}
}

func portOf(addr string) int {
	_, p, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(p)
	return n
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// dialVNCQuery dials /ws/vnc/:id with a query string (e.g. "allow=unencrypted").
func dialVNCQuery(t *testing.T, c *client, sessionID, query string) *vncConn {
	t.Helper()
	ws, _, err := websocket.Dial(context.Background(), c.h.wsURL("/ws/vnc/"+sessionID+"?"+query), &websocket.DialOptions{HTTPHeader: c.header()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	return &vncConn{ws: ws, nc: websocket.NetConn(context.Background(), ws, websocket.MessageBinary)}
}

// TestVNCUnencryptedFallbackNeedsConfirmation: a server whose anonymous TLS fails but that also accepts VNC
// Authentication without encryption. The viewer is stopped with 4426 (never silently downgraded); vnc-info explains
// the choice; the owner's confirmation (?allow=unencrypted) connects and is remembered for the session; the
// downgrade is visible in vnc-info and the audit log.
func TestVNCUnencryptedFallbackNeedsConfirmation(t *testing.T) {
	h := newHarness(t, config.ModeDesktop)
	admin := h.user("admin", model.RoleAdmin)
	answerPrompts(t, admin, func(model.Prompt) (bool, []string, bool) { return false, nil, false })
	srv := &vnctest.Server{Types: []byte{vnctest.SecVeNCrypt}, VeNCryptSubtypes: []uint32{vnctest.VcTLSVnc, vnctest.SecVNCAuth},
		Password: "pw"}
	addr, stop, _ := srv.Listen()
	defer stop()
	connID := createVNCConnection(t, admin, "downgrade", addr, map[string]string{"vncPassword": "pw"}, nil)
	s := openSession(t, admin, map[string]any{"connectionId": connID})

	v := dialVNC(t, admin, s.ID)
	if code, reason := v.closeStatus(t); code != vnc.CloseInsecure || reason != "The server refused the anonymous TLS handshake (handshake failure)" {
		t.Fatalf("close %d %q", code, reason)
	}
	var info vnc.Info
	admin.MustJSON("GET", "/api/sessions/"+s.ID+"/vnc-info", nil, &info)
	if info.Confirm == nil || !info.Confirm.Unencrypted || info.Confirm.WeakTLS || info.Connected || info.EncryptionPolicy != "prefer" {
		t.Fatalf("info %+v confirm %+v", info, info.Confirm)
	}
	if _, _, _, _, ok, _ := srv.Snapshot(); ok != 0 {
		t.Fatal("authenticated without encryption before the user confirmed")
	}

	v = dialVNCQuery(t, admin, s.ID, "allow=unencrypted")
	if _, err := vnctest.Viewer(v.nc); err != nil {
		t.Fatal(err)
	}
	info = vnc.Info{}
	waitFor(t, "connected", func() bool {
		admin.MustJSON("GET", "/api/sessions/"+s.ID+"/vnc-info", nil, &info)
		return info.Connected
	})
	if !info.Connected || info.Encrypted || info.Confirm != nil || info.EncryptionPolicy != "allow-unencrypted" ||
		!strings.Contains(info.Downgrade, "refused the anonymous TLS handshake") {
		t.Fatalf("after confirmation: %+v", info)
	}
	v.ws.Close(websocket.StatusNormalClosure, "")

	// Remembered for the session: the next viewer (e.g. after a network drop) needs no confirmation.
	v = dialVNC(t, admin, s.ID)
	if _, err := vnctest.Viewer(v.nc); err != nil {
		t.Fatal(err)
	}
	// A new session asks again.
	s2 := openSession(t, admin, map[string]any{"connectionId": connID})
	v2 := dialVNC(t, admin, s2.ID)
	if code, _ := v2.closeStatus(t); code != vnc.CloseInsecure {
		t.Fatalf("new session: close %d", code)
	}
	waitFor(t, "audit with the downgrade", func() bool {
		var entries []model.AuditEntry
		admin.MustJSON("GET", "/api/admin/audit?action=vnc.connect", nil, &entries)
		for _, e := range entries {
			var d map[string]any
			if json.Unmarshal(e.Details, &d) == nil && d["downgrade"] != nil && d["encryptionPolicy"] == "allow-unencrypted" {
				return true
			}
		}
		return false
	})
}

// TestVNCEncryptionPolicyOptions: options.encryption=require refuses unencrypted servers (4505, no confirmation
// possible); allow-unencrypted falls back without asking.
func TestVNCEncryptionPolicyOptions(t *testing.T) {
	h := newHarness(t, config.ModeDesktop)
	admin := h.user("admin", model.RoleAdmin)
	answerPrompts(t, admin, func(model.Prompt) (bool, []string, bool) { return false, nil, false })
	plain := &vnctest.Server{Types: []byte{vnctest.SecVNCAuth}, Password: "pw"}
	addr, stop, _ := plain.Listen()
	defer stop()
	req := createVNCConnection(t, admin, "required", addr, map[string]string{"vncPassword": "pw"}, map[string]any{"encryption": "require"})
	s := openSession(t, admin, map[string]any{"connectionId": req})
	// Even the confirmation parameter cannot override require.
	v := dialVNCQuery(t, admin, s.ID, "allow=unencrypted")
	if code, reason := v.closeStatus(t); code != vnc.CloseUnsupported || !strings.Contains(reason, "Encryption is required") {
		t.Fatalf("require: close %d %q", code, reason)
	}
	if _, _, _, _, ok, _ := plain.Snapshot(); ok != 0 {
		t.Fatal("the password was used although encryption is required")
	}

	fallback := &vnctest.Server{Types: []byte{vnctest.SecVeNCrypt}, VeNCryptSubtypes: []uint32{vnctest.VcTLSVnc, vnctest.SecVNCAuth},
		Password: "pw"}
	addr2, stop2, _ := fallback.Listen()
	defer stop2()
	legacy := createVNCConnection(t, admin, "legacy", addr2, map[string]string{"vncPassword": "pw"},
		map[string]any{"encryption": "allow-unencrypted"})
	s = openSession(t, admin, map[string]any{"connectionId": legacy})
	v = dialVNC(t, admin, s.ID)
	if _, err := vnctest.Viewer(v.nc); err != nil {
		t.Fatal(err)
	}
	var info vnc.Info
	waitFor(t, "connected", func() bool {
		admin.MustJSON("GET", "/api/sessions/"+s.ID+"/vnc-info", nil, &info)
		return info.Connected
	})
	if info.Encrypted || info.Downgrade == "" || info.EncryptionPolicy != "allow-unencrypted" {
		t.Fatalf("info %+v", info)
	}
}

// TestVNCWeakTLSConfirmation: a 1024-bit anonymous TLS group is only used after ?allow=weak.
func TestVNCWeakTLSConfirmation(t *testing.T) {
	h := newHarness(t, config.ModeDesktop)
	admin := h.user("admin", model.RoleAdmin)
	answerPrompts(t, admin, func(model.Prompt) (bool, []string, bool) { return false, nil, false })
	srv := &vnctest.Server{Types: []byte{vnctest.SecVeNCrypt}, VeNCryptSubtypes: []uint32{vnctest.VcTLSVnc}, Password: "pw",
		AnonTLS: &anontlstest.Config{DHPrime: anontlstest.RFC5114P1024, DHGenerator: anontlstest.RFC5114G1024}}
	addr, stop, _ := srv.Listen()
	defer stop()
	connID := createVNCConnection(t, admin, "weak", addr, map[string]string{"vncPassword": "pw"}, nil)
	s := openSession(t, admin, map[string]any{"connectionId": connID})
	v := dialVNC(t, admin, s.ID)
	if code, _ := v.closeStatus(t); code != vnc.CloseInsecure {
		t.Fatalf("close %d", code)
	}
	var info vnc.Info
	admin.MustJSON("GET", "/api/sessions/"+s.ID+"/vnc-info", nil, &info)
	if info.Confirm == nil || !info.Confirm.WeakTLS || info.Confirm.DHBits != 1024 || info.Confirm.Unencrypted {
		t.Fatalf("confirm %+v", info.Confirm)
	}
	v = dialVNCQuery(t, admin, s.ID, "allow=weak")
	if _, err := vnctest.Viewer(v.nc); err != nil {
		t.Fatal(err)
	}
	info = vnc.Info{}
	waitFor(t, "connected", func() bool {
		admin.MustJSON("GET", "/api/sessions/"+s.ID+"/vnc-info", nil, &info)
		return info.Connected
	})
	if !info.Encrypted || info.TLS == nil || !info.TLS.Weak || info.TLS.DHBits != 1024 || info.EncryptionPolicy != "allow-weak" {
		t.Fatalf("info %+v tls %+v", info, info.TLS)
	}
}

// TestVNCClipboardPolicy: options.clipboardDirection and the administrator's global vncPolicy limit the clipboard;
// local→remote is enforced by AstraTerm (ClientCutText never reaches the server).
func TestVNCClipboardPolicy(t *testing.T) {
	h := newHarness(t, config.ModeDesktop)
	admin := h.user("admin", model.RoleAdmin)
	answerPrompts(t, admin, func(model.Prompt) (bool, []string, bool) { return false, nil, false })
	received := make(chan []byte, 64)
	srv := &vnctest.Server{Handle: func(c net.Conn) {
		for {
			b := make([]byte, 256)
			n, err := c.Read(b)
			if err != nil {
				return
			}
			received <- b[:n]
		}
	}}
	addr, stop, _ := srv.Listen()
	defer stop()
	connID := createVNCConnection(t, admin, "clip", addr, nil, map[string]any{"clipboardDirection": "from-remote"})
	s := openSession(t, admin, map[string]any{"connectionId": connID})
	v := dialVNC(t, admin, s.ID)
	if _, err := vnctest.Viewer(v.nc); err != nil {
		t.Fatal(err)
	}
	var info vnc.Info
	waitFor(t, "connected", func() bool {
		admin.MustJSON("GET", "/api/sessions/"+s.ID+"/vnc-info", nil, &info)
		return info.Connected
	})
	if info.Clipboard != "from-remote" {
		t.Fatalf("clipboard %q", info.Clipboard)
	}
	cut := append([]byte{6, 0, 0, 0, 0, 0, 0, 6}, "secret"...)
	req := []byte{3, 0, 0, 0, 0, 0, 0, 1, 0, 1}
	if _, err := v.nc.Write(append(cut, req...)); err != nil {
		t.Fatal(err)
	}
	var got []byte
	deadline := time.After(5 * time.Second)
	for len(got) < len(req) {
		select {
		case b := <-received:
			got = append(got, b...)
		case <-deadline:
			t.Fatalf("server received %x", got)
		}
	}
	if !bytes.Equal(got, req) {
		t.Fatalf("server received %x (clipboard not filtered)", got)
	}
	// The administrator's policy applies on top (and users cannot override it).
	if err := h.d.Store.Settings.Set(context.Background(), store.ScopeGlobal, "vncPolicy", []byte(`{"clipboardDirection":"to-remote"}`)); err != nil {
		t.Fatal(err)
	}
	admin.MustJSON("GET", "/api/sessions/"+s.ID+"/vnc-info", nil, &info)
	if info.Clipboard != "from-remote" { // the established connection keeps its policy
		t.Fatalf("clipboard %q", info.Clipboard)
	}
	s2 := openSession(t, admin, map[string]any{"connectionId": connID})
	v2 := dialVNC(t, admin, s2.ID)
	if _, err := vnctest.Viewer(v2.nc); err != nil {
		t.Fatal(err)
	}
	info = vnc.Info{}
	waitFor(t, "connected", func() bool {
		admin.MustJSON("GET", "/api/sessions/"+s2.ID+"/vnc-info", nil, &info)
		return info.Connected
	})
	if info.Clipboard != "none" {
		t.Fatalf("combined policy %q", info.Clipboard)
	}
}
