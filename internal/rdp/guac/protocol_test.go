package guac

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestEncodeCountsUnits(t *testing.T) {
	in := New("name", "héllo", "😀x", "")
	if got := string(in.Encode(CodePoints)); got != "4.name,5.héllo,2.😀x,0.;" {
		t.Fatalf("code points: %q", got)
	}
	if got := string(in.Encode(UTF16Units)); got != "4.name,5.héllo,3.😀x,0.;" {
		t.Fatalf("utf-16: %q", got)
	}
	if got := New(InternalOpcode, "ping", "123").String(); got != "0.,4.ping,3.123;" {
		t.Fatalf("internal: %q", got)
	}
}

func TestParseAllRoundTrip(t *testing.T) {
	ins := []Instruction{
		New("size", "0", "1024", "768"),
		New("clipboard", "1", "text/plain"),
		New("file", "2", "application/pdf", "rapport-été 😀.pdf"),
		New(InternalOpcode, "ping", "1700000000000"),
		New("nop"),
	}
	for _, unit := range []LengthUnit{CodePoints, UTF16Units} {
		var b []byte
		for _, in := range ins {
			b = in.Append(b, unit)
		}
		got, err := ParseAll(string(b), unit)
		if err != nil {
			t.Fatalf("unit %d: %v", unit, err)
		}
		if len(got) != len(ins) {
			t.Fatalf("unit %d: got %d instructions", unit, len(got))
		}
		for i := range ins {
			if got[i].Opcode != ins[i].Opcode || strings.Join(got[i].Args, "|") != strings.Join(ins[i].Args, "|") {
				t.Fatalf("unit %d: instruction %d = %+v, want %+v", unit, i, got[i], ins[i])
			}
		}
	}
}

func TestParseAllRejectsMalformed(t *testing.T) {
	for _, s := range []string{
		"4.size,1.0",           // no terminator
		"4.size;x",             // trailing garbage
		"a.size;",              // bad length
		"5.size;",              // length past the terminator
		"4.size:",              // bad separator
		"999999999.x;",         // too many digits
		".size;",               // empty length
		"2.😀;",                 // code point length 2 of a single emoji (UTF-16 = 2, code points = 1)
		"1.😀;",                 // in UTF-16 mode this ends inside the surrogate pair
		"4.size,1.1,1.2,1.3;x", // trailing garbage after a valid instruction
	} {
		if _, err := ParseAll(s, UTF16Units); err == nil && s != "2.😀;" {
			t.Errorf("ParseAll(%q) succeeded", s)
		}
	}
	// "2.😀;" is valid in UTF-16 units but not in code points.
	if _, err := ParseAll("2.😀;", UTF16Units); err != nil {
		t.Errorf("utf-16 emoji: %v", err)
	}
	if _, err := ParseAll("2.😀;", CodePoints); err == nil {
		t.Errorf("code point emoji accepted with length 2")
	}
	if _, err := ParseAll("1.😀;", CodePoints); err != nil {
		t.Errorf("code point emoji: %v", err)
	}
}

func TestReaderStreams(t *testing.T) {
	src := "4.sync,13.1700000000000,1.0;4.blob,1.1,8.QUJDREVG;5.error,9.Ünïcödé 😀,3.519;4.name,0.;"
	for name, r := range map[string]io.Reader{
		"whole":    strings.NewReader(src),
		"one-byte": iotest.OneByteReader(strings.NewReader(src)),
		"half":     iotest.HalfReader(strings.NewReader(src)),
	} {
		rd := NewReader(r, CodePoints)
		var ops []string
		for {
			in, raw, err := rd.ReadRaw()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !bytes.Equal(raw, in.Encode(CodePoints)) {
				t.Fatalf("%s: raw %q != encoded %q", name, raw, in.Encode(CodePoints))
			}
			ops = append(ops, in.Opcode+":"+strings.Join(in.Args, "|"))
		}
		want := "sync:1700000000000|0 blob:1|QUJDREVG error:Ünïcödé 😀|519 name:"
		if got := strings.Join(ops, " "); got != want {
			t.Fatalf("%s: got %q", name, got)
		}
	}
}

func TestReaderErrors(t *testing.T) {
	rd := NewReader(strings.NewReader("4.sync,2.1"), CodePoints)
	if _, err := rd.Read(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated: %v", err)
	}
	rd = NewReader(strings.NewReader("4.sync;x.y;"), CodePoints)
	if _, err := rd.Read(); err != nil {
		t.Fatal(err)
	}
	if _, err := rd.Read(); !errors.Is(err, ErrMalformed) {
		t.Fatalf("malformed: %v", err)
	}
	rd = NewReader(strings.NewReader("4.blob,20.0123456789abcdefghij;"), CodePoints)
	rd.MaxInstruction = 16
	if _, err := rd.Read(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	// Invalid UTF-8 bytes count as one unit each (like the replacement character).
	rd = NewReader(strings.NewReader("3.a\xffb;"), CodePoints)
	in, err := rd.Read()
	if err != nil || in.Opcode != "a\xffb" {
		t.Fatalf("invalid utf-8: %q %v", in.Opcode, err)
	}
}

func TestVersions(t *testing.T) {
	if !AtLeast(Version150, Version110) || AtLeast(Version100, Version110) || !AtLeast("VERSION_2_0_0", Version150) {
		t.Fatal("AtLeast")
	}
	if lowerVersion("VERSION_1_3_0", Version150) != Version130 || lowerVersion("VERSION_9_0_0", Version150) != Version150 ||
		lowerVersion("garbage", Version150) != Version150 {
		t.Fatal("lowerVersion")
	}
}

func TestStatusText(t *testing.T) {
	if StatusText(StatusUpstreamNotFound) == "" || !strings.Contains(StatusText(12345), "12345") {
		t.Fatal("StatusText")
	}
}
