package automation

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Output taps: the automation features that read session output (expect in scripts, logon actions, triggers, batch
// captures, prompt waits of paced sends) share one tap per session. The term OnOutput hook runs synchronously in the
// session's output pump, so it only queues a copy of the chunk for sessions that have a tap; a per-tap goroutine
// strips escape sequences into a textLog and wakes readers. Sessions nobody watches cost one map lookup per chunk.

// outputSource is the part of *term.Session a tap needs to seed itself with recent output.
type outputSource interface {
	Offsets() (tail, head int64)
	Scrollback() []byte
}

// Errors of tap readers.
var (
	errExpectTimeout  = errors.New("timed out waiting for the expected output")
	errSessionClosed  = errors.New("the session was closed")
	errSessionDown    = errors.New("the session is disconnected")
	errSessionRestart = errors.New("the session reconnected")
)

const (
	maxTapQueue = 8 << 20
	seedBytes   = 16 << 10
)

type tapHub struct {
	mu   sync.Mutex
	taps map[string]*tap
}

func newTapHub() *tapHub { return &tapHub{taps: map[string]*tap{}} }

type tapChunk struct {
	start int64 // raw offset of data[0] in the session ring (-1 = unknown)
	data  []byte
}

// lineSink receives completed lines, the current partial line when it changed ("" = unchanged) and the commands
// that finished (OSC 133 shell integration).
type lineSink func(lines []string, partial string, cmds []commandEvent)

// commandEvent is a command that finished at a shell with OSC 133 integration (B = input starts, C = executing,
// D;exit = finished).
type commandEvent struct {
	Command  string
	Exit     *int
	Duration time.Duration
}

type tap struct {
	hub  *tapHub
	id   string
	refs int // guarded by hub.mu

	qmu     sync.Mutex
	queue   []tapChunk
	queued  int
	dropped bool
	wake    chan struct{}
	stop    chan struct{}

	mu           sync.Mutex
	log          *textLog
	strip        stripper
	fed          int64 // raw ring offset consumed so far (-1 = unknown)
	changed      chan struct{}
	lastOut      time.Time
	promptSeq    int64
	lastExit     *int
	gen          int64
	state        model.SessionState
	closed       bool
	sinks        map[int]lineSink
	sinkSeq      int
	pending      []string // lines collected while feeding (under mu)
	partialSent  string
	partialStart int64

	// OSC 133 command tracking: between B and the end of that line the user types a command (its echo is not
	// output, triggers skip it); the command finishes with D.
	inInput      bool
	cmdLineStart int64
	cmdCol       int64
	cmdText      string
	cmdAt        time.Time
	cmdSeen      bool
	pendingCmds  []commandEvent
}

// get returns the tap of a session (nil when none).
func (h *tapHub) get(id string) *tap {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.taps[id]
}

// acquire returns the session's tap, creating (and seeding) it when needed. Call release when done.
func (h *tapHub) acquire(id string, src outputSource, state model.SessionState) *tap {
	h.mu.Lock()
	if t := h.taps[id]; t != nil {
		t.refs++
		h.mu.Unlock()
		return t
	}
	t := &tap{
		hub:     h,
		id:      id,
		refs:    1,
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		log:     newTextLog(defaultLogMax),
		fed:     -1,
		changed: make(chan struct{}),
		lastOut: time.Now(),
		gen:     1,
		state:   state,
		sinks:   map[int]lineSink{},
	}
	t.strip.onLine = t.collectLine
	t.strip.onMark = t.onMark
	h.taps[id] = t
	h.mu.Unlock()
	// Seed before the worker starts: queued chunks are then de-duplicated against the seed by ring offset.
	t.seed(src)
	go t.run()
	return t
}

// release drops one reference; the last one stops the tap.
func (t *tap) release() {
	h := t.hub
	h.mu.Lock()
	t.refs--
	last := t.refs <= 0
	if last && h.taps[t.id] == t {
		delete(h.taps, t.id)
	}
	h.mu.Unlock()
	if last {
		close(t.stop)
	}
}

// push queues output (called from the session pump through the OnOutput hook: never blocks).
func (t *tap) push(start int64, data []byte) {
	if len(data) == 0 {
		return
	}
	cp := append([]byte(nil), data...)
	t.qmu.Lock()
	for len(t.queue) > 0 && t.queued+len(cp) > maxTapQueue {
		t.queued -= len(t.queue[0].data)
		t.queue[0] = tapChunk{}
		t.queue = t.queue[1:]
		t.dropped = true
	}
	t.queue = append(t.queue, tapChunk{start: start, data: cp})
	t.queued += len(cp)
	t.qmu.Unlock()
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *tap) seed(src outputSource) {
	if src == nil {
		return
	}
	var data []byte
	var end int64 = -1
	for range 3 {
		_, h0 := src.Offsets()
		snap := src.Scrollback()
		_, h1 := src.Offsets()
		if h0 == h1 {
			data, end = snap, h1
			break
		}
		data = snap
	}
	if len(data) > seedBytes {
		data = data[len(data)-seedBytes:]
		// Resynchronize on a line boundary (we may have cut an escape sequence or a UTF-8 character).
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	t.mu.Lock()
	// Seeded history is not reported to line sinks (triggers only see new output).
	onLine, onMark := t.strip.onLine, t.strip.onMark
	t.strip.onLine, t.strip.onMark = nil, nil
	t.strip.feed(t.log, data)
	t.strip.onLine, t.strip.onMark = onLine, onMark
	t.partialSent = string(t.log.currentLine())
	t.partialStart = t.log.lineStart
	t.fed = end
	t.mu.Unlock()
}

func (t *tap) run() {
	for {
		select {
		case <-t.stop:
			return
		case <-t.wake:
		}
		for {
			t.qmu.Lock()
			items, dropped := t.queue, t.dropped
			t.queue, t.queued, t.dropped = nil, 0, false
			t.qmu.Unlock()
			if len(items) == 0 {
				break
			}
			t.process(items, dropped)
		}
	}
}

func (t *tap) collectLine(line []byte, start int64) {
	if t.inInput {
		// The line the user typed a command on (prompt + echo): remember the command, never report it as output.
		t.inInput = false
		if start == t.cmdLineStart && int64(len(line)) >= t.cmdCol {
			cmd := strings.TrimSpace(string(line[t.cmdCol:]))
			t.cmdText, t.cmdSeen, t.cmdAt = truncateUTF8(cmd, 1000), cmd != "", time.Now()
		}
		t.partialSent, t.partialStart = "", -1
		return
	}
	if start == t.partialStart && string(line) == t.partialSent {
		// Already reported as a partial line (e.g. a prompt later terminated by Enter).
		t.partialSent, t.partialStart = "", -1
		return
	}
	t.pending = append(t.pending, string(line))
}

func (t *tap) onMark(kind byte, exit *int) {
	switch kind {
	case 'A':
		t.promptSeq++
		t.inInput = false
	case 'B':
		t.inInput = true
		t.cmdLineStart = t.log.lineStart
		t.cmdCol = t.log.end() - t.log.lineStart
	case 'C':
		if t.cmdSeen {
			t.cmdAt = time.Now() // execution starts now (the echo line may have ended earlier)
		}
	case 'D':
		t.lastExit = exit
		t.inInput = false
		if t.cmdSeen {
			t.cmdSeen = false
			if len(t.pendingCmds) < 64 {
				t.pendingCmds = append(t.pendingCmds, commandEvent{Command: t.cmdText, Exit: exit, Duration: time.Since(t.cmdAt)})
			}
		}
	}
}

func (t *tap) process(items []tapChunk, dropped bool) {
	t.mu.Lock()
	if dropped {
		t.fed = -1
	}
	for _, it := range items {
		data := it.data
		if it.start >= 0 && t.fed >= 0 {
			if it.start+int64(len(data)) <= t.fed {
				continue // part of the seed
			}
			if it.start < t.fed {
				data = data[t.fed-it.start:]
			}
		}
		if it.start >= 0 {
			t.fed = it.start + int64(len(it.data))
		}
		t.strip.feed(t.log, data)
	}
	t.lastOut = time.Now()
	lines, cmds := t.pending, t.pendingCmds
	t.pending, t.pendingCmds = nil, nil
	partial := ""
	if cur := string(t.log.currentLine()); cur != "" && !t.strip.altScreen && !t.inInput && (cur != t.partialSent || t.log.lineStart != t.partialStart) {
		t.partialSent, t.partialStart = cur, t.log.lineStart
		partial = cur
	}
	var sinks []lineSink
	if len(t.sinks) > 0 && (len(lines) > 0 || partial != "" || len(cmds) > 0) {
		sinks = make([]lineSink, 0, len(t.sinks))
		for _, s := range t.sinks {
			sinks = append(sinks, s)
		}
	}
	t.broadcastLocked()
	t.mu.Unlock()
	for _, s := range sinks {
		s(lines, partial, cmds)
	}
}

// feedDirect processes data synchronously (unit tests).
func (t *tap) feedDirect(data []byte) {
	t.process([]tapChunk{{start: -1, data: data}}, false)
}

func (t *tap) broadcastLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}

// addSink registers a line consumer (triggers); the returned func removes it.
func (t *tap) addSink(fn lineSink) func() {
	t.mu.Lock()
	t.sinkSeq++
	id := t.sinkSeq
	t.sinks[id] = fn
	t.mu.Unlock()
	return sync.OnceFunc(func() {
		t.mu.Lock()
		delete(t.sinks, id)
		t.mu.Unlock()
	})
}

func (t *tap) setState(st model.SessionState) {
	t.mu.Lock()
	if st == model.StateConnecting && t.state != model.StateConnecting && t.state != model.StateAuthenticating {
		t.gen++
	}
	t.state = st
	if st == model.StateClosed {
		t.closed = true
	}
	t.broadcastLocked()
	t.mu.Unlock()
}

func (t *tap) generation() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gen
}

