package apitest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/nexterm/nexterm/internal/config"
	"github.com/nexterm/nexterm/internal/server/servertest"
)

const adminPass = "correct horse battery staple"

// events is a test client of /ws/events that answers prompts and collects monitor events.
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
	ws.SetReadLimit(1 << 22)
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
			if m["type"] == "prompt" { // accept host keys, answer password prompts with the test password
				p, _ := m["prompt"].(map[string]any)
				resp := map[string]any{"type": "prompt.response", "id": p["id"], "accept": true, "save": true}
				if p["kind"] != "hostkey" {
					resp["values"], resp["save"] = []string{"test"}, false
				}
				e.send(resp)
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
	e.wait("hello", func(m map[string]any) bool { return m["type"] == "hello" })
	return e
}

func (e *events) send(v any) {
	b, _ := json.Marshal(v)
	_ = e.ws.Write(context.Background(), websocket.MessageText, b)
}

// wait returns the first collected message matching pred (within 20 s).
func (e *events) wait(what string, pred func(map[string]any) bool) map[string]any {
	e.t.Helper()
	deadline := time.After(20 * time.Second)
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
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			e.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func monitorStats(sessionID string) func(map[string]any) bool {
	return func(m map[string]any) bool {
		st, ok := m["stats"].(map[string]any)
		return m["type"] == "monitor" && m["sessionId"] == sessionID && ok && st["warmup"] != true
	}
}

func TestLocalHostEndpoints(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	if st := env.Client().JSON("GET", "/api/monitor/local", nil, nil); st != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", st)
	}
	var info struct {
		Host struct {
			Hostname string `json:"hostname"`
			OS       string `json:"os"`
		} `json:"host"`
		CPU struct {
			LogicalCores int `json:"logicalCores"`
		} `json:"cpu"`
		Mem   struct{ Total int64 } `json:"mem"`
		Disks []any                 `json:"disks"`
		Net   []any                 `json:"net"`
		Top   []struct{ PID int }   `json:"topProcesses"`
		Srv   struct{ PID int }     `json:"server"`
	}
	admin.MustJSON("GET", "/api/monitor/local", nil, &info)
	if info.Host.Hostname == "" || info.Host.OS == "" || info.CPU.LogicalCores < 1 || info.Mem.Total <= 0 || len(info.Disks) == 0 ||
		len(info.Net) == 0 || len(info.Top) == 0 || info.Srv.PID != os.Getpid() {
		t.Fatalf("system info: %+v", info)
	}
	var procs []struct {
		PID     int    `json:"pid"`
		Command string `json:"command"`
	}
	admin.MustJSON("GET", "/api/monitor/local/processes", nil, &procs)
	self := false
	for _, p := range procs {
		self = self || p.PID == os.Getpid()
	}
	if !self {
		t.Fatalf("own process missing from %d processes", len(procs))
	}
	var snap struct {
		Mem      struct{ Total int64 } `json:"mem"`
		Platform string                `json:"platform"`
	}
	admin.MustJSON("GET", "/api/monitor/local/snapshot", nil, &snap)
	if snap.Mem.Total <= 0 || snap.Platform != runtime.GOOS {
		t.Fatalf("snapshot: %+v", snap)
	}

	// Kill: validation, self-protection, and a real signal to a child process.
	for body, want := range map[string]int{`{"pid":1}`: 400, fmt.Sprintf(`{"pid":%d}`, os.Getpid()): 403, `{"pid":99999999,"signal":"FOO"}`: 400} {
		if st, _ := admin.ErrorCode("POST", "/api/monitor/local/kill", body); st != want {
			t.Fatalf("kill %s: %d, want %d", body, st, want)
		}
	}
	if runtime.GOOS != "windows" {
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		admin.MustJSON("POST", "/api/monitor/local/kill", map[string]any{"pid": cmd.Process.Pid, "signal": "TERM"}, nil)
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "terminated") {
				t.Fatalf("child: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatal("child not terminated")
		}
		if st, code := admin.ErrorCode("POST", "/api/monitor/local/kill", map[string]any{"pid": cmd.Process.Pid}); st != 404 || code != "not_found" {
			t.Fatalf("kill gone process: %d %s", st, code)
		}
	}

	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(dir+"/sub/f", make([]byte, 64<<10), 0o644)
	if runtime.GOOS != "windows" {
		var du struct {
			Total   int64 `json:"total"`
			Entries []struct {
				Name string `json:"name"`
			} `json:"entries"`
		}
		admin.MustJSON("GET", "/api/monitor/local/du?path="+dir, nil, &du)
		if du.Total <= 0 || len(du.Entries) == 0 || du.Entries[0].Name != "sub" {
			t.Fatalf("du: %+v", du)
		}
	}
	var svc struct {
		Manager  string `json:"manager"`
		Services []any  `json:"services"`
	}
	admin.MustJSON("GET", "/api/monitor/local/services", nil, &svc)
	if svc.Services == nil {
		t.Fatal("services must be a list")
	}
	if st, _ := admin.ErrorCode("GET", "/api/monitor/local/ssh-info", nil); st != 400 {
		t.Fatalf("ssh-info on local: %d", st)
	}
	if st, _ := admin.ErrorCode("GET", "/api/monitor/aaaaaaaaaaaaaaaaaaaa/processes", nil); st != 404 {
		t.Fatalf("unknown session: %d", st)
	}
	if st, _ := admin.ErrorCode("GET", "/api/monitor/local/du?path=relative", nil); st != 400 {
		t.Fatalf("relative du path: %d", st)
	}
	if st, _ := admin.ErrorCode("POST", "/api/monitor/local/services/bad;name/start", nil); st != 400 {
		t.Fatalf("bad unit name: %d", st)
	}

	// Caffeine: status for everyone; toggling in desktop mode.
	var caf struct {
		Supported bool `json:"supported"`
		Allowed   bool `json:"allowed"`
		Enabled   bool `json:"enabled"`
	}
	admin.MustJSON("GET", "/api/system/caffeine", nil, &caf)
	if !caf.Allowed {
		t.Fatal("caffeine not allowed in desktop mode")
	}
	if caf.Supported && os.Getenv("NEXTERM_TEST_CAFFEINE") == "1" {
		admin.MustJSON("POST", "/api/system/caffeine", map[string]any{"enabled": true, "durationMin": 1}, &caf)
		if !caf.Enabled {
			t.Fatal("caffeine not enabled")
		}
		admin.MustJSON("POST", "/api/system/caffeine", map[string]any{"enabled": false}, &caf)
		if caf.Enabled {
			t.Fatal("caffeine still enabled")
		}
	}
	if st, _ := admin.ErrorCode("POST", "/api/system/caffeine", map[string]any{"enabled": true, "durationMin": -1}); st != 400 {
		t.Fatalf("bad duration: %d", st)
	}
}

