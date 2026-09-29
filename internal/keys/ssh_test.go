package keys

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/netguard"
	"github.com/plzcloseyoureyes/astraterm/internal/sshx"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// answer builds a prompt answerer: host keys are accepted (and saved when save is set), passwords / passphrases /
// confirmations get the given values.
func answer(save bool, values map[string][]string, confirm bool) func(model.Prompt) model.PromptResponse {
	return func(p model.Prompt) model.PromptResponse {
		switch p.Kind {
		case model.PromptHostKey:
			return model.PromptResponse{Accept: true, Save: save}
		case model.PromptConfirm:
			return model.PromptResponse{Accept: confirm, Save: save}
		}
		if v, ok := values[p.Kind]; ok {
			return model.PromptResponse{Accept: true, Values: v, Save: save}
		}
		return model.PromptResponse{}
	}
}

func kinds(ps []model.Prompt) string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Kind)
	}
	return strings.Join(out, ",")
}

func (e *testEnv) saveConn(t *testing.T, c *model.Connection, secrets map[string]string) {
	t.Helper()
	if len(secrets) > 0 {
		enc, err := e.d.Vault.SealJSON(secrets)
		if err != nil {
			t.Fatal(err)
		}
		c.SecretsEnc, c.SecretKeys = enc, model.SortedKeys(secrets)
	}
	if err := e.d.Store.Connections.Create(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}

func TestInstallKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	e := newTestEnv(t, config.ModeDesktop)
	p := e.c.prompter(t)
	p.set(answer(true, nil, true))
	var k keyJSON
	e.c.must("POST", "/api/keys/generate", map[string]any{"name": "deploy", "comment": "deploy@astraterm"}, &k)

	// SFTP path: ~/.ssh is created (0700), the key appended, the file 0600; a second install finds it.
	home := t.TempDir()
	srv := startTestServer(t, testServerOpts{Password: "pw", HomeDir: home})
	conn := srv.conn(e.admin, "", model.AuthPassword, nil)
	e.saveConn(t, conn, map[string]string{"password": "pw"})
	var res installResult
	e.c.must("POST", "/api/keys/"+k.ID+"/install", map[string]any{"connectionId": conn.ID}, &res)
	if !res.Installed || res.AlreadyPresent || res.Method != "sftp" || !strings.HasSuffix(res.Path, "/.ssh/authorized_keys") {
		t.Fatalf("%+v", res)
	}
	checkAuthorized(t, home, k.PublicKey, 1)
	e.c.must("POST", "/api/keys/"+k.ID+"/install", map[string]any{"connectionId": conn.ID}, &res)
	if res.Installed || !res.AlreadyPresent {
		t.Fatalf("second install %+v", res)
	}
	checkAuthorized(t, home, k.PublicKey, 1)

	// Shell fallback (no SFTP subsystem), with an existing file lacking its final newline and a loose ~/.ssh mode.
	home2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home2, ".ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	other := strings.TrimSpace(string(fixture(t, "ca.pub")))
	if err := os.WriteFile(filepath.Join(home2, ".ssh", "authorized_keys"), []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}
	srv2 := startTestServer(t, testServerOpts{Password: "pw", HomeDir: home2, NoSFTP: true})
	conn2 := srv2.conn(e.admin, "", model.AuthPassword, nil)
	e.saveConn(t, conn2, map[string]string{"password": "pw"})
	e.c.must("POST", "/api/keys/"+k.ID+"/install", map[string]any{"connectionId": conn2.ID}, &res)
	if !res.Installed || res.Method != "shell" {
		t.Fatalf("%+v", res)
	}
	checkAuthorized(t, home2, k.PublicKey, 2)
	e.c.must("POST", "/api/keys/"+k.ID+"/install", map[string]any{"connectionId": conn2.ID}, &res)
	if !res.AlreadyPresent {
		t.Fatalf("%+v", res)
	}

	// The installed key authenticates: a server trusting what was written accepts the stored key.
	data, _ := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
	pub, _, _, _, _ := ssh.ParseAuthorizedKey(data)
	srv3 := startTestServer(t, testServerOpts{AuthorizedKeys: []ssh.PublicKey{pub}})
	cl, rel, err := e.core.SSH.Acquire(ctxT(t), e.admin, srv3.conn(e.admin, k.ID, model.AuthKey, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	rel()
	_ = cl
	if st, code := e.c.code("POST", "/api/keys/"+k.ID+"/install", map[string]any{"connectionId": "nosuchconnection00"}); st != 404 {
		t.Fatalf("unknown connection: %d %s", st, code)
	}
}

// TestInstallKeyKeepsExistingKeys: over SFTP, the key is appended after the existing lines even when the server
// ignores SSH_FXF_APPEND (pkg/sftp's server writes at the offsets the client sends): the other keys must survive.
func TestInstallKeyKeepsExistingKeys(t *testing.T) {
	e := newTestEnv(t, config.ModeDesktop)
	p := e.c.prompter(t)
	p.set(answer(true, nil, true))
	var k keyJSON
	e.c.must("POST", "/api/keys/generate", map[string]any{"name": "deploy", "comment": "deploy@astraterm"}, &k)

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	other1 := strings.TrimSpace(string(fixture(t, "ca.pub")))
	other2 := strings.TrimSpace(string(fixture(t, "openssh_ed25519.pub")))
	existing := "# managed by hand\n" + other1 + "\n" + other2 // no final newline
	path := filepath.Join(home, ".ssh", "authorized_keys")
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := startTestServer(t, testServerOpts{Password: "pw", HomeDir: home})
	conn := srv.conn(e.admin, "", model.AuthPassword, nil)
	e.saveConn(t, conn, map[string]string{"password": "pw"})
	var res installResult
	e.c.must("POST", "/api/keys/"+k.ID+"/install", map[string]any{"connectionId": conn.ID}, &res)
	if !res.Installed || res.Method != "sftp" {
		t.Fatalf("%+v", res)
	}
	data, _ := os.ReadFile(path)
	if want := existing + "\n" + k.PublicKey + "\n"; string(data) != want {
		t.Fatalf("authorized_keys:\n%q\nwant\n%q", data, want)
	}
}

func checkAuthorized(t *testing.T, home, line string, lines int) {
	t.Helper()
	dir := filepath.Join(home, ".ssh")
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("~/.ssh mode %v %v", st.Mode(), err)
	}
	fst, err := os.Stat(filepath.Join(dir, "authorized_keys"))
	if err != nil || fst.Mode().Perm() != 0o600 {
		t.Fatalf("authorized_keys mode %v %v", fst.Mode(), err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "authorized_keys"))
	got := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(got) != lines || strings.Count(string(data), line) != 1 || !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("authorized_keys:\n%s", data)
	}
}

