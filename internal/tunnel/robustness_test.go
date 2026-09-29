package tunnel

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	xproxy "golang.org/x/net/proxy"

	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/term"
)

// Load, teardown, counter and resilience tests of the data plane.

// echoServer accepts connections that echo everything back and close after the client's EOF.
func echoServer(t *testing.T) int {
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
				io.Copy(c, c)
				c.Close()
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// sinkServer reads exactly in bytes from each client, then writes out bytes and closes.
func sinkServer(t *testing.T, in, out int) int {
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
				if _, err := io.CopyN(io.Discard, c, int64(in)); err != nil {
					return
				}
				c.Write(make([]byte, out))
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// freezer is a TCP relay in front of an SSH server whose traffic can be held (a black-holed link).
type freezer struct {
	ln     net.Listener
	frozen atomic.Bool
	mu     sync.Mutex
	conns  []net.Conn
}

func newFreezer(t *testing.T, target string) *freezer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &freezer{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			f.mu.Lock()
			f.conns = append(f.conns, c, up)
			f.mu.Unlock()
			go f.copy(up, c)
			go f.copy(c, up)
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		f.mu.Lock()
		for _, c := range f.conns {
			c.Close()
		}
		f.mu.Unlock()
	})
	return f
}

func (f *freezer) copy(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		for f.frozen.Load() {
			time.Sleep(10 * time.Millisecond)
		}
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (f *freezer) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

// dataPlaneGoroutines counts goroutines running tunnel listener / handler / relay / proxy code.
func dataPlaneGoroutines() int {
	buf := make([]byte, 16<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "internal/tunnel.(*forward)") || strings.Contains(g, "internal/tunnel.relay") ||
			strings.Contains(g, "internal/tunnel.(*proxy)") || strings.Contains(g, "things-go/go-socks5") {
			n++
		}
	}
	return n
}

// Hundreds of concurrent SOCKS5 connections through one tunnel: all are served, counters are exact, and stopping the
// tunnel tears every connection and goroutine down promptly.
func TestSocksManyConcurrentConnectionsAndCleanTeardown(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	echoPort := echoServer(t)
	before := dataPlaneGoroutines()
	v := h.createTunnel(alice, Input{Name: "socks", Type: "dynamic", ConnectionID: conn.ID})
	st := h.startTunnel(alice, v.ID)
	d, err := xproxy.SOCKS5("tcp", st.LocalAddr, nil, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}

	const n = 300
	var (
		mu      sync.Mutex
		clients []net.Conn
		sent    atomic.Int64
		wg      sync.WaitGroup
		failed  atomic.Int32
		connect = make(chan struct{}, 32) // bounded connect burst: a kernel accept queue is only 128 deep on macOS
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			connect <- struct{}{}
			c, err := d.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", echoPort))
			<-connect
			if err != nil {
				failed.Add(1)
				t.Log(err)
				return
			}
			msg := fmt.Sprintf("hello from client %d", i)
			if _, err := io.WriteString(c, msg); err != nil {
				failed.Add(1)
				return
			}
			buf := make([]byte, len(msg))
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.ReadFull(c, buf); err != nil || string(buf) != msg {
				failed.Add(1)
				return
			}
			c.SetReadDeadline(time.Time{})
			sent.Add(int64(len(msg)))
			mu.Lock()
			clients = append(clients, c)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if failed.Load() != 0 || len(clients) != n {
		t.Fatalf("%d of %d connections failed", failed.Load(), n)
	}
	s := h.waitStatus(v.ID, func(s Status) bool { return s.ActiveConns == n })
	// Counters are the payload relayed to and from the destination, not the SOCKS negotiation.
	if s.BytesOut != sent.Load() || s.BytesIn != sent.Load() || s.TotalConns != n || s.FailedConns != 0 {
		t.Fatalf("counters: out=%d in=%d (payload %d) total=%d failed=%d", s.BytesOut, s.BytesIn, sent.Load(), s.TotalConns, s.FailedConns)
	}

	// Clients hanging up are accounted for.
	for _, c := range clients[:n/2] {
		c.Close()
	}
	h.waitStatus(v.ID, func(s Status) bool { return s.ActiveConns == n-n/2 })

	// Stopping tears the rest down at once.
	t0 := time.Now()
	h.must(alice, "POST", "/api/tunnels/"+v.ID+"/stop", nil, nil, http.StatusOK)
	if dt := time.Since(t0); dt > 3*time.Second {
		t.Fatalf("stopping took %s", dt)
	}
	for _, c := range clients[n/2:] {
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatal("a client connection survived the stop")
		} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("a client connection is still open after the stop")
		}
		c.Close()
	}
	waitFor(t, "data-plane goroutines to exit", func() bool { return dataPlaneGoroutines() <= before })
	if st := h.m.status(v.ID); st.State != model.TunnelStopped {
		t.Fatalf("status after stop: %+v", st)
	}
}

