package term

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/text/encoding"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Session is one runtime session (terminal or graphical). All mutable state is guarded by mu; outMu serializes the
// producers of output (backend pump, local notices) so the ring, recorder, text log and hooks observe one order.
// Lock order: outMu → mu.
type Session struct {
	m *Manager

	// Immutable after creation.
	ID        string
	Kind      model.SessionKind
	Protocol  model.Protocol
	OwnerID   string
	CreatedAt time.Time
	owner     *model.User

	outMu sync.Mutex

	mu      sync.Mutex
	inq     *inputQueue // input queue of the current backend generation
	cond    *sync.Cond  // pump pause (flow control); broadcast on ack / detach / close / generation change
	conn    *model.Connection
	secrets map[string]string
	quick   bool

	title       string
	oscTitle    string
	state       model.SessionState
	stateMsg    string
	exitCode    *int
	cols, rows  int
	cwd         string
	connectedAt *time.Time

	ring    *ring
	scanner oscScanner
	marks   []markRec // recent OSC 133 marks (replayed on attach)

	clients    map[*client]struct{}
	extClients int // viewers of graphical sessions (TrackClient)
	detachedAt time.Time
	lastWriter *client
	closed     bool

	gen            int // backend generation; bumped on (re)connect and close
	backend        Backend
	cancel         context.CancelFunc
	connectedOnce  bool
	attempts       int
	reconnectTimer *time.Timer

	enc            encoding.Encoding
	decoder        *transcoder // output, per backend generation
	encoder        *transcoder // input
	backspaceCtrlH bool
	termType       string
	autoReconnect  bool
	lastBell       time.Time

	recorder  *castRecorder
	recording *model.Recording
	logger    *textLogger
	logRec    *model.Recording

	pubMu    sync.Mutex
	lastPub  time.Time
	pubTimer *time.Timer

	echo atomic.Pointer[shellState] // FILE-2 shell integration: echo filter + alternate-screen tracker (shellint.go)
}

type markRec struct {
	offset int64
	msg    []byte
}

const (
	maxMarks      = 2048
	bellInterval  = 100 * time.Millisecond
	maxStateMsg   = 512
	maxBackoff    = 60 * time.Second
	exitWaitGrace = 5 * time.Second
)

// ---- accessors ----------------------------------------------------------------------------------------------------

// Info returns the JSON view of the session.
func (s *Session) Info() model.RuntimeSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.infoLocked()
}

func (s *Session) infoLocked() model.RuntimeSession {
	info := model.RuntimeSession{
		ID:           s.ID,
		Kind:         s.Kind,
		Protocol:     s.Protocol,
		Title:        s.title,
		State:        s.state,
		StateMessage: s.stateMsg,
		Cols:         s.cols,
		Rows:         s.rows,
		Clients:      len(s.clients) + s.extClients,
		Cwd:          s.cwd,
		Recording:    s.recorder != nil,
		Logging:      s.logger != nil,
		OwnerID:      s.OwnerID,
		CreatedAt:    s.CreatedAt,
	}
	if s.conn != nil {
		if !s.quick {
			info.ConnectionID = s.conn.ID
		}
		info.Host = s.conn.Host
		info.Username = s.conn.Username
	}
	if s.exitCode != nil {
		c := *s.exitCode
		info.ExitCode = &c
	}
	if s.connectedAt != nil {
		t := *s.connectedAt
		info.ConnectedAt = &t
	}
	if s.recording != nil {
		info.RecordingID = s.recording.ID
	}
	return info
}

// Owner returns the user that owns the session.
func (s *Session) Owner() *model.User { return s.owner }

// Size returns the current terminal size.
func (s *Session) Size() (cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols, s.rows
}

// State returns the current state and its message.
func (s *Session) State() (model.SessionState, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.stateMsg
}

// Title returns the session title.
func (s *Session) Title() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.title
}

// Cwd returns the last working directory reported by the shell (OSC 7), or "".
func (s *Session) Cwd() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cwd
}

