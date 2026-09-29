package servers

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// telnetService is the Telnet server (SRV-6): password login, then a shell on a PTY (as the NexTerm OS user).
type telnetService struct {
	m   *Manager
	in  *instance
	cfg *TelnetConfig
	db  *userDB
	ln  net.Listener
	wg  sync.WaitGroup
}

const (
	telnetLoginTimeout = 60 * time.Second
	telnetMaxAttempts  = 3
	telnetMaxLine      = 256
)

// Telnet protocol bytes (RFC 854) and options.
const (
	tnIAC  = 255
	tnDONT = 254
	tnDO   = 253
	tnWONT = 252
	tnWILL = 251
	tnSB   = 250
	tnGA   = 249
	tnEL   = 248
	tnEC   = 247
	tnAYT  = 246
	tnAO   = 245
	tnIP   = 244
	tnBRK  = 243
	tnDM   = 242
	tnNOP  = 241
	tnSE   = 240

	optBinary = 0
	optEcho   = 1
	optSGA    = 3
	optTType  = 24
	optNAWS   = 31
)

func newTelnetService(m *Manager, in *instance, cfg *TelnetConfig) (service, error) {
	if len(cfg.Users) == 0 {
		return nil, invalidf("add at least one user")
	}
	return &telnetService{m: m, in: in, cfg: cfg, db: newUserDB(cfg.Users, &in.stats)}, nil
}

func (s *telnetService) start() error {
	raw, err := net.Listen("tcp", hostPort(s.cfg.BindAddress, s.cfg.Port))
	if err != nil {
		return err
	}
	s.in.addrs = []string{raw.Addr().String()}
	s.in.url = serverURL("telnet", s.cfg.BindAddress, raw.Addr().(*net.TCPAddr).Port, "")
	s.ln = &trackingListener{Listener: raw, set: s.in.clients}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := s.ln.Accept()
			if err != nil {
				if s.in.ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
					s.in.failed(err)
				}
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.serve(conn)
			}()
		}
	}()
	return nil
}

func (s *telnetService) stop() {
	if s.ln != nil {
		_ = s.ln.Close()
	}
	s.in.clients.closeAll()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
	}
}

