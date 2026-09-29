package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testSSHServer is an in-process SSH server for tunnel tests: password auth, direct-tcpip, direct-streamlocal,
// tcpip-forward / streamlocal-forward (remote forwarding) and a scripted exec.
type testSSHServer struct {
	t        *testing.T
	ln       net.Listener
	cfg      *ssh.ServerConfig
	HostKey  ssh.Signer
	Host     string
	Port     int
	User     string
	Password string

	// Exec answers exec requests (command → stdout, exit code); nil = exit 0 without output.
	Exec func(cmd string) (string, int)
	// DenyForward rejects tcpip-forward / streamlocal-forward requests.
	DenyForward atomic.Bool

	Conns atomic.Int64

	mu    sync.Mutex
	conns map[*ssh.ServerConn]struct{}
	lns   map[string]net.Listener // remote listeners by "addr:port" / socket path
	dials []string
}

func newTestSSHServer(t *testing.T) *testSSHServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	s := &testSSHServer{t: t, HostKey: signer, User: "test", Password: "secret",
		conns: map[*ssh.ServerConn]struct{}{}, lns: map[string]net.Listener{}}
	s.cfg = &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == s.User && string(pw) == s.Password {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	s.cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.ln = ln
	s.Host = "127.0.0.1"
	s.Port = ln.Addr().(*net.TCPAddr).Port
	go s.serve()
	t.Cleanup(s.Close)
	return s
}

func (s *testSSHServer) Close() {
	s.ln.Close()
	s.KillConnections()
}

// KillConnections drops every SSH connection abruptly (simulates a network failure).
func (s *testSSHServer) KillConnections() {
	s.mu.Lock()
	conns := make([]*ssh.ServerConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	lns := s.lns
	s.lns = map[string]net.Listener{}
	s.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
	for _, l := range lns {
		l.Close()
	}
}

func (s *testSSHServer) Dials() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.dials...)
}

func (s *testSSHServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *testSSHServer) handle(nc net.Conn) {
	defer nc.Close()
	sc, chans, reqs, err := ssh.NewServerConn(nc, s.cfg)
	if err != nil {
		return
	}
	s.Conns.Add(1)
	s.mu.Lock()
	s.conns[sc] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, sc)
		s.mu.Unlock()
		sc.Close()
	}()
	owned := map[string]net.Listener{}
	var ownedMu sync.Mutex
	defer func() {
		ownedMu.Lock()
		for k, l := range owned {
			l.Close()
			s.mu.Lock()
			delete(s.lns, k)
			s.mu.Unlock()
		}
		ownedMu.Unlock()
	}()
	go func() {
		for r := range reqs {
			ok, reply := s.globalRequest(sc, r, owned, &ownedMu)
			if r.WantReply {
				r.Reply(ok, reply)
			}
		}
	}()
	for nch := range chans {
		switch nch.ChannelType() {
		case "direct-tcpip":
			go s.directTCPIP(nch)
		case "direct-streamlocal@openssh.com":
			go s.directStreamLocal(nch)
		case "session":
			go s.session(nch)
		default:
			nch.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}
}

