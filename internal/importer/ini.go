package importer

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/unicode"
)

// iniKV is one key/value pair (order preserved within a section).
type iniKV struct {
	Key   string
	Value string
}

// iniSection is a named INI section with its key/value pairs in file order.
type iniSection struct {
	Name string
	KVs  []iniKV
}

// iniFile is a parsed INI file: sections in file order (a leading unnamed section captures pre-header keys).
type iniFile struct {
	Sections []*iniSection
}

// get returns the first value of key in the section (case-insensitive), or "".
func (s *iniSection) get(key string) string {
	for _, kv := range s.KVs {
		if strings.EqualFold(kv.Key, key) {
			return kv.Value
		}
	}
	return ""
}

// parseINI parses INI text into ordered sections. The first '=' splits key/value (values keep '#', '%', ';', '"' as
// data); a line without '=' (comment, garbage) is ignored. Section headers are "[name]".
func parseINI(content []byte) *iniFile {
	text := decodeMaybeCP1252(content)
	f := &iniFile{}
	cur := &iniSection{Name: ""}
	f.Sections = append(f.Sections, cur)
	forEachLine(text, func(line string) {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			return
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			cur = &iniSection{Name: strings.TrimSpace(trimmed[1 : len(trimmed)-1])}
			f.Sections = append(f.Sections, cur)
			return
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			return
		}
		cur.KVs = append(cur.KVs, iniKV{Key: key, Value: line[eq+1:]})
	})
	return f
}

// forEachLine calls fn for every '\n'-separated line of s (no line-length limit, unlike bufio.Scanner, whose silent
// stop at an over-long line would truncate an import without notice).
func forEachLine(s string, fn func(line string)) {
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			fn(s)
			return
		}
		fn(s[:i])
		s = s[i+1:]
	}
}

// decodeMaybeCP1252 returns content as a UTF-8 string. UTF-16 (with BOM — common for Windows .reg exports) is decoded
// first; then valid UTF-8 is kept as-is; otherwise it is decoded from Windows-1252 (MobaXterm.ini's charset on Western
// systems), which never fails since CP1252 maps every byte. Use the `charset` preview option for other code pages.
func decodeMaybeCP1252(content []byte) string {
	if s, ok := decodeUTF16(content); ok {
		return s
	}
	b := dropBOM(content)
	if utf8.Valid(b) {
		return string(b)
	}
	out, err := charmap.Windows1252.NewDecoder().Bytes(b)
	if err != nil {
		return string(b)
	}
	return string(out)
}

// normCharset returns the canonical charset option ("" for automatic detection).
func normCharset(cs string) string {
	cs = strings.ToLower(strings.TrimSpace(cs))
	if cs == "auto" {
		return ""
	}
	return cs
}

// decodeWithCharset converts content to UTF-8 from an explicit charset (a WHATWG label such as "windows-1255",
// "gbk", "shift_jis"). A UTF-16 BOM always wins.
func decodeWithCharset(content []byte, charset string) (string, error) {
	if s, ok := decodeUTF16(content); ok {
		return s, nil
	}
	b := dropBOM(content)
	switch charset {
	case "", "utf-8", "utf8":
		return decodeMaybeCP1252(b), nil
	}
	enc, err := htmlindex.Get(charset)
	if err != nil || enc == nil {
		return "", badRequest("unknown text encoding " + truncate(charset, 40))
	}
	out, err := enc.NewDecoder().Bytes(b)
	if err != nil {
		return "", badRequest("the file is not valid " + charset + " text")
	}
	return string(out), nil
}

// decodeUTF16 decodes UTF-16 content that carries a BOM (LE FF FE or BE FE FF); ok is false for non-UTF-16 input.
func decodeUTF16(b []byte) (string, bool) {
	var order unicode.Endianness
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		order = unicode.LittleEndian
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		order = unicode.BigEndian
	default:
		return "", false
	}
	dec := unicode.UTF16(order, unicode.ExpectBOM).NewDecoder()
	out, err := dec.Bytes(b)
	if err != nil {
		return "", false
	}
	return string(out), true
}

func dropBOM(b []byte) []byte {
	if bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}) {
		return b[3:]
	}
	return b
}
