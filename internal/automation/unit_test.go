package automation

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/termstead/termstead/internal/model"
)

func TestTemplateVars(t *testing.T) {
	src := `ssh {{user}}@{{ host }} -p {{port|22}}; echo {{ env | dev | staging | prod }} {{pw:secret}} \{{literal}} ` +
		`{{.State.Status}} {{ 1bad }} {{name}} {{name|later-default}}`
	vars := TemplateVars(src)
	got := map[string]TemplateVar{}
	var order []string
	for _, v := range vars {
		got[v.Name] = v
		order = append(order, v.Name)
	}
	if want := []string{"user", "host", "port", "env", "pw", "name"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
	if !got["user"].Builtin || !got["host"].Builtin || !got["port"].Builtin || got["env"].Builtin {
		t.Fatalf("builtin flags wrong: %+v", got)
	}
	if got["port"].Default != "22" {
		t.Fatalf("port default %q", got["port"].Default)
	}
	if !reflect.DeepEqual(got["env"].Choices, []string{"dev", "staging", "prod"}) || got["env"].Default != "dev" {
		t.Fatalf("env choices %+v", got["env"])
	}
	if !got["pw"].Secret {
		t.Fatalf("pw should be secret")
	}
	if got["name"].Default != "later-default" {
		t.Fatalf("a later occurrence should add a default: %+v", got["name"])
	}
}

func TestRenderTemplate(t *testing.T) {
	src := "cd {{dir|/tmp}} && ls {{flags}} {{host}} \\{{x}} {{.Go}} {{missing}} {{clipboard}}"
	out, missing := RenderTemplate(src, map[string]string{"flags": "-la"}, map[string]string{"host": "srv1"})
	if out != "cd /tmp && ls -la srv1 {{x}} {{.Go}}  " {
		t.Fatalf("render %q", out)
	}
	if !reflect.DeepEqual(missing, []string{"missing"}) {
		t.Fatalf("missing %v", missing)
	}
	// Explicit empty values are not missing; values override built-ins.
	out, missing = RenderTemplate("a{{x}}b{{host}}", map[string]string{"x": "", "host": "override"}, map[string]string{"host": "h"})
	if out != "aboverride" || len(missing) != 0 {
		t.Fatalf("render %q %v", out, missing)
	}
	// Unterminated / multi-line braces stay literal.
	out, _ = RenderTemplate("{{a\n}} {{b", nil, nil)
	if out != "{{a\n}} {{b" {
		t.Fatalf("literal braces %q", out)
	}
}

func TestSessionBuiltins(t *testing.T) {
	ts := time.Date(2026, 9, 27, 10, 11, 12, 0, time.UTC)
	conn := &model.Connection{Name: "web", Host: "10.0.0.5", Username: "root", Protocol: model.ProtoSSH}
	b := sessionBuiltins(model.RuntimeSession{Title: "Web 1", Protocol: model.ProtoSSH}, conn, ts)
	if b["host"] != "10.0.0.5" || b["user"] != "root" || b["port"] != "22" || b["title"] != "Web 1" ||
		b["date"] != "2026-09-27" || b["time"] != "10:11:12" || b["protocol"] != "ssh" {
		t.Fatalf("builtins %v", b)
	}
}

func TestSnippetText(t *testing.T) {
	text, _ := snippetText("echo a\necho b", model.SendModeExecute, nil, nil)
	if text != "echo a\recho b\r" {
		t.Fatalf("execute %q", text)
	}
	text, _ = snippetText("echo a\r\n", model.SendModeExecute, nil, nil)
	if text != "echo a\r" {
		t.Fatalf("execute with trailing newline %q", text)
	}
	text, _ = snippetText("ls -la", model.SendModePaste, nil, nil)
	if text != "ls -la" {
		t.Fatalf("paste %q", text)
	}
}

func TestDangerousGuard(t *testing.T) {
	on := guardConfig{Enabled: true}
	strict := guardConfig{Enabled: true, Strict: true}
	cases := []struct {
		text   string
		cfg    guardConfig
		rule   string // expected first rule ("" = no hit)
		reason string
	}{
		{"rm -rf /", on, "rm-root", ""},
		{"sudo rm -rf / --no-preserve-root", on, "rm-root", ""},
		{"rm -r -f /*", on, "rm-root", ""},
		{"rm -fr ~", on, "rm-root", ""},
		{"rm -rf ./build", on, "", "relative path is fine unless strict"},
		{"rm -rf ./build", strict, "rm-rf", ""},
		{"rm -rf /tmp/x", on, "", ""},
		{"mkfs.ext4 /dev/sdb1", on, "mkfs", ""},
		{"man mkfs", on, "", ""},
		{"dd if=/dev/zero of=/dev/sda bs=1M", on, "dd-dev", ""},
		{"dd if=a.img of=b.img", on, "", ""},
		{"cat x > /dev/sda", on, "redirect-dev", ""},
		{"sudo reboot", on, "power", ""},
		{"shutdown -h now", on, "power", ""},
		{"echo reboot", on, "", "echoing is harmless"},
		{"systemctl reboot", on, "power", ""},
		{":(){ :|:& };:", on, "fork-bomb", ""},
		{"chmod -R 777 /", on, "chmod-root", ""},
		{"DROP DATABASE prod;", on, "sql-drop", ""},
		{"drop table users;", on, "", ""},
		{"drop table users;", strict, "sql-table", ""},
		{"DELETE FROM users;", strict, "sql-delete-all", ""},
		{"DELETE FROM users WHERE id = 1;", strict, "", ""},
		{"reload", on, "cisco", ""},
		{"show reload", on, "", ""},
		{"write erase", on, "cisco", ""},
		{"format c:", on, "windows-format", ""},
		{"ls -la\nrm -rf \\\n  /", on, "rm-root", "backslash continuation"},
		{"rm -rf /", guardConfig{Enabled: false}, "", "guard disabled"},
		{"kubectl delete ns prod", strict, "k8s-delete", ""},
		{"iptables -F", strict, "firewall-flush", ""},
	}
	for _, tc := range cases {
		hits := checkDangerous(tc.text, tc.cfg)
		got := ""
		if len(hits) > 0 {
			got = hits[0].Rule
		}
		if got != tc.rule {
			t.Errorf("%q (strict=%v): got rule %q, want %q %s (%+v)", tc.text, tc.cfg.Strict, got, tc.rule, tc.reason, hits)
		}
	}
	custom := guardConfig{Enabled: true, Custom: []guardRule{{id: "custom", message: "no prod", severity: sevDanger,
		re: compileCustom(`deploy\s+prod`)}}}
	if hits := checkDangerous("./deploy   PROD now", custom); len(hits) != 1 || hits[0].Message != "no prod" {
		t.Fatalf("custom rule: %+v", hits)
	}
	if compileCustom("(unclosed") != nil {
		t.Fatalf("invalid custom pattern must be ignored")
	}
}

func TestTypedText(t *testing.T) {
	// Keystrokes with corrections, arrow keys and Ctrl-U.
	in := "rm -x\x7frf /\x1b[D\x1b[C\rignored\x15ls\r"
	if got := typedText(in); got != "rm -rf /\nls\n" {
		t.Fatalf("typed %q", got)
	}
	if hits := checkDangerous(typedText(in), guardConfig{Enabled: true}); len(hits) == 0 {
		t.Fatalf("typed rm -rf / not detected")
	}
}

func TestUnescape(t *testing.T) {
	cases := map[string]string{
		`ls\r`:        "ls\r",
		`a\tb\\c`:     "a\tb\\c",
		`\x03`:        "\x03",
		`\e[A`:        "\x1b[A",
		`é\x41`:       "éA",
		`\q \x \xZZ`:  `\q \x \xZZ`,
		`trailing\`:   `trailing\`,
		`\xe9`:        "é",
		`plain`:       "plain",
		`\0end`:       "\x00end",
		`multi\r\n`:   "multi\r\n",
		`\u12`:        `\u12`,
		`nested\\\\r`: `nested\\r`,
	}
	for in, want := range cases {
		if got := Unescape(in); got != want {
			t.Errorf("Unescape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleaners(t *testing.T) {
	if f, _ := cleanFolder(" a / b//c/ "); f != "a/b/c" {
		t.Fatalf("folder %q", f)
	}
	tags, err := cleanTags([]string{" x", "X", "", "y"})
	if err != nil || !reflect.DeepEqual(tags, []string{"x", "y"}) {
		t.Fatalf("tags %v %v", tags, err)
	}
	if _, err := cleanName("  ", "name"); err == nil {
		t.Fatalf("empty name accepted")
	}
	if _, err := cleanName("a\x01b", "name"); err == nil {
		t.Fatalf("control characters accepted")
	}
	if !validSecretKey("sudoPassword") || validSecretKey("hop:abc:password") || validSecretKey("1x") || validSecretKey("") {
		t.Fatalf("validSecretKey")
	}
	if got := commandLines("a \\\nb\nc"); !reflect.DeepEqual(got, []string{"a  b", "c"}) {
		t.Fatalf("commandLines %q", got)
	}
}

func TestStripper(t *testing.T) {
	var lines []string
	l := newTextLog(0)
	s := stripper{onLine: func(b []byte, _ int64) { lines = append(lines, string(b)) }}
	feed := func(p string) { s.feed(l, []byte(p)) }
	// Colors, a title OSC, CRLF, progress bar redraws and a split escape sequence.
	feed("\x1b]0;my title\x07\x1b[1;31mError\x1b[0m: disk full\r\n")
	feed("10%\r20%\r100%\r\n")
	feed("abc\x1b[")
	feed("32mdef\x1b[0m\r")
	feed("\n")
	feed("typo\b\bpo\r\n")
	feed("Password: ")
	if want := []string{"Error: disk full", "100%", "abcdef", "typo"}; !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines %q, want %q", lines, want)
	}
	if cur := string(l.currentLine()); cur != "Password: " {
		t.Fatalf("partial line %q", cur)
	}
	// OSC 133 marks and the alternate screen.
	var marks []string
	s.onMark = func(kind byte, exit *int) {
		m := string(kind)
		if exit != nil {
			m += ":" + string(rune('0'+*exit))
		}
		marks = append(marks, m)
	}
	feed("\x1b]133;A\x07$ \x1b]133;B\x07ls\r\n\x1b]133;C\x07out\r\n\x1b]133;D;3\x1b\\")
	if !reflect.DeepEqual(marks, []string{"A", "B", "C", "D:3"}) {
		t.Fatalf("marks %v", marks)
	}
	n := len(lines)
	feed("\x1b[?1049hfull screen\r\n\x1b[?1049l")
	if len(lines) != n {
		t.Fatalf("alternate screen lines were reported: %q", lines[n:])
	}
	if !strings.Contains(StripText([]byte("a\x1b[31mb\x1bPq#0\x1b\\c\r\nd")), "abc\nd") {
		t.Fatalf("StripText %q", StripText([]byte("a\x1b[31mb\x1bPq#0\x1b\\c\r\nd")))
	}
}

func TestTextLogTrim(t *testing.T) {
	l := newTextLog(64)
	var s stripper
	for i := 0; i < 50; i++ {
		s.feed(l, []byte("line number xx\r\n"))
	}
	if len(l.buf) > 64 {
		t.Fatalf("log not trimmed: %d bytes", len(l.buf))
	}
	if l.base == 0 || !strings.HasPrefix(string(l.from(0)), "line") {
		t.Fatalf("trim should cut at a line boundary: base=%d %q", l.base, l.from(0))
	}
	if got := l.from(l.base - 10); string(got) != string(l.buf) {
		t.Fatalf("from() before base must clamp")
	}
}

func newTestTap() *tap {
	h := newTapHub()
	return h.acquire("s1", nil, model.StateConnected)
}

func TestTapExpect(t *testing.T) {
	tp := newTestTap()
	defer tp.release()
	r := tp.newReader(false)
	ctx := context.Background()
	go func() {
		time.Sleep(20 * time.Millisecond)
		tp.feedDirect([]byte("Last login: yesterday\r\nlogin: "))
	}()
	res, err := r.expect(ctx, []*regexp.Regexp{regexp.MustCompile(`login:\s*$`)}, 2*time.Second)
	if err != nil || res.Match != "login: " || !strings.Contains(res.Before, "Last login") {
		t.Fatalf("expect login: %+v %v", res, err)
	}
	// The consumed text is not matched again; the next prompt is.
	tp.feedDirect([]byte("admin\r\nPassword: "))
	res, err = r.expect(ctx, []*regexp.Regexp{regexp.MustCompile(`login:`), regexp.MustCompile(`(?i)pass(word):`)}, time.Second)
	if err != nil || res.Index != 1 || !reflect.DeepEqual(res.Groups, []string{"word"}) {
		t.Fatalf("expect password: %+v %v", res, err)
	}
	if _, err := r.expect(ctx, []*regexp.Regexp{regexp.MustCompile(`never`)}, 50*time.Millisecond); err != errExpectTimeout {
		t.Fatalf("timeout: %v", err)
	}
	// Disconnect wakes waiters.
	go func() {
		time.Sleep(20 * time.Millisecond)
		tp.setState(model.StateDisconnected)
	}()
	if _, err := r.expect(ctx, []*regexp.Regexp{regexp.MustCompile(`never`)}, 5*time.Second); err != errSessionDown {
		t.Fatalf("disconnect: %v", err)
	}
}

func TestTapReaderAtLineStartAndGeneration(t *testing.T) {
	tp := newTestTap()
	defer tp.release()
	tp.feedDirect([]byte("motd\r\nuser@host:~$ "))
	r := tp.newReader(true)
	if res, err := r.expect(context.Background(), []*regexp.Regexp{regexp.MustCompile(`\$ $`)}, time.Second); err != nil || res.Match != "$ " {
		t.Fatalf("prompt on the current line: %+v %v", res, err)
	}
	lr := tp.newReader(false)
	lr.bindGeneration()
	tp.setState(model.StateConnecting) // reconnect: new generation
	if _, err := lr.expect(context.Background(), []*regexp.Regexp{regexp.MustCompile(`x`)}, time.Second); err != errSessionRestart {
		t.Fatalf("generation: %v", err)
	}
}

func TestTapWaitPromptAndIdle(t *testing.T) {
	tp := newTestTap()
	defer tp.release()
	r := tp.newReader(false)
	ctx := context.Background()
	seq := tp.promptCount()
	go func() {
		time.Sleep(20 * time.Millisecond)
		tp.feedDirect([]byte("output\r\n\x1b]133;A\x07"))
	}()
	ok, err := r.waitPrompt(ctx, seq, nil, 10*time.Millisecond, 2*time.Second)
	if !ok || err != nil {
		t.Fatalf("OSC 133 prompt: %v %v", ok, err)
	}
	tp.feedDirect([]byte("more\r\nrouter#"))
	ok, err = r.waitPrompt(ctx, tp.promptCount(), defaultPromptRe, 30*time.Millisecond, 2*time.Second)
	if !ok || err != nil {
		t.Fatalf("regex prompt: %v %v", ok, err)
	}
	ok, _ = r.waitPrompt(ctx, tp.promptCount(), regexp.MustCompile(`never$`), 10*time.Millisecond, 60*time.Millisecond)
	if ok {
		t.Fatalf("prompt should time out")
	}
	start := time.Now()
	if err := r.waitIdle(ctx, 40*time.Millisecond, time.Second); err != nil || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("waitIdle: %v %s", err, time.Since(start))
	}
	if got := tp.screen(2); got != "more\nrouter#" {
		t.Fatalf("screen %q", got)
	}
}

func TestTapSinksAndPartialDedup(t *testing.T) {
	tp := newTestTap()
	defer tp.release()
	var got []string
	remove := tp.addSink(func(lines []string, partial string, _ []commandEvent) {
		got = append(got, lines...)
		if partial != "" {
			got = append(got, "partial:"+partial)
		}
	})
	tp.feedDirect([]byte("one\r\n[sudo] password for x: "))
	tp.feedDirect([]byte("\r\n"))                      // the prompt line completes: not reported twice
	tp.feedDirect([]byte("two\r\nthree"))              // partial
	tp.feedDirect([]byte("\x1b[?1049hhidden\r\npart")) // alternate screen: nothing
	want := []string{"one", "partial:[sudo] password for x: ", "two", "partial:three"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sink got %q, want %q", got, want)
	}
	remove()
	tp.feedDirect([]byte("\x1b[?1049lafter\r\n"))
	if len(got) != len(want) {
		t.Fatalf("removed sink still called")
	}
}

func TestTapSeedDedupe(t *testing.T) {
	src := &fakeSource{data: []byte("old line\r\nprompt$ ")}
	h := newTapHub()
	tp := h.acquire("s", src, model.StateConnected)
	defer tp.release()
	// A chunk that overlaps the seed (offset 10..19) plus new output.
	tp.process([]tapChunk{{start: 10, data: []byte("prompt$ ls\r\n")}}, false)
	if got := string(tp.log.from(0)); got != "old line\nprompt$ ls\n" {
		t.Fatalf("seed + overlap: %q", got)
	}
	// Second acquire shares the tap.
	if h.acquire("s", src, model.StateConnected) != tp {
		t.Fatalf("acquire must share the tap")
	}
	tp.release()
	if h.get("s") != tp {
		t.Fatalf("tap released too early")
	}
}

type fakeSource struct{ data []byte }

func (f *fakeSource) Offsets() (int64, int64) { return 0, int64(len(f.data)) }
func (f *fakeSource) Scrollback() []byte      { return append([]byte(nil), f.data...) }

func TestCleanCommandOutput(t *testing.T) {
	out := "uname -a\nLinux box 6.1\nuser@box:~$ "
	if got := cleanCommandOutput(out, "uname -a", defaultPromptRe); got != "Linux box 6.1" {
		t.Fatalf("clean %q", got)
	}
	out = "$ echo a; \\\n> echo b\na\nb\n$ "
	if got := cleanCommandOutput(out, "echo a; \\\necho b", defaultPromptRe); got != "a\nb" {
		t.Fatalf("clean multi-line %q", got)
	}
}

func TestParseSpec(t *testing.T) {
	for _, ok := range []string{"*/5 * * * *", "0 3 * * mon-fri", "@hourly", "@every 2m", "CRON_TZ=Europe/Paris 0 8 * * *"} {
		if _, err := parseSpec(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "* * *", "@every 10s", "61 * * * *", "@every 30s"} {
		if _, err := parseSpec(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseLogonActions(t *testing.T) {
	steps, err := parseLogonActions(model.Options{"logonActions": []any{
		map[string]any{"expect": "login:", "send": "admin"},
		map[string]any{"expect": "[Pp]assword:", "secret": "password", "timeoutSec": 5},
	}})
	if err != nil || len(steps) != 2 || steps[1].Secret != "password" || steps[1].TimeoutSec != 5 {
		t.Fatalf("steps %+v %v", steps, err)
	}
	if _, err := parseLogonActions(model.Options{"logonActions": []any{map[string]any{"expect": "(bad"}}}); err == nil {
		t.Fatalf("invalid regex accepted")
	}
	if steps, err := parseLogonActions(model.Options{}); err != nil || steps != nil {
		t.Fatalf("no logon actions: %v %v", steps, err)
	}
}

func TestScopeMatches(t *testing.T) {
	info := model.RuntimeSession{ConnectionID: "c1", Protocol: model.ProtoSSH}
	conn := &model.Connection{Tags: []string{"Prod", "web"}}
	cases := []struct {
		sc   TriggerScope
		want bool
	}{
		{TriggerScope{}, true},
		{TriggerScope{ConnectionIDs: []string{"c2"}}, false},
		{TriggerScope{ConnectionIDs: []string{"c1"}, Protocols: []string{"SSH"}}, true},
		{TriggerScope{Protocols: []string{"telnet"}}, false},
		{TriggerScope{Tags: []string{"prod"}}, true},
		{TriggerScope{Tags: []string{"db"}}, false},
	}
	for _, tc := range cases {
		if got := scopeMatches(tc.sc, info, conn); got != tc.want {
			t.Errorf("%+v: got %v", tc.sc, got)
		}
	}
	if scopeMatches(TriggerScope{Tags: []string{"prod"}}, model.RuntimeSession{Protocol: "ssh"}, nil) {
		t.Fatalf("quick sessions have no tags")
	}
}

func TestExpandGroups(t *testing.T) {
	if got := expandGroups("disk $1 at $2% ($0) $9 $", []string{"sda at 91%", "sda", "91"}); got != "disk sda at 91% (sda at 91%)  $" {
		t.Fatalf("expand %q", got)
	}
}

func TestScriptLimiter(t *testing.T) {
	l := newScriptLimiter()
	for i := 0; i < perUserScripts; i++ {
		if !l.acquire("u") {
			t.Fatalf("acquire %d", i)
		}
	}
	if l.acquire("u") {
		t.Fatalf("per-user limit not enforced")
	}
	if !l.acquire("other") {
		t.Fatalf("other users must not be limited")
	}
	l.release("u")
	if !l.acquire("u") {
		t.Fatalf("release did not free a slot")
	}
}
