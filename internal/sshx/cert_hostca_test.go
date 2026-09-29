package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/nexterm/nexterm/internal/model"
)

type fakeMarkers struct {
	cas     []ssh.PublicKey
	revoked []ssh.PublicKey
}

func (f *fakeMarkers) HostAuthorities(string, int) []ssh.PublicKey { return f.cas }
func (f *fakeMarkers) IsRevoked(_ string, _ int, k ssh.PublicKey) bool {
	return containsKey(f.revoked, k)
}

func testSigner(t *testing.T) ssh.Signer {
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

func hostCert(t *testing.T, ca, host ssh.Signer, principals []string, before uint64) *ssh.Certificate {
	t.Helper()
	c := &ssh.Certificate{Key: host.PublicKey(), CertType: ssh.HostCert, ValidPrincipals: principals, ValidBefore: before}
	if err := c.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCheckMarkers(t *testing.T) {
	ca, host, other := testSigner(t), testSigner(t), testSigner(t)
	pool := &Pool{ctx: context.Background()}
	m := &fakeMarkers{}
	pool.SetHostKeyMarkers(m)
	t.Cleanup(func() { poolProviders.Delete(pool) })
	newChecker := func() *hostKeyChecker {
		return &hostKeyChecker{p: pool, ctx: context.Background(), host: "srv.example.com", port: 22}
	}

	// A plain key without markers: not decided here.
	key := host.PublicKey()
	if done, err := newChecker().checkMarkers(&key); done || err != nil || !sameKeyT(key, host.PublicKey()) {
		t.Fatalf("plain key: %v %v", done, err)
	}

	// A certificate from a trusted CA for the right principal is accepted.
	m.cas = []ssh.PublicKey{ca.PublicKey()}
	cert := hostCert(t, ca, host, []string{"srv.example.com"}, ssh.CertTimeInfinity)
	h := newChecker()
	key = cert
	if done, err := h.checkMarkers(&key); !done || err != nil || h.fingerprint != ssh.FingerprintSHA256(host.PublicKey()) {
		t.Fatalf("trusted cert: %v %v %q", done, err, h.fingerprint)
	}
	// Re-key with the same (or a renewed) certificate of the same key is accepted; another key is not decided here.
	renewed := hostCert(t, ca, host, []string{"srv.example.com"}, 1<<40)
	key = renewed
	if done, err := h.checkMarkers(&key); !done || err != nil {
		t.Fatalf("renewed cert at re-key: %v %v", done, err)
	}
	intruder := ssh.PublicKey(hostCert(t, ca, other, []string{"srv.example.com"}, ssh.CertTimeInfinity))
	if done, _ := h.checkMarkers(&intruder); done || !sameKeyT(intruder, other.PublicKey()) {
		t.Fatal("a different host key at re-key must fall through to the re-key check")
	}
	// Re-key presenting the certified host key without its certificate: the same key, accepted; another plain key
	// falls through to the re-key check (which refuses it).
	key = host.PublicKey()
	if done, err := h.checkMarkers(&key); !done || err != nil {
		t.Fatalf("plain host key at re-key: %v %v", done, err)
	}
	key = other.PublicKey()
	if done, _ := h.checkMarkers(&key); done {
		t.Fatal("another plain key at re-key must not be accepted here")
	}
	if err := h.check("", nil, other.PublicKey()); err == nil {
		t.Fatal("a different host key at re-key was accepted")
	}

	// Wrong principal, untrusted CA or user certificate: verified as the plain key.
	for _, c := range []*ssh.Certificate{
		hostCert(t, ca, host, []string{"other.example.com"}, ssh.CertTimeInfinity),
		hostCert(t, other, host, []string{"srv.example.com"}, ssh.CertTimeInfinity),
		hostCert(t, ca, host, []string{"srv.example.com"}, 2), // expired
	} {
		key = c
		if done, err := newChecker().checkMarkers(&key); done || err != nil || !sameKeyT(key, host.PublicKey()) {
			t.Fatalf("downgrade: %v %v %s", done, err, key.Type())
		}
	}

	// Revocation of the key, the certificate's key or its CA is fatal.
	m.revoked = []ssh.PublicKey{host.PublicKey()}
	key = host.PublicKey()
	var he *hostKeyError
	if done, err := newChecker().checkMarkers(&key); !done || !errors.As(err, &he) || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked: %v %v", done, err)
	}
	m.revoked = []ssh.PublicKey{ca.PublicKey()}
	key = cert
	if done, err := newChecker().checkMarkers(&key); !done || err == nil {
		t.Fatal("certificate signed by a revoked CA accepted")
	}
}

func TestCertAlgorithmsFirst(t *testing.T) {
	pool := &Pool{ctx: context.Background()}
	t.Cleanup(func() { poolProviders.Delete(pool) })
	h := &hostKeyChecker{p: pool, ctx: context.Background(), host: "h", port: 22}
	edPub := testSigner(t).PublicKey()
	known := []*model.KnownHost{{KeyType: ssh.KeyAlgoED25519, PublicKey: FormatKnownHostKey(edPub)}}
	base := hostKeyAlgorithms(known, model.Options{})
	if got := h.algorithms(known, model.Options{}); !slices.Equal(got, base) {
		t.Fatal("no markers: unchanged ordering expected")
	}
	pool.SetHostKeyMarkers(&fakeMarkers{cas: []ssh.PublicKey{testSigner(t).PublicKey()}})
	got := h.algorithms(known, model.Options{})
	if len(got) != len(base) || !strings.Contains(got[0], "-cert-v01@openssh.com") {
		t.Fatalf("certificates first: %v", got)
	}
	if h.algorithms(nil, model.Options{}) != nil {
		t.Fatal("library defaults (nil) must be kept: they list certificates first")
	}
}

func sameKeyT(a, b ssh.PublicKey) bool { return string(a.Marshal()) == string(b.Marshal()) }
