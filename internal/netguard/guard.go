package netguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
)

// CodeBlocked is the API error code of a refused destination (HTTP 403).
const CodeBlocked = "destination_blocked"

// policyTTL bounds how long a policy stays cached: saves through this package apply at once, a value written by
// other means (PUT /api/admin/settings) within policyTTL.
const policyTTL = 5 * time.Second

// resolver resolves host names for the advisory checks (a variable for tests).
var resolver = net.DefaultResolver

// hostAddrTTL bounds how long the host's interface address list is cached.
const hostAddrTTL = 30 * time.Second

// BlockedError is returned (usually wrapped in a *net.OpError) when the policy refuses a destination. It unwraps to
// an *httpx.HTTPError (403 destination_blocked), so handlers can return it as is.
type BlockedError struct {
	Decision
	Addr string // host:port as dialed / checked
	http *httpx.HTTPError
}

func newBlocked(d Decision, addr string) *BlockedError {
	target := addr
	if target == "" {
		target = d.IP
	}
	msg := fmt.Sprintf("connection to %s is not allowed in server mode (AstraTerm network policy): %s", target, d.Reason)
	switch d.Class {
	case ClassPort, ClassLocalSocket, ClassInvalid:
	default:
		if d.IP != "" {
			msg = fmt.Sprintf("connection to %s is not allowed in server mode (AstraTerm network policy): %s is %s", target, d.IP, d.Reason)
		}
	}
	return &BlockedError{Decision: d, Addr: addr, http: httpx.NewError(http.StatusForbidden, CodeBlocked, msg)}
}

func (e *BlockedError) Error() string { return e.http.Message }

// Unwrap exposes the HTTP error (403 destination_blocked).
func (e *BlockedError) Unwrap() error { return e.http }

// IsBlocked reports whether err (or an error it wraps) is a policy refusal and returns it.
func IsBlocked(err error) (*BlockedError, bool) {
	var be *BlockedError
	ok := errors.As(err, &be)
	return be, ok
}

// ---- manager ------------------------------------------------------------------------------------------------------

// Manager holds the live policy of one AstraTerm instance (per *app.Deps).
type Manager struct {
	d   *app.Deps
	log *slog.Logger
	env *env

	mu      sync.Mutex
	cur     *compiled
	fetched time.Time
	raw     string // last stored JSON seen (to log invalid values once)
}

var (
	regMu    sync.Mutex
	registry = map[*app.Deps]*Manager{}
)

