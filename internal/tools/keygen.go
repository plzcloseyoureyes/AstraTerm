package tools

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
	"golang.org/x/crypto/ssh"

	"github.com/nexterm/nexterm/internal/httpx"
)

type keygenRequest struct {
	Type       string `json:"type"` // ed25519 | rsa | ecdsa
	Bits       int    `json:"bits"`
	Comment    string `json:"comment"`
	Passphrase string `json:"passphrase"`
}

type keygenResponse struct {
	Type           string `json:"type"`
	Bits           int    `json:"bits"`
	PublicKey      string `json:"publicKey"`      // authorized_keys line
	PrivateKey     string `json:"privateKey"`     // OpenSSH PEM (encrypted when a passphrase is given)
	Fingerprint    string `json:"fingerprint"`    // SHA256:…
	FingerprintMD5 string `json:"fingerprintMd5"` // MD5:aa:bb:…
	Comment        string `json:"comment"`
}

// keygen generates a fresh SSH key pair and returns the material WITHOUT storing it (SPEC §6.0
// POST /api/tools/keygen). The keys module owns persistent key storage; this is the store-less generator used by
// the tools UI.
func (h *handler) keygen(c *echo.Context) error {
	var req keygenRequest
	if err := httpx.BindOptional(c, &req); err != nil {
		return err
	}
	typ := strings.ToLower(strings.TrimSpace(req.Type))
	if typ == "" {
		typ = "ed25519"
	}

	var (
		signerKey any
		pub       ssh.PublicKey
		bits      int
		err       error
	)
	switch typ {
	case "ed25519":
		pk, sk, gerr := ed25519.GenerateKey(rand.Reader)
		if gerr != nil {
			return httpx.Internal(gerr)
		}
		signerKey = sk
		if pub, err = ssh.NewPublicKey(pk); err != nil {
			return httpx.Internal(err)
		}
		bits = 256
	case "rsa":
		bits = req.Bits
		if bits == 0 {
			bits = 4096
		}
		if bits < 2048 || bits > 8192 {
			return httpx.BadRequest("RSA key size must be between 2048 and 8192 bits")
		}
		// RSA generation is CPU-heavy (seconds for 8192 bits): bound concurrent generations.
		if h.keygenSem != nil {
			select {
			case h.keygenSem <- struct{}{}:
				defer func() { <-h.keygenSem }()
			case <-c.Request().Context().Done():
				return c.Request().Context().Err()
			}
		}
		key, gerr := rsa.GenerateKey(rand.Reader, bits)
		if gerr != nil {
			return httpx.Internal(gerr)
		}
		signerKey = key
		if pub, err = ssh.NewPublicKey(&key.PublicKey); err != nil {
			return httpx.Internal(err)
		}
	case "ecdsa":
		bits = req.Bits
		if bits == 0 {
			bits = 256
		}
		var curve elliptic.Curve
		switch bits {
		case 256:
			curve = elliptic.P256()
		case 384:
			curve = elliptic.P384()
		case 521:
			curve = elliptic.P521()
		default:
			return httpx.BadRequest("ECDSA key size must be 256, 384 or 521")
		}
		key, gerr := ecdsa.GenerateKey(curve, rand.Reader)
		if gerr != nil {
			return httpx.Internal(gerr)
		}
		signerKey = key
		if pub, err = ssh.NewPublicKey(&key.PublicKey); err != nil {
			return httpx.Internal(err)
		}
	default:
		return httpx.BadRequest("unsupported key type: " + typ)
	}

	if len(req.Comment) > 256 || strings.ContainsAny(req.Comment, "\r\n") {
		return httpx.BadRequest("invalid comment")
	}
	var block *pem.Block
	if req.Passphrase != "" {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(signerKey, req.Comment, []byte(req.Passphrase))
	} else {
		block, err = ssh.MarshalPrivateKey(signerKey, req.Comment)
	}
	if err != nil {
		return httpx.Internal(err)
	}

	authLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	if req.Comment != "" {
		authLine += " " + req.Comment
	}
	return c.JSON(http.StatusOK, keygenResponse{
		Type:           typ,
		Bits:           bits,
		PublicKey:      authLine,
		PrivateKey:     string(pem.EncodeToMemory(block)),
		Fingerprint:    ssh.FingerprintSHA256(pub),
		FingerprintMD5: "MD5:" + ssh.FingerprintLegacyMD5(pub),
		Comment:        req.Comment,
	})
}
