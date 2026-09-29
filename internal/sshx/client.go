package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/termstead/termstead/internal/model"
)

// Client is a pooled, authenticated SSH connection. It embeds *ssh.Client; Conn is the (secret-free) connection it
// was dialed for. Obtain clients from Pool.Get / Acquire / ForSession and always call the returned release func.
type Client struct {
	*ssh.Client
	Conn *model.Connection

	pool     *Pool
	key      string
	user     *model.User
	secrets  map[string]string // kept in memory to open overflow connections without prompting again
	overflow bool
	parent   func() // releases the gateway (jump host) client this one was dialed through

	refs int         // guarded by pool.mu
	idle *time.Timer // guarded by pool.mu

	done      chan struct{} // closed when the transport has ended
	closeOnce sync.Once
	errMu     sync.Mutex
	err       error

	info    model.SSHConnInfo
	latency atomic.Int64

	sftpMu  sync.Mutex
	sftp    *sftp.Client
	sftpRel func()

	agentOnce   sync.Once
	agentErr    error
	keyringOnce sync.Once
	keyring     agent.Agent
	x11Once     sync.Once
	x11         *x11Forwarder
	x11Err      error
}

// newClient wraps an established ssh.Client and starts its lifecycle goroutines.
func newClient(p *Pool, sc *ssh.Client, conn *model.Connection, user *model.User, secrets map[string]string) *Client {
	c := &Client{Client: sc, Conn: conn, pool: p, user: user, secrets: secrets, done: make(chan struct{})}
	c.info = model.SSHConnInfo{
		ServerVersion: string(sc.ServerVersion()),
		ClientVersion: string(sc.ClientVersion()),
	}
	if am, ok := sc.Conn.(ssh.AlgorithmsConnMetadata); ok {
		a := am.Algorithms()
		c.info.Kex, c.info.HostKeyAlgo, c.info.Cipher, c.info.MAC = a.KeyExchange, a.HostKey, a.Write.Cipher, a.Write.MAC
	}
	go func() {
		err := sc.Wait()
		c.setErr(err)
		close(c.done)
		c.cleanup()
	}()
	return c
}

func (c *Client) setErr(err error) {
	c.errMu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.errMu.Unlock()
}

// Err returns why the transport ended (nil while alive or after a clean close).
func (c *Client) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

// Alive reports whether the transport is still up.
func (c *Client) Alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// Done is closed when the connection has ended.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close tears the connection down (releasing the gateway it was dialed through). Pool users call release instead.
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.Client.Close()
	})
	return err
}

// fail closes a connection that is known to be dead (keepalive timeout).
func (c *Client) fail(err error) {
	c.setErr(err)
	_ = c.Close()
}

// cleanup runs once the transport ended.
func (c *Client) cleanup() {
	c.sftpMu.Lock()
	sc, rel := c.sftp, c.sftpRel
	c.sftp, c.sftpRel = nil, nil
	c.sftpMu.Unlock()
	if sc != nil {
		_ = sc.Close()
	}
	if rel != nil {
		rel()
	}
	// Synchronize with a concurrent enableX11 (and disable later ones) before reading c.x11.
	c.x11Once.Do(func() { c.x11Err = errors.New("ssh connection is closed") })
	if c.x11 != nil {
		c.x11.close()
	}
	if c.parent != nil {
		c.parent()
	}
}

// Info describes the transport: versions, negotiated algorithms, host key fingerprint and latency.
func (c *Client) Info() model.SSHConnInfo {
	info := c.info
	info.LatencyMs = c.latency.Load()
	return info
}

// DialContext opens a direct-tcpip channel to addr through the SSH server.
func (c *Client) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return c.Client.DialContext(ctx, network, addr)
}

// isChannelLimit reports whether opening a session channel failed because the server refuses more sessions on this
// connection (MaxSessions / no-more-sessions).
func isChannelLimit(err error) bool {
	var oce *ssh.OpenChannelError
	return errors.As(err, &oce) && (oce.Reason == ssh.ResourceShortage || oce.Reason == ssh.Prohibited)
}

