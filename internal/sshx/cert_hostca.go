package sshx

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Host certificates, @cert-authority and @revoked markers (SSH-18/20). The markers live in the keys module (its
// known-hosts manager), which registers them with Pool.SetHostKeyMarkers; hostKeyChecker.check consults them before the
// regular known_hosts lookup:
//
//   - a host key (or a host certificate's key or CA) marked @revoked is always rejected;
//   - a host certificate signed by a CA trusted for the host (principal = host name, validity window, signature) is
//     accepted without a prompt;
//   - any other certificate is verified as its plain host key, like OpenSSH does.
//
// When a CA covers the host, certificate host-key algorithms are offered first so the server presents its certificate.

// HostKeyMarkers supplies the @cert-authority and @revoked known_hosts markers. Implementations must be fast and
// safe for concurrent use (they run inside SSH handshakes).
type HostKeyMarkers interface {
	// HostAuthorities returns the CA keys trusted to sign host certificates for host:port.
	HostAuthorities(host string, port int) []ssh.PublicKey
	// IsRevoked reports whether key is marked @revoked for host:port.
	IsRevoked(host string, port int, key ssh.PublicKey) bool
}

// providers holds the per-pool registrations of the keys module (a process may run several AstraTerm instances, e.g.
// in tests, each with its own store): host key markers and the built-in agent.
type providers struct {
	markers HostKeyMarkers
	agent   BuiltinAgent
}

var poolProviders sync.Map // *Pool → *providers (copy-on-write values)

func (p *Pool) setProvider(update func(pr *providers)) {
	if p == nil {
		return
	}
	next := &providers{}
	if cur, ok := poolProviders.Load(p); ok {
		*next = *cur.(*providers)
	} else if p.ctx != nil {
		context.AfterFunc(p.ctx, func() { poolProviders.Delete(p) })
	}
	update(next)
	poolProviders.Store(p, next)
}

func (p *Pool) provider() *providers {
	if p != nil {
		if cur, ok := poolProviders.Load(p); ok {
			return cur.(*providers)
		}
	}
	return &providers{}
}

// SetHostKeyMarkers registers the marker provider of the pool (nil unregisters it).
func (p *Pool) SetHostKeyMarkers(m HostKeyMarkers) {
	p.setProvider(func(pr *providers) { pr.markers = m })
}

// checkMarkers runs before the known_hosts check. It returns done=true with the verdict when a marker (or an
// accepted certificate) decides; otherwise *key may have been replaced by a certificate's plain key, which is then
// verified like any other host key.
func (h *hostKeyChecker) checkMarkers(key *ssh.PublicKey) (done bool, err error) {
	m := h.p.provider().markers
	cert, isCert := (*key).(*ssh.Certificate)
	if m != nil {
		revoked := m.IsRevoked(h.host, h.port, *key)
		if isCert {
			revoked = revoked || m.IsRevoked(h.host, h.port, cert.Key) ||
				(cert.SignatureKey != nil && m.IsRevoked(h.host, h.port, cert.SignatureKey))
		}
		if revoked {
			fp := ssh.FingerprintSHA256(*key)
			if isCert {
				fp = ssh.FingerprintSHA256(cert.Key)
			}
			h.p.audit(h.ctx, h.user, "ssh.hostkey.revoked", h.target(), map[string]any{"fingerprint": fp})
			return true, &hostKeyError{msg: fmt.Sprintf("the host key of %s (%s) has been revoked", h.target(), fp)}
		}
	}
	if !isCert {
		if prev, ok := h.accepted.(*ssh.Certificate); ok && bytes.Equal(prev.Key.Marshal(), (*key).Marshal()) {
			return true, nil // re-key: the host key accepted through its certificate, presented without it
		}
		return false, nil
	}
	if h.accepted != nil {
		// Re-key: the same certificate, or a renewed certificate of the same host key, is fine.
		if bytes.Equal(h.accepted.Marshal(), cert.Marshal()) {
			return true, nil
		}
		if prev, ok := h.accepted.(*ssh.Certificate); ok && bytes.Equal(prev.Key.Marshal(), cert.Key.Marshal()) {
			return true, nil
		}
		*key = cert.Key
		return false, nil
	}
	if m != nil && cert.CertType == ssh.HostCert {
		if cas := m.HostAuthorities(h.host, h.port); len(cas) > 0 {
			checker := &ssh.CertChecker{IsHostAuthority: func(auth ssh.PublicKey, _ string) bool { return containsKey(cas, auth) }}
			err := checker.CheckHostKey(net.JoinHostPort(h.host, strconv.Itoa(h.port)), nil, cert)
			if err == nil {
				h.accept(cert, ssh.FingerprintSHA256(cert.Key))
				if h.session != nil {
					h.session.Notice(certNotice(h.target(), cert))
				}
				return true, nil
			}
			if containsKey(cas, cert.SignatureKey) && h.session != nil {
				h.session.Notice(fmt.Sprintf("The host certificate of %s was rejected (%s); verifying its host key instead.",
					h.target(), strings.TrimPrefix(err.Error(), "ssh: ")))
			}
		}
	}
	// No trusted CA vouches for the certificate: verify its plain host key (OpenSSH semantics).
	*key = cert.Key
	return false, nil
}

func certNotice(target string, cert *ssh.Certificate) string {
	until := "no expiry"
	if cert.ValidBefore != ssh.CertTimeInfinity && cert.ValidBefore < 1<<62 {
		until = "valid until " + time.Unix(int64(cert.ValidBefore), 0).UTC().Format("2006-01-02 15:04 MST")
	}
	return fmt.Sprintf("Host certificate of %s signed by trusted CA %s (%s).", target, ssh.FingerprintSHA256(cert.SignatureKey), until)
}

func containsKey(keys []ssh.PublicKey, k ssh.PublicKey) bool {
	if k == nil {
		return false
	}
	b := k.Marshal()
	for _, c := range keys {
		if c != nil && bytes.Equal(c.Marshal(), b) {
			return true
		}
	}
	return false
}

// algorithms returns the host-key algorithms to offer: hostKeyAlgorithms' ordering, with certificate algorithms
// moved first when a trusted CA covers the host (SSH-20: "cert host algorithms go first").
func (h *hostKeyChecker) algorithms(known []*model.KnownHost, opts model.Options) []string {
	algos := hostKeyAlgorithms(known, opts)
	m := h.p.provider().markers
	if m == nil || algos == nil || len(m.HostAuthorities(h.host, h.port)) == 0 {
		return algos // nil = library defaults, which already list certificate algorithms first
	}
	out := make([]string, 0, len(algos))
	for _, a := range algos {
		if strings.Contains(a, "-cert-v01@openssh.com") {
			out = append(out, a)
		}
	}
	for _, a := range algos {
		if !strings.Contains(a, "-cert-v01@openssh.com") {
			out = append(out, a)
		}
	}
	return out
}
