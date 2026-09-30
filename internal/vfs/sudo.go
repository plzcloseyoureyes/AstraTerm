package vfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// sudo support (FILE-11): "Save with sudo" (sudo tee over exec) and "browse as root" (sudo <sftp-server> spoken to
// through sftp.NewClientPipe). The password comes from the connection secret sudoPassword, then the login password,
// then an interactive prompt; it is validated with `sudo -k -S -v` before it is ever sent next to other data.
//
// The password travels on the command's stdin, in front of the file content (tee) or the SFTP protocol (sftp-server),
// so it must be consumed by sudo — never by the command. Sending it blindly is unsafe: whenever sudo does not ask
// (cached credentials — timestamp_type=global, a shared parent process —, or NOPASSWD for that one command) the
// password line would be written INTO the saved file or fed to sftp-server. Therefore commands run as
// `sudo -k -S -p <random marker> …` (-k: cached credentials are ignored, so sudo asks whenever the policy wants a
// password) and the password is written only after sudo printed its prompt on stderr; the data follows it. A
// second prompt (wrong password) kills the command. Without a prompt nothing is sent and the command is aborted.

// passwordSource returns the candidate sudo password for an attempt (0-based); save reports that the user asked to
// remember it. errNoMorePasswords ends the attempts.
type passwordSource func(ctx context.Context, attempt int, lastFailed bool) (pw string, save bool, err error)

var errNoMorePasswords = errors.New("no sudo password available")

// Tunables (tests shorten them).
var (
	sudoPromptTimeout = 20 * time.Second       // how long sudo may take to ask for the password
	sudoPromptQuiet   = 300 * time.Millisecond // a partial stderr line this quiet is a (PAM supplied) prompt
)

type sudoHelper struct {
	src     *sshSource
	getPass passwordSource
	onSave  func(ctx context.Context, pw string)

	mu       sync.Mutex
	state    int // 0 unknown, 1 no password needed, 2 password known
	password string
}

// ensure determines whether sudo needs a password and obtains a valid one. It returns "" when none is needed.
func (s *sudoHelper) ensure(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case 1:
		return "", nil
	case 2:
		return s.password, nil
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	stderr, code, err := s.src.Exec(pctx, "sudo -n true", nil, nil)
	cancel()
	if err != nil {
		return "", err
	}
	if code == 0 {
		s.state = 1
		return "", nil
	}
	if err := sudoFailure(stderr, code); err != nil {
		return "", err
	}
	lastFailed := false
	for attempt := range 8 {
		pw, save, err := s.getPass(ctx, attempt, lastFailed)
		if errors.Is(err, errNoMorePasswords) {
			break
		}
		if err != nil {
			return "", err
		}
		if pw == "" || strings.ContainsAny(pw, "\r\n") {
			continue // sudo reads one line: a password with a line break can never be typed
		}
		ok, err := s.validate(ctx, pw)
		if err != nil {
			return "", err
		}
		if ok {
			s.state, s.password = 2, pw
			if save && s.onSave != nil {
				s.onSave(ctx, pw)
			}
			return pw, nil
		}
		lastFailed = true
	}
	return "", fmt.Errorf("sudo: no valid password (%w)", errPermissionDenied)
}

// forget drops a "no password needed" / remembered-password state that turned out to be stale (the next call
// probes again).
func (s *sudoHelper) forget() {
	s.mu.Lock()
	s.state, s.password = 0, ""
	s.mu.Unlock()
}

// errPermissionDenied wraps fs.ErrPermission (HTTP 403 permission_denied).
var errPermissionDenied = fmt.Errorf("sudo refused: %w", fs.ErrPermission)

// sudoFailure classifies a failed `sudo -n true`: a missing sudo or a tty requirement are hard errors; "a password
// is required" is expected (nil).
func sudoFailure(stderr []byte, code int) error {
	msg := strings.ToLower(string(stderr))
	switch {
	case code == 127 || strings.Contains(msg, "not found"):
		return fmt.Errorf("sudo is not installed on the server: %w", ErrNotSupported)
	case strings.Contains(msg, "tty"):
		return fmt.Errorf("sudo requires a terminal on this server (requiretty): %w", ErrNotSupported)
	case strings.Contains(msg, "not in the sudoers") || strings.Contains(msg, "not allowed") ||
		strings.Contains(msg, "may not run sudo"):
		return fmt.Errorf("the account may not use sudo: %w", errPermissionDenied)
	}
	return nil
}

