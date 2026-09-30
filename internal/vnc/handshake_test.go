package vnc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/vnc/anontls/anontlstest"
	"github.com/plzcloseyoureyes/astraterm/internal/vnc/vnctest"
)

func TestVNCAuthResponseKnownAnswer(t *testing.T) {
	challenge, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	got, err := vncAuthResponse("vncpassword", challenge) // only the first 8 characters count
	if err != nil {
		t.Fatal(err)
	}
	// openssl enc -des-ecb -K 6e76c60e86ceceee (bit-reversed "vncpassw") -nopad
	if want := "6a4ae12743378554212b00007b9f7d1f"; hex.EncodeToString(got) != want {
		t.Fatalf("got %x, want %s", got, want)
	}
	if !bytes.Equal(got, vnctest.DESResponse("vncpassword", challenge)) {
		t.Fatal("mismatch with the test server implementation")
	}
}

func TestParseServerVersion(t *testing.T) {
	cases := map[string]rfbVersion{
		"RFB 003.003\n": rfb33, "RFB 003.005\n": rfb33, "RFB 003.006\n": rfb33, "RFB 003.007\n": rfb37,
		"RFB 003.008\n": rfb38, "RFB 003.889\n": rfb38, "RFB 004.001\n": rfb38, "RFB 005.000\n": rfb38,
	}
	for in, want := range cases {
		v, rep, err := parseServerVersion([]byte(in))
		if err != nil || rep || v != want {
			t.Errorf("%q: got %v %v %v", in, v, rep, err)
		}
	}
	if _, rep, err := parseServerVersion([]byte("RFB 000.000\n")); err != nil || !rep {
		t.Error("repeater greeting not recognized")
	}
	for _, bad := range []string{"SSH-2.0-Open", "RFB 003.00x\n", "RFB 002.000\n", "HTTP/1.1 400"} {
		b := []byte(bad)
		if len(b) != 12 {
			b = append(b, make([]byte, 12)...)[:12]
		}
		if _, _, err := parseServerVersion(b); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// ---- test doubles -------------------------------------------------------------------------------------------------

type testCreds struct {
	user, pass string
	answers    []string // passwords returned by successive prompts
	prompts    int
	lastReq    credRequest
}

func (c *testCreds) available() (bool, bool) { return c.user != "", c.pass != "" }

func (c *testCreds) get(_ context.Context, req credRequest) (string, string, error) {
	c.lastReq = req
	if c.pass != "" && (!req.needUser || c.user != "") {
		return c.user, c.pass, nil
	}
	if c.prompts >= len(c.answers) {
		return "", "", errCanceled
	}
	c.prompts++
	c.pass = c.answers[c.prompts-1]
	return c.user, c.pass, nil
}

type testTrust struct {
	calls  int
	reject bool
	subj   string
}

func (t *testTrust) verify(_ context.Context, _ string, _ int, certs []*x509.Certificate) (string, error) {
	t.calls++
	t.subj = certs[0].Subject.CommonName
	if t.reject {
		return "", errCertRejected
	}
	return "accepted", nil
}

func startFake(t *testing.T, s *vnctest.Server) string {
	t.Helper()
	addr, stop, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return addr
}

func handshakeTo(t *testing.T, addr string, cfg *handshakeConfig) (*handshakeResult, error) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if cfg.creds == nil {
		cfg.creds = &testCreds{}
	}
	if cfg.trust == nil {
		cfg.trust = &testTrust{}
	}
	if cfg.exclude == nil {
		cfg.exclude = map[uint32]bool{}
	}
	cfg.host, cfg.timeout = "127.0.0.1", 5*time.Second
	return clientHandshake(context.Background(), c, cfg)
}

func selfSignedTLS(t testing.TB, cn string) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{cn}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

// ---- handshake tests ----------------------------------------------------------------------------------------------

func TestHandshakeNoneAllVersions(t *testing.T) {
	for _, ver := range []string{"RFB 003.003\n", "RFB 003.007\n", "RFB 003.008\n", "RFB 003.889\n"} {
		srv := &vnctest.Server{Version: ver, Name: "desk", Width: 800, Height: 600}
		res, err := handshakeTo(t, startFake(t, srv), &handshakeConfig{shared: true})
		if err != nil {
			t.Fatalf("%q: %v", ver, err)
		}
		if res.secType != secNone || res.init == nil || res.init.name != "desk" || res.init.width != 800 {
			t.Fatalf("%q: result %+v init %+v", ver, res, res.init)
		}
		_, _, _, shared, _, _ := srv.Snapshot()
		if len(shared) != 1 || shared[0] != 1 {
			t.Fatalf("shared flag %v", shared)
		}
	}
}

func TestHandshakeVNCAuth(t *testing.T) {
	srv := &vnctest.Server{Types: []byte{vnctest.SecVNCAuth}, Password: "s3cret"}
	addr := startFake(t, srv)
	res, err := handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{pass: "s3cret"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.secType != secVNCAuth || !res.usedPassword || res.encrypted() {
		t.Fatalf("unexpected result %+v", res)
	}
	srv.FailReason = "Bad password"
	_, err = handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{pass: "wrong"}})
	var ae *authError
	if !errors.As(err, &ae) || ae.reason != "Bad password" {
		t.Fatalf("expected auth error with reason, got %v", err)
	}
	// Missing password: prompted.
	creds := &testCreds{answers: []string{"s3cret"}}
	if _, err := handshakeTo(t, addr, &handshakeConfig{creds: creds}); err != nil || creds.prompts != 1 {
		t.Fatalf("prompted handshake: %v (prompts %d)", err, creds.prompts)
	}
	// Canceled prompt.
	if _, err := handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{}}); !errors.Is(err, errCanceled) {
		t.Fatalf("expected cancel, got %v", err)
	}
}

