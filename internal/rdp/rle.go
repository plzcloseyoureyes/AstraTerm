package rdp

import (
	"encoding/binary"
	"errors"
)

// Interleaved RLE bitmap codec ([MS-RDPBCGR] 2.2.9.1.1.3.1.2.4 and 3.1.9) for 8, 15, 16 and 24 bpp. The decoder
// follows the reference pseudo-code (and FreeRDP / IronRDP) exactly, including the first-scanline rules and the
// foreground pixel inserted between consecutive background runs. The encoder only emits color runs, color images and
// background runs (never two background runs in a row, never a background run starting on the first scanline), which
// every decoder interprets the same way.
//
// Bitmaps are stored bottom-up (the first scanline of the stream is the bottom row); the codec itself is agnostic of
// that: "first line" is the first scanline of the buffer.

var errRLE = errors.New("malformed RLE bitmap stream")

// rlePixelSize is the number of bytes per pixel of an interleaved RLE color depth (0 when unsupported).
func rlePixelSize(bpp int) int {
	switch bpp {
	case 8:
		return 1
	case 15, 16:
		return 2
	case 24:
		return 3
	}
	return 0
}

func rleWhite(bpp int) uint32 {
	switch bpp {
	case 8:
		return 0xFF
	case 15:
		return 0x7FFF
	case 16:
		return 0xFFFF
	}
	return 0xFFFFFF
}

// Compression order codes.
const (
	rleRegularBGRun       = 0x00
	rleRegularFGRun       = 0x01
	rleRegularFGBGImage   = 0x02
	rleRegularColorRun    = 0x03
	rleRegularColorImage  = 0x04
	rleLiteSetFGFGRun     = 0x0C
	rleLiteSetFGFGBGImage = 0x0D
	rleLiteDitheredRun    = 0x0E
	rleMegaBGRun          = 0xF0
	rleMegaFGRun          = 0xF1
	rleMegaFGBGImage      = 0xF2
	rleMegaColorRun       = 0xF3
	rleMegaColorImage     = 0xF4
	rleMegaSetFGRun       = 0xF6
	rleMegaSetFGBGImage   = 0xF7
	rleMegaDitheredRun    = 0xF8
	rleSpecialFGBG1       = 0xF9
	rleSpecialFGBG2       = 0xFA
	rleSpecialWhite       = 0xFD
	rleSpecialBlack       = 0xFE
	rleMaskSpecialFGBG1   = 0x03
	rleMaskSpecialFGBG2   = 0x05
	rleMaxRun             = 0xFFFF // longest MEGA_MEGA run
	rleRegularShortMaxRun = 31
	rleRegularExtMaxRun   = 255 + 32
	rleMinColorRun        = 3 // shorter runs are cheaper inside a color image
	rleMinBackgroundRun   = 4
)

// rleCode extracts the order code of an order header byte.
func rleCode(h byte) byte {
	switch {
	case h&0xC0 != 0xC0: // regular orders: 000x xxxx … 100x xxxx
		return h >> 5
	case h&0xF0 == 0xF0: // MEGA_MEGA and special orders
		return h
	}
	return h >> 4 // lite orders: 1100 xxxx, 1101 xxxx, 1110 xxxx
}

type rleDecoder struct {
	src []byte
	sp  int
	dst []byte
	dp  int
	ps  int // bytes per pixel
	row int // scanline length in bytes
}

func (d *rleDecoder) next() (byte, bool) {
	if d.sp >= len(d.src) {
		return 0, false
	}
	b := d.src[d.sp]
	d.sp++
	return b, true
}

func (d *rleDecoder) readPixel() (uint32, bool) {
	if d.sp+d.ps > len(d.src) {
		return 0, false
	}
	v := pixelAt(d.src, d.sp, d.ps)
	d.sp += d.ps
	return v, true
}

// room reports whether n more pixels fit into the destination.
func (d *rleDecoder) room(n int) bool { return n >= 0 && d.dp+n*d.ps <= len(d.dst) }

func (d *rleDecoder) put(v uint32) {
	putPixel(d.dst, d.dp, d.ps, v)
	d.dp += d.ps
}

// above is the pixel one scanline above the write position (only called past the first scanline).
func (d *rleDecoder) above() uint32 { return pixelAt(d.dst, d.dp-d.row, d.ps) }