func (s *telnetService) serve(conn net.Conn) {
	defer conn.Close()
	addr := conn.RemoteAddr().String()
	cl := clientOf(conn)
	tc := newTelnetConn(conn)
	if s.db.lim.blocked(remoteIP(addr)) {
		_ = tc.writeString("Too many failed logins. Try again later.\r\n")
		s.in.logf(levelWarn, addr, "", "Connection refused: too many failed logins")
		return
	}
	s.in.logf(levelInfo, addr, "", "Connected")
	defer s.in.logf(levelInfo, addr, "", "Disconnected")
	tc.negotiate()

	user, ok := s.login(tc, conn, addr)
	if !ok {
		return
	}
	if cl != nil {
		cl.setUser(user)
		cl.setActivity("Shell")
	}
	cols, rows := tc.size()
	sh, err := startPTYShell(shellSpec{command: s.cfg.ShellCommand, dir: s.cfg.WorkingDir, term: tc.termType(),
		cols: cols, rows: rows})
	if err != nil {
		_ = tc.writeString("\r\nCannot start the shell: " + err.Error() + "\r\n")
		s.in.logf(levelError, addr, user, "Cannot start the shell: %v", err)
		return
	}
	defer sh.Close()
	s.in.logf(levelInfo, addr, user, "Shell started (%s, %dx%d)", tc.termType(), cols, rows)
	tc.onResize(func(c, r int) { sh.Resize(c, r) })

	idle := time.Duration(s.cfg.IdleTimeoutSec) * time.Second
	go func() {
		buf := make([]byte, 4096)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(idle))
			n, err := tc.Read(buf)
			if n > 0 {
				if _, werr := sh.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		sh.Close()
	}()
	buf := make([]byte, 16<<10)
	for {
		n, err := sh.Read(buf)
		if n > 0 {
			if werr := tc.writeData(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	select {
	case <-sh.Done():
		s.in.logf(levelInfo, addr, user, "Shell exited (code %d)", sh.ExitCode())
	case <-time.After(time.Second):
	}
}

// login runs the login dialog; it reports the authenticated user name.
func (s *telnetService) login(tc *telnetConn, conn net.Conn, addr string) (string, bool) {
	deadline := time.Now().Add(telnetLoginTimeout)
	_ = conn.SetDeadline(deadline)
	defer conn.SetDeadline(time.Time{})
	_ = tc.writeString("\r\nNexTerm telnet server\r\n\r\n")
	for attempt, empty := 0, 0; attempt < telnetMaxAttempts; {
		_ = tc.writeString("login: ")
		name, err := tc.readLine(true)
		if err != nil {
			return "", false
		}
		name = strings.TrimSpace(name)
		if name == "" {
			if empty++; empty > 10 {
				return "", false
			}
			continue
		}
		attempt++
		_ = tc.writeString("Password: ")
		pw, err := tc.readLine(false)
		if err != nil {
			return "", false
		}
		_ = tc.writeString("\r\n")
		u, err := s.db.checkPassword(remoteIP(addr), name, pw)
		if err == nil {
			s.in.logf(levelInfo, addr, u.Username, "Logged in")
			return u.Username, true
		}
		s.in.logf(levelWarn, addr, name, "Login failed: %v", err)
		if errors.Is(err, errTooManyFails) {
			_ = tc.writeString("Too many failed logins. Try again later.\r\n")
			return "", false
		}
		_ = tc.writeString("Login incorrect\r\n\r\n")
	}
	return "", false
}

// ---- telnet connection --------------------------------------------------------------------------------------------

// telnetConn filters telnet commands out of the input (answering option negotiation, tracking NAWS / TTYPE) and
// escapes IAC in the output.
type telnetConn struct {
	conn net.Conn
	br   *bufio.Reader

	wmu sync.Mutex

	mu       sync.Mutex
	cols     int
	rows     int
	term     string
	resize   func(c, r int)
	us       map[byte]bool // options enabled on our side (WILL)
	them     map[byte]bool // options enabled on the client side (DO)
	pendCR   bool
	sentType bool
}

func newTelnetConn(c net.Conn) *telnetConn {
	return &telnetConn{conn: c, br: bufio.NewReaderSize(c, 4096), cols: 80, rows: 24, us: map[byte]bool{},
		them: map[byte]bool{}}
}

func (t *telnetConn) write(b []byte) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	_ = t.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err := t.conn.Write(b)
	return err
}

func (t *telnetConn) writeString(s string) error { return t.writeData([]byte(s)) }

// writeData sends data, doubling IAC bytes.
func (t *telnetConn) writeData(b []byte) error {
	if bytes.IndexByte(b, tnIAC) >= 0 {
		b = bytes.ReplaceAll(b, []byte{tnIAC}, []byte{tnIAC, tnIAC})
	}
	return t.write(b)
}

// negotiate announces character mode with server-side echo and asks for window size and terminal type.
func (t *telnetConn) negotiate() {
	t.mu.Lock()
	t.us[optEcho], t.us[optSGA], t.us[optBinary] = true, true, true
	t.mu.Unlock()
	_ = t.write([]byte{
		tnIAC, tnWILL, optEcho,
		tnIAC, tnWILL, optSGA,
		tnIAC, tnDO, optSGA,
		tnIAC, tnWILL, optBinary,
		tnIAC, tnDO, optBinary,
		tnIAC, tnDO, optNAWS,
		tnIAC, tnDO, optTType,
	})
}

func (t *telnetConn) size() (int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cols, t.rows
}

func (t *telnetConn) termType() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.term == "" {
		return "xterm-256color"
	}
	return t.term
}

func (t *telnetConn) onResize(fn func(c, r int)) {
	t.mu.Lock()
	t.resize = fn
	t.mu.Unlock()
}

// Read returns user data with telnet commands removed and CR LF / CR NUL folded to CR.
func (t *telnetConn) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if n > 0 && t.br.Buffered() == 0 {
			break
		}
		b, err := t.br.ReadByte()
		if err != nil {
			return n, err
		}
		if b == tnIAC {
			d, err := t.command()
			if err != nil {
				return n, err
			}
			if d >= 0 {
				p[n] = byte(d)
				n++
			}
			continue
		}
		if t.pendCR {
			t.pendCR = false
			if b == '\n' || b == 0 {
				continue
			}
		}
		if b == '\r' {
			t.pendCR = true
		}
		p[n] = b
		n++
	}
	return n, nil
}

// command handles the bytes after IAC; it returns a data byte to deliver (or -1).
func (t *telnetConn) command() (int, error) {
	c, err := t.br.ReadByte()
	if err != nil {
		return -1, err
	}
	switch c {
	case tnIAC:
		return tnIAC, nil
	case tnDO, tnDONT, tnWILL, tnWONT:
		opt, err := t.br.ReadByte()
		if err != nil {
			return -1, err
		}
		t.option(c, opt)
	case tnSB:
		return -1, t.subnegotiation()
	case tnIP:
		return 0x03, nil // interrupt → Ctrl+C
	case tnEC:
		return 0x7f, nil
	case tnEL:
		return 0x15, nil
	case tnAYT:
		_ = t.write([]byte("\r\n[yes]\r\n"))
	}
	return -1, nil
}

