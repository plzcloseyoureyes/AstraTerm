package anontls

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"hash"
)

// Cipher suites implemented by this package (all TLS 1.2, anonymous key exchange). Values from the IANA TLS Cipher
// Suites registry (RFC 5246, RFC 4492, RFC 5288).
const (
	TLS_DH_anon_WITH_AES_128_CBC_SHA    uint16 = 0x0034
	TLS_DH_anon_WITH_AES_256_CBC_SHA    uint16 = 0x003A
	TLS_DH_anon_WITH_AES_128_CBC_SHA256 uint16 = 0x006C
	TLS_DH_anon_WITH_AES_256_CBC_SHA256 uint16 = 0x006D
	TLS_DH_anon_WITH_AES_128_GCM_SHA256 uint16 = 0x00A6
	TLS_DH_anon_WITH_AES_256_GCM_SHA384 uint16 = 0x00A7
	TLS_ECDH_anon_WITH_AES_128_CBC_SHA  uint16 = 0xC018
	TLS_ECDH_anon_WITH_AES_256_CBC_SHA  uint16 = 0xC019
	versionTLS12                        uint16 = 0x0303
	versionTLS10                        uint16 = 0x0301
)

type kxKind int

const (
	kxDH kxKind = iota
	kxECDH
)

type suite struct {
	id      uint16
	name    string
	kx      kxKind
	keyLen  int              // AES key length
	aead    bool             // AES-GCM (else AES-CBC + HMAC)
	mac     func() hash.Hash // CBC suites
	macLen  int
	prfHash func() hash.Hash // TLS 1.2 PRF hash (also used for the handshake transcript)
}

var suites = []*suite{
	// AES-256 before AES-128 for finite-field DH: OpenSSL servers that size their group automatically pick 3072 bits
	// for 256-bit anonymous suites but only 1024 bits (rejected, see dh.go) for 128-bit ones.
	{id: TLS_DH_anon_WITH_AES_256_GCM_SHA384, name: "TLS_DH_anon_WITH_AES_256_GCM_SHA384", kx: kxDH, keyLen: 32, aead: true, prfHash: sha512.New384},
	{id: TLS_DH_anon_WITH_AES_128_GCM_SHA256, name: "TLS_DH_anon_WITH_AES_128_GCM_SHA256", kx: kxDH, keyLen: 16, aead: true, prfHash: sha256.New},
	{id: TLS_ECDH_anon_WITH_AES_256_CBC_SHA, name: "TLS_ECDH_anon_WITH_AES_256_CBC_SHA", kx: kxECDH, keyLen: 32, mac: sha1.New, macLen: sha1.Size, prfHash: sha256.New},
	{id: TLS_ECDH_anon_WITH_AES_128_CBC_SHA, name: "TLS_ECDH_anon_WITH_AES_128_CBC_SHA", kx: kxECDH, keyLen: 16, mac: sha1.New, macLen: sha1.Size, prfHash: sha256.New},
	{id: TLS_DH_anon_WITH_AES_256_CBC_SHA256, name: "TLS_DH_anon_WITH_AES_256_CBC_SHA256", kx: kxDH, keyLen: 32, mac: sha256.New, macLen: sha256.Size, prfHash: sha256.New},
	{id: TLS_DH_anon_WITH_AES_128_CBC_SHA256, name: "TLS_DH_anon_WITH_AES_128_CBC_SHA256", kx: kxDH, keyLen: 16, mac: sha256.New, macLen: sha256.Size, prfHash: sha256.New},
	{id: TLS_DH_anon_WITH_AES_256_CBC_SHA, name: "TLS_DH_anon_WITH_AES_256_CBC_SHA", kx: kxDH, keyLen: 32, mac: sha1.New, macLen: sha1.Size, prfHash: sha256.New},
	{id: TLS_DH_anon_WITH_AES_128_CBC_SHA, name: "TLS_DH_anon_WITH_AES_128_CBC_SHA", kx: kxDH, keyLen: 16, mac: sha1.New, macLen: sha1.Size, prfHash: sha256.New},
}

// DefaultCipherSuites is the offer order: AEAD first, then ECDH, then finite-field DH with CBC (the CBC suites use
// encrypt-then-MAC whenever the server supports RFC 7366), AES-256 before AES-128.
func DefaultCipherSuites() []uint16 {
	out := make([]uint16, len(suites))
	for i, s := range suites {
		out[i] = s.id
	}
	return out
}

// CipherSuiteName returns the IANA name of a suite implemented by this package ("" if unknown).
func CipherSuiteName(id uint16) string {
	if s := suiteByID(id); s != nil {
		return s.name
	}
	return ""
}

func suiteByID(id uint16) *suite {
	for _, s := range suites {
		if s.id == id {
			return s
		}
	}
	return nil
}

// keyMaterial sizes: MAC keys, encryption keys, implicit IVs (GCM salt).
func (s *suite) macKeyLen() int {
	if s.aead {
		return 0
	}
	return s.macLen
}

func (s *suite) fixedIVLen() int {
	if s.aead {
		return 4
	}
	return 0
}

// newHalfConn builds the record protection for one direction (etm: RFC 7366 encrypt-then-MAC, CBC suites only).
// The key material is copied (AES key schedule, HMAC pads, GCM salt), so the caller may wipe it afterwards.
func (s *suite) newHalfConn(macKey, key, iv []byte, etm bool) (*halfConn, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	hc := &halfConn{suite: s}
	if s.aead {
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		hc.aead = gcm
		hc.salt = append([]byte(nil), iv...)
		return hc, nil
	}
	hc.block = block
	hc.mac = constantTimeMAC(s.mac, macKey)
	hc.etm = etm
	return hc, nil
}
