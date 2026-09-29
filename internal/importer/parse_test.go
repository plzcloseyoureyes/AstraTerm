package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/termstead/termstead/internal/model"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func mustParse(t *testing.T, format, fixture string, opts previewOptions) *parsed {
	t.Helper()
	p, err := parseSource(format, loadFixture(t, fixture), opts)
	if err != nil {
		t.Fatalf("parse %s (%s): %v", fixture, format, err)
	}
	return p
}

// findConn returns the parsed connection with the given name (or fails).
func findConn(t *testing.T, p *parsed, name string) *pconn {
	t.Helper()
	for _, c := range p.conns {
		if c.conn.Name == name {
			return c
		}
	}
	names := make([]string, 0, len(p.conns))
	for _, c := range p.conns {
		names = append(names, c.conn.Name)
	}
	t.Fatalf("connection %q not found; have %v", name, names)
	return nil
}

// folderPathOf returns the "/"-joined folder path of a parsed connection.
func folderPathOf(p *parsed, pc *pconn) string {
	byID := map[string]*pfolder{}
	for _, f := range p.folders {
		byID[f.tempID] = f
	}
	var parts []string
	for id := pc.folderID; id != ""; {
		f := byID[id]
		if f == nil {
			break
		}
		parts = append([]string{f.name}, parts...)
		id = f.parentID
	}
	return strings.Join(parts, "/")
}

func TestParseMobaXterm(t *testing.T) {
	p := mustParse(t, fmtMobaXterm, "mobaxterm.mxtsessions", previewOptions{})

	web := findConn(t, p, "web1")
	if web.conn.Protocol != model.ProtoSSH || web.conn.Host != "web1.example.com" || web.conn.Port != 2222 || web.conn.Username != "deploy" {
		t.Errorf("web1 mismatch: %+v", web.conn)
	}
	if folderPathOf(p, web) != "Production" {
		t.Errorf("web1 folder = %q, want Production", folderPathOf(p, web))
	}
	if web.conn.Notes != "Primary web server" {
		t.Errorf("web1 notes = %q", web.conn.Notes)
	}
	if web.conn.Options.Bool("x11Forwarding") != true || web.conn.Options.Bool("compression") != true {
		t.Errorf("web1 options = %v", web.conn.Options)
	}

	db := findConn(t, p, "db1")
	if len(db.hops) != 1 || db.hops[0].spec() != "jumpuser@gw.example.com:2222" {
		t.Errorf("db1 hops = %+v", db.hops)
	}
	if db.keyPath == "" || !strings.Contains(db.keyPath, "id_ed25519") || strings.Contains(db.keyPath, "_CurrentDrive_") {
		t.Errorf("db1 keyPath = %q (drive placeholder not unescaped?)", db.keyPath)
	}
	if db.conn.AuthMethod != model.AuthKey {
		t.Errorf("db1 authMethod = %q", db.conn.AuthMethod)
	}

	sw := findConn(t, p, "switch1")
	if sw.conn.Protocol != model.ProtoTelnet || sw.conn.Port != 23 || folderPathOf(p, sw) != "Production/Network" {
		t.Errorf("switch1 mismatch: %+v folder=%s", sw.conn, folderPathOf(p, sw))
	}

	win := findConn(t, p, "winbox")
	if win.conn.Protocol != model.ProtoRDP || win.conn.Port != 3389 || win.conn.Username != "Administrator" {
		t.Errorf("winbox mismatch: %+v", win.conn)
	}
	if !win.conn.Options.Bool("enableDrive") {
		t.Errorf("winbox should redirect drive: %v", win.conn.Options)
	}

	con := findConn(t, p, "console")
	if con.conn.Protocol != model.ProtoVNC || con.conn.Port != 5901 || !con.conn.Options.Bool("viewOnly") {
		t.Errorf("console mismatch: %+v", con.conn)
	}

	bk := findConn(t, p, "backup")
	if bk.conn.Protocol != model.ProtoSFTP || bk.conn.Options.String("initialPath") != "/var/data" {
		t.Errorf("backup mismatch: %+v", bk.conn)
	}

	roamer := findConn(t, p, "roamer")
	if roamer.conn.Protocol != model.ProtoMosh {
		t.Errorf("roamer protocol = %q", roamer.conn.Protocol)
	}

	serial := findConn(t, p, "console-serial")
	if serial.conn.Protocol != model.ProtoSerial || serial.conn.Options.String("device") != "COM3" {
		t.Errorf("serial mismatch: %+v", serial.conn)
	}
	if serial.conn.Options.Int("baud") != 9600 {
		t.Errorf("serial baud = %d", serial.conn.Options.Int("baud"))
	}

	// The local shell session is unsupported and must be reported, not imported.
	for _, c := range p.conns {
		if c.conn.Name == "localsh" {
			t.Errorf("localsh (Shell) should not be imported")
		}
	}
	if p.unsupported == 0 {
		t.Errorf("expected the Shell session to be counted unsupported")
	}
	// A password-warning must be present (secrets are never in the file).
	if !hasWarning(p.warnings, "password") {
		t.Errorf("expected a warning about passwords not being imported; got %v", p.warnings)
	}
}

