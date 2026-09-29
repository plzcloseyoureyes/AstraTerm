// Package apitest exercises the tools module end-to-end through a real in-process NexTerm server (HTTP API + events
// socket). It lives in its own package so the tools unit tests do not depend on every other module compiling.
package apitest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/nexterm/nexterm/internal/config"
	"github.com/nexterm/nexterm/internal/server/servertest"
)

const adminPass = "correct horse battery staple"

// events is a test client of /ws/events that accepts prompts (host keys) and collects job events.
type events struct {
	t      *testing.T
	ws     *websocket.Conn
	mu     sync.Mutex
	msgs   []map[string]any
	notify chan struct{}
}

func dialEvents(t *testing.T, env *servertest.Env, c *servertest.Client) *events {
	t.Helper()
	ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(env.URL("/ws/events"), "http"),
		&websocket.DialOptions{HTTPClient: c.HTTP})
	if err != nil {
		t.Fatalf("events socket: %v", err)
	}
	ws.SetReadLimit(1 << 24)
	e := &events{t: t, ws: ws, notify: make(chan struct{}, 1)}
	t.Cleanup(func() { ws.CloseNow() })
	go func() {
		for {
			_, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			if m["type"] == "prompt" {
				p, _ := m["prompt"].(map[string]any)
				resp := map[string]any{"type": "prompt.response", "id": p["id"], "accept": true, "save": true}
				if p["kind"] != "hostkey" {
					resp["values"], resp["save"] = []string{"test"}, false
				}
				b, _ := json.Marshal(resp)
				_ = ws.Write(context.Background(), websocket.MessageText, b)
			}
			e.mu.Lock()
			e.msgs = append(e.msgs, m)
			e.mu.Unlock()
			select {
			case e.notify <- struct{}{}:
			default:
			}
		}
	}()
	e.waitFor("hello", func(m map[string]any) bool { return m["type"] == "hello" }, 10*time.Second)
	return e
}

