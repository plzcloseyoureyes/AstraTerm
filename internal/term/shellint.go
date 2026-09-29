package term

import (
	"bytes"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Shell integration (FILE-2 "follow terminal folder"): most shells do not report their working directory, so the
// files module injects ONE setup line into a freshly started SSH shell that installs an OSC 7 cwd reporter
// (bash PROMPT_COMMAND, zsh precmd_functions, fish --on-variable PWD, ksh / busybox ash PS1). The line is sent like
// typed input but bypasses input hooks and recording, and an echo filter removes its echo and the prompt redraw it
// causes from the output stream, so the user keeps seeing the untouched first prompt. Output is only ever held
// back for a few seconds: if the shell does not behave as expected, everything held is released verbatim.
//
// Safety rules (a user's terminal must never be corrupted):
//   - nothing is typed unless the cursor line looks like a shell prompt and no full-screen program is running: the
//     alternate screen is tracked over the whole output stream of the backend generation (not a bounded window —
//     vim, less, tmux or htop can run for hours);
//   - keystrokes typed meanwhile are held back and delivered, in order, right after the hidden command;
//   - held output is dropped only when it is recognizably the echo of the injected line (and the prompt redraw it
//     causes), otherwise it is released byte for byte.

// ShellFamily classifies a login shell ($SHELL, `ps -o comm`) for ShellIntegrationLine: "posix" (bash, zsh, ksh,
// mksh, busybox ash, dash, sh), "fish", or "" when the shell is unknown / unsupported (csh, tcsh, xonsh, nu, ...).
func ShellFamily(shell string) string {
	s := strings.TrimSpace(shell)
	if i := strings.IndexAny(s, " \t\r\n"); i >= 0 {
		s = s[:i]
	}
	base := strings.TrimPrefix(path.Base(s), "-")
	switch base {
	case "bash", "zsh", "ksh", "ksh93", "mksh", "pdksh", "oksh", "lksh", "ash", "dash", "sh", "busybox", "rbash":
		return "posix"
	case "fish":
		return "fish"
	}
	return ""
}

// Setup code of the POSIX line (see posixLine). Every variant ends by reporting the folder once: that OSC 7 is the
// marker the echo filter waits for (and reports the initial folder). A folder name containing control characters
// is reported as an empty OSC 7 (ignored by the terminal scanner) so a crafted directory name can never inject
// escape sequences into the user's terminal.
const (
	shReport = `__nx7r(){ case $PWD in *[[:cntrl:]]*)printf "\033]7;\007";;*)printf "\033]7;%s\007" "$PWD";;esac;}`
	shBash   = `__nx7(){ local s=$?;__nx7r;return $s;};[[ "${PROMPT_COMMAND[*]-}" == *__nx7* ]]||` +
		`if ((BASH_VERSINFO[0]*100+BASH_VERSINFO[1]>=501));then PROMPT_COMMAND+=(__nx7);` +
		`else PROMPT_COMMAND="__nx7${PROMPT_COMMAND:+;$PROMPT_COMMAND}";fi`
	shZsh  = `__nx7(){ local s=$?;__nx7r;return $s;};(( ${precmd_functions[(I)__nx7]} ))||precmd_functions+=(__nx7)`
	shKsh  = `__nx7(){ __nx7r >/dev/tty;};case "$PS1" in *__nx7*);;*)PS1="\$(__nx7)$PS1";;esac`
	shHist = `[[ $(HISTTIMEFORMAT= history 1) == *__nx7* ]]&&history -d $HISTCMD`

	fishBody = `function __nx7 --on-variable PWD; if string match -qr '[[:cntrl:]]' -- $PWD; printf '\e]7;\a'; ` +
		`else; printf '\e]7;%s\a' $PWD; end; end; __nx7`
)

// ShellKind narrows a login shell down to the setup code it needs: "bash", "zsh", "ksh" (ksh93, mksh, pdksh, oksh,
// busybox ash), "posix" (sh, dash and unknown POSIX shells: every variant, chosen at run time), "fish", or "".
func ShellKind(shell string) string {
	s := strings.TrimSpace(shell)
	if i := strings.IndexAny(s, " \t\r\n"); i >= 0 {
		s = s[:i]
	}
	switch base := strings.TrimPrefix(path.Base(s), "-"); base {
	case "bash", "rbash":
		return "bash"
	case "zsh":
		return "zsh"
	case "ksh", "ksh93", "mksh", "pdksh", "oksh", "lksh", "ash", "busybox":
		return "ksh"
	case "fish":
		return "fish"
	}
	if ShellFamily(shell) == "posix" {
		return "posix"
	}
	return ""
}

// ShellIntegrationLine returns the setup line for a shell family or kind (see ShellFamily, ShellKind), or "" when
// unsupported.
func ShellIntegrationLine(family string) string { return ShellIntegrationLineFor(family, "") }

// ShellIntegrationLineFor is ShellIntegrationLine with a host guard: when host (the `uname -n` of the machine the
// session logged in to) is given, the reporter is only installed if the shell reading the line runs on that host —
// after `ssh other` or `docker exec` the user may be typing into another machine, whose folders the files panel of
// this session must not follow. family is "posix" / "fish" (ShellFamily) or a ShellKind; a kind line only carries
// its shell's setup (shorter), guarded at run time: another shell reading it only reports the folder once. The
// line starts with a space (history-free in shells with ignorespace / fish) and bash also removes it from its
// history list.
func ShellIntegrationLineFor(family, host string) string {
	if !validGuardHost(host) {
		host = ""
	}
	switch family {
	case "posix", "sh":
		return posixLine(host, "bash", "zsh", "ksh")
	case "bash", "zsh", "ksh":
		return posixLine(host, family)
	case "fish":
		if host == "" {
			return " " + fishBody
		}
		return ` if string match -q -- '` + host + `' (uname -n 2>/dev/null); ` + fishBody + `; else; printf '\e]7;\a'; end`
	}
	return ""
}

// posixLine builds the POSIX setup line. It is a polyglot: fish (e.g. exec'd from a bash login shell) parses it
// without error and just prints the marker, because everything POSIX-only is inside a single-quoted eval string
// that fish never evaluates ($OPTIND is always set in POSIX shells, never in fish; it is also safe under set -u).
// Shell-specific parts (bash / zsh / ksh syntax) are double-quoted eval strings so the other POSIX shells never
// parse them. The whole line contains no single quote inside the outer quotes, no newline and no control character.
func posixLine(host string, kinds ...string) string {
	guard := ":"
	if host != "" {
		guard = `[ "$(uname -n 2>/dev/null)" = "` + host + `" ]`
	}
	var b strings.Builder
	b.WriteString("if " + guard + ";then " + shReport + ";")
	kw := "if"
	for _, k := range kinds {
		switch k {
		case "bash":
			b.WriteString(kw + ` [ -n "${BASH_VERSION-}" ];then eval "` + dqEscape(shBash) + `";`)
		case "zsh":
			b.WriteString(kw + ` [ -n "${ZSH_VERSION-}" ];then eval "` + dqEscape(shZsh) + `";`)
		case "ksh":
			b.WriteString(kw + ` [ -n "${KSH_VERSION-}${BB_ASH_VERSION-}" ];then eval "` + dqEscape(shKsh) + `";`)
		default:
			continue
		}
		kw = "elif"
	}
	if kw == "elif" {
		b.WriteString("fi;")
	}
	b.WriteString(`__nx7r;else printf "\033]7;\007";fi;`)
	for _, k := range kinds {
		if k == "bash" {
			b.WriteString(`[ -z "${BASH_VERSION-}" ]||eval "` + dqEscape(shHist) + `";`)
		}
	}
	b.WriteString(":")
	return ` [ -n "$OPTIND" ]&&eval '` + b.String() + `'||printf '\033]7;\007'`
}

// dqEscape escapes s for a POSIX double-quoted string.
func dqEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(s)
}

