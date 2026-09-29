package vnc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/events"
	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/term"
)

// WebSocket close codes of /ws/vnc/:id (the viewer maps them to messages and decides whether to retry).
const (
	CloseServerEnded   = websocket.StatusNormalClosure // the VNC server closed the connection
	CloseShutdown      = websocket.StatusGoingAway     // Termstead is shutting down
	CloseBadRequest    = websocket.StatusCode(4400)    // not a VNC session / protocol error of the viewer
	CloseAuthFailed    = websocket.StatusCode(4401)    // authentication rejected (permanent)
	CloseForbidden     = websocket.StatusCode(4403)    // not allowed (e.g. pass-through for read-only viewers)
	CloseNotFound      = websocket.StatusCode(4404)    // unknown session
	CloseUnavailable   = websocket.StatusCode(4409)    // reverse connection already used / gone
	CloseSessionClosed = websocket.StatusCode(4410)    // the session was closed
	CloseLocked        = websocket.StatusCode(4423)    // the vault is locked
	CloseInsecure      = websocket.StatusCode(4426)    // weaker security than offered needs the user's confirmation
	CloseCanceled      = websocket.StatusCode(4499)    // the user canceled a prompt / rejected a certificate
	CloseInternal      = websocket.StatusCode(4500)
	CloseConnectFailed = websocket.StatusCode(4502) // the VNC server could not be reached / handshake failed (retryable)
	CloseTimeout       = websocket.StatusCode(4504)
	CloseUnsupported   = websocket.StatusCode(4505) // no usable security type (permanent)
)

const (
	handshakeTimeout = 30 * time.Second // per protocol step with the server
	presentTimeout   = 30 * time.Second // browser side of the RFB handshake
	pingInterval     = 25 * time.Second
	pingTimeout      = 20 * time.Second
	maxConnectTries  = 4
	relayBuffer      = 64 << 10
)

// viewer is one browser attached to a VNC session through /ws/vnc/:id.
type viewer struct {
	m        *Module
	s        *term.Session
	user     *model.User
	owner    bool
	readOnly bool
	ws       *websocket.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	started  time.Time
	clientIP string
	consent  encryptionPolicy   // the viewer's confirmation (?allow=weak|unencrypted)
	clip     clipboardDirection // effective clipboard policy of the current connection

	bytesUp   atomic.Int64 // browser → server
	bytesDown atomic.Int64 // server → browser

	mu         sync.Mutex
	upstream   net.Conn
	closeCode  websocket.StatusCode
	closeMsg   string
	closedOnce bool
	sessClosed bool
	audited    bool
	target     string
	security   string
}

func (m *Module) handleWS(c *echo.Context) error {
	user := httpx.UserFrom(c)
	id := c.Param("id")
	ws, err := httpx.AcceptWS(c, &websocket.AcceptOptions{Subprotocols: []string{"binary"}})
	if err != nil {
		m.log.Debug("vnc: websocket accept failed", "err", err)
		return nil // the handshake error response has been written
	}
	defer ws.CloseNow()
	ctx := c.Request().Context()

	var s *term.Session
	if model.ValidID(id) {
		s = m.c.Sessions.Get(id)
	}
	if s == nil || user == nil || (s.OwnerID != user.ID && !user.IsAdmin()) {
		closeWS(ws, CloseNotFound, "session not found")
		return nil
	}
	if s.Kind != model.KindVNC {
		closeWS(ws, CloseBadRequest, "not a VNC session")
		return nil
	}
	owner := s.OwnerID == user.ID
	release, err := m.c.Sessions.TrackClient(s.ID)
	if err != nil {
		closeWS(ws, CloseSessionClosed, "the session was closed")
		return nil
	}
	defer release()
	if !owner {
		m.audit(ctx, user, "session.shadow", s.ID, map[string]any{"ownerId": s.OwnerID, "protocol": model.ProtoVNC})
	}
	vctx, cancel := context.WithCancel(ctx)
	v := &viewer{m: m, s: s, user: user, owner: owner, readOnly: !owner, ws: ws, ctx: vctx, cancel: cancel,
		started: time.Now(), clientIP: httpx.ClientIP(c), consent: parseConsent(c.QueryParam("allow")),
		clip: clipboardDirection{toRemote: true, fromRemote: true}}
	v.run()
	return nil
}

