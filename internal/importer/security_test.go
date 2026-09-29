package importer

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
)

func httpStatus(err error) int {
	var he *httpx.HTTPError
	if errors.As(err, &he) {
		return he.Status
	}
	return 0
}

func TestDecryptRejectsHostileKDFParameters(t *testing.T) {
	env, err := encrypt([]byte("x"), "long enough pass", payloadExport)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(e *encEnvelope){
		func(e *encEnvelope) { e.KDF.MemKiB = 4 << 20 }, // 4 GiB: would OOM the server
		func(e *encEnvelope) { e.KDF.Time = 1000 },
		func(e *encEnvelope) { e.KDF.Threads = 255 },
		func(e *encEnvelope) { e.KDF.Alg = "scrypt" },
		func(e *encEnvelope) { e.Nonce = base64.StdEncoding.EncodeToString([]byte("short")) },
		func(e *encEnvelope) { e.KDF.Salt = "" },
	} {
		var e encEnvelope
		if err := json.Unmarshal(env, &e); err != nil {
			t.Fatal(err)
		}
		mutate(&e)
		raw, _ := json.Marshal(e)
		start := time.Now()
		if _, _, err := decrypt(raw, "long enough pass"); err == nil || httpStatus(err) != 400 {
			t.Errorf("hostile envelope accepted or wrong error: %v", err)
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("rejection took %v (KDF ran?)", time.Since(start))
		}
	}
}

func TestDecryptWrongPassphraseIs403(t *testing.T) {
	env, _ := encrypt([]byte("x"), "right passphrase", payloadExport)
	_, _, err := decrypt(env, "wrong passphrase")
	var he *httpx.HTTPError
	if !errors.As(err, &he) || he.Status != 403 || he.Code != "wrong_password" {
		t.Fatalf("want 403 wrong_password, got %v", err)
	}
}

func TestNewPassphraseMinimum(t *testing.T) {
	if _, err := encrypt([]byte("x"), "short", payloadExport); err == nil {
		t.Fatal("a 5-character passphrase must be refused")
	}
	if _, err := encrypt([]byte("x"), "12345678", payloadExport); err != nil {
		t.Fatalf("8 characters must be accepted: %v", err)
	}
}

