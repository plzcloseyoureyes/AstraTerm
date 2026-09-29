package rdp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/events"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/netguard"
	"github.com/plzcloseyoureyes/astraterm/internal/rdp/guac"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// The guacd engine (RESEARCH §3.11, SPEC §6.3): /ws/guac/{id}?token=…&width=…&height=…&dpi=…&audio=…&image=…
// is a Guacamole WebSocket tunnel as guacamole-common-js' WebSocketTunnel expects it — subprotocol "guacamole", text
// frames of whole instructions, the internal-opcode tunnel UUID first, pings echoed. The Go side performs the guacd
// handshake with the vault credentials (they never reach the browser), answers guacd's "required" requests through
// the prompt broker, enforces the clipboard policy on both directions and audits drive transfers.

const (
	guacBatchLimit    = 64 << 10 // coalesce guacd instructions into WebSocket frames up to this size
	guacWriteTimeout  = 30 * time.Second
	guacStreamReserve = 56 // stream indexes 56..63 are reserved for streams opened by the tunnel (argv answers)
	guacMaxStreams    = 64 // GUAC_USER_MAX_STREAMS
)

// handleGuac serves /ws/guac/{id}: the owner's viewer, or an administrator's read-only view (shadow ticket).
func (h *handler) handleGuac(c *echo.Context) error {
	s, u, err := h.session(c, true)
	if err != nil {
		return err
	}
	shadow := s.OwnerID != u.ID
	query := c.Request().URL.Query()
	ws, err := httpx.AcceptWS(c, &websocket.AcceptOptions{Subprotocols: []string{"guacamole"}})
	if err != nil {
		h.log.Debug("rdp: guacamole websocket accept failed", "err", err)
		return nil
	}
	defer ws.CloseNow()
	ctx, cancel := context.WithCancel(c.Request().Context())
	defer cancel()
	release, err := h.c.Sessions.TrackClient(s.ID)
	if err != nil {
		_ = ws.Close(websocket.StatusPolicyViolation, strconv.Itoa(guac.StatusSessionClosed)+" The session was closed")
		return nil
	}
	v := &viewer{sessionID: s.ID, engine: engineGuacd, shadow: shadow, cancel: cancel}
	v.active.Store(true)
	h.viewers.add(v)
	t := &guacTunnel{h: h, ws: ws, s: s, user: u, ctx: ctx, cancel: cancel, query: query, shadow: shadow, viewer: v,
		dropIn: map[string]bool{}, dropOut: map[string]bool{}, unrecorded: map[string]bool{}}
	endMsg := t.run()
	h.viewers.remove(v)
	release()
	if endMsg != "" && !shadow {
		h.endState(s, model.StateDisconnected, endMsg)
	}
	return nil
}

type guacTunnel struct {
	h      *handler
	ws     *websocket.Conn
	s      *term.Session
	user   *model.User
	ctx    context.Context
	cancel context.CancelFunc
	query  url.Values

	tk     *ticket
	opts   rdpOptions
	gc     atomic.Pointer[guac.Conn]
	shadow bool // an administrator's read-only view of the owner's connection
	viewer *viewer
	rec    *guacRecorder // session recording (owner's tunnel, options.recording)

	fmu        sync.Mutex
	dropIn     map[string]bool // browser → guacd streams being dropped (policy)
	dropOut    map[string]bool // guacd → browser streams being dropped (policy)
	unrecorded map[string]bool // guacd → browser streams kept out of the recording (clipboard)
	nextOwn    int
	audits     int // drive transfer audit entries written (bounded)

	synced      atomic.Bool
	commitSaved atomic.Bool
	answering   atomic.Bool
	failed      atomic.Bool
	guacdErr    atomic.Value // string: last "error" instruction from guacd
}

