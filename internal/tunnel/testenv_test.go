package tunnel

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	xproxy "golang.org/x/net/proxy"

	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/sshx"
	"github.com/termstead/termstead/internal/term"
)

// Integration tests against the shared Docker test environment (scripts/testenv): run with TERMSTEAD_TESTENV=1.
//   ssh1  127.0.0.1:22022  test/test, TCP forwarding enabled
//   web   web:80 (nginx), reachable from ssh1 only

const testenvSSH = "127.0.0.1:22022"

func testenv(t *testing.T) {
	t.Helper()
	if os.Getenv("TERMSTEAD_TESTENV") != "1" {
		t.Skip("set TERMSTEAD_TESTENV=1 to run against the Docker test environment")
	}
}

// testenvConn saves a connection to ssh1 for u (trusting its host key).
func (h *harness) testenvConn(u *model.User, opts model.Options) *model.Connection {
	h.t.Helper()
	var key ssh.PublicKey
	cfg := &ssh.ClientConfig{User: "test", Auth: []ssh.AuthMethod{ssh.Password("test")}, Timeout: 10 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error { key = k; return nil }}
	c, err := ssh.Dial("tcp", testenvSSH, cfg)
	if err != nil {
		h.t.Fatalf("test environment ssh1 is not reachable: %v", err)
	}
	c.Close()
	h.trust("127.0.0.1", 22022, key)
	enc, err := h.d.Vault.SealJSON(map[string]string{"password": "test"})
	if err != nil {
		h.t.Fatal(err)
	}
	if opts == nil {
		opts = model.Options{}
	}
	conn := &model.Connection{Name: "ssh1", Protocol: model.ProtoSSH, Host: "127.0.0.1", Port: 22022, Username: "test",
		AuthMethod: model.AuthPassword, Options: opts, SecretsEnc: enc, SecretKeys: []string{"password"}, OwnerID: u.ID}
	conn.Normalize()
	if err := h.d.Store.Connections.Create(h.ctx, conn); err != nil {
		h.t.Fatal(err)
	}
	return conn
}

func TestTestenvLocalForwardToWeb(t *testing.T) {
	testenv(t)
	h := newHarness(t)
	u := h.user("alice", false)
	conn := h.testenvConn(u, nil)
	v := h.createTunnel(u, Input{Name: "web", Type: "local", ConnectionID: conn.ID, DestHost: "web", DestPort: 80})
	st := h.startTunnel(u, v.ID)
	body := httpGet(t, st.LocalAddr, "/")
	if !strings.Contains(body, "nginx") {
		t.Fatalf("GET via local forward: %q", body)
	}
	h.waitStatus(v.ID, func(s Status) bool { return s.TotalConns >= 1 && s.BytesIn > int64(len(body)) })
}

func TestTestenvDynamicSocksToWeb(t *testing.T) {
	testenv(t)
	h := newHarness(t)
	u := h.user("alice", false)
	conn := h.testenvConn(u, nil)
	v := h.createTunnel(u, Input{Name: "socks", Type: "dynamic", ConnectionID: conn.ID})
	st := h.startTunnel(u, v.ID)
	d, err := xproxy.SOCKS5("tcp", st.LocalAddr, nil, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Dial: d.Dial, DisableKeepAlives: true}}
	resp, err := c.Get("http://web/") // "web" only resolves inside the lab network: remote DNS
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "nginx") {
		t.Fatalf("GET via SOCKS: %d %q", resp.StatusCode, b)
	}
}

