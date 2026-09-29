package importer

import (
	"regexp"
	"strconv"
	"strings"

	sshconfig "github.com/kevinburke/ssh_config"

	"github.com/termstead/termstead/internal/model"
)

// OpenSSH ~/.ssh/config importer (IMP-2, SSH-36). Concrete `Host` aliases (no wildcards/negation) become SSH
// connections; effective options are resolved with OpenSSH first-match-wins semantics across every matching block
// (including the implicit top-of-file `Host *`). Mapped directives: HostName (%h / %% tokens), Port, User, ProxyJump
// and `ProxyCommand ssh -W %h:%p host` (→ jump chain; hops naming another alias of the file become references to that
// imported connection), other ProxyCommands (→ proxyCommand), IdentityFile (→ key file, imported in desktop mode),
// Local/Remote/DynamicForward (→ options.forwards), ServerAliveInterval (→ keepAliveSec), ConnectTimeout, Compression,
// ForwardAgent, ForwardX11, Ciphers / KexAlgorithms / MACs / HostKeyAlgorithms, RemoteCommand, SetEnv. `Match` blocks
// are not supported by the parser library and are skipped with a warning.

// reMatchLine / reHostLine detect block keywords (keyword and arguments may be separated by spaces or '=').
var (
	reMatchLine = regexp.MustCompile(`(?i)^\s*match(\s|=|$)`)
	reHostLine  = regexp.MustCompile(`(?i)^\s*host(\s|=)`)
	// ProxyCommand forms equivalent to a jump host: "ssh [-q] -W %h:%p host" / "ssh [-q] host -W %h:%p".
	reProxySSHW = regexp.MustCompile(`^ssh(?:\s+-q)?\s+(?:-W\s*%h:%p\s+(\S+)|(\S+)\s+-W\s*%h:%p)$`)
)

// stripMatchBlocks removes `Match` blocks (from a Match line up to the next Host/Match line) and returns the count.
func stripMatchBlocks(text string) (string, int) {
	var out strings.Builder
	n, inMatch := 0, false
	forEachLine(text, func(line string) {
		switch {
		case reMatchLine.MatchString(line):
			inMatch = true
			n++
			return
		case reHostLine.MatchString(line):
			inMatch = false
		}
		if !inMatch {
			out.WriteString(line)
			out.WriteByte('\n')
		}
	})
	return out.String(), n
}

func parseSSHConfig(content []byte) (*parsed, error) {
	text, matches := stripMatchBlocks(decodeMaybeCP1252(content))
	cfg, err := sshconfig.DecodeBytes([]byte(text))
	if err != nil {
		return nil, badRequest("invalid ssh_config: " + err.Error())
	}
	b := newBuilder(fmtSSHConfig)
	if matches > 0 {
		b.warn("%d Match block(s) were skipped (conditional blocks are not supported); check the imported options.", matches)
	}

	// Collect concrete aliases in file order (dedupe, keep first occurrence).
	seen := map[string]bool{}
	var aliases []string
	hasInclude := false
	for _, host := range cfg.Hosts {
		for _, node := range host.Nodes {
			if _, ok := node.(*sshconfig.Include); ok {
				hasInclude = true
			}
		}
		for _, pat := range host.Patterns {
			s := strings.Trim(pat.String(), `"`)
			if s == "" || strings.ContainsAny(s, "*?!") {
				continue
			}
			if !seen[s] {
				seen[s] = true
				aliases = append(aliases, s)
			}
		}
	}
	if hasInclude {
		b.warn("Include directives were not followed (only this text was parsed). Use \"Import from ~/.ssh/config\" in desktop mode to resolve includes.")
	}
	if len(aliases) == 0 {
		return nil, badRequest("no concrete Host entries found in the ssh_config")
	}

	type entry struct {
		alias string
		pc    *pconn
		jump  []phop // unresolved hops (host may name an alias)
	}
	entries := make([]entry, 0, len(aliases))
	byAlias := map[string]*entry{}
	for _, alias := range aliases {
		single, multi := effectiveDirectives(cfg, alias)
		pc, jump := sshConfigConn(b, alias, single, multi)
		if pc.tempID == "" {
			continue
		}
		entries = append(entries, entry{alias: alias, pc: pc, jump: jump})
	}
	for i := range entries {
		byAlias[strings.ToLower(entries[i].alias)] = &entries[i]
	}
	// Resolve hops that name another alias of this file: a bare alias becomes a reference to that connection; an
	// alias with a user/port override is inlined with the alias's effective HostName.
	for _, e := range entries {
		for _, h := range e.jump {
			if ref, ok := byAlias[strings.ToLower(h.host)]; ok && ref.pc != e.pc {
				if h.user == "" && h.port == 0 {
					e.pc.hops = append(e.pc.hops, phop{ref: ref.pc.tempID})
					continue
				}
				h.host = ref.pc.conn.Host
				if h.port == 0 {
					h.port = ref.pc.conn.Port
				}
				if h.user == "" {
					h.user = ref.pc.conn.Username
				}
			}
			e.pc.hops = append(e.pc.hops, h)
		}
	}
	return b.finish()
}