// run performs the handshake and relays until either side ends; it returns the "disconnected" message ("" when the
// state was already set).
func (t *guacTunnel) run() string {
	h := t.h
	// The JS tunnel stays CONNECTING until the internal-opcode UUID arrives; it keeps pinging from then on, which
	// keeps the tunnel alive while guacd connects or the user answers prompts.
	if err := t.sendBrowser(guac.New(guac.InternalOpcode, newUUID())); err != nil {
		return "Viewer disconnected"
	}
	browserDone := make(chan error, 1)
	go func() { browserDone <- t.readBrowser() }()

	tk, ok := h.tickets.consume(t.query.Get("token"), t.s.ID, t.user.ID, engineGuacd)
	if !ok || tk.shadow != t.shadow {
		t.fail(guac.StatusClientUnauthorized, "The connection ticket is invalid or has expired — reconnect to get a new one")
		return ""
	}
	t.tk, t.opts = tk, parseOptions(tk.conn.Options)
	if t.shadow {
		return t.runShadow()
	}

	hs, cleanup, err := t.prepare()
	defer cleanup()
	if err != nil {
		var rej *certRejectedError
		if errors.As(err, &rej) {
			t.fail(guac.StatusClientForbidden, capitalize(rej.msg))
		} else if be, ok := netguard.IsBlocked(err); ok {
			t.fail(guac.StatusClientForbidden, capitalize(be.Error()))
		} else {
			t.fail(guac.StatusServerError, capitalize(err.Error()))
		}
		return ""
	}

	h.setState(t.s.ID, model.StateConnecting, "Connecting to guacd")
	conn, err := h.guacd.dial(t.ctx)
	if err != nil {
		t.fail(guac.StatusUpstreamNotFound, "Cannot reach guacd: "+guacdErrorText(err))
		return ""
	}
	gc, err := guac.Connect(t.ctx, conn, hs)
	if err != nil {
		_ = conn.Close()
		var ge *guac.Error
		if errors.As(err, &ge) {
			st := ge.Status
			if st == 0 {
				st = guac.StatusUpstreamError
			}
			t.fail(st, "guacd refused the connection: "+ge.Message)
		} else {
			t.fail(guac.StatusUpstreamError, "The guacd handshake failed: "+guacdErrorText(err))
		}
		return ""
	}
	defer gc.Close()
	// The recorder is set before the connection is published (t.gc): readBrowser records mouse input once it sees it.
	if t.opts.Recording {
		if rec, err := h.startGuacRecording(t.ctx, t.s, tk, hs.Width, hs.Height); err != nil {
			h.log.Warn("rdp: cannot record the session", "session", t.s.ID, "err", err)
		} else {
			t.rec = rec
			defer rec.finish()
			h.auditUser(t.ctx, t.user, "rdp.record.start", t.s.ID, map[string]any{"recordingId": rec.meta.ID})
		}
	}
	t.gc.Store(gc)
	// Administrators may join this guacd connection read-only while it runs.
	h.joins.set(t.s.ID, guacJoin{id: gc.ID, width: hs.Width, height: hs.Height})
	defer h.joins.drop(t.s.ID, gc.ID)
	dest := net.JoinHostPort(tk.host, strconv.Itoa(tk.port))
	h.setState(t.s.ID, model.StateConnecting, "Connecting to "+dest+" through guacd"+viaText(tk.conn))
	h.auditUser(t.ctx, t.user, "rdp.connect", t.s.ID, map[string]any{"engine": engineGuacd, "host": tk.host,
		"port": tk.port, "connectionId": tk.conn.ID, "via": routeDescription(tk.conn), "guacdConnection": gc.ID})

	// Stop the guacd connection when the browser side ends (or the session closes).
	go func() {
		select {
		case <-browserDone:
		case <-t.ctx.Done():
		}
		_ = gc.Send(guac.New("disconnect"))
		time.AfterFunc(2*time.Second, func() { _ = gc.Close() })
	}()

	end := t.pumpFromGuacd(gc)
	if !t.failed.Load() {
		_ = t.ws.Close(websocket.StatusNormalClosure, "")
	}
	t.cancel()
	if msg, _ := t.guacdErr.Load().(string); msg != "" {
		h.setState(t.s.ID, model.StateError, cleanMessage(msg))
		return ""
	}
	if t.failed.Load() || t.s.Closed() {
		return ""
	}
	return end
}

