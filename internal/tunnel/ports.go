package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Remote listening-port detection (TUN-9): one exec runs a POSIX shell probe that picks the best available tool
// (ss → netstat → /proc/net/tcp → lsof → BSD netstat) and prefixes its output with a marker naming the format;
// Windows servers (no POSIX shell) are queried with `netstat -ano` + `tasklist`.

// portsProbe is the probe script. It is one line without single quotes, run as `sh -c '<script>'`: sshd hands exec
// commands to the user's login shell, which may be fish, csh or tcsh (where the POSIX syntax — and, for csh, a
// quoted string spanning several lines — would fail), so the script always runs under sh.
var portsProbe = strings.Join([]string{
	"LC_ALL=C",
	"export LC_ALL",
	"if command -v ss >/dev/null 2>&1; then echo @@ss; ss -ltnp 2>/dev/null || ss -ltn; exit 0; fi",
	"if [ -r /proc/net/tcp ]; then if command -v netstat >/dev/null 2>&1 && netstat -ltnp >/dev/null 2>&1; then echo @@netstat; netstat -ltnp 2>/dev/null; exit 0; fi; echo @@proc; cat /proc/net/tcp /proc/net/tcp6 2>/dev/null; exit 0; fi",
	"if command -v lsof >/dev/null 2>&1; then echo @@lsof; lsof -nP -iTCP -sTCP:LISTEN 2>/dev/null; exit 0; fi",
	"if command -v netstat >/dev/null 2>&1; then echo @@bsd; netstat -an -p tcp 2>/dev/null; exit 0; fi",
	"echo @@none",
}, "; ")

// portsCommand is the exec command of the probe.
var portsCommand = "sh -c " + shellQuote(portsProbe)

const maxPorts = 2000

// detectPorts lists the TCP ports listening on the host behind ex.
func detectPorts(ctx context.Context, ex execer) (RemotePorts, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, _, _, err := ex.Exec(ctx, portsCommand)
	if err != nil && len(out) == 0 {
		return RemotePorts{}, fmt.Errorf("cannot run the port probe: %w", err)
	}
	method, body := splitMarker(out)
	if method == "" {
		// Not a POSIX shell: try Windows.
		wout, _, _, werr := ex.Exec(ctx, "netstat -ano")
		if werr != nil || !bytes.Contains(bytes.ToUpper(wout), []byte("LISTEN")) {
			return RemotePorts{}, errors.New("could not determine the listening ports (no ss, netstat, lsof or /proc on the server)")
		}
		ports := parseWindowsNetstat(wout)
		if tl, _, _, err := ex.Exec(ctx, "tasklist /FO CSV /NH"); err == nil {
			names := parseTasklist(tl)
			for i := range ports {
				ports[i].Process = names[ports[i].PID]
			}
		}
		return RemotePorts{Method: "windows", Ports: finishPorts(ports)}, nil
	}
	var ports []RemotePort
	note := ""
	switch method {
	case "ss":
		ports = parseSS(body)
	case "netstat":
		ports = parseNetstat(body)
	case "proc":
		ports = parseProcNet(body)
		note = "Process names are not available (no ss or netstat on the server)."
	case "lsof":
		ports = parseLsof(body)
	case "bsd":
		ports = parseBSDNetstat(body)
		note = "Process names are not available."
	default:
		return RemotePorts{}, errors.New("could not determine the listening ports (no ss, netstat, lsof or /proc on the server)")
	}
	ports = finishPorts(ports)
	if note == "" && len(ports) > 0 {
		missing := 0
		for _, p := range ports {
			if p.Process == "" {
				missing++
			}
		}
		if missing > 0 {
			note = "Processes owned by other users are not shown without root privileges."
		}
	}
	return RemotePorts{Method: method, Ports: ports, Note: note}, nil
}

// warnGatewayPorts is the Status.Warning of a remote forward that sshd bound to loopback although another address
// was requested.
const warnGatewayPorts = "Only programs on the SSH server can connect: sshd bound the port to loopback (GatewayPorts no). " +
	"Set “GatewayPorts clientspecified” in its sshd_config to accept other machines."

