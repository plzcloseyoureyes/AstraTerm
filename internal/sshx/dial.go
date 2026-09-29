package sshx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/netguard"
	"github.com/termstead/termstead/internal/term"
)

// Connection defaults (SPEC §5.3 ssh options).
const (
	DefaultConnectTimeout = 20 * time.Second
	DefaultKeepAlive      = 30 * time.Second
	keepAliveMaxMiss      = 3
	maxJumpHops           = 8
)

// dialOpts carries per-dial context.
type dialOpts struct {
	session *term.Session // runtime session the dial belongs to (prompts are associated with it)
	via     *Client       // gateway to dial through (jump chain); nil = conn's own route
	route   *route        // explicit route (first jump hop reached through the target's proxy)
	label   string        // human description for error messages ("jump host bastion")
}

// dial establishes a new authenticated client for conn along rt: port knock → TCP → SSH handshake with host-key
// verification and interactive authentication. On success the client takes over rt's gateway reference.
func (p *Pool) dial(ctx context.Context, user *model.User, conn *model.Connection, secrets map[string]string, rt *route, o dialOpts) (*Client, error) {
	conn = conn.Clone()
	if conn.Port == 0 {
		conn.Port = 22
	}
	if secrets == nil {
		secrets = map[string]string{}
	}
	opts := conn.Options
	timeout := connectTimeout(opts)
	addr := net.JoinHostPort(conn.Host, strconv.Itoa(conn.Port))

	if rt == nil {
		rt = &route{timeout: timeout, guard: p.guardFor(user)}
	}
	username := conn.Username
	if username == "" {
		username = secrets["username"]
	}
	af := &authFlow{p: p, ctx: ctx, user: user, conn: conn, secrets: secrets, session: o.session, label: o.label}
	if username == "" {
		// Ask before connecting so the server's login grace time does not run while the user types.
		u, err := af.askUsername()
		if err != nil {
			return nil, err
		}
		username = u
	}
	conn.Username = username
	af.username = username

	if o.session != nil {
		o.session.SetStatus(model.StateConnecting, "Connecting to "+addr+viaSuffix(rt))
	}
	if knocks := parseKnocks(opts); len(knocks) > 0 {
		p.portKnock(ctx, rt, conn.Host, knocks)
	}
	raw, err := rt.dial(ctx, "tcp", addr)
	if err != nil {
		if be, ok := netguard.IsBlocked(err); ok {
			// Refused by the destination policy (SEC-7): permanent, never auto-retried.
			// (Jump-hop errors get their label from buildRoute.)
			return nil, term.Permanent(be)
		}
		return nil, fmt.Errorf("connect to %s%s: %w", addr, viaSuffix(rt), friendlyNetError(err))
	}
	dc := newDeadlineConn(raw, timeout)
	af.dc = dc
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })

	hk := &hostKeyChecker{p: p, ctx: ctx, user: user, host: strings.ToLower(conn.Host), port: conn.Port, conn: conn,
		session: o.session, dc: dc, label: o.label}
	known, _ := hk.known()
	cfg := &ssh.ClientConfig{
		User:              username,
		HostKeyCallback:   hk.check,
		HostKeyAlgorithms: hk.algorithms(known, opts), // hostKeyAlgorithms + certificates first for CA-covered hosts
		AuthCallback:      af.next,
		BannerCallback: func(msg string) error {
			if o.session != nil {
				o.session.Banner(msg)
			}
			return nil
		},
		Timeout: timeout,
	}
	cfg.KeyExchanges, cfg.Ciphers, cfg.MACs = algorithmLists(opts)
	if v := strings.TrimSpace(opts.String("clientVersion", "")); strings.HasPrefix(v, "SSH-2.0-") && len(v) < 200 {
		cfg.ClientVersion = v
	}

	sc, chans, reqs, err := ssh.NewClientConn(dc, addr, cfg)
	aborted := !stop()
	af.closeAgent()
	if err != nil {
		raw.Close()
		if aborted || ctx.Err() != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, context.Canceled
		}
		return nil, p.classifyDialError(err, addr, o.label)
	}
	dc.clearDeadline()
	client := newClient(p, ssh.NewClient(sc, chans, reqs), conn, user, secrets)
	client.info.HostKeyFingerprint = hk.fingerprint
	client.parent = rt.handOver()

	af.onSuccess()
	if o.session != nil {
		o.session.SetStatus(model.StateConnecting, "Connected to "+addr+", opening session")
	}
	ka := DefaultKeepAlive
	if opts.Has("keepAliveSec") {
		ka = time.Duration(opts.Int("keepAliveSec", 30)) * time.Second
	}
	go func() {
		client.probe(10 * time.Second) // initial latency sample
		client.keepalive(ka, keepAliveMaxMiss)
	}()
	return client, nil
}

