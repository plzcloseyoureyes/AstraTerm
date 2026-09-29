package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/netguard"
)

// dialTimeout bounds how long a client connection waits for the SSH link and the destination.
const dialTimeout = 30 * time.Second

// closeWait bounds how long closing a forward waits for its connection handlers and for the SSH server to confirm
// that a remote listener was cancelled. On a black-holed link neither finishes before keep-alives kill the
// connection (minutes), and a stop must not hang that long; what is left finishes in the background.
const closeWait = 2 * time.Second

// link is the SSH transport a forward runs over (a pooled *sshx.Client in production).
type link interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
	Listen(network, addr string) (net.Listener, error)
	Done() <-chan struct{}
}

// execer is implemented by links that can run remote commands (remote port detection, stale socket cleanup).
type execer interface {
	Exec(ctx context.Context, cmd string) (stdout, stderr []byte, exitCode int, err error)
}

// linkProvider hands a live link to forwards whose listener lives on the AstraTerm host, waiting for (or, on demand,
// triggering) the SSH connection.
type linkProvider interface {
	linkFor(ctx context.Context) (link, error)
}

// forward is the data plane of one forwarding spec: the listener (on the AstraTerm host, or on the SSH server through a
// link) and the per-connection relays. It never dials SSH itself; the supervisor owns the link.
type forward struct {
	sp    *spec
	st    *stats
	log   *slog.Logger
	links linkProvider
	// localDial reaches destinations from the AstraTerm host (remote and remote-dynamic forwards).
	localDial func(ctx context.Context, network, addr string) (net.Conn, error)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	proxy  *proxy

	mu     sync.Mutex
	ln     net.Listener    // listener on the AstraTerm host
	rln    *remoteListener // current listener on the SSH server
	conns  map[net.Conn]struct{}
	closed bool
}

// remoteListener is a listener on the SSH server. Closing it removes the forward from the SSH client at once (Accept
// returns io.EOF) and then cancels it on the server with a request that waits for the server's reply; that part runs
// in the background so a dead link cannot block the caller. done is closed once the cancellation finished.
type remoteListener struct {
	net.Listener
	once sync.Once
	done chan struct{}
}

func newRemoteListener(l net.Listener) *remoteListener {
	return &remoteListener{Listener: l, done: make(chan struct{})}
}

// closeAsync starts closing the listener (once) and returns a channel closed when the server confirmed it.
func (l *remoteListener) closeAsync() <-chan struct{} {
	l.once.Do(func() {
		go func() {
			defer close(l.done)
			_ = l.Listener.Close()
		}()
	})
	return l.done
}

// Close closes the listener, waiting at most closeWait for the server's confirmation.
func (l *remoteListener) Close() error {
	select {
	case <-l.closeAsync():
	case <-time.After(closeWait):
	}
	return nil
}

func newForward(parent context.Context, sp *spec, st *stats, log *slog.Logger, links linkProvider) *forward {
	ctx, cancel := context.WithCancel(parent)
	f := &forward{sp: sp, st: st, log: log, links: links, ctx: ctx, cancel: cancel, conns: map[net.Conn]struct{}{}}
	f.localDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		// The owner's destination guard vets the concrete address (SEC-7): remote forwards and reverse SOCKS
		// clients must not reach the AstraTerm host's own services or other users' listeners.
		var g *netguard.Guard
		if sp.guard != nil {
			g = sp.guard()
		}
		return g.Dialer(dialTimeout).DialContext(ctx, network, addr)
	}
	if sp.kind.isProxy() {
		dial := f.sshDial
		if sp.kind == kindRemoteDynamic {
			dial = f.reverseProxyDial
		}
		f.proxy = newProxy(f, dial)
	}
	return f
}

