package importer

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// PuTTY / KiTTY registry-export (.reg) importer (IMP-2). A .reg export of HKCU\Software\SimonTatham\PuTTY\Sessions
// (KiTTY: HKCU\Software\9bis.com\KiTTY\Sessions) has one section per saved session:
//
//	[HKEY_CURRENT_USER\Software\SimonTatham\PuTTY\Sessions\My%20Server]
//	"HostName"="example.com"
//	"PortNumber"=dword:00000016
//	"Protocol"="ssh"
//	"UserName"="root"
//	"PublicKeyFile"="C:\\keys\\id.ppk"
//	"ProxyHost"="proxy.example.com"
//	"ProxyMethod"=dword:00000002
//
// Session key names are %XX-escaped; KiTTY adds a "Folder" value with the tree path. The host key cache
// (…\SshHostKeys: "ssh-ed25519@22:host"="0x…,0x…") becomes trusted host keys. Passwords are never in PuTTY's registry
// (KiTTY's obfuscated "Password" value is ignored), so none are imported.

func parsePuttyReg(content []byte) (*parsed, error) {
	b := newBuilder(fmtPuttyReg)
	text := decodeMaybeCP1252(content)

	var (
		curName     string
		curValues   map[string]regValue
		inHostKeys  bool
		sawAny      bool
		hostKeyLine []string
	)
	flush := func() {
		if curName != "" {
			puttyEmit(b, curName, curValues)
		}
		curName, curValues = "", nil
	}

	forEachLine(text, func(line string) {
		trimmed := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if trimmed == "" {
			return
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			flush()
			inHostKeys = false
			path := trimmed[1 : len(trimmed)-1]
			if strings.HasPrefix(path, "-") {
				return // "[-HKEY…]" deletes a key: nothing to import
			}
			low := strings.ToLower(path)
			if strings.HasSuffix(low, `\sshhostkeys`) {
				inHostKeys, sawAny = true, true
				return
			}
			idx := strings.Index(low, `\sessions\`)
			if idx < 0 {
				return
			}
			name := path[idx+len(`\sessions\`):]
			if name == "" || strings.Contains(name, `\`) { // a sub-key of a session, not a session
				return
			}
			curName = puttyUnescapeKey(name)
			curValues = map[string]regValue{}
			sawAny = true
			return
		}
		if inHostKeys {
			hostKeyLine = append(hostKeyLine, trimmed)
			return
		}
		if curValues == nil {
			return
		}
		if k, v, ok := parseRegKV(trimmed); ok {
			curValues[strings.ToLower(k)] = v
		}
	})
	flush()
	if !sawAny {
		return nil, badRequest("no PuTTY session keys found in the .reg export")
	}
	if len(hostKeyLine) > 0 {
		addPuttyHostKeys(b, hostKeyLine, "the SshHostKeys cache")
	}
	return b.finish()
}

type regValue struct {
	str     string
	dword   int
	isStr   bool
	isDword bool
}

// parseRegKV parses `"Name"="value"` or `"Name"=dword:hhhhhhhh`.
func parseRegKV(line string) (string, regValue, bool) {
	if !strings.HasPrefix(line, `"`) {
		return "", regValue{}, false
	}
	end := indexUnescapedQuote(line[1:])
	if end < 0 {
		return "", regValue{}, false
	}
	key := regUnescapeString(line[1 : 1+end])
	rest := strings.TrimSpace(line[1+end+1:])
	if !strings.HasPrefix(rest, "=") {
		return "", regValue{}, false
	}
	rest = strings.TrimSpace(rest[1:])
	switch {
	case strings.HasPrefix(rest, `"`):
		inner := rest[1:]
		e := indexUnescapedQuote(inner)
		if e < 0 {
			return "", regValue{}, false
		}
		return key, regValue{str: regUnescapeString(inner[:e]), isStr: true}, true
	case strings.HasPrefix(strings.ToLower(rest), "dword:"):
		n, err := strconv.ParseUint(strings.TrimSpace(rest[len("dword:"):]), 16, 32)
		if err != nil {
			return "", regValue{}, false
		}
		return key, regValue{dword: int(int32(uint32(n))), isDword: true}, true
	}
	return "", regValue{}, false // hex(...) / hex: multi-line values are ignored
}

// indexUnescapedQuote returns the index of the first unescaped '"' in s, or -1.
func indexUnescapedQuote(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == '"' {
			return i
		}
	}
	return -1
}

// regUnescapeString reverses .reg string escaping (\\ and \").
func regUnescapeString(s string) string {
	return strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(s)
}

// puttyUnescapeKey decodes a PuTTY session key name (%XX hex escapes of the ANSI bytes; a lone '%' or bad escape is
// kept literally). Non-UTF-8 results are read as Windows-1252.
func puttyUnescapeKey(s string) string {
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				out = append(out, byte(v))
				i += 2
				continue
			}
		}
		out = append(out, s[i])
	}
	if utf8.Valid(out) {
		return string(out)
	}
	if dec, err := charmap.Windows1252.NewDecoder().Bytes(out); err == nil {
		return string(dec)
	}
	return string(out)
}

