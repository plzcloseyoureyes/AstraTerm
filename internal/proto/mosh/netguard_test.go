package mosh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mosh "github.com/unixshells/mosh-go"

	"github.com/nexterm/nexterm/internal/app"
	"github.com/nexterm/nexterm/internal/config"
	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/netguard"
	"github.com/nexterm/nexterm/internal/term"
)

// SEC-7: the UDP leg of a restricted user's mosh session is vetted by netguard.

func testDeps(mode string) *app.Deps {
	return &app.Deps{Cfg: &config.Config{Mode: mode, Listen: "127.0.0.1:1"}}
}

var (
	alice = &model.User{ID: "u-alice", Username: "alice", Role: model.RoleUser}
	root  = &model.User{ID: "u-admin", Username: "admin", Role: model.RoleAdmin}
)

// fakeBootstrap stands in for the SSH connection: `mosh-server new` answers MOSH CONNECT <port> <key>.
type fakeBootstrap struct {
	port   int
	key    string
	remote net.Addr
	execs  atomic.Int32
}

func (f *fakeBootstrap) Exec(context.Context, string) ([]byte, []byte, int, error) {
	f.execs.Add(1)
	return []byte(fmt.Sprintf("MOSH CONNECT %d %s\n", f.port, f.key)), nil, 0, nil
}

func (f *fakeBootstrap) RemoteAddr() net.Addr { return f.remote }

// udpCounter is a loopback-only datagram service of the NexTerm host counting what reaches it.
func udpCounter(t *testing.T) (port int, received *atomic.Int32) {
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
			if _, _, err := pc.ReadFrom(buf); err != nil {
				return
			}
			received.Add(1)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port, received
}

func assertBlocked(t *testing.T, what string, err error) {
	t.Helper()
	if _, ok := netguard.IsBlocked(err); !ok || !term.IsPermanent(err) {
		t.Fatalf("%s: err = %v (permanent=%v), want a permanent netguard refusal", what, err, term.IsPermanent(err))
	}
	var he *httpx.HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusForbidden || he.Code != netguard.CodeBlocked ||
		!strings.Contains(err.Error(), "not allowed in server mode") {
		t.Fatalf("%s: err = %v, want 403 %s", what, err, netguard.CodeBlocked)
	}
}

const testKey = "xkCPKTj0erPOW0scew3EwA"

func TestNetguardRefusesUDPLegOfUsers(t *testing.T) {
	port, received := udpCounter(t)
	d := testDeps(config.ModeServer)
	g := netguard.ForUser(d, alice)
	if g == nil {
		t.Fatal("a server-mode user must be restricted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 22}

	for _, tc := range []struct {
		name   string
		conn   *model.Connection
		remote net.Addr
	}{
		// Direct: the UDP target is the address SSH reached.
		{"direct", &model.Connection{Protocol: model.ProtoMosh, Host: "ssh.example"}, loop},
		// Through a jump host: the host name is resolved on the NexTerm host.
		{"jump host, literal", &model.Connection{Protocol: model.ProtoMosh, Host: "127.0.0.1",
			Options: model.Options{"jumpHosts": []any{"x@192.0.2.1"}}}, &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 22}},
		{"proxy, mapped literal", &model.Connection{Protocol: model.ProtoMosh, Host: "::ffff:127.0.0.1",
			Options: model.Options{"proxy": map[string]any{"type": "socks5", "host": "192.0.2.1", "port": 1080}}}, nil},
	} {
		for _, system := range []bool{false, true} {
			fb := &fakeBootstrap{port: port, key: testKey, remote: tc.remote}
			// The system client path is refused before mosh-client would be spawned ("mosh-client" need not exist).
			b, err := startSession(ctx, g, term.OpenRequest{Connection: tc.conn, User: alice}, fb, system, "/nonexistent/mosh-client")
			if b != nil {
				b.Close()
			}
			assertBlocked(t, fmt.Sprintf("%s (system=%v)", tc.name, system), err)
		}
	}

	// The built-in client's socket is vetted in Control as well (defense in depth).
	_, err := dialBuiltin(ctx, g, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}, testKey, 80, 24)
	assertBlocked(t, "dialBuiltin", err)

	// The announced port is checked too: an allow rule for the host with allowedPorts excluding it refuses.
	pg, err := netguard.NewGuard(func() netguard.Policy {
		p := netguard.DefaultPolicy()
		p.Allow = []string{"127.0.0.1/32"}
		p.AllowedPorts = "22"
		return p
	}())
	if err != nil {
		t.Fatal(err)
	}
	_, err = startSession(ctx, pg, term.OpenRequest{Connection: &model.Connection{Protocol: model.ProtoMosh, Host: "h"}, User: alice},
		&fakeBootstrap{port: port, key: testKey, remote: loop}, false, "")
	assertBlocked(t, "port outside allowedPorts", err)

	// With jump hosts a literal / localhost destination is refused before mosh-server is started remotely.
	for _, host := range []string{"localhost", "127.0.0.1", "169.254.169.254"} {
		conn := &model.Connection{Protocol: model.ProtoMosh, Host: host, Options: model.Options{"jumpHosts": []any{"x@192.0.2.1"}}}
		_, err := open(ctx, d, nil, term.OpenRequest{Connection: conn, User: alice})
		assertBlocked(t, "open via jump host to "+host, err)
	}

	time.Sleep(50 * time.Millisecond)
	if n := received.Load(); n != 0 {
		t.Fatalf("the loopback service received %d datagrams from a restricted user", n)
	}
}

func TestNetguardAllowsUDPLegOfAdminsAndDesktop(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    *app.Deps
		user *model.User
	}{
		{"server admin", testDeps(config.ModeServer), root},
		{"desktop", testDeps(config.ModeDesktop), alice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := mosh.NewServer("", 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			rw := newEchoRW()
			go func() { _ = srv.ServeRW(rw, func(uint16, uint16) {}) }()
			defer rw.Close()

			g := netguard.ForUser(tc.d, tc.user)
			if g != nil {
				t.Fatal("expected an unrestricted guard")
			}
			fb := &fakeBootstrap{port: srv.Port(), key: srv.KeyBase64(), remote: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 22}}
			b, err := startSession(context.Background(), g, term.OpenRequest{Connection: &model.Connection{
				Protocol: model.ProtoMosh, Host: "127.0.0.1"}, User: tc.user}, fb, false, "")
			if err != nil {
				t.Fatalf("startSession: %v", err)
			}
			defer b.Close()
			if _, err := b.Write([]byte("hi " + strconv.Itoa(srv.Port()))); err != nil {
				t.Fatal(err)
			}
			var out []byte
			buf := make([]byte, 4096)
			for deadline := time.Now().Add(5 * time.Second); !bytes.Contains(out, []byte("hi ")) && time.Now().Before(deadline); {
				n, err := b.Read(buf)
				if err != nil {
					t.Fatalf("read: %v", err)
				}
				out = append(out, buf[:n]...)
			}
			if !bytes.Contains(out, []byte("hi ")) {
				t.Fatalf("echo not rendered: %q", out)
			}
		})
	}
}