func TestHandshakeVNCAuthRFB33(t *testing.T) {
	srv := &vnctest.Server{Version: "RFB 003.003\n", Type33: vnctest.SecVNCAuth, Password: "pw"}
	res, err := handshakeTo(t, startFake(t, srv), &handshakeConfig{creds: &testCreds{pass: "pw"}})
	if err != nil || res.version != rfb33 || res.secType != secVNCAuth {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func TestHandshakeARD(t *testing.T) {
	srv := &vnctest.Server{Types: []byte{vnctest.SecARD, vnctest.SecVNCAuth}, Username: "alice", ARDPassword: "mac-pw", Password: "vnc-pw"}
	addr := startFake(t, srv)
	// With a user name, ARD (account login) is preferred over the VNC password.
	res, err := handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{user: "alice", pass: "mac-pw"}})
	if err != nil || res.secType != secARD {
		t.Fatalf("ARD: res %+v err %v", res, err)
	}
	// Without one, VNC Authentication.
	res, err = handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{pass: "vnc-pw"}})
	if err != nil || res.secType != secVNCAuth {
		t.Fatalf("VNC auth: res %+v err %v", res, err)
	}
	// ARD only, wrong password.
	srv.Types = []byte{vnctest.SecARD}
	_, err = handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{user: "alice", pass: "nope"}})
	if _, ok := errors.AsType[*authError](err); !ok {
		t.Fatalf("expected auth failure, got %v", err)
	}
}

func TestHandshakeVeNCryptX509(t *testing.T) {
	srv := &vnctest.Server{
		Types:            []byte{vnctest.SecVeNCrypt, vnctest.SecVNCAuth},
		VeNCryptSubtypes: []uint32{vnctest.VcTLSVnc, vnctest.SecVNCAuth, vnctest.VcX509Vnc, vnctest.VcX509Plain},
		TLS:              selfSignedTLS(t, "vnc.example"),
		Password:         "pw",
		Username:         "bob",
		Name:             "tls desktop",
	}
	addr := startFake(t, srv)
	trust := &testTrust{}
	res, err := handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{pass: "pw"}, trust: trust})
	if err != nil {
		t.Fatal(err)
	}
	if res.secType != secVeNCrypt || res.subType != vcX509Vnc || !res.encrypted() || res.tls.Anonymous ||
		!strings.HasPrefix(res.tls.Fingerprint, "SHA256:") || trust.calls != 1 || trust.subj != "vnc.example" {
		t.Fatalf("unexpected result %+v tls %+v trust %+v", res, res.tls, trust)
	}
	if res.init.name != "tls desktop" {
		t.Fatalf("ServerInit through TLS: %+v", res.init)
	}
	// A configured user name prefers the Plain variant.
	res, err = handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{user: "bob", pass: "pw"}, trust: &testTrust{}})
	if err != nil || res.subType != vcX509Plain {
		t.Fatalf("X509Plain: res %+v err %v", res, err)
	}
	// A rejected certificate aborts (no downgrade to unencrypted types).
	_, err = handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{pass: "pw"}, trust: &testTrust{reject: true}})
	if !errors.Is(err, errCertRejected) {
		t.Fatalf("expected certificate rejection, got %v", err)
	}
}

