// Package mosh implements the "mosh" terminal protocol (PROTO-13, RESEARCH §3.8): it bootstraps mosh-server over an
// SSH exec through the shared SSH pool (so every SSH option, jump host and prompt applies), parses the
// "MOSH CONNECT <port> <key>" line, then talks to the server over UDP with the pure-Go mosh client
// (github.com/unixshells/mosh-go). options.moshClient="system" uses an installed mosh-client in a local PTY instead
// (it supports options.predict, local echo prediction, which the built-in client does not).
//
// The library's own Client is not used: its transport never lets the server discard acknowledged states (after ~1024
// keystrokes a reference mosh-server accepts one keystroke per 15 s) and renders diffs regardless of their base, so
// ssp.go implements the client side of the protocol on the library's wire primitives. The backend also resolves the
// UDP target itself (the library only accepts IPv4 literals), fails clearly when the server never answers (UDP
// blocked), ends the session when the remote shell exits (the server's shutdown state), and sends the protocol's
// shutdown when the session is closed so mosh-server and its shell do not linger on the remote host.
//
// The UDP leg is a direct socket of the NexTerm host, so it is vetted by the session owner's destination guard
// (internal/netguard, SEC-7): the concrete IP and the port announced by mosh-server are checked before the built-in
// client dials (whose socket is also vetted in Control) or mosh-client is spawned. With jump hosts / proxies the host
// name is resolved on the NexTerm host, so a literal / localhost destination is refused before mosh-server is started.
package mosh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/xpty"
	mosh "github.com/unixshells/mosh-go"

	"github.com/nexterm/nexterm/internal/app"
	"github.com/nexterm/nexterm/internal/core"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/netguard"
	"github.com/nexterm/nexterm/internal/term"
)

// Mount registers the "mosh" terminal protocol. The built-in client needs no local mosh installation, so the
// "mosh" feature flag (GET /api/auth/state) is always on.
func Mount(d *app.Deps, c *core.Core) error {
	app.RegisterFeature("mosh", func(context.Context) bool { return true })
	term.RegisterProtocol(string(model.ProtoMosh), func(ctx context.Context, req term.OpenRequest) (term.Backend, error) {
		return open(ctx, d, c, req)
	})
	return nil
}

// firstContactTimeout bounds the wait for the server's first datagram (UDP blocked → fail instead of hanging).
var firstContactTimeout = 15 * time.Second

func open(ctx context.Context, d *app.Deps, c *core.Core, req term.OpenRequest) (term.Backend, error) {
	conn := req.Connection
	if conn == nil {
		return nil, term.Permanent(errors.New("mosh: missing connection"))
	}
	if strings.TrimSpace(conn.Host) == "" {
		return nil, term.Permanent(errors.New("mosh: host is required"))
	}
	// The destination guard of the session owner (nil = unrestricted: desktop mode, administrators). The SSH leg is
	// vetted by the pool; the UDP leg here.
	g := netguard.ForUser(d, req.User)
	if routed(conn) {
		// The UDP leg resolves the host name on the NexTerm host (not on the jump host / proxy): refuse an obviously
		// blocked destination before starting mosh-server remotely (DNS names are checked once resolved).
		if err := g.CheckLiteral(conn.Host, 0); err != nil {
			return nil, term.Permanent(err)
		}
	}
	if c == nil || c.SSH == nil {
		return nil, term.Permanent(errors.New("mosh: SSH is not available"))
	}
	o := conn.Options
	useSystem := strings.EqualFold(strings.TrimSpace(o.String("moshClient", "")), "system")
	var systemClient string
	if useSystem {
		p, err := exec.LookPath("mosh-client")
		if err != nil {
			return nil, term.Permanent(errors.New("mosh: mosh-client is not installed on the NexTerm host (choose the built-in client)"))
		}
		systemClient = p
	}
	if req.Session != nil {
		req.Session.SetStatus(model.StateConnecting, "Starting mosh-server over SSH")
	}
	cl, release, err := c.SSH.Acquire(ctx, req.User, conn, req.Secrets)
	if err != nil {
		return nil, fmt.Errorf("mosh: SSH connection: %w", err)
	}
	// The UDP session is independent of SSH once bootstrapped.
	defer release()
	return startSession(ctx, g, req, cl, useSystem, systemClient)
}

