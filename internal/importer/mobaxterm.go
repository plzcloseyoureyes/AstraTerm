package importer

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// MobaXterm .mxtsessions / MobaXterm.ini / .moba importer (IMP-1).
//
// Format reference: the community documentation of the bookmark format (MobaXterm 26.3,
// gist.github.com/Ruzgfpegk/ab597838e4abbe8de30d7224afd062ea — field tables of SSH, RDP, VNC, SFTP, Browser and the
// terminal block), cross-checked against real files (Metasploit's MobaXterm module and its captured output, which
// show the Telnet / Rsh / FTP / Serial / Shell / File / XDMCP type codes and layouts, public MobaXterm.ini samples)
// and other converters (sessionator, RustConn, mobaConverterGo).
//
// A file is INI in the Windows ANSI code page (Windows-1252 on Western systems; the `charset` option handles others)
// with sections [Bookmarks], [Bookmarks_1], … each holding SubRep= (the folder path, '\'-separated, "" for the root
// "User sessions"), ImgNum= (the folder icon) and one line per session. A .moba file ("Save session to file") is a
// single session line without a section header. A session line is
//
//	Name=<logout>#<icon>#<type block>#<terminal block>#<start in>#<comment>#<tab colour>
//
//	<logout>     "" / " " (reconnection message shown) or ";  logout"
//	<icon>       session ImgNum (defaults: SSH 109, Telnet 98, Rsh 100, XDMCP 88, RDP 91, VNC 128, FTP 130, SFTP 140,
//	             Serial 131, File 84, Shell 97, Browser 313, Mosh 145, S3 343, WSL 151)
//	<type block> '%'-separated, field 0 = session type: 0 SSH · 1 Telnet · 2 Rsh · 3 XDMCP · 4 RDP · 5 VNC · 6 FTP ·
//	             7 SFTP · 8 Serial · 9 File · 10 Shell · 11 Browser · 12 Mosh · 13 S3 · 14 WSL (per-type layouts below)
//	<terminal>   font%size%bold%?%winpath%charset%fg%bg%cursor colour%cursor type%^H%log%log dir%term type%lock title%
//	             ?%colour scheme ("_Std_Colors_0_", or 16 "r,g,b" fields that shift the rest by 15)%cols%rows%fixed%
//	             syntax%bold bright%macro type%macro%paste delay%font charset%antialias%ligatures%expert settings
//	             (comma list ending in bell type, scrollback lines)
//	<comment>    '#' escaped as "__DIEZE__"
//	<tab colour> -1 (none) or a Windows COLORREF (0x00BBGGRR) in decimal; 536870911 = default
//
// In-field escapes: ';' "__PTVIRG__", '"' "__DBLQUO__", '|' "__PIPE__" (also the separator of gateway lists), '%'
// "__PERCENT__", '#' "__DIEZE__"; a stored path's "C:" is written "_CurrentDrive_:". Booleans are "-1" (on) / "0".
// Where a type's layout is only known up to host/port/user (Telnet, FTP, Mosh), nothing further is read. S3, WSL,
// Shell, File, XDMCP and Browser sessions have no faithful AstraTerm equivalent and are reported, not imported.
//
// Passwords are never part of a bookmark line (MobaXterm keeps them in its encrypted password store), except an SFTP
// session's proxy password, which is dropped with a warning.

// MobaXterm session-type codes.
const (
	mobaSSH     = 0
	mobaTelnet  = 1
	mobaRsh     = 2
	mobaXDMCP   = 3
	mobaRDP     = 4
	mobaVNC     = 5
	mobaFTP     = 6
	mobaSFTP    = 7
	mobaSerial  = 8
	mobaFile    = 9
	mobaShell   = 10
	mobaBrowser = 11
	mobaMosh    = 12
	mobaS3      = 13
	mobaWSL     = 14
)

var mobaTypeName = map[int]string{
	mobaSSH: "SSH", mobaTelnet: "Telnet", mobaRsh: "Rsh", mobaXDMCP: "XDMCP", mobaRDP: "RDP", mobaVNC: "VNC",
	mobaFTP: "FTP", mobaSFTP: "SFTP", mobaSerial: "Serial", mobaFile: "File", mobaShell: "Shell",
	mobaBrowser: "Browser", mobaMosh: "Mosh", mobaS3: "S3", mobaWSL: "WSL",
}

// mobaDefaultIcon is the ImgNum a new session of each type gets; sessions that keep it use AstraTerm's protocol icon.
var mobaDefaultIcon = map[int]int{
	mobaSSH: 109, mobaTelnet: 98, mobaRsh: 100, mobaXDMCP: 88, mobaRDP: 91, mobaVNC: 128, mobaFTP: 130, mobaSFTP: 140,
	mobaSerial: 131, mobaFile: 84, mobaShell: 97, mobaBrowser: 313, mobaMosh: 145, mobaS3: 343, mobaWSL: 151,
}

