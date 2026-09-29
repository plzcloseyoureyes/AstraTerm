package automation

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/termstead/termstead/internal/config"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/term"
)

func TestSnippetAndMacroREST(t *testing.T) {
	h := newHarness(t)
	alice := h.user("alice", model.RoleUser)
	bob := h.user("bob", model.RoleUser)

	var s model.Snippet
	h.must(alice, "POST", "/api/snippets", map[string]any{"name": " Disk usage ", "folder": "ops / linux/", "content": "df -h {{path|/}}",
		"tags": []string{"disk", "Disk", " linux"}, "sendMode": "execute", "shortcut": "Control+Alt+d"}, &s)
	if s.ID == "" || s.Name != "Disk usage" || s.Folder != "ops/linux" || len(s.Tags) != 2 || s.SendMode != "execute" {
		t.Fatalf("created %+v", s)
	}
	if st, _ := h.code(alice, "POST", "/api/snippets", map[string]any{"content": "x"}); st != 400 {
		t.Fatalf("missing name: %d", st)
	}
	if st, _ := h.code(alice, "POST", "/api/snippets", map[string]any{"name": "x", "sendMode": "run"}); st != 400 {
		t.Fatalf("bad send mode: %d", st)
	}
	var list []model.Snippet
	h.must(alice, "GET", "/api/snippets", nil, &list)
	if len(list) != 1 {
		t.Fatalf("list %+v", list)
	}
	h.must(bob, "GET", "/api/snippets", nil, &list)
	if len(list) != 0 {
		t.Fatalf("bob sees alice's snippets: %+v", list)
	}
	if st, _ := h.code(bob, "PATCH", "/api/snippets/"+s.ID, map[string]any{"name": "pwned"}); st != 404 {
		t.Fatalf("bob patched alice's snippet: %d", st)
	}
	var patched model.Snippet
	h.must(alice, "PATCH", "/api/snippets/"+s.ID, map[string]any{"description": "shows disks", "sendMode": "paste"}, &patched)
	if patched.Description != "shows disks" || patched.SendMode != "paste" || patched.Content != "df -h {{path|/}}" {
		t.Fatalf("patched %+v", patched)
	}
	if st, _ := h.code(anon(), "GET", "/api/snippets", nil); st != 401 {
		t.Fatalf("anonymous list: %d", st)
	}

	var mc model.Macro
	h.must(alice, "POST", "/api/macros", map[string]any{"name": "greet", "steps": []map[string]any{{"data": "echo hi", "delayMs": 0},
		{"data": "\r", "delayMs": 120}}}, &mc)
	if mc.ID == "" || len(mc.Steps) != 2 || mc.Steps[1].DelayMs != 120 {
		t.Fatalf("macro %+v", mc)
	}
	if st, _ := h.code(alice, "POST", "/api/macros", map[string]any{"name": "bad", "steps": []map[string]any{{"data": "x", "delayMs": -1}}}); st != 400 {
		t.Fatalf("negative delay: %d", st)
	}
	if st, _ := h.code(bob, "DELETE", "/api/macros/"+mc.ID, nil); st != 404 {
		t.Fatalf("bob deleted alice's macro: %d", st)
	}
	h.must(alice, "DELETE", "/api/macros/"+mc.ID, nil, nil)
	h.must(alice, "DELETE", "/api/snippets/"+s.ID, nil, nil)
	if st, _ := h.code(alice, "GET", "/api/snippets/"+s.ID, nil); st != 404 {
		t.Fatalf("deleted snippet: %d", st)
	}
}

func anon() *model.User { return nil }