// ---- readers ------------------------------------------------------------------------------------------------------

// reader is an expect cursor into a tap's text.
type reader struct {
	t   *tap
	pos int64
	gen int64 // when non-zero, a new connection generation fails the reader (logon actions)
}

// newReader starts at the end of the text (only new output) or, with lineStart, at the start of the current line
// (so a prompt that is already displayed can be matched).
func (t *tap) newReader(lineStart bool) *reader {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := &reader{t: t, pos: t.log.end()}
	if lineStart {
		r.pos = t.log.lineStart
	}
	return r
}

func (r *reader) bindGeneration() {
	r.gen = r.t.generation()
}

// mark returns the current position (for text captures).
func (r *reader) mark() int64 {
	r.t.mu.Lock()
	defer r.t.mu.Unlock()
	return r.t.log.end()
}

// textSince returns the text from pos to the end.
func (r *reader) textSince(pos int64) string {
	r.t.mu.Lock()
	defer r.t.mu.Unlock()
	return string(r.t.log.from(pos))
}

// skip moves the reader to the end of the current text.
func (r *reader) skip() {
	r.t.mu.Lock()
	r.pos = r.t.log.end()
	r.t.mu.Unlock()
}

// checkLocked reports why a wait must end (session closed / down / reconnected), or nil.
func (r *reader) checkLocked(allowDown bool) error {
	t := r.t
	switch {
	case t.closed:
		return errSessionClosed
	case r.gen != 0 && t.gen != r.gen:
		return errSessionRestart
	case !allowDown && (t.state == model.StateDisconnected || t.state == model.StateError):
		return errSessionDown
	}
	return nil
}

