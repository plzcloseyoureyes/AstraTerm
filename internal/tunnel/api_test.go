package tunnel

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

func TestCRUDAndOwnership(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	bob := h.user("bob", false)
	conn := h.sshConn(alice, srv, nil)

	// Validation.
	for _, bad := range []Input{
		{Name: "", Type: "local", ConnectionID: conn.ID, DestPort: 1},
		{Name: "x", Type: "local", DestPort: 1},
		{Name: "x", Type: "local", ConnectionID: conn.ID},
		{Name: "x", Type: "sideways", ConnectionID: conn.ID, DestPort: 1},
		{Name: strings.Repeat("n", 201), Type: "local", ConnectionID: conn.ID, DestPort: 1},
		{Name: "x", Type: "local", ConnectionID: conn.ID, DestPort: 1, Secrets: map[string]string{"other": "x"}},
	} {
		if code, body := h.call(alice, "POST", "/api/tunnels", bad, nil); code != http.StatusBadRequest {
			t.Fatalf("create %+v: %d %s", bad, code, body)
		}
	}
	// Bob cannot use Alice's private connection.
	if code, _ := h.call(bob, "POST", "/api/tunnels", Input{Name: "x", Type: "local", ConnectionID: conn.ID, DestPort: 1}, nil); code != http.StatusBadRequest {
		t.Fatalf("foreign connection accepted: %d", code)
	}
	// A non-SSH connection is refused.
	telnet := &model.Connection{Name: "t", Protocol: model.ProtoTelnet, Host: "h", Port: 23, OwnerID: alice.ID}
	telnet.Normalize()
	if err := h.d.Store.Connections.Create(h.ctx, telnet); err != nil {
		t.Fatal(err)
	}
	if code, body := h.call(alice, "POST", "/api/tunnels", Input{Name: "x", Type: "local", ConnectionID: telnet.ID, DestPort: 1}, nil); code != http.StatusBadRequest || !strings.Contains(string(body), "not an SSH connection") {
		t.Fatalf("telnet connection: %d %s", code, body)
	}

	a := h.createTunnel(alice, Input{Name: " db ", Type: "local", ConnectionID: conn.ID, BindPort: 15432, DestHost: "db", DestPort: 5432,
		AutoStart: true, Options: Options{Notes: "prod db", Color: "#f00"}})
	b := h.createTunnel(alice, Input{Name: "cache", Type: "local", ConnectionID: conn.ID, DestHost: "redis", DestPort: 6379})
	if a.Name != "db" || !a.AutoStart || a.Options.Notes != "prod db" || a.SortOrder >= b.SortOrder {
		t.Fatalf("created: %+v / %+v", a, b)
	}

	var list []View
	h.must(alice, "GET", "/api/tunnels", nil, &list, http.StatusOK)
	if len(list) != 2 || list[0].ID != a.ID || list[1].ID != b.ID || list[0].Connection == nil {
		t.Fatalf("list = %+v", list)
	}
	h.must(bob, "GET", "/api/tunnels", nil, &list, http.StatusOK)
	if len(list) != 0 {
		t.Fatalf("bob sees %d tunnels", len(list))
	}
	for _, p := range []string{"GET /api/tunnels/", "PATCH /api/tunnels/", "DELETE /api/tunnels/", "POST /api/tunnels/%/start"} {
		method, path, _ := strings.Cut(p, " ")
		path = strings.Replace(path, "%", a.ID, 1)
		if !strings.Contains(path, a.ID) {
			path += a.ID
		}
		if code, _ := h.call(bob, method, path, map[string]any{"name": "pwn"}, nil); code != http.StatusNotFound {
			t.Fatalf("bob %s %s: %d", method, path, code)
		}
	}
	if code, _ := h.call(nil, "GET", "/api/tunnels", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: %d", code)
	}

	// PATCH: only present fields change; options are replaced as a whole.
	var upd View
	h.must(alice, "PATCH", "/api/tunnels/"+a.ID, map[string]any{"name": "database", "options": map[string]any{"color": "#0f0"}, "status": "ignored"}, &upd, http.StatusOK)
	if upd.Name != "database" || upd.DestHost != "db" || upd.Options.Color != "#0f0" || upd.Options.Notes != "" || !upd.AutoStart {
		t.Fatalf("patched: %+v", upd)
	}
	if code, _ := h.call(alice, "PATCH", "/api/tunnels/"+a.ID, map[string]any{"bindPort": 99999}, nil); code != http.StatusBadRequest {
		t.Fatalf("invalid patch accepted: %d", code)
	}
	if code, _ := h.call(alice, "PATCH", "/api/tunnels/"+a.ID, map[string]any{"bindPort": "x"}, nil); code != http.StatusBadRequest {
		t.Fatalf("mistyped patch accepted: %d", code)
	}

	// Duplicate.
	var dup View
	h.must(alice, "POST", "/api/tunnels/"+a.ID+"/duplicate", nil, &dup, http.StatusCreated)
	if dup.ID == a.ID || dup.Name != "database (copy)" || dup.AutoStart || dup.DestHost != "db" {
		t.Fatalf("duplicate: %+v", dup)
	}

	// Reorder.
	h.must(alice, "POST", "/api/tunnels/reorder", map[string]any{"items": []map[string]any{
		{"id": dup.ID, "sortOrder": 1}, {"id": b.ID, "sortOrder": 2}, {"id": a.ID, "sortOrder": 3}}}, nil, http.StatusOK)
	h.must(alice, "GET", "/api/tunnels", nil, &list, http.StatusOK)
	if list[0].ID != dup.ID || list[1].ID != b.ID || list[2].ID != a.ID {
		t.Fatalf("order after reorder: %s %s %s", list[0].Name, list[1].Name, list[2].Name)
	}
	if code, _ := h.call(bob, "POST", "/api/tunnels/reorder", map[string]any{"items": []map[string]any{{"id": a.ID, "sortOrder": 0}}}, nil); code != http.StatusNotFound {
		t.Fatalf("bob reordered alice's tunnel: %d", code)
	}

	// Delete.
	h.must(alice, "DELETE", "/api/tunnels/"+dup.ID, nil, nil, http.StatusOK)
	if code, _ := h.call(alice, "GET", "/api/tunnels/"+dup.ID, nil, nil); code != http.StatusNotFound {
		t.Fatalf("deleted tunnel still readable: %d", code)
	}
	var n int
	h.d.Store.DB.QueryRow(`SELECT COUNT(*) FROM tunnel_meta WHERE tunnel_id = ?`, dup.ID).Scan(&n)
	if n != 0 {
		t.Fatal("tunnel_meta row survived the delete")
	}
}