func TestSnippetRunVariablesAndGuard(t *testing.T) {
	h := newHarness(t)
	alice := h.user("alice", model.RoleUser)
	bob := h.user("bob", model.RoleUser)
	rs := h.quickShell(alice, nil)

	var s model.Snippet
	h.must(alice, "POST", "/api/snippets", map[string]any{"name": "greet", "content": "echo snip-{{word}}-{{n|7}}-$((1+2))",
		"sendMode": "execute"}, &s)
	if st, _ := h.code(alice, "POST", "/api/snippets/"+s.ID+"/run", map[string]any{"sessionIds": []string{rs.ID}}); st != 400 {
		t.Fatalf("missing variable: %d", st)
	}
	var res sendResults
	h.must(alice, "POST", "/api/snippets/"+s.ID+"/run", map[string]any{"sessionIds": []string{rs.ID, "nosuchsessionxxxxxx"},
		"variables": map[string]string{"word": "ok"}}, &res)
	if len(res.Results) != 2 || !res.Results[0].OK || res.Results[1].OK {
		t.Fatalf("results %+v", res)
	}
	h.waitOutput(alice, rs.ID, "snip-ok-7-3")
	// Other users cannot target the session.
	var bobSnip model.Snippet
	h.must(bob, "POST", "/api/snippets", map[string]any{"name": "x", "content": "echo bob"}, &bobSnip)
	h.must(bob, "POST", "/api/snippets/"+bobSnip.ID+"/run", map[string]any{"sessionIds": []string{rs.ID}}, &res)
	if res.Results[0].OK {
		t.Fatalf("bob wrote into alice's session")
	}

	// Dangerous content is refused unless confirmed.
	var bad model.Snippet
	h.must(alice, "POST", "/api/snippets", map[string]any{"name": "danger", "content": "echo guard-test rm -rf /", "sendMode": "execute"}, &bad)
	st, body := h.do(alice, "POST", "/api/snippets/"+bad.ID+"/run", map[string]any{"sessionIds": []string{rs.ID}}, nil)
	var refused dangerousResponse
	_ = json.Unmarshal(body, &refused)
	if st != http.StatusConflict || refused.Code != "dangerous_command" || len(refused.Matches) == 0 || refused.Matches[0].Rule != "rm-root" {
		t.Fatalf("dangerous: %d %s", st, body)
	}
	h.must(alice, "POST", "/api/snippets/"+bad.ID+"/run", map[string]any{"sessionIds": []string{rs.ID}, "confirmDangerous": true}, &res)
	h.waitOutput(alice, rs.ID, "guard-test rm -rf /")
	// The guard can be disabled in the user's settings.
	if err := h.d.Store.Settings.SetJSON(t.Context(), alice.ID, "automation", map[string]any{"guardEnabled": false}); err != nil {
		t.Fatal(err)
	}
	h.must(alice, "POST", "/api/snippets/"+bad.ID+"/run", map[string]any{"sessionIds": []string{rs.ID}}, &res)
	var check guardCheckResponse
	h.must(alice, "POST", "/api/automation/guard/check", map[string]any{"text": "rm -rf /"}, &check)
	if check.Enabled || len(check.Matches) != 0 {
		t.Fatalf("guard check with guard disabled: %+v", check)
	}
	h.must(bob, "POST", "/api/automation/guard/check", map[string]any{"text": "sudo reboot"}, &check)
	if !check.Enabled || len(check.Matches) != 1 {
		t.Fatalf("guard check: %+v", check)
	}
}

func TestInjectSecret(t *testing.T) {
	h := newHarness(t)
	admin := h.user("admin", model.RoleAdmin)
	alice := h.user("alice", model.RoleUser)

	rs := h.quickShell(alice, map[string]any{"secrets": map[string]string{"sudoPassword": "sudo-S3cret"}, "password": "pw-S3cret"})
	var keys secretKeysResponse
	h.must(alice, "GET", "/api/sessions/"+rs.ID+"/secret-keys", nil, &keys)
	if !keys.Injectable || strings.Join(keys.Keys, ",") != "password,sudoPassword" {
		t.Fatalf("keys %+v", keys)
	}
	h.input(alice, rs.ID, "read -r X; echo \"got=[$X]\"\r")
	time.Sleep(200 * time.Millisecond)
	h.must(alice, "POST", "/api/sessions/"+rs.ID+"/inject-secret", map[string]any{"key": "sudoPassword"}, nil)
	h.waitOutput(alice, rs.ID, "got=[sudo-S3cret]")
	if st, code := h.code(alice, "POST", "/api/sessions/"+rs.ID+"/inject-secret", map[string]any{"key": "nope"}); st != 404 || code != "secret_not_found" {
		t.Fatalf("unknown secret: %d %s", st, code)
	}
	if st, _ := h.code(alice, "POST", "/api/sessions/"+rs.ID+"/inject-secret", map[string]any{"key": "hop:x:password"}); st != 400 {
		t.Fatalf("hop secret: %d", st)
	}
	if st, _ := h.code(admin, "POST", "/api/sessions/"+rs.ID+"/inject-secret", map[string]any{"key": "password"}); st != 404 {
		t.Fatalf("admin injected into alice's session: %d", st)
	}
	// Audit entry without the value.
	entries, err := h.d.Store.Audit.List(t.Context(), storeAuditFilter("session.inject_secret"))
	if err != nil || len(entries) == 0 || strings.Contains(string(entries[0].Details), "S3cret") {
		t.Fatalf("audit %+v %v", entries, err)
	}

	// A connection shared by an admin: its secrets must not be typed for other users.
	shared := h.savedShell(admin, "shared shell", nil, map[string]string{"password": "admin-only"})
	shared.Shared = true
	if err := h.d.Store.Connections.Update(t.Context(), shared); err != nil {
		t.Fatal(err)
	}
	var srs model.RuntimeSession
	h.must(alice, "POST", "/api/sessions", map[string]any{"connectionId": shared.ID, "cols": 80, "rows": 24}, &srs)
	h.waitOutput(alice, srs.ID, "nx$")
	h.must(alice, "GET", "/api/sessions/"+srs.ID+"/secret-keys", nil, &keys)
	if keys.Injectable || len(keys.Keys) != 0 {
		t.Fatalf("shared secrets listed: %+v", keys)
	}
	if st, _ := h.code(alice, "POST", "/api/sessions/"+srs.ID+"/inject-secret", map[string]any{"key": "password"}); st != 403 {
		t.Fatalf("shared secret injected: %d", st)
	}
}

