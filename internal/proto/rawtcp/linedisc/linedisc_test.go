package linedisc

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func TestTranslate(t *testing.T) {
	for _, tc := range []struct {
		e    Ending
		in   string
		want string
	}{
		{CRLF, "a\rb", "a\r\nb"},
		{LF, "a\rb", "a\nb"},
		{CR, "a\rb", "a\rb"},
		{CRLF, "no cr", "no cr"},
		{LF, "\r\r", "\n\n"},
	} {
		if got := tc.e.Translate([]byte(tc.in)); string(got) != tc.want {
			t.Errorf("%v.Translate(%q) = %q, want %q", tc.e, tc.in, got, tc.want)
		}
	}
	if ParseEnding("LF", CRLF) != LF || ParseEnding("", CR) != CR || ParseEnding("bogus", LF) != LF {
		t.Error("ParseEnding")
	}
}

func TestEchoRender(t *testing.T) {
	var e Echo
	cases := []struct{ in, want string }{
		{"abc", "abc"},
		{"\x7f", "\b \b"},          // erases "c"
		{"\x1b[D\x1bOA", ""},       // cursor keys are swallowed
		{"\x03", ""},               // Ctrl+C is not echoed
		{"é", "é"},                 // multi-byte input echoed as is
		{"\x7f", "\b \b"},          // erases the whole "é"
		{"\r", "\r\n"},             // Enter
		{"\x7f\x7f", ""},           // nothing typed on this line: never erase remote output
		{"\t\x1b]0;x\x07z", "\tz"}, // OSC sequences swallowed
	}
	for _, c := range cases {
		if got := string(e.Render([]byte(c.in))); got != c.want {
			t.Errorf("Render(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// An escape sequence split across writes stays swallowed.
	var e2 Echo
	if got := string(e2.Render([]byte("x\x1b["))) + string(e2.Render([]byte("1;5Cy"))); got != "xy" {
		t.Errorf("split CSI = %q", got)
	}
}

// blockingSrc is a source whose Read blocks until data is fed or it is closed.
type blockingSrc struct {
	ch     chan []byte
	closed chan struct{}
	once   sync.Once
}

func newSrc() *blockingSrc {
	return &blockingSrc{ch: make(chan []byte, 16), closed: make(chan struct{})}
}

func (s *blockingSrc) Read(p []byte) (int, error) {
	// Like a socket: data that arrived before the close is still returned first.
	select {
	case b := <-s.ch:
		return copy(p, b), nil
	default:
	}
	select {
	case b := <-s.ch:
		return copy(p, b), nil
	case <-s.closed:
		return 0, io.ErrClosedPipe
	}
}
func (s *blockingSrc) close() { s.once.Do(func() { close(s.closed) }) }

func readWithin(t *testing.T, r *Reader, d time.Duration) (string, error) {
	t.Helper()
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := r.Read(buf)
		ch <- res{string(buf[:n]), err}
	}()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-time.After(d):
		t.Fatal("Read did not return in time")
		return "", nil
	}
}

func TestReaderInjectWakesBlockedRead(t *testing.T) {
	src := newSrc()
	r := NewReader(src)
	defer src.close()
	go func() {
		time.Sleep(30 * time.Millisecond)
		r.Inject([]byte("echo"))
	}()
	if s, err := readWithin(t, r, 2*time.Second); s != "echo" || err != nil {
		t.Fatalf("got %q, %v", s, err)
	}
	src.ch <- []byte("remote")
	if s, err := readWithin(t, r, 2*time.Second); s != "remote" || err != nil {
		t.Fatalf("got %q, %v", s, err)
	}
}

func TestReaderErrorAfterData(t *testing.T) {
	src := newSrc()
	r := NewReader(src)
	src.ch <- []byte("last")
	time.Sleep(20 * time.Millisecond)
	src.close()
	time.Sleep(20 * time.Millisecond)
	if s, err := readWithin(t, r, time.Second); s != "last" || err != nil {
		t.Fatalf("buffered data first: %q, %v", s, err)
	}
	if _, err := readWithin(t, r, time.Second); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("then the source error, got %v", err)
	}
}

func TestReaderCloseUnblocks(t *testing.T) {
	src := newSrc()
	defer src.close()
	r := NewReader(src)
	go func() {
		time.Sleep(30 * time.Millisecond)
		r.Close()
	}()
	if _, err := readWithin(t, r, 2*time.Second); !errors.Is(err, io.EOF) {
		t.Fatalf("got %v, want EOF", err)
	}
	r.Inject([]byte("ignored")) // must not panic or block after Close
}

// TestReaderBackpressure: the pump stops reading the source while the consumer lags beyond the high-water mark.
func TestReaderBackpressure(t *testing.T) {
	var reads int
	var mu sync.Mutex
	src := readerFunc(func(p []byte) (int, error) {
		mu.Lock()
		reads++
		mu.Unlock()
		return len(p), nil // endless data
	})
	r := NewReader(src)
	buf := make([]byte, 1)
	if _, err := r.Read(buf); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	n := reads
	mu.Unlock()
	if n*(32<<10) > highWater+(64<<10) {
		t.Fatalf("pump read %d chunks without a consumer (no backpressure)", n)
	}
	r.Close()
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestReaderOrderPreserved(t *testing.T) {
	src := newSrc()
	defer src.close()
	r := NewReader(src)
	// Start the pump (it starts with the first Read, as the session pump does right after the backend opens).
	r.Inject([]byte("0"))
	if s, err := readWithin(t, r, time.Second); s != "0" || err != nil {
		t.Fatalf("got %q, %v", s, err)
	}
	src.ch <- []byte("1")
	time.Sleep(20 * time.Millisecond)
	r.Inject([]byte("2"))
	src.ch <- []byte("3")
	time.Sleep(20 * time.Millisecond)
	var got bytes.Buffer
	for got.Len() < 3 {
		s, err := readWithin(t, r, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		got.WriteString(s)
	}
	if got.String() != "123" {
		t.Fatalf("order = %q", got.String())
	}
}
