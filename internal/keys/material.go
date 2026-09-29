package keys

import (
	"bytes"
	"crypto"
	"crypto/dsa" //nolint:staticcheck // legacy DSA keys are importable (TOOL-1)
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/md5"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
)

// Key material: detection and parsing of private keys in every supported format (OpenSSH, traditional PEM with or
// without legacy encryption, PKCS#8 with or without PBES2 encryption, PuTTY PPK v2/v3), key metadata and the
// public-key encodings used by the UI (TOOL-1).

// Private key formats.
const (
	formatOpenSSH = "openssh"
	formatPEM     = "pem"   // traditional PEM: PKCS#1 RSA, SEC1 EC, OpenSSL DSA
	formatPKCS8   = "pkcs8" // "PRIVATE KEY" / "ENCRYPTED PRIVATE KEY"
	formatPPK     = "ppk"
)

const (
	maxRSABits = 16384
	// maxKeyText bounds pasted/uploaded key text (a 16384-bit RSA key is about 13 KiB in any format).
	maxKeyText = 64 << 10
)

// parsedKey is a decrypted private key.
type parsedKey struct {
	Raw        crypto.PrivateKey // *rsa.PrivateKey, *ecdsa.PrivateKey, ed25519.PrivateKey or *dsa.PrivateKey
	Pub        ssh.PublicKey
	Comment    string // comment embedded in the file (OpenSSH, PPK), else ""
	Format     string
	Encrypted  bool
	PPKVersion int
}

// Signer returns an ssh.Signer for the key.
func (p *parsedKey) Signer() (ssh.Signer, error) { return ssh.NewSignerFromKey(p.Raw) }

// ---- errors -------------------------------------------------------------------------------------------------------

// keyError is a problem with user-supplied key material; it is rendered as 400 {code}.
type keyError struct{ code, msg string }

func (e *keyError) Error() string { return e.msg }

func invalidKey(msg string) error     { return &keyError{"invalid_key", msg} }
func unsupportedKey(msg string) error { return &keyError{"unsupported_key", msg} }

// passphraseError reports an encrypted key without (wrong=false) or with an incorrect (wrong=true) passphrase. pub is
// set when the format stores the public key unencrypted.
type passphraseError struct {
	pub   ssh.PublicKey
	wrong bool
}

func (e *passphraseError) Error() string {
	if e.wrong {
		return "incorrect passphrase"
	}
	return "the private key is encrypted: enter its passphrase"
}

// Error codes of key operations (400).
const (
	codePassphraseRequired = "passphrase_required"
	codeWrongPassphrase    = "wrong_passphrase"
)

// httpKeyError maps key parsing errors to typed HTTP errors (other errors pass through).
func httpKeyError(err error) error {
	var pe *passphraseError
	var ke *keyError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &pe):
		if pe.wrong {
			return httpx.NewError(http.StatusBadRequest, codeWrongPassphrase, pe.Error())
		}
		return httpx.NewError(http.StatusBadRequest, codePassphraseRequired, pe.Error())
	case errors.As(err, &ke):
		return httpx.NewError(http.StatusBadRequest, ke.code, ke.msg)
	}
	return err
}

func trimSSHErr(err error) string { return strings.TrimPrefix(err.Error(), "ssh: ") }

// publicKeyError classifies an unparsable embedded public key: unsupported types / sizes vs corrupt data.
func publicKeyError(where string, err error) error {
	msg := trimSSHErr(err)
	if strings.Contains(msg, "unsupported") || strings.Contains(msg, "unknown key") {
		return unsupportedKey(fmt.Sprintf("the %s uses an unsupported key: %s", where, msg))
	}
	return invalidKey(fmt.Sprintf("invalid public key in the %s: %s", where, msg))
}

// ---- parsing ------------------------------------------------------------------------------------------------------

// cleanKeyText trims whitespace and a UTF-8 BOM, and unifies line endings.
func cleanKeyText(data []byte) []byte {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
	return bytes.TrimSpace(data)
}