// Connection returns a copy of the session's resolved connection (secrets stripped), e.g. for graphical modules.
func (s *Session) Connection() *model.Connection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.Clone()
}

// Resolve returns the session's connection and its current decrypted secrets: saved connections are re-resolved
// (so edits and newly saved secrets apply) and completed with answers remembered from prompts; quick-connect
// sessions return their ad-hoc secrets. Graphical modules use it when their viewer connects. Never serialize the
// secrets.
func (s *Session) Resolve(ctx context.Context) (*model.Connection, map[string]string, error) {
	return s.resolveForConnect(ctx)
}

// RememberSecret keeps a secret (e.g. a password answered at a prompt) in memory for later reconnects of this
// session. It is never persisted.
func (s *Session) RememberSecret(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secrets == nil {
		s.secrets = map[string]string{}
	}
	if value == "" {
		delete(s.secrets, key)
		return
	}
	s.secrets[key] = value
}

// Scrollback returns a copy of the output ring.
func (s *Session) Scrollback() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ring.Bytes()
}

// Offsets returns the ring's tail and head offsets.
func (s *Session) Offsets() (tail, head int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ring.Tail(), s.ring.Head()
}

// Closed reports whether the session has been closed.
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// SetStatus lets an opener report progress: StateConnecting or StateAuthenticating (while waiting for a prompt).
// Other states are managed by the session itself (use Manager.SetState for graphical sessions).
func (s *Session) SetStatus(st model.SessionState, msg string) {
	if st != model.StateConnecting && st != model.StateAuthenticating {
		return
	}
	s.mu.Lock()
	if s.closed || s.backend != nil || (s.state != model.StateConnecting && s.state != model.StateAuthenticating) {
		s.mu.Unlock()
		return
	}
	changed := s.state != st || s.stateMsg != msg
	s.state, s.stateMsg = st, cleanMessage(msg)
	if changed {
		s.queueStateLocked()
	}
	s.mu.Unlock()
	if changed {
		s.m.runStateHooks(s, st)
		s.m.publish(s)
	}
}

// Notice writes a local informational line into the terminal output (dimmed), e.g. an SSH banner or a reconnect
// separator. Newlines are normalized to CRLF and control characters are stripped.
func (s *Session) Notice(text string) {
	if s.Kind != model.KindTerminal {
		return
	}
	s.emitOutput(formatNotice(text))
}

// Banner writes server-provided text (e.g. the SSH pre-auth banner) into the terminal output, sanitized.
func (s *Session) Banner(text string) {
	if s.Kind != model.KindTerminal || strings.TrimSpace(text) == "" {
		return
	}
	s.emitOutput([]byte(sanitizeBanner(text)))
}

func formatNotice(text string) []byte {
	var b strings.Builder
	b.WriteString("\r\n\x1b[2m")
	for i, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(stripControls(line))
	}
	b.WriteString("\x1b[0m\r\n")
	return []byte(b.String())
}

// sanitizeBanner keeps printable text, tabs and newlines (as CRLF) of untrusted server text.
func sanitizeBanner(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		b.WriteString(stripControls(line))
		b.WriteString("\r\n")
	}
	out := b.String()
	if len(out) > 64<<10 {
		out = strings.ToValidUTF8(out[:64<<10], "") + "\r\n"
	}
	return out
}

func stripControls(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

func cleanMessage(msg string) string {
	msg = strings.ToValidUTF8(strings.TrimSpace(msg), "�")
	msg = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, msg)), " ")
	if len(msg) > maxStateMsg {
		msg = strings.ToValidUTF8(msg[:maxStateMsg], "") + "…"
	}
	return msg
}

// ---- output path --------------------------------------------------------------------------------------------------

