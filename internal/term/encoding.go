package term

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/transform"
)

// Character set conversion (TERM-8): the browser always talks UTF-8; sessions whose options.encoding names another
// charset get their output decoded to UTF-8 and their input encoded from UTF-8, both as streams (multi-byte
// sequences split across chunks are carried over).

// LookupEncoding resolves a charset name (IANA / WHATWG labels and common aliases such as cp437, cp1251, koi8-r,
// shift_jis, gbk, big5, euc-kr). It returns (nil, nil) for UTF-8 (no conversion needed).
func LookupEncoding(name string) (encoding.Encoding, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.ReplaceAll(n, "_", "-")
	switch n {
	case "", "utf-8", "utf8", "unicode-1-1-utf-8":
		return nil, nil
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1", "l1":
		// WHATWG maps latin1 to windows-1252; terminals want the real ISO-8859-1 (C1 range intact).
		return charmap.ISO8859_1, nil
	case "cp437", "ibm437", "437":
		return charmap.CodePage437, nil
	case "cp850", "ibm850", "850":
		return charmap.CodePage850, nil
	case "cp852", "ibm852":
		return charmap.CodePage852, nil
	case "cp866", "ibm866":
		return charmap.CodePage866, nil
	}
	if e, err := htmlindex.Get(n); err == nil && e != nil {
		return e, nil
	}
	if e, err := ianaindex.IANA.Encoding(n); err == nil && e != nil {
		return e, nil
	}
	if strings.HasPrefix(n, "cp") {
		if e, err := htmlindex.Get("windows-" + n[2:]); err == nil && e != nil {
			return e, nil
		}
	}
	return nil, fmt.Errorf("unsupported character encoding %q", name)
}

// transcoder applies a transform.Transformer to a stream of chunks.
type transcoder struct {
	t       transform.Transformer
	pending []byte
}

func newDecoder(e encoding.Encoding) *transcoder {
	if e == nil {
		return nil
	}
	return &transcoder{t: e.NewDecoder()}
}

func newEncoder(e encoding.Encoding) *transcoder {
	if e == nil {
		return nil
	}
	return &transcoder{t: encoding.ReplaceUnsupported(e.NewEncoder())}
}

const maxPending = 16

// Process converts src (appended to any carried-over bytes) and returns the converted output in a new slice.
// Incomplete trailing sequences are kept for the next call.
func (tc *transcoder) Process(src []byte) []byte {
	if tc == nil {
		return src
	}
	if len(tc.pending) > 0 {
		src = append(tc.pending, src...)
		tc.pending = nil
	}
	out := make([]byte, 0, len(src)*2+16)
	for len(src) > 0 {
		dst := out[len(out):cap(out)]
		if len(dst) < 64 {
			out = append(out, make([]byte, len(src)*2+64)...)[:len(out)]
			dst = out[len(out):cap(out)]
		}
		nDst, nSrc, err := tc.t.Transform(dst, src, false)
		out = out[:len(out)+nDst]
		src = src[nSrc:]
		switch {
		case err == nil:
			// Everything consumed (or nothing left to do).
			if nSrc == 0 && nDst == 0 {
				src = nil
			}
		case errors.Is(err, transform.ErrShortDst):
			if nSrc == 0 && nDst == 0 {
				out = append(out, make([]byte, len(src)*2+64)...)[:len(out)]
			}
		case errors.Is(err, transform.ErrShortSrc):
			if len(src) > maxPending {
				// Should not happen for real charsets; avoid unbounded carry.
				out = append(out, "�"...)
				src = src[1:]
				continue
			}
			tc.pending = append([]byte(nil), src...)
			src = nil
		default:
			// Invalid input: replace one byte and continue.
			out = append(out, "�"...)
			if len(src) > 0 {
				src = src[1:]
			}
		}
	}
	return out
}

// utf8Carry holds back an incomplete UTF-8 sequence at the end of a chunk so JSON encoders (asciicast) never split
// a character.
type utf8Carry struct{ pending []byte }

// Complete returns the longest prefix of pending+p that does not end inside a UTF-8 sequence.
func (c *utf8Carry) Complete(p []byte) []byte {
	if len(c.pending) > 0 {
		p = append(c.pending, p...)
		c.pending = nil
	}
	cut := incompleteSuffix(p)
	if cut > 0 {
		c.pending = append([]byte(nil), p[len(p)-cut:]...)
		p = p[:len(p)-cut]
	}
	return p
}

// Flush returns whatever is carried (used at end of stream).
func (c *utf8Carry) Flush() []byte {
	p := c.pending
	c.pending = nil
	return p
}

// incompleteSuffix returns the length of a trailing incomplete (but so far valid) UTF-8 sequence.
func incompleteSuffix(p []byte) int {
	for i := 1; i <= 3 && i <= len(p); i++ {
		b := p[len(p)-i]
		if b < 0x80 {
			return 0
		}
		if utf8.RuneStart(b) {
			need := 0
			switch {
			case b&0xe0 == 0xc0:
				need = 2
			case b&0xf0 == 0xe0:
				need = 3
			case b&0xf8 == 0xf0:
				need = 4
			default:
				return 0
			}
			if i < need {
				return i
			}
			return 0
		}
	}
	return 0
}

// mapBackspace rewrites DEL (0x7f, what xterm sends for Backspace) to ^H for sessions configured with
// options.backspace = "ctrl-h". It returns p unchanged (no copy) when nothing needs rewriting.
func mapBackspace(p []byte) []byte {
	i := strings.IndexByte(string(p), 0x7f)
	if i < 0 {
		return p
	}
	out := append([]byte(nil), p...)
	for ; i < len(out); i++ {
		if out[i] == 0x7f {
			out[i] = 0x08
		}
	}
	return out
}
