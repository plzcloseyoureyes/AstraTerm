package rdp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/netguard"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// Timeouts of the RDCleanPath relay.
const (
	requestTimeout   = 15 * time.Second // the client's first message
	x224Timeout      = 20 * time.Second // X.224 request → confirm
	handshakeTimeout = 30 * time.Second // TLS handshake (plus the time a certificate prompt is open)
	wsPingInterval   = 30 * time.Second
	pumpBuffer       = 32 << 10
)

// handleRelay serves /ws/rdp/{id}: the RDCleanPath proxy of the IronRDP web client (SPEC §6.3).
func (h *handler) handleRelay(c *echo.Context) error {
	s, u, err := h.session(c, false)
	if err != nil {
		return err
	}
	ws, err := httpx.AcceptWS(c, nil)
	if err != nil {
		h.log.Debug("rdp: websocket accept failed", "err", err)
		return nil
	}
	defer ws.CloseNow()

	ctx, cancel := context.WithCancel(c.Request().Context())
	defer cancel()
	release, err := h.c.Sessions.TrackClient(s.ID)
	if err != nil {
		_ = ws.Close(websocket.StatusPolicyViolation, "session closed")
		return nil
	}
	v := &viewer{sessionID: s.ID, engine: engineIronRDP, cancel: cancel}
	h.viewers.add(v)
	r := &relay{h: h, ws: ws, s: s, user: u, ctx: ctx, viewer: v}
	endMsg := r.run()
	h.viewers.remove(v)
	release()
	if endMsg != "" {
		h.endState(s, model.StateDisconnected, endMsg)
	}
	return nil
}

type relay struct {
	h      *handler
	ws     *websocket.Conn
	s      *term.Session
	user   *model.User
	ctx    context.Context
	viewer *viewer
}

// serverConn is an established, TLS-secured connection to the RDP server.
type serverConn struct {
	tls        *tls.Conn
	confirm    []byte // X.224 Connection Confirm as received
	chain      [][]byte
	serverAddr string
	selected   uint32
}

// relayError is a failure reported to the client (RDCleanPath error) and in the session state.
type relayError struct {
	fail cleanPathFailure
	msg  string
}

func (e *relayError) Error() string { return e.msg }

// run performs the RDCleanPath exchange and pumps data. It returns the "disconnected" message for the session, or
// "" when the session state was already set (errors).
func (r *relay) run() string {
	h, s := r.h, r.s
	raw, err := r.readRequest()
	if err != nil {
		h.log.Debug("rdp: invalid RDCleanPath request", "session", s.ID, "err", err)
		r.fail(&relayError{fail: cleanPathFailure{HTTPStatus: 400}, msg: "invalid RDCleanPath request: " + err.Error()})
		return ""
	}
	req, err := decodeCleanPath(raw)
	if err != nil {
		r.fail(&relayError{fail: cleanPathFailure{HTTPStatus: 400}, msg: err.Error()})
		return ""
	}
	if req.Version != cleanPathVersion {
		r.fail(&relayError{fail: cleanPathFailure{HTTPStatus: 400},
			msg: fmt.Sprintf("unsupported RDCleanPath version %d (expected %d)", req.Version, cleanPathVersion)})
		return ""
	}
	t, ok := h.tickets.consume(req.ProxyAuth, s.ID, r.user.ID, engineIronRDP)
	if !ok {
		r.fail(&relayError{fail: cleanPathFailure{HTTPStatus: 403},
			msg: "the connection ticket is invalid or has expired — reconnect to get a new one"})
		return ""
	}
	if len(req.X224ConnectionPDU) == 0 {
		r.fail(&relayError{fail: cleanPathFailure{HTTPStatus: 400}, msg: "the request carries no X.224 connection request"})
		return ""
	}
	xreq, err := parseConnectionRequest(req.X224ConnectionPDU)
	if err != nil {
		r.fail(&relayError{fail: cleanPathFailure{HTTPStatus: 400}, msg: err.Error()})
		return ""
	}
	dest := net.JoinHostPort(t.host, strconv.Itoa(t.port))
	if req.Destination != "" && !strings.EqualFold(req.Destination, dest) {
		// Never trusted (the ticket's destination is dialed); only worth a debug line.
		h.log.Debug("rdp: client destination differs from the ticket", "session", s.ID, "client", req.Destination, "ticket", dest)
	}

	h.setState(s.ID, model.StateConnecting, "Connecting to "+dest+viaText(t.conn))
	sc, rerr := r.connect(t, req.X224ConnectionPDU)
	if rerr != nil {
		r.fail(rerr)
		return ""
	}
	defer sc.tls.Close()
	resp, err := cleanPathResponse(sc.confirm, sc.chain, sc.serverAddr)
	if err != nil {
		r.fail(&relayError{fail: cleanPathFailure{HTTPStatus: 500}, msg: "cannot encode the RDCleanPath response"})
		return ""
	}
	wctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
	err = r.ws.Write(wctx, websocket.MessageBinary, resp)
	cancel()
	if err != nil {
		return "Viewer disconnected"
	}
	h.setState(s.ID, model.StateAuthenticating, "Signing in ("+protocolName(sc.selected)+")")
	h.auditUser(r.ctx, r.user, "rdp.connect", s.ID, map[string]any{"engine": engineIronRDP, "host": t.host,
		"port": t.port, "security": protocolName(sc.selected), "requested": xreq.requested,
		"connectionId": t.conn.ID, "via": routeDescription(t.conn)})
	r.viewer.active.Store(true) // relaying from now on: this viewer keeps the session alive
	return r.pump(sc.tls, sc.selected, t.autologon, channelPolicy(parseOptions(t.conn.Options)))
}

