package term

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

// Adversarial / property tests of the FILE-2 echo filter: whatever the chunking, timing and output, the filter may
// only ever drop the echo of the injected line and the redraw of the old prompt; everything else reaches the
// terminal byte for byte, and keystrokes typed meanwhile reach the shell exactly once, in order.

// randomOutput produces terminal output that is NOT the echo of an injected line: text, control characters, CSI /
// OSC / DCS sequences (including OSC 7 markers and alternate-screen switches), UTF-8, and a trailing partial
// sequence now and then.
func randomOutput(rnd *rand.Rand, n int) []byte {
	var b bytes.Buffer
	pieces := []string{"\r\n", "\r", "\b", "\t", "\x07", "\x1b[31m", "\x1b[0m", "\x1b[2K", "\x1b[10;5H", "\x1b[?2004h",
		"\x1b]7;/tmp\x07", "\x1b]7;file://h/x\x1b\\", "\x1b]0;title\x07", "\x1bP$q m\x1b\\", "\x1b[?1049h", "\x1b[?1049l",
		"héllo wörld ", "❯ ", "$ ", "[1]+  Done  sleep 1", "\x1b]133;A\x07", "\x1b(B", "\x1b7", "\x1b8"}
	for b.Len() < n {
		switch rnd.IntN(3) {
		case 0:
			b.WriteString(pieces[rnd.IntN(len(pieces))])
		default:
			for k := rnd.IntN(12); k >= 0; k-- {
				b.WriteByte(byte(' ' + rnd.IntN(95)))
			}
		}
	}
	if rnd.IntN(4) == 0 {
		b.WriteString("\x1b[3") // partial sequence at the end
	}
	return b.Bytes()
}

// injectAt starts a session showing prompt and injects line; it returns once the line reached the "shell".
func injectAt(t *testing.T, h *harness, prompt string, opts InjectOptions) (*fakeBackend, *Session, string) {
	t.Helper()
	b := newFake()
	s := h.create(b, nil)
	h.emit(b, s, []byte("motd line\r\n"+prompt))
	line := ShellIntegrationLineFor("bash", "")
	if err := h.m.InjectHidden(s.ID, line, opts); err != nil {
		t.Fatal(err)
	}
	siWait(t, "injected input", func() bool { return b.input() == line+"\r" })
	return b, s, line
}

func filterDone(s *Session) bool { return s.shell().filter.Load() == nil }

// Output of a program that is not a shell (the line was read by something else): nothing may be dropped.
func TestEchoFilterNeverDropsForeignOutput(t *testing.T) {
	for seed := uint64(1); seed <= 40; seed++ {
		rnd := rand.New(rand.NewPCG(seed, 99))
		h := newHarness(t, 0)
		b, s, _ := injectAt(t, h, "host:~$ ", InjectOptions{Timeout: 150 * time.Millisecond})
		stream := randomOutput(rnd, 200+rnd.IntN(4000))
		sendChunks(b, stream, rnd)
		siWait(t, "filter done", func() bool { return filterDone(s) })
		// Flush a trailing partial sequence through the (now inactive) filter.
		b.chunks <- []byte("m")
		deadline := time.Now().Add(3 * time.Second)
		for !strings.HasSuffix(string(s.Scrollback()), string(stream)+"m") && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		sb := string(s.Scrollback())
		if !strings.HasSuffix(sb, string(stream)+"m") {
			t.Fatalf("seed %d: output altered\nwant suffix %q\ngot %q", seed, stream, sb[max(0, len(sb)-len(stream)-40):])
		}
		h.m.Close(s.ID)
	}
}

// bashEcho is what readline prints for the injected line (wrapping with the " \r" artefact), the marker, a prompt
// command's OSC 7 and the redraw of a multi-line prompt.
func bashEcho(line, promptLast string, upper []string, cols int) []byte {
	var b bytes.Buffer
	col := len(promptLast)
	for i := 0; i < len(line); i++ {
		b.WriteByte(line[i])
		if col++; col == cols {
			b.WriteString(" \r")
			col = 0
		}
	}
	b.WriteString("\r\n\x1b[?2004l\r\x1b]7;/srv\x07\x1b]7;/srv\x07\x1b[?2004h")
	for _, u := range upper {
		b.WriteString("\x1b[1;34m" + u + "\x1b[0m\r\n")
	}
	b.WriteString("\x1b[01;32m" + promptLast + "\x1b[00m")
	return b.Bytes()
}

