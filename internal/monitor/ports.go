package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
)

// ListeningPort is a socket accepting connections (TCP LISTEN) or bound for datagrams (UDP) on a monitored host.
// Process details need the privileges to see the owning process (own processes only without sudo / root).
type ListeningPort struct {
	Proto   string `json:"proto"`   // tcp | udp
	Address string `json:"address"` // bind address: "0.0.0.0", "::", "127.0.0.1", "*"
	Port    int    `json:"port"`
	PID     int    `json:"pid,omitempty"`
	Process string `json:"process,omitempty"`
	User    string `json:"user,omitempty"`
}

func (s *Service) listPorts(ctx context.Context, t *target, sudo bool) ([]ListeningPort, error) {
	var ports []ListeningPort
	switch {
	case t.host.Platform == platLinux:
		if sudo {
			argv := []string{"netstat", "-ltnup"}
			if t.host.has("ss") {
				argv = []string{"ss", "-ltnup"}
			}
			res, err := s.execArgv(ctx, t, argv, true)
			if err != nil {
				return nil, err
			}
			if !res.ok() && len(res.Stdout) == 0 {
				return nil, actionError(res, "the socket list")
			}
			if argv[0] == "ss" {
				ports = parseSS(splitLines(res.Stdout))
			} else {
				ports = parseNetstatLinux(splitLines(res.Stdout))
			}
			break
		}
		res, err := s.run(ctx, t, command{sh: script(scriptLinuxPorts)})
		if err != nil {
			return nil, err
		}
		ports = parseLinuxPorts(res.Stdout)
	case t.host.Platform == platDarwin || isBSD(t.host.Platform):
		c := command{sh: script(scriptUnixPorts)}
		if sudo {
			pw, err := s.sudoAuth(ctx, t)
			if err != nil {
				return nil, err
			}
			line, doc := sudoLine([]string{"/bin/sh", "-c", script(scriptUnixPorts)}, pw)
			c = command{sh: "exec " + line + doc}
		}
		res, err := s.run(ctx, t, c)
		if err != nil {
			return nil, err
		}
		ports = parseUnixPorts(res.Stdout)
	case t.host.Platform == platWindows:
		if sudo {
			return nil, httpx.BadRequest("sudo is not available on Windows hosts")
		}
		res, err := s.run(ctx, t, command{ps: script(scriptWindowsPorts)})
		if err != nil {
			return nil, err
		}
		var perr error
		if ports, perr = parseWindowsPorts(res.Stdout); perr != nil {
			return nil, httpx.NewError(422, "command_failed", orDefault(res.errText(), perr.Error()))
		}
	default:
		return nil, httpx.NewError(422, "monitor_unavailable", "listing ports is not supported on "+t.host.Platform)
	}
	return normalizePorts(ports), nil
}

// normalizePorts de-duplicates (same proto/address/port/pid) and sorts by port.
func normalizePorts(in []ListeningPort) []ListeningPort {
	seen := map[string]int{}
	out := make([]ListeningPort, 0, len(in))
	for _, p := range in {
		if p.Port <= 0 || p.Port > 65535 {
			continue
		}
		key := fmt.Sprintf("%s|%s|%d|%d", p.Proto, p.Address, p.Port, p.PID)
		if _, ok := seen[key]; ok {
			continue
		}
		// An entry without process details is superseded by one that has them.
		bare := fmt.Sprintf("%s|%s|%d|0", p.Proto, p.Address, p.Port)
		if i, ok := seen[bare]; ok && p.PID > 0 {
			out[i] = p
			seen[key] = i
			continue
		}
		seen[key] = len(out)
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		if out[i].Proto != out[j].Proto {
			return out[i].Proto < out[j].Proto
		}
		return out[i].Address < out[j].Address
	})
	return out
}

// splitHostPort splits "addr:port" / "[v6]:port" / "addr.port" (BSD netstat, dot=true).
func splitHostPort(s string, dot bool) (string, int, bool) {
	sep := strings.LastIndexByte(s, ':')
	if dot {
		sep = strings.LastIndexByte(s, '.')
	}
	if sep < 0 {
		return "", 0, false
	}
	host, ps := s[:sep], s[sep+1:]
	port, err := strconv.Atoi(ps)
	if err != nil {
		return "", 0, false
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i] // interface scope ("127.0.0.53%lo", "fe80::1%lo0")
	}
	if strings.HasPrefix(host, "::ffff:") && net.ParseIP(host[7:]) != nil {
		host = host[7:]
	}
	if host == "" {
		host = "*"
	}
	return host, port, true
}