// parseKey parses a private key in any supported format, decrypting it with passphrase when needed.
func parseKey(data []byte, passphrase string) (*parsedKey, error) {
	text := cleanKeyText(data)
	switch {
	case len(text) == 0:
		return nil, invalidKey("no key given")
	case len(text) > maxKeyText:
		return nil, invalidKey("the key text is too large")
	case bytes.HasPrefix(text, []byte("PuTTY-User-Key-File-")):
		return parsePPK(text, passphrase)
	case bytes.HasPrefix(text, []byte("---- BEGIN SSH2 ENCRYPTED PRIVATE KEY ----")):
		return nil, unsupportedKey("SSH.COM (SECSH) private keys are not supported; convert them with ssh-keygen -i first")
	}
	block, rest := pem.Decode(text)
	for block != nil && block.Type == "EC PARAMETERS" { // `openssl ecparam -genkey` writes the curve first
		block, rest = pem.Decode(rest)
	}
	if block == nil {
		if _, _, err := parsePublicKeyText(string(text)); err == nil {
			return nil, &keyError{"public_key_only", "this is a public key; the private key is needed"}
		}
		return nil, invalidKey("unrecognized key format: expected an OpenSSH, PEM or PuTTY (.ppk) private key")
	}
	switch block.Type {
	case "OPENSSH PRIVATE KEY":
		return parseOpenSSHPrivateKey(block.Bytes, []byte(passphrase))
	case "RSA PRIVATE KEY", "EC PRIVATE KEY", "DSA PRIVATE KEY":
		return parseTraditionalPEM(block, passphrase)
	case "PRIVATE KEY":
		raw, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, invalidKey("invalid PKCS#8 private key: " + err.Error())
		}
		return newParsedKey(raw, "", formatPKCS8, false)
	case "ENCRYPTED PRIVATE KEY":
		if passphrase == "" {
			return nil, &passphraseError{}
		}
		raw, err := decryptPKCS8(block.Bytes, []byte(passphrase))
		if err != nil {
			return nil, err
		}
		return newParsedKey(raw, "", formatPKCS8, true)
	case "PUBLIC KEY", "RSA PUBLIC KEY", "SSH2 PUBLIC KEY", "CERTIFICATE":
		return nil, &keyError{"public_key_only", "this is a public key or certificate; the private key is needed"}
	}
	return nil, unsupportedKey(fmt.Sprintf("unsupported PEM block %q", block.Type))
}

// parseTraditionalPEM parses PKCS#1 / SEC1 / OpenSSL DSA keys, optionally protected by legacy PEM encryption.
func parseTraditionalPEM(block *pem.Block, passphrase string) (*parsedKey, error) {
	der := block.Bytes
	encrypted := x509.IsEncryptedPEMBlock(block) //nolint:staticcheck // reading legacy encrypted keys
	if encrypted {
		if passphrase == "" {
			return nil, &passphraseError{}
		}
		d, err := x509.DecryptPEMBlock(block, []byte(passphrase)) //nolint:staticcheck // reading legacy encrypted keys
		if err != nil {
			if errors.Is(err, x509.IncorrectPasswordError) {
				return nil, &passphraseError{wrong: true}
			}
			return nil, unsupportedKey("cannot decrypt the PEM key: " + err.Error())
		}
		defer clear(d) // the parsed key copies what it keeps
		der = d
	}
	var (
		raw crypto.PrivateKey
		err error
	)
	switch block.Type {
	case "RSA PRIVATE KEY":
		raw, err = x509.ParsePKCS1PrivateKey(der)
	case "EC PRIVATE KEY":
		raw, err = x509.ParseECPrivateKey(der)
	case "DSA PRIVATE KEY":
		raw, err = ssh.ParseDSAPrivateKey(der)
	}
	if err != nil {
		if encrypted {
			// Legacy PEM encryption cannot always detect a wrong password: the decrypted DER is noise.
			return nil, &passphraseError{wrong: true}
		}
		return nil, invalidKey("invalid PEM private key: " + err.Error())
	}
	return newParsedKey(raw, "", formatPEM, encrypted)
}