// closeWS closes a socket with a code and a reason truncated to the protocol limit.
func closeWS(ws *websocket.Conn, code websocket.StatusCode, reason string) {
	_ = ws.Close(code, truncateReason(reason))
}

func truncateReason(s string) string {
	const max = 120
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// setClose records how the socket will be closed (first caller wins).
func (v *viewer) setClose(code websocket.StatusCode, msg string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closeCode == 0 {
		v.closeCode, v.closeMsg = code, msg
	}
}

// sessionClosed is called when the runtime session is closed: audit now (the manager may be shutting down) and end.
func (v *viewer) sessionClosed() {
	v.mu.Lock()
	v.sessClosed = true
	up := v.upstream
	v.mu.Unlock()
	v.setClose(CloseSessionClosed, "the session was closed")
	v.auditEnd("session closed")
	if up != nil {
		_ = up.Close()
	}
	go closeWS(v.ws, CloseSessionClosed, "the session was closed")
	v.cancel()
}

func (v *viewer) run() {
	defer v.cancel()
	if !v.m.attach(v) {
		closeWS(v.ws, CloseShutdown, "Termstead is shutting down")
		return
	}
	if v.s.Closed() {
		v.m.detach(v, false, model.StateDisconnected, "")
		closeWS(v.ws, CloseSessionClosed, "the session was closed")
		return
	}
	wasConnected := false
	endState, endMsg := model.StateDisconnected, "Viewer disconnected"
	defer func() {
		v.m.detach(v, wasConnected, endState, endMsg)
	}()

	nc := websocket.NetConn(v.ctx, v.ws, websocket.MessageBinary)
	pr, pw := io.Pipe()
	browserDone := make(chan struct{})
	go func() {
		_, err := io.Copy(pw, nc)
		if err == nil {
			err = io.EOF
		}
		_ = pw.CloseWithError(err)
		close(browserDone)
		v.cancel()
	}()
	go v.keepalive()

	res, conn, err := v.connect()
	if err != nil {
		code, msg, st := v.classify(err)
		endState, endMsg = st, msg
		var ie *insecureError
		if errors.As(err, &ie) {
			v.m.setConfirm(v.s.ID, ie.info())
		}
		v.setClose(code, msg)
		v.finishSocket()
		return
	}
	defer res.conn.Close()
	v.mu.Lock()
	v.upstream = res.conn
	v.security = res.securityName()
	v.mu.Unlock()
	if v.ctx.Err() != nil {
		return
	}

	if res.pass != nil && (v.readOnly || !v.clip.toRemote) {
		// Termstead cannot parse (and so cannot filter) the viewer's messages while the browser authenticates.
		endState, endMsg = model.StateError, "read-only viewers cannot use browser-side authentication"
		if !v.readOnly {
			endMsg = "the clipboard policy cannot be enforced with browser-side authentication (" + res.pass.offered + ")"
		}
		v.setClose(CloseForbidden, endMsg)
		v.finishSocket()
		return
	}
	stop := time.AfterFunc(presentTimeout, v.cancel)
	if res.pass != nil {
		err = presentPassthrough(pr, nc, res.pass)
	} else {
		err = presentNone(pr, nc, res.init.raw)
	}
	stopped := stop.Stop()
	if err != nil || !stopped {
		if v.ctx.Err() == nil || !stopped {
			v.m.log.Debug("vnc: browser handshake failed", "session", v.s.ID, "err", err)
			v.setClose(CloseBadRequest, "the viewer did not complete the RFB handshake")
		}
		v.finishSocket()
		return
	}

	info := v.info(conn, res)
	wasConnected = true
	v.m.connected(v, info)
	details := map[string]any{
		"host": conn.Host, "port": portOf(conn), "security": info.Security, "encrypted": info.Encrypted,
		"route": info.Route, "readOnly": v.readOnly, "reverse": info.Reverse, "encryptionPolicy": info.EncryptionPolicy,
	}
	if info.Downgrade != "" {
		details["downgrade"] = info.Downgrade
	}
	if res.tls != nil && res.tls.Weak {
		details["weakTls"] = res.tls.Group
	}
	if info.Clipboard != "both" {
		details["clipboard"] = info.Clipboard
	}
	v.m.audit(v.ctx, v.user, "vnc.connect", v.s.ID, details)

	who, rerr := v.relay(pr, nc, res.conn)
	switch {
	case v.sessClosedNow():
		// ended by sessionClosed
	case who == "server" && (rerr == nil || errors.Is(rerr, io.EOF)):
		endState, endMsg = model.StateDisconnected, "The VNC server closed the connection"
	case who == "server" && !isClosedErr(rerr):
		endState, endMsg = model.StateError, "Connection to the VNC server lost: "+friendlyNetError(rerr).Error()
	case who == "browser" && rerr != nil && !errors.Is(rerr, io.EOF) && !isClosedErr(rerr):
		// e.g. an unexpected client message of a read-only viewer
		v.m.log.Debug("vnc: relay ended", "session", v.s.ID, "err", rerr)
	}
	v.auditEnd(endMsg)
	v.finishSocket()
	<-browserDone
}

// finishSocket closes the WebSocket with the recorded code (graceful close handshake, bounded by the library).
func (v *viewer) finishSocket() {
	v.mu.Lock()
	if v.closedOnce {
		v.mu.Unlock()
		return
	}
	v.closedOnce = true
	code, msg := v.closeCode, v.closeMsg
	v.mu.Unlock()
	if code == 0 {
		code, msg = websocket.StatusNormalClosure, ""
	}
	closeWS(v.ws, code, msg)
	v.cancel()
}

func (v *viewer) auditEnd(reason string) {
	v.mu.Lock()
	if v.audited || v.security == "" {
		v.mu.Unlock()
		return
	}
	v.audited = true
	v.mu.Unlock()
	v.m.audit(context.Background(), v.user, "vnc.disconnect", v.s.ID, map[string]any{
		"durationMs": time.Since(v.started).Milliseconds(), "bytesIn": v.bytesDown.Load(),
		"bytesOut": v.bytesUp.Load(), "reason": reason,
	})
}

func (v *viewer) keepalive() {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-v.ctx.Done():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(v.ctx, pingTimeout)
			err := v.ws.Ping(ctx)
			cancel()
			if err != nil && v.ctx.Err() == nil {
				v.m.log.Debug("vnc: viewer ping failed", "session", v.s.ID, "err", err)
				v.cancel()
				return
			}
		}
	}
}

