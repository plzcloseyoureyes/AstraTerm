package term

// ring is the per-session output buffer addressed by absolute stream offsets (SPEC §6.2). Offsets count every byte
// ever appended; the ring keeps the most recent size bytes, i.e. the offsets [Tail(), Head()). The backing buffer grows
// on demand up to size, so idle sessions stay small. It is not safe for concurrent use (Session.mu guards it).
type ring struct {
	size int
	buf  []byte // len(buf) ≤ size; circular once len(buf) == size
	head int64  // total bytes ever written
}

const ringInitialCap = 64 << 10

func newRing(size int) *ring {
	if size < 1024 {
		size = 1024
	}
	return &ring{size: size}
}

// Head is the offset one past the newest byte.
func (r *ring) Head() int64 { return r.head }

// Tail is the offset of the oldest byte still stored.
func (r *ring) Tail() int64 {
	if t := r.head - int64(len(r.buf)); t > 0 {
		return t
	}
	return 0
}

// Len is the number of bytes currently stored.
func (r *ring) Len() int { return len(r.buf) }

// Write appends p, discarding the oldest bytes once the ring is full.
func (r *ring) Write(p []byte) {
	n := len(p)
	if n == 0 {
		return
	}
	if len(r.buf) < r.size {
		// Growing phase: the data starts at index 0 and has not wrapped yet.
		room := r.size - len(r.buf)
		take := min(n, room)
		if cap(r.buf)-len(r.buf) < take {
			nc := max(ringInitialCap, 2*cap(r.buf), len(r.buf)+take)
			nb := make([]byte, len(r.buf), min(nc, r.size))
			copy(nb, r.buf)
			r.buf = nb
		}
		r.buf = append(r.buf, p[:take]...)
		r.head += int64(take)
		p = p[take:]
		if len(p) == 0 {
			return
		}
	}
	// Full: overwrite circularly. Only the last size bytes of p can survive.
	if len(p) > r.size {
		r.head += int64(len(p) - r.size)
		p = p[len(p)-r.size:]
	}
	pos := int(r.head % int64(r.size))
	c := copy(r.buf[pos:], p)
	if c < len(p) {
		copy(r.buf, p[c:])
	}
	r.head += int64(len(p))
}

// index maps an absolute offset inside [Tail, Head) to a buffer index.
func (r *ring) index(off int64) int {
	if len(r.buf) < r.size {
		return int(off - r.Tail())
	}
	return int(off % int64(r.size))
}

// ReadAt copies bytes starting at absolute offset off into p and returns the number copied. It returns 0 when off is
// outside [Tail, Head).
func (r *ring) ReadAt(p []byte, off int64) int {
	if off < r.Tail() || off >= r.head || len(p) == 0 {
		return 0
	}
	n := int(min(int64(len(p)), r.head-off))
	i := r.index(off)
	c := copy(p[:n], r.buf[i:])
	if c < n {
		copy(p[c:n], r.buf)
	}
	return n
}

// Slice returns a copy of the bytes in [from, to) clipped to the stored range.
func (r *ring) Slice(from, to int64) []byte {
	from = max(from, r.Tail())
	to = min(to, r.head)
	if to <= from {
		return nil
	}
	out := make([]byte, to-from)
	r.ReadAt(out, from)
	return out
}

// Bytes returns a copy of everything stored.
func (r *ring) Bytes() []byte { return r.Slice(r.Tail(), r.head) }