// effectiveDirectives resolves the effective (single-valued) and multi-valued directives for alias across every
// matching Host block, first-match-wins for single keys and accumulated (in order) for multi-valued keys.
func effectiveDirectives(cfg *sshconfig.Config, alias string) (map[string]string, map[string][]string) {
	single := map[string]string{}
	multi := map[string][]string{}
	for _, host := range cfg.Hosts {
		if !host.Matches(alias) {
			continue
		}
		for _, node := range host.Nodes {
			kv, ok := node.(*sshconfig.KV)
			if !ok {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(kv.Key))
			val := strings.TrimSpace(kv.Value)
			if key == "" {
				continue
			}
			if sshMultiKey(key) {
				multi[key] = append(multi[key], val)
			} else if _, ok := single[key]; !ok {
				single[key] = val
			}
		}
	}
	return single, multi
}

func sshMultiKey(key string) bool {
	switch key {
	case "localforward", "remoteforward", "dynamicforward", "identityfile", "setenv", "certificatefile":
		return true
	}
	return false
}

// sshConfigConn builds the connection of one alias; it returns the unresolved jump hops (hosts may name aliases).
func sshConfigConn(b *builder, alias string, s map[string]string, m map[string][]string) (*pconn, []phop) {
	host := expandSSHTokens(s["hostname"], alias, "", "", 0)
	if host == "" {
		host = alias
	}
	user := s["user"]
	port := clampPort(atoiSafe(s["port"], 0), model.ProtoSSH)
	c := model.Connection{
		Name:     cleanName(alias, host),
		Protocol: model.ProtoSSH,
		Host:     host,
		Port:     port,
		Username: user,
		Options:  model.Options{},
	}
	var notes []string
	var jump []phop
	if v := s["proxyjump"]; v != "" && !strings.EqualFold(v, "none") {
		for _, spec := range splitList(v, ",") {
			if h, ok := parseHopSpec(spec); ok {
				jump = append(jump, h)
			} else {
				notes = append(notes, "ProxyJump entry "+spec+" was not understood")
			}
		}
	} else if v := s["proxycommand"]; v != "" && !strings.EqualFold(v, "none") {
		if mm := reProxySSHW.FindStringSubmatch(strings.Join(strings.Fields(v), " ")); mm != nil {
			target := mm[1] + mm[2]
			if h, ok := parseHopSpec(target); ok {
				jump = append(jump, h)
			}
		} else {
			c.Options["proxyCommand"] = v
			notes = append(notes, "runs a local ProxyCommand on this machine when connecting: "+v)
		}
	}
	if v := s["serveraliveinterval"]; v != "" {
		if n := atoiSafe(v, -1); n >= 0 {
			c.Options["keepAliveSec"] = n
		}
	}
	if sshYes(s["compression"]) {
		c.Options["compression"] = true
	}
	if sshYes(s["forwardagent"]) {
		c.Options["agentForwarding"] = true
	}
	if sshYes(s["forwardx11"]) || sshYes(s["forwardx11trusted"]) {
		c.Options["x11Forwarding"] = true
	}
	if v := s["remotecommand"]; v != "" && !strings.EqualFold(v, "none") {
		c.Options["remoteCommand"] = v
	}
	if v := s["connecttimeout"]; v != "" {
		if n := atoiSafe(v, -1); n > 0 {
			c.Options["connectTimeoutSec"] = n
		}
	}
	setAlgs := func(optKey, cfgKey string) {
		if v := s[cfgKey]; v != "" {
			c.Options[optKey] = splitList(v, ",")
		}
	}
	setAlgs("ciphers", "ciphers")
	setAlgs("kex", "kexalgorithms")
	setAlgs("macs", "macs")
	setAlgs("hostKeyAlgorithms", "hostkeyalgorithms")
	if env := sshSetEnv(m["setenv"]); len(env) > 0 {
		c.Options["env"] = env
	}
	if fwds := sshForwards(m); len(fwds) > 0 {
		c.Options["forwards"] = fwds
	}
	if v := s["localcommand"]; v != "" {
		notes = append(notes, "LocalCommand is not supported and was not imported")
	}
	if v := s["stricthostkeychecking"]; v != "" && !strings.EqualFold(v, "ask") {
		notes = append(notes, "StrictHostKeyChecking="+v+" is not imported (Termstead always verifies host keys)")
	}
	if len(m["certificatefile"]) > 0 {
		notes = append(notes, "CertificateFile was not imported (attach the certificate to the key in Keys)")
	}
	pc := b.conn("", c)
	// IdentityFile → key file to import (desktop mode). An explicit directive is always honoured; OpenSSH only falls
	// back to its built-in default paths when none is given, so anything present here was user-set.
	for _, id := range m["identityfile"] {
		id = strings.TrimSpace(id)
		if id == "" || strings.EqualFold(id, "none") {
			continue
		}
		pc.keyPath = expandTilde(expandSSHTokens(id, alias, host, user, port))
		pc.conn.AuthMethod = model.AuthKey
		notes = append(notes, "IdentityFile "+id+" (imported in desktop mode when readable)")
		break
	}
	pc.warnings = append(pc.warnings, notes...)
	return pc, jump
}

