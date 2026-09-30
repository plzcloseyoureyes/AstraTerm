// Package telnet implements the "telnet" terminal protocol (PROTO-6, RESEARCH §3.4): a streaming RFC 854/855 IAC
// state machine with BINARY/ECHO/SGA/TTYPE/NAWS/NEW-ENVIRON negotiation (RFC 1143-style state tracking, so requests
// are never answered twice and loops are impossible), IAC escaping in both directions, CR NUL line handling, optional
// TLS (telnets), auto-login for login:/Password: prompts using the stored username/password, local echo while the
// server does not echo, and an IAC BRK breaker. The connection is dialed through the generic sshx Dialer so proxies,
// jump hosts and SSH gateways all apply.
package telnet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/core"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/proto/rawtcp/linedisc"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// Telnet command bytes (RFC 854).
const (
	iacSE   = 240
	iacNOP  = 241
	iacDM   = 242
	iacBRK  = 243
	iacGA   = 249
	iacSB   = 250
	iacWILL = 251
	iacWONT = 252
	iacDO   = 253
	iacDONT = 254
	iacIAC  = 255
)

// Telnet options.
const (
	optBINARY = 0
	optECHO   = 1
	optSGA    = 3
	optTTYPE  = 24
	optNAWS   = 31
	optNEWENV = 39

	subIS   = 0
	subSEND = 1

	// NEW-ENVIRON (RFC 1572) type codes.
	envVAR     = 0
	envVALUE   = 1
	envESC     = 2
	envUSERVAR = 3
)

// TLSPort is the conventional telnets port, used when options.tls is set without an explicit port.
const TLSPort = 992

// maxSubneg bounds a subnegotiation buffer (a hostile server cannot make us allocate without limit).
const maxSubneg = 4096

// Mount registers the "telnet" terminal protocol.
func Mount(d *app.Deps, c *core.Core) error {
	term.RegisterProtocol(string(model.ProtoTelnet), func(ctx context.Context, req term.OpenRequest) (term.Backend, error) {
		return open(ctx, c, req)
	})
	return nil
}

func open(ctx context.Context, c *core.Core, req term.OpenRequest) (term.Backend, error) {
	conn := req.Connection
	if conn == nil {
		return nil, term.Permanent(errors.New("telnet: missing connection"))
	}
	if strings.TrimSpace(conn.Host) == "" {
		return nil, term.Permanent(errors.New("telnet: host is required"))
	}
	if c == nil || c.SSH == nil {
		return nil, term.Permanent(errors.New("telnet: no dialer available"))
	}
	o := conn.Options
	useTLS := o.Bool("tls") || strings.EqualFold(o.String("transport", ""), "tls") || conn.Port == TLSPort
	spec := conn.Clone()
	if spec.Port <= 0 {
		spec.Port = model.DefaultPort(model.ProtoTelnet)
		if useTLS {
			spec.Port = TLSPort
		}
	}
	addr := net.JoinHostPort(spec.Host, strconv.Itoa(spec.Port))

	netConn, err := c.SSH.DialConnection(ctx, req.User, spec, req.Secrets)
	if err != nil {
		return nil, fmt.Errorf("telnet: connect to %s: %w", addr, err)
	}
	if useTLS {
		tconn := tls.Client(netConn, &tls.Config{
			ServerName:         spec.Host,
			InsecureSkipVerify: o.Bool("insecureTls") || o.Bool("insecure"),
			// telnets endpoints are mostly legacy network gear; TLS 1.0/1.1 still beats clear text.
			MinVersion: tls.VersionTLS10,
		})
		hctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := tconn.HandshakeContext(hctx)
		cancel()
		if err != nil {
			netConn.Close()
			if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
				// A certificate the user has to decide about: retrying cannot help.
				return nil, term.Permanent(fmt.Errorf("telnet: TLS certificate of %s rejected: %w", addr, err))
			}
			return nil, fmt.Errorf("telnet: TLS handshake with %s failed: %w", addr, err)
		}
		netConn = tconn
	}

	cols, rows := 80, 24
	if req.Session != nil {
		cols, rows = req.Session.Size()
	}
	b := newBackend(netConn, settings{
		negotiate: o.Bool("negotiate", true),
		termType:  o.String("term", "xterm-256color"),
		lineMode:  parseReturn(o.String("lineEnding", "")),
		localEcho: o.Bool("localEcho"),
		username:  conn.Username,
		password:  req.Secrets[model.SecretPassword],
		cols:      cols,
		rows:      rows,
	})
	if b.negotiate {
		b.sendInitial()
	}
	return b, nil
}

