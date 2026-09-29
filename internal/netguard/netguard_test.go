package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

func mustGuard(t *testing.T, p Policy) *Guard {
	t.Helper()
	g, err := NewGuard(p)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestDefaultClassification(t *testing.T) {
	g := &Guard{} // default policy
	blocked := map[string]string{
		"127.0.0.1":              ClassLoopback,
		"127.8.9.1":              ClassLoopback,
		"::1":                    ClassLoopback,
		"::ffff:127.0.0.1":       ClassLoopback, // IPv4-mapped
		"::ffff:7f00:1":          ClassLoopback,
		"::127.0.0.1":            ClassIPv4Compatible,
		"::10.0.0.1":             ClassIPv4Compatible,
		"0.0.0.0":                ClassUnspecified,
		"0.1.2.3":                ClassUnspecified,
		"::":                     ClassUnspecified,
		"169.254.169.254":        ClassLinkLocal,
		"169.254.170.2":          ClassLinkLocal,
		"fe80::1":                ClassLinkLocal,
		"fe80::1%en0":            ClassLinkLocal,
		"fd00:ec2::254":          ClassMetadata,
		"fd00:ec2::23":           ClassMetadata,
		"100.100.100.200":        ClassMetadata,
		"168.63.129.16":          ClassMetadata,
		"::ffff:169.254.169.254": ClassLinkLocal,
		"ff02::1":                ClassMulticast,
		"ff01::1":                ClassMulticast,
		"224.0.0.251":            ClassMulticast,
		"64:ff9b::7f00:1":        ClassEmbedded, // NAT64 of 127.0.0.1
		"64:ff9b::a9fe:a9fe":     ClassEmbedded, // NAT64 of 169.254.169.254
		"2002:7f00:1::1":         ClassEmbedded, // 6to4 of 127.0.0.1
	}
	for s, class := range blocked {
		d := g.Check(netip.MustParseAddr(s), 22)
		if d.Allowed || d.Class != class {
			t.Errorf("%s: got allowed=%v class=%s (%s), want blocked %s", s, d.Allowed, d.Class, d.Reason, class)
		}
	}
	allowed := map[string]string{
		"10.0.0.1":             ClassPrivate,
		"192.168.1.10":         ClassPrivate,
		"172.16.0.5":           ClassPrivate,
		"fd12:3456::1":         ClassPrivate,
		"1.1.1.1":              ClassPublic,
		"8.8.8.8":              ClassPublic,
		"100.64.0.1":           ClassPublic, // CGNAT / Tailscale
		"2606:4700:4700::1111": ClassPublic,
		"64:ff9b::808:808":     ClassPublic, // NAT64 of 8.8.8.8
		"::ffff:8.8.8.8":       ClassPublic,
	}
	for s, class := range allowed {
		d := g.Check(netip.MustParseAddr(s), 22)
		if !d.Allowed || d.Class != class {
			t.Errorf("%s: got allowed=%v class=%s (%s), want allowed %s", s, d.Allowed, d.Class, d.Reason, class)
		}
	}
	if d := g.Check(netip.Addr{}, 22); d.Allowed {
		t.Error("the invalid address must be refused")
	}
	var nilGuard *Guard
	if !nilGuard.Check(netip.MustParseAddr("127.0.0.1"), 22).Allowed || nilGuard.CheckAddr(netip.MustParseAddr("::1"), 1) != nil {
		t.Error("a nil guard must allow everything")
	}
}

func TestParseHostIPLegacyForms(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1":        "127.0.0.1",
		"127.1":            "127.0.0.1",
		"127.0.1":          "127.0.0.1",
		"2130706433":       "127.0.0.1", // decimal
		"0177.0.0.1":       "127.0.0.1", // octal
		"0x7f.0.0.1":       "127.0.0.1", // hex
		"0x7f000001":       "127.0.0.1",
		"017700000001":     "127.0.0.1",
		"0X7F.1":           "127.0.0.1",
		"0":                "0.0.0.0",
		"169.254.43518":    "169.254.169.254",
		"[::1]":            "::1",
		"[::]":             "::",
		"::ffff:127.0.0.1": "127.0.0.1",
		"fe80::1%eth0":     "fe80::1",
		"127.0.0.1.":       "127.0.0.1",
	}
	for in, want := range cases {
		a, ok := ParseHostIP(in)
		if !ok || a.String() != want {
			t.Errorf("ParseHostIP(%q) = %v, %v; want %s", in, a, ok, want)
		}
	}
	for _, in := range []string{"example.com", "256.1.1.1", "1.2.3.4.5", "1.2.3.256", "08.1.1.1", "0x", "1..2", "", "4294967296", "localhost"} {
		if a, ok := ParseHostIP(in); ok {
			t.Errorf("ParseHostIP(%q) = %v, want a host name", in, a)
		}
	}
}

