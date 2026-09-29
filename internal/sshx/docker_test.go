package sshx_test

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// Docker integration tests against lscr.io/linuxserver/openssh-server (enable with ASTRATERM_TEST_DOCKER=1). The test
// starts two containers on a private network: a jump host with a published port and an internal-only target.

const sshImage = "lscr.io/linuxserver/openssh-server:latest"

type dockerLab struct {
	net, jump, target string
	jumpPort          int
	keyPEM            []byte
	signer            ssh.Signer
}

func docker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func startDockerLab(t *testing.T) *dockerLab {
	t.Helper()
	if os.Getenv("ASTRATERM_TEST_DOCKER") != "1" {
		t.Skip("set ASTRATERM_TEST_DOCKER=1 to run Docker SSH integration tests")
	}
	if _, err := docker("version"); err != nil {
		t.Skip("docker unavailable")
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	block, _ := ssh.MarshalPrivateKey(priv, "astraterm-test")
	suffix := make([]byte, 4)
	rand.Read(suffix)
	id := hex.EncodeToString(suffix)
	lab := &dockerLab{net: "astraterm-b1-net-" + id, jump: "astraterm-b1-ssh1-" + id, target: "astraterm-b1-ssh2-" + id,
		keyPEM: pem.EncodeToMemory(block), signer: signer}
	t.Cleanup(func() {
		docker("rm", "-f", lab.jump, lab.target)
		docker("network", "rm", lab.net)
	})
	if out, err := docker("network", "create", lab.net); err != nil {
		t.Fatalf("network: %v %s", err, out)
	}
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	run := func(name, alias string, publish bool) {
		args := []string{"run", "-d", "--rm", "--name", name, "--network", lab.net, "--network-alias", alias,
			"-e", "PUID=1000", "-e", "PGID=1000", "-e", "PASSWORD_ACCESS=true", "-e", "USER_NAME=test",
			"-e", "USER_PASSWORD=test", "-e", "PUBLIC_KEY=" + pub, "--hostname", alias}
		if publish {
			args = append(args, "-p", "127.0.0.1::2222")
		}
		if out, err := docker(append(args, sshImage)...); err != nil {
			t.Fatalf("docker run %s: %v %s", name, err, out)
		}
	}
	run(lab.jump, "jumphost", true)
	run(lab.target, "sshtarget", false)
	out, err := docker("port", lab.jump, "2222/tcp")
	if err != nil {
		t.Fatalf("docker port: %v %s", err, out)
	}
	line := strings.Split(out, "\n")[0]
	fmt.Sscanf(line[strings.LastIndex(line, ":")+1:], "%d", &lab.jumpPort)
	lab.waitBanner(t)
	// The image disables TCP forwarding by default; enable it on the jump host.
	if out, err := docker("exec", lab.jump, "sh", "-c",
		"sed -i 's/^AllowTcpForwarding no/AllowTcpForwarding yes/' /config/sshd/sshd_config && "+
			"grep -q '^AllowTcpForwarding yes' /config/sshd/sshd_config && s6-svc -r /run/service/svc-openssh-server"); err != nil {
		t.Fatalf("enable forwarding: %v %s", err, out)
	}
	time.Sleep(time.Second)
	lab.waitBanner(t)
	lab.waitTarget(t)
	return lab
}

func (l *dockerLab) waitBanner(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", l.jumpPort), 2*time.Second)
		if err == nil {
			c.SetReadDeadline(time.Now().Add(3 * time.Second))
			line, _ := bufio.NewReader(c).ReadString('\n')
			c.Close()
			if strings.HasPrefix(line, "SSH-2.0-") {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("jump host sshd did not come up")
}

func (l *dockerLab) waitTarget(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if out, err := docker("exec", l.target, "sh", "-c", "nc -z localhost 2222 && echo up"); err == nil && strings.Contains(out, "up") {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("target sshd did not come up")
}

func TestDockerSSH(t *testing.T) {
	lab := startDockerLab(t)
	a := newAppEnv(t)
	ctxT := func() context.Context {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		t.Cleanup(cancel)
		return c
	}
	jumpConn := func(opts model.Options) *model.Connection {
		if opts == nil {
			opts = model.Options{}
		}
		return &model.Connection{Protocol: model.ProtoSSH, Host: "127.0.0.1", Port: lab.jumpPort, Username: "test",
			AuthMethod: model.AuthAuto, Options: opts}
	}

	t.Run("host key reject then TOFU accept and password prompt", func(t *testing.T) {
		a.setAnswer(func(model.Prompt) model.PromptResponse { return model.PromptResponse{} })
		if _, _, err := a.pool.Acquire(ctxT(), a.user, jumpConn(nil), nil); err == nil || !term.IsPermanent(err) {
			t.Fatalf("rejected host key: %v", err)
		}
		a.setAnswer(accept(true, map[string][]string{model.PromptPassword: {"test"}}))
		c, release, err := a.pool.Acquire(ctxT(), a.user, jumpConn(nil), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if a.kinds() != "hostkey,password" {
			t.Fatalf("prompts %q", a.kinds())
		}
		out, _, code, err := c.Exec(ctxT(), "id -un; hostname")
		if err != nil || code != 0 || !strings.Contains(string(out), "test\njumphost") {
			t.Fatalf("exec %q %d %v", out, code, err)
		}
		info := c.Info()
		if !strings.Contains(info.ServerVersion, "OpenSSH") || info.Kex == "" {
			t.Fatalf("info %+v", info)
		}
		c.Close()
	})

	t.Run("wrong then right password", func(t *testing.T) {
		n := 0
		a.setAnswer(func(p model.Prompt) model.PromptResponse {
			if p.Kind == model.PromptPassword {
				n++
				if n == 1 {
					return model.PromptResponse{Accept: true, Values: []string{"nope"}}
				}
				return model.PromptResponse{Accept: true, Values: []string{"test"}}
			}
			return model.PromptResponse{}
		})
		c, release, err := a.pool.Acquire(ctxT(), a.user, jumpConn(model.Options{"connectTimeoutSec": 30}), nil)
		if err != nil {
			t.Fatalf("%v (%q)", err, a.kinds())
		}
		release()
		c.Close()
		if n != 2 {
			t.Fatalf("password prompts %d", n)
		}
	})

	t.Run("stored key auth", func(t *testing.T) {
		enc, _ := a.Server.Deps.Vault.Seal(lab.keyPEM)
		key := &model.SSHKey{Name: "docker", Type: "ed25519", OwnerID: a.user.ID, PrivateKeyEnc: enc,
			PublicKey: string(ssh.MarshalAuthorizedKey(lab.signer.PublicKey()))}
		if err := a.Server.Deps.Store.Keys.Create(context.Background(), key); err != nil {
			t.Fatal(err)
		}
		conn := a.createConnection(t, map[string]any{"name": "jump-key", "protocol": "ssh", "host": "127.0.0.1",
			"port": lab.jumpPort, "username": "test", "keyId": key.ID, "authMethod": "key"})
		a.setAnswer(accept(false, nil))
		c, release, err := a.pool.Get(ctxT(), a.user, conn.ID)
		if err != nil {
			t.Fatalf("%v (%q)", err, a.kinds())
		}
		defer release()
		if len(a.seen()) != 0 {
			t.Fatalf("unexpected prompts %q", a.kinds())
		}
		out, _, _, err := c.Exec(ctxT(), "echo key-ok")
		if err != nil || strings.TrimSpace(string(out)) != "key-ok" {
			t.Fatalf("exec %q %v", out, err)
		}
	})

	t.Run("jump host to internal target, exec and sftp", func(t *testing.T) {
		hop := a.createConnection(t, map[string]any{"name": "jump", "protocol": "ssh", "host": "127.0.0.1",
			"port": lab.jumpPort, "username": "test", "secrets": map[string]string{"password": "test"}})
		a.setAnswer(accept(true, nil))
		target := &model.Connection{Protocol: model.ProtoSSH, Host: "sshtarget", Port: 2222, Username: "test",
			AuthMethod: model.AuthAuto, Options: model.Options{"jumpHosts": []any{hop.ID}}}
		c, release, err := a.pool.Acquire(ctxT(), a.user, target, map[string]string{"password": "test"})
		if err != nil {
			t.Fatalf("%v (%q)", err, a.kinds())
		}
		defer release()
		out, _, _, err := c.Exec(ctxT(), "hostname")
		if err != nil || strings.TrimSpace(string(out)) != "sshtarget" {
			t.Fatalf("hostname %q %v", out, err)
		}
		sc, err := c.SFTP()
		if err != nil {
			t.Fatal(err)
		}
		wd, _ := sc.Getwd()
		f, err := sc.Create(wd + "/astraterm-sftp.txt")
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(f, "through the jump host")
		f.Close()
		rf, err := sc.Open(wd + "/astraterm-sftp.txt")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rf)
		rf.Close()
		sc.Remove(wd + "/astraterm-sftp.txt")
		if string(data) != "through the jump host" {
			t.Fatalf("sftp read %q", data)
		}
	})

	t.Run("terminal session", func(t *testing.T) {
		a.setAnswer(accept(true, nil))
		s, err := a.Server.Core.Sessions.Create(context.Background(), a.user, term.CreateRequest{
			Connection: jumpConn(model.Options{"env": map[string]any{"LC_ASTRATERM": "yes"}}),
			Secrets:    map[string]string{"password": "test"}, Cols: 100, Rows: 30,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer a.Server.Core.Sessions.Close(s.ID)
		waitFor(t, func() bool { st, _ := s.State(); return st == model.StateConnected }, "connected")
		a.Server.Core.Sessions.Write(s.ID, []byte("echo term-$((6*7)); stty size\r"))
		waitFor(t, func() bool {
			out := string(s.Scrollback())
			return strings.Contains(out, "term-42") && strings.Contains(out, "30 100")
		}, "shell output")
		a.Server.Core.Sessions.Resize(s.ID, 120, 40)
		a.Server.Core.Sessions.Write(s.ID, []byte("stty size; exit 4\r"))
		waitFor(t, func() bool { st, _ := s.State(); return st == model.StateDisconnected }, "exit")
		if !strings.Contains(string(s.Scrollback()), "40 120") {
			t.Fatalf("resize not applied: %q", s.Scrollback())
		}
		if info := s.Info(); info.ExitCode == nil || *info.ExitCode != 4 {
			t.Fatalf("exit code %v", info.ExitCode)
		}
	})

	t.Run("keepalive detects a frozen server", func(t *testing.T) {
		// Keepalive settings apply when a connection is dialed: wait for the pooled clients to go away first.
		waitFor(t, func() bool { return a.pool.Stats() == 0 }, "pool drained")
		a.setAnswer(accept(true, nil))
		c, release, err := a.pool.Acquire(ctxT(), a.user, jumpConn(model.Options{"keepAliveSec": 1}),
			map[string]string{"password": "test"})
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if out, err := docker("pause", lab.jump); err != nil {
			t.Fatalf("pause: %v %s", err, out)
		}
		defer docker("unpause", lab.jump)
		select {
		case <-c.Done():
		case <-time.After(30 * time.Second):
			t.Fatal("frozen server not detected")
		}
		if err := c.Err(); err == nil || !strings.Contains(err.Error(), "keepalive") {
			t.Fatalf("err %v", err)
		}
	})
}