// emitOutput appends UTF-8 output to the ring and distributes it to clients, recorder, text log and hooks.
func (s *Session) emitOutput(data []byte) {
	if len(data) == 0 {
		return
	}
	s.outMu.Lock()
	var markers int
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.outMu.Unlock()
		return
	}
	base := s.ring.Head()
	s.ring.Write(data)
	cwdChanged := false
	s.scanner.Scan(data, base, func(ev oscEvent) {
		switch ev.Kind {
		case oscTitle:
			s.oscTitle = ev.Text
			s.queueCtrlLocked(ev.Offset, ctrlJSON(titleMsg{Type: "title", Title: ev.Text}), true)
		case oscCwd:
			if ev.Text != s.cwd {
				s.cwd = ev.Text
				cwdChanged = true
				s.queueCtrlLocked(ev.Offset, ctrlJSON(cwdMsg{Type: "cwd", Path: ev.Text}), true)
			}
		case oscBell:
			now := time.Now()
			if now.Sub(s.lastBell) >= bellInterval {
				s.lastBell = now
				s.queueCtrlLocked(ev.Offset, []byte(`{"type":"bell"}`), true)
			}
		case oscMark:
			msg := ctrlJSON(markMsg{Type: "prompt-mark", Kind: string(ev.Mark), Offset: ev.Offset, ExitCode: ev.ExitCode})
			s.marks = append(s.marks, markRec{offset: ev.Offset, msg: msg})
			if len(s.marks) > maxMarks {
				s.marks = append(s.marks[:0], s.marks[len(s.marks)-maxMarks/2:]...)
			}
			s.queueCtrlLocked(ev.Offset, msg, true)
			if ev.Mark == 'A' {
				markers++
			}
		}
	})
	for c := range s.clients {
		c.wake()
	}
	rec, lg := s.recorder, s.logger
	s.mu.Unlock()

	if rec != nil {
		rec.Output(data)
		for ; markers > 0; markers-- {
			rec.Marker("")
		}
	}
	if lg != nil {
		lg.Write(data)
	}
	s.outMu.Unlock()
	// Hooks run without session locks so they may call back into the manager (Write, Notice, ...).
	s.m.runOutputHooks(s, data)
	if cwdChanged {
		s.m.publish(s)
	}
}

// queueCtrlLocked queues an offset-bound control message on every client.
func (s *Session) queueCtrlLocked(offset int64, msg []byte, droppable bool) {
	for c := range s.clients {
		c.queueCtrl(offset, msg, droppable)
	}
}

func (s *Session) queueStateLocked() {
	s.queueCtrlLocked(s.ring.Head(), s.stateMsgLocked(), false)
}

func (s *Session) stateMsgLocked() []byte {
	m := stateMsg{Type: "state", State: s.state, Message: s.stateMsg}
	if s.exitCode != nil {
		c := *s.exitCode
		m.ExitCode = &c
	}
	return ctrlJSON(m)
}

// ---- input path ---------------------------------------------------------------------------------------------------

// writeInput queues input for the backend. from is the WebSocket client that typed it (nil for API injection).
func (s *Session) writeInput(data []byte, from *client) error {
	return s.writeInputOpts(data, from, false)
}