// Traffic counters measure the payload exchanged with destinations for every proxy protocol; negotiation bytes and
// locally answered requests (PAC file) are not traffic.
func TestProxyCountersArePayloadOnly(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	const up, down = 12345, 54321
	port := sinkServer(t, up, down)
	target := fmt.Sprintf("127.0.0.1:%d", port)
	v := h.createTunnel(alice, Input{Name: "p", Type: "dynamic", ConnectionID: conn.ID})
	st := h.startTunnel(alice, v.ID)

	exchange := func(c net.Conn) {
		t.Helper()
		if _, err := c.Write(make([]byte, up)); err != nil {
			t.Fatal(err)
		}
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		got, _ := io.Copy(io.Discard, c)
		c.Close()
		if got != down {
			t.Fatalf("received %d bytes, want %d", got, down)
		}
	}
	// expect checks the counters after conns client connections, relayed of which carried the payload.
	expect := func(what string, conns, relayed int64) {
		t.Helper()
		s := h.waitStatus(v.ID, func(s Status) bool { return s.ActiveConns == 0 && s.TotalConns >= conns })
		if s.BytesOut != relayed*up || s.BytesIn != relayed*down {
			t.Fatalf("%s: out=%d in=%d, want %d / %d", what, s.BytesOut, s.BytesIn, relayed*up, relayed*down)
		}
	}

	// SOCKS5.
	d, _ := xproxy.SOCKS5("tcp", st.LocalAddr, nil, xproxy.Direct)
	c, err := d.Dial("tcp", target)
	if err != nil {
		t.Fatal(err)
	}
	exchange(c)
	expect("SOCKS5", 1, 1)

	// SOCKS4.
	c, err = net.Dial("tcp", st.LocalAddr)
	if err != nil {
		t.Fatal(err)
	}
	req := []byte{4, 1, 0, 0, 127, 0, 0, 1, 0}
	binary.BigEndian.PutUint16(req[2:], uint16(port))
	c.Write(req)
	rep := make([]byte, 8)
	if _, err := io.ReadFull(c, rep); err != nil || rep[1] != 0x5A {
		t.Fatalf("SOCKS4 reply %v %v", rep, err)
	}
	exchange(c)
	expect("SOCKS4", 2, 2)

	// HTTP CONNECT.
	c, err = net.Dial("tcp", st.LocalAddr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	if br.Buffered() != 0 {
		t.Fatal("unexpected bytes after the CONNECT reply")
	}
	exchange(c)
	expect("CONNECT", 3, 3)

	// The PAC file is served locally.
	httpGet(t, st.LocalAddr, "/proxy.pac")
	expect("PAC", 4, 3)
}

// Stopping a remote forward whose SSH link is black-holed must not wait for the keep-alives to kill the connection:
// the cancellation of the remote listener finishes in the background.
func TestStopRemoteForwardOnBlackholedLink(t *testing.T) {
	h := newHarness(t)
	h.pool.IdleTTL = time.Minute // keep the idle transport: nothing but the tunnel may end it here
	srv := newTestSSHServer(t)
	fz := newFreezer(t, srv.addr())
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	conn.Port = fz.port()
	if err := h.d.Store.Connections.Update(h.ctx, conn); err != nil {
		t.Fatal(err)
	}
	h.trust(srv.Host, fz.port(), srv.HostKey.PublicKey())
	_, webPort := webServer(t)
	v := h.createTunnel(alice, Input{Name: "r", Type: "remote", ConnectionID: conn.ID, DestHost: "127.0.0.1", DestPort: webPort})
	st := h.startTunnel(alice, v.ID)
	_, port, _ := net.SplitHostPort(st.RemoteAddr)

	fz.frozen.Store(true)
	t0 := time.Now()
	h.must(alice, "POST", "/api/tunnels/"+v.ID+"/stop", nil, nil, http.StatusOK)
	if dt := time.Since(t0); dt > closeWait+1500*time.Millisecond {
		t.Fatalf("stopping over a dead link took %s", dt)
	}
	if !srv.hasListener(":" + port) {
		t.Fatal("the frozen link delivered the cancellation")
	}
	// Once the link recovers, the cancellation still reaches the server.
	fz.frozen.Store(false)
	waitFor(t, "remote listener cancelled", func() bool { return !srv.hasListener(":" + port) })
}

// Restarting a remote forward on a fixed port over a healthy link waits for the old listener's cancellation, so the
// server accepts the port again.
func TestRestartRemoteForwardOnFixedPort(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	_, webPort := webServer(t)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	fixed := l.Addr().(*net.TCPAddr).Port
	l.Close()
	v := h.createTunnel(alice, Input{Name: "r", Type: "remote", ConnectionID: conn.ID, BindPort: fixed, DestHost: "127.0.0.1", DestPort: webPort})
	h.startTunnel(alice, v.ID)
	for i := 0; i < 5; i++ {
		h.must(alice, "POST", "/api/tunnels/"+v.ID+"/restart", nil, nil, http.StatusOK)
		st := h.waitStatus(v.ID, func(s Status) bool {
			return (s.State == model.TunnelRunning && s.Connected) || s.State == model.TunnelError
		})
		if st.State != model.TunnelRunning {
			t.Fatalf("restart %d: %+v", i, st)
		}
		if got := httpGet(t, fmt.Sprintf("127.0.0.1:%d", fixed), "/r"); got != "hello /r" {
			t.Fatalf("restart %d: GET = %q", i, got)
		}
	}
}

func TestMaxConnsLimit(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	echoPort := echoServer(t)
	v := h.createTunnel(alice, Input{Name: "l", Type: "local", ConnectionID: conn.ID, DestHost: "127.0.0.1", DestPort: echoPort,
		Options: Options{MaxConns: 20}})
	st := h.startTunnel(alice, v.ID)
	served := 0
	var clients []net.Conn
	defer func() {
		for _, c := range clients {
			c.Close()
		}
	}()
	for i := 0; i < 25; i++ {
		c, err := net.Dial("tcp", st.LocalAddr)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
		c.Write([]byte("x"))
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			served++
		}
	}
	s := h.waitStatus(v.ID, func(s Status) bool { return s.FailedConns == 5 })
	if served != 20 || s.ActiveConns != 20 || !strings.Contains(s.LastError, "limit of 20") {
		t.Fatalf("served %d, status %+v", served, s)
	}
}