type settings struct {
	negotiate          bool
	termType           string
	lineMode           returnMode
	localEcho          bool
	username, password string
	cols, rows         int
}

// backend is a live telnet connection. The wire is parsed by a single pump goroutine (inside linedisc.Reader); input
// and negotiation replies share writeMu.
type backend struct {
	conn net.Conn
	out  *linedisc.Reader // application data from the server merged with local echo

	negotiate bool
	termType  string
	lineMode  returnMode
	localEcho bool

	writeMu sync.Mutex // serializes all writes to conn (user input, negotiation replies, auto-login)

	mu         sync.Mutex
	cols, rows int
	us, him    [256]qstate // RFC 1143 option state: our side (WILL) and the server's side (DO)
	echo       linedisc.Echo

	parser parser // used only by the pump goroutine

	username, password string
	login              loginState // used only by the pump goroutine

	closeOnce sync.Once
}

// qstate is an RFC 1143 option state (the WANTNO/queue refinements are unnecessary for the options we accept).
type qstate uint8

const (
	qNo qstate = iota
	qYes
	qWantYes // we asked; waiting for the peer's answer
)

type loginState struct {
	enabled  bool
	sentUser bool
	sentPass bool
	seen     []byte // rolling window of recent output for prompt matching
	bytes    int    // total application bytes observed (bounds the expect window)
}

func newBackend(conn net.Conn, s settings) *backend {
	b := &backend{
		conn:      conn,
		negotiate: s.negotiate,
		termType:  s.termType,
		lineMode:  s.lineMode,
		localEcho: s.localEcho,
		cols:      s.cols,
		rows:      s.rows,
		username:  s.username,
		password:  s.password,
	}
	if b.termType == "" {
		b.termType = "xterm-256color"
	}
	b.login.enabled = b.username != "" || b.password != ""
	b.out = linedisc.NewReader(wireReader{b})
	return b
}

// sendInitial volunteers the options a modern client supports (active negotiation); the server answers DO/DONT and
// WILL/WONT, which complete the RFC 1143 handshakes without further replies.
func (b *backend) sendInitial() {
	b.mu.Lock()
	var msg []byte
	for _, opt := range []byte{optTTYPE, optNAWS, optNEWENV, optSGA} {
		if b.us[opt] == qNo {
			b.us[opt] = qWantYes
			msg = append(msg, iacIAC, iacWILL, opt)
		}
	}
	for _, opt := range []byte{optSGA, optECHO} {
		if b.him[opt] == qNo {
			b.him[opt] = qWantYes
			msg = append(msg, iacIAC, iacDO, opt)
		}
	}
	b.mu.Unlock()
	b.write(msg)
}

func (b *backend) write(p []byte) {
	if len(p) == 0 {
		return
	}
	b.writeMu.Lock()
	_, _ = b.conn.Write(p)
	b.writeMu.Unlock()
}

// ---- Backend interface --------------------------------------------------------------------------------------------

// Read returns application data (IAC sequences removed) merged with local echo.
func (b *backend) Read(p []byte) (int, error) { return b.out.Read(p) }

// wireReader reads the raw connection and returns the application data of each chunk (possibly none) after running
// the IAC state machine and the auto-login expect. It runs on the linedisc pump goroutine only.
type wireReader struct{ b *backend }

