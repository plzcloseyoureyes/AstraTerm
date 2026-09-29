package sshx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// The "ssh" terminal protocol (PROTO-1/3/4, TERM-2): a PTY (TERM from options.term, modes consistent with the
// backspace and encoding options, window size) running the login shell or options.remoteCommand, with agent and X11
// forwarding and environment variables requested before the shell starts.

const exitStatusWait = 5 * time.Second

func (p *Pool) openTerminal(ctx context.Context, req term.OpenRequest) (term.Backend, error) {
	conn, s := req.Connection, req.Session
	if conn == nil || s == nil {
		return nil, term.Permanent(errors.New("ssh: missing connection"))
	}
	secrets := cloneSecrets(req.Secrets)
	c, release, err := p.acquire(ctx, req.User, conn, secrets, dialOpts{session: s})
	if err != nil {
		return nil, err
	}
	sess, owner, relOverflow, err := c.NewSessionContext(ctx)
	if err != nil {
		release()
		return nil, fmt.Errorf("open SSH session: %w", err)
	}
	b := &sshBackend{
		pool:      p,
		client:    owner,
		sess:      sess,
		sessionID: s.ID,
		release: func() {
			relOverflow()
			release()
		},
		waited: make(chan struct{}),
	}
	b.code.Store(-1)
	if err := b.start(ctx, req.User, s, conn); err != nil {
		b.Close()
		return nil, err
	}
	p.trackSession(s.ID, b)
	return b, nil
}

type sshBackend struct {
	pool      *Pool
	client    *Client
	sess      *ssh.Session
	sessionID string
	stdin     io.WriteCloser
	out       io.Reader
	pty       bool
	release   func()

	waited  chan struct{}
	waitErr error
	code    atomic.Int64
	once    sync.Once
}

func (b *sshBackend) start(ctx context.Context, user *model.User, s *term.Session, conn *model.Connection) error {
	o := conn.Options
	sess := b.sess
	cols, rows := s.Size()
	remote := strings.TrimSpace(o.String("remoteCommand", ""))
	b.pty = remote == "" || !o.Bool("noPty")
	if b.pty {
		modes := ssh.TerminalModes{
			ssh.ECHO:          1,
			ssh.TTY_OP_ISPEED: 38400,
			ssh.TTY_OP_OSPEED: 38400,
			ssh.VERASE:        127,
			ssh.IUTF8:         1,
		}
		if strings.EqualFold(o.String("backspace"), "ctrl-h") {
			modes[ssh.VERASE] = 8
		}
		if enc, _ := term.LookupEncoding(o.String("encoding")); enc != nil {
			modes[ssh.IUTF8] = 0
		}
		if err := sess.RequestPty(o.String("term"), rows, cols, modes); err != nil {
			return fmt.Errorf("PTY request refused: %w", err)
		}
	}
	if o.Bool("agentForwarding") {
		if err := b.client.forwardAgent(sess); err != nil {
			s.Notice("Agent forwarding is unavailable: " + err.Error())
		}
	}
	if o.Bool("x11Forwarding") {
		if b.pool.d == nil || b.pool.d.Cfg == nil || !b.pool.d.Cfg.IsDesktop() {
			s.Notice("X11 forwarding to a local X server is only available in desktop mode.")
		} else if fwd, err := b.client.enableX11(); err != nil {
			s.Notice("X11 forwarding is unavailable: " + err.Error())
		} else if err := fwd.request(sess, 0); err != nil {
			s.Notice("X11 forwarding is unavailable: " + err.Error())
		}
	}
	env := o.StringMap("env")
	if _, ok := env["COLORTERM"]; !ok && b.pty {
		_ = sess.Setenv("COLORTERM", "truecolor") // ignored unless the server's AcceptEnv allows it
	}
	for k, v := range env {
		if validEnvName(k) {
			_ = sess.Setenv(k, v)
		}
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return err
	}
	b.stdin, b.out = stdin, stdout
	if !b.pty {
		stderr, err := sess.StderrPipe()
		if err != nil {
			return err
		}
		b.out = mergeOutput(stdout, stderr)
	}
	if remote != "" {
		err = sess.Start(remote)
	} else {
		err = sess.Shell()
	}
	if err != nil {
		return fmt.Errorf("start remote shell: %w", err)
	}
	go func() {
		b.waitErr = sess.Wait()
		close(b.waited)
	}()
	return nil
}

