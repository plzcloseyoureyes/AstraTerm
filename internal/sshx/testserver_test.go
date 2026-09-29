package sshx_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/server/servertest"
	"github.com/termstead/termstead/internal/sshx"
)

// ---- in-process SSH server ----------------------------------------------------------------------------------------

type serverOpts struct {
	User          string
	Password      string
	KI            bool // require keyboard-interactive with password + OTP ("123456")
	AuthorizedKey ssh.PublicKey
	HostKey       ssh.Signer
	AllowForward  bool
	MaxSessions   int
	Banner        string
	SFTPDir       string
	Ciphers       []string
}

type sshServer struct {
	t    *testing.T
	opts serverOpts
	ln   net.Listener
	Host string
	Port int
	Key  ssh.Signer
	cfg  *ssh.ServerConfig

	Keepalives atomic.Int64
	Sessions   atomic.Int64 // currently open session channels (all connections)
	Conns      atomic.Int64 // accepted SSH connections

	mu          sync.Mutex
	PtyTerm     string
	PtySize     [2]int
	Env         map[string]string
	Forwards    []string
	Agent       bool
	AgentKeys   int // keys listed through the forwarded agent
	Signals     []string
	ConnectedAt []time.Time
}

func newHostKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func startSSHServer(t *testing.T, o serverOpts) *sshServer {
	t.Helper()
	if o.User == "" {
		o.User = "test"
	}
	if o.HostKey == nil {
		o.HostKey = newHostKey(t)
	}
	s := &sshServer{t: t, opts: o, Key: o.HostKey, Env: map[string]string{}}
	cfg := &ssh.ServerConfig{
		BannerCallback: func(ssh.ConnMetadata) string { return o.Banner },
	}
	if o.Password != "" && !o.KI {
		cfg.PasswordCallback = func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == o.User && string(pw) == o.Password {
				return nil, nil
			}
			return nil, errors.New("denied")
		}
	}
	if o.KI {
		cfg.KeyboardInteractiveCallback = func(c ssh.ConnMetadata, ch ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			// An info-only round first (like "Duo push sent"), then the real questions.
			if _, err := ch("", "Welcome to the KI test", nil, nil); err != nil {
				return nil, err
			}
			ans, err := ch("2FA", "Enter your credentials", []string{"Password: ", "Verification code: "}, []bool{false, true})
			if err != nil {
				return nil, err
			}
			if c.User() == o.User && len(ans) == 2 && ans[0] == o.Password && ans[1] == "123456" {
				return nil, nil
			}
			return nil, errors.New("denied")
		}
	}
	if o.AuthorizedKey != nil {
		cfg.PublicKeyCallback = func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if c.User() == o.User && string(key.Marshal()) == string(o.AuthorizedKey.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		}
	}
	cfg.Ciphers = o.Ciphers
	cfg.AddHostKey(o.HostKey)
	s.cfg = cfg
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.ln = ln
	s.Host = "127.0.0.1"
	s.Port = ln.Addr().(*net.TCPAddr).Port
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *sshServer) Addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

func (s *sshServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(c)
	}
}

