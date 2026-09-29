package recording_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/recording"
)

type listResp struct {
	Items []recording.RecordingItem `json:"items"`
	Total int                       `json:"total"`
}

func TestRecordingsLifecycle(t *testing.T) {
	e := newEnv(t)
	c := e.admin
	s := e.openSession(t, c, map[string]any{"record": true, "log": true, "logTimestamps": true, "marks": true})
	e.input(t, c, s.ID, "echo hello world\r")
	e.input(t, c, s.ID, "false\r")

	var list listResp
	waitFor(t, "two recordings", func() bool {
		c.MustJSON("GET", "/api/recordings", nil, &list)
		return list.Total == 2
	})
	var cast, log recording.RecordingItem
	for _, it := range list.Items {
		if it.Kind == model.RecordingAsciicast {
			cast = it
		} else {
			log = it
		}
	}
	if !cast.Live || !log.Live || cast.SessionID != s.ID {
		t.Fatalf("expected live recordings: %+v %+v", cast, log)
	}
	// A live recording cannot be deleted.
	if st, code := c.ErrorCode("DELETE", "/api/recordings/"+cast.ID, nil); st != http.StatusConflict || code != "recording_active" {
		t.Fatalf("delete live: %d %s", st, code)
	}
	// Close the session: recordings finish.
	c.MustJSON("DELETE", "/api/sessions/"+s.ID, nil, nil)
	waitFor(t, "finished recordings", func() bool {
		c.MustJSON("GET", "/api/recordings?kind=asciicast", nil, &list)
		return len(list.Items) == 1 && !list.Items[0].Live && list.Items[0].EndedAt != nil
	})

	// v3 as recorded.
	resp, body := c.Do("GET", "/api/recordings/"+cast.ID+"/file", nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(string(body), `{"version":3`) || !strings.Contains(string(body), "ran: echo hello world") {
		t.Fatalf("v3 file %d: %.300s", resp.StatusCode, body)
	}
	// v2 conversion: absolute times, width/height, no exit events.
	resp, body = c.Do("GET", "/api/recordings/"+cast.ID+"/file?format=v2&download=1", nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(string(body), `{"version":2,"width":80,"height":24`) {
		t.Fatalf("v2 file %d: %.300s", resp.StatusCode, body)
	}
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("disposition %q", resp.Header.Get("Content-Disposition"))
	}
	var last float64
	for i, line := range strings.Split(strings.TrimSpace(string(body)), "\n")[1:] {
		var ev []json.RawMessage
		if err := json.Unmarshal([]byte(line), &ev); err != nil || len(ev) != 3 {
			t.Fatalf("v2 line %d: %q", i, line)
		}
		var ts float64
		var code string
		_ = json.Unmarshal(ev[0], &ts)
		_ = json.Unmarshal(ev[1], &code)
		if ts < last || code == "x" {
			t.Fatalf("v2 event %d: %q", i, line)
		}
		last = ts
	}
	// Transcript.
	_, body = c.Do("GET", "/api/recordings/"+cast.ID+"/file?format=txt", nil)
	if !strings.Contains(string(body), "user@fake:~$ echo hello world\nran: echo hello world\n") {
		t.Fatalf("transcript: %q", body)
	}
	// Search inside the cast (times) and the log (line numbers + timestamps).
	var sr struct {
		Matches []recording.SearchMatch `json:"matches"`
		Total   int                     `json:"total"`
	}
	c.MustJSON("GET", "/api/recordings/"+cast.ID+"/search?q=HELLO", nil, &sr)
	if sr.Total != 2 || sr.Matches[0].Time == nil {
		t.Fatalf("cast search: %+v", sr)
	}
	c.MustJSON("GET", "/api/recordings/"+log.ID+"/search?q=ran:%20e.*d&regex=1", nil, &sr)
	if sr.Total != 1 || sr.Matches[0].Line == 0 || sr.Matches[0].TS == "" || sr.Matches[0].Text != "ran: echo hello world" {
		t.Fatalf("log search: %+v", sr)
	}
	var all struct {
		Results []recording.SearchResult `json:"results"`
	}
	c.MustJSON("GET", "/api/recordings/search?q=hello", nil, &all)
	if len(all.Results) != 2 {
		t.Fatalf("search all: %+v", all)
	}
	// Filters.
	c.MustJSON("GET", "/api/recordings?q=FAKE&kind=log", nil, &list)
	if list.Total != 1 || list.Items[0].ID != log.ID {
		t.Fatalf("title filter: %+v", list)
	}
	c.MustJSON("GET", "/api/recordings?from=2000-01-01&to=2000-01-02", nil, &list)
	if list.Total != 0 {
		t.Fatalf("date filter: %+v", list)
	}
	// Other users cannot see them.
	bob := e.CreateUser(c, "bob", "bob password 123", "user")
	if st, _ := bob.ErrorCode("GET", "/api/recordings/"+cast.ID, nil); st != 404 {
		t.Fatalf("foreign get: %d", st)
	}
	bob.MustJSON("GET", "/api/recordings", nil, &list)
	if list.Total != 0 {
		t.Fatalf("bob sees %d", list.Total)
	}
	// Usage and bulk delete.
	var usage recording.UsageResponse
	c.MustJSON("GET", "/api/recordings/usage", nil, &usage)
	if usage.Count != 2 || usage.Bytes == 0 {
		t.Fatalf("usage %+v", usage)
	}
	var bulk struct {
		Deleted []string                `json:"deleted"`
		Failed  []recording.BulkFailure `json:"failed"`
	}
	c.MustJSON("POST", "/api/recordings/bulk-delete", map[string]any{"ids": []string{cast.ID, log.ID, "nope"}}, &bulk)
	if len(bulk.Deleted) != 2 || len(bulk.Failed) != 1 {
		t.Fatalf("bulk %+v", bulk)
	}
	c.MustJSON("GET", "/api/recordings", nil, &list)
	if list.Total != 0 {
		t.Fatalf("left %d", list.Total)
	}
}

func TestCommandAudit(t *testing.T) {
	e := newEnv(t)
	c := e.admin
	c.MustJSON("PUT", "/api/admin/recordings/policy", map[string]any{"commandAudit": true}, nil)
	for _, marks := range []bool{true, false} {
		s := e.openSession(t, c, map[string]any{"marks": marks})
		e.input(t, c, s.ID, "ls -la\r")
		e.input(t, c, s.ID, "sudo\r")
		e.input(t, c, s.ID, "hunter2\r") // password: never echoed, never audited
		e.input(t, c, s.ID, "git statt\x7fus\r")
		e.input(t, c, s.ID, "false\r")
		type auditEntry struct {
			Target  string         `json:"target"`
			Details map[string]any `json:"details"`
		}
		var entries []auditEntry
		waitFor(t, "command audit entries", func() bool {
			c.MustJSON("GET", "/api/admin/audit?action=session.command&target="+s.ID, nil, &entries)
			return len(entries) >= 4
		})
		time.Sleep(200 * time.Millisecond)
		c.MustJSON("GET", "/api/admin/audit?action=session.command&target="+s.ID, nil, &entries)
		var got []string
		for i := len(entries) - 1; i >= 0; i-- {
			got = append(got, entries[i].Details["command"].(string))
			if strings.Contains(entries[i].Details["command"].(string), "hunter2") {
				t.Fatalf("password audited: %+v", entries[i])
			}
		}
		want := "ls -la|sudo|git status|false"
		if strings.Join(got, "|") != want {
			t.Fatalf("marks=%v commands %q, want %q", marks, strings.Join(got, "|"), want)
		}
		if marks {
			last := entries[0].Details
			if last["exitCode"] != float64(1) || last["source"] != "shell-integration" {
				t.Fatalf("exit code: %+v", last)
			}
		}
		var cmds struct {
			Commands []recording.CommandRecord `json:"commands"`
		}
		c.MustJSON("GET", "/api/sessions/"+s.ID+"/commands", nil, &cmds)
		if len(cmds.Commands) != 4 {
			t.Fatalf("session commands %+v", cmds)
		}
	}
}

func TestInstantReplay(t *testing.T) {
	e := newEnv(t)
	c := e.admin
	s := e.openSession(t, c, map[string]any{"marks": true})
	e.input(t, c, s.ID, "first\r")
	time.Sleep(120 * time.Millisecond)
	e.input(t, c, s.ID, "second\r")
	var body []byte
	waitFor(t, "replay", func() bool {
		_, body = c.Do("GET", "/api/sessions/"+s.ID+"/replay?minutes=0", nil)
		return strings.Contains(string(body), "ran: second")
	})
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if !strings.HasPrefix(lines[0], `{"version":3,"term":{"cols":80,"rows":24}`) {
		t.Fatalf("header %s", lines[0])
	}
	var out strings.Builder
	var markers []string
	_, err := recording.ReadCast(strings.NewReader(string(body)), func(ev recording.CastEvent) error {
		switch ev.Code {
		case "o":
			out.WriteString(ev.Data)
		case "m":
			markers = append(markers, ev.Data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Welcome to the fake shell") || !strings.Contains(out.String(), "ran: first") {
		t.Fatalf("replayed output %q", out.String())
	}
	if strings.Join(markers, "|") != "$ first|$ second" {
		t.Fatalf("markers %q", markers)
	}
	if st, _ := c.ErrorCode("GET", "/api/sessions/"+s.ID+"/replay?minutes=-1", nil); st != 400 {
		t.Fatalf("bad minutes: %d", st)
	}
}

func TestShareLinks(t *testing.T) {
	e := newEnv(t)
	c := e.admin
	s := e.openSession(t, c, map[string]any{})
	var share recording.ShareView
	c.MustJSON("POST", "/api/sessions/"+s.ID+"/share", map[string]any{"mode": "read", "expiresInSec": 600, "label": "demo"}, &share)
	if len(share.Token) != 43 || share.URL != "/share/"+share.Token || share.Mode != "read" {
		t.Fatalf("share %+v", share)
	}
	anon := e.Client()
	var info recording.ShareInfo
	anon.MustJSON("GET", "/api/share/"+share.Token, nil, &info)
	if info.Title != "fake box" || info.Mode != "read" || info.Cols != 80 || info.State != model.StateConnected {
		t.Fatalf("info %+v", info)
	}
	if _, body := anon.Do("GET", "/api/share/"+share.Token, nil); strings.Contains(string(body), s.ID) {
		t.Fatal("public share info leaks the session id")
	}

	// Anonymous read-only viewer.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, strings.Replace(e.URL("/ws/share/"+share.Token), "http", "ws", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	sawReadonly, sawWelcome := false, false
	for !sawReadonly || !sawWelcome {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ == websocket.MessageText && strings.Contains(string(data), `"type":"readonly","value":true`) {
			sawReadonly = true
		}
		if typ == websocket.MessageBinary && strings.Contains(string(data), "Welcome") {
			sawWelcome = true
		}
	}
	// Input from a read-only viewer is refused.
	_ = ws.Write(ctx, websocket.MessageBinary, []byte("rm -rf /\r"))
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if typ == websocket.MessageText && strings.Contains(string(data), "read-only") {
			break
		}
	}
	var shares []recording.ShareView
	waitFor(t, "viewer listed", func() bool {
		c.MustJSON("GET", "/api/sessions/"+s.ID+"/shares", nil, &shares)
		return len(shares) == 1 && len(shares[0].Viewers) == 1 && shares[0].Token == share.Token && shares[0].Uses == 1
	})
	// Revoking disconnects the viewer and kills the link.
	c.MustJSON("DELETE", "/api/shares/"+share.ID, nil, nil)
	for {
		if _, _, err := ws.Read(ctx); err != nil {
			break
		}
	}
	if st, code := anon.ErrorCode("GET", "/api/share/"+share.Token, nil); st != 404 || code != "share_not_found" {
		t.Fatalf("revoked: %d %s", st, code)
	}

	// Interactive link: the viewer types into the session.
	c.MustJSON("POST", "/api/sessions/"+s.ID+"/share", map[string]any{"mode": "write", "expiresInSec": 600}, &share)
	ws2, _, err := websocket.Dial(ctx, strings.Replace(e.URL("/ws/share/"+share.Token), "http", "ws", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws2.CloseNow()
	_ = ws2.Write(ctx, websocket.MessageBinary, []byte("from guest\r"))
	waitFor(t, "guest input", func() bool {
		_, body := c.Do("GET", "/api/sessions/"+s.ID+"/scrollback?raw=0", nil)
		return strings.Contains(string(body), "ran: from guest")
	})

	// Login-required links refuse anonymous viewers.
	c.MustJSON("POST", "/api/sessions/"+s.ID+"/share", map[string]any{"mode": "read", "expiresInSec": 600, "requireLogin": true}, &share)
	if st, code := anon.ErrorCode("GET", "/api/share/"+share.Token, nil); st != 401 || code != "login_required" {
		t.Fatalf("login required: %d %s", st, code)
	}
	c.MustJSON("GET", "/api/share/"+share.Token, nil, &info)

	// Policy: write shares off.
	c.MustJSON("PUT", "/api/admin/recordings/policy", map[string]any{"shareWriteEnabled": false}, nil)
	if st, _ := c.ErrorCode("POST", "/api/sessions/"+s.ID+"/share", map[string]any{"mode": "write", "expiresInSec": 600}); st != 403 {
		t.Fatalf("write share with policy off: %d", st)
	}
	// Other users cannot share someone else's session; bad expiry rejected.
	bob := e.CreateUser(c, "bob", "bob password 123", "user")
	if st, _ := bob.ErrorCode("POST", "/api/sessions/"+s.ID+"/share", map[string]any{"mode": "read", "expiresInSec": 600}); st != 404 {
		t.Fatalf("bob share: %d", st)
	}
	if st, _ := c.ErrorCode("POST", "/api/sessions/"+s.ID+"/share", map[string]any{"mode": "read", "expiresInSec": 5}); st != 400 {
		t.Fatalf("short expiry: %d", st)
	}
	// Closing the session removes its links.
	c.MustJSON("DELETE", "/api/sessions/"+s.ID, nil, nil)
	if st, _ := c.ErrorCode("GET", "/api/share/"+share.Token, nil); st != 404 {
		t.Fatalf("closed session share: %d", st)
	}
}

func TestShareRateLimit(t *testing.T) {
	e := newEnv(t)
	anon := e.Client()
	bad := strings.Repeat("A", 43)
	limited := false
	for i := 0; i < 30; i++ {
		st, _ := anon.ErrorCode("GET", "/api/share/"+bad, nil)
		if st == http.StatusTooManyRequests {
			limited = true
			break
		}
		if st != 404 {
			t.Fatalf("status %d", st)
		}
	}
	if !limited {
		t.Fatal("invalid tokens were never rate-limited")
	}
}

func TestAdminSessions(t *testing.T) {
	e := newEnv(t)
	c := e.admin
	bob := e.CreateUser(c, "bob", "bob password 123", "user")
	s := e.openSession(t, bob, map[string]any{})
	e.input(t, bob, s.ID, "hi\r")
	var rows []recording.AdminSession
	waitFor(t, "admin list", func() bool {
		c.MustJSON("GET", "/api/admin/sessions", nil, &rows)
		return len(rows) == 1 && rows[0].BytesIn > 0
	})
	if rows[0].Owner.Username != "bob" || rows[0].BytesOut == 0 {
		t.Fatalf("row %+v", rows[0])
	}
	if st, _ := bob.ErrorCode("GET", "/api/admin/sessions", nil); st != 403 {
		t.Fatalf("non-admin: %d", st)
	}
	c.MustJSON("POST", "/api/admin/sessions/"+s.ID+"/message", map[string]string{"text": "maintenance in 5 min"}, nil)
	waitFor(t, "message notice", func() bool {
		_, body := bob.Do("GET", "/api/sessions/"+s.ID+"/scrollback?raw=0", nil)
		return strings.Contains(string(body), "maintenance in 5 min")
	})
	c.MustJSON("POST", "/api/admin/sessions/"+s.ID+"/terminate", map[string]string{"reason": "policy"}, nil)
	if st, _ := bob.ErrorCode("GET", "/api/sessions/"+s.ID, nil); st != 404 {
		t.Fatalf("terminated session still there: %d", st)
	}
}

func TestRetention(t *testing.T) {
	e := newEnv(t)
	c := e.admin
	s := e.openSession(t, c, map[string]any{"record": true})
	e.input(t, c, s.ID, "x\r")
	c.MustJSON("DELETE", "/api/sessions/"+s.ID, nil, nil)
	var list listResp
	waitFor(t, "finished", func() bool {
		c.MustJSON("GET", "/api/recordings", nil, &list)
		return list.Total == 1 && list.Items[0].EndedAt != nil
	})
	// Age the recording by 10 days.
	old := time.Now().AddDate(0, 0, -10).UnixMilli()
	if _, err := e.Server.Deps.Store.DB.Exec(`UPDATE recordings SET started_at = ?, ended_at = ?`, old, old+1000); err != nil {
		t.Fatal(err)
	}
	c.MustJSON("PUT", "/api/admin/recordings/policy", map[string]any{"maxAgeDays": 30}, nil)
	var res recording.RetentionResult
	c.MustJSON("POST", "/api/admin/recordings/cleanup", map[string]any{"dryRun": true}, &res)
	if res.Deleted != 0 {
		t.Fatalf("30 days: %+v", res)
	}
	c.MustJSON("PUT", "/api/admin/recordings/policy", map[string]any{"maxAgeDays": 7}, nil)
	c.MustJSON("POST", "/api/admin/recordings/cleanup", map[string]any{"dryRun": true}, &res)
	if res.Deleted != 1 || !res.DryRun {
		t.Fatalf("dry run: %+v", res)
	}
	c.MustJSON("POST", "/api/admin/recordings/cleanup", nil, &res)
	c.MustJSON("GET", "/api/recordings", nil, &list)
	if res.Deleted != 1 || list.Total != 0 {
		t.Fatalf("cleanup: %+v, left %d", res, list.Total)
	}
	if st, _ := c.ErrorCode("PUT", "/api/admin/recordings/policy", map[string]any{"maxAgeDays": -1}); st != 400 {
		t.Fatalf("invalid policy: %d", st)
	}
}

func TestDebugLogs(t *testing.T) {
	e := newEnv(t)
	c := e.admin
	e.Server.Deps.Log.Warn("probe message", "password", "hunter2", "path", "/share/"+strings.Repeat("x", 43))
	var resp recording.LogsResponse
	c.MustJSON("GET", "/api/admin/logs?q=probe%20message&level=warn", nil, &resp)
	if len(resp.Entries) == 0 {
		t.Fatalf("no entries: %+v", resp)
	}
	got := resp.Entries[len(resp.Entries)-1]
	for _, a := range got.Attrs {
		if strings.Contains(a.V, "hunter2") || strings.Contains(a.V, strings.Repeat("x", 43)) {
			t.Fatalf("not redacted: %+v", got)
		}
	}
	_, body := c.Do("GET", "/api/admin/logs/export", nil)
	if !strings.Contains(string(body), "probe message") || strings.Contains(string(body), "hunter2") {
		t.Fatalf("export %q", body)
	}
	bob := e.CreateUser(c, "bob", "bob password 123", "user")
	if st, _ := bob.ErrorCode("GET", "/api/admin/logs", nil); st != 403 {
		t.Fatalf("non-admin logs: %d", st)
	}
}
