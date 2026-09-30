package netguard

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// Decision classes (Decision.Class).
const (
	ClassPublic         = "public"          // allowed: nothing matched
	ClassPrivate        = "private"         // private network (allowed unless allowPrivate is off)
	ClassLoopback       = "loopback"        // 127.0.0.0/8, ::1
	ClassUnspecified    = "unspecified"     // 0.0.0.0/8 ("this network"), ::
	ClassIPv4Compatible = "ipv4-compatible" // deprecated ::a.b.c.d
	ClassLinkLocal      = "link-local"      // 169.254.0.0/16, fe80::/10
	ClassMulticast      = "multicast"       // link-local / interface-local multicast
	ClassMetadata       = "metadata"        // cloud metadata / platform endpoints
	ClassHost           = "host"            // an address of the AstraTerm host's interfaces
	ClassAstraTerm      = "astraterm"       // AstraTerm's own listener (never allowed)
	ClassDenyRule       = "deny-rule"       // administrator deny entry
	ClassAllowRule      = "allow-rule"      // administrator allow entry (exception)
	ClassPort           = "port"            // port not in allowedPorts
	ClassEmbedded       = "embedded"        // NAT64 / 6to4 address embedding a refused IPv4 address
	ClassLocalSocket    = "local-socket"    // Unix socket on the AstraTerm host
	ClassInvalid        = "invalid"         // not an IP address
)

// Decision is the verdict for one destination address.
type Decision struct {
	IP      string `json:"ip"`
	Port    int    `json:"port,omitempty"`
	Allowed bool   `json:"allowed"`
	Class   string `json:"class"`
	Rule    string `json:"rule,omitempty"` // matching CIDR, or the allowedPorts list
	Reason  string `json:"reason"`
}

// rule is one prefix of the compiled policy.
type rule struct {
	pfx      netip.Prefix
	allow    bool
	explicit bool   // administrator rule (beats a built-in rule of the same length)
	class    string // for built-in denies
}

// builtinRule describes a built-in refused range (also listed by the admin API).
type builtinRule struct {
	CIDR   string `json:"cidr"`
	Class  string `json:"class"`
	Reason string `json:"reason"`
}

var builtinDeny = []builtinRule{
	{"127.0.0.0/8", ClassLoopback, "loopback (the AstraTerm host itself)"},
	{"::1/128", ClassLoopback, "loopback (the AstraTerm host itself)"},
	{"0.0.0.0/8", ClassUnspecified, `"this network" (0.0.0.0 reaches the AstraTerm host)`},
	{"::/128", ClassUnspecified, "unspecified (:: reaches the AstraTerm host)"},
	{"::/96", ClassIPv4Compatible, "a deprecated IPv4-compatible IPv6 address (may reach IPv4 loopback)"},
	{"169.254.0.0/16", ClassLinkLocal, "link-local (includes the 169.254.169.254 cloud metadata service)"},
	{"fe80::/10", ClassLinkLocal, "link-local"},
	{"224.0.0.0/24", ClassMulticast, "link-local multicast"},
	{"ff02::/16", ClassMulticast, "link-local multicast"},
	{"ff01::/16", ClassMulticast, "interface-local multicast"},
	{"fd00:ec2::254/128", ClassMetadata, "the AWS instance metadata service (IPv6)"},
	{"fd00:ec2::23/128", ClassMetadata, "the AWS EKS Pod Identity credentials endpoint (IPv6)"},
	{"100.100.100.200/32", ClassMetadata, "the Alibaba Cloud metadata service"},
	{"168.63.129.16/32", ClassMetadata, "the Azure WireServer platform endpoint"},
}

var privateRanges = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7", "fec0::/10"}

// Translation prefixes whose addresses embed an IPv4 address (checked against the IPv4 rules as well).
var (
	nat64WKP  = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour = netip.MustParsePrefix("2002::/16")
)

// BuiltinRules lists the built-in refused ranges (private ranges excluded).
func BuiltinRules() []builtinRule { return append([]builtinRule(nil), builtinDeny...) }

var builtinCompiled = func() []rule {
	out := make([]rule, 0, len(builtinDeny))
	for _, b := range builtinDeny {
		out = append(out, rule{pfx: netip.MustParsePrefix(b.CIDR), class: b.Class})
	}
	return out
}()

var privateCompiled = func() []rule {
	out := make([]rule, 0, len(privateRanges))
	for _, s := range privateRanges {
		out = append(out, rule{pfx: netip.MustParsePrefix(s), class: ClassPrivate})
	}
	return out
}()

