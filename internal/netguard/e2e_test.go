package netguard_test

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/termstead/termstead/internal/config"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/netguard"
	"github.com/termstead/termstead/internal/server/servertest"
)

const pw = "correct horse battery staple"

func serverMode(c *config.Config) { c.Mode = config.ModeServer }

// secretService is a loopback-only TCP service of the Termstead host (like another user's tunnel listener): it greets
// every client with a secret and counts connections.
func secretService(t *testing.T) (port int, accepted *atomic.Int32) {
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
			go func() {
				defer c.Close()
				_, _ = c.Write([]byte("SECRET-BANNER\r\n"))
				time.Sleep(2 * time.Second)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, accepted
}

func openQuick(t *testing.T, c *servertest.Client, quick map[string]any) model.RuntimeSession {
	t.Helper()
	var s model.RuntimeSession
	c.MustJSON("POST", "/api/sessions", map[string]any{"quick": quick, "cols": 80, "rows": 24}, &s)
	return s
}

// waitSettled polls the session until it is connected or failed.
func waitSettled(t *testing.T, c *servertest.Client, id string) model.RuntimeSession {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var s model.RuntimeSession
		c.MustJSON("GET", "/api/sessions/"+id, nil, &s)
		switch s.State {
		case model.StateConnected, model.StateError, model.StateClosed, model.StateDisconnected:
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s still %s: %s", id, s.State, s.StateMessage)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func scrollback(t *testing.T, c *servertest.Client, id string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, body := c.Do("GET", "/api/sessions/"+id+"/scrollback", nil)
		if strings.Contains(string(body), "SECRET-BANNER") || time.Now().After(deadline) {
			return string(body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestExploitRawTCPToLoopback reproduces the reported exploit: in server mode an ordinary user opened a raw TCP
// session to 127.0.0.1:<port> on the Termstead host (another user's tunnel listener). It must now be refused with a
// clear error before any connection is made, while an administrator is unaffected.
func TestExploitRawTCPToLoopback(t *testing.T) {
	env := servertest.New(t, serverMode)
	admin := env.Setup("admin", pw)
	bob := env.CreateUser(admin, "bob", pw, "user")
	port, accepted := secretService(t)

	for _, host := range []string{"127.0.0.1", "localhost", "127.1", "[::ffff:127.0.0.1]", "0.0.0.0"} {
		start := time.Now()
		s := openQuick(t, bob, map[string]any{"protocol": "raw", "host": strings.Trim(host, "[]"), "port": port})
		s = waitSettled(t, bob, s.ID)
		t.Logf("raw %s refused in %v", host, time.Since(start).Round(time.Millisecond))
		if s.State != model.StateError || !strings.Contains(s.StateMessage, "not allowed in server mode") {
			t.Fatalf("raw session to %s:%d: state %s %q", host, port, s.State, s.StateMessage)
		}
		if host == "127.0.0.1" && !strings.Contains(s.StateMessage, "loopback") {
			t.Fatalf("unclear error: %q", s.StateMessage)
		}
		if _, body := bob.Do("GET", "/api/sessions/"+s.ID+"/scrollback", nil); strings.Contains(string(body), "SECRET-BANNER") {
			t.Fatal("the user reached the loopback service")
		}
	}
	// Other protocols share the dial path: telnet, and SSH (direct, through a proxy on the host, via a jump host
	// on the host).
	for name, quick := range map[string]map[string]any{ // (localhost may resolve to ::1 first)
		"telnet": {"protocol": "telnet", "host": "127.0.0.1", "port": port},
		"ssh":    {"protocol": "ssh", "host": "127.0.0.1", "port": port, "username": "x"},
		"ssh via socks proxy": {"protocol": "ssh", "host": "192.0.2.10", "port": 22, "username": "x",
			"options": map[string]any{"proxy": map[string]any{"type": "socks5", "host": "127.0.0.1", "port": port}}},
		"ssh via http proxy": {"protocol": "ssh", "host": "192.0.2.10", "port": 22, "username": "x",
			"options": map[string]any{"proxy": map[string]any{"type": "http", "host": "127.0.0.1", "port": port}}},
		"ssh via jump host": {"protocol": "ssh", "host": "192.0.2.10", "port": 22, "username": "x",
			"options": map[string]any{"jumpHosts": []string{"x@127.0.0.1:" + strconv.Itoa(port)}}},
		"raw via jump host": {"protocol": "raw", "host": "192.0.2.10", "port": 22,
			"options": map[string]any{"jumpHosts": []string{"x@localhost:" + strconv.Itoa(port)}}},
	} {
		start := time.Now()
		s := waitSettled(t, bob, openQuick(t, bob, quick).ID)
		t.Logf("%s refused in %v", name, time.Since(start).Round(time.Millisecond))
		if s.State != model.StateError || !strings.Contains(s.StateMessage, "not allowed in server mode") ||
			!strings.Contains(s.StateMessage, "loopback") {
			t.Fatalf("%s: state %s %q", name, s.State, s.StateMessage)
		}
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("the loopback service saw %d connections from a restricted user", n)
	}

	// The administrator is not restricted (applyToAdmins is off by default).
	s := waitSettled(t, admin, openQuick(t, admin, map[string]any{"protocol": "raw", "host": "127.0.0.1", "port": port}).ID)
	if s.State != model.StateConnected {
		t.Fatalf("admin raw session: %s %q", s.State, s.StateMessage)
	}
	if !strings.Contains(scrollback(t, admin, s.ID), "SECRET-BANNER") {
		t.Fatal("admin session did not reach the service")
	}

	// An administrator exception opens exactly that service to users.
	admin.MustJSON("PUT", "/api/admin/network-policy", map[string]any{"allow": []string{"127.0.0.1/32"},
		"allowedPorts": fmt.Sprintf("22,%d", port)}, nil)
	s = waitSettled(t, bob, openQuick(t, bob, map[string]any{"protocol": "raw", "host": "127.0.0.1", "port": port}).ID)
	if s.State != model.StateConnected {
		t.Fatalf("raw session with an exception: %s %q", s.State, s.StateMessage)
	}
	// …but not other ports (allowedPorts), and applyToAdmins restricts the admin too.
	admin.MustJSON("PUT", "/api/admin/network-policy", map[string]any{"allow": []string{}, "applyToAdmins": true}, nil)
	s = waitSettled(t, admin, openQuick(t, admin, map[string]any{"protocol": "raw", "host": "127.0.0.1", "port": port}).ID)
	if s.State != model.StateError || !strings.Contains(s.StateMessage, "not allowed") {
		t.Fatalf("admin with applyToAdmins: %s %q", s.State, s.StateMessage)
	}
}

// TestDesktopModeUnrestricted: desktop mode is the user's own machine.
func TestDesktopModeUnrestricted(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", pw)
	port, _ := secretService(t)
	s := waitSettled(t, admin, openQuick(t, admin, map[string]any{"protocol": "raw", "host": "127.0.0.1", "port": port}).ID)
	if s.State != model.StateConnected || !strings.Contains(scrollback(t, admin, s.ID), "SECRET-BANNER") {
		t.Fatalf("desktop raw session: %s %q", s.State, s.StateMessage)
	}
	var v netguard.PolicyView
	admin.MustJSON("GET", "/api/admin/network-policy", nil, &v)
	if v.Enforced || v.Mode != "desktop" {
		t.Fatalf("desktop view: %+v", v)
	}
	var res netguard.TestResult
	admin.MustJSON("POST", "/api/admin/network-policy/test", map[string]any{"host": "127.0.0.1", "port": 22}, &res)
	if res.Decision != "unrestricted" || !res.Allowed {
		t.Fatalf("desktop dry run: %+v", res)
	}
}

func TestAdminAPI(t *testing.T) {
	env := servertest.New(t, serverMode)
	admin := env.Setup("admin", pw)
	bob := env.CreateUser(admin, "bob", pw, "user")

	// Non-admins and anonymous callers.
	for _, rq := range []struct{ method, path string }{{"GET", "/api/admin/network-policy"},
		{"PUT", "/api/admin/network-policy"}, {"POST", "/api/admin/network-policy/test"}} {
		if st, _ := bob.ErrorCode(rq.method, rq.path, map[string]any{"host": "127.0.0.1"}); st != http.StatusForbidden {
			t.Fatalf("user %s %s: %d", rq.method, rq.path, st)
		}
		if st, _ := env.Client().ErrorCode(rq.method, rq.path, map[string]any{}); st != http.StatusUnauthorized {
			t.Fatalf("anonymous %s %s: %d", rq.method, rq.path, st)
		}
	}

	var v netguard.PolicyView
	admin.MustJSON("GET", "/api/admin/network-policy", nil, &v)
	if !v.Enforced || !v.Policy.AllowPrivate || !v.Policy.BlockHostAddresses || v.Policy.ApplyToAdmins ||
		len(v.Builtin) == 0 || v.Policy.Deny == nil {
		t.Fatalf("default view: %+v", v)
	}

	for _, bad := range []map[string]any{{"deny": []string{"nope"}}, {"allowedPorts": "0-5"}, {"unknownKey": true},
		{"allow": "127.0.0.1"}} {
		if st, code := admin.ErrorCode("PUT", "/api/admin/network-policy", bad); st != http.StatusBadRequest || code != "bad_request" {
			t.Fatalf("invalid policy %v: %d %s", bad, st, code)
		}
	}
	admin.MustJSON("PUT", "/api/admin/network-policy", map[string]any{"allowPrivate": false, "deny": []string{"198.51.100.7/24"},
		"allow": []string{"10.1.0.0/16"}, "allowedPorts": "443, 22"}, &v)
	if v.Policy.AllowPrivate || strings.Join(v.Policy.Deny, ",") != "198.51.100.0/24" || v.Policy.AllowedPorts != "22,443" ||
		!v.Policy.BlockHostAddresses {
		t.Fatalf("saved policy: %+v", v.Policy)
	}
	// Omitted keys keep their value.
	admin.MustJSON("PUT", "/api/admin/network-policy", map[string]any{"allowedPorts": ""}, &v)
	if v.Policy.AllowPrivate || len(v.Policy.Deny) != 1 || v.Policy.AllowedPorts != "" {
		t.Fatalf("partial update: %+v", v.Policy)
	}
	var audit []model.AuditEntry
	admin.MustJSON("GET", "/api/admin/audit?action=netguard.policy.update", nil, &audit)
	if len(audit) != 2 {
		t.Fatalf("audit entries: %d", len(audit))
	}

	u, err := env.Server.Deps.Store.Users.GetByUsername(t.Context(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := env.Server.Deps.Store.Users.GetByUsername(t.Context(), "admin")
	cases := []struct {
		body     map[string]any
		decision string
		class    string
	}{
		{map[string]any{"host": "127.0.0.1", "port": 22}, "deny", netguard.ClassLoopback},
		{map[string]any{"host": "localhost", "port": 22}, "deny", netguard.ClassLoopback},
		{map[string]any{"host": "2130706433", "port": 22}, "deny", netguard.ClassLoopback},
		{map[string]any{"host": "169.254.169.254", "port": 80}, "deny", netguard.ClassLinkLocal},
		{map[string]any{"host": "10.9.0.1", "port": 22}, "deny", netguard.ClassPrivate},
		{map[string]any{"host": "10.1.2.3", "port": 22}, "allow", netguard.ClassAllowRule},
		{map[string]any{"host": "198.51.100.9", "port": 22}, "deny", netguard.ClassDenyRule},
		{map[string]any{"host": "8.8.8.8", "port": 53}, "allow", netguard.ClassPublic},
		{map[string]any{"host": "127.0.0.1", "port": 22, "userId": u.ID}, "deny", netguard.ClassLoopback},
		{map[string]any{"host": "127.0.0.1", "port": 22, "userId": a.ID}, "unrestricted", ""},
		{map[string]any{"host": "127.0.0.1", "port": 5432, "policy": map[string]any{"allowPrivate": true,
			"allow": []string{"127.0.0.1"}}}, "allow", netguard.ClassAllowRule},
	}
	for _, tc := range cases {
		var res netguard.TestResult
		admin.MustJSON("POST", "/api/admin/network-policy/test", tc.body, &res)
		if res.Decision != tc.decision || (tc.class != "" && (len(res.Addresses) == 0 || res.Addresses[0].Class != tc.class)) ||
			res.Reason == "" {
			t.Errorf("dry run %v: %+v", tc.body, res)
		}
	}
	if st, _ := admin.ErrorCode("POST", "/api/admin/network-policy/test", map[string]any{"host": "x", "userId": "nosuchuser00000000000"}); st != http.StatusNotFound {
		t.Fatalf("unknown user: %d", st)
	}
	for _, bad := range []map[string]any{{"host": ""}, {"host": "a b"}, {"host": "x", "port": 70000},
		{"host": "x", "policy": map[string]any{"deny": []string{"bad"}}}} {
		if st, _ := admin.ErrorCode("POST", "/api/admin/network-policy/test", bad); st != http.StatusBadRequest {
			t.Fatalf("bad dry run %v: %d", bad, st)
		}
	}
}
