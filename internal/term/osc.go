package term

import (
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// OSC scanner: a small VT escape-sequence state machine that watches the output stream for
//   - OSC 0 / OSC 2 (window title),
//   - OSC 7 (cwd as file://host/path), OSC 633;P;Cwd=, OSC 1337;CurrentDir=, OSC 9;9; (cwd variants),
//   - OSC 133 / OSC 633 A|B|C|D prompt marks (D carries the exit code),
//   - BEL outside of control strings.
// It keeps its state across calls, so sequences split across read chunks are recognized. 8-bit C1 controls are not
// interpreted (they are UTF-8 continuation bytes in a UTF-8 stream).

type oscKind uint8

const (
	oscTitle oscKind = iota + 1
	oscCwd
	oscBell
	oscMark
)

// oscEvent is something the scanner found; Offset is the absolute stream offset just past the sequence.
type oscEvent struct {
	Kind     oscKind
	Text     string // title or cwd
	Mark     byte   // 'A'..'D' for oscMark
	ExitCode *int   // OSC 133;D;<code>
	Offset   int64
}

const (
	scGround = iota
	scEsc
	scCSI
	scOSC
	scOSCEsc
	scStr    // DCS / APC / PM / SOS: ignored until ST
	scStrEsc // ESC seen inside a control string
)

// maxOSCPayload bounds the collected OSC payload; longer payloads (e.g. OSC 52 clipboard blobs) are skipped.
const (
	maxOSCPayload = 8 << 10
	maxTitleRunes = 256
	maxCwdBytes   = 4096
)

type oscScanner struct {
	state    int
	buf      []byte
	overflow bool
}

// Scan feeds data (which starts at absolute offset base) through the state machine and calls emit for every event.
func (sc *oscScanner) Scan(data []byte, base int64, emit func(oscEvent)) {
	for i := 0; i < len(data); i++ {
		b := data[i]
		switch sc.state {
		case scGround:
			switch b {
			case 0x1b:
				sc.state = scEsc
			case 0x07:
				emit(oscEvent{Kind: oscBell, Offset: base + int64(i) + 1})
			}
		case scEsc:
			sc.escape(b)
		case scCSI:
			switch {
			case b == 0x1b:
				sc.state = scEsc
			case b == 0x18 || b == 0x1a: // CAN / SUB abort
				sc.state = scGround
			case b == 0x07:
				emit(oscEvent{Kind: oscBell, Offset: base + int64(i) + 1})
			case b >= 0x40 && b <= 0x7e:
				sc.state = scGround
			}
		case scOSC:
			switch b {
			case 0x07: // BEL terminator (xterm)
				sc.dispatch(base+int64(i)+1, emit)
				sc.state = scGround
			case 0x1b:
				sc.state = scOSCEsc
			case 0x18, 0x1a:
				sc.state = scGround
			default:
				if len(sc.buf) < maxOSCPayload {
					sc.buf = append(sc.buf, b)
				} else {
					sc.overflow = true
				}
			}
		case scOSCEsc:
			if b == '\\' { // ST
				sc.dispatch(base+int64(i)+1, emit)
				sc.state = scGround
				continue
			}
			// Unterminated OSC interrupted by another escape sequence: drop it and reinterpret b.
			sc.escape(b)
		case scStr:
			switch b {
			case 0x1b:
				sc.state = scStrEsc
			case 0x18, 0x1a:
				sc.state = scGround
			}
		case scStrEsc:
			if b == '\\' {
				sc.state = scGround
				continue
			}
			sc.escape(b)
		}
	}
}

// escape handles the byte following ESC.
func (sc *oscScanner) escape(b byte) {
	switch b {
	case ']':
		sc.state = scOSC
		sc.buf = sc.buf[:0]
		sc.overflow = false
	case '[':
		sc.state = scCSI
	case 'P', '_', '^', 'X':
		sc.state = scStr
	case 0x1b:
		sc.state = scEsc
	default:
		// Two-byte escape (or an intermediate byte whose final byte is harmless ground text).
		sc.state = scGround
	}
}

func (sc *oscScanner) dispatch(offset int64, emit func(oscEvent)) {
	if sc.overflow {
		sc.buf = sc.buf[:0]
		sc.overflow = false
		return
	}
	payload := string(sc.buf)
	sc.buf = sc.buf[:0]
	ps, pt, _ := strings.Cut(payload, ";")
	switch ps {
	case "0", "2":
		emit(oscEvent{Kind: oscTitle, Text: sanitizeTitle(pt), Offset: offset})
	case "7":
		if p := parseCwdURL(pt); p != "" {
			emit(oscEvent{Kind: oscCwd, Text: p, Offset: offset})
		}
	case "133":
		if ev, ok := parseMark(pt); ok {
			ev.Offset = offset
			emit(ev)
		}
	case "633": // VS Code shell integration
		if strings.HasPrefix(pt, "P;") {
			if v, ok := strings.CutPrefix(pt[2:], "Cwd="); ok {
				if p := cleanCwd(unescape633(v)); p != "" {
					emit(oscEvent{Kind: oscCwd, Text: p, Offset: offset})
				}
			}
			return
		}
		if ev, ok := parseMark(pt); ok {
			ev.Offset = offset
			emit(ev)
		}
	case "1337": // iTerm2
		if v, ok := strings.CutPrefix(pt, "CurrentDir="); ok {
			if p := cleanCwd(v); p != "" {
				emit(oscEvent{Kind: oscCwd, Text: p, Offset: offset})
			}
		}
	case "9": // ConEmu / Windows Terminal: OSC 9;9;"<path>"
		if v, ok := strings.CutPrefix(pt, "9;"); ok {
			if p := cleanCwd(strings.Trim(v, `"`)); p != "" {
				emit(oscEvent{Kind: oscCwd, Text: p, Offset: offset})
			}
		}
	}
}

// parseMark parses the part after "133;" (or "633;"): "A", "B", "C", "D", "D;<exit>", optionally followed by ";k=v".
func parseMark(pt string) (oscEvent, bool) {
	if pt == "" {
		return oscEvent{}, false
	}
	kind := pt[0]
	if kind < 'A' || kind > 'D' || (len(pt) > 1 && pt[1] != ';') {
		return oscEvent{}, false
	}
	ev := oscEvent{Kind: oscMark, Mark: kind}
	if kind == 'D' && len(pt) > 2 {
		code, _, _ := strings.Cut(pt[2:], ";")
		if n, err := strconv.Atoi(strings.TrimSpace(code)); err == nil {
			ev.ExitCode = &n
		}
	}
	return ev, true
}

// parseCwdURL extracts the path of an OSC 7 "file://host/path" URL (percent-decoded). Bare absolute paths are
// accepted too.
func parseCwdURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "/") {
		return cleanCwd(s)
	}
	if u, err := url.Parse(s); err == nil && u.Scheme != "" {
		switch strings.ToLower(u.Scheme) {
		case "file", "kitty-shell-cwd":
			return cleanCwd(u.Path)
		}
		return ""
	}
	// Lenient fallback for URLs with unescaped characters: file://host/path.
	rest, ok := strings.CutPrefix(s, "file://")
	if !ok {
		return ""
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		p := rest[i:]
		if d, err := url.PathUnescape(p); err == nil {
			p = d
		}
		return cleanCwd(p)
	}
	return ""
}

// cleanCwd validates a working-directory path reported by the shell.
func cleanCwd(p string) string {
	if p == "" || len(p) > maxCwdBytes || !utf8.ValidString(p) {
		return ""
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	// "/C:/Users/x" (Windows file URL) → "C:/Users/x".
	if len(p) >= 4 && p[0] == '/' && p[2] == ':' && p[3] == '/' && unicode.IsLetter(rune(p[1])) {
		p = p[1:]
	}
	return p
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

// sanitizeTitle strips control characters and bounds the length of a window title.
func sanitizeTitle(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	var b strings.Builder
	n := 0
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			continue
		}
		if n >= maxTitleRunes {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}
