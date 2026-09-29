package tunnel

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	socks5 "github.com/things-go/go-socks5"
	"github.com/things-go/go-socks5/bufferpool"
)

// Dynamic forwards (TUN-4/TUN-5) serve one port speaking SOCKS5 (go-socks5, optional username/password), SOCKS4/4a
// (first-byte sniffing; no authentication) and — unless disabled — HTTP proxying (CONNECT and absolute-URI
// requests) plus a PAC file at /proxy.pac. Host names are never resolved locally: they travel to the exit side
// (the SSH server for -D, the Termstead host for reverse dynamic forwards). UDP ASSOCIATE and BIND are refused.

// handshakeTimeout bounds the proxy negotiation of a client, in nanoseconds (atomic so tests can shorten it).
var handshakeTimeout atomic.Int64

func init() { handshakeTimeout.Store(int64(30 * time.Second)) }

// sharedBufPool is shared by every SOCKS5 server instance.
var sharedBufPool = bufferpool.NewPool(32 << 10)

type proxy struct {
	f    *forward
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	user string
	pass string

	// The HTTP side starts with the first HTTP client. mu guards it against a concurrent close: a server started
	// after close() would never be shut down.
	mu        sync.Mutex
	closed    bool
	httpLn    *chanListener
	httpSrv   *http.Server
	transport *http.Transport // set once with httpLn; handlers of httpSrv read it without mu
}

func newProxy(f *forward, dial func(ctx context.Context, network, addr string) (net.Conn, error)) *proxy {
	return &proxy{f: f, dial: dial, user: f.sp.socksUser, pass: f.sp.socksPass}
}

// dialTarget dials a proxy destination with the forward's timeout, recording failures.
func (p *proxy) dialTarget(ctx context.Context, addr string) (net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	c, err := p.dial(dctx, "tcp", addr)
	if err != nil && p.f.ctx.Err() == nil {
		via := "the SSH server"
		if p.f.sp.kind == kindRemoteDynamic {
			via = "this host"
		}
		p.f.st.fail(dialError(addr, via, err))
	}
	return c, err
}

// bufConn reads through a buffered reader (after protocol sniffing) and keeps half-close support. handshakeDone
// disarms the client's negotiation watchdog.
type bufConn struct {
	*clientConn
	r  *bufio.Reader
	wd *time.Timer
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// handshakeDone ends the negotiation phase: the client is no longer closed after handshakeTimeout.
func (c *bufConn) handshakeDone() { c.wd.Stop() }

// serve handles one client connection until it is done. A client must finish the proxy negotiation (SOCKS greeting,
// authentication and request, or the first HTTP request header) within handshakeTimeout. The watchdog closes the
// connection instead of relying on deadlines, which the SSH channels of a reverse proxy's clients do not support.
func (p *proxy) serve(c *clientConn) {
	wd := time.AfterFunc(time.Duration(handshakeTimeout.Load()), func() { _ = c.Close() })
	defer wd.Stop()
	br := bufio.NewReaderSize(c, 4096)
	first, err := br.Peek(1)
	if err != nil {
		c.Close()
		return
	}
	bc := &bufConn{clientConn: c, r: br, wd: wd}
	switch first[0] {
	case 0x05:
		p.serveSocks5(bc)
	case 0x04:
		p.serveSocks4(bc)
	default:
		if p.f.sp.httpProxy && first[0] >= 'A' && first[0] <= 'Z' {
			p.serveHTTP(bc)
			return
		}
		c.Close()
		p.f.st.fail("rejected a connection that is neither SOCKS nor HTTP")
	}
}

// ---- SOCKS5 -------------------------------------------------------------------------------------------------------

type socksLogger struct{ p *proxy }

func (l socksLogger) Errorf(format string, args ...any) {
	l.p.f.log.Debug("tunnel: socks5: " + fmt.Sprintf(format, args...))
}

// remoteResolver leaves host names unresolved so they are resolved on the exit side.
type remoteResolver struct{}

func (remoteResolver) Resolve(ctx context.Context, _ string) (context.Context, net.IP, error) {
	return ctx, nil, nil
}

// credentials checks SOCKS5 username/password in constant time, slowing down failures.
type credentials struct{ user, pass string }

func (c credentials) Valid(user, password, _ string) bool {
	ok := constantTimeEq(user, c.user) & constantTimeEq(password, c.pass)
	if ok != 1 {
		time.Sleep(300 * time.Millisecond)
	}
	return ok == 1
}

func constantTimeEq(a, b string) int { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) }