func TestCheckLiteral(t *testing.T) {
	g := &Guard{}
	for _, h := range []string{"127.1", "2130706433", "0177.0.0.1", "0x7f.0.0.1", "[::]", "::ffff:127.0.0.1", "::127.0.0.1",
		"0.0.0.0", "localhost", "LOCALHOST.", "db.localhost", "fe80::1%lo0", "169.254.169.254", "[fd00:ec2::254]"} {
		err := g.CheckLiteral(h, 80)
		if _, ok := IsBlocked(err); !ok {
			t.Errorf("CheckLiteral(%q) = %v, want blocked", h, err)
		}
	}
	for _, h := range []string{"example.com", "10.1.2.3", "8.8.8.8", "internal.corp"} {
		if err := g.CheckLiteral(h, 80); err != nil {
			t.Errorf("CheckLiteral(%q) = %v", h, err)
		}
	}
}

func TestPolicyRules(t *testing.T) {
	check := func(g *Guard, ip string, port int) Decision { return g.Check(netip.MustParseAddr(ip), port) }

	g := mustGuard(t, Policy{AllowPrivate: false, Allow: []string{"10.1.2.0/24"}})
	if d := check(g, "10.9.9.9", 22); d.Allowed || d.Class != ClassPrivate {
		t.Errorf("private disabled: %+v", d)
	}
	if d := check(g, "fd12::1", 22); d.Allowed || d.Class != ClassPrivate {
		t.Errorf("ULA with private disabled: %+v", d)
	}
	if d := check(g, "10.1.2.3", 22); !d.Allowed || d.Class != ClassAllowRule || d.Rule != "10.1.2.0/24" {
		t.Errorf("exception inside a disabled private range: %+v", d)
	}
	// Same prefix length: the administrator's allow beats the built-in private deny.
	g = mustGuard(t, Policy{AllowPrivate: false, Allow: []string{"10.0.0.0/8"}})
	if d := check(g, "10.4.5.6", 22); !d.Allowed {
		t.Errorf("explicit allow of a whole private range: %+v", d)
	}
	// Longest prefix wins in both directions; a tie goes to deny.
	g = mustGuard(t, Policy{AllowPrivate: true, Deny: []string{"8.8.8.0/24", "9.9.9.9"}, Allow: []string{"8.8.8.8/32", "9.9.0.0/16", "1.1.1.0/24"}})
	if d := check(g, "8.8.8.8", 53); !d.Allowed {
		t.Errorf("more specific allow: %+v", d)
	}
	if d := check(g, "8.8.8.9", 53); d.Allowed || d.Class != ClassDenyRule || d.Rule != "8.8.8.0/24" {
		t.Errorf("deny rule: %+v", d)
	}
	if d := check(g, "9.9.9.9", 53); d.Allowed {
		t.Errorf("more specific deny: %+v", d)
	}
	g = mustGuard(t, Policy{AllowPrivate: true, Deny: []string{"1.1.1.0/24"}, Allow: []string{"1.1.1.0/24"}})
	if d := check(g, "1.1.1.1", 53); d.Allowed {
		t.Errorf("tie must deny: %+v", d)
	}
	// Exceptions for built-in ranges must be specific: allowing everything does not open loopback or metadata.
	g = mustGuard(t, Policy{AllowPrivate: true, Allow: []string{"0.0.0.0/0", "::/0"}})
	for _, ip := range []string{"127.0.0.1", "169.254.169.254", "::1", "fd00:ec2::254"} {
		if d := check(g, ip, 80); d.Allowed {
			t.Errorf("allow-all opened %s: %+v", ip, d)
		}
	}
	g = mustGuard(t, Policy{AllowPrivate: true, Allow: []string{"127.0.0.1/32"}})
	if d := check(g, "127.0.0.1", 5432); !d.Allowed || d.Class != ClassAllowRule {
		t.Errorf("loopback exception: %+v", d)
	}
	if d := check(g, "127.0.0.2", 5432); d.Allowed {
		t.Errorf("exception is too wide: %+v", d)
	}
	// An exception on an IPv4 address also covers its IPv4-mapped form.
	if d := check(g, "::ffff:127.0.0.1", 5432); !d.Allowed {
		t.Errorf("mapped form of an exception: %+v", d)
	}
}

