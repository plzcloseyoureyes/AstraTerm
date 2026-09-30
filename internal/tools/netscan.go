package tools

import (
	"context"
	"fmt"
	"maps"
	"math/rand/v2"
	"net"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/sync/semaphore"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
)

type netscanRequest struct {
	CIDR            string `json:"cidr"`
	Targets         string `json:"targets"` // alias: CIDR / range / list
	Ports           string `json:"ports"`   // ports to probe on each host (default: a small common set)
	TimeoutMs       int    `json:"timeoutMs"`
	Concurrency     int    `json:"concurrency"`
	ResolveNames    *bool  `json:"resolveNames"` // reverse DNS + NetBIOS + mDNS names (default true)
	NoICMP          bool   `json:"noIcmp"`
	AllHosts        bool   `json:"allHosts"` // stream down hosts too
	ViaConnectionID string `json:"viaConnectionId"`
}

// netscanDefaultPorts is the small set probed on each host for liveness + a service hint (SSH, Telnet, RDP, VNC, FTP,
// HTTP/S, SMB, …) per RESEARCH TOOL-4.
var netscanDefaultPorts = []int{21, 22, 23, 25, 53, 80, 139, 443, 445, 3306, 3389, 5432, 5900, 8080}

func prepareNetscan(ctx context.Context, cl *call) (runner, error) {
	var req netscanRequest
	if err := decode(cl.body, &req); err != nil {
		return nil, err
	}
	spec := strings.TrimSpace(req.CIDR)
	if spec == "" {
		spec = strings.TrimSpace(req.Targets)
	}
	if spec == "" {
		return nil, httpx.BadRequest("a CIDR, range or host list is required")
	}
	hosts, err := expandHosts(spec)
	if err != nil {
		return nil, err
	}
	ports := netscanDefaultPorts
	if strings.TrimSpace(req.Ports) != "" {
		if ports, err = parsePorts(req.Ports); err != nil {
			return nil, err
		}
	}
	if err := checkProbeBudget(len(hosts), len(ports)); err != nil {
		return nil, err
	}
	timeout := time.Duration(min(max(orDefault(req.TimeoutMs, 600), 50), 10000)) * time.Millisecond
	conc := min(max(orDefault(req.Concurrency, 256), 1), 1024)
	if req.ViaConnectionID != "" {
		if err := cl.checkSSHConnection(ctx, req.ViaConnectionID); err != nil {
			return nil, err
		}
		conc = min(conc, 64)
	}
	cl.target = spec
	cl.details = map[string]any{"hosts": len(hosts), "via": req.ViaConnectionID}
	return func(ctx context.Context, out *sink) error {
		dial, closeDial, err := cl.scanDialer(ctx, req.ViaConnectionID)
		if err != nil {
			return err
		}
		defer closeDial()
		return runNetscan(ctx, &req, hosts, ports, timeout, conc, dial, out)
	}, nil
}

// scanHost aggregates the probes of one host.
type scanHost struct {
	ip      net.IP // nil for a name dialed through an SSH gateway (the gateway resolves it)
	target  string // the name the user gave, when it was not an IP
	pending atomic.Int32
	mu      sync.Mutex
	open    []int
	refused bool
}

// addr is what gets dialed: the IP, or the gateway-resolved name.
func (h *scanHost) addr() string {
	if h.ip != nil {
		return h.ip.String()
	}
	return h.target
}