// parseLinuxPorts parses the Linux ports script (ss or netstat, /proc/net as the fallback). Process names come from
// /proc/<pid>/comm when available (BusyBox netstat prints a truncated argv instead).
func parseLinuxPorts(out []byte) []ListeningPort {
	sec := sections(splitLines(out))
	var ports []ListeningPort
	if ss := sec["SS"]; ss != nil {
		ports = parseSS(ss.lines)
	}
	if len(ports) == 0 && sec["NETSTAT"] != nil {
		ports = parseNetstatLinux(sec["NETSTAT"].lines)
	}
	if len(ports) == 0 {
		return parseProcNet(sec["PROC"])
	}
	if c := sec["COMM"]; c != nil {
		comm := map[int]string{}
		for _, l := range c.lines {
			if pid, name, ok := strings.Cut(strings.TrimSpace(l), " "); ok && isNum(pid) {
				comm[int(atoi64(pid))] = name
			}
		}
		for i := range ports {
			if name := comm[ports[i].PID]; ports[i].PID > 0 && name != "" {
				ports[i].Process = name
			}
		}
	}
	return ports
}

var ssUserRE = regexp.MustCompile(`\("([^"]*)",pid=(\d+)`)

// parseSS parses `ss -ltnup` (with or without its header).
func parseSS(lines []string) []ListeningPort {
	var out []ListeningPort
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 5 || f[0] == "Netid" || f[0] == "State" {
			continue
		}
		proto := f[0]
		if proto != "tcp" && proto != "udp" {
			continue
		}
		state := f[1]
		if proto == "tcp" && state != "LISTEN" {
			continue
		}
		if proto == "udp" && state != "UNCONN" {
			continue
		}
		host, port, ok := splitHostPort(f[4], false)
		if !ok {
			continue
		}
		p := ListeningPort{Proto: proto, Address: host, Port: port}
		if m := ssUserRE.FindStringSubmatch(l); m != nil {
			p.Process = m[1]
			p.PID, _ = strconv.Atoi(m[2])
		}
		out = append(out, p)
	}
	return out
}

// parseNetstatLinux parses net-tools / BusyBox `netstat -ltnup`.
func parseNetstatLinux(lines []string) []ListeningPort {
	var out []ListeningPort
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 5 {
			continue
		}
		proto := strings.TrimSuffix(f[0], "6")
		if proto != "tcp" && proto != "udp" {
			continue
		}
		last := ""
		switch {
		case proto == "tcp" && len(f) >= 6:
			if f[5] != "LISTEN" {
				continue
			}
			if len(f) >= 7 {
				last = f[6]
			}
		case proto == "udp" && len(f) >= 6:
			last = f[5]
		}
		host, port, ok := splitHostPort(f[3], false)
		if !ok {
			continue
		}
		p := ListeningPort{Proto: proto, Address: host, Port: port}
		if pid, prog, ok := strings.Cut(last, "/"); ok {
			p.PID, _ = strconv.Atoi(pid)
			p.Process = prog
		}
		out = append(out, p)
	}
	return out
}

// parseProcNet parses /proc/net/{tcp,tcp6,udp,udp6} (no process details).
func parseProcNet(sec *section) []ListeningPort {
	if sec == nil {
		return nil
	}
	var out []ListeningPort
	kind := ""
	for _, l := range sec.lines {
		if strings.HasPrefix(l, "##F ") {
			kind = strings.TrimSpace(l[4:])
			continue
		}
		f := strings.Fields(l)
		if len(f) < 4 || !strings.HasSuffix(f[0], ":") {
			continue
		}
		proto := strings.TrimSuffix(kind, "6")
		if (proto == "tcp" && f[3] != "0A") || (proto == "udp" && f[3] != "07") || proto == "" {
			continue
		}
		addr, port, ok := decodeProcAddr(f[1])
		if !ok {
			continue
		}
		out = append(out, ListeningPort{Proto: proto, Address: addr, Port: port})
	}
	return out
}