// newParsedKey canonicalizes and validates raw and derives its SSH public key.
func newParsedKey(raw crypto.PrivateKey, comment, format string, encrypted bool) (*parsedKey, error) {
	switch k := raw.(type) {
	case *ed25519.PrivateKey:
		raw = *k
	case ed25519.PrivateKey:
		if len(k) != ed25519.PrivateKeySize {
			return nil, invalidKey("invalid Ed25519 private key")
		}
	case *rsa.PrivateKey:
		if k.N.BitLen() > maxRSABits {
			return nil, unsupportedKey("RSA keys larger than 16384 bits are not supported")
		}
		if k.N.BitLen() < 1024 {
			return nil, unsupportedKey(fmt.Sprintf("%d-bit RSA keys are too weak to be used", k.N.BitLen()))
		}
	case *ecdsa.PrivateKey:
		if curveName(k.Curve) == "" {
			return nil, unsupportedKey("unsupported elliptic curve " + k.Curve.Params().Name)
		}
	case *dsa.PrivateKey:
		if err := validateDSA(k); err != nil {
			return nil, err
		}
	default:
		return nil, unsupportedKey(fmt.Sprintf("unsupported private key type %T", raw))
	}
	pub, err := ssh.NewPublicKey(publicOf(raw))
	if err != nil {
		return nil, invalidKey(trimSSHErr(err))
	}
	return &parsedKey{Raw: raw, Pub: pub, Comment: comment, Format: format, Encrypted: encrypted}, nil
}

// publicOf returns the crypto public key of a canonical private key.
func publicOf(raw crypto.PrivateKey) crypto.PublicKey {
	switch k := raw.(type) {
	case *rsa.PrivateKey:
		return &k.PublicKey
	case *ecdsa.PrivateKey:
		return &k.PublicKey
	case ed25519.PrivateKey:
		return k.Public()
	case *dsa.PrivateKey:
		return &k.PublicKey
	}
	return nil
}

func curveByName(name string) elliptic.Curve {
	switch name {
	case "nistp256":
		return elliptic.P256()
	case "nistp384":
		return elliptic.P384()
	case "nistp521":
		return elliptic.P521()
	}
	return nil
}

func curveName(c elliptic.Curve) string {
	switch c {
	case elliptic.P256():
		return "nistp256"
	case elliptic.P384():
		return "nistp384"
	case elliptic.P521():
		return "nistp521"
	}
	return ""
}

// ecdsaFromScalar builds an ECDSA key from its private scalar (validating the range).
func ecdsaFromScalar(curve elliptic.Curve, d *big.Int) (*ecdsa.PrivateKey, error) {
	size := (curve.Params().N.BitLen() + 7) / 8
	if d == nil || d.Sign() <= 0 || d.BitLen() > size*8 {
		return nil, errMalformed
	}
	return ecdsa.ParseRawPrivateKey(curve, d.FillBytes(make([]byte, size)))
}

// validateDSA enforces the only DSA parameters SSH supports (FIPS 186-2: 1024-bit p, 160-bit q) and checks y = g^x.
func validateDSA(k *dsa.PrivateKey) error {
	p := k.Parameters
	if p.P == nil || p.Q == nil || p.G == nil || k.Y == nil || k.X == nil {
		return invalidKey("invalid DSA key")
	}
	if p.P.BitLen() != 1024 || p.Q.BitLen() != 160 {
		return unsupportedKey("only 1024-bit DSA keys are supported by SSH")
	}
	if p.G.Sign() <= 0 || p.G.Cmp(p.P) >= 0 || k.Y.Sign() <= 0 || k.Y.Cmp(p.P) >= 0 || k.X.Sign() <= 0 || k.X.Cmp(p.Q) >= 0 {
		return invalidKey("invalid DSA key")
	}
	if new(big.Int).Exp(p.G, k.X, p.P).Cmp(k.Y) != 0 {
		return invalidKey("the DSA private key does not match its public key")
	}
	return nil
}

// ---- metadata -----------------------------------------------------------------------------------------------------

// keyTypeBits returns the short key type ("ed25519", "rsa", "ecdsa", "dsa", …) and the key size in bits.
func keyTypeBits(pub ssh.PublicKey) (string, int) {
	if cert, ok := pub.(*ssh.Certificate); ok {
		pub = cert.Key
	}
	switch pub.Type() {
	case ssh.KeyAlgoED25519:
		return "ed25519", 256
	case ssh.KeyAlgoSKED25519:
		return "ed25519-sk", 256
	case ssh.KeyAlgoSKECDSA256:
		return "ecdsa-sk", 256
	}
	cpk, ok := pub.(ssh.CryptoPublicKey)
	if !ok {
		return pub.Type(), 0
	}
	switch k := cpk.CryptoPublicKey().(type) {
	case *rsa.PublicKey:
		return "rsa", k.N.BitLen()
	case *ecdsa.PublicKey:
		return "ecdsa", k.Curve.Params().BitSize
	case *dsa.PublicKey:
		return "dsa", k.P.BitLen()
	case ed25519.PublicKey:
		return "ed25519", 256
	}
	return pub.Type(), 0
}

