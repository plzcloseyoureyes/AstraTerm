package servers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	charmssh "charm.land/ssh"
	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

// sshService is the SSH / SFTP server (SRV-5): SFTP jailed to the root folder for every user; interactive shells
// and remote commands only when enabled (they run as the Termstead OS user). Port forwarding, agent and X11
// forwarding are refused.
type sshService struct {
	m       *Manager
	in      *instance
	cfg     *SFTPConfig
	db      *userDB
	signers []gossh.Signer
	srv     *charmssh.Server
	raw     net.Listener
	files   openFiles
	wg      sync.WaitGroup
}

type sshCtxKey string

const (
	ctxClient  sshCtxKey = "termstead.client"
	ctxMethod  sshCtxKey = "termstead.authmethod"
	ctxAnnounc sshCtxKey = "termstead.announced"
)

func newSSHService(ctx context.Context, m *Manager, in *instance, cfg *SFTPConfig) (service, error) {
	if len(cfg.Users) == 0 {
		return nil, invalidf("add at least one user")
	}
	signers, err := m.secrets.hostKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("SSH host keys: %w", err)
	}
	in.fp = hostKeyFingerprint(signers)
	return &sshService{m: m, in: in, cfg: cfg, db: newUserDB(cfg.Users, &in.stats), signers: signers}, nil
}

func (s *sshService) start() error {
	raw, err := net.Listen("tcp", hostPort(s.cfg.BindAddress, s.cfg.Port))
	if err != nil {
		return err
	}
	s.raw = raw
	s.in.addrs = []string{raw.Addr().String()}
	s.in.url = serverURL("sftp", s.cfg.BindAddress, raw.Addr().(*net.TCPAddr).Port, "/")
	hostSigners := make([]charmssh.Signer, 0, len(s.signers))
	for _, sg := range s.signers {
		hostSigners = append(hostSigners, sg)
	}
	s.srv = &charmssh.Server{
		HostSigners:                hostSigners,
		Version:                    "Termstead",
		PasswordHandler:            s.password,
		PublicKeyHandler:           s.publicKey,
		KeyboardInteractiveHandler: s.keyboardInteractive,
		ConnCallback:               s.connected,
		ConnectionFailedCallback:   s.handshakeFailed,
		// Sessions are served by our own channel handler (PTY via xpty, raw channel I/O); no other channel types
		// (direct-tcpip, forwarded-tcpip, x11) and no global requests (tcpip-forward) are accepted.
		ChannelHandlers:   map[string]charmssh.ChannelHandler{"session": s.sessionChannel},
		RequestHandlers:   map[string]charmssh.RequestHandler{},
		SubsystemHandlers: map[string]charmssh.SubsystemHandler{},
		HandshakeTimeout:  30 * time.Second,
		IdleTimeout:       time.Duration(s.cfg.IdleTimeoutSec) * time.Second,
		ServerConfigCallback: func(charmssh.Context) *gossh.ServerConfig {
			return &gossh.ServerConfig{MaxAuthTries: 6}
		},
	}
	ln := &trackingListener{Listener: raw, set: s.in.clients}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, charmssh.ErrServerClosed) && s.in.ctx.Err() == nil {
			s.in.failed(err)
		}
	}()
	return nil
}

func (s *sshService) stop() {
	if s.srv != nil {
		_ = s.srv.Close()
	} else if s.raw != nil {
		_ = s.raw.Close()
	}
	s.files.closeAll()
	s.in.clients.closeAll()
	s.wg.Wait()
}

// ---- connection / authentication ----------------------------------------------------------------------------------

func (s *sshService) connected(ctx charmssh.Context, conn net.Conn) net.Conn {
	cl := clientOf(conn)
	if cl != nil {
		ctx.SetValue(ctxClient, cl)
	}
	if s.db.lim.blocked(remoteIP(conn.RemoteAddr().String())) {
		s.in.logf(levelWarn, conn.RemoteAddr().String(), "", "Connection refused: too many failed logins")
		return nil
	}
	s.in.logf(levelDebug, conn.RemoteAddr().String(), "", "Connected")
	return conn
}

func (s *sshService) handshakeFailed(conn net.Conn, err error) {
	if errors.Is(err, io.EOF) {
		return
	}
	s.in.logf(levelDebug, conn.RemoteAddr().String(), "", "Handshake failed: %v", err)
}

func ctxClientOf(ctx charmssh.Context) *client {
	cl, _ := ctx.Value(ctxClient).(*client)
	return cl
}

func (s *sshService) password(ctx charmssh.Context, password string) bool {
	addr := ctx.RemoteAddr().String()
	if _, err := s.db.checkPassword(remoteIP(addr), ctx.User(), password); err != nil {
		s.in.logf(levelWarn, addr, ctx.User(), "Password authentication failed: %v", err)
		return false
	}
	ctx.SetValue(ctxMethod, "password")
	return true
}

