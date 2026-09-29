package importer

import (
	"bytes"
	"encoding/xml"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"

	"github.com/termstead/termstead/internal/model"
)

// XML-based importers: mRemoteNG (confCons.xml), FileZilla (sitemanager.xml) and SecureCRT (VanDyke export).
// Passwords in these formats are AES-encrypted / obfuscated and are never decrypted or imported.
//
// Untrusted-input note: encoding/xml does not resolve external entities or DTDs (no XXE) and rejects references to
// undeclared entities in strict mode (no entity-expansion bombs); element nesting is bounded by the decoder.

// decodeXML unmarshals XML with a strict decoder that also accepts UTF-16 files (BOM) and legacy declared charsets.
func decodeXML(content []byte, v any) error {
	data, utf16 := content, false
	if s, ok := decodeUTF16(content); ok {
		data, utf16 = []byte(s), true
	}
	d := xml.NewDecoder(bytes.NewReader(dropBOM(data)))
	d.Strict = true
	d.CharsetReader = func(label string, in io.Reader) (io.Reader, error) {
		l := strings.ToLower(strings.TrimSpace(label))
		if l == "utf-8" || l == "utf8" || (utf16 && strings.HasPrefix(l, "utf-16")) {
			return in, nil
		}
		enc, err := htmlindex.Get(l)
		if err != nil {
			return nil, err
		}
		return enc.NewDecoder().Reader(in), nil
	}
	return d.Decode(v)
}

// xmlError shortens a decoder error for the user.
func xmlError(kind string, err error) error {
	return badRequest("invalid " + kind + " XML: " + truncate(err.Error(), 200))
}

// ---- mRemoteNG ----------------------------------------------------------------------------------------------------

type mrngNode struct {
	Attrs []xml.Attr `xml:",any,attr"`
	Nodes []mrngNode `xml:"Node"`
}

type mrngRoot struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Nodes   []mrngNode `xml:"Node"`
}

func xmlAttrs(attrs []xml.Attr) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, a := range attrs {
		m[strings.ToLower(a.Name.Local)] = a.Value
	}
	return m
}

func parseMRemoteNG(content []byte) (*parsed, error) {
	var root mrngRoot
	if err := decodeXML(content, &root); err != nil {
		return nil, xmlError("mRemoteNG", err)
	}
	ra := xmlAttrs(root.Attrs)
	if strings.EqualFold(ra["fullfileencryption"], "true") {
		return nil, badRequest("this mRemoteNG file is fully encrypted — export it unencrypted (File → Export, no full-file encryption) and import that file")
	}
	b := newBuilder(fmtMRemoteNG)
	var walk func(parentFolder string, parent map[string]string, nodes []mrngNode)
	walk = func(parentFolder string, parent map[string]string, nodes []mrngNode) {
		for _, n := range nodes {
			eff := mrngEffective(xmlAttrs(n.Attrs), parent)
			if strings.EqualFold(eff["type"], "Container") {
				fid := b.folder(parentFolder, eff["name"], "", "")
				walk(fid, eff, n.Nodes)
				continue
			}
			mrngConn(b, parentFolder, eff)
			if len(n.Nodes) > 0 { // tolerate connections with children (older exports)
				walk(parentFolder, eff, n.Nodes)
			}
		}
	}
	walk("", map[string]string{}, root.Nodes)
	if len(b.p.conns) == 0 && len(b.p.folders) == 0 {
		return nil, badRequest("no connections found in the mRemoteNG file")
	}
	return b.finish()
}

// mrngEffective applies mRemoteNG inheritance: a value whose Inherit<Name>="true" comes from the parent container.
func mrngEffective(own, parent map[string]string) map[string]string {
	eff := make(map[string]string, len(own))
	for k, v := range own {
		eff[k] = v
	}
	for k, v := range own {
		if strings.HasPrefix(k, "inherit") && strings.EqualFold(v, "true") {
			name := strings.TrimPrefix(k, "inherit")
			if pv, ok := parent[name]; ok {
				eff[name] = pv
			}
		}
	}
	return eff
}

