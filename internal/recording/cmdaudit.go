package recording

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/labstack/echo/v5"
	"golang.org/x/time/rate"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// Command audit (REC-5). Every terminal session gets a commandTracker fed from the term output/input hooks:
//
//   - Shell integration (OSC 133 / 633 marks A prompt, B input, C executed, D;exit finished): the command is what the
//     shell displayed between B and C — read back through a line emulator — or the explicit OSC 633;E command line.
//     The exit code and duration come from D.
//   - Without marks: the line the user types is tracked (keys, basic line editing) only to know *when* Enter was
//     pressed; the audited text is what the shell echoed on that line after the prompt (captured when the echo's
//     newline arrives). Input that is never echoed — passwords at no-echo prompts — is therefore never audited;
//     lines typed at prompts that look like secret prompts are skipped as well.
//   - Injected secrets (term.Manager.WriteSensitive: inject-secret, logon / macro / trigger / script secrets) arrive
//     masked; the line they are typed on is never audited, in either mode.
//
// Blind spots (documented): full-screen programs (alternate screen) are skipped; commands run inside editors,
// scripts or REPLs without shell integration appear as typed lines; history expansion shows the typed text.

// CommandRecord is one executed command.
type CommandRecord struct {
	Time       time.Time `json:"time"`
	Command    string    `json:"command"`
	ExitCode   *int      `json:"exitCode,omitempty"`
	DurationMs int64     `json:"durationMs,omitempty"`
	Cwd        string    `json:"cwd,omitempty"`
	Source     string    `json:"source"` // "shell-integration" | "input"
	Running    bool      `json:"running,omitempty"`
	// Guest is set when a viewer of an interactive share link typed (part of) the command.
	Guest *GuestRef `json:"guest,omitempty"`
}

// GuestRef identifies the share-link viewer who typed a command.
type GuestRef struct {
	ShareID  string `json:"shareId"`
	ViewerID string `json:"viewerId"`
	Username string `json:"username,omitempty"` // signed-in viewers
	IP       string `json:"ip,omitempty"`
	Label    string `json:"label,omitempty"` // the link's label
}

const (
	maxCommandRunes   = 4096
	maxRecentCommands = 200
	pendingEmitAfter  = 5 * time.Second // emit a running command without its exit code after this long
	awaitEchoTimeout  = 5 * time.Second
	maxAwaiting       = 64
)

var secretPrompt = regexp.MustCompile(`(?i)(pass(word|phrase|code)?|passwd|\bpin\b|token|secret|otp|one[- ]time|verification|2fa|mfa|authenticat|credential|private key)`)

// isSecretPrompt reports whether the text displayed before the cursor looks like a question for a secret.
func isSecretPrompt(prompt string) bool {
	p := strings.TrimSpace(prompt)
	if p == "" {
		return false
	}
	if i := strings.LastIndexAny(p, "\n"); i >= 0 {
		p = p[i+1:]
	}
	if len(p) > 200 {
		p = p[len(p)-200:]
	}
	return secretPrompt.MatchString(p)
}

type awaitCmd struct {
	input      string
	certain    bool
	promptCol  int
	promptText string
	at         time.Time
	guest      *GuestRef
	secret     bool // an injected secret was typed on the line
}

type pendingCmd struct {
	rec     CommandRecord
	emitted bool
}

// commandTracker follows one session's commands. Guarded by sessState.mu.
type commandTracker struct {
	vt  vtScanner
	ed  lineEd
	alt bool

	// shell integration
	marks    bool // A/B marks seen: the session has shell integration
	region   int  // 0 none, 1 prompt (A), 2 input (B), 3 running (C)
	bLine    int
	bCol     int
	explicit string
	pending  *pendingCmd
	// markSecret: an injected secret was typed at the current prompt, so the next command is not audited.
	markSecret bool

	// line-oriented fallback
	inLine     []rune
	inCol      int
	certain    bool
	lineActive bool
	promptCol  int
	promptText string
	pasting    bool
	inEsc      []byte
	awaiting   []awaitCmd
	lineSecret bool // an injected secret was typed on the current line

	// guest attribution: who typed the chunk being processed, the current line (plain mode) and the command being
	// entered at the prompt (shell integration).
	curGuest  *GuestRef
	lineGuest *GuestRef
	markGuest *GuestRef

	recent []CommandRecord
	emit   func(CommandRecord)
	cwd    func() string
	now    time.Time
}