// reverseProxyDial reaches destinations requested by reverse SOCKS clients (on the SSH server) from the AstraTerm host,
// except AstraTerm's own port: those clients are outside AstraTerm's trust boundary.
func (f *forward) reverseProxyDial(ctx context.Context, network, addr string) (net.Conn, error) {
	if f.sp.selfPort > 0 {
		if _, port, err := net.SplitHostPort(addr); err == nil && port == strconv.Itoa(f.sp.selfPort) {
			return nil, fmt.Errorf("refusing to connect to %s: this is AstraTerm's own port", addr)
		}
	}
	return f.hostDial(ctx, network, addr)
}

// sshDial opens a connection through the SSH server (local and dynamic forwards).
func (f *forward) sshDial(ctx context.Context, network, addr string) (net.Conn, error) {
	l, err := f.links.linkFor(ctx)
	if err != nil {
		return nil, err
	}
	c, err := l.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return f.trackUpstream(c)
}

// hostDial opens a connection from the AstraTerm host (remote and remote-dynamic forwards).
func (f *forward) hostDial(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := f.localDial(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return f.trackUpstream(c)
}

// trackUpstream registers an upstream connection so close() can tear it down.
func (f *forward) trackUpstream(c net.Conn) (net.Conn, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		c.Close()
		return nil, net.ErrClosed
	}
	t := &trackedConn{Conn: c, f: f}
	f.conns[t] = struct{}{}
	f.mu.Unlock()
	return t, nil
}

func (f *forward) untrack(c net.Conn) {
	f.mu.Lock()
	delete(f.conns, c)
	f.mu.Unlock()
}

// trackedConn is a connection to a destination (through the SSH server, or from this host). It removes itself from
// the forward's connection set when closed and counts the traffic: reads are bytes in (towards the client), writes
// bytes out. Counting on this side measures exactly the payload relayed to and from destinations — the SOCKS / HTTP
// proxy negotiation with the client and requests answered locally (PAC file, refused clients) are not traffic.
type trackedConn struct {
	net.Conn
	f    *forward
	once sync.Once
}

func (c *trackedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.f.st.bytesIn.Add(int64(n))
		c.f.st.touch()
	}
	return n, err
}

func (c *trackedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.f.st.bytesOut.Add(int64(n))
		c.f.st.touch()
	}
	return n, err
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.f.untrack(c) })
	return err
}

func (c *trackedConn) CloseWrite() error { return closeWrite(c.Conn) }

// ---- listener on the AstraTerm host (local, dynamic) ----------------------------------------------------------------

// listenLocal opens the forward's listener on the AstraTerm host and starts accepting. Errors are typed API errors
// (address in use, permission denied…).
func (f *forward) listenLocal() error {
	network, addr := f.sp.localListenAddr()
	var (
		ln  net.Listener
		err error
	)
	if network == "unix" {
		ln, err = listenUnixLocal(addr)
	} else {
		ln, err = net.Listen(network, addr)
	}
	if err != nil {
		return listenError(f.sp.bindLabel(), err)
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		ln.Close()
		return net.ErrClosed
	}
	f.ln = ln
	f.wg.Add(1)
	f.mu.Unlock()
	f.st.setLocalAddr(f.boundLocalLabel(ln.Addr()))
	go func() {
		defer f.wg.Done()
		f.acceptLoop(ln, f.handleLocalListener)
	}()
	return nil
}

// boundLocalLabel renders the bound address (resolving auto-assigned ports).
func (f *forward) boundLocalLabel(a net.Addr) string {
	if f.sp.bindSocket != "" {
		return f.sp.bindSocket
	}
	if ta, ok := a.(*net.TCPAddr); ok {
		host := f.sp.bindHost
		if host == "localhost" {
			host = ta.IP.String()
		}
		return hostPortLabel(host, strconv.Itoa(ta.Port))
	}
	return a.String()
}

