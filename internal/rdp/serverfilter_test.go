package rdp

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
)

// ---- builders -------------------------------------------------------------------------------------------------------------

// testRect builds a TS_BITMAP_DATA for the destination (left, top, cx × cy) whose bitmap is w × h pixels of bpp (the
// visible part is img, top-down cx × cy; the padding repeats each row's last pixel, as xrdp does).
func testRect(left, top, cx, cy, w, h, bpp int, img []byte, compressed, compHeader bool) []byte {
	ps := (bpp + 7) / 8
	pix := make([]byte, 0, w*h*ps)
	for row := h - 1; row >= 0; row-- { // bottom-up
		y := min(row, cy-1)
		line := img[y*cx*ps : (y+1)*cx*ps]
		pix = append(pix, line...)
		for x := cx; x < w; x++ {
			pix = append(pix, line[(cx-1)*ps:cx*ps]...)
		}
	}
	var body []byte
	flags := 0
	if compressed {
		body = rleEncode(pix, w, h, bpp)
		flags = bitmapCompression | noBitmapCompressionHdr
		if compHeader {
			hdr := make([]byte, 8)
			binary.LittleEndian.PutUint16(hdr[2:], uint16(len(body)))
			binary.LittleEndian.PutUint16(hdr[4:], uint16(w*ps))
			binary.LittleEndian.PutUint16(hdr[6:], uint16(w*h*ps))
			body = append(hdr, body...)
			flags = bitmapCompression
		}
	} else {
		row := w * ps
		padded := (row + 3) &^ 3
		for y := range h {
			body = append(body, pix[y*row:(y+1)*row]...)
			body = append(body, make([]byte, padded-row)...)
		}
	}
	b := make([]byte, bitmapDataFixed)
	for i, v := range []int{left, top, left + cx - 1, top + cy - 1, w, h, bpp, flags, len(body)} {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return append(b, body...)
}

func bitmapUpdateData(rects ...[]byte) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint16(b, updateTypeBitmap)
	binary.LittleEndian.PutUint16(b[2:], uint16(len(rects)))
	for _, r := range rects {
		b = append(b, r...)
	}
	return b
}

// slowPathPDU wraps update data like xrdp does (uncompressedLength = compressedLength = share total length).
func slowPathPDU(data []byte) []byte {
	total := shareHeadersLen + len(data)
	share := make([]byte, shareHeadersLen)
	binary.LittleEndian.PutUint16(share[0:], uint16(total))
	binary.LittleEndian.PutUint16(share[2:], 0x17)
	binary.LittleEndian.PutUint16(share[4:], 1005)
	binary.LittleEndian.PutUint32(share[6:], 0x103ea)
	share[11] = 1
	binary.LittleEndian.PutUint16(share[12:], uint16(total))
	share[14] = pduType2Update
	binary.LittleEndian.PutUint16(share[16:], uint16(total))
	return appendSlowPath(nil, []byte{0x68, 0x00, 0x04, 0x03, 0xEB, 0x70}, share, data, data)
}

func fastPathPDU(updates ...[]byte) []byte { return packFastPath(nil, updates) }

func fastPathFragment(code, frag byte, data []byte) []byte {
	u := []byte{code | frag<<4, byte(len(data)), byte(len(data) >> 8)}
	return append(u, data...)
}

func testImage(r *rand.Rand, cx, cy, ps int) []byte { return randomImage(r, cx, cy, ps, 5) }

// ---- a screen that applies bitmap updates ----------------------------------------------------------------------------------

type screen struct {
	w, h, ps int
	pix      []byte
}

func newScreen(w, h, ps int) *screen { return &screen{w: w, h: h, ps: ps, pix: make([]byte, w*h*ps)} }

// apply draws a rectangle. strideFromRect models IronRDP 0.7 (rows of the decoded bitmap are taken with the
// destination width), otherwise the bitmap width is used and the bitmap clipped (the protocol semantics).
func (s *screen) apply(r bitmapRect, strideFromRect bool) error {
	var buf []byte
	ps := (r.bpp + 7) / 8
	if r.flags&bitmapCompression != 0 {
		var err error
		if buf, err = rleDecode(r.data, r.width, r.height, r.bpp); err != nil {
			return err
		}
	} else {
		row := r.width * ps
		padded := (row + 3) &^ 3
		for y := range r.height {
			if (y+1)*padded > len(r.data) && y*padded+row > len(r.data) {
				return fmt.Errorf("short raw bitmap")
			}
			buf = append(buf, r.data[y*padded:y*padded+row]...)
		}
	}
	cx, cy := r.destWidth(), r.destHeight()
	stride := r.width
	if strideFromRect {
		stride = cx
	}
	rows := len(buf) / (stride * ps)
	for i := range rows { // i = 0 is the top row: scanlines are bottom-up
		src := buf[(rows-1-i)*stride*ps:]
		y := r.top + i
		if y > r.bottom && !strideFromRect || y >= s.h {
			continue
		}
		for x := 0; x < cx; x++ {
			copy(s.pix[((y*s.w)+r.left+x)*ps:], src[x*ps:(x+1)*ps])
		}
	}
	_ = cy
	return nil
}