// readRequest reads the DER RDCleanPath request (normally one binary message).
func (r *relay) readRequest() ([]byte, error) {
	ctx, cancel := context.WithTimeout(r.ctx, requestTimeout)
	defer cancel()
	var buf []byte
	for {
		typ, data, err := r.ws.Read(ctx)
		if err != nil {
			return nil, err
		}
		if typ != websocket.MessageBinary {
			return nil, errors.New("expected a binary message")
		}
		buf = append(buf, data...)
		n, err := derLength(buf)
		if err != nil {
			return nil, err
		}
		if n > 0 && len(buf) >= n {
			if len(buf) > n {
				return nil, errors.New("unexpected data after the RDCleanPath request")
			}
			return buf, nil
		}
		if len(buf) > maxCleanPathPDU {
			return nil, errors.New("RDCleanPath request too large")
		}
	}
}

// fail reports err to the session state and the client, then closes the socket.
func (r *relay) fail(e *relayError) {
	r.h.failState(r.s, r.viewer, e.msg)
	if b, err := e.fail.encode(); err == nil {
		ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
		_ = r.ws.Write(ctx, websocket.MessageBinary, b)
		cancel()
	}
	_ = r.ws.Close(websocket.StatusNormalClosure, "")
}

// connect dials the server and secures the connection. When the TLS handshake fails after the user took a while to
// accept an unknown certificate (servers drop half-finished handshakes), it reconnects once without asking again.
func (r *relay) connect(t *ticket, x224req []byte) (*serverConn, *relayError) {
	trust := r.h.newCertTrust(r.ctx, r.s, r.user, t)
	sc, err := r.h.secureConnect(r.ctx, r.s, r.user, t, x224req, trust)
	if err != nil {
		return nil, r.classify(err, t)
	}
	return sc, nil
}

// newCertTrust prepares the certificate verification of a connection to the ticket's destination.
func (h *handler) newCertTrust(ctx context.Context, s *term.Session, u *model.User, t *ticket) *certTrust {
	return &certTrust{
		h:          h,
		ctx:        ctx,
		user:       u,
		sessionID:  s.ID,
		connID:     t.conn.ID,
		host:       strings.ToLower(t.host),
		port:       t.port,
		serverName: t.host,
		ignore:     parseOptions(t.conn.Options).IgnoreCert,
	}
}

// secureConnect runs dialTLS, retrying once when the TLS handshake failed after an interactive certificate prompt.
func (h *handler) secureConnect(ctx context.Context, s *term.Session, u *model.User, t *ticket, x224req []byte, trust *certTrust) (*serverConn, error) {
	for attempt := 0; ; attempt++ {
		sc, err := h.dialTLS(ctx, s, u, t, x224req, trust)
		if err == nil {
			return sc, nil
		}
		if attempt == 0 && trust.promptedAccept() && !errors.Is(err, errCertRejected) && ctx.Err() == nil {
			h.log.Debug("rdp: TLS handshake failed after the certificate prompt; reconnecting", "session", s.ID, "err", err)
			continue
		}
		return nil, err
	}
}

// negotiationError is an RDP_NEG_FAILURE answer.
type negotiationError struct {
	confirm []byte
	code    uint32
}