func newCommandTracker() *commandTracker {
	t := &commandTracker{certain: true}
	t.ed.multi = true
	return t
}

// reset forgets the command state (after a reconnect: a new shell).
func (t *commandTracker) reset() {
	*t = commandTracker{recent: t.recent, certain: true}
	t.ed.multi = true
}

// ---- output -------------------------------------------------------------------------------------------------------

func (t *commandTracker) output(p []byte) { t.vt.feed(p, t) }

func (t *commandTracker) text(r rune) {
	if t.alt {
		return
	}
	t.ed.put(r)
}

func (t *commandTracker) ctrl(b byte) {
	if t.alt {
		return
	}
	var line string
	if b == '\n' {
		line = t.ed.text()
	}
	if t.ed.ctrl(b) && !t.marks {
		t.lineDone(line)
		t.ed.lines = t.ed.lines[:0] // plain mode keeps one line only
	}
}

func (t *commandTracker) csi(final byte, params string) {
	if strings.HasPrefix(params, "?") && (final == 'h' || final == 'l') {
		for _, p := range strings.Split(params[1:], ";") {
			switch p {
			case "1049", "1047", "47":
				t.alt = final == 'h'
				if t.alt {
					// A full-screen program started: lines typed so far are not commands of the shell.
					t.awaiting = t.awaiting[:0]
					t.lineActive = false
				}
			}
		}
		return
	}
	if !t.alt {
		t.ed.csi(final, params)
	}
}

func (t *commandTracker) esc(final byte) {
	if !t.alt {
		t.ed.esc(final)
	}
}

func (t *commandTracker) osc(payload string) {
	ps, pt, _ := strings.Cut(payload, ";")
	if ps != "133" && ps != "633" {
		return
	}
	if pt == "" {
		return
	}
	kind := pt[0]
	rest := ""
	if len(pt) > 2 && pt[1] == ';' {
		rest = pt[2:]
	} else if len(pt) > 1 {
		return
	}
	switch kind {
	case 'A':
		t.flushPending(nil)
		t.marks = true
		t.region = 1
		t.explicit = ""
		t.markGuest = nil
		t.markSecret = false
		t.ed.reset()
		t.awaiting = t.awaiting[:0]
		t.lineActive = false
	case 'B':
		t.marks = true
		t.region = 2
		t.bLine, t.bCol = len(t.ed.lines), t.ed.col
	case 'C':
		if t.region == 2 {
			cmd := t.explicit
			if cmd == "" {
				li := min(t.bLine, len(t.ed.lines))
				cmd = t.ed.from(li, t.bCol)
			}
			cmd = cleanCommand(cmd)
			if cmd != "" && !t.markSecret {
				t.pending = &pendingCmd{rec: CommandRecord{Time: t.now, Command: cmd, Cwd: t.cwdNow(), Source: "shell-integration", Guest: t.markGuest}}
			}
			t.markGuest = nil
			t.markSecret = false
		}
		t.region = 3
		t.explicit = ""
	case 'D':
		var code *int
		if rest != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(rest, ";", 2)[0])); err == nil {
				code = &n
			}
		}
		t.flushPending(code)
		t.region = 0
	case 'E':
		if ps == "633" {
			t.explicit = unescape633(strings.SplitN(rest, ";", 2)[0])
		}
	}
}