// prepare builds the guacd handshake: certificate trust through AstraTerm's store (guacd only knows "ignore-cert"),
// a loopback forwarder for gateway routes, and the connection parameters.
func (t *guacTunnel) prepare() (guac.Handshake, func(), error) {
	h, tk, opts := t.h, t.tk, t.opts
	cleanup := func() {}
	// guacd connects on its own: vet (and pin) the destination for restricted users first (SEC-7, netguard.go).
	target, err := h.guacdDestination(t.ctx, t.user, tk, opts)
	if err != nil {
		return guac.Handshake{}, cleanup, err
	}
	g := h.globalSettings(t.ctx)
	ignoreCert := opts.IgnoreCert
	if !ignoreCert && opts.Security != secRDP {
		h.setState(t.s.ID, model.StateConnecting, "Checking the server certificate")
		trusted, err := h.probeCertificate(t.ctx, t.s, t.user, tk)
		switch {
		case err == nil:
			ignoreCert = trusted
		case errors.Is(err, errCertRejected):
			return guac.Handshake{}, cleanup, err
		case isBlocked(err):
			return guac.Handshake{}, cleanup, err // never let guacd try a destination the policy refused
		default:
			// Unreachable / no TLS: guacd connects on its own and reports its own error.
			h.log.Debug("rdp: certificate probe failed", "session", t.s.ID, "err", err)
		}
	}
	host, port := target, tk.port
	if routeDescription(tk.conn) != "" {
		fwdHost := g.GuacdForwardHost
		if fwdHost == "" {
			fwdHost = "127.0.0.1"
		}
		f, err := h.startForwarder(t.ctx, forwardBindIP(fwdHost), t.user, tk.conn, tk.secrets, t.s, 16, 0)
		if err != nil {
			return guac.Handshake{}, cleanup, fmt.Errorf("cannot start the gateway forwarder: %w", err)
		}
		cleanup = f.Close
		host, port = fwdHost, f.Addr().Port
	}
	params := h.guacParams(tk, opts, g, host, port, ignoreCert)

	width, height, dpi := tk.width, tk.height, tk.dpi
	if w, err := strconv.Atoi(t.query.Get("width")); err == nil && w >= minDesktop && w <= maxDesktop {
		width = w
	}
	if hh, err := strconv.Atoi(t.query.Get("height")); err == nil && hh >= minDesktop && hh <= maxDesktop {
		height = hh
	}
	if d, err := strconv.Atoi(t.query.Get("dpi")); err == nil && d >= 72 && d <= 480 && opts.DPI == 0 {
		dpi = d
	}
	tz := opts.Timezone
	if tz == "" {
		tz = t.query.Get("timezone")
	}
	if !validTimezone(tz) {
		tz = ""
	}
	name := t.user.DisplayName
	if name == "" {
		name = t.user.Username
	}
	return guac.Handshake{
		Protocol: "rdp",
		Params:   params,
		Width:    width,
		Height:   height,
		DPI:      dpi,
		Audio:    mimetypes(t.query["audio"]),
		Video:    mimetypes(t.query["video"]),
		Image:    mimetypes(t.query["image"]),
		Timezone: tz,
		Name:     clipRunes(name, 64),
	}, cleanup, nil
}

var (
	mimeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]*/[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]*(;[A-Za-z0-9_.+=,-]+)*$`)
	tzRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+\-/]{0,63}$`)
)

