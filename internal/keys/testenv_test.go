package keys

// Integration tests against the shared Docker lab (scripts/testenv): the real OpenSSH server "ssh1". Run with
//
//	TERMSTEAD_TESTENV=1 go test ./internal/keys/ -run TestLab -v
//
// TERMSTEAD_TESTENV_SSH1 overrides its address (default 127.0.0.1:22022, user test / password test). The keys the
// tests install are removed from the server's authorized_keys afterwards.

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/termstead/termstead/internal/config"
	"github.com/termstead/termstead/internal/model"
)

const (
	labUser     = "test"
	labPassword = "test"
)

// labSSH1 returns the address of the lab's ssh1 server, skipping the test unless TERMSTEAD_TESTENV=1.
func labSSH1(t *testing.T) (string, int) {
	t.Helper()
	if os.Getenv("TERMSTEAD_TESTENV") != "1" {
		t.Skip("set TERMSTEAD_TESTENV=1 to run tests against the Docker test environment")
	}
	addr := cmp.Or(os.Getenv("TERMSTEAD_TESTENV_SSH1"), "127.0.0.1:22022")
	host, ps, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("the lab's ssh1 (%s) is not reachable: %v", addr, err)
	}
	c.Close()
	return host, port
}

// labAdmin opens a password session on the lab server (test setup and cleanup, outside Termstead).
func labAdmin(t *testing.T, host string, port int) *ssh.Client {
	t.Helper()
	cfg := &ssh.ClientConfig{User: labUser, Auth: []ssh.AuthMethod{ssh.Password(labPassword)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second} // #nosec G106 -- disposable lab container
	c, err := ssh.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)), cfg)
	if err != nil {
		t.Fatalf("lab login: %v", err)
	}
	return c
}