// flushPending emits the running command (with its exit code when known) and forgets it.
func (t *commandTracker) flushPending(code *int) {
	p := t.pending
	if p == nil {
		return
	}
	t.pending = nil
	rec := p.rec
	rec.ExitCode = code
	if code != nil {
		rec.DurationMs = t.now.Sub(rec.Time).Milliseconds()
	}
	rec.Running = false
	t.remember(rec, !p.emitted)
}

// sweep emits commands that have been running for a while without their exit code (the audit entry must not wait
// for a command that may run for days), and drops stale echo waits.
func (t *commandTracker) sweep(now time.Time) {
	if p := t.pending; p != nil && !p.emitted && now.Sub(p.rec.Time) >= pendingEmitAfter {
		p.emitted = true
		rec := p.rec
		rec.Running = true
		t.remember(rec, true)
	}
	for len(t.awaiting) > 0 && now.Sub(t.awaiting[0].at) > awaitEchoTimeout {
		t.awaiting = t.awaiting[1:]
	}
}

// lineDone runs when an output line ends (plain mode): it resolves the oldest Enter still waiting for its echo.
func (t *commandTracker) lineDone(line string) {
	if len(t.awaiting) == 0 {
		return
	}
	// The oldest Enter normally owns the line; when its input is clearly not on it but a later one's is (two people
	// typing into a shared session, or input reordered on its way to the shell), that later Enter owns it.
	i := 0
	if a0 := t.awaiting[0]; a0.certain && a0.input != "" && !strings.Contains(line, a0.input) {
		for j := 1; j < len(t.awaiting); j++ {
			if aj := t.awaiting[j]; aj.certain && aj.input != "" && strings.Contains(line, aj.input) {
				i = j
				break
			}
		}
	}
	a := t.awaiting[i]
	t.awaiting = append(t.awaiting[:i], t.awaiting[i+1:]...)
	if a.secret || isSecretPrompt(a.promptText) {
		return
	}
	runes := []rune(line)
	prompt := []rune(a.promptText)
	cmd := ""
	if a.promptCol <= len(runes) && sameRunes(runes, prompt, min(a.promptCol, len(prompt))) {
		cmd = string(runes[a.promptCol:])
		if isSecretPrompt(string(runes[:a.promptCol])) {
			return
		}
	} else if a.certain && a.input != "" && strings.Contains(line, a.input) {
		cmd = a.input
	}
	cmd = cleanCommand(cmd)
	if cmd == "" {
		return
	}
	t.remember(CommandRecord{Time: a.at, Command: cmd, Cwd: t.cwdNow(), Source: "input", Guest: a.guest}, true)
}

func sameRunes(a, b []rune, n int) bool {
	if len(a) < n || len(b) < n {
		return false
	}
	return strings.TrimRight(string(a[:n]), " ") == strings.TrimRight(string(b[:n]), " ")
}

// ---- input --------------------------------------------------------------------------------------------------------

// input follows typed keys; guest is the share viewer who sent p (nil = the owner or an API injection).
func (t *commandTracker) input(p []byte, guest *GuestRef) {
	if t.marks {
		if guest != nil && t.region != 3 {
			t.markGuest = guest // typed at the prompt: part of the next command
		}
		return
	}
	if t.alt {
		return
	}
	t.curGuest = guest
	defer func() { t.curGuest = nil }()
	s := string(p)
	for i := 0; i < len(s); {
		if len(t.inEsc) > 0 || s[i] == 0x1b {
			t.inEsc = append(t.inEsc, s[i])
			i++
			t.escKey()
			t.noteGuest()
			continue
		}
		r, size := rune(s[i]), 1
		if s[i] >= 0x80 {
			r, size = decodeRune(s[i:])
		}
		i += size
		t.key(r)
		t.noteGuest()
	}
}

// secretInput follows an injected secret (masked by term, trailing Enter kept): the line it is typed on — also when
// Enter follows as ordinary input — is never audited.
func (t *commandTracker) secretInput(masked []byte) {
	if t.marks {
		if t.region != 3 {
			t.markSecret = true // typed at the prompt: part of the next command
		}
		return
	}
	if t.alt {
		return
	}
	t.startLine()
	t.lineSecret = true
	t.input(masked, nil)
}