// For returns the manager of d (created on first use and dropped when d.Ctx ends). For(nil) is nil, and a nil
// manager hands out nil (unrestricted) guards.
func For(d *app.Deps) *Manager {
	if d == nil {
		return nil
	}
	regMu.Lock()
	defer regMu.Unlock()
	if m := registry[d]; m != nil {
		return m
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{d: d, log: log.With("module", "netguard"), env: newEnv(d)}
	registry[d] = m
	if d.Ctx != nil {
		context.AfterFunc(d.Ctx, func() {
			regMu.Lock()
			if registry[d] == m {
				delete(registry, d)
			}
			regMu.Unlock()
		})
	}
	return m
}

// ForUser is shorthand for For(d).ForUser(user).
func ForUser(d *app.Deps, user *model.User) *Guard { return For(d).ForUser(user) }

// Enforced reports whether the policy is enforced at all (server mode). Desktop mode — and a configuration-less
// instance (unit tests) — is unrestricted.
func (m *Manager) Enforced() bool {
	return m != nil && m.d.Cfg != nil && m.d.Cfg.IsServer()
}

// ForUser returns the guard for user's connections, or nil (unrestricted) in desktop mode and for administrators
// (unless applyToAdmins). A nil user is restricted. The guard reads the live policy on every check.
func (m *Manager) ForUser(user *model.User) *Guard {
	if !m.Enforced() {
		return nil
	}
	if user.IsAdmin() && !m.current().pol.ApplyToAdmins {
		return nil
	}
	return &Guard{m: m}
}

// Policy returns the current policy.
func (m *Manager) Policy() Policy {
	if m == nil {
		return DefaultPolicy()
	}
	return clonePolicy(m.current().pol)
}

// SetPolicy validates, stores and applies p.
func (m *Manager) SetPolicy(ctx context.Context, p Policy) (Policy, error) {
	c, err := compile(p)
	if err != nil {
		return Policy{}, httpx.BadRequest(err.Error())
	}
	if m.d.Store != nil {
		if err := m.d.Store.Settings.SetJSON(ctx, store.ScopeGlobal, SettingsKey, c.pol); err != nil {
			return Policy{}, err
		}
	}
	raw, _ := json.Marshal(c.pol)
	m.mu.Lock()
	m.cur, m.fetched, m.raw = c, time.Now(), string(raw)
	m.mu.Unlock()
	return clonePolicy(c.pol), nil
}

// current returns the compiled policy, re-reading the stored value after policyTTL. A missing value is the default
// policy; an invalid one is logged and replaced by the default policy (never by "allow everything").
func (m *Manager) current() *compiled {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur != nil && time.Since(m.fetched) < policyTTL {
		return m.cur
	}
	m.fetched = time.Now()
	if m.d.Store == nil {
		if m.cur == nil {
			m.cur = defaultCompiled
		}
		return m.cur
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := m.d.Store.Settings.Get(ctx, store.ScopeGlobal, SettingsKey)
	switch {
	case errors.Is(err, model.ErrNotFound):
		m.cur, m.raw = defaultCompiled, ""
		return m.cur
	case err != nil:
		m.log.Warn("cannot read the network policy; keeping the previous one", "err", err)
		if m.cur == nil {
			m.cur = defaultCompiled
		}
		return m.cur
	}
	if string(raw) == m.raw && m.cur != nil {
		return m.cur
	}
	m.raw = string(raw)
	p := DefaultPolicy()
	if err := json.Unmarshal(raw, &p); err != nil {
		m.log.Error("invalid network policy in settings; using the default policy", "err", err)
		m.cur = defaultCompiled
		return m.cur
	}
	c, err := compile(p)
	if err != nil {
		m.log.Error("invalid network policy in settings; using the default policy", "err", err)
		m.cur = defaultCompiled
		return m.cur
	}
	m.cur = c
	return c
}

func clonePolicy(p Policy) Policy {
	p.Deny = append([]string{}, p.Deny...)
	p.Allow = append([]string{}, p.Allow...)
	return p
}

// newEnv describes AstraTerm's own listener (Cfg.Listen) and host addresses.
func newEnv(d *app.Deps) *env {
	e := &env{hostAddrs: hostAddrs}
	if d.Cfg == nil {
		return e
	}
	host, port, err := net.SplitHostPort(d.Cfg.Listen)
	if err != nil {
		return e
	}
	e.listenPort, _ = strconv.Atoi(port)
	host = strings.Trim(host, "[]")
	switch {
	case host == "" || host == "*":
		e.listenWildcard = true
	case isLocalhostName(host):
		e.listenIPs = []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.IPv6Loopback()}
	default:
		if a, ok := ParseHostIP(host); ok {
			if a.IsUnspecified() {
				e.listenWildcard = true
			} else {
				e.listenIPs = []netip.Addr{a}
			}
		} else {
			e.listenWildcard = true // a host name: be conservative
		}
	}
	return e
}

var hostCache struct {
	sync.Mutex
	addrs map[netip.Addr]bool
	at    time.Time
}

// hostAddrs returns the addresses configured on this machine's interfaces, except loopback and link-local ones
// (cached for hostAddrTTL).
func hostAddrs() map[netip.Addr]bool {
	hostCache.Lock()
	defer hostCache.Unlock()
	if hostCache.addrs != nil && time.Since(hostCache.at) < hostAddrTTL {
		return hostCache.addrs
	}
	m := map[netip.Addr]bool{}
	if as, err := net.InterfaceAddrs(); err == nil {
		for _, a := range as {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			na, ok := netip.AddrFromSlice(ip)
			if !ok {
				continue
			}
			na = na.Unmap()
			// Loopback and link-local addresses are refused by their own (clearer) built-in classes.
			if na.IsLoopback() || na.IsLinkLocalUnicast() || na.IsUnspecified() {
				continue
			}
			m[na] = true
		}
	}
	hostCache.addrs, hostCache.at = m, time.Now()
	return m
}

// ---- guard --------------------------------------------------------------------------------------------------------

// Guard vets the destinations of one restricted user. A nil *Guard allows everything. The zero Guard (no manager)
// applies DefaultPolicy without knowledge of AstraTerm's listener or host addresses (tests, stand-alone use).
type Guard struct {
	m   *Manager
	pol *compiled // fixed policy (NewGuard); nil = the manager's live policy
}

// NewGuard returns a guard with a fixed policy (no knowledge of AstraTerm's listener or host addresses).
func NewGuard(p Policy) (*Guard, error) {
	c, err := compile(p)
	if err != nil {
		return nil, err
	}
	return &Guard{pol: c}, nil
}

// withPolicy returns a guard of m's instance using a draft policy (dry runs).
func (m *Manager) withPolicy(p Policy) (*Guard, error) {
	c, err := compile(p)
	if err != nil {
		return nil, err
	}
	return &Guard{m: m, pol: c}, nil
}

// Restricted reports whether g restricts anything (g != nil).
func (g *Guard) Restricted() bool { return g != nil }

func (g *Guard) parts() (*compiled, *env) {
	var e *env
	if g.m != nil {
		e = g.m.env
	}
	switch {
	case g.pol != nil:
		return g.pol, e
	case g.m != nil:
		return g.m.current(), e
	}
	return defaultCompiled, e
}

// Check returns the decision for ip:port (port ≤ 0 = unknown). A nil guard allows everything.
func (g *Guard) Check(ip netip.Addr, port int) Decision {
	if g == nil {
		return Decision{IP: ip.String(), Port: max(port, 0), Allowed: true, Class: ClassPublic, Reason: "unrestricted"}
	}
	c, e := g.parts()
	return c.check(ip, port, e)
}

// CheckAddr returns a *BlockedError when ip:port is refused (port ≤ 0 skips port rules).
func (g *Guard) CheckAddr(ip netip.Addr, port int) error {
	if g == nil {
		return nil
	}
	if d := g.Check(ip, port); !d.Allowed {
		addr := ip.WithZone("").Unmap().String()
		if port > 0 {
			addr = net.JoinHostPort(addr, strconv.Itoa(port))
		}
		return newBlocked(d, addr)
	}
	return nil
}

// CheckIP is CheckAddr for a net.IP.
func (g *Guard) CheckIP(ip net.IP, port int) error {
	if g == nil {
		return nil
	}
	a, _ := netip.AddrFromSlice(ip)
	return g.CheckAddr(a, port)
}

// Control is a net.Dialer.Control / net.ListenConfig-style hook vetting the concrete address a socket connects to.
// Unix sockets are refused (they are local endpoints of the AstraTerm host).
func (g *Guard) Control(network, address string, _ syscall.RawConn) error {
	if g == nil {
		return nil
	}
	switch network {
	case "unix", "unixgram", "unixpacket":
		return newBlocked(Decision{Class: ClassLocalSocket, Reason: "a local socket of the AstraTerm host"}, address)
	}
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		host, portStr = address, ""
	}
	port, _ := strconv.Atoi(portStr)
	a, err := netip.ParseAddr(host)
	if err != nil {
		return newBlocked(Decision{Class: ClassInvalid, Reason: "not an IP address"}, address)
	}
	if d := g.Check(a, port); !d.Allowed {
		return newBlocked(d, address)
	}
	return nil
}