// passwordRequired reports whether a failed `sudo -n …` failed only because a password is needed now.
func passwordRequired(stderr []byte) bool {
	msg := strings.ToLower(string(stderr))
	return strings.Contains(msg, "password is required") || strings.Contains(msg, "a password is required")
}

// validate checks pw with `sudo -k -S -v` (-k ignores cached credentials, so the password is really checked; it
// is the only stdin content, so nothing else can be taken for it).
func (s *sudoHelper) validate(ctx context.Context, pw string) (bool, error) {
	vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	stderr, code, err := s.src.Exec(vctx, "sudo -k -S -p '' -v", strings.NewReader(pw+"\n"), nil)
	if err != nil {
		return false, err
	}
	if code == 0 {
		return true, nil
	}
	if err := sudoFailure(stderr, code); err != nil && !errors.Is(err, errPermissionDenied) {
		return false, err
	}
	return false, nil
}

// ---- running a command under sudo ---------------------------------------------------------------------------------

// sudoMarker returns a random password prompt ("%" never appears: sudo expands %-escapes in prompts).
func sudoMarker() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "[astraterm-sudo:" + hex.EncodeToString(b) + "]"
}

// promptWatch is the stderr of a sudo command. It keeps a bounded copy for error messages and signals prompts: the
// marker (the -p prompt), or — for PAM modules that supply their own prompt text — a partial line (no trailing
// newline) followed by sudoPromptQuiet of silence, which is how every password prompt looks.
type promptWatch struct {
	marker []byte

	mu      sync.Mutex
	text    capWriter
	tail    []byte // the last len(marker)-1 bytes (a marker split across writes)
	partial bool   // the output ends inside a line
	gen     int    // bumped on every write (quiet detection)
	marked  bool   // the marker was seen: only markers count as prompts from then on
	prompts int
	notify  chan struct{} // one token per new prompt (capacity 1)
}

func newPromptWatch(marker string) *promptWatch {
	return &promptWatch{marker: []byte(marker), text: capWriter{max: 16 << 10}, notify: make(chan struct{}, 1)}
}

func (w *promptWatch) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.text.Write(p)
	data := append(w.tail, p...)
	if n := bytes.Count(data, w.marker); n > 0 {
		w.prompts += n
		w.marked = true
		w.signal()
	}
	if k := len(w.marker) - 1; len(data) > k {
		w.tail = append(w.tail[:0], data[len(data)-k:]...)
	} else {
		w.tail = append(w.tail[:0], data...)
	}
	if len(p) > 0 {
		w.partial = p[len(p)-1] != '\n'
		w.gen++
		if w.partial && !w.marked {
			gen := w.gen
			time.AfterFunc(sudoPromptQuiet, func() {
				w.mu.Lock()
				defer w.mu.Unlock()
				// A partial line that is a started marker is not a prompt of its own.
				if w.gen == gen && w.partial && !w.marked && !w.markerPrefixLocked() {
					w.prompts++
					w.partial = false // counted once
					w.signal()
				}
			})
		}
	}
	return len(p), nil
}

// markerPrefixLocked reports whether the output ends with the beginning of the marker.
func (w *promptWatch) markerPrefixLocked() bool {
	for k := min(len(w.tail), len(w.marker)-1); k > 0; k-- {
		if bytes.Equal(w.tail[len(w.tail)-k:], w.marker[:k]) {
			return true
		}
	}
	return false
}

func (w *promptWatch) signal() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

func (w *promptWatch) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.prompts
}

// message is stderr without the markers (for errors).
func (w *promptWatch) message() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.TrimSpace(strings.ReplaceAll(w.text.String(), string(w.marker), ""))
}

// sudoProc is one command running under sudo whose password handshake is done.
type sudoProc struct {
	sess    *ssh.Session
	release func()
	stdin   io.WriteCloser
	stdout  io.Reader
	watch   *promptWatch
	done    chan error // exit status (buffered)
	killed  chan struct{}
	once    sync.Once
}

