//go:build linux

package tools

import (
	"context"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// recvErrTracer is the Linux probing engine. Unprivileged sockets never see ICMP "time exceeded" through a normal read
// on Linux; with IP_RECVERR / IPV6_RECVERR the kernel queues those errors on the socket's error queue together with
// the offending router's address (struct sock_extended_err + SO_EE_OFFENDER) — this is how tracepath works without
// privileges. ICMP echo probes use an unprivileged ping socket (net.ipv4.ping_group_range); when that is not allowed,
// UDP probes to the traceroute port range are used, which works for every user.
type recvErrTracer struct {
	fd      int
	v6      bool
	udp     bool
	note    string
	dst     net.IP
	next    int
	backlog []traceAnswer
	buf     []byte
	oob     []byte
	payload []byte
}

func openTracer(dst net.IP, udp bool) (tracer, error) {
	v6 := dst.To4() == nil
	t := &recvErrTracer{v6: v6, dst: dst, buf: make([]byte, 2048), oob: make([]byte, 512), next: rand.IntN(0xffff),
		payload: []byte("Termstead traceroute probe 0123456")}
	var err error
	if !udp {
		if t.fd, err = openProbeSocket(v6, false); err != nil {
			// Ping sockets are not permitted for this group (net.ipv4.ping_group_range): UDP probes need no privilege.
			udp = true
			t.note = " (ICMP ping sockets not permitted)"
		}
	}
	if udp {
		if t.fd, err = openProbeSocket(v6, true); err != nil {
			return nil, err
		}
	}
	t.udp = udp
	level, opt := unix.IPPROTO_IP, unix.IP_RECVERR
	if v6 {
		level, opt = unix.IPPROTO_IPV6, unix.IPV6_RECVERR
	}
	if err := unix.SetsockoptInt(t.fd, level, opt, 1); err != nil {
		_ = unix.Close(t.fd)
		return nil, err
	}
	return t, nil
}

func openProbeSocket(v6, udp bool) (int, error) {
	family, proto := unix.AF_INET, unix.IPPROTO_ICMP
	if v6 {
		family, proto = unix.AF_INET6, unix.IPPROTO_ICMPV6
	}
	if udp {
		proto = unix.IPPROTO_UDP
	}
	return unix.Socket(family, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, proto)
}

func (t *recvErrTracer) engine() string {
	if t.udp {
		return "UDP" + t.note
	}
	return "ICMP echo"
}

func (t *recvErrTracer) close() error { return unix.Close(t.fd) }

func (t *recvErrTracer) sockaddr(port int) unix.Sockaddr {
	if t.v6 {
		sa := &unix.SockaddrInet6{Port: port}
		copy(sa.Addr[:], t.dst.To16())
		return sa
	}
	sa := &unix.SockaddrInet4{Port: port}
	copy(sa.Addr[:], t.dst.To4())
	return sa
}

// icmpErrno reports errors that the kernel derives from received ICMP messages (a pending socket error that the
// next send/recv reports once); they are not failures of the call itself.
func icmpErrno(err error) bool {
	return errors.Is(err, unix.EHOSTUNREACH) || errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.ECONNREFUSED) ||
		errors.Is(err, unix.EPROTO) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EMSGSIZE) ||
		errors.Is(err, unix.ENOPROTOOPT) || errors.Is(err, unix.EHOSTDOWN)
}

// send only touches the socket through setsockopt / sendto, so it can run concurrently with recv on the reader
// goroutine (the error queue and the backlog belong to recv).
func (t *recvErrTracer) send(ttl int) (int, error) {
	level, opt := unix.IPPROTO_IP, unix.IP_TTL
	if t.v6 {
		level, opt = unix.IPPROTO_IPV6, unix.IPV6_UNICAST_HOPS
	}
	if err := unix.SetsockoptInt(t.fd, level, opt, ttl); err != nil {
		return 0, err
	}
	t.next = (t.next + 1) & 0xffff
	key := t.next
	var msg []byte
	var sa unix.Sockaddr
	if t.udp {
		key %= udpPortSpan
		msg, sa = t.payload, t.sockaddr(udpBasePort+key)
	} else {
		// Echo request; the kernel fills in the identifier (the socket's "port") and the checksum.
		msg = make([]byte, 8+len(t.payload))
		msg[0] = ternary[byte](t.v6, 128, 8)
		binary.BigEndian.PutUint16(msg[6:8], uint16(key))
		copy(msg[8:], t.payload)
		sa = t.sockaddr(0)
	}
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if err = unix.Sendto(t.fd, msg, 0, sa); err == nil {
			return key, nil
		}
		switch {
		case errors.Is(err, unix.EINTR), icmpErrno(err) && attempt < 2:
			// A pending ICMP-derived socket error is reported (and cleared) by this call; the details stay in the
			// error queue for recv. Just try again.
		case errors.Is(err, unix.EAGAIN), errors.Is(err, unix.ENOBUFS):
			time.Sleep(2 * time.Millisecond)
		default:
			return 0, err
		}
	}
	return 0, err
}