func mrngConn(b *builder, folderID string, a map[string]string) {
	name := a["name"]
	proto := mrngProtocol(a["protocol"])
	if proto == "" {
		b.p.unsupported++
		b.warn("Skipped %q: unsupported mRemoteNG protocol %q", name, a["protocol"])
		return
	}
	host := strings.TrimSpace(a["hostname"])
	if host == "" {
		b.p.unsupported++
		b.warn("Skipped %q: no hostname", name)
		return
	}
	c := model.Connection{
		Name:     cleanName(name, host),
		Protocol: proto,
		Host:     host,
		Port:     clampPort(atoiSafe(a["port"], 0), proto),
		Username: strings.TrimSpace(a["username"]),
		Notes:    strings.TrimSpace(a["descr"]),
		Options:  model.Options{},
	}
	on := func(k string) bool { return strings.EqualFold(a[k], "true") }
	switch proto {
	case model.ProtoRDP:
		if d := strings.TrimSpace(a["domain"]); d != "" {
			c.Options["domain"] = d
		}
		if on("useconsolesession") {
			c.Options["console"] = true
		}
		if on("redirectdiskdrives") {
			c.Options["enableDrive"] = true
		}
		if on("redirectprinters") {
			c.Options["enablePrinting"] = true
		}
		if strings.EqualFold(a["redirectsound"], "BringToThisComputer") {
			c.Options["enableAudio"] = true
		}
		if strings.EqualFold(a["redirectclipboard"], "false") {
			c.Options["disableClipboard"] = true
		}
		if res := strings.TrimPrefix(a["resolution"], "Res"); strings.Contains(res, "x") {
			applyResolution(&c, res)
		}
		switch a["colors"] {
		case "Colors256":
			c.Options["colorDepth"] = 8
		case "Colors15Bit", "Colors16Bit":
			c.Options["colorDepth"] = 16
		case "Colors24Bit":
			c.Options["colorDepth"] = 24
		case "Colors32Bit":
			c.Options["colorDepth"] = 32
		}
		if gw := strings.TrimSpace(a["rdgatewayhostname"]); gw != "" && !strings.EqualFold(a["rdgatewayusagemethod"], "Never") {
			c.Options["gatewayHost"] = gw
			if u := strings.TrimSpace(a["rdgatewayusername"]); u != "" {
				c.Options["gatewayUsername"] = u
			}
			if d := strings.TrimSpace(a["rdgatewaydomain"]); d != "" {
				c.Options["gatewayDomain"] = d
			}
		}
	case model.ProtoVNC:
		if on("vncviewonly") {
			c.Options["viewOnly"] = true
		}
	}
	b.conn(folderID, c)
}

func mrngProtocol(s string) model.Protocol {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ssh2", "ssh1", "ssh":
		return model.ProtoSSH
	case "rdp":
		return model.ProtoRDP
	case "vnc":
		return model.ProtoVNC
	case "telnet":
		return model.ProtoTelnet
	case "rlogin":
		return model.ProtoRlogin
	case "raw":
		return model.ProtoRaw
	}
	return "" // HTTP/HTTPS/ICA/IntApp/PowerShell have no Termstead connection type
}

// ---- FileZilla ----------------------------------------------------------------------------------------------------

type fzServer struct {
	Host      string `xml:"Host"`
	Port      string `xml:"Port"`
	Protocol  string `xml:"Protocol"`
	Type      string `xml:"Type"`
	User      string `xml:"User"`
	Logontype string `xml:"Logontype"`
	Keyfile   string `xml:"Keyfile"`
	Name      string `xml:"Name"`
	RemoteDir string `xml:"RemoteDir"`
	Comments  string `xml:"Comments"`
}

type fzFolder struct {
	Name    string     `xml:",chardata"`
	Servers []fzServer `xml:"Server"`
	Folders []fzFolder `xml:"Folder"`
}

type fzRoot struct {
	XMLName xml.Name
	Servers struct {
		Servers []fzServer `xml:"Server"`
		Folders []fzFolder `xml:"Folder"`
	} `xml:"Servers"`
}

func parseFileZilla(content []byte) (*parsed, error) {
	var root fzRoot
	if err := decodeXML(content, &root); err != nil {
		return nil, xmlError("FileZilla", err)
	}
	if !strings.EqualFold(root.XMLName.Local, "FileZilla3") {
		return nil, badRequest("not a FileZilla sitemanager.xml")
	}
	b := newBuilder(fmtFileZilla)
	for _, s := range root.Servers.Servers {
		fzConn(b, "", s)
	}
	var walk func(parent string, folders []fzFolder)
	walk = func(parent string, folders []fzFolder) {
		for _, f := range folders {
			fid := b.folder(parent, strings.TrimSpace(f.Name), "", "")
			for _, s := range f.Servers {
				fzConn(b, fid, s)
			}
			walk(fid, f.Folders)
		}
	}
	walk("", root.Servers.Folders)
	if len(b.p.conns) == 0 {
		return nil, badRequest("no importable servers found in the FileZilla site manager")
	}
	return b.finish()
}

