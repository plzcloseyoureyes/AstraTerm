//go:build !linux

package tools

import (
	"context"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"net"
	"runtime"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// icmpTracer is the probing engine outside Linux. macOS and the BSDs let unprivileged processes open ICMP datagram
// sockets that receive every ICMP message addressed to the host, including "time exceeded" for our probes; Windows
// (and root elsewhere) use raw ICMP sockets. Answers are matched to probes through the quoted original packet (echo
// identifier + sequence, or the UDP ports).
type icmpTracer struct {
	conn      *icmp.PacketConn // receives ICMP; sends the echo probes in ICMP mode
	dgram     bool             // conn is an unprivileged datagram socket (addresses are *net.UDPAddr)
	udpConn   net.PacketConn   // UDP probe sender (UDP mode)
	udp4      *ipv4.PacketConn
	udp6      *ipv6.PacketConn
	localPort int
	v6, udp   bool
	dst       net.IP
	id        int
	next      int
	payload   []byte
	rb        []byte
}

func openTracer(dst net.IP, udp bool) (tracer, error) {
	v6 := dst.To4() == nil
	candidates := [][2]string{{"udp4", "0.0.0.0"}, {"ip4:icmp", "0.0.0.0"}}
	if v6 {
		candidates = [][2]string{{"udp6", "::"}, {"ip6:ipv6-icmp", "::"}}
	}
	t := &icmpTracer{v6: v6, udp: udp, dst: dst, id: rand.IntN(0xffff) + 1, next: rand.IntN(0xffff),
		payload: []byte("AstraTerm traceroute probe 0123456"), rb: make([]byte, 2048)}
	var lastErr error
	for _, c := range candidates {
		if runtime.GOOS == "windows" && (c[0] == "udp4" || c[0] == "udp6") {
			continue // no unprivileged ICMP sockets on Windows
		}
		conn, err := icmp.ListenPacket(c[0], c[1])
		if err != nil {
			lastErr = err
			continue
		}
		t.conn, t.dgram = conn, c[0] == "udp4" || c[0] == "udp6"
		break
	}
	if t.conn == nil {
		if lastErr == nil {
			lastErr = errors.New("no ICMP socket available")
		}
		return nil, lastErr
	}
	if udp {
		network, addr := "udp4", "0.0.0.0:0"
		if v6 {
			network, addr = "udp6", "[::]:0"
		}
		uc, err := net.ListenPacket(network, addr)
		if err != nil {
			_ = t.conn.Close()
			return nil, err
		}
		t.udpConn = uc
		t.localPort = uc.LocalAddr().(*net.UDPAddr).Port
		if v6 {
			t.udp6 = ipv6.NewPacketConn(uc)
		} else {
			t.udp4 = ipv4.NewPacketConn(uc)
		}
	}
	return t, nil
}

func (t *icmpTracer) engine() string {
	if t.udp {
		return "UDP"
	}
	return "ICMP echo"
}

func (t *icmpTracer) close() error {
	if t.udpConn != nil {
		_ = t.udpConn.Close()
	}
	return t.conn.Close()
}

func (t *icmpTracer) send(ttl int) (int, error) {
	t.next = (t.next + 1) & 0xffff
	key := t.next
	if t.udp {
		key %= udpPortSpan
		var err error
		if t.v6 {
			err = t.udp6.SetHopLimit(ttl)
		} else {
			err = t.udp4.SetTTL(ttl)
		}
		if err != nil {
			return 0, err
		}
		_, err = t.udpConn.WriteTo(t.payload, &net.UDPAddr{IP: t.dst, Port: udpBasePort + key})
		return key, err
	}
	var typ icmp.Type = ipv4.ICMPTypeEcho
	var err error
	if t.v6 {
		typ = ipv6.ICMPTypeEchoRequest
		err = t.conn.IPv6PacketConn().SetHopLimit(ttl)
	} else {
		err = t.conn.IPv4PacketConn().SetTTL(ttl)
	}
	if err != nil {
		return 0, err
	}
	b, err := (&icmp.Message{Type: typ, Body: &icmp.Echo{ID: t.id, Seq: key, Data: t.payload}}).Marshal(nil)
	if err != nil {
		return 0, err
	}
	var addr net.Addr = &net.IPAddr{IP: t.dst}
	if t.dgram {
		addr = &net.UDPAddr{IP: t.dst}
	}
	_, err = t.conn.WriteTo(b, addr)
	return key, err
}

func (t *icmpTracer) recv(ctx context.Context, deadline time.Time) (traceAnswer, error) {
	for {
		now := time.Now()
		if !now.Before(deadline) || ctx.Err() != nil {
			return traceAnswer{}, errNoAnswer
		}
		_ = t.conn.SetReadDeadline(minTime(deadline, now.Add(100*time.Millisecond)))
		n, peer, err := t.conn.ReadFrom(t.rb)
		if err != nil {
			if isTimeout(err) {
				continue
			}
			return traceAnswer{}, err
		}
		if a, ok := t.parse(t.rb[:n], addrIP(peer), time.Now()); ok {
			return a, nil
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// parse matches a received ICMP message to one of our probes.
func (t *icmpTracer) parse(b []byte, from net.IP, at time.Time) (traceAnswer, bool) {
	proto := 1
	if t.v6 {
		proto = 58
	} else if len(b) >= 20 && b[0]>>4 == 4 && b[9] == 1 {
		// Some raw sockets deliver the IPv4 header too.
		if ihl := int(b[0]&0x0f) * 4; ihl >= 20 && ihl < len(b) {
			if from == nil {
				from = net.IP(append([]byte(nil), b[12:16]...))
			}
			b = b[ihl:]
		}
	}
	m, err := icmp.ParseMessage(proto, b)
	if err != nil {
		return traceAnswer{}, false
	}
	a := traceAnswer{from: from, at: at}
	switch m.Type {
	case ipv4.ICMPTypeEchoReply, ipv6.ICMPTypeEchoReply:
		e, ok := m.Body.(*icmp.Echo)
		if t.udp || !ok || e.ID != t.id {
			return traceAnswer{}, false
		}
		a.key, a.reached = e.Seq, true
		return a, true
	case ipv4.ICMPTypeTimeExceeded, ipv6.ICMPTypeTimeExceeded:
		te, ok := m.Body.(*icmp.TimeExceeded)
		if !ok {
			return traceAnswer{}, false
		}
		key, ok := t.matchInner(te.Data)
		a.key = key
		return a, ok
	case ipv4.ICMPTypeDestinationUnreachable, ipv6.ICMPTypeDestinationUnreachable:
		du, ok := m.Body.(*icmp.DstUnreach)
		if !ok {
			return traceAnswer{}, false
		}
		key, ok := t.matchInner(du.Data)
		if !ok {
			return traceAnswer{}, false
		}
		a.key = key
		if t.v6 {
			a.code = unreachCode6(m.Code)
			a.reached = m.Code == 4
		} else {
			a.code = unreachCode4(m.Code)
			a.reached = m.Code == 3
		}
		if from != nil && from.Equal(t.dst) {
			a.reached = true
		}
		if a.reached {
			a.code = ""
		}
		return a, true
	}
	return traceAnswer{}, false
}

// matchInner inspects the original packet quoted in an ICMP error (IP header + at least 8 bytes of the transport
// header) and returns the probe key when it is one of ours.
func (t *icmpTracer) matchInner(d []byte) (int, bool) {
	var next byte
	var inner []byte
	if t.v6 {
		if len(d) < 48 || d[0]>>4 != 6 || !net.IP(d[24:40]).Equal(t.dst) {
			return 0, false
		}
		next, inner = d[6], d[40:]
	} else {
		if len(d) < 28 || d[0]>>4 != 4 {
			return 0, false
		}
		ihl := int(d[0]&0x0f) * 4
		if ihl < 20 || len(d) < ihl+8 || !net.IP(d[16:20]).Equal(t.dst) {
			return 0, false
		}
		next, inner = d[9], d[ihl:]
	}
	if t.udp {
		if next != 17 || int(binary.BigEndian.Uint16(inner[0:2])) != t.localPort {
			return 0, false
		}
		key := int(binary.BigEndian.Uint16(inner[2:4])) - udpBasePort
		return key, key >= 0 && key < udpPortSpan
	}
	echo := ternary[byte](t.v6, 128, 8)
	proto := ternary[byte](t.v6, 58, 1)
	if next != proto || inner[0] != echo || int(binary.BigEndian.Uint16(inner[4:6])) != t.id {
		return 0, false
	}
	return int(binary.BigEndian.Uint16(inner[6:8])), true
}