// writeInputOpts is writeInput; sensitive input (secrets) is kept out of the recorder and masked for hooks.
func (s *Session) writeInputOpts(data []byte, from *client, sensitive bool) error {
	if len(data) == 0 {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	if s.backend == nil || s.state != model.StateConnected {
		s.mu.Unlock()
		return ErrNotConnected
	}
	var resize bool
	var cols, rows int
	if from != nil {
		// Size policy: the most recently active writer's size wins.
		s.lastWriter = from
		if from.cols > 0 && (from.cols != s.cols || from.rows != s.rows) {
			resize, cols, rows = true, from.cols, from.rows
		}
	}
	rec := s.recorder
	q := s.inq
	s.mu.Unlock()

	if resize {
		s.resize(cols, rows, from)
	}
	s.m.runInputHooks(s, data, sensitive)
	if rec != nil && !sensitive {
		rec.Input(data)
	}
	if s.deferInput(data) { // typed while a hidden shell-integration command is in flight (shellint.go)
		return nil
	}
	return q.push(inputItem{data: append([]byte(nil), data...)})
}

// resize applies a new terminal size (clamped) requested by from (nil = API).
func (s *Session) resize(cols, rows int, from *client) {
	cols, rows = ClampSize(cols, 80), ClampSize(rows, 24)
	s.mu.Lock()
	if from != nil {
		from.cols, from.rows = cols, rows
		s.lastWriter = from
	}
	if s.closed || (cols == s.cols && rows == s.rows) {
		s.mu.Unlock()
		return
	}
	s.cols, s.rows = cols, rows
	msg := ctrlJSON(resizeMsg{Type: "resize", Cols: cols, Rows: rows})
	for c := range s.clients {
		if c != from {
			c.queueUrgent(msg)
		}
	}
	var q *inputQueue
	if s.backend != nil {
		q = s.inq // otherwise the size is applied when the backend connects
	}
	rec := s.recorder
	s.mu.Unlock()
	if q != nil {
		_ = q.push(inputItem{resize: true, cols: cols, rows: rows})
	}
	if rec != nil {
		rec.Resize(cols, rows)
	}
	s.m.publish(s)
}

// inputQueue decouples WebSocket readers from backend writes: a slow backend must never block the socket read loop
// (which also carries flow-control acks).
type inputQueue struct {
	mu     sync.Mutex
	items  []inputItem
	bytes  int
	notify chan struct{}
}

type inputItem struct {
	data       []byte
	resize     bool
	cols, rows int
}

const maxQueuedInput = 8 << 20

var errInputOverflow = &model.Error{Code: model.CodeConflict, Msg: "input buffer full"}

func newInputQueue() *inputQueue { return &inputQueue{notify: make(chan struct{}, 1)} }

func (q *inputQueue) push(it inputItem) error {
	q.mu.Lock()
	if q.bytes+len(it.data) > maxQueuedInput {
		q.mu.Unlock()
		return errInputOverflow
	}
	q.items = append(q.items, it)
	q.bytes += len(it.data)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return nil
}

func (q *inputQueue) pop() (inputItem, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return inputItem{}, false
	}
	it := q.items[0]
	q.items[0] = inputItem{}
	q.items = q.items[1:]
	q.bytes -= len(it.data)
	return it, true
}

func (q *inputQueue) reset() {
	q.mu.Lock()
	q.items, q.bytes = nil, 0
	q.mu.Unlock()
}

// inputLoop writes queued input and resizes to the backend of one generation (each generation has its own queue,
// so nothing typed for a new backend can be consumed by the loop of an old one).
func (s *Session) inputLoop(ctx context.Context, gen int, be Backend, q *inputQueue) {
	for {
		it, ok := q.pop()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-q.notify:
				continue
			}
		}
		if ctx.Err() != nil {
			return
		}
		if it.resize {
			if err := be.Resize(it.cols, it.rows); err != nil {
				s.m.log.Debug("term: resize failed", "session", s.ID, "err", err)
			}
			continue
		}
		s.mu.Lock()
		stale := s.gen != gen
		enc, ctrlH := s.encoder, s.backspaceCtrlH
		s.mu.Unlock()
		if stale {
			return
		}
		out := it.data
		if ctrlH {
			out = mapBackspace(out)
		}
		if enc != nil {
			out = enc.Process(out)
		}
		if _, err := be.Write(out); err != nil {
			s.m.log.Debug("term: backend write failed", "session", s.ID, "err", err)
			// The pump notices the broken backend; drop further input for this generation.
			q.reset()
		}
	}
}

// ---- connection lifecycle -----------------------------------------------------------------------------------------