func (e *negotiationError) Error() string { return negotiationFailureText(e.code) }

// errStandardSecurityOnly means the server selected standard RDP security (no TLS).
var errStandardSecurityOnly = errors.New("the server only offers standard RDP security (no TLS), which the built-in engine does not support; use the guacd engine")

type stageError struct {
	stage string
	err   error
}

func (e *stageError) Error() string { return e.stage + ": " + e.err.Error() }
func (e *stageError) Unwrap() error { return e.err }

// dialTLS connects to the ticket's destination along the connection's route, sends the pre-connection PDU (Hyper-V)
// and the client's X.224 Connection Request, reads the Connection Confirm and completes the TLS handshake.
func (h *handler) dialTLS(ctx context.Context, s *term.Session, u *model.User, t *ticket, x224req []byte, trust *certTrust) (*serverConn, error) {
	opts := parseOptions(t.conn.Options)
	dctx := term.WithSession(ctx, s) // prompts of SSH gateways belong to this session
	raw, err := h.dialConnection(dctx, u, t.conn, t.secrets)
	if err != nil {
		return nil, &stageError{stage: "connect", err: err}
	}
	ok := false
	defer func() {
		if !ok {
			raw.Close()
		}
	}()
	_ = raw.SetDeadline(time.Now().Add(x224Timeout))
	stop := context.AfterFunc(ctx, func() { _ = raw.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	if pcb := t.conn.Options.String("preconnectionBlob", ""); pcb != "" {
		if _, err := raw.Write(preconnectionPDU(uint32(opts.PreconnectionID), pcb)); err != nil {
			return nil, &stageError{stage: "send pre-connection PDU", err: err}
		}
	}
	if _, err := raw.Write(x224req); err != nil {
		return nil, &stageError{stage: "send connection request", err: err}
	}
	confirm, err := readTPKT(raw)
	if err != nil {
		return nil, &stageError{stage: "read connection confirm", err: err}
	}
	cc, err := parseConnectionConfirm(confirm)
	if err != nil {
		return nil, &stageError{stage: "connection confirm", err: err}
	}
	if cc.failed {
		return nil, &negotiationError{confirm: confirm, code: cc.failure}
	}
	if !cc.negotiation || cc.selected == protoRDP {
		return nil, &stageError{stage: "security negotiation", err: errStandardSecurityOnly}
	}

	cfg := &tls.Config{
		InsecureSkipVerify: true, // verified by trust.verify (system roots, pinned certificate or the user)
		VerifyConnection:   trust.verify,
		MinVersion:         tls.VersionTLS12,
	}
	if net.ParseIP(t.host) == nil {
		cfg.ServerName = t.host
	}
	if needsTLS12(cc.selected) {
		cfg.MaxVersion = tls.VersionTLS12
	}
	if opts.LegacyTLS {
		cfg.MinVersion = tls.VersionTLS10
		cfg.CipherSuites = legacyCipherSuites()
	}
	_ = raw.SetDeadline(time.Time{})
	tc := tls.Client(raw, cfg)
	// The handshake may wait for the user to confirm the certificate (the prompt has its own 3-minute timeout).
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout+3*time.Minute)
	defer cancel()
	if err := tc.HandshakeContext(hctx); err != nil {
		return nil, &stageError{stage: "TLS handshake", err: err}
	}
	state := tc.ConnectionState()
	chain := make([][]byte, 0, len(state.PeerCertificates))
	for _, c := range state.PeerCertificates {
		chain = append(chain, c.Raw)
	}
	ok = true
	return &serverConn{tls: tc, confirm: confirm, chain: chain, serverAddr: serverAddress(raw, t), selected: cc.selected}, nil
}

// dialConnection opens a TCP stream to conn's host:port along its route (options.sshTunnelVia / jumpHosts / proxy);
// closing the stream releases the gateways.
func (h *handler) dialConnection(ctx context.Context, user *model.User, conn *model.Connection, secrets map[string]string) (net.Conn, error) {
	if h.c != nil && h.c.SSH != nil {
		return h.c.SSH.DialConnection(ctx, user, conn, secrets) // guarded by the route (SEC-7)
	}
	// No pool (tests / minimal setups): still vet the concrete address for restricted users.
	d := h.guard(user).Dialer(20 * time.Second)
	return d.DialContext(ctx, "tcp", conn.Address())
}

// legacyCipherSuites re-enables RSA key exchange for old servers (Windows Server 2008 / 2008 R2 without updates);
// Go removed these suites from its defaults, but still negotiates them when listed explicitly.
func legacyCipherSuites() []uint16 {
	var out []uint16
	for _, s := range tls.CipherSuites() {
		out = append(out, s.ID)
	}
	for _, s := range tls.InsecureCipherSuites() {
		if strings.HasPrefix(s.Name, "TLS_RSA_WITH_AES") || s.Name == "TLS_RSA_WITH_3DES_EDE_CBC_SHA" {
			out = append(out, s.ID)
		}
	}
	return out
}

// serverAddress is the IP:port the relay reached (RDCleanPath server_addr). Through gateways the IP may be unknown:
// resolve the host locally, else report the unspecified address with the right port.
func serverAddress(c net.Conn, t *ticket) string {
	if a, ok := c.RemoteAddr().(*net.TCPAddr); ok && a != nil && a.IP != nil && a.Port == t.port {
		return a.String()
	}
	if ip := net.ParseIP(t.host); ip != nil {
		return net.JoinHostPort(ip.String(), strconv.Itoa(t.port))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if ips, err := net.DefaultResolver.LookupIP(ctx, "ip", t.host); err == nil && len(ips) > 0 {
		return net.JoinHostPort(ips[0].String(), strconv.Itoa(t.port))
	}
	return net.JoinHostPort("0.0.0.0", strconv.Itoa(t.port))
}

// classify maps a connection failure to the RDCleanPath error and a readable message.
func (r *relay) classify(err error, t *ticket) *relayError {
	dest := net.JoinHostPort(t.host, strconv.Itoa(t.port))
	if be, ok := netguard.IsBlocked(err); ok {
		// Refused by the destination policy (SEC-7): 403, never retried by the client.
		return &relayError{fail: cleanPathFailure{HTTPStatus: 403}, msg: capitalize(be.Error())}
	}
	if neg, ok := errors.AsType[*negotiationError](err); ok {
		return &relayError{fail: cleanPathFailure{Code: cleanPathNegotiationError, X224: neg.confirm, HTTPStatus: 502},
			msg: "Security negotiation with " + dest + " failed: " + neg.Error()}
	}
	if rej, ok := errors.AsType[*certRejectedError](err); ok {
		return &relayError{fail: cleanPathFailure{TLSAlert: 42}, msg: capitalize(rej.msg)}
	}
	if errors.Is(err, context.Canceled) {
		return &relayError{fail: cleanPathFailure{HTTPStatus: 499}, msg: "Connection canceled"}
	}
	stage := ""
	if st, ok := errors.AsType[*stageError](err); ok {
		stage = st.stage
	}
	if stage == "TLS handshake" {
		if code, ok := tlsAlertCode(err); ok {
			return &relayError{fail: cleanPathFailure{TLSAlert: code}, msg: "TLS handshake with " + dest + " failed: " + rootMessage(err)}
		}
		return &relayError{fail: cleanPathFailure{HTTPStatus: 502}, msg: "TLS handshake with " + dest + " failed: " + rootMessage(err)}
	}
	if stage == "connect" {
		wsa, text := socketError(err)
		msg := "Cannot connect to " + dest + viaText(t.conn) + ": " + text
		if wsa != 0 {
			return &relayError{fail: cleanPathFailure{WSAError: wsa}, msg: msg}
		}
		return &relayError{fail: cleanPathFailure{HTTPStatus: 502}, msg: msg}
	}
	if stage == "read connection confirm" && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
		return &relayError{fail: cleanPathFailure{HTTPStatus: 502},
			msg: dest + " closed the connection during the RDP handshake (is it an RDP server? is the security mode supported?)"}
	}
	if wsa, text := socketError(err); wsa != 0 {
		return &relayError{fail: cleanPathFailure{WSAError: wsa}, msg: "Connection to " + dest + " failed: " + text}
	}
	return &relayError{fail: cleanPathFailure{HTTPStatus: 502}, msg: "Connection to " + dest + " failed: " + rootMessage(err)}
}

func viaText(c *model.Connection) string {
	switch routeDescription(c) {
	case "ssh", "jump":
		return " through the SSH gateway"
	case "proxy", "proxy-command":
		return " through the proxy"
	}
	return ""
}

// WSA error codes reported to IronRDP (Windows socket error numbers, as its proxy protocol expects).
const (
	wsaNetUnreachable  = 10051
	wsaConnReset       = 10054
	wsaTimedOut        = 10060
	wsaConnRefused     = 10061
	wsaHostUnreachable = 10065
	wsaHostNotFound    = 11001
)

// socketError maps a dial error to a WSA code and a short text.
func socketError(err error) (int, string) {
	var dnsErr *net.DNSError
	var errno syscall.Errno
	isErrno := errors.As(err, &errno)
	msg := strings.ToLower(err.Error())
	switch {
	case errors.As(err, &dnsErr):
		return wsaHostNotFound, "host not found"
	case errors.Is(err, syscall.ECONNREFUSED) || isErrno && uintptr(errno) == wsaConnRefused ||
		strings.Contains(msg, "connection refused"):
		return wsaConnRefused, "connection refused"
	case errors.Is(err, syscall.EHOSTUNREACH) || isErrno && uintptr(errno) == wsaHostUnreachable ||
		strings.Contains(msg, "no route to host"):
		return wsaHostUnreachable, "host unreachable"
	case errors.Is(err, syscall.ENETUNREACH) || isErrno && uintptr(errno) == wsaNetUnreachable ||
		strings.Contains(msg, "network is unreachable"):
		return wsaNetUnreachable, "network unreachable"
	case errors.Is(err, syscall.ECONNRESET) || isErrno && uintptr(errno) == wsaConnReset ||
		strings.Contains(msg, "connection reset"):
		return wsaConnReset, "connection reset"
	case isTimeout(err) || strings.Contains(msg, "timed out") || strings.Contains(msg, "timeout"):
		return wsaTimedOut, "connection timed out"
	}
	return 0, rootMessage(err)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()
}

// tlsAlertCode extracts the alert of a TLS handshake failure ("remote error: tls: …" or an alert we sent).
func tlsAlertCode(err error) (int, bool) {
	if ae, ok := errors.AsType[tls.AlertError](err); ok {
		return int(ae), true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "remote error" && op.Err != nil {
		v := reflect.ValueOf(op.Err)
		if v.Kind() == reflect.Uint8 {
			return int(v.Uint()), true
		}
	}
	return 0, false
}

// rootMessage returns the innermost meaningful error text.
func rootMessage(err error) string {
	if st, ok := errors.AsType[*stageError](err); ok {
		err = st.err
	}
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "remote error: ")
	return msg
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// pump relays the WebSocket binary stream to and from the TLS plaintext until either side ends. Both directions pass
// through filters that work around IronRDP 0.7 limitations (serverfilter.go, clientfilter.go). It returns the message
// describing how the connection ended.
func (r *relay) pump(tc *tls.Conn, selected uint32, autologon bool, blockedChannels []string) string {
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	nc := websocket.NetConn(ctx, r.ws, websocket.MessageBinary)
	defer nc.Close()

	go func() {
		t := time.NewTicker(wsPingInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, 20*time.Second)
				err := r.ws.Ping(pctx)
				pcancel()
				if err != nil && ctx.Err() == nil {
					cancel()
					return
				}
			}
		}
	}()

	var (
		once   sync.Once
		reason string
	)
	finish := func(msg string) {
		once.Do(func() {
			reason = msg
			_ = tc.Close() // unblocks the server → browser copy
			_ = nc.Close() // graceful close handshake; unblocks the browser → server copy
		})
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		cf := newClientFilter(tc, selected, autologon, blockedChannels)
		_, err := io.Copy(cf, nc) // browser → server
		if len(cf.disabled) > 0 {
			r.h.log.Debug("rdp: disabled static channels by policy", "session", r.s.ID, "channels", cf.disabled)
		}
		if err == nil || errors.Is(err, io.EOF) || websocket.CloseStatus(err) != -1 {
			finish("Viewer disconnected")
			return
		}
		finish("Viewer connection lost")
	}()
	buf := make([]byte, pumpBuffer)
	filter := newServerFilter(nc, selected)
	_, err := io.CopyBuffer(filter, tc, buf) // server → browser
	if filter.fixed > 0 || filter.reactivations > 0 {
		r.h.log.Debug("rdp: server output adjusted for the web client", "session", r.s.ID,
			"rewrittenRects", filter.fixed, "reactivations", filter.reactivations)
	}
	switch {
	case err == nil || errors.Is(err, io.EOF):
		finish("The remote desktop closed the connection")
	case r.ctx.Err() != nil:
		finish("Disconnected")
	default:
		finish("Connection to the remote desktop lost: " + rootMessage(err))
	}
	<-done
	if r.s.Closed() {
		return ""
	}
	return reason
}
