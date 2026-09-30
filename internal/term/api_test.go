package term_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/server/servertest"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
)

type eventLog struct {
	mu     sync.Mutex
	events []map[string]any
}

func (e *eventLog) has(pred func(map[string]any) bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.ContainsFunc(e.events, pred)
}

func listenEvents(t *testing.T, env *servertest.Env, c *servertest.Client) *eventLog {
	t.Helper()
	url := strings.Replace(env.HTTP.URL, "http", "ws", 1) + "/ws/events"
	ws, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{HTTPClient: c.HTTP})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	log := &eventLog{}
	go func() {
		for {
			_, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var ev map[string]any
			if json.Unmarshal(data, &ev) == nil {
				log.mu.Lock()
				log.events = append(log.events, ev)
				log.mu.Unlock()
			}
		}
	}()
	return log
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSessionAPIWithLocalShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	env := servertest.New(t)
	admin := env.Setup("admin", "correct horse battery staple")
	events := listenEvents(t, env, admin)

	var rs model.RuntimeSession
	admin.MustJSON("POST", "/api/sessions", map[string]any{
		"quick": map[string]any{"protocol": "local", "options": map[string]any{"shell": "/bin/sh", "loginShell": false,
			"record": true, "log": true, "logTimestamps": true}},
		"cols": 90, "rows": 25, "title": "my shell",
	}, &rs)
	if rs.ID == "" || rs.Kind != model.KindTerminal || rs.Protocol != model.ProtoLocal || rs.Title != "my shell" ||
		rs.Cols != 90 || !rs.Recording || !rs.Logging || rs.RecordingID == "" {
		t.Fatalf("created %+v", rs)
	}
	waitUntil(t, "session.updated connected", func() bool {
		return events.has(func(ev map[string]any) bool {
			s, _ := ev["session"].(map[string]any)
			return ev["type"] == "session.updated" && s["id"] == rs.ID && s["state"] == "connected"
		})
	})

	// Input through the API, output in the scrollback (raw and stripped).
	admin.MustJSON("POST", "/api/sessions/"+rs.ID+"/input", map[string]string{"data": "printf '\\033[31mred-%s\\033[0m\\n' marker\r"}, nil)
	waitUntil(t, "output", func() bool {
		resp, body := admin.Do("GET", "/api/sessions/"+rs.ID+"/scrollback?raw=0", nil)
		return resp.StatusCode == 200 && strings.Contains(string(body), "red-marker")
	})
	resp, raw := admin.Do("GET", "/api/sessions/"+rs.ID+"/scrollback", nil)
	if resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" || !strings.Contains(string(raw), "\x1b[31mred-marker") {
		t.Fatalf("raw scrollback %q", raw)
	}
	_, stripped := admin.Do("GET", "/api/sessions/"+rs.ID+"/scrollback?raw=0", nil)
	if strings.Contains(string(stripped), "\x1b") {
		t.Fatalf("stripped scrollback still has escapes: %q", stripped)
	}

	// Rename, resize, list.
	var cur model.RuntimeSession
	admin.MustJSON("PATCH", "/api/sessions/"+rs.ID, map[string]string{"title": "renamed"}, &cur)
	if cur.Title != "renamed" {
		t.Fatalf("rename %+v", cur)
	}
	admin.MustJSON("POST", "/api/sessions/"+rs.ID+"/resize", map[string]int{"cols": 120, "rows": 40}, &cur)
	if cur.Cols != 120 || cur.Rows != 40 {
		t.Fatalf("resize %+v", cur)
	}
	var list []model.RuntimeSession
	admin.MustJSON("GET", "/api/sessions", nil, &list)
	if len(list) != 1 || list[0].ID != rs.ID {
		t.Fatalf("list %+v", list)
	}

	// Stop recording and logging: files and recording rows are finalized.
	admin.MustJSON("POST", "/api/sessions/"+rs.ID+"/record", map[string]bool{"enabled": false}, &cur)
	admin.MustJSON("POST", "/api/sessions/"+rs.ID+"/log", map[string]bool{"enabled": false}, &cur)
	if cur.Recording || cur.Logging {
		t.Fatalf("toggles %+v", cur)
	}
	recs, err := env.Server.Deps.Store.Recordings.ListBySession(context.Background(), rs.ID)
	if err != nil || len(recs) != 2 {
		t.Fatalf("recordings %v %v", recs, err)
	}
	for _, r := range recs {
		if r.EndedAt == nil || r.Size <= 0 {
			t.Fatalf("recording not finalized %+v", r)
		}
		data, err := os.ReadFile(r.Path)
		if err != nil || int64(len(data)) != r.Size {
			t.Fatalf("recording file %s: %v (%d vs %d)", r.Path, err, len(data), r.Size)
		}
		switch r.Kind {
		case model.RecordingAsciicast:
			if !strings.HasPrefix(string(data), `{"version":3,"term":{"cols":90,"rows":25,"type":"xterm-256color"}`) ||
				!strings.Contains(string(data), "red-marker") || !strings.Contains(string(data), `"r", "120x40"`) {
				t.Fatalf("cast %q", data)
			}
			if !strings.HasPrefix(r.Path, env.Cfg.RecordingsDir()) {
				t.Fatalf("cast path %s", r.Path)
			}
		case model.RecordingLog:
			if !strings.Contains(string(data), "red-marker") || strings.Contains(string(data), "\x1b[31m") ||
				!strings.Contains(string(data), "] ") || !strings.HasPrefix(r.Path, env.Cfg.LogsDir()) {
				t.Fatalf("log %q at %s", data, r.Path)
			}
		}
		if st, _ := os.Stat(r.Path); st.Mode().Perm() != 0o600 {
			t.Fatalf("recording mode %v", st.Mode())
		}
	}

	// Another user sees nothing and cannot touch the session; the admin can list all.
	bob := env.CreateUser(admin, "bob", "another good password", "user")
	if st, _ := bob.ErrorCode("GET", "/api/sessions/"+rs.ID, nil); st != http.StatusNotFound {
		t.Fatalf("bob get: %d", st)
	}
	if st, _ := bob.ErrorCode("DELETE", "/api/sessions/"+rs.ID, nil); st != http.StatusNotFound {
		t.Fatalf("bob delete: %d", st)
	}
	var bobList []model.RuntimeSession
	bob.MustJSON("GET", "/api/sessions?all=1", nil, &bobList)
	if len(bobList) != 0 {
		t.Fatalf("bob list %+v", bobList)
	}
	// Invalid requests.
	if st, code := admin.ErrorCode("POST", "/api/sessions", map[string]any{"cols": 80}); st != 400 || code != "bad_request" {
		t.Fatalf("empty create: %d %s", st, code)
	}
	if st, _ := admin.ErrorCode("POST", "/api/sessions", map[string]any{"quick": map[string]any{"protocol": "sftp", "host": "x"}}); st != 400 {
		t.Fatalf("file protocol as terminal: %d", st)
	}
	if st, _ := admin.ErrorCode("POST", "/api/sessions", map[string]any{"quick": map[string]any{"protocol": "ssh"}}); st != 400 {
		t.Fatalf("ssh without host: %d", st)
	}
	if st, _ := admin.ErrorCode("POST", "/api/sessions", map[string]any{"quick": map[string]any{"protocol": "local",
		"options": map[string]any{"encoding": "klingon"}}}); st != 400 {
		t.Fatalf("bad encoding: %d", st)
	}
	if st, _ := admin.ErrorCode("POST", "/api/sessions/"+rs.ID+"/signal", map[string]string{"name": "BOGUS"}); st != 400 {
		t.Fatalf("bad signal: %d", st)
	}

	// Local shells listing.
	var shells []map[string]any
	admin.MustJSON("GET", "/api/local/shells", nil, &shells)
	if len(shells) == 0 || shells[0]["id"] == "" || shells[0]["args"] == nil {
		t.Fatalf("shells %v", shells)
	}

	// Close: session.closed is published and the session is gone.
	admin.MustJSON("DELETE", "/api/sessions/"+rs.ID, nil, nil)
	waitUntil(t, "session.closed", func() bool {
		return events.has(func(ev map[string]any) bool { return ev["type"] == "session.closed" && ev["id"] == rs.ID })
	})
	if st, _ := admin.ErrorCode("GET", "/api/sessions/"+rs.ID, nil); st != http.StatusNotFound {
		t.Fatalf("closed session still served: %d", st)
	}
	entries, _ := env.Server.Deps.Store.Audit.List(context.Background(), store.AuditFilter{Action: "session.", Limit: 100})
	var actions []string
	for _, e := range entries {
		actions = append(actions, e.Action)
	}
	joined := strings.Join(actions, ",")
	for _, want := range []string{"session.open", "session.close", "session.record.stop", "session.log.stop"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("audit %v lacks %s", actions, want)
		}
	}
}

func TestAdminShadowAndServerModePolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	env := servertest.New(t, func(c *config.Config) { c.Mode = config.ModeServer })
	admin := env.Setup("admin", "correct horse battery staple")
	bob := env.CreateUser(admin, "bob", "another good password", "user")
	// Local shells are admin-only in server mode.
	if st, code := bob.ErrorCode("POST", "/api/sessions", map[string]any{"quick": map[string]any{"protocol": "local"}}); st != 403 || code != "forbidden" {
		t.Fatalf("bob local shell: %d %s", st, code)
	}
	if st, _ := bob.ErrorCode("GET", "/api/local/shells", nil); st != 403 {
		t.Fatalf("bob shells: %d", st)
	}
	var rs model.RuntimeSession
	admin.MustJSON("POST", "/api/sessions", map[string]any{"quick": map[string]any{"protocol": "local",
		"options": map[string]any{"shell": "/bin/sh", "loginShell": false}}}, &rs)

	// Admin shadowing another admin-owned session is the owner path; create a second admin to shadow read-only.
	admin.MustJSON("POST", "/api/admin/users", map[string]string{"username": "root2", "password": "yet another password", "role": "admin"}, nil)
	root2 := env.Login("root2", "yet another password")
	var all []model.RuntimeSession
	root2.MustJSON("GET", "/api/sessions?all=1", nil, &all)
	if len(all) != 1 || all[0].ID != rs.ID {
		t.Fatalf("admin list all %+v", all)
	}
	var mine []model.RuntimeSession
	root2.MustJSON("GET", "/api/sessions", nil, &mine)
	if len(mine) != 0 {
		t.Fatalf("own list %+v", mine)
	}
	url := strings.Replace(env.HTTP.URL, "http", "ws", 1) + "/ws/terminal/" + rs.ID
	ws, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{HTTPClient: root2.HTTP})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	gotRO := false
	deadline := time.Now().Add(5 * time.Second)
	for !gotRO && time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		typ, data, err := ws.Read(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if typ == websocket.MessageText && strings.Contains(string(data), `"type":"readonly","value":true`) {
			gotRO = true
		}
	}
	if !gotRO {
		t.Fatal("shadow attach is not read-only")
	}
	// Other admins may force-close but not inject input.
	if st, _ := root2.ErrorCode("POST", "/api/sessions/"+rs.ID+"/input", map[string]string{"data": "x"}); st != 403 {
		t.Fatalf("shadow input: %d", st)
	}
	root2.MustJSON("DELETE", "/api/sessions/"+rs.ID, nil, nil)
	// bob cannot attach at all.
	if resp, _ := bob.Do("GET", "/ws/terminal/"+rs.ID, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bob attach: %d", resp.StatusCode)
	}
}

