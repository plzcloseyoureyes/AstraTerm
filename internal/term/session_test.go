package term

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/config"
	"github.com/termstead/termstead/internal/model"
)

// ---- fake backend -------------------------------------------------------------------------------------------------

type fakeBackend struct {
	chunks    chan []byte
	pending   []byte
	closed    chan struct{}
	closeOnce sync.Once
	endErr    error
	exit      int
	reads     atomic.Int64

	mu      sync.Mutex
	written bytes.Buffer
	resizes [][2]int
	signals []string
}

func newFake() *fakeBackend {
	return &fakeBackend{chunks: make(chan []byte), closed: make(chan struct{}), endErr: io.EOF, exit: -1}
}

func (f *fakeBackend) Read(p []byte) (int, error) {
	f.reads.Add(1)
	if len(f.pending) == 0 {
		select {
		case c := <-f.chunks:
			f.pending = c
		case <-f.closed:
			return 0, f.endErr
		}
	}
	n := copy(p, f.pending)
	f.pending = f.pending[n:]
	return n, nil
}

func (f *fakeBackend) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written.Write(p)
}

func (f *fakeBackend) Resize(cols, rows int) error {
	f.mu.Lock()
	f.resizes = append(f.resizes, [2]int{cols, rows})
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) Signal(name string) error {
	f.mu.Lock()
	f.signals = append(f.signals, name)
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) ExitCode() int { return f.exit }

func (f *fakeBackend) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

// end finishes the backend as if the remote side ended with err (io.EOF = normal exit).
func (f *fakeBackend) end(err error, exit int) {
	f.endErr, f.exit = err, exit
	f.Close()
}

func (f *fakeBackend) input() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written.String()
}

func (f *fakeBackend) lastResize() [2]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.resizes) == 0 {
		return [2]int{}
	}
	return f.resizes[len(f.resizes)-1]
}

// ---- harness ------------------------------------------------------------------------------------------------------

const fakeProto = "faketest"

// backends feeds the fake protocol's opener; openErrs, when non-empty, makes the next opens fail.
type harness struct {
	t        *testing.T
	m        *Manager
	srv      *httptest.Server
	backends chan *fakeBackend
	openErrs chan error
	opens    atomic.Int64
	user     *model.User
	cancel   context.CancelFunc
}

var registerOnce sync.Once
var currentHarness atomic.Pointer[harness]

func newHarness(t *testing.T, ttl time.Duration) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	d := &app.Deps{
		Ctx: ctx,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Cfg: &config.Config{ScrollbackBytes: 64 << 10, DetachedSessionTTL: ttl, Mode: config.ModeDesktop},
	}
	h := &harness{t: t, m: New(d), backends: make(chan *fakeBackend, 8), openErrs: make(chan error, 8),
		user: &model.User{ID: "u1", Username: "alice", Role: model.RoleAdmin}, cancel: cancel}
	currentHarness.Store(h)
	registerOnce.Do(func() {
		RegisterProtocol(fakeProto, func(ctx context.Context, req OpenRequest) (Backend, error) {
			h := currentHarness.Load()
			h.opens.Add(1)
			select {
			case err := <-h.openErrs:
				return nil, err
			default:
			}
			select {
			case b := <-h.backends:
				return b, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	})
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ws.SetReadLimit(16 << 20)
		off, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		q := r.URL.Query()
		opts := AttachOptions{Offset: off, ReadOnly: q.Get("ro") == "1", Shadow: q.Get("shadow") == "1"}
		if u := q.Get("user"); u != "" {
			opts.User = &model.User{ID: "id-" + u, Username: u, Role: model.RoleAdmin}
		}
		_ = h.m.Attach(r.Context(), q.Get("id"), ws, opts)
	}))
	t.Cleanup(func() {
		h.srv.CloseClientConnections()
		h.srv.Close()
		cancel()
	})
	return h
}