func (s *sshServer) handleConn(nc net.Conn) {
	defer nc.Close()
	sconn, chans, reqs, err := ssh.NewServerConn(nc, s.cfg)
	if err != nil {
		return
	}
	defer sconn.Close()
	s.Conns.Add(1)
	s.mu.Lock()
	s.ConnectedAt = append(s.ConnectedAt, time.Now())
	s.mu.Unlock()
	go func() {
		for r := range reqs {
			if r.Type == "keepalive@openssh.com" {
				s.Keepalives.Add(1)
			}
			if r.WantReply {
				r.Reply(false, nil) // like OpenSSH: unknown global requests fail, which still proves liveness
			}
		}
	}()
	var perConn atomic.Int64
	for nch := range chans {
		switch nch.ChannelType() {
		case "session":
			if s.opts.MaxSessions > 0 && perConn.Load() >= int64(s.opts.MaxSessions) {
				nch.Reject(ssh.ResourceShortage, "too many sessions")
				continue
			}
			perConn.Add(1)
			s.Sessions.Add(1)
			go func() {
				s.handleSession(sconn, nch)
				perConn.Add(-1)
				s.Sessions.Add(-1)
			}()
		case "direct-tcpip":
			go s.handleForward(nch)
		default:
			nch.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}
}

func (s *sshServer) handleForward(nch ssh.NewChannel) {
	if !s.opts.AllowForward {
		nch.Reject(ssh.Prohibited, "forwarding disabled")
		return
	}
	var p struct {
		Host       string
		Port       uint32
		OriginHost string
		OriginPort uint32
	}
	if err := ssh.Unmarshal(nch.ExtraData(), &p); err != nil {
		nch.Reject(ssh.ConnectionFailed, "bad payload")
		return
	}
	dest := net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port)))
	s.mu.Lock()
	s.Forwards = append(s.Forwards, dest)
	s.mu.Unlock()
	up, err := net.DialTimeout("tcp", dest, 5*time.Second)
	if err != nil {
		nch.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nch.Accept()
	if err != nil {
		up.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() { io.Copy(ch, up); ch.CloseWrite() }()
	io.Copy(up, ch)
	up.Close()
	ch.Close()
}

func (s *sshServer) handleSession(sconn *ssh.ServerConn, nch ssh.NewChannel) {
	ch, reqs, err := nch.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	done := make(chan struct{})
	started := false
	for req := range reqs {
		ok := true
		switch req.Type {
		case "pty-req":
			var p struct {
				Term       string
				Cols, Rows uint32
				W, H       uint32
				Modes      string
			}
			ssh.Unmarshal(req.Payload, &p)
			s.mu.Lock()
			s.PtyTerm, s.PtySize = p.Term, [2]int{int(p.Cols), int(p.Rows)}
			s.mu.Unlock()
		case "window-change":
			var p struct{ Cols, Rows, W, H uint32 }
			ssh.Unmarshal(req.Payload, &p)
			s.mu.Lock()
			s.PtySize = [2]int{int(p.Cols), int(p.Rows)}
			s.mu.Unlock()
		case "env":
			var p struct{ Name, Value string }
			ssh.Unmarshal(req.Payload, &p)
			s.mu.Lock()
			s.Env[p.Name] = p.Value
			s.mu.Unlock()
		case "auth-agent-req@openssh.com":
			s.mu.Lock()
			s.Agent = true
			s.mu.Unlock()
			// Use the forwarded agent like a remote ssh client would.
			go func() {
				ch, reqs, err := sconn.OpenChannel("auth-agent@openssh.com", nil)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				defer ch.Close()
				keys, err := agent.NewClient(ch).List()
				if err == nil {
					s.mu.Lock()
					s.AgentKeys = len(keys)
					s.mu.Unlock()
				}
			}()
		case "signal":
			var p struct{ Name string }
			ssh.Unmarshal(req.Payload, &p)
			s.mu.Lock()
			s.Signals = append(s.Signals, p.Name)
			s.mu.Unlock()
		case "shell":
			if !started {
				started = true
				go func() { s.runShell(ch); close(done) }()
			}
		case "exec":
			var p struct{ Cmd string }
			ssh.Unmarshal(req.Payload, &p)
			if !started {
				started = true
				go func() { s.runExec(ch, p.Cmd); close(done) }()
			}
		case "subsystem":
			var p struct{ Name string }
			ssh.Unmarshal(req.Payload, &p)
			if p.Name != "sftp" || started {
				ok = false
				break
			}
			started = true
			go func() {
				opts := []sftp.ServerOption{}
				if s.opts.SFTPDir != "" {
					opts = append(opts, sftp.WithServerWorkingDirectory(s.opts.SFTPDir))
				}
				srv, err := sftp.NewServer(ch, opts...)
				if err == nil {
					srv.Serve()
				}
				close(done)
			}()
		default:
			ok = false
		}
		if req.WantReply {
			req.Reply(ok, nil)
		}
	}
	if started {
		<-done
	}
}

func exitStatus(ch ssh.Channel, code int) {
	ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
	ch.Close()
}

// runShell is a tiny line-oriented "shell": it echoes typed characters and answers each line with "out:<line>";
// "exit N" ends the session with status N.
func (s *sshServer) runShell(ch ssh.Channel) {
	io.WriteString(ch, "welcome\r\n$ ")
	var line []byte
	buf := make([]byte, 1024)
	for {
		n, err := ch.Read(buf)
		if err != nil {
			return
		}
		for _, b := range buf[:n] {
			if b != '\r' {
				ch.Write([]byte{b})
				line = append(line, b)
				continue
			}
			cmd := string(line)
			line = line[:0]
			if code, ok := strings.CutPrefix(cmd, "exit "); ok {
				n, _ := strconv.Atoi(code)
				io.WriteString(ch, "\r\nbye\r\n")
				exitStatus(ch, n)
				return
			}
			if cmd == "flood" {
				for i := 0; i < 2000; i++ {
					fmt.Fprintf(ch, "line %05d %s\r\n", i, strings.Repeat("x", 100))
				}
			}
			fmt.Fprintf(ch, "\r\nout:%s\r\n$ ", cmd)
		}
	}
}

func (s *sshServer) runExec(ch ssh.Channel, cmd string) {
	switch cmd {
	case "echo hello":
		io.WriteString(ch, "hello\n")
		exitStatus(ch, 0)
	case "fail":
		io.WriteString(ch.Stderr(), "boom\n")
		exitStatus(ch, 3)
	case "cat":
		io.Copy(ch, ch)
		exitStatus(ch, 0)
	case "sleep":
		time.Sleep(30 * time.Second)
		exitStatus(ch, 0)
	default:
		io.WriteString(ch.Stderr(), "unknown command\n")
		exitStatus(ch, 127)
	}
}

// ---- blackhole proxy (dead link simulation) ------------------------------------------------------------------------

type blackhole struct {
	ln     net.Listener
	target string
	frozen atomic.Bool
	Port   int
}

func startBlackhole(t *testing.T, target string) *blackhole {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &blackhole{ln: ln, target: target, Port: ln.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			pipe := func(dst, src net.Conn) {
				buf := make([]byte, 32<<10)
				for {
					n, err := src.Read(buf)
					if err != nil {
						dst.Close()
						return
					}
					if b.frozen.Load() {
						continue // swallow: the link is "dead"
					}
					dst.Write(buf[:n])
				}
			}
			go pipe(up, c)
			go pipe(c, up)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return b
}

// ---- app harness with an interactive events client ----------------------------------------------------------------

type promptAnswer func(p model.Prompt) model.PromptResponse

type appEnv struct {
	*servertest.Env
	admin   *servertest.Client
	user    *model.User
	pool    *sshx.Pool
	mu      sync.Mutex
	prompts []model.Prompt
	answer  promptAnswer
}

func newAppEnv(t *testing.T) *appEnv {
	t.Helper()
	env := servertest.New(t)
	admin := env.Setup("admin", "correct horse battery staple")
	u, err := env.Server.Deps.Store.Users.GetByUsername(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	a := &appEnv{Env: env, admin: admin, user: u, pool: env.Server.Core.SSH}
	a.pool.IdleTTL = 300 * time.Millisecond
	a.answer = func(model.Prompt) model.PromptResponse { return model.PromptResponse{} }
	a.connectEvents(t)
	return a
}

func (a *appEnv) setAnswer(fn promptAnswer) {
	a.mu.Lock()
	a.answer = fn
	a.prompts = nil
	a.mu.Unlock()
}

func (a *appEnv) seen() []model.Prompt {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]model.Prompt(nil), a.prompts...)
}

func (a *appEnv) kinds() string {
	var k []string
	for _, p := range a.seen() {
		k = append(k, p.Kind)
	}
	return strings.Join(k, ",")
}

// connectEvents opens /ws/events as the admin and answers prompts with a.answer.
func (a *appEnv) connectEvents(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := strings.Replace(a.HTTP.URL, "http", "ws", 1) + "/ws/events"
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: a.admin.HTTP})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	hello := make(chan struct{})
	go func() {
		var once sync.Once
		for {
			_, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var ev struct {
				Type   string       `json:"type"`
				Prompt model.Prompt `json:"prompt"`
			}
			if json.Unmarshal(data, &ev) != nil {
				continue
			}
			switch ev.Type {
			case "hello":
				once.Do(func() { close(hello) })
			case "prompt":
				a.mu.Lock()
				a.prompts = append(a.prompts, ev.Prompt)
				fn := a.answer
				a.mu.Unlock()
				resp := fn(ev.Prompt)
				out, _ := json.Marshal(map[string]any{"type": "prompt.response", "id": ev.Prompt.ID,
					"accept": resp.Accept, "values": resp.Values, "save": resp.Save})
				ws.Write(context.Background(), websocket.MessageText, out)
			}
		}
	}()
	select {
	case <-hello:
	case <-time.After(5 * time.Second):
		t.Fatal("no hello on the events socket")
	}
}

// accept answers host-key prompts with accept(+save) and password-like prompts with values.
func accept(save bool, values map[string][]string) promptAnswer {
	return func(p model.Prompt) model.PromptResponse {
		if p.Kind == model.PromptHostKey {
			return model.PromptResponse{Accept: true, Save: save}
		}
		if v, ok := values[p.Kind]; ok {
			return model.PromptResponse{Accept: true, Values: v, Save: save}
		}
		return model.PromptResponse{Accept: false}
	}
}

func (a *appEnv) createConnection(t *testing.T, body map[string]any) model.Connection {
	t.Helper()
	var c model.Connection
	a.admin.MustJSON("POST", "/api/connections", body, &c)
	return c
}

func quickConn(s *sshServer, opts model.Options) *model.Connection {
	if opts == nil {
		opts = model.Options{}
	}
	return &model.Connection{Protocol: model.ProtoSSH, Host: s.Host, Port: s.Port, Username: s.opts.User,
		AuthMethod: model.AuthAuto, Options: opts}
}
