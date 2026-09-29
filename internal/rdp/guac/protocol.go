// Package guac implements the Guacamole protocol (RESEARCH §3.11): the instruction codec used on both sides of the
// WebSocket tunnel and the client handshake with guacd.
//
// An instruction is a comma-separated list of elements terminated by ';', each element encoded as LENGTH.VALUE, the
// first element being the opcode: "4.size,1.0,4.1024,3.768;". The protocol specification (and guacd) counts LENGTH
// in Unicode code points. guacamole-common-js 1.5 (the browser side) counts UTF-16 code units instead (JavaScript
// string lengths), which differs for characters outside the Basic Multilingual Plane, so every codec function takes
// the LengthUnit of its peer.
package guac

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// LengthUnit selects how element lengths are counted.
type LengthUnit int

const (
	// CodePoints counts Unicode code points (guacd, the protocol specification).
	CodePoints LengthUnit = iota
	// UTF16Units counts UTF-16 code units (guacamole-common-js WebSocketTunnel: JavaScript string lengths).
	UTF16Units
)

// Limits protecting the codec against oversized input.
const (
	// MaxElementLength bounds one element (guacd sends blobs of at most a few KiB; file names and messages are short).
	MaxElementLength = 4 << 20
	// MaxElements bounds the number of elements of one instruction.
	MaxElements = 1024
	// maxLengthDigits bounds the LENGTH prefix.
	maxLengthDigits = 8
)

// InternalOpcode is the opcode of tunnel-internal instructions ("0.,4.ping,…;", "0.,36.<uuid>;"), which are
// exchanged between the browser and the tunnel endpoint and never forwarded to guacd.
const InternalOpcode = ""

// Errors returned by the codec.
var (
	ErrMalformed = errors.New("guac: malformed instruction")
	ErrTooLarge  = errors.New("guac: instruction too large")
)

// Instruction is one Guacamole protocol instruction.
type Instruction struct {
	Opcode string
	Args   []string
}

// New builds an instruction.
func New(opcode string, args ...string) Instruction { return Instruction{Opcode: opcode, Args: args} }

// String renders the instruction with code point lengths (guacd encoding), mostly for logs and tests.
func (in Instruction) String() string { return string(in.Append(nil, CodePoints)) }

// Arg returns argument i, or "" when absent.
func (in Instruction) Arg(i int) string {
	if i < 0 || i >= len(in.Args) {
		return ""
	}
	return in.Args[i]
}

// Append appends the encoded instruction to b.
func (in Instruction) Append(b []byte, unit LengthUnit) []byte {
	b = appendElement(b, in.Opcode, unit)
	for _, a := range in.Args {
		b = append(b, ',')
		b = appendElement(b, a, unit)
	}
	return append(b, ';')
}

// Encode returns the encoded instruction.
func (in Instruction) Encode(unit LengthUnit) []byte { return in.Append(nil, unit) }

func appendElement(b []byte, s string, unit LengthUnit) []byte {
	b = strconv.AppendInt(b, int64(Length(s, unit)), 10)
	b = append(b, '.')
	return append(b, s...)
}

// Length returns the protocol length of s in the given unit. Invalid UTF-8 bytes count as one code point / unit
// each (they are replaced by U+FFFD by JavaScript and guacd alike).
func Length(s string, unit LengthUnit) int {
	n := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			i++
			n++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		n++
		if unit == UTF16Units && r >= 0x10000 && size == 4 {
			n++ // surrogate pair
		}
	}
	return n
}

// ---- parsing a complete message (browser → tunnel: every WebSocket text frame holds whole instructions) -------------

// ParseAll parses every instruction of msg, which must consist of whole instructions only.
func ParseAll(msg string, unit LengthUnit) ([]Instruction, error) {
	var out []Instruction
	for len(msg) > 0 {
		in, rest, err := parseOne(msg, unit)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
		msg = rest
	}
	return out, nil
}

