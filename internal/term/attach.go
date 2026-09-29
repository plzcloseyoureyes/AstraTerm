package term

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/coder/websocket"

	"github.com/nexterm/nexterm/internal/model"
)

// AttachOptions configures Manager.Attach (used by /ws/terminal and /ws/share).
type AttachOptions struct {
	// Offset is the last stream offset the client fully rendered (0 for a fresh terminal).
	Offset int64
	// ReadOnly viewers never send input or resize (enforced here).
	ReadOnly bool
	// User is the attaching user (nil for anonymous share viewers).
	User *model.User
	// Label identifies the attachment in logs (e.g. "share").
	Label string
	// Shadow marks an administrator viewing another user's session (read-only): the owner's views are told who is
	// watching ({type:'shadow', viewers}) and the owner gets a notification.
	Shadow bool
}

// client is one attached WebSocket. Fields below the marker are guarded by the session's mu.
type client struct {
	s        *Session
	ws       *websocket.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	readOnly bool
	shadow   bool
	user     *model.User
	label    string
	wakeCh   chan struct{}

	// guarded by s.mu
	cursor   int64 // next offset to send
	acked    int64 // highest offset acknowledged by the client
	blocked  bool  // over the high-water mark
	urgent   [][]byte
	ctrl     []ctrlMsg
	cols     int
	rows     int
	lastSend time.Time
	closing  bool
	roWarned bool
}

// ctrlMsg is a JSON control message bound to a stream offset: it is delivered once every byte before off was sent.
type ctrlMsg struct {
	off  int64
	data []byte
	drop bool // may be discarded when the queue overflows (title, cwd, bell, marks)
}

const (
	maxCtrlQueue    = 1024
	wsWriteTimeout  = 15 * time.Second
	wsPingInterval  = 20 * time.Second
	wsCloseGrace    = 2 * time.Second
	closeNotFound   = websocket.StatusCode(4404)
	closeSessionEnd = websocket.StatusNormalClosure
)

func (c *client) wake() {
	select {
	case c.wakeCh <- struct{}{}:
	default:
	}
}

// queueUrgent queues a message that is not tied to the output stream (sent before pending output). s.mu held.
func (c *client) queueUrgent(msg []byte) {
	if len(c.urgent) >= maxCtrlQueue {
		c.urgent = c.urgent[1:]
	}
	c.urgent = append(c.urgent, msg)
	c.wake()
}

// queueCtrl queues an offset-bound message. s.mu held.
func (c *client) queueCtrl(off int64, msg []byte, droppable bool) {
	if len(c.ctrl) >= maxCtrlQueue {
		idx := -1
		for i := range c.ctrl {
			if c.ctrl[i].drop {
				idx = i
				break
			}
		}
		switch {
		case idx >= 0:
			c.ctrl = append(c.ctrl[:idx], c.ctrl[idx+1:]...)
		case droppable:
			return
		}
	}
	c.ctrl = append(c.ctrl, ctrlMsg{off: off, data: msg, drop: droppable})
	c.wake()
}

// Attach serves a terminal WebSocket for session id until the socket or the session closes (SPEC §6.2). The caller
// has authenticated and authorized the user; the socket is closed when Attach returns.
func (m *Manager) Attach(ctx context.Context, id string, ws *websocket.Conn, opts AttachOptions) error {
	s := m.Get(id)
	if s == nil {
		_ = ws.Close(closeNotFound, "session not found")
		return model.ErrNotFound
	}
	if s.Kind != model.KindTerminal {
		_ = ws.Close(websocket.StatusUnsupportedData, "not a terminal session")
		return ErrUnsupported
	}
	cctx, cancel := context.WithCancel(ctx)
	c := &client{s: s, ws: ws, ctx: cctx, cancel: cancel, readOnly: opts.ReadOnly, shadow: opts.Shadow && opts.ReadOnly,
		user: opts.User, label: opts.Label, wakeCh: make(chan struct{}, 1)}

	s.mu.Lock()
	if s.closed {
		msg := s.stateMsgLocked()
		s.mu.Unlock()
		cancel()
		wctx, wcancel := context.WithTimeout(ctx, time.Second)
		_ = ws.Write(wctx, websocket.MessageText, msg)
		wcancel()
		_ = ws.Close(closeSessionEnd, "session closed")
		return ErrClosed
	}
	head, tail := s.ring.Head(), s.ring.Tail()
	from, mode := opts.Offset, "delta"
	if from < tail || from > head {
		from, mode = tail, "reset"
	}
	c.cursor, c.acked = from, from
	c.urgent = append(c.urgent,
		ctrlJSON(attachMsg{Type: "attach", Mode: mode, From: from, Head: head}),
		ctrlJSON(readonlyMsg{Type: "readonly", Value: opts.ReadOnly}),
		s.stateMsgLocked(),
		ctrlJSON(resizeMsg{Type: "resize", Cols: s.cols, Rows: s.rows}),
	)
	if s.oscTitle != "" {
		c.urgent = append(c.urgent, ctrlJSON(titleMsg{Type: "title", Title: s.oscTitle}))
	}
	if s.cwd != "" {
		c.urgent = append(c.urgent, ctrlJSON(cwdMsg{Type: "cwd", Path: s.cwd}))
	}
	for _, mk := range s.marks {
		if mk.offset > from && mk.offset <= head {
			c.ctrl = append(c.ctrl, ctrlMsg{off: mk.offset, data: mk.msg, drop: true})
		}
	}
	c.ctrl = append(c.ctrl, ctrlMsg{off: head, data: ctrlJSON(attachEndMsg{Type: "attach-end", Head: head})})
	s.clients[c] = struct{}{}
	s.detachedAt = time.Time{}
	if c.shadow {
		s.announceShadowsLocked()
	} else if !c.readOnly && len(s.shadowNamesLocked()) > 0 {
		c.urgent = append(c.urgent, ctrlJSON(shadowMsg{Type: "shadow", Viewers: s.shadowNamesLocked()}))
	}
	s.mu.Unlock()
	c.wake()
	m.publish(s)
	if c.shadow && m.d != nil && m.d.Events != nil {
		who := "An administrator"
		if opts.User != nil {
			who = opts.User.Username
			if opts.User.DisplayName != "" {
				who = opts.User.DisplayName
			}
		}
		m.d.Events.Publish(s.OwnerID, model.Notify("info", "An administrator is viewing your session",
			who+" is watching “"+s.Title()+"” (read-only)."))
	}

	done := make(chan struct{}, 2)
	go func() { c.writeLoop(); done <- struct{}{} }()
	go func() { c.pingLoop(); done <- struct{}{} }()
	c.readLoop()
	cancel()
	<-done
	<-done
	m.detach(c)
	_ = ws.CloseNow()
	return nil
}

