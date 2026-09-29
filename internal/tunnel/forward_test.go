package tunnel

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	xproxy "golang.org/x/net/proxy"

	"github.com/nexterm/nexterm/internal/model"
)

func TestLocalForwardEndToEnd(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	_, webPort := webServer(t)

	v := h.createTunnel(alice, Input{Name: "web", Type: "local", ConnectionID: conn.ID, DestHost: "127.0.0.1", DestPort: webPort})
	if v.BindHost != "127.0.0.1" || v.Status.State != model.TunnelStopped || v.Connection == nil || v.Connection.Host != srv.Host {
		t.Fatalf("unexpected created tunnel: %+v", v)
	}
	st := h.startTunnel(alice, v.ID)
	if st.LocalAddr == "" || strings.HasSuffix(st.LocalAddr, ":auto") {
		t.Fatalf("bound address not reported: %+v", st)
	}
	if got := httpGet(t, st.LocalAddr, "/one"); got != "hello /one" {
		t.Fatalf("GET through tunnel = %q", got)
	}
	if got := httpGet(t, st.LocalAddr, "/two"); got != "hello /two" {
		t.Fatalf("GET through tunnel = %q", got)
	}
	st = h.waitStatus(v.ID, func(s Status) bool { return s.TotalConns >= 2 && s.ActiveConns == 0 && s.BytesIn > 0 && s.BytesOut > 0 })

	// The event-carried status and the REST view agree.
	var got View
	h.must(alice, "GET", "/api/tunnels/"+v.ID, nil, &got, http.StatusOK)
	if got.Status.State != model.TunnelRunning || got.Status.TotalConns < 2 {
		t.Fatalf("REST status = %+v", got.Status)
	}

	// Stopping closes the listener.
	h.must(alice, "POST", "/api/tunnels/"+v.ID+"/stop", nil, &got, http.StatusOK)
	if got.Status.State != model.TunnelStopped {
		t.Fatalf("after stop: %+v", got.Status)
	}
	if c, err := net.DialTimeout("tcp", st.LocalAddr, time.Second); err == nil {
		c.Close()
		t.Fatalf("listener %s still open after stop", st.LocalAddr)
	}
}

func TestLocalForwardDestinationFailureIsCounted(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	// A closed port: the SSH server cannot connect.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	deadPort := l.Addr().(*net.TCPAddr).Port
	l.Close()
	v := h.createTunnel(alice, Input{Name: "dead", Type: "local", ConnectionID: conn.ID, DestHost: "127.0.0.1", DestPort: deadPort})
	st := h.startTunnel(alice, v.ID)
	c, err := net.Dial("tcp", st.LocalAddr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected the connection to be closed")
	}
	c.Close()
	st = h.waitStatus(v.ID, func(s Status) bool { return s.FailedConns == 1 && s.LastError != "" })
	if !strings.Contains(st.LastError, "could not connect") || st.State != model.TunnelRunning {
		t.Fatalf("status = %+v", st)
	}
}

func TestPortInUseIsReportedSynchronously(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port
	v := h.createTunnel(alice, Input{Name: "busy", Type: "local", ConnectionID: conn.ID, BindPort: port, DestPort: 80})
	code, body := h.call(alice, "POST", "/api/tunnels/"+v.ID+"/start", nil, nil)
	if code != http.StatusConflict || !strings.Contains(string(body), "already in use") {
		t.Fatalf("start on a busy port: %d %s", code, body)
	}
	if st := h.m.status(v.ID); st.State != model.TunnelError || !strings.Contains(st.Error, "already in use") {
		t.Fatalf("status after failed start: %+v", st)
	}

	// The pre-flight check reports it too, with a free alternative.
	var res struct {
		Available  bool   `json:"available"`
		Error      string `json:"error"`
		Suggestion int    `json:"suggestion"`
	}
	h.must(alice, "POST", "/api/tunnels/check-bind", map[string]any{"bindHost": "127.0.0.1", "bindPort": port}, &res, http.StatusOK)
	if res.Available || res.Suggestion <= port || !strings.Contains(res.Error, "in use") {
		t.Fatalf("check-bind = %+v", res)
	}
}

