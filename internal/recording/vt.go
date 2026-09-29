package recording

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// vtHandler receives the parsed elements of a terminal output stream.
type vtHandler interface {
	text(r rune)
	ctrl(b byte)
	csi(final byte, params string)
	esc(final byte)
	osc(payload string)
}

// vtScanner is a small streaming VT parser (ground text with UTF-8, C0 controls, ESC, CSI, OSC; DCS/APC/PM/SOS
// strings are skipped). It keeps its state across calls, so sequences split between chunks are handled.
type vtScanner struct {
	state    int
	params   []byte
	osc      []byte
	overflow bool
	ubuf     [utf8.UTFMax]byte
	ulen     int
	uneed    int
}

const (
	vtGround = iota
	vtEsc
	vtEscInter
	vtCSI
	vtOSC
	vtOSCEsc
	vtStr
	vtStrEsc
)

const (
	maxCSIParams = 64
	maxOSC       = 8 << 10
)

func (v *vtScanner) feed(p []byte, h vtHandler) {
	for _, b := range p {
		if v.uneed > 0 {
			if b&0xc0 == 0x80 {
				v.ubuf[v.ulen] = b
				v.ulen++
				if v.ulen == v.uneed {
					r, _ := utf8.DecodeRune(v.ubuf[:v.ulen])
					v.uneed, v.ulen = 0, 0
					h.text(r)
				}
				continue
			}
			v.uneed, v.ulen = 0, 0 // broken sequence: reprocess b
		}
		switch v.state {
		case vtGround:
			switch {
			case b == 0x1b:
				v.state = vtEsc
			case b < 0x20 || b == 0x7f:
				h.ctrl(b)
			case b < 0x80:
				h.text(rune(b))
			default:
				n := leadLen(b)
				if n < 2 {
					continue // invalid byte (e.g. a C1 control or continuation): ignore
				}
				v.ubuf[0], v.ulen, v.uneed = b, 1, n
			}
		case vtEsc:
			v.escape(b, h)
		case vtEscInter:
			if b >= 0x30 && b <= 0x7e {
				v.state = vtGround
			} else if b == 0x1b {
				v.state = vtEsc
			}
		case vtCSI:
			switch {
			case b >= 0x40 && b <= 0x7e:
				v.state = vtGround
				h.csi(b, string(v.params))
			case b == 0x1b:
				v.state = vtEsc
			case b == 0x18 || b == 0x1a:
				v.state = vtGround
			case b < 0x20:
				h.ctrl(b) // C0 controls execute inside CSI
			default:
				if len(v.params) < maxCSIParams {
					v.params = append(v.params, b)
				}
			}
		case vtOSC:
			switch b {
			case 0x07:
				v.state = vtGround
				v.dispatchOSC(h)
			case 0x1b:
				v.state = vtOSCEsc
			case 0x18, 0x1a:
				v.state = vtGround
			default:
				if len(v.osc) < maxOSC {
					v.osc = append(v.osc, b)
				} else {
					v.overflow = true
				}
			}
		case vtOSCEsc:
			if b == '\\' {
				v.state = vtGround
				v.dispatchOSC(h)
				continue
			}
			v.escape(b, h)
		case vtStr:
			switch b {
			case 0x1b:
				v.state = vtStrEsc
			case 0x18, 0x1a:
				v.state = vtGround
			}
		case vtStrEsc:
			if b == '\\' {
				v.state = vtGround
				continue
			}
			v.escape(b, h)
		}
	}
}

func (v *vtScanner) escape(b byte, h vtHandler) {
	switch {
	case b == '[':
		v.state = vtCSI
		v.params = v.params[:0]
	case b == ']':
		v.state = vtOSC
		v.osc = v.osc[:0]
		v.overflow = false
	case b == 'P' || b == '_' || b == '^' || b == 'X':
		v.state = vtStr
	case b == 0x1b:
		v.state = vtEsc
	case b >= 0x20 && b <= 0x2f:
		v.state = vtEscInter
	default:
		v.state = vtGround
		h.esc(b)
	}
}

func (v *vtScanner) dispatchOSC(h vtHandler) {
	if v.overflow {
		v.overflow = false
		v.osc = v.osc[:0]
		return
	}
	h.osc(string(v.osc))
	v.osc = v.osc[:0]
}