// create starts a session on the fake protocol and waits until backend b is connected.
func (h *harness) create(b *fakeBackend, opts model.Options) *Session {
	h.t.Helper()
	if opts == nil {
		opts = model.Options{}
	}
	h.backends <- b
	s, err := h.m.Create(context.Background(), h.user, CreateRequest{
		Connection: &model.Connection{Protocol: fakeProto, Options: opts},
		Cols:       80, Rows: 24,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.waitState(s, model.StateConnected)
	return s
}

func (h *harness) waitState(s *Session, want model.SessionState) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := s.State(); st == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	st, msg := s.State()
	h.t.Fatalf("state %s (%s), want %s", st, msg, want)
}

// emit pushes output through the backend and waits until the session consumed it.
func (h *harness) emit(b *fakeBackend, s *Session, data []byte) {
	h.t.Helper()
	_, head := s.Offsets()
	b.chunks <- data
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, nh := s.Offsets(); nh >= head+int64(len(data)) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatal("output not consumed")
}

type wsClient struct {
	t      *testing.T
	ws     *websocket.Conn
	in     chan wsFrame
	from   int64
	data   bytes.Buffer
	msgs   []map[string]any
	resets int
}

type wsFrame struct {
	typ  websocket.MessageType
	data []byte
}

func (h *harness) attach(s *Session, offset int64, ro bool, extraQuery ...string) *wsClient {
	h.t.Helper()
	url := strings.Replace(h.srv.URL, "http", "ws", 1) + "/?id=" + s.ID + "&offset=" + strconv.FormatInt(offset, 10)
	if ro {
		url += "&ro=1"
	}
	for _, q := range extraQuery {
		url += "&" + q
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	ws.SetReadLimit(16 << 20)
	c := &wsClient{t: h.t, ws: ws, in: make(chan wsFrame)}
	// A dedicated reader: coder/websocket closes the connection when a Read context expires, so tests must not
	// read with timeouts. The unbuffered channel keeps TCP backpressure realistic.
	go func() {
		defer close(c.in)
		for {
			typ, p, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			c.in <- wsFrame{typ, p}
		}
	}()
	h.t.Cleanup(func() { ws.CloseNow() })
	return c
}

// read processes one message; it returns false on timeout / close.
func (c *wsClient) read(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var f wsFrame
	select {
	case fr, ok := <-c.in:
		if !ok {
			return false
		}
		f = fr
	case <-timer.C:
		return false
	}
	typ, p := f.typ, f.data
	if typ == websocket.MessageBinary {
		c.data.Write(p)
		return true
	}
	var m map[string]any
	if err := json.Unmarshal(p, &m); err != nil {
		c.t.Fatalf("bad json %q", p)
	}
	if m["type"] == "attach" {
		if m["mode"] == "reset" {
			c.resets++
			c.data.Reset()
		}
		c.from = int64(m["from"].(float64))
	}
	c.msgs = append(c.msgs, m)
	return true
}

func (c *wsClient) until(cond func() bool, timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			c.t.Fatalf("condition not met; received %d bytes, msgs %v", c.data.Len(), c.msgs)
		}
		c.read(time.Until(deadline))
	}
}

func (c *wsClient) offset() int64 { return c.from + int64(c.data.Len()) }

func (c *wsClient) send(v any) {
	c.t.Helper()
	b, _ := json.Marshal(v)
	if err := c.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *wsClient) ack() { c.send(map[string]any{"type": "ack", "offset": c.offset()}) }

func (c *wsClient) input(s string) {
	c.t.Helper()
	if err := c.ws.Write(context.Background(), websocket.MessageBinary, []byte(s)); err != nil {
		c.t.Fatal(err)
	}
}

func (c *wsClient) msg(typ string) map[string]any {
	for i := len(c.msgs) - 1; i >= 0; i-- {
		if c.msgs[i]["type"] == typ {
			return c.msgs[i]
		}
	}
	return nil
}

func (c *wsClient) hasMsg(typ string) bool { return c.msg(typ) != nil }

// ---- tests --------------------------------------------------------------------------------------------------------

