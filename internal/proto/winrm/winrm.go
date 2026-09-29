// Package winrm implements the "winrm" terminal protocol (CC-1): an interactive PowerShell or cmd.exe session to a
// Windows host over WinRM (github.com/masterzen/winrm), with NTLM or Basic auth over HTTP (5985) or HTTPS (5986,
// optional certificate skip). NTLM over plain HTTP uses WinRM message encryption (what Windows requires by default,
// AllowUnencrypted=false) when the host is reached directly.
//
// WinRM is not a PTY: the remote interpreter reads whole lines from stdin. The backend therefore runs a small local
// line editor (echo, Backspace, Ctrl+U/W, Up/Down history, Ctrl+C to drop the line, Ctrl+D to exit, escape sequences
// swallowed) and sends each completed line. Connections are dialed through the generic sshx Dialer so proxies and SSH
// gateways apply.
package winrm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/masterzen/winrm"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/core"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/term"
)

// Mount registers the "winrm" terminal protocol.
func Mount(d *app.Deps, c *core.Core) error {
	term.RegisterProtocol(string(model.ProtoWinRM), func(ctx context.Context, req term.OpenRequest) (term.Backend, error) {
		return open(ctx, c, req)
	})
	return nil
}

// dialFunc reaches addr (host:port) for the WinRM HTTP client.
type dialFunc func(network, addr string) (net.Conn, error)

func open(ctx context.Context, c *core.Core, req term.OpenRequest) (term.Backend, error) {
	conn := req.Connection
	if conn == nil {
		return nil, term.Permanent(errors.New("winrm: missing connection"))
	}
	if strings.TrimSpace(conn.Host) == "" {
		return nil, term.Permanent(errors.New("winrm: host is required"))
	}
	if c == nil || c.SSH == nil {
		return nil, term.Permanent(errors.New("winrm: no dialer available"))
	}
	dl, err := c.SSH.Dialer(ctx, req.User, conn, req.Secrets)
	if err != nil {
		return nil, fmt.Errorf("winrm: build route: %w", err)
	}
	dial := func(network, addr string) (net.Conn, error) {
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return dl.DialContext(dctx, network, addr)
	}
	b, err := start(ctx, conn, req.Secrets[model.SecretPassword], dial, dl.Via() == nil && !routedByProxy(conn), func() { dl.Close() })
	if err != nil {
		dl.Close()
		return nil, err
	}
	return b, nil
}

func routedByProxy(conn *model.Connection) bool {
	o := conn.Options
	if len(o.Strings("jumpHosts")) > 0 || strings.TrimSpace(o.String("proxyCommand", "")) != "" {
		return true
	}
	pt := strings.ToLower(strings.TrimSpace(model.Options(o.Map("proxy")).String("type", "")))
	return pt != "" && pt != "none"
}