// bindWarning checks where the SSH server really bound a remote forward that asked for a non-loopback address
// (bound = the requested "host:port", auto port resolved): sshd silently binds loopback only when GatewayPorts is
// "no", the OpenSSH default. It runs the read-only listening-port probe and returns warnGatewayPorts or "". Probe
// failures (no exec channel, no tool, link lost) count as no warning.
func bindWarning(ctx context.Context, l link, bound string) string {
	ex, ok := l.(execer)
	if !ok {
		return ""
	}
	_, ps, err := net.SplitHostPort(bound)
	port, _ := strconv.Atoi(ps)
	if err != nil || port <= 0 {
		return ""
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-l.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	res, err := detectPorts(ctx, ex)
	if err != nil {
		return ""
	}
	return gatewayWarning(res.Ports, port)
}

// gatewayWarning returns warnGatewayPorts when port is listening on loopback addresses only ("" when it is also
// reachable on other addresses, or not listed at all).
func gatewayWarning(ports []RemotePort, port int) string {
	found := false
	for _, p := range ports {
		if p.Port != port {
			continue
		}
		if p.Scope != "loopback" {
			return ""
		}
		found = true
	}
	if !found {
		return ""
	}
	return warnGatewayPorts
}

// splitMarker extracts the "@@method" line printed by the probe script.
func splitMarker(out []byte) (method string, body []byte) {
	i := bytes.Index(out, []byte("@@"))
	if i < 0 {
		return "", nil
	}
	rest := out[i+2:]
	nl := bytes.IndexByte(rest, '\n')
	if nl < 0 {
		return strings.TrimSpace(string(rest)), nil
	}
	return strings.TrimSpace(string(rest[:nl])), rest[nl+1:]
}

// splitAddrPort splits "host:port", "[v6]:port", "*:port", "host.port" (BSD, when dotted is set).
func splitAddrPort(s string, dotted bool) (string, int, bool) {
	s = strings.TrimSpace(s)
	sep := strings.LastIndexByte(s, ':')
	if dotted {
		sep = strings.LastIndexByte(s, '.')
	}
	if sep <= 0 || sep == len(s)-1 {
		return "", 0, false
	}
	port, err := strconv.Atoi(s[sep+1:])
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, false
	}
	host := strings.Trim(s[:sep], "[]")
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i] // interface scope ("127.0.0.53%lo", "fe80::1%eth0")
	}
	return host, port, true
}

// classify fills Scope and ConnectHost from Address.
func classify(p *RemotePort) {
	a := p.Address
	switch a {
	case "", "*", "0.0.0.0", "::", "::0", "[::]":
		p.Scope, p.ConnectHost = "all", "127.0.0.1"
		if a == "::" || a == "::0" || a == "[::]" {
			p.ConnectHost = "localhost"
		}
		return
	}
	if ip, err := netip.ParseAddr(a); err == nil {
		ip = ip.Unmap()
		switch {
		case ip.IsLoopback():
			p.Scope, p.ConnectHost = "loopback", ip.String()
		case ip.IsUnspecified():
			p.Scope, p.ConnectHost = "all", "127.0.0.1"
		default:
			p.Scope, p.ConnectHost = "address", ip.String()
		}
		p.Address = ip.String()
		return
	}
	p.Scope, p.ConnectHost = "address", a
}

// finishPorts classifies, de-duplicates (the same port on 0.0.0.0 and :: is one entry) and sorts ports.
func finishPorts(in []RemotePort) []RemotePort {
	type key struct {
		port        int
		scope, host string
	}
	seen := map[key]int{}
	out := []RemotePort{}
	for _, p := range in {
		classify(&p)
		k := key{p.Port, p.Scope, p.ConnectHost}
		if p.Scope != "address" {
			k.host = ""
		}
		if i, ok := seen[k]; ok {
			if out[i].Process == "" && p.Process != "" {
				out[i].Process, out[i].PID = p.Process, p.PID
			}
			if p.Scope == "loopback" && strings.Contains(out[i].ConnectHost, ":") && !strings.Contains(p.ConnectHost, ":") {
				out[i].ConnectHost, out[i].Address = p.ConnectHost, p.Address // prefer 127.0.0.1 over ::1
			}
			if p.Scope == "all" && out[i].ConnectHost == "localhost" && p.ConnectHost == "127.0.0.1" {
				out[i].ConnectHost, out[i].Address = p.ConnectHost, p.Address
			}
			continue
		}
		seen[k] = len(out)
		out = append(out, p)
		if len(out) >= maxPorts {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Address < out[j].Address
	})
	return out
}