// noteGuest marks the line being edited as (partly) typed by the current guest.
func (t *commandTracker) noteGuest() {
	if t.curGuest != nil && t.lineActive {
		t.lineGuest = t.curGuest
	}
}

func decodeRune(s string) (rune, int) {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		n = 1
	}
	return r, n
}

// escKey consumes a complete escape sequence typed by the user (arrows, Home/End, Delete, bracketed paste).
func (t *commandTracker) escKey() {
	e := string(t.inEsc)
	if len(e) > 32 {
		t.inEsc = t.inEsc[:0]
		return
	}
	if len(e) < 2 {
		return
	}
	if e[1] != '[' && e[1] != 'O' {
		// Meta+key (Alt-b, Alt-f, Alt-d...): unknown effect on the line.
		t.inEsc = t.inEsc[:0]
		t.startLine()
		t.certain = false
		return
	}
	if len(e) < 3 {
		return
	}
	last := e[len(e)-1]
	if e[1] == '[' && (last < 0x40 || last > 0x7e) {
		return // CSI still collecting parameters
	}
	t.inEsc = t.inEsc[:0]
	switch e {
	case "\x1b[200~":
		t.startLine()
		t.pasting = true
		return
	case "\x1b[201~":
		t.pasting = false
		return
	}
	t.startLine()
	switch last {
	case 'D':
		t.inCol = max(t.inCol-1, 0)
	case 'C':
		t.inCol = min(t.inCol+1, len(t.inLine))
	case 'H':
		t.inCol = 0
	case 'F':
		t.inCol = len(t.inLine)
	case '~':
		switch e {
		case "\x1b[3~":
			if t.inCol < len(t.inLine) {
				t.inLine = append(t.inLine[:t.inCol], t.inLine[t.inCol+1:]...)
			}
		case "\x1b[1~", "\x1b[7~":
			t.inCol = 0
		case "\x1b[4~", "\x1b[8~":
			t.inCol = len(t.inLine)
		}
	case 'A', 'B': // history
		t.certain = false
	}
}

// startLine snapshots the prompt when the user starts typing a new line.
func (t *commandTracker) startLine() {
	if t.lineActive {
		return
	}
	t.lineActive = true
	t.lineGuest = nil
	t.lineSecret = false
	t.inLine, t.inCol, t.certain = t.inLine[:0], 0, true
	t.promptCol = t.ed.col
	t.promptText = t.ed.text()
}

func (t *commandTracker) key(r rune) {
	if (r == '\r' || r == '\n') && !t.pasting {
		t.enter()
		return
	}
	t.startLine()
	switch {
	case r == '\r' || r == '\n': // newline inside a bracketed paste: part of the line
		t.insert('\n')
	case r == 0x7f || r == 0x08:
		if t.inCol > 0 {
			t.inLine = append(t.inLine[:t.inCol-1], t.inLine[t.inCol:]...)
			t.inCol--
		}
	case r == 0x15: // ^U
		t.inLine, t.inCol = append(t.inLine[:0], t.inLine[t.inCol:]...), 0
	case r == 0x0b: // ^K
		t.inLine = t.inLine[:t.inCol]
	case r == 0x17: // ^W
		i := t.inCol
		for i > 0 && t.inLine[i-1] == ' ' {
			i--
		}
		for i > 0 && t.inLine[i-1] != ' ' {
			i--
		}
		t.inLine = append(t.inLine[:i], t.inLine[t.inCol:]...)
		t.inCol = i
	case r == 0x01:
		t.inCol = 0
	case r == 0x05:
		t.inCol = len(t.inLine)
	case r == 0x02:
		t.inCol = max(t.inCol-1, 0)
	case r == 0x06:
		t.inCol = min(t.inCol+1, len(t.inLine))
	case r == 0x03: // ^C: the line is abandoned
		t.lineActive = false
		t.inLine, t.inCol = t.inLine[:0], 0
	case r == 0x04:
		if t.inCol < len(t.inLine) {
			t.inLine = append(t.inLine[:t.inCol], t.inLine[t.inCol+1:]...)
		}
	case r == '\t' || r == 0x12 || r == 0x10 || r == 0x0e || r == 0x19: // completion, history search, yank
		t.certain = false
	case r < 0x20:
	default:
		t.insert(r)
	}
}