func (p *proxy) serveSocks5(c *bufConn) {
	opts := []socks5.Option{
		socks5.WithBufferPool(sharedBufPool),
		socks5.WithLogger(socksLogger{p}),
		socks5.WithResolver(remoteResolver{}),
		socks5.WithRule(&socks5.PermitCommand{EnableConnect: true}),
		socks5.WithDialAndRequest(func(ctx context.Context, _, addr string, _ *socks5.Request) (net.Conn, error) {
			c.handshakeDone()
			return p.dialTarget(p.f.ctx, addr)
		}),
	}
	if p.user != "" {
		opts = append(opts, socks5.WithCredential(credentials{p.user, p.pass}))
	}
	srv := socks5.NewServer(opts...)
	// go-socks5 wraps authentication failures as "failed to authenticate: …".
	if err := srv.ServeConn(c); err != nil && strings.Contains(err.Error(), "failed to authenticate") {
		p.f.st.fail("rejected a SOCKS5 client: authentication failed")
	}
}

// ---- SOCKS4 / SOCKS4a ---------------------------------------------------------------------------------------------

func (p *proxy) serveSocks4(c *bufConn) {
	defer c.Close()
	var hdr [8]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return
	}
	reply := func(code byte) { _, _ = c.Write([]byte{0, code, 0, 0, 0, 0, 0, 0}) }
	cmd, port := hdr[1], int(binary.BigEndian.Uint16(hdr[2:4]))
	ip := net.IPv4(hdr[4], hdr[5], hdr[6], hdr[7])
	if _, err := readCString(c.r, 512); err != nil { // user id (unused)
		return
	}
	host := ip.String()
	if hdr[4] == 0 && hdr[5] == 0 && hdr[6] == 0 && hdr[7] != 0 { // SOCKS4a: the host name follows
		h, err := readCString(c.r, 255)
		if err != nil || h == "" {
			reply(0x5B)
			return
		}
		host = h
	}
	if p.user != "" {
		reply(0x5B)
		p.f.st.fail("rejected a SOCKS4 request: this proxy requires SOCKS5 username/password authentication")
		return
	}
	if cmd != 1 || port == 0 {
		reply(0x5B)
		return
	}
	c.handshakeDone()
	up, err := p.dialTarget(p.f.ctx, net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		reply(0x5B)
		return
	}
	reply(0x5A)
	relay(p.f.ctx, c, up)
}

func readCString(r *bufio.Reader, max int) (string, error) {
	var b []byte
	for {
		ch, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if ch == 0 {
			return string(b), nil
		}
		if len(b) >= max {
			return "", errors.New("socks4: field too long")
		}
		b = append(b, ch)
	}
}

// ---- HTTP proxy ---------------------------------------------------------------------------------------------------

// serveHTTP hands the connection to the proxy's HTTP server and blocks until the server is done with it (the caller
// closes the connection when this returns).
func (p *proxy) serveHTTP(c *bufConn) {
	ln := p.httpListener()
	if ln == nil {
		return // the forward is closing
	}
	hc := &httpConn{bufConn: c, done: make(chan struct{})}
	if !ln.push(hc) {
		return
	}
	select {
	case <-hc.done:
	case <-p.f.ctx.Done():
	}
}

// httpConn signals when the HTTP server closes a connection.
type httpConn struct {
	*bufConn
	done chan struct{}
	once sync.Once
}

func (c *httpConn) Close() error {
	err := c.bufConn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}

// httpListener returns the listener of the proxy's HTTP server, starting the server on first use; nil once the proxy
// is closed.
func (p *proxy) httpListener() *chanListener {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	if p.httpLn == nil {
		p.initHTTPLocked()
	}
	return p.httpLn
}

func (p *proxy) initHTTPLocked() {
	p.transport = &http.Transport{
		Proxy:                 nil,
		DialContext:           func(ctx context.Context, _, addr string) (net.Conn, error) { return p.dialTarget(ctx, addr) },
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
	}
	p.httpLn = newChanListener()
	p.httpSrv = &http.Server{
		Handler:           http.HandlerFunc(p.handleHTTP),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
		BaseContext:       func(net.Listener) context.Context { return p.f.ctx },
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, httpConnKey{}, c)
		},
	}
	srv, ln := p.httpSrv, p.httpLn
	go func() { _ = srv.Serve(ln) }()
}

// httpConnKey carries the *httpConn of a request (http.Server.ConnContext).
type httpConnKey struct{}

func (p *proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	// A complete request header ends the negotiation phase of the connection (the HTTP server's own timeouts
	// apply from here on, where the transport supports deadlines).
	if hc, ok := r.Context().Value(httpConnKey{}).(*httpConn); ok {
		hc.handshakeDone()
	}
	switch {
	case r.Method == http.MethodConnect:
		if p.authorize(w, r) {
			p.handleConnect(w, r)
		}
	case r.URL.IsAbs() && r.URL.Host != "":
		if p.authorize(w, r) {
			p.forwardHTTP(w, r)
		}
	case r.Method == http.MethodGet && (r.URL.Path == "/proxy.pac" || r.URL.Path == "/wpad.dat"):
		p.servePAC(w, r)
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "This is a Termstead SOCKS/HTTP proxy. Configure it as the proxy of your client, or use /proxy.pac.\n")
	}
}

