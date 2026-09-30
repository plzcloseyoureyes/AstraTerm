package keys

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/dsa" //nolint:staticcheck // DSA keys can still be imported (TOOL-1 "DSA import only").
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"

	"golang.org/x/crypto/blowfish"
	"golang.org/x/crypto/chacha20"
	"golang.org/x/crypto/poly1305" //nolint:staticcheck // the one-time-key Poly1305 of OpenSSH's chacha20-poly1305 construction
	"golang.org/x/crypto/ssh"
)

// OpenSSH private key format ("openssh-key-v1", OpenSSH PROTOCOL.key). x/crypto parses it too, but it drops the
// embedded comment, only knows two ciphers (aes256-ctr / aes256-cbc) and cannot read or write ssh-dss keys, so imports
// use this parser — which reads every cipher ssh-keygen -Z offers, including the AEAD ones (aes128/256-gcm@openssh.com,
// chacha20-poly1305@openssh.com) — and DSA exports use this writer. RSA, ECDSA and Ed25519 exports go through
// ssh.MarshalPrivateKeyWithPassphrase (aes256-ctr + bcrypt, ssh-keygen's default).

const opensshMagic = "openssh-key-v1\x00"

// maxBcryptRounds bounds the bcrypt KDF cost of imported keys (ssh-keygen's default is 16; the cost is linear).
const maxBcryptRounds = 1 << 11

type opensshEnvelope struct {
	CipherName   string
	KdfName      string
	KdfOpts      string
	NumKeys      uint32
	PubKey       []byte
	PrivKeyBlock []byte
	Rest         []byte `ssh:"rest"`
}

type opensshKDFOpts struct {
	Salt   []byte
	Rounds uint32
}

type opensshPrivHeader struct {
	Check1  uint32
	Check2  uint32
	Keytype string
	Rest    []byte `ssh:"rest"`
}

type opensshRSA struct {
	N, E, D, Iqmp, P, Q *big.Int
	Comment             string
	Pad                 []byte `ssh:"rest"`
}

type opensshEd25519 struct {
	Pub, Priv []byte
	Comment   string
	Pad       []byte `ssh:"rest"`
}

type opensshECDSA struct {
	Curve   string
	Pub     []byte
	D       *big.Int
	Comment string
	Pad     []byte `ssh:"rest"`
}

type opensshDSA struct {
	P, Q, G, Y, X *big.Int
	Comment       string
	Pad           []byte `ssh:"rest"`
}

// opensshCipher describes a symmetric cipher usable for key files (OpenSSH cipher.c: key / IV / block / tag sizes).
type opensshCipher struct {
	mode    cipherMode
	keyLen  int
	ivLen   int
	block   int // the private section is padded to this size
	authLen int // AEAD tag appended after the encrypted section (outside its length)
}

type cipherMode int

const (
	modeCTR cipherMode = iota
	modeCBC
	modeGCM
	modeChaChaPoly
)

var opensshCiphers = map[string]opensshCipher{
	"aes128-ctr":                    {modeCTR, 16, 16, 16, 0},
	"aes192-ctr":                    {modeCTR, 24, 16, 16, 0},
	"aes256-ctr":                    {modeCTR, 32, 16, 16, 0},
	"aes128-cbc":                    {modeCBC, 16, 16, 16, 0},
	"aes192-cbc":                    {modeCBC, 24, 16, 16, 0},
	"aes256-cbc":                    {modeCBC, 32, 16, 16, 0},
	"3des-cbc":                      {modeCBC, 24, 8, 8, 0},
	"aes128-gcm@openssh.com":        {modeGCM, 16, 12, 16, 16},
	"aes256-gcm@openssh.com":        {modeGCM, 32, 12, 16, 16},
	"chacha20-poly1305@openssh.com": {modeChaChaPoly, 64, 0, 8, 16},
}

// errAuthFailed: an AEAD tag did not verify — with a key file, that means a wrong passphrase.
var errAuthFailed = errors.New("message authentication failed")