// mobaIconMap maps a customised MobaXterm session icon to the closest icon of AstraTerm's built-in set
// (web/src/features/sessions/icons.tsx). Icons without a meaningful equivalent (distribution logos on terminals, …)
// are left to the protocol default rather than replaced by a generic glyph.
var mobaIconMap = map[int]string{
	83: "Monitor", 85: "Monitor", 88: "Monitor", 89: "Monitor", 90: "Laptop", 91: "Monitor", 92: "CircuitBoard",
	93: "Monitor", 103: "Zap", 114: "Computer", 116: "Router", 118: "Router", 119: "HardDrive", 120: "Shield",
	121: "Cloud", 122: "Briefcase", 124: "ServerCog", 125: "Satellite", 126: "HardDrive", 127: "FlaskConical",
	128: "Monitor", 130: "Globe", 131: "Cable", 132: "Layers", 133: "ShieldCheck", 134: "Database", 135: "House",
	137: "FlaskConical", 140: "HardDrive", 142: "HardDrive", 144: "Wifi", 145: "Antenna", 146: "Radio", 147: "Wrench",
	150: "CircuitBoard", 151: "Monitor", 194: "Smartphone", 196: "Container", 197: "Box", 202: "GitBranch",
	313: "Globe", 343: "Cloud",
}

var mobaTextReplacer = strings.NewReplacer(
	"__PIPE__", "|",
	"__PTVIRG__", ";",
	"__DBLQUO__", `"`,
	"__PERCENT__", "%",
	"__DIEZE__", "#",
)

// mobaText reverses MobaXterm's in-field escaping.
func mobaText(s string) string { return mobaTextReplacer.Replace(strings.TrimSpace(s)) }

// mobaPath reverses the escaping of a stored file path ("_CurrentDrive_" is the drive MobaXterm ran from; C is the
// only sensible guess on another machine).
func mobaPath(s string) string {
	return mobaText(strings.ReplaceAll(s, "_CurrentDrive_", "C"))
}

// mobaList splits a raw (still escaped) "__PIPE__"-separated gateway list, keeping empty positions.
func mobaList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "__PIPE__")
}

// mobaUser drops MobaXterm's "use the default login" marker.
func mobaUser(s string) string {
	if strings.EqualFold(s, "<default>") {
		return ""
	}
	return s
}

// mobaSession is one decoded bookmark line.
type mobaSession struct {
	name    string
	icon    int
	typ     int
	f       []string // the type block's '%' fields (raw)
	term    mobaTerm
	hasTerm bool
	comment string
	color   string
}

func (s *mobaSession) raw(i int) string {
	if i >= 0 && i < len(s.f) {
		return s.f[i]
	}
	return ""
}
func (s *mobaSession) get(i int) string  { return strings.TrimSpace(s.raw(i)) }
func (s *mobaSession) text(i int) string { return mobaText(s.raw(i)) }
func (s *mobaSession) path(i int) string { return mobaPath(s.raw(i)) }
func (s *mobaSession) on(i int) bool     { return s.get(i) == "-1" }

// base builds the common part of a connection.
func (s *mobaSession) base(proto model.Protocol, host string, port int, user string) model.Connection {
	return model.Connection{
		Name:     cleanName(s.name, host),
		Protocol: proto,
		Host:     host,
		Port:     clampPort(port, proto),
		Username: user,
		Icon:     mobaIcon(s.icon, s.typ),
		Color:    s.color,
		Notes:    s.comment,
		Options:  model.Options{},
	}
}

// mobaImport carries per-file state (aggregated skip reports).
type mobaImport struct {
	b       *builder
	skipped map[string][]string // reason → session names
}

func (m *mobaImport) skip(reason, name string) {
	m.b.p.unsupported++
	m.skipped[reason] = append(m.skipped[reason], name)
}