// parseOne parses the first instruction of s and returns the remainder.
func parseOne(s string, unit LengthUnit) (Instruction, string, error) {
	var in Instruction
	first := true
	for {
		dot := strings.IndexByte(s, '.')
		if dot <= 0 || dot > maxLengthDigits || strings.IndexFunc(s[:dot], func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return in, "", ErrMalformed // digits only, as the streaming reader (no sign)
		}
		n, err := strconv.Atoi(s[:dot])
		if err != nil || n > MaxElementLength {
			return in, "", ErrMalformed
		}
		s = s[dot+1:]
		end, ok := advance(s, n, unit)
		if !ok || end >= len(s) {
			return in, "", ErrMalformed
		}
		value, term := s[:end], s[end]
		s = s[end+1:]
		if first {
			in.Opcode, first = value, false
		} else {
			if len(in.Args) >= MaxElements {
				return in, "", ErrTooLarge
			}
			in.Args = append(in.Args, value)
		}
		switch term {
		case ';':
			return in, s, nil
		case ',':
		default:
			return in, "", ErrMalformed
		}
	}
}

// advance returns the byte offset in s after n length units.
func advance(s string, n int, unit LengthUnit) (int, bool) {
	i := 0
	for n > 0 {
		if i >= len(s) {
			return 0, false
		}
		c := s[i]
		if c < utf8.RuneSelf {
			i++
			n--
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		n--
		if unit == UTF16Units && r >= 0x10000 && size == 4 {
			if n == 0 {
				return 0, false // the length ends inside a surrogate pair
			}
			n--
		}
	}
	return i, true
}

// ---- streaming reader (guacd → tunnel) -----------------------------------------------------------------------------

// Reader reads instructions from a byte stream (the guacd TCP connection).
type Reader struct {
	br   *bufio.Reader
	unit LengthUnit
	// MaxInstruction bounds the encoded size of one instruction (0 = 8 MiB).
	MaxInstruction int
}

// NewReader returns a Reader over r counting lengths in unit.
func NewReader(r io.Reader, unit LengthUnit) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 64<<10), unit: unit}
}

// Buffered reports how many bytes are already buffered (callers batch instructions that arrived together).
func (r *Reader) Buffered() int { return r.br.Buffered() }

// Read reads one instruction. It returns io.EOF only at a clean instruction boundary; a stream ending inside an
// instruction yields io.ErrUnexpectedEOF.
func (r *Reader) Read() (Instruction, error) {
	in, _, err := r.read(false)
	return in, err
}

// ReadRaw reads one instruction and also returns its encoded bytes exactly as received.
func (r *Reader) ReadRaw() (Instruction, []byte, error) { return r.read(true) }