// validGuardHost accepts host names that can be embedded in the setup line without quoting issues.
func validGuardHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// InjectOptions tunes Manager.InjectHidden.
type InjectOptions struct {
	// MarkerOSC is the OSC code whose appearance proves the injected command ran (default "7").
	MarkerOSC string
	// Timeout bounds how long output is held back without progress before the marker arrives (default 4 s). While
	// the echo of the line keeps arriving (slow links) the hold is extended, up to 8 × Timeout in total.
	Timeout time.Duration
	// Precheck runs once the echo filter is armed (so any further input is deferred) and just before the line is
	// typed; returning false aborts the injection with ErrUserActive (e.g. the user typed meanwhile).
	Precheck func() bool
}

// Errors of InjectHidden.
var (
	ErrNotAtPrompt   = &model.Error{Code: model.CodeConflict, Msg: "the terminal does not show a shell prompt"}
	ErrInjectPending = &model.Error{Code: model.CodeConflict, Msg: "another hidden command is still running"}
	ErrUserActive    = &model.Error{Code: model.CodeConflict, Msg: "the user is typing"}
)

// InjectHidden types line (plus CR) into a connected session's shell without running input hooks, recording or
// logging it, and suppresses its echo and the prompt redraw it causes (see echoFilter). The session's cursor line
// must look like a shell prompt and no full-screen program may be running.
func (m *Manager) InjectHidden(sessionID, line string, opts InjectOptions) error {
	s := m.Get(sessionID)
	if s == nil {
		return model.ErrNotFound
	}
	if line == "" || strings.ContainsFunc(line, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return &model.Error{Code: model.CodeBadRequest, Msg: "invalid hidden command"}
	}
	if opts.MarkerOSC == "" {
		opts.MarkerOSC = "7"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 4 * time.Second
	}
	sh := s.shell()
	s.mu.Lock()
	if s.closed || s.backend == nil || s.state != model.StateConnected {
		s.mu.Unlock()
		return ErrNotConnected
	}
	gen, q, cols := s.gen, s.inq, s.cols
	head := s.ring.Head()
	tail := s.ring.Slice(max(head-promptScan, s.ring.Tail()), head)
	s.mu.Unlock()

	if sh.altActive(gen) {
		return ErrNotAtPrompt // a full-screen program (vim, less, tmux...) owns the terminal
	}
	lines := bytes.Split(tail, []byte{'\n'})
	promptText := cursorLine(lines[len(lines)-1])
	if !looksLikePrompt(promptText) {
		return ErrNotAtPrompt
	}
	// The lines above the cursor line: a multi-line prompt redraws them too (so may they be dropped).
	var known []string
	for i := len(lines) - 2; i >= 0 && i >= len(lines)-5; i-- {
		known = append(known, lineText(lines[i]))
	}
	now := time.Now()
	want := printables([]byte(line), len(line))
	f := &echoFilter{s: s, gen: gen, q: q, want: want, prompt: []byte(promptText), known: known,
		marker: opts.MarkerOSC, lastOut: now, log: m.log, cols: max(cols, 20), timeout: opts.Timeout,
		holdUntil: now.Add(8 * opts.Timeout),
		cov:       newCoverage(want, []byte(promptText))}
	if !sh.filter.CompareAndSwap(nil, f) {
		return ErrInjectPending
	}
	f.removeHook = m.AddHooks(Hooks{OnInput: func(is *Session, data []byte) {
		if is == s && !IsTerminalReply(data) {
			f.userTyped()
		}
	}})
	abort := func(err error) error {
		f.finish(endRelease)
		return err
	}
	// From here on typed input is deferred until the filter finishes; a last check catches input that arrived
	// between the caller's idle check and the arming of the filter.
	if opts.Precheck != nil && !opts.Precheck() {
		return abort(ErrUserActive)
	}
	f.mu.Lock()
	f.timer = time.AfterFunc(opts.Timeout, f.expire)
	f.mu.Unlock()
	if err := q.push(inputItem{data: []byte(line + "\r")}); err != nil {
		return abort(err)
	}
	return nil
}

