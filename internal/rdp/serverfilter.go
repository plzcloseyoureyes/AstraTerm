package rdp

import (
	"encoding/binary"
	"io"
)

// serverFilter sits between the RDP server's TLS plaintext and the IronRDP web client in the RDCleanPath relay and
// repairs bitmap updates the client would render wrongly.
//
// RDP servers send legacy bitmap updates ([MS-RDPBCGR] 2.2.9.1.1.3.1.2.2 TS_BITMAP_DATA) whose bitmap is wider than
// its destination rectangle: the bitmap width is padded to a multiple of 4 pixels and the destination rectangle clips
// it (xrdp pads every bitmap; its 350 px logon dialog travels as 352 px bitmaps). IronRDP 0.7 (the published
// @devolutions/iron-remote-desktop-rdp) uses the destination width as the row stride of the decoded bitmap, so every
// padded bitmap is drawn sheared (fixed upstream after the 0.7 release). The filter decodes such rectangles
// (interleaved RLE or raw), crops them to the visible rectangle and re-encodes them with bitmap width = destination
// width — lossless, and still correct for clients without the bug.
//
// It also smooths server-initiated reactivations (the Deactivation-Reactivation Sequence xrdp runs when the client
// resizes the display): xrdp sends a truncated Deactivate All PDU (only the share control header) that IronRDP 0.7
// cannot decode — the filter completes it — and keeps sending output before the client finished the reactivation,
// which IronRDP 0.7 rejects ("unexpected server message") — the filter holds that output back until the server's
// Font Map PDU and then releases it in order.
//
// The filter follows the server → client byte stream: CredSSP TSRequests (DER) while NLA runs, the 4-byte Early User
// Authorization Result of HYBRID_EX, then slow-path (TPKT) and fast-path output PDUs. Bitmap updates are rewritten in
// both; everything else is forwarded byte for byte. Anything it does not understand switches it to plain
// pass-through for the rest of the connection, so it cannot break a stream it cannot parse.
type serverFilter struct {
	out     io.Writer
	state   int
	euar    bool // HYBRID_EX: the Early User Authorization Result follows CredSSP
	pending []byte
	obuf    []byte
	frag    *fpFragments
	fixed   int // rectangles rewritten (diagnostics)
	// heldFragments: a fragmented bitmap update was reassembled (its fragments are re-packed on output).
	heldFragments bool

	// Server-initiated reactivation (Deactivate All → Demand Active → … → Font Map, e.g. after a display control
	// resize): IronRDP 0.7 only accepts the activation PDUs until the server's Font Map, while xrdp keeps painting.
	// Other output is held back meanwhile and released after the Font Map.
	reactivating  bool
	held          []byte
	shareID       uint32
	reactivations int // diagnostics
}

const (
	filterCredSSP = iota
	filterEUAR
	filterRDP
	filterPassthrough
)

// Output PDU framing ([MS-RDPBCGR] 2.2.9.1.1.3 slow-path, 2.2.9.1.2 fast-path).
const (
	fpActionFastPath = 0x0
	fpActionX224     = 0x3 // TPKT version 3: slow-path

	fpUpdateBitmap = 0x1

	fpFragSingle = 0x0
	fpFragLast   = 0x1
	fpFragFirst  = 0x2
	fpFragNext   = 0x3

	fpMaxPDU = 0x7FFF // 15-bit fast-path length

	mcsSendDataIndication = 26 // DomainMCSPDU choice (PER: the choice index in the top 6 bits)
	pduTypeDemandActive   = 0x1
	pduTypeDeactivateAll  = 0x6
	pduTypeData           = 0x7
	pduType2Update        = 0x02
	pduType2Control       = 0x14
	pduType2Synchronize   = 0x1F
	pduType2FontMap       = 0x28
	pduType2SetErrorInfo  = 0x2F
	packetCompressed      = 0x20
	shareHeadersLen       = 18     // TS_SHARECONTROLHEADER + TS_SHAREDATAHEADER
	maxPERLength          = 0x3FFF // longest MCS user data without PER fragmentation

	updateTypeBitmap = 0x0001

	bitmapCompression      = 0x0001
	noBitmapCompressionHdr = 0x0400

	bitmapDataFixed = 18 // TS_BITMAP_DATA without the bitmap stream

	// Re-encoded rectangles are tiled, and bitmap updates grouped, up to this size, so a rewritten update always fits
	// one slow-path PDU (MCS user data ≤ 16 KiB) or one unfragmented fast-path PDU.
	maxRewritten = 8 << 10

	// Limits of what the filter buffers before it gives up and passes the stream through.
	maxCredSSPMessage = 1 << 20
	maxReassembly     = 16 << 20
	maxHeld           = 64 << 20
)

