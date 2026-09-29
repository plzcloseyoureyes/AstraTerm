package automation

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/sshx"
)

// Integration tests against the shared Docker test environment (scripts/testenv): enable with ASTRATERM_TESTENV=1.
// ssh1 listens on 127.0.0.1:22022 (test/test).

const testenvSSH = "127.0.0.1:22022"

func requireTestenv(t *testing.T) {
	t.Helper()
	if os.Getenv("ASTRATERM_TESTENV") != "1" {
		t.Skip("set ASTRATERM_TESTENV=1 to run tests against the shared Docker test environment")
	}
	c, err := net.DialTimeout("tcp", testenvSSH, 2*time.Second)
	if err != nil {
		t.Skipf("test environment unreachable: %v", err)
	}
	c.Close()
}

// trustHost records the host key of addr as known (the tests have no browser to answer the host-key prompt).
func (h *harness) trustHost(host string, port int) {
	h.t.Helper()
	var key ssh.PublicKey
	cfg := &ssh.ClientConfig{User: "probe", Timeout: 5 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			key = k
			return errStopProbe
		}}
	_, err := ssh.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)), cfg)
	if key == nil {
		h.t.Fatalf("cannot read the host key of %s:%d: %v", host, port, err)
	}
	kh := &model.KnownHost{Host: strings.ToLower(host), Port: port, KeyType: key.Type(),
		PublicKey: sshx.FormatKnownHostKey(key), Fingerprint: ssh.FingerprintSHA256(key)}
	if err := h.d.Store.KnownHosts.Add(context.Background(), kh); err != nil {
		h.t.Fatal(err)
	}
}

type probeErr struct{}

func (probeErr) Error() string { return "probe done" }

var errStopProbe error = probeErr{}

func (h *harness) sshConn(u *model.User, name string, options map[string]any) *model.Connection {
	h.t.Helper()
	opts := model.Options{"useAgent": false}
	for k, v := range options {
		opts[k] = v
	}
	c := &model.Connection{OwnerID: u.ID, Name: name, Protocol: model.ProtoSSH, Host: "127.0.0.1", Port: 22022,
		Username: "test", AuthMethod: model.AuthPassword, Options: opts, Tags: []string{}}
	enc, err := h.d.Vault.SealJSON(map[string]string{"password": "test", "sudoPassword": "test"})
	if err != nil {
		h.t.Fatal(err)
	}
	c.SecretsEnc, c.SecretKeys = enc, []string{"password", "sudoPassword"}
	c.Normalize()
	if err := h.d.Store.Connections.Create(context.Background(), c); err != nil {
		h.t.Fatal(err)
	}
	return c
}