// ---- PDU validation --------------------------------------------------------------------------------------------------------

// replay parses a server → client byte stream (slow-path / fast-path output after the connection sequence), validates
// every header and applies all bitmap rectangles to s.
func replay(t *testing.T, stream []byte, s *screen, strideFromRect bool) (rects int) {
	t.Helper()
	var frag []byte
	for len(stream) > 0 {
		switch {
		case stream[0] == fpActionX224:
			n := int(binary.BigEndian.Uint16(stream[2:]))
			if n > len(stream) || n < 7 {
				t.Fatalf("TPKT length %d, %d bytes left", n, len(stream))
			}
			pdu := stream[:n]
			stream = stream[n:]
			_, share, upd, ok := parseSlowPathUpdate(pdu)
			if !ok {
				continue // not an update: nothing to draw
			}
			total := int(binary.LittleEndian.Uint16(share))
			if total != shareHeadersLen+len(upd) || total > maxPERLength {
				t.Fatalf("share total length %d for %d bytes", total, shareHeadersLen+len(upd))
			}
			if unc := int(binary.LittleEndian.Uint16(share[12:])); unc != total {
				t.Fatalf("uncompressedLength %d, total %d", unc, total)
			}
			rects += drawUpdate(t, upd, s, strideFromRect)
		case stream[0]&0x3F == 0:
			n, hdr := int(stream[1]), 2
			if stream[1]&0x80 != 0 {
				n, hdr = int(stream[1]&0x7F)<<8|int(stream[2]), 3
			}
			if n > len(stream) || n > fpMaxPDU {
				t.Fatalf("fast-path length %d, %d bytes left", n, len(stream))
			}
			updates, ok := parseFastPathUpdates(stream[hdr:n])
			if !ok {
				t.Fatalf("malformed fast-path PDU % x", stream[:min(n, 32)])
			}
			stream = stream[n:]
			for _, u := range updates {
				switch u.frag {
				case fpFragSingle:
					if frag != nil {
						t.Fatal("single update inside a fragmented one")
					}
					if u.code == fpUpdateBitmap {
						rects += drawUpdate(t, u.data, s, strideFromRect)
					}
				case fpFragFirst:
					frag = append([]byte(nil), u.data...)
				case fpFragNext:
					frag = append(frag, u.data...)
				case fpFragLast:
					frag = append(frag, u.data...)
					if u.code == fpUpdateBitmap {
						rects += drawUpdate(t, frag, s, strideFromRect)
					}
					frag = nil
				}
			}
		default:
			t.Fatalf("unexpected byte %#x", stream[0])
		}
	}
	return rects
}

func drawUpdate(t *testing.T, data []byte, s *screen, strideFromRect bool) int {
	t.Helper()
	rs, ok := parseBitmapUpdate(data)
	if !ok {
		t.Fatalf("malformed bitmap update")
	}
	for _, r := range rs {
		if err := s.apply(r, strideFromRect); err != nil {
			t.Fatal(err)
		}
	}
	return len(rs)
}

func filterAll(t *testing.T, selected uint32, input []byte, chunk int) ([]byte, *serverFilter) {
	t.Helper()
	var out bytes.Buffer
	f := newServerFilter(&out, selected)
	for len(input) > 0 {
		n := min(chunk, len(input))
		if chunk <= 0 {
			n = len(input)
		}
		if _, err := f.Write(input[:n]); err != nil {
			t.Fatal(err)
		}
		input = input[n:]
	}
	return out.Bytes(), f
}

// ---- tests ------------------------------------------------------------------------------------------------------------------