func TestDeleteRunningTunnelAndConnectionCascade(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	_, webPort := webServer(t)
	v := h.createTunnel(alice, Input{Name: "l", Type: "local", ConnectionID: conn.ID, DestHost: "127.0.0.1", DestPort: webPort})
	st := h.startTunnel(alice, v.ID)
	h.must(alice, "DELETE", "/api/tunnels/"+v.ID, nil, nil, http.StatusOK)
	if c, err := net.DialTimeout("tcp", st.LocalAddr, time.Second); err == nil {
		c.Close()
		t.Fatal("listener of a deleted tunnel still open")
	}

	// Deleting the connection cascades to its tunnels; the reconciler stops a running one.
	v2 := h.createTunnel(alice, Input{Name: "l2", Type: "local", ConnectionID: conn.ID, DestHost: "127.0.0.1", DestPort: webPort})
	st = h.startTunnel(alice, v2.ID)
	if err := h.d.Store.Connections.Delete(h.ctx, conn.ID); err != nil {
		t.Fatal(err)
	}
	h.m.reconcile()
	if h.m.running(v2.ID) {
		t.Fatal("tunnel of a deleted connection still running")
	}
	if c, err := net.DialTimeout("tcp", st.LocalAddr, time.Second); err == nil {
		c.Close()
		t.Fatal("listener still open after the reconcile")
	}
}

