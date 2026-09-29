package sshx_test

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	socks5 "github.com/things-go/go-socks5"
	"golang.org/x/crypto/ssh"

	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/sshx"
	"github.com/nexterm/nexterm/internal/term"
)

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestPasswordPromptAndHostKeyTOFU(t *testing.T) {
	a := newAppEnv(t)
	srv := startSSHServer(t, serverOpts{Password: "secret", Banner: "Authorized use only\n"})
	a.setAnswer(accept(true, map[string][]string{model.PromptPassword: {"secret"}}))

	c, release, err := a.pool.Acquire(ctx(t), a.user, quickConn(srv, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.kinds(); got != "hostkey,password" {
		t.Fatalf("prompts %q", got)
	}
	hk := a.seen()[0]
	if hk.HostKey == nil || hk.HostKey.Status != model.HostKeyUnknown || hk.HostKey.Fingerprint != ssh.FingerprintSHA256(srv.Key.PublicKey()) ||
		!strings.HasPrefix(hk.HostKey.FingerprintMD5, "MD5:") || !hk.AllowSave || hk.HostKey.Port != srv.Port {
		t.Fatalf("host key prompt %+v %+v", hk, hk.HostKey)
	}
	if pw := a.seen()[1]; pw.AllowSave || len(pw.Fields) != 1 || pw.Fields[0].Echo {
		t.Fatalf("password prompt %+v (quick connections cannot save)", pw)
	}
	known, _ := a.Server.Deps.Store.KnownHosts.Find(context.Background(), srv.Host, srv.Port)
	if len(known) != 1 || known[0].Fingerprint != ssh.FingerprintSHA256(srv.Key.PublicKey()) {
		t.Fatalf("known hosts %+v", known)
	}
	if pk, err := sshx.ParseKnownHostKey(known[0].PublicKey); err != nil || string(pk.Marshal()) != string(srv.Key.PublicKey().Marshal()) {
		t.Fatalf("stored key %q: %v", known[0].PublicKey, err)
	}
	info := c.Info()
	if info.Kex == "" || info.Cipher == "" || info.HostKeyFingerprint != known[0].Fingerprint || !strings.HasPrefix(info.ServerVersion, "SSH-2.0-") {
		t.Fatalf("info %+v", info)
	}

	// Reuse: a second acquire of the same user@host:port shares the client without prompting.
	a.setAnswer(accept(false, nil))
	c2, release2, err := a.pool.Acquire(ctx(t), a.user, quickConn(srv, nil), nil)
	if err != nil || c2 != c || len(a.seen()) != 0 {
		t.Fatalf("reuse: %v same=%v prompts=%q", err, c2 == c, a.kinds())
	}
	release()
	release2()
	release2() // idempotent
	waitFor(t, func() bool { return a.pool.Stats() == 0 && !c.Alive() }, "idle close")

	// Known host now: reconnecting with the stored password asks nothing.
	c3, release3, err := a.pool.Acquire(ctx(t), a.user, quickConn(srv, nil), map[string]string{"password": "secret"})
	if err != nil || len(a.seen()) != 0 {
		t.Fatalf("stored credentials: %v prompts %q", err, a.kinds())
	}
	release3()
	_ = c3
}

func TestHostKeyMismatch(t *testing.T) {
	a := newAppEnv(t)
	srv := startSSHServer(t, serverOpts{Password: "secret"})
	other := newHostKey(t)
	err := a.Server.Deps.Store.KnownHosts.Add(context.Background(), &model.KnownHost{Host: srv.Host, Port: srv.Port,
		KeyType: other.PublicKey().Type(), PublicKey: sshx.FormatKnownHostKey(other.PublicKey()),
		Fingerprint: ssh.FingerprintSHA256(other.PublicKey())})
	if err != nil {
		t.Fatal(err)
	}
	// Reject: permanent error, no password prompt.
	a.setAnswer(func(model.Prompt) model.PromptResponse { return model.PromptResponse{Accept: false} })
	_, _, err = a.pool.Acquire(ctx(t), a.user, quickConn(srv, nil), map[string]string{"password": "secret"})
	if err == nil || !term.IsPermanent(err) || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("err %v (permanent=%v)", err, term.IsPermanent(err))
	}
	p := a.seen()
	if len(p) != 1 || p[0].HostKey.Status != model.HostKeyMismatch || p[0].HostKey.KnownFingerprint != ssh.FingerprintSHA256(other.PublicKey()) {
		t.Fatalf("prompts %+v", p)
	}
	// Accept and save (desktop mode admin): the stored key is replaced.
	a.setAnswer(accept(true, nil))
	_, release, err := a.pool.Acquire(ctx(t), a.user, quickConn(srv, nil), map[string]string{"password": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	release()
	known, _ := a.Server.Deps.Store.KnownHosts.Find(context.Background(), srv.Host, srv.Port)
	if len(known) != 1 || known[0].Fingerprint != ssh.FingerprintSHA256(srv.Key.PublicKey()) {
		t.Fatalf("known hosts after replace %+v", known)
	}
}

func TestKeyboardInteractiveAndSavedPassword(t *testing.T) {
	a := newAppEnv(t)
	srv := startSSHServer(t, serverOpts{Password: "pw1", KI: true})
	conn := a.createConnection(t, map[string]any{"name": "ki", "protocol": "ssh", "host": srv.Host, "port": srv.Port,
		"username": "test"})
	a.setAnswer(func(p model.Prompt) model.PromptResponse {
		switch p.Kind {
		case model.PromptHostKey:
			return model.PromptResponse{Accept: true, Save: true}
		case model.PromptKeyboardInteractive:
			if len(p.Fields) == 2 && p.Fields[0].Label == "Password:" && !p.Fields[0].Echo && p.Fields[1].Echo {
				return model.PromptResponse{Accept: true, Values: []string{"pw1", "123456"}}
			}
		}
		return model.PromptResponse{}
	})
	c, release, err := a.pool.Get(ctx(t), a.user, conn.ID)
	if err != nil {
		t.Fatalf("%v (prompts %q)", err, a.kinds())
	}
	ki := a.seen()[1]
	if ki.Title != "2FA" || ki.Message != "Enter your credentials" || ki.ConnectionID != conn.ID {
		t.Fatalf("ki prompt %+v", ki)
	}
	release()
	c.Close()

	// Password server: answer with "save" → stored into the connection's secrets; the next dial asks nothing.
	pwSrv := startSSHServer(t, serverOpts{Password: "pw2"})
	conn2 := a.createConnection(t, map[string]any{"name": "pw", "protocol": "ssh", "host": pwSrv.Host,
		"port": pwSrv.Port, "username": "test"})
	var tries int
	a.setAnswer(func(p model.Prompt) model.PromptResponse {
		switch p.Kind {
		case model.PromptHostKey:
			return model.PromptResponse{Accept: true, Save: true}
		case model.PromptPassword:
			tries++
			if !p.AllowSave {
				t.Errorf("owner's saved connection must allow saving")
			}
			if tries == 1 {
				return model.PromptResponse{Accept: true, Values: []string{"wrong"}, Save: true}
			}
			return model.PromptResponse{Accept: true, Values: []string{"pw2"}, Save: true}
		}
		return model.PromptResponse{}
	})
	c2, release2, err := a.pool.Get(ctx(t), a.user, conn2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tries != 2 || !strings.Contains(a.seen()[2].Message, "try again") {
		t.Fatalf("tries %d prompts %+v", tries, a.seen())
	}
	release2()
	c2.Close()
	var saved model.Connection
	a.admin.MustJSON("GET", "/api/connections/"+conn2.ID, nil, &saved)
	if strings.Join(saved.SecretKeys, ",") != "password" {
		t.Fatalf("secret keys %v", saved.SecretKeys)
	}
	_, secrets, _ := a.Server.Deps.ResolveConnection(context.Background(), a.user, conn2.ID)
	if secrets["password"] != "pw2" {
		t.Fatal("the wrong password was saved")
	}
	waitFor(t, func() bool { return a.pool.Stats() == 0 }, "pool drained")
	a.setAnswer(accept(false, nil))
	_, release3, err := a.pool.Get(ctx(t), a.user, conn2.ID)
	if err != nil || len(a.seen()) != 0 {
		t.Fatalf("saved password: %v %q", err, a.kinds())
	}
	release3()
}

func TestStoredEncryptedKeyWithDeferredPassphrase(t *testing.T) {
	a := newAppEnv(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "test key", []byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := ssh.NewSignerFromKey(priv)
	enc, err := a.Server.Deps.Vault.Seal(pem.EncodeToMemory(block))
	if err != nil {
		t.Fatal(err)
	}
	key := &model.SSHKey{Name: "laptop", Type: "ed25519", PublicKey: string(ssh.MarshalAuthorizedKey(signer.PublicKey())),
		Fingerprint: ssh.FingerprintSHA256(signer.PublicKey()), OwnerID: a.user.ID, PrivateKeyEnc: enc}
	if err := a.Server.Deps.Store.Keys.Create(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	srv := startSSHServer(t, serverOpts{AuthorizedKey: signer.PublicKey()})
	conn := a.createConnection(t, map[string]any{"name": "key", "protocol": "ssh", "host": srv.Host, "port": srv.Port,
		"username": "test", "keyId": key.ID, "authMethod": "key"})
	a.setAnswer(accept(true, map[string][]string{model.PromptPassphrase: {"hunter2"}}))
	_, release, err := a.pool.Get(ctx(t), a.user, conn.ID)
	if err != nil {
		t.Fatalf("%v (prompts %q)", err, a.kinds())
	}
	release()
	if got := a.kinds(); got != "hostkey,passphrase" {
		t.Fatalf("prompts %q", got)
	}
	var saved model.Connection
	a.admin.MustJSON("GET", "/api/connections/"+conn.ID, nil, &saved)
	if strings.Join(saved.SecretKeys, ",") != "passphrase" {
		t.Fatalf("passphrase not saved: %v", saved.SecretKeys)
	}
	// A key the server does not accept never triggers a passphrase prompt.
	srv2 := startSSHServer(t, serverOpts{Password: "x"})
	conn2 := a.createConnection(t, map[string]any{"name": "key2", "protocol": "ssh", "host": srv2.Host, "port": srv2.Port,
		"username": "test", "keyId": key.ID, "authMethod": "key", "secrets": map[string]string{}})
	a.setAnswer(accept(true, nil))
	_, _, err = a.pool.Get(ctx(t), a.user, conn2.ID)
	if err == nil || !term.IsPermanent(err) || strings.Contains(a.kinds(), "passphrase") {
		t.Fatalf("err %v prompts %q", err, a.kinds())
	}
}

func TestJumpHostChainAndDialer(t *testing.T) {
	a := newAppEnv(t)
	jump := startSSHServer(t, serverOpts{Password: "jpw", AllowForward: true})
	target := startSSHServer(t, serverOpts{Password: "tpw"})
	hop := a.createConnection(t, map[string]any{"name": "bastion", "protocol": "ssh", "host": jump.Host,
		"port": jump.Port, "username": "test", "secrets": map[string]string{"password": "jpw"}})
	a.setAnswer(accept(true, map[string][]string{model.PromptPassword: {"tpw"}}))
	conn := quickConn(target, model.Options{"jumpHosts": []any{hop.ID}})
	c, release, err := a.pool.Acquire(ctx(t), a.user, conn, nil)
	if err != nil {
		t.Fatalf("%v (prompts %q)", err, a.kinds())
	}
	jump.mu.Lock()
	fw := append([]string(nil), jump.Forwards...)
	jump.mu.Unlock()
	if len(fw) != 1 || fw[0] != target.Addr() {
		t.Fatalf("jump forwards %v", fw)
	}
	// Host-key prompts for both hops, the target's password prompt.
	if got := a.kinds(); got != "hostkey,hostkey,password" {
		t.Fatalf("prompts %q", got)
	}
	if !strings.HasPrefix(a.seen()[0].Title, "jump host 1 (bastion)") {
		t.Fatalf("hop prompt title %q", a.seen()[0].Title)
	}
	if a.pool.Stats() != 2 {
		t.Fatalf("pool %d", a.pool.Stats())
	}
	// The gateway stays up while the target uses it, and goes away after the target.
	release()
	waitFor(t, func() bool { return a.pool.Stats() == 0 && !c.Alive() }, "chain teardown")

	// Generic Dialer (other protocols) through the same gateway to a web server.
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "via-jump") }))
	defer web.Close()
	wu, _ := net.ResolveTCPAddr("tcp", strings.TrimPrefix(web.URL, "http://"))
	vnc := &model.Connection{Protocol: model.ProtoVNC, Host: "127.0.0.1", Port: wu.Port,
		Options: model.Options{"sshTunnelVia": hop.ID}}
	nc, err := a.pool.DialConnection(ctx(t), a.user, vnc, nil)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(nc, "GET / HTTP/1.0\r\nHost: x\r\n\r\n")
	body, _ := io.ReadAll(nc)
	nc.Close()
	if !strings.HasSuffix(string(body), "via-jump") {
		t.Fatalf("response %q", body)
	}
	waitFor(t, func() bool { return a.pool.Stats() == 0 }, "dialer release")
}

func TestExecAndSFTP(t *testing.T) {
	a := newAppEnv(t)
	dir := t.TempDir()
	srv := startSSHServer(t, serverOpts{Password: "pw", SFTPDir: dir})
	a.setAnswer(accept(true, nil))
	c, release, err := a.pool.Acquire(ctx(t), a.user, quickConn(srv, nil), map[string]string{"password": "pw"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	out, _, code, err := c.Exec(ctx(t), "echo hello")
	if err != nil || string(out) != "hello\n" || code != 0 {
		t.Fatalf("exec: %q %d %v", out, code, err)
	}
	_, stderr, code, err := c.Exec(ctx(t), "fail")
	if err != nil || string(stderr) != "boom\n" || code != 3 {
		t.Fatalf("exec fail: %q %d %v", stderr, code, err)
	}
	cctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, _, _, err = c.Exec(cctx, "sleep")
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exec cancel: %v", err)
	}
	sc, err := c.SFTP()
	if err != nil {
		t.Fatal(err)
	}
	f, err := sc.Create("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("sftp works"))
	f.Close()
	if data, _ := os.ReadFile(filepath.Join(dir, "hello.txt")); string(data) != "sftp works" {
		t.Fatalf("file %q", data)
	}
	if sc2, _ := c.SFTP(); sc2 != sc {
		t.Fatal("SFTP client not shared")
	}
}

func TestMaxSessionsFallback(t *testing.T) {
	a := newAppEnv(t)
	srv := startSSHServer(t, serverOpts{Password: "pw", MaxSessions: 1})
	a.setAnswer(accept(true, nil))
	c, release, err := a.pool.Acquire(ctx(t), a.user, quickConn(srv, nil), map[string]string{"password": "pw"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	s1, owner1, rel1, err := c.NewSessionContext(ctx(t))
	if err != nil || owner1 != c {
		t.Fatalf("first session: %v", err)
	}
	defer rel1()
	defer s1.Close()
	// The server refuses a second session on this connection: an overflow connection is opened transparently,
	// without prompting again (the credentials are kept in memory).
	s2, owner2, rel2, err := c.NewSessionContext(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if owner2 == c || a.pool.Stats() != 2 || srv.Conns.Load() != 2 || len(a.seen()) != 1 {
		t.Fatalf("overflow: same=%v pool=%d conns=%d prompts=%q", owner2 == c, a.pool.Stats(), srv.Conns.Load(), a.kinds())
	}
	s2.Close()
	rel2()
	waitFor(t, func() bool { return a.pool.Stats() == 1 }, "overflow connection closed after idle")
}

func TestKeepaliveDetectsDeadLink(t *testing.T) {
	a := newAppEnv(t)
	srv := startSSHServer(t, serverOpts{Password: "pw"})
	bh := startBlackhole(t, srv.Addr())
	a.setAnswer(accept(true, nil))
	conn := &model.Connection{Protocol: model.ProtoSSH, Host: "127.0.0.1", Port: bh.Port, Username: "test",
		AuthMethod: model.AuthAuto, Options: model.Options{"keepAliveSec": 1}}
	c, release, err := a.pool.Acquire(ctx(t), a.user, conn, map[string]string{"password": "pw"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	waitFor(t, func() bool { return srv.Keepalives.Load() >= 2 }, "keepalives")
	if c.Info().LatencyMs <= 0 {
		t.Fatalf("latency %d", c.Info().LatencyMs)
	}
	bh.frozen.Store(true)
	select {
	case <-c.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("dead link not detected")
	}
	if err := c.Err(); err == nil || !strings.Contains(err.Error(), "keepalive") {
		t.Fatalf("err %v", err)
	}
}

func TestProxies(t *testing.T) {
	a := newAppEnv(t)
	srv := startSSHServer(t, serverOpts{Password: "pw"})
	a.setAnswer(accept(true, nil))

	// SOCKS5 with authentication.
	s5 := socks5.NewServer(socks5.WithCredential(socks5.StaticCredentials{"pu": "pp"}))
	ln5, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln5.Close()
	go s5.Serve(ln5)

	// HTTP CONNECT with basic auth.
	lnh, _ := net.Listen("tcp", "127.0.0.1:0")
	defer lnh.Close()
	go serveConnectProxy(lnh, "hu", "hp")

	// SOCKS4a.
	ln4, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln4.Close()
	go serveSOCKS4(ln4)

	cases := []struct {
		typ, user, pass string
		ln              net.Listener
	}{
		{"socks5", "pu", "pp", ln5},
		{"http", "hu", "hp", lnh},
		{"socks4a", "", "", ln4},
	}
	for _, tc := range cases {
		port := tc.ln.Addr().(*net.TCPAddr).Port
		conn := quickConn(srv, model.Options{"proxy": map[string]any{"type": tc.typ, "host": "127.0.0.1", "port": port,
			"username": tc.user}})
		// Distinct pool keys per proxy: use "localhost" vs IP where needed is not required; close between runs.
		c, release, err := a.pool.Acquire(ctx(t), a.user, conn, map[string]string{"password": "pw", "proxyPassword": tc.pass})
		if err != nil {
			t.Fatalf("%s: %v", tc.typ, err)
		}
		out, _, _, err := c.Exec(ctx(t), "echo hello")
		if err != nil || string(out) != "hello\n" {
			t.Fatalf("%s exec: %q %v", tc.typ, out, err)
		}
		release()
		c.Close()
		waitFor(t, func() bool { return a.pool.Stats() == 0 }, "pool drained")
	}
	// Wrong proxy password fails cleanly.
	conn := quickConn(srv, model.Options{"proxy": map[string]any{"type": "http", "host": "127.0.0.1",
		"port": lnh.Addr().(*net.TCPAddr).Port, "username": "hu"}})
	if _, _, err := a.pool.Acquire(ctx(t), a.user, conn, map[string]string{"password": "pw", "proxyPassword": "bad"}); err == nil ||
		!strings.Contains(err.Error(), "407") {
		t.Fatalf("bad proxy auth: %v", err)
	}
}

func TestProxyCommand(t *testing.T) {
	nc, err := exec.LookPath("nc")
	if err != nil {
		t.Skip("nc not installed")
	}
	a := newAppEnv(t)
	srv := startSSHServer(t, serverOpts{Password: "pw"})
	a.setAnswer(accept(true, nil))
	conn := quickConn(srv, model.Options{"proxyCommand": nc + " %h %p"})
	c, release, err := a.pool.Acquire(ctx(t), a.user, conn, map[string]string{"password": "pw"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if out, _, _, err := c.Exec(ctx(t), "echo hello"); err != nil || string(out) != "hello\n" {
		t.Fatalf("exec %q %v", out, err)
	}
}

// TestTerminalOverHTTP drives a full SSH terminal session through the REST API, the events prompts and the
// terminal WebSocket.
func TestTerminalOverHTTP(t *testing.T) {
	a := newAppEnv(t)
	srv := startSSHServer(t, serverOpts{Password: "pw", Banner: "Hello from banner"})
	a.setAnswer(accept(true, map[string][]string{model.PromptPassword: {"pw"}}))
	var rs model.RuntimeSession
	a.admin.MustJSON("POST", "/api/sessions", map[string]any{
		"quick": map[string]any{"protocol": "ssh", "host": srv.Host, "port": srv.Port, "username": "test",
			"options": map[string]any{"term": "xterm-direct", "env": map[string]string{"FOO": "bar"}}},
		"cols": 100, "rows": 30,
	}, &rs)
	if rs.Kind != model.KindTerminal || rs.Title != "test@127.0.0.1" || rs.Cols != 100 {
		t.Fatalf("session %+v", rs)
	}
	waitFor(t, func() bool {
		var cur model.RuntimeSession
		a.admin.MustJSON("GET", "/api/sessions/"+rs.ID, nil, &cur)
		return cur.State == model.StateConnected
	}, "connected")
	if p := a.seen(); len(p) != 2 || p[1].SessionID != rs.ID {
		t.Fatalf("prompts %+v", p)
	}
	srv.mu.Lock()
	if srv.PtyTerm != "xterm-direct" || srv.PtySize != [2]int{100, 30} || srv.Env["FOO"] != "bar" {
		t.Fatalf("pty %q %v env %v", srv.PtyTerm, srv.PtySize, srv.Env)
	}
	srv.mu.Unlock()

	url := strings.Replace(a.HTTP.URL, "http", "ws", 1) + "/ws/terminal/" + rs.ID
	ws, _, err := websocket.Dial(ctx(t), url, &websocket.DialOptions{HTTPClient: a.admin.HTTP})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	var out strings.Builder
	msgs := make(chan map[string]any, 100)
	go func() {
		for {
			typ, data, err := ws.Read(context.Background())
			if err != nil {
				close(msgs)
				return
			}
			if typ == websocket.MessageBinary {
				out.Write(data)
				msgs <- map[string]any{"type": "data"}
				continue
			}
			var m map[string]any
			json.Unmarshal(data, &m)
			msgs <- m
		}
	}()
	waitMsg := func(pred func(map[string]any) bool) {
		t.Helper()
		timeout := time.After(10 * time.Second)
		for {
			select {
			case m, ok := <-msgs:
				if !ok {
					t.Fatal("socket closed")
				}
				if pred(m) {
					return
				}
			case <-timeout:
				t.Fatalf("timeout; output %q", out.String())
			}
		}
	}
	waitMsg(func(m map[string]any) bool { return m["type"] == "attach-end" })
	ws.Write(context.Background(), websocket.MessageText, []byte(`{"type":"resize","cols":120,"rows":40}`))
	ws.Write(context.Background(), websocket.MessageBinary, []byte("hello-nexterm\r"))
	waitMsg(func(map[string]any) bool { return strings.Contains(out.String(), "out:hello-nexterm") })
	if !strings.Contains(out.String(), "Hello from banner") {
		t.Fatalf("banner missing from %q", out.String())
	}
	srv.mu.Lock()
	size := srv.PtySize
	srv.mu.Unlock()
	if size != [2]int{120, 40} {
		t.Fatalf("window change %v", size)
	}
	// SSH info through the session.
	var info model.SSHConnInfo
	a.admin.MustJSON("GET", "/api/sessions/"+rs.ID+"/ssh-info", nil, &info)
	if info.Kex == "" || info.HostKeyFingerprint == "" {
		t.Fatalf("ssh info %+v", info)
	}
	c, rel, err := a.pool.ForSession(ctx(t), a.user, rs.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out, _, _, err := c.Exec(ctx(t), "echo hello"); err != nil || string(out) != "hello\n" {
		t.Fatalf("exec on the session's client: %q %v", out, err)
	}
	rel()
	// Exit status reaches the client.
	ws.Write(context.Background(), websocket.MessageBinary, []byte("exit 5\r"))
	waitMsg(func(m map[string]any) bool {
		return m["type"] == "state" && m["state"] == "disconnected" && m["exitCode"] == float64(5)
	})
	a.admin.MustJSON("DELETE", "/api/sessions/"+rs.ID, nil, nil)
	waitMsg(func(m map[string]any) bool { return m["type"] == "state" && m["state"] == "closed" })
}

func TestAlgorithmsCatalog(t *testing.T) {
	a := newAppEnv(t)
	var cat sshx.AlgorithmCatalog
	a.admin.MustJSON("GET", "/api/ssh/algorithms", nil, &cat)
	if len(cat.Supported.Kex) == 0 || len(cat.Insecure.Ciphers) == 0 {
		t.Fatalf("catalog %+v", cat)
	}
	// A legacy-only server: without legacyAlgorithms the handshake fails permanently, with it it works.
	srv := startSSHServer(t, serverOpts{Password: "pw", Ciphers: []string{"aes128-cbc"}})
	a.setAnswer(accept(true, nil))
	_, _, err := a.pool.Acquire(ctx(t), a.user, quickConn(srv, nil), map[string]string{"password": "pw"})
	if err == nil || !term.IsPermanent(err) || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("modern-only dial: %v", err)
	}
	_, release, err := a.pool.Acquire(ctx(t), a.user, quickConn(srv, model.Options{"legacyAlgorithms": true}),
		map[string]string{"password": "pw"})
	if err != nil {
		t.Fatal(err)
	}
	release()
}

// ---- tiny proxies for tests ---------------------------------------------------------------------------------------

func serveConnectProxy(ln net.Listener, user, pass string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			br := bufio.NewReader(c)
			req, err := http.ReadRequest(br)
			if err != nil || req.Method != http.MethodConnect {
				return
			}
			u, p, ok := parseBasic(req.Header.Get("Proxy-Authorization"))
			if !ok || u != user || p != pass {
				io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n")
				return
			}
			up, err := net.Dial("tcp", req.Host)
			if err != nil {
				io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
				return
			}
			defer up.Close()
			io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
			go io.Copy(up, br)
			io.Copy(c, up)
		}(c)
	}
}

func parseBasic(h string) (string, string, bool) {
	r := &http.Request{Header: http.Header{"Authorization": {h}}}
	return r.BasicAuth()
}

func serveSOCKS4(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			br := bufio.NewReader(c)
			hdr := make([]byte, 8)
			if _, err := io.ReadFull(br, hdr); err != nil || hdr[0] != 4 || hdr[1] != 1 {
				return
			}
			port := int(hdr[2])<<8 | int(hdr[3])
			if _, err := br.ReadString(0); err != nil { // user id
				return
			}
			host := net.IP(hdr[4:8]).String()
			if hdr[4] == 0 && hdr[5] == 0 && hdr[6] == 0 && hdr[7] != 0 { // SOCKS4a
				name, err := br.ReadString(0)
				if err != nil {
					return
				}
				host = strings.TrimSuffix(name, "\x00")
			}
			up, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
			if err != nil {
				c.Write([]byte{0, 0x5b, 0, 0, 0, 0, 0, 0})
				return
			}
			defer up.Close()
			c.Write([]byte{0, 0x5a, 0, 0, 0, 0, 0, 0})
			go io.Copy(up, br)
			io.Copy(c, up)
		}(c)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
