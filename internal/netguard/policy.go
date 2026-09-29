// Package netguard is AstraTerm's single destination policy (RESEARCH SEC-7 "SSRF and destination controls"). In server
// mode every connection AstraTerm opens *from its own host* on behalf of an ordinary user — SSH targets and first jump
// hops, proxy servers, port knocks, telnet / rlogin / raw / VNC / RDP / FTP / S3 / WebDAV sockets, tool probes, the
// host side of remote port forwards — is vetted against this policy, so a user cannot aim AstraTerm at the host itself
// (loopback services, other users' tunnel listeners, AstraTerm's own API), at link-local / cloud metadata endpoints or at
// networks the administrator excluded. Connections that an SSH server, jump host or proxy makes on AstraTerm's behalf
// are that hop's business and are not checked.
//
// Enforcement happens on the concrete address being connected to (net.Dialer.Control), after DNS resolution, so DNS
// rebinding, HTTP redirects and FTP PASV replies cannot bypass it. Host-name level checks (CheckHostPort,
// CheckLiteral) are advisory: they give early, friendly errors.
//
// Desktop mode is unrestricted (it is the user's own machine); in server mode administrators are unrestricted unless
// the policy's applyToAdmins is set. Use For(d).ForUser(user): a nil *Guard allows everything.
package netguard

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// SettingsKey is the global settings key holding the administrator's Policy.
const SettingsKey = "netguard"

// Limits of a policy.
const (
	maxRules       = 256
	maxRuleLen     = 64
	maxPortSpecLen = 2000
)

// Policy is the administrator-configurable destination policy (global settings key "netguard"). The zero value is
// NOT the default; use DefaultPolicy.
type Policy struct {
	// AllowPrivate allows RFC 1918 / RFC 4193 private networks (10/8, 172.16/12, 192.168/16, fc00::/7, fec0::/10) —
	// the bastion use case. Default true.
	AllowPrivate bool `json:"allowPrivate"`
	// BlockHostAddresses refuses every address configured on the AstraTerm host's own interfaces (any port): services
	// bound to all interfaces but protected by an external firewall / security group are otherwise reachable from
	// the host itself. Default true.
	BlockHostAddresses bool `json:"blockHostAddresses"`
	// Deny lists extra refused networks (IP or CIDR).
	Deny []string `json:"deny"`
	// Allow lists exceptions (IP or CIDR), e.g. one loopback-bound service the administrator wants to expose. The
	// most specific (longest) matching prefix wins; an administrator rule beats a built-in rule of the same length and
	// deny beats allow on a tie. AstraTerm's own listener can never be allowed.
	Allow []string `json:"allow"`
	// AllowedPorts restricts destination ports ("22,80,443,5900-5999"); "" = every port. Applies to allowed
	// exceptions too.
	AllowedPorts string `json:"allowedPorts"`
	// ApplyToAdmins subjects administrators to the policy too (server mode). Default false.
	ApplyToAdmins bool `json:"applyToAdmins"`
}

// DefaultPolicy is the policy used until an administrator saves one.
func DefaultPolicy() Policy {
	return Policy{AllowPrivate: true, BlockHostAddresses: true, Deny: []string{}, Allow: []string{}}
}

// Normalize validates p and canonicalizes it in place (prefixes masked and deduplicated, port list sorted and
// merged). The error message is suitable for users.
func (p *Policy) Normalize() error {
	var err error
	if p.Deny, err = normalizeRules("deny", p.Deny); err != nil {
		return err
	}
	if p.Allow, err = normalizeRules("allow", p.Allow); err != nil {
		return err
	}
	ranges, err := parsePorts(p.AllowedPorts)
	if err != nil {
		return err
	}
	p.AllowedPorts = formatPorts(ranges)
	return nil
}

func normalizeRules(field string, in []string) ([]string, error) {
	if len(in) > maxRules {
		return nil, fmt.Errorf("%s: at most %d entries", field, maxRules)
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if len(s) > maxRuleLen {
			return nil, fmt.Errorf("%s: entry %.20q… is too long", field, s)
		}
		pfx, err := ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", field, err)
		}
		if k := pfx.String(); !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out, nil
}

// ParsePrefix parses an IP address ("10.0.0.5", "fd00::1") or a CIDR ("10.0.0.0/8"). Host bits are masked,
// IPv4-mapped IPv6 prefixes become IPv4 prefixes; zones are refused.
func ParsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "%") {
		return netip.Prefix{}, fmt.Errorf("%q: zones are not supported", s)
	}
	var pfx netip.Prefix
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%q is not a valid CIDR", s)
		}
		pfx = p
	} else {
		a, err := netip.ParseAddr(strings.Trim(s, "[]"))
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%q is not a valid IP address or CIDR", s)
		}
		pfx = netip.PrefixFrom(a, a.BitLen())
	}
	if a := pfx.Addr(); a.Is4In6() {
		if pfx.Bits() < 96 {
			return netip.Prefix{}, fmt.Errorf("%q: an IPv4-mapped prefix must be at least /96", s)
		}
		pfx = netip.PrefixFrom(a.Unmap(), pfx.Bits()-96)
	}
	return pfx.Masked(), nil
}

// portRange is an inclusive range of ports.
type portRange struct{ lo, hi int }

// parsePorts parses "22,80,1000-2000" ("" = no restriction → nil).
func parsePorts(s string) ([]portRange, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if len(s) > maxPortSpecLen {
		return nil, errors.New("allowedPorts is too long")
	}
	var out []portRange
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err1 := strconv.Atoi(strings.TrimSpace(lo))
		b := a
		var err2 error
		if isRange {
			b, err2 = strconv.Atoi(strings.TrimSpace(hi))
		}
		if err1 != nil || err2 != nil || a < 1 || b > 65535 || a > b {
			return nil, fmt.Errorf("allowedPorts: %q is not a port (1-65535) or range (a-b)", part)
		}
		out = append(out, portRange{a, b})
	}
	if len(out) == 0 {
		return nil, nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].lo < out[j].lo })
	merged := out[:1]
	for _, r := range out[1:] {
		last := &merged[len(merged)-1]
		if r.lo <= last.hi+1 {
			last.hi = max(last.hi, r.hi)
			continue
		}
		merged = append(merged, r)
	}
	return slices.Clip(merged), nil
}

func formatPorts(rs []portRange) string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		if r.lo == r.hi {
			parts = append(parts, strconv.Itoa(r.lo))
		} else {
			parts = append(parts, strconv.Itoa(r.lo)+"-"+strconv.Itoa(r.hi))
		}
	}
	return strings.Join(parts, ",")
}

func portAllowed(rs []portRange, port int) bool {
	if rs == nil || port <= 0 {
		return true
	}
	for _, r := range rs {
		if port >= r.lo && port <= r.hi {
			return true
		}
	}
	return false
}