func newServerFilter(out io.Writer, selected uint32) *serverFilter {
	f := &serverFilter{out: out, state: filterRDP}
	if selected&(protoHybrid|protoHybridEx) != 0 {
		f.state = filterCredSSP
		f.euar = selected&protoHybridEx != 0
	}
	return f
}

// Write consumes server bytes and forwards them (repaired) to the client. It returns len(p) unless the client write
// fails.
func (f *serverFilter) Write(p []byte) (int, error) {
	if f.state == filterPassthrough && len(f.pending) == 0 {
		if _, err := f.out.Write(p); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	f.pending = append(f.pending, p...)
	f.obuf = f.obuf[:0]
	consumed := 0
	for consumed < len(f.pending) {
		n := f.next(f.pending[consumed:])
		if n == 0 {
			break // incomplete: wait for more bytes
		}
		consumed += n
	}
	f.pending = append(f.pending[:0], f.pending[consumed:]...)
	if len(f.obuf) > 0 {
		if _, err := f.out.Write(f.obuf); err != nil {
			return 0, err
		}
	}
	if cap(f.obuf) > 1<<20 {
		f.obuf = nil // do not keep a large output buffer around
	}
	return len(p), nil
}

// next processes the first unit (message / PDU) of b and returns the number of bytes consumed (0 = incomplete).
func (f *serverFilter) next(b []byte) int {
	switch f.state {
	case filterPassthrough:
		f.emit(b)
		return len(b)
	case filterCredSSP:
		if b[0] != 0x30 { // CredSSP is over
			f.state = filterRDP
			if f.euar {
				f.state = filterEUAR
			}
			return f.next(b)
		}
		n, ok := derSize(b)
		switch {
		case !ok || n > maxCredSSPMessage:
			return f.passthrough(b)
		case n == 0 || len(b) < n:
			return 0
		}
		f.emit(b[:n])
		return n
	case filterEUAR:
		if len(b) < 4 {
			return 0
		}
		f.emit(b[:4])
		f.state = filterRDP
		return 4
	}
	switch {
	case b[0] == fpActionX224: // TPKT
		if len(b) < 4 {
			return 0
		}
		n := int(binary.BigEndian.Uint16(b[2:4]))
		if n < 4 {
			return f.passthrough(b)
		}
		if len(b) < n {
			return 0
		}
		f.slowPath(b[:n])
		return n
	case b[0]&0x3F == fpActionFastPath: // fast-path: action 0, reserved bits clear
		if len(b) < 2 {
			return 0
		}
		n, hdr := int(b[1]), 2
		if b[1]&0x80 != 0 {
			if len(b) < 3 {
				return 0
			}
			n, hdr = int(b[1]&0x7F)<<8|int(b[2]), 3
		}
		if n < hdr {
			return f.passthrough(b)
		}
		if len(b) < n {
			return 0
		}
		f.fastPath(b[:n], hdr)
		return n
	}
	return f.passthrough(b)
}

// passthrough gives up on parsing: everything from here on (including held fragments) is forwarded unchanged.
func (f *serverFilter) passthrough(b []byte) int {
	f.release()
	f.flushFragments()
	f.state = filterPassthrough
	f.emit(b)
	return len(b)
}

func (f *serverFilter) emit(b []byte) { f.obuf = append(f.obuf, b...) }

// derSize returns the total size of the DER element at the start of b (0 = more bytes needed).
func derSize(b []byte) (int, bool) {
	if len(b) < 2 {
		return 0, true
	}
	l := int(b[1])
	if l < 0x80 {
		return 2 + l, true
	}
	k := l & 0x7F
	if k == 0 || k > 4 {
		return 0, false
	}
	if len(b) < 2+k {
		return 0, true
	}
	size := 0
	for _, c := range b[2 : 2+k] {
		size = size<<8 | int(c)
	}
	if size < 0 || size > 1<<30 {
		return 0, false
	}
	return 2 + k + size, true
}

// ---- slow-path ---------------------------------------------------------------------------------------------------------

// slowPath handles one TPKT: an MCS Send Data Indication carrying a Share Data PDU whose bitmap update needs fixing is
// replaced by one or more PDUs with the same headers; everything else is forwarded unchanged.
func (f *serverFilter) slowPath(pdu []byte) {
	mark := len(f.obuf)
	f.flushFragments() // fast-path fragments cannot straddle a slow-path PDU
	f.holdSince(mark)
	mcs, body, ok := sendDataIndication(pdu)
	if !ok {
		f.emit(pdu) // MCS domain PDUs (disconnect …) are never held
		return
	}
	pduType, pduType2 := -1, -1
	if len(body) >= 6 {
		pduType = int(binary.LittleEndian.Uint16(body[2:]) & 0x0F)
		if pduType == pduTypeData && len(body) >= shareHeadersLen {
			pduType2 = int(body[14])
		}
		if len(body) >= 10 && (pduType == pduTypeData || pduType == pduTypeDemandActive) {
			f.shareID = binary.LittleEndian.Uint32(body[6:])
		}
	}
	switch {
	case pduType == pduTypeDeactivateAll:
		f.reactivating = true
		f.reactivations++
		f.obuf = appendDeactivateAll(f.obuf, pdu, mcs, body, f.shareID)
		return
	case f.reactivating && (pduType == pduTypeDemandActive || pduType2 == pduType2Synchronize ||
		pduType2 == pduType2Control || pduType2 == pduType2SetErrorInfo):
		f.emit(pdu)
		return
	case f.reactivating && pduType2 == pduType2FontMap:
		f.emit(pdu)
		f.release()
		return
	}
	mark = len(f.obuf)
	if share, upd, ok := updateData(body); ok {
		if bodies := f.fixBitmapUpdate(upd); bodies != nil {
			for _, d := range bodies {
				f.obuf = appendSlowPath(f.obuf, mcs, share, upd, d)
			}
			f.holdSince(mark)
			return
		}
	}
	f.emit(pdu)
	f.holdSince(mark)
}

// holdSince moves the output appended since mark to the held output while a reactivation runs.
func (f *serverFilter) holdSince(mark int) {
	if !f.reactivating || len(f.obuf) == mark {
		return
	}
	f.held = append(f.held, f.obuf[mark:]...)
	f.obuf = f.obuf[:mark]
	if len(f.held) > maxHeld { // the server never completes the reactivation: stop holding
		f.release()
	}
}

// release ends a reactivation: the held output follows.
func (f *serverFilter) release() {
	f.reactivating = false
	f.obuf = append(f.obuf, f.held...)
	f.held = nil
}

// sendDataIndication returns the MCS header (Send Data Indication up to the length) and the user data of a TPKT.
func sendDataIndication(pdu []byte) (mcs, body []byte, ok bool) {
	// TPKT (4) · X.224 Data TPDU 02 F0 80 · MCS SDin: choice, initiator (2), channelId (2), priority/segmentation (1),
	// PER length of the user data.
	const mcsStart = 7
	if len(pdu) < mcsStart+7 || pdu[4] != 0x02 || pdu[5] != 0xF0 || pdu[6] != 0x80 || pdu[mcsStart]>>2 != mcsSendDataIndication {
		return nil, nil, false
	}
	i := mcsStart + 6
	l := int(pdu[i])
	i++
	if l&0x80 != 0 {
		if l&0x40 != 0 || i >= len(pdu) {
			return nil, nil, false // PER fragmentation is never used for RDP
		}
		l = (l&0x3F)<<8 | int(pdu[i])
		i++
	}
	if i+l != len(pdu) {
		return nil, nil, false
	}
	return pdu[mcsStart : mcsStart+6], pdu[i:], true
}

// updateData returns the share headers and the update data of a Share Data PDU carrying an (uncompressed) update.
func updateData(body []byte) (share, upd []byte, ok bool) {
	if len(body) < shareHeadersLen || int(binary.LittleEndian.Uint16(body)) != len(body) ||
		binary.LittleEndian.Uint16(body[2:])&0x0F != pduTypeData || body[14] != pduType2Update || body[15]&packetCompressed != 0 {
		return nil, nil, false
	}
	return body[:shareHeadersLen], body[shareHeadersLen:], true
}

// parseSlowPathUpdate returns the MCS header, the share headers and the update data of a slow-path Update PDU (ok =
// false for any other PDU).
func parseSlowPathUpdate(pdu []byte) (mcs, share, upd []byte, ok bool) {
	mcs, body, ok := sendDataIndication(pdu)
	if !ok {
		return nil, nil, nil, false
	}
	share, upd, ok = updateData(body)
	return mcs, share, upd, ok
}

// appendDeactivateAll forwards a Deactivate All PDU, completing one that carries nothing but the share control header
// (xrdp) into a TS_DEACTIVATE_ALL_PDU ([MS-RDPBCGR] 2.2.3.1: shareID, lengthSourceDescriptor = 1, sourceDescriptor 0).
func appendDeactivateAll(dst, pdu, mcs, body []byte, shareID uint32) []byte {
	if len(body) >= 10 {
		return append(dst, pdu...)
	}
	b := make([]byte, 13)
	copy(b, body[:min(len(body), 6)])
	binary.LittleEndian.PutUint16(b[0:], 13)
	binary.LittleEndian.PutUint16(b[2:], 0x10|pduTypeDeactivateAll)
	if len(body) < 6 {
		binary.LittleEndian.PutUint16(b[4:], 0x03EA)
	}
	binary.LittleEndian.PutUint32(b[6:], shareID)
	binary.LittleEndian.PutUint16(b[10:], 1)
	n := 4 + 3 + len(mcs) + 1 + len(b)
	dst = append(dst, fpActionX224, 0, byte(n>>8), byte(n), 0x02, 0xF0, 0x80)
	dst = append(dst, mcs...)
	dst = append(dst, byte(len(b)))
	return append(dst, b...)
}

// appendSlowPath encodes a slow-path Update PDU carrying data, copying the headers of the original PDU.
func appendSlowPath(dst, mcs, share, orig, data []byte) []byte {
	total := shareHeadersLen + len(data)
	per := []byte{byte(total)}
	if total >= 0x80 {
		per = []byte{0x80 | byte(total>>8), byte(total)}
	}
	n := 4 + 3 + len(mcs) + len(per) + total
	dst = append(dst, fpActionX224, 0, byte(n>>8), byte(n), 0x02, 0xF0, 0x80)
	dst = append(dst, mcs...)
	dst = append(dst, per...)
	start := len(dst)
	dst = append(dst, share...)
	h := dst[start:]
	binary.LittleEndian.PutUint16(h[0:], uint16(total))
	// uncompressedLength / compressedLength: servers disagree on what they count; keep their relation to the size.
	delta := len(data) - len(orig)
	for _, off := range []int{12, 16} {
		if v := int(binary.LittleEndian.Uint16(share[off:])); v != 0 {
			binary.LittleEndian.PutUint16(h[off:], uint16(max(0, v+delta)))
		}
	}
	return append(dst, data...)
}

// ---- fast-path ---------------------------------------------------------------------------------------------------------

type fpUpdate struct {
	raw  []byte // the encoded update (header … data), as received
	code byte
	frag byte
	comp bool
	data []byte
}

// fpFragments holds a fragmented bitmap update until its last fragment arrives.
type fpFragments struct {
	raws [][]byte
	data []byte
}

// fastPath handles one fast-path output PDU (hdr = size of its header); its output is held during a reactivation.
func (f *serverFilter) fastPath(pdu []byte, hdr int) {
	mark := len(f.obuf)
	f.fastPathContent(pdu, hdr)
	if f.state != filterPassthrough {
		f.holdSince(mark)
	}
}

func (f *serverFilter) fastPathContent(pdu []byte, hdr int) {
	if pdu[0]&0xC0 != 0 { // encrypted / salted checksum (standard RDP security only): never touched
		f.flushFragments()
		f.emit(pdu)
		return
	}
	updates, ok := parseFastPathUpdates(pdu[hdr:])
	if !ok {
		f.passthrough(pdu)
		return
	}
	interesting := f.frag != nil
	for _, u := range updates {
		if u.code == fpUpdateBitmap && !u.comp {
			interesting = true
		}
	}
	if !interesting {
		f.emit(pdu)
		return
	}
	var out [][]byte
	changed := false
	for _, u := range updates {
		if f.frag != nil {
			if u.code == fpUpdateBitmap && !u.comp && (u.frag == fpFragNext || u.frag == fpFragLast) &&
				len(f.frag.data)+len(u.data) <= maxReassembly {
				f.frag.raws = append(f.frag.raws, append([]byte(nil), u.raw...))
				f.frag.data = append(f.frag.data, u.data...)
				changed = true
				if u.frag == fpFragLast {
					if bodies := f.fixBitmapUpdate(f.frag.data); bodies != nil {
						for _, d := range bodies {
							out = append(out, encodeFastPathUpdate(fpUpdateBitmap, d))
						}
					} else {
						out = append(out, f.frag.raws...)
					}
					f.frag = nil
				}
				continue
			}
			// Not the continuation expected: forward what was held and carry on unchanged.
			out = append(out, f.frag.raws...)
			f.frag = nil
			changed = true
		}
		if u.code == fpUpdateBitmap && !u.comp {
			switch u.frag {
			case fpFragSingle:
				if bodies := f.fixBitmapUpdate(u.data); bodies != nil {
					for _, d := range bodies {
						out = append(out, encodeFastPathUpdate(fpUpdateBitmap, d))
					}
					changed = true
					continue
				}
			case fpFragFirst:
				f.frag = &fpFragments{raws: [][]byte{append([]byte(nil), u.raw...)}, data: append([]byte(nil), u.data...)}
				f.heldFragments = true
				changed = true
				continue
			}
		}
		out = append(out, u.raw)
	}
	if !changed {
		f.emit(pdu)
		return
	}
	f.obuf = packFastPath(f.obuf, out)
}

// flushFragments forwards held fragments unchanged.
func (f *serverFilter) flushFragments() {
	if f.frag == nil {
		return
	}
	f.obuf = packFastPath(f.obuf, f.frag.raws)
	f.frag = nil
}

// parseFastPathUpdates splits the updates of a fast-path PDU body.
func parseFastPathUpdates(b []byte) ([]fpUpdate, bool) {
	var out []fpUpdate
	for len(b) > 0 {
		h := b[0]
		i := 1
		comp := h>>6 == 0x2 // FASTPATH_OUTPUT_COMPRESSION_USED: a compressionFlags byte follows
		if comp {
			i = 2
		}
		if len(b) < i+2 {
			return nil, false
		}
		size := int(binary.LittleEndian.Uint16(b[i:]))
		i += 2
		if len(b) < i+size {
			return nil, false
		}
		out = append(out, fpUpdate{raw: b[:i+size], code: h & 0x0F, frag: (h >> 4) & 0x3, comp: comp, data: b[i : i+size]})
		b = b[i+size:]
	}
	return out, true
}

// packFastPath appends fast-path PDUs carrying the encoded updates (in order) to dst. Updates too large for one PDU
// are split into fragments.
func packFastPath(dst []byte, updates [][]byte) []byte {
	const room = fpMaxPDU - 3 // 3-byte PDU header
	var cur []byte
	flush := func() {
		if len(cur) == 0 {
			return
		}
		n := len(cur) + 3
		dst = append(dst, fpActionFastPath, 0x80|byte(n>>8), byte(n))
		dst = append(dst, cur...)
		cur = cur[:0]
	}
	for _, u := range updates {
		if len(u) > room {
			flush()
			for _, part := range fragmentUpdate(u, room) {
				cur = append(cur, part...)
				flush()
			}
			continue
		}
		if len(cur)+len(u) > room {
			flush()
		}
		cur = append(cur, u...)
	}
	flush()
	return dst
}

// fragmentUpdate splits an encoded (single, uncompressed) update into FIRST / NEXT … / LAST fragments of at most room
// bytes each.
func fragmentUpdate(u []byte, room int) [][]byte {
	code := u[0] & 0x0F
	data := u[3:]
	chunk := room - 3
	var out [][]byte
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		frag := byte(fpFragNext)
		switch {
		case off == 0:
			frag = fpFragFirst
		case end == len(data):
			frag = fpFragLast
		}
		part := make([]byte, 0, 3+end-off)
		part = append(part, code|frag<<4, byte(end-off), byte((end-off)>>8))
		out = append(out, append(part, data[off:end]...))
	}
	return out
}

