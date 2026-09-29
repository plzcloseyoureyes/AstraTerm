package rdp

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// forwarder listens on a local address and connects every accepted stream to a connection's destination along its
// route (options.sshTunnelVia / jumpHosts / proxy). guacd and native RDP clients dial the target themselves, so
// connections that need AstraTerm's generic Dialer are handed to them as such a local endpoint.
type forwarder struct {
	h       *handler
	ln      net.Listener
	user    *model.User
	conn    *model.Connection
	secrets map[string]string
	sess    *term.Session

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	accepted int
	active   int
	lastUse  time.Time
	maxConns int
	idle     time.Duration
	wg       sync.WaitGroup
	closed   chan struct{}
	once     sync.Once
}

// startForwarder listens on bindIP:0. maxConns bounds the accepted streams (0 = unlimited); idle > 0 closes the
// forwarder after that long without an active stream.
func (h *handler) startForwarder(ctx context.Context, bindIP string, user *model.User, conn *model.Connection,
	secrets map[string]string, sess *term.Session, maxConns int, idle time.Duration) (*forwarder, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(bindIP, "0"))
	if err != nil {
		return nil, err
	}
	fctx, cancel := context.WithCancel(ctx)
	f := &forwarder{
		h: h, ln: ln, user: user, conn: conn.Clone(), secrets: secrets, sess: sess,
		ctx: fctx, cancel: cancel, maxConns: maxConns, idle: idle, lastUse: time.Now(), closed: make(chan struct{}),
	}
	go f.acceptLoop()
	go func() {
		<-fctx.Done()
		f.Close()
	}()
	if idle > 0 {
		go f.idleLoop()
	}
	return f, nil
}

// Addr is the listening address.
func (f *forwarder) Addr() *net.TCPAddr { return f.ln.Addr().(*net.TCPAddr) }

// Close stops accepting and ends every stream.
func (f *forwarder) Close() {
	f.once.Do(func() {
		close(f.closed)
		f.cancel()
		_ = f.ln.Close()
	})
}

// Done is closed once the forwarder is closed.
func (f *forwarder) Done() <-chan struct{} { return f.closed }

func (f *forwarder) acceptLoop() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				f.h.log.Debug("rdp: forwarder accept failed", "err", err)
			}
			f.Close()
			return
		}
		f.mu.Lock()
		if f.maxConns > 0 && f.accepted >= f.maxConns {
			f.mu.Unlock()
			_ = c.Close()
			continue
		}
		f.accepted++
		f.active++
		f.mu.Unlock()
		f.wg.Add(1)
		go f.serve(c)
	}
}

func (f *forwarder) serve(c net.Conn) {
	defer f.wg.Done()
	defer func() {
		f.mu.Lock()
		f.active--
		f.lastUse = time.Now()
		f.mu.Unlock()
	}()
	defer c.Close()
	ctx := f.ctx
	if f.sess != nil {
		ctx = term.WithSession(ctx, f.sess)
	}
	remote, err := f.h.dialConnection(ctx, f.user, f.conn, f.secrets)
	if err != nil {
		f.h.log.Debug("rdp: forwarder dial failed", "host", f.conn.Host, "err", err)
		return
	}
	defer remote.Close()
	stop := context.AfterFunc(f.ctx, func() {
		_ = c.Close()
		_ = remote.Close()
	})
	defer stop()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(remote, c)
		closeWrite(remote)
		close(done)
	}()
	_, _ = io.Copy(c, remote)
	closeWrite(c)
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

func (f *forwarder) idleLoop() {
	t := time.NewTicker(time.Second * 5)
	defer t.Stop()
	for {
		select {
		case <-f.closed:
			return
		case <-t.C:
			f.mu.Lock()
			idle := f.active == 0 && time.Since(f.lastUse) > f.idle
			f.mu.Unlock()
			if idle {
				f.Close()
				return
			}
		}
	}
}

// forwardBindIP picks the local address a forwarder listens on for a peer that reaches AstraTerm at host: that
// address when it is an IP of this machine (e.g. the Docker bridge gateway), else loopback.
func forwardBindIP(host string) string {
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() {
		return "127.0.0.1"
	}
	if ip.IsLoopback() {
		return ip.String()
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return ip.String()
		}
	}
	return "127.0.0.1"
}
