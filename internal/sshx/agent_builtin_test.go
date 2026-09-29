package sshx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/nexterm/nexterm/internal/model"
)

// builtinView emulates the keys module's agent view: unknown keys → ErrAgentKeyNotFound, optional refusal.
type builtinView struct {
	agent.ExtendedAgent
	deny bool
	exts []string
}

func newBuiltinView(t *testing.T, keys ...any) *builtinView {
	t.Helper()
	kr := agent.NewKeyring().(agent.ExtendedAgent)
	for _, k := range keys {
		if err := kr.Add(agent.AddedKey{PrivateKey: k}); err != nil {
			t.Fatal(err)
		}
	}
	return &builtinView{ExtendedAgent: kr}
}

func (v *builtinView) holds(key ssh.PublicKey) bool {
	keys, _ := v.ExtendedAgent.List()
	for _, k := range keys {
		if bytes.Equal(k.Blob, key.Marshal()) {
			return true
		}
	}
	return false
}

func (v *builtinView) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	return v.SignWithFlags(key, data, 0)
}

func (v *builtinView) SignWithFlags(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	if !v.holds(key) {
		return nil, ErrAgentKeyNotFound
	}
	if v.deny {
		return nil, errors.New("agent: signature refused")
	}
	return v.ExtendedAgent.SignWithFlags(key, data, flags)
}

// Signers sign through SignWithFlags, like the keys module's view signers (confirmations, refusals).
func (v *builtinView) Signers() ([]ssh.Signer, error) {
	keys, err := v.ExtendedAgent.List()
	if err != nil {
		return nil, err
	}
	out := make([]ssh.Signer, 0, len(keys))
	for _, k := range keys {
		pub, err := ssh.ParsePublicKey(k.Blob)
		if err != nil {
			return nil, err
		}
		out = append(out, &fakeViewSigner{v: v, pub: pub})
	}
	return out, nil
}

type fakeViewSigner struct {
	v   *builtinView
	pub ssh.PublicKey
}

func (s *fakeViewSigner) PublicKey() ssh.PublicKey { return s.pub }

func (s *fakeViewSigner) Sign(_ io.Reader, data []byte) (*ssh.Signature, error) {
	return s.v.SignWithFlags(s.pub, data, 0)
}

func (s *fakeViewSigner) SignWithAlgorithm(_ io.Reader, data []byte, algorithm string) (*ssh.Signature, error) {
	var f agent.SignatureFlags
	switch algorithm {
	case ssh.KeyAlgoRSASHA256:
		f = agent.SignatureFlagRsaSha256
	case ssh.KeyAlgoRSASHA512:
		f = agent.SignatureFlagRsaSha512
	}
	return s.v.SignWithFlags(s.pub, data, f)
}

func (v *builtinView) Extension(t string, _ []byte) ([]byte, error) {
	v.exts = append(v.exts, t)
	return nil, nil
}

type testKeys struct {
	a, b, shared, unknown       any
	aPub, bPub, sPub, unknownPk ssh.PublicKey
}

