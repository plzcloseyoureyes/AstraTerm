package term

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"sync"
	"time"
)

// castRecorder writes an asciicast v3 recording (REC-2): a JSON header line followed by NDJSON events
// [interval, code, data] where interval is the time in seconds since the previous event and code is one of
// "o" (output), "i" (input), "r" (resize "COLSxROWS"), "m" (marker) and "x" (exit status).
type castRecorder struct {
	mu          sync.Mutex
	f           *os.File
	w           *bufio.Writer
	last        time.Time
	outCarry    utf8Carry
	inCarry     utf8Carry
	recordInput bool
	size        int64
	err         error
	closed      bool
	enc         *json.Encoder
	encBuf      bytes.Buffer
}

type castHeader struct {
	Version   int      `json:"version"`
	Term      castTerm `json:"term"`
	Timestamp int64    `json:"timestamp"`
	Title     string   `json:"title,omitempty"`
}

type castTerm struct {
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
	Type string `json:"type,omitempty"`
}

func newCastRecorder(path string, cols, rows int, termType, title string, recordInput bool) (*castRecorder, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	r := &castRecorder{f: f, w: bufio.NewWriterSize(f, 64<<10), last: now, recordInput: recordInput}
	r.enc = json.NewEncoder(&r.encBuf)
	r.enc.SetEscapeHTML(false)
	hdr, err := json.Marshal(castHeader{Version: 3, Term: castTerm{Cols: cols, Rows: rows, Type: termType},
		Timestamp: now.Unix(), Title: title})
	if err != nil {
		f.Close()
		return nil, err
	}
	r.write(hdr)
	r.write([]byte{'\n'})
	if r.err != nil {
		f.Close()
		return nil, r.err
	}
	return r, nil
}

func (r *castRecorder) write(p []byte) {
	if r.err != nil {
		return
	}
	n, err := r.w.Write(p)
	r.size += int64(n)
	r.err = err
}

// event appends one event line; the caller holds r.mu.
func (r *castRecorder) event(code string, data string) {
	if r.closed {
		return
	}
	now := time.Now()
	interval := max(now.Sub(r.last), 0)
	r.last = now
	r.encBuf.Reset()
	_ = r.enc.Encode(data) // strings always encode; invalid UTF-8 becomes U+FFFD
	enc := bytes.TrimRight(r.encBuf.Bytes(), "\n")
	line := make([]byte, 0, len(enc)+32)
	line = append(line, '[')
	line = strconv.AppendFloat(line, interval.Seconds(), 'f', 6, 64)
	line = append(line, ", \""...)
	line = append(line, code...)
	line = append(line, "\", "...)
	line = append(line, enc...)
	line = append(line, "]\n"...)
	r.write(line)
}

// Output records terminal output.
func (r *castRecorder) Output(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.outCarry.Complete(p); len(s) > 0 {
		r.event("o", string(s))
	}
}

// Input records user input (only when input recording is enabled).
func (r *castRecorder) Input(p []byte) {
	if !r.recordInput {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.inCarry.Complete(p); len(s) > 0 {
		r.event("i", string(s))
	}
}

// Resize records a terminal size change.
func (r *castRecorder) Resize(cols, rows int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.event("r", strconv.Itoa(cols)+"x"+strconv.Itoa(rows))
}

// Marker records a navigation marker (shell prompt, OSC 133).
func (r *castRecorder) Marker(label string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.event("m", label)
}

// Exit records the exit status of the session's process.
func (r *castRecorder) Exit(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.outCarry.Flush(); len(s) > 0 {
		r.event("o", string(s))
	}
	r.event("x", strconv.Itoa(code))
}

// Flush pushes buffered events to disk.
func (r *castRecorder) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed && r.err == nil {
		r.err = r.w.Flush()
	}
}

// Close finalizes the file and returns its size.
func (r *castRecorder) Close() (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.size, r.err
	}
	if s := r.outCarry.Flush(); len(s) > 0 {
		r.event("o", string(s))
	}
	r.closed = true
	if err := r.w.Flush(); err != nil && r.err == nil {
		r.err = err
	}
	if err := r.f.Close(); err != nil && r.err == nil {
		r.err = err
	}
	return r.size, r.err
}
