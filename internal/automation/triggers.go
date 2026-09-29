package automation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v5"
	"golang.org/x/time/rate"

	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/term"
)

// Triggers (AUTO-7). Rules live in automation_triggers and are cached per owner. When a session of the owner
// connects, the rules whose scope matches it and that have backend actions are attached to the session's output
// tap: every completed line and every changed partial line (prompts) of the ANSI-stripped output is matched (RE2),
// so triggers work without a browser. Event triggers fire on connect, on disconnect and when a command finishes
// (OSC 133 shell integration; the line the user typed the command on is never matched as output). Visual actions
// (notify, sound) reach the browser as {type:'automation.trigger'} events; "highlight" actions are applied by the
// browser's keyword highlighter.
//
// Floods and loops are prevented by a per-rule cooldown, an optional once-per-connection flag, a per-session token
// bucket, echo suppression (a line that merely echoes what the trigger itself just typed does not fire it again) and
// a loop guard: a rule whose typing actions keep re-triggering it (fired again within cooldown + loopGap of its own
// previous input, loopMaxChain times in a row) is paused on that session until it reconnects or the rule is edited.

const (
	defaultCooldown = 2 * time.Second
	minCooldown     = 250 * time.Millisecond
	maxMatchLine    = 4096
	loopGap         = 3 * time.Second
	echoWindow      = 3 * time.Second
)

// loopMaxChain is how many self-caused fires in a row the loop guard tolerates (a variable for tests).
var loopMaxChain = 40

// TriggerEvent is published to the owner when a trigger fires.
type TriggerEvent struct {
	Type         string        `json:"type"` // "automation.trigger"
	TriggerID    string        `json:"triggerId"`
	Name         string        `json:"name"`
	Event        string        `json:"event,omitempty"`
	SessionID    string        `json:"sessionId"`
	SessionTitle string        `json:"sessionTitle"`
	ConnectionID string        `json:"connectionId,omitempty"`
	Line         string        `json:"line"`
	Match        string        `json:"match"`
	ExitCode     *int          `json:"exitCode,omitempty"`
	DurationMs   int64         `json:"durationMs,omitempty"`
	Notify       *NotifyAction `json:"notify,omitempty"`
	Sound        string        `json:"sound,omitempty"`
	Logged       bool          `json:"logged,omitempty"`
	Paused       bool          `json:"paused,omitempty"` // the loop guard paused this rule's typing actions
	Stats        TriggerStats  `json:"stats"`
}

// NotifyAction is the notification part of a TriggerEvent (texts with $0…$9 expanded).
type NotifyAction struct {
	Title   string `json:"title"`
	Message string `json:"message,omitempty"`
	Level   string `json:"level"`
	Desktop bool   `json:"desktop,omitempty"`
}

type compiledTrigger struct {
	t      *Trigger
	re     *regexp.Regexp // nil = any (event triggers without a pattern)
	server bool           // has actions the backend executes or announces (everything except highlight)
	input  bool           // has actions that type into the session (send, runSnippet, runScript)
}

func (r *compiledTrigger) event() string {
	if r.t.Event == "" {
		return EventOutput
	}
	return r.t.Event
}

// needsTap: output and command triggers read the session output.
func (r *compiledTrigger) needsTap() bool {
	ev := r.event()
	return ev == EventOutput || ev == EventCommand
}

type triggerEngine struct {
	m *Module

	mu        sync.Mutex
	users     map[string][]*compiledTrigger // owner → enabled rules
	sessions  map[string]*sessionTriggers
	stats     map[string]*TriggerStats
	connected map[string]bool // sessions seen connected (a later drop fires "disconnect" triggers)
}

// fireCtx describes what made a rule fire.
type fireCtx struct {
	event    string
	line     string
	sm       []string
	message  string // default notification message (event triggers)
	exit     *int
	duration time.Duration
}

type echoRecord struct {
	text string
	at   time.Time
}

type sessionTriggers struct {
	e       *triggerEngine
	id      string
	owner   *model.User
	session *term.Session

	mu        sync.Mutex
	tap       *tap
	unsink    func()
	closed    bool
	rules     []*compiledTrigger
	last      map[string]time.Time
	onceFired map[string]bool
	limiter   *rate.Limiter
	scriptRun bool
	lastInput map[string]time.Time
	chain     map[string]int
	paused    map[string]bool
	echo      map[string]echoRecord

	actions chan func()
	stop    chan struct{}
}

