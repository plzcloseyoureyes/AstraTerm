package tools

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// Host naming for the network scanner (RESEARCH TOOL-4 "hostname (reverse DNS, NetBIOS, mDNS)"): the three lookups
// run in parallel with short timeouts, only for hosts found alive. NetBIOS and mDNS are LAN protocols and are only
// tried when the scan runs from the NexTerm host itself (local).
func lookupHostNames(ctx context.Context, ip net.IP, local bool) map[string]any {
	out := map[string]any{}
	var mu sync.Mutex
	set := func(k string, v any) {
		mu.Lock()
		out[k] = v
		mu.Unlock()
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if n := reverseDNS(ctx, ip); n != "" {
			set("hostname", n)
		}
	}()
	if local && ip.To4() != nil {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if nb, err := netbiosStatus(ctx, ip, 800*time.Millisecond); err == nil {
				if nb.name != "" {
					set("netbios", nb.name)
				}
				if nb.group != "" {
					set("workgroup", nb.group)
				}
				if nb.mac != "" {
					set("netbiosMac", nb.mac)
				}
			}
		}()
		go func() {
			defer wg.Done()
			if n := mdnsName(ctx, ip, 800*time.Millisecond); n != "" {
				set("mdns", n)
			}
		}()
	}
	wg.Wait()
	return out
}

// ---- NetBIOS node status (NBSTAT, RFC 1002 §4.2.18) ----------------------------------------------------------------

type netbiosInfo struct {
	name, group, mac string
}

// nbstatRequest builds a node-status query for the wildcard name "*".
func nbstatRequest(tid uint16) []byte {
	b := make([]byte, 0, 50)
	b = binary.BigEndian.AppendUint16(b, tid)
	b = append(b, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00) // flags, QD=1, AN/NS/AR=0
	b = append(b, 0x20)                                                       // encoded name length
	name := [16]byte{'*'}
	for _, c := range name {
		b = append(b, 'A'+c>>4, 'A'+c&0x0f)
	}
	b = append(b, 0x00)       // root label
	b = append(b, 0x00, 0x21) // NBSTAT
	b = append(b, 0x00, 0x01) // IN
	return b
}

// parseNBStat decodes a node-status response.
func parseNBStat(b []byte, tid uint16) (netbiosInfo, error) {
	var info netbiosInfo
	if len(b) < 12 || binary.BigEndian.Uint16(b[0:2]) != tid || b[2]&0x80 == 0 {
		return info, fmt.Errorf("not a node status response")
	}
	qd, an := int(binary.BigEndian.Uint16(b[4:6])), int(binary.BigEndian.Uint16(b[6:8]))
	if an < 1 {
		return info, fmt.Errorf("no answer")
	}
	p := 12
	skipName := func() bool {
		for p < len(b) {
			l := int(b[p])
			switch {
			case l == 0:
				p++
				return true
			case l&0xc0 == 0xc0: // compression pointer
				p += 2
				return p <= len(b)
			default:
				p += 1 + l
			}
		}
		return false
	}
	for i := 0; i < qd; i++ {
		if !skipName() {
			return info, fmt.Errorf("truncated")
		}
		p += 4
	}
	if !skipName() || p+10 > len(b) {
		return info, fmt.Errorf("truncated")
	}
	if binary.BigEndian.Uint16(b[p:p+2]) != 0x21 {
		return info, fmt.Errorf("not NBSTAT")
	}
	rdlen := int(binary.BigEndian.Uint16(b[p+8 : p+10]))
	p += 10
	if p+rdlen > len(b) || rdlen < 1 {
		return info, fmt.Errorf("truncated")
	}
	rd := b[p : p+rdlen]
	n := int(rd[0])
	q := 1
	for i := 0; i < n && q+18 <= len(rd); i++ {
		raw := rd[q : q+15]
		suffix := rd[q+15]
		flags := binary.BigEndian.Uint16(rd[q+16 : q+18])
		q += 18
		name := strings.TrimRight(strings.Map(func(r rune) rune {
			if r < 0x20 || r > 0x7e {
				return -1
			}
			return r
		}, string(raw)), " ")
		if suffix != 0x00 || name == "" {
			continue
		}
		if flags&0x8000 != 0 { // group name
			if info.group == "" {
				info.group = name
			}
		} else if info.name == "" {
			info.name = name
		}
	}
	if q+6 <= len(rd) {
		mac := rd[q : q+6]
		if !allZero(mac) {
			info.mac = formatMAC(mac)
		}
	}
	return info, nil
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// netbiosStatus asks ip:137 for its NetBIOS name table (Windows / Samba hosts answer, even off-link).
func netbiosStatus(ctx context.Context, ip net.IP, timeout time.Duration) (netbiosInfo, error) {
	d := net.Dialer{}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := d.DialContext(cctx, "udp4", net.JoinHostPort(ip.String(), "137"))
	if err != nil {
		return netbiosInfo{}, err
	}
	defer conn.Close()
	deadline, _ := cctx.Deadline()
	_ = conn.SetDeadline(deadline)
	tid := uint16(rand.IntN(0xffff))
	if _, err := conn.Write(nbstatRequest(tid)); err != nil {
		return netbiosInfo{}, err
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		return netbiosInfo{}, err
	}
	return parseNBStat(buf[:n], tid)
}

// ---- mDNS ----------------------------------------------------------------------------------------------------------

// mdnsName asks the host itself (unicast to port 5353, an RFC 6762 §6.7 "legacy unicast" query) for the PTR record of
// its address: Apple devices, Linux (Avahi), printers and IoT gear answer with their .local name.
func mdnsName(ctx context.Context, ip net.IP, timeout time.Duration) string {
	arpa, err := dns.ReverseAddr(ip.String())
	if err != nil {
		return ""
	}
	m := new(dns.Msg)
	m.SetQuestion(arpa, dns.TypePTR)
	m.RecursionDesired = false
	c := &dns.Client{Net: "udp", Timeout: timeout}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r, _, err := c.ExchangeContext(cctx, m, net.JoinHostPort(ip.String(), "5353"))
	if err != nil || r == nil {
		return ""
	}
	for _, rr := range append(r.Answer, r.Extra...) {
		if p, ok := rr.(*dns.PTR); ok {
			return trimDot(p.Ptr)
		}
	}
	return ""
}
