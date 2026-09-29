package rdp

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"testing"
)

func pixels16(vals ...uint16) []byte {
	b := make([]byte, 2*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint16(b[2*i:], v)
	}
	return b
}

func TestRLEDecodeOrders(t *testing.T) {
	tests := []struct {
		name          string
		src           []byte
		width, height int
		want          []byte
	}{
		{
			name: "color run then background run copies the line above",
			// REGULAR_COLOR_RUN 4 × 0x1234, REGULAR_BG_RUN 4
			src:   []byte{0x60 | 4, 0x34, 0x12, 0x00 | 4},
			width: 4, height: 2,
			want: pixels16(0x1234, 0x1234, 0x1234, 0x1234, 0x1234, 0x1234, 0x1234, 0x1234),
		},
		{
			name:  "background run on the first line is black",
			src:   []byte{0x00 | 3},
			width: 3, height: 1,
			want: pixels16(0, 0, 0),
		},
		{
			name: "consecutive background runs insert a foreground pixel",
			// line 1: color image 1,2 ; line 2: BG run 1, BG run 1 → second starts with above ^ white
			src:   []byte{0x80 | 2, 0x01, 0x00, 0x02, 0x00, 0x00 | 1, 0x00 | 1},
			width: 2, height: 2,
			want: pixels16(1, 2, 1, 2^0xFFFF),
		},
		{
			name: "first-line foreground/background image uses fg and black",
			// LITE_SET_FG_FGBG_IMAGE with length 0 → next byte + 1 = 4 pixels, fg = 0x00AA, mask 0b0101
			src:   []byte{0xD0, 3, 0xAA, 0x00, 0x05},
			width: 4, height: 1,
			want: pixels16(0xAA, 0, 0xAA, 0),
		},
		{
			name: "foreground/background image past the first line XORs the line above",
			// line 1: color run 2 × 0x000F; then REGULAR_FGBG_IMAGE 1 byte ext (len 2) with mask 0b10 and default fg white
			src:   []byte{0x60 | 2, 0x0F, 0x00, 0x40, 1, 0x02},
			width: 2, height: 2,
			want: pixels16(0x0F, 0x0F, 0x0F, 0x0F^0xFFFF),
		},
		{
			name:  "white and black orders",
			src:   []byte{rleSpecialWhite, rleSpecialBlack},
			width: 2, height: 1,
			want: pixels16(0xFFFF, 0),
		},
		{
			name:  "dithered run",
			src:   []byte{0xE0 | 2, 0x01, 0x00, 0x02, 0x00},
			width: 4, height: 1,
			want: pixels16(1, 2, 1, 2),
		},
		{
			name:  "mega color image",
			src:   append([]byte{rleMegaColorImage, 3, 0}, pixels16(7, 8, 9)...),
			width: 3, height: 1,
			want: pixels16(7, 8, 9),
		},
		{
			name: "special fg/bg orders",
			// set fg with LITE_SET_FG_FG_RUN (len 1) on line 1 (8 px): pixel 0 = fg 0x0003; then SPECIAL_FGBG_1 (mask 0x03)
			// on the first line: 0x0003, 0x0003, then 6 × black → total 9 px > 8: use width 9.
			src:   []byte{0xC0 | 1, 0x03, 0x00, rleSpecialFGBG1},
			width: 9, height: 1,
			want: pixels16(3, 3, 3, 0, 0, 0, 0, 0, 0),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rleDecode(tt.src, tt.width, tt.height, 16)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("got % x\nwant % x", got, tt.want)
			}
		})
	}
}

func TestRLEDecodeRejectsMalformedStreams(t *testing.T) {
	bad := [][]byte{
		{0x60 | 4},              // color run without its pixel
		{0x60 | 9, 0x01, 0x00},  // run longer than the image
		{0x80 | 3, 0x01, 0x00},  // color image without its pixels
		{rleMegaColorRun, 0x10}, // truncated MEGA_MEGA length
		{0xA0},                  // undefined regular order
		{0xF5},                  // undefined MEGA order
		{0x40},                  // FGBG image with a missing length byte
		{0x40 | 1},              // FGBG image with a missing bitmask
		{rleSpecialWhite, rleSpecialWhite, rleSpecialWhite, rleSpecialWhite, rleSpecialWhite},
	}
	for _, src := range bad {
		if _, err := rleDecode(src, 2, 2, 16); err == nil {
			t.Errorf("% x: expected an error", src)
		}
	}
	for _, bpp := range []int{0, 1, 4, 32} {
		if _, err := rleDecode([]byte{0x60 | 1, 0}, 1, 1, bpp); err == nil {
			t.Errorf("bpp %d accepted", bpp)
		}
	}
}

