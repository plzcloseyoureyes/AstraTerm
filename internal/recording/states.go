package recording

import (
	"bytes"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/nexterm/nexterm/internal/term"
)

// sessState is this module's per-session bookkeeping, fed by the term hooks: the output timing index (instant
// replay), the command tracker (command audit) and the input byte counter (admin monitoring).
type sessState struct {
	mu       sync.Mutex
	sess     *term.Session
	tl       timeline
	cmd      *commandTracker
	bytesIn  int64
	limiter  *rate.Limiter
	lastSeen time.Time
	// guestQ holds the input chunks share guests are writing right now (see expectGuest), so the input hook can tell
	// a guest's keystrokes from the owner's.
	guestQ []*guestChunk
}

// guestChunk is one pending input write of a share guest.
type guestChunk struct {
	data []byte
	who  *GuestRef
}

type sessionStates struct {
	mu sync.RWMutex
	m  map[string]*sessState
}

func newSessionStates() *sessionStates { return &sessionStates{m: map[string]*sessState{}} }

// get returns the state of sess, creating it when create is set.
func (ss *sessionStates) get(sess *term.Session, create bool) *sessState {
	ss.mu.RLock()
	st := ss.m[sess.ID]
	ss.mu.RUnlock()
	if st != nil || !create {
		return st
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if st = ss.m[sess.ID]; st == nil {
		st = &sessState{sess: sess, cmd: newCommandTracker(), limiter: newCommandLimiter()}
		st.cmd.cwd = sess.Cwd
		ss.m[sess.ID] = st
	}
	return st
}

func (ss *sessionStates) lookup(id string) *sessState {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.m[id]
}

// drop forgets a closed session, emitting its still running command first.
func (ss *sessionStates) drop(id string) {
	ss.mu.Lock()
	st := ss.m[id]
	delete(ss.m, id)
	ss.mu.Unlock()
	if st != nil {
		st.mu.Lock()
		st.cmd.now = time.Now()
		if p := st.cmd.pending; p != nil && !p.emitted {
			st.cmd.flushPending(nil)
		}
		st.mu.Unlock()
	}
}

func (ss *sessionStates) all() []*sessState {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	out := make([]*sessState, 0, len(ss.m))
	for _, st := range ss.m {
		out = append(out, st)
	}
	return out
}

// sweep runs the trackers' timers (running commands without exit code, stale echo waits).
func (ss *sessionStates) sweep(now time.Time) {
	for _, st := range ss.all() {
		st.mu.Lock()
		st.cmd.now = now
		st.cmd.sweep(now)
		st.mu.Unlock()
	}
}

func (st *sessState) output(sess *term.Session, data []byte, now time.Time, a *commandAuditor) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.lastSeen = now
	st.tl.add(sess, data, now)
	st.cmd.now = now
	st.cmd.emit = func(rec CommandRecord) { a.enqueue(sess, st, rec) }
	st.cmd.output(data)
}

// input feeds typed input to the command tracker; sensitive input is an injected secret, already masked by term.
func (st *sessState) input(sess *term.Session, data []byte, now time.Time, a *commandAuditor, sensitive bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.bytesIn += int64(len(data))
	st.cmd.now = now
	st.cmd.emit = func(rec CommandRecord) { a.enqueue(sess, st, rec) }
	if sensitive {
		st.cmd.secretInput(data)
		return
	}
	var guest *GuestRef
	for i, g := range st.guestQ {
		if bytes.Equal(g.data, data) {
			guest = g.who
			st.guestQ = append(st.guestQ[:i], st.guestQ[i+1:]...)
			break
		}
	}
	st.cmd.input(data, guest)
}

// expectGuest announces that a share guest is about to write data (term runs the input hooks synchronously inside
// Manager.Write, so the chunk is matched by content there). The returned handle must be passed to forgetGuest after
// the write, which drops it when the write failed before reaching the hooks.
func (st *sessState) expectGuest(data []byte, who *GuestRef) *guestChunk {
	g := &guestChunk{data: data, who: who}
	st.mu.Lock()
	if len(st.guestQ) >= 64 {
		st.guestQ = st.guestQ[1:]
	}
	st.guestQ = append(st.guestQ, g)
	st.mu.Unlock()
	return g
}

func (st *sessState) forgetGuest(g *guestChunk) {
	st.mu.Lock()
	for i, x := range st.guestQ {
		if x == g {
			st.guestQ = append(st.guestQ[:i], st.guestQ[i+1:]...)
			break
		}
	}
	st.mu.Unlock()
}

func (st *sessState) resetCommands() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.cmd.reset()
	st.cmd.cwd = st.sess.Cwd
}

func (st *sessState) commands() []CommandRecord {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]CommandRecord(nil), st.cmd.recent...)
}

func (st *sessState) inputBytes() int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.bytesIn
}

// ---- timing index -------------------------------------------------------------------------------------------------

// tlPoint says: the output byte at stream offset off arrived at t, while the terminal was cols × rows.
type tlPoint struct {
	off        int64
	t          time.Time
	cols, rows int
}

const (
	tlMinGap    = 40 * time.Millisecond
	tlMaxPoints = 20000
)

// timeline indexes output arrival times by stream offset (the offsets of the session's ring buffer), at most one
// point per tlMinGap, so a replay of the scrollback can be given its real timing.
type timeline struct {
	init   bool
	off    int64
	points []tlPoint
}

func (tl *timeline) add(sess *term.Session, data []byte, now time.Time) {
	if !tl.init {
		tl.init = true
		_, head := sess.Offsets()
		tl.off = max(head-int64(len(data)), 0)
	}
	n := len(tl.points)
	if n == 0 || now.Sub(tl.points[n-1].t) >= tlMinGap {
		cols, rows := sess.Size()
		if n >= tlMaxPoints {
			tl.points = append(tl.points[:0], tl.points[n-tlMaxPoints/2:]...)
		}
		tl.points = append(tl.points, tlPoint{off: tl.off, t: now, cols: cols, rows: rows})
	}
	tl.off += int64(len(data))
}

// snapshot copies the points.
func (st *sessState) timelinePoints() []tlPoint {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]tlPoint(nil), st.tl.points...)
}