// start opens the remote shell and interpreter. direct reports that the host is reached without proxy or gateway
// (NTLM message encryption needs the library's own HTTP client, which cannot use the route's dialer); release is
// called once the remote shell is gone.
func start(ctx context.Context, conn *model.Connection, password string, dial dialFunc, direct bool, release func()) (*backend, error) {
	o := conn.Options
	https := o.Bool("https")
	port := conn.Port
	if port <= 0 {
		port = 5985
		if https {
			port = 5986
		}
	}
	authMode := strings.ToLower(strings.TrimSpace(o.String("winrmAuth", "ntlm")))
	if authMode != "ntlm" && authMode != "basic" {
		return nil, term.Permanent(fmt.Errorf("winrm: unsupported authentication %q (ntlm or basic)", authMode))
	}
	if conn.Username == "" {
		return nil, term.Permanent(errors.New("winrm: a user name is required"))
	}

	endpoint := winrm.NewEndpoint(conn.Host, port, https, o.Bool("insecureTls") || o.Bool("insecure"), nil, nil, nil, 70*time.Second)
	params := winrm.NewParameters("PT60S", "en-US", 153600)
	params.Dial = dial
	switch {
	case authMode == "ntlm" && !https && direct:
		params.TransportDecorator = func() winrm.Transporter {
			enc, err := winrm.NewEncryption("ntlm")
			if err != nil {
				return winrm.NewClientNTLMWithDial(dial)
			}
			return enc
		}
	case authMode == "ntlm":
		params.TransportDecorator = func() winrm.Transporter { return winrm.NewClientNTLMWithDial(dial) }
	default: // basic
		params.TransportDecorator = func() winrm.Transporter { return winrm.NewClientWithDial(dial) }
	}

	client, err := winrm.NewClientWithParameters(endpoint, conn.Username, password, params)
	if err != nil {
		return nil, term.Permanent(fmt.Errorf("winrm: %w", err))
	}
	type result struct {
		shell *winrm.Shell
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := client.CreateShell()
		ch <- result{s, err}
	}()
	var shell *winrm.Shell
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, classifyWinrmError(r.err)
		}
		shell = r.shell
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.shell != nil {
				_ = r.shell.Close()
			}
		}()
		return nil, ctx.Err()
	}

	interpreter := interpreterCommand(o.String("shell", ""))
	cmd, err := shell.ExecuteWithContext(context.Background(), interpreter)
	if err != nil {
		_ = shell.Close()
		return nil, term.Permanent(fmt.Errorf("winrm: start %s: %w", interpreter, err))
	}
	b := &backend{
		shell:     shell,
		cmd:       cmd,
		release:   release,
		out:       newOutbox(),
		editor:    lineEditor{echo: !o.Bool("noLocalEcho")},
		pumpsDone: make(chan struct{}),
	}
	b.code.Store(-1)
	var pumps sync.WaitGroup
	pumps.Add(2)
	go func() { defer pumps.Done(); b.pump(cmd.Stdout, false) }()
	go func() { defer pumps.Done(); b.pump(cmd.Stderr, true) }()
	go func() { pumps.Wait(); close(b.pumpsDone) }()
	go b.waitExit()
	return b, nil
}

// interpreterCommand maps the shell option to the interactive interpreter to run.
func interpreterCommand(shell string) string {
	switch strings.ToLower(strings.TrimSpace(shell)) {
	case "cmd", "cmd.exe":
		return "cmd.exe"
	default:
		return "powershell.exe -NoLogo -NoProfile"
	}
}

func classifyWinrmError(err error) error {
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(msg, "401") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "access is denied"):
		// Authentication problems will not fix themselves on retry.
		return term.Permanent(fmt.Errorf("winrm: authentication failed (check the user name, password and WinRM auth settings): %w", err))
	case strings.Contains(lower, "unencrypted") || strings.Contains(lower, "allowunencrypted"):
		return term.Permanent(fmt.Errorf("winrm: the server refuses unencrypted traffic: use HTTPS, or NTLM without a proxy/gateway: %w", err))
	case strings.Contains(lower, "x509") || strings.Contains(lower, "certificate"):
		return term.Permanent(fmt.Errorf("winrm: TLS certificate rejected (enable \"skip certificate verification\" for self-signed listeners): %w", err))
	}
	return fmt.Errorf("winrm: %w", err)
}

// ---- backend ------------------------------------------------------------------------------------------------------

type backend struct {
	shell   *winrm.Shell
	cmd     *winrm.Command
	release func()

	out *outbox

	editMu sync.Mutex // serializes Write (line editing + stdin requests)
	editor lineEditor

	closeOnce sync.Once
	pumpsDone chan struct{}
	code      atomic.Int64
}

// pump copies a remote output stream into the output buffer (stderr in red, bare LF as CR LF).
func (b *backend) pump(r io.Reader, isStderr bool) {
	buf := make([]byte, 16<<10)
	var nl newlineNormalizer
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := nl.normalize(buf[:n])
			if isStderr {
				data = append(append([]byte("\x1b[31m"), data...), "\x1b[0m"...)
			}
			b.out.push(data)
		}
		if err != nil {
			return
		}
	}
}

// Read returns remote output and local echo; io.EOF once the interpreter exited (after its last output) or the
// session closed the backend.
func (b *backend) Read(p []byte) (int, error) { return b.out.read(p) }