func (w wireReader) Read(p []byte) (int, error) {
	b := w.b
	n, err := b.conn.Read(p)
	if n > 0 {
		data := b.parser.feed(b, p[:n])
		if len(data) > 0 {
			b.runLogin(data)
		}
		n = copy(p, data) // data is never longer than the raw chunk
	}
	return n, err
}

// Write sends user input to the server: IAC bytes doubled, and CR translated per the return mode outside BINARY. When
// local echo is on and the server does not echo, the input is echoed immediately.
func (b *backend) Write(p []byte) (int, error) {
	b.mu.Lock()
	binary := b.us[optBINARY] == qYes
	serverEcho := b.him[optECHO] == qYes
	var echo []byte
	if b.localEcho && !serverEcho {
		echo = b.echo.Render(p)
	}
	b.mu.Unlock()
	out := encodeOutbound(p, binary, b.lineMode)
	b.writeMu.Lock()
	_, err := b.conn.Write(out)
	b.writeMu.Unlock()
	if err != nil {
		return 0, err
	}
	b.out.Inject(echo)
	return len(p), nil
}

// Resize records the size and sends NAWS when the server enabled it (also in passive negotiation mode).
func (b *backend) Resize(cols, rows int) error {
	b.mu.Lock()
	changed := b.cols != cols || b.rows != rows
	b.cols, b.rows = cols, rows
	naws := b.us[optNAWS] == qYes
	b.mu.Unlock()
	if naws && changed {
		b.sendNAWS(cols, rows)
	}
	return nil
}

// SendBreak sends the telnet BREAK command (IAC BRK).
func (b *backend) SendBreak() error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	_, err := b.conn.Write([]byte{iacIAC, iacBRK})
	return err
}

func (b *backend) Close() error {
	var err error
	b.closeOnce.Do(func() {
		b.out.Close()
		err = b.conn.Close()
	})
	return err
}

// ---- wire parser --------------------------------------------------------------------------------------------------

type parseState uint8

const (
	psData parseState = iota
	psCR              // previous data byte was CR (NUL after it is dropped outside BINARY)
	psIAC
	psOpt // waiting for the option byte of WILL/WONT/DO/DONT
	psSB
	psSBIAC
)

// parser is the streaming IAC state machine; sequences split across reads are handled by its state.
type parser struct {
	state parseState
	cmd   byte
	sb    []byte
	sbBad bool // subnegotiation exceeded maxSubneg: discarded
}

// feed consumes raw bytes and returns the application data they contain. Negotiation replies are written to the
// connection as a side effect.
func (ps *parser) feed(b *backend, in []byte) []byte {
	out := make([]byte, 0, len(in))
	for _, c := range in {
		switch ps.state {
		case psData, psCR:
			if c == iacIAC {
				ps.state = psIAC
				continue
			}
			if ps.state == psCR {
				ps.state = psData
				if c == 0 && !b.binaryIn() {
					continue // CR NUL = bare carriage return
				}
			}
			out = append(out, c)
			if c == '\r' {
				ps.state = psCR
			}
		case psIAC:
			ps.state = psData
			switch c {
			case iacIAC:
				out = append(out, iacIAC)
			case iacWILL, iacWONT, iacDO, iacDONT:
				ps.cmd, ps.state = c, psOpt
			case iacSB:
				ps.sb, ps.sbBad, ps.state = ps.sb[:0], false, psSB
			default:
				// NOP, DM, GA, BRK, IP, AO, AYT, EC, EL, stray SE: nothing to render.
			}
		case psOpt:
			ps.state = psData
			b.handleNegotiate(ps.cmd, c)
		case psSB:
			if c == iacIAC {
				ps.state = psSBIAC
				continue
			}
			ps.sbAppend(c)
		case psSBIAC:
			switch c {
			case iacIAC:
				ps.sbAppend(iacIAC)
				ps.state = psSB
			case iacSE:
				ps.state = psData
				if !ps.sbBad {
					b.handleSubneg(ps.sb)
				}
			default:
				// Protocol violation (IAC <cmd> inside SB): end the subnegotiation and treat c as a command.
				if !ps.sbBad {
					b.handleSubneg(ps.sb)
				}
				ps.state = psIAC
				out = append(out, ps.feed(b, []byte{c})...)
			}
		}
	}
	return out
}