func fzConn(b *builder, folderID string, s fzServer) {
	host := strings.TrimSpace(s.Host)
	name := strings.TrimSpace(s.Name)
	if host == "" {
		b.p.unsupported++
		b.warn("Skipped %q: no host", name)
		return
	}
	proto, tls, ok := fzProtocol(s.Protocol)
	if !ok {
		b.p.unsupported++
		b.warn("Skipped %q: FileZilla protocol %s has no Termstead equivalent", cleanName(name, host), strings.TrimSpace(s.Protocol))
		return
	}
	c := model.Connection{
		Name:     cleanName(name, host),
		Protocol: proto,
		Host:     host,
		Port:     clampPort(atoiSafe(s.Port, 0), proto),
		Username: strings.TrimSpace(s.User),
		Notes:    strings.TrimSpace(s.Comments),
		Options:  model.Options{},
	}
	var notes []string
	if tls != "" {
		c.Options["ftpTls"] = tls
	}
	if strings.TrimSpace(s.Protocol) == "0" || strings.TrimSpace(s.Protocol) == "" {
		notes = append(notes, "FileZilla used TLS only when the server offered it — set FTPS to None if this server has no TLS")
	}
	if proto == model.ProtoS3 && c.Username != "" {
		c.Options["accessKeyId"] = c.Username
		c.Username = ""
		notes = append(notes, "enter the S3 secret access key (it is not imported)")
	}
	if rd := strings.TrimSpace(s.RemoteDir); rd != "" {
		if p := fzDecodeRemoteDir(rd); p != "" {
			c.Options["initialPath"] = p
		}
	}
	pc := b.conn(folderID, c)
	if kf := strings.TrimSpace(s.Keyfile); kf != "" && proto == model.ProtoSFTP {
		pc.keyPath = kf
		pc.conn.AuthMethod = model.AuthKey
		notes = append(notes, "uses private key "+kf+" (imported in desktop mode when readable)")
	}
	pc.warnings = append(pc.warnings, notes...)
}

// fzProtocol maps FileZilla's ServerProtocol: 0 FTP (TLS if available) · 1 SFTP · 3 FTPS implicit · 4 FTPES explicit
// · 6 plain FTP · 7 S3. HTTP(S), WebDAV and the cloud-storage protocols have no Termstead connection type.
func fzProtocol(code string) (model.Protocol, string, bool) {
	switch strings.TrimSpace(code) {
	case "0", "":
		return model.ProtoFTP, "explicit", true
	case "1":
		return model.ProtoSFTP, "", true
	case "3":
		return model.ProtoFTP, "implicit", true
	case "4":
		return model.ProtoFTP, "explicit", true
	case "6":
		return model.ProtoFTP, "none", true
	case "7":
		return model.ProtoS3, "", true
	}
	return "", "", false
}

