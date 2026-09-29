package keys

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/termstead/termstead/internal/config"
	"github.com/termstead/termstead/internal/model"
)

type keyJSON struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Type            string    `json:"type"`
	Bits            int       `json:"bits"`
	PublicKey       string    `json:"publicKey"`
	Fingerprint     string    `json:"fingerprint"`
	FingerprintMD5  string    `json:"fingerprintMd5"`
	Comment         string    `json:"comment"`
	HasPassphrase   bool      `json:"hasPassphrase"`
	PassphraseSaved bool      `json:"passphraseSaved"`
	HasPrivateKey   bool      `json:"hasPrivateKey"`
	Certificate     string    `json:"certificate"`
	CertificateInfo *certInfo `json:"certificateInfo"`
	UsedBy          keyUsage  `json:"usedBy"`
}

func TestKeyLifecycle(t *testing.T) {
	e := newTestEnv(t, config.ModeDesktop)
	c := e.c

	var ed keyJSON
	if st := c.json("POST", "/api/keys/generate", map[string]any{"name": "Work laptop", "type": "ed25519", "comment": "me@work"}, &ed); st != http.StatusCreated {
		t.Fatalf("generate: %d", st)
	}
	if ed.Type != "ed25519" || ed.Bits != 256 || !strings.HasPrefix(ed.Fingerprint, "SHA256:") || !strings.HasPrefix(ed.FingerprintMD5, "MD5:") ||
		!ed.HasPrivateKey || ed.HasPassphrase || !strings.HasSuffix(ed.PublicKey, " me@work") {
		t.Fatalf("%+v", ed)
	}
	var rsaKey keyJSON
	c.must("POST", "/api/keys/generate", map[string]any{"name": "Legacy", "type": "rsa", "bits": 2048, "passphrase": "pw-1"}, &rsaKey)
	if !rsaKey.HasPassphrase || !rsaKey.PassphraseSaved || rsaKey.Bits != 2048 || !strings.Contains(rsaKey.Comment, "rsa-key-") {
		t.Fatalf("%+v", rsaKey)
	}
	if st, code := c.code("POST", "/api/keys/generate", map[string]any{"type": "rsa", "bits": 1024}); st != 400 || code != "invalid_key" {
		t.Fatalf("bad bits: %d %s", st, code)
	}

	// The stored material is usable by the connection side (app.KeyMaterial + sshx), including the passphrase.
	pemBytes, pass, err := e.d.KeyMaterial(context.Background(), e.admin, rsaKey.ID)
	if err != nil || pass != "pw-1" {
		t.Fatalf("%v %q", err, pass)
	}
	if pk, err := parseKey(pemBytes, pass); err != nil || !pk.Encrypted {
		t.Fatalf("stored key: %v", err)
	}

	// Exports.
	st, body, hdr := c.do("GET", "/api/keys/"+ed.ID+"/export?format=public", nil)
	if st != 200 || strings.TrimSpace(string(body)) != ed.PublicKey || !strings.Contains(hdr.Get("Content-Disposition"), "work-laptop.pub") {
		t.Fatalf("public export %d %q %q", st, body, hdr)
	}
	st, body, _ = c.do("GET", "/api/keys/"+rsaKey.ID+"/export?format=openssh", nil)
	if pk, err := parseKey(body, "pw-1"); st != 200 || err != nil || !pk.Encrypted {
		t.Fatalf("GET openssh keeps the passphrase: %d %v", st, err)
	}
	var res exportResult
	c.must("POST", "/api/keys/"+rsaKey.ID+"/export", map[string]any{"format": "ppk", "passphrase": "new-pw", "ppkVersion": 2}, &res)
	if pk, err := parseKey([]byte(res.Content), "new-pw"); err != nil || pk.PPKVersion != 2 || !res.Encrypted || res.Filename != "legacy.ppk" {
		t.Fatalf("ppk export %v %+v", err, res)
	}
	c.must("POST", "/api/keys/"+rsaKey.ID+"/export", map[string]any{"format": "pem"}, &res)
	if pk, err := parseKey([]byte(res.Content), ""); err != nil || pk.Encrypted {
		t.Fatalf("unencrypted pem export %v", err)
	}
	c.must("POST", "/api/keys/"+ed.ID+"/export", map[string]any{"format": "rfc4716"}, &res)
	if !strings.HasPrefix(res.Content, "---- BEGIN SSH2 PUBLIC KEY ----") {
		t.Fatal(res.Content)
	}

	// Rename / comment.
	var upd keyJSON
	c.must("PATCH", "/api/keys/"+ed.ID, map[string]any{"name": "  Renamed  key ", "comment": "new comment"}, &upd)
	if upd.Name != "Renamed key" || upd.Comment != "new comment" || !strings.HasSuffix(upd.PublicKey, " new comment") {
		t.Fatalf("%+v", upd)
	}
	if st, _ := c.code("PATCH", "/api/keys/"+ed.ID, map[string]any{"name": ""}); st != 400 {
		t.Fatalf("empty name: %d", st)
	}

	// Sign a user certificate for the Ed25519 key with the RSA key as CA, attached to the key.
	var signed signResult
	c.must("POST", "/api/keys/"+rsaKey.ID+"/sign", map[string]any{"subjectKeyId": ed.ID, "identity": "admin",
		"principals": []string{"admin", "root"}, "attach": true}, &signed)
	if !signed.Attached || signed.Info.Type != "user" || signed.Info.CAFingerprint != rsaKey.Fingerprint {
		t.Fatalf("%+v", signed)
	}
	var withCert keyJSON
	c.must("GET", "/api/keys/"+ed.ID, nil, &withCert)
	if withCert.CertificateInfo == nil || withCert.CertificateInfo.KeyID != "admin" || withCert.CertificateInfo.Status != certValid {
		t.Fatalf("%+v", withCert)
	}
	if st, code := c.code("PATCH", "/api/keys/"+rsaKey.ID, map[string]any{"certificate": withCert.Certificate}); st != 400 || code != "certificate_mismatch" {
		t.Fatalf("cert on the wrong key: %d %s", st, code)
	}
	c.must("PATCH", "/api/keys/"+ed.ID, map[string]any{"certificate": ""}, &upd)
	if upd.Certificate != "" || upd.CertificateInfo != nil {
		t.Fatal("detach")
	}
	c.must("PATCH", "/api/keys/"+ed.ID, map[string]any{"certificate": withCert.Certificate}, &upd)

	// Passphrase management: forget, remember (wrong / right), change, remove.
	c.must("PATCH", "/api/keys/"+rsaKey.ID, map[string]any{"passphrase": ""}, &upd)
	if upd.PassphraseSaved || !upd.HasPassphrase {
		t.Fatalf("forget: %+v", upd)
	}
	if st, code := c.code("GET", "/api/keys/"+rsaKey.ID+"/export?format=openssh", nil); st != 400 || code != codePassphraseRequired {
		t.Fatalf("export without remembered passphrase: %d %s", st, code)
	}
	if st, code := c.code("PATCH", "/api/keys/"+rsaKey.ID, map[string]any{"passphrase": "nope"}); st != 400 || code != codeWrongPassphrase {
		t.Fatalf("wrong passphrase: %d %s", st, code)
	}
	c.must("PATCH", "/api/keys/"+rsaKey.ID, map[string]any{"passphrase": "pw-1"}, &upd)
	if !upd.PassphraseSaved {
		t.Fatal("remember")
	}
	c.must("PATCH", "/api/keys/"+rsaKey.ID, map[string]any{"newPassphrase": "pw-2", "rememberPassphrase": false}, &upd)
	if !upd.HasPassphrase || upd.PassphraseSaved {
		t.Fatalf("change: %+v", upd)
	}
	c.must("PATCH", "/api/keys/"+rsaKey.ID, map[string]any{"passphrase": "pw-2", "newPassphrase": ""}, &upd)
	if upd.HasPassphrase || upd.PassphraseSaved || upd.Fingerprint != rsaKey.Fingerprint {
		t.Fatalf("remove passphrase: %+v", upd)
	}

	// Usage counts and deletion (references are unlinked).
	conn := &model.Connection{OwnerID: e.admin.ID, Name: "srv", Protocol: model.ProtoSSH, Host: "h", Port: 22, KeyID: ed.ID}
	conn.Normalize()
	if err := e.d.Store.Connections.Create(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	var list []keyJSON
	c.must("GET", "/api/keys", nil, &list)
	if len(list) != 2 || list[1].UsedBy.Connections != 1 && list[0].UsedBy.Connections != 1 {
		t.Fatalf("list %+v", list)
	}
	c.must("DELETE", "/api/keys/"+ed.ID, nil, nil)
	if got, _ := e.d.Store.Connections.Get(context.Background(), conn.ID); got.KeyID != "" {
		t.Fatal("connection still references the deleted key")
	}
	if st, _ := c.code("GET", "/api/keys/"+ed.ID, nil); st != 404 {
		t.Fatalf("deleted key: %d", st)
	}

	// Keys are private: another user sees nothing.
	other, _ := e.user("bob")
	if st, _ := other.code("GET", "/api/keys/"+rsaKey.ID, nil); st != 404 {
		t.Fatalf("foreign key visible: %d", st)
	}
	if st, _ := other.code("POST", "/api/keys/"+rsaKey.ID+"/export", map[string]any{"format": "openssh"}); st != 404 {
		t.Fatalf("foreign export: %d", st)
	}
}

func TestImportEndpoint(t *testing.T) {
	e := newTestEnv(t, config.ModeDesktop)
	c := e.c
	text := string(fixture(t, "openssh_ed25519_enc"))
	if st, code := c.code("POST", "/api/keys/import", map[string]any{"privateKey": text}); st != 400 || code != codePassphraseRequired {
		t.Fatalf("%d %s", st, code)
	}
	if st, code := c.code("POST", "/api/keys/import", map[string]any{"privateKey": text, "passphrase": "x"}); st != 400 || code != codeWrongPassphrase {
		t.Fatalf("%d %s", st, code)
	}
	var k keyJSON
	c.must("POST", "/api/keys/import", map[string]any{"privateKey": text, "passphrase": "testpass", "rememberPassphrase": false}, &k)
	if k.Name != "enc ed25519" || k.Comment != "enc ed25519" || !k.HasPassphrase || k.PassphraseSaved {
		t.Fatalf("%+v", k)
	}
	if st, code := c.code("POST", "/api/keys/import", map[string]any{"privateKey": text, "passphrase": "testpass"}); st != 409 || code != "key_exists" {
		t.Fatalf("duplicate: %d %s", st, code)
	}
	// PPK and encrypted PKCS#8 (normalized to OpenSSH for sshx) with an attached certificate.
	var p keyJSON
	c.must("POST", "/api/keys/import", map[string]any{"name": "putty", "privateKey": string(fixture(t, "putty_v3_rsa_enc.ppk")), "passphrase": "testkey"}, &p)
	if p.Type != "rsa" || !p.PassphraseSaved {
		t.Fatalf("%+v", p)
	}
	var withCert keyJSON
	c.must("POST", "/api/keys/import", map[string]any{"privateKey": string(fixture(t, "openssh_ed25519")),
		"certificate": string(fixture(t, "openssh_ed25519-cert.pub"))}, &withCert)
	if withCert.CertificateInfo == nil || withCert.CertificateInfo.KeyID != "alice@corp" {
		t.Fatalf("%+v", withCert)
	}
	var p8 keyJSON
	c.must("POST", "/api/keys/import", map[string]any{"privateKey": string(fixture(t, "pkcs8_ed25519_enc.pem")), "passphrase": "testpass"}, &p8)
	stored, pass, err := e.d.KeyMaterial(context.Background(), e.admin, p8.ID)
	if err != nil || !strings.Contains(string(stored), "OPENSSH PRIVATE KEY") || pass != "testpass" {
		t.Fatalf("normalized: %v %q", err, stored[:40])
	}
	if st, code := c.code("POST", "/api/keys/import", map[string]any{"privateKey": string(fixture(t, "ca.pub"))}); st != 400 || code != "public_key_only" {
		t.Fatalf("%d %s", st, code)
	}

	// Inspect (import dialog preview) and convert (no storage).
	var ins inspectResult
	c.must("POST", "/api/keys/inspect", map[string]any{"text": text}, &ins)
	if ins.Kind != "private" || !ins.NeedsPassphrase || ins.Fingerprint != k.Fingerprint || ins.ExistingKeyID != k.ID {
		t.Fatalf("%+v", ins)
	}
	c.must("POST", "/api/keys/inspect", map[string]any{"text": string(fixture(t, "openssh_ed25519-cert.pub"))}, &ins)
	if ins.Kind != "certificate" || ins.Certificate == nil || ins.Certificate.Principals[0] != "alice" {
		t.Fatalf("%+v", ins)
	}
	var conv exportResult
	c.must("POST", "/api/keys/convert", map[string]any{"privateKey": string(fixture(t, "putty_v2_ed25519_enc.ppk")),
		"passphrase": "testkey", "format": "openssh", "name": "converted"}, &conv)
	if pk, err := parseKey([]byte(conv.Content), ""); err != nil || pk.Comment != "a@b" || conv.Filename != "converted" {
		t.Fatalf("convert %v %+v", err, conv)
	}
}

func TestDrafts(t *testing.T) {
	e := newTestEnv(t, config.ModeDesktop)
	c := e.c
	var d draftView
	c.must("POST", "/api/keys/generate", map[string]any{"type": "ecdsa", "bits": 384, "passphrase": "pp", "store": false}, &d)
	if d.DraftID == "" || d.Type != "ecdsa" || d.Bits != 384 || !d.HasPassphrase {
		t.Fatalf("%+v", d)
	}
	var list []keyJSON
	c.must("GET", "/api/keys", nil, &list)
	if len(list) != 0 {
		t.Fatal("draft was stored")
	}
	var res exportResult
	c.must("POST", "/api/keys/drafts/"+d.DraftID+"/export", map[string]any{"format": "ppk"}, &res)
	if pk, err := parseKey([]byte(res.Content), "pp"); err != nil || !sameKey(pk.Pub, mustParsePub(t, d.PublicKey)) {
		t.Fatalf("draft export: %v", err)
	}
	var k keyJSON
	c.must("POST", "/api/keys/drafts/"+d.DraftID+"/store", map[string]any{"name": "From draft"}, &k)
	if k.Fingerprint != d.Fingerprint || !k.PassphraseSaved {
		t.Fatalf("%+v", k)
	}
	if st, _ := c.code("POST", "/api/keys/drafts/"+d.DraftID+"/export", map[string]any{"format": "public"}); st != 404 {
		t.Fatalf("stored draft still exists: %d", st)
	}
	other, _ := e.user("eve")
	c.must("POST", "/api/keys/generate", map[string]any{"store": false}, &d)
	if st, _ := other.code("POST", "/api/keys/drafts/"+d.DraftID+"/store", nil); st != 404 {
		t.Fatalf("foreign draft: %d", st)
	}
}

func mustParsePub(t *testing.T, s string) ssh.PublicKey {
	t.Helper()
	pk, _, err := parsePublicKeyText(s)
	if err != nil {
		t.Fatal(err)
	}
	return pk
}

func TestKnownHostsEndpoints(t *testing.T) {
	e := newTestEnv(t, config.ModeServer)
	c := e.c
	hostPub := strings.TrimSpace(string(fixture(t, "hostkey.pub")))
	edPub := strings.TrimSpace(string(fixture(t, "openssh_ed25519.pub")))

	var kh model.KnownHost
	c.must("POST", "/api/known-hosts", map[string]any{"host": "[Srv.Example.com]:2222", "publicKey": hostPub}, &kh)
	if kh.Host != "srv.example.com" || kh.Port != 2222 || kh.KeyType != "ecdsa-sha2-nistp256" || !strings.HasPrefix(kh.Fingerprint, "SHA256:") {
		t.Fatalf("%+v", kh)
	}
	if st, code := c.code("POST", "/api/known-hosts", map[string]any{"host": "srv.example.com", "port": 2222, "publicKey": hostPub}); st != 409 || code != "already_known" {
		t.Fatalf("%d %s", st, code)
	}
	if st, code := c.code("POST", "/api/known-hosts", map[string]any{"host": "*.example.com", "publicKey": hostPub}); st != 400 {
		t.Fatalf("pattern accepted: %d %s", st, code)
	}

	// Import OpenSSH text: plain, [host]:port, hashed, pattern, markers, invalid.
	text := strings.Join([]string{
		"a.example.com,10.1.1.1 " + edPub,
		"[srv.example.com]:2222 " + edPub, // new type for a known host → added
		string(fixture(t, "known_hosts_hashed")),
		"*.wild.example.com " + edPub,
		"@cert-authority *.example.com " + strings.TrimSpace(string(fixture(t, "ca.pub"))),
		"@revoked * " + edPub,
		"broken line",
	}, "\n")
	var res knownHostsImportResult
	c.must("POST", "/api/known-hosts/import", map[string]any{"text": text}, &res)
	if res.Format != "openssh" || res.Added != 3 || res.Hashed != 2 || res.Patterns != 1 || res.Markers != 2 || res.Invalid != 1 || len(res.Errors) != 1 {
		t.Fatalf("%+v", res)
	}
	c.must("POST", "/api/known-hosts/import", map[string]any{"text": text}, &res)
	if res.Added != 0 || res.Skipped != 5 {
		t.Fatalf("re-import %+v", res)
	}
	// The markers reach sshx.
	if cas := e.h.markers.HostAuthorities("x.example.com", 22); len(cas) != 1 || !e.h.markers.IsRevoked("any", 22, mustParsePub(t, edPub)) {
		t.Fatal("markers not effective")
	}

	var list []model.KnownHost
	c.must("GET", "/api/known-hosts?q=example", nil, &list)
	if len(list) != 3 {
		t.Fatalf("search: %d", len(list))
	}
	st, body, _ := c.do("GET", "/api/known-hosts/export?hashed=1", nil)
	if st != 200 || !strings.Contains(string(body), "@cert-authority *.example.com ") || !strings.Contains(string(body), "|1|") ||
		strings.Contains(string(body), "a.example.com ") {
		t.Fatalf("export %s", body)
	}
	back, errs := parseOpenSSHKnownHosts(string(body))
	if len(errs) != 0 || len(back) != 6 {
		t.Fatalf("export re-parse %d %v", len(back), errs)
	}

	var markers []HostKeyMarker
	c.must("GET", "/api/known-hosts/markers", nil, &markers)
	if len(markers) != 2 || markers[0].Marker != markerCertAuthority {
		t.Fatalf("%+v", markers)
	}

	// Non-admins may read but not change the global trust store in server mode.
	u, _ := e.user("carol")
	if st := u.json("GET", "/api/known-hosts", nil, nil); st != 200 {
		t.Fatalf("read: %d", st)
	}
	for _, r := range []struct{ m, p string }{{"POST", "/api/known-hosts"}, {"POST", "/api/known-hosts/import"},
		{"DELETE", "/api/known-hosts/" + kh.ID}, {"POST", "/api/known-hosts/markers"}, {"DELETE", "/api/known-hosts/markers/" + markers[0].ID},
		{"POST", "/api/known-hosts/bulk-delete"}} {
		if st, _ := u.code(r.m, r.p, map[string]any{"text": "x", "ids": []string{kh.ID}}); st != 403 {
			t.Fatalf("%s %s by a user: %d", r.m, r.p, st)
		}
	}

	c.must("DELETE", "/api/known-hosts/markers/"+markers[1].ID, nil, nil)
	c.must("DELETE", "/api/known-hosts/"+kh.ID, nil, nil)
	var del map[string]int
	ids := []string{}
	c.must("GET", "/api/known-hosts", nil, &list)
	for _, k := range list {
		ids = append(ids, k.ID)
	}
	c.must("POST", "/api/known-hosts/bulk-delete", map[string]any{"ids": ids}, &del)
	if del["deleted"] != len(ids) {
		t.Fatalf("%v", del)
	}

	// PuTTY registry export (auto-detected).
	x, y := edwardsX(t, mustParsePub(t, edPub).(ssh.CryptoPublicKey).CryptoPublicKey().(ed25519.PublicKey))
	reg := "[HKEY_CURRENT_USER\\Software\\SimonTatham\\PuTTY\\SshHostKeys]\n" +
		fmt.Sprintf(`"ssh-ed25519@2200:putty.example.com"="0x%x,0x%x"`, x, y) + "\n"
	c.must("POST", "/api/known-hosts/import", map[string]any{"text": reg}, &res)
	if res.Format != "putty" || res.Added != 1 {
		t.Fatalf("putty import %+v", res)
	}
	c.must("GET", "/api/known-hosts", nil, &list)
	if len(list) != 1 || list[0].Host != "putty.example.com" || list[0].Port != 2200 || list[0].Fingerprint != ssh.FingerprintSHA256(mustParsePub(t, edPub)) {
		t.Fatalf("%+v", list)
	}
}

func TestAgentEndpointsServerMode(t *testing.T) {
	e := newTestEnv(t, config.ModeServer)
	var st agentStatus
	e.c.must("GET", "/api/agent/status", nil, &st)
	if st.Supported || st.Running || st.Reason == "" {
		t.Fatalf("%+v", st)
	}
	if code, _ := e.c.code("POST", "/api/agent/start", nil); code != 403 {
		t.Fatalf("start in server mode: %d", code)
	}
	var keys []agentKeyView
	e.c.must("GET", "/api/agent/keys", nil, &keys)
	if keys == nil {
		t.Fatal("keys must be a list")
	}
}

// TestCrossUserIsolation: every endpoint that takes a key ID treats another user's key as nonexistent — as the key
// itself, as a certificate subject or CA, as a marker key and in the agent — and nothing leaks into the lists.
func TestCrossUserIsolation(t *testing.T) {
	e := newTestEnv(t, config.ModeDesktop)
	var mine keyJSON
	e.c.must("POST", "/api/keys/generate", map[string]any{"name": "admin key"}, &mine)
	bob, bobUser := e.user("bob")
	var bobKey keyJSON
	bob.must("POST", "/api/keys/generate", map[string]any{"name": "bob key"}, &bobKey)
	conn := &model.Connection{OwnerID: bobUser.ID, Name: "bob server", Protocol: model.ProtoSSH, Host: "127.0.0.1", Port: 1,
		Username: "bob", AuthMethod: model.AuthPassword, Options: model.Options{}}
	conn.Normalize()
	e.saveConn(t, conn, nil)

	for _, tc := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"GET", "/api/keys/" + mine.ID, nil, 404},
		{"PATCH", "/api/keys/" + mine.ID, map[string]any{"name": "stolen"}, 404},
		{"PATCH", "/api/keys/" + mine.ID, map[string]any{"newPassphrase": "x"}, 404},
		{"DELETE", "/api/keys/" + mine.ID, nil, 404},
		{"GET", "/api/keys/" + mine.ID + "/export?format=openssh", nil, 404},
		{"GET", "/api/keys/" + mine.ID + "/export?format=public", nil, 404},
		{"POST", "/api/keys/" + mine.ID + "/export", map[string]any{"format": "ppk"}, 404},
		{"POST", "/api/keys/" + mine.ID + "/install", map[string]any{"connectionId": conn.ID}, 404},
		{"POST", "/api/keys/" + mine.ID + "/sign", map[string]any{"subjectKeyId": bobKey.ID, "principals": []string{"bob"}}, 404},
		{"POST", "/api/keys/" + bobKey.ID + "/sign", map[string]any{"subjectKeyId": mine.ID, "principals": []string{"bob"}, "attach": true}, 400},
		{"POST", "/api/known-hosts/markers", map[string]any{"marker": "cert-authority", "hosts": "*", "keyId": mine.ID}, 404},
		{"DELETE", "/api/agent/keys/k:" + mine.ID, nil, 404},
	} {
		if st, code := bob.code(tc.method, tc.path, tc.body); st != tc.want {
			t.Errorf("%s %s: %d %s (want %d)", tc.method, tc.path, st, code, tc.want)
		}
	}
	var list []keyJSON
	bob.must("GET", "/api/keys", nil, &list)
	if len(list) != 1 || list[0].ID != bobKey.ID {
		t.Fatalf("bob's keys: %+v", list)
	}
	var agentKeys []agentKeyView
	bob.must("GET", "/api/agent/keys", nil, &agentKeys)
	for _, k := range agentKeys {
		if k.KeyID == mine.ID {
			t.Fatal("another user's key listed in the agent")
		}
	}
	// The admin's key is untouched and carries no certificate from bob.
	var after keyJSON
	e.c.must("GET", "/api/keys/"+mine.ID, nil, &after)
	if after.Name != "admin key" || after.Certificate != "" {
		t.Fatalf("%+v", after)
	}
}
