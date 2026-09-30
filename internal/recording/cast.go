package recording

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// asciicast v2 / v3 reading and writing (REC-2). v3 (what AstraTerm records): a header line
// {"version":3,"term":{"cols","rows","type"},"timestamp",...} followed by [interval, code, data] events whose first
// element is the time since the previous event. v2: {"version":2,"width","height","timestamp",...} and [time, code,
// data] with absolute times.

// castHeader is the union of the v2 and v3 header fields AstraTerm reads.
type castHeader struct {
	Version       int             `json:"version"`
	Term          *castTerm       `json:"term,omitempty"`
	Width         int             `json:"width,omitempty"`
	Height        int             `json:"height,omitempty"`
	Timestamp     int64           `json:"timestamp,omitempty"`
	IdleTimeLimit float64         `json:"idle_time_limit,omitempty"`
	Command       string          `json:"command,omitempty"`
	Title         string          `json:"title,omitempty"`
	Env           json.RawMessage `json:"env,omitempty"`
}

type castTerm struct {
	Cols  int             `json:"cols"`
	Rows  int             `json:"rows"`
	Type  string          `json:"type,omitempty"`
	Theme json.RawMessage `json:"theme,omitempty"`
}

func (h *castHeader) size() (int, int) {
	if h.Term != nil {
		return h.Term.Cols, h.Term.Rows
	}
	return h.Width, h.Height
}

// castEvent is one event with its absolute time (seconds since the start).
type castEvent struct {
	Time float64
	Code string
	Data string
}

// maxCastLine bounds one event line (the recorder writes ≤ 32 KiB of output per event; JSON escaping can grow it).
const maxCastLine = 16 << 20

var errNotCast = errors.New("not an asciicast recording")

// readCast parses a cast from r, calling fn for every event in order. Malformed event lines (e.g. the incomplete last
// line of a recording still being written) are skipped.
func readCast(r io.Reader, fn func(castEvent) error) (castHeader, error) {
	return readCastWith(r, nil, fn)
}

// readCastWith is readCast with a callback for the header (called before the first event).
func readCastWith(r io.Reader, onHeader func(castHeader) error, fn func(castEvent) error) (castHeader, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var h castHeader
	line, err := readLine(br)
	if err != nil && len(line) == 0 {
		if errors.Is(err, io.EOF) {
			return h, errNotCast
		}
		return h, err
	}
	if err := json.Unmarshal(line, &h); err != nil || (h.Version != 2 && h.Version != 3) {
		return h, errNotCast
	}
	if onHeader != nil {
		if err := onHeader(h); err != nil {
			return h, err
		}
	}
	var t float64
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			if ev, ok := parseEvent(line); ok {
				if h.Version == 3 {
					t += ev.Time
					ev.Time = t
				}
				if cerr := fn(ev); cerr != nil {
					return h, cerr
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return h, nil
			}
			return h, err
		}
	}
}

// readLine returns the next line without its newline; an over-long line is consumed and returned empty.
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(buf)+len(chunk) <= maxCastLine {
			buf = append(buf, chunk...)
		} else {
			buf = buf[:0:0]
			if errors.Is(err, bufio.ErrBufferFull) {
				// Skip the rest of this oversized line.
				for errors.Is(err, bufio.ErrBufferFull) {
					_, err = br.ReadSlice('\n')
				}
				if err == nil {
					return nil, nil
				}
				return nil, err
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimRight(buf, "\r\n"), err
	}
}