func (e *events) waitFor(what string, pred func(map[string]any) bool, timeout time.Duration) map[string]any {
	e.t.Helper()
	deadline := time.After(timeout)
	for {
		e.mu.Lock()
		for _, m := range e.msgs {
			if pred(m) {
				e.mu.Unlock()
				return m
			}
		}
		e.mu.Unlock()
		select {
		case <-e.notify:
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			e.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// job collects the rows and the final event of one job.
type job struct {
	rows  []map[string]any
	final string // "done" | "error"
	err   string
}

func (j *job) kind(k string) []map[string]any {
	var out []map[string]any
	for _, r := range j.rows {
		if r["kind"] == k {
			out = append(out, r)
		}
	}
	return out
}

// wait blocks until the job finished and returns its rows.
func (e *events) wait(id string, timeout time.Duration) *job {
	e.t.Helper()
	e.waitFor("job "+id, func(m map[string]any) bool {
		return m["type"] == "job" && m["jobId"] == id && (m["event"] == "done" || m["event"] == "error")
	}, timeout)
	j := &job{}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, m := range e.msgs {
		if m["type"] != "job" || m["jobId"] != id {
			continue
		}
		switch m["event"] {
		case "data":
			d, _ := m["data"].(map[string]any)
			rows, _ := d["rows"].([]any)
			for _, r := range rows {
				j.rows = append(j.rows, r.(map[string]any))
			}
		default:
			j.final, _ = m["event"].(string)
			j.err, _ = m["error"].(string)
		}
	}
	return j
}

func start(t *testing.T, c *servertest.Client, tool string, body any) string {
	t.Helper()
	var res struct {
		JobID string `json:"jobId"`
	}
	c.MustJSON("POST", "/api/tools/"+tool, body, &res)
	if res.JobID == "" {
		t.Fatalf("no job id for %s", tool)
	}
	return res.JobID
}

func TestToolsAPIDesktop(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	ev := dialEvents(t, env, admin)

	if st := env.Client().JSON("POST", "/api/tools/ping", map[string]any{"host": "127.0.0.1"}, nil); st != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", st)
	}
	noCSRF := *admin
	noCSRF.CSRF = false
	if st, code := noCSRF.ErrorCode("POST", "/api/tools/dns", map[string]any{"name": "example.com"}); st != http.StatusForbidden || code != "csrf" {
		t.Fatalf("missing CSRF header: %d %s", st, code)
	}
	if st, _ := admin.ErrorCode("POST", "/api/tools/bogus", map[string]any{}); st != http.StatusNotFound {
		t.Fatalf("unknown tool: %d", st)
	}
	for tool, body := range map[string]any{
		"ping":       map[string]any{},
		"traceroute": map[string]any{"host": "a;b"},
		"portscan":   map[string]any{"targets": "10.0.0.0/16", "ports": "all"},
		"dns":        map[string]any{"name": "x", "type": "NOPE"},
		"wol":        map[string]any{"mac": "zz"},
		"httpcheck":  map[string]any{"url": "gopher://x"},
		"snmp":       map[string]any{"host": "10.0.0.1", "oid": "nope.0"},
		"throughput": map[string]any{"mode": "ssh"},
	} {
		if st, code := admin.ErrorCode("POST", "/api/tools/"+tool, body); st != http.StatusBadRequest || code != "bad_request" {
			t.Errorf("%s invalid body: %d %s", tool, st, code)
		}
	}
	if st, _ := admin.ErrorCode("POST", "/api/tools/ping", `{"host":`); st != http.StatusBadRequest {
		t.Fatalf("malformed JSON: %d", st)
	}
	if st, _ := admin.ErrorCode("POST", "/api/tools/ping", map[string]any{"host": "127.0.0.1", "viaConnectionId": "nope"}); st != http.StatusNotFound {
		t.Fatalf("unknown via connection: %d", st)
	}

	// A real job: TCP ping to a local listener streams replies and a summary.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	id := start(t, admin, "ping", map[string]any{"host": "127.0.0.1", "mode": "tcp", "port": ln.Addr().(*net.TCPAddr).Port, "count": 3, "intervalMs": 100})
	j := ev.wait(id, 20*time.Second)
	if j.final != "done" || len(j.kind("reply")) != 3 || len(j.kind("summary")) != 1 {
		t.Fatalf("ping job: %+v", j)
	}

	// Cancellation: a long ping is stopped through POST /api/jobs/{id}/cancel.
	id = start(t, admin, "ping", map[string]any{"host": "127.0.0.1", "mode": "tcp", "port": ln.Addr().(*net.TCPAddr).Port, "count": 1000, "intervalMs": 200})
	time.Sleep(300 * time.Millisecond)
	admin.MustJSON("POST", "/api/jobs/"+id+"/cancel", nil, nil)
	if j := ev.wait(id, 10*time.Second); j.final != "error" || j.err != "canceled" {
		t.Fatalf("canceled job: %+v", j)
	}

	// Per-user job limit → 429 while eight long jobs run.
	var ids []string
	for i := 0; i < 8; i++ {
		ids = append(ids, start(t, admin, "ping", map[string]any{"host": "127.0.0.1", "mode": "tcp", "port": ln.Addr().(*net.TCPAddr).Port, "count": 1000, "intervalMs": 500}))
	}
	if st, code := admin.ErrorCode("POST", "/api/tools/dns", map[string]any{"name": "example.com"}); st != http.StatusTooManyRequests || code != "too_many_requests" {
		t.Fatalf("job limit: %d %s", st, code)
	}
	for _, id := range ids {
		admin.MustJSON("POST", "/api/jobs/"+id+"/cancel", nil, nil)
		ev.wait(id, 10*time.Second)
	}
	var jobs []any
	admin.MustJSON("GET", "/api/jobs", nil, &jobs)
	if len(jobs) != 0 {
		t.Fatalf("jobs still running: %v", jobs)
	}

	// Sync endpoints in desktop mode.
	var ifaces []map[string]any
	admin.MustJSON("GET", "/api/tools/interfaces", nil, &ifaces)
	if len(ifaces) == 0 {
		t.Fatal("no interfaces")
	}
	for _, i := range ifaces { // arrays are never null (the UI iterates them)
		if _, ok := i["flags"].([]any); !ok {
			t.Fatalf("interface %v: flags = %v", i["name"], i["flags"])
		}
		if _, ok := i["addrs"].([]any); !ok {
			t.Fatalf("interface %v: addrs = %v", i["name"], i["addrs"])
		}
	}
	var socks []map[string]any
	admin.MustJSON("GET", "/api/tools/listening", nil, &socks)
	found := false
	for _, s := range socks {
		if int(s["localPort"].(float64)) == ln.Addr().(*net.TCPAddr).Port {
			found = s["pid"].(float64) == float64(os.Getpid())
		}
	}
	if !found {
		t.Fatalf("own listener with our pid not listed")
	}
	if st, _ := admin.ErrorCode("POST", "/api/tools/listening/kill", map[string]any{"pid": os.Getpid()}); st != http.StatusBadRequest {
		t.Fatalf("killing NexTerm itself: %d", st)
	}
	if st, _ := admin.ErrorCode("POST", "/api/tools/listening/kill", map[string]any{"pid": 1}); st != http.StatusBadRequest {
		t.Fatalf("killing pid 1: %d", st)
	}
	if runtime.GOOS != "windows" { // terminate a process we own
		child := exec.Command("sleep", "60")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan error, 1)
		go func() { exited <- child.Wait() }()
		admin.MustJSON("POST", "/api/tools/listening/kill", map[string]any{"pid": child.Process.Pid, "signal": "TERM"}, nil)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = child.Process.Kill()
			t.Fatal("child was not terminated")
		}
		if st, _ := admin.ErrorCode("POST", "/api/tools/listening/kill", map[string]any{"pid": child.Process.Pid, "signal": "HUP"}); st != http.StatusBadRequest {
			t.Fatalf("unsupported signal: %d", st)
		}
	}
	var key map[string]any
	admin.MustJSON("POST", "/api/tools/keygen", map[string]any{"type": "ed25519", "comment": "t@x"}, &key)
	if !strings.HasPrefix(key["publicKey"].(string), "ssh-ed25519 ") || !strings.Contains(key["privateKey"].(string), "OPENSSH PRIVATE KEY") {
		t.Fatalf("keygen = %v", key)
	}
}

