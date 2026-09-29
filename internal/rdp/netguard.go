package rdp

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/netguard"
)

// Destination policy (SEC-7, internal/netguard) for RDP.
//
//   - IronRDP relay, certificate probe and the gateway forwarder dial through dialConnection: the sshx route (direct
//     dials and the first hop / proxy vetted in net.Dialer.Control), or a guarded dialer without a pool.
//   - guacd connects to its "hostname" itself, out of Termstead's reach. For a restricted user on a direct route the
//     destination is resolved and vetted here before guacd gets it, and the vetted IP is handed to guacd as its
//     hostname (pinning: a DNS answer changing between the check and guacd's own lookup cannot redirect guacd). The
//     host name stays in the ticket for display, audit and certificate trust (Termstead's probe verifies the certificate
//     by name; guacd then skips its own verification). Routed connections reach guacd through the loopback forwarder,
//     whose dial is guarded. An RD Gateway (gateway-hostname), which guacd also connects to directly, is resolved
//     and vetted the same way but not pinned (its HTTPS certificate is checked against the name).

// guard returns the destination guard of u (nil = unrestricted: desktop mode, administrators).
func (h *handler) guard(u *model.User) *netguard.Guard {
	if h == nil {
		return nil
	}
	return netguard.ForUser(h.d, u)
}

// isBlocked reports whether err is a destination policy refusal.
func isBlocked(err error) bool {
	_, ok := netguard.IsBlocked(err)
	return ok
}

// guacdResolveTimeout bounds the resolution of the destination before it is handed to guacd.
const guacdResolveTimeout = 10 * time.Second

// lookupNetIP resolves host names for pinning (a variable for tests).
var lookupNetIP = net.DefaultResolver.LookupNetIP

// guacdDestination vets what guacd will connect to for tk and returns the host to hand to guacd: tk.host unchanged
// for unrestricted users and routed connections (the forwarder's dial is guarded), otherwise the vetted IP.
func (h *handler) guacdDestination(ctx context.Context, u *model.User, tk *ticket, opts rdpOptions) (string, error) {
	g := h.guard(u)
	if g == nil {
		return tk.host, nil
	}
	if gw := strings.TrimSpace(opts.GatewayHost); gw != "" {
		port := opts.GatewayPort
		if port <= 0 {
			port = 443
		}
		if _, err := pinDestination(ctx, g, gw, port); err != nil {
			if isBlocked(err) {
				return "", err
			}
			return "", fmt.Errorf("RD Gateway: %w", err)
		}
	}
	if routeDescription(tk.conn) != "" {
		return tk.host, nil
	}
	ip, err := pinDestination(ctx, g, tk.host, tk.port)
	if err != nil {
		return "", err
	}
	return ip.String(), nil
}

// pinDestination resolves host (IP literals in any form, localhost names, DNS) and checks every address against g
// (like Guard.CheckHostPort, one refused address refuses the destination). It returns the address to connect to,
// preferring IPv4 (guacd often runs in a container without IPv6).
func pinDestination(ctx context.Context, g *netguard.Guard, host string, port int) (netip.Addr, error) {
	if err := g.CheckLiteral(host, port); err != nil {
		return netip.Addr{}, err
	}
	if a, ok := netguard.ParseHostIP(host); ok {
		return a.Unmap(), nil
	}
	cctx, cancel := context.WithTimeout(ctx, guacdResolveTimeout)
	defer cancel()
	ips, err := lookupNetIP(cctx, "ip", strings.Trim(strings.TrimSpace(host), "[]"))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("cannot resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return netip.Addr{}, fmt.Errorf("cannot resolve %s: no addresses", host)
	}
	var pick netip.Addr
	for _, ip := range ips {
		ip = ip.Unmap()
		if err := g.CheckAddr(ip, port); err != nil {
			return netip.Addr{}, err
		}
		if !pick.IsValid() || (!pick.Is4() && ip.Is4()) {
			pick = ip
		}
	}
	return pick.WithZone(""), nil
}