func encodeFastPathUpdate(code byte, data []byte) []byte {
	u := make([]byte, 0, 3+len(data))
	u = append(u, code|fpFragSingle<<4, byte(len(data)), byte(len(data)>>8))
	return append(u, data...)
}

// ---- bitmap updates -----------------------------------------------------------------------------------------------------

type bitmapRect struct {
	left, top, right, bottom int
	width, height, bpp       int
	flags                    int
	data                     []byte // bitmap stream (after the optional compression header)
	raw                      []byte // the encoded TS_BITMAP_DATA
}

// fixBitmapUpdate rewrites the rectangles of a TS_UPDATE_BITMAP_DATA whose bitmap is larger than its destination. It
// returns the replacement update data (updateType, numberRectangles, rectangles), split so that each is at most
// maxRewritten bytes unless it carries one larger unmodified rectangle, or nil when nothing needs (or can) be changed.
func (f *serverFilter) fixBitmapUpdate(data []byte) [][]byte {
	rects, ok := parseBitmapUpdate(data)
	if !ok {
		return nil
	}
	var out [][]byte // encoded TS_BITMAP_DATA
	fixed := 0
	for _, r := range rects {
		if r.needsFix() {
			if enc := r.rewrite(); enc != nil {
				out = append(out, enc...)
				fixed++
				continue
			}
		}
		out = append(out, r.raw)
	}
	if fixed == 0 {
		return nil
	}
	f.fixed += fixed
	var updates [][]byte
	var cur []byte
	count := 0
	flush := func() {
		if count == 0 {
			return
		}
		body := make([]byte, 4, 4+len(cur))
		binary.LittleEndian.PutUint16(body[0:], updateTypeBitmap)
		binary.LittleEndian.PutUint16(body[2:], uint16(count))
		updates = append(updates, append(body, cur...))
		cur, count = nil, 0
	}
	for _, r := range out {
		if count > 0 && (4+len(cur)+len(r) > maxRewritten || count == 0xFFFF) {
			flush()
		}
		cur = append(cur, r...)
		count++
	}
	flush()
	return updates
}