func TestToolsAPIServerMode(t *testing.T) {
	env := servertest.New(t, func(c *config.Config) { c.Mode = config.ModeServer })
	admin := env.Setup("admin", adminPass)
	user := env.CreateUser(admin, "bob", "bob's long password", "user")
	uev := dialEvents(t, env, user)
	aev := dialEvents(t, env, admin)

	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/tools/portscan"}, {"POST", "/api/tools/netscan"}, {"POST", "/api/tools/throughput"},
		{"GET", "/api/tools/interfaces"}, {"GET", "/api/tools/listening"}, {"POST", "/api/tools/listening/kill"},
	} {
		if st, code := user.ErrorCode(tc.method, tc.path, map[string]any{"targets": "10.0.0.1", "host": "10.0.0.1", "pid": 999999}); st != http.StatusForbidden || code != "forbidden" {
			t.Errorf("non-admin %s %s: %d %s", tc.method, tc.path, st, code)
		}
	}
	// Non-admins cannot aim tools at the NexTerm host (SSRF hardening); admins can.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "internal") }))
	defer srv.Close()
	id := start(t, user, "httpcheck", map[string]any{"url": srv.URL})
	if j := uev.wait(id, 20*time.Second); j.final != "error" || !strings.Contains(j.err, "not allowed in server mode") {
		t.Fatalf("guarded httpcheck: %+v", j)
	}
	id = start(t, admin, "httpcheck", map[string]any{"url": srv.URL})
	if j := aev.wait(id, 20*time.Second); j.final != "done" || len(j.kind("result")) != 1 {
		t.Fatalf("admin httpcheck: %+v", j)
	}
	// Job events belong to their owner, and foreign jobs cannot be canceled.
	id = start(t, admin, "dns", map[string]any{"name": "localhost", "resolver": "192.0.2.1"})
	if st, _ := user.ErrorCode("POST", "/api/jobs/"+id+"/cancel", nil); st != http.StatusNotFound {
		t.Fatalf("foreign cancel: %d", st)
	}
	time.Sleep(200 * time.Millisecond)
	uev.mu.Lock()
	for _, m := range uev.msgs {
		if m["jobId"] == id {
			t.Errorf("user received another user's job event: %v", m)
		}
	}
	uev.mu.Unlock()
	admin.MustJSON("POST", "/api/jobs/"+id+"/cancel", nil, nil)
}