func (t *recvErrTracer) recv(ctx context.Context, deadline time.Time) (traceAnswer, error) {
	for {
		if len(t.backlog) > 0 {
			a := t.backlog[0]
			t.backlog = t.backlog[1:]
			return a, nil
		}
		t.drainErrors()
		if len(t.backlog) > 0 {
			continue
		}
		if a, ok := t.readReply(); ok {
			return a, nil
		}
		now := time.Now()
		if !now.Before(deadline) || ctx.Err() != nil {
			return traceAnswer{}, errNoAnswer
		}
		wait := min(deadline.Sub(now), 50*time.Millisecond)
		fds := []unix.PollFd{{Fd: int32(t.fd), Events: unix.POLLIN}}
		_, _ = unix.Poll(fds, int(wait/time.Millisecond)+1) // POLLERR is always reported; EINTR just loops
	}
}

// readReply reads a normal datagram: an echo reply (ICMP) or, rarely, a UDP answer from the destination.
func (t *recvErrTracer) readReply() (traceAnswer, bool) {
	for i := 0; i < 64; i++ {
		n, from, err := unix.Recvfrom(t.fd, t.buf, unix.MSG_DONTWAIT)
		if err != nil {
			if icmpErrno(err) || errors.Is(err, unix.EINTR) {
				continue // a pending ICMP-derived socket error, reported once; the details are in the error queue
			}
			return traceAnswer{}, false
		}
		at := time.Now()
		ip, port := sockaddrIP(from)
		if t.udp {
			if key := port - udpBasePort; ip != nil && ip.Equal(t.dst) && key >= 0 && key < udpPortSpan {
				return traceAnswer{key: key, from: ip, at: at, reached: true}, true
			}
			continue
		}
		reply := ternary[byte](t.v6, 129, 0)
		if n >= 8 && t.buf[0] == reply {
			return traceAnswer{key: int(binary.BigEndian.Uint16(t.buf[6:8])), from: ip, at: at, reached: true}, true
		}
	}
	return traceAnswer{}, false
}

// drainErrors moves every queued ICMP error into the backlog.
func (t *recvErrTracer) drainErrors() {
	for i := 0; i < 256; i++ {
		n, oobn, _, from, err := unix.Recvmsg(t.fd, t.buf, t.oob, unix.MSG_ERRQUEUE|unix.MSG_DONTWAIT)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return // EAGAIN: queue empty
		}
		if a, ok := t.parseError(t.buf[:n], t.oob[:oobn], from); ok {
			t.backlog = append(t.backlog, a)
		}
	}
}

func (t *recvErrTracer) parseError(data, oob []byte, orig unix.Sockaddr) (traceAnswer, bool) {
	at := time.Now()
	cmsgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return traceAnswer{}, false
	}
	for _, cm := range cmsgs {
		v4 := cm.Header.Level == unix.SOL_IP && cm.Header.Type == unix.IP_RECVERR
		v6 := cm.Header.Level == unix.SOL_IPV6 && cm.Header.Type == unix.IPV6_RECVERR
		if !v4 && !v6 || len(cm.Data) < 16 {
			continue
		}
		origin, typ, code := cm.Data[4], int(cm.Data[5]), int(cm.Data[6])
		if origin != unix.SO_EE_ORIGIN_ICMP && origin != unix.SO_EE_ORIGIN_ICMP6 {
			return traceAnswer{}, false // a local error (e.g. EMSGSIZE), not a network answer
		}
		// The probe key: UDP → the original destination port (msg_name); ICMP → the echo sequence number of the
		// original request (the error's payload starts at the quoted ICMP header).
		key := -1
		if t.udp {
			if _, port := sockaddrIP(orig); port >= udpBasePort && port < udpBasePort+udpPortSpan {
				key = port - udpBasePort
			}
		} else if len(data) >= 8 {
			key = int(binary.BigEndian.Uint16(data[6:8]))
		}
		if key < 0 {
			return traceAnswer{}, false
		}
		a := traceAnswer{key: key, at: at, from: offenderIP(cm.Data[16:])}
		if origin == unix.SO_EE_ORIGIN_ICMP {
			switch typ {
			case 11: // time exceeded
			case 3: // destination unreachable
				a.code = unreachCode4(code)
				a.reached = code == 3 || a.from != nil && a.from.Equal(t.dst)
			default:
				return traceAnswer{}, false
			}
		} else {
			switch typ {
			case 3: // ICMPv6 time exceeded
			case 1: // ICMPv6 destination unreachable
				a.code = unreachCode6(code)
				a.reached = code == 4 || a.from != nil && a.from.Equal(t.dst)
			default:
				return traceAnswer{}, false
			}
		}
		if a.reached {
			a.code = "" // the destination answered: not a failure annotation
		}
		return a, true
	}
	return traceAnswer{}, false
}

// offenderIP decodes the SO_EE_OFFENDER sockaddr that follows struct sock_extended_err.
func offenderIP(b []byte) net.IP {
	if len(b) < 2 {
		return nil
	}
	switch binary.NativeEndian.Uint16(b[0:2]) {
	case unix.AF_INET:
		if len(b) >= 8 {
			return net.IPv4(b[4], b[5], b[6], b[7])
		}
	case unix.AF_INET6:
		if len(b) >= 24 {
			return net.IP(append([]byte(nil), b[8:24]...))
		}
	}
	return nil
}

func sockaddrIP(sa unix.Sockaddr) (net.IP, int) {
	switch v := sa.(type) {
	case *unix.SockaddrInet4:
		return net.IPv4(v.Addr[0], v.Addr[1], v.Addr[2], v.Addr[3]), v.Port
	case *unix.SockaddrInet6:
		return net.IP(append([]byte(nil), v.Addr[:]...)), v.Port
	}
	return nil, 0
}
