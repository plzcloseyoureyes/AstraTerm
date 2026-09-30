package webproxy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/netguard"
	"github.com/plzcloseyoureyes/astraterm/internal/sshx"
)

// Spec is what a client asks for (POST /api/webproxy). Exactly one of ConnectionID / SessionID / TunnelID selects the
// route (none = directly from the AstraTerm host). URL is a shorthand for Scheme + Host + Port + Path.
type Spec struct {
	ConnectionID string `json:"connectionId,omitempty"`
	SessionID    string `json:"sessionId,omitempty"`
	TunnelID     string `json:"tunnelId,omitempty"`
	URL          string `json:"url,omitempty"`
	Scheme       string `json:"scheme,omitempty"`
	Host         string `json:"host,omitempty"`
	Port         int    `json:"port,omitempty"`
	Path         string `json:"path,omitempty"`
	InsecureTLS  *bool  `json:"insecureTls,omitempty"`
	Title        string `json:"title,omitempty"`
}

// Target is the upstream origin (plus the initial path).
type Target struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Path   string `json:"path"`
}

func defaultPortFor(scheme string) int {
	if scheme == "https" {
		return 443
	}
	return 80
}

// HostPort is host:port for dialing (IPv6 literals bracketed).
func (t Target) HostPort() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

// authority is the Host header / URL authority (default port omitted).
func (t Target) authority() string {
	if t.Port == defaultPortFor(t.Scheme) {
		if strings.Contains(t.Host, ":") {
			return "[" + t.Host + "]"
		}
		return t.Host
	}
	return t.HostPort()
}

// Origin is scheme://authority.
func (t Target) Origin() string { return t.Scheme + "://" + t.authority() }

// String is the full URL of the initial page.
func (t Target) String() string { return t.Origin() + t.Path }

// Via describes the route to the upstream.
type Via struct {
	Kind  string `json:"kind"` // direct | ssh | session | tunnel | web
	ID    string `json:"id,omitempty"`
	Label string `json:"label"`
}

// route is a resolved spec: the upstream, how to reach it, and presentation details.
type route struct {
	target   Target
	via      Via
	insecure bool
	title    string
	auth     string // "Basic …" Authorization header for saved web sessions with credentials
	dial     func(ctx context.Context) (net.Conn, error)
}

var errInvalidURL = httpx.BadRequest("enter a web address such as http://localhost:8080/ (http or https)")

// parseURL splits a user-entered address into scheme, host, port and path. A missing scheme means http (https for
// port 443).
func parseURL(raw string) (Target, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Target{}, errInvalidURL
	}
	hadScheme := strings.Contains(raw, "://")
	if !hadScheme {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return Target{}, errInvalidURL
	}
	t := Target{Scheme: strings.ToLower(u.Scheme), Host: u.Hostname()}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return Target{}, errInvalidURL
		}
		t.Port = n
	}
	t.Path = u.EscapedPath()
	if u.RawQuery != "" {
		t.Path += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		t.Path += "#" + u.EscapedFragment()
	}
	if !hadScheme && t.Port == 443 {
		t.Scheme = "https"
	}
	return t, nil
}

// normalize validates and completes a target.
func (t *Target) normalize() error {
	t.Scheme = strings.ToLower(strings.TrimSpace(t.Scheme))
	if t.Scheme == "" {
		t.Scheme = "http"
		if t.Port == 443 {
			t.Scheme = "https"
		}
	}
	if t.Scheme != "http" && t.Scheme != "https" {
		return httpx.BadRequest("the web proxy supports http and https only")
	}
	t.Host = strings.Trim(strings.TrimSpace(t.Host), "[]")
	if err := validHost(t.Host); err != nil {
		return err
	}
	if t.Port == 0 {
		t.Port = defaultPortFor(t.Scheme)
	}
	if t.Port < 1 || t.Port > 65535 {
		return httpx.BadRequest("port must be between 1 and 65535")
	}
	p, err := cleanTargetPath(t.Path)
	if err != nil {
		return err
	}
	t.Path = p
	return nil
}

