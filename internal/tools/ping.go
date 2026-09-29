package tools

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	probing "github.com/prometheus-community/pro-bing"
	"golang.org/x/net/icmp"

	"github.com/nexterm/nexterm/internal/httpx"
)

type pingRequest struct {
	Host            string `json:"host"`
	Count           int    `json:"count"`
	IntervalMs      int    `json:"intervalMs"`
	TimeoutMs       int    `json:"timeoutMs"`
	Size            int    `json:"size"`
	Mode            string `json:"mode"` // "icmp" (default) | "tcp"
	Port            int    `json:"port"` // TCP-connect ping port (default 80)
	IPv6            bool   `json:"ipv6"` // prefer IPv6 when resolving
	ViaConnectionID string `json:"viaConnectionId"`
}

func (r *pingRequest) normalize() error {
	r.Host = strings.TrimSpace(r.Host)
	if r.Host == "" {
		return httpx.BadRequest("host is required")
	}
	if r.Count <= 0 {
		r.Count = 4
	}
	r.Count = min(r.Count, 1000)
	if r.IntervalMs <= 0 {
		r.IntervalMs = 1000
	}
	r.IntervalMs = clampInt(r.IntervalMs, 100, 60000)
	if r.TimeoutMs <= 0 {
		r.TimeoutMs = 2000
	}
	r.TimeoutMs = clampInt(r.TimeoutMs, 100, 60000)
	if r.Port == 0 {
		r.Port = 80
	}
	if r.Port < 1 || r.Port > 65535 {
		return httpx.BadRequest("port must be between 1 and 65535")
	}
	if r.Size < 0 || r.Size > 65000 {
		return httpx.BadRequest("size must be between 0 and 65000")
	}
	switch r.Mode = strings.ToLower(strings.TrimSpace(r.Mode)); r.Mode {
	case "", "icmp":
		r.Mode = "icmp"
	case "tcp":
	default:
		return httpx.BadRequest("mode must be icmp or tcp")
	}
	return nil
}

func preparePing(ctx context.Context, cl *call) (runner, error) {
	var req pingRequest
	if err := decode(cl.body, &req); err != nil {
		return nil, err
	}
	if err := req.normalize(); err != nil {
		return nil, err
	}
	cl.target = req.Host
	if req.ViaConnectionID != "" {
		host, err := safeHostArg(req.Host)
		if err != nil {
			return nil, err
		}
		req.Host = host
		if err := cl.checkSSHConnection(ctx, req.ViaConnectionID); err != nil {
			return nil, err
		}
		cl.details = map[string]any{"via": req.ViaConnectionID}
		return func(ctx context.Context, out *sink) error { return runPingViaSSH(ctx, cl, &req, out) }, nil
	}
	guard := cl.guard
	if req.Mode == "tcp" {
		return func(ctx context.Context, out *sink) error { return tcpPing(ctx, guard, &req, out) }, nil
	}
	return func(ctx context.Context, out *sink) error {
		mode := icmpMode(req.IPv6)
		if mode == "" {
			out.emitNow(row{"kind": "info", "message": "ICMP is not available to this process; using a TCP-connect ping on port " + strconv.Itoa(req.Port)})
			return tcpPing(ctx, guard, &req, out)
		}
		return icmpPing(ctx, &req, mode == "ip", out)
	}, nil
}

// icmpPing runs an ICMP echo ping (pro-bing) and streams each reply plus a final summary. privileged selects raw
// sockets (root / CAP_NET_RAW, Windows) instead of unprivileged datagram ICMP sockets.
func icmpPing(ctx context.Context, req *pingRequest, privileged bool, out *sink) error {
	pinger := probing.New(req.Host)
	pinger.SetPrivileged(privileged)
	pinger.Count = req.Count
	pinger.Interval = time.Duration(req.IntervalMs) * time.Millisecond
	// Overall deadline: time for every interval plus one reply timeout of slack.
	pinger.Timeout = time.Duration(req.Count)*pinger.Interval + time.Duration(req.TimeoutMs)*time.Millisecond
	pinger.RecordRtts = false
	if req.Size > 0 {
		pinger.Size = clampInt(req.Size, 24, 65000)
	}
	if req.IPv6 {
		pinger.SetNetwork("ip6")
	}
	pinger.OnSetup = func() {
		out.emitNow(row{"kind": "info", "message": fmt.Sprintf("PING %s (%s): %d data bytes", req.Host, pinger.IPAddr(), pinger.Size)})
	}
	pinger.OnRecv = func(pkt *probing.Packet) {
		out.add(row{"kind": "reply", "seq": pkt.Seq, "from": pkt.IPAddr.String(), "rttMs": ms(pkt.Rtt), "ttl": pkt.TTL, "bytes": pkt.Nbytes})
	}
	pinger.OnDuplicateRecv = func(pkt *probing.Packet) {
		out.add(row{"kind": "reply", "seq": pkt.Seq, "from": pkt.IPAddr.String(), "rttMs": ms(pkt.Rtt), "ttl": pkt.TTL, "bytes": pkt.Nbytes, "duplicate": true})
	}
	pinger.OnSendError = func(pkt *probing.Packet, err error) {
		out.add(row{"kind": "reply", "seq": pkt.Seq, "error": err.Error()})
	}
	pinger.OnFinish = func(st *probing.Statistics) {
		out.emitNow(pingSummary(st.Addr, st.IPAddr, st.PacketsSent, st.PacketsRecv, st.PacketLoss, st.MinRtt, st.AvgRtt, st.MaxRtt, st.StdDevRtt))
	}
	if err := pinger.RunWithContext(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("ping failed: %w", err)
	}
	return nil
}