var ssUsersRE = regexp.MustCompile(`\(\("([^"]*)",pid=(\d+)`)

// parseSS parses `ss -ltnp` (with or without the header line).
func parseSS(b []byte) []RemotePort {
	var out []RemotePort
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || strings.EqualFold(f[0], "State") || strings.EqualFold(f[0], "Netid") {
			continue
		}
		i := 3 // State Recv-Q Send-Q Local Peer [Process]
		if f[0] == "tcp" || f[0] == "tcp6" {
			i = 4 // Netid State Recv-Q Send-Q Local …
		}
		if i >= len(f) {
			continue
		}
		host, port, ok := splitAddrPort(f[i], false)
		if !ok {
			continue
		}
		p := RemotePort{Address: host, Port: port}
		if m := ssUsersRE.FindStringSubmatch(sc.Text()); m != nil {
			p.Process = m[1]
			p.PID, _ = strconv.Atoi(m[2])
		}
		out = append(out, p)
	}
	return out
}

// parseNetstat parses Linux `netstat -ltnp` (net-tools or busybox).
func parseNetstat(b []byte) []RemotePort {
	var out []RemotePort
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 6 || !strings.HasPrefix(f[0], "tcp") || f[5] != "LISTEN" {
			continue
		}
		host, port, ok := splitAddrPort(f[3], false)
		if !ok {
			continue
		}
		p := RemotePort{Address: host, Port: port}
		if len(f) >= 7 && f[6] != "-" {
			if pid, name, ok := strings.Cut(f[6], "/"); ok {
				p.PID, _ = strconv.Atoi(pid)
				p.Process = strings.Join(append([]string{name}, f[7:]...), " ")
			}
		}
		out = append(out, p)
	}
	return out
}

// parseProcNet parses /proc/net/tcp and /proc/net/tcp6 (state 0A = LISTEN).
func parseProcNet(b []byte) []RemotePort {
	var out []RemotePort
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || f[3] != "0A" {
			continue
		}
		hexAddr, hexPort, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		port, err := strconv.ParseUint(hexPort, 16, 16)
		if err != nil || port == 0 {
			continue
		}
		raw, err := hex.DecodeString(hexAddr)
		if err != nil || (len(raw) != 4 && len(raw) != 16) {
			continue
		}
		// The kernel prints each 32-bit word in host (little-endian) order.
		for i := 0; i+4 <= len(raw); i += 4 {
			raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
		}
		ip, ok := netip.AddrFromSlice(raw)
		if !ok {
			continue
		}
		out = append(out, RemotePort{Address: ip.Unmap().String(), Port: int(port)})
	}
	return out
}

// parseLsof parses `lsof -nP -iTCP -sTCP:LISTEN`.
func parseLsof(b []byte) []RemotePort {
	var out []RemotePort
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		f := strings.Fields(line)
		if len(f) < 9 || f[0] == "COMMAND" || !strings.Contains(line, "(LISTEN)") {
			continue
		}
		name := f[len(f)-2]
		host, port, ok := splitAddrPort(name, false)
		if !ok {
			continue
		}
		p := RemotePort{Address: host, Port: port, Process: strings.ReplaceAll(f[0], `\x20`, " ")}
		p.PID, _ = strconv.Atoi(f[1])
		out = append(out, p)
	}
	return out
}

// parseBSDNetstat parses `netstat -an -p tcp` of macOS / BSD ("127.0.0.1.631", "*.22").
func parseBSDNetstat(b []byte) []RemotePort {
	var out []RemotePort
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 6 || !strings.HasPrefix(f[0], "tcp") || f[len(f)-1] != "LISTEN" {
			continue
		}
		host, port, ok := splitAddrPort(f[3], true)
		if !ok {
			continue
		}
		out = append(out, RemotePort{Address: host, Port: port})
	}
	return out
}

