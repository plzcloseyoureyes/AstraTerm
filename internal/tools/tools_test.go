package tools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// collector accumulates the rows a sink emits so job runners can be tested without an events hub.
type collector struct {
	mu     sync.Mutex
	rows   []row
	events int
}

func (c *collector) sink() *sink {
	return newSink(func(v any) {
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		rows, ok := m["rows"].([]row)
		if !ok {
			return
		}
		c.mu.Lock()
		c.rows = append(c.rows, rows...)
		c.events++
		c.mu.Unlock()
	})
}

func (c *collector) all() []row {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]row(nil), c.rows...)
}

func (c *collector) byKind(kind string) []row {
	var out []row
	for _, r := range c.all() {
		if r["kind"] == kind {
			out = append(out, r)
		}
	}
	return out
}

// testCall is a desktop-mode invocation (no guard) by a regular user.
func testCall(body string) *call {
	return &call{h: newHandler(nil, nil), user: &model.User{ID: "u1", Role: model.RoleUser}, body: []byte(body)}
}

// tryJob prepares and runs a tool like runTool does (validation first, then the job with a flushing sink).
func tryJob(ctx context.Context, prepare prepareFunc, cl *call) (*collector, error) {
	run, err := prepare(ctx, cl)
	if err != nil {
		return nil, err
	}
	c := &collector{}
	s := c.sink()
	err = run(ctx, s)
	s.close()
	return c, err
}

// runJob drives a tool to completion and fails the test on any error.
func runJob(t *testing.T, prepare prepareFunc, bodyJSON string) *collector {
	t.Helper()
	c, err := tryJob(context.Background(), prepare, testCall(bodyJSON))
	if err != nil {
		t.Fatalf("job returned error: %v", err)
	}
	return c
}

// wantBadRequest asserts that prepare rejects the body with a 400.
func wantBadRequest(t *testing.T, prepare prepareFunc, body string) {
	t.Helper()
	_, err := prepare(context.Background(), testCall(body))
	var he *httpx.HTTPError
	if !errors.As(err, &he) || he.Status != 400 {
		t.Errorf("body %s: want 400, got %v", body, err)
	}
}

func TestSinkBatchesInOrderAndCloses(t *testing.T) {
	c := &collector{}
	s := c.sink()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				s.add(row{"kind": "n", "g": g, "i": i})
			}
		}(g)
	}
	wg.Wait()
	s.emitNow(row{"kind": "summary"})
	s.close()
	s.add(row{"kind": "late"}) // dropped after close
	s.close()                  // idempotent
	rows := c.all()
	if len(rows) != 8*500+1 || rows[len(rows)-1]["kind"] != "summary" {
		t.Fatalf("got %d rows, last %v", len(rows), rows[len(rows)-1])
	}
	last := map[int]int{}
	for _, r := range rows[:len(rows)-1] {
		g, i := r["g"].(int), r["i"].(int)
		if prev, ok := last[g]; ok && i != prev+1 {
			t.Fatalf("goroutine %d rows out of order: %d after %d", g, i, prev)
		}
		last[g] = i
	}
	if c.events > 20 {
		t.Errorf("4001 rows produced %d events; batching is not effective", c.events)
	}
}

