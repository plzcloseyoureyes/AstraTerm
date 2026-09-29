package tunnel

import (
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/netguard"
)

// kind is the runtime flavour of a forward.
type kind int

const (
	kindLocal         kind = iota // -L: listen on the AstraTerm host, connect from the SSH server
	kindRemote                    // -R: listen on the SSH server, connect from the AstraTerm host
	kindDynamic                   // -D: SOCKS/HTTP proxy on the AstraTerm host, exits through the SSH server
	kindRemoteDynamic             // -R SOCKS (TUN-5): proxy on the SSH server, exits through the AstraTerm host
)

// listensLocally reports whether the listener lives on the AstraTerm host.
func (k kind) listensLocally() bool { return k == kindLocal || k == kindDynamic }

// isProxy reports whether the forward is a SOCKS/HTTP proxy.
func (k kind) isProxy() bool { return k == kindDynamic || k == kindRemoteDynamic }

func (k kind) String() string {
	switch k {
	case kindLocal:
		return "local"
	case kindRemote:
		return "remote"
	case kindDynamic:
		return "dynamic"
	case kindRemoteDynamic:
		return "remote dynamic"
	}
	return "unknown"
}

// Limits.
const (
	defaultIdleTimeout = 5 * time.Minute
	minIdleTimeout     = 10 * time.Second
	maxIdleTimeout     = 24 * time.Hour
	defaultMaxConns    = 1024
	maxMaxConns        = 65535
	maxAllowFrom       = 64
	maxSocketPath      = 104 // sun_path is 104 bytes on macOS/BSD, 108 on Linux/Windows
	maxHostLen         = 255
	maxNameLen         = 200
	maxNotesLen        = 4096
	maxSecretLen       = 1024
)

// spec is a validated, normalized forwarding definition ready to run.
type spec struct {
	kind       kind
	bindHost   string // "" with bindSocket
	bindPort   int
	bindSocket string
	destHost   string // "" for proxies and with destSocket
	destPort   int
	destSocket string

	socksUser     string
	socksPass     string
	httpProxy     bool
	autoReconnect bool
	onDemand      bool
	idle          time.Duration
	maxConns      int
	allowFrom     []netip.Prefix
	// selfPort is AstraTerm's own listen port: reverse SOCKS clients (on the SSH server) may not reach it.
	selfPort int
	// guard returns the owner's destination guard (SEC-7, internal/netguard) for connections made from the AstraTerm
	// host (remote and remote-dynamic forwards), evaluated per connection so policy changes apply at once; nil or a
	// nil result = unrestricted.
	guard func() *netguard.Guard
}

// def is the user-editable part of a forwarding definition shared by saved tunnels and session forwards.
type def struct {
	Type       string
	BindHost   string
	BindPort   int
	DestHost   string
	DestPort   int
	Reverse    bool
	BindSocket string
	DestSocket string
}

func (d def) kind() kind {
	switch d.Type {
	case model.TunnelRemote:
		return kindRemote
	case model.TunnelDynamic:
		if d.Reverse {
			return kindRemoteDynamic
		}
		return kindDynamic
	}
	return kindLocal
}

