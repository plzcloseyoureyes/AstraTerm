// Package rlogin implements the "rlogin" terminal protocol with two variants selected by options.variant (PROTO-7,
// PROTO-8, RESEARCH §3.5): "rlogin" (RFC 1282, TCP 513, privileged source port with graceful fallback, window-size
// control messages, password auto-login) and "rsh" (BSD remote shell, TCP 514, stderr back-channel, runs
// options.command with stdin from the terminal). Both are plaintext and flagged insecure in the UI. Connections are
// dialed through the generic sshx Dialer so proxies and SSH gateways apply; privileged source ports (and the rsh
// stderr back-channel, which needs the server to connect back to us) are only used for direct connections. Direct
// connections are vetted by the session owner's destination guard (internal/netguard, SEC-7) on the concrete dialed
// address; routed ones by the sshx route.
package rlogin

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/core"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/netguard"
	"github.com/plzcloseyoureyes/astraterm/internal/proto/rawtcp/linedisc"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// RshPort is the rsh (shell) service port.
const RshPort = 514

// Mount registers the "rlogin" terminal protocol (the "rsh" variant is options.variant="rsh").
func Mount(d *app.Deps, c *core.Core) error {
	term.RegisterProtocol(string(model.ProtoRlogin), func(ctx context.Context, req term.OpenRequest) (term.Backend, error) {
		return open(ctx, d, c, req)
	})
	return nil
}

func open(ctx context.Context, d *app.Deps, c *core.Core, req term.OpenRequest) (term.Backend, error) {
	conn := req.Connection
	if conn == nil {
		return nil, term.Permanent(errors.New("rlogin: missing connection"))
	}
	if strings.TrimSpace(conn.Host) == "" {
		return nil, term.Permanent(errors.New("rlogin: host is required"))
	}
	// The destination guard of the session owner (nil = unrestricted: desktop mode, administrators).
	g := netguard.ForUser(d, req.User)
	switch variant := strings.ToLower(strings.TrimSpace(conn.Options.String("variant", "rlogin"))); variant {
	case "rsh":
		return openRsh(ctx, c, g, req)
	case "", "rlogin":
		return openRlogin(ctx, c, g, req)
	default:
		return nil, term.Permanent(fmt.Errorf("rlogin: unknown variant %q (use rlogin or rsh)", variant))
	}
}

// ---- shared dialing -----------------------------------------------------------------------------------------------

// dial connects to conn's host:port. Direct connections first try a reserved source port (512–1023, needed for
// .rhosts trust); if no reserved port can be bound (not root / no CAP_NET_BIND_SERVICE) an ephemeral port is used, and
// the server falls back to a password prompt. Routed connections use the generic dialer (which applies the user's
// guard itself). Direct dials are vetted by g (nil = unrestricted) on every concrete address, before the socket binds
// or connects; a refusal is permanent (no auto-reconnect loop) and unwraps to 403 destination_blocked.
func dial(ctx context.Context, c *core.Core, g *netguard.Guard, req term.OpenRequest, port int) (net.Conn, error) {
	conn := req.Connection
	if routed(conn) {
		if c == nil || c.SSH == nil {
			return nil, term.Permanent(errors.New("no dialer available"))
		}
		spec := conn.Clone()
		spec.Port = port
		return c.SSH.DialConnection(ctx, req.User, spec, req.Secrets)
	}
	addr := netguard.CanonicalAddr(net.JoinHostPort(conn.Host, strconv.Itoa(port)))
	nc, err := dialReservedPort(ctx, addr, g)
	if err != nil && errors.Is(err, errNoReservedPort) {
		d := &net.Dialer{Timeout: 20 * time.Second, Control: g.Control}
		nc, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		if be, ok := netguard.IsBlocked(err); ok {
			return nil, term.Permanent(be)
		}
		return nil, err
	}
	return nc, nil
}