// sshForwards maps LocalForward/RemoteForward/DynamicForward directives to options.forwards entries.
func sshForwards(m map[string][]string) []map[string]any {
	out := []map[string]any{}
	for _, lf := range m["localforward"] {
		if f := sshForward(lf, "local"); f != nil {
			out = append(out, f)
		}
	}
	for _, rf := range m["remoteforward"] {
		if fields := strings.Fields(rf); len(fields) == 1 {
			// OpenSSH ≥ 7.6: "RemoteForward [bind:]port" = a SOCKS proxy served on the remote side.
			if bh, bp := splitBindSpec(fields[0]); bp > 0 {
				out = append(out, map[string]any{"type": "dynamic", "reverse": true, "bindHost": bh, "bindPort": bp})
			}
			continue
		}
		if f := sshForward(rf, "remote"); f != nil {
			out = append(out, f)
		}
	}
	for _, df := range m["dynamicforward"] {
		bh, bp := splitBindSpec(strings.TrimSpace(df))
		if bp > 0 {
			out = append(out, map[string]any{"type": "dynamic", "bindHost": bh, "bindPort": bp})
		}
	}
	return out
}

// sshForward parses "[bind:]port host:hostport" (Local/RemoteForward).
func sshForward(v, kind string) map[string]any {
	fields := strings.Fields(v)
	if len(fields) != 2 {
		return nil
	}
	bh, bp := splitBindSpec(fields[0])
	dh, dp := splitBindSpec(fields[1])
	if bp == 0 || dh == "" || dp == 0 {
		return nil
	}
	return map[string]any{"type": kind, "bindHost": bh, "bindPort": bp, "destHost": dh, "destPort": dp}
}

// splitBindSpec parses "port", "host:port" or "[v6]:port" (host may be "*", "localhost", an IP).
func splitBindSpec(s string) (string, int) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") {
		if end := strings.Index(s, "]"); end >= 0 {
			host := s[1:end]
			rest := strings.TrimPrefix(s[end+1:], ":")
			return host, atoiSafe(rest, 0)
		}
	}
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		return strings.TrimSpace(s[:i]), atoiSafe(s[i+1:], 0)
	}
	return "", atoiSafe(s, 0)
}

// sshSetEnv parses SetEnv values: NAME=VALUE tokens, where a value may be double-quoted and contain spaces.
func sshSetEnv(vals []string) map[string]string {
	env := map[string]string{}
	for _, v := range vals {
		for _, tok := range splitQuoted(v) {
			if eq := strings.IndexByte(tok, '='); eq > 0 {
				env[tok[:eq]] = tok[eq+1:]
			}
		}
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

// splitQuoted splits s on whitespace, keeping double-quoted runs together (quotes removed).
func splitQuoted(s string) []string {
	var out []string
	var cur strings.Builder
	inQ, have := false, false
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
			have = true
		case (r == ' ' || r == '\t') && !inQ:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}

// expandSSHTokens expands the OpenSSH percent tokens meaningful at import time: %h (the host — the alias for
// HostName itself), %n (the alias), %r (remote user), %p (port), %d (local home), %u (local user), %% (a literal %).
// Unknown tokens are kept.
func expandSSHTokens(v, alias, host, user string, port int) string {
	if !strings.Contains(v, "%") {
		return v
	}
	if host == "" {
		host = alias
	}
	var out strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] != '%' || i+1 >= len(v) {
			out.WriteByte(v[i])
			continue
		}
		i++
		switch v[i] {
		case '%':
			out.WriteByte('%')
		case 'h':
			out.WriteString(host)
		case 'n':
			out.WriteString(alias)
		case 'r':
			out.WriteString(user)
		case 'p':
			if port > 0 {
				out.WriteString(strconv.Itoa(port))
			} else {
				out.WriteString("22")
			}
		case 'd':
			out.WriteString(userHomeDir())
		case 'u':
			out.WriteString(localUserName())
		default:
			out.WriteByte('%')
			out.WriteByte(v[i])
		}
	}
	return out.String()
}

func sshYes(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "true", "1":
		return true
	}
	return false
}

// expandTilde expands a leading ~/ against the current OS user's home directory (used when reading key files in
// desktop mode). It is only meaningful when the importer later reads the file.
func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home := userHomeDir(); home != "" {
			return home + p[1:]
		}
	}
	return p
}