// decrypt decrypts the private section of a key file with the bcrypt-derived key+IV kiv; tag is the AEAD tag that
// follows the section (nil for the other modes). Like OpenSSH, the AEAD modes use the derived IV (GCM) or sequence
// number 0 (chacha20-poly1305) and no additional data.
func (c opensshCipher) decrypt(kiv, block, tag []byte) ([]byte, error) {
	key, iv := kiv[:c.keyLen], kiv[c.keyLen:c.keyLen+c.ivLen]
	plain := make([]byte, len(block))
	switch c.mode {
	case modeCTR, modeCBC:
		var (
			bc  cipher.Block
			err error
		)
		if c.keyLen == 24 && c.block == 8 {
			bc, err = des.NewTripleDESCipher(key)
		} else {
			bc, err = aes.NewCipher(key)
		}
		if err != nil {
			return nil, err
		}
		if c.mode == modeCBC {
			cipher.NewCBCDecrypter(bc, iv).CryptBlocks(plain, block)
		} else {
			cipher.NewCTR(bc, iv).XORKeyStream(plain, block)
		}
	case modeGCM:
		bc, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		aead, err := cipher.NewGCM(bc)
		if err != nil {
			return nil, err
		}
		sealed := append(append(make([]byte, 0, len(block)+len(tag)), block...), tag...)
		if plain, err = aead.Open(plain[:0], iv, sealed, nil); err != nil {
			return nil, errAuthFailed
		}
	case modeChaChaPoly:
		// OpenSSH cipher-chachapoly.c: K_1 (the first 32 bytes) encrypts the payload from block counter 1; the
		// Poly1305 key is the first 32 bytes of K_1's keystream at counter 0; the nonce is the sequence number (0).
		var nonce [chacha20.NonceSize]byte
		s, err := chacha20.NewUnauthenticatedCipher(key[:chacha20.KeySize], nonce[:])
		if err != nil {
			return nil, err
		}
		var polyKey, discard [32]byte
		s.XORKeyStream(polyKey[:], polyKey[:])
		s.XORKeyStream(discard[:], discard[:]) // the rest of block 0
		var mac [poly1305.TagSize]byte
		copy(mac[:], tag)
		if len(tag) != poly1305.TagSize || !poly1305.Verify(&mac, block, &polyKey) {
			return nil, errAuthFailed
		}
		s.XORKeyStream(plain, block)
	default:
		return nil, errors.New("unsupported cipher mode")
	}
	return plain, nil
}

