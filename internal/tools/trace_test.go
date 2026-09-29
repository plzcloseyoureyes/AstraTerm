package tools

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseTraceLine(t *testing.T) {
	cases := []struct {
		line    string
		ttl     int
		from    any
		times   int
		timeout bool
		annot   string
	}{
		{" 3  10.0.0.1  1.234 ms  2.0 ms  1.5 ms", 3, "10.0.0.1", 3, false, ""},
		{" 1  172.17.0.1  0.006 ms  0.004 ms  0.004 ms", 1, "172.17.0.1", 3, false, ""},                // busybox
		{" 5  * * *", 5, nil, 3, true, ""},                                                             // silent hop
		{" 7  10.1.1.1  3.2 ms 10.1.1.2  3.5 ms *", 7, []string{"10.1.1.1", "10.1.1.2"}, 3, false, ""}, // ECMP
		{" 2  router.lan (192.168.1.1)  1.1 ms  1.0 ms  0.9 ms", 2, "192.168.1.1", 3, false, ""},
		{" 9  10.9.9.9  12.0 ms !H  11.0 ms !H  * ", 9, "10.9.9.9", 3, false, "!H"},
		{" 4  2001:db8::1  8.1 ms  8.0 ms  7.9 ms", 4, "2001:db8::1", 3, false, ""},
		{" 1:  192.168.1.1                                           0.612ms ", 1, "192.168.1.1", 1, false, ""}, // tracepath
		{" 2:  no reply", 2, nil, 0, true, ""},
		{" 3:  93.184.216.34                                        11.2ms reached", 3, "93.184.216.34", 1, false, ""},
		{"  1    <1 ms    <1 ms    <1 ms  192.168.1.1", 1, "192.168.1.1", 3, false, ""}, // tracert -d
		{"  2     *        *        *     Request timed out.", 2, nil, 3, true, ""},
		{"  3    12 ms    11 ms    12 ms  host.example [203.0.113.5]", 3, "203.0.113.5", 3, false, ""},
	}
	for _, tc := range cases {
		r := parseTraceLine(tc.line)
		if r == nil {
			t.Errorf("%q: not parsed", tc.line)
			continue
		}
		if r["ttl"] != tc.ttl || fmt.Sprint(r["from"]) != fmt.Sprint(tc.from) && !(tc.from == nil && r["from"] == nil) {
			t.Errorf("%q: ttl/from = %v/%v", tc.line, r["ttl"], r["from"])
		}
		if times, _ := r["timesMs"].([]any); len(times) != tc.times {
			t.Errorf("%q: times = %v", tc.line, r["timesMs"])
		}
		if r["timeout"] != tc.timeout {
			t.Errorf("%q: timeout = %v", tc.line, r["timeout"])
		}
		if a, _ := r["annotation"].(string); a != tc.annot {
			t.Errorf("%q: annotation = %q", tc.line, a)
		}
	}
	for _, l := range []string{
		"traceroute to example.com (93.184.216.34), 30 hops max, 60 byte packets",
		" 1?: [LOCALHOST]                      pmtu 1500",
		"     Resume: pmtu 1500 hops 3 back 3 ",
		"Tracing route to example.com [93.184.216.34]",
		"over a maximum of 30 hops:",
		"Trace complete.",
		"",
	} {
		if r := parseTraceLine(l); r != nil {
			t.Errorf("%q should not be a hop: %v", l, r)
		}
	}
	if r := parseTraceLine("  1    <1 ms    <1 ms    <1 ms  192.168.1.1"); r["timesMs"].([]any)[0] != 1.0 {
		t.Errorf("<1 ms parsed as %v", r["timesMs"])
	}
}