func routed(conn *model.Connection) bool {
	o := conn.Options
	if strings.TrimSpace(o.String("sshTunnelVia", "")) != "" {
		return true
	}
	if len(o.Strings("jumpHosts")) > 0 {
		return true
	}
	if strings.TrimSpace(o.String("proxyCommand", "")) != "" {
		return true
	}
	pt := strings.ToLower(strings.TrimSpace(model.Options(o.Map("proxy")).String("type", "")))
	return pt != "" && pt != "none"
}

var errNoReservedPort = errors.New("no reserved source port available")

// dialReservedPort is dialReserved (a variable so tests can exercise the unprivileged fallback dialer).
var dialReservedPort = dialReserved

// Reserved-port range tried from the top, like BSD rresvport().
const (
	reservedHigh = 1023
	reservedLow  = 512
	reservedTry  = 32
)

// dialReserved connects from a reserved source port. It returns errNoReservedPort when binding is not permitted or
// every tried port is busy; a failure to reach the server (refused, timeout, ...) is returned as is, without trying
// more ports (so an unreachable host fails once, not once per port). g (nil = unrestricted) vets the destination in
// Control, which runs before the source port is bound, so a refusal is returned as is (never errNoReservedPort).
func dialReserved(ctx context.Context, addr string, g *netguard.Guard) (net.Conn, error) {
	for p, tries := reservedHigh, 0; p >= reservedLow && tries < reservedTry; p, tries = p-1, tries+1 {
		d := &net.Dialer{Timeout: 20 * time.Second, LocalAddr: &net.TCPAddr{Port: p}, Control: g.Control}
		nc, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			return nc, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		switch {
		case isBindError(err, syscall.EACCES) || isBindError(err, syscall.EPERM):
			return nil, errNoReservedPort // not privileged: every reserved port fails the same way
		case isBindError(err, syscall.EADDRINUSE) || errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, syscall.EADDRNOTAVAIL):
			continue // port busy (or its 4-tuple still in TIME_WAIT): try the next one
		default:
			return nil, err
		}
	}
	return nil, errNoReservedPort
}

func isBindError(err error, errno syscall.Errno) bool {
	var se *os.SyscallError
	return errors.As(err, &se) && se.Syscall == "bind" && errors.Is(se.Err, errno)
}

func localUser(conn *model.Connection) string {
	if lu := strings.TrimSpace(conn.Options.String("localUser", "")); lu != "" {
		return lu
	}
	if conn.Username != "" {
		return conn.Username
	}
	return "root"
}

// validField rejects values that would break the NUL-separated handshake.
func validField(name, v string) error {
	if strings.ContainsRune(v, 0) {
		return term.Permanent(fmt.Errorf("rlogin: %s must not contain NUL bytes", name))
	}
	if len(v) > 1024 {
		return term.Permanent(fmt.Errorf("rlogin: %s is too long", name))
	}
	return nil
}

// ---- rlogin (RFC 1282) --------------------------------------------------------------------------------------------

func openRlogin(ctx context.Context, c *core.Core, g *netguard.Guard, req term.OpenRequest) (term.Backend, error) {
	conn := req.Connection
	port := conn.Port
	if port <= 0 {
		port = model.DefaultPort(model.ProtoRlogin)
	}
	for name, v := range map[string]string{"local user": localUser(conn), "user name": conn.Username, "terminal type": conn.Options.String("term", "")} {
		if err := validField(name, v); err != nil {
			return nil, err
		}
	}
	nc, err := dial(ctx, c, g, req, port)
	if err != nil {
		if _, blocked := netguard.IsBlocked(err); blocked {
			return nil, err // already names the destination; permanent
		}
		return nil, fmt.Errorf("rlogin: connect to %s: %w", net.JoinHostPort(conn.Host, strconv.Itoa(port)), err)
	}
	cols, rows := 80, 24
	if req.Session != nil {
		cols, rows = req.Session.Size()
	}
	var b *rloginBackend
	err = guardHandshake(ctx, nc, func() (err error) {
		b, err = startRlogin(nc, conn, req.Secrets, cols, rows)
		return err
	})
	if err != nil {
		nc.Close()
		return nil, err
	}
	return b, nil
}