func builtinReason(class string, pfx netip.Prefix) string {
	for _, b := range builtinDeny {
		if b.CIDR == pfx.String() {
			return b.Reason
		}
	}
	switch class {
	case ClassPrivate:
		return "a private-network address (private networks are disabled by the network policy)"
	case ClassHost:
		return "an address of the AstraTerm host itself"
	}
	return class
}

// compiled is a validated policy ready for checks.
type compiled struct {
	pol   Policy
	rules []rule // administrator rules + built-ins (+ private ranges when disallowed)
	ports []portRange
}

func compile(p Policy) (*compiled, error) {
	if err := p.Normalize(); err != nil {
		return nil, err
	}
	c := &compiled{pol: p}
	c.rules = append(c.rules, builtinCompiled...)
	if !p.AllowPrivate {
		c.rules = append(c.rules, privateCompiled...)
	}
	for _, s := range p.Deny {
		c.rules = append(c.rules, rule{pfx: netip.MustParsePrefix(s), explicit: true, class: ClassDenyRule})
	}
	for _, s := range p.Allow {
		c.rules = append(c.rules, rule{pfx: netip.MustParsePrefix(s), explicit: true, allow: true, class: ClassAllowRule})
	}
	c.ports, _ = parsePorts(p.AllowedPorts)
	return c, nil
}

var defaultCompiled = func() *compiled {
	c, err := compile(DefaultPolicy())
	if err != nil {
		panic(err)
	}
	return c
}()

// env is what a check knows about the AstraTerm host: its own listener and interface addresses.
type env struct {
	listenPort     int
	listenIPs      []netip.Addr // specific listen addresses (loopback for "localhost")
	listenWildcard bool         // listening on every interface
	hostAddrs      func() map[netip.Addr]bool
}

func (e *env) isHostAddr(a netip.Addr) bool {
	if e == nil || e.hostAddrs == nil {
		return false
	}
	return e.hostAddrs()[a]
}

// isSelf reports whether a:port reaches AstraTerm's own listener.
func (e *env) isSelf(a netip.Addr, port int) bool {
	if e == nil || e.listenPort <= 0 || port != e.listenPort {
		return false
	}
	if a.IsUnspecified() {
		return true // connecting to 0.0.0.0 / :: reaches any local listener
	}
	if e.listenWildcard {
		return a.IsLoopback() || e.isHostAddr(a)
	}
	for _, l := range e.listenIPs {
		if l == a {
			return true
		}
	}
	return false
}

// check decides whether a restricted user may connect to a:port (port ≤ 0 = unknown: port rules are skipped).
func (c *compiled) check(a netip.Addr, port int, e *env) Decision {
	d := Decision{Port: max(port, 0)}
	if !a.IsValid() {
		d.Class, d.Reason = ClassInvalid, "not an IP address"
		return d
	}
	a = a.WithZone("").Unmap()
	d.IP = a.String()
	if e.isSelf(a, port) {
		d.Class, d.Reason = ClassAstraTerm, "the address of AstraTerm's own listener"
		return d
	}
	if !portAllowed(c.ports, port) {
		d.Class, d.Rule = ClassPort, c.pol.AllowedPorts
		d.Reason = fmt.Sprintf("port %d is not an allowed port (%s)", port, c.pol.AllowedPorts)
		return d
	}
	best, ok := c.match(a, e)
	if ok {
		if best.allow {
			d.Allowed, d.Class, d.Rule = true, ClassAllowRule, best.pfx.String()
			d.Reason = "allowed by the network policy exception " + best.pfx.String()
			return d
		}
		d.Class, d.Rule = best.class, best.pfx.String()
		switch best.class {
		case ClassDenyRule:
			d.Reason = "refused by the network policy rule " + best.pfx.String()
		default:
			d.Reason = builtinReason(best.class, best.pfx)
		}
		return d
	}
	// IPv6 translation addresses carrying an IPv4 address are judged by that address too.
	if v4, kind := embeddedIPv4(a); v4.IsValid() {
		if sub := c.check(v4, 0, e); !sub.Allowed {
			d.Class, d.Rule = ClassEmbedded, sub.Rule
			d.Reason = fmt.Sprintf("a %s address embedding %s, which is %s", kind, v4, sub.Reason)
			return d
		}
	}
	d.Allowed = true
	if isPrivate(a) {
		d.Class, d.Reason = ClassPrivate, "a private-network address (allowed)"
	} else {
		d.Class, d.Reason = ClassPublic, "allowed"
	}
	return d
}

