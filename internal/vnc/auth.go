package vnc

import (
	"crypto/aes"
	"crypto/des"
	"crypto/md5"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
)

// vncAuthResponse computes the VNC Authentication response (RFC 6143 §7.2.2): the 16-byte challenge encrypted with
// DES-ECB, keyed with the first 8 bytes of the password (zero padded) where every key byte has its bits mirrored
// (the historical quirk of the reference implementation).
func vncAuthResponse(password string, challenge []byte) ([]byte, error) {
	if len(challenge) != 16 {
		return nil, fmt.Errorf("invalid VNC authentication challenge length %d", len(challenge))
	}
	var key [8]byte
	defer clear(key[:])
	copy(key[:], password)
	for i := range key {
		key[i] = reverseBits(key[i])
	}
	block, err := des.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	out := make([]byte, 16)
	block.Encrypt(out[:8], challenge[:8])
	block.Encrypt(out[8:], challenge[8:])
	return out, nil
}

func reverseBits(b byte) byte {
	b = (b&0xf0)>>4 | (b&0x0f)<<4
	b = (b&0xcc)>>2 | (b&0x33)<<2
	b = (b&0xaa)>>1 | (b&0x55)<<1
	return b
}

// Apple Remote Desktop authentication (security type 30), as implemented by macOS Screen Sharing and noVNC:
// the server sends a Diffie-Hellman generator (u16), the key length (u16), the prime modulus and its public key; the
// client answers with 128 bytes (username and password, NUL-terminated in two 64-byte halves, padded with random
// bytes) encrypted with AES-128-ECB under MD5(shared secret), followed by its own public key.
//
// The exchange is unauthenticated: whoever answers at the server's address receives the (account) password — the
// scheme's inherent weakness, like VNC Authentication's offline-crackable challenge-response. What the client can
// do is refuse groups a passive eavesdropper could break: macOS sends 128-byte (1024-bit) keys, smaller ones are
// rejected.

// minARDKeyLen / maxARDKeyLen bound the DH modulus the server may send (bytes).
const (
	minARDKeyLen = 128
	maxARDKeyLen = 1024
)

type ardParams struct {
	generator *big.Int
	prime     *big.Int
	serverPub *big.Int
	keyLen    int
}

func readARDParams(r io.Reader) (*ardParams, error) {
	g, err := readU16(r)
	if err != nil {
		return nil, err
	}
	keyLen, err := readU16(r)
	if err != nil {
		return nil, err
	}
	if keyLen < minARDKeyLen || keyLen > maxARDKeyLen {
		return nil, fmt.Errorf("unsupported Apple Remote Desktop key length (%d bits; %d to %d accepted)", 8*int(keyLen),
			8*minARDKeyLen, 8*maxARDKeyLen)
	}
	p, err := readFull(r, int(keyLen))
	if err != nil {
		return nil, err
	}
	pub, err := readFull(r, int(keyLen))
	if err != nil {
		return nil, err
	}
	a := &ardParams{
		generator: new(big.Int).SetUint64(uint64(g)),
		prime:     new(big.Int).SetBytes(p),
		serverPub: new(big.Int).SetBytes(pub),
		keyLen:    int(keyLen),
	}
	pm1 := new(big.Int).Sub(a.prime, big.NewInt(1))
	if a.prime.BitLen() < 8*minARDKeyLen-8 || a.prime.Bit(0) == 0 || a.generator.Cmp(big.NewInt(2)) < 0 ||
		a.generator.Cmp(pm1) >= 0 || a.serverPub.Cmp(big.NewInt(2)) < 0 || a.serverPub.Cmp(pm1) >= 0 {
		return nil, errors.New("invalid Apple Remote Desktop Diffie-Hellman parameters")
	}
	return a, nil
}

// ardResponse builds the client's answer: encrypted credentials (128 bytes) followed by the client public key. The
// plaintext credential block, the AES key and the DH secrets are wiped afterwards (Go strings holding the password
// itself cannot be).
func ardResponse(a *ardParams, username, password string, random io.Reader) ([]byte, error) {
	if random == nil {
		random = rand.Reader
	}
	// Private exponent in [2, p-2].
	max := new(big.Int).Sub(a.prime, big.NewInt(3))
	priv, err := rand.Int(random, max)
	if err != nil {
		return nil, err
	}
	priv.Add(priv, big.NewInt(2))
	pub := new(big.Int).Exp(a.generator, priv, a.prime)
	shared := new(big.Int).Exp(a.serverPub, priv, a.prime)
	sharedBytes := leftPad(shared.Bytes(), a.keyLen)
	key := md5.Sum(sharedBytes)
	wipeBig(priv, shared)
	clear(sharedBytes)
	defer clear(key[:])

	creds := make([]byte, 128)
	defer clear(creds)
	if _, err := io.ReadFull(random, creds); err != nil {
		return nil, err
	}
	u, p := []byte(username), []byte(password)
	defer clear(p)
	if len(u) > 63 {
		u = u[:63]
	}
	if len(p) > 63 {
		p = p[:63]
	}
	copy(creds, u)
	creds[len(u)] = 0
	copy(creds[64:], p)
	creds[64+len(p)] = 0

	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	out := make([]byte, 128, 128+a.keyLen)
	for i := 0; i < 128; i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], creds[i:i+aes.BlockSize])
	}
	return append(out, leftPad(pub.Bytes(), a.keyLen)...), nil
}

// wipeBig overwrites the magnitude of secret integers (best effort).
func wipeBig(xs ...*big.Int) {
	for _, x := range xs {
		if x != nil {
			clear(x.Bits())
		}
	}
}

func leftPad(b []byte, n int) []byte {
	if len(b) >= n {
		return b[len(b)-n:]
	}
	out := make([]byte, n)
	copy(out[n-len(b):], b)
	return out
}
