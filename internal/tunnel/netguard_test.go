package tunnel

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/netguard"
)

func serverModeCfg(c *config.Config) { c.Mode = config.ModeServer; c.Listen = "0.0.0.0:7822" }

// TestRemoteForwardToLoopbackRefused covers SEC-7 for remote (-R) forwards, whose connections are made from the
// AstraTerm host: ordinary users cannot create them (nor session forwards of that kind) in server mode, and the
// destination guard vets every connection of a running forward, so a policy change applies at once.
func TestRemoteForwardToLoopbackRefused(t *testing.T) {
	h := newHarness(t, serverModeCfg)
	srv := newTestSSHServer(t)
	user := h.user("user", false)
	admin := h.user("admin", true)
	uconn := h.sshConn(user, srv, nil)
	aconn := h.sshConn(admin, srv, nil)
	_, webPort := webServer(t)

	for _, in := range []Input{
		{Name: "r", Type: "remote", ConnectionID: uconn.ID, DestHost: "127.0.0.1", DestPort: webPort},
		{Name: "r2", Type: "remote", ConnectionID: uconn.ID, DestHost: "localhost", DestPort: 7822},
		{Name: "rs", Type: "dynamic", ConnectionID: uconn.ID, Options: Options{Reverse: true}},
	} {
		if code, body := h.call(user, "POST", "/api/tunnels", in, nil); code != http.StatusForbidden {
			t.Fatalf("user remote forward %s: %d %s", in.Name, code, body)
		}
	}

	// An administrator's remote forward to a loopback service works while the policy does not apply to admins…
	v := h.createTunnel(admin, Input{Name: "rev", Type: "remote", ConnectionID: aconn.ID, BindHost: "127.0.0.1",
		DestHost: "127.0.0.1", DestPort: webPort})
	st := h.startTunnel(admin, v.ID)
	if got := httpGet(t, st.RemoteAddr, "/x"); got != "hello /x" {
		t.Fatalf("GET through the remote forward = %q", got)
	}
	// …and every new connection is refused once it does (checked per connection, no restart needed).
	if _, err := netguard.For(h.d).SetPolicy(context.Background(), netguard.Policy{AllowPrivate: true, ApplyToAdmins: true}); err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	if resp, err := c.Get("http://" + st.RemoteAddr + "/y"); err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("remote forward reached loopback under the policy: %q", b)
	}
	st = h.waitStatus(v.ID, func(s Status) bool { return s.FailedConns > 0 })
	if !strings.Contains(st.LastError, "not allowed in server mode") || !strings.Contains(st.LastError, "loopback") {
		t.Fatalf("lastError: %q", st.LastError)
	}
	// New remote forwards to refused literals are rejected up front.
	for _, dest := range []string{"127.0.0.1", "localhost", "169.254.169.254", "0.0.0.0", "::1"} {
		in := Input{Name: "x", Type: "remote", ConnectionID: aconn.ID, DestHost: dest, DestPort: 80}
		if code, body := h.call(admin, "POST", "/api/tunnels", in, nil); code != http.StatusForbidden ||
			!strings.Contains(string(body), "destination_blocked") {
			t.Fatalf("admin remote forward to %s under applyToAdmins: %d %s", dest, code, body)
		}
	}
	in := Input{Name: "sock", Type: "remote", ConnectionID: aconn.ID, Options: Options{DestSocket: "/var/run/docker.sock"}}
	if code, _ := h.call(admin, "POST", "/api/tunnels", in, nil); code != http.StatusForbidden {
		t.Fatalf("admin remote forward to a Unix socket under applyToAdmins: %d", code)
	}
}

// TestHostDialGuard: the host-side dialer of remote / reverse-SOCKS forwards applies the owner's guard to the
// concrete address (names resolving to loopback, Unix sockets) and stays unrestricted without one.
func TestHostDialGuard(t *testing.T) {
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
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	restricted := &spec{kind: kindRemoteDynamic, guard: func() *netguard.Guard { return &netguard.Guard{} }}
	f := newForward(context.Background(), restricted, &stats{}, log, nil)
	defer f.close()
	for _, tc := range [][2]string{{"tcp", ln.Addr().String()}, {"tcp", "localhost:" + port},
		{"unix", filepath.Join(t.TempDir(), "s.sock")}} {
		if c, err := f.reverseProxyDial(context.Background(), tc[0], tc[1]); err == nil {
			c.Close()
			t.Fatalf("restricted host dial to %s succeeded", tc[1])
		} else if _, ok := netguard.IsBlocked(err); !ok {
			t.Fatalf("host dial %s: %v", tc[1], err)
		}
	}
	if accepted.Load() != 0 {
		t.Fatal("the loopback listener was reached")
	}
	open := &spec{kind: kindRemote, guard: func() *netguard.Guard { return nil }}
	f2 := newForward(context.Background(), open, &stats{}, log, nil)
	defer f2.close()
	c, err := f2.hostDial(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("unrestricted host dial: %v", err)
	}
	c.Close()
	blocked := (&netguard.Guard{}).Control("tcp", "127.0.0.1:1", nil)
	if msg := dialError("x", "this host", &net.OpError{Op: "dial", Net: "tcp", Err: blocked}); !strings.HasPrefix(msg, "connection to 127.0.0.1:1 is not allowed") {
		t.Fatalf("dialError: %q", msg)
	}
}