func makeTestKeys(t *testing.T) testKeys {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var k testKeys
	priv := func() any {
		_, p, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	k.a, k.b, k.shared, k.unknown = priv(), rsaKey, priv(), priv()
	for _, x := range []struct {
		priv any
		pub  *ssh.PublicKey
	}{{k.a, &k.aPub}, {k.b, &k.bPub}, {k.shared, &k.sPub}, {k.unknown, &k.unknownPk}} {
		s, err := ssh.NewSignerFromKey(x.priv)
		if err != nil {
			t.Fatal(err)
		}
		*x.pub = s.PublicKey()
	}
	return k
}

func TestMergedAgent(t *testing.T) {
	k := makeTestKeys(t)
	builtin := newBuiltinView(t, k.a, k.shared)
	hostKR := agent.NewKeyring().(agent.ExtendedAgent)
	for _, p := range []any{k.b, k.shared} {
		if err := hostKR.Add(agent.AddedKey{PrivateKey: p}); err != nil {
			t.Fatal(err)
		}
	}
	m := &mergedAgent{agents: []agent.Agent{builtin, readOnlyHost(hostKR)}}

	list, err := m.List()
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %v %d", err, len(list))
	}
	for i, want := range []ssh.PublicKey{k.aPub, k.sPub, k.bPub} { // built-in first, duplicates once
		if !bytes.Equal(list[i].Blob, want.Marshal()) {
			t.Fatalf("list order %d", i)
		}
	}
	if ss, err := m.Signers(); err != nil || len(ss) != 3 {
		t.Fatalf("signers: %v %d", err, len(ss))
	}

	data := []byte("session data")
	sig, err := m.Sign(k.aPub, data)
	if err != nil || k.aPub.Verify(data, sig) != nil {
		t.Fatalf("built-in key: %v", err)
	}
	// The host agent keeps its signature flags through the read-only wrapper (rsa-sha2-512 for RSA keys).
	sig, err = m.SignWithFlags(k.bPub, data, agent.SignatureFlagRsaSha512)
	if err != nil || sig.Format != ssh.KeyAlgoRSASHA512 || k.bPub.Verify(data, sig) != nil {
		t.Fatalf("host RSA key with flags: %v %v", err, sig)
	}
	if _, err := m.Sign(k.unknownPk, data); err == nil {
		t.Fatal("unknown key signed")
	}

	// A refusal of the built-in agent is final (not retried with the host agent); other keys still work.
	builtin.deny = true
	if _, err := m.Sign(k.sPub, data); err == nil || errors.Is(err, ErrAgentKeyNotFound) {
		t.Fatalf("refused signature: %v", err)
	}
	if _, err := m.Sign(k.bPub, data); err != nil {
		t.Fatalf("host key after refusal: %v", err)
	}

	// Read-only, for the merged agent and the wrapped host agent alike.
	for name, a := range map[string]agent.Agent{"merged": m, "host": readOnlyHost(hostKR), "plain": readOnlyHost(plainAgent{hostKR})} {
		if a.Add(agent.AddedKey{PrivateKey: k.unknown}) != errReadOnlyAgent || a.Remove(k.bPub) != errReadOnlyAgent ||
			a.RemoveAll() != errReadOnlyAgent || a.Lock([]byte("x")) != errReadOnlyAgent || a.Unlock([]byte("x")) != errReadOnlyAgent {
			t.Fatalf("%s agent is not read-only", name)
		}
	}
	if _, ok := readOnlyHost(plainAgent{hostKR}).(agent.ExtendedAgent); ok {
		t.Fatal("a plain host agent must not become an ExtendedAgent")
	}
	if l, _ := hostKR.List(); len(l) != 2 {
		t.Fatal("host agent modified")
	}

	// Extensions reach every agent; one success is enough.
	if _, err := m.Extension("session-bind@openssh.com", []byte("x")); err != nil || len(builtin.exts) != 1 {
		t.Fatalf("extension: %v %v", err, builtin.exts)
	}
	if _, err := (&mergedAgent{agents: []agent.Agent{hostKR}}).Extension("x@example.com", nil); !errors.Is(err, agent.ErrExtensionUnsupported) {
		t.Fatalf("unsupported extension: %v", err)
	}
}

func TestMergedAgentPartialFailure(t *testing.T) {
	k := makeTestKeys(t)
	good := newBuiltinView(t, k.a)
	broken := failingAgent{}
	if l, err := (&mergedAgent{agents: []agent.Agent{broken, good}}).List(); err != nil || len(l) != 1 {
		t.Fatalf("partial list: %v %d", err, len(l))
	}
	if _, err := (&mergedAgent{agents: []agent.Agent{broken, broken}}).List(); err == nil {
		t.Fatal("all agents failed: error expected")
	}
	if _, err := (&mergedAgent{agents: []agent.Agent{broken}}).Signers(); err == nil {
		t.Fatal("all agents failed: error expected")
	}
}

func TestPauseSigner(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	dc := newDeadlineConn(c1, time.Minute)
	paused := func() int {
		dc.mu.Lock()
		defer dc.mu.Unlock()
		return dc.paused
	}
	inner := testSigner(t).(ssh.AlgorithmSigner)
	probe := &probeSigner{AlgorithmSigner: inner, paused: paused}

	ps := pauseSigner(probe, dc)
	as, ok := ps.(ssh.AlgorithmSigner)
	if !ok {
		t.Fatal("an AlgorithmSigner must stay an AlgorithmSigner")
	}
	if _, err := as.SignWithAlgorithm(rand.Reader, []byte("x"), ssh.KeyAlgoED25519); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Sign(rand.Reader, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if len(probe.seen) != 2 || probe.seen[0] != 1 || probe.seen[1] != 1 || paused() != 0 {
		t.Fatalf("deadline not paused while signing / not resumed: %v, now %d", probe.seen, paused())
	}
	if _, ok := pauseSigner(plainSigner{inner}, dc).(ssh.AlgorithmSigner); ok {
		t.Fatal("a plain Signer must not become an AlgorithmSigner (x/crypto would pick SHA-2 RSA algorithms)")
	}

	kr := agent.NewKeyring().(agent.ExtendedAgent)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	if err := kr.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatal(err)
	}
	ss, err := (&pausingAgent{ExtendedAgent: kr, dc: dc}).Signers()
	if err != nil || len(ss) != 1 {
		t.Fatalf("signers: %v", err)
	}
	if _, ok := ss[0].(*pausingAlgorithmSigner); !ok {
		t.Fatalf("pausing agent signer type %T", ss[0])
	}
}

