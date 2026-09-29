package monitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/nexterm/nexterm/internal/sshx"
)

// command is one piece of work for a monitored host. Exactly one of sh, ps and argv is set.
type command struct {
	sh   string   // POSIX sh script: runs as `/bin/sh -s` with the script on stdin (login-shell independent)
	ps   string   // PowerShell script: runs with -EncodedCommand
	argv []string // plain command, exec'd directly (local host only: remote work always goes through sh or PowerShell)
	// stdin follows the script (sh) or is the whole input (argv), e.g. the sudo password line. Never logged.
	stdin string
	// hold keeps stdin open after the input was written until the stream is stopped (streams only): scripts detect
	// the end of the channel by reading EOF, so long-running helpers (tail -F) die with the stream.
	hold bool
}

// result is the outcome of a finished command.
type result struct {
	Stdout []byte
	Stderr []byte
	Code   int // exit status; -1 when unknown (killed by a signal, no status reported)
}

func (r *result) ok() bool { return r != nil && r.Code == 0 }

// errText returns the command's stderr (or stdout) as a short single-paragraph message.
func (r *result) errText() string {
	if r == nil {
		return ""
	}
	s := strings.TrimSpace(string(r.Stderr))
	if s == "" {
		s = strings.TrimSpace(string(r.Stdout))
	}
	return clip(s, 400)
}

// stream is a running command whose stdout is read incrementally. Call wait after stdout reached EOF (or after stop);
// every stream must end with close (or stop + wait), which releases the channel / reaps the local process.
type stream struct {
	Stdout io.Reader
	wait   func() (*result, error)
	stop   func()
}

// close stops the command and waits for it to end (idempotent).
func (s *stream) close() {
	s.stop()
	_, _ = s.wait()
}

// runner executes commands on a monitored host: an SSH transport or the NexTerm host itself.
type runner interface {
	run(ctx context.Context, c command) (*result, error)
	start(ctx context.Context, c command) (*stream, error)
}

// Output limits.
const (
	maxStdout = 48 << 20
	maxStderr = 64 << 10
)

// capBuffer keeps at most max bytes (the rest is counted and dropped).
type capBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room < len(p) {
		if room > 0 {
			b.buf.Write(p[:room])
		}
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *capBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && cut < len(s) && s[cut]&0xC0 == 0x80 { // do not split a UTF-8 sequence
		cut--
	}
	return s[:cut] + "…"
}

// ---- SSH ------------------------------------------------------------------------------------------------------------

// sshRunner runs commands over the session channels of an SSH transport (non-PTY exec, so nothing shows up in `who`).
type sshRunner struct {
	open func(ctx context.Context) (*ssh.Session, func(), error)
}

// newSSHRunner runs on a pooled transport; the pool opens an overflow connection when the server's MaxSessions limit
// is reached.
func newSSHRunner(c *sshx.Client) sshRunner {
	return sshRunner{open: func(ctx context.Context) (*ssh.Session, func(), error) {
		sess, _, release, err := c.NewSessionContext(ctx)
		return sess, release, err
	}}
}

func (r sshRunner) render(c command) (line string, stdin []byte, err error) {
	switch {
	case c.sh != "":
		return "/bin/sh -s", []byte(c.sh + "\n" + c.stdin), nil
	case c.ps != "":
		return powershellLine(c.ps), []byte(c.stdin), nil
	case len(c.argv) > 0:
		return "", nil, errors.New("plain argument vectors run on the local host only")
	}
	return "", nil, errors.New("empty command")
}

func (r sshRunner) run(ctx context.Context, c command) (*result, error) {
	line, in, err := r.render(c)
	if err != nil {
		return nil, err
	}
	sess, release, err := r.open(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	defer sess.Close()
	so, se := &capBuffer{max: maxStdout}, &capBuffer{max: maxStderr}
	sess.Stdout, sess.Stderr = so, se
	sess.Stdin = bytes.NewReader(in)
	if err := sess.Start(line); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		return nil, ctx.Err()
	}
	return sshResult(so.Bytes(), se.Bytes(), err)
}