// handshakeTimeout bounds the protocol handshakes (the server answers immediately when it is alive).
const handshakeTimeout = 20 * time.Second

// guardHandshake runs a blocking handshake on nc, closing nc when ctx ends (session closed) or the handshake takes
// longer than handshakeTimeout — connection deadlines are not supported by every route (SSH gateway channels).
func guardHandshake(ctx context.Context, nc net.Conn, fn func() error) error {
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	stop := context.AfterFunc(hctx, func() { nc.Close() })
	err := fn()
	if !stop() {
		// The watchdog fired and closed the connection: report why.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("the server did not complete the handshake in time")
	}
	return err
}

// startRlogin performs the rlogin handshake on an established connection and returns the backend.
func startRlogin(nc net.Conn, conn *model.Connection, secrets map[string]string, cols, rows int) (*rloginBackend, error) {
	o := conn.Options
	termType := o.String("term", "xterm-256color")
	speed := o.Int("baud", 38400)
	// Handshake: \0 localuser \0 remoteuser \0 termtype/speed \0
	hs := []byte{0}
	hs = append(hs, localUser(conn)...)
	hs = append(hs, 0)
	hs = append(hs, conn.Username...)
	hs = append(hs, 0)
	hs = append(hs, fmt.Sprintf("%s/%d", termType, speed)...)
	hs = append(hs, 0)
	if _, err := nc.Write(hs); err != nil {
		return nil, fmt.Errorf("rlogin: handshake: %w", err)
	}
	// The server confirms with a single NUL; rlogind reports a fatal error as \x01 + message instead.
	ack := make([]byte, 1)
	if _, err := io.ReadFull(nc, ack); err != nil {
		return nil, fmt.Errorf("rlogin: no handshake confirmation: %w", err)
	}
	if ack[0] != 0 {
		msg, _ := readLine(nc)
		if ack[0] != 1 {
			msg = string(ack) + msg
		}
		return nil, term.Permanent(fmt.Errorf("rlogin: server refused the connection: %s", cleanLine(msg)))
	}

	b := &rloginBackend{
		conn:      nc,
		lineMode:  linedisc.ParseEnding(o.String("lineEnding", ""), linedisc.CR),
		localEcho: o.Bool("localEcho"),
		password:  secrets[model.SecretPassword],
	}
	b.login.enabled = b.password != ""
	b.out = linedisc.NewReader(rloginWire{b})
	b.sendWindowSize(cols, rows)
	return b, nil
}

type rloginBackend struct {
	conn      net.Conn
	out       *linedisc.Reader
	lineMode  linedisc.Ending
	localEcho bool
	password  string

	writeMu    sync.Mutex
	echo       linedisc.Echo // guarded by writeMu
	cols, rows int           // last window size sent, guarded by writeMu
	login      loginState    // used by the pump goroutine only

	closeOnce sync.Once
}

// rloginWire feeds the remote stream through the password auto-login expect (pump goroutine only).
type rloginWire struct{ b *rloginBackend }

func (w rloginWire) Read(p []byte) (int, error) {
	n, err := w.b.conn.Read(p)
	if n > 0 {
		if send := w.b.login.match("", w.b.password, p[:n]); send != "" {
			w.b.writeMu.Lock()
			_, _ = w.b.conn.Write(w.b.lineMode.Translate([]byte(send)))
			w.b.writeMu.Unlock()
		}
	}
	return n, err
}

func (b *rloginBackend) Read(p []byte) (int, error) { return b.out.Read(p) }

func (b *rloginBackend) Write(p []byte) (int, error) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if _, err := b.conn.Write(b.lineMode.Translate(p)); err != nil {
		return 0, err
	}
	if b.localEcho {
		b.out.Inject(b.echo.Render(p))
	}
	return len(p), nil
}