// TestHandshakeAnonTLSFallback: a server whose anonymous TLS fails (here: refused) but that also offers an
// unencrypted subtype. The default policy stops and asks; allow-unencrypted (or the user's confirmation) retries
// without anonymous TLS; require fails.
func TestHandshakeAnonTLSFallback(t *testing.T) {
	srv := &vnctest.Server{
		Types:            []byte{vnctest.SecVeNCrypt},
		VeNCryptSubtypes: []uint32{vnctest.VcTLSVnc, vnctest.SecVNCAuth},
		Password:         "pw",
	}
	addr := startFake(t, srv)
	_, err := handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{pass: "pw"}})
	var ie *insecureError
	if !errors.As(err, &ie) || !ie.unencrypted || ie.weakTLS || ie.cleartext || ie.reason != "The server refused the anonymous TLS handshake (handshake failure)" {
		t.Fatalf("default policy: expected a confirmation request, got %v (%+v)", err, ie)
	}
	cfg := &handshakeConfig{creds: &testCreds{pass: "pw"}, policy: encAllowUnencrypted}
	_, err = handshakeTo(t, addr, cfg)
	var re *retryError
	if !errors.As(err, &re) || re.exclude != excludeAnonTLS || !re.downgrade {
		t.Fatalf("allow-unencrypted: expected a retry without anonymous TLS, got %v", err)
	}
	cfg.exclude = map[uint32]bool{re.exclude: true}
	res, err := handshakeTo(t, addr, cfg)
	if err != nil || res.subType != secVNCAuth || res.encrypted() {
		t.Fatalf("fallback: res %+v err %v", res, err)
	}
	var er *encryptionRequiredError
	if _, err := handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{pass: "pw"}, policy: encRequire}); !errors.As(err, &er) {
		t.Fatalf("require: %v", err)
	}
	// Anonymous TLS as the only subtype, VNC Authentication at the top level: the retry skips VeNCrypt entirely.
	srv.Types = []byte{vnctest.SecVeNCrypt, vnctest.SecVNCAuth}
	srv.VeNCryptSubtypes = []uint32{vnctest.VcTLSVnc}
	_, err = handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{pass: "pw"}, policy: encAllowUnencrypted})
	if !errors.As(err, &re) || re.exclude != secVeNCrypt {
		t.Fatalf("top-level alternative: %v", err)
	}
	// Nothing to fall back to: the TLS error itself.
	srv.Types = []byte{vnctest.SecVeNCrypt}
	_, err = handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{pass: "pw"}})
	if errors.As(err, &ie) || errors.As(err, &re) || !strings.Contains(fmt.Sprint(err), "refused the anonymous TLS handshake") {
		t.Fatalf("no alternative: %v", err)
	}
}

