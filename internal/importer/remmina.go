package importer

import (
	"strings"

	"github.com/nexterm/nexterm/internal/model"
)

// Remmina .remmina importer (IMP-2). Each file is an INI with a `[remmina]` section; several files may be
// concatenated (every `[remmina]` block is parsed). Passwords in Remmina are encrypted with the user's secret key
// and are not imported. Remmina's SSH tunnel (ssh_tunnel_*) becomes the connection's SSH gateway.

func parseRemmina(content []byte) (*parsed, error) {
	f := parseINI(content)
	b := newBuilder(fmtRemmina)
	found := 0
	for _, sec := range f.Sections {
		if !strings.EqualFold(sec.Name, "remmina") {
			continue
		}
		found++
		remminaConn(b, sec)
	}
	if found == 0 {
		return nil, badRequest("no [remmina] connection found")
	}
	return b.finish()
}

func remminaConn(b *builder, sec *iniSection) {
	get := func(k string) string { return strings.TrimSpace(sec.get(k)) }
	name := get("name")
	proto := parseProtocolName(get("protocol"))
	if proto == "" || proto == model.ProtoRaw {
		b.p.unsupported++
		b.warn("Skipped %q: unsupported Remmina protocol %q", name, get("protocol"))
		return
	}
	host, port := splitHostPortDefault(get("server"))
	if host == "" {
		b.p.unsupported++
		b.warn("Skipped %q: no server", name)
		return
	}
	user := get("username")
	if user == "" {
		user = get("ssh_username")
	}
	c := model.Connection{
		Name:     cleanName(name, host),
		Protocol: proto,
		Host:     host,
		Port:     clampPort(port, proto),
		Username: user,
		Notes:    get("notes_text"),
		Options:  model.Options{},
	}
	switch proto {
	case model.ProtoRDP:
		if d := get("domain"); d != "" {
			c.Options["domain"] = d
		}
		if get("console") == "1" {
			c.Options["console"] = true
		}
		if w, h := atoiSafe(get("resolution_width"), 0), atoiSafe(get("resolution_height"), 0); w > 0 && h > 0 {
			c.Options["width"], c.Options["height"] = w, h
		} else if r := get("resolution"); r != "" {
			applyResolution(&c, r)
		}
		switch get("colordepth") {
		case "8", "16", "24", "32":
			c.Options["colorDepth"] = atoiSafe(get("colordepth"), 0)
		}
		if gw := get("gateway_server"); gw != "" && get("gateway_usage") != "0" {
			gh, gp := splitHostPortDefault(gw)
			c.Options["gatewayHost"] = gh
			if gp > 0 {
				c.Options["gatewayPort"] = gp
			}
			if gu := get("gateway_username"); gu != "" {
				c.Options["gatewayUsername"] = gu
			}
			if gd := get("gateway_domain"); gd != "" {
				c.Options["gatewayDomain"] = gd
			}
		}
	case model.ProtoVNC:
		if get("viewonly") == "1" {
			c.Options["viewOnly"] = true
		}
	}
	folderID := ""
	if g := get("group"); g != "" {
		folderID = b.folderPath("", strings.FieldsFunc(g, func(r rune) bool { return r == '/' || r == '\\' })...)
	}
	pc := b.conn(folderID, c)
	if kp := get("ssh_privatekey"); kp != "" && isSSHFamily(proto) {
		pc.keyPath = expandTilde(kp)
		pc.conn.AuthMethod = model.AuthKey
		pc.warnings = append(pc.warnings, "uses private key "+kp+" (imported in desktop mode when readable)")
	}
	if get("ssh_tunnel_enabled") == "1" && !isSSHFamily(proto) {
		if th, tp := splitHostPortDefault(get("ssh_tunnel_server")); th != "" {
			hop := phop{host: th, port: tp, user: get("ssh_tunnel_username")}
			if hop.port == 0 {
				hop.port = 22
			}
			if kp := get("ssh_tunnel_privatekey"); kp != "" {
				hop.keyPath = expandTilde(kp)
			}
			pc.hops = append(pc.hops, hop)
		}
	}
}

// splitHostPortDefault splits "host", "host:port", "[v6]" or "[v6]:port" (a bare IPv6 literal has no port).
func splitHostPortDefault(s string) (string, int) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "["):
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return "", 0
		}
		host, rest := s[1:end], s[end+1:]
		if strings.HasPrefix(rest, ":") {
			return host, atoiSafe(rest[1:], 0)
		}
		return host, 0
	case strings.Count(s, ":") == 1:
		i := strings.IndexByte(s, ':')
		return strings.TrimSpace(s[:i]), atoiSafe(s[i+1:], 0)
	}
	return s, 0
}

// applyResolution parses "1920x1080" into width/height options (RDP/VNC).
func applyResolution(c *model.Connection, res string) {
	parts := strings.SplitN(strings.ToLower(res), "x", 2)
	if len(parts) != 2 {
		return
	}
	w, h := atoiSafe(parts[0], 0), atoiSafe(parts[1], 0)
	if w > 0 && h > 0 {
		c.Options["width"] = w
		c.Options["height"] = h
	}
}