// listenUnixLocal listens on a Unix socket, replacing a stale socket file (one nobody listens on) and restricting
// the socket to the AstraTerm user.
func listenUnixLocal(path string) (net.Listener, error) {
	ln, err := net.Listen("unix", path)
	if err != nil && isAddrInUse(err) {
		fi, serr := os.Lstat(path)
		if serr != nil || fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket: %w", path, err)
		}
		if c, derr := net.DialTimeout("unix", path, time.Second); derr == nil {
			c.Close()
			return nil, err // a live listener owns it
		}
		if rerr := os.Remove(path); rerr != nil {
			return nil, err
		}
		ln, err = net.Listen("unix", path)
	}
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return ln, nil
}

func (f *forward) handleLocalListener(c *clientConn) {
	if f.sp.kind.isProxy() {
		f.proxy.serve(c)
		return
	}
	network, addr := f.sp.destAddr()
	ctx, cancel := context.WithTimeout(f.ctx, dialTimeout)
	up, err := f.sshDial(ctx, network, addr)
	cancel()
	if err != nil {
		c.Close()
		if f.ctx.Err() == nil {
			f.st.fail(dialError(f.sp.destLabel(), "the SSH server", err))
		}
		return
	}
	relay(f.ctx, c, up)
}

// ---- listener on the SSH server (remote, remote dynamic) ----------------------------------------------------------

// errRemoteListen wraps a failure to open the remote listener (the link itself may still be fine).
type errRemoteListen struct{ err error }

func (e *errRemoteListen) Error() string { return e.err.Error() }
func (e *errRemoteListen) Unwrap() error { return e.err }

// serveRemote opens the forward's listener on the SSH server through l and serves it until the link ends, ctx is
// cancelled or the forward is closed. ready is called with the bound address once listening. A failure to listen
// is returned as *errRemoteListen.
func (f *forward) serveRemote(ctx context.Context, l link, ready func(addr string)) error {
	network, addr := f.sp.remoteListenAddr()
	rln, err := l.Listen(network, addr)
	if err != nil && network == "unix" && f.removeStaleRemoteSocket(ctx, l, addr) {
		rln, err = l.Listen(network, addr)
	}
	if err != nil {
		return &errRemoteListen{remoteListenError(f.sp.bindLabel(), network == "unix", err)}
	}
	rl := newRemoteListener(rln)
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		rl.closeAsync()
		return nil
	}
	f.rln = rl
	f.mu.Unlock()

	bound := f.sp.bindSocket
	if bound == "" {
		port := f.sp.bindPort
		if ta, ok := rln.Addr().(*net.TCPAddr); ok && ta.Port > 0 {
			port = ta.Port
		}
		bound = hostPortLabel(f.sp.bindHost, strconv.Itoa(port))
	}
	f.st.setRemoteAddr(bound)
	if ready != nil {
		ready(bound)
	}

	// Close the remote listener when the link dies, the forward is closed or ctx ends; Accept then fails. Closing
	// never waits for the server here: whoever stops the forward waits (bounded) in close().
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-f.ctx.Done():
		case <-l.Done():
		case <-stop:
			return
		}
		rl.closeAsync()
	}()
	f.acceptLoop(rl, f.handleRemoteListener)
	close(stop)
	// f.rln keeps pointing at this listener until the next one replaces it, so close() — typically racing with
	// this goroutine, which sees the cancelled context first — still waits for the server to cancel the forward
	// before a restart asks for the same port again.
	rl.closeAsync()
	return nil
}