func TestLogonActions(t *testing.T) {
	h := newHarness(t)
	alice := h.user("alice", model.RoleUser)
	// Input hooks must only ever see secrets masked (term.Manager.WriteSensitive).
	var inputMu sync.Mutex
	var input strings.Builder
	t.Cleanup(h.core.Sessions.AddHooks(term.Hooks{OnInput: func(_ *term.Session, data []byte) {
		inputMu.Lock()
		input.Write(data)
		inputMu.Unlock()
	}}))
	conn := h.savedShell(alice, "with logon", map[string]any{
		"startupCommand": "echo START-$((3*3))",
		"logonActions": []any{
			map[string]any{"expect": `nx\$ $`, "send": `echo LOGON-$((40+2))`},
			map[string]any{"expect": `LOGON-42`, "secret": "sudoPassword"}, // typed as a (failing) command
			map[string]any{"expect": `never-appears`, "send": "echo nope", "timeoutSec": 1, "optional": true},
			map[string]any{"send": `\x20echo LAST-$((2*5))`},
		}}, map[string]string{"sudoPassword": "typed-secret"})
	var rs model.RuntimeSession
	h.must(alice, "POST", "/api/sessions", map[string]any{"connectionId": conn.ID, "cols": 100, "rows": 30}, &rs)
	sb := h.waitOutput(alice, rs.ID, "START-9")
	if !strings.Contains(sb, "LOGON-42") || !strings.Contains(sb, "typed-secret") || strings.Contains(sb, "nope") {
		t.Fatalf("scrollback %q", sb)
	}
	// The start-up command runs after the last logon step, not before the first one.
	if first, last := strings.Index(sb, "START-9"), strings.LastIndex(sb, "LAST-10"); last < 0 || first < last {
		t.Fatalf("start-up command before the logon actions: %q", sb)
	}
	inputMu.Lock()
	typed := input.String()
	inputMu.Unlock()
	if strings.Contains(typed, "typed-secret") || !strings.Contains(typed, "************\r") {
		t.Fatalf("input hooks saw %q", typed)
	}
	// Reconnect runs them (and the start-up command) again.
	h.must(alice, "POST", "/api/sessions/"+rs.ID+"/reconnect", nil, nil)
	waitFor(t, "second logon", 15*time.Second, func() bool { return strings.Count(h.scrollback(alice, rs.ID), "START-9") >= 2 })

	// A failing step leaves a notice, and the start-up command is not typed into whatever the session shows.
	bad := h.savedShell(alice, "bad logon", map[string]any{"startupCommand": "echo NOT-$((1+1))", "logonActions": []any{
		map[string]any{"expect": `will-not-match`, "send": "x", "timeoutSec": 1}}}, nil)
	h.must(alice, "POST", "/api/sessions", map[string]any{"connectionId": bad.ID, "cols": 100, "rows": 30}, &rs)
	if sb := h.waitOutput(alice, rs.ID, "start-up command was not sent"); strings.Contains(sb, "NOT-2") {
		t.Fatalf("start-up command typed after a failed logon: %q", sb)
	}
}