func (t *commandTracker) insert(r rune) {
	if len(t.inLine) >= maxCommandRunes {
		return
	}
	t.inLine = append(t.inLine, 0)
	copy(t.inLine[t.inCol+1:], t.inLine[t.inCol:])
	t.inLine[t.inCol] = r
	t.inCol++
}

func (t *commandTracker) enter() {
	if !t.lineActive {
		// Enter on an untouched line: still consume one echoed newline so the queue stays aligned.
		t.startLine()
	}
	if len(t.awaiting) >= maxAwaiting {
		t.awaiting = t.awaiting[1:]
	}
	guest := t.lineGuest
	if t.curGuest != nil {
		guest = t.curGuest // the guest pressed Enter
	}
	t.awaiting = append(t.awaiting, awaitCmd{input: string(t.inLine), certain: t.certain, promptCol: t.promptCol,
		promptText: t.promptText, at: t.now, guest: guest, secret: t.lineSecret})
	t.lineGuest = nil
	t.lineSecret = false
	t.lineActive = false
	t.inLine, t.inCol = t.inLine[:0], 0
}

// ---- results ------------------------------------------------------------------------------------------------------

func (t *commandTracker) cwdNow() string {
	if t.cwd == nil {
		return ""
	}
	return t.cwd()
}

// remember keeps rec in the recent list (updating the running entry of the same command) and emits it.
func (t *commandTracker) remember(rec CommandRecord, emit bool) {
	updated := false
	if n := len(t.recent); n > 0 {
		last := &t.recent[n-1]
		if last.Running && last.Command == rec.Command && last.Time.Equal(rec.Time) {
			*last = rec
			updated = true
		}
	}
	if !updated {
		if len(t.recent) >= maxRecentCommands {
			t.recent = append(t.recent[:0], t.recent[len(t.recent)-maxRecentCommands/2:]...)
		}
		t.recent = append(t.recent, rec)
	}
	if emit && t.emit != nil {
		t.emit(rec)
	}
}

// cleanCommand trims, drops control characters and bounds the length of a command line.
func cleanCommand(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == unicode.ReplacementChar {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxCommandRunes {
		s = string(r[:maxCommandRunes]) + "…"
	}
	return s
}

// unescape633 decodes the VS Code escaping of OSC 633 values (\\ and \xHH).
func unescape633(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			if s[i+1] == '\\' {
				b.WriteByte('\\')
				i++
				continue
			}
			if s[i+1] == 'x' && i+3 < len(s) {
				if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
					b.WriteByte(byte(v))
					i += 3
					continue
				}
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ---- auditor ------------------------------------------------------------------------------------------------------

// commandAuditor writes session.command audit entries from a queue (hooks must never block on the database).
type commandAuditor struct {
	s       *Service
	ch      chan auditJob
	dropped atomic.Int64

	mu       sync.Mutex
	recStart map[string]time.Time
}

type auditJob struct {
	sess *term.Session
	rec  CommandRecord
}

func newCommandAuditor(s *Service) *commandAuditor {
	return &commandAuditor{s: s, ch: make(chan auditJob, 1024), recStart: map[string]time.Time{}}
}

// enqueue queues rec for sess when the policy enables command auditing (non-blocking; drops on overload).
func (a *commandAuditor) enqueue(sess *term.Session, st *sessState, rec CommandRecord) {
	if !a.s.policy().CommandAudit {
		return
	}
	if st.limiter != nil && !st.limiter.AllowN(a.s.now(), 1) {
		a.dropped.Add(1)
		return
	}
	select {
	case a.ch <- auditJob{sess: sess, rec: rec}:
	default:
		a.dropped.Add(1)
	}
}

func (a *commandAuditor) run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			// Drain what is queued so shutdown does not lose commands (bounded by the queue size).
			for {
				select {
				case j := <-a.ch:
					a.write(j)
				default:
					return
				}
			}
		case j := <-a.ch:
			a.write(j)
		case <-tick.C:
			a.s.states.sweep(a.s.now())
			if n := a.dropped.Swap(0); n > 0 {
				a.s.log.Warn("command audit: entries dropped (rate limit or overload)", "count", n)
			}
		}
	}
}