func (b *rloginBackend) Resize(cols, rows int) error {
	b.sendWindowSize(cols, rows)
	return nil
}

// windowSizeMessage is the rlogin window-change control message: FF FF 's' 's' rows cols xpix ypix (u16 BE).
func windowSizeMessage(cols, rows int) []byte {
	cols = min(max(cols, 1), 0xffff)
	rows = min(max(rows, 1), 0xffff)
	msg := make([]byte, 12)
	msg[0], msg[1], msg[2], msg[3] = 0xFF, 0xFF, 's', 's'
	binary.BigEndian.PutUint16(msg[4:], uint16(rows))
	binary.BigEndian.PutUint16(msg[6:], uint16(cols))
	return msg
}

// sendWindowSize sends the window size proactively (the server's out-of-band request byte cannot be read portably),
// once per change.
func (b *rloginBackend) sendWindowSize(cols, rows int) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if cols == b.cols && rows == b.rows {
		return
	}
	b.cols, b.rows = cols, rows
	_, _ = b.conn.Write(windowSizeMessage(cols, rows))
}

func (b *rloginBackend) Close() error {
	var err error
	b.closeOnce.Do(func() {
		b.out.Close()
		err = b.conn.Close()
	})
	return err
}

// ---- rsh (BSD remote shell) ---------------------------------------------------------------------------------------

func openRsh(ctx context.Context, c *core.Core, g *netguard.Guard, req term.OpenRequest) (term.Backend, error) {
	conn := req.Connection
	port := conn.Port
	if port <= 0 || port == model.DefaultPort(model.ProtoRlogin) {
		port = RshPort
	}
	command := strings.TrimSpace(conn.Options.String("command", ""))
	if command == "" {
		return nil, term.Permanent(errors.New("rsh: options.command is required"))
	}
	for name, v := range map[string]string{"local user": localUser(conn), "user name": conn.Username, "command": command} {
		if err := validField(name, v); err != nil {
			return nil, err
		}
	}
	nc, err := dial(ctx, c, g, req, port)
	if err != nil {
		if _, blocked := netguard.IsBlocked(err); blocked {
			return nil, err // already names the destination; permanent
		}
		return nil, fmt.Errorf("rsh: connect to %s: %w", net.JoinHostPort(conn.Host, strconv.Itoa(port)), err)
	}
	var b *rshBackend
	err = guardHandshake(ctx, nc, func() (err error) {
		b, err = startRsh(nc, conn, command, !routed(conn))
		return err
	})
	if err != nil {
		if b != nil {
			b.Close()
		}
		nc.Close()
		return nil, err
	}
	return b, nil
}