func connectTimeout(opts model.Options) time.Duration {
	if n := opts.Int("connectTimeoutSec", 0); n > 0 {
		return min(time.Duration(n)*time.Second, 10*time.Minute)
	}
	return DefaultConnectTimeout
}

func viaSuffix(rt *route) string {
	if rt == nil {
		return ""
	}
	switch {
	case rt.via != nil:
		return " via " + rt.via.Conn.Host
	case rt.proxy != nil:
		return " via " + rt.proxy.Type + " proxy " + net.JoinHostPort(rt.proxy.Host, strconv.Itoa(rt.proxy.Port))
	case rt.command != "":
		return " via ProxyCommand"
	}
	return ""
}

// classifyDialError turns handshake errors into user-facing ones; authentication and host-key failures are marked
// permanent (never auto-retried, PROTO-39).
func (p *Pool) classifyDialError(err error, addr, label string) error {
	prefix := ""
	if label != "" {
		prefix = label + ": "
	}
	var ae *authError
	var he *hostKeyError
	switch {
	case errors.As(err, &ae):
		return term.Permanent(fmt.Errorf("%s%w", prefix, ae))
	case errors.As(err, &he):
		return term.Permanent(fmt.Errorf("%s%w", prefix, he))
	case errors.Is(err, errPromptCanceled):
		return term.Permanent(fmt.Errorf("%sauthentication canceled", prefix))
	case errors.Is(err, httpx.ErrLocked):
		return err
	}
	var ne *ssh.AlgorithmNegotiationError
	if errors.As(err, &ne) {
		return term.Permanent(fmt.Errorf("%sno common algorithm with %s (%v); enable legacy algorithms for old devices", prefix, addr, ne))
	}
	msg := strings.TrimPrefix(err.Error(), "ssh: handshake failed: ")
	if isTimeout(err) {
		msg = "timed out during the SSH handshake"
	}
	return fmt.Errorf("%sSSH handshake with %s failed: %s", prefix, addr, msg)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func friendlyNetError(err error) error {
	var oe *ssh.OpenChannelError
	if errors.As(err, &oe) {
		switch oe.Reason {
		case ssh.Prohibited:
			return fmt.Errorf("the gateway refused to forward the connection (administratively prohibited: %s)", oe.Message)
		case ssh.ConnectionFailed:
			return fmt.Errorf("the gateway could not reach the destination (%s)", oe.Message)
		}
	}
	if isTimeout(err) {
		return errors.New("connection timed out")
	}
	return err
}

// ---- deadline-controlled handshake connection ---------------------------------------------------------------------

// deadlineConn bounds the SSH handshake with a deadline that is suspended while the user answers a prompt
// (RESEARCH decision 5: the handshake deadline is paused while a prompt is open).
type deadlineConn struct {
	net.Conn
	timeout time.Duration
	mu      sync.Mutex
	paused  int
	cleared bool
}

func newDeadlineConn(c net.Conn, timeout time.Duration) *deadlineConn {
	d := &deadlineConn{Conn: c, timeout: timeout}
	_ = c.SetDeadline(time.Now().Add(timeout))
	return d
}

func (d *deadlineConn) pause() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paused++
	if !d.cleared {
		_ = d.Conn.SetDeadline(time.Time{})
	}
}

