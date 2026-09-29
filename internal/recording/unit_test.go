package recording

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func edText(t *testing.T, stream string) string {
	t.Helper()
	tr := newCommandTracker()
	tr.ed.multi = false
	tr.output([]byte(stream))
	return tr.ed.text()
}

func TestLineEditor(t *testing.T) {
	cases := map[string]string{
		"abc\bd":                         "abd",
		"hello\rj":                       "jello",
		"abcdef\x1b[3D\x1b[K":            "abc",
		"abcdef\x1b[2G\x1b[2P":           "adef",
		"abc\x1b[1G\x1b[2@":              "  abc",
		"$ ls\x1b[2D\x1b[1Cx":            "$ lx",
		"a\x1b7bcd\x1b8Z":                "aZcd",
		"\x1b[31mred\x1b[0m text":        "red text",
		"x\x1b]0;title\x07y":             "xy",
		"héllo ✓":                        "héllo ✓",
		"partial \xe2\x9c":               "partial",
		"tab\there":                      "tab     here",
		"prompt$ echo\x1b[4D\x1b[4Xabcd": "prompt$ abcd",
	}
	for in, want := range cases {
		if got := edText(t, in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// feedTracker plays output (o) and input (i) steps and returns the commands.
func feedTracker(steps ...[2]string) []CommandRecord {
	tr := newCommandTracker()
	var got []CommandRecord
	tr.emit = func(r CommandRecord) { got = append(got, r) }
	now := time.Unix(1000, 0)
	for _, s := range steps {
		now = now.Add(10 * time.Millisecond)
		tr.now = now
		switch s[0] {
		case "o":
			tr.output([]byte(s[1]))
		case "s": // an injected secret, masked like term.SensitiveMask
			tr.secretInput([]byte(s[1]))
		default:
			tr.input([]byte(s[1]), nil)
		}
	}
	tr.now = now.Add(time.Hour)
	tr.sweep(tr.now)
	return got
}

func cmds(rs []CommandRecord) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.Command)
	}
	return strings.Join(s, "|")
}

func TestTrackerPlain(t *testing.T) {
	got := feedTracker(
		[2]string{"o", "user@h:~$ "},
		[2]string{"i", "ls\x03"}, // abandoned
		[2]string{"o", "^C\r\nuser@h:~$ "},
		[2]string{"i", "vim x\r"},
		[2]string{"o", "vim x\r\n\x1b[?1049h"},
		[2]string{"i", ":wq\r"}, // inside the full-screen editor: not a command
		[2]string{"o", "\x1b[?1049l\r\nuser@h:~$ "},
		[2]string{"i", "cat /etc/hostname\r"},
		[2]string{"o", "cat /etc/hostname\r\nbox\r\nEnter passphrase: "},
		[2]string{"i", "s3cret\r"}, // not echoed
		[2]string{"o", "\r\nuser@h:~$ "},
		[2]string{"i", "\x1b[A\r"}, // history recall: the echo tells what ran
		[2]string{"o", "cat /etc/hostname\r\nbox\r\nuser@h:~$ "},
	)
	if c := cmds(got); c != "vim x|cat /etc/hostname|cat /etc/hostname" {
		t.Fatalf("commands %q", c)
	}
}

func TestTrackerSecretPromptWithEcho(t *testing.T) {
	// Some prompts echo the secret (e.g. an OTP): never audit lines typed at secret-looking prompts.
	got := feedTracker(
		[2]string{"o", "Verification code: "},
		[2]string{"i", "123456\r"},
		[2]string{"o", "123456\r\n$ "},
	)
	if len(got) != 0 {
		t.Fatalf("secret audited: %+v", got)
	}
}

