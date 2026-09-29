package rdp

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/core"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/netguard"
	"github.com/plzcloseyoureyes/astraterm/internal/rdp/guac"
)

// SEC-7: guacd connects to its "hostname" itself, so AstraTerm vets (and pins) the destination of restricted users
// before handing it over; the IronRDP relay's dials are guarded.

func serverMode(c *config.Config) { c.Mode = config.ModeServer }

// loopbackRDP is a loopback-only "RDP server" of the AstraTerm host counting the connections that reach it.
func loopbackRDP(t *testing.T) (port int, accepted *atomic.Int32) {
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

// fakeDNS replaces the resolver used for pinning.
func fakeDNS(t *testing.T, answers map[string][]string) *atomic.Int32 {
	t.Helper()
	lookups := &atomic.Int32{}
	prev := lookupNetIP
	lookupNetIP = func(_ context.Context, _, host string) ([]netip.Addr, error) {
		lookups.Add(1)
		var out []netip.Addr
		for _, a := range answers[host] {
			out = append(out, netip.MustParseAddr(a))
		}
		if len(out) == 0 {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		return out, nil
	}
	t.Cleanup(func() { lookupNetIP = prev })
	return lookups
}

// idleGuacd accepts the handshake and waits for the disconnect.
func idleGuacd(c net.Conn, r *guac.Reader, f *fakeGuacd) {
	for {
		in, err := r.Read()
		if err != nil || in.Opcode == "disconnect" {
			return
		}
	}
}

// guacError opens the tunnel and returns the "error" instruction the browser gets.
func guacError(t *testing.T, c *testClient, sessionID, token string) guac.Instruction {
	t.Helper()
	ws := c.browserTunnel(t, sessionID, token)
	defer ws.CloseNow()
	ins, _ := readInstructions(t, ws, 20*time.Second, func(in guac.Instruction) bool { return in.Opcode == "error" })
	return ins[len(ins)-1]
}

// guacConnect opens the tunnel and returns the hostname/port guacd was asked to connect to.
func guacConnect(t *testing.T, c *testClient, gd *fakeGuacd, sessionID, token string) (string, string) {
	t.Helper()
	ws := c.browserTunnel(t, sessionID, token)
	defer ws.CloseNow()
	var host, port string
	waitFor(t, "guacd connect", func() bool {
		gd.mu.Lock()
		defer gd.mu.Unlock()
		host, port = gd.connect["hostname"], gd.connect["port"]
		return host != ""
	})
	gd.mu.Lock()
	gd.connect = map[string]string{}
	gd.mu.Unlock()
	return host, port
}

func TestNetguardGuacdRefusesUsers(t *testing.T) {
	gd := newFakeGuacd(t, idleGuacd)
	port, accepted := loopbackRDP(t)
	lookups := fakeDNS(t, map[string][]string{
		"rebind.test":   {"127.0.0.1"},
		"mixed.test":    {"192.0.2.10", "127.0.0.1"},
		"metadata.test": {"169.254.169.254"},
	})
	env := newTestEnv(t, serverMode, func(c *config.Config) { c.Guacd = gd.ln.Addr().String() })
	admin := env.setup()
	bob := env.createUser(admin, "bob")

	// A literal destination is refused when the ticket is requested (403 destination_blocked), for both engines.
	for _, host := range []string{"127.0.0.1", "localhost", "127.1", "::ffff:127.0.0.1", "0.0.0.0", "169.254.169.254"} {
		for _, engine := range []string{"guacd", "ironrdp"} {
			conn := env.createConnection(bob.user, host, port, "bob", nil, model.Options{"rdpEngine": engine})
			rs := bob.openSession(conn.ID)
			if st, code := bob.errorCode("POST", "/api/sessions/"+rs.ID+"/rdp-ticket", map[string]any{}); st != http.StatusForbidden || code != netguard.CodeBlocked {
				t.Fatalf("%s ticket for %s: %d %s", engine, host, st, code)
			}
		}
	}

	// A host name is vetted after resolution, before guacd (or the certificate probe) connects: guacd is never
	// asked, and one refused address refuses the destination.
	for _, host := range []string{"rebind.test", "mixed.test", "metadata.test"} {
		conn := env.createConnection(bob.user, host, port, "bob", map[string]string{model.SecretPassword: "pw"},
			model.Options{"rdpEngine": "guacd"}) // certificate probe enabled (ignoreCert false)
		rs := bob.openSession(conn.ID)
		tr := bob.ticket(rs.ID, map[string]any{})
		last := guacError(t, bob, rs.ID, tr.Token)
		if last.Arg(1) != "771" || !strings.Contains(last.Arg(0), "not allowed in server mode") {
			t.Fatalf("%s: error %+v", host, last)
		}
		if st, msg := env.sessionState(rs.ID); st != model.StateError || !strings.Contains(msg, "not allowed") {
			t.Fatalf("%s: state %s %q", host, st, msg)
		}
	}
	// An RD Gateway on the AstraTerm host is refused too (guacd would connect to it directly).
	conn := env.createConnection(bob.user, "192.0.2.10", 3389, "bob", nil,
		model.Options{"rdpEngine": "guacd", "ignoreCert": true, "gatewayHost": "127.0.0.1", "gatewayPort": port})
	rs := bob.openSession(conn.ID)
	if st, code := bob.errorCode("POST", "/api/sessions/"+rs.ID+"/rdp-ticket", map[string]any{}); st != http.StatusForbidden || code != netguard.CodeBlocked {
		t.Fatalf("gateway ticket: %d %s", st, code)
	}
	conn = env.createConnection(bob.user, "192.0.2.10", 3389, "bob", nil,
		model.Options{"rdpEngine": "guacd", "ignoreCert": true, "gatewayHost": "rebind.test", "gatewayPort": port})
	rs = bob.openSession(conn.ID)
	tr := bob.ticket(rs.ID, map[string]any{})
	if last := guacError(t, bob, rs.ID, tr.Token); last.Arg(1) != "771" {
		t.Fatalf("gateway: error %+v", last)
	}

	if n := gd.accepted.Load(); n != 0 {
		t.Fatalf("guacd was contacted %d times for refused destinations", n)
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("the loopback service saw %d connections from a restricted user", n)
	}
	if lookups.Load() == 0 {
		t.Fatal("host names were not resolved for the check")
	}

	// The IronRDP relay's dial is guarded as well (the pool route; and without a pool).
	target := &model.Connection{Protocol: model.ProtoRDP, Host: "127.0.0.1", Port: port}
	if _, err := env.h.dialConnection(context.Background(), bob.user, target, nil); !isBlocked(err) {
		t.Fatalf("relay dial through the pool: %v", err)
	}
	bare := &handler{d: env.d, c: &core.Core{}}
	_, err := bare.dialConnection(context.Background(), bob.user, target, nil)
	if !isBlocked(err) {
		t.Fatalf("relay dial without a pool: %v", err)
	}
	if re := (&relay{h: env.h}).classify(&stageError{stage: "connect", err: err}, &ticket{host: "127.0.0.1", port: port, conn: target}); re.fail.HTTPStatus != http.StatusForbidden || !strings.Contains(re.msg, "not allowed") {
		t.Fatalf("relay classification %+v %q", re.fail, re.msg)
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("the loopback service saw %d relay connections from a restricted user", n)
	}
}

func TestNetguardGuacdPinsAndAllows(t *testing.T) {
	gd := newFakeGuacd(t, idleGuacd)
	fakeDNS(t, map[string][]string{
		"desk.test":  {"2001:db8::10", "192.0.2.10"},
		"local.test": {"127.0.0.1"},
	})
	env := newTestEnv(t, serverMode, func(c *config.Config) { c.Guacd = gd.ln.Addr().String() })
	admin := env.setup()
	bob := env.createUser(admin, "bob")
	opts := model.Options{"rdpEngine": "guacd", "ignoreCert": true}

	// A restricted user's allowed host name reaches guacd as the vetted IP (IPv4 preferred): a changed DNS answer
	// between the check and guacd's own lookup cannot redirect guacd. Display keeps the name.
	rs := bob.openSession(env.createConnection(bob.user, "desk.test", 3390, "bob", nil, opts).ID)
	tr := bob.ticket(rs.ID, map[string]any{})
	if tr.Host != "desk.test" {
		t.Fatalf("ticket host %q", tr.Host)
	}
	if host, port := guacConnect(t, bob, gd, rs.ID, tr.Token); host != "192.0.2.10" || port != "3390" {
		t.Fatalf("guacd got %s:%s, want the pinned 192.0.2.10:3390", host, port)
	}
	// An inet_aton literal is handed over canonical.
	rs = bob.openSession(env.createConnection(bob.user, "3221225994", 3389, "bob", nil, opts).ID)
	tr = bob.ticket(rs.ID, map[string]any{})
	if host, _ := guacConnect(t, bob, gd, rs.ID, tr.Token); host != "192.0.2.10" {
		t.Fatalf("guacd got %s, want 192.0.2.10", host)
	}

	// Administrators are unrestricted: the destination is handed over as configured, loopback included.
	for _, host := range []string{"127.0.0.1", "local.test"} {
		rs := admin.openSession(env.createConnection(admin.user, host, 3389, "admin", nil, opts).ID)
		tr := admin.ticket(rs.ID, map[string]any{})
		if got, _ := guacConnect(t, admin, gd, rs.ID, tr.Token); got != host {
			t.Fatalf("admin: guacd got %s, want %s", got, host)
		}
	}
}

func TestNetguardGuacdDesktopUnrestricted(t *testing.T) {
	gd := newFakeGuacd(t, idleGuacd)
	env := newTestEnv(t, func(c *config.Config) { c.Guacd = gd.ln.Addr().String() })
	admin := env.setup()
	if g := env.h.guard(&model.User{ID: "x", Role: model.RoleUser}); g != nil {
		t.Fatal("desktop mode must be unrestricted")
	}
	rs := admin.openSession(env.createConnection(admin.user, "localhost", 3389, "admin", nil,
		model.Options{"rdpEngine": "guacd", "ignoreCert": true}).ID)
	tr := admin.ticket(rs.ID, map[string]any{})
	if got, _ := guacConnect(t, admin, gd, rs.ID, tr.Token); got != "localhost" {
		t.Fatalf("desktop: guacd got %s", got)
	}
}