func TestTraceLineStreamMergesTracepath(t *testing.T) {
	c := &collector{}
	s := c.sink()
	st := &traceLineStream{ctx: context.Background(), out: s}
	for _, l := range []string{
		" 1?: [LOCALHOST]                      pmtu 1500",
		" 1:  172.17.0.1                                            0.100ms ",
		" 1:  172.17.0.1                                            0.090ms ",
		" 2:  no reply",
		" 3:  1.1.1.1                                               5.1ms reached",
		"     Resume: pmtu 1500 hops 3 back 3 ",
	} {
		st.line(l)
	}
	st.finishHop()
	s.close()
	hops := c.byKind("hop")
	if len(hops) != 3 || len(hops[0]["timesMs"].([]any)) != 2 || hops[1]["timeout"] != true || hops[2]["from"] != "1.1.1.1" {
		t.Fatalf("hops = %v", hops)
	}
	if !st.reached() || st.hops != 3 {
		t.Fatalf("reached = %v hops = %d", st.reached(), st.hops)
	}
	// traceroute header gives the destination for the reached check.
	st2 := &traceLineStream{ctx: context.Background(), out: s, onHop: func(row) {}}
	st2.line("traceroute to one.one.one.one (1.1.1.1), 30 hops max, 46 byte packets")
	st2.line(" 1  1.1.1.1  3.0 ms")
	st2.finishHop()
	if st2.dest != "1.1.1.1" || !st2.reached() {
		t.Fatalf("dest = %q reached = %v", st2.dest, st2.reached())
	}
}

func TestRemotePingParsing(t *testing.T) {
	cases := map[string][]string{
		"64 bytes from 127.0.0.1: seq=0 ttl=64 time=1.085 ms":                     {"127.0.0.1", "", "0", "64", "1.085"},
		"64 bytes from 1.1.1.1: icmp_seq=1 ttl=57 time=12.3 ms":                   {"1.1.1.1", "", "1", "57", "12.3"},
		"64 bytes from one.one.one.one (1.1.1.1): icmp_seq=2 ttl=57 time=11.9 ms": {"one.one.one.one", "1.1.1.1", "2", "57", "11.9"},
		"64 bytes from 2606:4700:4700::1111: icmp_seq=3 ttl=58 time=10.0 ms":      {"2606:4700:4700::1111", "", "3", "58", "10.0"},
		"64 bytes from 10.0.0.1: icmp_seq=4 ttl=64 time<1 ms":                     {"10.0.0.1", "", "4", "64", "1"},
	}
	for line, want := range cases {
		m := rePingReply.FindStringSubmatch(line)
		if m == nil || m[1] != want[0] || m[2] != want[1] || m[3] != want[2] || m[4] != want[3] || m[5] != want[4] {
			t.Errorf("%q → %q", line, m)
		}
	}
	for _, l := range []string{
		"4 packets transmitted, 4 received, 0% packet loss, time 3004ms",
		"1 packets transmitted, 1 packets received, 0% packet loss",
		"4 packets transmitted, 0 received, +4 errors, 100% packet loss, time 3050ms",
		"5 packets transmitted, 5 packets received, 0.0% packet loss",
	} {
		if rePingLoss.FindStringSubmatch(l) == nil {
			t.Errorf("loss line not parsed: %q", l)
		}
	}
	for _, l := range []string{
		"rtt min/avg/max/mdev = 11.123/12.456/13.789/0.912 ms",
		"round-trip min/avg/max = 1.085/1.085/1.085 ms",
		"round-trip min/avg/max/stddev = 10.1/11.2/12.3/0.9 ms",
	} {
		if rePingRTTStat.FindStringSubmatch(l) == nil {
			t.Errorf("rtt line not parsed: %q", l)
		}
	}
	if m := rePingTimeout.FindStringSubmatch("Request timeout for icmp_seq 7"); m == nil || m[1] != "7" {
		t.Errorf("macOS timeout line: %v", m)
	}
}

