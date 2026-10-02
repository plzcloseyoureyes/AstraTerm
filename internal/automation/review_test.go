package automation

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dop251/goja"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Tests added by the automation review: macro wait / secret steps, event triggers, the trigger loop guard, the
// script heap watchdog and log throttling, batch limits.

func TestMacroWaitAndSecretSteps(t *testing.T) {
	h := newHarness(t)
	alice := h.user("alice", model.RoleUser)
	events := h.listen(alice)
	conn := h.savedShell(alice, "macro target", nil, map[string]string{"password": "macro-secret-42"})
	var rs model.RuntimeSession
	h.must(alice, "POST", "/api/sessions", map[string]any{"connectionId": conn.ID, "cols": 100, "rows": 30}, &rs)
	h.waitOutput(alice, rs.ID, "nx$")

	var mc Macro
	h.must(alice, "POST", "/api/macros", map[string]any{"name": "waits", "steps": []map[string]any{
		{"data": "sleep 0.4; echo READY-$((1+1))\r", "delayMs": 0},
		{"waitFor": `READY-2`, "timeoutMs": 5000, "data": "echo AFTER-WAIT\r", "delayMs": 0},
		{"secret": "password", "data": "\r", "delayMs": 0},
	}}, &mc)
	var got Macro
	h.must(alice, "GET", "/api/macros/"+mc.ID, nil, &got)
	if len(got.Steps) != 3 || got.Steps[1].WaitFor != "READY-2" || got.Steps[1].TimeoutMs != 5000 || got.Steps[2].Secret != "password" {
		t.Fatalf("extended steps not stored: %+v", got.Steps)
	}
	var js jobStarted
	h.must(alice, "POST", "/api/macros/"+mc.ID+"/run", map[string]any{"sessionIds": []string{rs.ID}}, &js)
	sb := h.waitOutput(alice, rs.ID, "macro-secret-42") // typed at the prompt: the shell echoes it as a command
	if i := strings.Index(sb, "READY-2\n"); i < 0 || !strings.Contains(sb[i:], "AFTER-WAIT") {
		t.Fatalf("wait step did not wait: %q", sb)
	}

	// A wait that never matches fails that session's replay after its timeout.
	h.must(alice, "POST", "/api/macros", map[string]any{"name": "times out", "steps": []map[string]any{
		{"waitFor": `never-shows-up`, "timeoutMs": 300, "data": "echo NOPE\r"}}}, &mc)
	h.must(alice, "POST", "/api/macros/"+mc.ID+"/run", map[string]any{"sessionIds": []string{rs.ID}}, &js)
	waitFor(t, "macro failure", 10*time.Second, func() bool {
		return events.find(func(ev map[string]any) bool {
			d, _ := ev["data"].(map[string]any)
			return ev["jobId"] == js.JobID && d["kind"] == "done" && strings.Contains(asString(d["error"]), "did not appear")
		}) != nil
	})
	if strings.Contains(h.scrollback(alice, rs.ID), "NOPE") {
		t.Fatal("step after a timed-out wait was typed")
	}
	// Invalid steps are refused.
	if st, _ := h.code(alice, "POST", "/api/macros", map[string]any{"name": "bad", "steps": []map[string]any{{"waitFor": "(", "data": "x"}}}); st != 400 {
		t.Fatalf("bad wait pattern: %d", st)
	}
	if st, _ := h.code(alice, "POST", "/api/macros", map[string]any{"name": "bad", "steps": []map[string]any{{"secret": "hop:x:password"}}}); st != 400 {
		t.Fatalf("bad secret name: %d", st)
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// shellWithMarks opens a /bin/sh session whose prompt carries OSC 133 shell-integration marks (D;exit A … B).
func (h *harness) shellWithMarks(u *model.User) model.RuntimeSession {
	h.t.Helper()
	ps1 := "\x1b]133;D;$?\x07\x1b]133;A\x07mk$ \x1b]133;B\x07"
	quick := map[string]any{"protocol": "local", "options": map[string]any{"shell": "/bin/sh", "loginShell": false,
		"env": map[string]any{"PS1": ps1}}}
	var rs model.RuntimeSession
	h.must(u, "POST", "/api/sessions", map[string]any{"quick": quick, "cols": 120, "rows": 30}, &rs)
	// The whole prompt, including its closing mark: input typed before it would be echoed inside the prompt and count
	// as output.
	h.waitPrompts(u, rs.ID, 1)
	return rs
}

// promptEnd closes the prompt of shellWithMarks.
const promptEnd = "mk$ \x1b]133;B\x07"

// waitPrompts waits until the shell printed n whole prompts, closing mark included: input typed before that would be
// echoed inside or before the prompt and count as output.
func (h *harness) waitPrompts(u *model.User, id string, n int) {
	h.t.Helper()
	waitFor(h.t, "the prompt", 15*time.Second, func() bool {
		return strings.Count(h.scrollbackAs(u, id, "1"), promptEnd) >= n
	})
}

// runAtPrompt types one command line and waits for the prompt that follows it.
func (h *harness) runAtPrompt(u *model.User, id, line string) {
	h.t.Helper()
	n := strings.Count(h.scrollbackAs(u, id, "1"), promptEnd)
	h.input(u, id, line+"\r")
	h.waitPrompts(u, id, n+1)
}

func TestEventTriggers(t *testing.T) {
	h := newHarness(t)
	alice := h.user("alice", model.RoleUser)
	events := h.listen(alice)

	// Command finished (OSC 133) with a failing exit code; output triggers skip the typed command line.
	h.must(alice, "POST", "/api/automation/triggers", map[string]any{"name": "failed", "event": "command", "exit": "error",
		"actions": []map[string]any{{"type": "notify", "level": "error"}, {"type": "log"}}}, nil)
	h.must(alice, "POST", "/api/automation/triggers", map[string]any{"name": "echo check", "pattern": "ECHOCHECK",
		"actions": []map[string]any{{"type": "notify"}}}, nil)
	if st, _ := h.code(alice, "POST", "/api/automation/triggers", map[string]any{"name": "x", "event": "disconnect",
		"actions": []map[string]any{{"type": "send", "text": "y"}}}); st != 400 {
		t.Fatalf("send on disconnect accepted: %d", st)
	}
	if st, _ := h.code(alice, "POST", "/api/automation/triggers", map[string]any{"name": "x", "event": "nope",
		"actions": []map[string]any{{"type": "log"}}}); st != 400 {
		t.Fatalf("unknown event accepted: %d", st)
	}
	rs := h.shellWithMarks(alice)
	h.runAtPrompt(alice, rs.ID, ": ECHOCHECK")
	h.runAtPrompt(alice, rs.ID, "true")
	h.runAtPrompt(alice, rs.ID, "sh -c 'exit 3'")
	waitFor(t, "command trigger", 10*time.Second, func() bool {
		return events.find(func(ev map[string]any) bool {
			n, _ := ev["notify"].(map[string]any)
			return ev["type"] == "automation.trigger" && ev["event"] == "command" && ev["line"] == "sh -c 'exit 3'" &&
				ev["exitCode"] == float64(3) && strings.Contains(asString(n["message"]), "exited with code 3")
		}) != nil
	})
	time.Sleep(300 * time.Millisecond)
	if ev := events.find(func(ev map[string]any) bool { return ev["name"] == "echo check" }); ev != nil {
		t.Fatalf("output trigger matched the typed command: %v", ev)
	}
	if ev := events.find(func(ev map[string]any) bool { return ev["name"] == "failed" && ev["line"] == "true" }); ev != nil {
		t.Fatalf("exit filter ignored: %v", ev)
	}

	// Connect: type something into every new session of a saved connection; disconnect: notify.
	conn := h.savedShell(alice, "evented", nil, nil)
	h.must(alice, "POST", "/api/automation/triggers", map[string]any{"name": "on connect", "event": "connect",
		"scope":   map[string]any{"connectionIds": []string{conn.ID}},
		"actions": []map[string]any{{"type": "send", "text": "echo HELLO-$((6*7))", "enter": true}}}, nil)
	h.must(alice, "POST", "/api/automation/triggers", map[string]any{"name": "on drop", "event": "disconnect",
		"scope":   map[string]any{"connectionIds": []string{conn.ID}},
		"actions": []map[string]any{{"type": "notify", "level": "warning"}}}, nil)
	var srs model.RuntimeSession
	h.must(alice, "POST", "/api/sessions", map[string]any{"connectionId": conn.ID, "cols": 100, "rows": 30}, &srs)
	h.waitOutput(alice, srs.ID, "HELLO-42")
	h.input(alice, srs.ID, "exit\r")
	waitFor(t, "disconnect trigger", 10*time.Second, func() bool {
		return events.find(func(ev map[string]any) bool {
			return ev["type"] == "automation.trigger" && ev["event"] == "disconnect" && ev["sessionId"] == srs.ID
		}) != nil
	})
}

func TestTriggerLoopGuardAndEcho(t *testing.T) {
	old := loopMaxChain
	loopMaxChain = 4
	t.Cleanup(func() { loopMaxChain = old })
	h := newHarness(t)
	alice := h.user("alice", model.RoleUser)
	events := h.listen(alice)
	rs := h.quickShell(alice, nil)

	// The rule's own output re-triggers it after its cooldown: it must be paused after loopMaxChain fires in a row.
	h.must(alice, "POST", "/api/automation/triggers", map[string]any{"name": "looper", "pattern": `^LOOP$`, "cooldownMs": 250,
		"actions": []map[string]any{{"type": "send", "text": "sleep 0.3; echo LOOP", "enter": true}}}, nil)
	h.input(alice, rs.ID, "echo LO''OP\r")
	waitFor(t, "loop paused", 20*time.Second, func() bool {
		return events.find(func(ev map[string]any) bool { return ev["name"] == "looper" && ev["paused"] == true }) != nil
	})
	sb := h.waitOutput(alice, rs.ID, "kept re-triggering itself")
	time.Sleep(1200 * time.Millisecond)
	n1 := strings.Count(h.scrollback(alice, rs.ID), "\nLOOP")
	time.Sleep(1200 * time.Millisecond)
	if n2 := strings.Count(h.scrollback(alice, rs.ID), "\nLOOP"); n2 != n1 || n1 > loopMaxChain+2 {
		t.Fatalf("loop not stopped: %d → %d lines (%q)", n1, n2, sb)
	}

	// A dangerous "send" text needs confirmation when the trigger is saved.
	if st, code := h.code(alice, "POST", "/api/automation/triggers", map[string]any{"name": "danger", "pattern": "x",
		"actions": []map[string]any{{"type": "send", "text": "sudo reboot", "enter": true}}}); st != 409 || code != "dangerous_command" {
		t.Fatalf("dangerous trigger send: %d %s", st, code)
	}
	h.must(alice, "POST", "/api/automation/triggers", map[string]any{"name": "danger", "pattern": "x", "confirmDangerous": true,
		"actions": []map[string]any{{"type": "send", "text": "sudo reboot", "enter": true}}}, nil)
}

func TestEchoSuppression(t *testing.T) {
	st := &sessionTriggers{echo: map[string]echoRecord{"t": {text: "echo hi", at: time.Now()}}}
	if !st.isEchoLocked("t", "nx$ echo hi", time.Now()) {
		t.Fatal("echo not recognised")
	}
	if st.isEchoLocked("t", "hi", time.Now()) || st.isEchoLocked("t", "nx$ echo hi", time.Now().Add(echoWindow+time.Second)) {
		t.Fatal("echo suppression too broad")
	}
}

func TestHeapWatchInterruptsScripts(t *testing.T) {
	var heap atomic.Uint64
	heap.Store(100 << 20)
	w := &heapWatch{vms: map[*goja.Runtime]struct{}{}, budget: 64 << 20, sample: func() uint64 { return heap.Load() }}
	vm := goja.New()
	done := w.add(vm, nil)
	defer done()
	errc := make(chan error, 1)
	go func() {
		_, err := vm.RunString(`var a = []; while (true) { a.push(1); if (a.length > 1000) a = []; }`)
		errc <- err
	}()
	time.Sleep(120 * time.Millisecond)
	heap.Store(200 << 20) // grew by more than the budget
	select {
	case err := <-errc:
		var ie *goja.InterruptedError
		if err == nil || !asInterrupted(err, &ie) || ie.Value() != errScriptMemory {
			t.Fatalf("unexpected result %v", err)
		}
	case <-time.After(5 * time.Second):
		vm.Interrupt("test timeout")
		t.Fatal("script was not interrupted")
	}
}

func asInterrupted(err error, target **goja.InterruptedError) bool {
	ie, ok := err.(*goja.InterruptedError)
	if ok {
		*target = ie
	}
	return ok
}

func TestLogThrottleAndLimiters(t *testing.T) {
	th := newLogThrottle()
	sent, notices := 0, 0
	for range 5000 {
		ok, notice := th.allow()
		if ok {
			sent++
		}
		if notice != "" {
			notices++
		}
	}
	if sent > logEventsBurst+50 || sent < logEventsBurst || notices != 1 {
		t.Fatalf("throttle: sent %d notices %d", sent, notices)
	}
	l := newCountLimiter(2, 3)
	// Two per key, three in total: a, a, (a refused), b, (c refused).
	for i, want := range []bool{true, true, false, true, false} {
		if got := l.acquire([]string{"a", "a", "a", "b", "c"}[i]); got != want {
			t.Fatalf("acquire #%d = %v, want %v", i+1, got, want)
		}
	}
	l.release("a")
	if !l.acquire("c") {
		t.Fatal("slot not freed")
	}
}

func TestScriptMemoryGuardEndToEnd(t *testing.T) {
	old := scriptHeap
	scriptHeap = &heapWatch{vms: map[*goja.Runtime]struct{}{}, budget: 96 << 20, sample: heapObjectBytes}
	t.Cleanup(func() { scriptHeap = old })
	lc := &logCollector{}
	m := bareModule()
	prg, err := compileScript("mem", `var s = "xxxxxxxxxxxxxxxx"; while (true) { s = s + s; }`)
	if err != nil {
		t.Fatal(err)
	}
	err = m.execScript(t.Context(), &model.User{ID: "u"}, scriptParams{name: "mem", program: prg, timeout: 30 * time.Second, logf: lc.logf})
	if err == nil || !strings.Contains(err.Error(), "too much memory") {
		t.Fatalf("runaway allocation not stopped: %v", err)
	}
}