// fingerprintMD5 returns the legacy "MD5:aa:bb:…" fingerprint (the SHA256 one is ssh.FingerprintSHA256).
func fingerprintMD5(pub ssh.PublicKey) string {
	sum := md5.Sum(pub.Marshal())
	h := hex.EncodeToString(sum[:])
	var b strings.Builder
	b.WriteString("MD5:")
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String()
}

// ---- comments and public encodings ------------------------------------------------------------------------------

const maxCommentLen = 1024

// cleanComment normalizes a key comment: single line, no control characters, bounded length.
func cleanComment(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	for len(s) > maxCommentLen {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// authorizedKeyLine renders pub as an authorized_keys line ("type base64 comment").
func authorizedKeyLine(pub ssh.PublicKey, comment string) string {
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	if c := cleanComment(comment); c != "" {
		line += " " + c
	}
	return line
}

// rfc4716 renders pub in the SECSH public key file format (RFC 4716), as ssh-keygen -e / PuTTYgen do.
func rfc4716(pub ssh.PublicKey, comment string) string {
	var b strings.Builder
	b.WriteString("---- BEGIN SSH2 PUBLIC KEY ----\n")
	if c := cleanComment(comment); c != "" {
		header := `Comment: "` + strings.ReplaceAll(c, `"`, `'`) + `"`
		// Header lines are at most 72 bytes; longer ones continue after a backslash.
		for len(header) > 70 {
			cut := 70
			for cut > 0 && !utf8.RuneStart(header[cut]) {
				cut--
			}
			b.WriteString(header[:cut] + "\\\n")
			header = header[cut:]
		}
		b.WriteString(header + "\n")
	}
	enc := base64.StdEncoding.EncodeToString(pub.Marshal())
	for len(enc) > 64 {
		b.WriteString(enc[:64] + "\n")
		enc = enc[64:]
	}
	b.WriteString(enc + "\n---- END SSH2 PUBLIC KEY ----\n")
	return b.String()
}

// parsePublicKeyText parses a public key (or certificate) given as an authorized_keys line (options allowed), a bare
// "type base64" pair, a bare base64 wire blob or an RFC 4716 block. It returns the key and its comment.
func parsePublicKeyText(s string) (ssh.PublicKey, string, error) {
	s = strings.TrimSpace(strings.TrimPrefix(s, "\uFEFF"))
	if s == "" {
		return nil, "", errors.New("no public key given")
	}
	if strings.HasPrefix(s, "---- BEGIN SSH2 PUBLIC KEY ----") {
		return parseRFC4716(s)
	}
	if pk, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(s)); err == nil {
		return pk, comment, nil
	}
	fields := strings.Fields(s)
	if len(fields) == 1 {
		raw, err := base64.StdEncoding.DecodeString(fields[0])
		if err == nil {
			if pk, err := ssh.ParsePublicKey(raw); err == nil {
				return pk, "", nil
			}
		}
	}
	return nil, "", errors.New("not a valid SSH public key")
}

func parseRFC4716(s string) (ssh.PublicKey, string, error) {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	var (
		body    strings.Builder
		comment string
		inBody  bool
		cont    string
	)
	for _, raw := range lines[1:] {
		line := strings.TrimSpace(raw)
		if line == "---- END SSH2 PUBLIC KEY ----" {
			pk, err := decodePublicBlob(body.String())
			return pk, comment, err
		}
		if !inBody {
			if cont != "" || strings.Contains(line, ":") {
				full := cont + line
				if strings.HasSuffix(full, "\\") {
					cont = strings.TrimSuffix(full, "\\")
					continue
				}
				cont = ""
				if name, value, ok := strings.Cut(full, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "comment") {
					comment = strings.Trim(strings.TrimSpace(value), `"`)
				}
				continue
			}
			inBody = true
		}
		body.WriteString(line)
	}
	return nil, "", errors.New("unterminated SSH2 public key block")
}

func decodePublicBlob(b64 string) (ssh.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, errors.New("invalid base64 in public key")
	}
	pk, err := ssh.ParsePublicKey(raw)
	if err != nil {
		return nil, errors.New(trimSSHErr(err))
	}
	return pk, nil
}

// sameKey reports whether a and b are the same public key (wire encoding).
func sameKey(a, b ssh.PublicKey) bool {
	return a != nil && b != nil && bytes.Equal(a.Marshal(), b.Marshal())
}