func TestAllowedPorts(t *testing.T) {
	g := mustGuard(t, Policy{AllowPrivate: true, AllowedPorts: "22, 80,1000-2000", Allow: []string{"127.0.0.1"}})
	for port, want := range map[int]bool{22: true, 80: true, 1000: true, 1500: true, 2000: true, 25: false, 2001: false, 443: false} {
		if d := g.Check(netip.MustParseAddr("8.8.8.8"), port); d.Allowed != want {
			t.Errorf("port %d: %+v", port, d)
		}
	}
	// Exceptions do not bypass the port list.
	if d := g.Check(netip.MustParseAddr("127.0.0.1"), 5432); d.Allowed || d.Class != ClassPort {
		t.Errorf("exception bypassed allowedPorts: %+v", d)
	}
	// Port unknown (≤ 0): port rules skipped.
	if d := g.Check(netip.MustParseAddr("8.8.8.8"), 0); !d.Allowed {
		t.Errorf("unknown port: %+v", d)
	}
}

func TestPolicyValidation(t *testing.T) {
	p := Policy{Deny: []string{" 10.1.2.3/8 ", "10.0.0.0/8", "::ffff:192.168.0.0/112", "2001:db8::1"}, Allow: []string{"", "[::1]"},
		AllowedPorts: "443,22,80-90,85-100,23"}
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Deny, " ") != "10.0.0.0/8 192.168.0.0/16 2001:db8::1/128" || strings.Join(p.Allow, " ") != "::1/128" {
		t.Errorf("normalized rules: deny=%v allow=%v", p.Deny, p.Allow)
	}
	if p.AllowedPorts != "22-23,80-100,443" {
		t.Errorf("normalized ports: %q", p.AllowedPorts)
	}
	bad := []Policy{
		{Deny: []string{"10.0.0.0/33"}},
		{Deny: []string{"example.com"}},
		{Allow: []string{"fe80::1%eth0"}},
		{Allow: []string{"::ffff:10.0.0.0/64"}},
		{AllowedPorts: "0"},
		{AllowedPorts: "70000"},
		{AllowedPorts: "5-3"},
		{AllowedPorts: "ssh"},
		{Deny: make([]string, maxRules+1)},
		{Deny: []string{strings.Repeat("1", 80)}},
	}
	for i := range bad[8].Deny {
		bad[8].Deny[i] = "10.0.0." + strconv.Itoa(i%250) + "/32"
	}
	for i, p := range bad {
		if err := p.Normalize(); err == nil {
			t.Errorf("bad policy %d accepted: %+v", i, p)
		}
	}
}