// promptScan bounds how much recent output is searched for the cursor line.
const promptScan = 64 << 10

// ---- per-session state -------------------------------------------------------------------------------------------

// shellState is the FILE-2 state of a session (Session.echo): the active echo filter, if any, and the alternate
// screen tracker fed with every backend output chunk.
type shellState struct {
	filter atomic.Pointer[echoFilter]

	mu  sync.Mutex
	gen int  // backend generation tracked
	alt bool // the alternate screen is active (a full-screen program runs)
	sc  altScanner
}

// shell returns the session's FILE-2 state (created on first use).
func (s *Session) shell() *shellState {
	if st := s.echo.Load(); st != nil {
		return st
	}
	s.echo.CompareAndSwap(nil, &shellState{})
	return s.echo.Load()
}

// track feeds one backend output chunk of generation gen to the alternate-screen tracker. A new generation (a new
// remote shell after a reconnect) starts on the normal screen; fresh reports that the first output of a new
// generation.
func (st *shellState) track(gen int, data []byte) (fresh bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	switch {
	case gen < st.gen:
		return false // late output of a replaced backend
	case gen > st.gen:
		st.gen, st.alt, st.sc, fresh = gen, false, altScanner{}, true
	}
	st.sc.scan(data, &st.alt)
	return fresh
}

// forgetCwd drops the working directory of a replaced backend: after a reconnect the new shell starts elsewhere
// (usually the home folder) and has no reporter yet, so the old folder would be stale. Clients get {type:'cwd',
// path:""} and a session.updated; the new shell's report (native or via the FILE-2 integration) sets it again.
func (s *Session) forgetCwd() {
	s.mu.Lock()
	changed := s.cwd != "" && !s.closed
	if changed {
		s.cwd = ""
		s.queueCtrlLocked(s.ring.Head(), ctrlJSON(cwdMsg{Type: "cwd", Path: ""}), true)
	}
	s.mu.Unlock()
	if changed {
		s.m.publish(s)
	}
}

// altActive reports whether a full-screen program owns the terminal of generation gen.
func (st *shellState) altActive(gen int) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.gen == gen && st.alt
}

// altScanner is an incremental parser for the alternate-screen switches (DECSET / DECRST 1049, 1047, 47) and RIS.
// OSC / DCS / APC / PM / SOS strings are skipped; sequences split across chunks are handled.
type altScanner struct {
	st      uint8
	private bool
	inter   bool
	n       int
	params  [32]byte
	overflw bool
}

const (
	asGround uint8 = iota
	asEsc
	asCSI
	asStr
	asStrEsc
)

func (a *altScanner) scan(data []byte, alt *bool) {
	for i := 0; i < len(data); i++ {
		if a.st == asGround {
			j := bytes.IndexByte(data[i:], 0x1b)
			if j < 0 {
				return
			}
			i += j
			a.st = asEsc
			continue
		}
		b := data[i]
		switch a.st {
		case asEsc:
			a.escape(b, alt)
		case asCSI:
			switch {
			case b == 0x1b:
				a.st = asEsc
			case b == 0x18 || b == 0x1a:
				a.st = asGround
			case b >= 0x40 && b <= 0x7e:
				if a.private && !a.inter && !a.overflw && (b == 'h' || b == 'l') {
					for _, p := range bytes.Split(a.params[:a.n], []byte{';'}) {
						switch string(p) {
						case "1049", "1047", "47":
							*alt = b == 'h'
						}
					}
				}
				a.st = asGround
			case b >= 0x20 && b <= 0x2f:
				a.inter = true
			case b == '?' && a.n == 0 && !a.private:
				a.private = true
			case b >= 0x30 && b <= 0x3f:
				if a.n < len(a.params) {
					a.params[a.n] = b
					a.n++
				} else {
					a.overflw = true
				}
			}
		case asStr:
			switch b {
			case 0x07, 0x18, 0x1a:
				a.st = asGround
			case 0x1b:
				a.st = asStrEsc
			}
		case asStrEsc:
			if b == '\\' {
				a.st = asGround
			} else {
				a.escape(b, alt)
			}
		}
	}
}