func TestParsePuttyReg(t *testing.T) {
	p := mustParse(t, fmtPuttyReg, "putty.reg", previewOptions{})
	web := findConn(t, p, "Prod Web") // %20 decoded
	if web.conn.Host != "web1.example.com" || web.conn.Port != 22 || web.conn.Username != "deploy" {
		t.Errorf("Prod Web mismatch: %+v", web.conn)
	}
	if folderPathOf(p, web) != "Production/Web" {
		t.Errorf("Prod Web folder = %q", folderPathOf(p, web))
	}
	if !web.conn.Options.Bool("compression") {
		t.Errorf("compression not set")
	}
	proxy := web.conn.Options.Map("proxy")
	if proxy == nil || proxy["type"] != "socks5" || proxy["host"] != "socks.example.com" {
		t.Errorf("proxy mismatch: %v", proxy)
	}
	fwds, _ := web.conn.Options["forwards"].([]map[string]any)
	if len(fwds) != 2 {
		t.Fatalf("forwards = %v", web.conn.Options["forwards"])
	}
	tel := findConn(t, p, "Legacy Telnet")
	if tel.conn.Protocol != model.ProtoTelnet || tel.conn.Port != 23 {
		t.Errorf("Legacy Telnet mismatch: %+v", tel.conn)
	}
	ser := findConn(t, p, "SerialConsole")
	if ser.conn.Protocol != model.ProtoSerial || ser.conn.Options.String("device") != "COM4" || ser.conn.Options.Int("baud") != 115200 {
		t.Errorf("SerialConsole mismatch: %+v", ser.conn)
	}
}

func TestParsePuttyRegUTF16(t *testing.T) {
	// Real regedit v5 exports are UTF-16LE with a BOM.
	src := loadFixture(t, "putty.reg")
	utf16 := toUTF16LE(string(src))
	p, err := parseSource(fmtAuto, utf16, previewOptions{})
	if err != nil {
		t.Fatalf("autodetect+parse UTF-16 putty: %v", err)
	}
	if p.format != fmtPuttyReg {
		t.Errorf("detected format = %q, want putty_reg", p.format)
	}
	findConn(t, p, "Prod Web")
}

func TestParseSSHConfig(t *testing.T) {
	p := mustParse(t, fmtSSHConfig, "ssh_config", previewOptions{})
	// web1/web2 expand %h; db-* is a wildcard and must be excluded.
	for _, c := range p.conns {
		if c.conn.Host == "should-not-import.example.com" {
			t.Errorf("wildcard host db-* must not be imported")
		}
	}
	web1 := findConn(t, p, "web1")
	if web1.conn.Host != "web1.example.com" {
		t.Errorf("web1 host = %q (%%h not expanded?)", web1.conn.Host)
	}
	if web1.conn.Port != 22 { // inherited default from implicit block? Port not set → default 22
		t.Errorf("web1 port = %d", web1.conn.Port)
	}
	bastionTemp := findConn(t, p, "bastion").tempID
	if len(web1.hops) != 1 || web1.hops[0].ref != bastionTemp {
		t.Errorf("web1 ProxyJump bastion must reference the imported bastion alias: %+v", web1.hops)
	}
	if web1.conn.Options.Bool("useAgent") {
		t.Errorf("ForwardAgent must not switch on agent authentication")
	}
	if !web1.conn.Options.Bool("compression") { // from Host *
		t.Errorf("web1 should inherit Compression from Host *")
	}
	if web1.conn.Options.Int("keepAliveSec") != 30 {
		t.Errorf("web1 keepAliveSec = %d", web1.conn.Options.Int("keepAliveSec"))
	}
	if !web1.conn.Options.Bool("agentForwarding") {
		t.Errorf("web1 agentForwarding not set")
	}
	fwds, _ := web1.conn.Options["forwards"].([]map[string]any)
	if len(fwds) != 2 {
		t.Errorf("web1 forwards = %v", web1.conn.Options["forwards"])
	}
	env := web1.conn.Options.StringMap("env")
	if env["FOO"] != "bar" {
		t.Errorf("web1 env = %v", env)
	}
	bastion := findConn(t, p, "bastion")
	if bastion.conn.Port != 2222 || bastion.conn.Username != "jump" || bastion.keyPath == "" {
		t.Errorf("bastion mismatch: %+v key=%s", bastion.conn, bastion.keyPath)
	}
	secure := findConn(t, p, "secure")
	if ch := secure.conn.Options.Strings("ciphers"); len(ch) != 2 {
		t.Errorf("secure ciphers = %v", secure.conn.Options.Strings("ciphers"))
	}
	if secure.conn.Options.String("remoteCommand") != "tmux attach" {
		t.Errorf("secure remoteCommand = %q", secure.conn.Options.String("remoteCommand"))
	}
}

