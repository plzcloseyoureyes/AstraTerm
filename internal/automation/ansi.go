package automation

import (
	"bytes"
	"strconv"
)

// textLog is the plain-text view of a session's output used by expect(), triggers and batch captures: completed
// lines (each ending in '\n') followed by the current line. Positions are absolute byte offsets so readers survive
// trimming. A lone carriage return followed by text rewrites the current line (progress bars, prompt redraws).
type textLog struct {
	base      int64 // absolute offset of buf[0]
	buf       []byte
	lineStart int64 // absolute offset where the current (unfinished) line starts
	max       int
}

const (
	defaultLogMax = 256 << 10
	maxLineBytes  = 16 << 10
)

func newTextLog(max int) *textLog {
	if max <= 0 {
		max = defaultLogMax
	}
	return &textLog{max: max}
}

func (l *textLog) end() int64 { return l.base + int64(len(l.buf)) }

func (l *textLog) append(p []byte) {
	if len(p) == 0 {
		return
	}
	l.buf = append(l.buf, p...)
	if l.end()-l.lineStart > maxLineBytes {
		// Pathological output without newlines: force a break so lines stay bounded.
		l.endLine()
	}
	l.trim()
}

// endLine terminates the current line and returns it (without the newline).
func (l *textLog) endLine() []byte {
	line := append([]byte(nil), l.buf[l.lineStart-l.base:]...)
	l.buf = append(l.buf, '\n')
	l.lineStart = l.end()
	l.trim()
	return line
}

// resetLine discards the current line's text (carriage return followed by new text).
func (l *textLog) resetLine() {
	l.buf = l.buf[:l.lineStart-l.base]
}

// backspace removes the last character of the current line (one UTF-8 sequence).
func (l *textLog) backspace() {
	start := int(l.lineStart - l.base)
	if len(l.buf) <= start {
		return
	}
	i := len(l.buf) - 1
	for i > start && l.buf[i]&0xc0 == 0x80 {
		i--
	}
	l.buf = l.buf[:i]
}

func (l *textLog) currentLine() []byte { return l.buf[l.lineStart-l.base:] }

// from returns the text from absolute offset pos (clamped to what is still kept).
func (l *textLog) from(pos int64) []byte {
	if pos < l.base {
		pos = l.base
	}
	if pos > l.end() {
		pos = l.end()
	}
	return l.buf[pos-l.base:]
}

// trim drops old text (at a line boundary when possible) so at most max bytes are kept.
func (l *textLog) trim() {
	if len(l.buf) <= l.max {
		return
	}
	cut := len(l.buf) - l.max/2
	if limit := int(l.lineStart - l.base); cut > limit {
		cut = limit // never cut into the current line
	}
	if i := bytes.IndexByte(l.buf[cut:], '\n'); i >= 0 && cut+i+1 <= int(l.lineStart-l.base) {
		cut += i + 1
	}
	if cut <= 0 {
		return
	}
	l.buf = append(l.buf[:0:0], l.buf[cut:]...)
	l.base += int64(cut)
}

// stripper parses terminal output (UTF-8, as produced by the session) into a textLog: escape sequences are removed,
// OSC 133 shell-integration marks and the alternate screen are tracked. It is resumable across chunk boundaries.
type stripper struct {
	state     int
	crPending bool
	param     []byte // CSI parameters (bounded)
	osc       []byte // OSC payload (bounded)
	altScreen bool

	// callbacks (optional)
	onLine   func(line []byte, start int64) // a completed line and its start offset
	onMark   func(kind byte, exit *int)     // OSC 133 A/B/C/D
	onAltScr func(on bool)
}

const (
	stGround = iota
	stEsc
	stEscInter
	stCSI
	stOSC
	stOSCEsc
	stStr
	stStrEsc
)

const maxSeqBytes = 512