func newTriggerEngine(m *Module) *triggerEngine {
	return &triggerEngine{m: m, users: map[string][]*compiledTrigger{}, sessions: map[string]*sessionTriggers{},
		stats: map[string]*TriggerStats{}, connected: map[string]bool{}}
}

func hasServerAction(t *Trigger) bool {
	for _, a := range t.Actions {
		if a.Type != ActHighlight {
			return true
		}
	}
	return false
}

func hasInputAction(t *Trigger) bool {
	for _, a := range t.Actions {
		switch a.Type {
		case ActSend, ActSnippet, ActScript:
			return true
		}
	}
	return false
}

func compileTrigger(t *Trigger) (*compiledTrigger, error) {
	ct := &compiledTrigger{t: t, server: hasServerAction(t), input: hasInputAction(t)}
	if t.Event != "" && t.Event != EventOutput && strings.TrimSpace(t.Pattern) == "" {
		return ct, nil
	}
	pat := t.Pattern
	if !t.CaseSensitive {
		pat = "(?i)" + pat
	}
	re, err := compilePattern(pat)
	if err != nil {
		return nil, err
	}
	ct.re = re
	return ct, nil
}

// loadAll fills the rule cache for every user (startup).
func (e *triggerEngine) loadAll(ctx context.Context) error {
	rows, err := e.m.repo.db.QueryContext(ctx, `SELECT DISTINCT owner_id FROM automation_triggers`)
	if err != nil {
		return err
	}
	var owners []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err == nil {
			owners = append(owners, o)
		}
	}
	rows.Close()
	for _, o := range owners {
		if err := e.reload(ctx, o); err != nil {
			return err
		}
	}
	return nil
}

// reload re-reads an owner's rules and re-attaches them to the owner's live sessions.
func (e *triggerEngine) reload(ctx context.Context, owner string) error {
	list, err := e.m.repo.listTriggers(ctx, owner)
	if err != nil {
		return err
	}
	var rules []*compiledTrigger
	live := map[string]bool{}
	for _, t := range list {
		live[t.ID] = true
		if !t.Enabled {
			continue
		}
		ct, err := compileTrigger(t)
		if err != nil {
			e.m.log.Warn("trigger has an invalid pattern", "trigger", t.ID, "err", err)
			continue
		}
		rules = append(rules, ct)
	}
	e.mu.Lock()
	for _, r := range e.users[owner] {
		if !live[r.t.ID] {
			delete(e.stats, r.t.ID) // deleted rule
		}
	}
	if len(rules) > 0 {
		e.users[owner] = rules
	} else {
		delete(e.users, owner)
	}
	e.mu.Unlock()
	if mgr := e.m.sessions(); mgr != nil {
		for _, s := range mgr.List(&model.User{ID: owner}, false) {
			if st, _ := s.State(); st == model.StateConnected && s.Kind == model.KindTerminal {
				e.attach(s)
			}
		}
	}
	return nil
}