func TestParseTermiusCSV(t *testing.T) {
	p := mustParse(t, fmtTermiusCSV, "termius.csv", previewOptions{})
	web := findConn(t, p, "web-eu")
	if web.conn.Host != "10.0.0.1" || web.conn.Username != "deploy" || folderPathOf(p, web) != "Prod/EU" {
		t.Errorf("web-eu mismatch: %+v folder=%s", web.conn, folderPathOf(p, web))
	}
	if len(web.conn.Tags) != 2 {
		t.Errorf("web-eu tags = %v", web.conn.Tags)
	}
	uri := findConn(t, p, "uri-host")
	if uri.conn.Host != "jump.example.com" || uri.conn.Port != 2022 || uri.conn.Username != "ops" || uri.conn.Protocol != model.ProtoSSH {
		t.Errorf("uri-host not parsed from URI: %+v", uri.conn)
	}
	if !hasWarning(p.warnings, "asswords") {
		t.Errorf("expected a dropped-password warning; got %v", p.warnings)
	}
}

func TestParseGenericCSVSemicolon(t *testing.T) {
	p := mustParse(t, fmtCSV, "generic.csv", previewOptions{CSVDelim: ";"})
	r := findConn(t, p, "Router A")
	if r.conn.Host != "192.168.1.1" || r.conn.Username != "admin" || r.conn.Protocol != model.ProtoSSH {
		t.Errorf("Router A mismatch: %+v", r.conn)
	}
	if folderPathOf(p, r) != "Network/Core" {
		t.Errorf("Router A folder = %q", folderPathOf(p, r))
	}
	v := findConn(t, p, "Old VNC")
	if v.conn.Protocol != model.ProtoVNC || v.conn.Port != 5900 {
		t.Errorf("Old VNC mismatch: %+v", v.conn)
	}
}

func TestParseMRemoteNG(t *testing.T) {
	p := mustParse(t, fmtMRemoteNG, "mremoteng.xml", previewOptions{})
	app := findConn(t, p, "app01")
	if app.conn.Protocol != model.ProtoSSH || app.conn.Host != "app01.example.com" || folderPathOf(p, app) != "Datacenter" {
		t.Errorf("app01 mismatch: %+v folder=%s", app.conn, folderPathOf(p, app))
	}
	rdp := findConn(t, p, "rdp-jump")
	if rdp.conn.Protocol != model.ProtoRDP || rdp.conn.Options.String("domain") != "CORP" {
		t.Errorf("rdp-jump mismatch: %+v", rdp.conn)
	}
	findConn(t, p, "vnc-lab")
}

func TestParseFileZilla(t *testing.T) {
	p := mustParse(t, fmtFileZilla, "filezilla.xml", previewOptions{})
	pub := findConn(t, p, "Public FTP")
	if pub.conn.Protocol != model.ProtoFTP || pub.conn.Port != 21 {
		t.Errorf("Public FTP mismatch: %+v", pub.conn)
	}
	dep := findConn(t, p, "Deploy SFTP")
	if dep.conn.Protocol != model.ProtoSFTP || folderPathOf(p, dep) != "Work" {
		t.Errorf("Deploy SFTP mismatch: %+v folder=%s", dep.conn, folderPathOf(p, dep))
	}
	sec := findConn(t, p, "Secure FTPS")
	if sec.conn.Protocol != model.ProtoFTP || sec.conn.Options.String("ftpTls") != "implicit" || folderPathOf(p, sec) != "Work/Nested" {
		t.Errorf("Secure FTPS mismatch: %+v folder=%s", sec.conn, folderPathOf(p, sec))
	}
}