// startRsh performs the rsh handshake on an established connection. With backChannel, it listens on a reserved port
// of the connection's local address for the server's stderr connection (rshd insists on a reserved port, so without
// one — or through a proxy/gateway the server cannot connect back through — stderr is merged into stdout).
func startRsh(nc net.Conn, conn *model.Connection, command string, backChannel bool) (*rshBackend, error) {
	o := conn.Options
	b := &rshBackend{
		conn:      nc,
		lineMode:  linedisc.ParseEnding(o.String("lineEnding", ""), linedisc.LF),
		localEcho: o.Bool("localEcho", true),
		stderrEOF: make(chan struct{}),
	}
	var ln *net.TCPListener
	if backChannel {
		ln = listenBackChannel(nc.LocalAddr())
	}
	stderrPort := "0"
	if ln != nil {
		stderrPort = strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	} else {
		close(b.stderrEOF)
	}

	// Protocol: <stderrport>\0 localuser\0 remoteuser\0 command\0, answered by a status byte (0 = ok, else a message).
	hs := append([]byte(stderrPort), 0)
	hs = append(hs, localUser(conn)...)
	hs = append(hs, 0)
	hs = append(hs, conn.Username...)
	hs = append(hs, 0)
	hs = append(hs, command...)
	hs = append(hs, 0)

	type acceptResult struct {
		c   net.Conn
		err error
	}
	accepted := make(chan acceptResult, 1)
	if ln != nil {
		serverIP := addrIP(nc.RemoteAddr())
		go func() {
			defer ln.Close()
			_ = ln.SetDeadline(time.Now().Add(20 * time.Second))
			for {
				c, err := ln.Accept()
				if err != nil {
					accepted <- acceptResult{err: err}
					return
				}
				// Only the server we talk to may attach our stderr (anyone could connect to the listener).
				if serverIP != nil && !addrIP(c.RemoteAddr()).Equal(serverIP) {
					c.Close()
					continue
				}
				accepted <- acceptResult{c: c}
				return
			}
		}()
	}
	fail := func(err error) (*rshBackend, error) {
		if ln != nil {
			ln.Close()
			if r := <-accepted; r.c != nil {
				r.c.Close()
			}
		}
		return nil, err
	}

	if _, err := nc.Write(hs); err != nil {
		return fail(fmt.Errorf("rsh: handshake: %w", err))
	}
	status := make([]byte, 1)
	if _, err := io.ReadFull(nc, status); err != nil {
		return fail(fmt.Errorf("rsh: no reply from the server: %w", err))
	}
	if status[0] != 0 {
		msg, _ := readLine(nc)
		if status[0] != 1 {
			msg = string(status) + msg
		}
		return fail(term.Permanent(fmt.Errorf("rsh: server rejected the command: %s", cleanLine(msg))))
	}

	b.out = linedisc.NewReader(rshStdout{b})
	if ln != nil {
		// rshd connects the back-channel before it confirms, so it is (almost) always accepted by now; never block
		// the session on it, though.
		go func() {
			defer close(b.stderrEOF)
			r := <-accepted
			if r.c == nil {
				return
			}
			b.mu.Lock()
			if b.closed {
				b.mu.Unlock()
				r.c.Close()
				return
			}
			b.stderr = r.c
			b.mu.Unlock()
			b.pumpStderr(r.c)
		}()
	}
	return b, nil
}

type rshBackend struct {
	conn      net.Conn
	out       *linedisc.Reader
	lineMode  linedisc.Ending
	localEcho bool

	writeMu  sync.Mutex
	echo     linedisc.Echo // guarded by writeMu
	stdinEOF bool          // guarded by writeMu

	mu        sync.Mutex
	stderr    net.Conn
	closed    bool
	stderrEOF chan struct{} // closed when the stderr stream ended (or never existed)

	closeOnce sync.Once
}

// rshStdout is the stdout stream; its EOF is delayed until stderr ended too (briefly), so trailing error output is
// not lost when the command exits.
type rshStdout struct{ b *rshBackend }

func (s rshStdout) Read(p []byte) (int, error) {
	n, err := s.b.conn.Read(p)
	if err == io.EOF {
		select {
		case <-s.b.stderrEOF:
		case <-time.After(time.Second):
		}
	}
	return n, err
}

func (b *rshBackend) pumpStderr(c net.Conn) {
	buf := make([]byte, 16<<10)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			chunk := make([]byte, 0, n+9)
			chunk = append(chunk, "\x1b[31m"...)
			chunk = append(chunk, buf[:n]...)
			chunk = append(chunk, "\x1b[0m"...)
			if _, werr := b.out.Write(chunk); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Read returns stdout and stderr (red) interleaved in arrival order, plus local echo.
func (b *rshBackend) Read(p []byte) (int, error) { return b.out.Read(p) }

// Write sends terminal input to the command's stdin (Enter → options.lineEnding, LF by default). Ctrl+D closes stdin
// (the command sees end-of-file); later input is discarded.
func (b *rshBackend) Write(p []byte) (int, error) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.stdinEOF {
		return len(p), nil
	}
	data := p
	eof := false
	if i := bytes.IndexByte(p, 0x04); i >= 0 {
		data, eof = p[:i], true
	}
	if len(data) > 0 {
		if _, err := b.conn.Write(b.lineMode.Translate(data)); err != nil {
			return 0, err
		}
		if b.localEcho {
			b.out.Inject(b.echo.Render(data))
		}
	}
	if eof {
		b.stdinEOF = true
		if cw, ok := b.conn.(interface{ CloseWrite() error }); ok {
			if err := cw.CloseWrite(); err != nil {
				return 0, err
			}
		}
		if b.localEcho {
			b.out.Inject([]byte("^D\r\n"))
		}
	}
	return len(p), nil
}