func TestRemoteScriptsQuoteHost(t *testing.T) {
	p := &pingRequest{Host: "example.com", Count: 4, IntervalMs: 500, TimeoutMs: 2000}
	s := remotePingScript(p)
	if !strings.Contains(s, "-c 4 -i 0.5") || !strings.HasSuffix(s, "'example.com'") {
		t.Errorf("ping script = %s", s)
	}
	tr := &tracerouteRequest{Host: "example.com", Protocol: "icmp", MaxHops: 20}
	s = remoteTraceScript(tr, 1, 2)
	if !strings.Contains(s, "-n -q 1 -m 20 -w 2 'example.com'") || !strings.Contains(s, "tracepath") {
		t.Errorf("trace script = %s", s)
	}
	if got := shScript("echo 'hi'"); got != `sh -c 'echo '\''hi'\'''` {
		t.Errorf("shScript = %s", got)
	}
}

// fakeTracer simulates a path of routers: hop i answers from 10.0.0.i after rtt, the destination sits at dest; lossy
// hops drop every other probe; silent hops never answer.
type fakeTracer struct {
	mu      sync.Mutex
	dest    int
	lossy   map[int]bool
	silent  map[int]bool
	next    int
	sentTTL map[int]int
	pending []traceAnswer
	count   map[int]int
}

func newFakeTracer(dest int) *fakeTracer {
	return &fakeTracer{dest: dest, lossy: map[int]bool{}, silent: map[int]bool{}, sentTTL: map[int]int{}, count: map[int]int{}}
}

func (f *fakeTracer) engine() string { return "fake" }
func (f *fakeTracer) close() error   { return nil }

func (f *fakeTracer) send(ttl int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	k := f.next
	f.count[ttl]++
	switch {
	case f.silent[ttl] && ttl < f.dest:
	case f.lossy[ttl] && f.count[ttl]%2 == 0:
	case ttl >= f.dest:
		f.pending = append(f.pending, traceAnswer{key: k, from: net.ParseIP("192.0.2.1"), at: time.Now().Add(time.Millisecond), reached: true})
	default:
		f.pending = append(f.pending, traceAnswer{key: k, from: net.ParseIP(fmt.Sprintf("10.0.0.%d", ttl)), at: time.Now().Add(time.Duration(ttl) * time.Millisecond)})
	}
	return k, nil
}