// normalize validates and canonicalizes a definition in place (defaults: bind 127.0.0.1, destination localhost).
func (d *def) normalize() error {
	d.Type = strings.ToLower(strings.TrimSpace(d.Type))
	switch d.Type {
	case model.TunnelLocal, model.TunnelRemote, model.TunnelDynamic:
	case "":
		return httpx.BadRequest("type is required (local, remote or dynamic)")
	default:
		return httpx.BadRequest("type must be local, remote or dynamic")
	}
	if d.Type != model.TunnelDynamic {
		d.Reverse = false
	}
	k := d.kind()
	d.BindHost = strings.TrimSpace(d.BindHost)
	d.DestHost = strings.TrimSpace(d.DestHost)
	d.BindSocket = strings.TrimSpace(d.BindSocket)
	d.DestSocket = strings.TrimSpace(d.DestSocket)

	// Listener.
	if d.BindSocket != "" {
		if err := checkSocketPath("bind socket", d.BindSocket, k.listensLocally()); err != nil {
			return err
		}
		d.BindHost, d.BindPort = "", 0
	} else {
		if d.BindPort < 0 || d.BindPort > 65535 {
			return httpx.BadRequest("bind port must be between 0 and 65535")
		}
		h, err := normalizeBindHost(d.BindHost, !k.listensLocally())
		if err != nil {
			return err
		}
		d.BindHost = h
	}

	// Destination.
	switch {
	case k.isProxy():
		d.DestHost, d.DestPort, d.DestSocket = "", 0, ""
	case d.DestSocket != "":
		// A local tunnel's destination is on the SSH server; a remote tunnel's on the AstraTerm host.
		if err := checkSocketPath("destination socket", d.DestSocket, k == kindRemote); err != nil {
			return err
		}
		d.DestHost, d.DestPort = "", 0
	default:
		if d.DestHost == "" {
			d.DestHost = "localhost"
		}
		h, err := normalizeHost(d.DestHost)
		if err != nil {
			return httpx.BadRequest("destination host: " + err.Error())
		}
		d.DestHost = h
		if d.DestPort < 1 || d.DestPort > 65535 {
			return httpx.BadRequest("destination port must be between 1 and 65535")
		}
	}
	return nil
}

// normalizeBindHost canonicalizes a listen address. "" means the default (127.0.0.1); "*" means every interface.
// Hostnames other than localhost are only accepted for remote listeners (the SSH server resolves them).
func normalizeBindHost(h string, remote bool) (string, error) {
	h = strings.Trim(strings.TrimSpace(h), "[]")
	switch strings.ToLower(h) {
	case "":
		return "127.0.0.1", nil
	case "*":
		return "*", nil
	case "localhost":
		return "localhost", nil
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return a.String(), nil
	}
	if !remote {
		return "", httpx.BadRequest("bind address must be an IP address of this machine, localhost or * (all interfaces)")
	}
	n, err := normalizeHost(h)
	if err != nil {
		return "", httpx.BadRequest("bind address: " + err.Error())
	}
	return n, nil
}

// normalizeHost validates a host name or IP literal (brackets around IPv6 literals are removed).
func normalizeHost(h string) (string, error) {
	h = strings.TrimSpace(h)
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	if h == "" {
		return "", fmt.Errorf("host is required")
	}
	if len(h) > maxHostLen {
		return "", fmt.Errorf("host is too long")
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return a.String(), nil
	}
	for _, r := range h {
		ok := r == '-' || r == '.' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !ok {
			return "", fmt.Errorf("invalid host name %q", h)
		}
	}
	if strings.HasPrefix(h, "-") || strings.HasPrefix(h, ".") || strings.Contains(h, "..") {
		return "", fmt.Errorf("invalid host name %q", h)
	}
	return h, nil
}

// checkSocketPath validates a Unix socket path. local paths live on the AstraTerm host (OS-specific absolute path);
// other paths live on the (POSIX) SSH server.
func checkSocketPath(field, p string, local bool) error {
	if len(p) > maxSocketPath {
		return httpx.BadRequest(fmt.Sprintf("%s path must be at most %d bytes", field, maxSocketPath))
	}
	if strings.ContainsFunc(p, func(r rune) bool { return r < ' ' || r == 0x7f }) || !utf8.ValidString(p) {
		return httpx.BadRequest(field + " path contains invalid characters")
	}
	if local {
		if !filepath.IsAbs(p) {
			return httpx.BadRequest(field + " must be an absolute path on the AstraTerm host")
		}
		if filepath.Clean(p) != p {
			return httpx.BadRequest(field + " path must be clean (no '..', '.' or duplicate separators)")
		}
		return nil
	}
	if !strings.HasPrefix(p, "/") {
		return httpx.BadRequest(field + " must be an absolute path on the SSH server (starting with /)")
	}
	return nil
}

// isLoopbackHost reports whether a bind host only accepts local clients.
func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(strings.Trim(h, "[]"))
	return err == nil && a.IsLoopback()
}

// exposedLocal reports whether the forward listens on the AstraTerm host on an address other machines can reach.
func (s *spec) exposedLocal() bool {
	return s.kind.listensLocally() && s.bindSocket == "" && !isLoopbackHost(s.bindHost)
}

