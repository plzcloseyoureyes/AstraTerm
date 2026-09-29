package sshx

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"

	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/netguard"
	"github.com/nexterm/nexterm/internal/term"
)

// Dialer reaches a connection's host for any protocol (telnet, raw, VNC, RDP, FTP, ...) through the connection's
// options.proxy / options.proxyCommand, options.jumpHosts chain, or options.sshTunnelVia SSH gateway (RESEARCH
// decision 3, "one Dialer abstraction"). Gateway connections stay referenced until Close.
type Dialer struct {
	rt     *route
	conn   *model.Connection
	mu     sync.Mutex
	closed bool
}

// Dialer builds the route for conn (secrets carries proxyPassword and is used to authenticate gateways). Prompts
// raised while connecting gateways are associated with the runtime session in ctx (term.WithSession), if any.
func (p *Pool) Dialer(ctx context.Context, user *model.User, conn *model.Connection, secrets map[string]string) (*Dialer, error) {
	if conn == nil {
		return nil, errors.New("sshx: nil connection")
	}
	if secrets == nil {
		secrets = map[string]string{}
	}
	rt, err := p.buildRoute(ctx, user, conn, secrets, term.SessionFromContext(ctx), false)
	if err != nil {
		return nil, err
	}
	return &Dialer{rt: rt, conn: conn.Clone()}, nil
}

// DialContext opens a stream to addr along the route ("tcp" only when the route goes through a proxy or gateway).
func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return nil, net.ErrClosed
	}
	c, err := d.rt.dial(ctx, network, addr)
	if err != nil {
		if be, ok := netguard.IsBlocked(err); ok {
			// Refused by the destination policy (SEC-7): permanent, so sessions do not auto-reconnect into it.
			return nil, term.Permanent(be)
		}
		return nil, err
	}
	return c, nil
}

// Dial connects to the connection's own host:port.
func (d *Dialer) Dial(ctx context.Context) (net.Conn, error) {
	port := d.conn.Port
	if port == 0 {
		port = model.DefaultPort(d.conn.Protocol)
	}
	return d.DialContext(ctx, "tcp", net.JoinHostPort(d.conn.Host, strconv.Itoa(port)))
}

// Via returns the SSH gateway client of the route (nil for direct / proxied routes).
func (d *Dialer) Via() *Client { return d.rt.via }

// Close releases the gateway connections held by the dialer.
func (d *Dialer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed {
		d.closed = true
		d.rt.release()
	}
	return nil
}

// DialConnection dials conn's host:port along its route; closing the returned net.Conn also releases the gateways.
func (p *Pool) DialConnection(ctx context.Context, user *model.User, conn *model.Connection, secrets map[string]string) (net.Conn, error) {
	d, err := p.Dialer(ctx, user, conn, secrets)
	if err != nil {
		return nil, err
	}
	c, err := d.Dial(ctx)
	if err != nil {
		d.Close()
		return nil, err
	}
	return &dialedConn{Conn: c, d: d}, nil
}

type dialedConn struct {
	net.Conn
	d    *Dialer
	once sync.Once
}

func (c *dialedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.d.Close() })
	return err
}

// CloseWrite half-closes the stream when the underlying connection supports it.
func (c *dialedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