func (d *deadlineConn) resume() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.paused > 0 {
		d.paused--
	}
	if d.paused == 0 && !d.cleared {
		_ = d.Conn.SetDeadline(time.Now().Add(d.timeout))
	}
}

func (d *deadlineConn) clearDeadline() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cleared = true
	_ = d.Conn.SetDeadline(time.Time{})
}

// ---- routes (proxy → jump chain) ----------------------------------------------------------------------------------

// route is how a destination is reached: through an SSH gateway client, a proxy, a ProxyCommand, or directly.
type route struct {
	via     *Client
	rel     func() // releases via
	proxy   *proxySpec
	command string // ProxyCommand template
	cmdUser string
	timeout time.Duration
	once    sync.Once
	// guard vets every connection the route opens from the Termstead host itself (direct dials, the proxy server,
	// UDP knocks); nil = unrestricted. Streams through an SSH gateway are the gateway's business.
	guard *netguard.Guard
}

// dial opens a stream to addr along the route.
func (r *route) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	switch {
	case r.via != nil:
		dctx, cancel := context.WithTimeout(ctx, r.timeout)
		defer cancel()
		return r.via.DialContext(dctx, network, addr)
	case r.command != "":
		host, port, _ := net.SplitHostPort(addr)
		return dialCommand(ctx, r.command, host, port, r.cmdUser)
	case r.proxy != nil:
		return dialProxy(ctx, *r.proxy, network, addr, r.timeout, r.guard)
	}
	return dialDirect(ctx, network, addr, r.timeout, r.guard)
}

// release drops the route's reference on its gateway (idempotent).
func (r *route) release() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		if r.rel != nil {
			r.rel()
		}
	})
}

// handOver transfers the gateway reference to the caller (a client dialed through the route keeps its gateway alive).
func (r *route) handOver() func() {
	if r == nil || r.rel == nil {
		return nil
	}
	rel := r.rel
	r.rel = nil
	return rel
}

func (r *route) direct() bool { return r.via == nil && r.proxy == nil && r.command == "" }

// buildRoute resolves conn's options.jumpHosts (acquiring each hop through the previous one), options.proxy and
// options.proxyCommand. includeTunnelVia also honors options.sshTunnelVia (non-SSH protocols).
func (p *Pool) buildRoute(ctx context.Context, user *model.User, conn *model.Connection, secrets map[string]string, sess *term.Session, sshTarget bool) (*route, error) {
	opts := conn.Options
	rt := &route{timeout: connectTimeout(opts), guard: p.guardFor(user)}

	if !sshTarget {
		if via := strings.TrimSpace(opts.String("sshTunnelVia", "")); via != "" {
			if via == conn.ID {
				return nil, httpx.BadRequest("sshTunnelVia refers to the connection itself")
			}
			c, rel, err := p.Get(term.WithSession(ctx, sess), user, via)
			if err != nil {
				return nil, fmt.Errorf("SSH gateway: %w", err)
			}
			rt.via, rt.rel = c, rel
			return rt, nil
		}
	}

	first, err := p.firstHopRoute(user, conn, secrets)
	if err != nil {
		return nil, err
	}
	hops := opts.Strings("jumpHosts")
	if len(hops) == 0 {
		return first, nil
	}
	if len(hops) > maxJumpHops {
		return nil, httpx.BadRequest(fmt.Sprintf("too many jump hosts (max %d)", maxJumpHops))
	}
	seen := map[string]bool{poolKey(user, conn): true}
	var via *Client
	var viaRel func()
	fail := func(err error) (*route, error) {
		if viaRel != nil {
			viaRel()
		}
		return nil, err
	}
	for i, ref := range hops {
		hop, hopSecrets, err := p.resolveHop(ctx, user, ref)
		if err != nil {
			return fail(fmt.Errorf("jump host %d (%s): %w", i+1, ref, err))
		}
		// A hop is reached through the chain it is part of: its own jump / tunnel settings do not apply (this also
		// rules out nested chains and lock cycles between them).
		delete(hop.Options, "jumpHosts")
		delete(hop.Options, "sshTunnelVia")
		k := poolKey(user, hop)
		if seen[k] {
			return fail(httpx.BadRequest("the jump host chain contains a loop"))
		}
		seen[k] = true
		label := fmt.Sprintf("jump host %d (%s)", i+1, hopName(hop))
		o := dialOpts{session: sess, label: label}
		if via == nil {
			// The first hop uses its own proxy settings, falling back to the target's proxy / ProxyCommand.
			hopRoute, err := p.firstHopRoute(user, hop, hopSecrets)
			if err != nil {
				return fail(fmt.Errorf("%s: %w", label, err))
			}
			if hopRoute.direct() {
				hopRoute = &route{proxy: first.proxy, command: first.command, cmdUser: hop.Username, timeout: first.timeout,
					guard: first.guard}
			}
			o.route = hopRoute
		} else {
			o.via = via
		}
		hc, hrel, err := p.acquire(ctx, user, hop, hopSecrets, o)
		if viaRel != nil {
			viaRel() // a newly dialed hop holds its own reference on its gateway
			viaRel = nil
		}
		if err != nil {
			return fail(fmt.Errorf("%s: %w", label, err))
		}
		via, viaRel = hc, hrel
	}
	return &route{via: via, rel: viaRel, timeout: rt.timeout, guard: rt.guard}, nil
}

