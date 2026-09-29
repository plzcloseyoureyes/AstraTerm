package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexterm/nexterm/internal/model"
)

// mobaxterm_full.mxtsessions is generated from the documented field tables (see mobaxterm.go) — CP1252 + CRLF like
// a real export, a ";  logout" prefix, a custom colour scheme (16 "r,g,b" fields shifting the terminal block),
// multi-hop gateways with a key list, the RDP 24.2 expert string, an SFTP proxy password, [SSH_Hostkeys] and every
// unsupported type. mobaxterm_full.hostkeys.expected holds the OpenSSH form of its three host keys.

func mobaFull(t *testing.T) *parsed {
	t.Helper()
	return mustParse(t, fmtAuto, "mobaxterm_full.mxtsessions", previewOptions{})
}

func TestMobaDetectAndDefaults(t *testing.T) {
	p := mobaFull(t)
	if p.format != fmtMobaXterm {
		t.Fatalf("detected %q", p.format)
	}
	ref := findConn(t, p, "Reference Session")
	c := ref.conn
	if c.Protocol != model.ProtoSSH || c.Host != "localhost" || c.Port != 22 || c.Username != "" {
		t.Errorf("reference session: %+v", c)
	}
	if c.Icon != "" || c.Color != "" {
		t.Errorf("default icon/colour must not be carried over: icon=%q color=%q", c.Icon, c.Color)
	}
	if !c.Options.Bool("x11Forwarding") || !c.Options.Bool("compression") {
		t.Errorf("X11/compression defaults (-1) not mapped: %v", c.Options)
	}
	for _, k := range []string{"term", "encoding", "terminal", "remoteCommand", "startupCommand", "sshBrowser", "useAgent", "jumpHosts", "proxy"} {
		if c.Options.Has(k) {
			t.Errorf("default session should not set %s: %v", k, c.Options[k])
		}
	}
	if folderPathOf(p, ref) != "" {
		t.Errorf("root session in folder %q", folderPathOf(p, ref))
	}
}

func TestMobaLogoutPrefixIconColourComment(t *testing.T) {
	p := mobaFull(t)
	c := findConn(t, p, "docker-host").conn
	if c.Host != "docker.example.com" || c.Username != "root" {
		t.Errorf("docker-host: %+v", c)
	}
	if c.Icon != "lucide:Container" {
		t.Errorf("icon 196 (Docker) → %q", c.Icon)
	}
	if c.Color != "#ff0000" {
		t.Errorf("tab colour 255 (COLORREF red) → %q", c.Color)
	}
	if c.Notes != "Build box #1" {
		t.Errorf("comment unescape: %q", c.Notes)
	}
	if c.Options.String("remoteCommand") != "docker ps" || c.Options.Has("startupCommand") {
		t.Errorf("command without 'do not exit' must be the remote command: %v", c.Options)
	}
	if v, ok := c.Options["useAgent"].(bool); !ok || v {
		t.Errorf("field 33 = 0 must disable the agent: %v", c.Options["useAgent"])
	}
}

func TestMobaSSHFullMapping(t *testing.T) {
	p := mobaFull(t)
	web := findConn(t, p, "web1")
	c := web.conn
	if c.Port != 2222 || c.Username != "deploy" || folderPathOf(p, web) != "Production" {
		t.Errorf("web1 basics: %+v folder=%s", c, folderPathOf(p, web))
	}
	if c.Options.String("startupCommand") != "tmux new -A -s main" || c.Options.Has("remoteCommand") {
		t.Errorf("'do not exit after command' must become a startup command: %v", c.Options)
	}
	if c.Options.String("sshBrowser") != "scp" {
		t.Errorf("browser protocol 2 (SCP) → %v", c.Options["sshBrowser"])
	}
	if !c.Options.Bool("agentForwarding") {
		t.Errorf("agent forwarding (34) not mapped")
	}
	if web.keyPath != `C:\Users\me\keys\web.ppk` || c.AuthMethod != model.AuthKey {
		t.Errorf("key path %q auth %q", web.keyPath, c.AuthMethod)
	}
	if c.Color != "#0000ff" || c.Notes != "Primary web server" {
		t.Errorf("colour %q notes %q", c.Color, c.Notes)
	}
	// Terminal block with a custom colour scheme: the 16 colours shift every later field by 15.
	if c.Options.String("term") != "vt220" || c.Options.String("encoding") != "iso-8859-15" || !c.Options.Bool("log") {
		t.Errorf("terminal options: %v", c.Options)
	}
	term := c.Options.Map("terminal")
	if term == nil || term["fontSize"] != 16 || term["cursorStyle"] != "underline" || term["cursorBlink"] != true || term["scrollback"] != 5000 {
		t.Errorf("terminal overrides: %v", term)
	}
	if !hasWarning(web.warnings, "colour scheme") || !hasWarning(web.warnings, "login macro") {
		t.Errorf("expected notes about the colour scheme and the login macro: %v", web.warnings)
	}
}