func pixelAt(b []byte, off, ps int) uint32 {
	switch ps {
	case 1:
		return uint32(b[off])
	case 2:
		return uint32(binary.LittleEndian.Uint16(b[off:]))
	}
	return uint32(b[off]) | uint32(b[off+1])<<8 | uint32(b[off+2])<<16
}

func putPixel(b []byte, off, ps int, v uint32) {
	switch ps {
	case 1:
		b[off] = byte(v)
	case 2:
		binary.LittleEndian.PutUint16(b[off:], uint16(v))
	default:
		b[off], b[off+1], b[off+2] = byte(v), byte(v>>8), byte(v>>16)
	}
}

// runLength extracts the run length of an order (the header byte h is already consumed).
func (d *rleDecoder) runLength(code, h byte) (int, error) {
	ext := func(add int) (int, error) {
		b, ok := d.next()
		if !ok {
			return 0, errRLE
		}
		return int(b) + add, nil
	}
	switch code {
	case rleRegularFGBGImage:
		if n := int(h & 0x1F); n != 0 {
			return n * 8, nil
		}
		return ext(1)
	case rleLiteSetFGFGBGImage:
		if n := int(h & 0x0F); n != 0 {
			return n * 8, nil
		}
		return ext(1)
	case rleRegularBGRun, rleRegularFGRun, rleRegularColorRun, rleRegularColorImage:
		if n := int(h & 0x1F); n != 0 {
			return n, nil
		}
		return ext(32)
	case rleLiteSetFGFGRun, rleLiteDitheredRun:
		if n := int(h & 0x0F); n != 0 {
			return n, nil
		}
		return ext(16)
	case rleMegaBGRun, rleMegaFGRun, rleMegaSetFGRun, rleMegaDitheredRun, rleMegaColorRun, rleMegaFGBGImage,
		rleMegaSetFGBGImage, rleMegaColorImage:
		if d.sp+2 > len(d.src) {
			return 0, errRLE
		}
		n := int(binary.LittleEndian.Uint16(d.src[d.sp:]))
		d.sp += 2
		return n, nil
	}
	return 0, nil
}

// fgbg writes up to 8 pixels of a foreground/background image: set bits are the foreground (XORed with the pixel
// above past the first scanline), clear bits the background (black on the first scanline, else the pixel above).
func (d *rleDecoder) fgbg(mask byte, fg uint32, bits int, first bool) bool {
	if !d.room(bits) {
		return false
	}
	for i := range bits {
		set := mask&(1<<uint(i)) != 0
		switch {
		case first && set:
			d.put(fg)
		case first:
			d.put(0)
		case set:
			d.put(d.above() ^ fg)
		default:
			d.put(d.above())
		}
	}
	return true
}