func validHost(h string) error {
	if h == "" {
		return httpx.BadRequest("host is required")
	}
	if len(h) > 253 {
		return httpx.BadRequest("host name is too long")
	}
	if ip := net.ParseIP(h); ip != nil {
		return nil
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_') {
			return httpx.BadRequest(fmt.Sprintf("invalid host name %q", h))
		}
	}
	return nil
}

// cleanTargetPath returns an absolute, control-character free request path (with optional query / fragment).
func cleanTargetPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/", nil
	}
	if len(p) > 4096 {
		return "", httpx.BadRequest("path is too long")
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f {
			return "", httpx.BadRequest("path contains control characters")
		}
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if strings.HasPrefix(p, "//") {
		return "", httpx.BadRequest("path must not start with //")
	}
	return p, nil
}

// resolve turns a spec into a route for user. It checks visibility of the referenced connection / session / tunnel
// and applies the server-mode destination policy to direct targets (advisory check here; authoritative at dial time).
func (s *Service) resolve(ctx context.Context, user *model.User, sp Spec) (*route, error) {
	n := 0
	for _, id := range []string{sp.ConnectionID, sp.SessionID, sp.TunnelID} {
		if strings.TrimSpace(id) != "" {
			n++
		}
	}
	if n > 1 {
		return nil, httpx.BadRequest("give only one of connectionId, sessionId and tunnelId")
	}
	var t Target
	if strings.TrimSpace(sp.URL) != "" {
		var err error
		if t, err = parseURL(sp.URL); err != nil {
			return nil, err
		}
	}
	if sp.Scheme != "" {
		t.Scheme = sp.Scheme
	}
	if sp.Host != "" {
		t.Host = sp.Host
	}
	if sp.Port != 0 {
		t.Port = sp.Port
	}
	if sp.Path != "" {
		t.Path = sp.Path
	}
	rt := &route{}
	if sp.InsecureTLS != nil {
		rt.insecure = *sp.InsecureTLS
	}
	pool := s.pool()

	switch {
	case sp.SessionID != "":
		sess := s.sessionInfo(sp.SessionID, user)
		if sess == nil {
			return nil, httpx.NotFound("SSH session not found")
		}
		if sess.Protocol != model.ProtoSSH {
			return nil, httpx.BadRequest("web services can be opened through SSH sessions only")
		}
		if t.Host == "" {
			t.Host = "localhost"
		}
		rt.via = Via{Kind: "session", ID: sp.SessionID, Label: sess.Title}
		sid := sp.SessionID
		rt.dial = func(ctx context.Context) (net.Conn, error) {
			c, rel, err := pool.ForSession(ctx, s.ownerOf(ctx, user), sid)
			if err != nil {
				if errors.Is(err, httpx.ErrNotFound) {
					return nil, &dialError{code: "session_gone", msg: "the SSH session is closed", err: err}
				}
				return nil, &dialError{code: "session_disconnected", msg: "the SSH session is not connected", err: err}
			}
			return sshDial(ctx, c, rel, rt.target.HostPort())
		}

	case sp.TunnelID != "":
		tun, err := s.d.Store.Tunnels.Get(ctx, sp.TunnelID)
		if err != nil || tun.OwnerID != user.ID {
			return nil, httpx.NotFound("tunnel not found")
		}
		conn, _, err := s.d.ResolveConnection(ctx, user, tun.ConnectionID)
		if err != nil {
			return nil, err
		}
		if t.Host == "" {
			t.Host = tun.DestHost
		}
		if t.Host == "" {
			t.Host = "localhost"
		}
		if t.Port == 0 {
			t.Port = tun.DestPort
		}
		label := tun.Name
		if label == "" {
			label = conn.Name
		}
		rt.via = Via{Kind: "tunnel", ID: tun.ID, Label: label}
		connID := tun.ConnectionID
		rt.dial = func(ctx context.Context) (net.Conn, error) {
			c, rel, err := pool.Get(ctx, s.ownerOf(ctx, user), connID)
			if err != nil {
				return nil, err
			}
			return sshDial(ctx, c, rel, rt.target.HostPort())
		}

	case sp.ConnectionID != "":
		conn, secrets, err := s.d.ResolveConnection(ctx, user, sp.ConnectionID)
		if err != nil {
			return nil, err
		}
		switch conn.Protocol {
		case model.ProtoSSH, model.ProtoSFTP, model.ProtoMosh:
			if t.Host == "" {
				t.Host = "localhost"
			}
			rt.via = Via{Kind: "ssh", ID: conn.ID, Label: conn.Name}
			connID := conn.ID
			rt.dial = func(ctx context.Context) (net.Conn, error) {
				c, rel, err := pool.Get(ctx, s.ownerOf(ctx, user), connID)
				if err != nil {
					return nil, err
				}
				return sshDial(ctx, c, rel, rt.target.HostPort())
			}
		case model.ProtoWeb:
			saved, err := parseURL(conn.Options.String("url", ""))
			if err != nil {
				if conn.Host == "" {
					return nil, httpx.BadRequest(fmt.Sprintf("the web session %q has no valid URL", conn.Name))
				}
				saved = Target{Host: conn.Host, Port: conn.Port}
			}
			if t.Host == "" {
				t.Host, t.Port = saved.Host, saved.Port
				if t.Scheme == "" {
					t.Scheme = saved.Scheme
				}
			}
			if t.Scheme == "" {
				t.Scheme = saved.Scheme
			}
			if t.Path == "" {
				t.Path = saved.Path
			}
			if sp.InsecureTLS == nil {
				rt.insecure = conn.Options.Bool("insecureTls", false)
			}
			if conn.Username != "" && conn.Options.Bool("basicAuth", true) {
				rt.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(conn.Username+":"+secrets[model.SecretPassword]))
			}
			rt.title = conn.Name
			label := "AstraTerm host"
			if via := conn.Options.String("sshTunnelVia", ""); via != "" {
				label = "SSH gateway"
				if gw, err := s.d.Store.Connections.Get(ctx, via); err == nil {
					label = gw.Name
				}
			} else if len(conn.Options.Strings("jumpHosts")) > 0 {
				label = "jump hosts"
			}
			rt.via = Via{Kind: "web", ID: conn.ID, Label: label}
			connID := conn.ID
			rt.dial = func(ctx context.Context) (net.Conn, error) {
				u := s.ownerOf(ctx, user)
				c, sec, err := s.d.ResolveConnection(ctx, u, connID)
				if err != nil {
					return nil, err
				}
				d, err := pool.Dialer(ctx, u, c, sec)
				if err != nil {
					return nil, err
				}
				nc, err := d.DialContext(ctx, "tcp", rt.target.HostPort())
				if err != nil {
					d.Close()
					return nil, err
				}
				return &releaseConn{Conn: nc, release: func() { d.Close() }}, nil
			}
		default:
			return nil, httpx.BadRequest(fmt.Sprintf("%q is not an SSH or web connection", conn.Name))
		}

	default:
		rt.via = Via{Kind: "direct", Label: "AstraTerm host"}
		rt.dial = func(ctx context.Context) (net.Conn, error) {
			u := s.ownerOf(ctx, user)
			conn := &model.Connection{Protocol: model.ProtoWeb, Host: rt.target.Host, Port: rt.target.Port, Options: model.Options{}}
			d, err := pool.Dialer(ctx, u, conn, nil)
			if err != nil {
				return nil, err
			}
			nc, err := d.DialContext(ctx, "tcp", rt.target.HostPort())
			if err != nil {
				d.Close()
				return nil, err
			}
			return &releaseConn{Conn: nc, release: func() { d.Close() }}, nil
		}
	}

	if err := t.normalize(); err != nil {
		return nil, err
	}
	if rt.via.Kind == "direct" {
		// Advisory, DNS-free check so obviously refused targets fail at creation (the dial-time check is authoritative).
		if err := netguard.ForUser(s.d, user).CheckLiteral(t.Host, t.Port); err != nil {
			return nil, err
		}
	}
	rt.target = t
	if sp.Title != "" {
		rt.title = truncate(strings.TrimSpace(sp.Title), 120)
	}
	if rt.title == "" {
		rt.title = t.authority()
	}
	return rt, nil
}