// ExpectResult is a successful expect() match.
type ExpectResult struct {
	Index  int      // which pattern matched (expectAny)
	Match  string   // the whole match
	Groups []string // capture groups
	Before string   // text between the previous position and the match
}

// expect waits until one of res matches the text after the reader's position, then moves past the match.
func (r *reader) expect(ctx context.Context, res []*regexp.Regexp, timeout time.Duration) (*ExpectResult, error) {
	var timer <-chan time.Time
	if timeout > 0 {
		tm := time.NewTimer(timeout)
		defer tm.Stop()
		timer = tm.C
	}
	t := r.t
	for {
		t.mu.Lock()
		if r.pos < t.log.base {
			r.pos = t.log.base
		}
		if r.pos > t.log.end() {
			// The current line was rewritten below our position.
			r.pos = t.log.lineStart
		}
		text := t.log.from(r.pos)
		best, bestLoc := -1, []int(nil)
		for i, re := range res {
			if loc := re.FindSubmatchIndex(text); loc != nil && (bestLoc == nil || loc[0] < bestLoc[0]) {
				best, bestLoc = i, loc
			}
		}
		if bestLoc != nil {
			res := &ExpectResult{Index: best, Match: string(text[bestLoc[0]:bestLoc[1]]), Before: string(text[:bestLoc[0]])}
			for g := 2; g+1 < len(bestLoc); g += 2 {
				if bestLoc[g] >= 0 {
					res.Groups = append(res.Groups, string(text[bestLoc[g]:bestLoc[g+1]]))
				} else {
					res.Groups = append(res.Groups, "")
				}
			}
			r.pos += int64(bestLoc[1])
			t.mu.Unlock()
			return res, nil
		}
		if err := r.checkLocked(false); err != nil {
			t.mu.Unlock()
			return nil, err
		}
		ch := t.changed
		t.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer:
			return nil, errExpectTimeout
		}
	}
}