func hostCertSigner(t *testing.T, ca, host ssh.Signer, principals []string, validBefore uint64) ssh.Signer {
	t.Helper()
	cert := &ssh.Certificate{Key: host.PublicKey(), CertType: ssh.HostCert, KeyId: "test host", ValidPrincipals: principals,
		ValidBefore: validBefore}
	if validBefore != ssh.CertTimeInfinity {
		cert.ValidAfter = 1
	}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewCertSigner(cert, host)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestHostCertificates covers SSH-20 through the real SSH pool: CA-signed host certificates are trusted without a
// prompt, certificates no CA vouches for are verified as their plain key (TOFU stores the plain key), expired
// certificates fall back to the plain key, and @revoked keys are refused.
func TestHostCertificates(t *testing.T) {
	e := newTestEnv(t, config.ModeDesktop)
	p := e.c.prompter(t)
	ca, host := newEd25519Signer(t), newEd25519Signer(t)
	srv := startTestServer(t, testServerOpts{Password: "pw", HostKey: hostCertSigner(t, ca, host, []string{"127.0.0.1"}, ssh.CertTimeInfinity)})
	conn := srv.conn(e.admin, "", model.AuthPassword, nil)
	secrets := map[string]string{"password": "pw"}
	pool := e.core.SSH
	acquire := func() error {
		t.Helper()
		_, rel, err := pool.Acquire(ctxT(t), e.admin, conn, secrets)
		if err != nil {
			return err
		}
		rel()
		waitFor(t, "idle close", func() bool { return pool.Stats() == 0 })
		return nil
	}

	var ma HostKeyMarker
	e.c.must("POST", "/api/known-hosts/markers", map[string]any{"marker": "cert-authority", "hosts": "127.0.0.*,localhost",
		"publicKey": authorizedKeyLine(ca.PublicKey(), "test CA")}, &ma)
	p.set(answer(true, nil, true))
	if err := acquire(); err != nil {
		t.Fatal(err)
	}
	if got := kinds(p.prompts()); got != "" {
		t.Fatalf("prompts with a trusted CA: %q", got)
	}
	if kh, _ := e.d.Store.KnownHosts.List(context.Background()); len(kh) != 0 {
		t.Fatal("a CA-verified host key was saved")
	}

	// Without the CA the certificate is verified as its plain key: TOFU prompt and saved plain key.
	e.c.must("DELETE", "/api/known-hosts/markers/"+ma.ID, nil, nil)
	p.set(answer(true, nil, true))
	if err := acquire(); err != nil {
		t.Fatal(err)
	}
	ps := p.prompts()
	if kinds(ps) != "hostkey" || ps[0].HostKey.KeyType != ssh.KeyAlgoED25519 || ps[0].HostKey.Fingerprint != ssh.FingerprintSHA256(host.PublicKey()) {
		t.Fatalf("%+v", ps)
	}
	kh, _ := e.d.Store.KnownHosts.List(context.Background())
	if len(kh) != 1 || kh[0].KeyType != ssh.KeyAlgoED25519 {
		t.Fatalf("saved %+v", kh)
	}

	// An expired certificate from a trusted CA is rejected and the (now known) plain key is used, silently.
	e.c.must("POST", "/api/known-hosts/markers", map[string]any{"marker": "cert-authority", "hosts": "*",
		"publicKey": authorizedKeyLine(ca.PublicKey(), "")}, &ma)
	expired := startTestServer(t, testServerOpts{Password: "pw", HostKey: hostCertSigner(t, ca, host, []string{"127.0.0.1"}, 2)})
	conn = expired.conn(e.admin, "", model.AuthPassword, nil)
	p.set(answer(true, nil, true))
	e.c.must("POST", "/api/known-hosts", map[string]any{"host": "127.0.0.1", "port": expired.Port, "publicKey": authorizedKeyLine(host.PublicKey(), "")}, nil)
	if err := acquire(); err != nil {
		t.Fatal(err)
	}
	if got := kinds(p.prompts()); got != "" {
		t.Fatalf("expired certificate: prompts %q", got)
	}

	// A revoked host key is refused whatever else trusts it.
	e.c.must("POST", "/api/known-hosts/markers", map[string]any{"marker": "revoked", "hosts": "*", "publicKey": authorizedKeyLine(host.PublicKey(), "")}, nil)
	conn = srv.conn(e.admin, "", model.AuthPassword, nil)
	if err := acquire(); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked key: %v", err)
	}
}