func zipOf(t *testing.T, files map[string][]byte, order ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range order {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUnzipBackupHardening(t *testing.T) {
	db := append([]byte(backupMagicSQLite), make([]byte, 100)...)
	// Well-formed.
	if gotDB, key, err := unzipBackup(zipOf(t, map[string][]byte{"nexterm.db": db, "system.key": []byte("k")}, "nexterm.db", "system.key")); err != nil || len(gotDB) != len(db) || string(key) != "k" {
		t.Fatalf("valid archive: %v", err)
	}
	cases := map[string][]byte{
		"unexpected entry":  zipOf(t, map[string][]byte{"nexterm.db": db, "../../etc/passwd": []byte("x")}, "nexterm.db", "../../etc/passwd"),
		"nested db":         zipOf(t, map[string][]byte{"a/nexterm.db": db}, "a/nexterm.db"),
		"missing db":        zipOf(t, map[string][]byte{"system.key": []byte("k")}, "system.key"),
		"oversized key":     zipOf(t, map[string][]byte{"nexterm.db": db, "system.key": bytes.Repeat([]byte("k"), maxSystemKeyBytes+1)}, "nexterm.db", "system.key"),
		"not a zip":         []byte("PK\x03\x04garbage"),
		"too many entries":  zipOf(t, map[string][]byte{"nexterm.db": db, "a": nil, "b": nil, "c": nil, "d": nil}, "nexterm.db", "a", "b", "c", "d"),
		"duplicate entries": dupZip(t, db),
	}
	for name, archive := range cases {
		if _, _, err := unzipBackup(archive); err == nil || httpStatus(err) != 400 {
			t.Errorf("%s: want 400, got %v", name, err)
		}
	}
}

// dupZip builds an archive containing nexterm.db twice.
func dupZip(t *testing.T, db []byte) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < 2; i++ {
		w, _ := zw.Create("nexterm.db")
		_, _ = w.Write(db)
	}
	_ = zw.Close()
	return buf.Bytes()
}

func TestUnzipBackupLyingHeaderIsNotTruncated(t *testing.T) {
	// A system.key entry whose real content exceeds the limit must fail, not be cut silently: forge the declared
	// size in the central directory to look small.
	big := bytes.Repeat([]byte("A"), maxSystemKeyBytes*4)
	db := append([]byte(backupMagicSQLite), 1)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range []struct {
		name string
		data []byte
	}{{"nexterm.db", db}, {"system.key", big}} {
		w, _ := zw.CreateHeader(&zip.FileHeader{Name: f.name, Method: zip.Deflate})
		_, _ = w.Write(f.data)
	}
	_ = zw.Close()
	raw := buf.Bytes()
	// Patch the central-directory uncompressed size (offset 24 in each central header) of system.key to 10.
	cd := bytes.LastIndex(raw, []byte("PK\x01\x02"))
	raw[cd+24], raw[cd+25], raw[cd+26], raw[cd+27] = 10, 0, 0, 0
	if _, key, err := unzipBackup(raw); err == nil {
		t.Fatalf("a lying header yielded %d key bytes without an error", len(key))
	}
}

func TestPlaintextJSONSecretsIgnored(t *testing.T) {
	doc := `{"format":"nexterm-export","version":1,"connections":[{"id":"c1","name":"x","protocol":"ssh","host":"x.example.com","secrets":{"password":"plain"}}],
	"keys":[{"id":"k1","name":"k","privateKey":"-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----"}],
	"identities":[{"id":"i1","name":"id","secrets":{"password":"plain2"}}]}`
	p, err := parseSource(fmtJSON, []byte(doc), previewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.conns[0].secrets) != 0 || len(p.keys) != 0 || len(p.identities[0].secrets) != 0 {
		t.Fatalf("plaintext secrets/keys were accepted: conn=%v keys=%d ident=%v", p.conns[0].secrets, len(p.keys), p.identities[0].secrets)
	}
	if !hasWarning(p.warnings, "ignored") {
		t.Errorf("missing warning: %v", p.warnings)
	}
}

func TestEncryptedJSONSecretsAccepted(t *testing.T) {
	doc := `{"format":"nexterm-export","version":1,"connections":[{"id":"c1","name":"x","protocol":"ssh","host":"x.example.com","secrets":{"password":"s3cret","bad name!":"v"}}]}`
	env, err := encrypt([]byte(doc), "export passphrase", payloadExport)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseSource(fmtAuto, env, previewOptions{}); httpStatus(err) != 422 {
		t.Fatalf("missing passphrase must be 422, got %v", err)
	}
	p, err := parseSource(fmtAuto, env, previewOptions{Passphrase: "export passphrase"})
	if err != nil {
		t.Fatal(err)
	}
	s := p.conns[0].secrets
	if s["password"] != "s3cret" || len(s) != 1 {
		t.Fatalf("secrets = %v (invalid names must be dropped)", s)
	}
}

func TestJSONOptionReferencesRemapped(t *testing.T) {
	doc := `{"format":"nexterm-export","version":1,"connections":[
	 {"id":"aaaaaaaaaaaaaaaaaaaa","name":"bastion","protocol":"ssh","host":"b.example.com"},
	 {"id":"bbbbbbbbbbbbbbbbbbbb","name":"app","protocol":"ssh","host":"app.internal","options":{"jumpHosts":["aaaaaaaaaaaaaaaaaaaa","ops@edge.example.com:2022","cccccccccccccccccccc"],"_managedBy":"ssh-config"}},
	 {"id":"dddddddddddddddddddd","name":"desk","protocol":"vnc","host":"desk.internal","options":{"sshTunnelVia":"aaaaaaaaaaaaaaaaaaaa"}}]}`
	p, err := parseSource(fmtJSON, []byte(doc), previewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bastion := findConn(t, p, "bastion")
	app := findConn(t, p, "app")
	if app.conn.Options.Has("jumpHosts") || app.conn.Options.Has("_managedBy") {
		t.Errorf("raw ids / sync markers must not survive: %v", app.conn.Options)
	}
	if len(app.hops) != 3 || app.hops[0].ref != bastion.tempID || app.hops[1].spec() != "ops@edge.example.com:2022" || app.hops[2].existingID != "cccccccccccccccccccc" {
		t.Errorf("app hops = %+v", app.hops)
	}
	desk := findConn(t, p, "desk")
	if len(desk.hops) != 1 || desk.hops[0].ref != bastion.tempID || desk.conn.Options.Has("sshTunnelVia") {
		t.Errorf("desk gateway = %+v %v", desk.hops, desk.conn.Options)
	}
}

func TestXMLEntityAttacksRejected(t *testing.T) {
	bomb := `<?xml version="1.0"?>
<!DOCTYPE lolz [<!ENTITY lol "lol"><!ENTITY lol2 "&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;">]>
<mrng:Connections xmlns:mrng="http://mremoteng.org" Name="&lol2;"><Node Name="&lol2;" Type="Connection" Protocol="SSH2" Hostname="h"/></mrng:Connections>`
	if _, err := parseSource(fmtMRemoteNG, []byte(bomb), previewOptions{}); err == nil {
		t.Errorf("entity expansion must be refused")
	}
	xxe := `<?xml version="1.0"?><!DOCTYPE x [<!ENTITY xxe SYSTEM "file:///etc/passwd">]>
<FileZilla3><Servers><Server><Host>&xxe;</Host><Protocol>1</Protocol></Server></Servers></FileZilla3>`
	if _, err := parseSource(fmtFileZilla, []byte(xxe), previewOptions{}); err == nil {
		t.Errorf("external entity must be refused")
	}
}

func TestMRemoteNGEncryptedAndInheritance(t *testing.T) {
	enc := `<?xml version="1.0" encoding="utf-8"?><mrng:Connections xmlns:mrng="http://mremoteng.org" Name="Connections" FullFileEncryption="true" ConfVersion="2.6">AAAAbase64==</mrng:Connections>`
	if _, err := parseSource(fmtMRemoteNG, []byte(enc), previewOptions{}); err == nil || !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("a fully encrypted file needs a clear message: %v", err)
	}
	inh := `<?xml version="1.0" encoding="utf-8"?><mrng:Connections xmlns:mrng="http://mremoteng.org" Name="Connections">
<Node Name="Win" Type="Container" Username="ops" Domain="CORP" Protocol="RDP">
  <Node Name="dc1" Type="Connection" Hostname="dc1.corp" Protocol="RDP" InheritUsername="true" InheritDomain="true" Resolution="Res1280x1024" Colors="Colors16Bit" RedirectDiskDrives="True"/>
  <Node Name="web" Type="Connection" Hostname="web.corp" Protocol="HTTPS"/>
</Node></mrng:Connections>`
	p, err := parseSource(fmtAuto, []byte(inh), previewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dc := findConn(t, p, "dc1").conn
	if dc.Username != "ops" || dc.Options.String("domain") != "CORP" || dc.Options.Int("width") != 1280 || dc.Options.Int("colorDepth") != 16 || !dc.Options.Bool("enableDrive") {
		t.Errorf("inherited / mapped RDP settings: %+v", dc)
	}
	if p.unsupported != 1 {
		t.Errorf("HTTPS node must be reported unsupported")
	}
}

func TestFileZillaProtocolsAndRemoteDir(t *testing.T) {
	src := `<?xml version="1.0" encoding="UTF-8"?><FileZilla3><Servers>
<Server><Host>s3.example.com</Host><Protocol>7</Protocol><User>AKIAX</User><Name>bucket</Name></Server>
<Server><Host>dav.example.com</Host><Protocol>9</Protocol><Name>dav</Name></Server>
<Server><Host>plain.example.com</Host><Protocol>6</Protocol><Name>plain</Name></Server>
<Server><Host>sftp.example.com</Host><Protocol>1</Protocol><Logontype>5</Logontype><Keyfile>/home/me/.ssh/id_ed25519</Keyfile><Name>keyed</Name><RemoteDir>1 0 4 home 10 my project</RemoteDir></Server>
</Servers></FileZilla3>`
	p, err := parseSource(fmtAuto, []byte(src), previewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b := findConn(t, p, "bucket").conn
	if b.Protocol != model.ProtoS3 || b.Options.String("accessKeyId") != "AKIAX" || b.Username != "" {
		t.Errorf("S3: %+v", b)
	}
	if pl := findConn(t, p, "plain").conn; pl.Options.String("ftpTls") != "none" {
		t.Errorf("insecure FTP: %v", pl.Options)
	}
	k := findConn(t, p, "keyed")
	if k.keyPath != "/home/me/.ssh/id_ed25519" || k.conn.Options.String("initialPath") != "/home/my project" {
		t.Errorf("keyed: key=%q path=%q", k.keyPath, k.conn.Options.String("initialPath"))
	}
	if p.unsupported != 1 {
		t.Errorf("WebDAV must be unsupported, got %d", p.unsupported)
	}
}

func TestSecureCRTDecimalDwords(t *testing.T) {
	src := `<?xml version="1.0" encoding="UTF-8"?><VanDyke version="3.0"><key name="Sessions">
<key name="lab"><key name="r1"><dword name="[SSH2] Port">2222</dword><dword name="Port">23</dword><string name="Hostname">r1.lab</string><string name="Protocol Name">SSH2</string><string name="Identity Filename V2">C:\keys\r1.pem::rawkey</string></key></key>
<key name="t1"><dword name="[SSH2] Port">22</dword><dword name="Port">2323</dword><string name="Hostname">t1.lab</string><string name="Protocol Name">Telnet</string></key>
<key name="com"><string name="Protocol Name">Serial</string><string name="Com Port">COM5</string><dword name="Baud Rate">115200</dword></key>
</key></VanDyke>`
	p, err := parseSource(fmtAuto, []byte(src), previewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r1 := findConn(t, p, "r1")
	if r1.conn.Port != 2222 || r1.keyPath != `C:\keys\r1.pem` || folderPathOf(p, r1) != "lab" {
		t.Errorf("r1 (decimal dword, identity file): port=%d key=%q", r1.conn.Port, r1.keyPath)
	}
	if t1 := findConn(t, p, "t1").conn; t1.Protocol != model.ProtoTelnet || t1.Port != 2323 {
		t.Errorf("telnet uses its own port key: %+v", t1)
	}
	if com := findConn(t, p, "com").conn; com.Protocol != model.ProtoSerial || com.Options.String("device") != "COM5" || com.Options.Int("baud") != 115200 {
		t.Errorf("serial: %+v", com)
	}
}

func TestSSHConfigExportCannotBeInjected(t *testing.T) {
	conns := []*model.Connection{
		{ID: "a", Name: "ok", Protocol: model.ProtoSSH, Host: "ok.example.com", Port: 22, Username: "Jane Doe",
			Options: model.Options{"jumpHosts": []any{"j1", "hop@edge:2200"}, "forwards": []any{map[string]any{"type": "local", "bindPort": 8080, "destHost": "db", "destPort": 5432}}}},
		{ID: "b", Name: "evil", Protocol: model.ProtoSSH, Host: "x.example.com\n    ProxyCommand calc", Port: 22, Options: model.Options{}},
		{ID: "j1", Name: "jump box", Protocol: model.ProtoSSH, Host: "jump.example.com", Port: 22, Options: model.Options{}},
		{ID: "c", Name: "cmd", Protocol: model.ProtoSSH, Host: "c.example.com", Port: 22, Username: "u\nProxyCommand evil",
			Options: model.Options{"remoteCommand": "top\nProxyCommand x"}},
	}
	out := string(exportSSHConfigFile(conns, conns).Data)
	for _, bad := range []string{"ProxyCommand calc", "ProxyCommand evil", "ProxyCommand x"} {
		if strings.Contains(out, bad) {
			t.Fatalf("injected directive %q in:\n%s", bad, out)
		}
	}
	for _, want := range []string{`User "Jane Doe"`, "ProxyJump jump-box,hop@edge:2200", "LocalForward 8080 db:5432", "# skipped evil"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "ProxyJump") != 1 {
		t.Errorf("OpenSSH honours only the first ProxyJump; the chain must be one directive:\n%s", out)
	}
}

func TestCSVExportFormulaGuard(t *testing.T) {
	res := exportCSVFile([]*model.Connection{{Name: "=HYPERLINK(\"http://x\")", Host: "h", Protocol: "ssh", Port: 22, Username: "@admin", Tags: []string{"-1"}}}, nil)
	out := string(res.Data)
	if !strings.Contains(out, `'=HYPERLINK`) || !strings.Contains(out, "'@admin") || strings.Contains(out, "'-1") {
		t.Errorf("formula guard: %s", out)
	}
}

func TestCSVDelimiterSniffAndURIPassword(t *testing.T) {
	src := "name;host;user\nr1;ssh://root:hunter2@r1.example.com:2200;\n"
	p, err := parseSource(fmtAuto, []byte(src), previewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c := findConn(t, p, "r1").conn
	if c.Host != "r1.example.com" || c.Port != 2200 || c.Username != "root" {
		t.Errorf("URI host: %+v", c)
	}
	raw, _ := json.Marshal(p.conns)
	if strings.Contains(string(raw), "hunter2") || !hasWarning(p.warnings, "password") {
		t.Errorf("inline password leaked or unreported")
	}
}

func TestSanitizeRejectsBadEntries(t *testing.T) {
	src := `{"format":"nexterm-export","version":1,"connections":[
	 {"name":"bad host","protocol":"ssh","host":"a b"},
	 {"name":"bad proto","protocol":"SSH; rm -rf","host":"h"},
	 {"name":"n\nl","protocol":"ssh","host":"ok.example.com","port":70000,"authMethod":"magic","color":"` + strings.Repeat("x", 100) + `","icon":"https://evil/x.png","tags":["a","a","b"]}]}`
	p, err := parseSource(fmtJSON, []byte(src), previewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.conns) != 1 || p.unsupported != 2 {
		t.Fatalf("conns=%d unsupported=%d", len(p.conns), p.unsupported)
	}
	c := p.conns[0].conn
	if c.Name != "nl" || c.Port != 22 || c.AuthMethod != model.AuthAuto || c.Color != "" || c.Icon != "" || len(c.Tags) != 2 {
		t.Errorf("sanitized: %+v", c)
	}
}

func TestBuilderCapIsEnforcedEarly(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("[Bookmarks]\r\nSubRep=\r\nImgNum=42\r\n")
	for i := 0; i < maxImportItems+50; i++ {
		fmt.Fprintf(&sb, "[Bookmarks_%d]\r\nSubRep=f%d\r\nImgNum=41\r\nh%d= #109#0%%h%d.example.com%%22%%u%%\r\n", i+1, i, i, i)
	}
	start := time.Now()
	_, err := parseSource(fmtMobaXterm, []byte(sb.String()), previewOptions{})
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected the item cap, got %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("folder de-duplication is too slow for large inputs: %v", d)
	}
}
