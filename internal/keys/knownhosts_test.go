package keys

import (
	"crypto/dsa" //nolint:staticcheck // PuTTY dss host keys
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestParseOpenSSHKnownHosts(t *testing.T) {
	host := strings.TrimSpace(string(fixture(t, "hostkey.pub")))
	ca := strings.TrimSpace(string(fixture(t, "ca.pub")))
	text := strings.Join([]string{
		"# comment",
		"",
		"Host.Example.com,10.0.0.5 " + host + " accepted by admin on 2026-09-27",
		"[jump.example.com]:2222 " + host,
		"@cert-authority *.example.com,!bad.example.com " + ca,
		"@revoked * " + host,
		"@bogus host " + host,
		"host ssh-rsa AAAAinvalid",
		"host ssh-rsa " + strings.Fields(host)[1], // type mismatch
	}, "\r\n")
	entries, errs := parseOpenSSHKnownHosts(text)
	if len(entries) != 4 || len(errs) != 3 {
		t.Fatalf("entries %d errs %v", len(entries), errs)
	}
	if e := entries[0]; e.Marker != "" || len(e.Hosts) != 2 || e.Hosts[0] != "host.example.com" || e.Comment != "accepted by admin on 2026-09-27" || e.Line != 3 {
		t.Fatalf("%+v", e)
	}
	if h, p, k := splitKnownHost(entries[1].Hosts[0]); h != "jump.example.com" || p != 2222 || k != hostPlain {
		t.Fatalf("%s %d %d", h, p, k)
	}
	if entries[2].Marker != markerCertAuthority || entries[3].Marker != markerRevoked {
		t.Fatal("markers")
	}
	if errs[0].Line != 7 {
		t.Fatalf("line numbers %+v", errs)
	}
	for _, tc := range []struct {
		in   string
		kind hostKind
	}{{"*.x.com", hostPattern}, {"!x", hostPattern}, {"|1|a|b", hostHashed}, {"[x]:99999", hostInvalid},
		{"[x", hostInvalid}, {"a b", hostInvalid}, {"[::1]:2200", hostPlain}, {"fe80::1", hostPlain}} {
		if _, _, k := splitKnownHost(tc.in); k != tc.kind {
			t.Fatalf("%q: kind %d", tc.in, k)
		}
	}
}

func TestHashedKnownHosts(t *testing.T) {
	entries, errs := parseOpenSSHKnownHosts(string(fixture(t, "known_hosts_hashed")))
	if len(errs) != 0 || len(entries) != 2 {
		t.Fatalf("%d %v", len(entries), errs)
	}
	if !hashedHostMatches(entries[0].Hosts[0], "host.example.com") || hashedHostMatches(entries[0].Hosts[0], "other") {
		t.Fatal("ssh-keygen -H entry")
	}
	if !hashedHostMatches(entries[1].Hosts[0], "[other.example.com]:2222") {
		t.Fatal("hashed non-standard port")
	}
	h := hashHostName("[x.example.com]:22")
	if !hashedHostMatches(h, "[x.example.com]:22") || !matchHostPatterns(h, "[x.example.com]:22") {
		t.Fatal("own hashing")
	}
}

func TestHostPatterns(t *testing.T) {
	cases := []struct {
		patterns, name string
		want           bool
	}{
		{"*.example.com", "a.example.com", true},
		{"*.example.com", "example.com", false},
		{"*.example.com,!bad.example.com", "bad.example.com", false},
		{"*.EXAMPLE.com", "A.example.COM", true},
		{"host?", "host1", true},
		{"host?", "host12", false},
		{"*", "[anything]:2222", true},
		{"*.example.com", "[a.example.com]:2222", false},
		{"[*.example.com]:*", "[a.example.com]:2222", true},
		{"10.0.*", "10.0.1.2", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
	}
	for _, tc := range cases {
		if got := matchHostPatterns(tc.patterns, tc.name); got != tc.want {
			t.Errorf("%q vs %q: %v", tc.patterns, tc.name, got)
		}
	}
	// Markers match the bare host on any port; negations of either form win.
	if !matchHostPatterns("*.example.com", markerNames("a.example.com", 2222)...) ||
		matchHostPatterns("[*.example.com]:*,!bad.example.com", markerNames("bad.example.com", 2222)...) ||
		matchHostPatterns("[*.example.com]:2222", markerNames("a.example.com", 22)...) {
		t.Fatal("marker names")
	}
	if _, err := validateHostPatterns(" , "); err == nil {
		t.Fatal("empty patterns accepted")
	}
	if p, err := validateHostPatterns("*.Example.com, !Bad.example.com"); err != nil || p != "*.example.com,!bad.example.com" {
		t.Fatalf("%q %v", p, err)
	}
	for _, bad := range []string{"a/b", "a b", "|1|nope", "|1|AAAA|BBBB", "!|2|x|y", "a|b"} {
		if _, err := validateHostPatterns(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestHashedMarkerPatterns: hashed names (base64 with "/" and "+", case-sensitive) are valid marker patterns, keep
// their case and match; negated hashed names win.
func TestHashedMarkerPatterns(t *testing.T) {
	var h string
	for i := 0; i < 1000 && !strings.ContainsAny(h, "/+"); i++ {
		h = hashHostName("[db.example.com]:2222")
	}
	neg := hashHostName("bad.example.com")
	p, err := validateHostPatterns(h + ",*.example.com,!" + neg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, h+",") || !strings.HasSuffix(p, ",!"+neg) {
		t.Fatalf("patterns changed: %q", p)
	}
	if !matchHostPatterns(p, markerNames("db.example.com", 2222)...) || !matchHostPatterns(p, markerNames("a.example.com", 22)...) ||
		matchHostPatterns(p, markerNames("bad.example.com", 22)...) {
		t.Fatal("hashed marker matching")
	}
	// An exported @cert-authority line with a hashed name imports again.
	ca := fixturePub(t, "ca.pub")
	entries, errs := parseOpenSSHKnownHosts("@cert-authority " + h + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(ca))) + " ca\n")
	if len(errs) != 0 || len(entries) != 1 {
		t.Fatalf("%v %v", entries, errs)
	}
	if got, err := validateHostPatterns(strings.Join(entries[0].Hosts, ",")); err != nil || got != h {
		t.Fatalf("%q %v", got, err)
	}
}

// edwardsX recovers the x coordinate of an Ed25519 public key (RFC 8032 decoding), to build PuTTY cache entries.
func edwardsX(t *testing.T, pub ed25519.PublicKey) (*big.Int, *big.Int) {
	p := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	enc := append([]byte(nil), pub...)
	sign := enc[31] >> 7
	enc[31] &= 0x7f
	for i, j := 0, len(enc)-1; i < j; i, j = i+1, j-1 {
		enc[i], enc[j] = enc[j], enc[i]
	}
	y := new(big.Int).SetBytes(enc)
	d := new(big.Int).Mul(big.NewInt(-121665), new(big.Int).ModInverse(big.NewInt(121666), p))
	d.Mod(d, p)
	y2 := new(big.Int).Mul(y, y)
	u := new(big.Int).Sub(y2, big.NewInt(1))
	v := new(big.Int).Add(new(big.Int).Mul(d, y2), big.NewInt(1))
	x2 := new(big.Int).Mul(u, new(big.Int).ModInverse(v, p))
	x2.Mod(x2, p)
	x := new(big.Int).Exp(x2, new(big.Int).Rsh(new(big.Int).Add(p, big.NewInt(3)), 3), p)
	if new(big.Int).Exp(x, big.NewInt(2), p).Cmp(x2) != 0 {
		i := new(big.Int).Exp(big.NewInt(2), new(big.Int).Rsh(new(big.Int).Sub(p, big.NewInt(1)), 2), p)
		x.Mul(x, i).Mod(x, p)
	}
	if uint(x.Bit(0)) != uint(sign) {
		x.Sub(p, x)
	}
	return x, y
}

func TestPuTTYHostKeys(t *testing.T) {
	edPub := fixturePub(t, "openssh_ed25519.pub")
	x, y := edwardsX(t, edPub.(ssh.CryptoPublicKey).CryptoPublicKey().(ed25519.PublicKey))
	ecPub := fixturePub(t, "hostkey.pub").(ssh.CryptoPublicKey).CryptoPublicKey().(*ecdsa.PublicKey)
	rk, _ := parseKey(fixture(t, "pem_rsa"), "")
	rsaPub := &rk.Raw.(*rsa.PrivateKey).PublicKey
	dk, _ := parseKey(fixture(t, "openssh_dsa_enc"), "testpass")
	dsaPub := &dk.Raw.(*dsa.PrivateKey).PublicKey
	text := strings.Join([]string{
		"Windows Registry Editor Version 5.00",
		"",
		`[HKEY_CURRENT_USER\Software\SimonTatham\PuTTY\SshHostKeys]`,
		fmt.Sprintf(`"ssh-ed25519@22:ed.example.com"="0x%x,0x%x"`, x, y),
		fmt.Sprintf(`"ecdsa-sha2-nistp256@2222:EC.example.com"="0x%x,0x%x"`, ecPub.X, ecPub.Y),
		fmt.Sprintf(`"rsa2@22:10.0.0.7"="0x%x,0x%x"`, rsaPub.E, rsaPub.N),
		"[SSH_Hostkeys]",
		fmt.Sprintf(`dss@22:old.example.com=0x%x,0x%x,0x%x,0x%x`, dsaPub.P, dsaPub.Q, dsaPub.G, dsaPub.Y),
		`ssh-ed448@22:x.example.com=0x1,0x2`,
	}, "\n")
	if !looksLikePuTTYHostKeys(text) || looksLikePuTTYHostKeys(string(fixture(t, "known_hosts_plain"))) {
		t.Fatal("format detection")
	}
	entries, errs := parsePuTTYHostKeys(text)
	if len(entries) != 4 || len(errs) != 1 {
		t.Fatalf("%d entries, errors %v", len(entries), errs)
	}
	want := []struct {
		host string
		pub  ssh.PublicKey
	}{
		{"ed.example.com", edPub},
		{"[ec.example.com]:2222", fixturePub(t, "hostkey.pub")},
		{"10.0.0.7", mustSSH(t, rsaPub)},
		{"old.example.com", dk.Pub},
	}
	for i, w := range want {
		if entries[i].Hosts[0] != w.host || !sameKey(entries[i].Key, w.pub) {
			t.Fatalf("entry %d: %v %s", i, entries[i].Hosts, entries[i].Key.Type())
		}
	}
}

func mustSSH(t *testing.T, k any) ssh.PublicKey {
	t.Helper()
	pk, err := ssh.NewPublicKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pk
}

func TestCertificates(t *testing.T) {
	userPub := fixturePub(t, "openssh_ed25519.pub")
	certText := string(fixture(t, "openssh_ed25519-cert.pub"))
	cert, line, err := validateUserCert(certText, userPub)
	if err != nil {
		t.Fatal(err)
	}
	info := describeCert(cert, time.Now())
	if info.Type != "user" || info.KeyID != "alice@corp" || strings.Join(info.Principals, ",") != "alice,deploy" ||
		info.Status != certValid || info.ValidBefore == nil || info.CAFingerprint != ssh.FingerprintSHA256(fixturePub(t, "ca.pub")) {
		t.Fatalf("%+v", info)
	}
	if strings.Contains(strings.Join(info.Extensions, ","), "permit-port-forwarding") {
		t.Fatal("-O no-port-forwarding ignored")
	}
	if !strings.HasPrefix(line, "ssh-ed25519-cert-v01@openssh.com ") {
		t.Fatal(line)
	}
	if describeCert(cert, time.Now().Add(20*365*24*time.Hour)).Status != certExpired {
		t.Fatal("expiry")
	}
	// Wrong key, host certificate, plain key.
	if _, _, err := validateUserCert(certText, fixturePub(t, "hostkey.pub")); err == nil {
		t.Fatal("mismatching key accepted")
	}
	if _, _, err := validateUserCert(string(fixture(t, "hostkey-cert.pub")), fixturePub(t, "hostkey.pub")); err == nil {
		t.Fatal("host certificate accepted as user certificate")
	}
	if _, _, err := validateUserCert(string(fixture(t, "ca.pub")), userPub); err == nil {
		t.Fatal("plain key accepted")
	}
	// A tampered certificate (signature no longer matching) is rejected.
	tampered := *cert
	tampered.KeyId = "mallory"
	if _, _, err := validateUserCert(authorizedKeyLine(&tampered, ""), userPub); err == nil {
		t.Fatal("tampered certificate accepted")
	}
}

func TestSignCertificates(t *testing.T) {
	caKey, _ := generateKey("rsa", 2048)
	ca, _ := newParsedKey(caKey, "", formatOpenSSH, false)
	signer, err := caSigner(ca)
	if err != nil {
		t.Fatal(err)
	}
	subject := fixturePub(t, "openssh_ed25519.pub")
	now := time.Now()
	req := &signRequest{Identity: "alice", Principals: []string{"alice", " ", "alice", "root"},
		ValidBefore: now.Add(time.Hour).Format(time.RFC3339), CriticalOptions: map[string]string{"source-address": "10.0.0.0/8, 192.168.1.5"}}
	cert, err := buildCertificate(req, subject, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.SignCert(rand.Reader, signer); err != nil {
		t.Fatal(err)
	}
	if cert.Signature.Format != ssh.KeyAlgoRSASHA512 {
		t.Fatalf("signature %s", cert.Signature.Format)
	}
	checker := ssh.CertChecker{SupportedCriticalOptions: []string{"source-address"}}
	if err := checker.CheckCert("root", cert); err != nil {
		t.Fatal(err)
	}
	if len(cert.ValidPrincipals) != 2 || len(cert.Extensions) != 5 {
		t.Fatalf("%v %v", cert.ValidPrincipals, cert.Extensions)
	}
	if _, _, err := validateUserCert(authorizedKeyLine(cert, ""), subject); err != nil {
		t.Fatal(err)
	}
	bad := []signRequest{
		{Principals: nil},
		{Principals: []string{"a b"}},
		{Principals: []string{"x"}, CertType: "machine"},
		{Principals: []string{"x"}, ValidBefore: now.Add(-time.Hour).Format(time.RFC3339)},
		{Principals: []string{"x"}, CriticalOptions: map[string]string{"source-address": "nonsense"}},
		{Principals: []string{"x"}, CriticalOptions: map[string]string{"permit-everything": "yes"}},
		{Principals: []string{"x"}, Extensions: []string{"unknown-ext"}},
		{Principals: []string{"x"}, CertType: "host", Extensions: []string{"permit-pty"}},
	}
	for i, r := range bad {
		if _, err := buildCertificate(&r, subject, now); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
	host, err := buildCertificate(&signRequest{CertType: "host", Principals: []string{"h.example.com"}}, subject, now)
	if err != nil || host.CertType != ssh.HostCert || host.ValidBefore != ssh.CertTimeInfinity || len(host.Extensions) != 0 {
		t.Fatalf("host cert %v %+v", err, host)
	}
	if _, err := buildCertificate(&signRequest{Principals: []string{"x"}}, cert, now); err == nil {
		t.Fatal("certificate as subject accepted")
	}
}