// waitIdle waits until no output arrived for idle (at most max).
func (r *reader) waitIdle(ctx context.Context, idle, max time.Duration) error {
	deadline := time.Now().Add(max)
	t := r.t
	for {
		t.mu.Lock()
		since := time.Since(t.lastOut)
		err := r.checkLocked(true)
		ch := t.changed
		t.mu.Unlock()
		if err != nil {
			return err
		}
		if since >= idle {
			return nil
		}
		wait := idle - since
		if left := time.Until(deadline); left <= 0 {
			return nil
		} else if wait > left {
			wait = left
		}
		tm := time.NewTimer(wait)
		select {
		case <-ch:
		case <-tm.C:
		case <-ctx.Done():
			tm.Stop()
			return ctx.Err()
		}
		tm.Stop()
	}
}

// promptSeq returns the number of OSC 133 prompt marks seen so far.
func (t *tap) promptCount() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.promptSeq
}

// waitPrompt waits for the shell to be ready for the next line: a new OSC 133 prompt mark (shell integration) or,
// once output has settled for settle, a current line matching promptRe. It returns false on timeout.
func (r *reader) waitPrompt(ctx context.Context, since int64, promptRe *regexp.Regexp, settle, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	t := r.t
	for {
		t.mu.Lock()
		if err := r.checkLocked(false); err != nil {
			t.mu.Unlock()
			return false, err
		}
		if t.promptSeq > since {
			t.mu.Unlock()
			return true, nil
		}
		quiet := time.Since(t.lastOut)
		ok := promptRe != nil && quiet >= settle && promptRe.Match(t.log.currentLine())
		ch := t.changed
		t.mu.Unlock()
		if ok {
			return true, nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false, nil
		}
		wait := left
		if promptRe != nil && quiet < settle && settle-quiet < wait {
			wait = settle - quiet
		}
		tm := time.NewTimer(wait)
		select {
		case <-ch:
		case <-tm.C:
		case <-ctx.Done():
			tm.Stop()
			return false, ctx.Err()
		}
		tm.Stop()
	}
}

// waitState waits until the session state satisfies ok (e.g. connected), failing on closed.
func (t *tap) waitState(ctx context.Context, timeout time.Duration, ok func(model.SessionState) bool, fail func(model.SessionState) bool) error {
	tm := time.NewTimer(timeout)
	defer tm.Stop()
	for {
		t.mu.Lock()
		st, closed, ch := t.state, t.closed, t.changed
		t.mu.Unlock()
		switch {
		case ok(st):
			return nil
		case closed:
			return errSessionClosed
		case fail != nil && fail(st):
			return errSessionDown
		}
		select {
		case <-ch:
		case <-tm.C:
			return errExpectTimeout
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// screen returns the last n lines of text (current line included).
func (t *tap) screen(n int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	text := t.log.from(0)
	if n <= 0 {
		return string(text)
	}
	i := len(text)
	for c := 0; i > 0; i-- {
		if text[i-1] == '\n' {
			c++
			if c >= n {
				break
			}
		}
	}
	return string(text[i:])
}