// rleDecode decompresses an interleaved RLE bitmap stream into width × height pixels of bpp (little-endian pixel
// values, scanlines in stream order). Pixels the stream does not cover stay zero.
func rleDecode(src []byte, width, height, bpp int) ([]byte, error) {
	ps := rlePixelSize(bpp)
	if ps == 0 || width <= 0 || height <= 0 || width*height > 1<<26 {
		return nil, errRLE
	}
	d := &rleDecoder{src: src, dst: make([]byte, width*height*ps), ps: ps, row: width * ps}
	fg := rleWhite(bpp)
	insertFG, first := false, true
	for d.sp < len(d.src) {
		if first && d.dp >= d.row {
			first, insertFG = false, false
		}
		h := d.src[d.sp]
		d.sp++
		code := rleCode(h)
		n, err := d.runLength(code, h)
		if err != nil {
			return nil, err
		}
		if code == rleRegularBGRun || code == rleMegaBGRun {
			if !d.room(n) {
				return nil, errRLE
			}
			if insertFG && n > 0 {
				if first {
					d.put(fg)
				} else {
					d.put(d.above() ^ fg)
				}
				n--
			}
			for range n {
				if first {
					d.put(0)
				} else {
					d.put(d.above())
				}
			}
			insertFG = true // a follow-on background run needs a foreground pixel inserted
			continue
		}
		insertFG = false
		switch code {
		case rleRegularFGRun, rleMegaFGRun, rleLiteSetFGFGRun, rleMegaSetFGRun:
			if code == rleLiteSetFGFGRun || code == rleMegaSetFGRun {
				v, ok := d.readPixel()
				if !ok {
					return nil, errRLE
				}
				fg = v
			}
			if !d.room(n) {
				return nil, errRLE
			}
			for range n {
				if first {
					d.put(fg)
				} else {
					d.put(d.above() ^ fg)
				}
			}
		case rleLiteDitheredRun, rleMegaDitheredRun:
			a, ok1 := d.readPixel()
			b, ok2 := d.readPixel()
			if !ok1 || !ok2 || !d.room(2*n) {
				return nil, errRLE
			}
			for range n {
				d.put(a)
				d.put(b)
			}
		case rleRegularColorRun, rleMegaColorRun:
			v, ok := d.readPixel()
			if !ok || !d.room(n) {
				return nil, errRLE
			}
			for range n {
				d.put(v)
			}
		case rleRegularFGBGImage, rleMegaFGBGImage, rleLiteSetFGFGBGImage, rleMegaSetFGBGImage:
			if code == rleLiteSetFGFGBGImage || code == rleMegaSetFGBGImage {
				v, ok := d.readPixel()
				if !ok {
					return nil, errRLE
				}
				fg = v
			}
			for n > 0 {
				bits := min(8, n)
				mask, ok := d.next()
				if !ok || !d.fgbg(mask, fg, bits, first) {
					return nil, errRLE
				}
				n -= bits
			}
		case rleRegularColorImage, rleMegaColorImage:
			size := n * ps
			if d.sp+size > len(d.src) || !d.room(n) {
				return nil, errRLE
			}
			copy(d.dst[d.dp:], d.src[d.sp:d.sp+size])
			d.sp += size
			d.dp += size
		case rleSpecialFGBG1:
			if !d.fgbg(rleMaskSpecialFGBG1, fg, 8, first) {
				return nil, errRLE
			}
		case rleSpecialFGBG2:
			if !d.fgbg(rleMaskSpecialFGBG2, fg, 8, first) {
				return nil, errRLE
			}
		case rleSpecialWhite:
			if !d.room(1) {
				return nil, errRLE
			}
			d.put(rleWhite(bpp))
		case rleSpecialBlack:
			if !d.room(1) {
				return nil, errRLE
			}
			d.put(0)
		default:
			return nil, errRLE
		}
	}
	return d.dst, nil
}

// rleEncode compresses width × height pixels of bpp (as produced by rleDecode) into an interleaved RLE stream.
func rleEncode(pix []byte, width, height, bpp int) []byte {
	ps := rlePixelSize(bpp)
	n := width * height
	if ps == 0 || n <= 0 || len(pix) < n*ps {
		return nil
	}
	px := make([]uint32, n)
	for i := range px {
		px[i] = pixelAt(pix, i*ps, ps)
	}
	out := make([]byte, 0, len(pix)/4+16)
	order := func(code byte, run int) {
		// regular: 1..31 in the header, 32..287 in an extra byte; longer runs use the MEGA_MEGA form.
		switch {
		case run <= rleRegularShortMaxRun:
			out = append(out, code<<5|byte(run))
		case run <= rleRegularExtMaxRun:
			out = append(out, code<<5, byte(run-32))
		default:
			out = append(out, 0xF0|code, byte(run), byte(run>>8))
		}
	}
	appendPixel := func(v uint32) {
		switch ps {
		case 1:
			out = append(out, byte(v))
		case 2:
			out = append(out, byte(v), byte(v>>8))
		default:
			out = append(out, byte(v), byte(v>>8), byte(v>>16))
		}
	}
	lit := -1       // start of the pending literal pixels (color image)
	lastBG := false // the previous order was a background run
	flush := func(end int) {
		for lit >= 0 && lit < end {
			k := min(end-lit, rleMaxRun)
			order(rleRegularColorImage, k)
			for _, v := range px[lit : lit+k] {
				appendPixel(v)
			}
			lit += k
			lastBG = false
		}
		lit = -1
	}
	for i := 0; i < n; {
		bg := 0
		if i >= width && !(lastBG && lit < 0) {
			for bg < rleMaxRun && i+bg < n && px[i+bg] == px[i+bg-width] {
				bg++
			}
		}
		run := 1
		for run < rleMaxRun && i+run < n && px[i+run] == px[i] {
			run++
		}
		switch {
		case bg >= rleMinBackgroundRun && bg >= run:
			flush(i)
			order(rleRegularBGRun, bg)
			lastBG = true
			i += bg
		case run >= rleMinColorRun:
			flush(i)
			order(rleRegularColorRun, run)
			appendPixel(px[i])
			lastBG = false
			i += run
		default:
			if lit < 0 {
				lit = i
			}
			i++
			if i-lit >= rleMaxRun {
				flush(i)
			}
		}
	}
	flush(n)
	return out
}