// Write runs the local line editor and sends completed lines to the interpreter's stdin.
func (b *backend) Write(p []byte) (int, error) {
	b.editMu.Lock()
	defer b.editMu.Unlock()
	echo, lines := b.editor.feed(p)
	b.out.push(echo)
	for _, line := range lines {
		if _, err := b.cmd.Stdin.Write(append([]byte(line), '\r', '\n')); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Resize is a no-op: WinRM has no PTY.
func (b *backend) Resize(int, int) error { return nil }

func (b *backend) ExitCode() int { return int(b.code.Load()) }

func (b *backend) waitExit() {
	b.cmd.Wait()
	b.code.Store(int64(b.cmd.ExitCode()))
	// Deliver the interpreter's last output before ending the stream.
	select {
	case <-b.pumpsDone:
	case <-time.After(2 * time.Second):
	}
	b.out.end()
	b.closeRemote()
}

func (b *backend) Close() error {
	b.out.close()
	b.closeRemote()
	return nil
}

// closeRemote terminates the command and deletes the remote shell (both are needed to free it on the server).
func (b *backend) closeRemote() {
	b.closeOnce.Do(func() {
		go func() {
			if b.cmd != nil {
				_ = b.cmd.Close()
			}
			if b.shell != nil {
				_ = b.shell.Close()
			}
			if b.release != nil {
				b.release()
			}
		}()
	})
}

// ---- output buffer -------------------------------------------------------------------------------------------------

// outbox is the output queue: the stdout/stderr pumps and the line editor's echo push into it, Read drains it.
type outbox struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	ended  bool // the interpreter exited: EOF once drained
	closed bool // the session closed the backend: EOF now
}

func newOutbox() *outbox {
	o := &outbox{}
	o.cond = sync.NewCond(&o.mu)
	return o
}

func (o *outbox) push(p []byte) {
	if len(p) == 0 {
		return
	}
	o.mu.Lock()
	if !o.closed {
		o.buf = append(o.buf, p...)
		o.cond.Broadcast()
	}
	o.mu.Unlock()
}

func (o *outbox) read(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for len(o.buf) == 0 && !o.ended && !o.closed {
		o.cond.Wait()
	}
	if o.closed || len(o.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, o.buf)
	o.buf = o.buf[n:]
	if len(o.buf) == 0 {
		o.buf = nil
	}
	return n, nil
}

func (o *outbox) end() {
	o.mu.Lock()
	o.ended = true
	o.cond.Broadcast()
	o.mu.Unlock()
}

func (o *outbox) close() {
	o.mu.Lock()
	o.closed = true
	o.buf = nil
	o.cond.Broadcast()
	o.mu.Unlock()
}

// newlineNormalizer converts lone LF to CR LF across chunk boundaries (WinRM output often uses bare \n).
type newlineNormalizer struct{ lastCR bool }

func (n *newlineNormalizer) normalize(p []byte) []byte {
	out := make([]byte, 0, len(p)+8)
	for _, c := range p {
		if c == '\n' && !n.lastCR {
			out = append(out, '\r')
		}
		out = append(out, c)
		n.lastCR = c == '\r'
	}
	return out
}

// ---- local line editor ---------------------------------------------------------------------------------------------

const maxHistory = 200

// lineEditor edits one input line locally (WinRM delivers whole lines). It is not safe for concurrent use.
type lineEditor struct {
	echo    bool
	line    []rune
	history []string
	histPos int    // index into history while browsing; len(history) = the line being typed
	saved   string // the line being typed, kept while browsing history
	esc     int    // escape-sequence state: 0 none, 1 ESC, 2 CSI, 3 SS3, 4 string (OSC/DCS)
	csi     []byte
	partial []byte // incomplete UTF-8 sequence
	lastCR  bool
}

// feed processes typed input and returns the bytes to echo and the completed lines.
func (e *lineEditor) feed(p []byte) (echo []byte, lines []string) {
	out := &echoBuf{on: e.echo}
	for _, c := range p {
		lines = e.feedByte(c, out, lines)
	}
	return out.b, lines
}

type echoBuf struct {
	on bool
	b  []byte
}

func (o *echoBuf) write(s string) {
	if o.on {
		o.b = append(o.b, s...)
	}
}

func (e *lineEditor) feedByte(c byte, out *echoBuf, lines []string) []string {
	switch e.esc {
	case 1:
		switch c {
		case '[':
			e.esc, e.csi = 2, e.csi[:0]
		case 'O':
			e.esc = 3
		case ']', 'P', '_', '^':
			e.esc = 4
		default:
			e.esc = 0 // Alt+key: ignored
		}
		return lines
	case 2:
		if c >= 0x40 && c <= 0x7e {
			e.esc = 0
			e.csiKey(c, out)
		} else if len(e.csi) < 16 {
			e.csi = append(e.csi, c)
		}
		return lines
	case 3:
		e.esc = 0
		e.csi = e.csi[:0]
		e.csiKey(c, out) // SS3 A/B (application cursor mode) = arrows
		return lines
	case 4:
		if c == 0x07 {
			e.esc = 0
		} else if c == 0x1b {
			e.esc = 1
		}
		return lines
	}
	if c == '\n' && e.lastCR {
		e.lastCR = false // CR LF from a paste: one line
		return lines
	}
	e.lastCR = c == '\r'
	switch c {
	case 0x1b:
		e.esc = 1
	case '\r', '\n':
		line := string(e.line)
		lines = append(lines, line)
		if strings.TrimSpace(line) != "" && (len(e.history) == 0 || e.history[len(e.history)-1] != line) {
			e.history = append(e.history, line)
			if len(e.history) > maxHistory {
				e.history = e.history[len(e.history)-maxHistory:]
			}
		}
		e.histPos, e.saved = len(e.history), ""
		e.line = e.line[:0]
		out.write("\r\n")
	case 0x7f, 0x08: // Backspace
		if n := len(e.line); n > 0 {
			out.write(erase(e.line[n-1:]))
			e.line = e.line[:n-1]
		}
	case 0x15: // Ctrl+U: kill the line
		out.write(erase(e.line))
		e.line = e.line[:0]
	case 0x17: // Ctrl+W: kill the previous word
		i := len(e.line)
		for i > 0 && e.line[i-1] == ' ' {
			i--
		}
		for i > 0 && e.line[i-1] != ' ' {
			i--
		}
		out.write(erase(e.line[i:]))
		e.line = e.line[:i]
	case 0x03: // Ctrl+C: drop the line (line mode cannot interrupt the remote interpreter)
		e.line = e.line[:0]
		e.histPos, e.saved = len(e.history), ""
		out.write("^C\r\n")
	case 0x04: // Ctrl+D on an empty line: leave the interpreter
		if len(e.line) == 0 {
			lines = append(lines, "exit")
			out.write("exit\r\n")
		}
	case 0x0c: // Ctrl+L: clear the screen, keep the line
		out.write("\x1b[H\x1b[2J" + string(e.line))
	case '\t':
		e.line = append(e.line, ' ')
		out.write(" ")
	default:
		if c < 0x20 {
			return lines // other control keys have no meaning in line mode
		}
		e.partial = append(e.partial, c)
		if !utf8.FullRune(e.partial) {
			if len(e.partial) >= utf8.UTFMax {
				e.partial = e.partial[:0]
			}
			return lines
		}
		r, _ := utf8.DecodeRune(e.partial)
		e.partial = e.partial[:0]
		if r == utf8.RuneError {
			return lines
		}
		if len(e.line) < 16<<10 {
			e.line = append(e.line, r)
			out.write(string(r))
		}
	}
	return lines
}

// csiKey handles the final byte of a cursor-key sequence: Up/Down browse the history, other keys are ignored.
func (e *lineEditor) csiKey(final byte, out *echoBuf) {
	switch final {
	case 'A': // Up
		if e.histPos == 0 || len(e.history) == 0 {
			return
		}
		if e.histPos == len(e.history) {
			e.saved = string(e.line)
		}
		e.histPos--
		e.replace(e.history[e.histPos], out)
	case 'B': // Down
		if e.histPos >= len(e.history) {
			return
		}
		e.histPos++
		if e.histPos == len(e.history) {
			e.replace(e.saved, out)
		} else {
			e.replace(e.history[e.histPos], out)
		}
	}
}

func (e *lineEditor) replace(s string, out *echoBuf) {
	out.write(erase(e.line))
	e.line = []rune(s)
	out.write(s)
}

// erase returns the echo that removes runes from the end of the displayed line (wide characters take two cells).
func erase(rs []rune) string {
	var sb strings.Builder
	for _, r := range rs {
		w := 1
		if isWide(r) {
			w = 2
		}
		for i := 0; i < w; i++ {
			sb.WriteString("\b \b")
		}
	}
	return sb.String()
}

func isWide(r rune) bool {
	return (r >= 0x1100 && r <= 0x115f) || (r >= 0x2e80 && r <= 0xa4cf) || (r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) || (r >= 0xfe30 && r <= 0xfe4f) || (r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) || (r >= 0x1f300 && r <= 0x1faff) || (r >= 0x20000 && r <= 0x3fffd)
}

var _ term.Backend = (*backend)(nil)
