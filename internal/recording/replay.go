package recording

import (
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/term"
)

// Instant replay (TERM-30): GET /api/sessions/{id}/replay?minutes=N returns an asciicast v3 built from the session's
// output ring buffer, with the real arrival times from this module's timing index, whether or not the session is
// being recorded. Output older than the last N minutes becomes the initial screen state (one event at 0 s); the rest
// plays in real time. Recognized commands are added as markers ("jump to command"). minutes=0 replays everything the
// ring still holds.

const maxReplayMinutes = 24 * 60

func (s *Service) handleReplay(c *echo.Context) error {
	sess, u, err := s.sessionFor(c, false)
	if err != nil {
		return err
	}
	if sess.Kind != model.KindTerminal {
		return httpx.BadRequest("not a terminal session")
	}
	minutes := 0
	if v := c.QueryParam("minutes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > maxReplayMinutes {
			return httpx.BadRequest("minutes must be between 0 and 1440")
		}
		minutes = n
	}
	if sess.OwnerID != u.ID {
		s.d.Audit.Log(c, "session.replay", sess.ID, map[string]any{"ownerId": sess.OwnerID, "minutes": minutes})
	}
	data, tail := ringSnapshot(sess)
	var (
		points   []tlPoint
		commands []CommandRecord
	)
	if st := s.states.lookup(sess.ID); st != nil {
		points = st.timelinePoints()
		commands = st.commands()
	}
	info := sess.Info()
	h := c.Response().Header()
	h.Set(echo.HeaderContentType, "application/x-asciicast; charset=utf-8")
	h.Set(echo.HeaderCacheControl, "no-store")
	h.Set("Content-Disposition", `inline; filename="replay.cast"`)
	c.Response().WriteHeader(http.StatusOK)
	return buildReplay(c.Response(), replayInput{
		data: data, tail: tail, points: points, commands: commands, cols: info.Cols, rows: info.Rows,
		title: info.Title, now: s.now(), minutes: minutes,
	})
}

// ringSnapshot copies the ring buffer and its tail offset consistently (retrying while output arrives).
func ringSnapshot(sess *term.Session) ([]byte, int64) {
	for range 4 {
		t1, h1 := sess.Offsets()
		data := sess.Scrollback()
		_, h2 := sess.Offsets()
		if h1 == h2 && int64(len(data)) == h1-t1 {
			return data, t1
		}
	}
	data := sess.Scrollback()
	_, head := sess.Offsets()
	return data, max(head-int64(len(data)), 0)
}

type replayInput struct {
	data       []byte
	tail       int64
	points     []tlPoint
	commands   []CommandRecord
	cols, rows int
	title      string
	now        time.Time
	minutes    int
}

const replayChunk = 32 << 10

// buildReplay writes the asciicast v3 replay of in to w.
func buildReplay(w io.Writer, in replayInput) error {
	head := in.tail + int64(len(in.data))
	// Points inside the ring, plus the last one before it (its time applies to the ring's first bytes).
	var pts []tlPoint
	for i, p := range in.points {
		if p.off >= head {
			break
		}
		if p.off < in.tail {
			if i+1 < len(in.points) && in.points[i+1].off <= in.tail {
				continue
			}
			p.off = in.tail
		}
		if n := len(pts); n > 0 && pts[n-1].off == p.off {
			pts[n-1] = p
			continue
		}
		pts = append(pts, p)
	}
	cols, rows := in.cols, in.rows
	cw := newCastWriter(w, 3)
	if len(pts) == 0 {
		cw.header(max(cols, 2), max(rows, 2), "", in.title, in.now.Unix(), 0, nil)
		emitBytes(cw, 0, in.data, true)
		return cw.flush()
	}
	// k = first point of the replayed period.
	k := 0
	if in.minutes > 0 {
		cut := in.now.Add(-time.Duration(in.minutes) * time.Minute)
		k = sort.Search(len(pts), func(i int) bool { return !pts[i].t.Before(cut) })
		if k == len(pts) {
			k = len(pts) - 1 // nothing new in the period: show the latest output as the last event
		}
	}
	start := pts[k].t
	sizeCols, sizeRows := pts[k].cols, pts[k].rows
	if k > 0 {
		sizeCols, sizeRows = pts[k-1].cols, pts[k-1].rows
	}
	if sizeCols <= 0 || sizeRows <= 0 {
		sizeCols, sizeRows = max(cols, 2), max(rows, 2)
	}
	cw.header(sizeCols, sizeRows, "", in.title, start.Unix(), 0, nil)
	var carry []byte
	first := true
	if pre := pts[k].off - in.tail; pre > 0 {
		carry = emitBytes(cw, 0, in.data[:pre], true)
		first = false
	}
	curCols, curRows := sizeCols, sizeRows
	ci := 0
	for ci < len(in.commands) && in.commands[ci].Time.Before(start) {
		ci++
	}
	for i := k; i < len(pts); i++ {
		p := pts[i]
		t := p.t.Sub(start).Seconds()
		for ci < len(in.commands) && !in.commands[ci].Time.After(p.t) {
			cw.event(max(in.commands[ci].Time.Sub(start).Seconds(), 0), "m", markerLabel(in.commands[ci].Command))
			ci++
		}
		if p.cols > 0 && p.rows > 0 && (p.cols != curCols || p.rows != curRows) {
			curCols, curRows = p.cols, p.rows
			cw.event(t, "r", strconv.Itoa(p.cols)+"x"+strconv.Itoa(p.rows))
		}
		end := head
		if i+1 < len(pts) {
			end = pts[i+1].off
		}
		seg := in.data[p.off-in.tail : end-in.tail]
		if len(carry) > 0 {
			seg = append(append([]byte(nil), carry...), seg...)
		}
		carry = emitBytes(cw, t, seg, first && p.off == in.tail)
		first = false
	}
	for ; ci < len(in.commands); ci++ {
		cw.event(max(in.commands[ci].Time.Sub(start).Seconds(), 0), "m", markerLabel(in.commands[ci].Command))
	}
	if len(carry) > 0 {
		cw.event(cw.last, "o", string(carry))
	}
	return cw.flush()
}

// emitBytes writes b as output events at time t (≤ replayChunk each), never splitting a UTF-8 sequence; it returns
// an incomplete trailing sequence to prepend to the next segment. first drops leading continuation bytes (the ring
// may start inside a character).
func emitBytes(cw *castWriter, t float64, b []byte, first bool) []byte {
	if first {
		for len(b) > 0 && b[0]&0xc0 == 0x80 {
			b = b[1:]
		}
	}
	for len(b) > 0 {
		n := min(len(b), replayChunk)
		chunk, rest := validUTF8Prefix(b[:n])
		if len(chunk) == 0 {
			if n == len(b) {
				return rest
			}
			chunk = b[:n] // cannot happen with n ≥ 4; keep going
		}
		cw.event(t, "o", string(chunk))
		b = b[len(chunk):]
	}
	return nil
}

func markerLabel(cmd string) string {
	r := []rune(cmd)
	for i, c := range r {
		if c == '\n' {
			r = append(r[:i], []rune(" …")...)
			break
		}
	}
	if len(r) > 80 {
		r = append(r[:79], '…')
	}
	return "$ " + string(r)
}