func (r *Reader) read(keepRaw bool) (Instruction, []byte, error) {
	var (
		in    Instruction
		raw   []byte
		total int
	)
	limit := r.MaxInstruction
	if limit <= 0 {
		limit = 8 << 20
	}
	for idx := 0; ; idx++ {
		n, digits, err := r.readLength()
		if err != nil {
			if idx == 0 && err == io.EOF {
				return in, nil, io.EOF
			}
			return in, nil, unexpected(err)
		}
		value, err := r.readValue(n)
		if err != nil {
			return in, nil, unexpected(err)
		}
		term, err := r.br.ReadByte()
		if err != nil {
			return in, nil, unexpected(err)
		}
		total += len(digits) + 1 + len(value) + 1
		if total > limit {
			return in, nil, ErrTooLarge
		}
		if keepRaw {
			raw = append(raw, digits...)
			raw = append(raw, '.')
			raw = append(raw, value...)
			raw = append(raw, term)
		}
		if idx == 0 {
			in.Opcode = value
		} else {
			if len(in.Args) >= MaxElements {
				return in, nil, ErrTooLarge
			}
			in.Args = append(in.Args, value)
		}
		switch term {
		case ';':
			return in, raw, nil
		case ',':
		default:
			return in, nil, ErrMalformed
		}
	}
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// readLength reads "LENGTH." and returns the length and its digits.
func (r *Reader) readLength() (int, []byte, error) {
	var digits [maxLengthDigits]byte
	nd := 0
	for {
		c, err := r.br.ReadByte()
		if err != nil {
			if nd > 0 {
				return 0, nil, unexpected(err)
			}
			return 0, nil, err
		}
		if c == '.' {
			if nd == 0 {
				return 0, nil, ErrMalformed
			}
			n, err := strconv.Atoi(string(digits[:nd]))
			if err != nil || n > MaxElementLength {
				return 0, nil, ErrMalformed
			}
			return n, append([]byte(nil), digits[:nd]...), nil
		}
		if c < '0' || c > '9' || nd == maxLengthDigits {
			return 0, nil, ErrMalformed
		}
		digits[nd] = c
		nd++
	}
}

// readValue reads an element value of n length units.
func (r *Reader) readValue(n int) (string, error) {
	if n == 0 {
		return "", nil
	}
	buf := make([]byte, 0, n)
	for n > 0 {
		if _, err := r.br.Peek(1); err != nil {
			return "", err
		}
		p, _ := r.br.Peek(r.br.Buffered())
		i := 0
		for i < len(p) && n > 0 {
			c := p[i]
			if c < utf8.RuneSelf {
				i++
				n--
				continue
			}
			if !utf8.FullRune(p[i:]) {
				break
			}
			ru, size := utf8.DecodeRune(p[i:])
			units := 1
			if r.unit == UTF16Units && ru >= 0x10000 && size == 4 {
				units = 2
			}
			if units > n {
				return "", ErrMalformed
			}
			i += size
			n -= units
		}
		if i == 0 {
			// An incomplete multi-byte sequence at the end of the buffer: wait for the rest of it (at most 4 bytes).
			need := 2
			for {
				q, err := r.br.Peek(need)
				if err != nil {
					if len(q) > 0 && (err == io.EOF || err == bufio.ErrBufferFull) {
						// Truncated / invalid sequence: take one byte as a replacement character.
						b, _ := r.br.ReadByte()
						buf = append(buf, b)
						n--
						break
					}
					return "", err
				}
				if utf8.FullRune(q) || need >= utf8.UTFMax {
					ru, size := utf8.DecodeRune(q)
					units := 1
					if r.unit == UTF16Units && ru >= 0x10000 && size == 4 {
						units = 2
					}
					if units > n {
						return "", ErrMalformed
					}
					buf = append(buf, q[:size]...)
					_, _ = r.br.Discard(size)
					n -= units
					break
				}
				need++
			}
			continue
		}
		buf = append(buf, p[:i]...)
		_, _ = r.br.Discard(i)
	}
	return string(buf), nil
}

// ---- helpers --------------------------------------------------------------------------------------------------------

// Status codes (Guacamole protocol "status" values) used when closing tunnels or reporting errors.
const (
	StatusSuccess             = 0x0000
	StatusUnsupported         = 0x0100
	StatusServerError         = 0x0200
	StatusServerBusy          = 0x0201
	StatusUpstreamTimeout     = 0x0202
	StatusUpstreamError       = 0x0203
	StatusResourceNotFound    = 0x0204
	StatusResourceConflict    = 0x0205
	StatusResourceClosed      = 0x0206
	StatusUpstreamNotFound    = 0x0207
	StatusUpstreamUnavailable = 0x0208
	StatusSessionConflict     = 0x0209
	StatusSessionTimeout      = 0x020A
	StatusSessionClosed       = 0x020B
	StatusClientBadRequest    = 0x0300
	StatusClientUnauthorized  = 0x0301
	StatusClientForbidden     = 0x0303
	StatusClientTimeout       = 0x0308
	StatusClientOverrun       = 0x030D
	StatusClientBadType       = 0x030F
	StatusClientTooMany       = 0x031D
)

// StatusText describes a Guacamole status code.
func StatusText(code int) string {
	switch code {
	case StatusSuccess:
		return "success"
	case StatusUnsupported:
		return "operation not supported"
	case StatusServerError:
		return "internal error in guacd"
	case StatusServerBusy:
		return "guacd is busy"
	case StatusUpstreamTimeout:
		return "the remote desktop server is not responding"
	case StatusUpstreamError:
		return "the remote desktop server reported an error"
	case StatusResourceNotFound:
		return "resource not found"
	case StatusResourceConflict:
		return "resource conflict"
	case StatusResourceClosed:
		return "resource closed"
	case StatusUpstreamNotFound:
		return "the remote desktop server could not be reached"
	case StatusUpstreamUnavailable:
		return "the remote desktop server refused the connection"
	case StatusSessionConflict:
		return "the session was taken over by another connection"
	case StatusSessionTimeout:
		return "the remote session timed out"
	case StatusSessionClosed:
		return "the remote session was closed"
	case StatusClientBadRequest:
		return "invalid request"
	case StatusClientUnauthorized:
		return "authentication failed"
	case StatusClientForbidden:
		return "access denied"
	case StatusClientTimeout:
		return "client timeout"
	case StatusClientOverrun:
		return "too much data"
	case StatusClientBadType:
		return "unsupported data type"
	case StatusClientTooMany:
		return "too many connections"
	}
	return fmt.Sprintf("status %d", code)
}