// pool returns the SSH pool (nil-safe for tests without SSH).
func (s *Service) pool() *sshx.Pool {
	if s.c == nil {
		return nil
	}
	return s.c.SSH
}

type sessionMeta struct {
	Protocol model.Protocol
	Title    string
}

func (s *Service) sessionInfo(id string, user *model.User) *sessionMeta {
	if s.c == nil || s.c.Sessions == nil {
		return nil
	}
	sess := s.c.Sessions.Get(id)
	if sess == nil || sess.OwnerID != user.ID {
		return nil
	}
	return &sessionMeta{Protocol: sess.Protocol, Title: sess.Info().Title}
}

// ownerOf returns the proxy owner's current record when ctx carries a proxy (refreshed account), else user.
func (s *Service) ownerOf(ctx context.Context, user *model.User) *model.User {
	if p, ok := ctx.Value(proxyCtxKey{}).(*Proxy); ok && p != nil {
		return p.currentOwner()
	}
	return user
}

type proxyCtxKey struct{}

// sshDial opens a direct-tcpip channel through c; closing the stream releases the pooled client.
func sshDial(ctx context.Context, c *sshx.Client, rel func(), addr string) (net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	nc, err := c.DialContext(dctx, "tcp", addr)
	if err != nil {
		rel()
		return nil, &dialError{code: "upstream_unreachable", msg: fmt.Sprintf("the SSH server could not connect to %s: %s", addr, sshReason(err)), err: err}
	}
	return &releaseConn{Conn: nc, release: rel}, nil
}