func TestServerModeGating(t *testing.T) {
	env := servertest.New(t, func(c *config.Config) { c.Mode = config.ModeServer })
	admin := env.Setup("admin", adminPass)
	user := env.CreateUser(admin, "bob", "bob correct horse battery", "user")
	if st, _ := user.ErrorCode("GET", "/api/monitor/local", nil); st != 403 {
		t.Fatalf("user system info: %d", st)
	}
	if st, _ := user.ErrorCode("GET", "/api/monitor/local/processes", nil); st != 403 {
		t.Fatalf("user local processes: %d", st)
	}
	if st, _ := user.ErrorCode("POST", "/api/system/caffeine", map[string]any{"enabled": true}); st != 403 {
		t.Fatalf("user caffeine: %d", st)
	}
	var caf struct {
		Allowed bool `json:"allowed"`
	}
	user.MustJSON("GET", "/api/system/caffeine", nil, &caf)
	if caf.Allowed {
		t.Fatal("caffeine allowed for a user in server mode")
	}
	if st := admin.JSON("GET", "/api/monitor/local", nil, nil); st != 200 {
		t.Fatalf("admin system info: %d", st)
	}
}

func TestLocalSessionMonitoring(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local shell test uses a POSIX shell")
	}
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	ev := dialEvents(t, env, admin)
	var sess struct {
		ID string `json:"id"`
	}
	admin.MustJSON("POST", "/api/sessions", map[string]any{"quick": map[string]any{"protocol": "local"}, "cols": 80, "rows": 24}, &sess)
	t.Cleanup(func() { admin.JSON("DELETE", "/api/sessions/"+sess.ID, nil, nil) })
	ev.send(map[string]any{"type": "subscribe", "topic": "monitor", "sessionId": sess.ID})
	m := ev.wait("local monitor stats", monitorStats(sess.ID))
	st := m["stats"].(map[string]any)
	if st["platform"] != runtime.GOOS || st["mem"].(map[string]any)["total"].(float64) <= 0 {
		t.Fatalf("stats: %v", st)
	}
	var procs []any
	admin.MustJSON("GET", "/api/monitor/"+sess.ID+"/processes", nil, &procs)
	if len(procs) == 0 {
		t.Fatal("no processes")
	}
	// Another user cannot watch it.
	other := env.CreateUser(admin, "eve", "eve correct horse battery", "user")
	oev := dialEvents(t, env, other)
	oev.send(map[string]any{"type": "subscribe", "topic": "monitor", "sessionId": sess.ID})
	oev.wait("subscribe error", func(m map[string]any) bool { return m["type"] == "subscribe.error" })
	if st, _ := other.ErrorCode("GET", "/api/monitor/"+sess.ID+"/processes", nil); st != 404 {
		t.Fatalf("foreign session: %d", st)
	}
	// Closing the session tells the subscribers.
	admin.MustJSON("DELETE", "/api/sessions/"+sess.ID, nil, nil)
	ev.wait("closed state", func(m map[string]any) bool {
		return m["type"] == "monitor" && m["sessionId"] == sess.ID && m["state"] == "closed"
	})
}

