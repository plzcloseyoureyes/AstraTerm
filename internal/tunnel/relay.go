package tunnel

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// stats are the live counters of one forward. All fields are safe for concurrent use.
type stats struct {
	active   atomic.Int64
	total    atomic.Int64
	failed   atomic.Int64
	bytesIn  atomic.Int64 // towards the client
	bytesOut atomic.Int64 // from the client
	lastUse  atomic.Int64 // unix nanoseconds of the last byte / connection
	dirty    atomic.Bool  // changed since the last published snapshot

	mu        sync.Mutex
	lastErr   string
	lastErrAt time.Time

	// rate computation (owned by the publisher)
	rateMu               sync.Mutex
	lastIn, lastOut      int64
	lastSample           time.Time
	rateIn, rateOut      int64
	localAddr, remoteAdr atomic.Pointer[string]
}

func (s *stats) touch() { s.lastUse.Store(time.Now().UnixNano()); s.dirty.Store(true) }

func (s *stats) opened() {
	s.active.Add(1)
	s.total.Add(1)
	s.touch()
}

func (s *stats) closed() {
	s.active.Add(-1)
	s.touch()
}

// fail records a connection that could not be forwarded.
func (s *stats) fail(msg string) {
	s.failed.Add(1)
	s.setLastError(msg)
}

func (s *stats) setLastError(msg string) {
	s.mu.Lock()
	s.lastErr, s.lastErrAt = truncate(msg, 400), time.Now().UTC()
	s.mu.Unlock()
	s.dirty.Store(true)
}

// idleFor reports how long the forward has been without any traffic or connection.
func (s *stats) idleFor() time.Duration {
	last := s.lastUse.Load()
	if last == 0 {
		return 0
	}
	return time.Since(time.Unix(0, last))
}

func (s *stats) setLocalAddr(a string) {
	s.localAddr.Store(&a)
	s.dirty.Store(true)
}

func (s *stats) setRemoteAddr(a string) {
	s.remoteAdr.Store(&a)
	s.dirty.Store(true)
}

func loadStr(p *atomic.Pointer[string]) string {
	if v := p.Load(); v != nil {
		return *v
	}
	return ""
}

// sampleRates updates the throughput estimate (called about once per second by the publisher).
func (s *stats) sampleRates(now time.Time) {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	in, out := s.bytesIn.Load(), s.bytesOut.Load()
	if !s.lastSample.IsZero() {
		if dt := now.Sub(s.lastSample).Seconds(); dt > 0.2 {
			ri := int64(float64(in-s.lastIn) / dt)
			ro := int64(float64(out-s.lastOut) / dt)
			if ri != s.rateIn || ro != s.rateOut {
				s.dirty.Store(true)
			}
			s.rateIn, s.rateOut = max(ri, 0), max(ro, 0)
		}
	}
	s.lastIn, s.lastOut, s.lastSample = in, out, now
}

// fill copies the counters into a status.
func (s *stats) fill(st *Status) {
	st.ActiveConns = int(s.active.Load())
	st.TotalConns = s.total.Load()
	st.BytesIn = s.bytesIn.Load()
	st.BytesOut = s.bytesOut.Load()
	st.FailedConns = s.failed.Load()
	st.LocalAddr = loadStr(&s.localAddr)
	st.RemoteAddr = loadStr(&s.remoteAdr)
	s.rateMu.Lock()
	st.RateIn, st.RateOut = s.rateIn, s.rateOut
	s.rateMu.Unlock()
	s.mu.Lock()
	st.LastError = s.lastErr
	if !s.lastErrAt.IsZero() {
		t := s.lastErrAt
		st.LastErrorAt = &t
	}
	s.mu.Unlock()
}

// clientConn wraps an accepted client connection (TCP, Unix socket, or an SSH channel of a remote listener) and passes
// CloseWrite through for half-close. Traffic is counted on the destination side (trackedConn).
type clientConn struct {
	net.Conn
}

// CloseWrite half-closes the connection when the underlying transport supports it.
func (c *clientConn) CloseWrite() error { return closeWrite(c.Conn) }

func closeWrite(c net.Conn) error {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

var bufPool = sync.Pool{New: func() any {
	b := make([]byte, 32<<10)
	return &b
}}

// relay copies both directions between a client and an upstream connection until both are finished (EOF on one
// side half-closes the other, like OpenSSH), one side fails, or ctx ends. Both connections are closed on return.
func relay(ctx context.Context, client, upstream net.Conn) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}
	stop := context.AfterFunc(ctx, closeBoth)
	defer stop()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		bp := bufPool.Get().(*[]byte)
		_, err := io.CopyBuffer(dst, onlyReader{src}, *bp)
		bufPool.Put(bp)
		if err != nil {
			closeBoth()
		} else if closeWrite(dst) != nil {
			closeBoth()
		}
		done <- struct{}{}
	}
	go pipe(upstream, client)
	go pipe(client, upstream)
	<-done
	<-done
	closeBoth()
}

// onlyReader hides WriterTo so io.CopyBuffer uses the pooled buffer (and counting wrappers see every byte).
type onlyReader struct{ io.Reader }

// truncate shortens s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