func (a *altScanner) escape(b byte, alt *bool) {
	switch b {
	case '[':
		a.st, a.private, a.inter, a.n, a.overflw = asCSI, false, false, 0, false
	case ']', 'P', '_', '^', 'X':
		a.st = asStr
	case 'c': // RIS: full reset, back to the normal screen
		*alt = false
		a.st = asGround
	case 0x1b:
		a.st = asEsc
	default:
		a.st = asGround
	}
}

// deferInput holds input typed while a hidden command is in flight; it is delivered when the filter finishes, so
// the user's keystrokes (and their echo) never mix with the hidden command. It reports whether data was taken.
func (s *Session) deferInput(data []byte) bool {
	f := s.shell().filter.Load()
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.flushed || f.pendingBytes+len(data) > maxDeferredInput {
		return false // done, or an enormous paste: it goes straight to the shell (the filter then releases)
	}
	f.pending = append(f.pending, append([]byte(nil), data...))
	f.pendingBytes += len(data)
	return true
}

// maxDeferredInput bounds the input held back while a hidden command runs.
const maxDeferredInput = 1 << 20

// IsTerminalReply reports whether input consists only of automatic terminal answers (cursor position / device
// attribute / status / mode / keyboard-protocol / focus reports, OSC and DCS replies) rather than keystrokes. Shells such as
// busybox ash query the cursor position at every prompt; the terminal's answer must not count as user typing.
func IsTerminalReply(data []byte) bool {
	if len(data) == 0 || data[0] != 0x1b {
		return false
	}
	var tz tokenizer
	reply := true
	tz.feed(data, func(t token) {
		if t.kind != tokSeq || len(t.b) < 3 {
			reply = false
			return
		}
		switch t.b[1] {
		case ']', 'P', '_', '^':
			return // OSC (color / clipboard answers) and DCS / APC replies
		case '[':
			switch t.b[len(t.b)-1] {
			case 'R', 'c', 'n', 'y', 't':
				return
			case 'I', 'O': // focus in / out reports (mode 1004)
				if len(t.b) == 3 {
					return
				}
			case 'u':
				if t.b[2] == '?' {
					return
				}
			}
		}
		reply = false
	})
	return reply && tz.state == tzGround
}

// cursorLine models the cursor line of the terminal (the output since the last line feed) and returns the text left
// of the cursor: printable characters overwrite cells, CR / BS / TAB and the cursor movements used by line editors
// and right-hand prompts (CUF, CUB, CHA, HPA, EL, save / restore cursor) move or clear, colours and other sequences
// are ignored. Wide characters count as one cell (approximation).
func cursorLine(line []byte) string {
	cells, col := lineState(line)
	return string(cells[:col])
}

// lineText is the visible text of a whole line (trailing blanks removed).
func lineText(line []byte) string {
	cells, _ := lineState(line)
	return strings.TrimRight(string(cells), " \r")
}

// lineState emulates one terminal line (see cursorLine) and returns its cells and the cursor column.
func lineState(line []byte) ([]rune, int) {
	const maxCells = 1024
	var cells []rune
	col, saved := 0, 0
	put := func(r rune) {
		if col >= maxCells {
			return
		}
		for len(cells) <= col {
			cells = append(cells, ' ')
		}
		cells[col] = r
		col++
	}
	param := func(seq []byte, def int) int {
		n, got := 0, false
		for _, c := range seq[2 : len(seq)-1] {
			if c >= '0' && c <= '9' {
				n, got = min(n*10+int(c-'0'), maxCells), true
			} else {
				break
			}
		}
		if !got || n == 0 {
			return def
		}
		return n
	}
	var tz tokenizer
	var rbuf []byte
	tz.feed(line, func(t token) {
		switch t.kind {
		case tokPrint:
			rbuf = append(rbuf, t.b[0])
			if utf8.FullRune(rbuf) || len(rbuf) >= utf8.UTFMax {
				r, _ := utf8.DecodeRune(rbuf)
				rbuf = rbuf[:0]
				put(r)
			}
		case tokCtrl:
			rbuf = rbuf[:0]
			switch t.b[0] {
			case '\r':
				col = 0
			case '\b':
				col = max(col-1, 0)
			case '\t':
				col = min((col/8+1)*8, maxCells)
			}
		case tokSeq:
			rbuf = rbuf[:0]
			seq := t.b
			switch {
			case len(seq) == 2 && seq[1] == '7':
				saved = col
			case len(seq) == 2 && seq[1] == '8':
				col = saved
			case len(seq) >= 3 && seq[1] == '[':
				if len(seq) > 3 && seq[2] >= 0x3c && seq[2] <= 0x3f {
					return // private sequences (modes)
				}
				switch seq[len(seq)-1] {
				case 'C', 'a':
					col = min(col+param(seq, 1), maxCells)
				case 'D':
					col = max(col-param(seq, 1), 0)
				case 'G', '`':
					col = min(param(seq, 1)-1, maxCells)
				case 'K':
					switch param(seq, 0) {
					case 0:
						if col < len(cells) {
							cells = cells[:col]
						}
					case 1, 2:
						for i := 0; i < min(col+1, len(cells)); i++ {
							cells[i] = ' '
						}
						if param(seq, 0) == 2 {
							cells = cells[:min(col, len(cells))]
						}
					}
				case 'P': // delete characters
					if n := param(seq, 1); col < len(cells) {
						cells = append(cells[:col], cells[min(col+n, len(cells)):]...)
					}
				case 'X': // erase characters
					for i := col; i < min(col+param(seq, 1), len(cells)); i++ {
						cells[i] = ' '
					}
				case 's':
					saved = col
				case 'u':
					col = saved
				}
			}
		}
	})
	if col > len(cells) {
		col = len(cells)
	}
	return cells, col
}