func (v *viewer) info(conn *model.Connection, res *handshakeResult) *Info {
	info := &Info{
		SessionID:     v.s.ID,
		Connected:     true,
		Host:          conn.Host,
		Port:          portOf(conn),
		ServerVersion: res.serverVersion,
		Protocol:      res.version.String(),
		Security:      res.securityName(),
		Encrypted:     res.encrypted(),
		TLS:           res.tls,
		Passthrough:   res.pass != nil,
		Route:         describeRoute(conn),
		Reverse:       conn.Options.Bool("reverse"),

		EncryptionPolicy:  res.policy.String(),
		Downgrade:         res.downgrade,
		PasswordCleartext: res.subType == vcPlain,
		Clipboard:         v.clip.String(),
	}
	if res.init != nil {
		info.DesktopName, info.Width, info.Height = res.init.name, res.init.width, res.init.height
	}
	return info
}

// classify maps a connect error to the socket close code, the user message and the session state.
func (v *viewer) classify(err error) (websocket.StatusCode, string, model.SessionState) {
	var ae *authError
	var re *refusedError
	var ue *unsupportedError
	var ie *insecureError
	var er *encryptionRequiredError
	switch {
	case v.sessClosedNow():
		return CloseSessionClosed, "the session was closed", model.StateDisconnected
	case errors.Is(err, context.Canceled) && v.ctx.Err() != nil:
		return websocket.StatusNormalClosure, "Viewer disconnected", model.StateDisconnected
	case errors.Is(err, httpx.ErrLocked) || errors.Is(err, model.ErrLocked):
		return CloseLocked, "The vault is locked: unlock it to use the stored VNC password", model.StateError
	case errors.Is(err, errCanceled):
		return CloseCanceled, "Authentication canceled", model.StateError
	case errors.As(err, &ae):
		return CloseAuthFailed, capitalize(ae.Error()), model.StateError
	case errors.As(err, &ue):
		return CloseUnsupported, capitalize(ue.Error()), model.StateError
	case errors.As(err, &ie):
		return CloseInsecure, capitalize(ie.Error()), model.StateError
	case errors.As(err, &er):
		return CloseUnsupported, capitalize(er.Error()), model.StateError
	case errors.As(err, &re):
		return CloseConnectFailed, capitalize(re.Error()), model.StateError
	case errors.Is(err, errReverseUnavailable):
		return CloseUnavailable, capitalize(err.Error()), model.StateDisconnected
	case errors.Is(err, errCertRejected):
		return CloseCanceled, capitalize(err.Error()), model.StateError
	case isTimeout(err):
		return CloseTimeout, "Timed out: " + err.Error(), model.StateError
	case errors.Is(err, httpx.ErrNotFound) || errors.Is(err, model.ErrNotFound):
		return CloseAuthFailed, "The connection (or its SSH gateway) no longer exists", model.StateError
	case term.IsPermanent(err):
		return CloseAuthFailed, capitalize(err.Error()), model.StateError
	}
	var he *httpx.HTTPError
	if errors.As(err, &he) && he.Status < 500 {
		return CloseForbidden, capitalize(he.Message), model.StateError
	}
	return CloseConnectFailed, capitalize(friendlyNetError(err).Error()), model.StateError
}