func (ps *parser) sbAppend(c byte) {
	if ps.sbBad {
		return
	}
	if len(ps.sb) >= maxSubneg {
		ps.sbBad = true
		return
	}
	ps.sb = append(ps.sb, c)
}

func (b *backend) binaryIn() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.him[optBINARY] == qYes
}

// serverEchoes reports whether the server performs ECHO (local echo is then suppressed).
func (b *backend) serverEchoes() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.him[optECHO] == qYes
}

// ---- negotiation --------------------------------------------------------------------------------------------------

// supportedLocal lists the options we are willing to perform (WILL).
func supportedLocal(opt byte) bool {
	switch opt {
	case optTTYPE, optNAWS, optNEWENV, optSGA, optBINARY:
		return true
	}
	return false
}

// supportedRemote lists the options we let the server perform (DO).
func supportedRemote(opt byte) bool {
	switch opt {
	case optECHO, optSGA, optBINARY:
		return true
	}
	return false
}

// handleNegotiate applies RFC 1143 rules: a request for the current state is ignored, a confirmation of our own
// request is not answered, a refusal of an unsupported option is sent at most once per request.
func (b *backend) handleNegotiate(cmd, opt byte) {
	var reply []byte
	sendNAWS := false
	b.mu.Lock()
	switch cmd {
	case iacWILL: // the server wants to (or agrees to) perform opt
		switch b.him[opt] {
		case qNo:
			if supportedRemote(opt) {
				b.him[opt] = qYes
				reply = []byte{iacIAC, iacDO, opt}
			} else {
				reply = []byte{iacIAC, iacDONT, opt}
			}
		case qWantYes:
			b.him[opt] = qYes // answer to our DO
		}
	case iacWONT:
		switch b.him[opt] {
		case qYes:
			b.him[opt] = qNo
			reply = []byte{iacIAC, iacDONT, opt}
		case qWantYes:
			b.him[opt] = qNo // our DO was refused
		}
	case iacDO: // the server asks us to perform opt
		switch b.us[opt] {
		case qNo:
			if supportedLocal(opt) {
				b.us[opt] = qYes
				reply = []byte{iacIAC, iacWILL, opt}
				sendNAWS = opt == optNAWS
			} else {
				reply = []byte{iacIAC, iacWONT, opt}
			}
		case qWantYes:
			b.us[opt] = qYes
			sendNAWS = opt == optNAWS
		}
	case iacDONT:
		switch b.us[opt] {
		case qYes:
			b.us[opt] = qNo
			reply = []byte{iacIAC, iacWONT, opt}
		case qWantYes:
			b.us[opt] = qNo
		}
	}
	cols, rows := b.cols, b.rows
	b.mu.Unlock()

	b.write(reply)
	if sendNAWS {
		b.sendNAWS(cols, rows)
	}
}

func (b *backend) handleSubneg(data []byte) {
	if len(data) < 2 || data[1] != subSEND {
		return
	}
	switch data[0] {
	case optTTYPE:
		payload := []byte{iacIAC, iacSB, optTTYPE, subIS}
		payload = appendDoubled(payload, []byte(strings.ToUpper(b.termType))...)
		payload = append(payload, iacIAC, iacSE)
		b.write(payload)
	case optNEWENV:
		b.write(newEnvReply(b.username))
	}
}

// newEnvReply builds the NEW-ENVIRON IS reply carrying USER (RFC 1572): type codes inside names and values are
// escaped with ESC, IAC is doubled.
func newEnvReply(username string) []byte {
	payload := []byte{iacIAC, iacSB, optNEWENV, subIS}
	if username != "" {
		payload = append(payload, envVAR)
		payload = appendEnvString(payload, "USER")
		payload = append(payload, envVALUE)
		payload = appendEnvString(payload, username)
	}
	return append(payload, iacIAC, iacSE)
}

