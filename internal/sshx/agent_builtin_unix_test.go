//go:build unix

package sshx

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// serveHostAgent runs keyring as the host agent (SSH_AUTH_SOCK) for the test.
func serveHostAgent(t *testing.T, keyring agent.Agent) {
	t.Helper()
	dir, err := os.MkdirTemp("", "nxag") // short path: unix socket paths are limited to ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = agent.ServeAgent(keyring, c)
			}()
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", sock)
}

// remoteView serves a on a pipe and returns the client end, like a server using the forwarded agent.
func remoteView(t *testing.T, a agent.Agent) agent.ExtendedAgent {
	t.Helper()
	p1, p2 := net.Pipe()
	t.Cleanup(func() { _ = p1.Close(); _ = p2.Close() })
	go func() { _ = agent.ServeAgent(a, p1) }()
	return agent.NewClient(p2)
}

func TestDialForwardAgent(t *testing.T) {
	k := makeTestKeys(t)
	hostKR := agent.NewKeyring()
	if err := hostKR.Add(agent.AddedKey{PrivateKey: k.b}); err != nil {
		t.Fatal(err)
	}
	serveHostAgent(t, hostKR)

	builtin := newBuiltinView(t, k.a)
	fb := &fakeBuiltin{view: builtin}
	pool := &Pool{ctx: context.Background()}
	t.Cleanup(func() { poolProviders.Delete(pool) })
	pool.SetBuiltinAgent(fb)
	c := &Client{pool: pool, done: make(chan struct{})}

	ag, closer, err := c.dialForwardAgent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	remote := remoteView(t, ag)
	keys, err := remote.List()
	if err != nil || len(keys) != 2 || !bytes.Equal(keys[0].Blob, k.aPub.Marshal()) {
		t.Fatalf("forwarded list: %v %d", err, len(keys))
	}
	data := []byte("forwarded")
	sig, err := remote.SignWithFlags(k.bPub, data, agent.SignatureFlagRsaSha256)
	if err != nil || sig.Format != ssh.KeyAlgoRSASHA256 || k.bPub.Verify(data, sig) != nil {
		t.Fatalf("host RSA key through the forwarded merged agent: %v", err)
	}
	if sig, err := remote.Sign(k.aPub, data); err != nil || k.aPub.Verify(data, sig) != nil {
		t.Fatalf("built-in key through the forwarded merged agent: %v", err)
	}
	if err := remote.Add(agent.AddedKey{PrivateKey: k.unknown}); err == nil {
		t.Fatal("the forwarded agent must be read-only")
	}
	if err := remote.RemoveAll(); err == nil {
		t.Fatal("the forwarded agent must be read-only")
	}
	if l, _ := hostKR.List(); len(l) != 1 {
		t.Fatal("host agent modified through forwarding")
	}
	if _, err := remote.Extension("session-bind@openssh.com", []byte("bind")); err != nil || len(builtin.exts) != 1 {
		t.Fatalf("session-bind: %v %v", err, builtin.exts)
	}
	_ = closer.Close()
	if fb.ctxs[0].Err() == nil {
		t.Fatal("closing the forwarded agent must end the built-in view's context")
	}

	// Built-in agent not running: the host agent alone.
	fb.view = nil
	ag, closer, err = c.dialForwardAgent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, merged := ag.(*mergedAgent); merged {
		t.Fatal("no built-in agent: the host agent is expected")
	}
	_ = closer.Close()
	if fb.ctxs[1].Err() == nil {
		t.Fatal("unused built-in context not released")
	}

	// No host agent: the built-in view alone.
	fb.view = builtin
	t.Setenv("SSH_AUTH_SOCK", "")
	ag, closer, err = c.dialForwardAgent(context.Background())
	if err != nil || ag != agent.Agent(builtin) {
		t.Fatalf("no host agent: built-in view expected (%v, %T)", err, ag)
	}
	_ = closer.Close()
}

func TestAuthDialAgent(t *testing.T) {
	k := makeTestKeys(t)
	hostKR := agent.NewKeyring()
	if err := hostKR.Add(agent.AddedKey{PrivateKey: k.b}); err != nil {
		t.Fatal(err)
	}
	serveHostAgent(t, hostKR)
	builtin := newBuiltinView(t, k.a)
	pool := &Pool{ctx: context.Background()}
	t.Cleanup(func() { poolProviders.Delete(pool) })
	pool.SetBuiltinAgent(&fakeBuiltin{view: builtin})

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	a := &authFlow{p: pool, ctx: context.Background(), dc: newDeadlineConn(c1, time.Minute)}
	ag, closer, err := a.dialAgent()
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	ss, err := ag.Signers()
	if err != nil || len(ss) != 2 {
		t.Fatalf("auth signers: %v %d", err, len(ss))
	}
	if _, ok := ss[0].(*pausingAlgorithmSigner); !ok || !bytes.Equal(ss[0].PublicKey().Marshal(), k.aPub.Marshal()) {
		t.Fatalf("built-in keys first, pausing the handshake deadline: %T", ss[0])
	}
	if ag.Add(agent.AddedKey{PrivateKey: k.unknown}) == nil {
		t.Fatal("the authentication agent must be read-only")
	}

	// A refused built-in signature does not fall through to the host agent.
	builtin.deny = true
	if _, err := ss[0].Sign(nil, []byte("x")); err == nil || errors.Is(err, ErrAgentKeyNotFound) {
		t.Fatalf("refusal: %v", err)
	}
}
