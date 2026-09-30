// Package rawtcp implements the "raw" terminal protocol (PROTO-9, RESEARCH §3.6): a byte stream to host:port over
// TCP, TLS (with an insecure toggle) or UDP, selected by options.transport. The line discipline (local echo and
// Enter translation, see package linedisc) runs server-side. TCP/TLS connections are dialed through the generic sshx
// Dialer so proxies and SSH gateways apply (and the user's destination guard, internal/netguard); UDP is a direct
// datagram socket vetted by the same guard. There is no negotiation.
package rawtcp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
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

// Mount registers the "raw" terminal protocol.
func Mount(d *app.Deps, c *core.Core) error {
	term.RegisterProtocol(string(model.ProtoRaw), func(ctx context.Context, req term.OpenRequest) (term.Backend, error) {
		return open(ctx, d, c, req)
	})
	return nil
}

func open(ctx context.Context, d *app.Deps, c *core.Core, req term.OpenRequest) (term.Backend, error) {
	conn := req.Connection
	if conn == nil {
		return nil, term.Permanent(errors.New("raw: missing connection"))
	}
	if strings.TrimSpace(conn.Host) == "" {
		return nil, term.Permanent(errors.New("raw: host is required"))
	}
	if conn.Port <= 0 || conn.Port > 65535 {
		return nil, term.Permanent(errors.New("raw: a port (1-65535) is required"))
	}
	o := conn.Options
	transport := strings.ToLower(strings.TrimSpace(o.String("transport", "tcp")))
	addr := net.JoinHostPort(conn.Host, strconv.Itoa(conn.Port))
	if (transport == "" || transport == "tcp" || transport == "tls") && (c == nil || c.SSH == nil) {
		return nil, term.Permanent(errors.New("raw: no dialer available"))
	}

	var nc net.Conn
	var err error
	switch transport {
	case "", "tcp":
		nc, err = c.SSH.DialConnection(ctx, req.User, conn, req.Secrets)
	case "tls":
		nc, err = c.SSH.DialConnection(ctx, req.User, conn, req.Secrets)
		if err == nil {
			tconn := tls.Client(nc, &tls.Config{
				ServerName:         conn.Host,
				InsecureSkipVerify: o.Bool("insecureTls") || o.Bool("insecure"),
				// Raw TLS mostly targets devices and legacy services; TLS 1.0/1.1 still beats clear text.
				MinVersion: tls.VersionTLS10,
			})
			hctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			herr := tconn.HandshakeContext(hctx)
			cancel()
			if herr != nil {
				nc.Close()
				if _, ok := errors.AsType[*tls.CertificateVerificationError](herr); ok {
					return nil, term.Permanent(fmt.Errorf("raw: TLS certificate of %s rejected: %w", addr, herr))
				}
				return nil, fmt.Errorf("raw: TLS handshake with %s failed: %w", addr, herr)
			}
			nc = tconn
		}
	case "udp":
		if routed(conn) {
			return nil, term.Permanent(errors.New("raw: UDP cannot be routed through a proxy or SSH gateway"))
		}
		// A direct socket of the AstraTerm host: vetted by the owner's destination guard (SEC-7) on the concrete
		// address, after DNS (nil guard = unrestricted: desktop mode, administrators).
		nc, err = netguard.ForUser(d, req.User).DialContext(ctx, "udp", addr, 15*time.Second)
		if err == nil {
			nc = &udpConn{Conn: nc}
		}
	default:
		return nil, term.Permanent(fmt.Errorf("raw: unsupported transport %q (use tcp, tls or udp)", transport))
	}
	if err != nil {
		if be, ok := netguard.IsBlocked(err); ok {
			// Refused by the destination policy: permanent (no auto-reconnect loop), 403 destination_blocked.
			return nil, term.Permanent(be)
		}
		return nil, fmt.Errorf("raw: connect to %s: %w", addr, err)
	}
	return newBackend(nc, linedisc.ParseEnding(o.String("lineEnding", ""), linedisc.CRLF), o.Bool("localEcho")), nil
}

// routed reports whether conn's options send its traffic through a proxy or an SSH gateway (UDP cannot follow).
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

// udpConn is a connected UDP socket whose reads survive ICMP "port unreachable" (ECONNREFUSED; WSAECONNRESET on
// Windows): datagram services often start listening after the first probe, so a refusal must not end the session.
type udpConn struct{ net.Conn }

func (u *udpConn) Read(p []byte) (int, error) {
	for {
		n, err := u.Conn.Read(p)
		if err != nil && n == 0 && (errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET)) {
			continue
		}
		return n, err
	}
}

type backend struct {
	conn     net.Conn
	out      *linedisc.Reader
	lineMode linedisc.Ending

	writeMu   sync.Mutex
	localEcho bool
	echo      linedisc.Echo // guarded by writeMu

	closeOnce sync.Once
}

func newBackend(nc net.Conn, ending linedisc.Ending, localEcho bool) *backend {
	return &backend{conn: nc, out: linedisc.NewReader(nc), lineMode: ending, localEcho: localEcho}
}

// Read returns remote data merged with local echo; io.EOF when the peer closed or the session closed the backend.
func (b *backend) Read(p []byte) (int, error) { return b.out.Read(p) }

func (b *backend) Write(p []byte) (int, error) {
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

// Resize is a no-op: a raw socket has no window size.
func (b *backend) Resize(int, int) error { return nil }

func (b *backend) Close() error {
	var err error
	b.closeOnce.Do(func() {
		b.out.Close()
		err = b.conn.Close()
	})
	return err
}

var _ term.Backend = (*backend)(nil)