func (a *commandAuditor) write(j auditJob) {
	info := j.sess.Info()
	d := map[string]any{
		"command":  j.rec.Command,
		"protocol": info.Protocol,
		"source":   j.rec.Source,
		"at":       j.rec.Time.UTC().Format(time.RFC3339Nano),
	}
	if info.ConnectionID != "" {
		d["connectionId"] = info.ConnectionID
	}
	if info.Host != "" {
		d["host"] = info.Host
	}
	if info.Username != "" {
		d["username"] = info.Username
	}
	if info.Title != "" {
		d["title"] = info.Title
	}
	if j.rec.Cwd != "" {
		d["cwd"] = j.rec.Cwd
	}
	if j.rec.ExitCode != nil {
		d["exitCode"] = *j.rec.ExitCode
		d["durationMs"] = j.rec.DurationMs
	}
	if j.rec.Running {
		d["running"] = true
	}
	if g := j.rec.Guest; g != nil {
		// Typed by a viewer of an interactive share link (it ran with the owner's permissions).
		guest := map[string]any{"shareId": g.ShareID, "viewerId": g.ViewerID, "ip": g.IP}
		if g.Username != "" {
			guest["username"] = g.Username
		}
		if g.Label != "" {
			guest["label"] = g.Label
		}
		d["guest"] = guest
	}
	if info.RecordingID != "" {
		if start, ok := a.recordingStart(info.RecordingID); ok {
			d["recordingId"] = info.RecordingID
			d["recordingTime"] = max(j.rec.Time.Sub(start).Seconds(), 0)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.s.d.Audit.LogUser(ctx, j.sess.Owner(), "session.command", j.sess.ID, d)
}

func (a *commandAuditor) recordingStart(id string) (time.Time, bool) {
	a.mu.Lock()
	t, ok := a.recStart[id]
	a.mu.Unlock()
	if ok {
		return t, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rec, err := a.s.d.Store.Recordings.Get(ctx, id)
	if err != nil {
		return time.Time{}, false
	}
	a.mu.Lock()
	if len(a.recStart) > 4096 {
		clear(a.recStart)
	}
	a.recStart[id] = rec.StartedAt
	a.mu.Unlock()
	return rec.StartedAt, true
}

func newCommandLimiter() *rate.Limiter { return rate.NewLimiter(rate.Limit(5), 30) }

// ---- REST ---------------------------------------------------------------------------------------------------------

// handleSessionCommands: GET /api/sessions/{id}/commands → the commands recognized in a live session (owner or
// admin; in memory, newest last).
func (s *Service) handleSessionCommands(c *echo.Context) error {
	sess, u, err := s.sessionFor(c, false)
	if err != nil {
		return err
	}
	if sess.Kind != model.KindTerminal {
		return httpx.BadRequest("not a terminal session")
	}
	if sess.OwnerID != u.ID {
		s.d.Audit.Log(c, "session.commands.view", sess.ID, map[string]any{"ownerId": sess.OwnerID})
	}
	out := []CommandRecord{}
	if st := s.states.get(sess, false); st != nil {
		out = st.commands()
	}
	return c.JSON(http.StatusOK, map[string]any{"commands": out, "auditing": s.policy().CommandAudit})
}
