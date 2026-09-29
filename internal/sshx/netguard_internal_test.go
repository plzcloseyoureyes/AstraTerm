package sshx

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/termstead/termstead/internal/netguard"
	"github.com/termstead/termstead/internal/term"
)

// countingListener counts TCP connections to a loopback port.
func countingListener(t *testing.T) (*net.TCPAddr, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var n atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr), &n
}

// TestGuardedRoutes: every connection a route opens from the Termstead host — direct dials, the proxy server, TCP and
// UDP port knocks — is vetted by the route's guard (SEC-7); a nil guard is unrestricted.
func TestGuardedRoutes(t *testing.T) {
	addr, tcpHits := countingListener(t)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	var udpHits atomic.Int32
	go func() {
		buf := make([]byte, 16)
		for {
			if _, _, err := pc.ReadFrom(buf); err != nil {
				return
			}
			udpHits.Add(1)
		}
	}()
	g := &netguard.Guard{}
	ctx := context.Background()

	direct := &route{timeout: 2 * time.Second, guard: g}
	if _, err := direct.dial(ctx, "tcp", addr.String()); err == nil {
		t.Fatal("guarded direct dial succeeded")
	} else if _, ok := netguard.IsBlocked(err); !ok {
		t.Fatalf("direct: %v", err)
	}
	for _, typ := range []string{"socks5", "socks4", "socks4a", "http"} {
		rt := &route{timeout: 2 * time.Second, guard: g, proxy: &proxySpec{Type: typ, Host: "127.0.0.1", Port: addr.Port}}
		_, err := rt.dial(ctx, "tcp", "192.0.2.10:22")
		if be, ok := netguard.IsBlocked(err); !ok || !strings.Contains(be.Error(), addr.String()) {
			t.Fatalf("%s proxy on loopback: %v", typ, err)
		}
	}
	p := &Pool{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	knocks := []knock{{Port: pc.LocalAddr().(*net.UDPAddr).Port, Proto: "udp", DelayMs: 1}, {Port: addr.Port, DelayMs: 1}}
	p.portKnock(ctx, direct, "127.0.0.1", knocks)
	time.Sleep(100 * time.Millisecond)
	if tcpHits.Load() != 0 || udpHits.Load() != 0 {
		t.Fatalf("guarded route reached loopback: tcp=%d udp=%d", tcpHits.Load(), udpHits.Load())
	}

	// Unrestricted routes behave as before.
	open := &route{timeout: 2 * time.Second}
	p.portKnock(ctx, open, "127.0.0.1", knocks)
	waitCond(t, func() bool { return tcpHits.Load() == 1 && udpHits.Load() == 1 })
	c, err := open.dial(ctx, "tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()

	// The generic Dialer marks policy refusals permanent (no auto-reconnect loop).
	d := &Dialer{rt: direct}
	if _, err := d.DialContext(ctx, "tcp", addr.String()); !term.IsPermanent(err) {
		t.Fatalf("Dialer refusal is not permanent: %v", err)
	}
}

func waitCond(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