// looksLikePrompt applies conservative heuristics to the text left of the cursor. It must end like a shell prompt
// ($ # % ❯ ➜ → » λ ▶ ⟩, or ">" in a prompt that shows a user@host or a path, as fish and PowerShell do; oh-my-zsh's
// "➜  dir" also counts), and must not be a password / confirmation / pager prompt, a percentage (vim / busybox vi
// ruler "50%", progress), a half-typed command, a continuation prompt ("> ", zsh "for> ", mysql "->") or a REPL
// (">>>", "mysql>", "irb(main):001:0>", psql "db=#").
func looksLikePrompt(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || len(t) > 400 {
		return false
	}
	low := strings.ToLower(t)
	for _, bad := range []string{"password", "passphrase", "passcode", "verification code", "otp", "token",
		"(yes/no", "[y/n]", "(y/n)", "[y/n", "--more--", "(end)", "press ", "login:", "username"} {
		if strings.Contains(low, bad) {
			return false
		}
	}
	r := []rune(t)
	last, prev := r[len(r)-1], rune(0)
	if len(r) >= 2 {
		prev = r[len(r)-2]
	}
	if prev == '=' {
		return false // psql "db=#" / "db=>"
	}
	switch last {
	case '%':
		return prev < '0' || prev > '9' // "host%" / "~ %", not "50%"
	case '$', '#', '❯', '➜', '→', '»', 'λ', '▶', '⟩':
		return true
	case '>':
		if prev == 0 || prev == '>' || prev == '-' {
			return false
		}
		return strings.ContainsAny(t, "@/~\\")
	}
	// oh-my-zsh's default theme puts the arrow first: "➜  ~" / "➜  repo git:(main) ✗".
	return r[0] == '➜' && len(r) <= 200
}

// printables returns the printable bytes of data (escape sequences and control characters removed), at most max.
func printables(data []byte, max int) []byte {
	var tz tokenizer
	var out []byte
	tz.feed(data, func(t token) {
		if t.kind == tokPrint && len(out) < max {
			out = append(out, t.b...)
		}
	})
	return out
}

// ---- echo filter --------------------------------------------------------------------------------------------------

// Filter phases.
const (
	phaseHold   = iota // everything is held until the marker OSC arrives (or the timeout releases it verbatim)
	phasePrompt        // after the marker: the prompt redraw is dropped line by line (state-changing sequences pass)
	phaseDone
)

// Filter limits.
const (
	promptIdleEnd  = 300 * time.Millisecond
	promptPhaseMax = 2 * time.Second
	maxHeldBytes   = 64 << 10
	minEchoRun     = 16 // the echo must show at least this much of the line contiguously (or all of it)
	coverRun       = 6  // printables in a run this long of the line / prompt count as echo / redraw
)

// echoFilter hides the echo of a hidden command and the prompt redraw it causes. It sees the decoded backend output
// of one session generation before the ring buffer, recorder, logger and clients do.
//
// Hold phase: everything is held until the marker OSC. The held output is dropped (its state-changing sequences
// kept) only if it is recognizably the echo: a long contiguous piece of the injected line appears, and EVERY line of
// the held output consists (almost) entirely of runs of the injected line or of the prompt — an unrelated line
// (a background job, an error message, a program that read the line) makes it release everything verbatim.
// Prompt phase: the redraw is held line by line; a completed line is dropped only when it equals a line shown above
// the old prompt (multi-line prompts) or is blank; the redraw ends when the old cursor-line text has been printed
// again. Anything else is released from that point on.
type echoFilter struct {
	s      *Session
	gen    int
	want   []byte   // printable bytes of the injected line
	prompt []byte   // text left of the cursor before the injection (the prompt's last line)
	known  []string // lines shown above the cursor line (a multi-line prompt's upper lines)
	marker string
	cols   int
	log    interface {
		Debug(msg string, args ...any)
	}

	emitMu    sync.Mutex // serializes the filter's output emissions (held while emitting, never with mu)
	mu        sync.Mutex // guards the state below; never held while emitting output (hooks may write input)
	phase     int
	tz        tokenizer
	held      []token
	heldBytes int
	echoMatch int // greedy subsequence match of want within the held printables (progress of the echo)
	cov       *coverage
	foreign   bool // a held line is not echo / prompt text
	lfs       int
	pmatch    int // prompt phase: match index into prompt within the current redraw line
	line      []token
	lineBytes int
	lastOut   time.Time
	markerAt  time.Time
	typed     bool
	timeout   time.Duration // hold timeout without progress
	holdUntil time.Time     // hard limit of the hold phase
	timer     *time.Timer

	removeHook   func()
	q            *inputQueue // the generation's input queue (deferred input is delivered there)
	pending      [][]byte    // input typed while the hidden command was in flight
	pendingBytes int
	flushed      bool // pending input was delivered; later input goes straight to the queue
}

// emitBackend is the pump's output path: output feeds the alternate-screen tracker and passes the echo filter of
// the session, if one is active.
func (s *Session) emitBackend(gen int, data []byte) {
	sh := s.shell()
	if sh.track(gen, data) {
		s.forgetCwd() // before the chunk is scanned: a report in it belongs to the new shell
	}
	if f := sh.filter.Load(); f != nil && f.feed(gen, data) {
		return
	}
	s.emitOutput(data)
}