func TestTriggers(t *testing.T) {
	h := newHarness(t)
	alice := h.user("alice", model.RoleUser)
	events := h.listen(alice)
	rs := h.quickShell(alice, nil)

	var tr Trigger
	h.must(alice, "POST", "/api/automation/triggers", map[string]any{"name": "t1", "pattern": `TRIG(GER)-ME`,
		"actions": []map[string]any{
			{"type": "send", "text": `echo TRIGGERED-$((1+2))`, "enter": true},
			{"type": "log"},
			{"type": "notify", "title": "Hit $0", "level": "warning"},
			{"type": "highlight", "color": "red"},
		}}, &tr)
	if st, _ := h.code(alice, "POST", "/api/automation/triggers", map[string]any{"name": "bad", "pattern": `(?=x)`,
		"actions": []map[string]any{{"type": "log"}}}); st != 400 {
		t.Fatalf("look-ahead accepted: %d", st)
	}
	if st, _ := h.code(alice, "POST", "/api/automation/triggers", map[string]any{"name": "bad", "pattern": `x`,
		"actions": []map[string]any{{"type": "highlight", "color": "url(evil)"}}}); st != 400 {
		t.Fatalf("bad colour accepted: %d", st)
	}
	// The session connected before the trigger existed: the rule is attached on creation.
	h.input(alice, rs.ID, "echo TRIG''GER-ME\r")
	h.waitOutput(alice, rs.ID, "TRIGGERED-3")
	waitFor(t, "trigger event", 5*time.Second, func() bool {
		return events.find(func(ev map[string]any) bool {
			n, _ := ev["notify"].(map[string]any)
			return ev["type"] == "automation.trigger" && n["title"] == "Hit TRIGGER-ME" && ev["logged"] == true
		}) != nil
	})
	var logEntries []TriggerLogEntry
	waitFor(t, "trigger log", 5*time.Second, func() bool {
		h.must(alice, "GET", "/api/automation/trigger-log?triggerId="+tr.ID, nil, &logEntries)
		return len(logEntries) > 0
	})
	if logEntries[0].Line != "TRIGGER-ME" || logEntries[0].SessionID != rs.ID {
		t.Fatalf("log %+v", logEntries[0])
	}
	var list []Trigger
	h.must(alice, "GET", "/api/automation/triggers", nil, &list)
	if len(list) != 1 || list[0].Stats.Hits < 1 {
		t.Fatalf("stats %+v", list)
	}
	// Disabling detaches the rule.
	h.must(alice, "PATCH", "/api/automation/triggers/"+tr.ID, map[string]any{"enabled": false}, nil)
	h.input(alice, rs.ID, "echo TRIG''GER-ME again; echo MARK-END\r")
	h.waitOutput(alice, rs.ID, "MARK-END")
	time.Sleep(300 * time.Millisecond)
	if n := strings.Count(h.scrollback(alice, rs.ID), "TRIGGERED-3"); n != 1 { // output of the first fire only
		t.Fatalf("disabled trigger fired again (%d)", n)
	}

	// Partial-line prompts fire too (sudo-style prompt without newline), scoped by protocol.
	h.must(alice, "POST", "/api/automation/triggers", map[string]any{"name": "prompt", "pattern": `Continue\? \[y/N\] $`,
		"scope":   map[string]any{"protocols": []string{"local"}},
		"actions": []map[string]any{{"type": "send", "text": "y", "enter": true}}}, nil)
	h.input(alice, rs.ID, "printf 'Continue? [y/N] '; read A; echo \"answer=$A\"\r")
	h.waitOutput(alice, rs.ID, "answer=y")
	if st, _ := h.code(alice, "DELETE", "/api/automation/triggers/"+tr.ID, nil); st != 200 {
		t.Fatalf("delete: %d", st)
	}
	var rt regexTestResponse
	h.must(alice, "POST", "/api/automation/regex/test", map[string]any{"pattern": `err(or)?`, "caseSensitive": false,
		"text": "ok\nERROR: x\nerr"}, &rt)
	if !rt.Valid || len(rt.Matches) != 2 || rt.Matches[0].Groups[0] != "OR" {
		t.Fatalf("regex test %+v", rt)
	}
}

