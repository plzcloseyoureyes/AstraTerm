package keys

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/dsa" //nolint:staticcheck // legacy DSA keys (import / conversion only)
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"math/big"
	"strings"

	"github.com/kayrus/putty"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/ssh"
)

// PuTTY private key files (PPK v2 and v3). Reading uses github.com/kayrus/putty after bounding the Argon2 cost of v3
// files (an unbounded Argon2-Memory header would let a crafted file exhaust the server's memory); writing is our own,
// following the format description in appendix C of the PuTTY manual.

// Argon2id parameters of exported PPK v3 files: PuTTYgen's defaults (8 MiB, one lane) with passes chosen for about
// 100 ms, which is what PuTTYgen calibrates to.
const (
	ppkArgon2MemoryKiB   = 8192
	ppkArgon2Passes      = 16
	ppkArgon2Parallelism = 1

	maxPPKArgon2MemoryKiB = 128 << 10
	maxPPKArgon2Passes    = 64
	maxPPKArgon2Lanes     = 8
)

// parsePPK parses (and, with passphrase, decrypts) a PuTTY private key.
func parsePPK(text []byte, passphrase string) (*parsedKey, error) {
	k, err := putty.New(text)
	if err != nil {
		return nil, invalidKey("invalid PuTTY key: " + err.Error())
	}
	pub, err := ssh.ParsePublicKey(k.PublicKey)
	if err != nil {
		return nil, publicKeyError("PuTTY key", err)
	}
	encrypted := k.Encryption != "" && k.Encryption != "none"
	if encrypted && k.Version >= 3 {
		if k.Argon2Memory > maxPPKArgon2MemoryKiB || k.Argon2Passes > maxPPKArgon2Passes || k.Argon2Parallelism > maxPPKArgon2Lanes {
			return nil, unsupportedKey(fmt.Sprintf("the PuTTY key uses an excessive key derivation cost (%d KiB, %d passes, %d lanes)",
				k.Argon2Memory, k.Argon2Passes, k.Argon2Parallelism))
		}
	}
	if encrypted && passphrase == "" {
		return nil, &passphraseError{pub: pub}
	}
	var raw any
	parse := func() (err error) {
		raw, err = k.ParseRawPrivateKey([]byte(passphrase))
		return err
	}
	if encrypted && k.Version >= 3 {
		err = withKDF(parse) // Argon2 with the file's cost
	} else {
		err = parse()
	}
	if err != nil {
		if isKDFBusy(err) {
			return nil, err
		}
		if encrypted {
			return nil, &passphraseError{pub: pub, wrong: true}
		}
		return nil, invalidKey("invalid PuTTY key: " + err.Error())
	}
	pk, err := newParsedKey(raw, k.Comment, formatPPK, encrypted)
	if err != nil {
		return nil, err
	}
	if !sameKey(pk.Pub, pub) {
		return nil, invalidKey("the PuTTY private key does not match its public key")
	}
	pk.PPKVersion = k.Version
	return pk, nil
}

// ppkKDF are the Argon2id parameters of an encrypted PPK v3 file.
type ppkKDF struct {
	memoryKiB, passes uint32
	lanes             uint8
	salt              []byte
}

// marshalPPK encodes raw as a PuTTY key file of the given version (2 or 3), encrypted with aes256-cbc when a
// passphrase is given (v3: Argon2id with PuTTYgen's default memory and a random salt).
func marshalPPK(raw crypto.PrivateKey, comment string, passphrase []byte, version int) ([]byte, error) {
	var kdf *ppkKDF
	if version == 3 && len(passphrase) > 0 {
		kdf = &ppkKDF{memoryKiB: ppkArgon2MemoryKiB, passes: ppkArgon2Passes, lanes: ppkArgon2Parallelism, salt: make([]byte, 16)}
		if _, err := rand.Read(kdf.salt); err != nil {
			return nil, err
		}
	}
	return marshalPPKWith(raw, comment, passphrase, version, kdf)
}