// ---- via SSH (shared Docker test environment) ----------------------------------------------------------------------

// createConn saves an SSH connection (password auth) and returns its id.
func createConn(t *testing.T, c *servertest.Client, name, host string, port int, user, pass string) string {
	t.Helper()
	var conn struct {
		ID string `json:"id"`
	}
	c.MustJSON("POST", "/api/connections", map[string]any{
		"name": name, "protocol": "ssh", "host": host, "port": port, "username": user,
		"authMethod": "password", "secrets": map[string]string{"password": pass},
	}, &conn)
	return conn.ID
}

func requireTestenv(t *testing.T) {
	t.Helper()
	if os.Getenv("NEXTERM_TESTENV") != "1" {
		t.Skip("set NEXTERM_TESTENV=1 to run tests against the Docker test environment")
	}
}

func TestToolsViaSSH(t *testing.T) {
	requireTestenv(t)
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	ev := dialEvents(t, env, admin)
	conn := struct{ ID string }{createConn(t, admin, "ssh1", "127.0.0.1", 22022, "test", "test")}

	// Remote ping (busybox output on the Alpine target).
	id := start(t, admin, "ping", map[string]any{"host": "127.0.0.1", "count": 2, "intervalMs": 200, "viaConnectionId": conn.ID})
	j := ev.wait(id, 60*time.Second)
	if j.final != "done" || len(j.kind("reply")) != 2 || len(j.kind("summary")) != 1 || j.kind("summary")[0]["recv"] != 2.0 {
		t.Fatalf("remote ping: %+v", j)
	}
	// busybox traceroute needs raw sockets: as the unprivileged "test" user the failure must be explained.
	id = start(t, admin, "traceroute", map[string]any{"host": "ssh2", "maxHops": 5, "viaConnectionId": conn.ID})
	j = ev.wait(id, 60*time.Second)
	if j.final != "error" || !strings.Contains(j.err, "needs root privileges") {
		t.Fatalf("unprivileged remote traceroute: %+v", j)
	}
	// Port scan through the SSH gateway: the internal web server is only reachable from ssh1.
	id = start(t, admin, "portscan", map[string]any{"targets": "web", "ports": "80,81", "showClosed": true, "viaConnectionId": conn.ID})
	j = ev.wait(id, 60*time.Second)
	states := map[float64]any{}
	for _, r := range j.kind("port") {
		states[r["port"].(float64)] = r["state"]
	}
	if j.final != "done" || states[80] != "open" || states[81] != "closed" {
		t.Fatalf("gateway port scan: %+v states=%v", j, states)
	}
	// Network scan through the gateway.
	id = start(t, admin, "netscan", map[string]any{"targets": "web,ssh2", "ports": "80,2222", "viaConnectionId": conn.ID})
	j = ev.wait(id, 60*time.Second)
	if j.final != "done" || len(j.kind("host")) != 2 {
		t.Fatalf("gateway netscan: %+v", j)
	}
	// SSH channel throughput, both directions.
	for _, reverse := range []bool{false, true} {
		id = start(t, admin, "throughput", map[string]any{"mode": "ssh", "viaConnectionId": conn.ID, "durationSec": 2, "reverse": reverse})
		j = ev.wait(id, 60*time.Second)
		if j.final != "done" || len(j.kind("summary")) != 1 || j.kind("summary")[0]["receiverBitsPerSec"].(float64) <= 0 {
			t.Fatalf("ssh throughput reverse=%v: %+v", reverse, j)
		}
		t.Logf("ssh throughput reverse=%v: %.1f Mbit/s", reverse, j.kind("summary")[0]["receiverBitsPerSec"].(float64)/1e6)
	}
	// Wake-on-LAN from the SSH host (python3 or wakeonlan must exist there; otherwise a clear error).
	id = start(t, admin, "wol", map[string]any{"mac": "02:00:00:00:00:01", "broadcast": "255.255.255.255", "viaConnectionId": conn.ID})
	j = ev.wait(id, 60*time.Second)
	if j.final == "done" {
		t.Logf("remote WoL sent: %v", j.kind("summary"))
	} else if !strings.Contains(j.err, "wakeonlan nor python3") {
		t.Fatalf("remote WoL: %+v", j)
	}
}