// TestHandshakeAnonTLS runs VeNCrypt TLSVnc / TLSPlain / TLSNone over real anonymous TLS (anontlstest).
func TestHandshakeAnonTLS(t *testing.T) {
	for _, sub := range []uint32{vnctest.VcTLSVnc, vnctest.VcTLSPlain, vnctest.VcTLSNone} {
		srv := &vnctest.Server{
			Types:            []byte{vnctest.SecVeNCrypt, vnctest.SecVNCAuth},
			VeNCryptSubtypes: []uint32{sub},
			AnonTLS:          &anontlstest.Config{},
			Password:         "pw",
			Username:         "carol",
			Name:             "anon tls desktop",
		}
		res, err := handshakeTo(t, startFake(t, srv), &handshakeConfig{creds: &testCreds{user: "carol", pass: "pw"}})
		if err != nil {
			t.Fatalf("%s: %v", securityTypeName(sub), err)
		}
		if !res.encrypted() || !res.tls.Anonymous || res.tls.Weak || res.tls.Group != "ffdhe2048" || res.tls.DHBits != 2048 ||
			res.init.name != "anon tls desktop" || res.subType != sub {
			t.Fatalf("%s: res %+v tls %+v", securityTypeName(sub), res, res.tls)
		}
	}
	// ECDH + CBC with encrypt-then-MAC.
	srv := &vnctest.Server{Types: []byte{vnctest.SecVeNCrypt}, VeNCryptSubtypes: []uint32{vnctest.VcTLSVnc}, Password: "pw",
		AnonTLS: &anontlstest.Config{Suites: []uint16{anontlstest.ECDHAES256CBC}}}
	res, err := handshakeTo(t, startFake(t, srv), &handshakeConfig{creds: &testCreds{pass: "pw"}})
	if err != nil || res.tls.Group != "X25519" || !res.tls.EncryptThenMAC {
		t.Fatalf("ECDH: %v %+v", err, res)
	}
}

// TestHandshakeWeakGroup: a 1024-bit group is refused by default (the user is asked, offering weak encryption),
// accepted with allow-weak (or the confirmation), and never with require.
func TestHandshakeWeakGroup(t *testing.T) {
	srv := &vnctest.Server{
		Types:            []byte{vnctest.SecVeNCrypt, vnctest.SecVNCAuth},
		VeNCryptSubtypes: []uint32{vnctest.VcTLSVnc},
		AnonTLS:          &anontlstest.Config{DHPrime: anontlstest.RFC5114P1024, DHGenerator: anontlstest.RFC5114G1024},
		Password:         "pw",
	}
	addr := startFake(t, srv)
	_, err := handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{pass: "pw"}})
	var ie *insecureError
	if !errors.As(err, &ie) || !ie.weakTLS || ie.dhBits != 1024 || !ie.unencrypted || !strings.Contains(ie.reason, "1024-bit") {
		t.Fatalf("default: %v %+v", err, ie)
	}
	res, err := handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{pass: "pw"}, policy: encAllowWeak})
	if err != nil || !res.encrypted() || !res.tls.Weak || res.tls.DHBits != 1024 {
		t.Fatalf("allow-weak: %v %+v", err, res)
	}
	var er *encryptionRequiredError
	if _, err := handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{pass: "pw"}, policy: encRequire}); !errors.As(err, &er) ||
		!strings.Contains(err.Error(), "1024-bit") {
		t.Fatalf("require: %v", err)
	}
}

func TestHandshakeRequirePolicy(t *testing.T) {
	for _, c := range []struct {
		name string
		srv  *vnctest.Server
	}{
		{"VNC Authentication only", &vnctest.Server{Types: []byte{vnctest.SecVNCAuth}, Password: "pw"}},
		{"None only", &vnctest.Server{}},
		{"RFB 3.3", &vnctest.Server{Version: "RFB 003.003\n", Type33: vnctest.SecVNCAuth, Password: "pw"}},
		{"browser-side types", &vnctest.Server{Types: []byte{vnctest.SecTight}}},
		{"ARD", &vnctest.Server{Types: []byte{vnctest.SecARD}, Username: "u", Password: "pw"}},
		{"VeNCrypt without TLS", &vnctest.Server{Types: []byte{vnctest.SecVeNCrypt},
			VeNCryptSubtypes: []uint32{vnctest.VcPlain, vnctest.SecVNCAuth}, Password: "pw"}},
	} {
		_, err := handshakeTo(t, startFake(t, c.srv), &handshakeConfig{creds: &testCreds{user: "u", pass: "pw"},
			policy: encRequire, allowPassthrough: true})
		if _, ok := errors.AsType[*encryptionRequiredError](err); !ok {
			t.Fatalf("%s: expected encryption required, got %v", c.name, err)
		}
	}
	srv := &vnctest.Server{Types: []byte{vnctest.SecVNCAuth, vnctest.SecVeNCrypt}, VeNCryptSubtypes: []uint32{vnctest.VcX509Vnc},
		TLS: selfSignedTLS(t, "secure.example"), Password: "pw"}
	res, err := handshakeTo(t, startFake(t, srv), &handshakeConfig{creds: &testCreds{pass: "pw"}, policy: encRequire})
	if err != nil || !res.encrypted() {
		t.Fatalf("X509 under require: %v %+v", err, res)
	}
}