func cloneSecrets(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// firstHopRoute is the non-SSH part of a route: ProxyCommand, proxy, or direct.
func (p *Pool) firstHopRoute(user *model.User, conn *model.Connection, secrets map[string]string) (*route, error) {
	opts := conn.Options
	rt := &route{timeout: connectTimeout(opts), guard: p.guardFor(user)}
	if cmd := strings.TrimSpace(opts.String("proxyCommand", "")); cmd != "" {
		if !p.allowLocalExec(user) {
			return nil, httpx.Forbidden("ProxyCommand is only available in desktop mode or to administrators")
		}
		rt.command, rt.cmdUser = cmd, conn.Username
		return rt, nil
	}
	var spec proxySpec
	if err := opts.Decode("proxy", &spec); err != nil {
		return nil, httpx.BadRequest("invalid proxy option")
	}
	spec.Type = strings.ToLower(strings.TrimSpace(spec.Type))
	if spec.Type != "" && spec.Type != "none" {
		if spec.Host == "" || spec.Port <= 0 || spec.Port > 65535 {
			return nil, httpx.BadRequest("proxy host and port are required")
		}
		switch spec.Type {
		case "socks5", "socks5h", "socks4", "socks4a", "http":
		default:
			return nil, httpx.BadRequest("unsupported proxy type " + spec.Type)
		}
		spec.Password = secrets[model.SecretProxyPassword]
		rt.proxy = &spec
	}
	return rt, nil
}

// allowLocalExec reports whether user may run host-side helpers (ProxyCommand, host agent, local X server).
func (p *Pool) allowLocalExec(user *model.User) bool {
	if p.d == nil || p.d.Cfg == nil {
		return false
	}
	return p.d.Cfg.IsDesktop() || user.IsAdmin()
}

// guardFor returns the destination guard (SEC-7, internal/netguard) for connections Termstead opens from its own host
// on user's behalf: nil (unrestricted) in desktop mode and for administrators unless the policy says otherwise.
func (p *Pool) guardFor(user *model.User) *netguard.Guard {
	if p.d == nil {
		return nil
	}
	return netguard.For(p.d).ForUser(user)
}

// resolveHop resolves a jumpHosts entry: a saved connection ID, or an ad-hoc "[user@]host[:port]" spec.
func (p *Pool) resolveHop(ctx context.Context, user *model.User, ref string) (*model.Connection, map[string]string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, nil, httpx.BadRequest("empty jump host entry")
	}
	if model.ValidID(ref) && !strings.ContainsAny(ref, "@:.") {
		if p.d == nil {
			return nil, nil, httpx.ErrNotFound
		}
		conn, secrets, err := p.d.ResolveConnection(ctx, user, ref)
		if err != nil {
			return nil, nil, err
		}
		switch conn.Protocol {
		case model.ProtoSSH, model.ProtoSFTP:
		default:
			return nil, nil, httpx.BadRequest(fmt.Sprintf("jump host %q is not an SSH connection", conn.Name))
		}
		if conn.Port == 0 {
			conn.Port = 22
		}
		return conn, secrets, nil
	}
	c := &model.Connection{Protocol: model.ProtoSSH, Port: 22, AuthMethod: model.AuthAuto, OwnerID: user.ID}
	hostPort := ref
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		c.Username, hostPort = ref[:at], ref[at+1:]
	}
	if h, portStr, err := net.SplitHostPort(hostPort); err == nil {
		port, err := strconv.Atoi(portStr)
		if err != nil || port <= 0 || port > 65535 {
			return nil, nil, httpx.BadRequest("invalid jump host port in " + ref)
		}
		c.Host, c.Port = h, port
	} else {
		c.Host = strings.Trim(hostPort, "[]")
	}
	if c.Host == "" || strings.ContainsAny(c.Host, " \t/\\") {
		return nil, nil, httpx.BadRequest("invalid jump host " + ref)
	}
	c.Normalize()
	return c, map[string]string{}, nil
}

