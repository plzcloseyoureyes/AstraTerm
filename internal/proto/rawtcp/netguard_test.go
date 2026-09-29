package rawtcp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexterm/nexterm/internal/app"
	"github.com/nexterm/nexterm/internal/config"
	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/netguard"
	"github.com/nexterm/nexterm/internal/term"
)

// SEC-7: the direct UDP transport of a restricted user is vetted by netguard.

func testDeps(mode string) *app.Deps {
	return &app.Deps{Cfg: &config.Config{Mode: mode, Listen: "127.0.0.1:1"}}
}

var (
	alice = &model.User{ID: "u-alice", Username: "alice", Role: model.RoleUser}
	root  = &model.User{ID: "u-admin", Username: "admin", Role: model.RoleAdmin}
)

// udpService is a loopback-only datagram service of the NexTerm host; it counts datagrams and answers "pong".
func udpService(t *testing.T) (port int, received *atomic.Int32) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	received = &atomic.Int32{}
	go func() {
		buf := make([]byte, 2048)
		for {
			_, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			received.Add(1)
			_, _ = pc.WriteTo([]byte("pong"), from)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port, received
}

func udpConnection(host string, port int) *model.Connection {
	return &model.Connection{Protocol: model.ProtoRaw, Host: host, Port: port, Options: model.Options{"transport": "udp"}}
}

func TestNetguardRefusesUDPOfUsers(t *testing.T) {
	port, received := udpService(t)
	d := testDeps(config.ModeServer)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, user := range []*model.User{alice, nil} {
		for _, host := range []string{"127.0.0.1", "localhost", "127.1", "::ffff:127.0.0.1", "0.0.0.0", "169.254.169.254"} {
			b, err := open(ctx, d, nil, term.OpenRequest{Connection: udpConnection(host, port), User: user})
			if err == nil {
				_, _ = b.Write([]byte("ping\r"))
				b.Close()
				t.Fatalf("udp to %s: opened, want a refusal", host)
			}
			if _, ok := netguard.IsBlocked(err); !ok || !term.IsPermanent(err) {
				t.Fatalf("udp to %s: err = %v (permanent=%v), want a permanent netguard refusal", host, err, term.IsPermanent(err))
			}
			var he *httpx.HTTPError
			if !errors.As(err, &he) || he.Status != http.StatusForbidden || he.Code != netguard.CodeBlocked ||
				!strings.Contains(err.Error(), "not allowed in server mode") {
				t.Fatalf("udp to %s: err = %v, want 403 %s", host, err, netguard.CodeBlocked)
			}
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := received.Load(); n != 0 {
		t.Fatalf("the loopback service received %d datagrams from a restricted user", n)
	}
}

func TestNetguardAllowsUDPOfAdminsAndDesktop(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    *app.Deps
		user *model.User
	}{
		{"server admin", testDeps(config.ModeServer), root},
		{"desktop", testDeps(config.ModeDesktop), alice},
		{"no deps", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port, received := udpService(t)
			b, err := open(context.Background(), tc.d, nil, term.OpenRequest{Connection: udpConnection("127.0.0.1", port), User: tc.user})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer b.Close()
			col := collect(b)
			if _, err := b.Write([]byte("ping\r")); err != nil {
				t.Fatal(err)
			}
			if got := col.until(3*time.Second, func(acc []byte) bool { return strings.Contains(string(acc), "pong") }); !strings.Contains(string(got), "pong") {
				t.Fatalf("no reply: %q", got)
			}
			if received.Load() == 0 {
				t.Fatal("the service received nothing")
			}
		})
	}
}

// The TCP/TLS transports still require the pool (their guard lives in sshx), and a nil core is reported, not a panic.
func TestTCPNeedsDialer(t *testing.T) {
	_, err := open(context.Background(), testDeps(config.ModeServer), nil,
		term.OpenRequest{Connection: &model.Connection{Protocol: model.ProtoRaw, Host: "127.0.0.1", Port: 1}, User: alice})
	if err == nil || !strings.Contains(err.Error(), "no dialer") {
		t.Fatalf("err = %v", err)
	}
}