func TestStartAllStopAllAndRestartOnEdit(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	_, webPort := webServer(t)
	a := h.createTunnel(alice, Input{Name: "a", Type: "local", ConnectionID: conn.ID, DestHost: "127.0.0.1", DestPort: webPort})
	b := h.createTunnel(alice, Input{Name: "b", Type: "dynamic", ConnectionID: conn.ID})
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	c := h.createTunnel(alice, Input{Name: "c", Type: "local", ConnectionID: conn.ID, BindPort: busy.Addr().(*net.TCPAddr).Port, DestPort: 1})

	var res struct {
		Started int           `json:"started"`
		Failed  []bulkFailure `json:"failed"`
	}
	h.must(alice, "POST", "/api/tunnels/start-all", nil, &res, http.StatusOK)
	if res.Started != 2 || len(res.Failed) != 1 || res.Failed[0].ID != c.ID || !strings.Contains(res.Failed[0].Error, "in use") {
		t.Fatalf("start-all = %+v", res)
	}
	h.waitStatus(a.ID, func(s Status) bool { return s.State == model.TunnelRunning && s.Connected })
	h.waitStatus(b.ID, func(s Status) bool { return s.State == model.TunnelRunning && s.Connected })

	// Editing the forward of a running tunnel restarts it with the new settings.
	old := h.m.status(a.ID).LocalAddr
	var upd View
	h.must(alice, "PATCH", "/api/tunnels/"+a.ID, map[string]any{"bindPort": 0, "bindHost": "localhost"}, &upd, http.StatusOK)
	st := h.waitStatus(a.ID, func(s Status) bool { return s.State == model.TunnelRunning && s.Connected })
	if got := httpGet(t, st.LocalAddr, "/edited"); got != "hello /edited" {
		t.Fatalf("after edit: %q (was %s now %s)", got, old, st.LocalAddr)
	}
	// Renaming alone does not restart.
	before := h.m.status(a.ID).StartedAt
	h.must(alice, "PATCH", "/api/tunnels/"+a.ID, map[string]any{"name": "renamed"}, &upd, http.StatusOK)
	if after := h.m.status(a.ID).StartedAt; after == nil || before == nil || !after.Equal(*before) {
		t.Fatal("a rename restarted the tunnel")
	}

	var stopped struct {
		Stopped int `json:"stopped"`
	}
	h.must(alice, "POST", "/api/tunnels/stop-all", map[string]any{"ids": []string{b.ID}}, &stopped, http.StatusOK)
	if stopped.Stopped != 1 || h.m.running(b.ID) || !h.m.running(a.ID) {
		t.Fatalf("stop-all with ids: %+v", stopped)
	}
	h.must(alice, "POST", "/api/tunnels/stop-all", nil, &stopped, http.StatusOK)
	if stopped.Stopped != 1 || h.m.running(a.ID) {
		t.Fatalf("stop-all: %+v", stopped)
	}
}