// feed processes one chunk; it returns false when the filter is inactive (the caller emits the data itself).
func (f *echoFilter) feed(gen int, data []byte) bool {
	f.emitMu.Lock()
	defer f.emitMu.Unlock()
	out, handled := f.process(gen, data)
	if len(out) > 0 {
		f.s.emitOutput(out)
	}
	f.unregisterIfDone()
	return handled
}

// unregisterIfDone detaches a finished filter from the session. It runs under emitMu AFTER the filter's last
// output was emitted: until then the pump keeps calling feed (which waits for emitMu), so newer output can never
// overtake released output.
func (f *echoFilter) unregisterIfDone() {
	f.mu.Lock()
	done := f.phase == phaseDone
	f.mu.Unlock()
	if done {
		f.s.shell().filter.CompareAndSwap(f, nil)
	}
}

// process runs the state machine over data and returns what must be emitted. It takes f.mu.
func (f *echoFilter) process(gen int, data []byte) (out []byte, handled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.phase == phaseDone {
		return nil, false
	}
	if gen != f.gen {
		return f.finishLocked(endRelease), false
	}
	now := time.Now()
	if f.phase == phasePrompt && (now.Sub(f.lastOut) >= promptIdleEnd || now.Sub(f.markerAt) >= promptPhaseMax) {
		return f.finishLocked(endPrompt), false
	}
	f.lastOut = now
	rest := -1 // index in data from which everything passes unfiltered
	f.tz.feed(data, func(t token) {
		if rest >= 0 {
			return
		}
		switch f.phase {
		case phaseHold:
			f.hold(t)
			switch {
			case t.kind == tokSeq && t.osc == f.marker:
				if f.echoLike() {
					out = appendSafe(out, f.held)
					f.held, f.heldBytes = nil, 0
					f.phase, f.markerAt = phasePrompt, now
					if f.timer != nil {
						if f.typed {
							f.timer.Reset(typedFinish)
						} else {
							f.timer.Reset(promptPhaseMax)
						}
					}
				} else {
					out = appendAll(out, f.held)
					f.held, f.heldBytes = nil, 0
					rest = t.end
				}
			case f.heldBytes > maxHeldBytes:
				out = appendAll(out, f.held)
				f.held, f.heldBytes = nil, 0
				rest = t.end
			}
		case phasePrompt:
			var done bool
			out, done = f.redraw(t, out)
			if done {
				rest = t.end
			}
		}
	})
	if rest >= 0 {
		// Everything after the decision point passes unfiltered (including a trailing partial sequence, whose
		// bytes are part of data[rest:]).
		out = append(out, data[rest:]...)
		f.tz.reset()
		out = append(out, f.finishLocked(endQuiet)...)
	}
	return out, true
}

// hold stores a token of the hold phase and tracks the echo. Progress of the echo extends the hold (slow links),
// up to holdUntil.
func (f *echoFilter) hold(t token) {
	f.held = append(f.held, t)
	f.heldBytes += len(t.b)
	switch t.kind {
	case tokCtrl:
		if t.b[0] == '\n' {
			f.lfs++
			if !f.cov.lineOK() {
				f.foreign = true
			}
			f.cov.newLine()
		}
		return
	case tokSeq:
		return
	}
	f.cov.add(t.b[0])
	if f.echoMatch < len(f.want) && t.b[0] == f.want[f.echoMatch] {
		f.echoMatch++
		if f.timer != nil {
			if left := time.Until(f.holdUntil); left > 0 {
				f.timer.Reset(min(f.timeout, left))
			}
		}
	}
}

// echoLike decides whether the held output is the echo of the injected line (plus line-editor redraws).
func (f *echoFilter) echoLike() bool {
	rows := (len(f.prompt)+len(f.want))/f.cols + 1
	return f.cov.best >= min(len(f.want), minEchoRun) && !f.foreign && f.cov.lineOK() && f.lfs <= rows+2
}

// redraw handles one token of the prompt phase: the redraw is held line by line. It returns done when the old
// prompt has been redrawn (the filter ends; the rest passes) or when unrelated output was recognized (released).
func (f *echoFilter) redraw(t token, out []byte) ([]byte, bool) {
	f.line = append(f.line, t)
	f.lineBytes += len(t.b)
	switch {
	case t.kind == tokCtrl && t.b[0] == '\n':
		text := lineText(tokenBytes(f.line))
		if f.knownLine(text) {
			out = appendSafe(out, f.line) // an upper line of a multi-line prompt (or a blank line)
			f.line, f.lineBytes, f.pmatch = f.line[:0], 0, 0
			return out, false
		}
		return appendAll(out, f.line), true // unrelated output: show it and stop filtering
	case t.kind == tokPrint:
		if f.pmatch < len(f.prompt) && t.b[0] == f.prompt[f.pmatch] {
			f.pmatch++
		}
		if len(f.prompt) > 0 && f.pmatch == len(f.prompt) {
			return appendSafe(out, f.line), true // the old prompt line is back: done
		}
	}
	// zsh's PROMPT_SP prints a line of spaces first: allow for the terminal width.
	if f.lineBytes > promptSlackBytes(len(f.prompt), f.cols) {
		return appendAll(out, f.line), true // not a redraw of the old prompt after all
	}
	return out, false
}

func promptSlackBytes(prompt, cols int) int { return 4096 + 8*prompt + 4*cols }

