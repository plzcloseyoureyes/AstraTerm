package tools

import (
	"context"
	"errors"
	"math"
	"net"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nexterm/nexterm/internal/httpx"
)

// ms converts a duration to milliseconds rounded to three decimals (µs precision) for JSON.
func ms(d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return math.Round(float64(d.Microseconds())) / 1000
}

// round1 rounds to one decimal place.
func round1(v float64) float64 { return math.Round(v*10) / 10 }

// round3 rounds to three decimal places.
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func sqrt(v float64) float64 { return math.Sqrt(v) }

// clampInt clamps v into [lo, hi].
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func orString(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// sleep waits for d or until ctx is done; it returns false when ctx was cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// isTimeout reports whether err is a network timeout.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// isRefused reports an actively refused connection (RST / ICMP port unreachable).
func isRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// isUnreachable reports "no route" style failures (ICMP host/net unreachable reported by the stack).
func isUnreachable(err error) bool {
	return errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH)
}

// resolveOne resolves host to a single IP (host may already be a literal). ipv6 prefers an AAAA result.
func resolveOne(ctx context.Context, host string, ipv6 bool) (net.IP, error) {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if ip := net.ParseIP(host); ip != nil {
		return ip, nil
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(rctx, host)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, httpx.BadRequest("cannot resolve " + host + ": " + dnsErrText(err))
	}
	if len(ips) == 0 {
		return nil, httpx.BadRequest("no addresses for " + host)
	}
	// Prefer the requested family, else the first result.
	for _, a := range ips {
		if (a.IP.To4() == nil) == ipv6 {
			return a.IP, nil
		}
	}
	return ips[0].IP, nil
}

// dnsErrText shortens resolver errors ("lookup x on 1.2.3.4:53: no such host" → "no such host").
func dnsErrText(err error) string {
	var de *net.DNSError
	if errors.As(err, &de) {
		if de.IsNotFound {
			return "no such host"
		}
		return de.Err
	}
	return err.Error()
}

// reverseDNS returns the first PTR name for ip (without the trailing dot), or "".
func reverseDNS(ctx context.Context, ip net.IP) string {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	names, err := net.DefaultResolver.LookupAddr(c, ip.String())
	if err != nil || len(names) == 0 {
		return ""
	}
	return trimDot(names[0])
}

// rdnsCache resolves and memoizes reverse DNS names; lookups run in the background (bounded) so a slow resolver
// never stalls a probe loop.
type rdnsCache struct {
	mu      sync.Mutex
	names   map[string]string // ip → name ("" = no PTR)
	pending map[string]bool
	sem     chan struct{}
}

func newRDNSCache() *rdnsCache {
	return &rdnsCache{names: map[string]string{}, pending: map[string]bool{}, sem: make(chan struct{}, 4)}
}

// lookup returns the cached name of ip and whether it is known; unknown IPs are queued for resolution.
func (c *rdnsCache) lookup(ctx context.Context, ip string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n, ok := c.names[ip]; ok {
		return n, true
	}
	if !c.pending[ip] {
		c.pending[ip] = true
		go func() {
			select {
			case c.sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-c.sem }()
			name := ""
			if p := net.ParseIP(ip); p != nil {
				name = reverseDNS(ctx, p)
			}
			c.mu.Lock()
			c.names[ip] = name
			delete(c.pending, ip)
			c.mu.Unlock()
		}()
	}
	return "", false
}

// wait blocks until every queued lookup finished or d elapsed.
func (c *rdnsCache) wait(ctx context.Context, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		c.mu.Lock()
		n := len(c.pending)
		c.mu.Unlock()
		if n == 0 {
			return
		}
		sleep(ctx, 20*time.Millisecond)
	}
}

func trimDot(s string) string {
	return strings.TrimRight(s, ".")
}

// safeHost matches host arguments (IPv4/IPv6/hostnames) that are safe to pass to a remote shell or a local program.
// It rejects shell metacharacters and a leading '-' (which the program would parse as an option).
var safeHost = regexp.MustCompile(`^[A-Za-z0-9_.:%\[\]][A-Za-z0-9._:\-\[\]%]{0,254}$`)

// safeHostArg validates a host that will be placed on a command line (always quoted as well).
func safeHostArg(host string) (string, error) {
	host = strings.TrimSpace(host)
	if !safeHost.MatchString(host) {
		return "", httpx.BadRequest("invalid host: use a hostname or an IP address")
	}
	return host, nil
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func ternary[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// addrIP extracts the IP of a packet peer address.
func addrIP(a net.Addr) net.IP {
	switch v := a.(type) {
	case *net.UDPAddr:
		return v.IP
	case *net.IPAddr:
		return v.IP
	}
	if a == nil {
		return nil
	}
	if host, _, err := net.SplitHostPort(a.String()); err == nil {
		return net.ParseIP(host)
	}
	return net.ParseIP(a.String())
}