func TestPACFile(t *testing.T) {
	if got := pacProxies("127.0.0.1:1080", false); got != "SOCKS5 127.0.0.1:1080; SOCKS 127.0.0.1:1080; PROXY 127.0.0.1:1080" {
		t.Fatalf("pac without auth = %q", got)
	}
	// Browsers cannot authenticate to SOCKS proxies: the HTTP proxy (Basic auth) comes first, SOCKS4 is refused.
	if got := pacProxies("[::1]:1080", true); got != "PROXY [::1]:1080; SOCKS5 [::1]:1080" {
		t.Fatalf("pac with auth = %q", got)
	}

	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	v := h.createTunnel(alice, Input{Name: "s", Type: "dynamic", ConnectionID: conn.ID, Options: Options{SocksUsername: "u"},
		Secrets: map[string]string{"socksPassword": "p"}})
	st := h.startTunnel(alice, v.ID)
	resp, err := http.Get("http://" + st.LocalAddr + "/proxy.pac")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Content-Type") != "application/x-ns-proxy-autoconfig" || !strings.Contains(string(b), `"PROXY `+st.LocalAddr+"; SOCKS5 "+st.LocalAddr+`"`) {
		t.Fatalf("PAC: %s %q", resp.Header.Get("Content-Type"), b)
	}
	// A hostile Host header cannot inject script into the PAC.
	req, _ := http.NewRequest("GET", "http://"+st.LocalAddr+"/proxy.pac", nil)
	req.Host = `x");alert(1);("`
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), "alert") {
		t.Fatalf("PAC reflects the Host header: %q", b)
	}
}

