package term

import (
	"bytes"
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// ---- ring ---------------------------------------------------------------------------------------------------------

func TestRingOffsets(t *testing.T) {
	r := newRing(1024)
	if r.Head() != 0 || r.Tail() != 0 || r.Len() != 0 {
		t.Fatal("empty ring")
	}
	var all []byte
	rnd := rand.New(rand.NewPCG(1, 2))
	for i := range 500 {
		n := rnd.IntN(700)
		chunk := make([]byte, n)
		for j := range chunk {
			chunk[j] = byte(rnd.IntN(256))
		}
		r.Write(chunk)
		all = append(all, chunk...)
		if r.Head() != int64(len(all)) {
			t.Fatalf("head %d want %d", r.Head(), len(all))
		}
		wantTail := max(int64(len(all))-1024, 0)
		if r.Tail() != wantTail {
			t.Fatalf("tail %d want %d", r.Tail(), wantTail)
		}
		if !bytes.Equal(r.Bytes(), all[wantTail:]) {
			t.Fatalf("iteration %d: content mismatch", i)
		}
		// Random sub-slices.
		from := wantTail + int64(rnd.IntN(int(r.Head()-wantTail)+1))
		to := from + int64(rnd.IntN(int(r.Head()-from)+1))
		if got := r.Slice(from, to); !bytes.Equal(got, all[from:to]) {
			t.Fatalf("slice [%d,%d) mismatch", from, to)
		}
	}
	// Offsets outside the ring yield nothing.
	if n := r.ReadAt(make([]byte, 10), r.Tail()-1); n != 0 {
		t.Fatal("read before tail")
	}
	if n := r.ReadAt(make([]byte, 10), r.Head()); n != 0 {
		t.Fatal("read at head")
	}
}

func TestRingHugeWrite(t *testing.T) {
	r := newRing(1024)
	big := bytes.Repeat([]byte("0123456789"), 500) // 5000 bytes
	r.Write([]byte("abc"))
	r.Write(big)
	if r.Head() != 5003 || r.Tail() != 5003-1024 {
		t.Fatalf("head %d tail %d", r.Head(), r.Tail())
	}
	if !bytes.Equal(r.Bytes(), big[len(big)-1024:]) {
		t.Fatal("content")
	}
}

// ---- OSC scanner --------------------------------------------------------------------------------------------------

func scanAll(chunks [][]byte) []oscEvent {
	var sc oscScanner
	var evs []oscEvent
	var off int64
	for _, c := range chunks {
		sc.Scan(c, off, func(e oscEvent) { evs = append(evs, e) })
		off += int64(len(c))
	}
	return evs
}

func splitEvery(data []byte, n int) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		k := min(n, len(data))
		out = append(out, data[:k])
		data = data[k:]
	}
	return out
}

func TestOSCScannerAcrossChunks(t *testing.T) {
	stream := []byte("hello\x1b]0;my title\x07world\x07" +
		"\x1b]7;file://host/home/us%20er/dir\x1b\\" +
		"\x1b]133;A\x07$ \x1b]133;B\x07ls\r\n\x1b]133;C\x07out\r\n\x1b]133;D;2\x1b\\" +
		"\x1bP+q\x07ignored\x1b\\" + // BEL inside DCS is not a bell
		"\x1b[31mred\x1b[0m" +
		"\x1b]2;second\x1b\\" +
		"\x1b]633;P;Cwd=/tmp/a\\x3bb\x07" +
		"\x1b]1337;CurrentDir=/srv/data\x07")
	want := []struct {
		kind oscKind
		text string
		mark byte
		code int
	}{
		{oscTitle, "my title", 0, -1},
		{oscBell, "", 0, -1},
		{oscCwd, "/home/us er/dir", 0, -1},
		{oscMark, "", 'A', -1},
		{oscMark, "", 'B', -1},
		{oscMark, "", 'C', -1},
		{oscMark, "", 'D', 2},
		{oscTitle, "second", 0, -1},
		{oscCwd, "/tmp/a;b", 0, -1},
		{oscCwd, "/srv/data", 0, -1},
	}
	check := func(name string, evs []oscEvent) {
		t.Helper()
		if len(evs) != len(want) {
			t.Fatalf("%s: got %d events %+v", name, len(evs), evs)
		}
		for i, w := range want {
			e := evs[i]
			if e.Kind != w.kind || e.Text != w.text || e.Mark != w.mark {
				t.Fatalf("%s: event %d = %+v want %+v", name, i, e, w)
			}
			if w.code >= 0 && (e.ExitCode == nil || *e.ExitCode != w.code) {
				t.Fatalf("%s: event %d exit code %v", name, i, e.ExitCode)
			}
			if e.Offset <= 0 || e.Offset > int64(len(stream)) {
				t.Fatalf("%s: bad offset %d", name, e.Offset)
			}
		}
	}
	whole := scanAll([][]byte{stream})
	check("whole", whole)
	// Every split size, and a random split, yields the same events at the same offsets.
	for n := 1; n <= 16; n++ {
		evs := scanAll(splitEvery(stream, n))
		check("split", evs)
		for i := range evs {
			if evs[i].Offset != whole[i].Offset {
				t.Fatalf("split %d: offset %d != %d", n, evs[i].Offset, whole[i].Offset)
			}
		}
	}
	// The D mark offset points right after its terminator.
	end := bytes.Index(stream, []byte("\x1b]133;D;2\x1b\\")) + len("\x1b]133;D;2\x1b\\")
	if whole[6].Offset != int64(end) {
		t.Fatalf("mark offset %d want %d", whole[6].Offset, end)
	}
}