// removeStaleRemoteSocket deletes a leftover remote socket file (from a previous connection) when nothing listens
// on it any more. It reports whether a retry makes sense.
func (f *forward) removeStaleRemoteSocket(ctx context.Context, l link, path string) bool {
	ex, ok := l.(execer)
	if !ok {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := l.DialContext(cctx, "unix", path)
	if err == nil {
		c.Close()
		return false // somebody is listening: never touch it
	}
	var oce *ssh.OpenChannelError
	if !errors.As(err, &oce) || oce.Reason != ssh.ConnectionFailed {
		return false // not a dead socket (forwarding prohibited, link down…)
	}
	q := shellQuote(path)
	_, _, code, err := ex.Exec(cctx, "[ -S "+q+" ] && rm -f -- "+q)
	return err == nil && code == 0
}

func (f *forward) handleRemoteListener(c *clientConn) {
	if f.sp.kind.isProxy() {
		f.proxy.serve(c)
		return
	}
	network, addr := f.sp.destAddr()
	ctx, cancel := context.WithTimeout(f.ctx, dialTimeout)
	up, err := f.hostDial(ctx, network, addr)
	cancel()
	if err != nil {
		c.Close()
		if f.ctx.Err() == nil {
			f.st.fail(dialError(f.sp.destLabel(), "this host", err))
		}
		return
	}
	relay(f.ctx, c, up)
}

// ---- accepting ----------------------------------------------------------------------------------------------------

func (f *forward) acceptLoop(ln net.Listener, handle func(*clientConn)) {
	var delay time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if f.ctx.Err() != nil || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
				return
			}
			// Transient failure (EMFILE, aborted handshake, channel accept error): back off briefly.
			delay = min(max(delay*2, 5*time.Millisecond), time.Second)
			f.log.Debug("tunnel: accept failed", "err", err)
			select {
			case <-f.ctx.Done():
				return
			case <-time.After(delay):
			}
			continue
		}
		delay = 0
		f.accept(c, handle)
	}
}

func (f *forward) accept(c net.Conn, handle func(*clientConn)) {
	if !f.clientAllowed(c.RemoteAddr()) {
		c.Close()
		f.st.fail(fmt.Sprintf("rejected a connection from %s (not in the allowed clients list)", c.RemoteAddr()))
		return
	}
	cc := &clientConn{Conn: c}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		c.Close()
		return
	}
	if f.sp.maxConns > 0 && int(f.st.active.Load()) >= f.sp.maxConns {
		f.mu.Unlock()
		c.Close()
		f.st.fail(fmt.Sprintf("rejected a connection: the limit of %d concurrent connections is reached", f.sp.maxConns))
		return
	}
	f.conns[cc] = struct{}{}
	f.wg.Add(1)
	f.mu.Unlock()
	f.st.opened()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				f.log.Error("tunnel: connection handler panicked", "panic", r)
			}
			cc.Close()
			f.untrack(cc)
			f.st.closed()
			f.wg.Done()
		}()
		handle(cc)
	}()
}

// clientAllowed applies options.allowFrom to TCP clients.
func (f *forward) clientAllowed(a net.Addr) bool {
	if len(f.sp.allowFrom) == 0 {
		return true
	}
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return true // Unix socket clients are governed by the socket file permissions
	}
	ip, ok := netip.AddrFromSlice(ta.IP)
	if !ok {
		return false
	}
	ip = ip.Unmap()
	for _, p := range f.sp.allowFrom {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// close stops listening, tears down every connection and waits (bounded by closeWait) for the handlers to finish and
// for the SSH server to cancel a remote listener — so a restart on a healthy link binds the same remote port again.
func (f *forward) close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	ln, rln := f.ln, f.rln
	conns := make([]net.Conn, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.mu.Unlock()
	f.cancel()
	if ln != nil {
		ln.Close()
	}
	var cancelled <-chan struct{}
	if rln != nil {
		cancelled = rln.closeAsync()
	}
	for _, c := range conns {
		c.Close()
	}
	if f.proxy != nil {
		f.proxy.close()
	}
	deadline := time.Now().Add(closeWait)
	waitTimeout(&f.wg, time.Until(deadline))
	if cancelled != nil {
		select {
		case <-cancelled:
		case <-time.After(time.Until(deadline)):
		}
	}
}

// waitTimeout waits for wg, giving up after d (a wedged remote write must not block a stop forever).
func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