// The echo and the redraw are dropped whatever the chunking, and the result is the same for every chunking.
func TestEchoFilterChunkingInvariance(t *testing.T) {
	upper := []string{"", "[alice@web01 /srv/app] (main)"}
	var first string
	for seed := uint64(1); seed <= 30; seed++ {
		rnd := rand.New(rand.NewPCG(seed, 5))
		h := newHarness(t, 0)
		b := newFake()
		s := h.create(b, nil)
		h.emit(b, s, []byte("motd\r\n\r\n\x1b[1;34m"+upper[1]+"\x1b[0m\r\nλ "))
		before := len(s.Scrollback())
		line := ShellIntegrationLineFor("bash", "web01")
		if err := h.m.InjectHidden(s.ID, line, InjectOptions{}); err != nil {
			t.Fatal(err)
		}
		siWait(t, "input", func() bool { return b.input() == line+"\r" })
		sendChunks(b, bashEcho(line, "λ ", upper, 80), rnd)
		siWait(t, "cwd", func() bool { return s.Cwd() == "/srv" })
		siWait(t, "done", func() bool { return filterDone(s) })
		h.emit(b, s, []byte("ls\r\nfile\r\nλ "))
		got := string(s.Scrollback()[before:])
		if strings.Contains(got, "nx7") || strings.Contains(got, "alice@web01") {
			t.Fatalf("seed %d: echo / redraw leaked: %q", seed, got)
		}
		if !strings.HasSuffix(got, "ls\r\nfile\r\nλ ") {
			t.Fatalf("seed %d: later output altered: %q", seed, got)
		}
		if first == "" {
			first = got
		} else if got != first {
			t.Fatalf("seed %d: result depends on chunking:\n%q\n%q", seed, got, first)
		}
		h.m.Close(s.ID)
	}
}

// An unrelated line arriving while the echo is held (a background job) is released, never dropped.
func TestEchoFilterReleasesInterleavedLine(t *testing.T) {
	for seed := uint64(1); seed <= 10; seed++ {
		rnd := rand.New(rand.NewPCG(seed, 11))
		h := newHarness(t, 0)
		b, s, line := injectAt(t, h, "host:~$ ", InjectOptions{})
		k := 40 + rnd.IntN(len(line)-80)
		stream := line[:k] + "\r\n[1]+  Done                    sleep 1\r\n" + line[k:] + "\r\n\x1b]7;/h\x07host:~$ "
		sendChunks(b, []byte(stream), rnd)
		siWait(t, "done", func() bool { return filterDone(s) })
		if !strings.Contains(string(s.Scrollback()), "[1]+  Done") {
			t.Fatalf("seed %d: interleaved output dropped: %q", seed, s.Scrollback())
		}
		h.m.Close(s.ID)
	}
}

// After the marker, only the old prompt's lines are dropped: an unrelated line printed before the prompt redraw
// (bash job notifications, a PROMPT_COMMAND that prints something new) is shown.
func TestEchoFilterPromptPhaseKeepsUnrelatedLines(t *testing.T) {
	h := newHarness(t, 0)
	b, s, line := injectAt(t, h, "host:~$ ", InjectOptions{})
	b.chunks <- []byte(line + "\r\n\x1b]7;/h\x07[1]+  Done   make\r\nhost:~$ ")
	siWait(t, "done", func() bool { return filterDone(s) })
	sb := string(s.Scrollback())
	if !strings.Contains(sb, "[1]+  Done   make\r\n") {
		t.Fatalf("job notification dropped: %q", sb)
	}
	if strings.Contains(sb, "nx7") {
		t.Fatalf("echo leaked: %q", sb)
	}
}

// A slow link: the echo trickles in for much longer than Timeout; the hold is extended while it progresses.
func TestEchoFilterSlowLink(t *testing.T) {
	h := newHarness(t, 0)
	b, s, line := injectAt(t, h, "host:~$ ", InjectOptions{Timeout: 250 * time.Millisecond})
	echo := bashEcho(line, "host:~$ ", nil, 80)
	start := time.Now()
	for len(echo) > 0 {
		n := min(len(echo), 40)
		b.chunks <- append([]byte(nil), echo[:n]...)
		echo = echo[n:]
		time.Sleep(40 * time.Millisecond) // serial-like: ~1 KB/s
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Fatal("test did not exceed the timeout")
	}
	siWait(t, "done", func() bool { return filterDone(s) })
	if strings.Contains(string(s.Scrollback()), "nx7") {
		t.Fatalf("slow echo leaked: %q", s.Scrollback())
	}
}