func TestOSCScannerEdgeCases(t *testing.T) {
	// Oversized OSC payloads (e.g. OSC 52) are skipped without buffering them.
	big := append([]byte("\x1b]52;c;"), bytes.Repeat([]byte("A"), 100<<10)...)
	big = append(big, 0x07)
	big = append(big, "\x1b]0;ok\x07"...)
	evs := scanAll([][]byte{big})
	if len(evs) != 1 || evs[0].Text != "ok" {
		t.Fatalf("events %+v", evs)
	}
	// An OSC interrupted by CSI is dropped; control characters are stripped from titles.
	evs = scanAll([][]byte{[]byte("\x1b]0;broken\x1b[1m\x1b]0;a\x01b\x07")})
	if len(evs) != 1 || evs[0].Text != "ab" {
		t.Fatalf("events %+v", evs)
	}
	// Windows file URL and bare path in OSC 7.
	if p := parseCwdURL("file:///C:/Users/me"); p != "C:/Users/me" {
		t.Fatalf("got %q", p)
	}
	if p := parseCwdURL("/plain/path"); p != "/plain/path" {
		t.Fatalf("got %q", p)
	}
	if p := parseCwdURL("file://h/with%zzbad"); p != "/with%zzbad" {
		t.Fatalf("lenient decode got %q", p)
	}
	if p := parseCwdURL("http://example.com/x"); p != "" {
		t.Fatalf("non-file URL accepted: %q", p)
	}
}

// ---- encodings ----------------------------------------------------------------------------------------------------

func TestEncodingRoundTrip(t *testing.T) {
	cases := map[string]string{
		"koi8-r":       "Привет, мир! ёЁ",
		"cp1251":       "Съешь же ещё этих мягких французских булок",
		"shift_jis":    "こんにちは世界 ｶﾀｶﾅ",
		"euc-jp":       "日本語のテキスト",
		"gbk":          "你好，世界",
		"gb18030":      "中文字符 测试",
		"big5":         "繁體中文測試",
		"euc-kr":       "안녕하세요 세계",
		"cp437":        "box ╔═╗ ░▒▓",
		"iso-8859-15":  "Grüße €",
		"windows-1252": "café – “quotes”",
	}
	rnd := rand.New(rand.NewPCG(7, 9))
	for name, text := range cases {
		enc, err := LookupEncoding(name)
		if err != nil || enc == nil {
			t.Fatalf("%s: %v", name, err)
		}
		// UTF-8 → charset (input path), fed byte by byte to exercise the carry.
		e := newEncoder(enc)
		var encoded []byte
		for _, b := range []byte(text) {
			encoded = append(encoded, e.Process([]byte{b})...)
		}
		// charset → UTF-8 (output path) in random chunks.
		d := newDecoder(enc)
		var decoded []byte
		for rest := encoded; len(rest) > 0; {
			n := 1 + rnd.IntN(4)
			n = min(n, len(rest))
			decoded = append(decoded, d.Process(rest[:n])...)
			rest = rest[n:]
		}
		if string(decoded) != text {
			t.Fatalf("%s: round trip %q -> %q", name, text, decoded)
		}
	}
	if e, err := LookupEncoding("UTF-8"); e != nil || err != nil {
		t.Fatal("utf-8 must need no conversion")
	}
	if _, err := LookupEncoding("klingon-1"); err == nil {
		t.Fatal("unknown charset accepted")
	}
}