// startConnectLocked (re)starts the opener in a new backend generation. The caller holds s.mu.
func (s *Session) startConnectLocked(msg string) {
	if s.closed || s.Kind != model.KindTerminal {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.reconnectTimer != nil {
		s.reconnectTimer.Stop()
		s.reconnectTimer = nil
	}
	old := s.backend
	s.backend = nil
	s.gen++
	gen := s.gen
	ctx, cancel := context.WithCancel(WithSession(s.m.ctx, s))
	s.cancel = cancel
	s.state, s.stateMsg, s.exitCode = model.StateConnecting, cleanMessage(msg), nil
	s.queueStateLocked()
	s.cond.Broadcast()
	s.inq = newInputQueue()
	if old != nil {
		go old.Close()
	}
	go s.connect(ctx, gen)
}

func (s *Session) connect(ctx context.Context, gen int) {
	defer func() {
		if r := recover(); r != nil {
			s.m.log.Error("term: opener panicked", "session", s.ID, "protocol", s.Protocol, "panic", r)
			s.connectFailed(gen, fmt.Errorf("internal error in %s backend", s.Protocol))
		}
	}()
	s.m.runStateHooks(s, model.StateConnecting)
	s.m.publish(s)

	open := lookupOpener(string(s.Protocol))
	if open == nil {
		s.connectFailed(gen, Permanent(fmt.Errorf("protocol %q is not available", s.Protocol)))
		return
	}
	conn, secrets, err := s.resolveForConnect(ctx)
	if err != nil {
		s.connectFailed(gen, err)
		return
	}
	be, err := open(ctx, OpenRequest{Session: s, Connection: conn, Secrets: secrets, User: s.owner})
	if err != nil {
		if be != nil {
			be.Close()
		}
		s.connectFailed(gen, err)
		return
	}
	if be == nil {
		s.connectFailed(gen, fmt.Errorf("%s backend returned no connection", s.Protocol))
		return
	}

	s.mu.Lock()
	if s.closed || s.gen != gen {
		s.mu.Unlock()
		be.Close()
		return
	}
	now := time.Now().UTC()
	reconnected := s.connectedOnce
	s.backend = be
	s.state, s.stateMsg, s.exitCode = model.StateConnected, "", nil
	s.connectedAt = &now
	s.connectedOnce = true
	s.attempts = 0
	s.decoder = newDecoder(s.enc)
	cols, rows := s.cols, s.rows
	startup := ""
	if s.conn != nil {
		startup = s.conn.Options.String("startupCommand", "")
	}
	q := s.inq
	s.queueStateLocked()
	s.mu.Unlock()

	if err := be.Resize(cols, rows); err != nil {
		s.m.log.Debug("term: initial resize failed", "session", s.ID, "err", err)
	}
	go s.inputLoop(ctx, gen, be, q)
	go s.pump(gen, be)
	if reconnected {
		s.Notice("──── reconnected " + now.Local().Format("2006-01-02 15:04:05") + " ────")
	}
	if startup != "" {
		var once sync.Once
		send := func() {
			once.Do(func() {
				s.mu.Lock()
				live := !s.closed && s.gen == gen && s.state == model.StateConnected
				s.mu.Unlock()
				if !live {
					return
				}
				for _, line := range strings.Split(strings.ReplaceAll(startup, "\r\n", "\n"), "\n") {
					_ = q.push(inputItem{data: []byte(line + "\r")})
				}
			})
		}
		if h := s.m.startup.Load(); h == nil || !s.m.claimStartup(*h, s, conn, send) {
			send()
		}
	}
	s.m.runStateHooks(s, model.StateConnected)
	s.m.publish(s)
}

// resolveForConnect re-resolves saved connections on every (re)connect so edits and newly saved secrets apply;
// quick-connect sessions reuse their synthesized connection. Remembered prompt answers fill missing secrets.
func (s *Session) resolveForConnect(ctx context.Context) (*model.Connection, map[string]string, error) {
	s.mu.Lock()
	quick, conn := s.quick, s.conn.Clone()
	remembered := make(map[string]string, len(s.secrets))
	for k, v := range s.secrets {
		remembered[k] = v
	}
	s.mu.Unlock()
	if quick || conn == nil || conn.ID == "" || s.m.d == nil || s.m.d.Store == nil {
		return conn, remembered, nil
	}
	fresh, secrets, err := s.m.d.ResolveConnection(ctx, s.owner, conn.ID)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, nil, Permanent(fmt.Errorf("connection no longer exists: %w", err))
		}
		return nil, nil, err
	}
	for k, v := range remembered {
		if _, ok := secrets[k]; !ok {
			secrets[k] = v
		}
	}
	s.mu.Lock()
	s.conn = fresh.Clone()
	s.mu.Unlock()
	return fresh, secrets, nil
}