func TestDynamicSocksAndHTTPProxy(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	_, webPort := webServer(t)
	v := h.createTunnel(alice, Input{Name: "socks", Type: "dynamic", ConnectionID: conn.ID})
	st := h.startTunnel(alice, v.ID)

	// SOCKS5 with a host name: resolved on the SSH server side (the test server dials "localhost:port").
	d, err := xproxy.SOCKS5("tcp", st.LocalAddr, nil, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Dial: d.Dial, DisableKeepAlives: true}}
	resp, err := c.Get("http://localhost:" + itoa(webPort) + "/socks5")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hello /socks5" {
		t.Fatalf("SOCKS5 GET = %q", b)
	}
	found := false
	for _, dial := range srv.Dials() {
		if dial == "localhost:"+itoa(webPort) {
			found = true
		}
	}
	if !found {
		t.Fatalf("host name was resolved locally; server dials: %v", srv.Dials())
	}

	// SOCKS4a (hand-made request).
	raw, err := net.Dial("tcp", st.LocalAddr)
	if err != nil {
		t.Fatal(err)
	}
	req := []byte{4, 1, 0, 0, 0, 0, 0, 1}
	binary.BigEndian.PutUint16(req[2:], uint16(webPort))
	req = append(req, 'u', 0)
	req = append(req, []byte("localhost")...)
	req = append(req, 0)
	raw.Write(req)
	rep := make([]byte, 8)
	if _, err := io.ReadFull(raw, rep); err != nil || rep[1] != 0x5A {
		t.Fatalf("SOCKS4a reply %v %v", rep, err)
	}
	io.WriteString(raw, "GET /socks4 HTTP/1.0\r\nHost: localhost\r\n\r\n")
	all, _ := io.ReadAll(raw)
	raw.Close()
	if !strings.HasSuffix(string(all), "hello /socks4") {
		t.Fatalf("SOCKS4a response %q", all)
	}

	// HTTP proxy: absolute-URI request.
	pu, _ := url.Parse("http://" + st.LocalAddr)
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(pu), DisableKeepAlives: true}}
	resp, err = hc.Get("http://127.0.0.1:" + itoa(webPort) + "/http")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hello /http" {
		t.Fatalf("HTTP proxy GET = %q", b)
	}

	// HTTP CONNECT.
	raw, err = net.Dial("tcp", st.LocalAddr)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(raw, "CONNECT 127.0.0.1:"+itoa(webPort)+" HTTP/1.1\r\nHost: 127.0.0.1:"+itoa(webPort)+"\r\n\r\n")
	br := bufio.NewReader(raw)
	line, _ := br.ReadString('\n')
	if !strings.Contains(line, "200") {
		t.Fatalf("CONNECT reply %q", line)
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil || l == "\r\n" {
			break
		}
	}
	io.WriteString(raw, "GET /connect HTTP/1.0\r\nHost: x\r\n\r\n")
	all, _ = io.ReadAll(br)
	raw.Close()
	if !strings.HasSuffix(string(all), "hello /connect") {
		t.Fatalf("CONNECT tunnel response %q", all)
	}

	// PAC file.
	body := httpGet(t, st.LocalAddr, "/proxy.pac")
	if !strings.Contains(body, "FindProxyForURL") || !strings.Contains(body, "SOCKS5 "+st.LocalAddr) {
		t.Fatalf("PAC = %q", body)
	}
	h.waitStatus(v.ID, func(s Status) bool { return s.TotalConns >= 5 && s.ActiveConns == 0 })
}