func mimetypes(in []string) []string {
	var out []string
	for _, v := range in {
		for _, m := range strings.Split(v, "\n") {
			m = strings.TrimSpace(m)
			if m != "" && len(m) <= 100 && mimeRe.MatchString(m) && len(out) < 16 {
				out = append(out, m)
			}
		}
	}
	return out
}

func validTimezone(tz string) bool {
	return tz != "" && tzRe.MatchString(tz) && !strings.Contains(tz, "..")
}

// fail reports an error to the browser (as a Guacamole "error" instruction, which the client shows and follows with a
// disconnect) and to the session state, then closes the socket.
func (t *guacTunnel) fail(status int, msg string) {
	t.failed.Store(true)
	if !t.shadow {
		t.h.failState(t.s, t.viewer, msg)
	}
	_ = t.sendBrowser(guac.New("error", msg, strconv.Itoa(status)))
	_ = t.ws.Close(websocket.StatusNormalClosure, closeReason(strconv.Itoa(status)+" "+msg))
	t.cancel()
}

// closeReason fits a WebSocket close reason (≤ 123 bytes of valid UTF-8).
func closeReason(s string) string {
	if len(s) <= 120 {
		return s
	}
	cut := 120
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// runShadow joins the owner's guacd connection read-only (an administrator's view): no credentials, no session
// state changes, input dropped here and refused by guacd ("read-only" argument).
func (t *guacTunnel) runShadow() string {
	h, tk := t.h, t.tk
	conn, err := h.guacd.dial(t.ctx)
	if err != nil {
		t.fail(guac.StatusUpstreamNotFound, "Cannot reach guacd: "+guacdErrorText(err))
		return ""
	}
	width, height := tk.width, tk.height
	if w, err := strconv.Atoi(t.query.Get("width")); err == nil && w >= minDesktop && w <= maxDesktop {
		width = w
	}
	if hh, err := strconv.Atoi(t.query.Get("height")); err == nil && hh >= minDesktop && hh <= maxDesktop {
		height = hh
	}
	tz := t.query.Get("timezone")
	if !validTimezone(tz) {
		tz = ""
	}
	name := t.user.DisplayName
	if name == "" {
		name = t.user.Username
	}
	gc, err := guac.Connect(t.ctx, conn, guac.Handshake{
		Protocol: tk.join,
		Params:   map[string]string{"read-only": "true"},
		Width:    width,
		Height:   height,
		DPI:      defaultDPI,
		Image:    mimetypes(t.query["image"]),
		Timezone: tz,
		Name:     clipRunes(name+" (view only)", 64),
	})
	if err != nil {
		_ = conn.Close()
		t.fail(guac.StatusResourceClosed, "The remote desktop session is no longer available to view: "+guacdErrorText(err))
		return ""
	}
	defer gc.Close()
	t.gc.Store(gc)
	go func() {
		<-t.ctx.Done()
		_ = gc.Send(guac.New("disconnect"))
		time.AfterFunc(2*time.Second, func() { _ = gc.Close() })
	}()
	_ = t.pumpFromGuacd(gc)
	if !t.failed.Load() {
		_ = t.ws.Close(websocket.StatusNormalClosure, "")
	}
	t.cancel()
	return ""
}

// sendBrowser writes instructions to the browser as one text frame.
func (t *guacTunnel) sendBrowser(ins ...guac.Instruction) error {
	var b []byte
	for _, in := range ins {
		b = in.Append(b, guac.UTF16Units)
	}
	ctx, cancel := context.WithTimeout(t.ctx, guacWriteTimeout)
	defer cancel()
	return t.ws.Write(ctx, websocket.MessageText, b)
}

// readBrowser relays browser instructions to guacd (pings are echoed; nothing is forwarded before guacd is ready).
func (t *guacTunnel) readBrowser() error {
	for {
		typ, data, err := t.ws.Read(t.ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		ins, err := guac.ParseAll(string(data), guac.UTF16Units)
		if err != nil {
			t.h.log.Debug("rdp: malformed instruction from the browser", "session", t.s.ID, "err", err)
			return err
		}
		gc := t.gc.Load()
		var out []byte
		for _, in := range ins {
			if in.Opcode == guac.InternalOpcode {
				if in.Arg(0) == "ping" {
					if err := t.sendBrowser(in); err != nil {
						return err
					}
				}
				continue
			}
			if gc == nil || !t.allowFromBrowser(in) {
				continue
			}
			out = in.Append(out, guac.CodePoints)
			if in.Opcode == "mouse" {
				t.rec.write(in.Append(nil, guac.CodePoints))
			}
		}
		if len(out) > 0 && gc != nil {
			_ = gc.SetWriteDeadline(time.Now().Add(guacWriteTimeout))
			if err := gc.Write(out); err != nil {
				return err
			}
		}
	}
}

// shadowInput are the only browser instructions a read-only view forwards.
var shadowInput = map[string]bool{"sync": true, "nop": true, "disconnect": true, "ack": true}

// allowFromBrowser applies the policy to a browser instruction and audits transfers.
func (t *guacTunnel) allowFromBrowser(in guac.Instruction) bool {
	t.fmu.Lock()
	defer t.fmu.Unlock()
	if t.shadow && !shadowInput[in.Opcode] {
		return false
	}
	switch in.Opcode {
	case "argv", "pipe":
		// Parameter updates and named pipes are the tunnel's business (argv answers "required"), never the browser's.
		return false
	}
	if isStreamOpcode(in.Opcode) {
		if n, err := strconv.Atoi(in.Arg(0)); err == nil && n >= guacStreamReserve {
			return false // the browser never owns the tunnel's reserved streams
		}
	}
	switch in.Opcode {
	case "clipboard":
		if t.opts.DisableClipboard {
			t.dropIn[in.Arg(0)] = true
			return false
		}
	case "blob", "end":
		if t.dropIn[in.Arg(0)] {
			if in.Opcode == "end" {
				delete(t.dropIn, in.Arg(0))
			}
			return false
		}
	case "file":
		// file,<stream>,<mimetype>,<filename>: upload to the virtual drive (GFX-7).
		if !t.opts.EnableDrive {
			return false
		}
		t.auditTransfer("rdp.drive.upload", in.Arg(2), in.Arg(1))
	case "put":
		// put,<object>,<stream>,<mimetype>,<name>: upload through a filesystem object.
		if !t.opts.EnableDrive {
			return false
		}
		if n, err := strconv.Atoi(in.Arg(1)); err == nil && n >= guacStreamReserve {
			return false
		}
		t.auditTransfer("rdp.drive.upload", in.Arg(3), in.Arg(2))
	}
	return true
}

// isStreamOpcode reports browser instructions whose first argument is a stream index.
func isStreamOpcode(op string) bool {
	switch op {
	case "ack", "blob", "end", "clipboard", "file", "audio", "argv", "pipe", "img", "video":
		return true
	}
	return false
}

// maxTransferAudits bounds the drive transfer audit entries of one tunnel (a client could otherwise flood the log).
const maxTransferAudits = 1000

// auditTransfer records a drive transfer (t.fmu held).
func (t *guacTunnel) auditTransfer(action, name, mimetype string) {
	if t.audits >= maxTransferAudits {
		return
	}
	t.audits++
	t.h.auditUser(t.ctx, t.user, action, t.s.ID, map[string]any{"name": clipRunes(name, 255), "mimetype": clipRunes(mimetype, 100)})
}

// pumpFromGuacd relays guacd instructions to the browser, coalescing instructions that arrived together into one
// text frame (never splitting an instruction). It returns the disconnect message.
func (t *guacTunnel) pumpFromGuacd(gc *guac.Conn) string {
	var batch []byte
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		ctx, cancel := context.WithTimeout(t.ctx, guacWriteTimeout)
		err := t.ws.Write(ctx, websocket.MessageText, batch)
		cancel()
		batch = batch[:0]
		return err
	}
	for {
		in, raw, err := gc.R.ReadRaw()
		if err != nil {
			_ = flush()
			switch {
			case t.ctx.Err() != nil:
				return "Viewer disconnected"
			case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed):
				return "The remote desktop session ended"
			}
			return "Connection to guacd lost: " + rootMessage(err)
		}
		if !t.allowFromGuacd(gc, in) {
			continue
		}
		if t.rec != nil && t.recordable(in) {
			t.rec.write(raw)
		}
		batch = appendForBrowser(batch, in, raw)
		if gc.R.Buffered() == 0 || len(batch) >= guacBatchLimit {
			if err := flush(); err != nil {
				return "Viewer disconnected"
			}
		}
		if in.Opcode == "disconnect" {
			return "The remote desktop session ended"
		}
	}
}

