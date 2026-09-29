package tools

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/nexterm/nexterm/internal/httpx"
)

// maxHosts and maxProbes bound a single scan so a broad spec cannot exhaust the host or flood the client: at most a
// /19 of hosts, and at most 262144 host×port probes (e.g. 4 hosts × all ports, or a /20 × the top 64 ports).
const (
	maxHosts  = 8192
	maxProbes = 1 << 18
)

// parsePorts expands a port spec into a de-duplicated, sorted list. It accepts comma-separated single ports, ranges
// ("1000-2000"), and the aliases "top100"/"common" and "all". Whitespace is ignored.
func parsePorts(spec string) ([]int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return append([]int(nil), topPorts...), nil
	}
	seen := map[int]bool{}
	var out []int
	add := func(p int) {
		if p >= 1 && p <= 65535 && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if part == "" {
			continue
		}
		switch part {
		case "top100", "top", "common":
			for _, p := range topPorts {
				add(p)
			}
			continue
		case "all", "*", "1-65535":
			for p := 1; p <= 65535; p++ {
				add(p)
			}
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			l, err1 := strconv.Atoi(strings.TrimSpace(lo))
			h, err2 := strconv.Atoi(strings.TrimSpace(hi))
			if err1 != nil || err2 != nil || l < 1 || h < 1 || l > 65535 || h > 65535 {
				return nil, httpx.BadRequest("invalid port range: " + part)
			}
			if l > h {
				l, h = h, l
			}
			for p := l; p <= h; p++ {
				add(p)
			}
			continue
		}
		p, err := strconv.Atoi(part)
		if err != nil || p < 1 || p > 65535 {
			return nil, httpx.BadRequest("invalid port: " + part)
		}
		add(p)
	}
	if len(out) == 0 {
		return nil, httpx.BadRequest("no ports selected")
	}
	sort.Ints(out)
	return out, nil
}

// checkProbeBudget rejects scans whose host × port product exceeds maxProbes.
func checkProbeBudget(hosts, ports int) error {
	if int64(hosts)*int64(ports) > maxProbes {
		return httpx.BadRequest(fmt.Sprintf("scan too large: %d hosts × %d ports exceeds %d probes; narrow the targets or ports", hosts, ports, maxProbes))
	}
	return nil
}

// expandHosts expands a target spec into a list of host strings. It accepts comma/space-separated entries, each a
// single host/IP, a CIDR ("10.0.0.0/24"), a last-octet range ("192.168.1.10-40") or a full range
// ("192.168.1.10-192.168.1.40"). The total is capped at maxHosts.
func expandHosts(spec string) ([]string, error) {
	fields := strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
	if len(fields) == 0 {
		return nil, httpx.BadRequest("no targets")
	}
	seen := map[string]bool{}
	var out []string
	add := func(h string) bool {
		if h == "" || seen[h] {
			return true
		}
		if len(out) >= maxHosts {
			return false
		}
		seen[h] = true
		out = append(out, h)
		return true
	}
	for _, f := range fields {
		hosts, err := expandOne(f)
		if err != nil {
			return nil, err
		}
		for _, h := range hosts {
			if !add(h) {
				return nil, httpx.BadRequest(fmt.Sprintf("too many hosts (max %d)", maxHosts))
			}
		}
	}
	if len(out) == 0 {
		return nil, httpx.BadRequest("no targets")
	}
	return out, nil
}

func expandOne(f string) ([]string, error) {
	if strings.Contains(f, "/") {
		return expandCIDR(f)
	}
	// "a-b" is a range only when a is an IP address: host names may contain hyphens ("web-01.example.com").
	if lo, hi, ok := strings.Cut(f, "-"); ok {
		if _, err := netip.ParseAddr(strings.TrimSpace(lo)); err == nil {
			return expandRange(strings.TrimSpace(lo), strings.TrimSpace(hi))
		}
	}
	host, err := safeHostArg(f)
	if err != nil {
		return nil, httpx.BadRequest("invalid target: " + f)
	}
	return []string{strings.Trim(host, "[]")}, nil
}

// expandCIDR lists the usable host addresses of a CIDR. For IPv4 /31 and /32 every address is included; for wider
// prefixes the network and broadcast addresses are dropped. IPv6 CIDRs are capped at maxHosts.
func expandCIDR(cidr string) ([]string, error) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, httpx.BadRequest("invalid CIDR: " + cidr)
	}
	p = p.Masked()
	var out []string
	addr := p.Addr()
	bits := p.Bits()
	isV4 := addr.Is4()
	total := 0
	dropNetBroadcast := isV4 && bits <= 30
	for a := addr; p.Contains(a); a = a.Next() {
		if !a.IsValid() {
			break
		}
		total++
		if total > maxHosts {
			return nil, httpx.BadRequest(fmt.Sprintf("CIDR too large (max %d hosts)", maxHosts))
		}
		out = append(out, a.String())
	}
	if dropNetBroadcast && len(out) >= 2 {
		out = out[1 : len(out)-1] // drop network + broadcast
	}
	return out, nil
}

// expandRange expands "a-b". hi may be a bare last octet ("192.168.1.10-40") or a full address.
func expandRange(lo, hi string) ([]string, error) {
	start, err := netip.ParseAddr(lo)
	if err != nil {
		return nil, httpx.BadRequest("invalid range start: " + lo)
	}
	var end netip.Addr
	if strings.Contains(hi, ".") || strings.Contains(hi, ":") {
		end, err = netip.ParseAddr(hi)
		if err != nil {
			return nil, httpx.BadRequest("invalid range end: " + hi)
		}
	} else {
		// Bare number: replace the final octet of an IPv4 start.
		if !start.Is4() {
			return nil, httpx.BadRequest("short range end requires an IPv4 start: " + hi)
		}
		n, err := strconv.Atoi(hi)
		if err != nil || n < 0 || n > 255 {
			return nil, httpx.BadRequest("invalid range end: " + hi)
		}
		b := start.As4()
		b[3] = byte(n)
		end = netip.AddrFrom4(b)
	}
	if start.Is4() != end.Is4() {
		return nil, httpx.BadRequest("range endpoints must be the same IP family")
	}
	if end.Less(start) {
		start, end = end, start
	}
	var out []string
	for a := start; ; a = a.Next() {
		out = append(out, a.String())
		if len(out) > maxHosts {
			return nil, httpx.BadRequest(fmt.Sprintf("range too large (max %d hosts)", maxHosts))
		}
		if a == end {
			break
		}
	}
	return out, nil
}