func parseEvent(line []byte) (castEvent, bool) {
	line = bytes.TrimSpace(line)
	if len(line) < 2 || line[0] != '[' {
		return castEvent{}, false
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil || len(raw) < 3 {
		return castEvent{}, false
	}
	var ev castEvent
	if err := json.Unmarshal(raw[0], &ev.Time); err != nil || ev.Time < 0 || ev.Time != ev.Time {
		return castEvent{}, false
	}
	if err := json.Unmarshal(raw[1], &ev.Code); err != nil || ev.Code == "" {
		return castEvent{}, false
	}
	if err := json.Unmarshal(raw[2], &ev.Data); err != nil {
		// v3 "x" events may carry a number; keep its text.
		ev.Data = string(bytes.Trim(raw[2], `"`))
	}
	return ev, true
}

// ---- writing ------------------------------------------------------------------------------------------------------

// castWriter writes an asciicast (v2 or v3) event stream.
type castWriter struct {
	w       *bufio.Writer
	version int
	last    float64
	err     error
	enc     *json.Encoder
	encBuf  bytes.Buffer
}

func newCastWriter(w io.Writer, version int) *castWriter {
	cw := &castWriter{w: bufio.NewWriterSize(w, 64<<10), version: version}
	cw.enc = json.NewEncoder(&cw.encBuf)
	cw.enc.SetEscapeHTML(false)
	return cw
}

// header writes the header line for the given version.
func (cw *castWriter) header(cols, rows int, termType, title string, ts int64, idle float64, env json.RawMessage) {
	var v any
	if cw.version == 2 {
		type h2 struct {
			Version       int             `json:"version"`
			Width         int             `json:"width"`
			Height        int             `json:"height"`
			Timestamp     int64           `json:"timestamp,omitempty"`
			IdleTimeLimit float64         `json:"idle_time_limit,omitempty"`
			Title         string          `json:"title,omitempty"`
			Env           json.RawMessage `json:"env,omitempty"`
		}
		if env == nil && termType != "" {
			env, _ = json.Marshal(map[string]string{"TERM": termType})
		}
		v = h2{2, cols, rows, ts, idle, title, env}
	} else {
		type h3 struct {
			Version       int      `json:"version"`
			Term          castTerm `json:"term"`
			Timestamp     int64    `json:"timestamp,omitempty"`
			IdleTimeLimit float64  `json:"idle_time_limit,omitempty"`
			Title         string   `json:"title,omitempty"`
		}
		v = h3{3, castTerm{Cols: cols, Rows: rows, Type: termType}, ts, idle, title}
	}
	b, err := json.Marshal(v)
	if err != nil {
		cw.err = err
		return
	}
	cw.write(b)
	cw.write([]byte{'\n'})
}

func (cw *castWriter) write(p []byte) {
	if cw.err == nil {
		_, cw.err = cw.w.Write(p)
	}
}

// event writes one event at absolute time t (seconds).
func (cw *castWriter) event(t float64, code, data string) {
	if cw.err != nil {
		return
	}
	stamp := t
	if cw.version == 3 {
		stamp = max(t-cw.last, 0)
		cw.last = max(t, cw.last)
	}
	cw.encBuf.Reset()
	_ = cw.enc.Encode(data)
	enc := bytes.TrimRight(cw.encBuf.Bytes(), "\n")
	line := make([]byte, 0, len(enc)+32)
	line = append(line, '[')
	line = strconv.AppendFloat(line, stamp, 'f', 6, 64)
	line = append(line, ", "...)
	line = strconv.AppendQuote(line, code)
	line = append(line, ", "...)
	line = append(line, enc...)
	line = append(line, "]\n"...)
	cw.write(line)
}

func (cw *castWriter) flush() error {
	if cw.err != nil {
		return cw.err
	}
	return cw.w.Flush()
}

// convertCast re-encodes a cast (v2 or v3) as the requested version. v2 has no exit ("x") events; they are dropped.
func convertCast(dst io.Writer, src io.Reader, version int) error {
	cw := newCastWriter(dst, version)
	_, err := readCastWith(src, func(h castHeader) error {
		cols, rows := h.size()
		termType := ""
		if h.Term != nil {
			termType = h.Term.Type
		}
		cw.header(cols, rows, termType, h.Title, h.Timestamp, h.IdleTimeLimit, nil)
		return cw.err
	}, func(ev castEvent) error {
		if version == 2 && ev.Code == "x" {
			return nil
		}
		cw.event(ev.Time, ev.Code, ev.Data)
		return cw.err
	})
	if err != nil {
		return err
	}
	return cw.flush()
}

// ---- transcript ---------------------------------------------------------------------------------------------------

// transcriptLine is one line of a cast's plain-text transcript with the time its first character appeared.
type transcriptLine struct {
	Time float64
	Text string
}

// castTranscript turns the output events of a cast into plain text lines (escape sequences removed, CR / BS / TAB
// and cursor-column movements applied), calling fn for every completed line.
func castTranscript(r io.Reader, fn func(transcriptLine) error) (castHeader, error) {
	var (
		vt    vtScanner
		tr    = &transcriber{fn: fn}
		stopE error
	)
	h, err := readCast(r, func(ev castEvent) error {
		if ev.Code != "o" {
			return nil
		}
		tr.t = ev.Time
		vt.feed([]byte(ev.Data), tr)
		if tr.err != nil {
			stopE = tr.err
			return tr.err
		}
		return nil
	})
	if err == nil && stopE == nil {
		err = tr.finish()
	}
	if stopE != nil {
		return h, stopE
	}
	return h, err
}

type transcriber struct {
	ed      lineEd
	alt     bool
	t       float64
	started float64
	has     bool
	fn      func(transcriptLine) error
	err     error
}

func (tr *transcriber) mark() {
	if !tr.has {
		tr.has, tr.started = true, tr.t
	}
}

func (tr *transcriber) text(r rune) {
	if tr.alt {
		return
	}
	tr.mark()
	tr.ed.put(r)
}

func (tr *transcriber) ctrl(b byte) {
	if tr.alt || tr.err != nil {
		return
	}
	if b == '\n' {
		tr.mark()
		line := tr.ed.text()
		tr.ed.ctrl(b)
		tr.has = false
		tr.err = tr.fn(transcriptLine{Time: tr.started, Text: line})
		return
	}
	tr.ed.ctrl(b)
}

func (tr *transcriber) csi(final byte, params string) {
	if strings.HasPrefix(params, "?") && (final == 'h' || final == 'l') {
		for p := range strings.SplitSeq(params[1:], ";") {
			if p == "1049" || p == "1047" || p == "47" {
				tr.alt = final == 'h'
			}
		}
		return
	}
	if !tr.alt {
		tr.ed.csi(final, params)
	}
}

func (tr *transcriber) esc(final byte) {
	if !tr.alt {
		tr.ed.esc(final)
	}
}

func (tr *transcriber) osc(string) {}

func (tr *transcriber) finish() error {
	if tr.err != nil {
		return tr.err
	}
	if line := tr.ed.text(); line != "" {
		return tr.fn(transcriptLine{Time: tr.started, Text: line})
	}
	return nil
}

// validUTF8Prefix returns the longest prefix of p that does not end inside a UTF-8 sequence, and the rest.
func validUTF8Prefix(p []byte) ([]byte, []byte) {
	n := len(p)
	for i := n - 1; i >= 0 && i >= n-utf8.UTFMax; i-- {
		b := p[i]
		if b&0xc0 == 0x80 {
			continue
		}
		if b < 0x80 {
			return p, nil
		}
		if need := leadLen(b); need > 0 && i+need > n {
			return p[:i], p[i:]
		}
		return p, nil
	}
	return p, nil
}
