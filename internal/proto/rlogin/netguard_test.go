package rlogin

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/netguard"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// SEC-7: direct (non-routed) rlogin / rsh dials of a restricted user are vetted by netguard.

func testDeps(mode string) *app.Deps {
	return &app.Deps{Cfg: &config.Config{Mode: mode, Listen: "127.0.0.1:1"}}
}

var (
	alice = &model.User{ID: "u-alice", Username: "alice", Role: model.RoleUser}
	root  = &model.User{ID: "u-admin", Username: "admin", Role: model.RoleAdmin}
)

// loopbackService is a loopback-only listener of the AstraTerm host that counts the connections reaching it.
func loopbackService(t *testing.T) (port int, accepted *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted = &atomic.Int32{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, accepted
}

func assertBlocked(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: connected, want a destination_blocked refusal", what)
	}
	if _, ok := netguard.IsBlocked(err); !ok {
		t.Fatalf("%s: err = %v, want a netguard refusal", what, err)
	}
	if !term.IsPermanent(err) {
		t.Fatalf("%s: refusal is not permanent (sessions would auto-reconnect into it): %v", what, err)
	}
	var he *httpx.HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusForbidden || he.Code != netguard.CodeBlocked {
		t.Fatalf("%s: err = %v, want 403 %s", what, err, netguard.CodeBlocked)
	}
	if !strings.Contains(err.Error(), "not allowed in server mode") {
		t.Fatalf("%s: unclear message %q", what, err)
	}
}

func TestNetguardRefusesDirectDialsOfUsers(t *testing.T) {
	port, accepted := loopbackService(t)
	d := testDeps(config.ModeServer)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, host := range []string{"127.0.0.1", "localhost", "127.1", "::ffff:127.0.0.1", "0.0.0.0"} {
		for _, opts := range []model.Options{{"variant": "rlogin"}, {"variant": "rsh", "command": "id"}} {
			conn := &model.Connection{Protocol: model.ProtoRlogin, Host: host, Port: port, Username: "x", Options: opts}
			// A nil core: a direct connection must not need the pool (and must not fall back to it).
			b, err := open(ctx, d, nil, term.OpenRequest{Connection: conn, User: alice})
			if b != nil {
				b.Close()
			}
			assertBlocked(t, opts.String("variant", "")+" to "+host, err)
		}
		// Same for an unknown (nil) user.
		conn := &model.Connection{Protocol: model.ProtoRlogin, Host: host, Port: port, Username: "x"}
		_, err := open(ctx, d, nil, term.OpenRequest{Connection: conn})
		assertBlocked(t, "nil user to "+host, err)
	}

	// Both dialers: the reserved-source-port one and the unprivileged fallback.
	g := netguard.ForUser(d, alice)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if _, err := dialReserved(ctx, addr, g); err == nil || errors.Is(err, errNoReservedPort) {
		t.Fatalf("dialReserved: err = %v, want a refusal (not a fallback to the unprivileged dialer)", err)
	} else if _, ok := netguard.IsBlocked(err); !ok {
		t.Fatalf("dialReserved: err = %v", err)
	}
	prev := dialReservedPort
	dialReservedPort = func(context.Context, string, *netguard.Guard) (net.Conn, error) { return nil, errNoReservedPort }
	defer func() { dialReservedPort = prev }()
	_, err := dial(ctx, nil, g, term.OpenRequest{Connection: &model.Connection{Host: "127.0.0.1", Port: port}, User: alice}, port)
	assertBlocked(t, "fallback dialer", err)

	time.Sleep(50 * time.Millisecond)
	if n := accepted.Load(); n != 0 {
		t.Fatalf("the loopback service saw %d connections from a restricted user", n)
	}
}

func TestNetguardAllowsAdminsAndDesktop(t *testing.T) {
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
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			f := startFakeRlogind(t, "")
			conn := &model.Connection{Protocol: model.ProtoRlogin, Host: "127.0.0.1", Port: f.port(), Username: "alice"}
			b, err := open(ctx, tc.d, nil, term.OpenRequest{Connection: conn, User: tc.user})
			if err != nil {
				t.Fatalf("rlogin: %v", err)
			}
			b.Close()

			port, got := startFakeRshd(t)
			conn = &model.Connection{Protocol: model.ProtoRlogin, Host: "127.0.0.1", Port: port, Username: "alice",
				Options: model.Options{"variant": "rsh", "command": "cat"}}
			b, err = open(ctx, tc.d, nil, term.OpenRequest{Connection: conn, User: tc.user})
			if err != nil {
				t.Fatalf("rsh: %v", err)
			}
			defer b.Close()
			if _, err := b.Write([]byte("\x04")); err != nil {
				t.Fatal(err)
			}
			select {
			case <-got:
			case <-ctx.Done():
				t.Fatal("rshd did not complete the session")
			}
		})
	}
}

// TestNetguardAdminPolicyException: an administrator's allow rule opens exactly that destination to users.
func TestNetguardAdminPolicyException(t *testing.T) {
	f := startFakeRlogind(t, "")
	g, err := netguard.NewGuard(func() netguard.Policy {
		p := netguard.DefaultPolicy()
		p.Allow = []string{"127.0.0.1/32"}
		return p
	}())
	if err != nil {
		t.Fatal(err)
	}
	conn := &model.Connection{Protocol: model.ProtoRlogin, Host: "127.0.0.1", Port: f.port(), Username: "alice"}
	b, err := openRlogin(context.Background(), nil, g, term.OpenRequest{Connection: conn, User: alice})
	if err != nil {
		t.Fatalf("rlogin with an allow rule: %v", err)
	}
	b.Close()
}
