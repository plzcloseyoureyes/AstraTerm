package tools

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// listenLoopback starts a TCP listener on 127.0.0.1 that immediately writes a banner and returns its port.
func listenLoopback(t *testing.T, banner string) (int, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if banner != "" {
				_, _ = c.Write([]byte(banner))
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() { _ = ln.Close() }
}

func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p
}

func TestPortscanLoopback(t *testing.T) {
	port, stop := listenLoopback(t, "HELLO-BANNER\x1b[2J\r\n")
	defer stop()
	cp := closedPort(t)
	body := fmt.Sprintf(`{"targets":"127.0.0.1,nonexistent-host.invalid","ports":"%d,%d","banner":true,"showClosed":true,"timeoutMs":800}`, port, cp)
	c := runJob(t, preparePortscan, body)

	var openRow, closedRow row
	for _, r := range c.byKind("port") {
		switch r["port"] {
		case port:
			openRow = r
		case cp:
			closedRow = r
		}
	}
	if openRow == nil || openRow["state"] != "open" {
		t.Fatalf("expected open port row, got %v", c.byKind("port"))
	}
	if b, _ := openRow["banner"].(string); b != "HELLO-BANNER.[2J" {
		t.Errorf("banner grab/sanitize: %q", openRow["banner"])
	}
	if closedRow == nil || closedRow["state"] != "closed" {
		t.Errorf("closed port: %v", closedRow)
	}
	if he := c.byKind("hosterror"); len(he) != 1 || he[0]["host"] != "nonexistent-host.invalid" {
		t.Errorf("unresolvable host should be reported once, got %v", he)
	}
	sum := c.byKind("summary")
	if len(sum) != 1 || sum[0]["open"] != int64(1) || sum[0]["unresolved"] != int64(1) {
		t.Errorf("summary = %v", sum)
	}
}

func TestPortscanUDPClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not answer loopback probes with ICMP / RST like other systems")
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close() // nothing listens: the stack answers ICMP port unreachable
	c := runJob(t, preparePortscan, fmt.Sprintf(`{"targets":"127.0.0.1","ports":"%d","udp":true,"showClosed":true,"timeoutMs":500}`, port))
	ports := c.byKind("port")
	if len(ports) != 1 || ports[0]["state"] != "closed" || ports[0]["proto"] != "udp" {
		t.Fatalf("udp closed port = %v", ports)
	}
}

func TestPortscanCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	// 192.0.2.0/24 (TEST-NET-1) is never routed: probes hang until their timeout, so cancellation must cut them.
	_, err := tryJob(ctx, preparePortscan, testCall(`{"targets":"192.0.2.0/28","ports":"1-1000","timeoutMs":5000,"concurrency":64}`))
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("cancel took %v (err %v)", time.Since(start), err)
	}
}

func TestTCPPingLoopback(t *testing.T) {
	port, stop := listenLoopback(t, "")
	defer stop()
	c := runJob(t, preparePing, fmt.Sprintf(`{"host":"127.0.0.1","port":%d,"count":3,"intervalMs":100,"timeoutMs":500,"mode":"tcp"}`, port))
	got := 0
	for _, r := range c.byKind("reply") {
		if _, ok := r["rttMs"]; ok {
			got++
		}
	}
	if got != 3 {
		t.Fatalf("expected 3 successful tcp replies, got %d (%v)", got, c.byKind("reply"))
	}
	sum := c.byKind("summary")
	if len(sum) != 1 || sum[0]["recv"] != 3 || sum[0]["loss"] != 0.0 {
		t.Fatalf("summary = %v", sum)
	}
	// Refused probes.
	c = runJob(t, preparePing, fmt.Sprintf(`{"host":"127.0.0.1","port":%d,"count":2,"intervalMs":100,"mode":"tcp"}`, closedPort(t)))
	if sum := c.byKind("summary"); len(sum) != 1 || sum[0]["recv"] != 0 || sum[0]["loss"] != 100.0 {
		t.Fatalf("refused summary = %v", sum)
	}
}