// match returns the most specific matching rule: longest prefix; then administrator over built-in; then deny over
// allow. The AstraTerm host's own addresses count as built-in single-address denies.
func (c *compiled) match(a netip.Addr, e *env) (rule, bool) {
	var best rule
	found := false
	better := func(r rule) bool {
		if !found {
			return true
		}
		if r.pfx.Bits() != best.pfx.Bits() {
			return r.pfx.Bits() > best.pfx.Bits()
		}
		if r.explicit != best.explicit {
			return r.explicit
		}
		return !r.allow && best.allow
	}
	for _, r := range c.rules {
		if r.pfx.Contains(a) && better(r) {
			best, found = r, true
		}
	}
	if c.pol.BlockHostAddresses && e.isHostAddr(a) {
		r := rule{pfx: netip.PrefixFrom(a, a.BitLen()), class: ClassHost}
		if better(r) {
			best, found = r, true
		}
	}
	return best, found
}

func isPrivate(a netip.Addr) bool {
	for _, r := range privateCompiled {
		if r.pfx.Contains(a) {
			return true
		}
	}
	return false
}

// embeddedIPv4 extracts the IPv4 address of a NAT64 (64:ff9b::/96) or 6to4 (2002::/16) address.
func embeddedIPv4(a netip.Addr) (netip.Addr, string) {
	if !a.Is6() {
		return netip.Addr{}, ""
	}
	b := a.As16()
	switch {
	case nat64WKP.Contains(a):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), "NAT64"
	case sixToFour.Contains(a):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), "6to4"
	}
	return netip.Addr{}, ""
}

// ParseHostIP interprets host as an IP address the way connect(2)-level resolvers may: standard IPv4 / IPv6
// notation (brackets and zones allowed) and the legacy inet_aton forms ("127.1", "2130706433", "0177.0.0.1",
// "0x7f.1"). ok is false for host names.
func ParseHostIP(host string) (netip.Addr, bool) {
	h := strings.TrimSuffix(strings.Trim(strings.TrimSpace(host), "[]"), ".")
	if a, err := netip.ParseAddr(h); err == nil {
		return a.WithZone("").Unmap(), true
	}
	if a, ok := parseLegacyIPv4(h); ok {
		return a, true
	}
	return netip.Addr{}, false
}

// CanonicalAddr rewrites a host:port whose host is a legacy IPv4 form ("127.1", "0x7f000001", …) to the dotted
// address, so every platform dials the same host: the system resolver of some platforms maps these forms to an
// address and Go's own resolver does not. The dial-time Control check then sees the real address. Other addresses are
// returned unchanged.
func CanonicalAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if _, perr := netip.ParseAddr(host); perr == nil {
		return addr
	}
	if a, ok := parseLegacyIPv4(strings.TrimSuffix(host, ".")); ok {
		return net.JoinHostPort(a.String(), port)
	}
	return addr
}

// parseLegacyIPv4 implements inet_aton: 1-4 dot-separated parts in decimal, octal (leading 0) or hex (0x); the last
// part fills the remaining bytes.
func parseLegacyIPv4(s string) (netip.Addr, bool) {
	if s == "" || len(s) > 64 {
		return netip.Addr{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	vals := make([]uint64, len(parts))
	for i, p := range parts {
		if p == "" {
			return netip.Addr{}, false
		}
		base := 10
		switch {
		case len(p) > 2 && (p[:2] == "0x" || p[:2] == "0X"):
			base, p = 16, p[2:]
		case len(p) > 1 && p[0] == '0':
			base, p = 8, p[1:]
		}
		v, err := strconv.ParseUint(p, base, 32)
		if err != nil {
			return netip.Addr{}, false
		}
		vals[i] = v
	}
	var n uint64
	last := len(vals) - 1
	for i, v := range vals[:last] {
		if v > 255 {
			return netip.Addr{}, false
		}
		n |= v << (8 * uint(3-i))
	}
	if vals[last] >= 1<<(8*uint(4-last)) {
		return netip.Addr{}, false
	}
	n |= vals[last]
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}), true
}

// isLocalhostName reports names that always mean the local host (RFC 6761).
func isLocalhostName(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	return h == "localhost" || strings.HasSuffix(h, ".localhost") || h == "localhost.localdomain" ||
		h == "ip6-localhost" || h == "ip6-loopback"
}