// knownLine reports whether a completed redraw line may be dropped: blank, or equal to a line above the old prompt.
func (f *echoFilter) knownLine(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return true
	}
	for _, k := range f.known {
		if strings.TrimSpace(k) == text {
			return true
		}
	}
	return false
}

// expire runs when the hold / prompt phase took too long: held output is released verbatim (the prompt phase keeps
// dropping a partial line that is a prompt).
func (f *echoFilter) expire() {
	f.mu.Lock()
	hold := f.phase == phaseHold
	f.mu.Unlock()
	if hold && f.log != nil {
		f.log.Debug("term: shell integration marker not seen; releasing held output", "session", f.s.ID)
	}
	f.finish(endRelease)
}

// End modes of the filter.
const (
	endRelease = iota // timeout / abort / new generation: held output is released verbatim
	endPrompt         // the prompt phase went quiet: a partial redraw line that looks like a prompt is dropped
	endQuiet          // decided inside process (the caller already emitted what is due)
)

// finish ends the filter from outside the pump (timer, abort) and emits what it released.
func (f *echoFilter) finish(mode int) {
	f.emitMu.Lock()
	defer f.emitMu.Unlock()
	f.mu.Lock()
	out := f.finishLocked(mode)
	f.mu.Unlock()
	if len(out) > 0 {
		f.s.emitOutput(out)
	}
	f.unregisterIfDone()
}

// typedFinish is how long the prompt phase may continue after the user typed (their input waits meanwhile).
const typedFinish = 150 * time.Millisecond

func (f *echoFilter) userTyped() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.typed = true
	if f.phase == phasePrompt && f.timer != nil {
		f.timer.Reset(typedFinish)
	}
}

// finishLocked ends the filter and returns the output to emit. Input typed meanwhile is delivered to the shell now,
// in order. f.mu held (and emitMu by the caller, so the returned output is emitted before any later output).
func (f *echoFilter) finishLocked(mode int) []byte {
	if f.phase == phaseDone {
		return nil
	}
	var out []byte
	switch {
	case f.phase == phaseHold && mode != endQuiet:
		out = appendAll(out, f.held)
		out = append(out, f.tz.pending()...)
	case f.phase == phasePrompt && mode != endQuiet:
		// A partial redraw line: dropped if it is a prompt (the redraw of a prompt that changed, e.g. a clock),
		// shown otherwise. A trailing partial sequence passes (its end arrives later).
		if looksLikePrompt(cursorLine(tokenBytes(f.line))) || len(f.line) == 0 {
			out = appendSafe(out, f.line)
		} else {
			out = appendAll(out, f.line)
		}
		out = append(out, f.tz.pending()...)
	}
	f.held, f.heldBytes, f.line, f.lineBytes = nil, 0, nil, 0
	f.tz.reset()
	f.phase = phaseDone
	if f.timer != nil {
		f.timer.Stop()
	}
	if f.removeHook != nil {
		go f.removeHook()
	}
	// The queue has its own lock and runs no hooks: deliver deferred input under mu so nothing typed later
	// can overtake it.
	if f.q != nil {
		for _, p := range f.pending {
			_ = f.q.push(inputItem{data: p})
		}
	}
	f.pending, f.pendingBytes, f.flushed = nil, 0, true
	return out
}

func appendAll(out []byte, toks []token) []byte {
	for _, t := range toks {
		out = append(out, t.b...)
	}
	return out
}

// appendSafe keeps the state-changing but screen-neutral sequences (SGR, modes, OSC...) of dropped output.
func appendSafe(out []byte, toks []token) []byte {
	for _, t := range toks {
		if t.kind == tokSeq && t.safe {
			out = append(out, t.b...)
		}
	}
	return out
}

func tokenBytes(toks []token) []byte {
	var b []byte
	for _, t := range toks {
		b = append(b, t.b...)
	}
	return b
}

// coverage tracks which printables of the current held line belong to runs of at least coverRun bytes of one of
// the patterns (the injected line, the prompt), with the longest run of the first pattern (the line).
type coverage struct {
	pats [][]byte
	runs [][]uint16
	best int // longest run of pats[0] seen

	n           int           // printables of the current line
	recent      [1 << 16]byte // the line's printables (ring; runs are bounded by the pattern lengths)
	uncovered   int
	lastCovered int // index of the last covered printable in the line (-1 none)
}

func newCoverage(pats ...[]byte) *coverage {
	c := &coverage{lastCovered: -1}
	for _, p := range pats {
		if len(p) == 0 {
			continue
		}
		c.pats = append(c.pats, p)
		c.runs = append(c.runs, make([]uint16, len(p)))
	}
	return c
}

// add processes one printable byte. Blanks never count as uncovered: line editors pad redraws with them
// (mksh / ksh93 horizontal scrolling erase the rest of the window with spaces).
func (c *coverage) add(b byte) {
	i := c.n
	c.recent[i%len(c.recent)] = b
	c.n++
	if b != ' ' {
		c.uncovered++
	}
	for k, p := range c.pats {
		runs := c.runs[k]
		m := 0
		for j := len(p) - 1; j >= 0; j-- {
			if p[j] != b {
				runs[j] = 0
				continue
			}
			v := 1
			if j > 0 {
				v = int(runs[j-1]) + 1
			}
			runs[j] = uint16(min(v, 1<<16-1))
			m = max(m, v)
		}
		if k == 0 {
			c.best = max(c.best, m)
		}
		if need := min(coverRun, len(p)); m >= need {
			start := max(c.lastCovered+1, i-m+1)
			if start <= i {
				c.uncovered -= c.nonBlank(start, i)
				c.lastCovered = i
			}
		}
	}
}