func TestUTF8Carry(t *testing.T) {
	var c utf8Carry
	s := []byte("aé€😀b")
	var out []byte
	for i := range s {
		out = append(out, c.Complete(s[i:i+1])...)
		if !utf8.Valid(out) {
			t.Fatalf("invalid prefix %q", out)
		}
	}
	out = append(out, c.Flush()...)
	if string(out) != string(s) {
		t.Fatalf("got %q", out)
	}
}

func TestMapBackspace(t *testing.T) {
	in := []byte("ab\x7fc\x7f")
	out := mapBackspace(in)
	if string(out) != "ab\bc\b" || string(in) != "ab\x7fc\x7f" {
		t.Fatalf("got %q (input %q)", out, in)
	}
	same := []byte("plain")
	if &mapBackspace(same)[0] != &same[0] {
		t.Fatal("no-op mapping must not copy")
	}
}

// ---- text stripping & logs ----------------------------------------------------------------------------------------

func TestStripText(t *testing.T) {
	raw := "\x1b[1;31mError\x1b[0m: bad\r\n" +
		"progress 10%\rprogress 100%\r\n" +
		"abc\b\bX\n" +
		"tab\tend\n" +
		"\x1b]0;title\x07after osc\n" +
		"unicode é😀\n" +
		"partial"
	got := StripText([]byte(raw))
	want := "Error: bad\nprogress 100%\naXc\ntab     end\nafter osc\nunicode é😀\npartial\n"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	// Same result when fed in tiny pieces through the streaming stripper.
	var b strings.Builder
	la := &lineAssembler{now: time.Now, emit: func(line string, _ time.Time) { b.WriteString(line + "\n") }}
	var st ansiStripper
	for _, c := range splitEvery([]byte(raw), 3) {
		st.Feed(c, la.text, la.control)
	}
	if la.pending() {
		la.flush()
	}
	if b.String() != want {
		t.Fatalf("streamed got %q", b.String())
	}
}

func TestTextLoggerTimestamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	l, err := newTextLogger(path, "=== header ===", true)
	if err != nil {
		t.Fatal(err)
	}
	l.Write([]byte("one\r\ntw"))
	l.Write([]byte("o\x1b[0m\r\nthree"))
	if _, err := l.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 4 || lines[0] != "=== header ===" {
		t.Fatalf("lines %q", lines)
	}
	for i, want := range []string{"one", "two", "three"} {
		l := lines[i+1]
		if len(l) < 26 || l[0] != '[' || l[24] != ']' || l[26:] != want {
			t.Fatalf("line %d = %q", i, l)
		}
	}
}

// ---- asciicast ----------------------------------------------------------------------------------------------------

func TestCastRecorder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.cast")
	r, err := newCastRecorder(path, 100, 30, "xterm-256color", "demo", true)
	if err != nil {
		t.Fatal(err)
	}
	r.Output([]byte("hello \xe2\x82")) // split euro sign
	r.Output([]byte("\xac <b>&"))
	r.Input([]byte("ls\r"))
	r.Resize(120, 40)
	r.Marker("")
	r.Exit(3)
	size, err := r.Close()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if int64(len(data)) != size {
		t.Fatalf("size %d vs file %d", size, len(data))
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var hdr struct {
		Version int `json:"version"`
		Term    struct {
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
			Type string `json:"type"`
		} `json:"term"`
		Timestamp int64  `json:"timestamp"`
		Title     string `json:"title"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &hdr); err != nil {
		t.Fatal(err)
	}
	if hdr.Version != 3 || hdr.Term.Cols != 100 || hdr.Term.Rows != 30 || hdr.Term.Type != "xterm-256color" ||
		hdr.Timestamp == 0 || hdr.Title != "demo" {
		t.Fatalf("header %+v", hdr)
	}
	var codes, datas []string
	for _, l := range lines[1:] {
		var ev []any
		if err := json.Unmarshal([]byte(l), &ev); err != nil || len(ev) != 3 {
			t.Fatalf("bad event %q: %v", l, err)
		}
		if iv, ok := ev[0].(float64); !ok || iv < 0 {
			t.Fatalf("bad interval in %q", l)
		}
		codes = append(codes, ev[1].(string))
		datas = append(datas, ev[2].(string))
	}
	if strings.Join(codes, ",") != "o,o,i,r,m,x" {
		t.Fatalf("codes %v", codes)
	}
	if datas[0]+datas[1] != "hello € <b>&" || datas[2] != "ls\r" || datas[3] != "120x40" || datas[5] != "3" {
		t.Fatalf("data %q", datas)
	}
}