func parseBitmapUpdate(b []byte) ([]bitmapRect, bool) {
	if len(b) < 4 || binary.LittleEndian.Uint16(b) != updateTypeBitmap {
		return nil, false
	}
	n := int(binary.LittleEndian.Uint16(b[2:]))
	b = b[4:]
	rects := make([]bitmapRect, 0, min(n, 1024))
	for range n {
		if len(b) < bitmapDataFixed {
			return nil, false
		}
		u16 := func(i int) int { return int(binary.LittleEndian.Uint16(b[i:])) }
		r := bitmapRect{left: u16(0), top: u16(2), right: u16(4), bottom: u16(6), width: u16(8), height: u16(10),
			bpp: u16(12), flags: u16(14)}
		size := u16(16)
		if len(b) < bitmapDataFixed+size {
			return nil, false
		}
		r.raw = b[:bitmapDataFixed+size]
		r.data = b[bitmapDataFixed : bitmapDataFixed+size]
		if r.flags&bitmapCompression != 0 && r.flags&noBitmapCompressionHdr == 0 {
			if len(r.data) < 8 {
				return nil, false
			}
			r.data = r.data[8:] // TS_CD_HEADER
		}
		rects = append(rects, r)
		b = b[bitmapDataFixed+size:]
	}
	return rects, len(b) == 0
}