func (s *testSSHServer) globalRequest(sc *ssh.ServerConn, r *ssh.Request, owned map[string]net.Listener, ownedMu *sync.Mutex) (bool, []byte) {
	switch r.Type {
	case "keepalive@openssh.com":
		return true, nil
	case "tcpip-forward":
		if s.DenyForward.Load() {
			return false, nil
		}
		var p struct {
			Addr string
			Port uint32
		}
		if ssh.Unmarshal(r.Payload, &p) != nil {
			return false, nil
		}
		bind := "127.0.0.1"
		ln, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(int(p.Port))))
		if err != nil {
			return false, nil
		}
		port := ln.Addr().(*net.TCPAddr).Port
		key := net.JoinHostPort(p.Addr, strconv.Itoa(port))
		ownedMu.Lock()
		owned[key] = ln
		ownedMu.Unlock()
		s.mu.Lock()
		s.lns[key] = ln
		s.mu.Unlock()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					ra := c.RemoteAddr().(*net.TCPAddr)
					payload := ssh.Marshal(struct {
						Addr       string
						Port       uint32
						OriginAddr string
						OriginPort uint32
					}{p.Addr, uint32(port), ra.IP.String(), uint32(ra.Port)})
					ch, reqs, err := sc.OpenChannel("forwarded-tcpip", payload)
					if err != nil {
						c.Close()
						return
					}
					go ssh.DiscardRequests(reqs)
					pipe(ch, c)
				}()
			}
		}()
		if p.Port == 0 {
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], uint32(port))
			return true, b[:]
		}
		return true, nil
	case "cancel-tcpip-forward":
		var p struct {
			Addr string
			Port uint32
		}
		if ssh.Unmarshal(r.Payload, &p) != nil {
			return false, nil
		}
		key := net.JoinHostPort(p.Addr, strconv.Itoa(int(p.Port)))
		ownedMu.Lock()
		ln := owned[key]
		delete(owned, key)
		ownedMu.Unlock()
		if ln == nil {
			return false, nil
		}
		ln.Close()
		s.mu.Lock()
		delete(s.lns, key)
		s.mu.Unlock()
		return true, nil
	case "streamlocal-forward@openssh.com":
		if s.DenyForward.Load() {
			return false, nil
		}
		var p struct{ Path string }
		if ssh.Unmarshal(r.Payload, &p) != nil {
			return false, nil
		}
		ln, err := net.Listen("unix", p.Path)
		if err != nil {
			return false, nil
		}
		ownedMu.Lock()
		owned[p.Path] = ln
		ownedMu.Unlock()
		s.mu.Lock()
		s.lns[p.Path] = ln
		s.mu.Unlock()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					payload := ssh.Marshal(struct {
						Path     string
						Reserved string
					}{p.Path, ""})
					ch, reqs, err := sc.OpenChannel("forwarded-streamlocal@openssh.com", payload)
					if err != nil {
						c.Close()
						return
					}
					go ssh.DiscardRequests(reqs)
					pipe(ch, c)
				}()
			}
		}()
		return true, nil
	case "cancel-streamlocal-forward@openssh.com":
		var p struct{ Path string }
		if ssh.Unmarshal(r.Payload, &p) != nil {
			return false, nil
		}
		ownedMu.Lock()
		ln := owned[p.Path]
		delete(owned, p.Path)
		ownedMu.Unlock()
		if ln != nil {
			ln.Close()
		}
		return ln != nil, nil
	}
	return false, nil
}

func (s *testSSHServer) directTCPIP(nch ssh.NewChannel) {
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
	s.dials = append(s.dials, dest)
	s.mu.Unlock()
	up, err := net.DialTimeout("tcp", dest, 5*time.Second)
	if err != nil {
		nch.Reject(ssh.ConnectionFailed, "Connection refused")
		return
	}
	ch, reqs, err := nch.Accept()
	if err != nil {
		up.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	pipe(ch, up)
}

func (s *testSSHServer) directStreamLocal(nch ssh.NewChannel) {
	var p struct {
		Path      string
		Reserved0 string
		Reserved1 uint32
	}
	if err := ssh.Unmarshal(nch.ExtraData(), &p); err != nil {
		nch.Reject(ssh.ConnectionFailed, "bad payload")
		return
	}
	s.mu.Lock()
	s.dials = append(s.dials, "unix:"+p.Path)
	s.mu.Unlock()
	up, err := net.DialTimeout("unix", p.Path, 5*time.Second)
	if err != nil {
		nch.Reject(ssh.ConnectionFailed, "Connection refused")
		return
	}
	ch, reqs, err := nch.Accept()
	if err != nil {
		up.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	pipe(ch, up)
}

func (s *testSSHServer) session(nch ssh.NewChannel) {
	ch, reqs, err := nch.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	for r := range reqs {
		switch r.Type {
		case "exec":
			var p struct{ Cmd string }
			ssh.Unmarshal(r.Payload, &p)
			r.Reply(true, nil)
			out, code := "", 0
			if s.Exec != nil {
				out, code = s.Exec(p.Cmd)
			}
			io.WriteString(ch, out)
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
			return
		case "shell", "pty-req", "env":
			// A minimal interactive shell: echo input back.
			if r.WantReply {
				r.Reply(true, nil)
			}
			if r.Type == "shell" {
				go io.Copy(ch, ch)
			}
		default:
			if r.WantReply {
				r.Reply(false, nil)
			}
		}
	}
}

// pipe copies both ways between an SSH channel and a connection, closing both at the end.
func pipe(ch ssh.Channel, c net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(ch, c)
		ch.CloseWrite()
		done <- struct{}{}
	}()
	go func() {
		io.Copy(c, ch)
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
	ch.Close()
	c.Close()
}

// clientConfig returns an x/crypto client config for direct tests.
func (s *testSSHServer) clientConfig() *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            s.User,
		Auth:            []ssh.AuthMethod{ssh.Password(s.Password)},
		HostKeyCallback: ssh.FixedHostKey(s.HostKey.PublicKey()),
		Timeout:         5 * time.Second,
	}
}

func (s *testSSHServer) addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

// hasListener reports whether the server currently holds a remote listener whose key contains substr.
func (s *testSSHServer) hasListener(substr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.lns {
		if strings.Contains(k, substr) {
			return true
		}
	}
	return false
}