func hopName(c *model.Connection) string {
	if c.Name != "" {
		return c.Name
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// dialDirect connects from the Termstead host; g (nil = unrestricted) vets every address actually connected to.
func dialDirect(ctx context.Context, network, addr string, timeout time.Duration, g *netguard.Guard) (net.Conn, error) {
	d := &net.Dialer{
		Timeout: timeout,
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     30 * time.Second,
			Interval: 15 * time.Second,
			Count:    4,
		},
	}
	if g != nil {
		d.Control = g.Control
	}
	return d.DialContext(ctx, network, addr)
}

// ---- port knocking (SSH-39) ---------------------------------------------------------------------------------------

type knock struct {
	Port    int    `json:"port"`
	Proto   string `json:"proto"`
	DelayMs int    `json:"delayMs"`
}

func parseKnocks(opts model.Options) []knock {
	var ks []knock
	if err := opts.Decode("portKnock", &ks); err != nil {
		return nil
	}
	out := ks[:0]
	for _, k := range ks {
		if k.Port > 0 && k.Port <= 65535 {
			out = append(out, k)
		}
	}
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

// portKnock sends the configured TCP/UDP knock sequence to host before connecting. TCP knocks follow the route
// (proxy / gateway); UDP knocks are only possible on direct routes.
func (p *Pool) portKnock(ctx context.Context, rt *route, host string, knocks []knock) {
	for _, k := range knocks {
		if ctx.Err() != nil {
			return
		}
		addr := net.JoinHostPort(host, strconv.Itoa(k.Port))
		switch strings.ToLower(k.Proto) {
		case "udp":
			if !rt.direct() {
				p.log.Debug("ssh: skipping UDP knock on a proxied route", "port", k.Port)
				break
			}
			if c, err := rt.guard.Dialer(2*time.Second).DialContext(ctx, "udp", addr); err == nil {
				_, _ = c.Write([]byte{0})
				c.Close()
			}
		default:
			kctx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
			if c, err := rt.dial(kctx, "tcp", addr); err == nil {
				c.Close()
			}
			cancel()
		}
		delay := time.Duration(k.DelayMs) * time.Millisecond
		if delay <= 0 {
			delay = 100 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(min(delay, 10*time.Second)):
		}
	}
	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
	}
}