var (
	errSudoNoPrompt = fmt.Errorf("sudo did not ask for the password (NOPASSWD for this command only?): %w", ErrNotSupported)
	errSudoWrongPw  = fmt.Errorf("sudo rejected the password: %w", fs.ErrPermission)
)

func (p *sudoProc) kill() {
	p.once.Do(func() {
		close(p.killed)
		_ = p.sess.Signal(ssh.SIGKILL)
		_ = p.sess.Close()
		p.release()
	})
}

// exitError maps the command's exit to an error (nil on success).
func (p *sudoProc) exitError(err error) error {
	if err == nil {
		return nil
	}
	if p.watch.count() > 1 {
		return errSudoWrongPw // killed at the second prompt
	}
	if ee, ok := errors.AsType[*ssh.ExitError](err); ok {
		return shellError([]byte(p.watch.message()), ee.ExitStatus(), "sudo")
	}
	return err
}

// start runs `sudo cmd` (cmd is a shell command line whose words are already quoted): with a validated password it
// waits for sudo's prompt, answers it and returns with the command running; stdin/stdout are the command's. A later
// second prompt (the password was refused) kills the command before it can read any data. Without a password
// ("sudo -n") the command starts right away; sudo never reads stdin then.
func (s *sudoHelper) start(ctx context.Context, cmd string, wantStdout bool) (*sudoProc, error) {
	pw, err := s.ensure(ctx)
	if err != nil {
		return nil, err
	}
	cl, err := s.src.client(ctx)
	if err != nil {
		return nil, err
	}
	sess, _, release, err := cl.NewSessionContext(ctx)
	if err != nil {
		return nil, err
	}
	p := &sudoProc{sess: sess, release: release, watch: newPromptWatch(sudoMarker()), done: make(chan error, 1),
		killed: make(chan struct{})}
	fail := func(err error) (*sudoProc, error) {
		p.kill()
		return nil, err
	}
	if p.stdin, err = sess.StdinPipe(); err != nil {
		return fail(err)
	}
	if wantStdout {
		if p.stdout, err = sess.StdoutPipe(); err != nil {
			return fail(err)
		}
	}
	sess.Stderr = p.watch
	full := "sudo -n " + cmd
	if pw != "" {
		full = "sudo -k -S -p " + shq(string(p.watch.marker)) + " " + cmd
	}
	if err := sess.Start(full); err != nil {
		return fail(err)
	}
	go func() { p.done <- sess.Wait() }()
	if pw == "" {
		return p, nil
	}
	// Wait for the prompt; nothing is written before it appeared.
	timer := time.NewTimer(sudoPromptTimeout)
	defer timer.Stop()
	for p.watch.count() == 0 {
		select {
		case <-p.watch.notify:
		case err := <-p.done:
			p.done <- err
			if err := p.exitError(err); err != nil {
				return fail(err)
			}
			return fail(errors.New("sudo ended without running the command"))
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-timer.C:
			return fail(errSudoNoPrompt)
		}
	}
	if _, err := io.WriteString(p.stdin, pw+"\n"); err != nil {
		return fail(err)
	}
	// A second prompt means the password was refused: stop the command before the data can be taken for a retry.
	go func() {
		for {
			select {
			case <-p.killed:
				return
			case <-p.watch.notify:
				if p.watch.count() > 1 {
					p.kill()
					return
				}
			}
		}
	}()
	return p, nil
}

// wait returns the command's exit (as an error) or ctx's error (the command is killed then).
func (p *sudoProc) wait(ctx context.Context) error {
	select {
	case err := <-p.done:
		p.done <- err
		return p.exitError(err)
	case <-ctx.Done():
		p.kill()
		return ctx.Err()
	}
}

// tee writes data to p as root (keeps the file's inode, owner and mode).
func (s *sudoHelper) tee(ctx context.Context, p string, data []byte) error {
	for retry := 0; ; retry++ {
		proc, err := s.start(ctx, "tee -- "+shq(p)+" >/dev/null", false)
		if err != nil {
			return err
		}
		_, werr := proc.stdin.Write(data)
		_ = proc.stdin.Close()
		err = proc.wait(ctx)
		stale := err != nil && passwordRequired([]byte(proc.watch.message()))
		proc.kill()
		switch {
		case stale && retry == 0:
			s.forget() // "no password needed" was cached credentials that expired: probe again, once
			continue
		case errors.Is(err, errSudoWrongPw):
			s.forget()
			return err
		case err != nil:
			return err
		}
		return werr
	}
}

