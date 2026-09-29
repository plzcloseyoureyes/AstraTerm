package tools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sync/semaphore"

	"github.com/nexterm/nexterm/internal/httpx"
)

type portscanRequest struct {
	Targets         string `json:"targets"` // host / list / CIDR / range
	Host            string `json:"host"`    // alias for a single target
	Ports           string `json:"ports"`   // "22,80,1000-2000,top100"
	TimeoutMs       int    `json:"timeoutMs"`
	Concurrency     int    `json:"concurrency"`
	Banner          bool   `json:"banner"`
	UDP             bool   `json:"udp"`
	ShowClosed      bool   `json:"showClosed"`
	ViaConnectionID string `json:"viaConnectionId"`
}

// dialFunc opens a TCP/UDP connection; the local variant uses net.Dialer, the SSH variant a gateway's direct-tcpip
// channel so the scan runs from the remote host's vantage point.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func preparePortscan(ctx context.Context, cl *call) (runner, error) {
	var req portscanRequest
	if err := decode(cl.body, &req); err != nil {
		return nil, err
	}
	spec := strings.TrimSpace(req.Targets)
	if spec == "" {
		spec = strings.TrimSpace(req.Host)
	}
	if spec == "" {
		return nil, httpx.BadRequest("a target host is required")
	}
	hosts, err := expandHosts(spec)
	if err != nil {
		return nil, err
	}
	ports, err := parsePorts(req.Ports)
	if err != nil {
		return nil, err
	}
	if err := checkProbeBudget(len(hosts), len(ports)); err != nil {
		return nil, err
	}
	timeout := time.Duration(clampInt(orDefault(req.TimeoutMs, 800), 50, 15000)) * time.Millisecond
	conc := clampInt(orDefault(req.Concurrency, 200), 1, 1024)
	if req.ViaConnectionID != "" {
		if req.UDP {
			return nil, httpx.BadRequest("UDP scanning is not supported through an SSH gateway")
		}
		if err := cl.checkSSHConnection(ctx, req.ViaConnectionID); err != nil {
			return nil, err
		}
		conc = min(conc, 64) // do not open an unreasonable number of simultaneous channels on the gateway
	}
	cl.target = spec
	cl.details = map[string]any{"ports": req.Ports, "hosts": len(hosts), "via": req.ViaConnectionID, "udp": req.UDP}
	return func(ctx context.Context, out *sink) error {
		dial, closeDial, err := cl.scanDialer(ctx, req.ViaConnectionID)
		if err != nil {
			return err
		}
		defer closeDial()
		return runPortscan(ctx, &req, hosts, ports, timeout, conc, dial, out)
	}, nil
}

func runPortscan(ctx context.Context, req *portscanRequest, hosts []string, ports []int, timeout time.Duration, conc int, dial dialFunc, out *sink) error {
	total := int64(len(hosts)) * int64(len(ports))
	proto := ternary(req.UDP, "UDP", "TCP")
	out.emitNow(row{"kind": "info", "message": fmt.Sprintf("Scanning %d host(s) × %d %s port(s) = %d probes", len(hosts), len(ports), proto, total)})

	sem := semaphore.NewWeighted(int64(conc))
	var wg sync.WaitGroup
	var open, done, skipped atomic.Int64

	// Progress ticker (at most ~6 rows per second).
	progressDone := make(chan struct{})
	go func() {
		t := time.NewTicker(150 * time.Millisecond)
		defer t.Stop()
		var last int64 = -1
		for {
			select {
			case <-progressDone:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if d := done.Load(); d != last {
					last = d
					out.add(row{"kind": "progress", "done": d, "total": total, "open": open.Load()})
				}
			}
		}
	}()

	for _, host := range hosts {
		if ctx.Err() != nil {
			break
		}
		ip := host
		if net.ParseIP(host) == nil && req.ViaConnectionID == "" {
			resolved, err := resolveOne(ctx, host, false)
			if err != nil {
				if ctx.Err() != nil {
					break
				}
				out.add(row{"kind": "hosterror", "host": host, "error": err.Error()})
				done.Add(int64(len(ports)))
				skipped.Add(1)
				continue
			}
			ip = resolved.String()
		}
		for _, port := range ports {
			if err := sem.Acquire(ctx, 1); err != nil {
				break
			}
			wg.Add(1)
			go func(host, ip string, port int) {
				defer wg.Done()
				defer sem.Release(1)
				r := scanPort(ctx, dial, host, ip, port, timeout, req.Banner, req.UDP)
				done.Add(1)
				if ctx.Err() != nil {
					return
				}
				if r["state"] == "open" {
					open.Add(1)
				}
				if r["state"] == "open" || r["state"] == "error" || req.ShowClosed {
					out.add(r)
				}
			}(host, ip, port)
		}
	}
	wg.Wait()
	close(progressDone)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	out.emitNow(row{"kind": "progress", "done": done.Load(), "total": total, "open": open.Load()})
	out.emitNow(row{"kind": "summary", "hosts": len(hosts), "ports": len(ports), "probes": total, "open": open.Load(), "unresolved": skipped.Load()})
	return nil
}