// parseOpenSSHPrivateKey parses the body of an "OPENSSH PRIVATE KEY" PEM block. An encrypted key without passphrase
// yields a *passphraseError carrying the (unencrypted) public key.
func parseOpenSSHPrivateKey(der []byte, passphrase []byte) (*parsedKey, error) {
	if len(der) < len(opensshMagic) || string(der[:len(opensshMagic)]) != opensshMagic {
		return nil, invalidKey("invalid OpenSSH private key")
	}
	var env opensshEnvelope
	if err := ssh.Unmarshal(der[len(opensshMagic):], &env); err != nil {
		return nil, invalidKey("invalid OpenSSH private key")
	}
	if env.NumKeys != 1 {
		return nil, invalidKey("OpenSSH key files with several keys are not supported")
	}
	pub, err := ssh.ParsePublicKey(env.PubKey)
	if err != nil {
		return nil, publicKeyError("OpenSSH private key", err)
	}
	encrypted := env.CipherName != "none"
	block := env.PrivKeyBlock
	if encrypted {
		c, ok := opensshCiphers[env.CipherName]
		if !ok {
			return nil, unsupportedKey(fmt.Sprintf("the OpenSSH key is encrypted with %q, which is not supported", env.CipherName))
		}
		if env.KdfName != "bcrypt" {
			return nil, unsupportedKey(fmt.Sprintf("unsupported key derivation %q", env.KdfName))
		}
		var opts opensshKDFOpts
		if err := ssh.Unmarshal([]byte(env.KdfOpts), &opts); err != nil {
			return nil, invalidKey("invalid OpenSSH key derivation options")
		}
		if opts.Rounds == 0 || opts.Rounds > maxBcryptRounds {
			return nil, unsupportedKey(fmt.Sprintf("the key derivation uses %d rounds (at most %d are supported)", opts.Rounds, maxBcryptRounds))
		}
		// The AEAD tag follows the encrypted section, outside its length (OpenSSH sshkey.c private2_decrypt).
		if len(block) == 0 || len(block)%c.block != 0 || len(env.Rest) != c.authLen {
			return nil, invalidKey("invalid OpenSSH private key length")
		}
		if len(passphrase) == 0 {
			return nil, &passphraseError{pub: pub}
		}
		var kiv []byte
		err := withKDF(func() (err error) {
			kiv, err = bcryptPBKDF(passphrase, opts.Salt, int(opts.Rounds), c.keyLen+c.ivLen)
			return err
		})
		if err != nil {
			if errors.Is(err, errKDFBusy) {
				return nil, err
			}
			return nil, invalidKey("invalid OpenSSH key derivation options")
		}
		plain, err := c.decrypt(kiv, block, env.Rest)
		clear(kiv)
		if err != nil {
			if errors.Is(err, errAuthFailed) {
				return nil, &passphraseError{pub: pub, wrong: true}
			}
			return nil, err
		}
		defer clear(plain)
		block = plain
	} else if env.KdfName != "none" {
		return nil, invalidKey("invalid OpenSSH private key (unencrypted key with a key derivation)")
	} else if len(env.Rest) != 0 {
		return nil, invalidKey("invalid OpenSSH private key (trailing data)")
	}

	var hdr opensshPrivHeader
	if err := ssh.Unmarshal(block, &hdr); err != nil || hdr.Check1 != hdr.Check2 {
		if encrypted {
			return nil, &passphraseError{pub: pub, wrong: true}
		}
		return nil, invalidKey("malformed OpenSSH private key")
	}
	raw, comment, pad, err := decodeOpenSSHPrivate(hdr.Keytype, hdr.Rest)
	if err != nil {
		if encrypted && errors.Is(err, errMalformed) {
			return nil, &passphraseError{pub: pub, wrong: true}
		}
		return nil, err
	}
	for i, b := range pad {
		if int(b) != i+1 {
			return nil, invalidKey("malformed OpenSSH private key (padding)")
		}
	}
	derived, err := ssh.NewPublicKey(publicOf(raw))
	if err != nil {
		return nil, invalidKey(trimSSHErr(err))
	}
	if string(derived.Marshal()) != string(pub.Marshal()) {
		return nil, invalidKey("the OpenSSH private key does not match its public key")
	}
	return &parsedKey{Raw: raw, Pub: derived, Comment: comment, Format: formatOpenSSH, Encrypted: encrypted}, nil
}

var errMalformed = errors.New("malformed private key")