// TestHandshakePlainCleartext: VeNCrypt Plain without TLS would send the password in clear text — VNC
// Authentication is preferred, and Plain alone needs confirmation.
func TestHandshakePlainCleartext(t *testing.T) {
	srv := &vnctest.Server{Types: []byte{vnctest.SecVeNCrypt}, VeNCryptSubtypes: []uint32{vnctest.VcPlain, vnctest.SecVNCAuth},
		Username: "bob", Password: "pw"}
	addr := startFake(t, srv)
	res, err := handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{user: "bob", pass: "pw"}})
	if err != nil || res.subType != secVNCAuth {
		t.Fatalf("Plain + VNC Authentication: %v %+v", err, res)
	}
	srv.VeNCryptSubtypes = []uint32{vnctest.VcPlain}
	_, err = handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{user: "bob", pass: "pw"}})
	var ie *insecureError
	if !errors.As(err, &ie) || !ie.cleartext || !ie.unencrypted {
		t.Fatalf("Plain only: %v", err)
	}
	_, _, subtypes, _, _, _ := srv.Snapshot()
	if len(subtypes) != 1 { // the refused attempt never sent the subtype, let alone the password
		t.Fatalf("subtypes chosen: %v", subtypes)
	}
	res, err = handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{user: "bob", pass: "pw"}, policy: encAllowUnencrypted})
	if err != nil || res.subType != vcPlain {
		t.Fatalf("Plain after confirmation: %v %+v", err, res)
	}
}

func TestHandshakeARDKeyLengthFloor(t *testing.T) {
	srv := &vnctest.Server{Types: []byte{vnctest.SecARD}, Username: "alice", Password: "pw", ARDKeyLen: 64}
	_, err := handshakeTo(t, startFake(t, srv), &handshakeConfig{creds: &testCreds{user: "alice", pass: "pw"}})
	if err == nil || !strings.Contains(err.Error(), "key length") {
		t.Fatalf("512-bit ARD group accepted: %v", err)
	}
}

func TestForwardClientMessagesClipboardPolicy(t *testing.T) {
	encs := []int32{0, 7, pseudoEncodingExtendedClipboard, -223}
	setEnc := []byte{2, 0, 0, byte(len(encs))}
	for _, e := range encs {
		setEnc = binary.BigEndian.AppendUint32(setEnc, uint32(e))
	}
	msgs := [][]byte{
		setEnc,
		append([]byte{6, 0, 0, 0, 0, 0, 0, 3}, "abc"...),               // ClientCutText
		append([]byte{6, 0, 0, 0, 0xff, 0xff, 0xff, 0xfc}, 1, 2, 3, 4), // extended clipboard
		{4, 1, 0, 0, 0, 0, 0xff, 0x0d},                                 // KeyEvent passes
		{5, 1, 0, 10, 0, 20},                                           // PointerEvent passes
	}
	var in bytes.Buffer
	for _, m := range msgs {
		in.Write(m)
	}
	var out bytes.Buffer
	if err := forwardClientMessages(bufio.NewReader(&in), &out, clientFilter{dropClipboard: true}); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	want := []byte{2, 0, 0, 3}
	for _, e := range []int32{0, 7, -223} {
		want = binary.BigEndian.AppendUint32(want, uint32(e))
	}
	want = append(append(want, msgs[3]...), msgs[4]...)
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("filtered stream:\n got %x\nwant %x", out.Bytes(), want)
	}
}