// TestUserCertificateAuth covers SSH-5: a stored key with an attached user certificate logs in to a server that only
// trusts the CA.
func TestUserCertificateAuth(t *testing.T) {
	e := newTestEnv(t, config.ModeDesktop)
	p := e.c.prompter(t)
	p.set(answer(true, nil, true))
	var ca, user keyJSON
	e.c.must("POST", "/api/keys/generate", map[string]any{"name": "CA", "type": "ecdsa", "bits": 256}, &ca)
	e.c.must("POST", "/api/keys/generate", map[string]any{"name": "me"}, &user)
	srv := startTestServer(t, testServerOpts{UserCA: mustParsePub(t, ca.PublicKey)})
	conn := srv.conn(e.admin, user.ID, model.AuthKey, nil)
	e.saveConn(t, conn, nil)
	if _, _, err := e.core.SSH.Get(ctxT(t), e.admin, conn.ID); err == nil {
		t.Fatal("logged in without a certificate")
	}
	e.c.must("POST", "/api/keys/"+ca.ID+"/sign", map[string]any{"subjectKeyId": user.ID, "principals": []string{"test"},
		"validBefore": time.Now().Add(time.Hour).Format(time.RFC3339), "attach": true}, nil)
	_, rel, err := e.core.SSH.Get(ctxT(t), e.admin, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	rel()
}

func agentSocketTest(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix socket test")
	}
	t.Setenv("SSH_AUTH_SOCK", "")
}

func dialAgent(t *testing.T, path string) agent.ExtendedAgent {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return agent.NewClient(c)
}

func (e *testEnv) setKeysSettings(t *testing.T, userID string, v map[string]any) {
	t.Helper()
	if err := e.d.Store.Settings.SetJSON(context.Background(), userID, settingsSection, v); err != nil {
		t.Fatal(err)
	}
}