func TestParseWinSCP(t *testing.T) {
	p := mustParse(t, fmtWinSCP, "winscp.ini", previewOptions{})
	for _, c := range p.conns {
		if strings.Contains(strings.ToLower(c.conn.Name), "default") {
			t.Errorf("Default Settings must be skipped")
		}
	}
	web := findConn(t, p, "web")
	if web.conn.Protocol != model.ProtoSFTP || web.conn.Port != 2222 || folderPathOf(p, web) != "Prod" {
		t.Errorf("web mismatch: %+v folder=%s", web.conn, folderPathOf(p, web))
	}
	if web.keyPath == "" {
		t.Errorf("web should reference a key file")
	}
	ftp := findConn(t, p, "ftp-legacy")
	if ftp.conn.Protocol != model.ProtoFTP || ftp.conn.Options.String("ftpTls") != "explicit" {
		t.Errorf("ftp-legacy mismatch: %+v", ftp.conn)
	}
	tun := findConn(t, p, "tunneled")
	if len(tun.hops) != 1 || tun.hops[0].spec() != "jump@bastion.example.com:2222" {
		t.Errorf("tunneled hops = %+v", tun.hops)
	}
}

func TestParseSecureCRT(t *testing.T) {
	p := mustParse(t, fmtSecureCRT, "securecrt.xml", previewOptions{})
	edge := findConn(t, p, "edge-01")
	if edge.conn.Host != "edge01.example.com" || edge.conn.Port != 22 || edge.conn.Username != "netadmin" || folderPathOf(p, edge) != "Routers" {
		t.Errorf("edge-01 mismatch: %+v folder=%s", edge.conn, folderPathOf(p, edge))
	}
	tel := findConn(t, p, "legacy-telnet")
	if tel.conn.Protocol != model.ProtoTelnet || tel.conn.Port != 23 {
		t.Errorf("legacy-telnet mismatch: %+v", tel.conn)
	}
}

func TestParseRemmina(t *testing.T) {
	p := mustParse(t, fmtRemmina, "remmina.remmina", previewOptions{})
	app := findConn(t, p, "App Server")
	if app.conn.Protocol != model.ProtoSSH || app.conn.Host != "app.example.com" || app.conn.Port != 2022 || folderPathOf(p, app) != "Prod/App" {
		t.Errorf("App Server mismatch: %+v folder=%s", app.conn, folderPathOf(p, app))
	}
	if app.keyPath == "" {
		t.Errorf("App Server should reference a key")
	}
	win := findConn(t, p, "Windows Box")
	if win.conn.Protocol != model.ProtoRDP || win.conn.Options.String("domain") != "CORP" || win.conn.Options.Int("width") != 1920 {
		t.Errorf("Windows Box mismatch: %+v", win.conn)
	}
}

func TestParseKnownHosts(t *testing.T) {
	p := mustParse(t, fmtKnownHosts, "known_hosts", previewOptions{})
	if len(p.knownHosts) != 2 {
		t.Fatalf("expected 2 importable host keys, got %d", len(p.knownHosts))
	}
	var gh, gl *pknownHost
	for _, kh := range p.knownHosts {
		switch kh.host {
		case "github.example.com":
			gh = kh
		case "gitlab.example.com":
			gl = kh
		}
	}
	if gh == nil || gh.port != 22 || gh.keyType != "ssh-ed25519" || !strings.HasPrefix(gh.fingerprint, "SHA256:") {
		t.Errorf("github host mismatch: %+v", gh)
	}
	if gl == nil || gl.port != 2222 || gl.keyType != "ssh-rsa" {
		t.Errorf("gitlab host mismatch: %+v", gl)
	}
	if !hasWarning(p.warnings, "hashed") || !hasWarning(p.warnings, "cert-authority") {
		t.Errorf("expected warnings about hashed and marker entries; got %v", p.warnings)
	}
}

func TestParseTermsteadJSON(t *testing.T) {
	p := mustParse(t, fmtJSON, "termstead.json", previewOptions{})
	bastion := findConn(t, p, "bastion")
	if bastion.conn.Host != "bastion.aws.example.com" || folderPathOf(p, bastion) != "Cloud/AWS" {
		t.Errorf("bastion mismatch: %+v folder=%s", bastion.conn, folderPathOf(p, bastion))
	}
	if len(p.identities) != 1 {
		t.Errorf("expected 1 identity, got %d", len(p.identities))
	}
	if len(p.snippets) != 1 || p.snippets[0].content != "df -h" {
		t.Errorf("snippets = %+v", p.snippets)
	}
}

func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(strings.ToLower(w), strings.ToLower(substr)) {
			return true
		}
	}
	return false
}

// toUTF16LE encodes s as UTF-16LE with a BOM (for the .reg UTF-16 test).
func toUTF16LE(s string) []byte {
	out := []byte{0xFF, 0xFE}
	for _, r := range s {
		if r > 0xFFFF {
			r = 0xFFFD
		}
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}
