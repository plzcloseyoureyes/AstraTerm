package sshx

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"encoding/pem"
	"slices"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

func TestConfiguredList(t *testing.T) {
	sup := []string{"a", "b", "c"}
	ins := []string{"old1", "old2"}
	cases := []struct {
		opts model.Options
		want []string
	}{
		{model.Options{}, nil},
		{model.Options{"legacyAlgorithms": true}, []string{"a", "b", "c", "old1", "old2"}},
		{model.Options{"k": []any{"c", "a", "bogus"}}, []string{"c", "a"}},
		{model.Options{"k": "b,old2"}, []string{"b", "old2"}},
		{model.Options{"k": []any{"+old1", "-b", "^c"}}, []string{"c", "a", "old1"}},
		{model.Options{"k": []any{"bogus"}}, nil},
	}
	for i, tc := range cases {
		if got := configuredList(tc.opts, "k", sup, ins); !slices.Equal(got, tc.want) {
			t.Fatalf("case %d: %v want %v", i, got, tc.want)
		}
	}
}

func TestHostKeyAlgorithmOrdering(t *testing.T) {
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	rsaPub, _ := ssh.NewPublicKey(&rk.PublicKey)
	known := []*model.KnownHost{{KeyType: rsaPub.Type(), PublicKey: FormatKnownHostKey(rsaPub)}}
	got := hostKeyAlgorithms(known, model.Options{})
	if len(got) < 3 || got[0] != ssh.KeyAlgoRSASHA512 || got[1] != ssh.KeyAlgoRSASHA256 {
		t.Fatalf("rsa-first ordering %v", got)
	}
	if slices.Contains(got, ssh.KeyAlgoRSA) {
		t.Fatal("SHA-1 ssh-rsa offered without legacyAlgorithms")
	}
	_, ek, _ := ed25519.GenerateKey(rand.Reader)
	edPub, _ := ssh.NewPublicKey(ek.Public())
	got = hostKeyAlgorithms([]*model.KnownHost{{KeyType: "ssh-ed25519", PublicKey: FormatKnownHostKey(edPub)}}, model.Options{})
	if got[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("ed25519-first ordering %v", got)
	}
	if hostKeyAlgorithms(nil, model.Options{}) != nil {
		t.Fatal("no known keys → library defaults")
	}
}

func TestX11SetupRewrite(t *testing.T) {
	f := &x11Forwarder{fake: bytes.Repeat([]byte{7}, 16), realName: []byte(x11AuthProto), realData: bytes.Repeat([]byte{9}, 16)}
	build := func(bo binary.ByteOrder, order byte, name, data []byte) []byte {
		hdr := make([]byte, 12)
		hdr[0] = order
		bo.PutUint16(hdr[2:4], 11)
		bo.PutUint16(hdr[6:8], uint16(len(name)))
		bo.PutUint16(hdr[8:10], uint16(len(data)))
		out := append(hdr, name...)
		out = append(out, make([]byte, pad4(len(name)))...)
		out = append(out, data...)
		return append(out, make([]byte, pad4(len(data)))...)
	}
	for _, v := range []struct {
		bo    binary.ByteOrder
		order byte
	}{{binary.BigEndian, 'B'}, {binary.LittleEndian, 'l'}} {
		in := build(v.bo, v.order, []byte(x11AuthProto), f.fake)
		out, err := f.rewriteSetup(bytes.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		if want := build(v.bo, v.order, f.realName, f.realData); !bytes.Equal(out, want) {
			t.Fatalf("rewritten %x want %x", out, want)
		}
		// A wrong cookie is refused.
		bad := build(v.bo, v.order, []byte(x11AuthProto), bytes.Repeat([]byte{1}, 16))
		if _, err := f.rewriteSetup(bytes.NewReader(bad)); err == nil {
			t.Fatal("wrong cookie accepted")
		}
	}
	// No real cookie: the local server gets an unauthenticated setup.
	f2 := &x11Forwarder{fake: f.fake}
	out, err := f2.rewriteSetup(bytes.NewReader(build(binary.LittleEndian, 'l', []byte(x11AuthProto), f.fake)))
	if err != nil || len(out) != 12 || out[6] != 0 || out[8] != 0 {
		t.Fatalf("no-auth rewrite %x %v", out, err)
	}
}

func TestParseDisplay(t *testing.T) {
	for in, want := range map[string]struct {
		host string
		num  int
	}{
		":0": {"", 0}, ":1.0": {"", 1}, "localhost:10.0": {"localhost", 10},
		"/private/tmp/com.apple.launchd.abc/org.xquartz:0": {"/private/tmp/com.apple.launchd.abc/org.xquartz", 0},
	} {
		h, n, err := parseDisplay(in)
		if err != nil || h != want.host || n != want.num {
			t.Fatalf("%q → %q %d %v", in, h, n, err)
		}
	}
	if _, _, err := parseDisplay("nonsense"); err == nil {
		t.Fatal("invalid display accepted")
	}
}

func TestParsePrivateKeyFormats(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(priv, "c")
	if _, err := ParsePrivateKey(pemEncode(block), ""); err != nil {
		t.Fatal(err)
	}
	enc, _ := ssh.MarshalPrivateKeyWithPassphrase(priv, "c", []byte("pw"))
	_, err := ParsePrivateKey(pemEncode(enc), "")
	np, ok := err.(*NeedsPassphraseError)
	if !ok || np.PublicKey == nil || np.Wrong {
		t.Fatalf("missing passphrase: %v", err)
	}
	if _, err := ParsePrivateKey(pemEncode(enc), "nope"); err == nil || !err.(*NeedsPassphraseError).Wrong {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if s, err := ParsePrivateKey(pemEncode(enc), "pw"); err != nil || s == nil {
		t.Fatal(err)
	}
}

func pemEncode(b *pem.Block) []byte { return pem.EncodeToMemory(b) }