func TestDynamicSocksAuthentication(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	_, webPort := webServer(t)

	// A username without password is rejected.
	code, _ := h.call(alice, "POST", "/api/tunnels", Input{Name: "s", Type: "dynamic", ConnectionID: conn.ID,
		Options: Options{SocksUsername: "bob"}}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("username without password: %d", code)
	}
	// A proxy on all interfaces needs authentication or an allow list.
	code, _ = h.call(alice, "POST", "/api/tunnels", Input{Name: "s", Type: "dynamic", ConnectionID: conn.ID, BindHost: "*"}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("open proxy on all interfaces: %d", code)
	}

	v := h.createTunnel(alice, Input{Name: "s", Type: "dynamic", ConnectionID: conn.ID, Options: Options{SocksUsername: "bob"},
		Secrets: map[string]string{"socksPassword": "pw"}})
	if len(v.SecretKeys) != 1 || v.SecretKeys[0] != "socksPassword" {
		t.Fatalf("secretKeys = %v", v.SecretKeys)
	}
	st := h.startTunnel(alice, v.ID)
	target := "127.0.0.1:" + itoa(webPort)

	bad, _ := xproxy.SOCKS5("tcp", st.LocalAddr, &xproxy.Auth{User: "bob", Password: "nope"}, xproxy.Direct)
	if c, err := bad.Dial("tcp", target); err == nil {
		c.Close()
		t.Fatal("wrong SOCKS password accepted")
	}
	none, _ := xproxy.SOCKS5("tcp", st.LocalAddr, nil, xproxy.Direct)
	if c, err := none.Dial("tcp", target); err == nil {
		c.Close()
		t.Fatal("unauthenticated SOCKS accepted")
	}
	good, _ := xproxy.SOCKS5("tcp", st.LocalAddr, &xproxy.Auth{User: "bob", Password: "pw"}, xproxy.Direct)
	c, err := good.Dial("tcp", target)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()

	// HTTP proxy requires Proxy-Authorization as well.
	pu, _ := url.Parse("http://" + st.LocalAddr)
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(pu), DisableKeepAlives: true}}
	resp, err := hc.Get("http://" + target + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("HTTP proxy without credentials: %d", resp.StatusCode)
	}
	pu.User = url.UserPassword("bob", "pw")
	hc = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(pu), DisableKeepAlives: true}}
	resp, err = hc.Get("http://" + target + "/auth")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hello /auth" {
		t.Fatalf("authenticated HTTP proxy GET = %d %q", resp.StatusCode, b)
	}

	// Clearing the username keeps working; the secret can be removed.
	var upd View
	h.must(alice, "PATCH", "/api/tunnels/"+v.ID, map[string]any{"options": map[string]any{}, "secrets": map[string]string{"socksPassword": ""}}, &upd, http.StatusOK)
	if len(upd.SecretKeys) != 0 || upd.Options.SocksUsername != "" {
		t.Fatalf("after clearing auth: %+v", upd)
	}
}

func TestRemoteForwardEndToEnd(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	_, webPort := webServer(t)
	v := h.createTunnel(alice, Input{Name: "rev", Type: "remote", ConnectionID: conn.ID, BindHost: "127.0.0.1", BindPort: 0,
		DestHost: "127.0.0.1", DestPort: webPort})
	st := h.startTunnel(alice, v.ID)
	if st.RemoteAddr == "" || strings.HasSuffix(st.RemoteAddr, ":auto") || strings.HasSuffix(st.RemoteAddr, ":0") {
		t.Fatalf("remote address not reported: %+v", st)
	}
	// Connecting to the "remote" listener (the test server listens locally) reaches the local web server.
	if got := httpGet(t, st.RemoteAddr, "/remote"); got != "hello /remote" {
		t.Fatalf("GET through remote forward = %q", got)
	}
	h.must(alice, "POST", "/api/tunnels/"+v.ID+"/stop", nil, nil, http.StatusOK)
	waitFor(t, "remote listener cancelled", func() bool { return !srv.hasListener(":" + strings.Split(st.RemoteAddr, ":")[1]) })

	// A refused remote listener is a configuration error on first start.
	srv.DenyForward.Store(true)
	h.must(alice, "POST", "/api/tunnels/"+v.ID+"/start", nil, nil, http.StatusOK)
	st = h.waitStatus(v.ID, func(s Status) bool { return s.State == model.TunnelError })
	if !strings.Contains(st.Error, "refused to listen") {
		t.Fatalf("refused remote forward: %+v", st)
	}
}