func TestSavedConnectionAndGraphicalSessions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	env := servertest.New(t)
	admin := env.Setup("admin", "correct horse battery staple")
	var conn model.Connection
	admin.MustJSON("POST", "/api/connections", map[string]any{"name": "Local sh", "protocol": "local",
		"options": map[string]any{"shell": "/bin/sh", "loginShell": false, "startupCommand": "echo started-$((1+1))"}}, &conn)
	var rs model.RuntimeSession
	admin.MustJSON("POST", "/api/sessions", map[string]any{"connectionId": conn.ID, "cols": 80, "rows": 24}, &rs)
	if rs.ConnectionID != conn.ID || rs.Title != "Local sh" {
		t.Fatalf("session %+v", rs)
	}
	waitUntil(t, "startup command output", func() bool {
		_, body := admin.Do("GET", "/api/sessions/"+rs.ID+"/scrollback?raw=0", nil)
		return strings.Contains(string(body), "started-2")
	})
	var saved model.Connection
	admin.MustJSON("GET", "/api/connections/"+conn.ID, nil, &saved)
	if saved.LastUsedAt == nil {
		t.Fatal("lastUsedAt not touched")
	}
	if st, _ := admin.ErrorCode("POST", "/api/sessions", map[string]any{"connectionId": "doesnotexist00000000"}); st != 404 {
		t.Fatalf("unknown connection: %d", st)
	}

	// Graphical sessions have no backend; their module drives state and viewers.
	var g model.RuntimeSession
	admin.MustJSON("POST", "/api/sessions", map[string]any{"quick": map[string]any{"protocol": "vnc", "host": "10.0.0.5",
		"password": "vncpw"}}, &g)
	if g.Kind != model.KindVNC || g.State != model.StateConnecting {
		t.Fatalf("graphical session %+v", g)
	}
	m := env.Server.Core.Sessions
	s := m.Get(g.ID)
	_, secrets, err := s.Resolve(context.Background())
	if err != nil || secrets["password"] != "vncpw" {
		t.Fatalf("resolve: %v %v", secrets, err)
	}
	m.SetState(g.ID, model.StateConnected, "")
	release, err := m.TrackClient(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	info := s.Info()
	if info.State != model.StateConnected || info.ConnectedAt == nil || info.Clients != 1 {
		t.Fatalf("info %+v", info)
	}
	release()
	release()
	if s.Info().Clients != 0 {
		t.Fatal("viewer not released")
	}
	if st, _ := admin.ErrorCode("GET", "/api/sessions/"+g.ID+"/scrollback", nil); st != 400 {
		t.Fatalf("graphical scrollback: %d", st)
	}
	m.SetState(g.ID, model.StateClosed, "")
	if m.Get(g.ID) != nil {
		t.Fatal("closed graphical session still listed")
	}
}