// nonBlank counts the non-blank printables among the line's printables start..i (kept in c.recent).
func (c *coverage) nonBlank(start, i int) int {
	n := 0
	for k := start; k <= i; k++ {
		if c.recent[k%len(c.recent)] != ' ' {
			n++
		}
	}
	return n
}

// lineOK reports whether the current line is (almost) entirely covered: a few printables may be line-editor
// artefacts (a space at a wrap, horizontal-scroll markers).
func (c *coverage) lineOK() bool { return c.uncovered <= 4+c.n/16 }

// newLine starts the accounting of a new line. Runs continue across line feeds: line editors such as busybox's
// wrap a long line with explicit CR LF, and the fragment after the break continues the run.
func (c *coverage) newLine() {
	c.n, c.uncovered, c.lastCovered = 0, 0, -1
}

// ---- tokenizer ----------------------------------------------------------------------------------------------------

type tokKind uint8

const (
	tokPrint tokKind = iota + 1 // one printable byte (UTF-8 bytes count individually)
	tokCtrl                     // one C0 control byte
	tokSeq                      // a complete escape sequence
)

// token is one unit of terminal output. For sequences, safe marks those that change no screen content or cursor
// position (SGR, modes, OSC, queries, ...), which survive while a redraw is dropped; osc is the OSC code.
type token struct {
	kind tokKind
	b    []byte
	end  int // offset in the current chunk just past the token's last byte
	safe bool
	osc  string
}

// Tokenizer states.
const (
	tzGround = iota
	tzEsc
	tzEscInter
	tzCSI
	tzOSC
	tzOSCEsc
	tzStr
	tzStrEsc
)

const maxSeqLen = 64 << 10

// tokenizer splits terminal output into tokens, keeping incomplete escape sequences across chunks.
type tokenizer struct {
	state int
	seq   []byte
}

func (t *tokenizer) pending() []byte { return append([]byte(nil), t.seq...) }

func (t *tokenizer) reset() {
	t.state = tzGround
	t.seq = t.seq[:0]
}

func (t *tokenizer) feed(data []byte, emit func(token)) {
	for i := 0; i < len(data); i++ {
		b := data[i]
		if t.state == tzGround {
			switch {
			case b == 0x1b:
				t.state, t.seq = tzEsc, append(t.seq[:0], b)
			case b < 0x20 || b == 0x7f:
				emit(token{kind: tokCtrl, b: []byte{b}, end: i + 1})
			default:
				emit(token{kind: tokPrint, b: []byte{b}, end: i + 1})
			}
			continue
		}
		t.seq = append(t.seq, b)
		done := false
		switch t.state {
		case tzEsc:
			switch {
			case b == '[':
				t.state = tzCSI
			case b == ']':
				t.state = tzOSC
			case b == 'P' || b == '_' || b == '^' || b == 'X':
				t.state = tzStr
			case b >= 0x20 && b <= 0x2f:
				t.state = tzEscInter
			default:
				done = true
			}
		case tzEscInter:
			if b >= 0x30 && b <= 0x7e {
				done = true
			}
		case tzCSI:
			if b >= 0x40 && b <= 0x7e {
				done = true
			}
		case tzOSC:
			switch b {
			case 0x07:
				done = true
			case 0x1b:
				t.state = tzOSCEsc
			}
		case tzOSCEsc:
			if b == '\\' {
				done = true
			} else {
				t.state = tzOSC
			}
		case tzStr:
			if b == 0x1b {
				t.state = tzStrEsc
			}
		case tzStrEsc:
			if b == '\\' {
				done = true
			} else {
				t.state = tzStr
			}
		}
		if !done && len(t.seq) > maxSeqLen {
			done = true // runaway sequence: flush it as an opaque (safe) token
		}
		if done {
			seq := append([]byte(nil), t.seq...)
			tok := token{kind: tokSeq, b: seq, end: i + 1}
			tok.safe, tok.osc = classify(seq)
			emit(tok)
			t.state, t.seq = tzGround, t.seq[:0]
		}
	}
}

// classify reports whether a complete escape sequence is safe to keep while a redraw is dropped (it does not move
// the cursor or change screen content) and, for OSC, its code.
func classify(seq []byte) (safe bool, osc string) {
	if len(seq) < 2 {
		return true, ""
	}
	switch seq[1] {
	case ']':
		body := seq[2:]
		body = bytes.TrimSuffix(bytes.TrimSuffix(body, []byte{0x07}), []byte{0x1b, '\\'})
		code, _, _ := bytes.Cut(body, []byte{';'})
		return true, string(code)
	case '[':
		final := seq[len(seq)-1]
		params := seq[2 : len(seq)-1]
		private := len(params) > 0 && params[0] >= 0x3c && params[0] <= 0x3f
		switch final {
		case '@', 'A', 'B', 'C', 'D', 'E', 'F', 'G', 'H', 'I', 'L', 'M', 'P', 'S', 'T', 'X', 'Z', '`', 'a', 'b',
			'd', 'e', 'f':
			return false, ""
		case 'J', 'K':
			return false, ""
		case 'r', 's', 'u':
			return private, ""
		}
		return true, ""
	case 'P', '_', '^', 'X':
		return true, ""
	case '7', '8', 'M', 'D', 'E', 'c':
		return false, ""
	}
	return true, ""
}