// TestAgentSocket exercises the built-in agent (SSH-11) through its Unix socket like ssh-add / ssh do.
func TestAgentSocket(t *testing.T) {
	agentSocketTest(t)
	e := newTestEnv(t, config.ModeDesktop)
	p := e.c.prompter(t)
	var a, b keyJSON
	e.c.must("POST", "/api/keys/generate", map[string]any{"name": "alpha"}, &a)
	e.c.must("POST", "/api/keys/generate", map[string]any{"name": "beta", "type": "rsa", "bits": 2048, "passphrase": "pw", "rememberPassphrase": false}, &b)

	var st agentStatus
	e.c.must("POST", "/api/agent/start", nil, &st)
	if !st.Running || !st.Owner || st.SocketPath == "" || st.KeyCount != 2 {
		t.Fatalf("%+v", st)
	}
	t.Cleanup(func() { os.Remove(filepath.Dir(st.SocketPath)) })
	if fi, err := os.Stat(st.SocketPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v %v", fi, err)
	}
	ac := dialAgent(t, st.SocketPath)
	keys, err := ac.List()
	if err != nil || len(keys) != 2 {
		t.Fatalf("%d %v", len(keys), err)
	}
	pubA, pubB := mustParsePub(t, a.PublicKey), mustParsePub(t, b.PublicKey)
	sig, err := ac.Sign(pubA, []byte("data"))
	if err != nil || pubA.Verify([]byte("data"), sig) != nil {
		t.Fatalf("sign: %v", err)
	}
	// The encrypted key asks for its passphrase once, then stays unlocked.
	p.set(answer(false, map[string][]string{model.PromptPassphrase: {"pw"}}, true))
	sig, err = ac.SignWithFlags(pubB, []byte("data"), agent.SignatureFlagRsaSha256)
	if err != nil || sig.Format != ssh.KeyAlgoRSASHA256 || pubB.Verify([]byte("data"), sig) != nil {
		t.Fatalf("rsa-sha2-256: %v", err)
	}
	if _, err := ac.SignWithFlags(pubB, []byte("more"), agent.SignatureFlagRsaSha512); err != nil {
		t.Fatal(err)
	}
	if got := kinds(p.prompts()); got != "passphrase" {
		t.Fatalf("prompts %q", got)
	}

	// Confirm before use: refused, then accepted and remembered.
	e.setKeysSettings(t, e.admin.ID, map[string]any{"agentConfirm": true})
	p.set(answer(false, nil, false))
	if _, err := ac.Sign(pubA, []byte("x")); err == nil {
		t.Fatal("refused confirmation signed anyway")
	}
	p.set(answer(true, nil, true))
	if _, err := ac.Sign(pubA, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Sign(pubA, []byte("y")); err != nil {
		t.Fatal(err)
	}
	if ps := p.prompts(); len(ps) != 1 || ps[0].Kind != model.PromptConfirm || !strings.Contains(ps[0].Message, "alpha") {
		t.Fatalf("%+v", ps)
	}

	// Keys excluded in the settings are not offered.
	e.setKeysSettings(t, e.admin.ID, map[string]any{"agentExclude": []string{b.ID}})
	if keys, _ = ac.List(); len(keys) != 1 {
		t.Fatalf("exclusion: %d", len(keys))
	}
	e.setKeysSettings(t, e.admin.ID, map[string]any{})

	// ssh-add: a key with a lifetime.
	_, extra, _ := ed25519Key()
	if err := ac.Add(agent.AddedKey{PrivateKey: extra, Comment: "temp", LifetimeSecs: 1}); err != nil {
		t.Fatal(err)
	}
	if keys, _ = ac.List(); len(keys) != 3 {
		t.Fatalf("after add: %d", len(keys))
	}
	var ak []agentKeyView
	e.c.must("GET", "/api/agent/keys", nil, &ak)
	if len(ak) != 3 || ak[2].Source != "added" || ak[2].ExpiresAt == nil || !ak[1].Unlocked {
		t.Fatalf("%+v", ak)
	}
	time.Sleep(1100 * time.Millisecond)
	if keys, _ = ac.List(); len(keys) != 2 {
		t.Fatalf("lifetime: %d", len(keys))
	}
	if err := ac.Add(agent.AddedKey{PrivateKey: extra, Comment: "restricted", ConstraintExtensions: []agent.ConstraintExtension{{ExtensionName: "restrict-destination-v00@openssh.com"}}}); err == nil {
		t.Fatal("destination constraints must be refused")
	}

	// ssh-add -d on a stored key unloads it until a reload.
	if err := ac.Remove(pubB); err != nil {
		t.Fatal(err)
	}
	if keys, _ = ac.List(); len(keys) != 1 {
		t.Fatalf("remove: %d", len(keys))
	}
	e.c.must("POST", "/api/agent/reload", nil, nil)
	if keys, _ = ac.List(); len(keys) != 2 {
		t.Fatalf("reload: %d", len(keys))
	}

	// ssh-add -x / -X, and locks from AstraTerm (which only AstraTerm can undo).
	if err := ac.Lock([]byte("lock-pw")); err != nil {
		t.Fatal(err)
	}
	if keys, _ = ac.List(); len(keys) != 0 {
		t.Fatal("locked agent lists keys")
	}
	if ac.Unlock([]byte("bad")) == nil || ac.Unlock([]byte("lock-pw")) != nil {
		t.Fatal("unlock")
	}
	e.c.must("POST", "/api/agent/lock", nil, nil)
	if ac.Unlock([]byte("lock-pw")) == nil {
		t.Fatal("AstraTerm lock undone by ssh-add -X")
	}
	e.c.must("POST", "/api/agent/unlock", nil, nil)
	if keys, _ = ac.List(); len(keys) != 2 {
		t.Fatalf("unlocked: %d", len(keys))
	}

	// A locked vault hides the stored keys.
	if err := e.d.Vault.SetMasterPassword(context.Background(), "", "master password 1"); err != nil {
		t.Fatal(err)
	}
	if err := e.d.Vault.Lock(); err != nil {
		t.Fatal(err)
	}
	if keys, _ = ac.List(); len(keys) != 0 {
		t.Fatalf("vault locked: %d", len(keys))
	}
	if err := e.d.Vault.Unlock(context.Background(), "master password 1"); err != nil {
		t.Fatal(err)
	}

	// Another user cannot see or stop the agent; stopping removes the socket.
	other, _ := e.user("dave")
	var ost agentStatus
	other.must("GET", "/api/agent/status", nil, &ost)
	if !ost.Running || ost.Owner || ost.SocketPath != "" {
		t.Fatalf("%+v", ost)
	}
	if st, _ := other.code("POST", "/api/agent/stop", nil); st != 403 {
		t.Fatalf("foreign stop: %d", st)
	}
	if st, _ := other.code("POST", "/api/agent/start", nil); st != 409 {
		t.Fatalf("second owner: %d", st)
	}
	e.c.must("POST", "/api/agent/stop", nil, nil)
	if _, err := os.Stat(st.SocketPath); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
	e.c.must("GET", "/api/agent/status", nil, &st)
	if st.Running {
		t.Fatal("still running")
	}
}

// TestAgentSessionBind: prompts name the destination announced with session-bind@openssh.com.
func TestAgentSessionBind(t *testing.T) {
	agentSocketTest(t)
	e := newTestEnv(t, config.ModeDesktop)
	p := e.c.prompter(t)
	var a keyJSON
	e.c.must("POST", "/api/keys/generate", map[string]any{"name": "alpha"}, &a)
	host := newEd25519Signer(t)
	e.c.must("POST", "/api/known-hosts", map[string]any{"host": "prod.example.com", "publicKey": authorizedKeyLine(host.PublicKey(), "")}, nil)
	var st agentStatus
	e.c.must("POST", "/api/agent/start", nil, &st)
	t.Cleanup(func() { os.Remove(filepath.Dir(st.SocketPath)) })
	e.setKeysSettings(t, e.admin.ID, map[string]any{"agentConfirm": true})
	ac := dialAgent(t, st.SocketPath)
	sid := []byte("session identifier")
	sig, _ := host.Sign(rand.Reader, sid)
	payload := ssh.Marshal(struct {
		HostKey, SessionID, Signature []byte
		Forwarding                    bool
	}{host.PublicKey().Marshal(), sid, ssh.Marshal(sig), false})
	if _, err := ac.Extension("session-bind@openssh.com", payload); err != nil {
		t.Fatal(err)
	}
	bad := ssh.Marshal(struct {
		HostKey, SessionID, Signature []byte
		Forwarding                    bool
	}{host.PublicKey().Marshal(), []byte("other"), ssh.Marshal(sig), false})
	if _, err := ac.Extension("session-bind@openssh.com", bad); err == nil {
		t.Fatal("unverifiable session-bind accepted")
	}
	p.set(answer(false, nil, true))
	if _, err := ac.Sign(mustParsePub(t, a.PublicKey), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if ps := p.prompts(); len(ps) != 1 || !strings.Contains(ps[0].Message, "prod.example.com") {
		t.Fatalf("%+v", ps)
	}
}

// TestBuiltinAgentAuthAndForwarding: while the built-in agent runs, its keys authenticate connections that use the
// agent, and agent forwarding serves it.
func TestBuiltinAgentAuthAndForwarding(t *testing.T) {
	agentSocketTest(t)
	e := newTestEnv(t, config.ModeDesktop)
	p := e.c.prompter(t)
	p.set(answer(true, nil, true))
	var k keyJSON
	e.c.must("POST", "/api/keys/generate", map[string]any{"name": "agent key"}, &k)
	srv := startTestServer(t, testServerOpts{AuthorizedKeys: []ssh.PublicKey{mustParsePub(t, k.PublicKey)}})
	conn := srv.conn(e.admin, "", model.AuthAgent, model.Options{"agentForwarding": true})
	if _, _, err := e.core.SSH.Acquire(ctxT(t), e.admin, conn, nil); err == nil {
		t.Fatal("authenticated without any agent")
	}
	var st agentStatus
	e.c.must("POST", "/api/agent/start", nil, &st)
	t.Cleanup(func() { os.Remove(filepath.Dir(st.SocketPath)) })
	_, rel, err := e.core.SSH.Acquire(ctxT(t), e.admin, conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	rel()

	s, err := e.core.Sessions.Create(context.Background(), e.admin, term.CreateRequest{Connection: conn, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer e.core.Sessions.Close(s.ID)
	waitFor(t, "forwarded agent signature", func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return srv.AgentSigned == 1 && len(srv.AgentKeys) == 1
	})
}

// serveTestHostAgent runs keyring as the host's ssh-agent (SSH_AUTH_SOCK) for the test.
func serveTestHostAgent(t *testing.T, keyring agent.Agent) {
	t.Helper()
	dir, err := os.MkdirTemp("", "nxk") // short: Unix socket paths are limited to ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "a.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = agent.ServeAgent(keyring, c)
			}()
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(dir, "a.sock"))
}

// TestLockedAgentKeepsHostAgent: a locked built-in agent holds no usable key, so the merged agents of AstraTerm (agent
// forwarding, logins) still sign with the host agent's keys; a locked agent also refuses extensions, and wrong
// ssh-add -X passphrases are throttled.
func TestLockedAgentKeepsHostAgent(t *testing.T) {
	agentSocketTest(t)
	hostPub, hostKey, _ := ed25519Key()
	hostKR := agent.NewKeyring()
	if err := hostKR.Add(agent.AddedKey{PrivateKey: hostKey, Comment: "host agent key"}); err != nil {
		t.Fatal(err)
	}
	serveTestHostAgent(t, hostKR)

	e := newTestEnv(t, config.ModeDesktop)
	p := e.c.prompter(t)
	p.set(answer(true, nil, true))
	var k keyJSON
	e.c.must("POST", "/api/keys/generate", map[string]any{"name": "stored"}, &k)
	var st agentStatus
	e.c.must("POST", "/api/agent/start", nil, &st)
	t.Cleanup(func() { os.Remove(filepath.Dir(st.SocketPath)) })
	e.c.must("POST", "/api/agent/lock", nil, nil)

	// The view AstraTerm merges with the host agent: locked → "key not found", so the next agent is tried.
	view := e.handler().agent.Agent(context.Background(), e.admin, nil, true)
	if _, err := view.SignWithFlags(mustParsePub(t, k.PublicKey), []byte("x"), 0); !errors.Is(err, sshx.ErrAgentKeyNotFound) {
		t.Fatalf("locked view: %v", err)
	}
	if _, err := view.Extension("session-bind@openssh.com", nil); err == nil {
		t.Fatal("a locked agent accepted an extension")
	}

	// Forwarding through the merged agent: the remote side lists and signs with the host agent's key.
	srv := startTestServer(t, testServerOpts{Password: "pw"})
	conn := srv.conn(e.admin, "", model.AuthPassword, model.Options{"agentForwarding": true})
	s, err := e.core.Sessions.Create(context.Background(), e.admin, term.CreateRequest{Connection: conn,
		Secrets: map[string]string{"password": "pw"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer e.core.Sessions.Close(s.ID)
	waitFor(t, "forwarded host agent signature", func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return srv.AgentSigned == 1 || srv.AgentSignErr != nil
	})
	srv.mu.Lock()
	listed, signErr := srv.AgentKeys, srv.AgentSignErr
	srv.mu.Unlock()
	if signErr != nil || len(listed) != 1 || !sameKey(hostPub, mustParsePub(t, listed[0].String())) {
		t.Fatalf("forwarded keys %v, sign error %v", listed, signErr)
	}

	// ssh-add -x / -X: wrong passphrases are answered more and more slowly; the right one resets the delay.
	e.c.must("POST", "/api/agent/unlock", nil, nil)
	ac := dialAgent(t, st.SocketPath)
	if err := ac.Lock([]byte("lock-pw")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for range 2 {
		if ac.Unlock([]byte("wrong")) == nil {
			t.Fatal("wrong passphrase unlocked the agent")
		}
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("two wrong unlocks took only %v", d)
	}
	if err := ac.Unlock([]byte("lock-pw")); err != nil {
		t.Fatal(err)
	}
	r := e.handler().agent.ring(e.admin.ID)
	r.mu.Lock()
	failed, locked := r.failedUnlocks, r.locked
	r.mu.Unlock()
	if failed != 0 || locked {
		t.Fatalf("after unlock: %d failures, locked %v", failed, locked)
	}
}

// TestForwardingKeyringServerMode covers SSH-12 for users without a host agent: the forwarded keyring offers only
// the keys selected for the agent and asks before a remote host uses one.
func TestForwardingKeyringServerMode(t *testing.T) {
	e := newTestEnv(t, config.ModeServer)
	bob, bobUser := e.user("bob")
	p := bob.prompter(t)
	var k1, k2 keyJSON
	bob.must("POST", "/api/keys/generate", map[string]any{"name": "shared with servers"}, &k1)
	bob.must("POST", "/api/keys/generate", map[string]any{"name": "private"}, &k2)
	e.setKeysSettings(t, bobUser.ID, map[string]any{"agentExclude": []string{k2.ID}, "agentForwardConfirm": true})
	srv := startTestServer(t, testServerOpts{Password: "pw"})
	// bob must stay a non-admin (the forwarding keyring is what non-admins get in server mode), but the server-mode
	// network policy refuses loopback destinations for him: allow exactly the loopback test server as an exception.
	pol := netguard.DefaultPolicy()
	pol.Allow = []string{srv.Host + "/32"}
	pol.AllowedPorts = strconv.Itoa(srv.Port)
	if _, err := netguard.For(e.d).SetPolicy(context.Background(), pol); err != nil {
		t.Fatal(err)
	}
	conn := srv.conn(bobUser, "", model.AuthPassword, model.Options{"agentForwarding": true})
	p.set(answer(true, nil, true))
	s, err := e.core.Sessions.Create(context.Background(), bobUser, term.CreateRequest{Connection: conn,
		Secrets: map[string]string{"password": "pw"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer e.core.Sessions.Close(s.ID)
	waitFor(t, "forwarded keyring signature", func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return srv.AgentSigned == 1
	})
	srv.mu.Lock()
	listed := srv.AgentKeys
	srv.mu.Unlock()
	if len(listed) != 1 || !sameKey(mustParsePub(t, k1.PublicKey), mustParsePub(t, listed[0].String())) {
		t.Fatalf("forwarded keys %v", listed)
	}
	var confirm *model.Prompt
	for _, pr := range p.prompts() {
		if pr.Kind == model.PromptConfirm {
			confirm = &pr
		}
	}
	if confirm == nil || !strings.Contains(confirm.Message, "forwarded agent") || !strings.Contains(confirm.Message, srv.Host) {
		t.Fatalf("confirmation %+v", p.prompts())
	}
}

func ed25519Key() (ssh.PublicKey, any, error) {
	k, err := generateKey("ed25519", 0)
	if err != nil {
		return nil, nil, err
	}
	pk, err := ssh.NewPublicKey(publicOf(k))
	return pk, k, err
}
