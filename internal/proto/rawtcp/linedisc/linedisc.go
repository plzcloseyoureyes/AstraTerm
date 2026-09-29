// Package linedisc is the small client-side line discipline shared by NexTerm's byte-stream terminal backends (raw
// sockets, serial ports, rlogin/rsh and telnet): translation of the Enter key to the line ending a device expects,
// and local echo for peers that do not echo typed input themselves.
//
// Local echo has to reach the terminal while the backend's Read is blocked waiting for remote data, so a Reader
// pumps the remote stream in a goroutine and merges it with locally produced output in arrival order. The pump keeps
// at most a small buffer of unread remote data, so the session's flow control (a paused reader) still throttles the
// remote peer.
package linedisc

import (
	"io"
	"strings"
	"sync"
	"unicode/utf8"
)

// Ending is what the Enter key (CR from the terminal) is translated to.
type Ending int

// Line endings.
const (
	CRLF Ending = iota
	LF
	CR
)

// ParseEnding maps an options.lineEnding value ("crlf" | "lf" | "cr") to an Ending; anything else yields def.
func ParseEnding(s string, def Ending) Ending {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "crlf":
		return CRLF
	case "lf":
		return LF
	case "cr":
		return CR
	}
	return def
}

// String returns the option value of e.
func (e Ending) String() string {
	switch e {
	case LF:
		return "lf"
	case CR:
		return "cr"
	}
	return "crlf"
}

// Translate returns p with every CR replaced by the line ending (p itself when nothing changes).
func (e Ending) Translate(p []byte) []byte {
	if e == CR {
		return p
	}
	n := 0
	for _, c := range p {
		if c == '\r' {
			n++
		}
	}
	if n == 0 {
		return p
	}
	out := make([]byte, 0, len(p)+n)
	for _, c := range p {
		if c != '\r' {
			out = append(out, c)
			continue
		}
		if e == LF {
			out = append(out, '\n')
		} else {
			out = append(out, '\r', '\n')
		}
	}
	return out
}

// Echo is the stateful local-echo renderer for typed input: printable text is echoed as is, Enter as CR LF,
// Backspace/Delete erase the previous character, and control characters and escape sequences (arrow and function
// keys) are swallowed so they do not move the local cursor. It is not safe for concurrent use.
type Echo struct {
	esc     int // escape-sequence parser state (0 = none)
	pending []byte
}

const (
	escNone = iota
	escStart
	escCSI
	escSS3
	escOSC
)

// Render returns the bytes to show locally for input p.
func (e *Echo) Render(p []byte) []byte {
	out := make([]byte, 0, len(p)+4)
	for _, c := range p {
		switch e.esc {
		case escStart:
			switch c {
			case '[':
				e.esc = escCSI
			case 'O':
				e.esc = escSS3
			case ']', 'P', '_', '^':
				e.esc = escOSC
			default:
				e.esc = escNone // two-byte sequence (Alt+key): swallow
			}
			continue
		case escCSI:
			if c >= 0x40 && c <= 0x7e {
				e.esc = escNone
			}
			continue
		case escSS3:
			e.esc = escNone
			continue
		case escOSC:
			if c == 0x07 || c == 0x9c {
				e.esc = escNone
			} else if c == 0x1b {
				e.esc = escStart // ST = ESC \
			}
			continue
		}
		switch {
		case c == 0x1b:
			e.esc = escStart
		case c == '\r' || c == '\n':
			e.pending = e.pending[:0]
			out = append(out, '\r', '\n')
		case c == 0x7f || c == 0x08:
			// Erase one character (not one byte) of what we echoed on this line; never erase remote output.
			if len(e.pending) > 0 {
				_, size := utf8.DecodeLastRune(e.pending)
				e.pending = e.pending[:len(e.pending)-size]
				out = append(out, '\b', ' ', '\b')
			}
		case c == '\t':
			out = append(out, c)
		case c < 0x20:
			// Other control characters (Ctrl+C, Ctrl+D, ...) are sent but not echoed.
		default:
			out = append(out, c)
			if len(e.pending) < 256 {
				e.pending = append(e.pending, c)
			} else {
				e.pending = append(e.pending[:0], c)
			}
		}
	}
	return out
}

// Reader merges a blocking source stream with locally injected output. Read returns bytes in arrival order: remote
// data read by the pump goroutine and bytes given to Inject. After the source fails, buffered data is returned first,
// then the source's error. Close makes Read return io.EOF; the owner must also close the source so the pump goroutine
// ends.
type Reader struct {
	src io.Reader

	mu      sync.Mutex
	cond    *sync.Cond
	buf     []byte
	err     error // sticky source error
	closed  bool
	started bool
}

// highWater bounds the remote bytes buffered ahead of the consumer (flow control stays with the session pump).
const highWater = 64 << 10

// NewReader returns a Reader pumping src. The pump starts with the first Read.
func NewReader(src io.Reader) *Reader {
	r := &Reader{src: src}
	r.cond = sync.NewCond(&r.mu)
	return r
}

func (r *Reader) pump() {
	tmp := make([]byte, 32<<10)
	for {
		n, err := r.src.Read(tmp)
		r.mu.Lock()
		if n > 0 && !r.closed {
			r.buf = append(r.buf, tmp[:n]...)
		}
		if err != nil && r.err == nil {
			r.err = err
		}
		r.cond.Broadcast()
		for !r.closed && r.err == nil && len(r.buf) >= highWater {
			r.cond.Wait()
		}
		stop := r.closed || r.err != nil
		r.mu.Unlock()
		if stop {
			return
		}
	}
}

// Read implements io.Reader.
func (r *Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started && !r.closed {
		r.started = true
		go r.pump()
	}
	for len(r.buf) == 0 && r.err == nil && !r.closed {
		r.cond.Wait()
	}
	if r.closed {
		return 0, io.EOF
	}
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		if len(r.buf) == 0 {
			r.buf = nil
		}
		r.cond.Broadcast() // the pump may be waiting below the high-water mark
		return n, nil
	}
	return 0, r.err
}

// Write queues output from a secondary stream (e.g. a separate stderr channel) like Inject, but blocks while the
// consumer lags beyond the high-water mark, so a chatty stream is throttled too. It fails with io.ErrClosedPipe after
// Close.
func (r *Reader) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for !r.closed && len(r.buf) >= highWater {
		r.cond.Wait()
	}
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	r.buf = append(r.buf, p...)
	r.cond.Broadcast()
	return len(p), nil
}

// Inject queues locally produced output (e.g. echoed input) and wakes a blocked Read.
func (r *Reader) Inject(p []byte) {
	if len(p) == 0 {
		return
	}
	r.mu.Lock()
	if !r.closed {
		r.buf = append(r.buf, p...)
		r.cond.Broadcast()
	}
	r.mu.Unlock()
}

// Close makes pending and future Reads return io.EOF.
func (r *Reader) Close() {
	r.mu.Lock()
	r.closed = true
	r.buf = nil
	r.cond.Broadcast()
	r.mu.Unlock()
}