func runNetscan(ctx context.Context, req *netscanRequest, targets []string, ports []int, timeout time.Duration, conc int, dial dialFunc, out *sink) error {
	via := req.ViaConnectionID != ""
	resolveNames := req.ResolveNames == nil || *req.ResolveNames
	out.emitNow(row{"kind": "info", "message": fmt.Sprintf("Sweeping %d host(s), %d port(s) each", len(targets), len(ports))})

	// Resolve to IPs (CIDR/range entries already are IPs).
	hosts := make([]*scanHost, 0, len(targets))
	for _, t := range targets {
		if ip := net.ParseIP(t); ip != nil {
			hosts = append(hosts, &scanHost{ip: ip})
			continue
		}
		if via { // names are resolved by the SSH gateway (they may exist only on its network)
			hosts = append(hosts, &scanHost{target: t})
			continue
		}
		ip, err := resolveOne(ctx, t, false)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			out.add(row{"kind": "hosterror", "host": t, "error": err.Error()})
			continue
		}
		hosts = append(hosts, &scanHost{ip: ip, target: t})
	}

	// ICMP sweep (best effort, local only) to catch hosts that answer ping but expose none of the probed ports.
	pinged := map[string]time.Duration{}
	if !req.NoICMP && !via {
		ips := make([]net.IP, 0, len(hosts))
		for _, h := range hosts {
			if h.ip != nil {
				ips = append(ips, h.ip)
			}
		}
		pinged = icmpSweep(ctx, ips, timeout)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	total := int64(len(hosts))
	var doneHosts, alive atomic.Int64
	progressDone := make(chan struct{})
	go func() {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		var last int64 = -1
		for {
			select {
			case <-progressDone:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if d := doneHosts.Load(); d != last {
					last = d
					out.add(row{"kind": "progress", "done": d, "total": total, "alive": alive.Load()})
				}
			}
		}
	}()

	var aliveMu sync.Mutex
	var aliveIPs []string
	nameSem := semaphore.NewWeighted(32)
	var nameWG sync.WaitGroup
	finish := func(h *scanHost) {
		ipStr := h.addr()
		h.mu.Lock()
		open := append([]int(nil), h.open...)
		refused := h.refused
		h.mu.Unlock()
		sort.Ints(open)
		rtt, isPinged := pinged[ipStr]
		isAlive := isPinged || len(open) > 0 || refused
		doneHosts.Add(1)
		if !isAlive && !req.AllHosts {
			return
		}
		r := row{"kind": "host", "ip": ipStr, "alive": isAlive, "ports": open}
		if h.target != "" {
			r["target"] = h.target
		}
		if isPinged {
			r["rttMs"] = ms(rtt)
		}
		if isAlive {
			alive.Add(1)
			aliveMu.Lock()
			aliveIPs = append(aliveIPs, ipStr)
			aliveMu.Unlock()
		}
		if !isAlive || !resolveNames {
			out.add(r)
			return
		}
		// Names are looked up off the probe path so slow resolvers do not hold probe slots.
		nameWG.Go(func() {
			if err := nameSem.Acquire(ctx, 1); err != nil {
				out.add(r)
				return
			}
			defer nameSem.Release(1)
			if h.ip != nil {
				maps.Copy(r, lookupHostNames(ctx, h.ip, !via))
			}
			out.add(r)
		})
	}

	sem := semaphore.NewWeighted(int64(conc))
	var wg sync.WaitGroup
	for _, h := range hosts {
		h.pending.Store(int32(len(ports)))
	}
probe:
	for _, h := range hosts {
		for _, port := range ports {
			if err := sem.Acquire(ctx, 1); err != nil {
				break probe
			}
			wg.Add(1)
			go func(h *scanHost, port int) {
				defer wg.Done()
				defer sem.Release(1)
				open, refused := probePort(ctx, dial, h.addr(), port, timeout, via)
				if ctx.Err() != nil {
					return
				}
				h.mu.Lock()
				if open {
					h.open = append(h.open, port)
				}
				h.refused = h.refused || refused
				h.mu.Unlock()
				if h.pending.Add(-1) == 0 {
					finish(h)
				}
			}(h, port)
		}
	}
	wg.Wait()
	nameWG.Wait()
	close(progressDone)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Annotate discovered hosts with MAC + vendor from the ARP / neighbor cache (warmed by the probes above).
	if !via {
		arp := arpTable(ctx)
		aliveMu.Lock()
		discovered := append([]string(nil), aliveIPs...)
		aliveMu.Unlock()
		for _, ipStr := range discovered {
			if mac, ok := arp[ipStr]; ok {
				r := row{"kind": "mac", "ip": ipStr, "mac": mac}
				if v := vendorForMAC(mac); v != "" {
					r["vendor"] = v
				}
				out.add(r)
			}
		}
	}
	out.emitNow(row{"kind": "progress", "done": doneHosts.Load(), "total": total, "alive": alive.Load()})
	out.emitNow(row{"kind": "summary", "scanned": total, "alive": alive.Load()})
	return nil
}