// removeLabKeys drops the lines authorizing keys from the lab user's ~/.ssh/authorized_keys (atomic replace).
func removeLabKeys(t *testing.T, host string, port int, keys ...ssh.PublicKey) {
	t.Helper()
	c := labAdmin(t, host, port)
	defer c.Close()
	sc, err := sftp.NewClient(c)
	if err != nil {
		t.Errorf("cleanup: %v", err)
		return
	}
	defer sc.Close()
	const path = ".ssh/authorized_keys"
	f, err := sc.Open(path)
	if err != nil {
		t.Errorf("cleanup: %v", err)
		return
	}
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		t.Errorf("cleanup: %v", err)
		return
	}
	var kept []string
	removed := 0
	for line := range strings.Lines(string(data)) {
		if pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err == nil && containsBlob(keys, pk) {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	if removed == 0 {
		return
	}
	tmp := fmt.Sprintf("%s.termstead-keys-%s", path, randomHex(4))
	w, err := sc.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err == nil {
		_ = w.Chmod(0o600)
		_, err = w.Write([]byte(strings.Join(kept, "")))
		if cerr := w.Close(); err == nil {
			err = cerr
		}
	}
	if err == nil {
		err = sc.PosixRename(tmp, path)
	}
	if err != nil {
		_ = sc.Remove(tmp)
		t.Errorf("cleanup of %s on the lab server: %v", path, err)
	}
}

func containsBlob(keys []ssh.PublicKey, k ssh.PublicKey) bool {
	for _, c := range keys {
		if bytes.Equal(c.Marshal(), k.Marshal()) {
			return true
		}
	}
	return false
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// run executes cmd on an SSH client and returns its trimmed output.
func run(t *testing.T, c *ssh.Client, cmd string) string {
	t.Helper()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out, err := s.CombinedOutput(cmd)
	if err != nil {
		t.Fatalf("%s: %v (%s)", cmd, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestLabInstallAndAgent installs generated keys on a real OpenSSH server (ssh-copy-id semantics), logs in with them
// through Termstead's SSH core and through the built-in agent socket (rsa-sha2 signatures, agent forwarding), and
// checks the trusted host key round trip through the known hosts export / import.
func TestLabInstallAndAgent(t *testing.T) {
	host, port := labSSH1(t)
	agentSocketTest(t)
	e := newTestEnv(t, config.ModeDesktop)
	p := e.c.prompter(t)
	p.set(answer(true, nil, true)) // trust (and save) the lab's host key

	tag := "termstead-keys-lab-" + randomHex(4)
	var ed, rsaKey keyJSON
	e.c.must("POST", "/api/keys/generate", map[string]any{"name": "lab ed25519", "comment": tag}, &ed)
	e.c.must("POST", "/api/keys/generate", map[string]any{"name": "lab rsa", "type": "rsa", "bits": 3072, "comment": tag,
		"passphrase": "lab passphrase", "rememberPassphrase": true}, &rsaKey)
	edPub, rsaPub := mustParsePub(t, ed.PublicKey), mustParsePub(t, rsaKey.PublicKey)
	t.Cleanup(func() { removeLabKeys(t, host, port, edPub, rsaPub) })

	conn := &model.Connection{OwnerID: e.admin.ID, Name: "lab ssh1", Protocol: model.ProtoSSH, Host: host, Port: port,
		Username: labUser, AuthMethod: model.AuthPassword, Options: model.Options{}}
	conn.Normalize()
	e.saveConn(t, conn, map[string]string{"password": labPassword})

	// Install (SFTP), then again: already present.
	for _, k := range []keyJSON{ed, rsaKey} {
		var res installResult
		e.c.must("POST", "/api/keys/"+k.ID+"/install", map[string]any{"connectionId": conn.ID}, &res)
		if !res.Installed || res.AlreadyPresent || res.Method != "sftp" || !strings.HasSuffix(res.Path, "/.ssh/authorized_keys") {
			t.Fatalf("install %s: %+v", k.Name, res)
		}
		e.c.must("POST", "/api/keys/"+k.ID+"/install", map[string]any{"connectionId": conn.ID}, &res)
		if res.Installed || !res.AlreadyPresent {
			t.Fatalf("second install %s: %+v", k.Name, res)
		}
	}
	admin := labAdmin(t, host, port)
	defer admin.Close()
	if mode := run(t, admin, "stat -c %a ~/.ssh/authorized_keys 2>/dev/null || stat -f %Lp ~/.ssh/authorized_keys"); mode != "600" {
		t.Fatalf("authorized_keys mode %s", mode)
	}
	if n := run(t, admin, "grep -c "+tag+" ~/.ssh/authorized_keys"); n != "2" {
		t.Fatalf("authorized_keys lines with the tag: %s", n)
	}

	// The stored keys log in through Termstead's SSH core (public-key authentication, the encrypted key with its
	// remembered passphrase).
	for _, k := range []keyJSON{ed, rsaKey} {
		kc := &model.Connection{OwnerID: e.admin.ID, Name: "lab " + k.Name, Protocol: model.ProtoSSH, Host: host, Port: port,
			Username: labUser, KeyID: k.ID, AuthMethod: model.AuthKey, Options: model.Options{}}
		kc.Normalize()
		e.saveConn(t, kc, nil)
		cl, rel, err := e.core.SSH.Acquire(ctxT(t), e.admin, kc, nil)
		if err != nil {
			t.Fatalf("login with %s: %v", k.Name, err)
		}
		if who := run(t, cl.Client, "id -un"); who != labUser {
			t.Fatalf("id -un: %q", who)
		}
		rel()
	}

	// The trusted host key: saved at the first connection; the export is a valid known_hosts file that a real
	// OpenSSH client check accepts, and importing it again changes nothing.
	var hosts []model.KnownHost
	e.c.must("GET", "/api/known-hosts", nil, &hosts)
	if len(hosts) == 0 {
		t.Fatal("the lab's host key was not saved")
	}
	st, exported, _ := e.c.do("GET", "/api/known-hosts/export", nil)
	if st != 200 || !strings.Contains(string(exported), fmt.Sprintf("[%s]:%d ", host, port)) {
		t.Fatalf("export %d: %s", st, exported)
	}
	var imp knownHostsImportResult
	e.c.must("POST", "/api/known-hosts/import", map[string]any{"text": string(exported)}, &imp)
	if imp.Added != 0 || imp.Skipped != len(hosts) {
		t.Fatalf("re-import %+v", imp)
	}
	hostKeyCheck := func(_ string, _ net.Addr, key ssh.PublicKey) error {
		for _, h := range hosts {
			if h.Fingerprint == ssh.FingerprintSHA256(key) {
				return nil
			}
		}
		return fmt.Errorf("host key %s is not trusted", ssh.FingerprintSHA256(key))
	}

	// The built-in agent: OpenSSH accepts its signatures (RSA as rsa-sha2-*: OpenSSH ≥ 8.8 refuses ssh-rsa).
	var ast agentStatus
	e.c.must("POST", "/api/agent/start", nil, &ast)
	t.Cleanup(func() { os.Remove(filepath.Dir(ast.SocketPath)) })
	ac := dialAgent(t, ast.SocketPath)
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	for i, pub := range []ssh.PublicKey{edPub, rsaPub} {
		only := func() ([]ssh.Signer, error) {
			ss, err := ac.Signers()
			var out []ssh.Signer
			for _, s := range ss {
				if bytes.Equal(s.PublicKey().Marshal(), pub.Marshal()) {
					out = append(out, s)
				}
			}
			return out, err
		}
		c, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{User: labUser, Auth: []ssh.AuthMethod{ssh.PublicKeysCallback(only)},
			HostKeyCallback: hostKeyCheck, Timeout: 10 * time.Second})
		if err != nil {
			t.Fatalf("agent login with %s: %v", pub.Type(), err)
		}
		if who := run(t, c, "id -un"); who != labUser {
			t.Fatalf("id -un: %q", who)
		}
		if i == 0 {
			// ssh -A: the remote side lists the agent's keys through agent forwarding.
			if err := agent.ForwardToAgent(c, ac); err != nil {
				t.Fatal(err)
			}
			s, err := c.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			if err := agent.RequestAgentForwarding(s); err != nil {
				t.Fatal(err)
			}
			out, err := s.CombinedOutput("ssh-add -L")
			s.Close()
			if err != nil {
				t.Fatalf("ssh-add -L on the lab server: %v (%s)", err, out)
			}
			if !strings.Contains(string(out), strings.Fields(ed.PublicKey)[1]) || !strings.Contains(string(out), strings.Fields(rsaKey.PublicKey)[1]) {
				t.Fatalf("forwarded agent keys:\n%s", out)
			}
		}
		c.Close()
	}

	// Confirm before use: a refused confirmation fails the login.
	e.setKeysSettings(t, e.admin.ID, map[string]any{"agentConfirm": true})
	p.set(answer(false, nil, false))
	if c, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{User: labUser, Auth: []ssh.AuthMethod{ssh.PublicKeysCallback(ac.Signers)},
		HostKeyCallback: hostKeyCheck, Timeout: 10 * time.Second}); err == nil {
		c.Close()
		t.Fatal("logged in although the confirmation was refused")
	}
	if ps := p.prompts(); len(ps) == 0 || ps[0].Kind != model.PromptConfirm {
		t.Fatalf("prompts %q", kinds(ps))
	}
	e.setKeysSettings(t, e.admin.ID, map[string]any{})
}