// appendForBrowser re-encodes an instruction with UTF-16 lengths; ASCII instructions are copied as received.
func appendForBrowser(b []byte, in guac.Instruction, raw []byte) []byte {
	for _, c := range raw {
		if c >= 0x80 {
			return in.Append(b, guac.UTF16Units)
		}
	}
	return append(b, raw...)
}

// allowFromGuacd applies the policy to a guacd instruction, tracks the connection state and intercepts the
// instructions the tunnel handles itself.
func (t *guacTunnel) allowFromGuacd(gc *guac.Conn, in guac.Instruction) bool {
	if t.shadow {
		return t.allowFromGuacdShadow(in)
	}
	switch in.Opcode {
	case "required":
		// Credentials guacd needs (NLA without stored credentials): answered here, never by the browser.
		if t.answering.CompareAndSwap(false, true) {
			args := append([]string(nil), in.Args...)
			go func() {
				defer t.answering.Store(false)
				t.answerRequired(gc, args)
			}()
		}
		return false
	case "ack":
		if n, err := strconv.Atoi(in.Arg(0)); err == nil && n >= guacStreamReserve {
			return false // acknowledgements of the tunnel's own streams
		}
	case "size":
		// size,0,<w>,<h>: the remote desktop's size (layer 0) — what joining administrators and the recording see.
		if in.Arg(0) == "0" {
			w, errW := strconv.Atoi(in.Arg(1))
			hgt, errH := strconv.Atoi(in.Arg(2))
			if errW == nil && errH == nil && w > 0 && hgt > 0 {
				t.h.joins.set(t.s.ID, guacJoin{id: gc.ID, width: w, height: hgt})
				t.rec.setSize(w, hgt)
			}
		}
	case "sync":
		if t.synced.CompareAndSwap(false, true) {
			t.h.setState(t.s.ID, model.StateConnected, "")
		}
		if t.commitSaved.CompareAndSwap(true, false) {
			t.h.commitPending(t.ctx, t.s, t.user)
		}
	case "error":
		msg := strings.TrimSpace(in.Arg(0))
		if code, err := strconv.Atoi(in.Arg(1)); err == nil && msg == "" {
			msg = guac.StatusText(code)
		}
		if msg != "" {
			t.guacdErr.Store(msg)
		}
	case "clipboard":
		if t.opts.DisableClipboard {
			t.fmu.Lock()
			t.dropOut[in.Arg(0)] = true
			t.fmu.Unlock()
			return false
		}
	case "blob", "end":
		t.fmu.Lock()
		drop := t.dropOut[in.Arg(0)]
		if drop && in.Opcode == "end" {
			delete(t.dropOut, in.Arg(0))
		}
		t.fmu.Unlock()
		if drop {
			return false
		}
	case "file":
		// file,<stream>,<mimetype>,<filename>: a download from the virtual drive or a printed PDF.
		t.fmu.Lock()
		t.auditTransfer("rdp.drive.download", in.Arg(2), in.Arg(1))
		t.fmu.Unlock()
	}
	return true
}

