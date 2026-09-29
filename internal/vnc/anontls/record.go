package anontls

import (
	"crypto/cipher"
	"crypto/hmac"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"hash"
	"io"
)

// TLS record content types (RFC 5246 section 6.2.1).
const (
	recordChangeCipherSpec uint8 = 20
	recordAlert            uint8 = 21
	recordHandshake        uint8 = 22
	recordApplicationData  uint8 = 23
)

const (
	recordHeaderLen = 5
	maxPlaintext    = 1 << 14
	maxCiphertext   = maxPlaintext + 2048
)

var errBadRecordMAC = errors.New("anontls: bad record MAC")

// halfConn is the record protection state of one direction.
type halfConn struct {
	suite *suite
	seq   uint64

	// AES-GCM (RFC 5288): 4-byte implicit salt + 8-byte explicit nonce carried in each record.
	aead cipher.AEAD
	salt []byte

	// AES-CBC with HMAC and an explicit per-record IV (RFC 5246 section 6.2.3.2): MAC-then-encrypt, or
	// encrypt-then-MAC when RFC 7366 was negotiated.
	block cipher.Block
	mac   hash.Hash
	etm   bool
}

func (hc *halfConn) seqBytes() []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], hc.seq)
	return b[:]
}

func (hc *halfConn) incSeq() error {
	if hc.seq == ^uint64(0) {
		return errors.New("anontls: sequence number overflow")
	}
	hc.seq++
	return nil
}

// additionalData is seq_num + type + version + length (the MAC input prefix for CBC suites, the AEAD associated data
// for GCM).
func (hc *halfConn) additionalData(typ uint8, version uint16, n int) []byte {
	ad := make([]byte, 13)
	copy(ad, hc.seqBytes())
	ad[8] = typ
	binary.BigEndian.PutUint16(ad[9:], version)
	binary.BigEndian.PutUint16(ad[11:], uint16(n))
	return ad
}

// macOf computes the record MAC over the header fields and data. extra is hashed after the digest was taken, so the
// amount of hashing does not depend on how data and extra split a decrypted payload (the Lucky Thirteen mitigation
// of crypto/tls; see openMtE).
func (hc *halfConn) macOf(typ uint8, version uint16, n int, data, extra []byte) []byte {
	hc.mac.Reset()
	hc.mac.Write(hc.additionalData(typ, version, n))
	hc.mac.Write(data)
	sum := hc.mac.Sum(nil)
	if extra != nil {
		hc.mac.Write(extra)
	}
	return sum
}

// seal protects plaintext and returns the record fragment.
func (hc *halfConn) seal(typ uint8, version uint16, plaintext []byte, random io.Reader) ([]byte, error) {
	if hc.aead != nil {
		explicit := hc.seqBytes() // unique per key: the sequence number never repeats
		nonce := append(append(make([]byte, 0, 12), hc.salt...), explicit...)
		out := append(make([]byte, 0, 8+len(plaintext)+hc.aead.Overhead()), explicit...)
		out = hc.aead.Seal(out, nonce, plaintext, hc.additionalData(typ, version, len(plaintext)))
		return out, hc.incSeq()
	}
	bs := hc.block.BlockSize()
	var mac []byte
	if !hc.etm {
		mac = hc.macOf(typ, version, len(plaintext), plaintext, nil)
	}
	padLen := bs - (len(plaintext)+len(mac)+1)%bs
	if padLen == bs {
		padLen = 0
	}
	total := len(plaintext) + len(mac) + padLen + 1
	macTail := 0
	if hc.etm {
		macTail = hc.mac.Size()
	}
	out := make([]byte, bs+total, bs+total+macTail)
	iv := out[:bs]
	if _, err := io.ReadFull(random, iv); err != nil {
		return nil, err
	}
	payload := out[bs:]
	n := copy(payload, plaintext)
	n += copy(payload[n:], mac)
	for i := n; i < total; i++ {
		payload[i] = byte(padLen)
	}
	cipher.NewCBCEncrypter(hc.block, iv).CryptBlocks(payload, payload)
	if hc.etm {
		// RFC 7366 section 3: the MAC covers the IV and the ciphertext; the length field is theirs.
		out = append(out, hc.macOf(typ, version, len(out), out, nil)...)
	}
	return out, hc.incSeq()
}