// ssListing renders the test server's remote TCP listeners as `ss -ltn` output (behind the probe marker). The test
// server always binds loopback, like sshd with GatewayPorts no; wildcard reports the listeners on 0.0.0.0 instead,
// like a server that honours the requested address.
func ssListing(srv *testSSHServer, wildcard bool) string {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	var b strings.Builder
	b.WriteString("@@ss\nState  Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n")
	for _, ln := range srv.lns {
		ta, ok := ln.Addr().(*net.TCPAddr)
		if !ok {
			continue
		}
		host := ta.IP.String()
		if wildcard {
			host = "0.0.0.0"
		}
		fmt.Fprintf(&b, "LISTEN 0      128    %s:%d      0.0.0.0:*\n", host, ta.Port)
	}
	return b.String()
}

func TestRemoteForwardGatewayPortsProbe(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	var wildcard atomic.Bool
	var probes atomic.Int32
	srv.Exec = func(cmd string) (string, int) {
		if !strings.Contains(cmd, "ss -ltn") {
			return "", 0
		}
		probes.Add(1)
		return ssListing(srv, wildcard.Load()), 0
	}
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	_, webPort := webServer(t)
	runnerOf := func(id string) *runner {
		h.m.mu.Lock()
		defer h.m.mu.Unlock()
		return h.m.runners[id]
	}
	waitProbe := func(id string) {
		t.Helper()
		select {
		case <-runnerOf(id).probeDone:
		case <-time.After(30 * time.Second):
			t.Fatal("the bind probe did not finish")
		}
	}

	// Asked for all interfaces, bound to loopback only (GatewayPorts no): the running tunnel carries a warning.
	v := h.createTunnel(alice, Input{Name: "exposed", Type: "remote", ConnectionID: conn.ID, BindHost: "*",
		DestHost: "127.0.0.1", DestPort: webPort})
	h.startTunnel(alice, v.ID)
	waitProbe(v.ID)
	st := h.waitStatus(v.ID, func(s Status) bool { return s.Warning != "" })
	if st.State != model.TunnelRunning || !strings.Contains(st.Warning, "GatewayPorts") {
		t.Fatalf("status = %+v", st)
	}
	_, port, _ := net.SplitHostPort(st.RemoteAddr)
	if got := httpGet(t, "127.0.0.1:"+port, "/gw"); got != "hello /gw" {
		t.Fatalf("GET through the loopback-bound forward = %q", got)
	}
	h.must(alice, "POST", "/api/tunnels/"+v.ID+"/stop", nil, nil, http.StatusOK)
	var view View
	h.must(alice, "GET", "/api/tunnels/"+v.ID, nil, &view, http.StatusOK)
	if view.Status.Warning != "" {
		t.Fatalf("a stopped tunnel kept its warning: %+v", view.Status)
	}

	// A server that honours the requested address: no warning.
	wildcard.Store(true)
	v2 := h.createTunnel(alice, Input{Name: "honoured", Type: "remote", ConnectionID: conn.ID, BindHost: "0.0.0.0",
		DestHost: "127.0.0.1", DestPort: webPort})
	h.startTunnel(alice, v2.ID)
	waitProbe(v2.ID)
	h.must(alice, "GET", "/api/tunnels/"+v2.ID, nil, &view, http.StatusOK)
	if view.Status.State != model.TunnelRunning || view.Status.Warning != "" {
		t.Fatalf("status = %+v", view.Status)
	}

	// Loopback binds are never probed.
	n := probes.Load()
	v3 := h.createTunnel(alice, Input{Name: "loopback", Type: "remote", ConnectionID: conn.ID, BindHost: "127.0.0.1",
		DestHost: "127.0.0.1", DestPort: webPort})
	h.startTunnel(alice, v3.ID)
	time.Sleep(300 * time.Millisecond)
	r := runnerOf(v3.ID)
	r.mu.Lock()
	probed := r.probed
	r.mu.Unlock()
	if probed || probes.Load() != n {
		t.Fatalf("loopback bind probed (probed=%v, probes %d → %d)", probed, n, probes.Load())
	}
}