func TestTestenvSSHBatchScriptsAndLogon(t *testing.T) {
	requireTestenv(t)
	h := newHarness(t)
	alice := h.user("alice", model.RoleUser)
	h.trustHost("127.0.0.1", 22022)
	c1 := h.sshConn(alice, "ssh1 exec", nil)
	c2 := h.sshConn(alice, "ssh1 logon", map[string]any{"logonActions": []any{
		map[string]any{"expect": `\$\s*$`, "send": `echo LOGON-SSH-$((20+1))`},
	}})

	// Batch over exec channels: clean output and exit codes.
	var js jobStarted
	h.must(alice, "POST", "/api/automation/batch", map[string]any{"connectionIds": []string{c1.ID}, "kind": "command",
		"command": "echo EXEC-$(id -un) {{host}}; exit 3"}, &js)
	run := h.waitRun(alice, js.RunID)
	if len(run.Results) != 1 || run.Results[0].Mode != ModeExec || run.Results[0].ExitCode == nil || *run.Results[0].ExitCode != 3 ||
		!strings.Contains(run.Results[0].Output, "EXEC-test 127.0.0.1") || run.Status != StatusError {
		t.Fatalf("exec batch %+v", run)
	}
	// Session mode over SSH.
	h.must(alice, "POST", "/api/automation/batch", map[string]any{"connectionIds": []string{c1.ID}, "kind": "command",
		"command": "echo SESS-$((3*3))", "mode": "session"}, &js)
	if run = h.waitRun(alice, js.RunID); run.Status != StatusOK || !strings.Contains(run.Results[0].Output, "SESS-9") {
		t.Fatalf("session batch %+v", run)
	}

	// Logon actions over SSH.
	var rs model.RuntimeSession
	h.must(alice, "POST", "/api/sessions", map[string]any{"connectionId": c2.ID, "cols": 120, "rows": 30}, &rs)
	h.waitOutput(alice, rs.ID, "LOGON-SSH-21")

	// A script driving that session: expect, run, exec, sudo with the stored secret.
	var sc Script
	h.must(alice, "POST", "/api/scripts", map[string]any{"name": "ssh probe", "content": `
		var who = session.exec("id -un");
		log("exec=" + who.stdout.trim() + " code=" + who.code);
		log("run=" + session.run("echo RUN-$((7*6))"));
		session.sendLine("sudo -k; sudo -p 'SUDO-PW:' id -u");
		session.expect(/SUDO-PW:\s*$/, 10000);
		session.sendSecret("sudoPassword");
		var r = session.expect(/^(\d+)\s*$/m, 10000);
		log("uid=" + r.groups[0]);
	`}, &sc)
	h.must(alice, "POST", "/api/scripts/"+sc.ID+"/run", map[string]any{"sessionId": rs.ID}, &js)
	run = h.waitRun(alice, js.RunID)
	if run.Status != StatusOK || !strings.Contains(run.Log, "exec=test code=0") || !strings.Contains(run.Log, "run=RUN-42") ||
		!strings.Contains(run.Log, "uid=0") {
		t.Fatalf("ssh script %+v", run)
	}
	if strings.Contains(h.scrollback(alice, rs.ID), "SUDO-PW:test") {
		t.Fatalf("the sudo password was echoed")
	}

	// Paced send waiting for the prompt, then inject-secret.
	h.must(alice, "POST", "/api/automation/send", map[string]any{"sessionIds": []string{rs.ID},
		"text": "echo PACED-A\necho PACED-B", "waitPrompt": true}, &js)
	h.waitOutput(alice, rs.ID, "PACED-B")

	// Macro over SSH: wait for sudo's prompt, type the stored secret (review addition: waitFor + secret steps).
	var mc Macro
	h.must(alice, "POST", "/api/macros", map[string]any{"name": "sudo macro", "steps": []map[string]any{
		{"data": "sudo -k; sudo -p 'MACRO-PW:' sh -c 'echo MACRO-UID-$(id -u)'\r", "delayMs": 0},
		{"waitFor": `MACRO-PW:\s*$`, "timeoutMs": 10000, "secret": "sudoPassword", "data": "\r", "delayMs": 0},
	}}, &mc)
	h.must(alice, "POST", "/api/macros/"+mc.ID+"/run", map[string]any{"sessionIds": []string{rs.ID}}, &js)
	sb := h.waitOutput(alice, rs.ID, "MACRO-UID-0")
	if strings.Contains(sb, "MACRO-PW:test") {
		t.Fatal("the sudo password typed by the macro was echoed")
	}

	// Command-finished trigger over SSH with an OSC 133 prompt (shell integration emulated in PS1).
	events := h.listen(alice)
	h.must(alice, "POST", "/api/automation/triggers", map[string]any{"name": "ssh failed", "event": "command", "exit": "error",
		"scope":   map[string]any{"connectionIds": []string{c2.ID}},
		"actions": []map[string]any{{"type": "notify", "level": "error"}}}, nil)
	h.input(alice, rs.ID, `PS1='\[\e]133;D;$?\a\e]133;A\a\]mk\$ \[\e]133;B\a\]'`+"\r")
	h.waitOutput(alice, rs.ID, "mk$")
	h.input(alice, rs.ID, "ls /no/such/dir\r")
	waitFor(t, "ssh command trigger", 10*time.Second, func() bool {
		return events.find(func(ev map[string]any) bool {
			return ev["type"] == "automation.trigger" && ev["event"] == "command" && ev["line"] == "ls /no/such/dir" && ev["exitCode"] != nil
		}) != nil
	})
}