func TestHandshakeVeNCryptUnsupportedSubtypes(t *testing.T) {
	srv := &vnctest.Server{Types: []byte{vnctest.SecVeNCrypt, vnctest.SecVNCAuth}, VeNCryptSubtypes: []uint32{vnctest.VcTLSSASL},
		Password: "pw"}
	addr := startFake(t, srv)
	// The server offered VeNCrypt (encryption) that AstraTerm cannot use: continuing unencrypted needs confirmation.
	_, err := handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{pass: "pw"}})
	var ie *insecureError
	if !errors.As(err, &ie) || !ie.unencrypted {
		t.Fatalf("default policy: expected a confirmation request, got %v", err)
	}
	_, err = handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{pass: "pw"}, policy: encAllowUnencrypted})
	var re *retryError
	if !errors.As(err, &re) || re.exclude != secVeNCrypt || !re.downgrade {
		t.Fatalf("expected retry without VeNCrypt, got %v", err)
	}
	res, err := handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{pass: "pw"}, exclude: map[uint32]bool{secVeNCrypt: true}})
	if err != nil || res.secType != secVNCAuth {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func TestHandshakePassthroughAndRefusal(t *testing.T) {
	srv := &vnctest.Server{Types: []byte{vnctest.SecTight, 113}}
	addr := startFake(t, srv)
	res, err := handshakeTo(t, addr, &handshakeConfig{allowPassthrough: true})
	if err != nil || res.pass == nil || !bytes.Equal(res.pass.security, []byte{2, vnctest.SecTight, 113}) {
		t.Fatalf("pass-through: res %+v err %v", res, err)
	}
	var ue *unsupportedError
	if _, err := handshakeTo(t, addr, &handshakeConfig{}); !errors.As(err, &ue) {
		t.Fatalf("expected unsupported without pass-through, got %v", err)
	}
	srv2 := &vnctest.Server{RefuseWith: "Too many security failures"}
	var rf *refusedError
	if _, err := handshakeTo(t, startFake(t, srv2), &handshakeConfig{}); !errors.As(err, &rf) || rf.reason != "Too many security failures" {
		t.Fatalf("expected refusal, got %v", err)
	}
}

func TestHandshakeRepeater(t *testing.T) {
	srv := &vnctest.Server{RepeaterID: "1234"}
	addr := startFake(t, srv)
	if _, err := handshakeTo(t, addr, &handshakeConfig{repeaterID: "1234"}); err != nil {
		t.Fatal(err)
	}
	if _, err := handshakeTo(t, addr, &handshakeConfig{}); !errors.Is(err, errRepeater) {
		t.Fatalf("expected repeater error, got %v", err)
	}
}

func TestHandshakeNonePreferredWithoutPassword(t *testing.T) {
	srv := &vnctest.Server{Types: []byte{vnctest.SecVNCAuth, vnctest.SecNone}, Password: "pw"}
	addr := startFake(t, srv)
	res, err := handshakeTo(t, addr, &handshakeConfig{})
	if err != nil || res.secType != secNone {
		t.Fatalf("without password: res %+v err %v", res, err)
	}
	res, err = handshakeTo(t, addr, &handshakeConfig{creds: &testCreds{pass: "pw"}})
	if err != nil || res.secType != secVNCAuth {
		t.Fatalf("with password: res %+v err %v", res, err)
	}
}

// ---- browser side -------------------------------------------------------------------------------------------------

func TestPresentNone(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	init := append([]byte{0, 10, 0, 20}, make([]byte, 16)...)
	init = append(append(init, 0, 0, 0, 3), "abc"...)
	errc := make(chan error, 1)
	go func() { errc <- presentNone(a, a, init) }()
	got, err := vnctest.Viewer(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, init) {
		t.Fatalf("ServerInit mismatch: %x", got)
	}
}

func TestPresentPassthrough(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	p := &passthrough{version: rfb37, security: []byte{1, 16}}
	errc := make(chan error, 1)
	go func() { errc <- presentPassthrough(a, a, p) }()
	v := make([]byte, 12)
	if _, err := io.ReadFull(b, v); err != nil || string(v) != "RFB 003.007\n" {
		t.Fatalf("version %q %v", v, err)
	}
	if _, err := b.Write(v); err != nil {
		t.Fatal(err)
	}
	sec := make([]byte, 2)
	if _, err := io.ReadFull(b, sec); err != nil || !bytes.Equal(sec, []byte{1, 16}) {
		t.Fatalf("security %v %v", sec, err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestForwardClientMessages(t *testing.T) {
	var in bytes.Buffer
	msgs := [][]byte{
		append([]byte{0}, make([]byte, 19)...),                         // SetPixelFormat
		{2, 0, 0, 2, 0, 0, 0, 7, 0xff, 0xff, 0xff, 0x21},               // SetEncodings (2)
		{3, 1, 0, 0, 0, 0, 4, 0, 3, 0},                                 // FramebufferUpdateRequest
		{4, 1, 0, 0, 0, 0, 0xff, 0x0d},                                 // KeyEvent
		{5, 1, 0, 10, 0, 20},                                           // PointerEvent
		{5, 0x81, 0, 10, 0, 20, 1},                                     // extended PointerEvent
		append([]byte{6, 0, 0, 0, 0, 0, 0, 3}, "abc"...),               // ClientCutText
		append([]byte{6, 0, 0, 0, 0xff, 0xff, 0xff, 0xfc}, 1, 2, 3, 4), // extended ClientCutText (-4)
		{150, 1, 0, 0, 0, 0, 4, 0, 3, 0},                               // EnableContinuousUpdates
		{248, 0, 0, 0, 0, 0, 0, 1, 2, 'h', 'i'},                        // ClientFence
		{250, 0, 1, 2},                                                 // XVP
		append([]byte{251, 0, 4, 0, 3, 0, 1, 0}, make([]byte, 16)...),  // SetDesktopSize
		{255, 0, 0, 1, 0, 0, 0, 0x61, 0, 0, 0, 0x1e},                   // QEMU extended key
	}
	for _, m := range msgs {
		in.Write(m)
	}
	var all, ro bytes.Buffer
	if err := forwardClientMessages(bufio.NewReader(bytes.NewReader(in.Bytes())), &all, clientFilter{}); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !bytes.Equal(all.Bytes(), in.Bytes()) {
		t.Fatal("unfiltered stream changed")
	}
	if err := forwardClientMessages(bufio.NewReader(bytes.NewReader(in.Bytes())), &ro, clientFilter{dropInput: true}); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	var want bytes.Buffer
	for _, i := range []int{0, 1, 2, 8, 9} {
		want.Write(msgs[i])
	}
	if !bytes.Equal(ro.Bytes(), want.Bytes()) {
		t.Fatalf("read-only stream:\n got %x\nwant %x", ro.Bytes(), want.Bytes())
	}
	// Unknown message types stop the stream.
	if err := forwardClientMessages(bufio.NewReader(bytes.NewReader([]byte{99})), io.Discard, clientFilter{dropInput: true}); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("unknown type accepted: %v", err)
	}
}

func TestRepeaterMessage(t *testing.T) {
	m := repeaterMessage("42")
	if len(m) != 250 || string(m[:5]) != "ID:42" || m[5] != 0 {
		t.Fatalf("bad message %q", m[:8])
	}
	if m := repeaterMessage("id:7"); string(m[:4]) != "id:7" {
		t.Fatalf("prefixed id rewritten: %q", m[:6])
	}
}

func TestARDResponseRoundTrip(t *testing.T) {
	// ardResponse against the test server's decryption (vnctest.Server.ard) is covered by TestHandshakeARD; check the
	// layout here: 128 encrypted bytes + key-length public key.
	p := new(big.Int).SetBytes(bytes.Repeat([]byte{0xff}, 32))
	a := &ardParams{generator: big.NewInt(2), prime: p, serverPub: big.NewInt(12345), keyLen: 32}
	out, err := ardResponse(a, "u", "p", nil)
	if err != nil || len(out) != 128+32 {
		t.Fatalf("len %d err %v", len(out), err)
	}
	if binary.BigEndian.Uint32(out[128:132]) == 0 && binary.BigEndian.Uint32(out[132:136]) == 0 &&
		new(big.Int).SetBytes(out[128:]).Sign() == 0 {
		t.Fatal("zero public key")
	}
}