// Dialer returns a net.Dialer (timeout, 30 s TCP keep-alive) whose every connection attempt is vetted. For a nil
// guard it is a plain dialer.
func (g *Guard) Dialer(timeout time.Duration) *net.Dialer {
	d := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	if g != nil {
		d.Control = g.Control
	}
	return d
}

// DialContext dials with Dialer(timeout).
func (g *Guard) DialContext(ctx context.Context, network, addr string, timeout time.Duration) (net.Conn, error) {
	return g.Dialer(timeout).DialContext(ctx, network, CanonicalAddr(addr))
}

// CheckLiteral is the advisory, DNS-free check of a host as written: IP literals (including legacy forms such as
// "127.1" or "0x7f000001") and localhost names are judged; other host names pass (they are vetted when dialed).
func (g *Guard) CheckLiteral(host string, port int) error {
	if g == nil {
		return nil
	}
	if isLocalhostName(host) {
		d := g.Check(netip.MustParseAddr("127.0.0.1"), port)
		if !d.Allowed {
			d.IP = strings.TrimSpace(host)
			return newBlocked(d, joinHostPort(host, port))
		}
		return nil
	}
	if a, ok := ParseHostIP(host); ok {
		return g.CheckAddr(a, port)
	}
	return nil
}

// CheckHostPort resolves host and checks every address (advisory: the dial-time Control check is authoritative).
// It returns a *BlockedError when any address is refused, or the resolution error.
func (g *Guard) CheckHostPort(ctx context.Context, host string, port int) error {
	if g == nil {
		return nil
	}
	if err := g.CheckLiteral(host, port); err != nil {
		return err
	}
	if _, ok := ParseHostIP(host); ok || isLocalhostName(host) {
		return nil
	}
	ips, err := resolver.LookupNetIP(ctx, "ip", strings.Trim(host, "[]"))
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if d := g.Check(ip, port); !d.Allowed {
			return newBlocked(d, joinHostPort(host, port))
		}
	}
	return nil
}

// Transport returns a clone of base (http.DefaultTransport when nil) whose connections are vetted. For a
// restricted guard, environment proxies are disabled (a proxy would reach targets the guard cannot see).
func (g *Guard) Transport(base *http.Transport) *http.Transport {
	if base == nil {
		base, _ = http.DefaultTransport.(*http.Transport)
	}
	var t *http.Transport
	if base != nil {
		t = base.Clone()
	} else {
		t = &http.Transport{}
	}
	if g != nil {
		t.DialContext = g.Dialer(30 * time.Second).DialContext
		t.DialTLSContext = nil
		t.Proxy = nil
	}
	return t
}

func joinHostPort(host string, port int) string {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if port <= 0 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}