func TestMobaGatewaysAndProxies(t *testing.T) {
	p := mobaFull(t)
	db := findConn(t, p, "db1")
	if len(db.hops) != 2 {
		t.Fatalf("db1 hops = %+v", db.hops)
	}
	if h := db.hops[0]; h.host != "gw1.example.com" || h.port != 22 || h.user != "u1" || h.keyPath != "" {
		t.Errorf("hop 1 = %+v", h)
	}
	if h := db.hops[1]; h.host != "gw2.example.com" || h.port != 2222 || h.user != "jumpuser" || h.keyPath != `C:\keys\gw2.ppk` {
		t.Errorf("hop 2 = %+v", h)
	}
	proxy := db.conn.Options.Map("proxy")
	if proxy == nil || proxy["type"] != "socks5" || proxy["host"] != "socks.example.com" || proxy["port"] != 1080 || proxy["username"] != "sockuser" {
		t.Errorf("db1 proxy = %v", proxy)
	}
	if db.conn.Options.Bool("x11Forwarding") || db.conn.Options.Bool("compression") {
		t.Errorf("db1 disabled X11/compression: %v", db.conn.Options)
	}

	lc := findConn(t, p, "local-cmd-proxy").conn
	if got := lc.Options.String("proxyCommand"); got != "nc -X connect -x proxy:3128 %h %p" {
		t.Errorf("local proxy command = %q", got)
	}
	if !runsLocalCommand(&lc) {
		t.Errorf("a local proxy command must be flagged")
	}
	sp := findConn(t, p, "ssh-proxy")
	if len(sp.hops) != 1 || sp.hops[0].host != "bastion.example.com" || sp.hops[0].port != 2022 || sp.hops[0].user != "hopper" {
		t.Errorf("SSH-forwarding proxy must become a first hop: %+v", sp.hops)
	}
}

func TestMobaOtherTypes(t *testing.T) {
	p := mobaFull(t)
	sw := findConn(t, p, "switch1")
	if sw.conn.Protocol != model.ProtoTelnet || sw.conn.Port != 2323 || sw.conn.Username != "operator" {
		t.Errorf("telnet: %+v", sw.conn)
	}
	if folderPathOf(p, sw) != "Production/Network/Core/Edge" {
		t.Errorf("deep SubRep folder = %q", folderPathOf(p, sw))
	}
	if sw.conn.Icon != "lucide:Router" {
		t.Errorf("icon 116 → %q", sw.conn.Icon)
	}
	rsh := findConn(t, p, "rsh-box")
	if rsh.conn.Protocol != model.ProtoRlogin || rsh.conn.Username != "oper" || rsh.conn.Port != 513 {
		t.Errorf("rsh (user in field 2): %+v", rsh.conn)
	}
	ftp := findConn(t, p, "ftp-site")
	if ftp.conn.Protocol != model.ProtoFTP || ftp.conn.Port != 2121 || ftp.conn.Username != "ftpuser" || ftp.conn.Icon != "" {
		t.Errorf("ftp: %+v", ftp.conn)
	}
	ser := findConn(t, p, "console-serial")
	if ser.conn.Protocol != model.ProtoSerial || ser.conn.Options.String("device") != "COM2" || ser.conn.Host != "" {
		t.Errorf("serial: %+v", ser.conn)
	}
	mosh := findConn(t, p, "roamer")
	if mosh.conn.Protocol != model.ProtoMosh || mosh.conn.Port != 2222 || mosh.conn.Username != "mobile" || mosh.keyPath != `C:\keys\mosh.ppk` {
		t.Errorf("mosh: %+v key=%q", mosh.conn, mosh.keyPath)
	}
	cafe := findConn(t, p, "Café été") // CP1252 bytes decoded
	if cafe.conn.Username != "élise" {
		t.Errorf("CP1252 username = %q", cafe.conn.Username)
	}
	for _, name := range []string{"empty-host", "localsh", "files", "xdmcp", "portal", "bucket", "ubuntu-wsl"} {
		for _, c := range p.conns {
			if c.conn.Name == name {
				t.Errorf("%s must not be imported", name)
			}
		}
	}
	if p.unsupported != 7 {
		t.Errorf("unsupported = %d, want 7", p.unsupported)
	}
	for _, want := range []string{"S3", "WSL", "Shell", "File", "XDMCP", "Browser", "no remote host", "password"} {
		if !hasWarning(p.warnings, want) {
			t.Errorf("missing warning about %q: %v", want, p.warnings)
		}
	}
}

