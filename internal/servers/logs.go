package servers

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Log levels of server log entries.
const (
	levelDebug = "debug"
	levelInfo  = "info"
	levelWarn  = "warn"
	levelError = "error"
)

// LogEntry is one line of a server's connection / activity log (GET /api/servers/{kind}/logs, {type:'server.log'}).
type LogEntry struct {
	ID      int64     `json:"id"`
	TS      time.Time `json:"ts"`
	Level   string    `json:"level"`
	Client  string    `json:"client,omitempty"` // remote address
	User    string    `json:"user,omitempty"`
	Message string    `json:"message"`
}

const (
	logRingSize     = 2000
	logFlushEvery   = 250 * time.Millisecond
	logMaxBatch     = 500
	maxLogMessage   = 2048
	defaultLogLimit = 500
	maxLogLimit     = logRingSize
)

// logRing keeps the last logRingSize entries of one server (across restarts) and batches new entries for live
// delivery (publish is called at most every logFlushEvery with up to logMaxBatch entries).
type logRing struct {
	mu      sync.Mutex
	buf     []LogEntry
	start   int // index of the oldest entry
	n       int
	nextID  int64
	pending []LogEntry
	dropped int
	timer   *time.Timer
	publish func(entries []LogEntry, dropped int)
}

func newLogRing(publish func([]LogEntry, int)) *logRing {
	return &logRing{buf: make([]LogEntry, logRingSize), nextID: 1, publish: publish}
}

// add appends an entry (message sanitized: control characters escaped, length capped).
func (r *logRing) add(level, client, user, msg string) {
	e := LogEntry{TS: time.Now().UTC(), Level: level, Client: client, User: sanitizeLog(user, 128), Message: sanitizeLog(msg, maxLogMessage)}
	r.mu.Lock()
	e.ID = r.nextID
	r.nextID++
	idx := (r.start + r.n) % len(r.buf)
	r.buf[idx] = e
	if r.n < len(r.buf) {
		r.n++
	} else {
		r.start = (r.start + 1) % len(r.buf)
	}
	if r.publish != nil {
		if len(r.pending) >= logMaxBatch {
			r.pending = r.pending[1:]
			r.dropped++
		}
		r.pending = append(r.pending, e)
		if r.timer == nil {
			r.timer = time.AfterFunc(logFlushEvery, r.flush)
		}
	}
	r.mu.Unlock()
}

func (r *logRing) flush() {
	r.mu.Lock()
	batch, dropped := r.pending, r.dropped
	r.pending, r.dropped, r.timer = nil, 0, nil
	r.mu.Unlock()
	if len(batch) > 0 && r.publish != nil {
		r.publish(batch, dropped)
	}
}

// since returns up to limit entries with ID > after (oldest first) and the newest ID in the ring.
func (r *logRing) since(after int64, limit int) ([]LogEntry, int64) {
	if limit <= 0 {
		limit = defaultLogLimit
	}
	if limit > maxLogLimit {
		limit = maxLogLimit
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	last := r.nextID - 1
	var out []LogEntry
	for i := 0; i < r.n; i++ {
		e := r.buf[(r.start+i)%len(r.buf)]
		if e.ID > after {
			out = append(out, e)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	if out == nil {
		out = []LogEntry{}
	}
	return out, last
}

// clear drops every entry (IDs keep increasing).
func (r *logRing) clear() {
	r.mu.Lock()
	r.start, r.n = 0, 0
	r.pending, r.dropped = nil, 0
	r.mu.Unlock()
}

// sanitizeLog escapes control characters (log injection) and caps the length.
func sanitizeLog(s string, max int) string {
	clean := true
	for _, c := range s {
		if c < 0x20 || c == 0x7f || c == 0x2028 || c == 0x2029 {
			clean = false
			break
		}
	}
	if !clean {
		var b strings.Builder
		for _, c := range s {
			switch {
			case c == '\t':
				b.WriteByte(' ')
			case c < 0x20 || c == 0x7f || c == 0x2028 || c == 0x2029:
				fmt.Fprintf(&b, "\\x%02x", c)
			default:
				b.WriteRune(c)
			}
		}
		s = b.String()
	}
	return truncate(s, max)
}