func (r *bitmapRect) destWidth() int  { return r.right - r.left + 1 }
func (r *bitmapRect) destHeight() int { return r.bottom - r.top + 1 }

// needsFix reports a bitmap larger than its (valid) destination rectangle in a format the filter can re-encode.
func (r *bitmapRect) needsFix() bool {
	cx, cy := r.destWidth(), r.destHeight()
	if cx < 1 || cy < 1 || r.width < cx || r.height < cy || (r.width == cx && r.height == cy) {
		return false
	}
	if r.flags&bitmapCompression != 0 {
		return rlePixelSize(r.bpp) != 0 // 32 bpp bitmaps use the RDP 6.0 planar codec: left alone
	}
	return r.bpp == 32 || rlePixelSize(r.bpp) != 0
}

// pixels returns the bitmap's pixels (bottom-up scanlines of width pixels, no padding) and the pixel size.
func (r *bitmapRect) pixels() ([]byte, int) {
	if r.flags&bitmapCompression != 0 {
		pix, err := rleDecode(r.data, r.width, r.height, r.bpp)
		if err != nil {
			return nil, 0
		}
		return pix, rlePixelSize(r.bpp)
	}
	ps := (r.bpp + 7) / 8
	row := r.width * ps
	padded := (row + 3) &^ 3
	if len(r.data) < padded*r.height {
		return nil, 0
	}
	pix := make([]byte, 0, row*r.height)
	for y := range r.height {
		pix = append(pix, r.data[y*padded:y*padded+row]...)
	}
	return pix, ps
}