func TestExportImport(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	bob := h.user("bob", false)
	conn := h.sshConn(alice, srv, nil)
	h.createTunnel(alice, Input{Name: "web", Type: "local", ConnectionID: conn.ID, BindPort: 18080, DestHost: "web", DestPort: 80, AutoStart: true})
	h.createTunnel(alice, Input{Name: "socks", Type: "dynamic", ConnectionID: conn.ID, Options: Options{SocksUsername: "u"},
		Secrets: map[string]string{"socksPassword": "p"}})

	code, body := h.call(alice, "GET", "/api/tunnels/export", nil, nil)
	if code != http.StatusOK || strings.Contains(string(body), `"p"`) || strings.Contains(string(body), "socksPassword") {
		t.Fatalf("export: %d %s", code, body)
	}
	var file ExportFile
	if err := json.Unmarshal(body, &file); err != nil || file.Format != exportFormat || len(file.Tunnels) != 2 || file.Tunnels[0].Connection.Name != "srv" {
		t.Fatalf("export file: %+v %v", file, err)
	}

	// Bob has a connection to the same address: it is matched by address; nothing is created on a dry run.
	bconn := h.sshConn(bob, srv, nil)
	var res struct {
		Created []View          `json:"created"`
		Planned []importPlanned `json:"planned"`
		Skipped []importSkipped `json:"skipped"`
	}
	h.must(bob, "POST", "/api/tunnels/import", map[string]any{"file": file, "dryRun": true}, &res, http.StatusOK)
	if len(res.Planned) != 2 || res.Planned[0].ConnectionID != bconn.ID || res.Planned[0].Matched != "name+address" || len(res.Created) != 0 {
		t.Fatalf("dry run: %+v", res)
	}
	h.must(bob, "POST", "/api/tunnels/import", map[string]any{"file": file}, &res, http.StatusOK)
	if len(res.Created) != 2 || res.Created[0].AutoStart || res.Created[1].Options.SocksUsername != "" {
		t.Fatalf("import: %+v", res)
	}
	var list []View
	h.must(bob, "GET", "/api/tunnels", nil, &list, http.StatusOK)
	if len(list) != 2 || list[0].ConnectionID != bconn.ID {
		t.Fatalf("bob's tunnels: %+v", list)
	}

	// Unmatched connections are skipped unless a default connection is given.
	file.Tunnels[0].Connection = ConnRef{ID: "nope", Name: "elsewhere", Host: "10.9.9.9", Port: 22}
	file.Tunnels = file.Tunnels[:1]
	carol := h.user("carol", false)
	cconn := h.sshConn(carol, srv, nil)
	h.must(carol, "POST", "/api/tunnels/import", map[string]any{"file": file}, &res, http.StatusOK)
	if len(res.Created) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("unmatched import: %+v", res)
	}
	h.must(carol, "POST", "/api/tunnels/import", map[string]any{"file": file, "defaultConnectionId": cconn.ID}, &res, http.StatusOK)
	if len(res.Created) != 1 || res.Planned[0].Matched != "default" {
		t.Fatalf("default connection import: %+v", res)
	}
	if code, _ := h.call(carol, "POST", "/api/tunnels/import", map[string]any{"file": map[string]any{"format": "other"}}, nil); code != http.StatusBadRequest {
		t.Fatalf("foreign file format: %d", code)
	}
}

func TestServerModePolicyThroughAPI(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Mode = config.ModeServer; c.Listen = "0.0.0.0:7822" })
	srv := newTestSSHServer(t)
	user := h.user("user", false)
	admin := h.user("admin", true)
	uconn := h.sshConn(user, srv, nil)
	aconn := h.sshConn(admin, srv, nil)
	for _, in := range []Input{
		{Name: "r", Type: "remote", ConnectionID: uconn.ID, DestPort: 80},
		{Name: "rd", Type: "dynamic", ConnectionID: uconn.ID, Options: Options{Reverse: true}},
		{Name: "all", Type: "local", ConnectionID: uconn.ID, BindHost: "0.0.0.0", DestPort: 80},
		{Name: "low", Type: "local", ConnectionID: uconn.ID, BindPort: 80, DestPort: 80},
	} {
		if code, body := h.call(user, "POST", "/api/tunnels", in, nil); code != http.StatusForbidden {
			t.Fatalf("server mode user %s: %d %s", in.Name, code, body)
		}
	}
	h.createTunnel(user, Input{Name: "ok", Type: "local", ConnectionID: uconn.ID, DestPort: 80})
	h.createTunnel(admin, Input{Name: "r", Type: "remote", ConnectionID: aconn.ID, BindHost: "*", DestPort: 80})
}