// An allowed-clients list that admits every address does not make an unauthenticated proxy on a public address
// acceptable (it would be an open proxy).
func TestProxyExposureNeedsRestrictiveAllowList(t *testing.T) {
	d := def{Type: model.TunnelDynamic, BindHost: "*", BindPort: 1080}
	if err := d.normalize(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		allow []string
		ok    bool
	}{
		{nil, false},
		{[]string{"0.0.0.0/0"}, false},
		{[]string{"10.0.0.0/8", "::/0"}, false},
		{[]string{"10.0.0.0/8"}, true},
		{[]string{"192.168.1.20"}, true},
	} {
		_, err := buildSpec(d, Options{AllowFrom: c.allow}, nil)
		if (err == nil) != c.ok {
			t.Errorf("allowFrom %v: err = %v", c.allow, err)
		}
	}
	if !restrictive([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}) || restrictive(nil) {
		t.Fatal("restrictive()")
	}
}

// A client that does not finish the proxy negotiation is dropped, also on a reverse proxy whose clients arrive as SSH
// channels (no deadline support); a negotiated connection is not affected.
func TestProxyHandshakeWatchdog(t *testing.T) {
	old := handshakeTimeout.Load()
	handshakeTimeout.Store(int64(300 * time.Millisecond))
	defer handshakeTimeout.Store(old)

	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	echoPort := echoServer(t)
	local := h.createTunnel(alice, Input{Name: "d", Type: "dynamic", ConnectionID: conn.ID})
	lst := h.startTunnel(alice, local.ID)
	reverse := h.createTunnel(alice, Input{Name: "rd", Type: "dynamic", ConnectionID: conn.ID, Options: Options{Reverse: true}})
	rst := h.startTunnel(alice, reverse.ID)

	for _, addr := range []string{lst.LocalAddr, rst.RemoteAddr} {
		for _, greeting := range []string{"", "\x05", "GET / HTTP/1.1\r\nHost: x\r\n"} {
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			io.WriteString(c, greeting)
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			t0 := time.Now()
			_, err = io.Copy(io.Discard, c)
			c.Close()
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatalf("%s with greeting %q: an idle client was not dropped", addr, greeting)
			}
			if dt := time.Since(t0); dt > 3*time.Second {
				t.Fatalf("%s: dropped after %s", addr, dt)
			}
		}
		// A completed negotiation outlives the handshake timeout.
		d, _ := xproxy.SOCKS5("tcp", addr, nil, xproxy.Direct)
		c, err := d.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", echoPort))
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(600 * time.Millisecond)
		io.WriteString(c, "still here")
		buf := make([]byte, 10)
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "still here" {
			t.Fatalf("%s: negotiated connection broken after the handshake timeout: %q %v", addr, buf, err)
		}
		c.Close()
	}
}