func (s *sshService) publicKey(ctx charmssh.Context, key charmssh.PublicKey) bool {
	addr := ctx.RemoteAddr().String()
	if _, err := s.db.checkKey(remoteIP(addr), ctx.User(), key); err != nil {
		s.in.logf(levelDebug, addr, ctx.User(), "Public key %s rejected", gossh.FingerprintSHA256(key))
		return false
	}
	ctx.SetValue(ctxMethod, "public key "+gossh.FingerprintSHA256(key))
	return true
}

func (s *sshService) keyboardInteractive(ctx charmssh.Context, challenge gossh.KeyboardInteractiveChallenge) bool {
	answers, err := challenge("", "", []string{"Password: "}, []bool{false})
	if err != nil || len(answers) != 1 {
		return false
	}
	return s.password(ctx, answers[0])
}

// announce logs a successful login once per connection and records the user on the client.
func (s *sshService) announce(ctx charmssh.Context) {
	ctx.Lock()
	done, _ := ctx.Value(ctxAnnounc).(bool)
	if !done {
		ctx.SetValue(ctxAnnounc, true)
	}
	ctx.Unlock()
	if done {
		return
	}
	if cl := ctxClientOf(ctx); cl != nil {
		cl.setUser(ctx.User())
	}
	method, _ := ctx.Value(ctxMethod).(string)
	if method == "" {
		method = "authenticated"
	}
	s.in.logf(levelInfo, ctx.RemoteAddr().String(), ctx.User(), "Logged in (%s, %s)", method, ctx.ClientVersion())
}

func (s *sshService) readOnlyFor(user string) bool {
	if s.cfg.ReadOnly {
		return true
	}
	u := s.db.lookup(user)
	return u == nil || u.ReadOnly
}

// ---- sessions -----------------------------------------------------------------------------------------------------

// ptyRequest is the payload of "pty-req" (RFC 4254 §6.2).
type ptyRequest struct {
	Term                         string
	Columns, Rows, Width, Height uint32
	Modes                        string
}

// sshSession is one "session" channel.
type sshSession struct {
	s      *sshService
	ch     gossh.Channel
	ctx    charmssh.Context
	user   string
	addr   string
	local  string
	resize chan [2]int
	gone   chan struct{} // closed when the client closed the channel

	mu      sync.Mutex
	started bool
	pty     *ptyRequest
	env     []string
}

func (s *sshService) sessionChannel(_ *charmssh.Server, _ *gossh.ServerConn, newChan gossh.NewChannel, ctx charmssh.Context) {
	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}
	ss := &sshSession{s: s, ch: ch, ctx: ctx, user: ctx.User(), addr: ctx.RemoteAddr().String(),
		local: ctx.LocalAddr().String(), resize: make(chan [2]int, 8), gone: make(chan struct{})}
	ss.serve(reqs)
}

// begin marks the session as started (one shell / exec / subsystem per channel).
func (ss *sshSession) begin() bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.started {
		return false
	}
	ss.started = true
	return true
}

func (ss *sshSession) serve(reqs <-chan *gossh.Request) {
	defer close(ss.gone)
	for req := range reqs {
		ok := false
		var run func()
		switch req.Type {
		case "pty-req":
			var p ptyRequest
			if gossh.Unmarshal(req.Payload, &p) == nil {
				ss.mu.Lock()
				if !ss.started && ss.pty == nil {
					ss.pty, ok = &p, true
				}
				ss.mu.Unlock()
			}
		case "window-change":
			var w struct{ Columns, Rows, Width, Height uint32 }
			if gossh.Unmarshal(req.Payload, &w) == nil {
				ok = true
				select {
				case ss.resize <- [2]int{int(w.Columns), int(w.Rows)}:
				default:
				}
			}
		case "env":
			var kv struct{ Name, Value string }
			if gossh.Unmarshal(req.Payload, &kv) == nil && acceptEnv(kv.Name, kv.Value) {
				ss.mu.Lock()
				if !ss.started && len(ss.env) < 32 {
					ss.env = append(ss.env, kv.Name+"="+kv.Value)
					ok = true
				}
				ss.mu.Unlock()
			}
		case "shell", "exec":
			var cmd struct{ Command string }
			if req.Type == "exec" && gossh.Unmarshal(req.Payload, &cmd) != nil {
				break
			}
			if ss.begin() {
				ok = true
				command := cmd.Command
				run = func() { ss.runShell(command) }
			}
		case "subsystem":
			var sub struct{ Name string }
			if gossh.Unmarshal(req.Payload, &sub) == nil && sub.Name == "sftp" && ss.begin() {
				ok, run = true, ss.runSFTP
			}
		}
		if req.WantReply {
			_ = req.Reply(ok, nil)
		}
		if run != nil {
			go run()
		}
	}
}

// acceptEnv keeps the locale variables a client sends (OpenSSH's default AcceptEnv LANG LC_*).
func acceptEnv(name, value string) bool {
	if name != "LANG" && !strings.HasPrefix(name, "LC_") {
		return false
	}
	return len(name) < 64 && len(value) < 256 && !strings.ContainsRune(name, '=') && !strings.ContainsRune(name+value, 0)
}