func validEnvName(k string) bool {
	if k == "" || len(k) > 256 {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// Read returns remote output; at the end it reports io.EOF for a normal exit and an error when the connection was
// lost (so the session may auto-reconnect).
func (b *sshBackend) Read(p []byte) (int, error) {
	n, err := b.out.Read(p)
	if err == nil {
		return n, nil
	}
	return n, b.finish()
}

func (b *sshBackend) finish() error {
	select {
	case <-b.waited:
	case <-time.After(exitStatusWait):
		// Some devices never close the channel; decide from the transport state.
		if !b.client.Alive() {
			return b.lost()
		}
		return io.EOF
	}
	var ee *ssh.ExitError
	switch err := b.waitErr; {
	case err == nil:
		b.code.Store(0)
		return io.EOF
	case errors.As(err, &ee):
		b.code.Store(int64(ee.ExitStatus()))
		return io.EOF
	}
	// Exit status missing or an I/O error: a dead transport means the connection was lost.
	select {
	case <-b.client.Done():
	case <-time.After(time.Second):
	}
	if !b.client.Alive() {
		return b.lost()
	}
	return io.EOF
}

func (b *sshBackend) lost() error {
	if err := b.client.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("connection to %s lost: %w", b.client.Conn.Host, err)
	}
	return fmt.Errorf("connection to %s lost", b.client.Conn.Host)
}

func (b *sshBackend) Write(p []byte) (int, error) { return b.stdin.Write(p) }

func (b *sshBackend) Resize(cols, rows int) error {
	if !b.pty {
		return nil
	}
	return b.sess.WindowChange(rows, cols)
}

// Signal delivers a signal. Under a PTY, INT and QUIT are sent as their control characters, which works on every
// server (many ignore "signal" requests for PTY sessions).
func (b *sshBackend) Signal(name string) error {
	if b.pty {
		switch name {
		case "INT":
			_, err := b.stdin.Write([]byte{0x03})
			return err
		case "QUIT":
			_, err := b.stdin.Write([]byte{0x1c})
			return err
		}
	}
	return b.sess.Signal(ssh.Signal(name))
}

// SendBreak sends an RFC 4335 BREAK (500 ms).
func (b *sshBackend) SendBreak() error {
	_, err := b.sess.SendRequest("break", false, ssh.Marshal(struct{ BreakLength uint32 }{500}))
	return err
}

func (b *sshBackend) ExitCode() int { return int(b.code.Load()) }

func (b *sshBackend) Close() error {
	b.once.Do(func() {
		if b.stdin != nil {
			_ = b.stdin.Close()
		}
		_ = b.sess.Close()
		b.pool.untrackSession(b.sessionID, b)
		b.release()
	})
	return nil
}

// mergeOutput interleaves stdout and stderr of a PTY-less command for the terminal: newlines become CRLF and stderr
// is shown in red (PROTO-3).
func mergeOutput(stdout, stderr io.Reader) io.Reader {
	pr, pw := io.Pipe()
	var mu sync.Mutex
	var wg sync.WaitGroup
	pump := func(r io.Reader, red bool) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		var last byte
		for {
			n, err := r.Read(buf)
			if n > 0 {
				data := toCRLF(buf[:n], &last)
				if red {
					data = append(append([]byte("\x1b[31m"), data...), "\x1b[0m"...)
				}
				mu.Lock()
				_, werr := pw.Write(data)
				mu.Unlock()
				if werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump(stdout, false)
	go pump(stderr, true)
	go func() {
		wg.Wait()
		pw.Close()
	}()
	return pr
}

func toCRLF(p []byte, last *byte) []byte {
	out := make([]byte, 0, len(p)+len(p)/8)
	for _, c := range p {
		if c == '\n' && *last != '\r' {
			out = append(out, '\r')
		}
		out = append(out, c)
		*last = c
	}
	return out
}