func TestTestenvRemoteForward(t *testing.T) {
	testenv(t)
	h := newHarness(t)
	u := h.user("alice", false)
	conn := h.testenvConn(u, nil)
	_, webPort := webServer(t)
	v := h.createTunnel(u, Input{Name: "rev", Type: "remote", ConnectionID: conn.ID, BindHost: "127.0.0.1", BindPort: 0,
		DestHost: "127.0.0.1", DestPort: webPort})
	st := h.startTunnel(u, v.ID)
	_, port, err := net.SplitHostPort(st.RemoteAddr)
	if err != nil || port == "0" {
		t.Fatalf("remote address %q", st.RemoteAddr)
	}
	// Fetch from ssh1 itself through the remote listener.
	cl, rel, err := h.pool.Get(h.ctx, u, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	out, _, code, err := cl.Exec(h.ctx, "wget -qO- http://127.0.0.1:"+port+"/from-ssh1")
	if err != nil || code != 0 || string(out) != "hello /from-ssh1" {
		t.Fatalf("wget on ssh1: %q code=%d err=%v", out, code, err)
	}
}

// ssh1 runs with GatewayPorts clientspecified: a remote forward on all interfaces really listens on all of them,
// and the bind probe must not flag it.
func TestTestenvRemoteForwardAllInterfaces(t *testing.T) {
	testenv(t)
	h := newHarness(t)
	u := h.user("alice", false)
	conn := h.testenvConn(u, nil)
	_, webPort := webServer(t)
	v := h.createTunnel(u, Input{Name: "rev-all", Type: "remote", ConnectionID: conn.ID, BindHost: "*", BindPort: 0,
		DestHost: "127.0.0.1", DestPort: webPort})
	st := h.startTunnel(u, v.ID)
	_, port, err := net.SplitHostPort(st.RemoteAddr)
	if err != nil || port == "0" {
		t.Fatalf("remote address %q", st.RemoteAddr)
	}
	h.m.mu.Lock()
	r := h.m.runners[v.ID]
	h.m.mu.Unlock()
	select {
	case <-r.probeDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the bind probe did not finish")
	}
	var view View
	h.must(u, "GET", "/api/tunnels/"+v.ID, nil, &view, http.StatusOK)
	if view.Status.State != model.TunnelRunning || view.Status.Warning != "" {
		t.Fatalf("status = %+v", view.Status)
	}
	var ports RemotePorts
	h.must(u, "GET", "/api/tunnels/remote-ports?connectionId="+conn.ID, nil, &ports, http.StatusOK)
	found := false
	for _, p := range ports.Ports {
		if strconv.Itoa(p.Port) == port {
			found = true
			if p.Scope != "all" {
				t.Fatalf("forward listens on %+v", p)
			}
		}
	}
	if !found {
		t.Fatalf("port %s not detected: %+v", port, ports)
	}
	// Reachable through ssh1's network address, not only its loopback.
	cl, rel, err := h.pool.Get(h.ctx, u, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	out, _, code, err := cl.Exec(h.ctx, "wget -qO- http://$(hostname -i | cut -d' ' -f1):"+port+"/via-lan")
	if err != nil || code != 0 || string(out) != "hello /via-lan" {
		t.Fatalf("wget via ssh1's address: %q code=%d err=%v", out, code, err)
	}
}

func TestTestenvReconnectAfterKillingClient(t *testing.T) {
	testenv(t)
	h := newHarness(t)
	u := h.user("alice", false)
	conn := h.testenvConn(u, nil)
	v := h.createTunnel(u, Input{Name: "web", Type: "local", ConnectionID: conn.ID, DestHost: "web", DestPort: 80})
	st := h.startTunnel(u, v.ID)
	if !strings.Contains(httpGet(t, st.LocalAddr, "/"), "nginx") {
		t.Fatal("tunnel not working before the kill")
	}
	h.m.mu.Lock()
	r := h.m.runners[v.ID]
	h.m.mu.Unlock()
	r.mu.Lock()
	cl, _ := r.cur.(*sshx.Client)
	r.mu.Unlock()
	if cl == nil {
		t.Fatal("no live SSH client")
	}
	cl.Close() // kill the SSH transport under the tunnel
	st = h.waitStatus(v.ID, func(s Status) bool { return s.Reconnects >= 1 && s.State == model.TunnelRunning && s.Connected })
	if !strings.Contains(httpGet(t, st.LocalAddr, "/"), "nginx") {
		t.Fatal("tunnel not working after the reconnect")
	}
}

func TestTestenvRemotePortsAndSessionForward(t *testing.T) {
	testenv(t)
	h := newHarness(t)
	u := h.user("alice", false)
	conn := h.testenvConn(u, model.Options{"forwards": []any{map[string]any{"type": "local", "destHost": "web", "destPort": 80}}})
	var ports RemotePorts
	h.must(u, "GET", "/api/tunnels/remote-ports?connectionId="+conn.ID, nil, &ports, http.StatusOK)
	found := false
	for _, p := range ports.Ports {
		if p.Port == 2222 {
			found = true
		}
	}
	if !found {
		t.Fatalf("sshd port not detected: %+v", ports)
	}
	s, err := h.sessions.Create(context.Background(), u, term.CreateRequest{ConnectionID: conn.ID, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer h.sessions.Close(s.ID)
	var fw SessionForward
	waitFor(t, "session forward", func() bool {
		list := h.m.sf.list(u, s.ID)
		if len(list) == 1 && list[0].Status.State == model.TunnelRunning {
			fw = list[0]
			return true
		}
		return false
	})
	if !strings.Contains(httpGet(t, fw.Status.LocalAddr, "/"), "nginx") {
		t.Fatal("session forward to web:80 not working")
	}
}