func (b *rshBackend) Resize(int, int) error { return nil }

func (b *rshBackend) Close() error {
	var err error
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		se := b.stderr
		b.mu.Unlock()
		if b.out != nil {
			b.out.Close()
		}
		if se != nil {
			se.Close()
		}
		err = b.conn.Close()
	})
	return err
}

// listenBackChannel opens the rsh stderr listener (a variable so tests can use an unprivileged port).
var listenBackChannel = listenReserved

// listenReserved listens on a reserved port of local's IP (the address the server sees us from), or returns nil.
func listenReserved(local net.Addr) *net.TCPListener {
	ip := addrIP(local)
	for p, tries := reservedHigh-1, 0; p >= reservedLow && tries < reservedTry; p, tries = p-1, tries+1 {
		ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: ip, Port: p})
		if err == nil {
			return ln
		}
		if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
			return nil
		}
	}
	return nil
}

func addrIP(a net.Addr) net.IP {
	if ta, ok := a.(*net.TCPAddr); ok {
		return ta.IP
	}
	if a == nil {
		return nil
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

// ---- shared helpers -----------------------------------------------------------------------------------------------

func readLine(r io.Reader) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 1)
	for i := 0; i < 512; i++ {
		if _, err := r.Read(buf); err != nil {
			return sb.String(), err
		}
		if buf[0] == '\n' {
			break
		}
		sb.WriteByte(buf[0])
	}
	return sb.String(), nil
}

// cleanLine makes a server message safe for a status line.
func cleanLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
	if s = strings.TrimSpace(s); s == "" {
		return "no reason given"
	}
	return s
}

type loginState struct {
	enabled  bool
	sentUser bool
	sentPass bool
	seen     []byte
	bytes    int
}

const loginWindow = 8 << 10

// match watches output for login:/Password: prompts and returns the credential to send (with a trailing CR), or "".
// Each prompt is answered at most once per connection.
func (st *loginState) match(username, password string, data []byte) string {
	if !st.enabled || (st.sentUser && st.sentPass) || st.bytes > loginWindow {
		return ""
	}
	st.bytes += len(data)
	st.seen = append(st.seen, data...)
	if len(st.seen) > 256 {
		st.seen = append(st.seen[:0], st.seen[len(st.seen)-256:]...)
	}
	tail := strings.ToLower(strings.TrimRight(string(st.seen), " \t"))
	switch {
	case !st.sentUser && username != "" && hasPromptSuffix(tail, "login:", "username:", "user name:"):
		st.sentUser = true
		st.seen = st.seen[:0]
		return username + "\r"
	case !st.sentPass && password != "" && hasPromptSuffix(tail, "password:", "passcode:"):
		st.sentPass = true
		st.seen = st.seen[:0]
		if username == "" {
			st.sentUser = true
		}
		return password + "\r"
	}
	return ""
}

func hasPromptSuffix(tail string, prompts ...string) bool {
	for _, p := range prompts {
		if strings.HasSuffix(tail, p) {
			return true
		}
	}
	return false
}

var (
	_ term.Backend = (*rloginBackend)(nil)
	_ term.Backend = (*rshBackend)(nil)
)