func TestGuardBlocksLoopbackTargets(t *testing.T) {
	port, stop := listenLoopback(t, "x")
	defer stop()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "secret") }))
	defer srv.Close()
	guarded := func(body string) *call {
		cl := testCall(body)
		cl.guard = &netGuard{}
		return cl
	}
	cases := []struct {
		name    string
		prepare prepareFunc
		body    string
	}{
		{"tcp ping", preparePing, fmt.Sprintf(`{"host":"127.0.0.1","port":%d,"mode":"tcp","count":1}`, port)},
		{"httpcheck", prepareHTTPCheck, `{"url":"` + srv.URL + `"}`},
		{"httpcheck localhost", prepareHTTPCheck, `{"url":"http://localhost:` + strconv.Itoa(port) + `/"}`},
		{"tlscert", prepareTLSCert, fmt.Sprintf(`{"host":"127.0.0.1","port":%d}`, port)},
		{"sshaudit", prepareSSHAudit, fmt.Sprintf(`{"host":"127.0.0.1","port":%d}`, port)},
		{"dns", prepareDNS, `{"name":"example.com","resolver":"127.0.0.1:` + strconv.Itoa(port) + `","tcp":true}`},
		{"whois", prepareWhois, `{"query":"example.com","server":"127.0.0.1:` + strconv.Itoa(port) + `"}`},
		{"snmp", prepareSNMP, `{"host":"127.0.0.1"}`},
		{"iperf3", prepareThroughput, fmt.Sprintf(`{"host":"127.0.0.1","port":%d,"durationSec":1}`, port)},
	}
	for _, tc := range cases {
		c, err := tryJob(context.Background(), tc.prepare, guarded(tc.body))
		if err == nil || !strings.Contains(err.Error(), "not allowed in server mode") {
			var rows []row
			if c != nil {
				rows = c.all()
			}
			t.Errorf("%s: guard did not block loopback: err=%v rows=%v", tc.name, err, rows)
		}
	}
}

func TestNetscanLoopback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not answer loopback probes with ICMP / RST like other systems")
	}
	port, stop := listenLoopback(t, "")
	defer stop()
	cp := closedPort(t)
	body := fmt.Sprintf(`{"targets":"127.0.0.1","ports":"%d,%d","timeoutMs":500,"resolveNames":false,"noIcmp":true}`, port, cp)
	c := runJob(t, prepareNetscan, body)
	hosts := c.byKind("host")
	if len(hosts) != 1 || hosts[0]["alive"] != true {
		t.Fatalf("expected an alive host, got %v", c.all())
	}
	if ports, _ := hosts[0]["ports"].([]int); len(ports) != 1 || ports[0] != port {
		t.Fatalf("open ports = %v", hosts[0]["ports"])
	}
	// A host with only closed (refusing) ports is still alive.
	c = runJob(t, prepareNetscan, fmt.Sprintf(`{"targets":"127.0.0.1","ports":"%d","resolveNames":false,"noIcmp":true}`, cp))
	if hosts := c.byKind("host"); len(hosts) != 1 || hosts[0]["alive"] != true {
		t.Fatalf("refusing host should be alive: %v", c.all())
	}
	if sum := c.byKind("summary"); len(sum) != 1 || sum[0]["alive"] != int64(1) {
		t.Fatalf("summary = %v", sum)
	}
}