// sshReason shortens x/crypto/ssh channel-open errors.
func sshReason(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "ssh: rejected: "); i >= 0 {
		msg = msg[i+len("ssh: rejected: "):]
	}
	return msg
}

// releaseConn calls release once when closed.
type releaseConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *releaseConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		if c.release != nil {
			c.release()
		}
	})
	return err
}

// CloseWrite half-closes when the underlying stream supports it.
func (c *releaseConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// dialError is a dial failure with a stable code and a user-facing message.
type dialError struct {
	code string
	msg  string
	err  error
}

func (e *dialError) Error() string { return e.msg }
func (e *dialError) Unwrap() error { return e.err }

// preflight dials the upstream once (bounded) so creation fails fast with a clear message.
func (s *Service) preflight(ctx context.Context, rt *route) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute) // SSH logins may wait for prompts
	defer cancel()
	c, err := rt.dial(ctx)
	if err != nil {
		return upstreamError(err, rt.target)
	}
	c.Close()
	return nil
}

// upstreamError converts a dial error into an API error (4xx so its message reaches the client).
func upstreamError(err error, t Target) error {
	if _, ok := errors.AsType[*httpx.HTTPError](err); ok {
		return err
	}
	code, msg := classifyDialErr(err, t)
	return httpx.NewError(422, code, msg)
}

// classifyDialErr returns a code and message for a failed upstream connection.
func classifyDialErr(err error, t Target) (code, msg string) {
	if de, ok := errors.AsType[*dialError](err); ok {
		return de.code, de.msg
	}
	if be, ok := netguard.IsBlocked(err); ok {
		return netguard.CodeBlocked, be.Error()
	}
	var ae interface{ ErrorCode() string }
	if errors.As(err, &ae) && ae.ErrorCode() == model.CodeLocked {
		return model.CodeLocked, "the vault is locked: unlock AstraTerm to use this connection"
	}
	s := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(s, "i/o timeout"):
		return "upstream_timeout", fmt.Sprintf("connecting to %s timed out", t.HostPort())
	case strings.Contains(s, "connection refused") || strings.Contains(s, "Connection refused"):
		return "upstream_refused", fmt.Sprintf("%s refused the connection (is the service running?)", t.HostPort())
	case strings.Contains(s, "no such host"):
		return "upstream_unreachable", fmt.Sprintf("the host name %q could not be resolved", t.Host)
	}
	return "upstream_unreachable", fmt.Sprintf("cannot connect to %s: %s", t.HostPort(), s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