// exposedRemote reports whether the forward asks the SSH server for a TCP listener reachable from other machines,
// which sshd only honours with GatewayPorts yes / clientspecified.
func (s *spec) exposedRemote() bool {
	return !s.kind.listensLocally() && s.bindSocket == "" && !isLoopbackHost(s.bindHost)
}

// localListenAddr returns the net.Listen arguments of a listener on the AstraTerm host.
func (s *spec) localListenAddr() (network, addr string) {
	if s.bindSocket != "" {
		return "unix", s.bindSocket
	}
	host := s.bindHost
	if host == "*" {
		host = ""
	}
	return "tcp", net.JoinHostPort(host, strconv.Itoa(s.bindPort))
}

// remoteListenAddr returns the ssh Listen arguments of a listener on the SSH server ("" = all interfaces, RFC 4254
// §7.1).
func (s *spec) remoteListenAddr() (network, addr string) {
	if s.bindSocket != "" {
		return "unix", s.bindSocket
	}
	host := s.bindHost
	if host == "*" {
		host = ""
	}
	return "tcp", net.JoinHostPort(host, strconv.Itoa(s.bindPort))
}

// destAddr returns the dial arguments of the forward's fixed destination.
func (s *spec) destAddr() (network, addr string) {
	if s.destSocket != "" {
		return "unix", s.destSocket
	}
	return "tcp", net.JoinHostPort(s.destHost, strconv.Itoa(s.destPort))
}

// bindLabel renders the listen side ("127.0.0.1:8080", "*:1080", "/tmp/x.sock").
func (s *spec) bindLabel() string {
	if s.bindSocket != "" {
		return s.bindSocket
	}
	port := "auto"
	if s.bindPort > 0 {
		port = strconv.Itoa(s.bindPort)
	}
	return hostPortLabel(s.bindHost, port)
}

// destLabel renders the destination ("web:80", "/var/run/docker.sock", "SOCKS").
func (s *spec) destLabel() string {
	if s.kind.isProxy() {
		return "SOCKS/HTTP proxy"
	}
	if s.destSocket != "" {
		return s.destSocket
	}
	return hostPortLabel(s.destHost, strconv.Itoa(s.destPort))
}