func TestICMPSweepLoopback(t *testing.T) {
	if icmpMode(false) == "" {
		t.Skip("no ICMP socket available to this process")
	}
	alive := icmpSweep(context.Background(), []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.2")}, 500*time.Millisecond)
	if _, ok := alive["127.0.0.1"]; !ok {
		t.Fatalf("127.0.0.1 did not answer the sweep: %v", alive)
	}
}

// ---- TLS -----------------------------------------------------------------------------------------------------------

func testCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "astraterm-test"},
		DNSNames:     []string{"localhost", "astraterm-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// startServer accepts connections and runs handle for each one.
func startServer(t *testing.T, handle func(c net.Conn)) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				handle(c)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func serveTLS(c net.Conn, cert tls.Certificate, minV uint16) {
	tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: minV})
	if tc.Handshake() == nil {
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTLSCertLoopback(t *testing.T) {
	cert := testCert(t)
	port := startServer(t, func(c net.Conn) { serveTLS(c, cert, tls.VersionTLS12) })
	c := runJob(t, prepareTLSCert, fmt.Sprintf(`{"host":"127.0.0.1","port":%d,"serverName":"localhost","timeoutMs":3000}`, port))
	certs := c.byKind("cert")
	if len(certs) == 0 || certs[0]["commonName"] != "astraterm-test" || certs[0]["keyAlg"] != "ECDSA" || certs[0]["keyBits"] != 256 {
		t.Fatalf("certificate rows = %v", certs)
	}
	conn := c.byKind("connection")
	if len(conn) == 0 || conn[0]["verified"] != false || conn[0]["serverName"] != "localhost" {
		t.Errorf("self-signed cert should be unverified: %v", conn)
	}
	sum := c.byKind("summary")
	if len(sum) != 1 || sum[0]["hostnameMatch"] != true {
		t.Errorf("summary = %v", sum)
	}
	versions := map[string]bool{}
	for _, v := range c.byKind("version") {
		versions[v["version"].(string)] = v["supported"].(bool)
	}
	if !versions["TLS 1.3"] || !versions["TLS 1.2"] || versions["TLS 1.1"] || versions["TLS 1.0"] {
		t.Errorf("version support = %v", versions)
	}
}

func TestTLSCertStartTLS(t *testing.T) {
	cert := testCert(t)
	smtp := startServer(t, func(c net.Conn) {
		br := bufio.NewReader(c)
		fmt.Fprint(c, "220-mail.test ESMTP\r\n220 ready\r\n")
		if l, _ := br.ReadString('\n'); !strings.HasPrefix(l, "EHLO ") {
			return
		}
		fmt.Fprint(c, "250-mail.test\r\n250-PIPELINING\r\n250-SIZE 1000\r\n250-STARTTLS\r\n250 8BITMIME\r\n")
		if l, _ := br.ReadString('\n'); l != "STARTTLS\r\n" {
			return
		}
		fmt.Fprint(c, "220 2.0.0 go ahead\r\n")
		serveTLS(c, cert, tls.VersionTLS12)
	})
	ftp := startServer(t, func(c net.Conn) {
		br := bufio.NewReader(c)
		fmt.Fprint(c, "220-Welcome\r\n220-to the test\r\n220 FTP ready\r\n")
		if l, _ := br.ReadString('\n'); l != "AUTH TLS\r\n" {
			return
		}
		fmt.Fprint(c, "234 AUTH TLS ok\r\n")
		serveTLS(c, cert, tls.VersionTLS12)
	})
	imap := startServer(t, func(c net.Conn) {
		br := bufio.NewReader(c)
		fmt.Fprint(c, "* OK IMAP4rev1 ready\r\n")
		if l, _ := br.ReadString('\n'); l != "a1 STARTTLS\r\n" {
			return
		}
		fmt.Fprint(c, "* CAPABILITY IMAP4rev1\r\na1 OK Begin TLS\r\n")
		serveTLS(c, cert, tls.VersionTLS12)
	})
	pg := startServer(t, func(c net.Conn) {
		var b [8]byte
		if _, err := io.ReadFull(c, b[:]); err != nil || binary.BigEndian.Uint32(b[4:]) != 80877103 {
			return
		}
		_, _ = c.Write([]byte{'S'})
		serveTLS(c, cert, tls.VersionTLS12)
	})
	ldap := startServer(t, func(c net.Conn) {
		b := make([]byte, 64)
		n, _ := c.Read(b)
		if n < 5 || !strings.Contains(string(b[:n]), "1.3.6.1.4.1.1466.20037") {
			return
		}
		// LDAPMessage{id 1, ExtendedResponse{resultCode success(0), matchedDN "", diagnosticMessage ""}}
		_, _ = c.Write([]byte{0x30, 0x0c, 0x02, 0x01, 0x01, 0x78, 0x07, 0x0a, 0x01, 0x00, 0x04, 0x00, 0x04, 0x00})
		serveTLS(c, cert, tls.VersionTLS12)
	})
	noStartTLS := startServer(t, func(c net.Conn) {
		br := bufio.NewReader(c)
		fmt.Fprint(c, "220 ready\r\n")
		_, _ = br.ReadString('\n')
		fmt.Fprint(c, "250-mail.test\r\n250 8BITMIME\r\n")
	})
	for name, tc := range map[string]struct {
		port  int
		proto string
	}{"smtp": {smtp, "smtp"}, "ftp": {ftp, "ftp"}, "imap": {imap, "imap"}, "postgres": {pg, "postgres"}, "ldap": {ldap, "ldap"}} {
		c, err := tryJob(context.Background(), prepareTLSCert, testCall(fmt.Sprintf(`{"host":"127.0.0.1","port":%d,"startTls":%q,"serverName":"localhost","probeVersions":false}`, tc.port, tc.proto)))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if certs := c.byKind("cert"); len(certs) != 1 || certs[0]["commonName"] != "astraterm-test" {
			t.Errorf("%s: certs = %v", name, certs)
		}
	}
	_, err := tryJob(context.Background(), prepareTLSCert, testCall(fmt.Sprintf(`{"host":"127.0.0.1","port":%d,"startTls":"smtp","probeVersions":false}`, noStartTLS)))
	if err == nil || !strings.Contains(err.Error(), "does not offer STARTTLS") {
		t.Errorf("missing STARTTLS should be reported, got %v", err)
	}
}

// ---- HTTP ----------------------------------------------------------------------------------------------------------

func TestHTTPCheckLoopback(t *testing.T) {
	var gotHost, gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/final", http.StatusFound) })
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotAuth = r.Host, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Test", "1")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := runJob(t, prepareHTTPCheck, `{"url":"`+srv.URL+`/start","followRedirects":true,"headers":{"Host":"virtual.test","Authorization":"Bearer abc"}}`)
	res := c.byKind("result")
	if len(res) != 1 {
		t.Fatalf("results = %v", c.all())
	}
	r := res[0]
	if r["status"] != 200 || r["redirects"] != 1 || r["bodyPreview"] != `{"status":"ok"}` || r["contentType"] != "application/json" {
		t.Fatalf("result = %v", r)
	}
	timing := r["timing"].(row)
	// On loopback the first byte can arrive within the clock's resolution (Windows): only the total must be positive.
	if timing["totalMs"].(float64) <= 0 || timing["ttfbMs"].(float64) < 0 {
		t.Errorf("timing = %v", timing)
	}
	if gotHost != "virtual.test" || gotAuth != "Bearer abc" {
		t.Errorf("host header %q, auth %q", gotHost, gotAuth)
	}
	if red := c.byKind("redirect"); len(red) != 1 || !strings.HasSuffix(red[0]["to"].(string), "/final") {
		t.Errorf("redirect rows = %v", red)
	}
	// No redirect following: the 302 itself is the result.
	c = runJob(t, prepareHTTPCheck, `{"url":"`+srv.URL+`/start"}`)
	if res := c.byKind("result"); len(res) != 1 || res[0]["status"] != 302 {
		t.Fatalf("no-follow result = %v", res)
	}
	// httping mode.
	c = runJob(t, prepareHTTPCheck, `{"url":"`+srv.URL+`/final","count":3,"intervalMs":100}`)
	if res, sum := c.byKind("result"), c.byKind("summary"); len(res) != 3 || len(sum) != 1 || sum[0]["ok"] != 3 {
		t.Fatalf("httping results = %v summary = %v", res, sum)
	}
}

// ---- DNS / whois / WoL / SSH audit ---------------------------------------------------------------------------------

func TestDNSLocalServer(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := dns.NewServeMux()
	mux.HandleFunc("example.test.", func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Authoritative = true
		rr, _ := dns.NewRR("example.test. 300 IN A 192.0.2.10")
		m.Answer = append(m.Answer, rr)
		mx, _ := dns.NewRR("example.test. 300 IN NS ns1.example.test.")
		m.Ns = append(m.Ns, mx)
		_ = w.WriteMsg(m)
	})
	srv := &dns.Server{PacketConn: pc, Handler: mux}
	go func() { _ = srv.ActivateAndServe() }()
	defer srv.Shutdown()
	c := runJob(t, prepareDNS, `{"name":"example.test","type":"A","resolver":"`+pc.LocalAddr().String()+`"}`)
	recs := c.byKind("record")
	if len(recs) != 2 || recs[0]["data"] != "192.0.2.10" || recs[0]["section"] != "answer" || recs[1]["section"] != "authority" {
		t.Fatalf("records = %v", recs)
	}
	sum := c.byKind("summary")
	if len(sum) != 1 || sum[0]["rcode"] != "NOERROR" || sum[0]["authoritative"] != true {
		t.Fatalf("summary = %v", sum)
	}
}