// Keystrokes typed at random moments of the injection reach the shell exactly once, in order, after the line.
func TestEchoFilterTypingMidInjection(t *testing.T) {
	for seed := uint64(1); seed <= 15; seed++ {
		rnd := rand.New(rand.NewPCG(seed, 3))
		h := newHarness(t, 0)
		b, s, line := injectAt(t, h, "host:~$ ", InjectOptions{})
		echo := bashEcho(line, "host:~$ ", nil, 80)
		var typed strings.Builder
		for i := range 5 {
			k := rnd.IntN(len(echo) + 1)
			sendChunks(b, echo[:k], rnd)
			echo = echo[k:]
			key := fmt.Sprintf("k%d;", i)
			typed.WriteString(key)
			if err := h.m.Write(s.ID, []byte(key)); err != nil {
				t.Fatal(err)
			}
		}
		sendChunks(b, echo, rnd)
		siWait(t, "typed input delivered", func() bool { return b.input() == line+"\r"+typed.String() })
		siWait(t, "done", func() bool { return filterDone(s) })
		h.m.Close(s.ID)
	}
}

// The shell refuses / is not a shell (no marker ever): everything is released verbatim after the timeout.
func TestEchoFilterRefusedInjection(t *testing.T) {
	h := newHarness(t, 0)
	b, s, line := injectAt(t, h, "router# ", InjectOptions{Timeout: 200 * time.Millisecond})
	out := line + "\r\n% Invalid input detected at '^' marker.\r\nrouter# "
	b.chunks <- []byte(out)
	siWait(t, "done", func() bool { return filterDone(s) })
	if !strings.HasSuffix(string(s.Scrollback()), out) {
		t.Fatalf("not released verbatim: %q", s.Scrollback())
	}
}

// Full-screen programs are tracked over the whole stream, however much they print (the former 256 KiB window
// let vim / htop sessions look like a prompt after a while: typing into vim would edit the user's file).
func TestAltScreenTrackedBeyondWindow(t *testing.T) {
	h := newHarness(t, 0)
	b := newFake()
	s := h.create(b, nil)
	h.emit(b, s, []byte("host:~$ vim notes.txt\r\n\x1b[?1049h\x1b[H\x1b[2J"))
	screen := bytes.Repeat([]byte("\x1b[H~ lorem ipsum dolor sit amet, consectetur adipiscing elit\r\n"), 600)
	for range 20 { // ~700 KiB of redraws
		h.emit(b, s, screen)
	}
	h.emit(b, s, []byte("\x1b[24;1H\"notes.txt\" 10L, 200C    3,5     50%"))
	if err := h.m.InjectHidden(s.ID, " true", InjectOptions{}); err != ErrNotAtPrompt {
		t.Fatalf("injected into a full-screen program: %v", err)
	}
	h.emit(b, s, []byte("\x1b[?1049l\r\nhost:~$ "))
	if err := h.m.InjectHidden(s.ID, " true", InjectOptions{Timeout: 50 * time.Millisecond}); err != nil {
		t.Fatalf("after leaving vim: %v", err)
	}
	// A new backend generation (reconnect) starts on the normal screen.
	st := s.shell()
	st.track(1000, []byte("\x1b[?1049h"))
	st.track(999, []byte("\x1b[?1049l")) // late output of an old generation is ignored
	if !st.altActive(1000) {
		t.Fatal("generation tracking")
	}
	st.track(1001, []byte("prompt$ "))
	if st.altActive(1001) {
		t.Fatal("a new generation must start on the normal screen")
	}
}

func TestAltScannerSplits(t *testing.T) {
	stream := []byte("a\x1b]0;\x1b[?1049h in title\x07b\x1bP\x1b[?1049h\x1b\\c\x1b[?1;1049;2h d \x1b[?25l\x1b[?47l\x1b[?1047hx\x1bc")
	var want []bool
	var a altScanner
	alt := false
	for i := range stream {
		a.scan(stream[i:i+1], &alt)
		want = append(want, alt)
	}
	for seed := uint64(1); seed <= 50; seed++ {
		rnd := rand.New(rand.NewPCG(seed, 1))
		var a2 altScanner
		alt2 := false
		for i := 0; i < len(stream); {
			n := 1 + rnd.IntN(8)
			n = min(n, len(stream)-i)
			a2.scan(stream[i:i+n], &alt2)
			i += n
			if alt2 != want[i-1] {
				t.Fatalf("seed %d: state after %d bytes = %v, want %v", seed, i, alt2, want[i-1])
			}
		}
	}
	// strings are skipped, multi-parameter DECSET counts, RIS resets
	var a3 altScanner
	alt3 := false
	a3.scan([]byte("\x1b]0;[?1049h title\x07\x1bP[?1049h\x1b\\"), &alt3)
	if alt3 {
		t.Fatal("text inside an OSC / DCS string must be ignored")
	}
	a3.scan([]byte("\x1b]0;x\x1b[?1049h"), &alt3) // ESC ends the string (VT parser): this one switches
	if !alt3 {
		t.Fatal("ESC inside a string starts a new sequence")
	}
	a3.scan([]byte("\x1b[?1049l"), &alt3)
	a3.scan([]byte("\x1b[?1;1049;2h"), &alt3)
	if !alt3 {
		t.Fatal("multi-parameter DECSET")
	}
	a3.scan([]byte("\x1bc"), &alt3)
	if alt3 {
		t.Fatal("RIS must leave the alternate screen")
	}
}