// fzDecodeRemoteDir decodes FileZilla's length-prefixed "safe path" ("<type> <prefix len> [prefix ]<len> <segment>
// …", e.g. "1 0 4 home 6 deploy" = /home/deploy); segments may contain spaces, their length is authoritative.
// Anything else is returned as is.
func fzDecodeRemoteDir(s string) string {
	rest := s
	readInt := func() (int, bool) {
		rest = strings.TrimLeft(rest, " ")
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 0 {
			return 0, false
		}
		n, err := strconv.Atoi(rest[:i])
		if err != nil {
			return 0, false
		}
		rest = strings.TrimPrefix(rest[i:], " ")
		return n, true
	}
	take := func(n int) (string, bool) { // n characters (runes)
		if n > utf8.RuneCountInString(rest) {
			return "", false
		}
		r := []rune(rest)
		out := string(r[:n])
		rest = strings.TrimPrefix(string(r[n:]), " ")
		return out, true
	}
	typ, ok := readInt()
	if !ok {
		return s
	}
	plen, ok := readInt()
	if !ok {
		return s
	}
	prefix, ok := take(plen)
	if !ok {
		return s
	}
	var segs []string
	for strings.TrimSpace(rest) != "" {
		n, ok := readInt()
		if !ok {
			return s
		}
		seg, ok := take(n)
		if !ok {
			return s
		}
		segs = append(segs, seg)
	}
	if typ == 3 || typ == 8 { // DOS-style paths
		return prefix + strings.Join(segs, `\`)
	}
	return prefix + "/" + strings.Join(segs, "/")
}

// ---- SecureCRT ----------------------------------------------------------------------------------------------------

type scrtVal struct {
	Name  string `xml:"name,attr"`
	Value string `xml:",chardata"`
}

type scrtKey struct {
	Name    string    `xml:"name,attr"`
	Keys    []scrtKey `xml:"key"`
	Strings []scrtVal `xml:"string"`
	Dwords  []scrtVal `xml:"dword"`
}

type scrtRoot struct {
	XMLName xml.Name
	Keys    []scrtKey `xml:"key"`
}

func (k scrtKey) str(name string) string {
	for _, v := range k.Strings {
		if strings.EqualFold(v.Name, name) {
			return strings.TrimSpace(v.Value)
		}
	}
	return ""
}

// dword reads a SecureCRT DWORD: the XML export writes decimal ("22"); session .ini files use 8 hex digits
// ("00000016"), accepted too.
func (k scrtKey) dword(name string) int {
	for _, v := range k.Dwords {
		if !strings.EqualFold(v.Name, name) {
			continue
		}
		s := strings.TrimSpace(v.Value)
		base := 10
		if len(s) == 8 && strings.Trim(strings.ToLower(s), "0123456789abcdef") == "" {
			base = 16
		}
		if n, err := strconv.ParseInt(s, base, 64); err == nil {
			return int(n)
		}
	}
	return 0
}

func parseSecureCRT(content []byte) (*parsed, error) {
	var root scrtRoot
	if err := decodeXML(content, &root); err != nil {
		return nil, xmlError("SecureCRT", err)
	}
	var sessions *scrtKey
	for i := range root.Keys {
		if strings.EqualFold(root.Keys[i].Name, "Sessions") {
			sessions = &root.Keys[i]
			break
		}
	}
	if sessions == nil {
		return nil, badRequest("no <key name=\"Sessions\"> found in the SecureCRT export")
	}
	b := newBuilder(fmtSecureCRT)
	var walk func(folderID string, k scrtKey)
	walk = func(folderID string, k scrtKey) {
		for _, child := range k.Keys {
			if strings.EqualFold(child.Name, "Default") || strings.EqualFold(child.Name, "__FolderData__") {
				continue
			}
			if scrtIsSession(child) {
				scrtConn(b, folderID, child)
			} else {
				walk(b.folder(folderID, child.Name, "", ""), child)
			}
		}
	}
	walk("", *sessions)
	if len(b.p.conns) == 0 {
		return nil, badRequest("no importable sessions found in the SecureCRT export")
	}
	return b.finish()
}

func scrtIsSession(k scrtKey) bool {
	return k.str("Hostname") != "" || k.str("Protocol Name") != ""
}

func scrtConn(b *builder, folderID string, k scrtKey) {
	protoName := k.str("Protocol Name")
	proto := parseProtocolName(protoName)
	if proto == "" && protoName == "" {
		proto = model.ProtoSSH
	}
	if proto == "" {
		b.p.unsupported++
		b.warn("Skipped %q: SecureCRT protocol %q is not supported", k.Name, protoName)
		return
	}
	if proto == model.ProtoSerial {
		dev := k.str("Com Port")
		if dev == "" {
			b.p.unsupported++
			b.warn("Skipped %q: no serial port", k.Name)
			return
		}
		c := model.Connection{Name: cleanName(k.Name, dev), Protocol: proto, Options: model.Options{"device": dev}}
		if baud := k.dword("Baud Rate"); baud > 0 {
			c.Options["baud"] = baud
		}
		b.conn(folderID, c)
		return
	}
	host := k.str("Hostname")
	if host == "" {
		b.p.unsupported++
		b.warn("Skipped %q: no host name", k.Name)
		return
	}
	// Every session carries defaults for other protocols too, so read the port key of its own protocol.
	portKeys := []string{"Port"}
	switch strings.ToLower(protoName) {
	case "ssh2", "":
		portKeys = []string{"[SSH2] Port"}
	case "ssh1":
		portKeys = []string{"[SSH1] Port"}
	case "telnet", "telnet/ssl":
		portKeys = []string{"[Telnet] Port", "Port"}
	case "rlogin":
		portKeys = []string{"[RLogin] Port", "Port"}
	}
	port := 0
	for _, key := range portKeys {
		if p := k.dword(key); p > 0 {
			port = p
			break
		}
	}
	c := model.Connection{
		Name:     cleanName(k.Name, host),
		Protocol: proto,
		Host:     host,
		Port:     clampPort(port, proto),
		Username: k.str("Username"),
		Notes:    k.str("Description"),
		Options:  model.Options{},
	}
	if strings.EqualFold(protoName, "telnet/ssl") {
		c.Options["tls"] = true
	}
	pc := b.conn(folderID, c)
	if id := k.str("Identity Filename V2"); id != "" && proto == model.ProtoSSH {
		if i := strings.Index(id, "::"); i >= 0 { // "<path>::rawkey"
			id = id[:i]
		}
		if id = strings.TrimSpace(id); id != "" {
			pc.keyPath = id
			pc.conn.AuthMethod = model.AuthKey
			pc.warnings = append(pc.warnings, "uses private key "+id+" (imported in desktop mode when readable)")
		}
	}
}