func leadLen(b byte) int {
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

// csiInt returns the i-th numeric CSI parameter (def when missing or 0 when def is 0).
func csiInt(params string, i, def int) int {
	params = strings.TrimLeft(params, "?>=<")
	parts := strings.Split(params, ";")
	if i >= len(parts) || parts[i] == "" {
		return def
	}
	n, err := strconv.Atoi(strings.SplitN(parts[i], ":", 2)[0])
	if err != nil || n < 0 {
		return def
	}
	if n == 0 && def > 0 {
		return def
	}
	return min(n, 100000)
}

// ---- line editor --------------------------------------------------------------------------------------------------

const (
	maxLineRunes = 8 << 10
	maxEdLines   = 64
)

// lineEd emulates one terminal line (plus, in multi mode, the lines completed since the last reset) well enough to
// read back what a shell's line editor displayed: printable characters overwrite at the cursor, CR / BS / TAB and
// the cursor-movement and erase sequences shells use (CUB, CUF, CHA, EL, DCH, ICH, ECH, CUP column, DECSC/DECRC) are
// applied. Rows (cursor up/down) are ignored.
type lineEd struct {
	lines []string
	cur   []rune
	col   int
	saved int
	multi bool
}

func (e *lineEd) reset() {
	e.lines = e.lines[:0]
	e.cur = e.cur[:0]
	e.col, e.saved = 0, 0
}

func (e *lineEd) put(r rune) {
	if e.col >= maxLineRunes {
		return
	}
	for len(e.cur) < e.col {
		e.cur = append(e.cur, ' ')
	}
	if e.col < len(e.cur) {
		e.cur[e.col] = r
	} else {
		e.cur = append(e.cur, r)
	}
	e.col++
}

// ctrl applies a C0 control; it returns true when it completed a line (LF).
func (e *lineEd) ctrl(b byte) bool {
	switch b {
	case '\r':
		e.col = 0
	case '\n', '\v', '\f':
		if e.multi {
			if len(e.lines) >= maxEdLines {
				e.lines = append(e.lines[:0], e.lines[len(e.lines)-maxEdLines/2:]...)
			}
			e.lines = append(e.lines, e.text())
		}
		e.cur = e.cur[:0]
		e.col = 0
		return true
	case '\b':
		if e.col > 0 {
			e.col--
		}
	case '\t':
		e.col = min((e.col/8+1)*8, maxLineRunes)
	}
	return false
}

func (e *lineEd) csi(final byte, params string) {
	if strings.HasPrefix(params, "?") || strings.HasPrefix(params, ">") {
		return
	}
	switch final {
	case 'D': // CUB
		e.col = max(e.col-csiInt(params, 0, 1), 0)
	case 'C': // CUF
		e.col = min(e.col+csiInt(params, 0, 1), maxLineRunes)
	case 'G', '`': // CHA / HPA
		e.col = min(csiInt(params, 0, 1)-1, maxLineRunes)
	case 'H', 'f': // CUP: keep the column part
		e.col = min(csiInt(params, 1, 1)-1, maxLineRunes)
	case 'K': // EL
		switch csiInt(params, 0, 0) {
		case 0:
			if e.col < len(e.cur) {
				e.cur = e.cur[:e.col]
			}
		case 1:
			for i := 0; i <= e.col && i < len(e.cur); i++ {
				e.cur[i] = ' '
			}
		case 2:
			e.cur = e.cur[:0]
		}
	case 'J': // ED below / all: clears the rest of the current line too
		if n := csiInt(params, 0, 0); n == 0 && e.col < len(e.cur) {
			e.cur = e.cur[:e.col]
		} else if n == 2 || n == 3 {
			e.cur = e.cur[:0]
		}
	case 'P': // DCH
		n := csiInt(params, 0, 1)
		if e.col < len(e.cur) {
			end := min(e.col+n, len(e.cur))
			e.cur = append(e.cur[:e.col], e.cur[end:]...)
		}
	case '@': // ICH
		n := min(csiInt(params, 0, 1), maxLineRunes)
		if e.col < len(e.cur) {
			ins := make([]rune, n)
			for i := range ins {
				ins[i] = ' '
			}
			rest := append(ins, e.cur[e.col:]...)
			e.cur = append(e.cur[:e.col], rest...)
			if len(e.cur) > maxLineRunes {
				e.cur = e.cur[:maxLineRunes]
			}
		}
	case 'X': // ECH
		n := csiInt(params, 0, 1)
		for i := e.col; i < e.col+n && i < len(e.cur); i++ {
			e.cur[i] = ' '
		}
	case 's':
		e.saved = e.col
	case 'u':
		e.col = e.saved
	}
}

func (e *lineEd) esc(final byte) {
	switch final {
	case '7':
		e.saved = e.col
	case '8':
		e.col = e.saved
	}
}

// text is the current line without trailing blanks.
func (e *lineEd) text() string { return strings.TrimRight(string(e.cur), " ") }

// from returns the text displayed from line index li (len(lines) = the current line), column col, to the end.
func (e *lineEd) from(li, col int) string {
	var parts []string
	for i := li; i <= len(e.lines); i++ {
		var line []rune
		if i == len(e.lines) {
			line = e.cur
		} else if i >= 0 {
			line = []rune(e.lines[i])
		}
		if i == li {
			if col >= len(line) {
				line = nil
			} else if col > 0 {
				line = line[col:]
			}
		}
		parts = append(parts, strings.TrimRight(string(line), " "))
	}
	return strings.Join(parts, "\n")
}