func TestAttachDeltaAndReset(t *testing.T) {
	h := newHarness(t, 0)
	b := newFake()
	s := h.create(b, nil)
	h.emit(b, s, bytes.Repeat([]byte("a"), 100))

	c := h.attach(s, 0, false)
	c.until(func() bool { return c.hasMsg("attach-end") }, 5*time.Second)
	at := c.msg("attach")
	if at["mode"] != "delta" || at["from"].(float64) != 0 || at["head"].(float64) != 100 || c.data.Len() != 100 {
		t.Fatalf("attach %v, %d bytes", at, c.data.Len())
	}
	if !c.hasMsg("state") || !c.hasMsg("resize") || c.msg("readonly")["value"] != false {
		t.Fatalf("initial messages %v", c.msgs)
	}

	// Delta from the middle.
	c2 := h.attach(s, 60, false)
	c2.until(func() bool { return c2.hasMsg("attach-end") }, 5*time.Second)
	if at := c2.msg("attach"); at["mode"] != "delta" || at["from"].(float64) != 60 || c2.data.Len() != 40 {
		t.Fatalf("delta attach %v, %d bytes", at, c2.data.Len())
	}

	// Offset beyond head → reset.
	c3 := h.attach(s, 5000, false)
	c3.until(func() bool { return c3.hasMsg("attach-end") }, 5*time.Second)
	if at := c3.msg("attach"); at["mode"] != "reset" || at["from"].(float64) != 0 {
		t.Fatalf("reset attach %v", at)
	}

	// Overflow the ring (MinScrollback): an old offset yields a reset from the ring tail. Detach the idle viewers
	// first — clients that never acknowledge would (correctly) pause the reader.
	for _, cl := range []*wsClient{c, c2, c3} {
		cl.ws.Close(websocket.StatusNormalClosure, "")
	}
	waitFor(t, func() bool { return s.Info().Clients == 0 }, "detach")
	stream := make([]byte, 3<<20)
	for i := range stream {
		stream[i] = byte('A' + i%26)
	}
	for i := 0; i < len(stream); i += 64 << 10 {
		h.emit(b, s, stream[i:i+64<<10])
	}
	tail, head := s.Offsets()
	if head != int64(100+len(stream)) || tail != head-MinScrollback {
		t.Fatalf("tail %d head %d", tail, head)
	}
	c4 := h.attach(s, 10, false)
	for !c4.hasMsg("attach-end") {
		if !c4.read(5 * time.Second) {
			t.Fatal("no attach-end")
		}
		c4.ack()
	}
	if at := c4.msg("attach"); at["mode"] != "reset" || int64(at["from"].(float64)) != tail {
		t.Fatalf("reset attach %v", at)
	}
	if !bytes.Equal(c4.data.Bytes(), stream[len(stream)-MinScrollback:]) {
		t.Fatal("reset replay content mismatch")
	}
	c4.ack()
	// Live output keeps streaming after attach-end, contiguous with the replay.
	h.emit(b, s, []byte("LIVE"))
	c4.until(func() bool { return strings.HasSuffix(c4.data.String(), "LIVE") }, 5*time.Second)
	if c4.offset() != head+4 {
		t.Fatalf("offset %d want %d", c4.offset(), head+4)
	}
}

func TestFlowControlPausesReader(t *testing.T) {
	h := newHarness(t, 0)
	b := newFake()
	s := h.create(b, nil)
	c := h.attach(s, 0, false)
	c.until(func() bool { return c.hasMsg("attach-end") }, 5*time.Second)

	const total = 3 << 20
	stream := make([]byte, total)
	for i := range stream {
		stream[i] = byte(i % 251)
	}
	var sent atomic.Int64
	go func() {
		for i := 0; i < total; i += 16 << 10 {
			b.chunks <- stream[i : i+16<<10]
			sent.Add(16 << 10)
		}
	}()
	// Without acks the client receives at most the high-water mark (+ one frame) and the reader pauses.
	time.Sleep(300 * time.Millisecond)
	for c.read(200 * time.Millisecond) {
	}
	if got := c.data.Len(); got > HighWater+64<<10 || got < HighWater {
		t.Fatalf("received %d bytes without acking", got)
	}
	stalled := sent.Load()
	time.Sleep(200 * time.Millisecond)
	if sent.Load() != stalled || stalled >= total {
		t.Fatalf("reader did not pause: %d → %d of %d", stalled, sent.Load(), total)
	}
	// Acking resumes the stream until everything arrived intact.
	deadline := time.Now().Add(20 * time.Second)
	for c.data.Len() < total && time.Now().Before(deadline) {
		c.ack()
		for c.read(50 * time.Millisecond) {
			if c.data.Len()%(256<<10) == 0 {
				break
			}
		}
	}
	if !bytes.Equal(c.data.Bytes(), stream) {
		t.Fatalf("stream mismatch: got %d bytes", c.data.Len())
	}
}