func TestTrackerInjectedSecret(t *testing.T) {
	// An injected secret echoed by the shell (typed at the wrong place) is never audited: neither with Enter in the
	// same write, nor when Enter follows as ordinary input (macro secret steps), nor with shell integration.
	got := feedTracker(
		[2]string{"o", "$ "},
		[2]string{"s", "******\r"},
		[2]string{"o", "hunter\r\nhunter: command not found\r\n$ "},
		[2]string{"s", "******"},
		[2]string{"i", "\r"},
		[2]string{"o", "hunter\r\nhunter: command not found\r\n$ "},
		[2]string{"i", "ls\r"},
		[2]string{"o", "ls\r\nfile\r\n$ "},
		[2]string{"o", "\x1b]133;A\x07$ \x1b]133;B\x07"},
		[2]string{"s", "******\r"},
		[2]string{"o", "hunter\r\n\x1b]133;C\x07\x1b]133;D;127\x07\x1b]133;A\x07$ \x1b]133;B\x07"},
		[2]string{"o", "pwd\r\n\x1b]133;C\x07/root\r\n\x1b]133;D;0\x07"},
	)
	if c := cmds(got); c != "ls|pwd" {
		t.Fatalf("commands %q", c)
	}
}

func TestTrackerShellIntegration(t *testing.T) {
	got := feedTracker(
		[2]string{"o", "\x1b]133;A\x07~ ❯ \x1b]133;B\x07"},
		[2]string{"o", "make tset\b\b\bes"},
		[2]string{"o", "\r\n\x1b]133;C\x07building...\r\n"},
		[2]string{"o", "\x1b]133;D;2\x07\x1b]133;A\x07~ ❯ \x1b]133;B\x07"},
		[2]string{"o", "\x1b]633;E;echo a\\x3bb\x07echo a;b\r\n\x1b]133;C\x07a\r\n\x1b]133;D;0\x07"},
	)
	if c := cmds(got); c != "make test|echo a;b" {
		t.Fatalf("commands %q", c)
	}
	if got[0].ExitCode == nil || *got[0].ExitCode != 2 || got[0].Source != "shell-integration" {
		t.Fatalf("first %+v", got[0])
	}
}

func TestTrackerLongRunningCommand(t *testing.T) {
	tr := newCommandTracker()
	var got []CommandRecord
	tr.emit = func(r CommandRecord) { got = append(got, r) }
	now := time.Unix(1000, 0)
	tr.now = now
	tr.output([]byte("\x1b]133;A\x07$ \x1b]133;B\x07tail -f log\r\n\x1b]133;C\x07"))
	tr.sweep(now.Add(time.Second))
	if len(got) != 0 {
		t.Fatal("emitted too early")
	}
	tr.sweep(now.Add(6 * time.Second))
	if len(got) != 1 || !got[0].Running || got[0].ExitCode != nil {
		t.Fatalf("running: %+v", got)
	}
	tr.now = now.Add(time.Minute)
	tr.output([]byte("\x1b]133;D;130\x07"))
	if len(got) != 1 || len(tr.recent) != 1 || tr.recent[0].ExitCode == nil || *tr.recent[0].ExitCode != 130 {
		t.Fatalf("finished: %+v / %+v", got, tr.recent)
	}
}

func TestCastConversionAndPartialLines(t *testing.T) {
	v3 := `{"version":3,"term":{"cols":100,"rows":30,"type":"xterm-256color"},"timestamp":1700000000,"title":"t"}
[0.5, "o", "a\u001b[1mb\r\n"]
[0.25, "r", "120x40"]
[1.000000, "m", ""]
[0.1, "x", "0"]
[0.3, "o", "trunc`
	var out bytes.Buffer
	if err := convertCast(&out, strings.NewReader(v3), 2); err != nil {
		t.Fatal(err)
	}
	want := `{"version":2,"width":100,"height":30,"timestamp":1700000000,"title":"t","env":{"TERM":"xterm-256color"}}
[0.500000, "o", "a\u001b[1mb\r\n"]
[0.750000, "r", "120x40"]
[1.750000, "m", ""]
`
	if out.String() != want {
		t.Fatalf("v2:\n%s\nwant:\n%s", out.String(), want)
	}
	// Round trip back to v3 keeps the intervals.
	var back bytes.Buffer
	if err := convertCast(&back, strings.NewReader(out.String()), 3); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(back.String(), `[0.250000, "r", "120x40"]`) {
		t.Fatalf("v3 again: %s", back.String())
	}
	if _, err := readCast(strings.NewReader("not a cast"), func(castEvent) error { return nil }); err != errNotCast {
		t.Fatalf("garbage: %v", err)
	}
}