// open verifies and decrypts a record fragment (in place).
func (hc *halfConn) open(typ uint8, version uint16, fragment []byte) ([]byte, error) {
	switch {
	case hc.aead != nil:
		return hc.openAEAD(typ, version, fragment)
	case hc.etm:
		return hc.openEtM(typ, version, fragment)
	}
	return hc.openMtE(typ, version, fragment)
}

func (hc *halfConn) openAEAD(typ uint8, version uint16, fragment []byte) ([]byte, error) {
	if len(fragment) < 8+hc.aead.Overhead() {
		return nil, errBadRecordMAC
	}
	nonce := append(append(make([]byte, 0, 12), hc.salt...), fragment[:8]...)
	ct := fragment[8:]
	n := len(ct) - hc.aead.Overhead()
	pt, err := hc.aead.Open(ct[:0], nonce, ct, hc.additionalData(typ, version, n))
	if err != nil {
		return nil, errBadRecordMAC
	}
	return pt, hc.incSeq()
}

// openEtM authenticates IV + ciphertext before decrypting (RFC 7366), which removes the padding oracle of
// MAC-then-encrypt: the padding of an authenticated record can be checked in variable time.
func (hc *halfConn) openEtM(typ uint8, version uint16, fragment []byte) ([]byte, error) {
	bs := hc.block.BlockSize()
	macLen := hc.mac.Size()
	if len(fragment) < 2*bs+macLen || (len(fragment)-macLen)%bs != 0 {
		return nil, errBadRecordMAC
	}
	body := fragment[:len(fragment)-macLen]
	remoteMAC := fragment[len(body):]
	localMAC := hc.macOf(typ, version, len(body), body, nil)
	if subtle.ConstantTimeCompare(localMAC, remoteMAC) != 1 {
		return nil, errBadRecordMAC
	}
	iv, payload := body[:bs], body[bs:]
	cipher.NewCBCDecrypter(hc.block, iv).CryptBlocks(payload, payload)
	pad := int(payload[len(payload)-1])
	if pad+1 > len(payload) {
		return nil, errBadRecordMAC
	}
	for _, b := range payload[len(payload)-1-pad : len(payload)-1] {
		if int(b) != pad {
			return nil, errBadRecordMAC
		}
	}
	return payload[:len(payload)-1-pad], hc.incSeq()
}

// openMtE is MAC-then-encrypt CBC (servers without RFC 7366), hardened like crypto/tls against padding oracles and
// Lucky Thirteen: the padding is inspected in constant time, a bad padding is reported exactly like a bad MAC (same
// error, same alert, after the same work), and the bytes after the MAC are fed to the hash after the digest so the
// hashing time does not depend on the secret padding length (modulo cache effects; SHA-1 MACs also finish in
// constant time, see constantTimeMAC).
func (hc *halfConn) openMtE(typ uint8, version uint16, fragment []byte) ([]byte, error) {
	bs := hc.block.BlockSize()
	macLen := hc.mac.Size()
	// explicit IV + whole blocks holding at least the MAC and the padding length byte.
	minLen := bs + ((macLen+1+bs-1)/bs)*bs
	if len(fragment) < minLen || (len(fragment)-bs)%bs != 0 {
		return nil, errBadRecordMAC
	}
	iv := fragment[:bs]
	payload := fragment[bs:]
	cipher.NewCBCDecrypter(hc.block, iv).CryptBlocks(payload, payload)

	toRemove, good := paddingLen(payload)
	n := len(payload) - macLen - toRemove
	n = subtle.ConstantTimeSelect(int(uint32(n)>>31), 0, n) // if n < 0 { n = 0 }
	remoteMAC := payload[n : n+macLen]
	localMAC := hc.macOf(typ, version, n, payload[:n], payload[n+macLen:])
	if subtle.ConstantTimeCompare(localMAC, remoteMAC)&int(good) != 1 {
		return nil, errBadRecordMAC
	}
	return payload[:n], hc.incSeq()
}