func TestServerFilterFixesPaddedBitmaps(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for _, bpp := range []int{8, 15, 16, 24, 32} {
		for _, compressed := range []bool{true, false} {
			if bpp == 32 && compressed {
				continue // planar: not handled
			}
			ps := (bpp + 7) / 8
			const W, H = 300, 200
			var slow, fast []byte
			want := newScreen(W, H, ps)
			for i := range 30 {
				cx, cy := 1+r.IntN(70), 1+r.IntN(40)
				w, h := (cx+3)&^3, cy
				if i%3 == 0 {
					h = cy + r.IntN(3) // taller bitmaps are clipped too
				}
				x, y := r.IntN(W-cx), r.IntN(H-cy)
				img := testImage(r, cx, cy, ps)
				rect := testRect(x, y, cx, cy, w, h, bpp, img, compressed, i%2 == 0)
				rs, _ := parseBitmapUpdate(bitmapUpdateData(rect))
				if err := want.apply(rs[0], false); err != nil {
					t.Fatal(err)
				}
				data := bitmapUpdateData(rect)
				slow = append(slow, slowPathPDU(data)...)
				fast = append(fast, fastPathPDU(encodeFastPathUpdate(fpUpdateBitmap, data))...)
			}
			for name, stream := range map[string][]byte{"slow-path": slow, "fast-path": fast} {
				for _, chunk := range []int{0, 1, 7, 1000} {
					out, f := filterAll(t, protoSSL, stream, chunk)
					if f.state == filterPassthrough {
						t.Fatalf("bpp %d %s: filter gave up", bpp, name)
					}
					got := newScreen(W, H, ps)
					replay(t, out, got, true)
					if !bytes.Equal(got.pix, want.pix) {
						t.Fatalf("bpp %d compressed %v %s chunk %d: IronRDP 0.7 model renders a different image", bpp, compressed, name, chunk)
					}
					// Without the filter the model shows the bug (sanity check of the test itself).
					bad := newScreen(W, H, ps)
					replay(t, stream, bad, true)
					if bytes.Equal(bad.pix, want.pix) {
						t.Fatalf("bpp %d %s: the unfiltered stream renders correctly — test does not exercise the bug", bpp, name)
					}
				}
			}
		}
	}
}

func TestServerFilterRealXrdpUpdates(t *testing.T) {
	fh, err := os.Open("testdata/xrdp_bitmap_pdus.hex")
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	var stream []byte
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		b, err := hex.DecodeString(strings.TrimSpace(sc.Text()))
		if err != nil {
			t.Fatal(err)
		}
		stream = append(stream, b...)
	}
	if len(stream) == 0 {
		t.Fatal("no vectors")
	}
	want := newScreen(1920, 1200, 2)
	n := replay(t, stream, want, false)
	out, f := filterAll(t, protoSSL, stream, 333)
	if f.fixed == 0 || f.state == filterPassthrough {
		t.Fatalf("fixed %d rectangles, state %d", f.fixed, f.state)
	}
	got := newScreen(1920, 1200, 2)
	if m := replay(t, out, got, true); m < n {
		t.Fatalf("%d rectangles out, %d in", m, n)
	}
	if !bytes.Equal(got.pix, want.pix) {
		t.Fatal("xrdp updates render differently after the filter")
	}
}

func TestServerFilterLargeAndFragmentedUpdates(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	const W, H = 1400, 300
	want := newScreen(W, H, 2)
	// A noisy 1301 × 25 raw bitmap (padded to 1304; 65 200 bytes, about the largest a TS_BITMAP_DATA can carry): it
	// arrives fragmented and, re-encoded, must be tiled below the size limits.
	cx, cy := 1301, 25
	img := make([]byte, cx*cy*2)
	for i := range img {
		img[i] = byte(r.Uint32())
	}
	rect := testRect(20, 10, cx, cy, 1304, cy, 16, img, false, false)
	rs, _ := parseBitmapUpdate(bitmapUpdateData(rect))
	if err := want.apply(rs[0], false); err != nil {
		t.Fatal(err)
	}
	data := bitmapUpdateData(rect)
	// Fragmented fast-path update (FIRST / NEXT … / LAST), with a pointer update before it in the same PDU.
	var stream []byte
	pointer := fastPathFragment(0x5, fpFragSingle, nil) // PTR_NULL
	chunks := fragmentUpdate(encodeFastPathUpdate(fpUpdateBitmap, data), 20000)
	stream = append(stream, fastPathPDU(pointer, chunks[0])...)
	for _, c := range chunks[1:] {
		stream = append(stream, fastPathPDU(c)...)
	}
	out, f := filterAll(t, protoSSL, stream, 4096)
	if f.fixed != 1 {
		t.Fatalf("fixed %d", f.fixed)
	}
	got := newScreen(W, H, 2)
	if n := replay(t, out, got, true); n < 2 {
		t.Fatalf("expected the rectangle to be tiled, got %d rectangles", n)
	}
	if !bytes.Equal(got.pix, want.pix) {
		t.Fatal("tiled rendering differs")
	}
	// Over slow-path (at most 16 KiB per PDU) a noisy 8 KiB bitmap is re-encoded into several PDUs, each within the
	// MCS length limit (checked by replay).
	img = make([]byte, 61*60*2)
	for i := range img {
		img[i] = byte(r.Uint32())
	}
	slow := slowPathPDU(bitmapUpdateData(testRect(20, 10, 61, 60, 64, 60, 16, img, false, false)))
	want = newScreen(W, H, 2)
	replay(t, slow, want, false)
	out, _ = filterAll(t, protoSSL, slow, 0)
	got = newScreen(W, H, 2)
	replay(t, out, got, true)
	if !bytes.Equal(got.pix, want.pix) {
		t.Fatal("slow-path tiled rendering differs")
	}
}