// A tunnel over a shared connection stops once the connection is no longer shared with its owner.
func TestReconcileStopsTunnelWhenConnectionUnshared(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	admin := h.user("root", true)
	alice := h.user("alice", false)
	conn := h.sshConn(admin, srv, nil)
	conn.Shared = true
	if err := h.d.Store.Connections.Update(h.ctx, conn); err != nil {
		t.Fatal(err)
	}
	_, webPort := webServer(t)
	v := h.createTunnel(alice, Input{Name: "l", Type: "local", ConnectionID: conn.ID, DestHost: "127.0.0.1", DestPort: webPort})
	st := h.startTunnel(alice, v.ID)
	h.m.reconcile()
	if !h.m.running(v.ID) {
		t.Fatal("reconcile stopped a tunnel over a visible shared connection")
	}
	conn.Shared = false
	if err := h.d.Store.Connections.Update(h.ctx, conn); err != nil {
		t.Fatal(err)
	}
	h.m.reconcile()
	if h.m.running(v.ID) {
		t.Fatal("tunnel still running over a connection that is no longer shared")
	}
	if s := h.m.status(v.ID); s.State != model.TunnelError || !strings.Contains(s.Error, "no longer shared") {
		t.Fatalf("status = %+v", s)
	}
	if c, err := net.DialTimeout("tcp", st.LocalAddr, time.Second); err == nil {
		c.Close()
		t.Fatal("listener still open")
	}
}

// The listening-port probe runs under sh whatever the login shell is (sshd hands exec commands to it).
func TestPortsProbeRunsUnderAnyLoginShell(t *testing.T) {
	if strings.ContainsAny(portsProbe, "'\n") || !strings.HasPrefix(portsCommand, "sh -c '") {
		t.Fatalf("probe must be one line without single quotes: %q", portsCommand)
	}
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX shells")
	}
	tried := 0
	for _, sh := range []string{"sh", "bash", "dash", "zsh", "ksh", "csh", "tcsh", "fish"} {
		path, err := exec.LookPath(sh)
		if err != nil {
			continue
		}
		tried++
		out, err := exec.Command(path, "-c", portsCommand).Output()
		if m, _ := splitMarker(out); m == "" {
			t.Errorf("%s: no probe marker in %q (%v)", sh, out, err)
		}
	}
	if tried == 0 {
		t.Skip("no shell found")
	}
}

func TestCreateWithStartAndImportExposureWarnings(t *testing.T) {
	h := newHarness(t)
	srv := newTestSSHServer(t)
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	_, webPort := webServer(t)
	v := h.createTunnel(alice, Input{Name: "l", Type: "local", ConnectionID: conn.ID, DestHost: "127.0.0.1", DestPort: webPort, Start: true})
	st := h.waitStatus(v.ID, func(s Status) bool { return s.State == model.TunnelRunning && s.Connected })
	if got := httpGet(t, st.LocalAddr, "/started"); got != "hello /started" {
		t.Fatalf("GET = %q", got)
	}

	file := map[string]any{"format": exportFormat, "version": 1, "tunnels": []map[string]any{
		{"name": "private", "type": "local", "bindHost": "127.0.0.1", "bindPort": 0, "destHost": "db", "destPort": 5432, "connection": map[string]any{"id": conn.ID}},
		{"name": "exposed", "type": "local", "bindHost": "*", "bindPort": 0, "destHost": "db", "destPort": 5432, "connection": map[string]any{"id": conn.ID}},
		{"name": "remote", "type": "remote", "bindHost": "0.0.0.0", "bindPort": 0, "destHost": "localhost", "destPort": 80, "connection": map[string]any{"id": conn.ID}},
	}}
	var res struct {
		Planned []importPlanned `json:"planned"`
	}
	h.must(alice, "POST", "/api/tunnels/import", map[string]any{"file": file, "dryRun": true}, &res, http.StatusOK)
	if len(res.Planned) != 3 || res.Planned[0].Warning != "" || !strings.Contains(res.Planned[1].Warning, "other machines") ||
		!strings.Contains(res.Planned[2].Warning, "SSH server") {
		t.Fatalf("planned = %+v", res.Planned)
	}
}