func TestWhoisLocalServer(t *testing.T) {
	port := startServer(t, func(c net.Conn) {
		l, _ := bufio.NewReader(c).ReadString('\n')
		if strings.TrimSpace(l) == "big.test" {
			chunk := strings.Repeat("x", 64*1024)
			for range 40 { // 2.5 MiB: must be cut at 1 MiB
				if _, err := io.WriteString(c, chunk); err != nil {
					return
				}
			}
			return
		}
		fmt.Fprintf(c, "Domain Name: %s\r\nRegistrar: Test\x1b[31m Registrar\r\n", strings.TrimSpace(l))
	})
	c := runJob(t, prepareWhois, fmt.Sprintf(`{"query":"example.test","server":"127.0.0.1:%d"}`, port))
	text := c.byKind("text")
	if len(text) != 1 || !strings.Contains(text[0]["text"].(string), "Domain Name: example.test\nRegistrar: Test[31m Registrar") {
		t.Fatalf("whois text = %v", c.all())
	}
	c = runJob(t, prepareWhois, fmt.Sprintf(`{"query":"big.test","server":"127.0.0.1:%d"}`, port))
	if text := c.byKind("text"); len(text) != 1 || len(text[0]["text"].(string)) > maxWhoisBytes+256 { // + the query-time footer
		t.Fatalf("oversized whois reply not capped: %d bytes", len(text[0]["text"].(string)))
	}
}