func TestCursorLine(t *testing.T) {
	cases := map[string]string{
		"user@host:~$ ": "user@host:~$ ",
		"%" + strings.Repeat(" ", 199) + "\r \ruser@host ~ % ": "user@host ~ % ", // zsh PROMPT_SP
		"host ~ % \x1b[50C[12:00]\r\x1b[9C":                    "host ~ % ",      // zsh RPROMPT
		"\x1b[01;32mme@box\x1b[00m:\x1b[01;34m~\x1b[00m$ ":     "me@box:~$ ",
		"progress 10%\rprogress 100%\r\x1b[Kdone $ ":           "done $ ",
		"abc\b\bX":              "aX",
		"\x1b]0;title\x07➜  ~ ": "➜  ~ ",
	}
	for in, want := range cases {
		if got := cursorLine([]byte(in)); got != want {
			t.Errorf("cursorLine(%q) = %q, want %q", in, got, want)
		}
	}
	if got := lineText([]byte("[u@h dir]\x1b[K   \r")); got != "[u@h dir]" {
		t.Errorf("lineText = %q", got)
	}
}

func TestLooksLikePromptMore(t *testing.T) {
	for _, s := range []string{"➜  ~", "➜  repo git:(main) ✗", "user@host ~ % ", "host%"} {
		if !looksLikePrompt(s) {
			t.Errorf("%q should look like a prompt", s)
		}
	}
	for _, s := range []string{"3,5     50%", "\"notes.txt\" 10L, 200C 100%", "downloading... 42%", "-- INSERT --"} {
		if looksLikePrompt(s) {
			t.Errorf("%q should not look like a prompt", s)
		}
	}
}

func TestShellKind(t *testing.T) {
	cases := map[string]string{"/bin/bash": "bash", "-zsh": "zsh", "/bin/mksh": "ksh", "/bin/busybox": "ksh",
		"/bin/sh": "posix", "/usr/bin/dash": "posix", "/usr/bin/fish": "fish", "/bin/tcsh": "", "": ""}
	for in, want := range cases {
		if got := ShellKind(in); got != want {
			t.Errorf("ShellKind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCoverage(t *testing.T) {
	line := []byte(ShellIntegrationLine("bash"))
	c := newCoverage(line, []byte("host:~$ "))
	for _, b := range line[:300] {
		c.add(b)
	}
	if !c.lineOK() || c.best < 300 {
		t.Fatalf("echo not covered: best %d uncovered %d", c.best, c.uncovered)
	}
	c.newLine()
	for _, b := range []byte("[1]+  Done                    sleep 1") {
		c.add(b)
	}
	if c.lineOK() {
		t.Fatal("an unrelated line must not count as echo")
	}
}

// A reconnect drops the previous connection's folder (the new shell starts elsewhere and must be integrated again);
// the new shell's own report then sets it.
func TestReconnectForgetsCwd(t *testing.T) {
	h := newHarness(t, 0)
	b := newFake()
	s := h.create(b, nil)
	h.emit(b, s, []byte("\x1b]7;/var/log\x07host:/var/log$ "))
	if s.Cwd() != "/var/log" {
		t.Fatalf("cwd %q", s.Cwd())
	}
	c := h.attach(s, 0, false)
	defer c.ws.CloseNow()
	b2 := newFake()
	h.backends <- b2
	if err := h.m.Reconnect(s.ID); err != nil {
		t.Fatal(err)
	}
	h.waitState(s, "connected")
	if s.Cwd() != "/var/log" {
		t.Fatal("the folder is kept until the new shell speaks")
	}
	h.emit(b2, s, []byte("Last login: today\r\nhost:~$ "))
	if s.Cwd() != "" {
		t.Fatalf("stale folder kept after reconnect: %q", s.Cwd())
	}
	h.emit(b2, s, []byte("\x1b]7;/home/u\x07"))
	if s.Cwd() != "/home/u" {
		t.Fatalf("new report ignored: %q", s.Cwd())
	}
	// Late output of the replaced backend changes nothing.
	s.shell().track(0, []byte("x"))
	if s.Cwd() != "/home/u" {
		t.Fatal("late output of an old generation must not reset the folder")
	}
}
