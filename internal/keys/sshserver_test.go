package keys

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/nexterm/nexterm/internal/model"
)

// An in-process SSH server for the module's integration tests: host key or host certificate, password / public key /
// user certificate authentication, SFTP rooted in a directory, "sh -s" run by a real local shell with HOME set to that
// directory (ssh-copy-id fallback), and agent forwarding (the server lists the forwarded agent's keys and can sign).

type testServerOpts struct {
	User           string
	Password       string
	AuthorizedKeys []ssh.PublicKey
	UserCA         ssh.PublicKey // trust user certificates signed by this CA
	HostKey        ssh.Signer    // may be a certificate signer
	HomeDir        string        // SFTP working directory / HOME of "sh -s" ("" = no SFTP, no shell)
	NoSFTP         bool
}

type testServer struct {
	t    *testing.T
	opts testServerOpts
	ln   net.Listener
	cfg  *ssh.ServerConfig
	Host string
	Port int

	mu           sync.Mutex
	AgentKeys    []*agent.Key
	AgentSignErr error
	AgentSigned  int
	AuthKeys     []string // fingerprints of keys that authenticated
}

func newEd25519Signer(t *testing.T) ssh.Signer {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func startTestServer(t *testing.T, o testServerOpts) *testServer {
	t.Helper()
	if o.User == "" {
		o.User = "test"
	}
	if o.HostKey == nil {
		o.HostKey = newEd25519Signer(t)
	}
	s := &testServer{t: t, opts: o}
	cfg := &ssh.ServerConfig{}
	if o.Password != "" {
		cfg.PasswordCallback = func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == o.User && string(pw) == o.Password {
				return nil, nil
			}
			return nil, errors.New("denied")
		}
	}
	checker := &ssh.CertChecker{
		IsUserAuthority: func(auth ssh.PublicKey) bool { return o.UserCA != nil && sameKey(auth, o.UserCA) },
		UserKeyFallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			for _, k := range o.AuthorizedKeys {
				if sameKey(k, key) {
					return nil, nil
				}
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.PublicKeyCallback = func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if c.User() != o.User {
			return nil, errors.New("unknown user")
		}
		p, err := checker.Authenticate(c, key)
		if err == nil {
			s.mu.Lock()
			s.AuthKeys = append(s.AuthKeys, ssh.FingerprintSHA256(key))
			s.mu.Unlock()
		}
		return p, err
	}
	cfg.AddHostKey(o.HostKey)
	s.cfg = cfg
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.ln, s.Host, s.Port = ln, "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *testServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *testServer) handle(nc net.Conn) {
	defer nc.Close()
	sc, chans, reqs, err := ssh.NewServerConn(nc, s.cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		go s.session(sc, nch)
	}
}

func (s *testServer) session(sc *ssh.ServerConn, nch ssh.NewChannel) {
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
		case "pty-req", "env", "window-change":
		case "auth-agent-req@openssh.com":
			go s.useForwardedAgent(sc)
		case "shell":
			if !started {
				started = true
				go func() {
					io.Copy(io.Discard, ch)
					close(done)
				}()
			}
		case "exec":
			var p struct{ Cmd string }
			ssh.Unmarshal(req.Payload, &p)
			if started || s.opts.HomeDir == "" || p.Cmd != "sh -s" {
				ok = false
				break
			}
			started = true
			go func() {
				s.runShell(ch)
				close(done)
			}()
		case "subsystem":
			var p struct{ Name string }
			ssh.Unmarshal(req.Payload, &p)
			if p.Name != "sftp" || started || s.opts.NoSFTP || s.opts.HomeDir == "" {
				ok = false
				break
			}
			started = true
			go func() {
				if srv, err := sftp.NewServer(ch, sftp.WithServerWorkingDirectory(s.opts.HomeDir)); err == nil {
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

func (s *testServer) runShell(ch ssh.Channel) {
	cmd := exec.Command("sh", "-s")
	cmd.Dir = s.opts.HomeDir
	cmd.Env = []string{"HOME=" + s.opts.HomeDir, "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	cmd.Stdin = ch
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, ch.Stderr()
	code := 0
	if err := cmd.Run(); err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	ch.Write(out.Bytes())
	ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
	ch.Close()
}

// useForwardedAgent acts like a remote ssh client: it lists the forwarded agent's keys and signs with the first.
func (s *testServer) useForwardedAgent(sc *ssh.ServerConn) {
	ch, reqs, err := sc.OpenChannel("auth-agent@openssh.com", nil)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	defer ch.Close()
	ac := agent.NewClient(ch)
	keys, err := ac.List()
	s.mu.Lock()
	s.AgentKeys = keys
	s.mu.Unlock()
	if err != nil || len(keys) == 0 {
		return
	}
	_, err = ac.Sign(keys[0], []byte("challenge"))
	s.mu.Lock()
	s.AgentSignErr = err
	if err == nil {
		s.AgentSigned++
	}
	s.mu.Unlock()
}

func (s *testServer) conn(owner *model.User, keyID, auth string, opts model.Options) *model.Connection {
	if opts == nil {
		opts = model.Options{}
	}
	c := &model.Connection{OwnerID: owner.ID, Name: "test server", Protocol: model.ProtoSSH, Host: s.Host, Port: s.Port,
		Username: s.opts.User, KeyID: keyID, AuthMethod: auth, Options: opts}
	c.Normalize()
	return c
}

func (s *testServer) addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}