// rewrite crops the bitmap to its destination and re-encodes it as one or more TS_BITMAP_DATA whose bitmap size equals
// their destination size (nil when the bitmap cannot be decoded).
func (r *bitmapRect) rewrite() [][]byte {
	pix, ps := r.pixels()
	if pix == nil {
		return nil
	}
	cx, cy := r.destWidth(), r.destHeight()
	// Top-down visible pixels: bitmap row y (from the top) is scanline height-1-y.
	vis := make([]byte, cx*cy*ps)
	for y := range cy {
		src := (r.height - 1 - y) * r.width * ps
		copy(vis[y*cx*ps:(y+1)*cx*ps], pix[src:src+cx*ps])
	}
	var out [][]byte
	r.tile(&out, vis, cx, ps, 0, 0, cx, cy)
	return out
}

// tile encodes the region (x, y, w, h) of the top-down visible pixels, splitting it until each piece is small enough.
func (r *bitmapRect) tile(out *[][]byte, vis []byte, stride, ps, x, y, w, h int) {
	enc := r.encode(vis, stride, ps, x, y, w, h)
	if len(enc) <= maxRewritten-4 || (w == 1 && h == 1) {
		*out = append(*out, enc)
		return
	}
	if h > 1 {
		top := h / 2
		r.tile(out, vis, stride, ps, x, y, w, top)
		r.tile(out, vis, stride, ps, x, y+top, w, h-top)
		return
	}
	left := w / 2
	r.tile(out, vis, stride, ps, x, y, left, h)
	r.tile(out, vis, stride, ps, x+left, y, w-left, h)
}

// encode builds the TS_BITMAP_DATA of a region: RLE (8/15/16/24 bpp) or raw 32 bpp scanlines, bottom-up.
func (r *bitmapRect) encode(vis []byte, stride, ps, x, y, w, h int) []byte {
	pix := make([]byte, 0, w*h*ps)
	for row := h - 1; row >= 0; row-- {
		off := ((y+row)*stride + x) * ps
		pix = append(pix, vis[off:off+w*ps]...)
	}
	body, flags := pix, 0
	if r.bpp != 32 {
		body, flags = rleEncode(pix, w, h, r.bpp), bitmapCompression|noBitmapCompressionHdr
	}
	b := make([]byte, bitmapDataFixed, bitmapDataFixed+len(body))
	put := func(i, v int) { binary.LittleEndian.PutUint16(b[i:], uint16(v)) }
	put(0, r.left+x)
	put(2, r.top+y)
	put(4, r.left+x+w-1)
	put(6, r.top+y+h-1)
	put(8, w)
	put(10, h)
	put(12, r.bpp)
	put(14, flags)
	put(16, len(body))
	return append(b, body...)
}