func (v *viewer) sessClosedNow() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.sessClosed
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	r, n := utf8.DecodeRuneInString(s)
	return strings.ToUpper(string(r)) + s[n:]
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

func isClosedErr(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, context.Canceled) ||
		websocket.CloseStatus(err) != -1
}

func friendlyNetError(err error) error {
	switch {
	case err == nil:
		return errors.New("connection closed")
	case errors.Is(err, syscall.ECONNREFUSED):
		return errors.New("connection refused (is the VNC server running on that port?)")
	case errors.Is(err, syscall.ECONNRESET):
		return errors.New("connection reset by the server")
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return errors.New("the server closed the connection")
	case isTimeout(err):
		return errors.New("connection timed out")
	}
	return err
}

// ---- upstream connection ------------------------------------------------------------------------------------------

// connect dials the VNC server (or takes the parked reverse connection) and completes the RFB handshake, prompting
// for credentials and retrying after rejected passwords, lost connections during prompts, and security choices the
// server cannot complete.
func (v *viewer) connect() (*handshakeResult, *model.Connection, error) {
	conn, secrets, err := v.s.Resolve(v.ctx)
	if err != nil {
		return nil, nil, err
	}
	reverse := conn.Options.Bool("reverse")
	creds := newViewerCreds(v, conn, secrets)
	exclude := map[uint32]bool{}
	target := hostPort(conn.Host, portOf(conn))
	connPolicy := connectionPolicy(conn.Options)
	policy := effectivePolicy(connPolicy, v.m.rememberedConsent(v.s.ID), v.consent)
	clip := v.m.clipboardPolicy(v.ctx, conn)
	v.mu.Lock()
	v.target = target
	v.clip = clip
	v.mu.Unlock()
	downgrade := ""

	var lastErr error
	for attempt := 1; attempt <= maxConnectTries; attempt++ {
		if err := v.ctx.Err(); err != nil {
			return nil, nil, err
		}
		var raw net.Conn
		if reverse {
			if attempt > 1 {
				break // an incoming connection cannot be re-established from here
			}
			v.m.report(v, model.StateConnecting, "Incoming connection from "+conn.Host)
			raw, err = v.m.takeReverse(v.s.ID)
		} else {
			v.m.report(v, model.StateConnecting, "Connecting to "+target+routeSuffix(conn))
			raw, err = v.m.c.SSH.DialConnection(term.WithSession(v.ctx, v.s), v.s.Owner(), conn, secrets)
		}
		if err != nil {
			return nil, nil, err
		}
		stop := context.AfterFunc(v.ctx, func() { _ = raw.Close() })
		prompts := creds.prompts
		res, err := clientHandshake(v.ctx, raw, &handshakeConfig{
			host:             conn.Host,
			port:             portOf(conn),
			repeaterID:       strings.TrimSpace(conn.Options.String("repeaterId", "")),
			shared:           v.readOnly || conn.Options.Bool("shared", true),
			allowPassthrough: !v.readOnly,
			policy:           policy,
			exclude:          exclude,
			creds:            creds,
			trust:            &viewerTrust{v: v},
			timeout:          handshakeTimeout,
			status:           func(msg string) { v.m.report(v, model.StateConnecting, msg) },
		})
		if !stop() {
			// The viewer went away while connecting.
			_ = raw.Close()
			if err == nil {
				_ = res.conn.Close()
			}
			return nil, nil, v.ctx.Err()
		}
		if err == nil {
			creds.succeeded(v.ctx, res)
			res.policy, res.downgrade = policy, downgrade
			if res.encrypted() && !res.tls.Weak {
				res.downgrade = ""
			}
			relaxed := res.downgrade != "" || (res.tls != nil && res.tls.Weak) || res.subType == vcPlain
			if relaxed && v.owner && policy > connPolicy {
				v.m.rememberConsent(v.s.ID, policy) // the owner confirmed: later viewers of this session need not ask again
			}
			return res, conn, nil
		}
		_ = raw.Close()
		lastErr = err
		var ae *authError
		var re *retryError
		var ie *insecureError
		switch {
		case reverse && errors.As(err, &ie):
			// The incoming stream is consumed: a confirmed retry would have nothing to connect to.
			return nil, nil, fmt.Errorf("%s; an incoming connection cannot be retried with less security (have the server "+
				"connect again with the security it can negotiate)", ie.reason)
		case errors.As(err, &ae):
			if reverse || !creds.reject(ae.reason) {
				return nil, nil, err
			}
			v.m.log.Info("vnc: authentication rejected", "session", v.s.ID, "target", target, "reason", ae.reason)
		case errors.As(err, &re):
			exclude[re.exclude] = true
			if re.downgrade && downgrade == "" {
				downgrade = capitalize(re.cause.Error())
			}
			v.m.log.Info("vnc: retrying without a security choice", "session", v.s.ID, "exclude", re.exclude,
				"cause", re.cause, "policy", policy.String())
		case creds.prompts > prompts && !reverse && isConnLoss(err):
			// The server dropped the connection while the user was answering: reconnect with the answer.
			v.m.log.Debug("vnc: connection lost during a prompt, reconnecting", "session", v.s.ID, "err", err)
		default:
			return nil, nil, err
		}
	}
	return nil, nil, lastErr
}