// sftpServerPaths are probed (in order) for "browse as root".
var sftpServerPaths = []string{
	"/usr/lib/openssh/sftp-server", "/usr/libexec/openssh/sftp-server", "/usr/lib/ssh/sftp-server",
	"/usr/libexec/sftp-server", "/usr/lib/sftp-server", "/usr/local/libexec/sftp-server",
	"/usr/libexec/ssh/sftp-server", "/usr/sbin/sftp-server", "/usr/local/lib/sftp-server",
}

// findSFTPServer locates the sftp-server binary (custom command wins).
func (s *sudoHelper) findSFTPServer(ctx context.Context, custom string) (string, error) {
	if custom = strings.TrimSpace(custom); custom != "" {
		return custom, nil
	}
	var b strings.Builder
	b.WriteString("for p in")
	for _, p := range sftpServerPaths {
		b.WriteString(" " + shq(p))
	}
	b.WriteString(`; do if [ -x "$p" ]; then echo "$p"; exit 0; fi; done; command -v sftp-server || exit 1`)
	out, err := runShell(ctx, s.src, b.String(), nil)
	if err != nil {
		return "", fmt.Errorf("no sftp-server binary found on the server (only internal-sftp?): %w", ErrNotSupported)
	}
	p := strings.TrimSpace(firstLine(string(out)))
	if p == "" {
		return "", fmt.Errorf("no sftp-server binary found on the server: %w", ErrNotSupported)
	}
	return shq(p), nil
}

// sudoChannel is a private SFTP client talking to "sudo sftp-server" over one exec channel; it is re-created after
// the channel or transport died.
type sudoChannel struct {
	h      *sudoHelper
	server string

	mu      sync.Mutex
	sc      *sftp.Client
	release func()
}

func (c *sudoChannel) get(ctx context.Context) (*sftp.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sc != nil {
		return c.sc, nil
	}
	sc, err := c.open(ctx)
	if err != nil && errors.Is(err, errSudoStale) {
		c.h.forget() // "no password needed" was cached credentials that expired: probe again, once
		sc, err = c.open(ctx)
	}
	return sc, err
}

var errSudoStale = errors.New("sudo: a password is required")

func (c *sudoChannel) open(ctx context.Context) (*sftp.Client, error) {
	proc, err := c.h.start(ctx, c.server, true)
	if err != nil {
		return nil, err
	}
	type res struct {
		sc  *sftp.Client
		err error
	}
	ch := make(chan res, 1)
	go func() {
		sc, err := sftp.NewClientPipe(proc.stdout, proc.stdin, sftp.UseConcurrentReads(true), sftp.UseConcurrentWrites(true),
			sftp.MaxConcurrentRequestsPerFile(64))
		ch <- res{sc, err}
	}()
	var r res
	select {
	case r = <-ch:
	case <-time.After(20 * time.Second):
		r.err = errors.New("timeout")
	case <-ctx.Done():
		r.err = ctx.Err()
	}
	if r.err != nil {
		proc.kill()
		if r.sc != nil {
			r.sc.Close()
		}
		if proc.watch.count() > 1 {
			c.h.forget()
			return nil, errSudoWrongPw
		}
		msg := proc.watch.message()
		if msg == "" {
			msg = r.err.Error()
		}
		if passwordRequired([]byte(msg)) {
			return nil, fmt.Errorf("%w: %s", errSudoStale, cleanMsg(msg))
		}
		return nil, fmt.Errorf("sudo sftp-server: %s", cleanMsg(msg))
	}
	c.sc = r.sc
	c.release = func() {
		r.sc.Close()
		proc.kill()
	}
	go func(sc *sftp.Client) {
		_ = sc.Wait()
		c.mu.Lock()
		if c.sc == sc {
			c.sc = nil
			rel := c.release
			c.release = nil
			c.mu.Unlock()
			if rel != nil {
				rel()
			}
			return
		}
		c.mu.Unlock()
	}(r.sc)
	return r.sc, nil
}

func (c *sudoChannel) close() {
	c.mu.Lock()
	rel := c.release
	c.sc, c.release = nil, nil
	c.mu.Unlock()
	if rel != nil {
		rel()
	}
}