// decodeOpenSSHPrivate decodes the type-specific private fields and validates them.
func decodeOpenSSHPrivate(keytype string, rest []byte) (crypto.PrivateKey, string, []byte, error) {
	switch keytype {
	case ssh.KeyAlgoRSA:
		var k opensshRSA
		if err := ssh.Unmarshal(rest, &k); err != nil {
			return nil, "", nil, errMalformed
		}
		if k.N.BitLen() > maxRSABits || k.P.BitLen() > maxRSABits/2+8 || k.Q.BitLen() > maxRSABits/2+8 {
			return nil, "", nil, unsupportedKey("RSA keys larger than 16384 bits are not supported")
		}
		if k.E.BitLen() > 24 || k.E.Int64() < 3 || k.E.Int64()&1 == 0 {
			return nil, "", nil, invalidKey("invalid RSA public exponent")
		}
		pk := &rsa.PrivateKey{PublicKey: rsa.PublicKey{N: k.N, E: int(k.E.Int64())}, D: k.D, Primes: []*big.Int{k.P, k.Q}}
		if err := pk.Validate(); err != nil {
			return nil, "", nil, errMalformed
		}
		pk.Precompute()
		return pk, k.Comment, k.Pad, nil
	case ssh.KeyAlgoED25519:
		var k opensshEd25519
		if err := ssh.Unmarshal(rest, &k); err != nil {
			return nil, "", nil, errMalformed
		}
		if len(k.Priv) != ed25519.PrivateKeySize || len(k.Pub) != ed25519.PublicKeySize {
			return nil, "", nil, errMalformed
		}
		pk := ed25519.NewKeyFromSeed(k.Priv[:ed25519.SeedSize])
		if string(pk[ed25519.SeedSize:]) != string(k.Pub) {
			return nil, "", nil, errMalformed
		}
		return pk, k.Comment, k.Pad, nil
	case ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
		var k opensshECDSA
		if err := ssh.Unmarshal(rest, &k); err != nil {
			return nil, "", nil, errMalformed
		}
		curve := curveByName(k.Curve)
		if curve == nil {
			return nil, "", nil, unsupportedKey("unsupported elliptic curve " + k.Curve)
		}
		pk, err := ecdsaFromScalar(curve, k.D)
		if err != nil {
			return nil, "", nil, errMalformed
		}
		if point, err := pk.PublicKey.Bytes(); err != nil || string(point) != string(k.Pub) {
			return nil, "", nil, errMalformed
		}
		return pk, k.Comment, k.Pad, nil
	case ssh.InsecureKeyAlgoDSA:
		var k opensshDSA
		if err := ssh.Unmarshal(rest, &k); err != nil {
			return nil, "", nil, errMalformed
		}
		pk := &dsa.PrivateKey{PublicKey: dsa.PublicKey{Parameters: dsa.Parameters{P: k.P, Q: k.Q, G: k.G}, Y: k.Y}, X: k.X}
		if err := validateDSA(pk); err != nil {
			return nil, "", nil, errMalformed
		}
		return pk, k.Comment, k.Pad, nil
	case "sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com":
		return nil, "", nil, unsupportedKey("FIDO security-key (sk-*) keys cannot be imported; use them through an SSH agent")
	}
	return nil, "", nil, unsupportedKey(fmt.Sprintf("unsupported key type %q", keytype))
}

// marshalOpenSSHPrivateKey encodes raw in the OpenSSH format, encrypted with aes256-ctr + bcrypt (16 rounds) when a
// passphrase is given.
func marshalOpenSSHPrivateKey(raw crypto.PrivateKey, comment string, passphrase []byte) ([]byte, error) {
	if _, ok := raw.(*dsa.PrivateKey); !ok {
		var (
			block *pem.Block
			err   error
		)
		if len(passphrase) > 0 {
			block, err = ssh.MarshalPrivateKeyWithPassphrase(raw, comment, passphrase)
		} else {
			block, err = ssh.MarshalPrivateKey(raw, comment)
		}
		if err != nil {
			return nil, err
		}
		return pem.EncodeToMemory(block), nil
	}
	k := raw.(*dsa.PrivateKey)
	var check [4]byte
	if _, err := rand.Read(check[:]); err != nil {
		return nil, err
	}
	c := binary.BigEndian.Uint32(check[:])
	priv := ssh.Marshal(struct {
		Check1, Check2 uint32
		Keytype        string
		P, Q, G, Y, X  *big.Int
		Comment        string
	}{c, c, ssh.InsecureKeyAlgoDSA, k.P, k.Q, k.G, k.Y, k.X, comment})
	pub, err := ssh.NewPublicKey(&k.PublicKey)
	if err != nil {
		return nil, err
	}
	env := opensshEnvelope{CipherName: "none", KdfName: "none", NumKeys: 1, PubKey: pub.Marshal()}
	if len(passphrase) == 0 {
		env.PrivKeyBlock = opensshPad(priv, 8)
	} else {
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
		const rounds = 16
		key, err := bcryptPBKDF(passphrase, salt, rounds, 32+16)
		if err != nil {
			return nil, err
		}
		bc, err := aes.NewCipher(key[:32])
		if err != nil {
			return nil, err
		}
		plain := opensshPad(priv, aes.BlockSize)
		env.PrivKeyBlock = make([]byte, len(plain))
		cipher.NewCTR(bc, key[32:]).XORKeyStream(env.PrivKeyBlock, plain)
		env.CipherName, env.KdfName = "aes256-ctr", "bcrypt"
		env.KdfOpts = string(ssh.Marshal(opensshKDFOpts{Salt: salt, Rounds: rounds}))
	}
	body := append([]byte(opensshMagic), ssh.Marshal(env)...)
	return pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: body}), nil
}