func isConnLoss(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) || isTimeout(err)
}

func routeSuffix(c *model.Connection) string {
	switch r := describeRoute(c); r {
	case "direct", "incoming connection":
		return ""
	default:
		return " via " + r
	}
}

// prompt asks the viewing user through the prompt broker, showing "authenticating" meanwhile.
func (v *viewer) prompt(ctx context.Context, p model.Prompt, waiting string) (model.PromptResponse, error) {
	if v.m.d.Events == nil {
		return model.PromptResponse{}, errors.New("interactive prompts are not available")
	}
	p.SessionID = v.s.ID
	if conn := v.s.Connection(); conn != nil && conn.ID != "" {
		p.ConnectionID = conn.ID
	}
	v.m.report(v, model.StateAuthenticating, waiting)
	resp, err := v.m.d.Events.Prompt(ctx, v.user.ID, p)
	v.m.report(v, model.StateConnecting, "Authenticating")
	switch {
	case err == nil:
		return resp, nil
	case errors.Is(err, events.ErrNoInteractiveClient):
		return resp, fmt.Errorf("%s: input is required but no Termstead window is connected", strings.ToLower(p.Title))
	case errors.Is(err, events.ErrPromptTimeout):
		return resp, fmt.Errorf("no answer to the %q prompt", p.Title)
	}
	return resp, err
}
