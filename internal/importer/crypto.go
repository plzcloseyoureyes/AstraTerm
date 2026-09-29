package importer

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
)

// Passphrase-based encryption for exports that include secrets (IMP-3) and encrypted backups (IMP-4): argon2id
// derives a 32-byte key from the passphrase and a random salt; XChaCha20-Poly1305 seals the payload. The envelope is
// self-describing JSON so a future decrypt knows the KDF parameters.

const (
	encEnvelopeType = "astraterm-encrypted"
	encAlg          = "argon2id+xchacha20poly1305"
	// minPassphrase is the minimum length of a passphrase that protects exported secrets or a backup.
	minPassphrase = 8
	maxPassphrase = 1024
)

// Bounds on the KDF parameters an uploaded envelope may ask for: the envelope is attacker-controlled input, and
// argon2 allocates its memory parameter up front (AstraTerm itself writes 64 MiB / 3 passes).
const (
	maxKDFMemKiB  = 256 << 10
	maxKDFTime    = 16
	maxKDFThreads = 16
)

// exportKDF mirrors the vault's argon2id defaults; tests may lower them.
var exportKDF = struct {
	Time    uint32
	MemKiB  uint32
	Threads uint8
}{Time: 3, MemKiB: 64 * 1024, Threads: 4}

// kdfSlots bounds concurrent argon2 derivations (each may use up to maxKDFMemKiB); a request that waits longer than
// kdfWait gets 429.
var (
	kdfSlots = make(chan struct{}, 2)
	kdfWait  = 20 * time.Second
)

// deriveKey runs argon2id under the process-wide slot limit.
func deriveKey(passphrase string, salt []byte, t, memKiB uint32, threads uint8) ([]byte, error) {
	timer := time.NewTimer(kdfWait)
	defer timer.Stop()
	select {
	case kdfSlots <- struct{}{}:
	case <-timer.C:
		return nil, httpx.TooManyRequests("the server is busy deriving encryption keys; try again shortly", 5)
	}
	defer func() { <-kdfSlots }()
	return argon2.IDKey([]byte(passphrase), salt, t, memKiB, threads, chacha20poly1305.KeySize), nil
}

// encEnvelope is the on-disk container for an encrypted export/backup.
type encEnvelope struct {
	Envelope string    `json:"envelope"` // encEnvelopeType
	Alg      string    `json:"alg"`
	Payload  string    `json:"payload"` // what the plaintext is ("astraterm-export" | "astraterm-backup")
	KDF      encKDFDoc `json:"kdf"`
	Nonce    string    `json:"nonce"`      // base64 (24 bytes)
	Cipher   string    `json:"ciphertext"` // base64
}

type encKDFDoc struct {
	Alg     string `json:"alg"`
	Time    uint32 `json:"t"`
	MemKiB  uint32 `json:"m"`
	Threads uint8  `json:"p"`
	Salt    string `json:"salt"` // base64 (16 bytes)
}

// checkNewPassphrase validates a passphrase chosen to protect exported data.
func checkNewPassphrase(passphrase string) error {
	n := utf8.RuneCountInString(passphrase)
	if n < minPassphrase {
		return badRequest("the passphrase must be at least 8 characters long")
	}
	if len(passphrase) > maxPassphrase {
		return badRequest("the passphrase is too long")
	}
	return nil
}

// encrypt seals plaintext with passphrase, tagging the envelope with payloadKind. The result is indented JSON.
func encrypt(plaintext []byte, passphrase, payloadKind string) ([]byte, error) {
	if err := checkNewPassphrase(passphrase); err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	key, err := deriveKey(passphrase, salt, exportKDF.Time, exportKDF.MemKiB, exportKDF.Threads)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ct := aead.Seal(nil, nonce, plaintext, []byte(encEnvelopeType))
	env := encEnvelope{
		Envelope: encEnvelopeType,
		Alg:      encAlg,
		Payload:  payloadKind,
		KDF: encKDFDoc{
			Alg: "argon2id", Time: exportKDF.Time, MemKiB: exportKDF.MemKiB, Threads: exportKDF.Threads,
			Salt: base64.StdEncoding.EncodeToString(salt),
		},
		Nonce:  base64.StdEncoding.EncodeToString(nonce),
		Cipher: base64.StdEncoding.EncodeToString(ct),
	}
	return json.MarshalIndent(env, "", "  ")
}

// decrypt opens an encEnvelope with passphrase, returning the plaintext and the payload kind.
func decrypt(envelope []byte, passphrase string) ([]byte, string, error) {
	var env encEnvelope
	if err := json.Unmarshal(envelope, &env); err != nil {
		return nil, "", badRequest("not a valid encrypted AstraTerm file")
	}
	if env.Envelope != encEnvelopeType {
		return nil, "", badRequest("not a AstraTerm encrypted envelope")
	}
	if env.Alg != "" && env.Alg != encAlg {
		return nil, "", badRequest("unsupported encryption algorithm")
	}
	if env.KDF.Alg != "argon2id" {
		return nil, "", badRequest("unsupported key-derivation algorithm")
	}
	if env.KDF.Time == 0 || env.KDF.MemKiB < 8 || env.KDF.Threads == 0 ||
		env.KDF.Time > maxKDFTime || env.KDF.MemKiB > maxKDFMemKiB || env.KDF.Threads > maxKDFThreads {
		return nil, "", badRequest("invalid key-derivation parameters")
	}
	if passphrase == "" || len(passphrase) > maxPassphrase {
		return nil, "", badRequest("wrong passphrase or corrupt file")
	}
	salt, err := base64.StdEncoding.DecodeString(env.KDF.Salt)
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return nil, "", badRequest("invalid salt")
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil || len(nonce) != chacha20poly1305.NonceSizeX {
		return nil, "", badRequest("invalid nonce")
	}
	ct, err := base64.StdEncoding.DecodeString(env.Cipher)
	if err != nil {
		return nil, "", badRequest("invalid ciphertext")
	}
	key, err := deriveKey(passphrase, salt, env.KDF.Time, env.KDF.MemKiB, env.KDF.Threads)
	if err != nil {
		return nil, "", err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, "", err
	}
	pt, err := aead.Open(nil, nonce, ct, []byte(encEnvelopeType))
	if err != nil {
		return nil, "", httpx.NewError(http.StatusForbidden, "wrong_password", "wrong passphrase (or the file is corrupt)")
	}
	return pt, env.Payload, nil
}

// isEncryptedEnvelope reports whether content is an encEnvelope (cheap check before asking for a passphrase).
func isEncryptedEnvelope(content []byte) bool {
	var probe struct {
		Envelope string `json:"envelope"`
	}
	return json.Unmarshal(content, &probe) == nil && probe.Envelope == encEnvelopeType
}