func randomImage(r *rand.Rand, w, h, ps int, palette int) []byte {
	pix := make([]byte, w*h*ps)
	colors := make([]uint32, palette)
	for i := range colors {
		colors[i] = r.Uint32()
	}
	for i := 0; i < w*h; i++ {
		var v uint32
		switch {
		case i >= w && r.IntN(3) == 0: // vertical repetition
			v = pixelAt(pix, (i-w)*ps, ps)
		case i > 0 && r.IntN(2) == 0: // horizontal runs
			v = pixelAt(pix, (i-1)*ps, ps)
		default:
			v = colors[r.IntN(palette)]
		}
		putPixel(pix, i*ps, ps, v)
	}
	return pix
}

func TestRLERoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, bpp := range []int{8, 15, 16, 24} {
		ps := rlePixelSize(bpp)
		for range 200 {
			w, h := 1+r.IntN(90), 1+r.IntN(40)
			pix := randomImage(r, w, h, ps, 1+r.IntN(6))
			if bpp == 15 {
				for i := 0; i < len(pix); i += 2 {
					pix[i+1] &= 0x7F
				}
			}
			enc := rleEncode(pix, w, h, bpp)
			dec, err := rleDecode(enc, w, h, bpp)
			if err != nil {
				t.Fatalf("bpp %d %dx%d: %v", bpp, w, h, err)
			}
			if !bytes.Equal(dec, pix) {
				t.Fatalf("bpp %d %dx%d: round trip mismatch", bpp, w, h)
			}
		}
	}
}

func TestRLEEncodeLongRunsAndCompression(t *testing.T) {
	// A large flat image (runs longer than 65535 pixels) and one with only vertical repetition.
	w, h := 1000, 150
	flat := make([]byte, w*h*2)
	for i := 0; i < w*h; i++ {
		binary.LittleEndian.PutUint16(flat[2*i:], 0x4321)
	}
	enc := rleEncode(flat, w, h, 16)
	if len(enc) > 64 {
		t.Fatalf("flat image encoded to %d bytes", len(enc))
	}
	if dec, err := rleDecode(enc, w, h, 16); err != nil || !bytes.Equal(dec, flat) {
		t.Fatalf("flat round trip failed: %v", err)
	}
	stripes := make([]byte, w*h*2)
	for i := 0; i < w*h; i++ {
		binary.LittleEndian.PutUint16(stripes[2*i:], uint16(i%w*7))
	}
	enc = rleEncode(stripes, w, h, 16)
	if len(enc) > len(stripes)/50 {
		t.Fatalf("vertically repeating image encoded to %d of %d bytes", len(enc), len(stripes))
	}
	if dec, err := rleDecode(enc, w, h, 16); err != nil || !bytes.Equal(dec, stripes) {
		t.Fatalf("stripes round trip failed: %v", err)
	}
}

func FuzzRLEDecode(f *testing.F) {
	f.Add([]byte{0x60 | 4, 0x34, 0x12, 0x00 | 4}, uint8(4), uint8(2), uint8(16))
	f.Add([]byte{0xD0, 3, 0xAA, 0x00, 0x05}, uint8(4), uint8(1), uint8(8))
	f.Fuzz(func(t *testing.T, src []byte, w, h, bpp uint8) {
		pix, err := rleDecode(src, int(w), int(h), int(bpp))
		if err != nil {
			return
		}
		ps := rlePixelSize(int(bpp))
		if len(pix) != int(w)*int(h)*ps {
			t.Fatalf("decoded %d bytes for %dx%d", len(pix), w, h)
		}
		// Whatever decodes must re-encode losslessly.
		dec, err := rleDecode(rleEncode(pix, int(w), int(h), int(bpp)), int(w), int(h), int(bpp))
		if err != nil || !bytes.Equal(dec, pix) {
			t.Fatalf("re-encode mismatch: %v", err)
		}
	})
}