func puttyEmit(b *builder, name string, v map[string]regValue) {
	if strings.EqualFold(name, "Default Settings") {
		return // PuTTY's template for new sessions, not a session
	}
	protoStr := strings.ToLower(strings.TrimSpace(v["protocol"].str))
	proto := puttyProtocol(protoStr)
	if proto == "" {
		b.p.unsupported++
		b.warn("Skipped %q: PuTTY protocol %q is not supported", name, protoStr)
		return
	}
	host := strings.TrimSpace(v["hostname"].str)
	user := strings.TrimSpace(v["username"].str)
	if at := strings.LastIndexByte(host, '@'); at > 0 && proto != model.ProtoSerial { // "user@host"
		if user == "" {
			user = host[:at]
		}
		host = host[at+1:]
	}
	if host == "" && proto != model.ProtoSerial {
		b.p.unsupported++
		b.warn("Skipped %q: no HostName", name)
		return
	}
	folderID := ""
	if folder := strings.TrimSpace(v["folder"].str); folder != "" && !strings.EqualFold(folder, "Default") {
		folderID = b.folderPath("", strings.FieldsFunc(folder, func(r rune) bool { return r == '\\' || r == '/' })...)
	}
	c := model.Connection{
		Name:     cleanName(name, host),
		Protocol: proto,
		Host:     host,
		Username: user,
		Options:  model.Options{},
	}
	port := 0
	if pv := v["portnumber"]; pv.isDword {
		port = pv.dword
	}
	c.Port = clampPort(port, proto)
	o := c.Options
	var notes []string
	if proto == model.ProtoSerial {
		c.Host, c.Port = "", 0
		if line := strings.TrimSpace(v["serialline"].str); line != "" {
			o["device"] = line
		}
		if sp := v["serialspeed"]; sp.isDword && sp.dword > 0 {
			o["baud"] = sp.dword
		}
		if db := v["serialdatabits"]; db.isDword && db.dword >= 5 && db.dword <= 9 {
			o["dataBits"] = db.dword
		}
		switch sb := v["serialstophalfbits"]; {
		case sb.isDword && sb.dword == 3:
			o["stopBits"] = "1.5"
		case sb.isDword && sb.dword == 4:
			o["stopBits"] = "2"
		}
		if p := v["serialparity"]; p.isDword {
			if s, ok := map[int]string{0: "none", 1: "odd", 2: "even", 3: "mark", 4: "space"}[p.dword]; ok {
				o["parity"] = s
			}
		}
		if fc := v["serialflowcontrol"]; fc.isDword {
			if s, ok := map[int]string{0: "none", 1: "xonxoff", 2: "rtscts", 3: "dsrdtr"}[fc.dword]; ok {
				o["flowControl"] = s
			}
		}
	}
	if v["compression"].isDword && v["compression"].dword != 0 {
		o["compression"] = true
	}
	if v["agentfwd"].isDword && v["agentfwd"].dword != 0 {
		o["agentForwarding"] = true
	}
	if v["x11forward"].isDword && v["x11forward"].dword != 0 {
		o["x11Forwarding"] = true
	}
	if rc := strings.TrimSpace(v["remotecommand"].str); rc != "" && proto == model.ProtoSSH {
		o["remoteCommand"] = rc
	}
	if tt := strings.ToLower(strings.TrimSpace(v["terminaltype"].str)); tt != "" && tt != "xterm" {
		o["term"] = tt
	}
	if enc := puttyEncoding(v["linecodepage"].str); enc != "" {
		o["encoding"] = enc
	}
	if fh := v["fontheight"]; fh.isDword && fh.dword > 0 && fh.dword != 10 && proto != model.ProtoRaw {
		o["terminal"] = map[string]any{"fontSize": min(max(int(math.Round(float64(fh.dword)*4/3)), 6), 72)}
	}
	if strings.TrimSpace(v["detachedcertificate"].str) != "" {
		notes = append(notes, "the detached certificate was not imported (attach it to the key in Keys)")
	}
	pc := b.conn(folderID, c)
	if kf := strings.TrimSpace(v["publickeyfile"].str); kf != "" && proto == model.ProtoSSH {
		pc.keyPath = kf
		pc.conn.AuthMethod = model.AuthKey
		notes = append(notes, "uses private key "+kf+" (imported in desktop mode when readable)")
	}
	pc.hops = puttyProxy(pc, v, &notes)
	if fwd := strings.TrimSpace(v["portforwardings"].str); fwd != "" && proto == model.ProtoSSH {
		if list := puttyForwards(fwd); len(list) > 0 {
			pc.conn.Options["forwards"] = list
		}
	}
	pc.warnings = append(pc.warnings, notes...)
}

