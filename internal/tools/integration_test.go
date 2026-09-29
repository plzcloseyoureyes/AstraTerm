package tools

import (
	"os"
	"strings"
	"testing"
)

// These tests hit the shared Docker test environment and the internet; they run only when NEXTERM_TESTENV=1.
// (Tools that run "via" a saved SSH connection are covered end-to-end through the real server in ./apitest.)
func requireTestenv(t *testing.T) {
	t.Helper()
	if os.Getenv("NEXTERM_TESTENV") != "1" {
		t.Skip("set NEXTERM_TESTENV=1 to run tests against the Docker test environment")
	}
}

func TestSSHAuditIntegration(t *testing.T) {
	requireTestenv(t)
	c := runJob(t, prepareSSHAudit, `{"host":"127.0.0.1","port":22022,"timeoutMs":8000}`)

	if srv := c.byKind("server"); len(srv) == 0 || !strings.HasPrefix(srv[0]["banner"].(string), "SSH-2.0-") {
		t.Fatalf("expected a server banner row, got %v", c.all())
	}
	cats := c.byKind("category")
	if len(cats) < 4 {
		t.Fatalf("expected algorithm categories, got %d", len(cats))
	}
	for _, cat := range cats {
		if cat["id"] == "kex" {
			if items, ok := cat["algorithms"].([]row); !ok || len(items) == 0 {
				t.Fatal("kex category has no algorithms")
			}
		}
	}
	if len(c.byKind("terrapin")) == 0 {
		t.Fatal("expected a terrapin assessment")
	}
	keys := c.byKind("hostkey")
	if len(keys) == 0 {
		t.Fatal("expected host key fingerprints")
	}
	for _, k := range keys {
		if !strings.HasPrefix(k["sha256"].(string), "SHA256:") {
			t.Fatalf("host key row = %v", k)
		}
	}
	if sum := c.byKind("summary"); len(sum) == 0 || sum[0]["grade"] == nil {
		t.Fatalf("expected a summary with a grade, got %v", sum)
	}
}

func TestPortscanIntegration(t *testing.T) {
	requireTestenv(t)
	// ssh1 listens on 22022, telnet on 22023; 22024 is closed.
	c := runJob(t, preparePortscan, `{"targets":"127.0.0.1","ports":"22022-22024","banner":true,"showClosed":true,"timeoutMs":1500}`)
	states := map[any]any{}
	for _, r := range c.byKind("port") {
		states[r["port"]] = r["state"]
		if r["port"] == 22022 && !strings.HasPrefix(r["banner"].(string), "SSH-2.0-") {
			t.Errorf("ssh banner = %v", r["banner"])
		}
	}
	if states[22022] != "open" || states[22023] != "open" || states[22024] != "closed" {
		t.Fatalf("states = %v", states)
	}
}

func TestDNSIntegration(t *testing.T) {
	requireTestenv(t)
	c := runJob(t, prepareDNS, `{"name":"one.one.one.one","type":"A","resolver":"1.1.1.1"}`)
	if len(c.byKind("record")) == 0 || len(c.byKind("summary")) == 0 {
		t.Fatalf("expected DNS records, got %v", c.all())
	}
	c = runJob(t, prepareDNS, `{"name":"1.1.1.1","type":"PTR","resolver":"1.1.1.1","dot":true}`)
	recs := c.byKind("record")
	if len(recs) == 0 || !strings.Contains(recs[0]["data"].(string), "one.one.one.one") {
		t.Fatalf("DoT PTR = %v", c.all())
	}
}

func TestPingIntegration(t *testing.T) {
	requireTestenv(t)
	if icmpMode(false) == "" {
		t.Skip("no ICMP socket available to this process (TCP fallback is covered by TestTCPPingLoopback)")
	}
	c := runJob(t, preparePing, `{"host":"127.0.0.1","count":2,"intervalMs":200,"timeoutMs":1000}`)
	sum := c.byKind("summary")
	if len(sum) == 0 || sum[0]["recv"] != 2 {
		t.Fatalf("expected a ping summary, got %v", c.all())
	}
}

func TestTracerouteInternetIntegration(t *testing.T) {
	requireTestenv(t)
	c := runJob(t, prepareTraceroute, `{"host":"1.1.1.1","maxHops":20,"probes":1,"timeoutMs":1500,"resolveNames":false}`)
	hops := c.byKind("hop")
	sum := c.byKind("summary")
	if len(hops) < 2 || len(sum) != 1 {
		t.Fatalf("hops=%v summary=%v", hops, sum)
	}
	answered := 0
	for _, h := range hops[:len(hops)-1] {
		if h["timeout"] == false {
			answered++
		}
	}
	if answered == 0 {
		t.Fatalf("no intermediate hop answered (time exceeded not received?): %v", hops)
	}
	engine, _ := sum[0]["engine"].(string)
	// UDP probes may not reach through NAT'd container networks; ICMP echo must.
	if strings.HasPrefix(engine, "ICMP") && sum[0]["reached"] != true {
		t.Fatalf("ICMP trace did not reach 1.1.1.1: %v", hops)
	}
	t.Logf("engine %v: %d hops, %d intermediate answered, reached=%v", engine, len(hops), answered, sum[0]["reached"])
}

func TestHTTPCheckIntegration(t *testing.T) {
	requireTestenv(t)
	c := runJob(t, prepareHTTPCheck, `{"url":"https://one.one.one.one/","followRedirects":true}`)
	res := c.byKind("result")
	if len(res) != 1 || res[0]["status"] != 200 || res[0]["tls"] == nil {
		t.Fatalf("result = %v", c.all())
	}
	timing := res[0]["timing"].(row)
	if timing["tlsMs"].(float64) <= 0 || timing["connectMs"].(float64) <= 0 {
		t.Fatalf("timing = %v", timing)
	}
}

func TestTLSCertIntegration(t *testing.T) {
	requireTestenv(t)
	c := runJob(t, prepareTLSCert, `{"host":"one.one.one.one"}`)
	conn := c.byKind("connection")
	if len(conn) != 1 || conn[0]["verified"] != true {
		t.Fatalf("connection = %v", c.all())
	}
	if len(c.byKind("version")) != 4 {
		t.Fatalf("version rows = %v", c.byKind("version"))
	}
}

func TestWhoisIntegration(t *testing.T) {
	requireTestenv(t)
	c := runJob(t, prepareWhois, `{"query":"example.com"}`)
	if text := c.byKind("text"); len(text) != 1 || !strings.Contains(strings.ToLower(text[0]["text"].(string)), "example.com") {
		t.Fatalf("whois = %v", c.all())
	}
}

func TestNetscanIntegration(t *testing.T) {
	requireTestenv(t)
	c := runJob(t, prepareNetscan, `{"targets":"127.0.0.1","ports":"22022,22023,22024","timeoutMs":800}`)
	hosts := c.byKind("host")
	if len(hosts) != 1 || len(hosts[0]["ports"].([]int)) != 2 {
		t.Fatalf("hosts = %v", c.all())
	}
}
