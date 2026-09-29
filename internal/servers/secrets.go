package servers

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
)

// Persistent server secrets live in the settings table under a module-private scope (never merged into anyone's
// GET /api/settings), sealed with the vault's system key so autostart works while the vault is locked.
const (
	settingsScope = "servers"
	keyHostKeys   = "servers.hostkeys"
	keyTLS        = "servers.tls"
)

// rsaHostKeyBits is the size of the generated RSA host key (tests lower it).
var rsaHostKeyBits = 3072

type sealedSet struct {
	Version int               `json:"v"`
	Items   map[string][]byte `json:"items"` // name → SystemSeal(PEM)
}

// secretStore loads (or generates and stores) the SSH host keys and the self-signed TLS certificate shared by the
// servers.
type secretStore struct {
	d *app.Deps

	mu      sync.Mutex
	signers []ssh.Signer
	cert    *tls.Certificate
	certFP  string
}

func (s *secretStore) load(ctx context.Context, key string) (map[string][]byte, error) {
	var set sealedSet
	ok, err := s.d.Store.Settings.GetJSON(ctx, settingsScope, key, &set)
	if err != nil || !ok {
		return nil, err
	}
	out := map[string][]byte{}
	for name, ct := range set.Items {
		pt, err := s.d.Vault.SystemOpen(ct)
		if err != nil {
			return nil, fmt.Errorf("decrypt %s: %w", name, err)
		}
		out[name] = pt
	}
	return out, nil
}

func (s *secretStore) save(ctx context.Context, key string, items map[string][]byte) error {
	set := sealedSet{Version: 1, Items: map[string][]byte{}}
	for name, pt := range items {
		ct, err := s.d.Vault.SystemSeal(pt)
		if err != nil {
			return err
		}
		set.Items[name] = ct
	}
	return s.d.Store.Settings.SetJSON(ctx, settingsScope, key, set)
}

// hostKeys returns the SSH host keys (ed25519, ecdsa-p256, rsa), generating the missing ones on first use.
func (s *secretStore) hostKeys(ctx context.Context) ([]ssh.Signer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.signers != nil {
		return s.signers, nil
	}
	items, err := s.load(ctx, keyHostKeys)
	if err != nil {
		// Undecryptable (system key replaced): start over with fresh keys rather than failing forever.
		s.d.Log.Warn("servers: cannot read the stored SSH host keys, generating new ones", "err", err)
		items = nil
	}
	if items == nil {
		items = map[string][]byte{}
	}
	changed := false
	var signers []ssh.Signer
	for _, alg := range []string{"ed25519", "ecdsa", "rsa"} {
		pemBytes := items[alg]
		var signer ssh.Signer
		if len(pemBytes) > 0 {
			signer, err = ssh.ParsePrivateKey(pemBytes)
			if err != nil {
				s.d.Log.Warn("servers: stored SSH host key is invalid, regenerating", "alg", alg, "err", err)
				signer = nil
			}
		}
		if signer == nil {
			key, err := generateHostKey(alg)
			if err != nil {
				return nil, fmt.Errorf("generate %s host key: %w", alg, err)
			}
			block, err := ssh.MarshalPrivateKey(key, "astraterm-embedded-server")
			if err != nil {
				return nil, err
			}
			items[alg] = pem.EncodeToMemory(block)
			if signer, err = ssh.NewSignerFromKey(key); err != nil {
				return nil, err
			}
			changed = true
		}
		signers = append(signers, signer)
	}
	if changed {
		if err := s.save(ctx, keyHostKeys, items); err != nil {
			return nil, fmt.Errorf("store host keys: %w", err)
		}
	}
	s.signers = signers
	return signers, nil
}

func generateHostKey(alg string) (crypto.Signer, error) {
	switch alg {
	case "ed25519":
		_, k, err := ed25519.GenerateKey(rand.Reader)
		return k, err
	case "ecdsa":
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "rsa":
		return rsa.GenerateKey(rand.Reader, rsaHostKeyBits)
	}
	return nil, fmt.Errorf("unknown algorithm %s", alg)
}

// hostKeyFingerprint is the SHA256 fingerprint of the first (ed25519) host key.
func hostKeyFingerprint(signers []ssh.Signer) string {
	if len(signers) == 0 {
		return ""
	}
	return ssh.FingerprintSHA256(signers[0].PublicKey())
}

// certificate returns the self-signed TLS certificate (ECDSA P-256) used by HTTPS and FTPS, generating it on first
// use, plus its SHA-256 fingerprint.
func (s *secretStore) certificate(ctx context.Context) (*tls.Certificate, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cert != nil && time.Now().Before(s.cert.Leaf.NotAfter.Add(-24*time.Hour)) {
		return s.cert, s.certFP, nil
	}
	items, err := s.load(ctx, keyTLS)
	if err != nil {
		s.d.Log.Warn("servers: cannot read the stored TLS certificate, generating a new one", "err", err)
		items = nil
	}
	var cert *tls.Certificate
	if len(items["cert"]) > 0 && len(items["key"]) > 0 {
		if c, err := tls.X509KeyPair(items["cert"], items["key"]); err == nil && c.Leaf != nil &&
			time.Now().Before(c.Leaf.NotAfter.Add(-24*time.Hour)) {
			cert = &c
		}
	}
	if cert == nil {
		certPEM, keyPEM, err := selfSigned()
		if err != nil {
			return nil, "", err
		}
		c, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, "", err
		}
		if err := s.save(ctx, keyTLS, map[string][]byte{"cert": certPEM, "key": keyPEM}); err != nil {
			return nil, "", fmt.Errorf("store certificate: %w", err)
		}
		cert = &c
	}
	sum := sha256.Sum256(cert.Certificate[0])
	s.cert, s.certFP = cert, colonHex(sum[:])
	return s.cert, s.certFP, nil
}

// selfSigned creates a 10-year ECDSA certificate for localhost, the host name and the current interface addresses.
func selfSigned() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	host, _ := os.Hostname()
	dns := []string{"localhost"}
	if host != "" && !strings.EqualFold(host, "localhost") {
		dns = append(dns, host)
	}
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
				ips = append(ips, ipn.IP)
			}
		}
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "AstraTerm embedded server", Organization: []string{"AstraTerm"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), nil
}

// tlsConfig returns a server TLS configuration (TLS ≥ 1.2) for cert.
func tlsConfig(cert *tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12}
}

func colonHex(b []byte) string {
	h := strings.ToUpper(hex.EncodeToString(b))
	var sb strings.Builder
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			sb.WriteByte(':')
		}
		sb.WriteString(h[i : i+2])
	}
	return sb.String()
}