func (m *Manager) detach(c *client) {
	s := c.s
	s.mu.Lock()
	delete(s.clients, c)
	if c.shadow && !s.closed {
		s.announceShadowsLocked()
	}
	if s.lastWriter == c {
		s.lastWriter = nil
	}
	if !s.closed && len(s.clients)+s.extClients == 0 {
		s.detachedAt = time.Now()
	}
	s.cond.Broadcast() // the remaining clients may no longer all be blocked
	closed := s.closed
	s.mu.Unlock()
	if !closed {
		m.publish(s)
	}
}

// ---- writer -------------------------------------------------------------------------------------------------------

func (c *client) writeLoop() {
	defer c.cancel()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	for {
		urgent, ctrl, frame, wait, done := c.next()
		for _, msg := range urgent {
			if !c.write(websocket.MessageText, msg) {
				return
			}
		}
		for _, msg := range ctrl {
			if !c.write(websocket.MessageText, msg) {
				return
			}
		}
		if frame != nil && !c.write(websocket.MessageBinary, frame) {
			return
		}
		if done {
			ctx, cancel := context.WithTimeout(context.Background(), wsCloseGrace)
			go func() { <-ctx.Done(); _ = c.ws.CloseNow() }()
			_ = c.ws.Close(closeSessionEnd, "session closed")
			cancel()
			return
		}
		if len(urgent) > 0 || len(ctrl) > 0 || frame != nil {
			continue
		}
		if wait > 0 {
			timer.Reset(wait)
		}
		select {
		case <-c.ctx.Done():
			timer.Stop()
			return
		case <-c.wakeCh:
		case <-timer.C:
		}
		if wait > 0 && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

func (c *client) write(typ websocket.MessageType, p []byte) bool {
	ctx, cancel := context.WithTimeout(c.ctx, wsWriteTimeout)
	defer cancel()
	return c.ws.Write(ctx, typ, p) == nil
}

// next computes what to send: urgent messages, ready offset-bound messages and at most one output frame.
func (c *client) next() (urgent, ctrl [][]byte, frame []byte, wait time.Duration, done bool) {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	head, tail := s.ring.Head(), s.ring.Tail()
	if c.cursor < tail {
		// The ring overwrote data this client had not received yet (another, faster client kept the reader going):
		// resynchronize with an in-band reset, exactly like a fresh attach in reset mode.
		c.cursor, c.acked, c.blocked = tail, tail, false
		c.urgent = append(c.urgent, ctrlJSON(attachMsg{Type: "attach", Mode: "reset", From: tail, Head: head}))
		c.ctrl = append(c.ctrl, ctrlMsg{off: head, data: ctrlJSON(attachEndMsg{Type: "attach-end", Head: head})})
		s.cond.Broadcast()
	}
	urgent, c.urgent = c.urgent, nil
	n := 0
	for n < len(c.ctrl) && c.ctrl[n].off <= c.cursor {
		ctrl = append(ctrl, c.ctrl[n].data)
		n++
	}
	if n == len(c.ctrl) {
		c.ctrl = nil
	} else if n > 0 {
		c.ctrl = append([]ctrlMsg(nil), c.ctrl[n:]...)
	}
	if !c.blocked && c.cursor < head {
		limit := head
		if len(c.ctrl) > 0 && c.ctrl[0].off < limit {
			limit = c.ctrl[0].off
		}
		if pending := limit - c.cursor; pending > 0 {
			since := time.Since(c.lastSend)
			if pending < MaxFrame && limit == head && since < CoalesceDelay && len(urgent) == 0 && len(ctrl) == 0 {
				// Coalesce: at most one partial frame per CoalesceDelay.
				wait = CoalesceDelay - since
			} else {
				sz := min(pending, MaxFrame)
				frame = s.ring.Slice(c.cursor, c.cursor+sz)
				c.cursor += int64(len(frame))
				c.lastSend = time.Now()
				if c.cursor-c.acked > HighWater {
					c.blocked = true
				}
			}
		}
	}
	if c.closing && frame == nil && len(urgent) == 0 && len(ctrl) == 0 && len(c.ctrl) == 0 && c.cursor >= head {
		done = true
	}
	return urgent, ctrl, frame, wait, done
}

// ack records the client's rendered offset and lifts flow control below the low-water mark.
func (c *client) ack(off int64) {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if off > c.cursor {
		off = c.cursor
	}
	if off > c.acked {
		c.acked = off
	}
	if c.blocked && c.cursor-c.acked < LowWater {
		c.blocked = false
		c.wake()
	}
	if s.ring.Head()-c.acked <= HighWater {
		s.cond.Broadcast() // the reader may be paused waiting for this client
	}
}

func (c *client) pingLoop() {
	t := time.NewTicker(wsPingInterval)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(c.ctx, wsPingInterval)
			err := c.ws.Ping(ctx)
			cancel()
			if err != nil {
				c.cancel()
				return
			}
		}
	}
}