// parseWindowsNetstat parses the TCP rows of Windows `netstat -ano`.
func parseWindowsNetstat(b []byte) []RemotePort {
	var out []RemotePort
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || !strings.EqualFold(f[0], "TCP") || !strings.EqualFold(f[3], "LISTENING") {
			continue
		}
		host, port, ok := splitAddrPort(f[1], false)
		if !ok {
			continue
		}
		p := RemotePort{Address: host, Port: port}
		p.PID, _ = strconv.Atoi(f[4])
		out = append(out, p)
	}
	return out
}

// parseTasklist maps PIDs to image names (`tasklist /FO CSV /NH`).
func parseTasklist(b []byte) map[int]string {
	out := map[int]string{}
	r := csv.NewReader(bytes.NewReader(b))
	r.FieldsPerRecord = -1
	for {
		rec, err := r.Read()
		if err != nil {
			break
		}
		if len(rec) >= 2 {
			if pid, err := strconv.Atoi(strings.TrimSpace(rec[1])); err == nil {
				out[pid] = rec[0]
			}
		}
	}
	return out
}

// ---- endpoint & watcher -------------------------------------------------------------------------------------------

// remotePorts implements GET /api/tunnels/remote-ports for a session (its SSH client) or a saved connection.
func (m *Manager) remotePorts(ctx context.Context, user *model.User, sessionID, connID string) (RemotePorts, error) {
	var (
		l   link
		rel func()
		err error
	)
	switch {
	case sessionID != "":
		if m.forSession == nil {
			return RemotePorts{}, httpx.Conflict("SSH is not available")
		}
		l, rel, err = m.forSession(ctx, user, sessionID)
	case connID != "":
		if m.acquire == nil {
			return RemotePorts{}, httpx.Conflict("SSH is not available")
		}
		l, rel, err = m.acquire(ctx, user, connID)
	default:
		return RemotePorts{}, httpx.BadRequest("sessionId or connectionId is required")
	}
	if err != nil {
		return RemotePorts{}, err
	}
	defer rel()
	ex, ok := l.(execer)
	if !ok {
		return RemotePorts{}, httpx.Conflict("this connection cannot run commands")
	}
	res, err := detectPorts(ctx, ex)
	if err != nil {
		return RemotePorts{}, httpx.Conflict(err.Error())
	}
	return res, nil
}

// watchInterval is the polling period of the "tunnel.ports" topic.
const watchInterval = 5 * time.Second

// portsTopic implements the "tunnel.ports" events topic ({sessionId}): the listening ports of a live SSH session
// are polled and differences pushed to the subscribed windows, so the UI can offer "port 8080 opened — Forward".
// Every window watching a session shares one poller (one exec per interval, however many windows are open).
func (m *Manager) portsTopic(ctx context.Context, user *model.User, clientID string, params json.RawMessage) (func(), error) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || !model.ValidID(p.SessionID) {
		return nil, httpx.BadRequest("sessionId is required")
	}
	if m.sessions == nil || m.forSession == nil {
		return nil, httpx.Conflict("SSH is not available")
	}
	s := m.sessions.Get(p.SessionID)
	if s == nil || s.OwnerID != user.ID {
		return nil, httpx.ErrNotFound
	}
	if s.Protocol != model.ProtoSSH {
		return nil, httpx.BadRequest("not an SSH session")
	}
	return m.watchPorts(user, clientID, p.SessionID), nil
}

// portWatch polls the listening ports of one SSH session for the windows subscribed to it.
type portWatch struct {
	m         *Manager
	user      *model.User // the session owner (the only user who may subscribe)
	sessionID string
	cancel    context.CancelFunc

	mu      sync.Mutex
	subs    map[string]bool       // client id → got its initial list
	ports   map[string]RemotePort // last list by portKey (nil before the first successful poll)
	list    []RemotePort
	lastErr string
}