func TestMobaRDP(t *testing.T) {
	p := mobaFull(t)
	win := findConn(t, p, "win01")
	o := win.conn.Options
	if win.conn.Protocol != model.ProtoRDP || win.conn.Port != 3390 || win.conn.Username != "alice" || o.String("domain") != "CORP" {
		t.Errorf("rdp basics: %+v", win.conn)
	}
	checks := map[string]any{
		"console": true, "enableDrive": true, "width": 1920, "height": 1080, "initialProgram": `C:\Tools\start.cmd`,
		"enableAudio": true, "disableClipboard": true, "enableMic": true, "colorDepth": 32, "autoReconnect": true,
		"enableWallpaper": true, "enableTheming": true, "gatewayHost": "rdgw.corp.example", "gatewayDomain": "CORP",
		"gatewayUsername": "gwuser",
	}
	for k, want := range checks {
		if got := o[k]; got != want {
			t.Errorf("rdp %s = %v (%T), want %v", k, got, got, want)
		}
	}
	if o.Has("enablePrinting") || o.Has("enableFontSmoothing") {
		t.Errorf("unset flags mapped: %v", o)
	}
	if len(win.hops) != 1 || win.hops[0].host != "gw.example.com" || win.hops[0].port != 2200 || win.hops[0].user != "jump" || win.hops[0].keyPath != `C:\keys\gw.ppk` {
		t.Errorf("rdp SSH gateway = %+v", win.hops)
	}
	if o.Has("sshTunnelVia") {
		t.Errorf("sshTunnelVia must be resolved at commit (a saved connection id), not an inline spec: %v", o["sshTunnelVia"])
	}
	win2 := findConn(t, p, "win02")
	if win2.conn.Options.Has("gatewayHost") || win2.conn.Options.Has("enableAudio") || win2.conn.Options.Has("colorDepth") {
		t.Errorf("win02 (gateway 'Never', audio off, depth auto): %v", win2.conn.Options)
	}
}

func TestMobaVNCAndSFTP(t *testing.T) {
	p := mobaFull(t)
	vnc := findConn(t, p, "console")
	o := vnc.conn.Options
	if vnc.conn.Port != 5901 || !o.Bool("viewOnly") || o.String("scaling") != "none" {
		t.Errorf("vnc: %+v", vnc.conn)
	}
	if proxy := o.Map("proxy"); proxy == nil || proxy["type"] != "socks5" || proxy["port"] != 1081 || proxy["username"] != "proxyuser" {
		t.Errorf("vnc proxy = %v", o["proxy"])
	}
	if len(vnc.hops) != 2 || vnc.hops[1].host != "hop2.example.com" || vnc.hops[1].port != 2022 || vnc.hops[1].user != "u2" {
		t.Errorf("vnc gateways = %+v", vnc.hops)
	}
	sftp := findConn(t, p, "backup")
	so := sftp.conn.Options
	if sftp.conn.Protocol != model.ProtoSFTP || sftp.conn.Port != 2022 || so.String("initialPath") != "/srv/backup" || !so.Bool("compression") {
		t.Errorf("sftp: %+v", sftp.conn)
	}
	if proxy := so.Map("proxy"); proxy == nil || proxy["type"] != "socks5" || proxy["host"] != "proxy.example.com" || proxy["port"] != 1085 {
		t.Errorf("sftp proxy (type 4 = SOCKS5 with auth) = %v", so["proxy"])
	}
	if sftp.keyPath != `C:\keys\backup.ppk` {
		t.Errorf("sftp key = %q", sftp.keyPath)
	}
	if !hasWarning(sftp.warnings, "proxy password") {
		t.Errorf("the plain-text proxy password must be reported: %v", sftp.warnings)
	}
	for _, c := range p.conns {
		if strings.Contains(c.conn.Options.String("proxy"), "Sup3rS3cret") || len(c.secrets) > 0 {
			t.Fatalf("a MobaXterm password leaked into the import: %+v", c)
		}
		raw, _ := c.conn.Options.MarshalJSON()
		if strings.Contains(string(raw), "Sup3rS3cret") {
			t.Fatalf("proxy password in options: %s", raw)
		}
	}
}

func TestMobaHostKeys(t *testing.T) {
	p := mobaFull(t)
	want, err := os.ReadFile(filepath.Join("testdata", "mobaxterm_full.hostkeys.expected"))
	if err != nil {
		t.Fatal(err)
	}
	expected := strings.Fields(strings.ReplaceAll(string(want), "\n", " "))
	if len(p.knownHosts) != 3 {
		t.Fatalf("host keys = %d", len(p.knownHosts))
	}
	got := map[string]*pknownHost{}
	for _, kh := range p.knownHosts {
		got[kh.host] = kh
	}
	cases := []struct {
		host string
		port int
		typ  string
		blob string
	}{
		{"ssh1.example.com", 22, "ssh-ed25519", expected[1]},
		{"legacy.example.com", 2222, "ssh-rsa", expected[3]},
		{"10.0.0.9", 22, "ecdsa-sha2-nistp256", expected[5]},
	}
	for _, tc := range cases {
		kh := got[tc.host]
		if kh == nil || kh.port != tc.port || kh.keyType != tc.typ || normalizePubKey(kh.publicKey) != tc.blob {
			t.Errorf("%s: got %+v, want %s %s", tc.host, kh, tc.typ, tc.blob)
		}
	}
}