// allowFromGuacdShadow filters what an administrator's read-only view receives: the display, not the owner's
// clipboard, files or credential requests.
func (t *guacTunnel) allowFromGuacdShadow(in guac.Instruction) bool {
	t.fmu.Lock()
	defer t.fmu.Unlock()
	switch in.Opcode {
	case "required", "argv":
		return false
	case "clipboard", "file", "pipe":
		t.dropOut[in.Arg(0)] = true
		return false
	case "blob", "end":
		drop := t.dropOut[in.Arg(0)]
		if drop && in.Opcode == "end" {
			delete(t.dropOut, in.Arg(0))
		}
		return !drop
	case "ack":
		if n, err := strconv.Atoi(in.Arg(0)); err == nil && n >= guacStreamReserve {
			return false
		}
	}
	return true
}

// recordable reports whether a forwarded guacd instruction belongs in the session recording: clipboard contents
// (and the streams carrying them) are left out.
func (t *guacTunnel) recordable(in guac.Instruction) bool {
	t.fmu.Lock()
	defer t.fmu.Unlock()
	switch in.Opcode {
	case "clipboard":
		t.unrecorded[in.Arg(0)] = true
		return false
	case "blob", "end":
		if t.unrecorded[in.Arg(0)] {
			if in.Opcode == "end" {
				delete(t.unrecorded, in.Arg(0))
			}
			return false
		}
	}
	return true
}