// option answers DO/DONT/WILL/WONT without creating negotiation loops (only state changes are acknowledged).
func (t *telnetConn) option(cmd, opt byte) {
	t.mu.Lock()
	var reply []byte
	var sendTType bool
	switch cmd {
	case tnDO:
		switch opt {
		case optEcho, optSGA, optBinary:
			if !t.us[opt] {
				t.us[opt] = true
				reply = []byte{tnIAC, tnWILL, opt}
			}
		default:
			reply = []byte{tnIAC, tnWONT, opt}
		}
	case tnDONT:
		if t.us[opt] {
			t.us[opt] = false
			reply = []byte{tnIAC, tnWONT, opt}
		}
	case tnWILL:
		switch opt {
		case optNAWS, optTType, optSGA, optBinary:
			if !t.them[opt] {
				t.them[opt] = true
				if opt != optNAWS && opt != optTType {
					reply = []byte{tnIAC, tnDO, opt}
				}
			}
			if opt == optTType && !t.sentType {
				t.sentType = true
				sendTType = true
			}
		default:
			reply = []byte{tnIAC, tnDONT, opt}
		}
	case tnWONT:
		if t.them[opt] {
			t.them[opt] = false
			reply = []byte{tnIAC, tnDONT, opt}
		}
	}
	t.mu.Unlock()
	if reply != nil {
		_ = t.write(reply)
	}
	if sendTType {
		_ = t.write([]byte{tnIAC, tnSB, optTType, 1 /* SEND */, tnIAC, tnSE})
	}
}

// subnegotiation reads IAC SB … IAC SE (NAWS window size, TTYPE IS).
func (t *telnetConn) subnegotiation() error {
	var buf []byte
	for {
		b, err := t.br.ReadByte()
		if err != nil {
			return err
		}
		if b == tnIAC {
			nb, err := t.br.ReadByte()
			if err != nil {
				return err
			}
			if nb == tnSE {
				break
			}
			b = nb // IAC IAC = 255
		}
		if len(buf) < 512 {
			buf = append(buf, b)
		}
	}
	if len(buf) == 0 {
		return nil
	}
	switch buf[0] {
	case optNAWS:
		if len(buf) >= 5 {
			cols := int(buf[1])<<8 | int(buf[2])
			rows := int(buf[3])<<8 | int(buf[4])
			if cols > 0 && rows > 0 && cols <= 1000 && rows <= 1000 {
				t.mu.Lock()
				t.cols, t.rows = cols, rows
				fn := t.resize
				t.mu.Unlock()
				if fn != nil {
					fn(cols, rows)
				}
			}
		}
	case optTType:
		if len(buf) >= 2 && buf[1] == 0 /* IS */ {
			name := strings.ToLower(strings.TrimSpace(string(buf[2:])))
			if name != "" && len(name) <= 64 && isPrintableASCII(name) {
				t.mu.Lock()
				t.term = mapTermType(name)
				t.mu.Unlock()
			}
		}
	}
	return nil
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// mapTermType normalizes TTYPE names (e.g. "XTERM", "ANSI", "VT100") to terminfo names.
func mapTermType(name string) string {
	switch name {
	case "ansi", "vt100", "vt102", "vt220", "linux", "screen", "screen-256color", "tmux", "tmux-256color", "dumb":
		return name
	}
	return "xterm-256color" // xterm and unknown names: every modern client handles it
}

// readLine reads one line of the login dialog, echoing it when echo is set (the server owns echo: WILL ECHO).
func (t *telnetConn) readLine(echo bool) (string, error) {
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := t.Read(buf)
		if err != nil {
			return "", err
		}
		if n == 0 {
			continue
		}
		c := buf[0]
		switch {
		case c == '\r' || c == '\n':
			if echo {
				_ = t.write([]byte("\r\n"))
			}
			return string(line), nil
		case c == 0x7f || c == 0x08:
			if len(line) > 0 {
				line = line[:len(line)-1]
				if echo {
					_ = t.write([]byte("\b \b"))
				}
			}
		case c == 0x03 || c == 0x04:
			return "", io.EOF
		case c == 0x15: // Ctrl+U
			if echo {
				_ = t.write(bytes.Repeat([]byte("\b \b"), len(line)))
			}
			line = line[:0]
		case c >= 0x20 && len(line) < telnetMaxLine:
			line = append(line, c)
			if echo {
				_ = t.write([]byte{c})
			}
		}
	}
}
