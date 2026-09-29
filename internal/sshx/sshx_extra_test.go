package sshx_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/term"
)

func TestHostAgentAuthAndForwarding(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix agent socket")
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	kr := agent.NewKeyring()
	if err := kr.Add(agent.AddedKey{PrivateKey: priv, Comment: "agent key"}); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "nxa") // short path: unix socket names are limited to ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", dir+"/agent.sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { agent.ServeAgent(kr, c); c.Close() }()
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", dir+"/agent.sock")

	a := newAppEnv(t)
	srv := startSSHServer(t, serverOpts{AuthorizedKey: signer.PublicKey()})
	a.setAnswer(accept(true, nil))
	s, err := a.Server.Core.Sessions.Create(context.Background(), a.user, term.CreateRequest{
		Connection: &model.Connection{Protocol: model.ProtoSSH, Host: srv.Host, Port: srv.Port, Username: "test",
			AuthMethod: model.AuthAgent, Options: model.Options{"agentForwarding": true}},
		Cols: 80, Rows: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Server.Core.Sessions.Close(s.ID)
	waitFor(t, func() bool { st, _ := s.State(); return st == model.StateConnected }, "agent-authenticated session")
	waitFor(t, func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return srv.Agent && srv.AgentKeys == 1
	}, "forwarded agent listing the key")
}

func TestPortKnockAndConnectTimeout(t *testing.T) {
	a := newAppEnv(t)
	srv := startSSHServer(t, serverOpts{Password: "pw"})
	a.setAnswer(accept(true, nil))
	knockLn, _ := net.Listen("tcp", "127.0.0.1:0")
	defer knockLn.Close()
	var knockedAt atomic.Int64
	go func() {
		for {
			c, err := knockLn.Accept()
			if err != nil {
				return
			}
			knockedAt.Store(time.Now().UnixNano())
			c.Close()
		}
	}()
	conn := quickConn(srv, model.Options{"portKnock": []any{map[string]any{"port": knockLn.Addr().(*net.TCPAddr).Port, "proto": "tcp"}}})
	_, release, err := a.pool.Acquire(ctx(t), a.user, conn, map[string]string{"password": "pw"})
	if err != nil {
		t.Fatal(err)
	}
	release()
	srv.mu.Lock()
	connected := srv.ConnectedAt[0]
	srv.mu.Unlock()
	if k := knockedAt.Load(); k == 0 || k > connected.UnixNano() {
		t.Fatalf("knock at %d, ssh at %d", k, connected.UnixNano())
	}

	// A server that accepts TCP but never speaks SSH times out after connectTimeoutSec.
	mute, _ := net.Listen("tcp", "127.0.0.1:0")
	defer mute.Close()
	go func() {
		for {
			c, err := mute.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	start := time.Now()
	_, _, err = a.pool.Acquire(ctx(t), a.user, &model.Connection{Protocol: model.ProtoSSH, Host: "127.0.0.1",
		Port: mute.Addr().(*net.TCPAddr).Port, Username: "test", Options: model.Options{"connectTimeoutSec": 1}}, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout: %v after %s", err, time.Since(start))
	}
}

func TestRemoteCommandWithoutPTYAndUsernamePrompt(t *testing.T) {
	a := newAppEnv(t)
	srv := startSSHServer(t, serverOpts{Password: "pw"})
	a.setAnswer(func(p model.Prompt) model.PromptResponse {
		switch {
		case p.Kind == model.PromptHostKey:
			return model.PromptResponse{Accept: true}
		case p.Title == "Login as" && len(p.Fields) == 1 && p.Fields[0].Echo:
			return model.PromptResponse{Accept: true, Values: []string{"test"}}
		}
		return model.PromptResponse{}
	})
	s, err := a.Server.Core.Sessions.Create(context.Background(), a.user, term.CreateRequest{
		Connection: &model.Connection{Protocol: model.ProtoSSH, Host: srv.Host, Port: srv.Port,
			Options: model.Options{"remoteCommand": "fail", "noPty": true}},
		Secrets: map[string]string{"password": "pw"},
		Cols:    80, Rows: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Server.Core.Sessions.Close(s.ID)
	waitFor(t, func() bool { st, _ := s.State(); return st == model.StateDisconnected }, "command finished")
	info := s.Info()
	if info.ExitCode == nil || *info.ExitCode != 3 {
		t.Fatalf("exit code %v", info.ExitCode)
	}
	if out := string(s.Scrollback()); !strings.Contains(out, "\x1b[31mboom\r\n\x1b[0m") {
		t.Fatalf("stderr not shown in red with CRLF: %q", out)
	}
	if got := a.kinds(); got != "keyboard-interactive,hostkey" {
		t.Fatalf("prompts %q", got)
	}
	// The typed user name is remembered for reconnects.
	a.setAnswer(accept(false, nil))
	if err := a.Server.Core.Sessions.Reconnect(s.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { st, _ := s.State(); return st == model.StateDisconnected }, "second run")
	if strings.Contains(a.kinds(), "keyboard-interactive") {
		t.Fatalf("user name asked again: %q", a.kinds())
	}
}

func TestSignalsOverSSH(t *testing.T) {
	a := newAppEnv(t)
	srv := startSSHServer(t, serverOpts{Password: "pw"})
	a.setAnswer(accept(true, nil))
	s, err := a.Server.Core.Sessions.Create(context.Background(), a.user, term.CreateRequest{
		Connection: &model.Connection{Protocol: model.ProtoSSH, Host: srv.Host, Port: srv.Port, Username: "test"},
		Secrets:    map[string]string{"password": "pw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Server.Core.Sessions.Close(s.ID)
	waitFor(t, func() bool { st, _ := s.State(); return st == model.StateConnected }, "connected")
	if err := s.Signal("TERM"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return len(srv.Signals) == 1 && srv.Signals[0] == "TERM"
	}, "signal")
	// INT under a PTY is delivered as ^C (the test shell echoes it).
	if err := s.Signal("INT"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(string(s.Scrollback()), "\x03") }, "^C")
	if err := s.Break(); err != nil {
		t.Fatal(err)
	}
}