func TestSelfAndHostAddresses(t *testing.T) {
	c, err := compile(Policy{AllowPrivate: true, BlockHostAddresses: true, Allow: []string{"127.0.0.1/32", "203.0.113.8/32"}})
	if err != nil {
		t.Fatal(err)
	}
	hosts := map[netip.Addr]bool{netip.MustParseAddr("203.0.113.7"): true, netip.MustParseAddr("203.0.113.8"): true}
	wild := &env{listenPort: 7822, listenWildcard: true, hostAddrs: func() map[netip.Addr]bool { return hosts }}
	for _, tc := range []struct {
		ip      string
		port    int
		allowed bool
		class   string
	}{
		{"127.0.0.1", 7822, false, ClassAstraTerm}, // AstraTerm's own listener, even with a loopback exception
		{"203.0.113.7", 7822, false, ClassAstraTerm},
		{"203.0.113.8", 7822, false, ClassAstraTerm}, // exceptions never open AstraTerm's port
		{"0.0.0.0", 7822, false, ClassAstraTerm},
		{"127.0.0.1", 7823, true, ClassAllowRule},
		{"203.0.113.7", 22, false, ClassHost}, // a host address, any port
		{"203.0.113.8", 22, true, ClassAllowRule},
		{"8.8.8.8", 7822, true, ClassPublic},
	} {
		d := c.check(netip.MustParseAddr(tc.ip), tc.port, wild)
		if d.Allowed != tc.allowed || d.Class != tc.class {
			t.Errorf("%s:%d: %+v", tc.ip, tc.port, d)
		}
	}
	specific := &env{listenPort: 7822, listenIPs: []netip.Addr{netip.MustParseAddr("203.0.113.7")}}
	if d := c.check(netip.MustParseAddr("127.0.0.1"), 7822, specific); !d.Allowed {
		t.Errorf("loopback exception with a specific listen address: %+v", d)
	}
	if d := c.check(netip.MustParseAddr("203.0.113.7"), 7822, specific); d.Class != ClassAstraTerm {
		t.Errorf("specific listen address: %+v", d)
	}
	c2, _ := compile(Policy{AllowPrivate: true, BlockHostAddresses: false})
	if d := c2.check(netip.MustParseAddr("203.0.113.7"), 22, wild); !d.Allowed {
		t.Errorf("blockHostAddresses off: %+v", d)
	}
	for _, listen := range []string{"0.0.0.0:7822", "[::]:7822", ":7822", "localhost:7822", "10.0.0.5:7822"} {
		e := newEnv(&app.Deps{Cfg: &config.Config{Listen: listen}})
		if e.listenPort != 7822 {
			t.Errorf("%s: port %d", listen, e.listenPort)
		}
		if got := e.isSelf(netip.MustParseAddr("127.0.0.1"), 7822); got == (listen == "10.0.0.5:7822") {
			t.Errorf("%s: loopback self = %v", listen, got)
		}
	}
}

func TestControlAndDialer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
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
	g := &Guard{}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	for _, addr := range []string{ln.Addr().String(), "localhost:" + port} {
		c, err := g.Dialer(2*time.Second).Dial("tcp", addr)
		if err == nil {
			c.Close()
			t.Fatalf("guarded dial to %s succeeded", addr)
		}
		be, ok := IsBlocked(err)
		if !ok || !strings.Contains(err.Error(), "not allowed in server mode") || !strings.Contains(err.Error(), "loopback") {
			t.Fatalf("dial %s: %v", addr, err)
		}
		var he *httpx.HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusForbidden || he.Code != CodeBlocked {
			t.Fatalf("blocked error does not map to 403 %s: %#v", CodeBlocked, he)
		}
		if be.Class != ClassLoopback {
			t.Fatalf("class %s", be.Class)
		}
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("the listener saw %d connections", n)
	}
	for _, tc := range [][2]string{{"unix", "/var/run/docker.sock"}, {"tcp", "[fe80::1%lo0]:22"}, {"tcp", "not-an-ip:22"},
		{"udp4", "169.254.169.254:53"}, {"tcp6", "[::ffff:127.0.0.1]:22"}} {
		if err := g.Control(tc[0], tc[1], nil); err == nil {
			t.Errorf("Control(%s, %s) allowed", tc[0], tc[1])
		}
	}
	if err := g.Control("tcp", "10.0.0.1:22", nil); err != nil {
		t.Errorf("private address refused: %v", err)
	}
	// Unrestricted.
	var ng *Guard
	c, err := ng.Dialer(2*time.Second).Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("unguarded dial: %v", err)
	}
	c.Close()
}