func TestPacedSendAndMacro(t *testing.T) {
	h := newHarness(t)
	alice := h.user("alice", model.RoleUser)
	events := h.listen(alice)
	rs := h.quickShell(alice, nil)

	var js jobStarted
	h.must(alice, "POST", "/api/automation/send", map[string]any{"sessionIds": []string{rs.ID},
		"text": "echo PACE-$((1+1))\necho PACE-$((2+2))\n", "lineDelayMs": 50, "waitPrompt": true}, &js)
	h.waitOutput(alice, rs.ID, "PACE-4")
	waitFor(t, "job done", 10*time.Second, func() bool {
		return events.find(func(ev map[string]any) bool { return ev["jobId"] == js.JobID && ev["event"] == "done" }) != nil
	})
	if st, code := h.code(alice, "POST", "/api/automation/send", map[string]any{"sessionIds": []string{rs.ID}, "text": "sudo reboot"}); st != 409 || code != "dangerous_command" {
		t.Fatalf("dangerous send: %d %s", st, code)
	}
	if st, _ := h.code(alice, "POST", "/api/automation/send", map[string]any{"sessionIds": []string{rs.ID}, "text": "x", "waitPrompt": true, "promptPattern": "("}); st != 400 {
		t.Fatalf("bad prompt pattern: %d", st)
	}

	var mc model.Macro
	h.must(alice, "POST", "/api/macros", map[string]any{"name": "m", "steps": []map[string]any{
		{"data": "echo MAC", "delayMs": 0}, {"data": "RO-$((5*5))", "delayMs": 30}, {"data": "\r", "delayMs": 30}}}, &mc)
	h.must(alice, "POST", "/api/macros/"+mc.ID+"/run", map[string]any{"sessionIds": []string{rs.ID}, "speed": 2}, &js)
	h.waitOutput(alice, rs.ID, "MACRO-25")
	var dm model.Macro
	h.must(alice, "POST", "/api/macros", map[string]any{"name": "d", "steps": []map[string]any{
		{"data": "rm -x\x7frf /", "delayMs": 0}, {"data": "\r", "delayMs": 0}}}, &dm)
	if st, code := h.code(alice, "POST", "/api/macros/"+dm.ID+"/run", map[string]any{"sessionIds": []string{rs.ID}}); st != 409 || code != "dangerous_command" {
		t.Fatalf("dangerous macro: %d %s", st, code)
	}
}

func TestScriptRuns(t *testing.T) {
	h := newHarness(t)
	alice := h.user("alice", model.RoleUser)
	rs := h.quickShell(alice, map[string]any{"secrets": map[string]string{"password": "scr-pw"}})

	var sc Script
	h.must(alice, "POST", "/api/scripts", map[string]any{"name": "probe", "content": `
		session.sendLine("echo SCRIPT-$((6*7))");
		var r = session.expect(/SCRIPT-(\d+)/, 5000);
		log("got " + r.groups[0] + " var=" + vars.x);
		var out = session.run("echo RUN-OUT");
		log("run=" + JSON.stringify(out));
		if (session.waitFor(/never/, 100) !== null) throw new Error("waitFor should time out");
		try { session.expect("never", 50); } catch (e) { log("caught " + e.name); }
		session.sendLine("read -r P; echo \"pw=$P\"");
		sleep(100);
		session.sendSecret("password");
		session.expect(/pw=scr-pw/, 5000);
		log("sessions=" + sessions.list().length + " protocol=" + session.protocol);
	`}, &sc)
	var js jobStarted
	h.must(alice, "POST", "/api/scripts/"+sc.ID+"/run", map[string]any{"sessionId": rs.ID, "variables": map[string]string{"x": "1"}}, &js)
	run := h.waitRun(alice, js.RunID)
	if run.Status != StatusOK {
		t.Fatalf("run %+v", run)
	}
	for _, want := range []string{"got 42 var=1", `run="RUN-OUT"`, "caught TimeoutError", "sessions=1 protocol=local"} {
		if !strings.Contains(run.Log, want) {
			t.Fatalf("log %q missing %q", run.Log, want)
		}
	}
	// Ad-hoc run with a syntax error → 400; a failing script → error run.
	if st, _ := h.code(alice, "POST", "/api/scripts/run", map[string]any{"content": "function ("}); st != 400 {
		t.Fatalf("syntax error: %d", st)
	}
	h.must(alice, "POST", "/api/scripts/run", map[string]any{"content": `throw new Error("nope")`, "name": "adhoc"}, &js)
	if run = h.waitRun(alice, js.RunID); run.Status != StatusError || !strings.Contains(run.Error, "nope") {
		t.Fatalf("failing run %+v", run)
	}
	// Opening a saved connection from a script.
	conn := h.savedShell(alice, "Script Target", nil, nil)
	h.must(alice, "POST", "/api/scripts/run", map[string]any{"content": `
		var s = sessions.open("script target");
		log(s.run("echo OPENED-$((3+3))"));
	`}, &js)
	if run = h.waitRun(alice, js.RunID); run.Status != StatusOK || !strings.Contains(run.Log, "OPENED-6") {
		t.Fatalf("open run %+v", run)
	}
	waitFor(t, "script session closed", 5*time.Second, func() bool {
		for _, s := range h.core.Sessions.List(alice, false) {
			if s.Info().ConnectionID == conn.ID {
				return false
			}
		}
		return true
	})
	// Cancel through the jobs API.
	h.must(alice, "POST", "/api/scripts/run", map[string]any{"content": `sleep(60000)`}, &js)
	time.Sleep(100 * time.Millisecond)
	h.must(alice, "POST", "/api/jobs/"+js.JobID+"/cancel", nil, nil)
	if run = h.waitRun(alice, js.RunID); run.Status != StatusCanceled {
		t.Fatalf("cancel %+v", run)
	}
	var runs []Run
	h.must(alice, "GET", "/api/automation/runs?kind=script", nil, &runs)
	if len(runs) < 4 {
		t.Fatalf("history %d", len(runs))
	}
}

