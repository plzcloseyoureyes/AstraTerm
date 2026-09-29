package importer

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"

	"github.com/nexterm/nexterm/internal/model"
)

// WinSCP.ini importer (IMP-2). Sessions live in `[Sessions\<name>]` sections; the subkey name is %XX-encoded and
// uses '/' as a folder separator. FSProtocol selects the transfer protocol (0 SCP · 1 SFTP with SCP fallback · 2 SFTP
// · 5 FTP · 6 WebDAV · 7 S3). Passwords are obfuscated and not imported.

func parseWinSCP(content []byte) (*parsed, error) {
	f := parseINI(content)
	b := newBuilder(fmtWinSCP)
	sessions := 0
	for _, sec := range f.Sections {
		low := strings.ToLower(sec.Name)
		if !strings.HasPrefix(low, `sessions\`) {
			continue
		}
		rawName := sec.Name[len("Sessions\\"):]
		if rawName == "" || strings.EqualFold(winscpDecode(rawName), "Default Settings") {
			continue
		}
		sessions++
		winscpConn(b, rawName, sec)
	}
	if sessions == 0 {
		return nil, badRequest("no [Sessions\\...] entries found in the WinSCP.ini")
	}
	return b.finish()
}

func winscpConn(b *builder, rawName string, sec *iniSection) {
	val := func(k string) string { return winscpDecode(strings.TrimSpace(sec.get(k))) }
	// The subkey path is "Folder/Sub/Session"; decode each segment.
	segments := strings.Split(rawName, "/")
	for i := range segments {
		segments[i] = winscpDecode(segments[i])
	}
	name := segments[len(segments)-1]
	host := val("HostName")
	if host == "" {
		b.p.unsupported++
		b.warn("Skipped %q: no host name", name)
		return
	}
	proto := winscpProtocol(sec.get("FSProtocol"))
	if proto == "" {
		b.p.unsupported++
		b.warn("Skipped %q: WinSCP WebDAV / unknown protocols are not imported", name)
		return
	}
	folderID := b.folderPath("", segments[:len(segments)-1]...)
	c := model.Connection{
		Name:     cleanName(name, host),
		Protocol: proto,
		Host:     host,
		Port:     clampPort(atoiSafe(sec.get("PortNumber"), 0), proto),
		Username: val("UserName"),
		Options:  model.Options{},
	}
	var notes []string
	switch proto {
	case model.ProtoFTP:
		switch strings.TrimSpace(sec.get("Ftps")) { // 0 none · 1 implicit · 2 explicit SSL · 3 explicit TLS
		case "1":
			c.Options["ftpTls"] = "implicit"
		case "2", "3":
			c.Options["ftpTls"] = "explicit"
		}
	case model.ProtoS3:
		if c.Username != "" { // WinSCP keeps the access key ID in UserName (the secret key is the password)
			c.Options["accessKeyId"] = c.Username
			c.Username = ""
		}
		if r := val("S3DefaultRegion"); r != "" {
			c.Options["region"] = r
		}
		notes = append(notes, "enter the S3 secret access key (it is not imported)")
	}
	if rd := val("RemoteDirectory"); rd != "" {
		c.Options["initialPath"] = rd
	}
	pc := b.conn(folderID, c)
	if kf := val("PublicKeyFile"); kf != "" && proto == model.ProtoSFTP {
		pc.keyPath = kf
		pc.conn.AuthMethod = model.AuthKey
		notes = append(notes, "uses private key "+kf+" (imported in desktop mode when readable)")
	}
	// SSH tunnel → first jump hop (inline, with its own key).
	if strings.TrimSpace(sec.get("Tunnel")) == "1" {
		if th := val("TunnelHostName"); th != "" {
			hop := phop{host: th, port: 22, user: val("TunnelUserName"), keyPath: val("TunnelPublicKeyFile")}
			if tp := atoiSafe(sec.get("TunnelPortNumber"), 0); tp > 0 && tp <= 65535 {
				hop.port = tp
			}
			pc.hops = append(pc.hops, hop)
		}
	}
	// Proxy (0 none · 1 SOCKS4 · 2 SOCKS5 · 3 HTTP · 4 Telnet · 5 local command).
	if kind := map[string]string{"1": "socks4", "2": "socks5", "3": "http"}[strings.TrimSpace(sec.get("ProxyMethod"))]; kind != "" {
		if ph := val("ProxyHost"); ph != "" {
			proxy := map[string]any{"type": kind, "host": ph, "port": clampProxyPort(sec.get("ProxyPort"), kind)}
			if pu := val("ProxyUsername"); pu != "" {
				proxy["username"] = pu
			}
			pc.conn.Options["proxy"] = proxy
		}
	}
	pc.warnings = append(pc.warnings, notes...)
}

// winscpProtocol maps a WinSCP FSProtocol integer to a NexTerm protocol ("" = not importable).
func winscpProtocol(code string) model.Protocol {
	switch strings.TrimSpace(code) {
	case "0", "1", "2", "":
		return model.ProtoSFTP
	case "5":
		return model.ProtoFTP
	case "7":
		return model.ProtoS3
	default:
		return "" // 6 WebDAV and anything else
	}
}

// winscpDecode decodes WinSCP's %XX escaping in session names / values (ANSI bytes; non-UTF-8 read as Windows-1252).
func winscpDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
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