// authorize checks Proxy-Authorization (Basic) when the proxy has credentials.
func (p *proxy) authorize(w http.ResponseWriter, r *http.Request) bool {
	if p.user == "" {
		return true
	}
	if u, pw, ok := parseProxyAuth(r.Header.Get("Proxy-Authorization")); ok &&
		constantTimeEq(u, p.user)&constantTimeEq(pw, p.pass) == 1 {
		return true
	}
	time.Sleep(300 * time.Millisecond)
	w.Header().Set("Proxy-Authenticate", `Basic realm="Termstead tunnel", charset="UTF-8"`)
	http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
	return false
}

func parseProxyAuth(h string) (user, pass string, ok bool) {
	const prefix = "basic "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", "", false
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	return strings.Cut(string(b), ":")
}

func (p *proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if _, port, err := net.SplitHostPort(target); err != nil || port == "" {
		http.Error(w, "CONNECT needs host:port", http.StatusBadRequest)
		return
	}
	up, err := p.dialTarget(r.Context(), target)
	if err != nil {
		http.Error(w, "Termstead proxy: "+dialError(target, "the tunnel", err), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	_ = conn.SetDeadline(time.Time{})
	if _, err := rw.WriteString("HTTP/1.1 200 Connection established\r\n\r\n"); err != nil || rw.Flush() != nil {
		conn.Close()
		up.Close()
		return
	}
	// Bytes the client pipelined after the CONNECT header (e.g. a TLS ClientHello) sit in the server's buffer.
	if n := rw.Reader.Buffered(); n > 0 {
		b, _ := rw.Reader.Peek(n)
		if _, err := up.Write(b); err != nil {
			conn.Close()
			up.Close()
			return
		}
	}
	relay(p.f.ctx, conn, up)
}

func (p *proxy) forwardHTTP(w http.ResponseWriter, r *http.Request) {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			u := *pr.In.URL
			pr.Out.URL = &u
			pr.Out.Host = pr.In.Host
		},
		Transport:     p.transport,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil {
				http.Error(w, "Termstead proxy: "+dialError(r.URL.Host, "the tunnel", err), http.StatusBadGateway)
			}
		},
	}
	rp.ServeHTTP(w, r)
}

// servePAC returns a proxy auto-config file pointing at this proxy (as reached by the client).
func (p *proxy) servePAC(w http.ResponseWriter, r *http.Request) {
	hp := r.Host
	if !validHostPort(hp) {
		hp = loadStr(&p.f.st.localAddr)
		if hp == "" {
			hp = loadStr(&p.f.st.remoteAdr)
		}
		if h, port, err := net.SplitHostPort(hp); err == nil && (h == "*" || h == "") {
			hp = net.JoinHostPort("127.0.0.1", port)
		}
		if !validHostPort(hp) {
			http.Error(w, "cannot determine the proxy address; request /proxy.pac through host:port", http.StatusBadRequest)
			return
		}
	}
	w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprintf(w, "function FindProxyForURL(url, host) {\n  return %q;\n}\n", pacProxies(hp, p.user != ""))
}

// pacProxies is the proxy list a PAC file returns for this proxy at hp (host:port). The PAC is served on the HTTP
// side, so the HTTP proxy is always available. Browsers cannot authenticate to SOCKS proxies, so a proxy with
// credentials is offered as HTTP proxy first (browsers ask for the password); without credentials SOCKS5 comes
// first (host names travel to the exit side), then SOCKS4 and HTTP. There is deliberately no DIRECT fallback:
// traffic meant for the tunnel must not silently bypass it.
func pacProxies(hp string, auth bool) string {
	if auth {
		return fmt.Sprintf("PROXY %[1]s; SOCKS5 %[1]s", hp)
	}
	return fmt.Sprintf("SOCKS5 %[1]s; SOCKS %[1]s; PROXY %[1]s", hp)
}

// validHostPort accepts host:port strings made only of host-name / IP literal characters (safe to embed in the PAC).
func validHostPort(s string) bool {
	host, port, err := net.SplitHostPort(s)
	if err != nil || host == "" || port == "" {
		return false
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return false
	}
	for _, r := range host {
		if !(r == '.' || r == '-' || r == ':' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func (p *proxy) close() {
	p.mu.Lock()
	p.closed = true
	ln, srv, tr := p.httpLn, p.httpSrv, p.transport
	p.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	if srv != nil {
		_ = srv.Close()
	}
	if tr != nil {
		tr.CloseIdleConnections()
	}
}

// dropIdleUpstreams closes the HTTP proxy's idle pooled destination connections (after the SSH link they ran over
// ended: reusing them would fail requests).
func (p *proxy) dropIdleUpstreams() {
	p.mu.Lock()
	tr := p.transport
	p.mu.Unlock()
	if tr != nil {
		tr.CloseIdleConnections()
	}
}

// chanListener feeds sniffed HTTP connections to the proxy's http.Server.
type chanListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newChanListener() *chanListener {
	return &chanListener{ch: make(chan net.Conn), done: make(chan struct{})}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) push(c net.Conn) bool {
	select {
	case l.ch <- c:
		return true
	case <-l.done:
		return false
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4zero} }