// TestSSHSessionMonitoring runs the whole stack against the Docker test environment: a saved SSH connection, the
// monitor topic over /ws/events, REST endpoints (sudo through the login password), and the log-follow WebSocket.
func TestSSHSessionMonitoring(t *testing.T) {
	if os.Getenv("NEXTERM_TESTENV") != "1" {
		t.Skip("set NEXTERM_TESTENV=1 to run tests against the Docker test environment")
	}
	env := servertest.New(t)
	admin := env.Setup("admin", adminPass)
	ev := dialEvents(t, env, admin)
	var conn struct {
		ID string `json:"id"`
	}
	admin.MustJSON("POST", "/api/connections", map[string]any{"name": "ssh1", "protocol": "ssh", "host": "127.0.0.1", "port": 22022,
		"username": "test", "authMethod": "password", "secrets": map[string]string{"password": "test"}}, &conn)
	var sess struct {
		ID string `json:"id"`
	}
	admin.MustJSON("POST", "/api/sessions", map[string]any{"connectionId": conn.ID, "cols": 100, "rows": 30}, &sess)
	t.Cleanup(func() { admin.JSON("DELETE", "/api/sessions/"+sess.ID, nil, nil) })

	// Subscribing before the connection is up is fine: the feed waits for it.
	ev.send(map[string]any{"type": "subscribe", "topic": "monitor", "sessionId": sess.ID})
	m := ev.wait("ssh monitor stats", monitorStats(sess.ID))
	st := m["stats"].(map[string]any)
	if st["hostname"] != "ssh1" || st["platform"] != "linux" || st["cpu"].(map[string]any)["cores"].(float64) < 1 {
		t.Fatalf("stats: %v", st)
	}
	base := "/api/monitor/" + sess.ID
	var host struct {
		Platform string `json:"platform"`
		OS       string `json:"os"`
	}
	admin.MustJSON("GET", base+"/host", nil, &host)
	if host.Platform != "linux" || !strings.Contains(host.OS, "Alpine") {
		t.Fatalf("host: %+v", host)
	}
	var snap struct {
		Hostname string `json:"hostname"`
	}
	admin.MustJSON("GET", base+"/snapshot", nil, &snap)
	if snap.Hostname != "ssh1" {
		t.Fatalf("snapshot: %+v", snap)
	}
	var procs []struct {
		PID     int    `json:"pid"`
		User    string `json:"user"`
		Command string `json:"command"`
	}
	admin.MustJSON("GET", base+"/processes", nil, &procs)
	sshd := false
	for _, p := range procs {
		sshd = sshd || strings.Contains(p.Command, "sshd")
	}
	if !sshd {
		t.Fatalf("no sshd among %d processes", len(procs))
	}
	var info struct {
		Kex        string  `json:"kex"`
		Cipher     string  `json:"cipher"`
		ProbeMs    float64 `json:"probeMs"`
		Host       string  `json:"host"`
		RemoteAddr string  `json:"remoteAddr"`
		User       string  `json:"user"`
	}
	admin.MustJSON("GET", base+"/ssh-info", nil, &info)
	if info.Kex == "" || info.Cipher == "" || info.ProbeMs <= 0 || info.Host != "127.0.0.1" || info.User != "test" || info.RemoteAddr == "" {
		t.Fatalf("ssh-info: %+v", info)
	}

	// Session history (SSH-38 / MON-4): a manual reconnect is counted, the terminal stream's bytes too.
	admin.MustJSON("POST", "/api/sessions/"+sess.ID+"/reconnect", nil, nil)
	var hist struct {
		Reconnects       int    `json:"reconnects"`
		FirstConnectedAt string `json:"firstConnectedAt"`
		TermBytesIn      int64  `json:"termBytesIn"`
		TermBytesOut     int64  `json:"termBytesOut"`
		HostCert         *bool  `json:"hostCert"`
	}
	for deadline := time.Now().Add(20 * time.Second); ; {
		if st := admin.JSON("GET", base+"/ssh-info", nil, &hist); st == http.StatusOK && hist.Reconnects == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reconnect not counted: %+v", hist)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if hist.FirstConnectedAt == "" || hist.TermBytesIn <= 0 || hist.HostCert == nil || *hist.HostCert {
		t.Fatalf("session history: %+v", hist)
	}

	// sudo: /root is unreadable for "test"; with sudo (the login password is tried) it is not.
	type duResp struct {
		Total   int64 `json:"total"`
		Partial bool  `json:"partial"`
	}
	var plain, elevated duResp
	st1, _ := admin.ErrorCode("GET", base+"/du?path=/root", nil)
	if st1 == 200 {
		admin.MustJSON("GET", base+"/du?path=/root", nil, &plain)
		if !plain.Partial {
			t.Fatalf("unprivileged du of /root: %+v", plain)
		}
	} else if st1 != 403 {
		t.Fatalf("unprivileged du of /root: %d", st1)
	}
	admin.MustJSON("GET", base+"/du?path=/root&sudo=1", nil, &elevated)
	if elevated.Partial || elevated.Total <= 0 {
		t.Fatalf("sudo du of /root: %+v", elevated)
	}
	var ports []struct {
		Port int `json:"port"`
		PID  int `json:"pid"`
	}
	// (Process details under sudo depend on CAP_SYS_PTRACE, which Docker containers lack: only the port is checked.)
	admin.MustJSON("GET", base+"/ports?sudo=1", nil, &ports)
	found := false
	for _, p := range ports {
		found = found || p.Port == 2222
	}
	if !found {
		t.Fatalf("sudo ports: %+v", ports)
	}

	// Log following over the WebSocket.
	raw, err := ssh.Dial("tcp", "127.0.0.1:22022", &ssh.ClientConfig{User: "test", Auth: []ssh.AuthMethod{ssh.Password("test")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	run := func(cmd string) string {
		s, err := raw.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		out, err := s.Output(cmd)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		return strings.TrimSpace(string(out))
	}
	file := run("f=$(mktemp); echo hello-from-log >> $f; echo $f")
	defer run("rm -f " + file)
	wsURL := "ws" + strings.TrimPrefix(env.URL("/ws/monitor/"+sess.ID+"/tail?lines=5&path="+file), "http")
	tail, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{HTTPClient: admin.HTTP})
	if err != nil {
		t.Fatal(err)
	}
	defer tail.CloseNow()
	readUntil := func(want string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for {
			_, data, err := tail.Read(ctx)
			if err != nil {
				t.Fatalf("waiting for %q: %v", want, err)
			}
			if strings.Contains(string(data), want) {
				return
			}
		}
	}
	readUntil(`"type":"start"`)
	readUntil("hello-from-log")
	run("echo second-line >> " + file)
	readUntil("second-line")
	_ = tail.Close(websocket.StatusNormalClosure, "")

	if st, _ := admin.ErrorCode("POST", base+"/kill", map[string]any{"pid": 1, "signal": "TERM"}); st != 400 {
		t.Fatalf("kill pid 1: %d", st)
	}
	var audit []struct {
		Action string `json:"action"`
	}
	admin.MustJSON("GET", "/api/audit/me?action=monitor.", nil, &audit)
	actions := map[string]bool{}
	for _, a := range audit {
		actions[a.Action] = true
	}
	if !actions["monitor.tail"] || !actions["monitor.kill"] {
		t.Fatalf("audit: %v", actions)
	}
}