func TestServerFilterPassesOtherTrafficUnchanged(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	img := testImage(r, 16, 8, 2)
	aligned := bitmapUpdateData(testRect(0, 0, 16, 8, 16, 8, 16, img, true, false)) // width = destination: untouched
	var stream []byte
	stream = append(stream, 0x03, 0x00, 0x00, 0x0B, 0x02, 0xF0, 0x80, 0x7F, 0x66, 0x01, 0x02) // TPKT, not an SDin update
	stream = append(stream, slowPathPDU(aligned)...)
	stream = append(stream, fastPathPDU(encodeFastPathUpdate(fpUpdateBitmap, aligned), fastPathFragment(0xB, 0, []byte{1, 2, 3}))...)
	stream = append(stream, 0x00, 0x05, 0x05, 0x00, 0x00) // tiny fast-path PDU with a one-byte-length header
	out, f := filterAll(t, protoSSL, stream, 3)
	if !bytes.Equal(out, stream) || f.fixed != 0 {
		t.Fatalf("stream changed")
	}
}

func TestServerFilterCredSSPFraming(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 10))
	img := testImage(r, 10, 5, 2)
	upd := slowPathPDU(bitmapUpdateData(testRect(3, 4, 10, 5, 12, 5, 16, img, true, false)))
	tsreq := append([]byte{0x30, 0x82, 0x01, 0x00}, make([]byte, 256)...)
	tsreq2 := []byte{0x30, 0x03, 0xA0, 0x01, 0x06}
	for _, tc := range []struct {
		name     string
		selected uint32
		stream   []byte
	}{
		{"HYBRID", protoSSL | protoHybrid, concat(tsreq, tsreq2, upd)},
		{"HYBRID_EX", protoHybridEx, concat(tsreq, tsreq2, []byte{0, 0, 0, 0}, upd)},
		{"TLS", protoSSL, upd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, f := filterAll(t, tc.selected, tc.stream, 5)
			if f.fixed != 1 || f.state != filterRDP {
				t.Fatalf("fixed %d state %d", f.fixed, f.state)
			}
			prefix := len(tc.stream) - len(upd)
			if !bytes.Equal(out[:prefix], tc.stream[:prefix]) {
				t.Fatal("CredSSP messages changed")
			}
			got := newScreen(40, 20, 2)
			replay(t, out[prefix:], got, true)
			want := newScreen(40, 20, 2)
			replay(t, upd, want, false)
			if !bytes.Equal(got.pix, want.pix) {
				t.Fatal("rendering differs")
			}
		})
	}
}

func TestServerFilterGivesUpOnUnknownData(t *testing.T) {
	stream := concat([]byte{0x42, 0x01, 0x02}, slowPathPDU(bitmapUpdateData(testRect(0, 0, 3, 1, 4, 1, 16, make([]byte, 6), true, false))))
	out, f := filterAll(t, protoSSL, stream, 2)
	if f.state != filterPassthrough || !bytes.Equal(out, stream) {
		t.Fatal("unknown data must switch the filter to pass-through")
	}
}

func concat(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

func FuzzServerFilter(f *testing.F) {
	r := rand.New(rand.NewPCG(11, 12))
	img := testImage(r, 5, 3, 2)
	f.Add(slowPathPDU(bitmapUpdateData(testRect(1, 1, 5, 3, 8, 3, 16, img, true, false))), uint8(3))
	f.Add(fastPathPDU(encodeFastPathUpdate(fpUpdateBitmap, bitmapUpdateData(testRect(1, 1, 5, 3, 8, 3, 16, img, false, false)))), uint8(1))
	f.Add([]byte{0x30, 0x82, 0xFF, 0xFF, 1}, uint8(0))
	f.Fuzz(func(t *testing.T, data []byte, mode uint8) {
		selected := []uint32{protoSSL, protoHybrid, protoHybridEx}[int(mode)%3]
		var out bytes.Buffer
		fl := newServerFilter(&out, selected)
		for i := 0; i < len(data); i += 1 + int(mode)%5 {
			_, _ = fl.Write(data[i:min(len(data), i+1+int(mode)%5)])
		}
		if fl.fixed == 0 && fl.frag == nil && !fl.heldFragments {
			if want := data[:len(data)-len(fl.pending)]; !bytes.Equal(out.Bytes(), want) {
				t.Fatalf("unchanged stream altered:\n in % x\nout % x", want, out.Bytes())
			}
		}
	})
}