func parseMobaXterm(content []byte) (*parsed, error) {
	f := parseINI(content)
	m := &mobaImport{b: newBuilder(fmtMobaXterm), skipped: map[string][]string{}}
	b := m.b
	sawBookmarks, sawPasswords := false, false
	var hostKeyLines []string

	for _, sec := range f.Sections {
		lname := strings.ToLower(sec.Name)
		switch {
		case lname == "ssh_hostkeys":
			for _, kv := range sec.KVs {
				hostKeyLines = append(hostKeyLines, kv.Key+"="+kv.Value)
			}
			continue
		case lname == "passwords" || lname == "credentials" || lname == "sesspass":
			sawPasswords = sawPasswords || len(sec.KVs) > 0
			continue
		case lname == "bookmarks" || strings.HasPrefix(lname, "bookmarks_"):
		case lname == "" && hasMobaSessionLines(sec):
			// A .moba file: session lines without a section header.
		default:
			continue
		}
		sawBookmarks = true
		folderID := ""
		if subRep := mobaText(sec.get("SubRep")); subRep != "" {
			folderID = b.folderPath("", strings.Split(subRep, `\`)...)
		}
		for _, kv := range sec.KVs {
			if strings.EqualFold(kv.Key, "SubRep") || strings.EqualFold(kv.Key, "ImgNum") {
				continue
			}
			m.session(folderID, strings.TrimSpace(kv.Key), kv.Value)
		}
	}
	if !sawBookmarks && len(hostKeyLines) == 0 {
		return nil, badRequest("no [Bookmarks] sections or session lines found — not a MobaXterm sessions file")
	}
	if len(hostKeyLines) > 0 {
		addPuttyHostKeys(b, hostKeyLines, "[SSH_Hostkeys]")
	}
	reasons := make([]string, 0, len(m.skipped))
	for r := range m.skipped {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		names := m.skipped[r]
		list := strings.Join(names[:min(len(names), 5)], ", ")
		if len(names) > 5 {
			list += fmt.Sprintf(" and %d more", len(names)-5)
		}
		b.warn("Skipped %d session(s) — %s: %s", len(names), r, list)
	}
	if len(b.p.conns) > 0 || sawPasswords {
		b.warn("Saved passwords are not imported: MobaXterm keeps them in its own encrypted password store. Re-enter them in AstraTerm or use SSH keys.")
	}
	return b.finish()
}

// hasMobaSessionLines reports whether an unnamed (header-less) section holds bookmark lines (a .moba file).
func hasMobaSessionLines(sec *iniSection) bool {
	for _, kv := range sec.KVs {
		if reMobaSession.MatchString(kv.Key + "=" + kv.Value) {
			return true
		}
	}
	return false
}

// session decodes one "Name=value" bookmark line.
func (m *mobaImport) session(folderID, name, value string) {
	groups := strings.Split(strings.TrimRight(value, "\r"), "#")
	if len(groups) < 3 || strings.TrimSpace(groups[2]) == "" {
		m.skip("unrecognised session line", name)
		return
	}
	fields := strings.Split(groups[2], "%")
	typeCode, err := strconv.Atoi(strings.TrimSpace(fields[0]))
	if err != nil {
		m.skip("unrecognised session line", name)
		return
	}
	s := &mobaSession{name: name, icon: atoiSafe(groups[1], -1), typ: typeCode, f: fields}
	if len(groups) > 3 && strings.TrimSpace(groups[3]) != "" {
		s.term, s.hasTerm = parseMobaTerminal(groups[3]), true
	}
	if len(groups) > 5 {
		s.comment = mobaText(groups[5])
	}
	if len(groups) > 6 {
		s.color = mobaTabColor(groups[6])
	}

	switch typeCode {
	case mobaSSH:
		m.ssh(folderID, s)
	case mobaTelnet:
		m.telnet(folderID, s)
	case mobaRsh:
		m.rsh(folderID, s)
	case mobaRDP:
		m.rdp(folderID, s)
	case mobaVNC:
		m.vnc(folderID, s)
	case mobaFTP:
		m.ftp(folderID, s)
	case mobaSFTP:
		m.sftp(folderID, s)
	case mobaSerial:
		m.serial(folderID, s)
	case mobaMosh:
		m.mosh(folderID, s)
	default: // XDMCP, File, Shell, Browser, S3, WSL and unknown codes have no faithful AstraTerm mapping.
		tn := mobaTypeName[typeCode]
		if tn == "" {
			tn = "type " + strconv.Itoa(typeCode)
		}
		reason := "MobaXterm " + tn + " sessions have no AstraTerm equivalent"
		if typeCode == mobaS3 {
			reason = "MobaXterm S3 sessions are not imported (create an S3 connection in AstraTerm)"
		}
		m.skip(reason, name)
	}
}

// requireHost returns the host of field i or reports the session as skipped.
func (m *mobaImport) requireHost(s *mobaSession, i int) (string, bool) {
	host := s.text(i)
	if host == "" {
		m.skip("no remote host", s.name)
		return "", false
	}
	return host, true
}

// ssh — type 0: host(1) port(2) user(3) ·(4) X11(5) compression(6) command(7) gateway hosts/ports/users(8/9/10)
// keep-shell-after-command(11) ·(12) remote desktop environment(13) key(14) gateway keys(15) SSH-browser(16) follow
// path(17) ·(18) proxy type/host/port/login(19-22) locales(23) SCP-over-SFTP(24) browser protocol(25) proxy command(26)
// SSH version(27) KEX/hostkey/cipher lists(28-30) ·(31-32) use agent(33) agent forwarding(34) certificate(35) host
// certificate(36).
func (m *mobaImport) ssh(folderID string, s *mobaSession) {
	host, ok := m.requireHost(s, 1)
	if !ok {
		return
	}
	c := s.base(model.ProtoSSH, host, atoiSafe(s.get(2), 22), mobaUser(s.text(3)))
	o := c.Options
	var notes []string
	if s.on(5) {
		o["x11Forwarding"] = true
	}
	if s.on(6) {
		o["compression"] = true // MobaXterm's default; the SSH core has no zlib yet (RESEARCH SSH-26)
	}
	if cmd := s.text(7); cmd != "" {
		if s.on(11) { // "Do not exit after command ends": the shell stays → run it inside the shell
			o["startupCommand"] = cmd
		} else {
			o["remoteCommand"] = cmd
		}
	}
	if env := atoiSafe(s.get(13), 0); env != 0 {
		notes = append(notes, "the remote X11 desktop environment setting was not imported")
	}
	switch {
	case s.get(16) == "0" || s.get(25) == "0":
		o["sshBrowser"] = "none"
	case s.get(25) == "2" || s.get(25) == "3" || s.on(24):
		o["sshBrowser"] = "scp"
	}
	if s.get(27) == "2" {
		notes = append(notes, "MobaXterm forced SSH protocol 1, which AstraTerm does not support")
	}
	if s.get(28) != "" || s.get(29) != "" || s.get(30) != "" {
		notes = append(notes, "custom KEX / host-key / cipher preferences (PuTTY names) were not imported")
	}
	if s.get(33) == "0" {
		o["useAgent"] = false
	}
	if s.on(34) {
		o["agentForwarding"] = true
	}
	if cert := s.path(35); cert != "" {
		notes = append(notes, "certificate "+cert+" was not imported (attach it to the key in Keys)")
	}
	pc := m.b.conn(folderID, c)
	if kp := s.path(14); kp != "" {
		pc.keyPath = kp
		pc.conn.AuthMethod = model.AuthKey
		notes = append(notes, "uses private key "+kp+" (imported in desktop mode when readable)")
	}
	pc.hops = append(mobaProxy(pc, s, 19, 20, 21, 22, 26, &notes), mobaGateways(s, 8, 9, 10, 15)...)
	notes = append(notes, s.term.apply(pc, s.hasTerm)...)
	pc.warnings = append(pc.warnings, notes...)
}

// telnet — type 1: host(1) port(2) user(3); the rest of the layout is undocumented and not read.
func (m *mobaImport) telnet(folderID string, s *mobaSession) {
	host, ok := m.requireHost(s, 1)
	if !ok {
		return
	}
	pc := m.b.conn(folderID, s.base(model.ProtoTelnet, host, atoiSafe(s.get(2), 23), mobaUser(s.text(3))))
	pc.warnings = append(pc.warnings, s.term.apply(pc, s.hasTerm)...)
}

// rsh — type 2: host(1) user(2) (no port: rsh is fixed to its service port). Imported as an interactive Rlogin
// session, which is what `rsh host` without a command runs.
func (m *mobaImport) rsh(folderID string, s *mobaSession) {
	host, ok := m.requireHost(s, 1)
	if !ok {
		return
	}
	pc := m.b.conn(folderID, s.base(model.ProtoRlogin, host, 0, mobaUser(s.text(2))))
	pc.warnings = append(pc.warnings, "imported as an interactive Rlogin session")
	pc.warnings = append(pc.warnings, s.term.apply(pc, s.hasTerm)...)
}

// ftp — type 6: host(1) port(2) user(3); passive / FTPS options are undocumented and not read.
func (m *mobaImport) ftp(folderID string, s *mobaSession) {
	host, ok := m.requireHost(s, 1)
	if !ok {
		return
	}
	pc := m.b.conn(folderID, s.base(model.ProtoFTP, host, atoiSafe(s.get(2), 21), mobaUser(s.text(3))))
	pc.warnings = append(pc.warnings, "check the FTPS mode (MobaXterm's FTP security settings are not imported)")
}

// mosh — type 12: host(1) port(2) user(3); the SSH key path is taken when a field clearly holds one.
func (m *mobaImport) mosh(folderID string, s *mobaSession) {
	host, ok := m.requireHost(s, 1)
	if !ok {
		return
	}
	port := atoiSafe(s.get(2), 0)
	user := mobaUser(s.text(3))
	if _, err := strconv.Atoi(user); err == nil { // a flag, not a login
		user = ""
	}
	pc := m.b.conn(folderID, s.base(model.ProtoMosh, host, port, user))
	for i := 4; i < len(s.f); i++ {
		if v := s.path(i); looksLikeKeyPath(v) {
			pc.keyPath = v
			pc.conn.AuthMethod = model.AuthKey
			pc.warnings = append(pc.warnings, "uses private key "+v+" (imported in desktop mode when readable)")
			break
		}
	}
	pc.warnings = append(pc.warnings, s.term.apply(pc, s.hasTerm)...)
}

// looksLikeKeyPath reports whether v is unmistakably a private key file path.
func looksLikeKeyPath(v string) bool {
	low := strings.ToLower(v)
	if !strings.ContainsAny(v, `\/`) {
		return false
	}
	return strings.HasSuffix(low, ".ppk") || strings.HasSuffix(low, ".pem") || strings.HasSuffix(low, ".key") ||
		strings.Contains(low, "id_rsa") || strings.Contains(low, "id_ed25519") || strings.Contains(low, "id_ecdsa")
}

// rdp — type 4: host(1) port(2) user(3) console(4) ports(5) drives(6) printers(7) ·(8) enhanced graphics(9)
// resolution(10) ·(11) remote command(12) SSH gateway hosts/ports/users(13/14/15) audio(16) native auth(17) gateway
// keys(18) clipboard(19) RDP gateway(20) keyboard shortcuts(21) settings bar(22) ·(23) CredSSP(24) microphone(25)
// autoscale(26) zoom(27) colour depth(28) smartcards(29) server authentication(30) expert settings(31: "<12
// booleans>,<use gateway>,<gateway server>,<gateway login>,<?>,<gateway auth>").
func (m *mobaImport) rdp(folderID string, s *mobaSession) {
	host, ok := m.requireHost(s, 1)
	if !ok {
		return
	}
	user, domain := s.text(3), ""
	if i := strings.IndexByte(user, '\\'); i > 0 { // DOMAIN\user
		domain, user = user[:i], user[i+1:]
	}
	c := s.base(model.ProtoRDP, host, atoiSafe(s.get(2), 3389), user)
	o := c.Options
	var notes []string
	if domain != "" {
		o["domain"] = domain
	}
	if s.on(4) {
		o["console"] = true
	}
	if s.on(6) {
		o["enableDrive"] = true
	}
	if s.on(7) {
		o["enablePrinting"] = true
	}
	if s.on(5) || s.on(29) {
		notes = append(notes, "serial-port / smart-card redirection is not available in the browser")
	}
	if w, h := mobaRDPResolution(s.get(10)); w > 0 {
		o["width"], o["height"] = w, h
	}
	if cmd := s.text(12); cmd != "" {
		o["initialProgram"] = cmd
	}
	if s.get(16) == "1" {
		o["enableAudio"] = true
	}
	if s.get(19) == "0" {
		o["disableClipboard"] = true
	}
	if s.on(25) {
		o["enableMic"] = true
	}
	switch s.get(28) {
	case "1":
		o["colorDepth"] = 8
	case "2":
		o["colorDepth"] = 16
	case "3":
		o["colorDepth"] = 24
	case "4":
		o["colorDepth"] = 32
	}
	// RDP gateway: field 20 (all versions) or the expert settings (24.2+), which also say whether to use it.
	gwHost, gwUser, gwUse := s.text(20), "", ""
	if exp := strings.Split(s.raw(31), ","); len(exp) >= 2 {
		flags := strings.TrimSpace(exp[0])
		bit := func(i int) bool { return i < len(flags) && flags[i] == '1' }
		if len(flags) >= 10 {
			if bit(3) {
				o["autoReconnect"] = true
			}
			if bit(7) {
				o["enableWallpaper"] = true
			}
			if bit(8) {
				o["enableTheming"] = true
			}
			if bit(9) {
				o["enableFontSmoothing"] = true
			}
		}
		gwUse = strings.TrimSpace(exp[1])
		if len(exp) >= 3 && mobaText(exp[2]) != "" {
			gwHost = mobaText(exp[2])
		}
		if len(exp) >= 4 {
			gwUser = mobaText(exp[3])
		}
	}
	if gwHost != "" && gwUse != "0" {
		h, p := gwHost, 0
		if strings.Count(gwHost, ":") == 1 {
			h, p = splitHostPortLoose(gwHost)
		}
		o["gatewayHost"] = h
		if p > 0 {
			o["gatewayPort"] = p
		}
		if gwUser != "" {
			if i := strings.IndexByte(gwUser, '\\'); i > 0 {
				o["gatewayDomain"], gwUser = gwUser[:i], gwUser[i+1:]
			}
			o["gatewayUsername"] = gwUser
		}
	}
	pc := m.b.conn(folderID, c)
	pc.hops = mobaGateways(s, 13, 14, 15, 18)
	pc.warnings = append(pc.warnings, notes...)
}

// mobaRDPResolution maps the resolution index (0 fit to terminal, 1 fit to screen, 2… fixed sizes).
func mobaRDPResolution(v string) (int, int) {
	sizes := map[string][2]int{
		"2": {640, 480}, "3": {800, 600}, "4": {1024, 768}, "5": {1152, 864}, "6": {1280, 720}, "7": {1280, 968},
		"8": {1280, 1024}, "9": {1400, 1050}, "10": {1600, 1200}, "11": {1920, 1080}, "12": {1276, 936},
		"13": {1916, 988}, "14": {1920, 1200}, "15": {1280, 800}, "16": {1360, 768}, "17": {1366, 768},
		"18": {1440, 900}, "19": {1536, 864}, "20": {1600, 900}, "21": {1680, 1050}, "22": {2048, 1152},
		"23": {2560, 1080}, "24": {2560, 1440}, "25": {3440, 1440}, "26": {3840, 2160},
	}
	if wh, ok := sizes[v]; ok {
		return wh[0], wh[1]
	}
	return 0, 0
}

// vnc — type 5: host(1) port(2) auto-scale(3) view-only(4) SSH gateway hosts/ports/users/keys(5-8) settings bar(9)
// new engine(10) SSL tunnel(11) Unix login(12) proxy type/host/port/login(13-16).
func (m *mobaImport) vnc(folderID string, s *mobaSession) {
	host, ok := m.requireHost(s, 1)
	if !ok {
		return
	}
	c := s.base(model.ProtoVNC, host, atoiSafe(s.get(2), 5900), "")
	if s.get(3) == "0" {
		c.Options["scaling"] = "none"
	}
	if s.on(4) {
		c.Options["viewOnly"] = true
	}
	var notes []string
	if s.on(11) {
		notes = append(notes, "MobaXterm's SSL tunnel option was not imported (AstraTerm negotiates VeNCrypt TLS itself)")
	}
	pc := m.b.conn(folderID, c)
	pc.hops = append(mobaProxy(pc, s, 13, 14, 15, 16, -1, &notes), mobaGateways(s, 5, 6, 7, 8)...)
	pc.warnings = append(pc.warnings, notes...)
}

// sftp — type 7: host(1) port(2) user(3) UTF-8(4) compression(5) remote folder(6) ASCII(7) 2-step auth(8) key(9)
// proxy type(10: 1/2 SOCKS4, 3/4 SOCKS5, 5-8 HTTP) server(11) port(12) user(13) password(14, plain text — dropped)
// local folder(15) preserve dates(16).
func (m *mobaImport) sftp(folderID string, s *mobaSession) {
	host, ok := m.requireHost(s, 1)
	if !ok {
		return
	}
	c := s.base(model.ProtoSFTP, host, atoiSafe(s.get(2), 22), mobaUser(s.text(3)))
	if s.on(5) {
		c.Options["compression"] = true
	}
	if root := s.text(6); root != "" {
		c.Options["initialPath"] = root
	}
	var notes []string
	pc := m.b.conn(folderID, c)
	if kp := s.path(9); kp != "" {
		pc.keyPath = kp
		pc.conn.AuthMethod = model.AuthKey
		notes = append(notes, "uses private key "+kp+" (imported in desktop mode when readable)")
	}
	kind := ""
	switch s.get(10) {
	case "1", "2":
		kind = "socks4"
	case "3", "4":
		kind = "socks5"
	case "5", "6", "7", "8":
		kind = "http"
	}
	if ph := s.text(11); kind != "" && ph != "" {
		proxy := map[string]any{"type": kind, "host": ph, "port": clampProxyPort(s.get(12), kind)}
		if u := s.text(13); u != "" {
			proxy["username"] = u
		}
		pc.conn.Options["proxy"] = proxy
	}
	if s.get(14) != "" {
		notes = append(notes, "the proxy password MobaXterm stored in plain text was not imported (set it in the connection's secrets)")
	}
	pc.warnings = append(pc.warnings, notes...)
}

// serial — type 8. Only the device ("COM3", "/dev/ttyUSB0", possibly followed by a description) and a standard baud
// rate are unambiguous in the undocumented layout; line settings are left at AstraTerm's defaults (8N1).
func (m *mobaImport) serial(folderID string, s *mobaSession) {
	device, baud := "", 0
	for i := 1; i < len(s.f); i++ {
		v := strings.TrimSpace(s.f[i])
		if device == "" {
			if up := strings.ToUpper(v); strings.HasPrefix(up, "COM") && len(v) > 3 && v[3] >= '0' && v[3] <= '9' || strings.HasPrefix(v, "/dev/") {
				device = strings.Fields(v)[0]
				continue
			}
		}
		if baud == 0 && isStandardBaud(v) {
			baud = atoiSafe(v, 0)
		}
	}
	if device == "" {
		m.skip("the serial device could not be determined", s.name)
		return
	}
	c := s.base(model.ProtoSerial, "", 0, "")
	c.Name = cleanName(s.name, device)
	c.Options["device"] = device
	if baud > 0 {
		c.Options["baud"] = baud
	}
	pc := m.b.conn(folderID, c)
	pc.warnings = append(pc.warnings, "serial line settings (data/parity/stop bits, flow control) were not imported — check them")
	pc.warnings = append(pc.warnings, s.term.apply(pc, s.hasTerm)...)
}

func isStandardBaud(s string) bool {
	switch atoiSafe(s, 0) {
	case 110, 300, 600, 1200, 2400, 4800, 9600, 14400, 19200, 28800, 38400, 57600, 115200, 128000, 230400, 256000,
		460800, 921600:
		return true
	}
	return false
}

// mobaGateways builds the SSH gateway chain from MobaXterm's positional "__PIPE__" lists (hosts, ports, users,
// private keys).
func mobaGateways(s *mobaSession, hostIdx, portIdx, userIdx, keyIdx int) []phop {
	hosts := mobaList(s.raw(hostIdx))
	ports := mobaList(s.raw(portIdx))
	users := mobaList(s.raw(userIdx))
	keys := mobaList(s.raw(keyIdx))
	var out []phop
	for i, rh := range hosts {
		h := mobaText(rh)
		if h == "" {
			continue
		}
		hop := phop{host: h, port: 22}
		if i < len(ports) {
			if p := atoiSafe(ports[i], 0); p > 0 && p <= 65535 {
				hop.port = p
			}
		}
		if i < len(users) {
			hop.user = mobaUser(mobaText(users[i]))
		}
		if i < len(keys) {
			hop.keyPath = mobaPath(keys[i])
		}
		out = append(out, hop)
	}
	return out
}

// mobaProxy maps MobaXterm's network proxy (type 0 none, 1 SOCKS4, 2 SOCKS5, 3 HTTP, 4 Telnet, 5 local command,
// 6 SSH forwarding, 7 SSH command). SOCKS/HTTP become options.proxy; a local command becomes options.proxyCommand
// (PuTTY-style %host/%port/%user variables translated); the SSH types reach the target through the proxy's SSH server,
// i.e. a first jump hop, which is returned. cmdIdx < 0 means the session type has no command field.
func mobaProxy(pc *pconn, s *mobaSession, typeIdx, hostIdx, portIdx, loginIdx, cmdIdx int, notes *[]string) []phop {
	t := atoiSafe(s.get(typeIdx), 0)
	host, login := s.text(hostIdx), s.text(loginIdx)
	switch t {
	case 1, 2, 3:
		if host == "" {
			return nil
		}
		kind := map[int]string{1: "socks4", 2: "socks5", 3: "http"}[t]
		proxy := map[string]any{"type": kind, "host": host, "port": clampProxyPort(s.get(portIdx), kind)}
		if login != "" {
			proxy["username"] = login
		}
		pc.conn.Options["proxy"] = proxy
	case 4:
		*notes = append(*notes, "the Telnet proxy was not imported (not supported)")
	case 5:
		cmd, ok := "", false
		if cmdIdx >= 0 {
			cmd, ok = mobaProxyCommand(s.raw(cmdIdx), host, s.get(portIdx))
		}
		if !ok {
			*notes = append(*notes, "the local proxy command was not imported (empty, or it uses %pass)")
			return nil
		}
		pc.conn.Options["proxyCommand"] = cmd
		*notes = append(*notes, "runs a local proxy command on this machine when connecting: "+cmd)
	case 6, 7:
		if host == "" {
			return nil
		}
		port := atoiSafe(s.get(portIdx), 22)
		if port <= 0 || port > 65535 {
			port = 22
		}
		return []phop{{host: host, port: port, user: login}}
	}
	return nil
}

// mobaProxyCommand translates a MobaXterm/PuTTY local proxy command to AstraTerm's ProxyCommand tokens (%h %p %r %%).
// ok is false for an empty command or one needing the password (%pass), which AstraTerm never substitutes.
func mobaProxyCommand(raw, proxyHost, proxyPort string) (string, bool) {
	s := mobaText(raw)
	if s == "" {
		return "", false
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			out.WriteByte(s[i])
			continue
		}
		rest := strings.ToLower(s[i+1:])
		switch {
		case strings.HasPrefix(rest, "%"):
			out.WriteString("%%")
			i++
		case strings.HasPrefix(rest, "proxyhost"):
			out.WriteString(proxyHost)
			i += len("proxyhost")
		case strings.HasPrefix(rest, "proxyport"):
			out.WriteString(proxyPort)
			i += len("proxyport")
		case strings.HasPrefix(rest, "host"):
			out.WriteString("%h")
			i += len("host")
		case strings.HasPrefix(rest, "port"):
			out.WriteString("%p")
			i += len("port")
		case strings.HasPrefix(rest, "user"):
			out.WriteString("%r")
			i += len("user")
		case strings.HasPrefix(rest, "pass"):
			return "", false
		default:
			out.WriteString("%%")
		}
	}
	return strings.TrimSpace(out.String()), true
}

func clampProxyPort(s, kind string) int {
	p := atoiSafe(s, 0)
	if p < 1 || p > 65535 {
		if kind == "http" {
			return 8080
		}
		return 1080
	}
	return p
}

// mobaIcon maps a session's ImgNum: the type's default icon → "" (AstraTerm's protocol icon), else the closest built-in
// icon, if any.
func mobaIcon(icon, typ int) string {
	if icon < 0 || icon == mobaDefaultIcon[typ] {
		return ""
	}
	if name, ok := mobaIconMap[icon]; ok {
		return "lucide:" + name
	}
	return ""
}

// mobaTabColor converts the custom tab colour (a decimal Windows COLORREF 0x00BBGGRR; -1 none, 536870911 default).
func mobaTabColor(s string) string {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || v < 0 || v > 0xFFFFFF {
		return ""
	}
	return fmt.Sprintf("#%02x%02x%02x", v&0xFF, (v>>8)&0xFF, (v>>16)&0xFF)
}

// ---- terminal block ------------------------------------------------------------------------------------------------

// mobaTerm is the per-session terminal configuration that has a AstraTerm equivalent.
type mobaTerm struct {
	fontPt     int    // font size in points (MobaXterm default 10)
	encoding   string // AstraTerm encoding name ("" = UTF-8)
	cursor     int    // 0 block, 1 underline, 2 line, 3-5 blinking variants; -1 unknown
	log        bool
	termType   string
	scrollback int // 0 = not set
	macro      bool
	colors     bool // a per-session colour scheme was set
}

func parseMobaTerminal(block string) mobaTerm {
	f := strings.Split(block, "%")
	get := func(i int) string {
		if i >= 0 && i < len(f) {
			return strings.TrimSpace(f[i])
		}
		return ""
	}
	t := mobaTerm{fontPt: atoiSafe(get(1), 0), cursor: atoiSafe(get(9), -1), log: get(11) == "-1", termType: get(13)}
	switch get(5) {
	case "0":
		t.encoding = "iso-8859-1"
	case "13":
		t.encoding = "iso-8859-15"
	case "22":
		t.encoding = "cp850"
	}
	shift := 0
	if cs := get(16); cs != "" && !strings.HasPrefix(cs, "_") && strings.Count(cs, ",") == 2 {
		shift, t.colors = 15, true // 16 "r,g,b" colours instead of the "_Std_Colors_0_" token
	}
	t.macro = get(22+shift) == "<custom macro>"
	if exp := get(28 + shift); exp != "" {
		parts := strings.Split(exp, ",")
		if n := atoiSafe(parts[len(parts)-1], 0); n > 0 && len(parts) >= 2 { // scrollback is the last item
			t.scrollback = n
		}
	}
	return t
}

// apply maps the terminal settings onto a terminal-type connection and returns notes for the preview.
func (t mobaTerm) apply(pc *pconn, present bool) []string {
	if !present {
		return nil
	}
	o := pc.conn.Options
	var notes []string
	switch strings.ToLower(t.termType) {
	case "xterm-256color", "vt100", "vt220", "xterm-r6":
		o["term"] = strings.ToLower(t.termType)
	}
	if t.encoding != "" {
		o["encoding"] = t.encoding
	}
	if t.log {
		o["log"] = true
	}
	ov := map[string]any{}
	if t.fontPt > 0 && t.fontPt != 10 {
		px := int(math.Round(float64(t.fontPt) * 4 / 3))
		ov["fontSize"] = min(max(px, 6), 72)
	}
	switch t.cursor {
	case 1:
		ov["cursorStyle"], ov["cursorBlink"] = "underline", false
	case 2:
		ov["cursorStyle"], ov["cursorBlink"] = "bar", false
	case 3:
		ov["cursorStyle"], ov["cursorBlink"] = "block", true
	case 4:
		ov["cursorStyle"], ov["cursorBlink"] = "underline", true
	case 5:
		ov["cursorStyle"], ov["cursorBlink"] = "bar", true
	}
	if t.scrollback > 0 {
		ov["scrollback"] = min(t.scrollback, 1_000_000)
	}
	if len(ov) > 0 {
		o["terminal"] = ov
	}
	if t.colors {
		notes = append(notes, "the session's own colour scheme was not imported (pick a AstraTerm theme)")
	}
	if t.macro {
		notes = append(notes, "the login macro was not imported (use a startup command or a snippet)")
	}
	return notes
}