// exit reports the exit status and closes the channel.
func (ss *sshSession) exit(code int) {
	if code < 0 {
		code = 255
	}
	_, _ = ss.ch.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{uint32(code)}))
	_ = ss.ch.Close()
}

func (ss *sshSession) runShell(command string) {
	s := ss.s
	s.announce(ss.ctx)
	ss.mu.Lock()
	pty := ss.pty
	env := append([]string(nil), ss.env...)
	ss.mu.Unlock()
	nl := "\n"
	if pty != nil {
		nl = "\r\n"
	}
	if !s.cfg.Shell {
		s.in.logf(levelInfo, ss.addr, ss.user, "Shell / command refused (SFTP only)")
		_, _ = io.WriteString(ss.ch.Stderr(), "This service allows SFTP connections only."+nl)
		ss.exit(1)
		return
	}
	if rh, rp, err := net.SplitHostPort(ss.addr); err == nil {
		if lh, lp, err := net.SplitHostPort(ss.local); err == nil {
			env = append(env, "SSH_CONNECTION="+rh+" "+rp+" "+lh+" "+lp, "SSH_CLIENT="+rh+" "+rp+" "+lp)
		}
	}
	spec := shellSpec{command: s.cfg.ShellCommand, dir: s.cfg.Root, exec: command, env: env}
	cl := ctxClientOf(ss.ctx)
	if command == "" {
		s.in.logf(levelInfo, ss.addr, ss.user, "Shell started")
		if cl != nil {
			cl.setActivity("Shell")
		}
	} else {
		s.in.logf(levelInfo, ss.addr, ss.user, "Command: %s", truncate(command, 300))
		if cl != nil {
			cl.setActivity("Command: " + command)
		}
	}
	var code int
	if pty != nil {
		spec.term, spec.cols, spec.rows = pty.Term, int(pty.Columns), int(pty.Rows)
		code = ss.runPTY(spec)
	} else {
		code = ss.runPipes(spec)
	}
	s.in.logf(levelInfo, ss.addr, ss.user, "Session ended (exit code %d)", code)
	ss.exit(code)
}

func (ss *sshSession) runPTY(spec shellSpec) int {
	sh, err := startPTYShell(spec)
	if err != nil {
		_, _ = io.WriteString(ss.ch.Stderr(), "Cannot start the shell: "+err.Error()+"\r\n")
		return 1
	}
	defer sh.Close()
	go func() {
		for {
			select {
			case w := <-ss.resize:
				sh.Resize(w[0], w[1])
			case <-sh.Done():
				return
			case <-ss.gone:
				return
			}
		}
	}()
	go func() { _, _ = io.Copy(sh, ss.ch) }()
	out := make(chan struct{})
	go func() {
		_, _ = io.Copy(ss.ch, sh)
		close(out)
	}()
	select {
	case <-out:
	case <-ss.gone:
		return 255
	case <-ss.s.in.ctx.Done():
		return 255
	}
	select {
	case <-sh.Done():
		return sh.ExitCode()
	case <-time.After(2 * time.Second):
		return 0
	}
}

func (ss *sshSession) runPipes(spec shellSpec) int {
	cmd, err := buildShellCmd(spec)
	if err != nil {
		_, _ = io.WriteString(ss.ch.Stderr(), "Cannot start the shell: "+err.Error()+"\n")
		return 1
	}
	setProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 1
	}
	cmd.Stdout = ss.ch
	cmd.Stderr = ss.ch.Stderr()
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		_, _ = io.WriteString(ss.ch.Stderr(), "Cannot start the command: "+err.Error()+"\n")
		return 1
	}
	go func() {
		_, _ = io.Copy(stdin, ss.ch)
		_ = stdin.Close()
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return exitStatus(cmd, err)
	case <-ss.gone:
	case <-ss.s.in.ctx.Done():
	}
	killGroup(cmd)
	<-done
	return 255
}

// runSFTP serves the SFTP subsystem, jailed to the root folder.
func (ss *sshSession) runSFTP() {
	s := ss.s
	s.announce(ss.ctx)
	ro := s.readOnlyFor(ss.user)
	h := &sftpHandler{fs: s.in.root.withReadOnly(ro), s: s, cl: ctxClientOf(ss.ctx), user: ss.user, addr: ss.addr}
	if h.cl != nil {
		h.cl.setActivity("SFTP")
	}
	s.in.logf(levelInfo, ss.addr, ss.user, "SFTP session started%s", roSuffix(ro))
	rs := sftp.NewRequestServer(ss.ch, sftp.Handlers{FileGet: h, FilePut: h, FileCmd: h, FileList: h},
		sftp.WithStartDirectory("/"))
	err := rs.Serve()
	_ = rs.Close()
	if err != nil && !errors.Is(err, io.EOF) && s.in.ctx.Err() == nil {
		s.in.logf(levelDebug, ss.addr, ss.user, "SFTP session error: %v", err)
	}
	s.in.logf(levelInfo, ss.addr, ss.user, "SFTP session ended")
}