// paddingLen inspects CBC padding (RFC 5246 section 6.2.3.2) without branching on secret data: it returns how many
// bytes to strip (padding plus its length byte) and good = 0xff when every padding byte equals the length byte, 0
// otherwise. On bad padding toRemove is 1, so the unchecked bytes stay inside the MACed data (as in crypto/tls).
func paddingLen(payload []byte) (toRemove int, good byte) {
	if len(payload) == 0 {
		return 0, 0
	}
	pad := payload[len(payload)-1]
	// good starts as 0xff when the padding fits in the payload.
	fits := uint(len(payload)-1) - uint(pad) // high bit clear ⇔ pad <= len-1
	good = byte(int32(^fits) >> 31)
	check := min(256, len(payload))
	for i := 0; i < check; i++ {
		inPad := byte(int32(^(uint(pad) - uint(i))) >> 31) // 0xff when i <= pad
		b := payload[len(payload)-1-i]
		good &^= inPad & (pad ^ b)
	}
	// Collapse: good must be all ones.
	good &= good << 4
	good &= good << 2
	good &= good << 1
	good = byte(int8(good) >> 7)
	pad &= good
	return int(pad) + 1, good
}

// ---- constant-time MAC finalization ---------------------------------------------------------------------------------

// constantTimeHash is implemented by crypto/sha1's digest: its Sum does not branch on the message length.
type constantTimeHash interface {
	hash.Hash
	ConstantTimeSum(b []byte) []byte
}

// cthWrapper replaces Sum with ConstantTimeSum (crypto/tls does the same for its HMAC-SHA1 suites).
type cthWrapper struct{ h constantTimeHash }

func (c *cthWrapper) Size() int                   { return c.h.Size() }
func (c *cthWrapper) BlockSize() int              { return c.h.BlockSize() }
func (c *cthWrapper) Reset()                      { c.h.Reset() }
func (c *cthWrapper) Write(p []byte) (int, error) { return c.h.Write(p) }
func (c *cthWrapper) Sum(b []byte) []byte         { return c.h.ConstantTimeSum(b) }

// constantTimeMAC returns an HMAC whose inner and outer hashes finish in constant time when the hash supports it.
func constantTimeMAC(h func() hash.Hash, key []byte) hash.Hash {
	if _, ok := h().(constantTimeHash); ok {
		return hmac.New(func() hash.Hash { return &cthWrapper{h().(constantTimeHash)} }, key)
	}
	return hmac.New(h, key)
}

// ---- TLS 1.2 PRF (RFC 5246 section 5) ---------------------------------------------------------------------------------

func pHash(h func() hash.Hash, secret, seed []byte, n int) []byte {
	out := make([]byte, 0, n)
	mac := hmac.New(h, secret)
	mac.Write(seed)
	a := mac.Sum(nil)
	for len(out) < n {
		mac.Reset()
		mac.Write(a)
		mac.Write(seed)
		out = append(out, mac.Sum(nil)...)
		mac.Reset()
		mac.Write(a)
		a = mac.Sum(nil)
	}
	return out[:n]
}

// prf12 is the TLS 1.2 PRF: P_hash(secret, label + seed).
func prf12(h func() hash.Hash, secret []byte, label string, seed []byte, n int) []byte {
	ls := make([]byte, 0, len(label)+len(seed))
	ls = append(append(ls, label...), seed...)
	return pHash(h, secret, ls, n)
}