func TestSlowClientDoesNotBlockFastOne(t *testing.T) {
	h := newHarness(t, 0)
	b := newFake()
	s := h.create(b, nil)
	fast := h.attach(s, 0, false)
	slow := h.attach(s, 0, false)
	fast.until(func() bool { return fast.hasMsg("attach-end") }, 5*time.Second)
	slow.until(func() bool { return slow.hasMsg("attach-end") }, 5*time.Second)

	const total = 6 << 20
	stream := make([]byte, total)
	for i := range stream {
		stream[i] = byte(i % 253)
	}
	go func() {
		for i := 0; i < total; i += 32 << 10 {
			b.chunks <- stream[i : i+32<<10]
		}
	}()
	// The fast client keeps acking and receives everything although the slow one never acks.
	deadline := time.Now().Add(20 * time.Second)
	for fast.data.Len() < total && time.Now().Before(deadline) {
		if fast.read(100 * time.Millisecond) {
			fast.ack()
		}
	}
	if !bytes.Equal(fast.data.Bytes(), stream) {
		t.Fatalf("fast client got %d bytes", fast.data.Len())
	}
	// The slow client fell out of the ring: it gets an in-band reset and then the current ring tail.
	for slow.read(300 * time.Millisecond) {
	}
	slow.ack()
	slow.until(func() bool { return slow.resets > 0 && slow.offset() == int64(total) }, 10*time.Second)
	if !bytes.Equal(slow.data.Bytes(), stream[slow.from:]) {
		t.Fatal("slow client data after reset does not match the stream")
	}
}

func TestInputResizeAndReadOnly(t *testing.T) {
	h := newHarness(t, 0)
	b := newFake()
	s := h.create(b, model.Options{"backspace": "ctrl-h"})
	w := h.attach(s, 0, false)
	ro := h.attach(s, 0, true)
	w.until(func() bool { return w.hasMsg("attach-end") }, 5*time.Second)
	ro.until(func() bool { return ro.hasMsg("attach-end") }, 5*time.Second)
	if ro.msg("readonly")["value"] != true {
		t.Fatal("viewer not told it is read-only")
	}

	w.input("ls\x7f\r")
	waitFor(t, func() bool { return b.input() == "ls\b\r" }, "input with ctrl-h mapping")

	w.send(map[string]any{"type": "resize", "cols": 5000, "rows": 1})
	waitFor(t, func() bool { return b.lastResize() == [2]int{MaxSize, MinSize} }, "clamped resize")
	ro.until(func() bool {
		m := ro.msg("resize")
		return m != nil && m["cols"].(float64) == MaxSize && m["rows"].(float64) == MinSize
	}, 5*time.Second)

	// The viewer can neither type nor resize.
	ro.input("rm -rf /\r")
	ro.send(map[string]any{"type": "resize", "cols": 40, "rows": 10})
	ro.until(func() bool { return ro.hasMsg("error") }, 5*time.Second)
	time.Sleep(100 * time.Millisecond)
	if b.input() != "ls\b\r" || b.lastResize() != [2]int{MaxSize, MinSize} {
		t.Fatalf("read-only client changed the session: %q %v", b.input(), b.lastResize())
	}

	// Signals and pings.
	w.send(map[string]any{"type": "signal", "name": "int"})
	waitFor(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return len(b.signals) == 1 && b.signals[0] == "INT" }, "signal")
	w.send(map[string]any{"type": "ping"})
	w.until(func() bool { return w.hasMsg("pong") }, 5*time.Second)

	// API injection goes through the same queue.
	if err := h.m.Write(s.ID, []byte("echo hi\r")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.HasSuffix(b.input(), "echo hi\r") }, "api input")
}