// NewSessionContext opens a session channel, transparently falling back to another pooled (or newly dialed)
// connection when the server's MaxSessions limit is reached. release must be called after the session is closed
// (it releases the overflow connection, if one was used). owner is the client that carries the session.
func (c *Client) NewSessionContext(ctx context.Context) (sess *ssh.Session, owner *Client, release func(), err error) {
	s, err := c.Client.NewSession()
	if err == nil {
		return s, c, func() {}, nil
	}
	if !isChannelLimit(err) || c.pool == nil || c.key == "" {
		return nil, nil, nil, err
	}
	for _, oc := range c.pool.overflowCandidates(c) {
		if s2, err2 := oc.Client.NewSession(); err2 == nil {
			return s2, oc, c.pool.releaser(oc), nil
		}
		c.pool.release(oc)
	}
	nc, rel, derr := c.pool.dialOverflow(ctx, c)
	if derr != nil {
		return nil, nil, nil, fmt.Errorf("%w (opening an additional connection failed: %v)", err, derr)
	}
	s, err = nc.Client.NewSession()
	if err != nil {
		rel()
		return nil, nil, nil, err
	}
	return s, nc, rel, nil
}

// maxExecOutput bounds the captured stdout / stderr of Exec.
const maxExecOutput = 64 << 20

type capBuffer struct {
	bytes.Buffer
	truncated bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if room := maxExecOutput - b.Len(); room < len(p) {
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		b.truncated = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// Exec runs cmd (without a PTY) and returns its output and exit status. exitCode is -1 when the server did not
// report one. Cancelling ctx kills the remote command.
func (c *Client) Exec(ctx context.Context, cmd string) (stdout, stderr []byte, exitCode int, err error) {
	sess, _, release, err := c.NewSessionContext(ctx)
	if err != nil {
		return nil, nil, -1, err
	}
	defer release()
	defer sess.Close()
	var so, se capBuffer
	sess.Stdout, sess.Stderr = &so, &se
	if err := sess.Start(cmd); err != nil {
		return nil, nil, -1, err
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
		return so.Bytes(), se.Bytes(), -1, ctx.Err()
	}
	exitCode = 0
	var ee *ssh.ExitError
	var em *ssh.ExitMissingError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		exitCode, err = ee.ExitStatus(), nil
	case errors.As(err, &em):
		exitCode, err = -1, nil
	default:
		exitCode = -1
	}
	return so.Bytes(), se.Bytes(), exitCode, err
}

// SFTP returns the connection's shared SFTP client, creating it on first use and again after it died.
func (c *Client) SFTP() (*sftp.Client, error) {
	c.sftpMu.Lock()
	defer c.sftpMu.Unlock()
	if c.sftp != nil {
		return c.sftp, nil
	}
	if !c.Alive() {
		return nil, errors.New("ssh connection is closed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, _, release, err := c.NewSessionContext(ctx)
	if err != nil {
		return nil, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		release()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		release()
		return nil, err
	}
	if err := sess.RequestSubsystem("sftp"); err != nil {
		sess.Close()
		release()
		return nil, fmt.Errorf("sftp subsystem unavailable: %w", err)
	}
	sc, err := sftp.NewClientPipe(stdout, stdin,
		sftp.UseConcurrentReads(true),
		sftp.UseConcurrentWrites(true),
		sftp.MaxConcurrentRequestsPerFile(64))
	if err != nil {
		sess.Close()
		release()
		return nil, fmt.Errorf("sftp: %w", err)
	}
	c.sftp, c.sftpRel = sc, release
	go func() {
		_ = sc.Wait()
		_ = sess.Close()
		c.sftpMu.Lock()
		var rel func()
		if c.sftp == sc {
			c.sftp, rel = nil, c.sftpRel
			c.sftpRel = nil
		}
		c.sftpMu.Unlock()
		if rel != nil {
			rel()
		}
	}()
	return sc, nil
}

// keepalive sends keepalive@openssh.com every interval and closes the connection after maxMiss unanswered probes
// (dead-link detection, SSH-27). RTT feeds Info().LatencyMs.
func (c *Client) keepalive(interval time.Duration, maxMiss int) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	misses := 0
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
		}
		if c.probe(max(interval, 2*time.Second)) {
			misses = 0
			continue
		}
		misses++
		if misses >= maxMiss {
			c.pool.log.Info("ssh keepalive timeout, closing connection", "host", c.Conn.Host, "misses", misses)
			c.fail(fmt.Errorf("keepalive timeout: no response from %s after %d probes", c.Conn.Host, misses))
			return
		}
	}
}

// probe sends one keepalive request and reports whether it was answered within timeout.
func (c *Client) probe(timeout time.Duration) bool {
	start := time.Now()
	res := make(chan error, 1)
	go func() {
		_, _, err := c.Client.SendRequest("keepalive@openssh.com", true, nil)
		res <- err
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-res:
		if err != nil {
			return false
		}
		c.latency.Store(max(time.Since(start).Milliseconds(), 1))
		return true
	case <-timer.C:
		return false
	case <-c.done:
		return false
	}
}