// answerRequired answers guacd's "required" instruction with argv streams: stored values first, the rest asked
// through the prompt broker.
func (t *guacTunnel) answerRequired(gc *guac.Conn, names []string) {
	values, err := t.h.requiredValues(t.ctx, t.s, t.user, t.tk, t.opts, names)
	if err != nil {
		t.fail(guac.StatusClientUnauthorized, capitalize(err.Error()))
		_ = gc.Send(guac.New("disconnect"))
		return
	}
	var ins []guac.Instruction
	t.fmu.Lock()
	for _, name := range names {
		idx := strconv.Itoa(guacStreamReserve + t.nextOwn%(guacMaxStreams-guacStreamReserve))
		t.nextOwn++
		ins = append(ins,
			guac.New("argv", idx, "text/plain", name),
			guac.New("blob", idx, base64.StdEncoding.EncodeToString([]byte(values[name]))),
			guac.New("end", idx))
	}
	t.fmu.Unlock()
	if err := gc.Send(ins...); err != nil {
		t.h.log.Debug("rdp: sending argv to guacd failed", "session", t.s.ID, "err", err)
		return
	}
	t.h.setState(t.s.ID, model.StateConnecting, "Signing in")
	if t.h.hasPending(t.s.ID) {
		t.commitSaved.Store(true)
	}
}