func TestOSCEventsAndMarksReplay(t *testing.T) {
	h := newHarness(t, 0)
	b := newFake()
	s := h.create(b, nil)
	c := h.attach(s, 0, false)
	c.until(func() bool { return c.hasMsg("attach-end") }, 5*time.Second)
	h.emit(b, s, []byte("\x1b]0;hello\x07\x1b]7;file://h/srv/x\x07\x1b]133;A\x07$ \x1b]133;D;1\x07\a"))
	c.until(func() bool { return c.hasMsg("title") && c.hasMsg("cwd") && c.hasMsg("bell") && len(c.marks()) == 2 }, 5*time.Second)
	if c.msg("title")["title"] != "hello" || c.msg("cwd")["path"] != "/srv/x" || s.Cwd() != "/srv/x" {
		t.Fatalf("msgs %v", c.msgs)
	}
	if m := c.marks()[1]; m["kind"] != "D" || m["exitCode"].(float64) != 1 {
		t.Fatalf("mark %v", m)
	}
	// A fresh attach replays title, cwd and prompt marks.
	c2 := h.attach(s, 0, false)
	c2.until(func() bool { return c2.hasMsg("attach-end") }, 5*time.Second)
	if c2.msg("title")["title"] != "hello" || c2.msg("cwd")["path"] != "/srv/x" || len(c2.marks()) != 2 {
		t.Fatalf("replayed msgs %v", c2.msgs)
	}
}

func (c *wsClient) marks() []map[string]any {
	var out []map[string]any
	for _, m := range c.msgs {
		if m["type"] == "prompt-mark" {
			out = append(out, m)
		}
	}
	return out
}

func TestExitReconnectAndClose(t *testing.T) {
	h := newHarness(t, 0)
	b := newFake()
	s := h.create(b, model.Options{"startupCommand": "cd /tmp"})
	waitFor(t, func() bool { return b.input() == "cd /tmp\r" }, "startup command")

	var states []model.SessionState
	var mu sync.Mutex
	closed := make(chan struct{})
	remove := h.m.AddHooks(Hooks{
		OnState: func(_ *Session, st model.SessionState) { mu.Lock(); states = append(states, st); mu.Unlock() },
		OnClose: func(*Session) { close(closed) },
	})
	defer remove()

	c := h.attach(s, 0, false)
	c.until(func() bool { return c.hasMsg("attach-end") }, 5*time.Second)
	b.end(io.EOF, 3)
	h.waitState(s, model.StateDisconnected)
	c.until(func() bool {
		m := c.msg("state")
		return m != nil && m["state"] == "disconnected" && m["exitCode"].(float64) == 3
	}, 5*time.Second)
	if info := s.Info(); info.ExitCode == nil || *info.ExitCode != 3 {
		t.Fatalf("info %+v", info)
	}
	// Input while disconnected is rejected.
	if err := h.m.Write(s.ID, []byte("x")); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("write err %v", err)
	}

	// Manual reconnect over the socket opens a new backend on the same session.
	b2 := newFake()
	h.backends <- b2
	c.send(map[string]any{"type": "reconnect"})
	h.waitState(s, model.StateConnected)
	c.until(func() bool { return strings.Contains(c.data.String(), "reconnected") }, 5*time.Second)
	waitFor(t, func() bool { return b2.input() == "cd /tmp\r" }, "startup command after reconnect")

	if err := h.m.Close(s.ID); err != nil {
		t.Fatal(err)
	}
	c.until(func() bool { m := c.msg("state"); return m != nil && m["state"] == "closed" }, 5*time.Second)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("OnClose not called")
	}
	select {
	case <-b2.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("backend not closed")
	}
	if h.m.Get(s.ID) != nil {
		t.Fatal("closed session still listed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(states) == 0 || states[len(states)-1] != model.StateClosed {
		t.Fatalf("states %v", states)
	}
}