func TestReverseDynamicProxy(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	_, webPort := webServer(t)
	v := h.createTunnel(alice, Input{Name: "rsocks", Type: "dynamic", ConnectionID: conn.ID, Options: Options{Reverse: true}})
	if !v.Options.Reverse {
		t.Fatal("reverse flag lost")
	}
	st := h.startTunnel(alice, v.ID)
	d, _ := xproxy.SOCKS5("tcp", st.RemoteAddr, nil, xproxy.Direct)
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Dial: d.Dial, DisableKeepAlives: true}}
	resp, err := c.Get("http://127.0.0.1:" + itoa(webPort) + "/reverse")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hello /reverse" {
		t.Fatalf("reverse SOCKS GET = %q", b)
	}
	// Clients on the SSH server never reach NexTerm's own port (harness: 127.0.0.1:7822) through the reverse proxy.
	if c, err := d.Dial("tcp", "127.0.0.1:7822"); err == nil {
		c.Close()
		t.Fatal("reverse SOCKS reached NexTerm's own port")
	}
	h.waitStatus(v.ID, func(s Status) bool { return strings.Contains(s.LastError, "NexTerm's own port") })
}

func TestUnixSocketForwards(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	web, webPort := webServer(t)
	dir := shortTempDir(t)

	// A Unix socket "on the SSH server" serving HTTP (the test server runs locally).
	remoteSock := filepath.Join(dir, "r.sock")
	rl, err := net.Listen("unix", remoteSock)
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(rl, web.Config.Handler)
	defer rl.Close()

	// Local TCP → remote Unix socket (direct-streamlocal, e.g. docker.sock).
	v1 := h.createTunnel(alice, Input{Name: "docker", Type: "local", ConnectionID: conn.ID, Options: Options{DestSocket: remoteSock}})
	if v1.DestHost != "" || v1.DestPort != 0 || v1.Options.DestSocket != remoteSock {
		t.Fatalf("socket destination not normalized: %+v", v1)
	}
	st := h.startTunnel(alice, v1.ID)
	if got := httpGet(t, st.LocalAddr, "/unix-dest"); got != "hello /unix-dest" {
		t.Fatalf("GET via remote socket = %q", got)
	}

	// Local Unix socket listener → remote TCP.
	localSock := filepath.Join(dir, "l.sock")
	v2 := h.createTunnel(alice, Input{Name: "sock", Type: "local", ConnectionID: conn.ID, DestHost: "127.0.0.1", DestPort: webPort,
		Options: Options{BindSocket: localSock}})
	st = h.startTunnel(alice, v2.ID)
	if st.LocalAddr != localSock {
		t.Fatalf("local socket address = %q", st.LocalAddr)
	}
	uc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Dial: func(string, string) (net.Conn, error) { return net.Dial("unix", localSock) }, DisableKeepAlives: true}}
	resp, err := uc.Get("http://unix/unix-listen")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hello /unix-listen" {
		t.Fatalf("GET via local socket = %q", b)
	}
	h.must(alice, "POST", "/api/tunnels/"+v2.ID+"/stop", nil, nil, http.StatusOK)
	if _, err := net.Dial("unix", localSock); err == nil {
		t.Fatal("local socket still accepting after stop")
	}

	// Remote Unix socket listener (streamlocal-forward) → local TCP.
	remoteListen := filepath.Join(dir, "rl.sock")
	v3 := h.createTunnel(alice, Input{Name: "rsock", Type: "remote", ConnectionID: conn.ID, DestHost: "127.0.0.1", DestPort: webPort,
		Options: Options{BindSocket: remoteListen}})
	st = h.startTunnel(alice, v3.ID)
	if st.RemoteAddr != remoteListen {
		t.Fatalf("remote socket address = %q", st.RemoteAddr)
	}
	uc = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Dial: func(string, string) (net.Conn, error) { return net.Dial("unix", remoteListen) }, DisableKeepAlives: true}}
	resp, err = uc.Get("http://unix/unix-remote")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hello /unix-remote" {
		t.Fatalf("GET via remote socket listener = %q", b)
	}
}