func (s *stripper) feed(l *textLog, data []byte) {
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch s.state {
		case stGround:
			if c >= 0x20 && c != 0x7f {
				// Fast path: a run of printable bytes (UTF-8 continuation bytes are ≥ 0x80).
				j := i + 1
				for j < len(data) && data[j] >= 0x20 && data[j] != 0x7f {
					j++
				}
				if s.crPending {
					l.resetLine()
					s.crPending = false
				}
				l.append(data[i:j])
				i = j - 1
				continue
			}
			switch c {
			case 0x1b:
				s.state = stEsc
			case '\n':
				s.crPending = false
				start := l.lineStart
				line := l.endLine()
				if s.onLine != nil && !s.altScreen {
					s.onLine(line, start)
				}
			case '\r':
				s.crPending = true
			case '\b':
				l.backspace()
			case '\t':
				if s.crPending {
					l.resetLine()
					s.crPending = false
				}
				l.append([]byte{'\t'})
			}
		case stEsc:
			switch {
			case c == '[':
				s.state, s.param = stCSI, s.param[:0]
			case c == ']':
				s.state, s.osc = stOSC, s.osc[:0]
			case c == 'P' || c == 'X' || c == '^' || c == '_':
				s.state = stStr
			case c >= 0x20 && c <= 0x2f:
				s.state = stEscInter
			case c == 0x1b:
				// ESC ESC: stay
			default:
				s.state = stGround
			}
		case stEscInter:
			if c >= 0x30 && c <= 0x7e {
				s.state = stGround
			} else if c < 0x20 || c > 0x2f {
				s.state = stGround
			}
		case stCSI:
			if c >= 0x40 && c <= 0x7e {
				s.csiDone(c)
				s.state = stGround
			} else if c == 0x1b {
				s.state = stEsc
			} else if len(s.param) < 64 {
				s.param = append(s.param, c)
			}
		case stOSC:
			switch c {
			case 0x07:
				s.oscDone()
				s.state = stGround
			case 0x1b:
				s.state = stOSCEsc
			default:
				if len(s.osc) < maxSeqBytes {
					s.osc = append(s.osc, c)
				}
			}
		case stOSCEsc:
			if c == '\\' {
				s.oscDone()
				s.state = stGround
			} else {
				// Unterminated OSC followed by a new escape sequence.
				s.oscDone()
				s.state = stEsc
				i--
			}
		case stStr:
			if c == 0x1b {
				s.state = stStrEsc
			} else if c == 0x07 {
				s.state = stGround
			}
		case stStrEsc:
			if c == '\\' {
				s.state = stGround
			} else {
				s.state = stStr
			}
		}
	}
}

// A pending carriage return is applied lazily: "text\r" keeps the text visible until something overwrites it, so a
// prompt such as "Password: \r" remains matchable.

func (s *stripper) csiDone(final byte) {
	if final != 'h' && final != 'l' {
		return
	}
	p := s.param
	if len(p) == 0 || p[0] != '?' {
		return
	}
	on := final == 'h'
	for f := range bytes.SplitSeq(p[1:], []byte{';'}) {
		switch string(f) {
		case "1049", "1047", "47":
			if s.altScreen != on {
				s.altScreen = on
				if s.onAltScr != nil {
					s.onAltScr(on)
				}
			}
		}
	}
}

func (s *stripper) oscDone() {
	p := s.osc
	if len(p) < 5 || string(p[:4]) != "133;" {
		return
	}
	kind := p[4]
	if kind != 'A' && kind != 'B' && kind != 'C' && kind != 'D' {
		return
	}
	var exit *int
	if kind == 'D' && len(p) > 6 && p[5] == ';' {
		rest := p[6:]
		if j := bytes.IndexByte(rest, ';'); j >= 0 {
			rest = rest[:j]
		}
		if n, err := strconv.Atoi(string(rest)); err == nil {
			exit = &n
		}
	}
	if s.onMark != nil {
		s.onMark(kind, exit)
	}
}

// StripText converts a complete chunk of terminal output into plain text lines joined by '\n' (tests, batch
// captures).
func StripText(data []byte) string {
	l := newTextLog(len(data) + 1024)
	var s stripper
	s.feed(l, data)
	return string(l.from(0))
}