func TestScriptsGatedInServerMode(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Mode = config.ModeServer })
	admin := h.user("admin", model.RoleAdmin)
	alice := h.user("alice", model.RoleUser)
	if st, _ := h.code(alice, "POST", "/api/scripts/run", map[string]any{"content": "log(1)"}); st != 403 {
		t.Fatalf("user script in server mode: %d", st)
	}
	var caps capabilitiesResponse
	h.must(alice, "GET", "/api/automation/capabilities", nil, &caps)
	if caps.Scripts || caps.Mode != "server" {
		t.Fatalf("caps %+v", caps)
	}
	var js jobStarted
	h.must(admin, "POST", "/api/scripts/run", map[string]any{"content": "log(1)"}, &js)
	if run := h.waitRun(admin, js.RunID); run.Status != StatusOK {
		t.Fatalf("admin run %+v", run)
	}
	// An admin can allow scripts for everyone.
	if err := h.d.Store.Settings.SetJSON(t.Context(), "global", "automation", map[string]any{"userScripts": true}); err != nil {
		t.Fatal(err)
	}
	h.must(alice, "POST", "/api/scripts/run", map[string]any{"content": "log(1)"}, &js)
	if run := h.waitRun(alice, js.RunID); run.Status != StatusOK {
		t.Fatalf("user run %+v", run)
	}
}