func (s *Session) connectFailed(gen int, err error) {
	s.mu.Lock()
	if s.closed || s.gen != gen {
		s.mu.Unlock()
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	msg := err.Error()
	if errors.Is(err, context.Canceled) {
		msg = "connection canceled"
	}
	s.state, s.stateMsg = model.StateError, cleanMessage(msg)
	retry := s.connectedOnce && s.autoReconnect && !IsPermanent(err) && !errors.Is(err, context.Canceled)
	var delay time.Duration
	if retry {
		delay = s.scheduleReconnectLocked(gen)
	}
	s.queueStateLocked()
	s.mu.Unlock()

	s.m.log.Debug("term: connect failed", "session", s.ID, "protocol", s.Protocol, "err", err)
	if retry {
		s.Notice(fmt.Sprintf("Connection failed: %s — retrying in %s", cleanMessage(msg), delay.Round(time.Second)))
	} else {
		s.Notice("Connection failed: " + cleanMessage(msg))
	}
	s.m.runStateHooks(s, model.StateError)
	s.m.publish(s)
}

// scheduleReconnectLocked arms the auto-reconnect timer with exponential backoff (1s … 60s, ±20% jitter).
func (s *Session) scheduleReconnectLocked(gen int) time.Duration {
	s.attempts++
	d := time.Second << min(s.attempts-1, 6)
	d = min(d, maxBackoff)
	d = time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
	if s.stateMsg != "" {
		s.stateMsg = cleanMessage(fmt.Sprintf("%s — reconnecting in %s (attempt %d)", s.stateMsg, d.Round(time.Second), s.attempts))
	} else {
		s.stateMsg = fmt.Sprintf("reconnecting in %s (attempt %d)", d.Round(time.Second), s.attempts)
	}
	if s.reconnectTimer != nil {
		s.reconnectTimer.Stop()
	}
	s.reconnectTimer = time.AfterFunc(d, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed || s.gen != gen || (s.state != model.StateDisconnected && s.state != model.StateError) {
			return
		}
		s.startConnectLocked(fmt.Sprintf("reconnecting (attempt %d)", s.attempts))
	})
	return d
}

// pump copies backend output into the session until the backend ends or the generation changes.
func (s *Session) pump(gen int, be Backend) {
	buf := make([]byte, readChunk)
	for {
		s.mu.Lock()
		for !s.closed && s.gen == gen && s.readerPausedLocked() {
			s.cond.Wait()
		}
		stale := s.closed || s.gen != gen
		dec := s.decoder
		s.mu.Unlock()
		if stale {
			return
		}
		n, err := be.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if dec != nil {
				chunk = dec.Process(chunk)
			}
			s.emitBackend(gen, chunk) // passes the shell-integration echo filter (shellint.go)
		}
		if err != nil {
			s.backendEnded(gen, be, err)
			return
		}
	}
}

// readerPausedLocked reports whether the backend reader must pause: every attached client has more than HighWater
// bytes of output it has not acknowledged yet (produced, whether sent or not). Pausing throttles the remote (e.g.
// through SSH window flow control) and keeps the ring from overrunning the clients. With no clients attached the
// reader never pauses and the ring overwrites old output.
func (s *Session) readerPausedLocked() bool {
	if len(s.clients) == 0 {
		return false
	}
	head := s.ring.Head()
	for c := range s.clients {
		if head-c.acked <= HighWater {
			return false
		}
	}
	return true
}