// probePort TCP-connects to ip:port: open, or refused (an RST — or a gateway reporting "connection refused" —
// proves the host is up even if the port is closed).
func probePort(ctx context.Context, dial dialFunc, addr string, port int, timeout time.Duration, via bool) (open, refused bool) {
	for attempt := range 5 {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		conn, err := dial(cctx, "tcp", net.JoinHostPort(addr, strconv.Itoa(port)))
		cancel()
		if err == nil {
			_ = conn.Close()
			return true, false
		}
		if transientDialErr(err) && ctx.Err() == nil {
			sleep(ctx, time.Duration(20*(attempt+1))*time.Millisecond)
			continue
		}
		if ctx.Err() != nil || isTimeout(err) {
			return false, false
		}
		return false, isRefused(err) || via && strings.Contains(strings.ToLower(err.Error()), "refused")
	}
	return false, false
}

// icmpSweep sends one echo request to every IPv4 target over a single socket (paced, with the reader running
// concurrently so replies are not dropped) and returns the responders with their RTT (RESEARCH TOOL-4 "ping
// sweep"). Unprivileged where possible; on failure it returns an empty map and the caller relies on TCP probes.
func icmpSweep(ctx context.Context, ips []net.IP, timeout time.Duration) map[string]time.Duration {
	alive := map[string]time.Duration{}
	mode := icmpMode(false)
	if mode == "" {
		return alive
	}
	network := ternary(mode == "udp", "udp4", "ip4:icmp")
	conn, err := icmp.ListenPacket(network, "0.0.0.0")
	if err != nil {
		return alive
	}
	id := rand.IntN(0xffff) + 1
	type probe struct {
		ip net.IP
		at time.Time
	}
	var mu sync.Mutex
	sent := map[int]probe{}
	// Linux ping sockets rewrite the echo identifier; elsewhere it is ours and filters other processes' replies.
	checkID := !(mode == "udp" && runtime.GOOS == "linux")

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		rb := make([]byte, 1500)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, peer, err := conn.ReadFrom(rb)
			if err != nil {
				if isTimeout(err) && ctx.Err() == nil {
					continue
				}
				return // closed
			}
			b := rb[:n]
			if len(b) >= 20 && b[0]>>4 == 4 { // raw socket with IPv4 header
				b = b[int(b[0]&0x0f)*4:]
			}
			m, err := icmp.ParseMessage(1, b)
			if err != nil || m.Type != ipv4.ICMPTypeEchoReply {
				continue
			}
			e, ok := m.Body.(*icmp.Echo)
			if !ok || checkID && e.ID != id {
				continue
			}
			from := addrIP(peer)
			mu.Lock()
			if p, ok := sent[e.Seq]; ok && (from == nil || from.Equal(p.ip)) {
				if _, dup := alive[p.ip.String()]; !dup {
					alive[p.ip.String()] = time.Since(p.at)
				}
			}
			mu.Unlock()
		}
	}()

	seq := 0
	for _, ip := range ips {
		if ctx.Err() != nil {
			break
		}
		if ip.To4() == nil {
			continue
		}
		seq++
		wb, err := (&icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: id, Seq: seq, Data: []byte("astraterm-netscan")}}).Marshal(nil)
		if err != nil {
			continue
		}
		var addr net.Addr = &net.IPAddr{IP: ip}
		if mode == "udp" {
			addr = &net.UDPAddr{IP: ip}
		}
		mu.Lock()
		sent[seq] = probe{ip: ip, at: time.Now()}
		mu.Unlock()
		if _, err := conn.WriteTo(wb, addr); err != nil {
			mu.Lock()
			delete(sent, seq)
			mu.Unlock()
		}
		if seq%32 == 0 {
			sleep(ctx, 5*time.Millisecond) // pace: ~6k packets/s, gentle on switches and the socket buffer
		}
	}
	// Wait for stragglers: until everything answered or the timeout after the last send.
	deadline := time.Now().Add(timeout + 300*time.Millisecond)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		mu.Lock()
		all := len(alive) >= len(sent)
		mu.Unlock()
		if all {
			break
		}
		sleep(ctx, 20*time.Millisecond)
	}
	_ = conn.Close()
	<-readerDone
	mu.Lock()
	defer mu.Unlock()
	out := make(map[string]time.Duration, len(alive))
	maps.Copy(out, alive)
	return out
}