// puttyProtocol maps PuTTY's Protocol value ("" = PuTTY's default, SSH).
func puttyProtocol(s string) model.Protocol {
	switch s {
	case "ssh", "":
		return model.ProtoSSH
	case "telnet":
		return model.ProtoTelnet
	case "rlogin":
		return model.ProtoRlogin
	case "raw":
		return model.ProtoRaw
	case "serial":
		return model.ProtoSerial
	}
	return ""
}

var reCodePage = regexp.MustCompile(`(?i)^(ISO-8859-\d+|KOI8-[RU]|CP\d+|Win\d+|UTF-8)`)

// puttyEncoding maps PuTTY's LineCodePage ("ISO-8859-1:1998 (Latin-1, West Europe)", "Win1251 (Cyrillic)", "CP866",
// "UTF-8", "Use font encoding") to a AstraTerm encoding name ("" = unchanged / UTF-8).
func puttyEncoding(cp string) string {
	m := reCodePage.FindString(strings.TrimSpace(cp))
	if m == "" {
		return ""
	}
	low := strings.ToLower(m)
	switch {
	case low == "utf-8":
		return ""
	case strings.HasPrefix(low, "win"):
		return "windows-" + low[3:]
	}
	return low
}

// puttyProxy maps ProxyMethod (0 none, 1 SOCKS4, 2 SOCKS5, 3 HTTP, 4 Telnet, 5 local command, 6-8 SSH proxy) with
// ProxyHost/ProxyPort/ProxyUsername to options.proxy, options.proxyCommand or a first jump hop (returned).
func puttyProxy(pc *pconn, v map[string]regValue, notes *[]string) []phop {
	method := 0
	if v["proxymethod"].isDword {
		method = v["proxymethod"].dword
	}
	host := strings.TrimSpace(v["proxyhost"].str)
	port := 0
	if v["proxyport"].isDword {
		port = v["proxyport"].dword
	}
	user := strings.TrimSpace(v["proxyusername"].str)
	switch method {
	case 1, 2, 3:
		if host == "" {
			return nil
		}
		kind := map[int]string{1: "socks4", 2: "socks5", 3: "http"}[method]
		if port <= 0 || port > 65535 {
			port = map[string]int{"socks4": 1080, "socks5": 1080, "http": 8080}[kind]
		}
		proxy := map[string]any{"type": kind, "host": host, "port": port}
		if user != "" {
			proxy["username"] = user
		}
		pc.conn.Options["proxy"] = proxy
	case 4:
		*notes = append(*notes, "the Telnet proxy was not imported (not supported)")
	case 5:
		cmd, ok := mobaProxyCommand(v["proxytelnetcommand"].str, host, strconv.Itoa(port))
		if !ok {
			*notes = append(*notes, "the local proxy command was not imported (empty, or it uses %pass)")
			return nil
		}
		pc.conn.Options["proxyCommand"] = cmd
		*notes = append(*notes, "runs a local proxy command on this machine when connecting: "+cmd)
	case 6, 7, 8:
		if host == "" {
			return nil
		}
		if port <= 0 || port > 65535 {
			port = 22
		}
		return []phop{{host: host, port: port, user: user}}
	}
	return nil
}

// puttyForwards parses PuTTY's PortForwardings value: comma-separated "[4|6]L[bind:]port=host:port", "…R…", "…D[bind:]port".
func puttyForwards(s string) []map[string]any {
	out := []map[string]any{}
	for _, e := range strings.Split(s, ",") {
		e = strings.TrimSpace(e)
		if len(e) > 0 && (e[0] == '4' || e[0] == '6') { // address-family prefix
			e = e[1:]
		}
		if e == "" {
			continue
		}
		dir := e[0]
		rest := e[1:]
		// Dynamic forwards have no "=dest": "D1080" or "D127.0.0.1:1080".
		if dir == 'D' || dir == 'd' {
			bh, bp := splitBindSpec(rest)
			if bp > 0 {
				out = append(out, map[string]any{"type": "dynamic", "bindHost": bh, "bindPort": bp})
			}
			continue
		}
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			continue
		}
		bindHost, bindPort := splitBindSpec(rest[:eq])
		dh, dp := splitBindSpec(rest[eq+1:])
		if bindPort == 0 || dh == "" || dp == 0 {
			continue
		}
		switch dir {
		case 'L', 'l':
			out = append(out, map[string]any{"type": "local", "bindHost": bindHost, "bindPort": bindPort, "destHost": dh, "destPort": dp})
		case 'R', 'r':
			out = append(out, map[string]any{"type": "remote", "bindHost": bindHost, "bindPort": bindPort, "destHost": dh, "destPort": dp})
		}
	}
	return out
}

// splitHostPortLoose splits "host:port" or bare "port"; a missing host yields "".
func splitHostPortLoose(s string) (string, int) {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		return strings.TrimSpace(s[:i]), atoiSafe(s[i+1:], 0)
	}
	return "", atoiSafe(s, 0)
}