func TestBatchAndSchedules(t *testing.T) {
	h := newHarness(t)
	alice := h.user("alice", model.RoleUser)
	c1 := h.savedShell(alice, "host one", nil, nil)
	c2 := h.savedShell(alice, "host two", nil, nil)

	var js jobStarted
	h.must(alice, "POST", "/api/automation/batch", map[string]any{"connectionIds": []string{c1.ID, c2.ID}, "kind": "command",
		"command": "echo BATCH-$((2+3)) {{title}}", "parallel": 2, "timeoutSec": 30}, &js)
	run := h.waitRun(alice, js.RunID)
	if run.Status != StatusOK || run.Summary.OK != 2 || len(run.Results) != 2 {
		t.Fatalf("batch %+v", run)
	}
	for _, r := range run.Results {
		if r.Mode != ModeSession || !strings.Contains(r.Output, "BATCH-5 "+r.Name) || strings.Contains(r.Output, "nx$") {
			t.Fatalf("result %+v", r)
		}
	}
	// Sessions opened by the batch are closed again.
	waitFor(t, "batch sessions closed", 5*time.Second, func() bool { return len(h.core.Sessions.List(alice, false)) == 0 })

	// A failing host with stopOnError.
	h.must(alice, "POST", "/api/automation/batch", map[string]any{"connectionIds": []string{c1.ID}, "kind": "command",
		"command": "echo x", "mode": "exec"}, &js)
	if run = h.waitRun(alice, js.RunID); run.Status != StatusError || !strings.Contains(run.Results[0].Error, "SSH") {
		t.Fatalf("exec on a local shell %+v", run)
	}
	if st, code := h.code(alice, "POST", "/api/automation/batch", map[string]any{"connectionIds": []string{c1.ID}, "kind": "command",
		"command": "mkfs.ext4 /dev/sdz1"}); st != 409 || code != "dangerous_command" {
		t.Fatalf("dangerous batch: %d %s", st, code)
	}

	var prev previewResponse
	h.must(alice, "GET", "/api/automation/schedules/preview?spec=*/15+*+*+*+*&count=3", nil, &prev)
	if !prev.Valid || len(prev.Next) != 3 || prev.Next[1].Sub(prev.Next[0]) != 15*time.Minute {
		t.Fatalf("preview %+v", prev)
	}
	h.must(alice, "GET", "/api/automation/schedules/preview?spec=@every+5s", nil, &prev)
	if prev.Valid {
		t.Fatalf("5s schedule accepted")
	}
	var sch Schedule
	h.must(alice, "POST", "/api/automation/schedules", map[string]any{"name": "nightly", "spec": "@daily",
		"action": map[string]any{"kind": "command", "command": "echo SCHED-OK"}, "connectionIds": []string{c1.ID}, "notify": "always"}, &sch)
	if sch.NextRunAt == nil || !sch.Enabled {
		t.Fatalf("schedule %+v", sch)
	}
	h.must(alice, "POST", "/api/automation/schedules/"+sch.ID+"/run", nil, &js)
	if run = h.waitRun(alice, js.RunID); run.Status != StatusOK || !strings.Contains(run.Results[0].Output, "SCHED-OK") ||
		run.RefID != sch.ID || run.Target != "1 connection" {
		t.Fatalf("schedule run %+v", run)
	}
	// A script task without connections runs once; its runs belong to the task (its "show runs" history).
	var sc Script
	h.must(alice, "POST", "/api/scripts", map[string]any{"name": "tick", "content": `log("tick " + vars.n)`}, &sc)
	var scriptTask Schedule
	h.must(alice, "POST", "/api/automation/schedules", map[string]any{"name": "ticker", "spec": "@hourly",
		"action": map[string]any{"kind": "script", "scriptId": sc.ID, "variables": map[string]string{"n": "7"}}}, &scriptTask)
	var sjs jobStarted
	h.must(alice, "POST", "/api/automation/schedules/"+scriptTask.ID+"/run", nil, &sjs)
	if srun := h.waitRun(alice, sjs.RunID); srun.Status != StatusOK || srun.Kind != RunScript || srun.RefID != scriptTask.ID ||
		!strings.Contains(srun.Log, "tick 7") {
		t.Fatalf("script task run %+v", srun)
	}
	var taskRuns []Run
	h.must(alice, "GET", "/api/automation/runs?refId="+scriptTask.ID, nil, &taskRuns)
	if len(taskRuns) != 1 || taskRuns[0].ID != sjs.RunID {
		t.Fatalf("task runs %+v", taskRuns)
	}
	h.must(alice, "DELETE", "/api/automation/schedules/"+scriptTask.ID, nil, nil)
	var list []Schedule
	waitFor(t, "last status", 5*time.Second, func() bool {
		h.must(alice, "GET", "/api/automation/schedules", nil, &list)
		return len(list) == 1 && list[0].LastStatus == StatusOK && list[0].LastRunID == js.RunID
	})
	var disabled Schedule
	h.must(alice, "PATCH", "/api/automation/schedules/"+sch.ID, map[string]any{"enabled": false}, &disabled)
	if disabled.NextRunAt != nil || disabled.Enabled {
		t.Fatalf("disabled schedule still scheduled")
	}
	if st, _ := h.code(alice, "POST", "/api/automation/schedules", map[string]any{"name": "x", "spec": "* * *",
		"action": map[string]any{"kind": "command", "command": "x"}, "connectionIds": []string{c1.ID}}); st != 400 {
		t.Fatalf("bad spec: %d", st)
	}
	h.must(alice, "DELETE", "/api/automation/schedules/"+sch.ID, nil, nil)
	h.must(alice, "DELETE", "/api/automation/runs", nil, nil)
	var runs []Run
	h.must(alice, "GET", "/api/automation/runs", nil, &runs)
	if len(runs) != 0 {
		t.Fatalf("runs not cleared: %d", len(runs))
	}
}