// marshalPPKWith is marshalPPK with explicit v3 key derivation parameters. Everything else in a PPK file is
// deterministic (v2 derives its key from the passphrase alone, the CBC IV is derived too and the padding is the SHA-1 of
// the private blob), so tests can compare the output with puttygen's byte for byte.
func marshalPPKWith(raw crypto.PrivateKey, comment string, passphrase []byte, version int, kdfParams *ppkKDF) ([]byte, error) {
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported PPK version %d", version)
	}
	pub, err := ssh.NewPublicKey(publicOf(raw))
	if err != nil {
		return nil, err
	}
	priv, err := ppkPrivateBlob(raw)
	if err != nil {
		return nil, err
	}
	algo, pubBlob, comment := pub.Type(), pub.Marshal(), cleanComment(comment)
	encryption := "none"
	var (
		cipherKey, iv, macKey []byte
		kdf                   string
	)
	if len(passphrase) > 0 {
		encryption = "aes256-cbc"
		if version == 2 {
			cipherKey = append(ppkSHA1(0, passphrase), ppkSHA1(1, passphrase)...)[:32]
			iv = make([]byte, aes.BlockSize)
			m := sha1.Sum(append([]byte("putty-private-key-file-mac-key"), passphrase...))
			macKey = m[:]
		} else {
			if kdfParams == nil || len(kdfParams.salt) == 0 || kdfParams.passes == 0 || kdfParams.lanes == 0 {
				return nil, errors.New("PPK v3: missing key derivation parameters")
			}
			p := kdfParams
			h := argon2.IDKey(passphrase, p.salt, p.passes, p.memoryKiB, p.lanes, 80)
			cipherKey, iv, macKey = h[:32], h[32:48], h[48:]
			kdf = fmt.Sprintf("Key-Derivation: Argon2id\nArgon2-Memory: %d\nArgon2-Passes: %d\nArgon2-Parallelism: %d\nArgon2-Salt: %s\n",
				p.memoryKiB, p.passes, p.lanes, hex.EncodeToString(p.salt))
		}
		// Pad to the cipher block size with the SHA-1 of the unpadded blob, like PuTTY. A block-aligned blob still gets
		// a full padding block: PuTTY ignores trailing bytes, and kayrus/putty (used by sshx) requires them.
		sum := sha1.Sum(priv)
		priv = append(priv, sum[:aes.BlockSize-len(priv)%aes.BlockSize]...)
	} else if version == 2 {
		m := sha1.Sum([]byte("putty-private-key-file-mac-key"))
		macKey = m[:]
	}

	var mac hash.Hash
	if version == 2 {
		mac = hmac.New(sha1.New, macKey)
	} else {
		mac = hmac.New(sha256.New, macKey)
	}
	for _, field := range [][]byte{[]byte(algo), []byte(encryption), []byte(comment), pubBlob, priv} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(field)))
		mac.Write(n[:])
		mac.Write(field)
	}
	macHex := hex.EncodeToString(mac.Sum(nil))

	privOut := priv
	if encryption != "none" {
		block, err := aes.NewCipher(cipherKey)
		if err != nil {
			return nil, err
		}
		privOut = make([]byte, len(priv))
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(privOut, priv)
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "PuTTY-User-Key-File-%d: %s\n", version, algo)
	fmt.Fprintf(&b, "Encryption: %s\n", encryption)
	fmt.Fprintf(&b, "Comment: %s\n", comment)
	writePPKLines(&b, "Public-Lines", pubBlob)
	b.WriteString(kdf)
	writePPKLines(&b, "Private-Lines", privOut)
	fmt.Fprintf(&b, "Private-MAC: %s\n", macHex)
	return b.Bytes(), nil
}

// ppkSHA1 is SHA-1(uint32(seq) || passphrase), the PPK v2 cipher key derivation.
func ppkSHA1(seq uint32, passphrase []byte) []byte {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], seq)
	h := sha1.New()
	h.Write(n[:])
	h.Write(passphrase)
	return h.Sum(nil)
}

func writePPKLines(b *bytes.Buffer, header string, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	var lines []string
	for len(enc) > 64 {
		lines = append(lines, enc[:64])
		enc = enc[64:]
	}
	lines = append(lines, enc)
	fmt.Fprintf(b, "%s: %d\n%s\n", header, len(lines), strings.Join(lines, "\n"))
}

// ppkPrivateBlob returns PuTTY's private key blob of raw.
func ppkPrivateBlob(raw crypto.PrivateKey) ([]byte, error) {
	switch k := raw.(type) {
	case *rsa.PrivateKey:
		if len(k.Primes) != 2 {
			return nil, unsupportedKey("multi-prime RSA keys cannot be saved in PuTTY format")
		}
		p, q := k.Primes[0], k.Primes[1]
		iqmp := new(big.Int).ModInverse(q, p) // PuTTY: iqmp = q^-1 mod p
		if iqmp == nil {
			return nil, invalidKey("invalid RSA key")
		}
		return ssh.Marshal(struct{ D, P, Q, Iqmp *big.Int }{k.D, p, q, iqmp}), nil
	case *dsa.PrivateKey:
		return ssh.Marshal(struct{ X *big.Int }{k.X}), nil
	case *ecdsa.PrivateKey:
		return ssh.Marshal(struct{ D *big.Int }{k.D}), nil
	case ed25519.PrivateKey:
		// The 32-byte seed as a string (PuTTY reads it as a little-endian integer; kayrus as raw bytes).
		return ssh.Marshal(struct{ Seed []byte }{k.Seed()}), nil
	}
	return nil, unsupportedKey(fmt.Sprintf("unsupported private key type %T", raw))
}