// TestRemoteTraceAsRoot runs the remote traceroute / mtr path (busybox traceroute output) on an SSH host where the
// login user is root. NEXTERM_TOOLS_ROOT_SSH=host:port:user:password (e.g. a disposable container on the testenv
// network that can reach "ssh2").
func TestRemoteTraceAsRoot(t *testing.T) {
	spec := os.Getenv("NEXTERM_TOOLS_ROOT_SSH")
	if spec == "" {
		t.Skip("set NEXTERM_TOOLS_ROOT_SSH=host:port:user:password to test remote traceroute as root")
	}
	parts := strings.SplitN(spec, ":", 4)
	if len(parts) != 4 {
		t.Fatalf("bad NEXTERM_TOOLS_ROOT_SSH %q", spec)
	}
	var port int
	fmt.Sscan(parts[1], &port)
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	ev := dialEvents(t, env, admin)
	connID := createConn(t, admin, "root-host", parts[0], port, parts[2], parts[3])

	id := start(t, admin, "traceroute", map[string]any{"host": "ssh2", "maxHops": 5, "viaConnectionId": connID})
	j := ev.wait(id, 60*time.Second)
	if j.final != "done" || len(j.kind("hop")) == 0 || j.kind("summary")[0]["reached"] != true {
		t.Fatalf("remote traceroute as root: %+v", j)
	}
	id = start(t, admin, "traceroute", map[string]any{"host": "ssh2", "mode": "mtr", "rounds": 3, "intervalMs": 200, "maxHops": 5, "viaConnectionId": connID})
	j = ev.wait(id, 60*time.Second)
	if j.final != "done" || len(j.kind("round")) != 3 || len(j.kind("mtr")) == 0 {
		t.Fatalf("remote mtr as root: %+v", j)
	}
	if last := j.kind("mtr")[len(j.kind("mtr"))-1]; last["sent"] != 3.0 || last["recv"] != 3.0 {
		t.Fatalf("remote mtr stats: %v", last)
	}
	// ICMP probes with -I (root may use them).
	id = start(t, admin, "traceroute", map[string]any{"host": "ssh2", "protocol": "icmp", "maxHops": 5, "viaConnectionId": connID})
	if j := ev.wait(id, 60*time.Second); j.final != "done" || j.kind("summary")[0]["reached"] != true {
		t.Fatalf("remote ICMP traceroute as root: %+v", j)
	}
}