func TestSessionForwardGatewayPortsWarning(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	srv.Exec = func(cmd string) (string, int) {
		if strings.Contains(cmd, "ss -ltn") {
			return ssListing(srv, false), 0
		}
		return "", 0
	}
	alice := h.user("alice", false)
	_, webPort := webServer(t)
	conn := h.sshConn(alice, srv, model.Options{"forwards": []any{
		map[string]any{"type": "remote", "bindHost": "*", "bindPort": 0, "destHost": "127.0.0.1", "destPort": webPort},
	}})
	s, err := h.sessions.Create(h.ctx, alice, term.CreateRequest{ConnectionID: conn.ID, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "session forward warning", func() bool {
		fwds := h.m.sf.list(alice, s.ID)
		return len(fwds) == 1 && fwds[0].Status.State == model.TunnelRunning && strings.Contains(fwds[0].Status.Warning, "GatewayPorts")
	})
	waitFor(t, "warning notice", func() bool { return strings.Contains(string(s.Scrollback()), "GatewayPorts") })
}

func TestSessionIntegratedForwards(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	_, webPort := webServer(t)
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	port := free.Addr().(*net.TCPAddr).Port
	free.Close()
	conn := h.sshConn(alice, srv, model.Options{"forwards": []any{
		map[string]any{"type": "local", "bindPort": port, "destHost": "127.0.0.1", "destPort": webPort},
		map[string]any{"type": "remote", "bindPort": 0, "destHost": "127.0.0.1", "destPort": webPort},
		map[string]any{"type": "local", "bindPort": -5, "destPort": 1},
	}})
	s, err := h.sessions.Create(h.ctx, alice, term.CreateRequest{ConnectionID: conn.ID, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	var fwds []SessionForward
	waitFor(t, "session forwards", func() bool {
		fwds = h.m.sf.list(alice, s.ID)
		running := 0
		for _, f := range fwds {
			if f.Status.State == model.TunnelRunning {
				running++
			}
		}
		return len(fwds) == 3 && running == 2
	})
	var remoteAddr string
	for _, f := range fwds {
		if f.Spec.Type == "remote" {
			remoteAddr = f.Status.RemoteAddr
		}
		if f.Spec.BindPort == -5 && f.Status.State != model.TunnelError {
			t.Fatalf("invalid configured forward: %+v", f)
		}
		if f.Source != "connection" || f.SessionID != s.ID || f.ConnectionID != conn.ID {
			t.Fatalf("forward view: %+v", f)
		}
	}
	if got := httpGet(t, "127.0.0.1:"+itoa(port), "/session"); got != "hello /session" {
		t.Fatalf("session local forward = %q", got)
	}
	if got := httpGet(t, remoteAddr, "/session-remote"); got != "hello /session-remote" {
		t.Fatalf("session remote forward = %q", got)
	}
	// The terminal shows notices.
	waitFor(t, "notices", func() bool {
		sb := string(s.Scrollback())
		return strings.Contains(sb, "Port forward active") && strings.Contains(sb, "failed")
	})

	// Ad-hoc forward via the API.
	var adhoc SessionForward
	h.must(alice, "POST", "/api/tunnels/session-forwards", map[string]any{"sessionId": s.ID, "type": "local", "destHost": "127.0.0.1", "destPort": webPort}, &adhoc, http.StatusCreated)
	if adhoc.Source != "adhoc" || adhoc.Status.State != model.TunnelRunning || adhoc.Status.LocalAddr == "" {
		t.Fatalf("ad-hoc forward: %+v", adhoc)
	}
	if got := httpGet(t, adhoc.Status.LocalAddr, "/adhoc"); got != "hello /adhoc" {
		t.Fatalf("ad-hoc forward GET = %q", got)
	}
	bob := h.user("bob", false)
	if code, _ := h.call(bob, "POST", "/api/tunnels/session-forwards", map[string]any{"sessionId": s.ID, "type": "local", "destPort": 1}, nil); code != http.StatusNotFound {
		t.Fatalf("bob added a forward to alice's session: %d", code)
	}
	var list []SessionForward
	h.must(alice, "GET", "/api/tunnels/session-forwards", nil, &list, http.StatusOK)
	if len(list) != 4 {
		t.Fatalf("session forwards = %d", len(list))
	}
	h.must(bob, "GET", "/api/tunnels/session-forwards", nil, &list, http.StatusOK)
	if len(list) != 0 {
		t.Fatal("bob sees alice's session forwards")
	}

	// Reconnect: forwards come back on the new SSH client.
	srv.KillConnections()
	waitFor(t, "forwards down", func() bool {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(port), 200*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err != nil
	})
	if err := h.sessions.Reconnect(s.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "forwards back", func() bool {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(port), 200*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	if got := httpGet(t, "127.0.0.1:"+itoa(port), "/again"); got != "hello /again" {
		t.Fatalf("after reconnect = %q", got)
	}

	// Removing the ad-hoc forward, then closing the session, stops everything.
	h.must(alice, "DELETE", "/api/tunnels/session-forwards/"+adhoc.ID, nil, nil, http.StatusOK)
	if err := h.sessions.Close(s.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "forwards gone", func() bool {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(port), 200*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err != nil && len(h.m.sf.list(alice, "")) == 0
	})
}

func TestRemotePortsEndpoint(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	srv.Exec = func(cmd string) (string, int) {
		if strings.Contains(cmd, "@@ss") {
			return "@@ss\n" + ssSample, 0
		}
		return "", 127
	}
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	var res RemotePorts
	h.must(alice, "GET", "/api/tunnels/remote-ports?connectionId="+conn.ID, nil, &res, http.StatusOK)
	if res.Method != "ss" || len(res.Ports) != 5 || res.Ports[0].Port != 22 || res.Ports[0].Process != "sshd" {
		t.Fatalf("remote ports = %+v", res)
	}
	if code, _ := h.call(alice, "GET", "/api/tunnels/remote-ports", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("no target: %d", code)
	}
	bob := h.user("bob", false)
	if code, _ := h.call(bob, "GET", "/api/tunnels/remote-ports?connectionId="+conn.ID, nil, nil); code != http.StatusNotFound {
		t.Fatalf("bob probed alice's connection: %d", code)
	}
}

func TestPortsWatcherTopic(t *testing.T) {
	h := newHarness(t)
	h.d.Router.WS("/ws/events", h.d.Events.ServeWS)
	srv := newTestSSHServer(t)
	var mu sync.Mutex
	out := "@@ss\n" + "LISTEN 0 128 0.0.0.0:22 0.0.0.0:*\n"
	srv.Exec = func(cmd string) (string, int) {
		mu.Lock()
		defer mu.Unlock()
		return out, 0
	}
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	s, err := h.sessions.Create(h.ctx, alice, term.CreateRequest{ConnectionID: conn.ID, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "session connected", func() bool { st, _ := s.State(); return st == model.StateConnected })

	ctx, cancel := context.WithTimeout(h.ctx, 30*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, strings.Replace(h.api.URL, "http", "ws", 1)+"/ws/events",
		&websocket.DialOptions{HTTPHeader: http.Header{"X-Test-User": {"alice"}, "Origin": {h.api.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	sub, _ := json.Marshal(map[string]any{"type": "subscribe", "topic": "tunnel.ports", "sessionId": s.ID})
	ws.Write(ctx, websocket.MessageText, sub)
	next := func() PortsEvent {
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var ev PortsEvent
			if json.Unmarshal(b, &ev) == nil && ev.Type == evPorts {
				return ev
			}
		}
	}
	ev := next()
	if !ev.Initial || len(ev.Ports) != 1 || ev.Ports[0].Port != 22 {
		t.Fatalf("initial event: %+v", ev)
	}
	mu.Lock()
	out = "@@ss\n" + "LISTEN 0 128 0.0.0.0:22 0.0.0.0:*\nLISTEN 0 128 127.0.0.1:8080 0.0.0.0:* users:((\"node\",pid=7,fd=3))\n"
	mu.Unlock()
	ev = next()
	if ev.Initial || len(ev.Added) != 1 || ev.Added[0].Port != 8080 || ev.Added[0].Process != "node" {
		t.Fatalf("added event: %+v", ev)
	}
}

func TestAutostartWaitsForVaultUnlock(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	_, webPort := webServer(t)
	v := h.createTunnel(alice, Input{Name: "auto", Type: "local", ConnectionID: conn.ID, DestHost: "127.0.0.1", DestPort: webPort, AutoStart: true})
	if err := h.d.Vault.SetMasterPassword(h.ctx, "", "master-pw"); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Vault.Lock(); err != nil {
		t.Fatal(err)
	}
	// Manual start with a locked vault: 423 for the UI to unlock and retry.
	if code, _ := h.call(alice, "POST", "/api/tunnels/"+v.ID+"/start", nil, nil); code != http.StatusLocked {
		t.Fatalf("start with a locked vault: %d", code)
	}
	// Autostart waits for the unlock, then connects.
	go h.m.autostartAll()
	h.waitStatus(v.ID, func(s Status) bool { return s.Waiting == "vault" })
	if err := h.d.Vault.Unlock(h.ctx, "master-pw"); err != nil {
		t.Fatal(err)
	}
	st := h.waitStatus(v.ID, func(s Status) bool { return s.State == model.TunnelRunning && s.Connected })
	if got := httpGet(t, st.LocalAddr, "/auto"); got != "hello /auto" {
		t.Fatalf("GET = %q", got)
	}
}

func TestAutostartSkipsDisabledOwnersAndRetries(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	_, webPort := webServer(t)
	v := h.createTunnel(alice, Input{Name: "auto", Type: "local", ConnectionID: conn.ID, DestHost: "127.0.0.1", DestPort: webPort, AutoStart: true})
	h.m.autostartAll()
	h.waitStatus(v.ID, func(s Status) bool { return s.State == model.TunnelRunning && s.Connected })
	h.m.stop(v.ID)

	// The SSH server is down at boot: an autostart keeps retrying instead of failing.
	srv.ln.Close()
	srv.KillConnections()
	go h.m.autostartAll()
	st := h.waitStatus(v.ID, func(s Status) bool { return s.State == model.TunnelStarting && s.RetryAt != nil })
	if !strings.Contains(st.Error, "retrying") {
		t.Fatalf("retrying autostart: %+v", st)
	}
	h.m.stop(v.ID)

	alice.Disabled = true
	if err := h.d.Store.Users.Update(h.ctx, alice); err != nil {
		t.Fatal(err)
	}
	h.m.autostartAll()
	if h.m.running(v.ID) {
		t.Fatal("tunnel of a disabled user was autostarted")
	}
}

func TestCheckBindNamesOwnTunnels(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	_, webPort := webServer(t)
	a := h.createTunnel(alice, Input{Name: "Web A", Type: "local", ConnectionID: conn.ID, DestHost: "127.0.0.1", DestPort: webPort})
	st := h.startTunnel(alice, a.ID)
	_, portStr, _ := net.SplitHostPort(st.LocalAddr)
	port := 0
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	var res struct {
		Available  bool   `json:"available"`
		Error      string `json:"error"`
		Suggestion int    `json:"suggestion"`
	}
	h.must(alice, "POST", "/api/tunnels/check-bind", map[string]any{"bindHost": "127.0.0.1", "bindPort": port}, &res, http.StatusOK)
	if res.Available || !strings.Contains(res.Error, "your tunnel “Web A”") || res.Suggestion == 0 {
		t.Fatalf("check-bind on an own tunnel's port = %+v", res)
	}
	// Editing that tunnel: its own port is fine.
	res.Available, res.Error = false, ""
	h.must(alice, "POST", "/api/tunnels/check-bind", map[string]any{"bindHost": "127.0.0.1", "bindPort": port, "id": a.ID}, &res, http.StatusOK)
	if !res.Available {
		t.Fatalf("check-bind for the edited tunnel = %+v", res)
	}
	// A wildcard bind overlaps too.
	res.Available, res.Error = false, ""
	h.must(alice, "POST", "/api/tunnels/check-bind", map[string]any{"bindHost": "*", "bindPort": port}, &res, http.StatusOK)
	if res.Available || !strings.Contains(res.Error, "Web A") {
		t.Fatalf("wildcard check-bind = %+v", res)
	}
}

func TestConcurrentStartsAndDemotedOwner(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Mode = config.ModeServer; c.Listen = "0.0.0.0:7822" })
	srv := newTestSSHServer(t)
	admin := h.user("admin", true)
	conn := h.sshConn(admin, srv, nil)
	_, webPort := webServer(t)
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	port := free.Addr().(*net.TCPAddr).Port
	free.Close()
	local := h.createTunnel(admin, Input{Name: "l", Type: "local", ConnectionID: conn.ID, BindPort: port, DestHost: "127.0.0.1", DestPort: webPort})

	// Many simultaneous starts of one tunnel: all succeed, one listener.
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := h.call(admin, "POST", "/api/tunnels/"+local.ID+"/start", nil, nil)
			codes <- code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("concurrent start answered %d", code)
		}
	}
	h.waitStatus(local.ID, func(s Status) bool { return s.State == model.TunnelRunning && s.Connected })

	// A remote forward of an administrator who is then demoted stops at the next reconcile.
	remote := h.createTunnel(admin, Input{Name: "r", Type: "remote", ConnectionID: conn.ID, DestHost: "127.0.0.1", DestPort: webPort})
	h.startTunnel(admin, remote.ID)
	demoted := *admin
	demoted.Role = model.RoleUser
	if err := h.d.Store.Users.Update(h.ctx, &demoted); err != nil {
		t.Fatal(err)
	}
	h.m.reconcile()
	st := h.m.status(remote.ID)
	if st.State != model.TunnelError || !strings.Contains(st.Error, "no longer allowed") {
		t.Fatalf("remote forward after demotion: %+v", st)
	}
	if h.m.status(local.ID).State != model.TunnelRunning {
		t.Fatal("the loopback local forward (still allowed) was stopped")
	}
}

func TestViewHidesUnsharedConnection(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice, bob := h.user("alice", false), h.user("bob", false)
	conn := h.sshConn(bob, srv, nil)
	conn.Shared = true
	if err := h.d.Store.Connections.Update(h.ctx, conn); err != nil {
		t.Fatal(err)
	}
	v := h.createTunnel(alice, Input{Name: "shared", Type: "local", ConnectionID: conn.ID, DestPort: 80})
	if v.Connection == nil || v.Connection.Host != srv.Host {
		t.Fatalf("shared connection not described: %+v", v.Connection)
	}
	conn.Shared = false
	if err := h.d.Store.Connections.Update(h.ctx, conn); err != nil {
		t.Fatal(err)
	}
	var one View
	h.must(alice, "GET", "/api/tunnels/"+v.ID, nil, &one, http.StatusOK)
	var all []View
	h.must(alice, "GET", "/api/tunnels", nil, &all, http.StatusOK)
	if one.Connection != nil || len(all) != 1 || all[0].Connection != nil {
		t.Fatalf("unshared connection still described: %+v / %+v", one.Connection, all)
	}
}
