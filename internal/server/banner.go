package server

import (
	"cmp"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
)

// listenURLs returns the browser URL the start-up banner shows (and opens) and, for a wildcard listener, the other
// URLs the server answers on. bound is the listener's address, configured the --listen value; addrs are the machine's
// interface addresses and hostname its host name (both only used for wildcard listeners).
//
// A wildcard listener (0.0.0.0, ::, or an empty host) is shown by its primary network address — a private IPv4 address
// first, then any other routable one — rather than a loopback address the operator cannot open from another machine;
// the host name, the remaining addresses and loopback are listed as alternatives. 0.0.0.0 lists IPv4 addresses only.
// Link-local addresses are skipped (they need a zone and rarely work in a browser).
func listenURLs(scheme, bound, configured string, addrs []netip.Addr, hostname string) (primary string, others []string) {
	host, port, err := net.SplitHostPort(bound)
	if err != nil {
		return scheme + "://" + bound + "/", nil
	}
	mk := func(h string) string { return scheme + "://" + net.JoinHostPort(h, port) + "/" }
	ch, _, _ := net.SplitHostPort(configured)
	if strings.EqualFold(ch, "localhost") {
		return mk("localhost"), nil
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil || !ip.IsUnspecified() {
		return mk(host), nil
	}
	cip, cerr := netip.ParseAddr(strings.Trim(ch, "[]"))
	v4Only := cerr == nil && cip.Is4()

	var cands []netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		if !a.IsValid() || a.IsUnspecified() || a.IsLinkLocalUnicast() || a.IsMulticast() || (v4Only && !a.Is4()) {
			continue
		}
		if !slices.Contains(cands, a) {
			cands = append(cands, a)
		}
	}
	if !slices.ContainsFunc(cands, netip.Addr.IsLoopback) {
		cands = append(cands, netip.AddrFrom4([4]byte{127, 0, 0, 1}))
	}
	slices.SortStableFunc(cands, func(a, b netip.Addr) int { return cmp.Compare(addrRank(a), addrRank(b)) })

	primary = mk(cands[0].String())
	if h := strings.ToLower(strings.TrimSpace(hostname)); h != "" && h != "localhost" && !cands[0].IsLoopback() {
		others = append(others, mk(h))
	}
	for _, a := range cands[1:] {
		others = append(others, mk(a.String()))
	}
	return primary, others
}

// addrRank orders candidate addresses for listenURLs: private IPv4, other IPv4, unique-local / global IPv6, loopback.
func addrRank(a netip.Addr) int {
	switch {
	case a.IsLoopback():
		if a.Is4() {
			return 4
		}
		return 5
	case a.Is4() && a.IsPrivate():
		return 0
	case a.Is4():
		return 1
	case a.IsPrivate():
		return 2
	default:
		return 3
	}
}

// interfaceAddrs lists the addresses of the machine's up interfaces (best effort).
func interfaceAddrs() []netip.Addr {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(ipn.IP); ok {
					out = append(out, ip.Unmap())
				}
			}
		}
	}
	return out
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}