func hostPortLabel(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// label describes the forward for notices and logs.
func (s *spec) label() string { return s.labelWith(s.bindLabel()) }

// labelWith describes the forward with the given listen address (e.g. the bound one, auto ports resolved).
func (s *spec) labelWith(bind string) string {
	switch s.kind {
	case kindLocal:
		return bind + " → " + s.destLabel()
	case kindRemote:
		return "remote " + bind + " → " + s.destLabel()
	case kindDynamic:
		return "SOCKS proxy on " + bind
	default:
		return "remote SOCKS proxy on " + bind
	}
}

// parseAllowFrom parses an allow list of IPs / CIDRs.
func parseAllowFrom(list []string) ([]netip.Prefix, []string, error) {
	if len(list) > maxAllowFrom {
		return nil, nil, httpx.BadRequest(fmt.Sprintf("at most %d allowed client entries", maxAllowFrom))
	}
	var out []netip.Prefix
	var clean []string
	for _, e := range list {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if p, err := netip.ParsePrefix(e); err == nil {
			out = append(out, p.Masked())
			clean = append(clean, p.Masked().String())
			continue
		}
		a, err := netip.ParseAddr(e)
		if err != nil {
			return nil, nil, httpx.BadRequest(fmt.Sprintf("allowed client %q is not an IP address or CIDR", e))
		}
		a = a.WithZone("")
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
		clean = append(clean, a.String())
	}
	return out, clean, nil
}

// normalizeOptions validates the tunnel options for a definition kind and fills defaults.
func normalizeOptions(o *Options, k kind) error {
	t := true
	o.SocksUsername = strings.TrimSpace(o.SocksUsername)
	if len(o.SocksUsername) > 255 || strings.ContainsFunc(o.SocksUsername, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return httpx.BadRequest("SOCKS username is invalid")
	}
	if !k.isProxy() {
		o.SocksUsername, o.HTTPProxy = "", nil
	} else if o.HTTPProxy == nil {
		o.HTTPProxy = &t
	}
	if o.AutoReconnect == nil {
		o.AutoReconnect = &t
	}
	if !k.listensLocally() {
		o.OnDemand = false
		o.AllowFrom = nil
	}
	if o.IdleTimeoutSec < 0 || o.IdleTimeoutSec > int(maxIdleTimeout/time.Second) {
		return httpx.BadRequest("idle timeout must be between 10 seconds and 24 hours")
	}
	if o.IdleTimeoutSec != 0 && o.IdleTimeoutSec < int(minIdleTimeout/time.Second) {
		o.IdleTimeoutSec = int(minIdleTimeout / time.Second)
	}
	if o.MaxConns < 0 || o.MaxConns > maxMaxConns {
		return httpx.BadRequest(fmt.Sprintf("max connections must be between 0 and %d", maxMaxConns))
	}
	_, clean, err := parseAllowFrom(o.AllowFrom)
	if err != nil {
		return err
	}
	o.AllowFrom = clean
	o.Scheme = strings.ToLower(strings.TrimSpace(o.Scheme))
	switch o.Scheme {
	case "", "http", "https":
	default:
		return httpx.BadRequest("scheme must be http or https")
	}
	if len(o.Color) > 64 || len(o.Icon) > 64 {
		return httpx.BadRequest("color and icon must be at most 64 characters")
	}
	if utf8.RuneCountInString(o.Notes) > maxNotesLen {
		return httpx.BadRequest(fmt.Sprintf("notes must be at most %d characters", maxNotesLen))
	}
	o.BindSocket = strings.TrimSpace(o.BindSocket)
	o.DestSocket = strings.TrimSpace(o.DestSocket)
	return nil
}

// buildSpec assembles the runtime spec of a normalized definition + options + decrypted secrets.
func buildSpec(d def, o Options, secrets map[string]string) (*spec, error) {
	k := d.kind()
	allow, _, err := parseAllowFrom(o.AllowFrom)
	if err != nil {
		return nil, err
	}
	s := &spec{
		kind:          k,
		bindHost:      d.BindHost,
		bindPort:      d.BindPort,
		bindSocket:    d.BindSocket,
		destHost:      d.DestHost,
		destPort:      d.DestPort,
		destSocket:    d.DestSocket,
		httpProxy:     o.HTTPProxy == nil || *o.HTTPProxy,
		autoReconnect: o.AutoReconnect == nil || *o.AutoReconnect,
		onDemand:      o.OnDemand && k.listensLocally(),
		idle:          defaultIdleTimeout,
		maxConns:      defaultMaxConns,
		allowFrom:     allow,
	}
	if o.IdleTimeoutSec > 0 {
		s.idle = min(max(time.Duration(o.IdleTimeoutSec)*time.Second, minIdleTimeout), maxIdleTimeout)
	}
	if o.MaxConns > 0 {
		s.maxConns = o.MaxConns
	}
	if k.isProxy() && o.SocksUsername != "" {
		s.socksUser = o.SocksUsername
		s.socksPass = secrets[secretSocksPassword]
		if s.socksPass == "" {
			return nil, httpx.BadRequest("the SOCKS password is missing (set it or clear the SOCKS username)")
		}
	}
	if k.isProxy() && s.socksUser == "" && !restrictive(s.allowFrom) && s.bindSocket == "" && !isLoopbackHost(s.bindHost) {
		return nil, httpx.BadRequest("a proxy listening on a non-loopback address needs a SOCKS username/password or an allowed-clients list (one that does not admit every address)")
	}
	return s, nil
}

// restrictive reports whether an allowed-clients list limits who may connect: it is not empty and no entry admits
// every address (0.0.0.0/0, ::/0) — such a list would turn an unauthenticated proxy into an open proxy.
func restrictive(allow []netip.Prefix) bool {
	if len(allow) == 0 {
		return false
	}
	for _, p := range allow {
		if p.Bits() == 0 {
			return false
		}
	}
	return true
}

// secretPolicy validates a write-only secrets patch.
func checkSecrets(m map[string]string) error {
	for k, v := range m {
		if k != secretSocksPassword {
			return httpx.BadRequest(fmt.Sprintf("unknown secret %q", k))
		}
		if len(v) > maxSecretLen {
			return httpx.BadRequest("secret is too long")
		}
	}
	return nil
}