func (r sshRunner) start(ctx context.Context, c command) (*stream, error) {
	line, in, err := r.render(c)
	if err != nil {
		return nil, err
	}
	sess, rel, err := r.open(ctx)
	if err != nil {
		return nil, err
	}
	// The release of an overflow connection (MaxSessions) must happen exactly once, whether the stream ends through
	// wait or only through stop (a caller abandoning the stream on an error path).
	release := sync.OnceFunc(rel)
	fail := func(err error) (*stream, error) {
		sess.Close()
		release()
		return nil, err
	}
	out, err := sess.StdoutPipe()
	if err != nil {
		return fail(err)
	}
	se := &capBuffer{max: maxStderr}
	sess.Stderr = se
	var stdin io.WriteCloser
	if c.hold {
		if stdin, err = sess.StdinPipe(); err != nil {
			return fail(err)
		}
	} else {
		sess.Stdin = bytes.NewReader(in)
	}
	if err := sess.Start(line); err != nil {
		return fail(err)
	}
	if stdin != nil {
		go func() { _, _ = stdin.Write(in) }() // the rest stays open until stop
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			if stdin != nil {
				_ = stdin.Close()
			}
			_ = sess.Signal(ssh.SIGKILL)
			_ = sess.Close()
		})
		release() // the channel is closed: the transport it ran on may go
	}
	unwatch := context.AfterFunc(ctx, stop)
	var waitOnce sync.Once
	var res *result
	var werr error
	wait := func() (*result, error) {
		waitOnce.Do(func() {
			done := make(chan error, 1)
			go func() { done <- sess.Wait() }()
			select {
			case werr = <-done:
			case <-time.After(5 * time.Second): // stdout reached EOF but no exit status: give up
				stop()
				werr = <-done
			}
			unwatch()
			stop()
			release()
			res, werr = sshResult(nil, se.Bytes(), werr)
		})
		return res, werr
	}
	return &stream{Stdout: out, wait: wait, stop: stop}, nil
}

func sshResult(stdout, stderr []byte, err error) (*result, error) {
	res := &result{Stdout: stdout, Stderr: stderr}
	var ee *ssh.ExitError
	var em *ssh.ExitMissingError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.Code = ee.ExitStatus()
		if ee.Signal() != "" {
			res.Code = -1
		}
	case errors.As(err, &em):
		res.Code = -1
	default:
		res.Code = -1
		return res, err
	}
	return res, nil
}

// ---- local host -----------------------------------------------------------------------------------------------------

// localRunner runs commands on the NexTerm host (local sessions and the System info view). Callers gate it to desktop
// mode or administrators.
type localRunner struct{}

func (localRunner) command(ctx context.Context, c command) (*exec.Cmd, []byte, error) {
	var cmd *exec.Cmd
	var in []byte
	switch {
	case c.sh != "":
		if runtime.GOOS == "windows" {
			return nil, nil, errors.New("POSIX shell scripts are not available on Windows")
		}
		cmd = exec.CommandContext(ctx, "/bin/sh", "-s")
		in = []byte(c.sh + "\n" + c.stdin)
	case c.ps != "":
		cmd = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", psEncode(c.ps))
		in = []byte(c.stdin)
	case len(c.argv) > 0:
		cmd = exec.CommandContext(ctx, c.argv[0], c.argv[1:]...)
		in = []byte(c.stdin)
	default:
		return nil, nil, errors.New("empty command")
	}
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.WaitDelay = 2 * time.Second
	return cmd, in, nil
}

func (l localRunner) run(ctx context.Context, c command) (*result, error) {
	cmd, in, err := l.command(ctx, c)
	if err != nil {
		return nil, err
	}
	so, se := &capBuffer{max: maxStdout}, &capBuffer{max: maxStderr}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(in), so, se
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return execResult(so.Bytes(), se.Bytes(), err)
}

func (l localRunner) start(ctx context.Context, c command) (*stream, error) {
	cmd, in, err := l.command(ctx, c)
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	se := &capBuffer{max: maxStderr}
	cmd.Stderr = se
	var stdin io.WriteCloser
	if c.hold {
		if stdin, err = cmd.StdinPipe(); err != nil {
			return nil, err
		}
	} else {
		cmd.Stdin = bytes.NewReader(in)
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if stdin != nil {
		go func() { _, _ = stdin.Write(in) }()
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			if stdin != nil {
				_ = stdin.Close()
			}
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
	}
	var waitOnce sync.Once
	var res *result
	var werr error
	wait := func() (*result, error) {
		waitOnce.Do(func() {
			werr = cmd.Wait()
			stop()
			res, werr = execResult(nil, se.Bytes(), werr)
		})
		return res, werr
	}
	return &stream{Stdout: out, wait: wait, stop: stop}, nil
}

func execResult(stdout, stderr []byte, err error) (*result, error) {
	res := &result{Stdout: stdout, Stderr: stderr}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.Code = ee.ExitCode()
	case errors.Is(err, exec.ErrNotFound):
		res.Code = 127
		res.Stderr = append(res.Stderr, []byte(fmt.Sprintf("%v", err))...)
	default:
		res.Code = -1
		return res, err
	}
	return res, nil
}
