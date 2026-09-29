package keys

import (
	"crypto/dsa" //nolint:staticcheck // legacy DSA keys (conversion only)
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Export formats (GET/POST /api/keys/{id}/export, drafts, /api/keys/convert).
const (
	exportOpenSSH = "openssh" // OpenSSH private key (ssh-keygen's default format)
	exportPPK     = "ppk"     // PuTTY private key, v3 (default) or v2
	exportPEM     = "pem"     // traditional PEM: PKCS#1 RSA / SEC1 EC / OpenSSL DSA (PKCS#8 for Ed25519)
	exportPKCS8   = "pkcs8"   // PKCS#8, PBES2-encrypted with a passphrase
	exportPublic  = "public"  // authorized_keys line
	exportRFC4716 = "rfc4716" // SECSH public key file
)

func isPrivateFormat(f string) bool {
	switch f {
	case exportOpenSSH, exportPPK, exportPEM, exportPKCS8:
		return true
	}
	return false
}

func isPublicFormat(f string) bool { return f == exportPublic || f == exportRFC4716 }

// exportResult is the body of export responses.
type exportResult struct {
	Content  string `json:"content"`
	Filename string `json:"filename"`
	Mime     string `json:"mime"`
	// Encrypted reports whether a private key output is passphrase-protected.
	Encrypted bool `json:"encrypted"`
}

// exportPrivateKey renders a decrypted key in a private format, encrypted with passphrase when it is not empty (the
// key derivation then runs in a KDF slot, see kdf.go).
func exportPrivateKey(p *parsedKey, format, comment, passphrase string, ppkVersion int, base string) (*exportResult, error) {
	if passphrase == "" {
		return encodePrivateKey(p, format, comment, "", ppkVersion, base)
	}
	var (
		res *exportResult
		err error
	)
	if kerr := withKDF(func() error {
		res, err = encodePrivateKey(p, format, comment, passphrase, ppkVersion, base)
		return nil
	}); kerr != nil {
		return nil, kerr
	}
	return res, err
}

func encodePrivateKey(p *parsedKey, format, comment, passphrase string, ppkVersion int, base string) (*exportResult, error) {
	pass := []byte(passphrase)
	res := &exportResult{Mime: "application/x-pem-file", Encrypted: passphrase != ""}
	switch format {
	case exportOpenSSH:
		out, err := marshalOpenSSHPrivateKey(p.Raw, cleanComment(comment), pass)
		if err != nil {
			return nil, err
		}
		res.Content, res.Filename = string(out), base
	case exportPPK:
		if ppkVersion == 0 {
			ppkVersion = 3
		}
		if ppkVersion != 2 && ppkVersion != 3 {
			return nil, invalidKey("PPK version must be 2 or 3")
		}
		out, err := marshalPPK(p.Raw, comment, pass, ppkVersion)
		if err != nil {
			return nil, err
		}
		res.Content, res.Filename, res.Mime = string(out), base+".ppk", "application/octet-stream"
	case exportPEM:
		block, err := traditionalPEM(p, pass)
		if err != nil {
			return nil, err
		}
		res.Content, res.Filename = string(pem.EncodeToMemory(block)), base+".pem"
	case exportPKCS8:
		if _, ok := p.Raw.(*dsa.PrivateKey); ok {
			return nil, unsupportedKey("DSA keys cannot be saved as PKCS#8; use the PEM or PuTTY format")
		}
		var block *pem.Block
		if len(pass) > 0 {
			b, err := encryptPKCS8(p.Raw, pass)
			if err != nil {
				return nil, err
			}
			block = b
		} else {
			der, err := x509.MarshalPKCS8PrivateKey(p.Raw)
			if err != nil {
				return nil, err
			}
			block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
		}
		res.Content, res.Filename = string(pem.EncodeToMemory(block)), base+".p8.pem"
	default:
		return nil, invalidKey(fmt.Sprintf("unknown private key format %q", format))
	}
	return res, nil
}

// traditionalPEM encodes RSA (PKCS#1), ECDSA (SEC1) and DSA (OpenSSL) keys with legacy PEM encryption, as
// `ssh-keygen -m PEM` does; Ed25519 has no traditional encoding and uses PKCS#8.
func traditionalPEM(p *parsedKey, pass []byte) (*pem.Block, error) {
	var (
		typ string
		der []byte
		err error
	)
	switch k := p.Raw.(type) {
	case *rsa.PrivateKey:
		typ, der = "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(k)
	case *ecdsa.PrivateKey:
		typ = "EC PRIVATE KEY"
		der, err = x509.MarshalECPrivateKey(k)
	case *dsa.PrivateKey:
		typ = "DSA PRIVATE KEY"
		der, err = asn1.Marshal(struct {
			Version       int
			P, Q, G, Y, X *big.Int
		}{0, k.P, k.Q, k.G, k.Y, k.X})
	case ed25519.PrivateKey:
		if len(pass) > 0 {
			return encryptPKCS8(k, pass)
		}
		d, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			return nil, err
		}
		return &pem.Block{Type: "PRIVATE KEY", Bytes: d}, nil
	default:
		return nil, unsupportedKey(fmt.Sprintf("unsupported private key type %T", p.Raw))
	}
	if err != nil {
		return nil, err
	}
	if len(pass) == 0 {
		return &pem.Block{Type: typ, Bytes: der}, nil
	}
	return x509.EncryptPEMBlock(rand.Reader, typ, der, pass, x509.PEMCipherAES256) //nolint:staticcheck // the traditional PEM format only has legacy encryption
}

// exportPublicKey renders a public key (or certificate) in a public format.
func exportPublicKey(pub ssh.PublicKey, format, comment, base string) (*exportResult, error) {
	switch format {
	case exportPublic:
		return &exportResult{Content: authorizedKeyLine(pub, comment) + "\n", Filename: base + ".pub", Mime: "text/plain"}, nil
	case exportRFC4716:
		return &exportResult{Content: rfc4716(pub, comment), Filename: base + ".rfc4716.pub", Mime: "text/plain"}, nil
	}
	return nil, invalidKey(fmt.Sprintf("unknown public key format %q", format))
}

// fileBase derives a safe download file name (without extension) from a key name, e.g. "Work laptop" → "work-laptop";
// empty names become id_<type> like ssh-keygen's defaults.
func fileBase(name, keyType string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
		if b.Len() >= 64 {
			break
		}
	}
	s := strings.Trim(b.String(), "-.")
	if s == "" {
		if keyType == "" {
			keyType = "key"
		}
		return "id_" + keyType
	}
	return s
}