// Several windows watching the same session share one poller (one exec per interval), each gets the full list first,
// and the poller stops with the last window.
func TestPortsWatcherIsSharedBetweenWindows(t *testing.T) {
	h := newHarness(t)
	h.d.Router.WS("/ws/events", h.d.Events.ServeWS)
	srv := newTestSSHServer(t)
	var probes atomic.Int32
	srv.Exec = func(cmd string) (string, int) {
		if !strings.Contains(cmd, "@@ss") {
			return "", 127
		}
		probes.Add(1)
		return "@@ss\nLISTEN 0 128 0.0.0.0:22 0.0.0.0:*\n", 0
	}
	alice := h.user("alice", false)
	conn := h.sshConn(alice, srv, nil)
	s, err := h.sessions.Create(h.ctx, alice, term.CreateRequest{ConnectionID: conn.ID, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer h.sessions.Close(s.ID)
	waitFor(t, "session connected", func() bool { st, _ := s.State(); return st == model.StateConnected })

	ctx, cancel := context.WithTimeout(h.ctx, 30*time.Second)
	defer cancel()
	window := func() *websocket.Conn {
		ws, _, err := websocket.Dial(ctx, strings.Replace(h.api.URL, "http", "ws", 1)+"/ws/events",
			&websocket.DialOptions{HTTPHeader: http.Header{"X-Test-User": {"alice"}, "Origin": {h.api.URL}}})
		if err != nil {
			t.Fatal(err)
		}
		sub, _ := json.Marshal(map[string]any{"type": "subscribe", "topic": "tunnel.ports", "sessionId": s.ID})
		ws.Write(ctx, websocket.MessageText, sub)
		return ws
	}
	initial := func(ws *websocket.Conn) {
		t.Helper()
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var ev PortsEvent
			if json.Unmarshal(b, &ev) == nil && ev.Type == evPorts {
				if !ev.Initial || len(ev.Ports) != 1 {
					t.Fatalf("first event: %+v", ev)
				}
				return
			}
		}
	}
	a, b, c := window(), window(), window()
	initial(a)
	initial(b)
	initial(c)
	time.Sleep(watchInterval + time.Second)
	if n := probes.Load(); n > 3 {
		t.Fatalf("%d probes for three windows over one interval: pollers are not shared", n)
	}
	h.m.pwMu.Lock()
	n := len(h.m.watches)
	h.m.pwMu.Unlock()
	if n != 1 {
		t.Fatalf("%d watchers", n)
	}
	a.CloseNow()
	b.CloseNow()
	c.CloseNow()
	waitFor(t, "watcher stopped", func() bool {
		h.m.pwMu.Lock()
		defer h.m.pwMu.Unlock()
		return len(h.m.watches) == 0
	})
}

// The HTTP side of a proxy starts with its first HTTP client. A client arriving while the forward closes must not
// start a server nobody shuts down; a started server ends with the forward.
func TestProxyHTTPSideLifecycle(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sp := &spec{kind: kindDynamic, bindHost: "127.0.0.1", httpProxy: true, maxConns: 8}
	before := dataPlaneGoroutines()

	f := newForward(context.Background(), sp, &stats{}, log, nil)
	f.close()
	if f.proxy.httpListener() != nil {
		t.Fatal("HTTP proxy server started after the forward was closed")
	}

	f = newForward(context.Background(), sp, &stats{}, log, nil)
	if f.proxy.httpListener() == nil {
		t.Fatal("HTTP proxy server not started")
	}
	f.close()
	waitFor(t, "HTTP proxy server to stop", func() bool { return dataPlaneGoroutines() <= before })
}