func appendEnvString(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case envVAR, envVALUE, envESC, envUSERVAR:
			dst = append(dst, envESC, c)
		case iacIAC:
			dst = append(dst, iacIAC, iacIAC)
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

func (b *backend) sendNAWS(cols, rows int) {
	cols = min(max(cols, 1), 0xffff)
	rows = min(max(rows, 1), 0xffff)
	sb := []byte{iacIAC, iacSB, optNAWS}
	sb = appendDoubled(sb, byte(cols>>8), byte(cols), byte(rows>>8), byte(rows))
	sb = append(sb, iacIAC, iacSE)
	b.write(sb)
}

// ---- auto-login ---------------------------------------------------------------------------------------------------

const loginWindow = 8 << 10 // stop watching for prompts after this many bytes

// runLogin answers login:/Password: prompts with the stored credentials, each at most once per connection.
func (b *backend) runLogin(data []byte) {
	st := &b.login
	if !st.enabled || (st.sentUser && st.sentPass) || st.bytes > loginWindow {
		return
	}
	st.bytes += len(data)
	st.seen = append(st.seen, data...)
	if len(st.seen) > 256 {
		st.seen = append(st.seen[:0], st.seen[len(st.seen)-256:]...)
	}
	tail := strings.ToLower(strings.TrimRight(string(st.seen), " \t"))
	var send string
	switch {
	case !st.sentUser && b.username != "" && matchesPrompt(tail, "login:", "username:", "user name:"):
		st.sentUser = true
		send = b.username + "\r"
		st.seen = st.seen[:0]
	case !st.sentPass && b.password != "" && matchesPrompt(tail, "password:", "passcode:"):
		st.sentPass = true
		send = b.password + "\r"
		st.seen = st.seen[:0]
		if b.username == "" {
			st.sentUser = true
		}
	}
	if send == "" {
		return
	}
	b.mu.Lock()
	binary := b.us[optBINARY] == qYes
	b.mu.Unlock()
	b.write(encodeOutbound([]byte(send), binary, b.lineMode))
}

func matchesPrompt(tail string, prompts ...string) bool {
	for _, p := range prompts {
		if strings.HasSuffix(tail, p) {
			return true
		}
	}
	return false
}

// ---- outbound encoding --------------------------------------------------------------------------------------------

type returnMode int

const (
	returnCRNUL returnMode = iota // telnet default: CR → CR NUL (outside BINARY)
	returnCRLF
	returnLF
	returnCR
)

func parseReturn(s string) returnMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "crlf":
		return returnCRLF
	case "lf":
		return returnLF
	case "cr":
		return returnCR
	default:
		return returnCRNUL
	}
}

// encodeOutbound doubles IAC bytes and translates CR according to the return mode (only meaningful outside BINARY).
func encodeOutbound(p []byte, binary bool, mode returnMode) []byte {
	out := make([]byte, 0, len(p)+8)
	for _, c := range p {
		switch {
		case c == iacIAC:
			out = append(out, iacIAC, iacIAC)
		case c == '\r' && !binary:
			switch mode {
			case returnCRLF:
				out = append(out, '\r', '\n')
			case returnLF:
				out = append(out, '\n')
			case returnCR:
				out = append(out, '\r')
			default: // returnCRNUL
				out = append(out, '\r', 0)
			}
		default:
			out = append(out, c)
		}
	}
	return out
}

func appendDoubled(dst []byte, bs ...byte) []byte {
	for _, c := range bs {
		if c == iacIAC {
			dst = append(dst, iacIAC, iacIAC)
		} else {
			dst = append(dst, c)
		}
	}
	return dst
}

// compile-time interface checks
var (
	_ term.Backend = (*backend)(nil)
	_ term.Breaker = (*backend)(nil)
	_ io.Reader    = wireReader{}
)
