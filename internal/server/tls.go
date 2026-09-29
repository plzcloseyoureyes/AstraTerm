package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/nexterm/nexterm/internal/config"
)

// tlsConfig builds the server TLS configuration from --tls-cert/--tls-key or a generated self-signed certificate.
func tlsConfig(cfg *config.Config) (*tls.Config, error) {
	var (
		cert tls.Certificate
		err  error
	)
	if cfg.TLSSelfSigned {
		cert, err = selfSignedCert(filepath.Join(cfg.DataDir, "tls"), cfg.Listen)
	} else {
		cert, err = tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
	}
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2", "http/1.1"},
	}, nil
}

// selfSignedCert loads dir/self-signed.{crt,key}, (re)generating an ECDSA P-256 certificate when missing or expiring
// within 30 days. SANs cover localhost, loopback, the hostname, the listen address and local interface addresses.
func selfSignedCert(dir, listen string) (tls.Certificate, error) {
	certPath, keyPath := filepath.Join(dir, "self-signed.crt"), filepath.Join(dir, "self-signed.key")
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil && c.Leaf != nil && time.Until(c.Leaf.NotAfter) > 30*24*time.Hour {
		return c, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "NexTerm self-signed", Organization: []string{"NexTerm"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(397 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		tmpl.DNSNames = append(tmpl.DNSNames, h)
	}
	if host, _, err := net.SplitHostPort(listen); err == nil {
		if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() && !ip.IsLoopback() {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if ip == nil && host != "" && host != "localhost" {
			tmpl.DNSNames = append(tmpl.DNSNames, host)
		}
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
				tmpl.IPAddresses = append(tmpl.IPAddresses, ipn.IP)
			}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := writePEM(keyPath, "PRIVATE KEY", keyDER, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	return tls.LoadX509KeyPair(certPath, keyPath)
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := pem.Encode(f, &pem.Block{Type: typ, Bytes: der}); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