func TestAutoReconnect(t *testing.T) {
	h := newHarness(t, 0)
	b := newFake()
	s := h.create(b, model.Options{"autoReconnect": true})
	// A clean exit does not reconnect.
	b.end(io.EOF, 0)
	h.waitState(s, model.StateDisconnected)
	time.Sleep(1500 * time.Millisecond)
	if st, _ := s.State(); st != model.StateDisconnected || h.opens.Load() != 1 {
		t.Fatalf("clean exit reconnected: %s opens=%d", st, h.opens.Load())
	}
	// A lost connection does (after ~1s backoff).
	b2 := newFake()
	h.backends <- b2
	if err := h.m.Reconnect(s.ID); err != nil {
		t.Fatal(err)
	}
	h.waitState(s, model.StateConnected)
	b3 := newFake()
	h.backends <- b3
	b2.end(errors.New("connection reset by peer"), -1)
	h.waitState(s, model.StateDisconnected)
	if _, msg := s.State(); !strings.Contains(msg, "reconnecting") {
		t.Fatalf("message %q", msg)
	}
	h.waitState(s, model.StateConnected)
	// An auth failure (permanent) on retry stops the loop.
	h.openErrs <- Permanent(errors.New("permission denied"))
	b3.end(errors.New("broken pipe"), -1)
	h.waitState(s, model.StateError)
	opens := h.opens.Load()
	time.Sleep(2500 * time.Millisecond)
	if h.opens.Load() != opens {
		t.Fatal("retried after a permanent error")
	}
}

func TestEncodingSession(t *testing.T) {
	h := newHarness(t, 0)
	b := newFake()
	s := h.create(b, model.Options{"encoding": "koi8-r"})
	c := h.attach(s, 0, false)
	c.until(func() bool { return c.hasMsg("attach-end") }, 5*time.Second)
	enc, _ := LookupEncoding("koi8-r")
	koi, _ := enc.NewEncoder().Bytes([]byte("Привет"))
	// Output split inside nothing (single-byte charset) but in two chunks anyway.
	h.emit(b, s, koi[:3])
	h.emit(b, s, koi[3:])
	c.until(func() bool { return c.data.String() == "Привет" }, 5*time.Second)
	c.input("мир")
	want, _ := enc.NewEncoder().Bytes([]byte("мир"))
	waitFor(t, func() bool { return b.input() == string(want) }, "encoded input")
}

func TestDetachedSessionReaper(t *testing.T) {
	h := newHarness(t, time.Second)
	b := newFake()
	s := h.create(b, nil)
	c := h.attach(s, 0, false)
	c.until(func() bool { return c.hasMsg("attach-end") }, 5*time.Second)
	time.Sleep(2500 * time.Millisecond)
	if h.m.Get(s.ID) == nil {
		t.Fatal("attached session was reaped")
	}
	c.ws.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return h.m.Get(s.ID) == nil }, "reaping")
}

func TestManagerListAndRename(t *testing.T) {
	h := newHarness(t, 0)
	b1, b2 := newFake(), newFake()
	s1 := h.create(b1, nil)
	s2 := h.create(b2, nil)
	other := &model.User{ID: "u2", Username: "bob", Role: model.RoleUser}
	if got := h.m.List(h.user, false); len(got) != 2 || got[0] != s1 || got[1] != s2 {
		t.Fatalf("list %v", got)
	}
	if got := h.m.List(other, true); len(got) != 0 {
		t.Fatal("non-admin listed other sessions")
	}
	if err := h.m.Rename(s1.ID, "  prod\x1b[31m db "); err != nil {
		t.Fatal(err)
	}
	if s1.Title() != "prod[31m db" {
		t.Fatalf("title %q", s1.Title())
	}
	if err := h.m.Rename(s1.ID, ""); err != nil || s1.Title() != fakeProto {
		t.Fatalf("default title %q", s1.Title())
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