// TestDNSRebinding: a name resolving to a public address when checked and to loopback when dialed is still refused,
// because the dial-time Control hook sees the concrete address.
func TestDNSRebinding(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
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
	var aQueries atomic.Int32
	mux := dns.NewServeMux()
	mux.HandleFunc(".", func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		q := r.Question[0]
		if q.Qtype == dns.TypeA && strings.EqualFold(q.Name, "rebind.test.") {
			ip := "93.184.216.34"
			if aQueries.Add(1) > 1 {
				ip = "127.0.0.1"
			}
			rr, _ := dns.NewRR(q.Name + " 0 IN A " + ip)
			m.Answer = append(m.Answer, rr)
		}
		_ = w.WriteMsg(m)
	})
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: mux}
	go func() { _ = srv.ActivateAndServe() }()
	defer srv.Shutdown()
	res := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", pc.LocalAddr().String())
	}}
	old := resolver
	resolver = res
	defer func() { resolver = old }()

	g := &Guard{}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := g.CheckHostPort(ctx, "rebind.test.", p); err != nil {
		t.Fatalf("first (public) answer: %v", err)
	}
	d := g.Dialer(2 * time.Second)
	d.Resolver = res
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort("rebind.test.", port))
	if err == nil {
		c.Close()
		t.Fatal("rebound dial succeeded")
	}
	if _, ok := IsBlocked(err); !ok {
		t.Fatalf("rebound dial: %v", err)
	}
	if accepted.Load() != 0 {
		t.Fatal("the loopback listener was reached")
	}
	// The advisory check now sees the loopback answer too.
	if _, ok := IsBlocked(g.CheckHostPort(ctx, "rebind.test.", p)); !ok {
		t.Fatal("CheckHostPort after rebinding")
	}
}

func TestTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("secret")) }))
	defer srv.Close()
	g := &Guard{}
	cl := &http.Client{Transport: g.Transport(nil), Timeout: 5 * time.Second}
	if resp, err := cl.Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("guarded HTTP request to loopback succeeded")
	} else if _, ok := IsBlocked(err); !ok {
		t.Fatalf("guarded request: %v", err)
	}
	if tr := g.Transport(nil); tr.Proxy != nil {
		t.Fatal("a restricted transport must not use environment proxies")
	}
	cl = &http.Client{Transport: (*Guard)(nil).Transport(nil), Timeout: 5 * time.Second}
	resp, err := cl.Get(srv.URL)
	if err != nil {
		t.Fatalf("unrestricted transport: %v", err)
	}
	resp.Body.Close()
}

func TestManagerForUser(t *testing.T) {
	admin := &model.User{ID: "a", Role: model.RoleAdmin}
	user := &model.User{ID: "u", Role: model.RoleUser}
	desk := For(&app.Deps{Cfg: &config.Config{Mode: config.ModeDesktop, Listen: "127.0.0.1:7822"}})
	if desk.ForUser(user) != nil || desk.ForUser(nil) != nil {
		t.Fatal("desktop mode must be unrestricted")
	}
	if For(nil).ForUser(user) != nil || ForUser(nil, user) != nil {
		t.Fatal("no deps: unrestricted")
	}
	d := &app.Deps{Cfg: &config.Config{Mode: config.ModeServer, Listen: "0.0.0.0:7822"}}
	m := For(d)
	if For(d) != m {
		t.Fatal("For must return the same manager")
	}
	if m.ForUser(admin) != nil {
		t.Fatal("admins are unrestricted by default")
	}
	if m.ForUser(user) == nil || m.ForUser(nil) == nil {
		t.Fatal("ordinary (and unknown) users are restricted")
	}
	if _, err := m.SetPolicy(context.Background(), Policy{AllowPrivate: true, ApplyToAdmins: true}); err != nil {
		t.Fatal(err)
	}
	g := m.ForUser(admin)
	if g == nil {
		t.Fatal("applyToAdmins")
	}
	if d := g.Check(netip.MustParseAddr("127.0.0.1"), 7822); d.Class != ClassAstraTerm {
		t.Fatalf("manager guard knows the listener: %+v", d)
	}
	if _, err := m.SetPolicy(context.Background(), Policy{AllowedPorts: "x"}); err == nil {
		t.Fatal("invalid policy saved")
	}
	if !m.Policy().ApplyToAdmins {
		t.Fatal("an invalid save replaced the policy")
	}
}