type fakeBuiltin struct {
	view    agent.ExtendedAgent
	keyring agent.Agent
	ctxs    []context.Context
}

func (f *fakeBuiltin) Agent(ctx context.Context, _ *model.User, _ *model.Connection, _ bool) agent.ExtendedAgent {
	f.ctxs = append(f.ctxs, ctx)
	return f.view
}

func (f *fakeBuiltin) Keyring(ctx context.Context, _ *model.User, _ *model.Connection) agent.Agent {
	f.ctxs = append(f.ctxs, ctx)
	return f.keyring
}

func TestProvidersAndFallbackKeyring(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pool := &Pool{ctx: ctx}
	kr := agent.NewKeyring()
	fb := &fakeBuiltin{keyring: kr}
	m := &fakeMarkers{}
	pool.SetHostKeyMarkers(m)
	pool.SetBuiltinAgent(fb)
	if pr := pool.provider(); pr.markers != m || pr.agent != fb {
		t.Fatal("providers not kept side by side")
	}
	pool.SetHostKeyMarkers(nil)
	if pr := pool.provider(); pr.markers != nil || pr.agent != fb {
		t.Fatal("unregistering the markers dropped the agent")
	}
	var nilPool *Pool
	nilPool.SetBuiltinAgent(fb) // no panic
	if nilPool.provider().agent != nil {
		t.Fatal("nil pool provider")
	}

	c := &Client{pool: pool, done: make(chan struct{})}
	got, release := c.fallbackKeyring()
	if got != kr {
		t.Fatal("the built-in keyring must replace the static one")
	}
	kctx := fb.ctxs[len(fb.ctxs)-1]
	close(c.done) // the connection ends: prompts of the forwarded keyring are withdrawn
	select {
	case <-kctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("keyring context not canceled when the connection ended")
	}
	release()

	fb.keyring = nil
	c2 := &Client{pool: pool, done: make(chan struct{})}
	got, release = c2.fallbackKeyring()
	defer release()
	if _, ok := got.(readOnlyAgent); !ok {
		t.Fatalf("static keyring expected, got %T", got)
	}
	if ctx := fb.ctxs[len(fb.ctxs)-1]; ctx.Err() == nil {
		t.Fatal("unused keyring context not released")
	}

	cancel() // the pool ends: its registrations are dropped
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := poolProviders.Load(pool); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("providers of an ended pool not dropped")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---- helpers ----

type probeSigner struct {
	ssh.AlgorithmSigner
	paused func() int
	seen   []int
}

func (p *probeSigner) Sign(r io.Reader, data []byte) (*ssh.Signature, error) {
	p.seen = append(p.seen, p.paused())
	return p.AlgorithmSigner.Sign(r, data)
}

func (p *probeSigner) SignWithAlgorithm(r io.Reader, data []byte, algorithm string) (*ssh.Signature, error) {
	p.seen = append(p.seen, p.paused())
	return p.AlgorithmSigner.SignWithAlgorithm(r, data, algorithm)
}

type plainSigner struct{ s ssh.Signer }

func (p plainSigner) PublicKey() ssh.PublicKey { return p.s.PublicKey() }
func (p plainSigner) Sign(r io.Reader, data []byte) (*ssh.Signature, error) {
	return p.s.Sign(r, data)
}

// plainAgent hides the ExtendedAgent methods of an agent.
type plainAgent struct{ a agent.Agent }

func (p plainAgent) List() ([]*agent.Key, error)                            { return p.a.List() }
func (p plainAgent) Sign(k ssh.PublicKey, d []byte) (*ssh.Signature, error) { return p.a.Sign(k, d) }
func (p plainAgent) Add(k agent.AddedKey) error                             { return p.a.Add(k) }
func (p plainAgent) Remove(k ssh.PublicKey) error                           { return p.a.Remove(k) }
func (p plainAgent) RemoveAll() error                                       { return p.a.RemoveAll() }
func (p plainAgent) Lock(pp []byte) error                                   { return p.a.Lock(pp) }
func (p plainAgent) Unlock(pp []byte) error                                 { return p.a.Unlock(pp) }
func (p plainAgent) Signers() ([]ssh.Signer, error)                         { return p.a.Signers() }

type failingAgent struct{ plainAgent }

var errBroken = errors.New("agent: broken pipe")

func (failingAgent) List() ([]*agent.Key, error)    { return nil, errBroken }
func (failingAgent) Signers() ([]ssh.Signer, error) { return nil, errBroken }