func TestSinkFlushesOnTimer(t *testing.T) {
	c := &collector{}
	s := c.sink()
	defer s.close()
	s.add(row{"kind": "lonely"})
	deadline := time.Now().Add(time.Second)
	for len(c.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(c.all()) != 1 {
		t.Fatal("a single pending row was not flushed by the ticker")
	}
}

func TestJobSlots(t *testing.T) {
	h := newHandler(nil, nil)
	u := &model.User{ID: "a"}
	var releases []func()
	for i := 0; i < maxJobsPerUser; i++ {
		rel, err := h.acquireSlot(u, toolPing, 0)
		if err != nil {
			t.Fatalf("slot %d: %v", i, err)
		}
		releases = append(releases, rel)
	}
	if _, err := h.acquireSlot(u, toolDNS, 0); err == nil {
		t.Fatal("per-user limit not enforced")
	}
	releases[0]()
	releases[0]() // idempotent
	if _, err := h.acquireSlot(u, toolDNS, 0); err != nil {
		t.Fatalf("slot after release: %v", err)
	}
	other := &model.User{ID: "b"}
	rel, err := h.acquireSlot(other, toolThroughput, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.acquireSlot(other, toolThroughput, 1); err == nil {
		t.Fatal("per-tool limit not enforced")
	}
	rel()
	if _, err := h.acquireSlot(other, toolThroughput, 1); err != nil {
		t.Fatalf("per-tool slot after release: %v", err)
	}
}

func TestBlockedAddr(t *testing.T) {
	blocked := []string{"127.0.0.1", "127.8.9.1", "::1", "0.0.0.0", "0.1.2.3", "::", "169.254.169.254", "fe80::1",
		"::ffff:127.0.0.1", "fd00:ec2::254", "100.100.100.200", "::127.0.0.1", "ff02::1"}
	allowed := []string{"10.0.0.1", "192.168.1.10", "172.16.0.5", "1.1.1.1", "2606:4700:4700::1111", "fd12:3456::1", "100.64.0.1"}
	for _, s := range blocked {
		if !blockedAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range allowed {
		if blockedAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
	var g *netGuard
	if g.checkIP(net.ParseIP("127.0.0.1")) != nil || g.dialer(time.Second).Control != nil {
		t.Error("a nil guard must allow everything")
	}
	g = &netGuard{}
	if g.checkIP(net.ParseIP("127.0.0.1")) == nil || g.control("tcp", "[::1]:22", nil) == nil {
		t.Error("guard must refuse loopback")
	}
	if g.control("tcp", "10.1.2.3:22", nil) != nil {
		t.Error("guard must allow private addresses")
	}
}

func TestGuardedDialerRefusesLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	g := &netGuard{}
	if c, err := g.dialer(time.Second).Dial("tcp", ln.Addr().String()); err == nil {
		c.Close()
		t.Fatal("guarded dial to loopback succeeded")
	}
	c, err := (*netGuard)(nil).dialer(time.Second).Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("unguarded dial: %v", err)
	}
	c.Close()
}

func TestGuardForMode(t *testing.T) {
	h := newHandler(nil, nil)
	if h.guardFor(&model.User{Role: model.RoleUser}) != nil {
		t.Fatal("desktop mode must not restrict")
	}
	if err := h.gate(&model.User{Role: model.RoleUser}, true); err != nil {
		t.Fatalf("desktop gate: %v", err)
	}
}

func TestParsePorts(t *testing.T) {
	got, err := parsePorts("22,80,90-92,22")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[22 80 90 91 92]" {
		t.Fatalf("got %v", got)
	}
	if top, _ := parsePorts("top100"); len(top) < 50 {
		t.Fatalf("top100 expanded to %d ports", len(top))
	}
	if all, _ := parsePorts("all"); len(all) != 65535 {
		t.Fatalf("all = %d ports", len(all))
	}
	for _, bad := range []string{"nope", "0", "70000", "1-70000", "0-5", "a-b"} {
		if _, err := parsePorts(bad); err == nil {
			t.Errorf("parsePorts(%q) should fail", bad)
		}
	}
	if def, _ := parsePorts(""); len(def) == 0 {
		t.Fatal("empty spec should default to top ports")
	}
	if err := checkProbeBudget(4, 65535); err != nil {
		t.Errorf("4 hosts × all ports should be allowed: %v", err)
	}
	if err := checkProbeBudget(5, 65535); err == nil {
		t.Error("5 hosts × all ports should exceed the budget")
	}
}

func TestExpandHosts(t *testing.T) {
	cidr, err := expandHosts("192.168.1.0/30")
	if err != nil {
		t.Fatal(err)
	}
	// /30 → 4 addresses, minus network + broadcast = .1 and .2
	if len(cidr) != 2 || cidr[0] != "192.168.1.1" || cidr[1] != "192.168.1.2" {
		t.Fatalf("cidr /30 = %v", cidr)
	}
	rng, err := expandHosts("10.0.0.5-8")
	if err != nil {
		t.Fatal(err)
	}
	if len(rng) != 4 || rng[0] != "10.0.0.5" || rng[3] != "10.0.0.8" {
		t.Fatalf("range = %v", rng)
	}
	full, err := expandHosts("10.0.0.254-10.0.1.1")
	if err != nil || len(full) != 4 {
		t.Fatalf("full range = %v, %v", full, err)
	}
	list, err := expandHosts("127.0.0.1, example.test\nweb-01.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[2] != "web-01.example.com" {
		t.Fatalf("list = %v (host names with hyphens must not be treated as ranges)", list)
	}
	single, err := expandHosts("192.168.1.10/32")
	if err != nil || len(single) != 1 || single[0] != "192.168.1.10" {
		t.Fatalf("/32 = %v %v", single, err)
	}
	if v6, err := expandHosts("2001:db8::/126"); err != nil || len(v6) != 4 {
		t.Fatalf("v6 /126 = %v %v", v6, err)
	}
	for _, bad := range []string{"10.0.0.0/8", "2001:db8::/64", "-oProxyCommand=x", "a;b", "10.0.0.1-300", ""} {
		if _, err := expandHosts(bad); err == nil {
			t.Errorf("expandHosts(%q) should fail", bad)
		}
	}
}

func TestSafeHostArgAndShellQuote(t *testing.T) {
	for _, ok := range []string{"example.com", "10.0.0.1", "host-1.local", "fe80::1", "[2001:db8::1]", "fe80::1%eth0", "_srv.example.com"} {
		if _, err := safeHostArg(ok); err != nil {
			t.Errorf("safeHostArg(%q) unexpectedly rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"a; rm -rf /", "$(reboot)", "a`b`", "a b", "a|b", "-f", "--help", "-oProxyCommand=sh", "", "a'b", strings.Repeat("a", 300)} {
		if _, err := safeHostArg(bad); err == nil {
			t.Errorf("safeHostArg(%q) should be rejected", bad)
		}
	}
	if shellQuote("a'b") != `'a'\''b'` {
		t.Errorf("shellQuote escaping wrong: %q", shellQuote("a'b"))
	}
}

func TestServiceName(t *testing.T) {
	if serviceName(22, false) != "ssh" || serviceName(443, false) != "https" {
		t.Fatal("tcp service names wrong")
	}
	if serviceName(53, true) != "domain" || serviceName(161, true) != "snmp" {
		t.Fatal("udp service names wrong")
	}
	if serviceName(65000, false) != "" {
		t.Fatal("unknown port should have no name")
	}
}

func TestVendorForMAC(t *testing.T) {
	cases := map[string]string{
		"00:0c:29:aa:bb:cc": "VMware",
		"B8-27-EB-11-22-33": "Raspberry Pi",
		"525400abcdef":      "QEMU/KVM",
		"ff:ff:ff:ff:ff:ff": "",
	}
	for mac, want := range cases {
		if got := vendorForMAC(mac); got != want {
			t.Errorf("vendorForMAC(%q) = %q want %q", mac, got, want)
		}
	}
}

func TestParseMACAndMagicPacket(t *testing.T) {
	mac, err := parseMAC("01:02:03:04:05:06")
	if err != nil {
		t.Fatal(err)
	}
	pkt := buildMagicPacket(mac, nil)
	if len(pkt) != 6+16*6 {
		t.Fatalf("magic packet length %d", len(pkt))
	}
	for i := 0; i < 6; i++ {
		if pkt[i] != 0xFF || pkt[6+i] != mac[i] || pkt[6+15*6+i] != mac[i] {
			t.Fatal("magic packet layout wrong")
		}
	}
	sec, _ := parseMAC("aabbccddeeff")
	if p := buildMagicPacket(mac, sec); len(p) != 6+16*6+6 {
		t.Fatalf("SecureOn packet length %d", len(p))
	}
	for _, ok := range []string{"01-02-03-04-05-06", "0102.0304.0506", "010203040506"} {
		if _, err := parseMAC(ok); err != nil {
			t.Errorf("parseMAC(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"xyz", "01:02:03:04:05", "01:02:03:04:05:06:07", "0x1:02:03:04:05:06:07:08:09:10:11:12", "01 02 03 04 05 06"} {
		if _, err := parseMAC(bad); err == nil {
			t.Errorf("parseMAC(%q) should fail", bad)
		}
	}
}

func TestResolverAddr(t *testing.T) {
	cases := map[string]string{
		"1.1.1.1":           "1.1.1.1:53",
		"8.8.8.8:5353":      "8.8.8.8:5353",
		"2606:4700::1111":   "[2606:4700::1111]:53",
		"[2606:4700::1111]": "[2606:4700::1111]:53",
		"dns.google":        "dns.google:53",
	}
	for in, want := range cases {
		got, err := resolverAddr(in, false)
		if err != nil || got != want {
			t.Errorf("resolverAddr(%q) = %q, %v want %q", in, got, err, want)
		}
	}
	if got, _ := resolverAddr("9.9.9.9", true); got != "9.9.9.9:853" {
		t.Errorf("DoT default port: %q", got)
	}
	for _, bad := range []string{"1.1.1.1:0", "1.1.1.1:99999", "a b", "x/y"} {
		if _, err := resolverAddr(bad, false); err == nil {
			t.Errorf("resolverAddr(%q) should fail", bad)
		}
	}
}

func TestValidationErrorsAreBadRequests(t *testing.T) {
	wantBadRequest(t, preparePing, `{}`)
	wantBadRequest(t, preparePing, `{"host":"x","mode":"udp"}`)
	wantBadRequest(t, preparePing, `{"host":"x","port":70000}`)
	wantBadRequest(t, preparePing, `{"host":"-f","viaConnectionId":"c1"}`)
	wantBadRequest(t, preparePing, `not json`)
	wantBadRequest(t, prepareTraceroute, `{"host":"a;b"}`)
	wantBadRequest(t, prepareTraceroute, `{"host":"example.com","mode":"bogus"}`)
	wantBadRequest(t, preparePortscan, `{"targets":"10.0.0.0/16","ports":"all"}`)
	wantBadRequest(t, preparePortscan, `{"targets":"127.0.0.1","ports":"0-10"}`)
	wantBadRequest(t, prepareNetscan, `{}`)
	wantBadRequest(t, prepareDNS, `{"name":"example.com","type":"BOGUS"}`)
	wantBadRequest(t, prepareDNS, `{"name":"a b"}`)
	wantBadRequest(t, prepareWhois, `{"query":""}`)
	wantBadRequest(t, prepareWhois, `{"query":"x","server":"$(id)"}`)
	wantBadRequest(t, prepareWOL, `{"mac":"nope"}`)
	wantBadRequest(t, prepareWOL, `{"mac":"01:02:03:04:05:06","port":0,"secureOn":"zz"}`)
	wantBadRequest(t, prepareHTTPCheck, `{"url":"ftp://example.com"}`)
	wantBadRequest(t, prepareHTTPCheck, `{"url":"https://example.com","headers":{"Bad Header":"x"}}`)
	wantBadRequest(t, prepareHTTPCheck, `{"url":"https://example.com","headers":{"X-A":"line\r\nInjected: 1"}}`)
	wantBadRequest(t, prepareHTTPCheck, `{"url":"https://example.com","method":"GE T"}`)
	wantBadRequest(t, prepareTLSCert, `{"host":"example.com","port":70000}`)
	wantBadRequest(t, prepareTLSCert, `{"host":"example.com","startTls":"gopher"}`)
	wantBadRequest(t, prepareSNMP, `{"host":"10.0.0.1","oid":"notAMibName.0"}`)
	wantBadRequest(t, prepareSNMP, `{"host":"10.0.0.1","version":"3","username":"u","secLevel":"authNoPriv","authProto":"SHA","authPass":"short"}`)
	wantBadRequest(t, prepareSNMP, `{"host":"10.0.0.1","operation":"set"}`)
	wantBadRequest(t, prepareSSHAudit, `{"host":""}`)
	wantBadRequest(t, prepareThroughput, `{"mode":"iperf3"}`)
	wantBadRequest(t, prepareThroughput, `{"mode":"ssh"}`)
	wantBadRequest(t, prepareThroughput, `{"mode":"udp","host":"x"}`)
}

func TestHopStats(t *testing.T) {
	s := newHopStats(3)
	s.sent = 5
	for _, v := range []float64{10, 12, 14} {
		s.addReply("10.0.0.1", v)
	}
	s.addReply("10.0.0.2", 16)
	r := s.row(context.Background(), newRDNSCache(), false)
	if r["recv"] != 4 || r["loss"] != 20.0 || r["bestMs"] != 10.0 || r["worstMs"] != 16.0 || r["avgMs"] != 13.0 || r["lastMs"] != 16.0 {
		t.Fatalf("stats row = %v", r)
	}
	if sd := r["stdevMs"].(float64); sd < 2.23 || sd > 2.24 { // population stddev of 10,12,14,16 = √5
		t.Fatalf("stdev = %v", sd)
	}
	if r["from"] != "10.0.0.1" || len(r["hosts"].([]string)) != 2 {
		t.Fatalf("responders = %v %v", r["from"], r["hosts"])
	}
}

func TestMIBNames(t *testing.T) {
	cases := map[string]string{
		"sysDescr.0":                 "1.3.6.1.2.1.1.1.0",
		"SNMPv2-MIB::sysName.0":      "1.3.6.1.2.1.1.5.0",
		"ifTable":                    "1.3.6.1.2.1.2.2",
		"1.3.6.1.2.1.1.3.0":          "1.3.6.1.2.1.1.3.0",
		".1.3.6.1.4.1.2021.10.1.3.1": "1.3.6.1.4.1.2021.10.1.3.1",
		"IFDESCR.3":                  "1.3.6.1.2.1.2.2.1.2.3",
	}
	for in, want := range cases {
		if got, ok := resolveOIDName(in); !ok || got != want {
			t.Errorf("resolveOIDName(%q) = %q, %v want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"bogus", "sysDescr.x", "1..2", ""} {
		if _, ok := resolveOIDName(bad); ok {
			t.Errorf("resolveOIDName(%q) should fail", bad)
		}
	}
	if l := oidLabel("1.3.6.1.2.1.2.2.1.2.7"); l != "ifDescr.7" {
		t.Errorf("label = %q", l)
	}
	if l := oidLabel("1.3.6.1.4.1.9.1.1"); l != "enterprises.9.1.1" {
		t.Errorf("label = %q", l)
	}
	if l := oidLabel("1.2.840.10045"); l != "" {
		t.Errorf("label for an unknown tree = %q", l)
	}
}

func TestNetBIOSStatusParsing(t *testing.T) {
	req := nbstatRequest(0x1234)
	if len(req) != 50 || string(req[13:15]) != "CK" || req[12] != 0x20 || req[len(req)-3] != 0x21 {
		t.Fatalf("request = % x", req)
	}
	// Build a response: header, answer name (compression pointer), NBSTAT RR with two names + MAC.
	resp := []byte{0x12, 0x34, 0x84, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00}
	resp = append(resp, req[12:46]...) // the encoded name
	resp = append(resp, 0x00, 0x21, 0x00, 0x01, 0, 0, 0, 0)
	names := []byte{2}
	entry := func(name string, suffix byte, flags uint16) {
		n := []byte(fmt.Sprintf("%-15s", name))
		names = append(names, n...)
		names = append(names, suffix, byte(flags>>8), byte(flags))
	}
	entry("WORKGROUP", 0x00, 0x8400)
	entry("FILESRV", 0x00, 0x0400)
	names = append(names, 0x00, 0x15, 0x5d, 0x01, 0x02, 0x03)
	names = append(names, make([]byte, 40)...)
	resp = append(resp, byte(len(names)>>8), byte(len(names)))
	resp = append(resp, names...)
	info, err := parseNBStat(resp, 0x1234)
	if err != nil {
		t.Fatal(err)
	}
	if info.name != "FILESRV" || info.group != "WORKGROUP" || info.mac != "00:15:5d:01:02:03" {
		t.Fatalf("info = %+v", info)
	}
	if _, err := parseNBStat(resp, 0x9999); err == nil {
		t.Fatal("transaction id mismatch should fail")
	}
	if _, err := parseNBStat(resp[:40], 0x1234); err == nil {
		t.Fatal("truncated response should fail")
	}
}

func TestSanitizers(t *testing.T) {
	if s := sanitizeBanner([]byte("SSH-2.0-OpenSSH_9.6\x1b[31m\r\nrest")); s != "SSH-2.0-OpenSSH_9.6.[31m" {
		t.Errorf("banner = %q", s)
	}
	if s := sanitizeText("a\x00b\r\nc\x1bd\u009be"); s != "ab\ncde" {
		t.Errorf("text = %q", s)
	}
}