// opensshPad appends the 1, 2, 3, … padding of the OpenSSH format up to a multiple of blockSize.
func opensshPad(b []byte, blockSize int) []byte {
	out := append([]byte(nil), b...)
	for i := 0; len(out)%blockSize != 0; i++ {
		out = append(out, byte(i+1))
	}
	return out
}

// ---- bcrypt_pbkdf (OpenBSD), as used by OpenSSH key files -----------------------------------------------------------

// bcryptPBKDF derives keyLen bytes from password and salt (OpenBSD bcrypt_pbkdf(3); the same algorithm as the
// internal x/crypto/ssh/internal/bcrypt_pbkdf package, which cannot be imported).
func bcryptPBKDF(password, salt []byte, rounds, keyLen int) ([]byte, error) {
	const blockSize = 32
	if rounds < 1 {
		return nil, errors.New("bcrypt_pbkdf: number of rounds is too small")
	}
	if len(password) == 0 {
		return nil, errors.New("bcrypt_pbkdf: empty password")
	}
	if len(salt) == 0 || len(salt) > 1<<20 {
		return nil, errors.New("bcrypt_pbkdf: bad salt length")
	}
	if keyLen > 1024 {
		return nil, errors.New("bcrypt_pbkdf: keyLen is too large")
	}
	numBlocks := (keyLen + blockSize - 1) / blockSize
	key := make([]byte, numBlocks*blockSize)

	h := sha512.New()
	h.Write(password)
	shapass := h.Sum(nil)

	shasalt := make([]byte, 0, sha512.Size)
	cnt, tmp := make([]byte, 4), make([]byte, blockSize)
	for block := 1; block <= numBlocks; block++ {
		h.Reset()
		h.Write(salt)
		binary.BigEndian.PutUint32(cnt, uint32(block))
		h.Write(cnt)
		bcryptHash(tmp, shapass, h.Sum(shasalt))

		out := make([]byte, blockSize)
		copy(out, tmp)
		for i := 2; i <= rounds; i++ {
			h.Reset()
			h.Write(tmp)
			bcryptHash(tmp, shapass, h.Sum(shasalt))
			for j := range out {
				out[j] ^= tmp[j]
			}
		}
		for i, v := range out {
			key[i*numBlocks+(block-1)] = v
		}
	}
	return key[:keyLen], nil
}

var bcryptMagic = []byte("OxychromaticBlowfishSwatDynamite")

func bcryptHash(out, shapass, shasalt []byte) {
	c, err := blowfish.NewSaltedCipher(shapass, shasalt)
	if err != nil {
		panic(err) // unreachable: shapass is a 64-byte SHA-512 digest
	}
	for range 64 {
		blowfish.ExpandKey(shasalt, c)
		blowfish.ExpandKey(shapass, c)
	}
	copy(out, bcryptMagic)
	for i := 0; i < 32; i += 8 {
		for range 64 {
			c.Encrypt(out[i:i+8], out[i:i+8])
		}
	}
	// Swap bytes due to different endianness.
	for i := 0; i < 32; i += 4 {
		out[i+3], out[i+2], out[i+1], out[i] = out[i], out[i+1], out[i+2], out[i+3]
	}
}
