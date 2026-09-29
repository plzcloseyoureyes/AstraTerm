package tools

import (
	"context"
	"net"
	"net/netip"
	"syscall"
	"time"

	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/netguard"
)

// netGuard adapts the shared destination policy (internal/netguard, SEC-7) to the tools: in server mode, ordinary
// users' probes must not reach the NexTerm host itself (loopback, its own addresses and listener), unspecified,
// link-local or cloud-metadata addresses, nor anything the administrator's network policy refuses. The check runs
// on the address actually dialed (net.Dialer.Control), so DNS rebinding and HTTP redirects cannot bypass it. A nil
// *netGuard allows everything (desktop mode, admins unless the policy applies to them); a zero netGuard applies the
// default policy (tests).
type netGuard struct{ g *netguard.Guard }

// guardFor returns the policy for user: nil (unrestricted) in desktop mode and for admins (unless applyToAdmins).
func (h *handler) guardFor(user *model.User) *netGuard {
	if h.isDesktop() {
		return nil
	}
	g := netguard.For(h.d).ForUser(user)
	if g == nil {
		return nil
	}
	return &netGuard{g: g}
}

// guard returns the shared guard (nil = unrestricted).
func (ng *netGuard) guard() *netguard.Guard {
	switch {
	case ng == nil:
		return nil
	case ng.g == nil:
		return &netguard.Guard{} // default policy
	}
	return ng.g
}

// blockedAddr reports whether a is off-limits under the default policy.
func blockedAddr(a netip.Addr) bool { return !(&netguard.Guard{}).Check(a, 0).Allowed }

// checkIP returns an error when ip is off-limits (always nil for a nil guard).
func (ng *netGuard) checkIP(ip net.IP) error { return ng.guard().CheckIP(ip, 0) }

// control is a net.Dialer.Control hook: it vets the concrete address being connected to.
func (ng *netGuard) control(network, address string, c syscall.RawConn) error {
	return ng.guard().Control(network, address, c)
}

// dialer returns a net.Dialer with the given timeout that enforces the guard on every connection attempt.
func (ng *netGuard) dialer(timeout time.Duration) *net.Dialer { return ng.guard().Dialer(timeout) }

// resolve resolves host to one IP (preferring IPv6 when asked) and vets it.
func (ng *netGuard) resolve(ctx context.Context, host string, ipv6 bool) (net.IP, error) {
	ip, err := resolveOne(ctx, host, ipv6)
	if err != nil {
		return nil, err
	}
	if err := ng.checkIP(ip); err != nil {
		return nil, err
	}
	return ip, nil
}

// checkSSHConnection validates a "run via" SSH connection synchronously: it must exist, be visible to the caller, be
// an SSH connection and have decryptable secrets (423 when the vault is locked, so the UI can offer to unlock). The
// actual dial (which may prompt for host keys / passwords) happens later, inside the job.
func (cl *call) checkSSHConnection(ctx context.Context, connID string) error {
	if cl.h == nil || cl.h.d == nil || cl.h.c == nil || cl.h.c.SSH == nil {
		return httpx.BadRequest("SSH is not available")
	}
	conn, _, err := cl.h.d.ResolveConnection(ctx, cl.user, connID)
	if err != nil {
		return err
	}
	switch conn.Protocol {
	case model.ProtoSSH, model.ProtoSFTP, model.ProtoMosh:
		return nil
	default:
		return httpx.BadRequest("connection " + conn.Name + " is not an SSH connection")
	}
}