func TestWOLLoopback(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	port := pc.LocalAddr().(*net.UDPAddr).Port
	got := make(chan int, 4)
	go func() {
		buf := make([]byte, 256)
		for {
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			got <- n
		}
	}()
	c := runJob(t, prepareWOL, fmt.Sprintf(`{"mac":"01:02:03:04:05:06","broadcast":"127.0.0.1","port":%d,"count":2,"secureOn":"aa:bb:cc:dd:ee:ff"}`, port))
	if sum := c.byKind("summary"); len(sum) != 1 || sum[0]["packets"] != 2 || sum[0]["secureOn"] != true {
		t.Fatalf("wol summary = %v", c.all())
	}
	for range 2 {
		select {
		case n := <-got:
			if n != 108 {
				t.Errorf("magic packet size = %d want 108", n)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("magic packet not received")
		}
	}
}

func TestSSHAuditBannerLimit(t *testing.T) {
	port := startServer(t, func(c net.Conn) {
		_, _ = c.Write([]byte(strings.Repeat("A", 8192)))
		time.Sleep(200 * time.Millisecond)
	})
	_, err := tryJob(context.Background(), prepareSSHAudit, testCall(fmt.Sprintf(`{"host":"127.0.0.1","port":%d,"timeoutMs":2000}`, port)))
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("an endless banner line must be rejected, got %v", err)
	}
}

func TestBannerHTTPFallback(t *testing.T) {
	port := startServer(t, func(c net.Conn) {
		// A silent service that answers HTTP.
		br := bufio.NewReader(c)
		if l, _ := br.ReadString('\n'); strings.HasPrefix(l, "HEAD / HTTP/1.0") {
			fmt.Fprint(c, "HTTP/1.0 200 OK\r\nServer: quiet\r\n\r\n")
		}
	})
	c := runJob(t, preparePortscan, fmt.Sprintf(`{"targets":"127.0.0.1","ports":"%d","banner":true,"timeoutMs":800}`, port))
	ports := c.byKind("port")
	if len(ports) != 1 || ports[0]["banner"] != "HTTP/1.0 200 OK" {
		t.Fatalf("silent HTTP service banner = %v", ports)
	}
}