// scanDialer returns the dial function for a scan. Through an SSH gateway every probe is a direct-tcpip channel; the
// number of channel opens still pending on the gateway (including ones we stopped waiting for) is bounded so a scan of
// filtered ports cannot pile up thousands of stuck opens on the server.
func (cl *call) scanDialer(ctx context.Context, viaConnID string) (dialFunc, func(), error) {
	if viaConnID == "" {
		d := cl.guard.dialer(0)
		return d.DialContext, func() {}, nil
	}
	client, release, err := cl.sshClientFor(ctx, viaConnID)
	if err != nil {
		return nil, nil, err
	}
	pending := make(chan struct{}, 256)
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		select {
		case pending <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		type result struct {
			c   net.Conn
			err error
		}
		ch := make(chan result, 1)
		go func() {
			c, err := client.Dial(network, addr) // returns once the gateway answers the channel open
			<-pending
			ch <- result{c, err}
		}()
		select {
		case r := <-ch:
			return r.c, r.err
		case <-ctx.Done():
			go func() { // close a late success
				if r := <-ch; r.c != nil {
					_ = r.c.Close()
				}
			}()
			return nil, ctx.Err()
		}
	}, release, nil
}

// gatewayDialState classifies a failed channel open through an SSH gateway ("ssh: rejected: connect failed
// (Connection refused)") and other unexpected dial errors.
func gatewayDialState(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "refused"):
		return "closed"
	case strings.Contains(msg, "timed out"), strings.Contains(msg, "timeout"), strings.Contains(msg, "no route"),
		strings.Contains(msg, "unreachable"):
		return "filtered"
	default:
		return "closed"
	}
}

// transientDialErr reports local resource exhaustion (not a verdict about the port).
func transientDialErr(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) || errors.Is(err, syscall.ENOBUFS) ||
		errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.EAGAIN)
}

// scanPort probes one host:port and returns a "port" row with state open / closed / filtered (TCP) or open /
// closed / open|filtered (UDP), the service name and, when requested and available, a banner.
func scanPort(ctx context.Context, dial dialFunc, host, ip string, port int, timeout time.Duration, banner, udp bool) row {
	network := ternary(udp, "udp", "tcp")
	r := row{"kind": "port", "host": host, "ip": ip, "port": port, "service": serviceName(port, udp), "proto": network}
	var conn net.Conn
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		dctx, cancel := context.WithTimeout(ctx, timeout)
		conn, err = dial(dctx, network, net.JoinHostPort(ip, strconv.Itoa(port)))
		cancel()
		if err == nil || !transientDialErr(err) || ctx.Err() != nil {
			break
		}
		sleep(ctx, time.Duration(20*(attempt+1))*time.Millisecond)
	}
	if err != nil {
		switch {
		case isTimeout(err) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
			r["state"] = ternary(udp, "open|filtered", "filtered")
		case isRefused(err):
			r["state"] = "closed"
		case transientDialErr(err):
			r["state"] = "error"
			r["error"] = "local resource limit: " + err.Error()
		case isUnreachable(err):
			r["state"] = "filtered"
		default:
			r["state"] = gatewayDialState(err)
		}
		return r
	}
	defer conn.Close()
	if udp {
		// A UDP "connect" never fails; probe for a reply to distinguish open from open|filtered, and treat an ICMP
		// port unreachable (reported as ECONNREFUSED on the connected socket) as closed.
		r["state"] = udpProbe(conn, port, timeout)
		return r
	}
	r["state"] = "open"
	if banner {
		if b := grabBanner(conn, port, timeout); b != "" {
			r["banner"] = b
		}
	}
	return r
}

