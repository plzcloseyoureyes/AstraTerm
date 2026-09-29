package term

import (
	"bufio"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi/parser"
)

// ---- streaming ANSI stripper --------------------------------------------------------------------------------------

// ansiStripper removes escape sequences from a byte stream using the x/ansi VT parser transition table. Unlike
// ansi.Strip it keeps its state between calls, so sequences split across chunks are handled.
type ansiStripper struct {
	state parser.State
	rbuf  [utf8.UTFMax]byte
	rlen  int // bytes collected for the current rune
	rneed int // total bytes of the current rune
}

// Feed calls text for every printable rune and control for every executed C0 control byte (\r, \n, \b, \t, ...).
func (a *ansiStripper) Feed(p []byte, text func(r rune), control func(b byte)) {
	for _, b := range p {
		if a.state == parser.Utf8State {
			if b&0xc0 != 0x80 { // broken sequence: drop it and reprocess b in ground state
				a.state = parser.GroundState
				a.rlen = 0
			} else {
				a.rbuf[a.rlen] = b
				a.rlen++
				if a.rlen < a.rneed {
					continue
				}
				r, _ := utf8.DecodeRune(a.rbuf[:a.rlen])
				text(r)
				a.state, a.rlen = parser.GroundState, 0
				continue
			}
		}
		next, action := parser.Table.Transition(a.state, b)
		switch action {
		case parser.CollectAction:
			if next == parser.Utf8State {
				n := utf8LeadLen(b)
				if n < 2 {
					next = parser.GroundState
					break
				}
				a.rbuf[0], a.rlen, a.rneed = b, 1, n
			}
		case parser.PrintAction:
			text(rune(b))
		case parser.ExecuteAction:
			control(b)
		}
		a.state = next
	}
}

func utf8LeadLen(b byte) int {
	switch {
	case b&0xe0 == 0xc0:
		return 2
	case b&0xf0 == 0xe0:
		return 3
	case b&0xf8 == 0xf0:
		return 4
	}
	return 0
}

// ---- line assembly ------------------------------------------------------------------------------------------------

// lineAssembler turns stripped runes and controls into lines, emulating carriage return (overwrite from column 0),
// backspace and tabs so progress bars and shell line editing produce readable logs.
type lineAssembler struct {
	line    []rune
	col     int
	started time.Time // arrival time of the first character of the current line
	emit    func(line string, at time.Time)
	now     func() time.Time
}

const maxLineRunes = 16 << 10

func (l *lineAssembler) text(r rune) {
	if l.started.IsZero() {
		l.started = l.now()
	}
	if l.col < len(l.line) {
		l.line[l.col] = r
	} else {
		for len(l.line) < l.col {
			l.line = append(l.line, ' ')
		}
		l.line = append(l.line, r)
	}
	l.col++
	if len(l.line) >= maxLineRunes {
		l.flush()
	}
}

func (l *lineAssembler) control(b byte) {
	switch b {
	case '\n':
		if l.started.IsZero() {
			l.started = l.now()
		}
		l.flush()
	case '\r':
		l.col = 0
	case '\b':
		if l.col > 0 {
			l.col--
		}
	case '\t':
		l.col = (l.col/8 + 1) * 8
	}
}

func (l *lineAssembler) flush() {
	s := strings.TrimRight(string(l.line), " ")
	at := l.started
	if at.IsZero() {
		at = l.now()
	}
	l.line, l.col, l.started = l.line[:0], 0, time.Time{}
	l.emit(s, at)
}

// pending reports whether a partial line is buffered.
func (l *lineAssembler) pending() bool { return len(l.line) > 0 }

// StripText converts raw terminal output into plain text (escape sequences removed, CR/BS/TAB applied).
func StripText(p []byte) string {
	var b strings.Builder
	la := &lineAssembler{now: time.Now, emit: func(line string, _ time.Time) {
		b.WriteString(line)
		b.WriteByte('\n')
	}}
	var st ansiStripper
	st.Feed(p, la.text, la.control)
	if la.pending() {
		la.emit(strings.TrimRight(string(la.line), " "), time.Time{})
	}
	return b.String()
}

// ---- text logger (REC-1) ------------------------------------------------------------------------------------------

// textLogger writes an ANSI-stripped transcript of a session to a file, optionally prefixing every line with its
// arrival time.
type textLogger struct {
	mu         sync.Mutex
	f          *os.File
	w          *bufio.Writer
	strip      ansiStripper
	lines      lineAssembler
	timestamps bool
	size       int64
	err        error
	closed     bool
}

const logTimeFormat = "2006-01-02 15:04:05.000"

func newTextLogger(path, header string, timestamps bool) (*textLogger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	l := &textLogger{f: f, w: bufio.NewWriterSize(f, 32<<10), timestamps: timestamps}
	l.lines = lineAssembler{now: time.Now, emit: l.writeLine}
	if header != "" {
		l.writeRaw(header + "\n")
	}
	return l, nil
}

func (l *textLogger) writeRaw(s string) {
	if l.err != nil {
		return
	}
	n, err := l.w.WriteString(s)
	l.size += int64(n)
	l.err = err
}

func (l *textLogger) writeLine(line string, at time.Time) {
	if l.timestamps {
		l.writeRaw("[" + at.Format(logTimeFormat) + "] ")
	}
	l.writeRaw(line)
	l.writeRaw("\n")
}

// Write feeds session output.
func (l *textLogger) Write(p []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.strip.Feed(p, l.lines.text, l.lines.control)
}

// Note writes an out-of-band line (e.g. "session disconnected").
func (l *textLogger) Note(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	if l.lines.pending() {
		l.lines.flush()
	}
	l.writeLine(text, time.Now())
}

// Flush pushes buffered data to the file.
func (l *textLogger) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed && l.err == nil {
		l.err = l.w.Flush()
	}
}

// Close flushes the partial line and closes the file; it returns the file size.
func (l *textLogger) Close() (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return l.size, l.err
	}
	l.closed = true
	if l.lines.pending() {
		l.lines.flush()
	}
	if err := l.w.Flush(); err != nil && l.err == nil {
		l.err = err
	}
	if err := l.f.Close(); err != nil && l.err == nil {
		l.err = err
	}
	return l.size, l.err
}