// requiredValues resolves the values of parameters guacd requires, prompting for those not stored.
func (h *handler) requiredValues(ctx context.Context, s *term.Session, u *model.User, tk *ticket, opts rdpOptions, names []string) (map[string]string, error) {
	out := map[string]string{}
	known := map[string]string{
		"username":         firstNonEmpty(tk.conn.Username, tk.secrets["username"]),
		"password":         tk.secrets[model.SecretPassword],
		"domain":           opts.Domain,
		"gateway-username": opts.GatewayUsername,
		"gateway-password": tk.secrets[model.SecretGatewayPassword],
		"gateway-domain":   opts.GatewayDomain,
	}
	var fields []model.PromptField
	var asked []string
	for _, n := range names {
		if v := known[n]; v != "" {
			out[n] = v
			continue
		}
		if n == "domain" || n == "gateway-domain" {
			out[n] = "" // optional: never worth a prompt on its own
			continue
		}
		label := requiredLabel(n)
		fields = append(fields, model.PromptField{Label: label, Echo: !strings.Contains(n, "password")})
		asked = append(asked, n)
	}
	if len(asked) == 0 {
		return out, nil
	}
	if h.d.Events == nil {
		return nil, errCredentialsRequired
	}
	canSave := tk.conn.ID != "" && tk.conn.OwnerID == u.ID
	h.setState(s.ID, model.StateAuthenticating, "Waiting for credentials")
	resp, err := h.d.Events.Prompt(ctx, u.ID, model.Prompt{
		Kind:         model.PromptPassword,
		Title:        "Remote desktop sign-in",
		Message:      "The remote desktop " + tk.host + " requires credentials.",
		SessionID:    s.ID,
		ConnectionID: tk.conn.ID,
		Fields:       fields,
		AllowSave:    canSave,
	})
	switch {
	case errors.Is(err, events.ErrNoInteractiveClient):
		return nil, errors.New("credentials are required but no AstraTerm window is connected")
	case errors.Is(err, events.ErrPromptTimeout):
		return nil, errors.New("the credential prompt was not answered in time")
	case err != nil:
		return nil, err
	}
	if !resp.Accept || len(resp.Values) < len(asked) {
		return nil, errors.New("sign-in canceled")
	}
	save := map[string]string{}
	for i, n := range asked {
		v := resp.Values[i]
		out[n] = v
		switch n {
		case "username":
			s.RememberSecret("username", strings.TrimSpace(v))
		case "password":
			s.RememberSecret(model.SecretPassword, v)
			if v != "" {
				save[model.SecretPassword] = v
			}
		case "gateway-password":
			s.RememberSecret(model.SecretGatewayPassword, v)
			if v != "" {
				save[model.SecretGatewayPassword] = v
			}
		}
	}
	if resp.Save && canSave && known["username"] != "" {
		h.pending.put(s.ID, tk.conn.ID, u.ID, save)
	}
	return out, nil
}

func (h *handler) hasPending(sessionID string) bool {
	h.pending.mu.Lock()
	defer h.pending.mu.Unlock()
	_, ok := h.pending.m[sessionID]
	return ok
}

func requiredLabel(name string) string {
	switch name {
	case "username":
		return "Username"
	case "password":
		return "Password"
	case "gateway-username":
		return "Gateway username"
	case "gateway-password":
		return "Gateway password"
	}
	return strings.ToUpper(name[:1]) + strings.ReplaceAll(name[1:], "-", " ")
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// probeCertificate checks the server's TLS certificate with AstraTerm's trust store (system roots, pinned rdp-tls
// certificates, or the user through the prompt broker) before guacd connects. It returns true when the certificate is
// trusted (guacd then skips its own verification), false when the server does not offer TLS.
func (h *handler) probeCertificate(ctx context.Context, s *term.Session, u *model.User, tk *ticket) (bool, error) {
	trust := h.newCertTrust(ctx, s, u, tk)
	sc, err := h.secureConnect(ctx, s, u, tk, probeConnectionRequest(protoSSL|protoHybrid|protoHybridEx), trust)
	if err != nil {
		var neg *negotiationError
		if errors.As(err, &neg) || errors.Is(err, errStandardSecurityOnly) {
			return false, nil
		}
		return false, err
	}
	_ = sc.tls.Close()
	return true, nil
}

// probeConnectionRequest builds a minimal X.224 Connection Request with an RDP_NEG_REQ for the given protocols.
func probeConnectionRequest(requested uint32) []byte {
	b := make([]byte, 19)
	b[0] = 3
	binary.BigEndian.PutUint16(b[2:4], 19)
	b[4] = 14 // length indicator
	b[5] = x224TypeCR
	b[11] = negTypeReq
	binary.LittleEndian.PutUint16(b[13:15], 8)
	binary.LittleEndian.PutUint32(b[15:19], requested)
	return b
}

// newUUID returns a random (version 4) UUID string.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("rdp: crypto/rand failed: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