// decodeProcAddr decodes "0100007F:1F90" (IPv4) or a 32-hex-digit IPv6 address, both in host (little-endian) word order.
func decodeProcAddr(s string) (string, int, bool) {
	hexAddr, hexPort, ok := strings.Cut(s, ":")
	if !ok {
		return "", 0, false
	}
	port, err := strconv.ParseUint(hexPort, 16, 16)
	if err != nil {
		return "", 0, false
	}
	var ip net.IP
	switch len(hexAddr) {
	case 8:
		v, err := strconv.ParseUint(hexAddr, 16, 32)
		if err != nil {
			return "", 0, false
		}
		ip = net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	case 32:
		ip = make(net.IP, 16)
		for w := range 4 {
			v, err := strconv.ParseUint(hexAddr[w*8:w*8+8], 16, 32)
			if err != nil {
				return "", 0, false
			}
			ip[w*4], ip[w*4+1], ip[w*4+2], ip[w*4+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
		}
	default:
		return "", 0, false
	}
	addr := ip.String()
	if ip.To4() != nil && len(hexAddr) == 32 && strings.HasPrefix(addr, "::ffff:") {
		addr = strings.TrimPrefix(addr, "::ffff:")
	}
	return addr, int(port), true
}

// parseUnixPorts parses the macOS / BSD ports script: netstat -an for every socket, lsof / sockstat for owners.
func parseUnixPorts(out []byte) []ListeningPort {
	sec := sections(splitLines(out))
	var owned []ListeningPort
	if ss := sec["SOCKSTAT"]; ss != nil {
		owned = append(owned, parseSockstat(ss.lines)...)
	}
	for _, name := range []string{"LSOFTCP", "LSOFUDP"} {
		if ls := sec[name]; ls != nil {
			owned = append(owned, parseLsofF(ls.lines)...)
		}
	}
	var all []ListeningPort
	if ns := sec["NETSTAT"]; ns != nil {
		for _, l := range ns.lines {
			f := strings.Fields(l)
			if len(f) < 5 {
				continue
			}
			proto := strings.TrimRight(f[0], "46")
			if proto != "tcp" && proto != "udp" {
				continue
			}
			if proto == "tcp" && (len(f) < 6 || f[5] != "LISTEN") {
				continue
			}
			if proto == "udp" && f[4] != "*.*" {
				continue
			}
			host, port, ok := splitHostPort(f[3], true)
			if !ok {
				continue
			}
			all = append(all, ListeningPort{Proto: proto, Address: host, Port: port})
		}
	}
	// Attach owners to the complete netstat list; keep owned sockets netstat did not show.
	type key struct {
		proto string
		port  int
	}
	byKey := map[key][]ListeningPort{}
	for _, o := range owned {
		byKey[key{o.Proto, o.Port}] = append(byKey[key{o.Proto, o.Port}], o)
	}
	used := map[key]bool{}
	for i, p := range all {
		if own := byKey[key{p.Proto, p.Port}]; len(own) > 0 {
			all[i].PID, all[i].Process, all[i].User = own[0].PID, own[0].Process, own[0].User
			used[key{p.Proto, p.Port}] = true
		}
	}
	for k, own := range byKey {
		if !used[k] {
			all = append(all, own...)
		}
	}
	return all
}

// parseSockstat parses FreeBSD `sockstat -46l`.
func parseSockstat(lines []string) []ListeningPort {
	var out []ListeningPort
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 6 || f[0] == "USER" {
			continue
		}
		proto := strings.TrimRight(f[4], "46")
		if proto != "tcp" && proto != "udp" {
			continue
		}
		host, port, ok := splitHostPort(f[5], false)
		if !ok {
			continue
		}
		pid, _ := strconv.Atoi(f[2])
		out = append(out, ListeningPort{Proto: proto, Address: host, Port: port, PID: pid, Process: f[1], User: f[0]})
	}
	return out
}

// parseLsofF parses `lsof -F pcLPn` output (process sets p/c/L, then per-file P/n lines).
func parseLsofF(lines []string) []ListeningPort {
	var out []ListeningPort
	var pid int
	var cmd, user, proto string
	for _, l := range lines {
		if l == "" {
			continue
		}
		v := l[1:]
		switch l[0] {
		case 'p':
			pid, _ = strconv.Atoi(v)
			cmd, user, proto = "", "", ""
		case 'c':
			cmd = v
		case 'L':
			user = v
		case 'f':
			proto = ""
		case 'P':
			proto = strings.ToLower(v)
		case 'n':
			if strings.Contains(v, "->") || (proto != "tcp" && proto != "udp") {
				continue
			}
			host, port, ok := splitHostPort(v, false)
			if !ok {
				continue
			}
			out = append(out, ListeningPort{Proto: proto, Address: host, Port: port, PID: pid, Process: cmd, User: user})
		}
	}
	return out
}

type winPort struct {
	PR string `json:"pr"`
	A  string `json:"a"`
	P  int    `json:"p"`
	I  int    `json:"i"`
	C  string `json:"c"`
}

func parseWindowsPorts(out []byte) ([]ListeningPort, error) {
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return []ListeningPort{}, nil
	}
	if strings.HasPrefix(raw, "{") {
		raw = "[" + raw + "]"
	}
	var list []winPort
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, err
	}
	ports := make([]ListeningPort, 0, len(list))
	for _, w := range list {
		ports = append(ports, ListeningPort{Proto: w.PR, Address: w.A, Port: w.P, PID: w.I, Process: w.C})
	}
	return ports, nil
}
