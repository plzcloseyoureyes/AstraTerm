package term

import (
	"bytes"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func TestShellFamily(t *testing.T) {
	cases := map[string]string{
		"/bin/bash": "posix", "-bash": "posix", "/usr/bin/zsh\n": "posix", "ksh93": "posix", "/bin/sh": "posix",
		"/bin/busybox": "posix", "/usr/local/bin/fish": "fish", "/bin/tcsh": "", "/bin/csh": "", "xonsh": "",
		"": "", "/usr/bin/nu": "",
	}
	for in, want := range cases {
		if got := ShellFamily(in); got != want {
			t.Errorf("ShellFamily(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShellIntegrationLine(t *testing.T) {
	for _, fam := range []string{"posix", "fish"} {
		for _, host := range []string{"", "web-01.example.com"} {
			line := ShellIntegrationLineFor(fam, host)
			if line == "" || line[0] != ' ' {
				t.Fatalf("%s: line must start with a space: %q", fam, line)
			}
			if strings.ContainsFunc(line, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
				t.Fatalf("%s: line must be a single line without control characters", fam)
			}
			if host != "" && !strings.Contains(line, host) {
				t.Fatalf("%s: host guard missing: %q", fam, line)
			}
		}
	}
	if ShellIntegrationLine("") != "" || ShellIntegrationLine("csh") != "" {
		t.Fatal("unsupported families must yield no line")
	}
	// Host names that would need quoting are not embedded (no guard instead).
	if l := ShellIntegrationLineFor("posix", "a'b"); strings.Contains(l, "a'b") || strings.Contains(l, "uname") {
		t.Fatalf("unsafe host embedded: %q", l)
	}
	posix := ShellIntegrationLine("posix")
	// The outer eval string is single-quoted: it must contain no single quote itself.
	inner := strings.TrimSuffix(strings.TrimPrefix(posix, ` [ -n "$OPTIND" ]&&eval '`), `'||printf '\033]7;\007'`)
	if inner == posix || strings.Contains(inner, "'") {
		t.Fatalf("unexpected structure / nested single quote: %s", posix)
	}
	for _, want := range []string{"PROMPT_COMMAND", "precmd_functions", "PS1", "history -d \\$HISTCMD", "BB_ASH_VERSION",
		"[[:cntrl:]]"} {
		if !strings.Contains(posix, want) {
			t.Errorf("posix line lacks %q", want)
		}
	}
	if fish := ShellIntegrationLine("fish"); !strings.Contains(fish, "--on-variable PWD") || !strings.HasSuffix(fish, "__nx7") {
		t.Fatalf("fish line %q", fish)
	}
}

func TestLooksLikePrompt(t *testing.T) {
	good := []string{"user@host:~$ ", "ssh1:~$ ", "root@box:/etc# ", "% ", "❯ ", "PS C:\\Users\\me> ", "[user@host ~]$",
		"$ ", "lior@mac ~/Projects> ", "user@host ~>", "host ➜ "}
	bad := []string{"", "   ", "Password: ", "[sudo] password for test:", "Are you sure (yes/no)? ", "--More--",
		"login:", "Enter passphrase for key '/x':", "Continue?", ">>> ", "mysql> ", "    -> ", "> ", "for> ", "dquote> ",
		"irb(main):001:0> ", "postgres=# ", "db=> ", "127.0.0.1:6379> ", "ssh1:~$ ls -la", "sqlite> "}
	for _, s := range good {
		if !looksLikePrompt(s) {
			t.Errorf("%q should look like a prompt", s)
		}
	}
	for _, s := range bad {
		if looksLikePrompt(s) {
			t.Errorf("%q should not look like a prompt", s)
		}
	}
}

func TestTokenizerAcrossChunks(t *testing.T) {
	stream := []byte("ab\x1b[31mc\x1b]7;/tmp\x07d\x1b]0;title\x1b\\e\x1b[?2004h\x1b(Bf\x1b7g\x1bPdcs\x1b\\\r\n")
	var whole []token
	var tz tokenizer
	tz.feed(stream, func(t token) { whole = append(whole, t) })
	for split := 1; split < len(stream); split++ {
		var got []token
		var tz2 tokenizer
		tz2.feed(stream[:split], func(t token) { got = append(got, t) })
		tz2.feed(stream[split:], func(t token) { got = append(got, t) })
		if len(got) != len(whole) {
			t.Fatalf("split %d: %d tokens, want %d", split, len(got), len(whole))
		}
		for i := range got {
			if !bytes.Equal(got[i].b, whole[i].b) || got[i].kind != whole[i].kind || got[i].safe != whole[i].safe || got[i].osc != whole[i].osc {
				t.Fatalf("split %d token %d: %+v vs %+v", split, i, got[i], whole[i])
			}
		}
	}
	var oscs []string
	var unsafe []string
	for _, tk := range whole {
		if tk.kind == tokSeq && tk.osc != "" {
			oscs = append(oscs, tk.osc)
		}
		if tk.kind == tokSeq && !tk.safe {
			unsafe = append(unsafe, string(tk.b))
		}
	}
	if strings.Join(oscs, ",") != "7,0" {
		t.Fatalf("oscs %v", oscs)
	}
	if len(unsafe) != 1 || unsafe[0] != "\x1b7" {
		t.Fatalf("unsafe %q", unsafe)
	}
	if got := string(printables(stream, 100)); got != "abcdefg" {
		t.Fatalf("printables %q", got)
	}
}

func TestClassify(t *testing.T) {
	safe := []string{"\x1b[0m", "\x1b[?2004h", "\x1b[?1l", "\x1b[6n", "\x1b[>1u", "\x1b[?u", "\x1b[2 q", "\x1b=", "\x1b>",
		"\x1b(B", "\x1b]133;A\x07"}
	unsafe := []string{"\x1b[K", "\x1b[2J", "\x1b[10;1H", "\x1b[3C", "\x1b[A", "\x1b[s", "\x1b[u", "\x1b[1;24r", "\x1b8",
		"\x1bM", "\x1bc", "\x1b[5X", "\x1b[2P", "\x1b[?1J"}
	for _, s := range safe {
		if ok, _ := classify([]byte(s)); !ok {
			t.Errorf("%q should be safe", s)
		}
	}
	for _, s := range unsafe {
		if ok, _ := classify([]byte(s)); ok {
			t.Errorf("%q should be dropped during a redraw", s)
		}
	}
}

// ---- echo filter through a real session ---------------------------------------------------------------------------

// shellReply simulates what bash/readline prints after the injected line arrives at an idle prompt.
func shellReply(line, prompt, cwd string, cols int) []byte {
	var b bytes.Buffer
	// readline echo, wrapped at the terminal width (with the " \r" artefact some versions print)
	col := len(prompt)
	for i := 0; i < len(line); i++ {
		b.WriteByte(line[i])
		col++
		if col == cols {
			b.WriteString(" \r")
			col = 0
		}
	}
	b.WriteString("\r\n")          // accept-line
	b.WriteString("\x1b[?2004l\r") // bracketed paste off
	b.WriteString("\x1b]7;" + cwd + "\x07")
	b.WriteString("\x1b]7;" + cwd + "\x07") // PROMPT_COMMAND hook
	b.WriteString("\x1b[?2004h")
	b.WriteString("\x1b[01;32m" + prompt[:len(prompt)-2] + "\x1b[00m" + prompt[len(prompt)-2:])
	return b.Bytes()
}

// siWait polls cond.
func siWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func sendChunks(b *fakeBackend, data []byte, rnd *rand.Rand) {
	for len(data) > 0 {
		n := 1 + rnd.IntN(min(len(data), 40))
		b.chunks <- append([]byte(nil), data[:n]...)
		data = data[n:]
	}
}

func TestInjectHiddenSuppressesEcho(t *testing.T) {
	line := ShellIntegrationLine("posix")
	const prompt = "ssh1:~$ "
	for seed := uint64(1); seed <= 12; seed++ {
		h := newHarness(t, 0)
		b := newFake()
		s := h.create(b, nil)
		h.emit(b, s, []byte("Welcome to OpenSSH Server\r\n"+prompt))
		before := string(s.Scrollback())
		if err := h.m.InjectHidden(s.ID, line, InjectOptions{}); err != nil {
			t.Fatal(err)
		}
		siWait(t, "injected input", func() bool { return b.input() == line+"\r" })
		rnd := rand.New(rand.NewPCG(seed, 7))
		sendChunks(b, shellReply(line, prompt, "/config", 80), rnd)
		siWait(t, "cwd", func() bool { return s.Cwd() == "/config" })
		siWait(t, "filter finished", func() bool { return s.shell().filter.Load() == nil })
		// The released bytes are emitted right after the filter unregistered itself.
		siWait(t, "redraw processed", func() bool { return strings.Contains(string(s.Scrollback()), "\x1b[?2004h") })
		got := string(s.Scrollback())
		rest := strings.TrimPrefix(got, before)
		if strings.Contains(rest, "__nx7") || strings.Contains(rest, "PROMPT_COMMAND") {
			t.Fatalf("seed %d: echo leaked: %q", seed, rest)
		}
		if strings.Contains(rest, "ssh1") {
			t.Fatalf("seed %d: prompt redraw leaked: %q", seed, rest)
		}
		if !strings.Contains(rest, "\x1b]7;/config\x07") || !strings.Contains(rest, "\x1b[?2004h") {
			t.Fatalf("seed %d: state sequences lost: %q", seed, rest)
		}
		// Later output is untouched.
		h.emit(b, s, []byte("ls\r\nfile.txt\r\n"+prompt))
		if !strings.HasSuffix(string(s.Scrollback()), "ls\r\nfile.txt\r\n"+prompt) {
			t.Fatalf("seed %d: later output altered: %q", seed, s.Scrollback())
		}
		h.m.Close(s.ID)
	}
}

func TestInjectHiddenReleasesOnTimeout(t *testing.T) {
	h := newHarness(t, 0)
	b := newFake()
	s := h.create(b, nil)
	h.emit(b, s, []byte("host:~$ "))
	line := ShellIntegrationLine("posix")
	if err := h.m.InjectHidden(s.ID, line, InjectOptions{Timeout: 300 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	siWait(t, "input", func() bool { return b.input() != "" })
	// The shell never answers with OSC 7 (e.g. a program read the line): the echo is released verbatim.
	b.chunks <- []byte(line[:50])
	b.chunks <- []byte(line[50:] + "\r\nsyntax error\r\nhost:~$ ")
	siWait(t, "release", func() bool { return strings.Contains(string(s.Scrollback()), "syntax error") })
	if !strings.Contains(string(s.Scrollback()), line) {
		t.Fatalf("held output must be released verbatim: %q", s.Scrollback())
	}
	siWait(t, "filter finished", func() bool { return s.shell().filter.Load() == nil })
}

func TestInjectHiddenNotEchoLike(t *testing.T) {
	h := newHarness(t, 0)
	b := newFake()
	s := h.create(b, nil)
	h.emit(b, s, []byte("host:~$ "))
	if err := h.m.InjectHidden(s.ID, ShellIntegrationLine("posix"), InjectOptions{}); err != nil {
		t.Fatal(err)
	}
	siWait(t, "input", func() bool { return b.input() != "" })
	// Unrelated output ending with an OSC 7 must not be swallowed.
	b.chunks <- []byte("something else entirely\r\n\x1b]7;/x\x07more")
	siWait(t, "release", func() bool { return strings.Contains(string(s.Scrollback()), "more") })
	if !strings.Contains(string(s.Scrollback()), "something else entirely") {
		t.Fatalf("output lost: %q", s.Scrollback())
	}
}

func TestInjectHiddenRefusesNonPrompt(t *testing.T) {
	h := newHarness(t, 0)
	b := newFake()
	s := h.create(b, nil)
	h.emit(b, s, []byte("[sudo] password for test: "))
	if err := h.m.InjectHidden(s.ID, " true", InjectOptions{}); err != ErrNotAtPrompt {
		t.Fatalf("err = %v, want ErrNotAtPrompt", err)
	}
	if b.input() != "" {
		t.Fatal("nothing must be typed")
	}
	if err := h.m.InjectHidden(s.ID, "a\nb", InjectOptions{}); err == nil {
		t.Fatal("multi-line commands must be refused")
	}
}

func TestInjectHiddenUserTypingEndsPromptPhase(t *testing.T) {
	h := newHarness(t, 0)
	b := newFake()
	s := h.create(b, nil)
	const prompt = "host:~$ "
	h.emit(b, s, []byte(prompt))
	line := ShellIntegrationLine("posix")
	if err := h.m.InjectHidden(s.ID, line, InjectOptions{}); err != nil {
		t.Fatal(err)
	}
	siWait(t, "input", func() bool { return b.input() != "" })
	// Marker arrives, but the prompt redraw differs and never completes: the user starts typing.
	b.chunks <- []byte(line + "\r\n\x1b]7;/home/u\x07" + "host:/home/u$ ")
	siWait(t, "cwd", func() bool { return s.Cwd() == "/home/u" })
	start := time.Now()
	if err := h.m.Write(s.ID, []byte("l")); err != nil {
		t.Fatal(err)
	}
	// The keystroke is held while the filter runs, then delivered promptly (the prompt phase ends early).
	siWait(t, "typed input delivered", func() bool { return strings.HasSuffix(b.input(), "l") })
	if d := time.Since(start); d > time.Second {
		t.Fatalf("typed input delayed %v", d)
	}
	siWait(t, "filter done", func() bool { return s.shell().filter.Load() == nil })
	b.chunks <- []byte("l") // the shell echoes it
	siWait(t, "typed echo", func() bool { return strings.HasSuffix(string(s.Scrollback()), "l") })
	if strings.Contains(string(s.Scrollback()), "/home/u$") {
		t.Fatal("a (changed) prompt redraw before the typing should be dropped")
	}
}

func TestIsTerminalReply(t *testing.T) {
	replies := []string{"\x1b[12;1R", "\x1b[?1;2c", "\x1b[>0;276;0c", "\x1b[0n", "\x1b[?2004;1$y", "\x1b[?1u",
		"\x1b]11;rgb:0000/0000/0000\x1b\\", "\x1bP1$r0m\x1b\\", "\x1b[1;1R\x1b[?62c", "\x1b[I", "\x1b[O"}
	keys := []string{"a", "ls\r", "\x1b[A", "\x1bOA", "\x1b[3~", "\x1b", "\x1b[12;1Rx", "\x1b[200~paste\x1b[201~", ""}
	for _, s := range replies {
		if !IsTerminalReply([]byte(s)) {
			t.Errorf("%q should be a terminal reply", s)
		}
	}
	for _, s := range keys {
		if IsTerminalReply([]byte(s)) {
			t.Errorf("%q should count as typing", s)
		}
	}
}

func TestInjectHiddenDefersTypedInput(t *testing.T) {
	h := newHarness(t, 0)
	b := newFake()
	s := h.create(b, nil)
	const prompt = "host:~$ "
	h.emit(b, s, []byte(prompt))
	line := ShellIntegrationLine("posix")
	if err := h.m.InjectHidden(s.ID, line, InjectOptions{}); err != nil {
		t.Fatal(err)
	}
	siWait(t, "injected", func() bool { return b.input() == line+"\r" })
	// The user types while the hidden command runs: the keystrokes wait until the filter is done.
	if err := h.m.Write(s.ID, []byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if b.input() != line+"\r" {
		t.Fatalf("typed input reached the shell during the hidden command: %q", b.input())
	}
	b.chunks <- shellReply(line, prompt, "/home/u", 200)
	siWait(t, "typed input delivered after the hidden command", func() bool { return b.input() == line+"\r"+"ls\r" })
	siWait(t, "filter done", func() bool { return s.shell().filter.Load() == nil })
	// The echo and output of the user's own command are never filtered.
	h.emit(b, s, []byte("ls\r\nfile\r\n"+prompt))
	sb := string(s.Scrollback())
	if !strings.HasSuffix(sb, "ls\r\nfile\r\n"+prompt) || strings.Contains(sb, "__nx7") {
		t.Fatalf("user output altered: %q", sb)
	}
	if s.Cwd() != "/home/u" {
		t.Fatalf("cwd %q", s.Cwd())
	}
}

func TestInjectHiddenPrecheckAndAltScreen(t *testing.T) {
	h := newHarness(t, 0)
	b := newFake()
	s := h.create(b, nil)
	h.emit(b, s, []byte("host:~$ "))
	if err := h.m.InjectHidden(s.ID, " true", InjectOptions{Precheck: func() bool { return false }}); err != ErrUserActive {
		t.Fatalf("precheck: %v", err)
	}
	if b.input() != "" || s.shell().filter.Load() != nil {
		t.Fatal("an aborted injection must leave no trace")
	}
	// Input typed right after the abort goes straight to the shell.
	h.m.Write(s.ID, []byte("x"))
	siWait(t, "direct input", func() bool { return b.input() == "x" })
	// A full-screen program (alternate screen) is never typed into, even if its last line looks like a prompt.
	h.emit(b, s, []byte("\r\n\x1b[?1049h\x1b[Hsome editor text\r\n~ $"))
	if err := h.m.InjectHidden(s.ID, " true", InjectOptions{}); err != ErrNotAtPrompt {
		t.Fatalf("alternate screen: %v", err)
	}
	h.emit(b, s, []byte("\x1b[?1049l\r\nhost:~$ "))
	if err := h.m.InjectHidden(s.ID, " true", InjectOptions{Timeout: 50 * time.Millisecond}); err != nil {
		t.Fatalf("after leaving the alternate screen: %v", err)
	}
}

func TestInjectHiddenReleasesBackgroundOutput(t *testing.T) {
	h := newHarness(t, 0)
	b := newFake()
	s := h.create(b, nil)
	const prompt = "host:~$ "
	h.emit(b, s, []byte(prompt))
	line := ShellIntegrationLine("posix")
	if err := h.m.InjectHidden(s.ID, line, InjectOptions{}); err != nil {
		t.Fatal(err)
	}
	siWait(t, "input", func() bool { return b.input() != "" })
	// A background job prints lines while the hidden line is echoed: nothing may be dropped.
	b.chunks <- []byte(line[:100] + "\r\n[1]+ Done  sleep 1\r\nbuild finished\r\nall good\r\n" + line[100:] + "\r\n\x1b]7;/h\x07" + prompt)
	siWait(t, "release", func() bool { return strings.Contains(string(s.Scrollback()), "all good") })
	for _, want := range []string{"build finished", "[1]+ Done"} {
		if !strings.Contains(string(s.Scrollback()), want) {
			t.Fatalf("background output %q dropped: %q", want, s.Scrollback())
		}
	}
}