func (f *fakeTracer) recv(ctx context.Context, deadline time.Time) (traceAnswer, error) {
	for {
		f.mu.Lock()
		if len(f.pending) > 0 {
			a := f.pending[0]
			f.pending = f.pending[1:]
			f.mu.Unlock()
			return a, nil
		}
		f.mu.Unlock()
		if time.Now().After(deadline) || ctx.Err() != nil {
			return traceAnswer{}, errNoAnswer
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTraceClassicFake(t *testing.T) {
	ft := newFakeTracer(4)
	ft.silent[2] = true
	no := false
	req := &tracerouteRequest{Host: "dest.test", MaxHops: 10, Probes: 3, TimeoutMs: 200, ResolveNames: &no}
	c := &collector{}
	s := c.sink()
	rs := startReader(context.Background(), ft)
	defer rs.stop()
	if err := traceClassic(context.Background(), ft, rs, req, net.ParseIP("192.0.2.1"), newRDNSCache(), s); err != nil {
		t.Fatal(err)
	}
	s.close()
	hops := c.byKind("hop")
	if len(hops) != 4 {
		t.Fatalf("hops = %v", hops)
	}
	if hops[0]["from"] != "10.0.0.1" || hops[1]["timeout"] != true || hops[3]["from"] != "192.0.2.1" {
		t.Fatalf("hops = %v", hops)
	}
	for _, v := range hops[0]["timesMs"].([]any) {
		if v == nil {
			t.Fatalf("hop 1 lost a probe: %v", hops[0])
		}
	}
	sum := c.byKind("summary")
	if len(sum) != 1 || sum[0]["reached"] != true || sum[0]["hops"] != 4 {
		t.Fatalf("summary = %v", sum)
	}
}

func TestTraceMTRFake(t *testing.T) {
	ft := newFakeTracer(3)
	ft.lossy[2] = true
	no := false
	req := &tracerouteRequest{Host: "dest.test", Mode: "mtr", MaxHops: 8, TimeoutMs: 100, IntervalMs: 200, Rounds: 4, ResolveNames: &no}
	c := &collector{}
	s := c.sink()
	rs := startReader(context.Background(), ft)
	defer rs.stop()
	if err := traceMTR(context.Background(), ft, rs, req, net.ParseIP("192.0.2.1"), newRDNSCache(), s); err != nil {
		t.Fatal(err)
	}
	s.close()
	rounds := c.byKind("round")
	if len(rounds) != 4 || rounds[3]["lastTtl"] != 3 || rounds[3]["reached"] != true {
		t.Fatalf("rounds = %v", rounds)
	}
	latest := map[int]row{}
	for _, r := range c.byKind("mtr") {
		latest[r["ttl"].(int)] = r
	}
	if len(latest) != 3 {
		t.Fatalf("mtr rows for %d hops (the rows beyond the destination must be dropped): %v", len(latest), latest)
	}
	if latest[1]["sent"] != 4 || latest[1]["recv"] != 4 || latest[1]["loss"] != 0.0 {
		t.Errorf("hop 1 = %v", latest[1])
	}
	if latest[2]["sent"] != 4 || latest[2]["recv"] != 2 || latest[2]["loss"] != 50.0 {
		t.Errorf("lossy hop 2 = %v", latest[2])
	}
	// After the first round the destination is known, so later rounds probe only up to it.
	if ft.count[5] != 1 {
		t.Errorf("hop 5 probed %d times; rounds after discovery must stop at the destination", ft.count[5])
	}
	if sum := c.byKind("summary"); len(sum) != 1 || sum[0]["rounds"] != 4 || sum[0]["hops"] != 3 {
		t.Errorf("summary = %v", sum)
	}
}

func TestTraceMTRStopKeepsSummary(t *testing.T) {
	ft := newFakeTracer(2)
	no := false
	req := &tracerouteRequest{Host: "dest.test", Mode: "mtr", MaxHops: 5, TimeoutMs: 50, IntervalMs: 50, Rounds: maxMTRRounds, ResolveNames: &no}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	c := &collector{}
	s := c.sink()
	rs := startReader(ctx, ft)
	defer rs.stop()
	err := traceMTR(ctx, ft, rs, req, net.ParseIP("192.0.2.1"), newRDNSCache(), s)
	s.close()
	if err == nil {
		t.Fatal("expected the context error after stop")
	}
	if sum := c.byKind("summary"); len(sum) != 1 || sum[0]["rounds"].(int) < 2 {
		t.Fatalf("summary after stop = %v", sum)
	}
}

// TestLocalTracerLoopback exercises the platform probing engine against 127.0.0.1 (one hop: the destination).
func TestLocalTracerLoopback(t *testing.T) {
	for _, udp := range []bool{false, true} {
		tr, err := openTracer(net.ParseIP("127.0.0.1"), udp)
		if err != nil {
			t.Logf("udp=%v: no probing engine here (%v)", udp, err)
			continue
		}
		no := false
		req := &tracerouteRequest{Host: "127.0.0.1", MaxHops: 3, Probes: 2, TimeoutMs: 1000, ResolveNames: &no}
		c := &collector{}
		s := c.sink()
		rs := startReader(context.Background(), tr)
		err = traceClassic(context.Background(), tr, rs, req, net.ParseIP("127.0.0.1"), newRDNSCache(), s)
		s.close()
		rs.stop()
		tr.close()
		if err != nil {
			t.Fatalf("udp=%v (%s): %v", udp, tr.engine(), err)
		}
		sum := c.byKind("summary")
		hops := c.byKind("hop")
		if len(sum) != 1 || sum[0]["reached"] != true || len(hops) != 1 || hops[0]["from"] != "127.0.0.1" {
			t.Fatalf("udp=%v (%s): hops=%v summary=%v", udp, tr.engine(), hops, sum)
		}
	}
}