// watchPorts subscribes a window to a session's port watcher (starting it when needed) and returns the unsubscribe
// func. A window joining a running watcher gets the last list right away.
func (m *Manager) watchPorts(user *model.User, clientID, sessionID string) func() {
	m.pwMu.Lock()
	w := m.watches[sessionID]
	if w == nil {
		ctx, cancel := context.WithCancel(m.ctx)
		w = &portWatch{m: m, user: user, sessionID: sessionID, cancel: cancel, subs: map[string]bool{}}
		m.watches[sessionID] = w
		go w.run(ctx)
	}
	w.mu.Lock()
	w.subs[clientID] = false
	var initial *PortsEvent
	if w.list != nil {
		w.subs[clientID] = true
		initial = &PortsEvent{Type: evPorts, SessionID: sessionID, Initial: true, Ports: w.list}
	}
	w.mu.Unlock()
	m.pwMu.Unlock()
	if initial != nil {
		m.d.Events.PublishClient(clientID, *initial)
	}
	return func() { m.unwatchPorts(w, clientID) }
}

// unwatchPorts removes a window from a watcher and stops the watcher after its last window.
func (m *Manager) unwatchPorts(w *portWatch, clientID string) {
	m.pwMu.Lock()
	defer m.pwMu.Unlock()
	w.mu.Lock()
	delete(w.subs, clientID)
	empty := len(w.subs) == 0
	w.mu.Unlock()
	if empty {
		m.dropWatchLocked(w)
	}
}

func (m *Manager) dropWatchLocked(w *portWatch) {
	if m.watches[w.sessionID] == w {
		delete(m.watches, w.sessionID)
	}
	w.cancel()
}

func (w *portWatch) run(ctx context.Context) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		t.Reset(watchInterval)
		s := w.m.sessions.Get(w.sessionID)
		if s == nil {
			w.m.pwMu.Lock()
			w.m.dropWatchLocked(w) // session closed
			w.m.pwMu.Unlock()
			return
		}
		if st, _ := s.State(); st != model.StateConnected {
			continue
		}
		res, err := w.m.remotePorts(ctx, w.user, w.sessionID, "")
		if ctx.Err() != nil {
			return
		}
		w.deliver(res, err)
	}
}

// deliver pushes a poll result: the full list (initial) to windows that have none yet, the differences to the others;
// an error once per distinct message.
func (w *portWatch) deliver(res RemotePorts, err error) {
	type msg struct {
		client string
		ev     PortsEvent
	}
	var out []msg
	w.mu.Lock()
	if err != nil {
		if e := err.Error(); e != w.lastErr {
			w.lastErr = e
			for c := range w.subs {
				out = append(out, msg{c, PortsEvent{Type: evPorts, SessionID: w.sessionID, Ports: []RemotePort{}, Error: e}})
			}
		}
	} else {
		w.lastErr = ""
		cur := make(map[string]RemotePort, len(res.Ports))
		for _, p := range res.Ports {
			cur[portKey(p)] = p
		}
		var added, removed []RemotePort
		if w.ports != nil {
			for k, p := range cur {
				if _, ok := w.ports[k]; !ok {
					added = append(added, p)
				}
			}
			for k, p := range w.ports {
				if _, ok := cur[k]; !ok {
					removed = append(removed, p)
				}
			}
		}
		changed := len(added) > 0 || len(removed) > 0
		w.ports, w.list = cur, res.Ports
		for c, got := range w.subs {
			switch {
			case !got:
				w.subs[c] = true
				out = append(out, msg{c, PortsEvent{Type: evPorts, SessionID: w.sessionID, Initial: true, Ports: res.Ports}})
			case changed:
				out = append(out, msg{c, PortsEvent{Type: evPorts, SessionID: w.sessionID, Ports: res.Ports, Added: added, Removed: removed}})
			}
		}
	}
	w.mu.Unlock()
	for _, o := range out {
		w.m.d.Events.PublishClient(o.client, o.ev)
	}
}

func portKey(p RemotePort) string { return p.Scope + "|" + p.ConnectHost + "|" + strconv.Itoa(p.Port) }