// ---- reader -------------------------------------------------------------------------------------------------------

type clientMsg struct {
	Type   string `json:"type"`
	Cols   int    `json:"cols"`
	Rows   int    `json:"rows"`
	Offset int64  `json:"offset"`
	Name   string `json:"name"`
}

func (c *client) readLoop() {
	s := c.s
	for {
		typ, data, err := c.ws.Read(c.ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			if c.readOnly {
				c.warnReadOnly()
				continue
			}
			if err := s.writeInput(data, c); err != nil && !errors.Is(err, ErrNotConnected) {
				c.sendError(err.Error())
			}
			continue
		}
		var m clientMsg
		if err := json.Unmarshal(data, &m); err != nil {
			c.sendError("invalid control message")
			continue
		}
		switch m.Type {
		case "ack":
			c.ack(m.Offset)
		case "ping":
			c.urgentLocked([]byte(`{"type":"pong"}`))
		case "resize":
			if c.readOnly {
				continue // viewers follow the authoritative size
			}
			if m.Cols > 0 && m.Rows > 0 {
				s.resize(m.Cols, m.Rows, c)
			}
		case "reconnect":
			if c.readOnly {
				c.warnReadOnly()
				continue
			}
			if err := s.reconnect(); err != nil {
				c.sendError(err.Error())
			}
		case "break":
			if c.readOnly {
				c.warnReadOnly()
				continue
			}
			if err := s.Break(); err != nil {
				c.sendError(err.Error())
			}
		case "signal":
			if c.readOnly {
				c.warnReadOnly()
				continue
			}
			if err := s.Signal(m.Name); err != nil {
				c.sendError(err.Error())
			}
		default:
			c.sendError("unknown message type")
		}
	}
}

func (c *client) urgentLocked(msg []byte) {
	c.s.mu.Lock()
	c.queueUrgent(msg)
	c.s.mu.Unlock()
}

func (c *client) sendError(msg string) {
	c.urgentLocked(ctrlJSON(errorMsg{Type: "error", Message: msg}))
}

func (c *client) warnReadOnly() {
	c.s.mu.Lock()
	warned := c.roWarned
	c.roWarned = true
	c.s.mu.Unlock()
	if !warned {
		c.sendError("this view is read-only")
	}
}

// ---- admin shadow viewers -----------------------------------------------------------------------------------------

type shadowMsg struct {
	Type    string   `json:"type"` // "shadow"
	Viewers []string `json:"viewers"`
}

// shadowNamesLocked lists the administrators viewing the session read-only (one entry per viewer). s.mu held.
func (s *Session) shadowNamesLocked() []string {
	names := []string{}
	for c := range s.clients {
		if !c.shadow {
			continue
		}
		name := "administrator"
		if c.user != nil {
			name = c.user.Username
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// announceShadowsLocked tells the session's writable views who is shadowing it now (an empty list ends the hint).
func (s *Session) announceShadowsLocked() {
	msg := ctrlJSON(shadowMsg{Type: "shadow", Viewers: s.shadowNamesLocked()})
	for c := range s.clients {
		if !c.readOnly {
			c.queueUrgent(msg)
		}
	}
}