func TestReplayUTF8Split(t *testing.T) {
	data := []byte("\x9c✓ start ✓") // starts inside a character
	pts := []tlPoint{{off: 100, t: time.Unix(10, 0), cols: 80, rows: 24}, {off: 106, t: time.Unix(11, 0), cols: 90, rows: 24}}
	// offset 106 falls inside the second "✓" (bytes 104..106 of "✓ start ✓" relative to 100 → split).
	var out bytes.Buffer
	if err := buildReplay(&out, replayInput{data: data, tail: 100, points: pts, cols: 90, rows: 24, now: time.Unix(20, 0)}); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var resized bool
	if _, err := readCast(strings.NewReader(out.String()), func(ev castEvent) error {
		if ev.Code == "o" {
			text.WriteString(ev.Data)
		}
		if ev.Code == "r" && ev.Data == "90x24" {
			resized = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if text.String() != "✓ start ✓" || strings.ContainsRune(text.String(), '\uFFFD') || !resized {
		t.Fatalf("replay %q resized=%v:\n%s", text.String(), resized, out.String())
	}
}

func TestCaptureHandler(t *testing.T) {
	var sink bytes.Buffer
	base := slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelWarn})
	log := slog.New(CaptureLogs(base)).With("module", "unit").WithGroup("req")
	captureOn.Store(true)
	captureLevel.Store(int64(slog.LevelDebug))
	defer captureLevel.Store(int64(slog.LevelInfo))
	_, _, before := debugRing.stats()
	_ = before
	log.Debug("dbg only in ring", "token", "abc", "url", "/x?launch=SECRET&y=1")
	if sink.Len() != 0 {
		t.Fatalf("base got a debug record: %s", sink.String())
	}
	es := debugRing.entries(0)
	e := es[len(es)-1]
	if e.Msg != "dbg only in ring" || e.Module != "unit" || e.Level != "debug" {
		t.Fatalf("entry %+v", e)
	}
	for _, a := range e.Attrs {
		if (a.K == "req.token" && a.V != "[redacted]") || strings.Contains(a.V, "SECRET") {
			t.Fatalf("attr %+v", a)
		}
	}
	if !CaptureLogs(CaptureLogs(base)).(*captureHandler).Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("capture disabled")
	}
}

func TestSecretPrompt(t *testing.T) {
	for _, p := range []string{"Password:", "[sudo] password for bob: ", "Enter passphrase for key '/x':", "Token code:", "PIN: "} {
		if !isSecretPrompt(p) {
			t.Errorf("%q not detected", p)
		}
	}
	for _, p := range []string{"user@host:~$ ", "mysql> ", "~/src ❯ "} {
		if isSecretPrompt(p) {
			t.Errorf("%q misdetected", p)
		}
	}
}

func TestRedaction(t *testing.T) {
	cases := map[string]string{
		"/proxy/abcdefghijklmnopqrst-Zk3_9-xYz/app/":            "/proxy/abcdefghijklmnopqrst-[redacted]/app/",
		"/?__termstead_proxy_token=abcDEF123&x=1":               "/?__termstead_proxy_token=[redacted]&x=1",
		"https://bob:hunter2@example.com/x":                     "https://bob:[redacted]@example.com/x",
		"Authorization: Basic Ym9iOmh1bnRlcjI=":                 "Authorization: Basic [redacted]",
		"/ws/share/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA": "/ws/share/[redacted]",
		"/x?api_key=k1&password=p2":                             "/x?api_key=[redacted]&password=[redacted]",
	}
	for in, want := range cases {
		if got := redactValue(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
	type conn struct{ Password string }
	var fields []LogField
	fields = appendAttr(fields, "", slog.Any("conn", conn{Password: "hunter2"}))
	fields = appendAttr(fields, "", slog.String("apiKey", "k"))
	fields = appendAttr(fields, "", slog.Any("err", errors.New("dial https://u:pw@h/")))
	for _, f := range fields {
		if strings.Contains(f.V, "hunter2") || f.V == "k" || strings.Contains(f.V, ":pw@") {
			t.Fatalf("leaked: %+v", f)
		}
	}
}
