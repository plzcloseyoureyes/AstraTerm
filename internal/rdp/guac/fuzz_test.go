package guac

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"
)

// FuzzParse feeds arbitrary bytes to both parsers: they must never panic or hang, and whatever parses must re-encode
// to the same bytes (lengths are canonical).
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"4.size,1.0,4.1024,3.768;",
		"0.,4.ping,13.1700000000000;",
		"4.file,1.6,15.application/pdf,13.report 😀.pdf;",
		"5.error,10.bad thing!,3.515;4.sync,4.1000;",
		"3.key,2.65,1.1;9.clipboard,1.1,10.text/plain;",
		"99999999.x;", "4.size,1.0", ";", "1.a,", "-1.a;", "2.é;", "1.😀;",
	} {
		f.Add([]byte(seed), false)
		f.Add([]byte(seed), true)
	}
	f.Fuzz(func(t *testing.T, data []byte, utf16 bool) {
		unit := CodePoints
		if utf16 {
			unit = UTF16Units
		}
		if ins, err := ParseAll(string(data), unit); err == nil && utf8.Valid(data) {
			// Re-encoding is canonical (lengths may have had leading zeros): parsing it gives the same instructions.
			var b []byte
			for _, in := range ins {
				b = in.Append(b, unit)
			}
			again, err := ParseAll(string(b), unit)
			if err != nil || len(again) != len(ins) {
				t.Fatalf("re-encoded %q: %v", b, err)
			}
			for i := range ins {
				if again[i].Opcode != ins[i].Opcode || !slices.Equal(again[i].Args, ins[i].Args) {
					t.Fatalf("round trip of %q changed instruction %d", data, i)
				}
			}
		}
		// The streaming reader, one byte at a time (guacd's TCP stream may split anywhere).
		r := NewReader(iotest.OneByteReader(bytes.NewReader(data)), unit)
		var out []byte
		for range len(data) + 1 {
			in, raw, err := r.ReadRaw()
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, ErrMalformed) &&
					!errors.Is(err, ErrTooLarge) {
					t.Fatalf("reader error %v", err)
				}
				break
			}
			if utf8.Valid(raw) {
				if again, err := ParseAll(string(in.Encode(unit)), unit); err != nil || len(again) != 1 || again[0].Opcode != in.Opcode ||
					!slices.Equal(again[0].Args, in.Args) {
					t.Fatalf("raw %q does not round trip: %v", raw, err)
				}
			}
			out = append(out, raw...)
		}
		if !bytes.HasPrefix(data, out) {
			t.Fatalf("reader output %q is not a prefix of the input %q", out, data)
		}
	})
}

// FuzzRoundTrip builds instructions from arbitrary strings: encoding then parsing gives them back, in both units.
func FuzzRoundTrip(f *testing.F) {
	f.Add("clipboard", "1", "text/plain", "héllo 😀,;.")
	f.Fuzz(func(t *testing.T, op, a, b, c string) {
		if !utf8.ValidString(op + a + b + c) {
			return
		}
		want := New(op, a, b, c)
		for _, unit := range []LengthUnit{CodePoints, UTF16Units} {
			enc := want.Encode(unit)
			got, err := ParseAll(string(enc), unit)
			if err != nil || len(got) != 1 || got[0].Opcode != op || !slices.Equal(got[0].Args, want.Args) {
				t.Fatalf("unit %d: %q → %+v %v", unit, enc, got, err)
			}
			in, err := NewReader(iotest.OneByteReader(strings.NewReader(string(enc)+string(enc))), unit).Read()
			if err != nil || in.Opcode != op || !slices.Equal(in.Args, want.Args) {
				t.Fatalf("reader unit %d: %+v %v", unit, in, err)
			}
		}
	})
}
