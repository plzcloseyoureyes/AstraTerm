package sshx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"github.com/plzcloseyoureyes/astraterm/internal/netguard"
)

// proxySpec is options.proxy (password from secrets.proxyPassword).
type proxySpec struct {
	Type     string `json:"type"` // socks5 | socks4 | socks4a | http
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"-"`
}

func (s proxySpec) addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

// dialProxy connects to addr through a SOCKS4/4a/5 or HTTP CONNECT proxy (SSH-30/31). Host names are resolved by the
// proxy (remote DNS) except for plain SOCKS4, which only carries IPv4 addresses. g vets the connection to the proxy
// server (made from the AstraTerm host); the proxy's own connection to addr is its business.
func dialProxy(ctx context.Context, spec proxySpec, network, addr string, timeout time.Duration, g *netguard.Guard) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("%s proxies only carry TCP", spec.Type)
	}
	switch spec.Type {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if spec.Username != "" {
			auth = &proxy.Auth{User: spec.Username, Password: spec.Password}
		}
		d, err := proxy.SOCKS5("tcp", spec.addr(), auth, g.Dialer(timeout))
		if err != nil {
			return nil, err
		}
		cd, ok := d.(proxy.ContextDialer)
		if !ok {
			return nil, errors.New("socks5 dialer does not support contexts")
		}
		dctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		c, err := cd.DialContext(dctx, "tcp", addr)
		if err != nil {
			if be, ok := netguard.IsBlocked(err); ok {
				return nil, be
			}
			return nil, fmt.Errorf("socks5 proxy %s: %w", spec.addr(), err)
		}
		return c, nil
	case "socks4", "socks4a":
		return dialSOCKS4(ctx, spec, addr, timeout, g)
	case "http":
		return dialHTTPConnect(ctx, spec, addr, timeout, g)
	}
	return nil, fmt.Errorf("unsupported proxy type %q", spec.Type)
}

func dialSOCKS4(ctx context.Context, spec proxySpec, addr string, timeout time.Duration, g *netguard.Guard) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("invalid port %q", portStr)
	}
	ip := net.ParseIP(host).To4()
	if ip == nil && net.ParseIP(host) != nil {
		return nil, errors.New("SOCKS4 proxies do not support IPv6 destinations")
	}
	if ip == nil && spec.Type == "socks4" {
		// Plain SOCKS4: resolve locally.
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("resolve %s: %w", host, err)
		}
		ip = ips[0].To4()
	}
	c, err := dialDirect(ctx, "tcp", spec.addr(), timeout, g)
	if err != nil {
		if be, ok := netguard.IsBlocked(err); ok {
			return nil, be
		}
		return nil, fmt.Errorf("socks4 proxy %s: %w", spec.addr(), err)
	}
	_ = c.SetDeadline(time.Now().Add(timeout))
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	req := []byte{4, 1, byte(port >> 8), byte(port)}
	if ip != nil {
		req = append(req, ip...)
	} else {
		req = append(req, 0, 0, 0, 1) // SOCKS4a: let the proxy resolve the name
	}
	req = append(req, spec.Username...)
	req = append(req, 0)
	if ip == nil {
		req = append(req, host...)
		req = append(req, 0)
	}
	if _, err := c.Write(req); err != nil {
		c.Close()
		return nil, fmt.Errorf("socks4 proxy: %w", err)
	}
	var resp [8]byte
	if _, err := io.ReadFull(c, resp[:]); err != nil {
		c.Close()
		return nil, fmt.Errorf("socks4 proxy: %w", err)
	}
	if resp[1] != 0x5a {
		c.Close()
		switch resp[1] {
		case 0x5b:
			return nil, errors.New("socks4 proxy rejected the connection")
		case 0x5c, 0x5d:
			return nil, errors.New("socks4 proxy refused the request (identd check failed)")
		}
		return nil, fmt.Errorf("socks4 proxy failed (code 0x%02x)", resp[1])
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

func dialHTTPConnect(ctx context.Context, spec proxySpec, addr string, timeout time.Duration, g *netguard.Guard) (net.Conn, error) {
	c, err := dialDirect(ctx, "tcp", spec.addr(), timeout, g)
	if err != nil {
		if be, ok := netguard.IsBlocked(err); ok {
			return nil, be
		}
		return nil, fmt.Errorf("http proxy %s: %w", spec.addr(), err)
	}
	_ = c.SetDeadline(time.Now().Add(timeout))
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	var b strings.Builder
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: AstraTerm\r\n", addr, addr)
	if spec.Username != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(spec.Username + ":" + spec.Password))
		fmt.Fprintf(&b, "Proxy-Authorization: Basic %s\r\n", cred)
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(c, b.String()); err != nil {
		c.Close()
		return nil, fmt.Errorf("http proxy: %w", err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("http proxy: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.Close()
		if resp.StatusCode == http.StatusProxyAuthRequired {
			return nil, errors.New("http proxy requires authentication (407)")
		}
		return nil, fmt.Errorf("http proxy refused CONNECT: %s", resp.Status)
	}
	_ = c.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		// The reader may already hold the start of the tunneled stream (e.g. the SSH banner).
		return &bufferedConn{Conn: c, r: br}, nil
	}
	return c, nil
}

// bufferedConn serves bytes buffered while parsing the proxy response before reading from the connection.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// ---- ProxyCommand (SSH-33) ----------------------------------------------------------------------------------------

// dialCommand runs a local ProxyCommand whose stdin/stdout become the transport. Tokens: %h host, %p port, %r
// remote user, %n original host, %% a literal percent sign.
func dialCommand(ctx context.Context, template, host, port, user string) (net.Conn, error) {
	command := expandTokens(template, host, port, user)
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", command)
	} else {
		cmd = exec.Command("/bin/sh", "-c", command)
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	stderr := &tailBuffer{max: 4096}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, stderr
	if err := cmd.Start(); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, fmt.Errorf("ProxyCommand: %w", err)
	}
	inR.Close()
	outW.Close()
	c := &cmdConn{cmd: cmd, w: inW, r: outR, stderr: stderr, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(c.done)
	}()
	if ctx.Err() != nil {
		c.Close()
		return nil, ctx.Err()
	}
	return c, nil
}

func expandTokens(t, host, port, user string) string {
	var b strings.Builder
	for i := 0; i < len(t); i++ {
		if t[i] != '%' || i+1 >= len(t) {
			b.WriteByte(t[i])
			continue
		}
		i++
		switch t[i] {
		case 'h', 'n':
			b.WriteString(shellQuote(host))
		case 'p':
			b.WriteString(shellQuote(port))
		case 'r':
			b.WriteString(shellQuote(user))
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(t[i])
		}
	}
	return b.String()
}

// shellQuote makes a token safe to splice into a shell command line (values come from the connection).
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._:@[]", r))
	}) < 0 {
		return s
	}
	if runtime.GOOS == "windows" {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cmdConn adapts a ProxyCommand process to net.Conn.
type cmdConn struct {
	cmd    *exec.Cmd
	w      *os.File
	r      *os.File
	stderr *tailBuffer
	done   chan struct{}
	once   sync.Once
}

func (c *cmdConn) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err == io.EOF {
		if msg := strings.TrimSpace(c.stderr.String()); msg != "" {
			return n, fmt.Errorf("ProxyCommand exited: %s", msg)
		}
	}
	return n, err
}

func (c *cmdConn) Write(p []byte) (int, error) { return c.w.Write(p) }

func (c *cmdConn) Close() error {
	c.once.Do(func() {
		c.w.Close()
		c.r.Close()
		select {
		case <-c.done:
		case <-time.After(2 * time.Second):
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
		}
	})
	return nil
}

func (c *cmdConn) LocalAddr() net.Addr  { return pipeAddr("proxycommand") }
func (c *cmdConn) RemoteAddr() net.Addr { return pipeAddr("proxycommand") }

func (c *cmdConn) SetDeadline(t time.Time) error {
	_ = c.r.SetReadDeadline(t)
	_ = c.w.SetWriteDeadline(t)
	return nil
}
func (c *cmdConn) SetReadDeadline(t time.Time) error  { _ = c.r.SetReadDeadline(t); return nil }
func (c *cmdConn) SetWriteDeadline(t time.Time) error { _ = c.w.SetWriteDeadline(t); return nil }

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if over := t.buf.Len() - t.max; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}