func scopeMatches(sc TriggerScope, info model.RuntimeSession, conn *model.Connection) bool {
	if len(sc.ConnectionIDs) > 0 {
		ok := false
		for _, id := range sc.ConnectionIDs {
			if id == info.ConnectionID && id != "" {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if len(sc.Protocols) > 0 {
		ok := false
		for _, p := range sc.Protocols {
			if strings.EqualFold(p, string(info.Protocol)) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if len(sc.Tags) > 0 {
		if conn == nil || info.ConnectionID == "" {
			return false
		}
		ok := false
		for _, want := range sc.Tags {
			for _, have := range conn.Tags {
				if strings.EqualFold(want, have) {
					ok = true
				}
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func (e *triggerEngine) onState(s *term.Session, st model.SessionState) {
	switch st {
	case model.StateConnected:
		stt := e.attach(s)
		e.mu.Lock()
		e.connected[s.ID] = true
		e.mu.Unlock()
		if stt != nil {
			info := s.Info()
			stt.fireEvent(EventConnect, fireCtx{event: EventConnect, line: info.Title, message: "Connected to " + info.Title})
		}
	case model.StateConnecting:
		e.mu.Lock()
		stt := e.sessions[s.ID]
		e.mu.Unlock()
		if stt != nil {
			stt.mu.Lock()
			stt.onceFired = map[string]bool{} // "once" = once per connection
			stt.chain, stt.paused = map[string]int{}, map[string]bool{}
			stt.mu.Unlock()
		}
	case model.StateDisconnected, model.StateError:
		e.mu.Lock()
		was := e.connected[s.ID]
		delete(e.connected, s.ID)
		stt := e.sessions[s.ID]
		e.mu.Unlock()
		if was && stt != nil {
			_, msg := s.State()
			if msg == "" {
				msg = "the connection ended"
			}
			stt.fireEvent(EventDisconnect, fireCtx{event: EventDisconnect, line: msg, message: s.Info().Title + ": " + msg})
		}
	}
}

func (e *triggerEngine) onClose(s *term.Session) {
	e.mu.Lock()
	delete(e.connected, s.ID)
	e.mu.Unlock()
	e.detach(s.ID)
}

// attach (re)computes the rules that apply to a session and hooks it to the session's tap when some rule reads the
// output. It returns the session's trigger state (nil when no rule applies).
func (e *triggerEngine) attach(s *term.Session) *sessionTriggers {
	info := s.Info()
	conn := s.Connection()
	e.mu.Lock()
	var rules []*compiledTrigger
	needTap := false
	for _, r := range e.users[s.OwnerID] {
		if r.server && scopeMatches(r.t.Scope, info, conn) {
			rules = append(rules, r)
			needTap = needTap || r.needsTap()
		}
	}
	cur := e.sessions[s.ID]
	if len(rules) == 0 {
		delete(e.sessions, s.ID)
		e.mu.Unlock()
		if cur != nil {
			cur.close()
		}
		return nil
	}
	stt := cur
	if stt == nil {
		stt = &sessionTriggers{
			e: e, id: s.ID, owner: s.Owner(), session: s,
			last: map[string]time.Time{}, onceFired: map[string]bool{},
			limiter:   rate.NewLimiter(rate.Every(250*time.Millisecond), 20),
			lastInput: map[string]time.Time{}, chain: map[string]int{}, paused: map[string]bool{}, echo: map[string]echoRecord{},
			actions: make(chan func(), 64), stop: make(chan struct{}),
		}
		e.sessions[s.ID] = stt
		go stt.worker()
	}
	e.mu.Unlock()

	stt.mu.Lock()
	if stt.closed {
		stt.mu.Unlock()
		return nil
	}
	if cur != nil {
		// Edited rules start with a clean loop guard.
		stt.chain, stt.paused = map[string]int{}, map[string]bool{}
	}
	stt.rules = rules
	hasTap := stt.tap != nil
	stt.mu.Unlock()
	if needTap && !hasTap {
		t := e.m.acquireTap(s)
		unsink := t.addSink(stt.onOutput)
		stt.mu.Lock()
		if stt.closed || stt.tap != nil {
			// Closed meanwhile (session gone) or attached concurrently: undo ours.
			stt.mu.Unlock()
			unsink()
			t.release()
		} else {
			stt.tap, stt.unsink = t, unsink
			stt.mu.Unlock()
		}
	}
	return stt
}

func (e *triggerEngine) detach(id string) {
	e.mu.Lock()
	stt := e.sessions[id]
	delete(e.sessions, id)
	e.mu.Unlock()
	if stt != nil {
		stt.close()
	}
}

func (st *sessionTriggers) close() {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return
	}
	st.closed = true
	close(st.stop)
	unsink, t := st.unsink, st.tap
	st.unsink, st.tap = nil, nil
	st.mu.Unlock()
	if unsink != nil {
		unsink()
	}
	if t != nil {
		t.release()
	}
}

func (st *sessionTriggers) worker() {
	for {
		select {
		case <-st.stop:
			return
		case fn := <-st.actions:
			func() {
				defer func() {
					if r := recover(); r != nil {
						st.e.m.log.Error("trigger action panicked", "panic", r)
					}
				}()
				fn()
			}()
		}
	}
}

// onOutput receives the tap's lines and finished commands.
func (st *sessionTriggers) onOutput(lines []string, partial string, cmds []commandEvent) {
	st.mu.Lock()
	rules := st.rules
	st.mu.Unlock()
	check := func(line string) {
		if line == "" {
			return
		}
		if len(line) > maxMatchLine {
			line = truncateUTF8(line, maxMatchLine)
		}
		for _, r := range rules {
			if r.event() != EventOutput || r.re == nil {
				continue
			}
			if sm := r.re.FindStringSubmatch(line); sm != nil {
				st.fire(r, fireCtx{event: EventOutput, line: line, sm: sm})
			}
		}
	}
	for _, l := range lines {
		check(l)
	}
	check(partial)
	for _, c := range cmds {
		for _, r := range rules {
			if r.event() == EventCommand {
				st.fireCommand(r, c)
			}
		}
	}
}

func (st *sessionTriggers) fireCommand(r *compiledTrigger, c commandEvent) {
	t := r.t
	switch t.Exit {
	case ExitOK:
		if c.Exit == nil || *c.Exit != 0 {
			return
		}
	case ExitError:
		if c.Exit == nil || *c.Exit == 0 {
			return
		}
	}
	if t.MinDuration > 0 && c.Duration < time.Duration(t.MinDuration)*time.Second {
		return
	}
	sm := []string{c.Command}
	if r.re != nil {
		if sm = r.re.FindStringSubmatch(c.Command); sm == nil {
			return
		}
	}
	msg := fmt.Sprintf("“%s” finished after %s", truncateUTF8(c.Command, 120), c.Duration.Round(100*time.Millisecond))
	if c.Exit != nil {
		msg = fmt.Sprintf("“%s” exited with code %d after %s", truncateUTF8(c.Command, 120), *c.Exit, c.Duration.Round(100*time.Millisecond))
	}
	st.fire(r, fireCtx{event: EventCommand, line: c.Command, sm: sm, message: msg, exit: c.Exit, duration: c.Duration})
}

// fireEvent fires the rules of a session event (connect / disconnect).
func (st *sessionTriggers) fireEvent(event string, fc fireCtx) {
	st.mu.Lock()
	rules := st.rules
	st.mu.Unlock()
	for _, r := range rules {
		if r.event() != event {
			continue
		}
		sm := []string{fc.line}
		if r.re != nil {
			if sm = r.re.FindStringSubmatch(fc.line); sm == nil {
				continue
			}
		}
		f := fc
		f.sm = sm
		st.fire(r, f)
	}
}

// expandGroups replaces $0…$9 in s with the match groups (display texts only).
func expandGroups(s string, sm []string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '$' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
			if n := int(s[i+1] - '0'); n < len(sm) {
				b.WriteString(sm[n])
			}
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// isEcho reports whether line only echoes text this rule typed moments ago (the shell echoing our own input).
func (st *sessionTriggers) isEchoLocked(id, line string, now time.Time) bool {
	rec, ok := st.echo[id]
	if !ok || now.Sub(rec.at) > echoWindow || len(rec.text) < 2 {
		return false
	}
	return strings.Contains(line, rec.text)
}

func (st *sessionTriggers) fire(r *compiledTrigger, fc fireCtx) {
	t := r.t
	now := time.Now()
	cooldown := defaultCooldown
	if t.CooldownMs > 0 {
		cooldown = max(time.Duration(t.CooldownMs)*time.Millisecond, minCooldown)
	}
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return
	}
	if fc.event == EventOutput && st.isEchoLocked(t.ID, fc.line, now) {
		st.mu.Unlock()
		return
	}
	if last, ok := st.last[t.ID]; ok && now.Sub(last) < cooldown {
		st.mu.Unlock()
		return
	}
	if t.Once && st.onceFired[t.ID] {
		st.mu.Unlock()
		return
	}
	if !st.limiter.Allow() {
		st.mu.Unlock()
		return
	}
	st.last[t.ID] = now
	st.onceFired[t.ID] = true
	allowInput := r.input && fc.event != EventDisconnect
	justPaused := false
	if allowInput {
		if prev, ok := st.lastInput[t.ID]; ok && now.Sub(prev) <= cooldown+loopGap {
			st.chain[t.ID]++
		} else {
			st.chain[t.ID] = 1
		}
		st.lastInput[t.ID] = now
		if st.paused[t.ID] {
			allowInput = false
		} else if st.chain[t.ID] > loopMaxChain {
			st.paused[t.ID] = true
			allowInput, justPaused = false, true
		}
	}
	st.mu.Unlock()

	e := st.e
	e.mu.Lock()
	stats := e.stats[t.ID]
	if stats == nil {
		stats = &TriggerStats{}
		e.stats[t.ID] = stats
	}
	stats.Hits++
	ts := now.UTC()
	stats.LastHitAt = &ts
	snap := TriggerStats{Hits: stats.Hits, LastHitAt: &ts}
	e.mu.Unlock()

	info := st.session.Info()
	match := fc.line
	if len(fc.sm) > 0 {
		match = fc.sm[0]
	}
	ev := TriggerEvent{Type: "automation.trigger", TriggerID: t.ID, Name: t.Name, Event: fc.event, SessionID: st.id,
		SessionTitle: info.Title, ConnectionID: info.ConnectionID, Line: truncateUTF8(fc.line, 500), Match: truncateUTF8(match, 200),
		ExitCode: fc.exit, DurationMs: fc.duration.Milliseconds(), Paused: justPaused, Stats: snap}
	for _, a := range t.Actions {
		a := a
		switch a.Type {
		case ActNotify:
			level := a.Level
			switch level {
			case "info", "success", "warning", "error":
			default:
				level = "info"
			}
			title := expandGroups(a.Title, fc.sm)
			if strings.TrimSpace(title) == "" {
				title = t.Name
			}
			msg := expandGroups(a.Message, fc.sm)
			if strings.TrimSpace(msg) == "" {
				msg = fc.message
			}
			ev.Notify = &NotifyAction{Title: truncateUTF8(title, 200), Message: truncateUTF8(msg, 1000), Level: level, Desktop: a.Desktop}
		case ActSound:
			ev.Sound = a.Sound
			if ev.Sound == "" {
				ev.Sound = "beep"
			}
		case ActLog:
			ev.Logged = true
			line := fc.line
			if fc.event != EventOutput && fc.message != "" {
				line = fc.message
			}
			st.enqueue(func() { st.logLine(t, info, line) })
		case ActSend:
			if allowInput {
				st.enqueue(func() { st.send(t.ID, a) })
			}
		case ActSnippet:
			if allowInput {
				st.enqueue(func() { st.runSnippet(a.SnippetID, t) })
			}
		case ActScript:
			if allowInput {
				st.enqueue(func() { st.runScript(a.ScriptID, t) })
			}
		}
	}
	if justPaused {
		msg := fmt.Sprintf("Trigger %q kept re-triggering itself with its own input and was paused on this session (it resumes after a reconnect or when you edit it).", t.Name)
		st.session.Notice(msg)
		e.m.notify(st.owner.ID, "warning", "Trigger paused: possible loop", msg)
	}
	e.m.publish(st.owner.ID, ev)
}

func (st *sessionTriggers) enqueue(fn func()) {
	select {
	case st.actions <- fn:
	default:
		st.e.m.log.Debug("trigger action queue full, dropping action", "session", st.id)
	}
}

func (st *sessionTriggers) logLine(t *Trigger, info model.RuntimeSession, line string) {
	e := &TriggerLogEntry{TriggerID: t.ID, TriggerName: t.Name, SessionID: st.id, SessionTitle: info.Title,
		ConnectionID: info.ConnectionID, Line: truncateUTF8(line, 2000), TS: now()}
	if err := st.e.m.repo.insertTriggerLog(st.e.m.ctx, st.owner.ID, e); err != nil {
		st.e.m.log.Debug("cannot log trigger hit", "err", err)
	}
}

func (st *sessionTriggers) send(triggerID string, a TriggerAction) {
	m := st.e.m
	if a.Secret != "" {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		v, err := m.sessionSecret(ctx, st.owner, st.session, a.Secret)
		cancel()
		if err != nil {
			st.session.Notice("Trigger could not send the stored secret: " + errorText(err))
			return
		}
		if err := m.writeSecret(st.id, v, a.Enter); err != nil {
			m.log.Debug("trigger send failed", "session", st.id, "err", err)
		}
		return
	}
	text := Unescape(a.Text)
	// Remember what we typed: the shell's echo of it must not fire the rule again.
	if echo := strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, text)); echo != "" {
		st.mu.Lock()
		st.echo[triggerID] = echoRecord{text: echo, at: time.Now()}
		st.mu.Unlock()
	}
	if a.Enter {
		text += "\r"
	}
	if text == "" {
		return
	}
	if err := m.write(st.id, text); err != nil {
		m.log.Debug("trigger send failed", "session", st.id, "err", err)
	}
}

func (st *sessionTriggers) runSnippet(id string, t *Trigger) {
	m := st.e.m
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	defer cancel()
	snip, err := m.ownSnippet(ctx, st.owner, id)
	if err != nil {
		st.session.Notice("Trigger " + strconv.Quote(t.Name) + ": snippet not found")
		return
	}
	text, missing := snippetText(snip.Content, snip.SendMode, nil, sessionBuiltins(st.session.Info(), st.session.Connection(), time.Now()))
	if len(missing) > 0 {
		st.session.Notice("Trigger " + strconv.Quote(t.Name) + ": the snippet needs values for " + strings.Join(missing, ", "))
		return
	}
	if hits := checkDangerous(text, loadGuardConfig(ctx, m.d.Store, st.owner.ID)); len(hits) > 0 {
		m.notify(st.owner.ID, "warning", "Trigger blocked", "Trigger "+strconv.Quote(t.Name)+" did not run snippet "+
			strconv.Quote(snip.Name)+": "+hits[0].Message)
		return
	}
	if first := strings.TrimSpace(strings.SplitN(normalizeNewlines(text), "\r", 2)[0]); first != "" {
		st.mu.Lock()
		st.echo[t.ID] = echoRecord{text: first, at: time.Now()}
		st.mu.Unlock()
	}
	if err := m.write(st.id, text); err != nil {
		m.log.Debug("trigger snippet failed", "session", st.id, "err", err)
	}
}

func (st *sessionTriggers) runScript(id string, t *Trigger) {
	m := st.e.m
	st.mu.Lock()
	if st.scriptRun {
		st.mu.Unlock()
		return
	}
	st.scriptRun = true
	st.mu.Unlock()
	done := func() {
		st.mu.Lock()
		st.scriptRun = false
		st.mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	defer cancel()
	if !m.scriptsAllowed(ctx, st.owner) {
		done()
		return
	}
	sc, err := m.ownScript(ctx, st.owner, id)
	if err != nil {
		done()
		st.session.Notice("Trigger " + strconv.Quote(t.Name) + ": script not found")
		return
	}
	_, _, err = m.startScript(st.owner, scriptStart{script: sc, content: sc.Content, sessionID: st.id, origin: OriginTrigger,
		onDone: done})
	if err != nil {
		done()
		m.notify(st.owner.ID, "warning", "Trigger could not run a script", errorText(err))
	}
}

// statsFor returns the runtime counters of a trigger.
func (e *triggerEngine) statsFor(id string) TriggerStats {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s := e.stats[id]; s != nil {
		return TriggerStats{Hits: s.Hits, LastHitAt: s.LastHitAt}
	}
	return TriggerStats{}
}

// ---- REST ---------------------------------------------------------------------------------------------------------

type triggerInput struct {
	Name             *string          `json:"name"`
	Enabled          *bool            `json:"enabled"`
	Event            *string          `json:"event"`
	Exit             *string          `json:"exit"`
	MinDuration      *int             `json:"minDurationSec"`
	ConfirmDangerous bool             `json:"confirmDangerous"`
	Pattern          *string          `json:"pattern"`
	CaseSensitive    *bool            `json:"caseSensitive"`
	Scope            *TriggerScope    `json:"scope"`
	Actions          *[]TriggerAction `json:"actions"`
	CooldownMs       *int             `json:"cooldownMs"`
	Once             *bool            `json:"once"`
	SortOrder        *int             `json:"sortOrder"`
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// validColor accepts #rrggbb or an ANSI colour name (resolved by the browser against the terminal scheme).
func validColor(c string) bool {
	if c == "" || hexColor.MatchString(c) {
		return true
	}
	switch c {
	case "black", "red", "green", "yellow", "blue", "magenta", "cyan", "white", "brightBlack", "brightRed", "brightGreen",
		"brightYellow", "brightBlue", "brightMagenta", "brightCyan", "brightWhite":
		return true
	}
	return false
}

func (m *Module) validateActions(ctx context.Context, user *model.User, actions []TriggerAction) error {
	if len(actions) == 0 {
		return httpx.BadRequest("a trigger needs at least one action")
	}
	if len(actions) > maxTriggerActions {
		return httpx.BadRequest("too many actions")
	}
	for i, a := range actions {
		bad := func(msg string) error { return httpx.BadRequest("action " + strconv.Itoa(i+1) + ": " + msg) }
		switch a.Type {
		case ActHighlight:
			if !validColor(a.Color) || !validColor(a.Background) || a.Color == "" && a.Background == "" {
				return bad("choose a colour (#rrggbb or an ANSI colour name)")
			}
		case ActNotify:
			if utf8.RuneCountInString(a.Title) > 200 || utf8.RuneCountInString(a.Message) > 1000 {
				return bad("notification text is too long")
			}
		case ActSound:
			if len(a.Sound) > 32 {
				return bad("invalid sound")
			}
		case ActLog:
		case ActSend:
			if a.Secret != "" {
				if !validSecretKey(a.Secret) {
					return bad("invalid secret name")
				}
			} else if a.Text == "" && !a.Enter {
				return bad("nothing to send")
			}
			if len(a.Text) > 64<<10 {
				return bad("text is too long")
			}
		case ActSnippet:
			if _, err := m.ownSnippet(ctx, user, a.SnippetID); err != nil {
				return bad("snippet not found")
			}
		case ActScript:
			if _, err := m.ownScript(ctx, user, a.ScriptID); err != nil {
				return bad("script not found")
			}
			if !m.scriptsAllowed(ctx, user) {
				return errScriptsForbidden
			}
		default:
			return bad("unknown action type")
		}
	}
	return nil
}

func (in *triggerInput) apply(ctx context.Context, m *Module, user *model.User, t *Trigger) error {
	if in.Name != nil {
		n, err := cleanName(*in.Name, "name")
		if err != nil {
			return err
		}
		t.Name = n
	}
	if in.Enabled != nil {
		t.Enabled = *in.Enabled
	}
	if in.Pattern != nil {
		t.Pattern = *in.Pattern
	}
	if in.CaseSensitive != nil {
		t.CaseSensitive = *in.CaseSensitive
	}
	if in.Scope != nil {
		sc := *in.Scope
		if len(sc.ConnectionIDs) > 1000 || len(sc.Protocols) > 50 || len(sc.Tags) > 100 {
			return httpx.BadRequest("scope is too large")
		}
		for _, id := range sc.ConnectionIDs {
			if !model.ValidID(id) {
				return httpx.BadRequest("invalid connection id in scope")
			}
		}
		t.Scope = sc
	}
	if in.Actions != nil {
		if err := m.validateActions(ctx, user, *in.Actions); err != nil {
			return err
		}
		t.Actions = *in.Actions
	}
	if in.CooldownMs != nil {
		if *in.CooldownMs < 0 || *in.CooldownMs > 24*3600*1000 {
			return httpx.BadRequest("cooldownMs is out of range")
		}
		t.CooldownMs = *in.CooldownMs
	}
	if in.Once != nil {
		t.Once = *in.Once
	}
	if in.SortOrder != nil {
		t.SortOrder = *in.SortOrder
	}
	if in.Event != nil {
		switch *in.Event {
		case "", EventOutput:
			t.Event = EventOutput
		case EventConnect, EventDisconnect, EventCommand:
			t.Event = *in.Event
		default:
			return httpx.BadRequest("event must be output, connect, disconnect or command")
		}
	}
	if t.Event == "" {
		t.Event = EventOutput
	}
	if in.Exit != nil {
		switch *in.Exit {
		case "", ExitAny:
			t.Exit = ""
		case ExitOK, ExitError:
			t.Exit = *in.Exit
		default:
			return httpx.BadRequest("exit must be any, ok or error")
		}
	}
	if in.MinDuration != nil {
		if *in.MinDuration < 0 || *in.MinDuration > 7*24*3600 {
			return httpx.BadRequest("minDurationSec is out of range")
		}
		t.MinDuration = *in.MinDuration
	}
	if t.Event != EventCommand {
		t.Exit, t.MinDuration = "", 0
	}
	if t.Event == EventOutput || strings.TrimSpace(t.Pattern) != "" {
		pat := t.Pattern
		if !t.CaseSensitive {
			pat = "(?i)" + pat
		}
		if _, err := compilePattern(pat); err != nil {
			return httpx.BadRequest("invalid pattern: " + err.Error())
		}
	}
	for _, a := range t.Actions {
		if a.Type == ActHighlight && t.Event != EventOutput {
			return httpx.BadRequest("highlight actions only apply to output triggers")
		}
		if (a.Type == ActSend || a.Type == ActSnippet || a.Type == ActScript) && t.Event == EventDisconnect {
			return httpx.BadRequest("a disconnected session cannot receive input: use notify, sound or log")
		}
	}
	if in.Actions != nil && !in.ConfirmDangerous {
		var typed strings.Builder
		for _, a := range t.Actions {
			if a.Type == ActSend && a.Secret == "" {
				typed.WriteString(Unescape(a.Text))
				typed.WriteByte('\n')
			}
		}
		if hits := checkDangerous(typed.String(), loadGuardConfig(ctx, m.d.Store, user.ID)); len(hits) > 0 {
			return &dangerError{matches: hits}
		}
	}
	return nil
}

func (m *Module) ownTrigger(ctx context.Context, user *model.User, id string) (*Trigger, error) {
	if !model.ValidID(id) {
		return nil, httpx.ErrNotFound
	}
	t, err := m.repo.getTrigger(ctx, id)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, httpx.ErrNotFound
		}
		return nil, err
	}
	if t.OwnerID != user.ID {
		return nil, httpx.ErrNotFound
	}
	return t, nil
}

func (m *Module) listTriggers(c *echo.Context) error {
	list, err := m.repo.listTriggers(c.Request().Context(), httpx.UserFrom(c).ID)
	if err != nil {
		return err
	}
	for _, t := range list {
		t.Stats = m.triggers.statsFor(t.ID)
	}
	return c.JSON(http.StatusOK, list)
}

func (m *Module) createTrigger(c *echo.Context) error {
	var in triggerInput
	if err := httpx.BindLimit(c, &in, 512<<10); err != nil {
		return err
	}
	ctx, user := c.Request().Context(), httpx.UserFrom(c)
	if in.Name == nil || in.Actions == nil || (in.Pattern == nil && (in.Event == nil || *in.Event == "" || *in.Event == EventOutput)) {
		return httpx.BadRequest("name, pattern and actions are required")
	}
	list, err := m.repo.listTriggers(ctx, user.ID)
	if err != nil {
		return err
	}
	if len(list) >= maxTriggers {
		return httpx.Conflict("too many triggers")
	}
	t := &Trigger{OwnerID: user.ID, Enabled: true, Event: EventOutput, CooldownMs: int(defaultCooldown / time.Millisecond),
		SortOrder: len(list), Actions: []TriggerAction{}}
	if err := in.apply(ctx, m, user, t); err != nil {
		_, err = asDanger(c, err)
		return err
	}
	if err := m.repo.createTrigger(ctx, t); err != nil {
		return err
	}
	m.reloadTriggers(ctx, user.ID)
	return c.JSON(http.StatusCreated, t)
}

func (m *Module) patchTrigger(c *echo.Context) error {
	var in triggerInput
	if err := httpx.BindLimit(c, &in, 512<<10); err != nil {
		return err
	}
	ctx, user := c.Request().Context(), httpx.UserFrom(c)
	t, err := m.ownTrigger(ctx, user, c.Param("id"))
	if err != nil {
		return err
	}
	if err := in.apply(ctx, m, user, t); err != nil {
		_, err = asDanger(c, err)
		return err
	}
	if err := m.repo.updateTrigger(ctx, t); err != nil {
		return err
	}
	m.reloadTriggers(ctx, user.ID)
	t.Stats = m.triggers.statsFor(t.ID)
	return c.JSON(http.StatusOK, t)
}

func (m *Module) deleteTrigger(c *echo.Context) error {
	ctx, user := c.Request().Context(), httpx.UserFrom(c)
	t, err := m.ownTrigger(ctx, user, c.Param("id"))
	if err != nil {
		return err
	}
	if err := m.repo.deleteTrigger(ctx, t.ID); err != nil {
		return err
	}
	m.reloadTriggers(ctx, user.ID)
	return httpx.OK(c)
}

func (m *Module) reloadTriggers(ctx context.Context, owner string) {
	if err := m.triggers.reload(ctx, owner); err != nil {
		m.log.Warn("cannot reload triggers", "owner", owner, "err", err)
	}
}

func (m *Module) listTriggerLog(c *echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	if limit <= 0 || limit > maxTriggerLog {
		limit = 200
	}
	id := c.QueryParam("triggerId")
	if id != "" && !model.ValidID(id) {
		return httpx.BadRequest("invalid trigger id")
	}
	list, err := m.repo.listTriggerLog(c.Request().Context(), httpx.UserFrom(c).ID, id, limit)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, list)
}

func (m *Module) clearTriggerLog(c *echo.Context) error {
	if err := m.repo.clearTriggerLog(c.Request().Context(), httpx.UserFrom(c).ID); err != nil {
		return err
	}
	return httpx.OK(c)
}