// httpProbePorts are plain-HTTP ports whose servers stay silent until they get a request.
var httpProbePorts = map[int]bool{80: true, 81: true, 591: true, 8000: true, 8008: true, 8080: true, 8081: true, 8888: true}

const httpProbe = "HEAD / HTTP/1.0\r\nHost: scan\r\nUser-Agent: NexTerm\r\n\r\n"

// tlsPorts speak TLS first; a plain-text probe only produces an alert there.
var tlsPorts = map[int]bool{443: true, 465: true, 636: true, 853: true, 990: true, 993: true, 995: true, 5061: true,
	5986: true, 6443: true, 8443: true, 9443: true}

// grabBanner reads a service greeting. Known HTTP ports get a minimal request right away; other ports that stay
// silent for a moment (HTTP, many APIs) get the same probe as a second chance.
func grabBanner(conn net.Conn, port int, timeout time.Duration) string {
	wait := min(timeout, 1200*time.Millisecond) + 300*time.Millisecond
	svc := serviceName(port, false)
	probed := false
	if httpProbePorts[port] || svc == "http" || svc == "http-alt" || svc == "http-proxy" {
		_, _ = conn.Write([]byte(httpProbe))
		probed = true
	} else {
		wait = min(wait, 600*time.Millisecond)
	}
	buf := make([]byte, 512)
	_ = conn.SetDeadline(time.Now().Add(wait))
	n, err := conn.Read(buf)
	if n <= 0 && !probed && !tlsPorts[port] && isTimeout(err) {
		_ = conn.SetDeadline(time.Now().Add(min(timeout, 1200*time.Millisecond)))
		if _, werr := conn.Write([]byte(httpProbe)); werr == nil {
			n, _ = conn.Read(buf)
		}
	}
	if n <= 0 {
		return ""
	}
	return sanitizeBanner(buf[:n])
}

// udpProbe sends a small datagram and classifies the answer.
func udpProbe(conn net.Conn, port int, timeout time.Duration) string {
	_ = conn.SetDeadline(time.Now().Add(min(timeout, 1500*time.Millisecond)))
	payload := udpPayloads[port]
	if payload == nil {
		payload = []byte{0}
	}
	if _, err := conn.Write(payload); err != nil {
		if isRefused(err) {
			return "closed"
		}
		return "open|filtered"
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	switch {
	case err == nil && n > 0:
		return "open"
	case err != nil && isRefused(err):
		return "closed"
	default:
		return "open|filtered"
	}
}

// udpPayloads are protocol-correct requests that make common UDP services reply (DNS version query, NTP client
// request, SNMP get sysDescr with community "public").
var udpPayloads = map[int][]byte{
	53: {0x13, 0x37, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x07, 'v', 'e', 'r', 's', 'i', 'o', 'n', 0x04, 'b', 'i', 'n', 'd', 0x00, 0x00, 0x10, 0x00, 0x03},
	123: append([]byte{0x1b}, make([]byte, 47)...),
	161: {0x30, 0x26, 0x02, 0x01, 0x01, 0x04, 0x06, 'p', 'u', 'b', 'l', 'i', 'c', 0xa0, 0x19, 0x02, 0x01, 0x01,
		0x02, 0x01, 0x00, 0x02, 0x01, 0x00, 0x30, 0x0e, 0x30, 0x0c, 0x06, 0x08, 0x2b, 0x06, 0x01, 0x02, 0x01, 0x01,
		0x01, 0x00, 0x05, 0x00},
}

// sanitizeBanner trims a raw banner to one line of printable characters (never leak control sequences to the UI).
func sanitizeBanner(b []byte) string {
	s := string(b)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r == utf8.RuneError:
			sb.WriteRune('.')
		case unicode.IsPrint(r):
			sb.WriteRune(r)
		default:
			sb.WriteRune('.')
		}
	}
	out := strings.TrimSpace(sb.String())
	if utf8.RuneCountInString(out) > 200 {
		out = string([]rune(out)[:200]) + "…"
	}
	return out
}