// bootstrapper is what the UDP leg needs from the bootstrap SSH connection (*sshx.Client).
type bootstrapper interface {
	execer
	RemoteAddr() net.Addr
}

// startSession starts mosh-server over cl and connects the UDP leg, vetted by g (nil = unrestricted).
func startSession(ctx context.Context, g *netguard.Guard, req term.OpenRequest, cl bootstrapper, useSystem bool, systemClient string) (term.Backend, error) {
	conn := req.Connection
	o := conn.Options
	port, key, err := bootstrap(ctx, cl, o)
	if err != nil {
		return nil, err
	}
	var sshAddr net.Addr
	if !routed(conn) {
		sshAddr = cl.RemoteAddr()
	}
	ip, err := udpTarget(ctx, conn.Host, sshAddr)
	if err != nil {
		return nil, term.Permanent(err)
	}
	// The concrete UDP destination: the resolved IP and the port announced by mosh-server (SEC-7). Also covers
	// mosh-client, which is handed the IP literal below.
	if err := g.CheckIP(ip, port); err != nil {
		return nil, term.Permanent(err)
	}
	target := &net.UDPAddr{IP: ip, Port: port}

	cols, rows := 80, 24
	if req.Session != nil {
		cols, rows = req.Session.Size()
		req.Session.SetStatus(model.StateConnecting, "Connecting to mosh-server on UDP "+target.String())
	}
	if useSystem {
		return startSystemClient(systemClient, ip.String(), port, key, o.String("predict", "adaptive"), cols, rows)
	}
	b, err := dialBuiltin(ctx, g, target, key, cols, rows)
	if err != nil {
		return nil, err // never a typed nil: the session would Close() it
	}
	return b, nil
}

// ---- bootstrap ----------------------------------------------------------------------------------------------------

// execer runs a command over SSH (sshx.Client.Exec).
type execer interface {
	Exec(ctx context.Context, cmd string) (stdout, stderr []byte, exitCode int, err error)
}

var errLocale = errors.New("mosh-server rejected the locale")

// bootstrap starts mosh-server over SSH and returns its UDP port and session key. A server without the en_US.UTF-8
// locale is retried with C.UTF-8.
func bootstrap(ctx context.Context, ex execer, o model.Options) (int, string, error) {
	var lastErr error
	for _, locale := range []string{"LANG=en_US.UTF-8", "LC_ALL=C.UTF-8"} {
		cmd, err := buildServerCommand(o, locale)
		if err != nil {
			return 0, "", term.Permanent(err)
		}
		stdout, stderr, code, err := ex.Exec(ctx, cmd)
		if err != nil {
			return 0, "", fmt.Errorf("mosh: run mosh-server: %w", err)
		}
		port, key, perr := parseMoshConnect(string(stdout))
		if perr == nil {
			return port, key, nil
		}
		msg := strings.TrimSpace(string(stderr) + "\n" + string(stdout))
		switch {
		case strings.Contains(msg, "UTF-8 native locale"):
			lastErr = errLocale
			continue
		case code == 127 || strings.Contains(msg, "not found"):
			return 0, "", term.Permanent(errors.New("mosh: mosh-server is not installed on the remote host (install the mosh package there, or set its path)"))
		case msg != "":
			return 0, "", term.Permanent(fmt.Errorf("mosh: mosh-server did not start: %s", firstLine(msg)))
		}
		return 0, "", term.Permanent(fmt.Errorf("mosh: %w", perr))
	}
	return 0, "", term.Permanent(fmt.Errorf("mosh: %w: the remote host needs a UTF-8 locale (en_US.UTF-8 or C.UTF-8)", lastErr))
}