func (s *Session) backendEnded(gen int, be Backend, err error) {
	code := -1
	if ec, ok := be.(ExitCoder); ok {
		code = ec.ExitCode()
	}
	abnormal := err != nil && !errors.Is(err, io.EOF)

	s.mu.Lock()
	if s.closed || s.gen != gen {
		s.mu.Unlock()
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.backend = nil
	s.state = model.StateDisconnected
	s.stateMsg = ""
	if abnormal {
		s.stateMsg = cleanMessage("connection lost: " + err.Error())
	}
	if code >= 0 {
		c := code
		s.exitCode = &c
	}
	retry := abnormal && s.autoReconnect
	var delay time.Duration
	if retry {
		delay = s.scheduleReconnectLocked(gen)
	}
	s.queueStateLocked()
	rec, lg := s.recorder, s.logger
	s.cond.Broadcast()
	s.mu.Unlock()

	be.Close()
	var note string
	switch {
	case abnormal:
		note = "Connection lost: " + cleanMessage(err.Error())
	case code >= 0:
		note = fmt.Sprintf("Session ended (exit code %d).", code)
	default:
		note = "Session ended."
	}
	if retry {
		note += fmt.Sprintf(" Reconnecting in %s…", delay.Round(time.Second))
	}
	if rec != nil && code >= 0 {
		rec.Exit(code)
	}
	s.Notice(note)
	if lg != nil {
		lg.Note("--- " + note)
	}
	s.m.runStateHooks(s, model.StateDisconnected)
	s.m.publish(s)
}

// reconnect restarts the backend (manual reconnect). It is allowed in every state except closed.
func (s *Session) reconnect() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.Kind != model.KindTerminal {
		return ErrUnsupported
	}
	s.attempts = 0
	s.startConnectLocked("reconnecting")
	return nil
}

// Signal delivers a signal through the backend (Signaler).
func (s *Session) Signal(name string) error {
	name = strings.ToUpper(strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(name)), "SIG"))
	switch name {
	case "INT", "TERM", "KILL", "HUP", "QUIT", "USR1", "USR2", "STOP", "CONT", "WINCH":
	default:
		return &model.Error{Code: model.CodeBadRequest, Msg: "unsupported signal " + name}
	}
	be, err := s.liveBackend()
	if err != nil {
		return err
	}
	sg, ok := be.(Signaler)
	if !ok {
		return ErrUnsupported
	}
	return sg.Signal(name)
}

// Break sends a BREAK through the backend (Breaker).
func (s *Session) Break() error {
	be, err := s.liveBackend()
	if err != nil {
		return err
	}
	br, ok := be.(Breaker)
	if !ok {
		return ErrUnsupported
	}
	return br.SendBreak()
}

func (s *Session) liveBackend() (Backend, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.backend == nil {
		return nil, ErrNotConnected
	}
	return s.backend, nil
}

// ---- control messages (server → client JSON) ----------------------------------------------------------------------

type attachMsg struct {
	Type string `json:"type"`
	Mode string `json:"mode"`
	From int64  `json:"from"`
	Head int64  `json:"head"`
}

type attachEndMsg struct {
	Type string `json:"type"`
	Head int64  `json:"head"`
}

type stateMsg struct {
	Type     string             `json:"type"`
	State    model.SessionState `json:"state"`
	Message  string             `json:"message,omitempty"`
	ExitCode *int               `json:"exitCode,omitempty"`
}

type titleMsg struct {
	Type  string `json:"type"`
	Title string `json:"title"`
}

type cwdMsg struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

type resizeMsg struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

type readonlyMsg struct {
	Type  string `json:"type"`
	Value bool   `json:"value"`
}

type errorMsg struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type markMsg struct {
	Type     string `json:"type"`
	Kind     string `json:"kind"`
	Offset   int64  `json:"offset"`
	ExitCode *int   `json:"exitCode,omitempty"`
}

func ctrlJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"type":"error","message":"internal encoding error"}`)
	}
	return b
}
