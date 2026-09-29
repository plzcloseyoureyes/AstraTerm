package servers

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// TestTestEnvRemoteClients drives the embedded servers from a real remote machine: the shared Docker test
// environment's ssh1 container (curl, nc, OpenSSH sftp) connects back to Termstead through host.docker.internal.
// Run with TERMSTEAD_TESTENV=1 (ssh1 on 127.0.0.1:22022, key scripts/testenv/keys/id_ed25519).
func TestTestEnvRemoteClients(t *testing.T) {
	if os.Getenv("TERMSTEAD_TESTENV") != "1" {
		t.Skip("set TERMSTEAD_TESTENV=1 to run against the shared Docker test environment")
	}
	keyPEM, err := os.ReadFile(filepath.Join("..", "..", "scripts", "testenv", "keys", "id_ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := ssh.Dial("tcp", "127.0.0.1:22022", &ssh.ClientConfig{User: "test", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second}) //nolint:gosec // throwaway test target
	if err != nil {
		t.Fatalf("ssh1: %v", err)
	}
	defer remote.Close()
	run := func(cmd string) string {
		t.Helper()
		sess, err := remote.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		var out, errb bytes.Buffer
		sess.Stdout, sess.Stderr = &out, &errb
		if err := sess.Run(cmd); err != nil {
			t.Fatalf("%s: %v (stderr %q, stdout %q)", cmd, err, errb.String(), out.String())
		}
		return out.String()
	}
	const host = "host.docker.internal"

	h := newHarness(t)
	u := h.user("admin", true)
	root := h.shareDir()

	// HTTP: download and PUT upload from the container.
	hp := freePort(t, "tcp")
	h.configure(u, KindHTTP, map[string]any{"root": root, "port": hp, "readOnly": false})
	h.start(u, KindHTTP)
	if got := run(fmt.Sprintf("curl -sf http://%s:%d/hello.txt", host, hp)); got != "hello world\n" {
		t.Fatalf("http download %q", got)
	}
	run(fmt.Sprintf("printf 'from-container' | curl -sf -T - http://%s:%d/sub/remote.txt", host, hp))
	if b, _ := os.ReadFile(filepath.Join(root, "sub", "remote.txt")); string(b) != "from-container" {
		t.Fatalf("http upload %q", b)
	}

	// Escapes from a real client (curl --path-as-is sends the dots unnormalized).
	for _, p := range []string{"/../secret.txt", "/sub/../../secret.txt", "/%2e%2e/secret.txt", "/..%2fsecret.txt"} {
		if got := run(fmt.Sprintf("curl -s --path-as-is -L http://%s:%d%s; true", host, hp, p)); strings.Contains(got, "top secret") {
			t.Fatalf("http escape through %s", p)
		}
	}

	// FTP (EPSV passive through the NAT) with a user.
	fp := freePort(t, "tcp")
	h.configure(u, KindFTP, map[string]any{"root": root, "port": fp, "tls": "off",
		"users": []map[string]any{{"username": "alice", "password": "alice-pass"}}})
	h.start(u, KindFTP)
	if got := run(fmt.Sprintf("curl -sf --user alice:alice-pass ftp://%s:%d/sub/nested.txt", host, fp)); got != "nested\n" {
		t.Fatalf("ftp download %q", got)
	}
	run(fmt.Sprintf("printf 'ftp-upload' | curl -sf --user alice:alice-pass -T - ftp://%s:%d/ftp-up.txt", host, fp))
	if b, _ := os.ReadFile(filepath.Join(root, "ftp-up.txt")); string(b) != "ftp-upload" {
		t.Fatalf("ftp upload %q", b)
	}

	// TFTP in single-port mode (NAT friendly).
	tp := freePort(t, "udp")
	h.configure(u, KindTFTP, map[string]any{"root": root, "port": tp, "singlePort": true})
	h.start(u, KindTFTP)
	if got := run(fmt.Sprintf("curl -sf --max-time 10 tftp://%s:%d/hello.txt", host, tp)); got != "hello world\n" {
		t.Fatalf("tftp download %q", got)
	}

	// SFTP with a public key, using the container's OpenSSH client.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	spk, _ := ssh.NewPublicKey(pub)
	block, _ := ssh.MarshalPrivateKey(priv, "")
	sp := freePort(t, "tcp")
	h.configure(u, KindSFTP, map[string]any{"root": root, "port": sp,
		"users": []map[string]any{{"username": "carol", "publicKeys": []string{string(ssh.MarshalAuthorizedKey(spk))}}}})
	h.start(u, KindSFTP)
	keyPath := "/tmp/termstead-servers-test-key"
	sess, _ := remote.NewSession()
	sess.Stdin = bytes.NewReader(pem.EncodeToMemory(block))
	if err := sess.Run("umask 077 && cat > " + keyPath); err != nil {
		t.Fatal(err)
	}
	sess.Close()
	defer run("rm -f " + keyPath)
	out := run(fmt.Sprintf("printf 'ls /\\nget /hello.txt /tmp/termstead-sftp-hello\\n' | sftp -q -b - -i %s -P %d "+
		"-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null carol@%s && cat /tmp/termstead-sftp-hello && "+
		"rm -f /tmp/termstead-sftp-hello", keyPath, sp, host))
	if !strings.Contains(out, "hello.txt") || !strings.HasSuffix(out, "hello world\n") {
		t.Fatalf("sftp output %q", out)
	}
	// Port forwarding is refused: stdio forwarding (direct-tcpip) to the container's own sshd fails, no banner.
	fwd := run(fmt.Sprintf("ssh -i %s -p %d -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes "+
		"-o ConnectTimeout=5 -W 127.0.0.1:2222 carol@%s </dev/null 2>&1; echo rc=$?", keyPath, sp, host))
	if strings.Contains(fwd, "SSH-2.0") || strings.Contains(fwd, "rc=0") {
		t.Fatalf("direct-tcpip forwarding allowed: %q", fwd)
	}
	t.Logf("forwarding attempt: %q", strings.TrimSpace(fwd))
	// Symlinks escaping the jail cannot be created from a real client.
	sl := run(fmt.Sprintf("printf 'symlink ../../etc /esc\\nsymlink /etc /abs\\n' | sftp -b - -i %s -P %d "+
		"-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null carol@%s 2>&1; true", keyPath, sp, host))
	for _, name := range []string{"esc", "abs"} {
		if _, err := os.Lstat(filepath.Join(root, name)); err == nil {
			t.Fatalf("escaping symlink %s created (%q)", name, sl)
		}
	}
	t.Logf("symlink attempts: %q", strings.TrimSpace(sl))

	// Syslog over UDP and TCP from the container.
	lp := freePort(t, "both")
	h.configure(u, KindSyslog, map[string]any{"port": lp})
	h.start(u, KindSyslog)
	run(fmt.Sprintf("printf '<14>Oct 11 22:14:15 ssh1 app: udp from the lab' | nc -u -w1 %s %d; "+
		"printf '<11>1 - ssh1 tcpapp - - - tcp from the lab\\n' | nc -w1 %s %d; true", host, lp, host, lp))
	var page SyslogPage
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.must(u, http.MethodGet, "/api/servers/syslog/messages?q=from+the+lab", nil, &page, http.StatusOK)
		if len(page.Messages) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(page.Messages) != 2 {
		t.Fatalf("syslog messages from the lab: %+v", page.Messages)
	}
	for _, m := range page.Messages {
		if m.Hostname != "ssh1" {
			t.Errorf("host %q", m.Hostname)
		}
	}
	t.Logf("remote clients OK (http %d, ftp %d, tftp %d, sftp %d, syslog %s)", hp, fp, tp, sp, strconv.Itoa(lp))
}