var (
	serverPathRe = regexp.MustCompile(`^[A-Za-z0-9_./+~-]{1,256}$`)
	envNameRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

// buildServerCommand builds the remote mosh-server invocation. Fixed parts are validated (moshPorts = PORT or
// LOW:HIGH for -p, moshServer = path); free text (remoteCommand, env values) is single-quoted for the remote shell.
// options.env reaches the session through `env` (mosh-server passes its environment on to the shell, and no AcceptEnv
// is needed); options.remoteCommand runs through /bin/sh -c instead of the login shell.
func buildServerCommand(o model.Options, locale string) (string, error) {
	server := strings.TrimSpace(o.String("moshServer", ""))
	if server == "" {
		server = "mosh-server"
	}
	if !serverPathRe.MatchString(server) {
		return "", fmt.Errorf("mosh: invalid mosh-server path %q", server)
	}
	var args []string
	if env := o.StringMap("env"); len(env) > 0 {
		keys := make([]string, 0, len(env))
		for k := range env {
			if !envNameRe.MatchString(k) {
				return "", fmt.Errorf("mosh: invalid environment variable name %q", k)
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		args = append(args, "env")
		for _, k := range keys {
			args = append(args, shellQuote(k+"="+env[k]))
		}
	}
	args = append(args, server, "new", "-s", "-c", "256", "-l", locale)
	if ports := strings.TrimSpace(o.String("moshPorts", "")); ports != "" {
		if !validPortRange(ports) {
			return "", fmt.Errorf("mosh: invalid UDP port range %q (use PORT or FIRST:LAST)", ports)
		}
		args = append(args, "-p", ports)
	}
	if cmd := strings.TrimSpace(o.String("remoteCommand", "")); cmd != "" {
		if strings.ContainsRune(cmd, 0) || len(cmd) > 8192 {
			return "", errors.New("mosh: invalid remote command")
		}
		args = append(args, "--", "/bin/sh", "-c", shellQuote(cmd))
	}
	return strings.Join(args, " "), nil
}

// shellQuote quotes s as a single word for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var portRangeRe = regexp.MustCompile(`^(\d{1,5})(?::(\d{1,5}))?$`)

func validPortRange(s string) bool {
	m := portRangeRe.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	lo, _ := strconv.Atoi(m[1])
	if lo < 1 || lo > 65535 {
		return false
	}
	if m[2] != "" {
		hi, _ := strconv.Atoi(m[2])
		if hi < lo || hi > 65535 {
			return false
		}
	}
	return true
}

var moshConnectRe = regexp.MustCompile(`(?m)^\s*MOSH CONNECT\s+(\d{1,5})\s+([A-Za-z0-9+/]{22}(?:==)?)\s*$`)

// parseMoshConnect extracts the UDP port and base64 session key from mosh-server's "MOSH CONNECT" line.
func parseMoshConnect(output string) (port int, key string, err error) {
	m := moshConnectRe.FindStringSubmatch(output)
	if m == nil {
		return 0, "", errors.New("mosh-server did not print a MOSH CONNECT line")
	}
	port, err = strconv.Atoi(m[1])
	if err != nil || port < 1 || port > 65535 {
		return 0, "", fmt.Errorf("invalid mosh port %q", m[1])
	}
	return port, m[2], nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func routed(conn *model.Connection) bool {
	o := conn.Options
	if len(o.Strings("jumpHosts")) > 0 || strings.TrimSpace(o.String("proxyCommand", "")) != "" {
		return true
	}
	pt := strings.ToLower(strings.TrimSpace(model.Options(o.Map("proxy")).String("type", "")))
	return pt != "" && pt != "none"
}

// udpTarget picks the IP to send mosh datagrams to: the address the SSH connection actually reached (like the mosh
// CLI) when SSH was direct, otherwise the host name resolved here (the UDP leg cannot use SSH gateways or proxies).
func udpTarget(ctx context.Context, host string, sshAddr net.Addr) (net.IP, error) {
	if ta, ok := sshAddr.(*net.TCPAddr); ok && ta.IP != nil && !ta.IP.IsUnspecified() {
		return ta.IP, nil
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip, nil
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(rctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("mosh: cannot resolve %s for the UDP connection (mosh needs direct UDP access to the server): %v", host, err)
	}
	for _, ip := range ips {
		if ip.To4() != nil {
			return ip, nil
		}
	}
	return ips[0], nil
}

// ---- built-in (pure Go) client --------------------------------------------------------------------------------------

// goBackend is a mosh session over the built-in protocol client (ssp.go).
type goBackend struct {
	client *sspClient
}

// dialBuiltin connects the built-in client to target; g (nil = unrestricted) vets the socket's address in Control.
func dialBuiltin(ctx context.Context, g *netguard.Guard, target *net.UDPAddr, key string, cols, rows int) (*goBackend, error) {
	ocb, err := newOCB(key)
	if err != nil {
		return nil, term.Permanent(fmt.Errorf("mosh: %w", err))
	}
	uc, err := g.DialContext(ctx, "udp", target.String(), 15*time.Second)
	if err != nil {
		if be, ok := netguard.IsBlocked(err); ok {
			return nil, term.Permanent(be)
		}
		return nil, fmt.Errorf("mosh: UDP socket to %s: %w", target, err)
	}
	b := &goBackend{client: newSSPClient(uc, ocb, cols, rows)}

	// Wait for the server's first datagram so a blocked UDP path fails clearly instead of a silent, frozen session.
	start := time.Now()
	for b.client.LastContact().IsZero() {
		select {
		case <-ctx.Done():
			b.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		if time.Since(start) > firstContactTimeout {
			b.Close()
			return nil, fmt.Errorf("mosh: no reply from mosh-server at %s over UDP within %s (UDP must be reachable from the NexTerm host — firewall, NAT or port range?)", target, firstContactTimeout)
		}
	}
	return b, nil
}

func newOCB(key string) (*mosh.OCB, error) {
	for len(key)%4 != 0 {
		key += "="
	}
	return mosh.NewOCBFromBase64(key)
}

// Read returns terminal output; io.EOF when the remote shell exited (server shutdown) or the backend was closed.
func (b *goBackend) Read(p []byte) (int, error) { return b.client.Read(p) }

func (b *goBackend) Write(p []byte) (int, error) {
	if err := b.client.Send(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (b *goBackend) Resize(cols, rows int) error {
	b.client.Resize(cols, rows)
	return nil
}

// Close tells the server to shut down (or acknowledges its shutdown) and stops the client. Without it a C
// mosh-server keeps the remote shell alive indefinitely, waiting for the client to roam back.
func (b *goBackend) Close() error {
	b.client.Close()
	return nil
}

// ---- system mosh-client -------------------------------------------------------------------------------------------

type ptyBackend struct {
	pty       xpty.Pty
	cmd       *exec.Cmd
	closeOnce sync.Once
	ptyOnce   sync.Once
	done      chan struct{}
}

func startSystemClient(bin, host string, port int, key, predict string, cols, rows int) (term.Backend, error) {
	switch predict = strings.ToLower(strings.TrimSpace(predict)); predict {
	case "adaptive", "always", "never", "experimental":
	default:
		predict = "adaptive"
	}
	p, err := xpty.NewPty(cols, rows)
	if err != nil {
		return nil, fmt.Errorf("mosh: create pseudo-terminal: %w", err)
	}
	cmd := exec.Command(bin, host, strconv.Itoa(port))
	cmd.Env = []string{"MOSH_KEY=" + key, "MOSH_PREDICTION_DISPLAY=" + predict, "TERM=xterm-256color", "LANG=en_US.UTF-8"}
	if err := p.Start(cmd); err != nil {
		p.Close()
		return nil, fmt.Errorf("mosh: start mosh-client: %w", err)
	}
	b := &ptyBackend{pty: p, cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = xpty.WaitProcess(context.Background(), cmd)
		close(b.done)
		time.Sleep(200 * time.Millisecond)
		b.ptyOnce.Do(func() { _ = b.pty.Close() })
	}()
	return b, nil
}

func (b *ptyBackend) Read(p []byte) (int, error) {
	n, err := b.pty.Read(p)
	if err == nil {
		return n, nil
	}
	select {
	case <-b.done:
	case <-time.After(2 * time.Second):
	}
	return n, io.EOF
}

func (b *ptyBackend) Write(p []byte) (int, error) { return b.pty.Write(p) }
func (b *ptyBackend) Resize(cols, rows int) error { return b.pty.Resize(cols, rows) }

// Close hangs up mosh-client (SIGHUP makes it send the shutdown to the server), killing it if it does not exit.
func (b *ptyBackend) Close() error {
	b.closeOnce.Do(func() {
		hangup(b.cmd)
		go func() {
			select {
			case <-b.done:
			case <-time.After(3 * time.Second):
				if b.cmd.Process != nil {
					_ = b.cmd.Process.Kill()
				}
			}
			b.ptyOnce.Do(func() { _ = b.pty.Close() })
		}()
	})
	return nil
}

var (
	_ term.Backend = (*goBackend)(nil)
	_ term.Backend = (*ptyBackend)(nil)
)
