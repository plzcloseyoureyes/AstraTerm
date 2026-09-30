package automation

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
)

// cleanName trims a display name and rejects empty / oversized / control-character names.
func cleanName(s, what string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", httpx.BadRequest(what + " is required")
	}
	if utf8.RuneCountInString(s) > maxNameLen {
		return "", httpx.BadRequest(what + " is too long")
	}
	if strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", httpx.BadRequest(what + " must not contain control characters")
	}
	return s, nil
}

// cleanFolder normalizes a snippet folder path: "a / b//c/" → "a/b/c".
func cleanFolder(s string) (string, error) {
	var parts []string
	for p := range strings.SplitSeq(strings.ReplaceAll(s, "\\", "/"), "/") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	out := strings.Join(parts, "/")
	if utf8.RuneCountInString(out) > maxNameLen {
		return "", httpx.BadRequest("folder is too long")
	}
	if strings.ContainsFunc(out, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", httpx.BadRequest("folder must not contain control characters")
	}
	return out, nil
}

// cleanTags trims, de-duplicates (case-insensitively) and bounds a tag list.
func cleanTags(in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if utf8.RuneCountInString(t) > maxTagLen {
			return nil, httpx.BadRequest("tag is too long")
		}
		k := strings.ToLower(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, t)
	}
	if len(out) > maxTags {
		return nil, httpx.BadRequest("too many tags")
	}
	return out, nil
}

// truncateUTF8 cuts s to at most n bytes on a rune boundary.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// tailUTF8 keeps the last n bytes of s (starting on a rune boundary).
func tailUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// Unescape decodes the C-style escapes used by button bars, trigger "send" actions and logon actions:
// \r \n \t \e (ESC) \a \b \f \v \0 \\ \xHH \uHHHH and ^X control notation is NOT interpreted (it is literal text).
// Unknown escapes are kept literally ("\q" stays "\q").
func Unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		n := s[i+1]
		switch n {
		case 'r':
			b.WriteByte('\r')
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'e', 'E':
			b.WriteByte(0x1b)
		case 'a':
			b.WriteByte(0x07)
		case 'b':
			b.WriteByte(0x08)
		case 'f':
			b.WriteByte(0x0c)
		case 'v':
			b.WriteByte(0x0b)
		case '0':
			b.WriteByte(0)
		case '\\':
			b.WriteByte('\\')
		case 'x':
			if i+4 <= len(s) {
				if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
					if v < 0x80 {
						b.WriteByte(byte(v))
					} else {
						b.WriteRune(rune(v)) // keep the text valid UTF-8 (Latin-1 code point)
					}
					i += 3
					continue
				}
			}
			b.WriteString(`\x`)
		case 'u':
			if i+6 <= len(s) {
				if v, err := strconv.ParseUint(s[i+2:i+6], 16, 32); err == nil {
					b.WriteRune(rune(v))
					i += 5
					continue
				}
			}
			b.WriteString(`\u`)
		default:
			b.WriteByte('\\')
			b.WriteByte(n)
		}
		i++
	}
	return b.String()
}

// normalizeNewlines converts CRLF / LF / CR line breaks to CR (what a terminal's Enter key sends).
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r")
}

// splitLines splits text on any newline convention, dropping one trailing empty line.
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// validSecretKey reports whether key names a secret a user may have typed into a session: a plain identifier (jump
// host answers "hop:<id>:<key>" belong to other hosts and are never injected).
func validSecretKey(key string) bool {
	if key == "" || len(key) > 64 {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && (c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.')) {
			return false
		}
	}
	return true
}

// countOf renders "1 connection" / "3 connections".
func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