func pingSummary(addr string, ip *net.IPAddr, sent, recv int, loss float64, minRtt, avgRtt, maxRtt, stddev time.Duration) row {
	target := addr
	if ip != nil {
		target = ip.String()
	}
	r := row{"kind": "summary", "target": target, "sent": sent, "recv": recv, "loss": round1(loss)}
	if recv > 0 {
		r["minMs"], r["avgMs"], r["maxMs"], r["stddevMs"] = ms(minRtt), ms(avgRtt), ms(maxRtt), ms(stddev)
	}
	return r
}

// tcpPing repeatedly opens a TCP connection to host:port and reports the connect latency (a "ping" for hosts that
// filter ICMP). Timeouts and refusals are streamed as unreachable replies.
func tcpPing(ctx context.Context, guard *netGuard, req *pingRequest, out *sink) error {
	ip, err := guard.resolve(ctx, req.Host, req.IPv6)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(req.Port))
	out.emitNow(row{"kind": "info", "message": fmt.Sprintf("TCP-connect ping to %s", addr)})
	interval := time.Duration(req.IntervalMs) * time.Millisecond
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	var sent, recv int
	var minRtt, maxRtt, sumRtt time.Duration
	var rtts []time.Duration
	for seq := 0; seq < req.Count; seq++ {
		if seq > 0 && !sleep(ctx, interval) {
			break
		}
		if ctx.Err() != nil {
			break
		}
		sent++
		start := time.Now()
		conn, derr := guard.dialer(timeout).DialContext(ctx, "tcp", addr)
		rtt := time.Since(start)
		if derr != nil {
			if ctx.Err() != nil {
				sent--
				break
			}
			reason := "refused"
			switch {
			case isTimeout(derr):
				reason = "timeout"
			case isUnreachable(derr):
				reason = "unreachable"
			}
			out.add(row{"kind": "reply", "seq": seq, "error": reason})
			continue
		}
		_ = conn.Close()
		recv++
		if minRtt == 0 || rtt < minRtt {
			minRtt = rtt
		}
		maxRtt = max(maxRtt, rtt)
		sumRtt += rtt
		rtts = append(rtts, rtt)
		out.add(row{"kind": "reply", "seq": seq, "from": addr, "rttMs": ms(rtt), "tcp": true})
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	loss := 0.0
	if sent > 0 {
		loss = float64(sent-recv) / float64(sent) * 100
	}
	var avg, sd time.Duration
	if recv > 0 {
		avg = sumRtt / time.Duration(recv)
		sd = stddev(rtts, avg)
	}
	sum := pingSummary(addr, nil, sent, recv, loss, minRtt, avg, maxRtt, sd)
	sum["tcp"] = true
	out.emitNow(sum)
	return nil
}

func stddev(v []time.Duration, mean time.Duration) time.Duration {
	if len(v) < 2 {
		return 0
	}
	var sq float64
	for _, x := range v {
		d := float64(x - mean)
		sq += d * d
	}
	return time.Duration(sqrt(sq / float64(len(v))))
}

// icmpMode reports which kind of ICMP socket this process can open: "udp" (unprivileged datagram ICMP — macOS, Linux
// with net.ipv4.ping_group_range covering our group), "ip" (raw sockets — root / CAP_NET_RAW, and Windows where
// pro-bing's "privileged" mode needs no elevation) or "" (none: callers fall back to a TCP-connect ping).
func icmpMode(ipv6 bool) string {
	icmpModeOnce.Do(probeICMPModes)
	if ipv6 {
		return icmpModes[1]
	}
	return icmpModes[0]
}

var (
	icmpModeOnce sync.Once
	icmpModes    [2]string
)

func probeICMPModes() {
	try := func(network, addr string) bool {
		c, err := icmp.ListenPacket(network, addr)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}
	for i, fam := range []struct{ dgram, raw, any string }{
		{"udp4", "ip4:icmp", "0.0.0.0"},
		{"udp6", "ip6:ipv6-icmp", "::"},
	} {
		switch {
		case runtime.GOOS != "windows" && try(fam.dgram, fam.any):
			icmpModes[i] = "udp"
		case try(fam.raw, fam.any):
			icmpModes[i] = "ip"
		}
	}
}