func TestMobaHeaderlessMobaFile(t *testing.T) {
	// "Save session to file" writes a .moba file: one session line, no section header.
	src := "prod-db= #109#0%db.example.com%2200%admin%%0%0%%%%%0%0%0%%%-1%0%0%0%%1080%%0%0%1%#MobaFont%10%0%0%-1%15%236,236,236%30,30,30%180,180,192%0%-1%0%%xterm%-1%0%_Std_Colors_0_%80%24%0%1%-1%<none>%%0#0# #-1\r\n"
	p, err := parseSource(fmtAuto, []byte(src), previewOptions{})
	if err != nil {
		t.Fatalf("parse .moba: %v", err)
	}
	c := findConn(t, p, "prod-db").conn
	if c.Host != "db.example.com" || c.Port != 2200 || c.Username != "admin" {
		t.Errorf(".moba session: %+v", c)
	}
}

func TestMobaCharsetOption(t *testing.T) {
	// A Hebrew Windows writes MobaXterm.ini in Windows-1255: "שרת" = F9 F8 FA.
	src := []byte("[Bookmarks]\r\nSubRep=\r\nImgNum=42\r\n\xf9\xf8\xfa= #109#0%h.example.com%22%u%\r\n")
	p, err := parseSource(fmtMobaXterm, src, previewOptions{Charset: "windows-1255"})
	if err != nil {
		t.Fatal(err)
	}
	findConn(t, p, "שרת")
	if _, err := parseSource(fmtMobaXterm, src, previewOptions{Charset: "no-such-charset"}); err == nil {
		t.Errorf("an unknown charset must be rejected")
	}
}

func TestMobaIconNamesExistInCatalog(t *testing.T) {
	// Names of the built-in icon set (web/src/features/sessions/icons.tsx ICON_GROUPS).
	catalog := strings.Fields(`Server ServerCog Database HardDrive Cpu MemoryStick CircuitBoard Microchip PcCase Computer
		Monitor Laptop Tablet Smartphone Tv Printer Camera Router Network Wifi Antenna Radio Satellite Cable Plug Usb Globe
		Earth Cloud CloudCog Webhook Radar Terminal SquareTerminal Shell Code GitBranch Container Box Boxes Package Layers
		Bug FlaskConical Rocket Bot Cog Wrench Shield ShieldCheck Lock KeyRound House Building Factory Warehouse Store
		Briefcase GraduationCap User Users MapPin Map Compass Plane Car Ship Truck Star Heart Flame Zap Leaf Sprout TreePine
		Mountain Snowflake Sun Moon Anchor Flag Bookmark Target Trophy Crown Gem Gamepad2 Music Book Coffee Beer Brain Atom
		Microscope Telescope Orbit Cat Dog Bird Fish Rabbit Turtle Skull`)
	have := map[string]bool{}
	for _, n := range catalog {
		have[n] = true
	}
	for id, name := range mobaIconMap {
		if !have[name] {
			t.Errorf("icon %d maps to %q, which is not in the built-in set", id, name)
		}
	}
	if !have["Cog"] {
		t.Fatal("catalog list out of date")
	}
}

func TestMobaTabColour(t *testing.T) {
	for in, want := range map[string]string{"-1": "", "536870911": "", "0": "#000000", "65280": "#00ff00", "16711680": "#0000ff", "x": ""} {
		if got := mobaTabColor(in); got != want {
			t.Errorf("mobaTabColor(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestMobaProxyCommandTokens(t *testing.T) {
	for in, want := range map[string]string{
		"nc __PERCENT__host __PERCENT__port":                                      "nc %h %p",
		"ssh -W __PERCENT__host:__PERCENT__port __PERCENT__user@bastion":          "ssh -W %h:%p %r@bastion",
		"connect -H __PERCENT__proxyhost:__PERCENT__proxyport __PERCENT__host 22": "connect -H px:3128 %h 22",
		"echo 100__PERCENT____PERCENT__":                                          "echo 100%%",
	} {
		got, ok := mobaProxyCommand(in, "px", "3128")
		if !ok || got != want {
			t.Errorf("mobaProxyCommand(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if _, ok := mobaProxyCommand("plink -pw __PERCENT__pass x", "", ""); ok {
		t.Errorf("a command needing the password must not be imported")
	}
}