func TestReconnectAfterConnectionLoss(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	_, webPort := webServer(t)
	local := h.createTunnel(alice, Input{Name: "l", Type: "local", ConnectionID: conn.ID, DestHost: "127.0.0.1", DestPort: webPort})
	remote := h.createTunnel(alice, Input{Name: "r", Type: "remote", ConnectionID: conn.ID, BindPort: 0, DestHost: "127.0.0.1", DestPort: webPort})
	lst := h.startTunnel(alice, local.ID)
	h.startTunnel(alice, remote.ID)
	before := srv.Conns.Load()

	srv.KillConnections()
	lst = h.waitStatus(local.ID, func(s Status) bool { return s.Reconnects >= 1 && s.State == model.TunnelRunning && s.Connected })
	rst := h.waitStatus(remote.ID, func(s Status) bool { return s.Reconnects >= 1 && s.State == model.TunnelRunning && s.Connected })
	if srv.Conns.Load() <= before {
		t.Fatal("no new SSH connection after the loss")
	}
	if got := httpGet(t, lst.LocalAddr, "/again"); got != "hello /again" {
		t.Fatalf("local forward after reconnect = %q", got)
	}
	if got := httpGet(t, rst.RemoteAddr, "/again-remote"); got != "hello /again-remote" {
		t.Fatalf("remote forward after reconnect = %q", got)
	}
}

func TestNoAutoReconnectEndsWithError(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	f := false
	v := h.createTunnel(alice, Input{Name: "l", Type: "local", ConnectionID: conn.ID, DestPort: 80, Options: Options{AutoReconnect: &f}})
	st := h.startTunnel(alice, v.ID)
	srv.KillConnections()
	h.waitStatus(v.ID, func(s Status) bool {
		return s.State == model.TunnelError && strings.Contains(s.Error, "connection lost")
	})
	waitFor(t, "listener closed", func() bool {
		c, err := net.DialTimeout("tcp", st.LocalAddr, time.Second)
		if err == nil {
			c.Close()
		}
		return err != nil
	})
}

func TestOnDemandTunnel(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	_, webPort := webServer(t)
	v := h.createTunnel(alice, Input{Name: "od", Type: "local", ConnectionID: conn.ID, DestHost: "127.0.0.1", DestPort: webPort,
		Options: Options{OnDemand: true, IdleTimeoutSec: 10}})
	h.must(alice, "POST", "/api/tunnels/"+v.ID+"/start", nil, nil, http.StatusOK)
	st := h.waitStatus(v.ID, func(s Status) bool { return s.State == model.TunnelRunning && s.Waiting == "demand" })
	if st.Connected || srv.Conns.Load() != 0 {
		t.Fatalf("on-demand tunnel connected before the first client: %+v", st)
	}
	if got := httpGet(t, st.LocalAddr, "/od"); got != "hello /od" {
		t.Fatalf("GET = %q", got)
	}
	if srv.Conns.Load() != 1 {
		t.Fatalf("SSH connections = %d", srv.Conns.Load())
	}
	// Shorten the idle timeout and wait for the disconnect.
	h.m.mu.Lock()
	r := h.m.runners[v.ID]
	h.m.mu.Unlock()
	r.idle.Store(int64(200 * time.Millisecond))
	h.waitStatus(v.ID, func(s Status) bool { return !s.Connected && s.Waiting == "demand" })
	// And it reconnects on the next client.
	if got := httpGet(t, st.LocalAddr, "/od2"); got != "hello /od2" {
		t.Fatalf("GET after idle = %q", got)
	}
}

func TestAllowFromRejectsClients(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	_, webPort := webServer(t)
	v := h.createTunnel(alice, Input{Name: "acl", Type: "local", ConnectionID: conn.ID, DestHost: "127.0.0.1", DestPort: webPort,
		Options: Options{AllowFrom: []string{"10.0.0.0/8"}}})
	st := h.startTunnel(alice, v.ID)
	c, err := net.Dial("tcp", st.LocalAddr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("client outside the allow list was served")
	}
	c.Close()
	h.waitStatus(v.ID, func(s Status) bool { return s.FailedConns == 1 && strings.Contains(s.LastError, "allowed clients") })
}
