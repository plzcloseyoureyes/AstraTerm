// Package anontlstest is a minimal anonymous TLS 1.2 *server* for tests of VeNCrypt clients (TLSNone / TLSVnc /
// TLSPlain): TLS_DH_anon and TLS_ECDH_anon (X25519) key exchange, AES-GCM and AES-CBC (MAC-then-encrypt or RFC 7366
// encrypt-then-MAC), extended master secret. It is written independently of package anontls (standard library
// primitives only), so tests exercise the client against a second implementation. Not for production use: it does
// no validation beyond what the tests need.
package anontlstest

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
	"net"
	"sync"
	"time"
)

// Config configures the server.
type Config struct {
	// Suites lists acceptable cipher suites in the server's preference order (default: all supported).
	Suites []uint16
	// DHPrime / DHGenerator is the finite-field group (default: RFC 7919 ffdhe2048, generator 2).
	DHPrime, DHGenerator *big.Int
	// NoEMS / NoETM disable extended master secret / encrypt-then-MAC.
	NoEMS, NoETM bool
}

// Negotiated describes a completed handshake.
type Negotiated struct {
	Suite          uint16
	EMS, ETM       bool
	DHBits         int
	ClientOffered  []uint16
	ClientGroupsOK bool // the ClientHello advertised FFDHE groups
}

// Suites supported by this server.
const (
	DHAES128GCM    uint16 = 0x00A6
	DHAES256GCM    uint16 = 0x00A7
	ECDHAES128CBC  uint16 = 0xC018
	ECDHAES256CBC  uint16 = 0xC019
	DHAES128CBC256 uint16 = 0x006C
	DHAES256CBC256 uint16 = 0x006D
	DHAES128CBC    uint16 = 0x0034
	DHAES256CBC    uint16 = 0x003A
)

type suiteInfo struct {
	ecdh    bool
	keyLen  int
	gcm     bool
	mac     func() hash.Hash
	prfHash func() hash.Hash
}

var suiteTable = map[uint16]suiteInfo{
	DHAES128GCM:    {keyLen: 16, gcm: true, prfHash: sha256.New},
	DHAES256GCM:    {keyLen: 32, gcm: true, prfHash: sha512.New384},
	ECDHAES128CBC:  {ecdh: true, keyLen: 16, mac: sha1.New, prfHash: sha256.New},
	ECDHAES256CBC:  {ecdh: true, keyLen: 32, mac: sha1.New, prfHash: sha256.New},
	DHAES128CBC256: {keyLen: 16, mac: sha256.New, prfHash: sha256.New},
	DHAES256CBC256: {keyLen: 32, mac: sha256.New, prfHash: sha256.New},
	DHAES128CBC:    {keyLen: 16, mac: sha1.New, prfHash: sha256.New},
	DHAES256CBC:    {keyLen: 32, mac: sha1.New, prfHash: sha256.New},
}

// FFDHE2048 is the RFC 7919 group (the default).
var FFDHE2048, _ = new(big.Int).SetString(""+
	"FFFFFFFFFFFFFFFFADF85458A2BB4A9AAFDC5620273D3CF1D8B9C583CE2D3695A9E13641146433FBCC939DCE249B3EF9"+
	"7D2FE363630C75D8F681B202AEC4617AD3DF1ED5D5FD65612433F51F5F066ED0856365553DED1AF3B557135E7F57C935"+
	"984F0C70E0E68B77E2A689DAF3EFE8721DF158A136ADE73530ACCA4F483A797ABC0AB182B324FB61D108A94BB2C8E3FB"+
	"B96ADAB760D7F4681D4F42A3DE394DF4AE56EDE76372BB190B07A7C8EE0A6D709E02FCE1CDF7E2ECC03404CD28342F61"+
	"9172FE9CE98583FF8E4F1232EEF28183C3FE3B1B4C6FAD733BB5FCBC2EC22005C58EF1837D1683B2C6F34A26C1B2EFFA"+
	"886B423861285C97FFFFFFFFFFFFFFFF", 16)

// RFC5114P1024 / RFC5114G1024 is a 1024-bit group (RFC 5114 section 2.1), for "weak group" tests.
var (
	RFC5114P1024, _ = new(big.Int).SetString(""+
		"B10B8F96A080E01DDE92DE5EAE5D54EC52C99FBCFB06A3C69A6A9DCA52D23B616073E28675A23D189838EF1E2EE652C0"+
		"13ECB4AEA906112324975C3CD49B83BFACCBDD7D90C4BD7098488E9C219A73724EFFD6FAE5644738FAA31A4FF55BCCC0"+
		"A151AF5F0DC8B4BD45BF37DF365C1A65E68CFDA76D4DA708DF1FB2BC2E4A4371", 16)
	RFC5114G1024, _ = new(big.Int).SetString(""+
		"A4D1CBD5C3FD34126765A442EFB99905F8104DD258AC507FD6406CFF14266D31266FEA1E5C41564B777E690F5504F213"+
		"160217B4B01B886A5E91547F9E2749F4D7FBD7D3B9A92EE1909D0D2263F80A76A6A24C087A091F531DBF0A0169B6A28A"+
		"D662A4D18E73AFA32D779D5918D08BC8858F4DCEF97C2A24855E6EEB22B3B2E5", 16)
)

var defaultSuites = []uint16{DHAES128GCM, DHAES256GCM, ECDHAES128CBC, ECDHAES256CBC, DHAES128CBC256, DHAES256CBC256,
	DHAES128CBC, DHAES256CBC}

// Serve runs the server handshake on c and returns a net.Conn carrying the application data.
func Serve(c net.Conn, cfg *Config) (net.Conn, *Negotiated, error) {
	if cfg == nil {
		cfg = &Config{}
	}
	s := &conn{Conn: c}
	neg, err := s.handshake(cfg)
	if err != nil {
		return nil, neg, err
	}
	return s, neg, nil
}

type direction struct {
	seq   uint64
	gcm   cipher.AEAD
	salt  []byte
	block cipher.Block
	mac   hash.Hash
	etm   bool
}

type conn struct {
	net.Conn
	in, out *direction
	rmu     sync.Mutex
	pending []byte
	wmu     sync.Mutex
}

func (s *conn) readRecord() (byte, []byte, error) {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(s.Conn, hdr); err != nil {
		return 0, nil, err
	}
	body := make([]byte, binary.BigEndian.Uint16(hdr[3:]))
	if _, err := io.ReadFull(s.Conn, body); err != nil {
		return 0, nil, err
	}
	if s.in == nil {
		return hdr[0], body, nil
	}
	pt, err := s.in.open(hdr[0], body)
	return hdr[0], pt, err
}

func (s *conn) writeRecord(typ byte, data []byte) error {
	frag := data
	if s.out != nil {
		frag = s.out.seal(typ, data)
	}
	rec := append([]byte{typ, 3, 3}, byte(len(frag)>>8), byte(len(frag)))
	_, err := s.Conn.Write(append(rec, frag...))
	return err
}

func ad(seq uint64, typ byte, n int) []byte {
	b := binary.BigEndian.AppendUint64(nil, seq)
	return append(b, typ, 3, 3, byte(n>>8), byte(n))
}

func (d *direction) macOf(typ byte, n int, parts ...[]byte) []byte {
	d.mac.Reset()
	d.mac.Write(ad(d.seq, typ, n))
	for _, p := range parts {
		d.mac.Write(p)
	}
	return d.mac.Sum(nil)
}

func (d *direction) seal(typ byte, pt []byte) []byte {
	defer func() { d.seq++ }()
	if d.gcm != nil {
		explicit := binary.BigEndian.AppendUint64(nil, d.seq)
		return d.gcm.Seal(explicit, append(append([]byte{}, d.salt...), explicit...), pt, ad(d.seq, typ, len(pt)))
	}
	body := append([]byte{}, pt...)
	if !d.etm {
		body = append(body, d.macOf(typ, len(pt), pt)...)
	}
	pad := 16 - (len(body)+1)%16
	if pad == 16 {
		pad = 0
	}
	for i := 0; i <= pad; i++ {
		body = append(body, byte(pad))
	}
	iv := make([]byte, 16)
	_, _ = rand.Read(iv)
	cipher.NewCBCEncrypter(d.block, iv).CryptBlocks(body, body)
	out := append(iv, body...)
	if d.etm {
		out = append(out, d.macOf(typ, len(out), out)...)
	}
	return out
}

var errMAC = errors.New("anontlstest: bad record MAC")

func (d *direction) open(typ byte, frag []byte) ([]byte, error) {
	if d.gcm != nil {
		if len(frag) < 8+16 {
			return nil, errMAC
		}
		nonce := append(append([]byte{}, d.salt...), frag[:8]...)
		pt, err := d.gcm.Open(nil, nonce, frag[8:], ad(d.seq, typ, len(frag)-8-16))
		if err != nil {
			return nil, errMAC
		}
		d.seq++
		return pt, nil
	}
	macLen := d.mac.Size()
	if d.etm {
		if len(frag) < 32+macLen || (len(frag)-macLen)%16 != 0 {
			return nil, errMAC
		}
		body, tag := frag[:len(frag)-macLen], frag[len(frag)-macLen:]
		if subtle.ConstantTimeCompare(d.macOf(typ, len(body), body), tag) != 1 {
			return nil, errMAC
		}
		pt := append([]byte{}, body[16:]...)
		cipher.NewCBCDecrypter(d.block, body[:16]).CryptBlocks(pt, pt)
		pad := int(pt[len(pt)-1])
		if pad+1 > len(pt) {
			return nil, errMAC
		}
		d.seq++
		return pt[:len(pt)-pad-1], nil
	}
	if len(frag) < 32 || len(frag)%16 != 0 {
		return nil, errMAC
	}
	pt := append([]byte{}, frag[16:]...)
	cipher.NewCBCDecrypter(d.block, frag[:16]).CryptBlocks(pt, pt)
	pad := int(pt[len(pt)-1])
	if pad+1+macLen > len(pt) {
		return nil, errMAC
	}
	body := pt[:len(pt)-pad-1-macLen]
	tag := pt[len(body) : len(body)+macLen]
	if !hmac.Equal(d.macOf(typ, len(body), body), tag) {
		return nil, errMAC
	}
	d.seq++
	return body, nil
}

func prf(h func() hash.Hash, secret []byte, label string, seed []byte, n int) []byte {
	seed = append([]byte(label), seed...)
	mac := hmac.New(h, secret)
	mac.Write(seed)
	a := mac.Sum(nil)
	var out []byte
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

func hsMsg(typ byte, body []byte) []byte {
	return append([]byte{typ, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}, body...)
}

func (s *conn) handshake(cfg *Config) (*Negotiated, error) {
	_ = s.Conn.SetDeadline(time.Now().Add(20 * time.Second))
	defer func() { _ = s.Conn.SetDeadline(time.Time{}) }()
	var transcript bytes.Buffer
	var hello []byte
	for len(hello) < 4 || len(hello) < 4+(int(hello[1])<<16|int(hello[2])<<8|int(hello[3])) {
		typ, data, err := s.readRecord()
		if err != nil {
			return nil, err
		}
		if typ != 22 {
			return nil, fmt.Errorf("anontlstest: expected a handshake record, got %d", typ)
		}
		hello = append(hello, data...)
	}
	if hello[0] != 1 || len(hello) < 4+2+32+1 {
		return nil, errors.New("anontlstest: expected ClientHello")
	}
	transcript.Write(hello)
	b := hello[4:]
	clientRandom := b[2:34]
	b = b[34:]
	b = b[1+int(b[0]):]
	csLen := int(binary.BigEndian.Uint16(b))
	neg := &Negotiated{}
	for i := 0; i < csLen; i += 2 {
		neg.ClientOffered = append(neg.ClientOffered, binary.BigEndian.Uint16(b[2+i:]))
	}
	b = b[2+csLen:]
	b = b[1+int(b[0]):]
	exts := map[uint16][]byte{}
	if len(b) >= 2 {
		b = b[2:]
		for len(b) >= 4 {
			t, n := binary.BigEndian.Uint16(b), int(binary.BigEndian.Uint16(b[2:]))
			exts[t] = b[4 : 4+n]
			b = b[4+n:]
		}
	}
	if g, ok := exts[10]; ok && bytes.Contains(g, []byte{1, 0}) {
		neg.ClientGroupsOK = true
	}
	prefs := cfg.Suites
	if len(prefs) == 0 {
		prefs = defaultSuites
	}
	var id uint16
	for _, p := range prefs {
		for _, o := range neg.ClientOffered {
			if p == o && id == 0 {
				id = p
			}
		}
	}
	if id == 0 {
		_ = s.writeRecord(21, []byte{2, 40})
		return neg, errors.New("anontlstest: no common cipher suite")
	}
	st := suiteTable[id]
	neg.Suite = id
	_, ems := exts[23]
	neg.EMS = ems && !cfg.NoEMS
	_, etm := exts[22]
	neg.ETM = etm && !cfg.NoETM && !st.gcm

	serverRandom := make([]byte, 32)
	_, _ = rand.Read(serverRandom)
	sh := append([]byte{3, 3}, serverRandom...)
	sh = append(sh, 0, byte(id>>8), byte(id), 0)
	var ext []byte
	ext = append(ext, 0xff, 0x01, 0, 1, 0)
	if neg.EMS {
		ext = append(ext, 0, 23, 0, 0)
	}
	if neg.ETM {
		ext = append(ext, 0, 22, 0, 0)
	}
	if st.ecdh {
		ext = append(ext, 0, 11, 0, 2, 1, 0)
	}
	sh = append(sh, byte(len(ext)>>8), byte(len(ext)))
	sh = append(sh, ext...)

	var ske []byte
	var premaster func(ckx []byte) ([]byte, error)
	if st.ecdh {
		priv, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return neg, err
		}
		pub := priv.PublicKey().Bytes()
		ske = append([]byte{3, 0, 29, byte(len(pub))}, pub...)
		premaster = func(ckx []byte) ([]byte, error) {
			if len(ckx) < 1 || int(ckx[0]) != len(ckx)-1 {
				return nil, errors.New("anontlstest: malformed ClientKeyExchange")
			}
			peer, err := ecdh.X25519().NewPublicKey(ckx[1:])
			if err != nil {
				return nil, err
			}
			return priv.ECDH(peer)
		}
	} else {
		p, g := cfg.DHPrime, cfg.DHGenerator
		if p == nil {
			p, g = FFDHE2048, big.NewInt(2)
		}
		neg.DHBits = p.BitLen()
		y, _ := rand.Int(rand.Reader, new(big.Int).Sub(p, big.NewInt(3)))
		y.Add(y, big.NewInt(2))
		for _, v := range []*big.Int{p, g, new(big.Int).Exp(g, y, p)} {
			vb := v.Bytes()
			ske = append(ske, byte(len(vb)>>8), byte(len(vb)))
			ske = append(ske, vb...)
		}
		premaster = func(ckx []byte) ([]byte, error) {
			if len(ckx) < 2 || int(binary.BigEndian.Uint16(ckx)) != len(ckx)-2 {
				return nil, errors.New("anontlstest: malformed ClientKeyExchange")
			}
			return new(big.Int).Exp(new(big.Int).SetBytes(ckx[2:]), y, p).Bytes(), nil
		}
	}
	flight := [][]byte{hsMsg(2, sh), hsMsg(12, ske), hsMsg(14, nil)}
	for _, m := range flight {
		transcript.Write(m)
	}
	if err := s.writeRecord(22, bytes.Join(flight, nil)); err != nil {
		return neg, err
	}

	typ, cke, err := s.readRecord()
	if err != nil {
		return neg, err
	}
	if typ != 22 || len(cke) < 4 || cke[0] != 16 {
		return neg, fmt.Errorf("anontlstest: expected ClientKeyExchange (record %d)", typ)
	}
	transcript.Write(cke)
	pm, err := premaster(cke[4:])
	if err != nil {
		return neg, err
	}
	sum := func() []byte {
		h := st.prfHash()
		h.Write(transcript.Bytes())
		return h.Sum(nil)
	}
	var master []byte
	if neg.EMS {
		master = prf(st.prfHash, pm, "extended master secret", sum(), 48)
	} else {
		master = prf(st.prfHash, pm, "master secret", append(append([]byte{}, clientRandom...), serverRandom...), 48)
	}
	macLen, ivLen := 0, 0
	if st.gcm {
		ivLen = 4
	} else {
		macLen = st.mac().Size()
	}
	kb := prf(st.prfHash, master, "key expansion", append(append([]byte{}, serverRandom...), clientRandom...),
		2*macLen+2*st.keyLen+2*ivLen)
	take := func(n int) []byte {
		v := kb[:n]
		kb = kb[n:]
		return v
	}
	cMAC, sMAC, cKey, sKey, cIV, sIV := take(macLen), take(macLen), take(st.keyLen), take(st.keyLen), take(ivLen), take(ivLen)
	mk := func(macKey, key, iv []byte) *direction {
		block, _ := aes.NewCipher(key)
		d := &direction{block: block, etm: neg.ETM}
		if st.gcm {
			d.gcm, _ = cipher.NewGCM(block)
			d.salt = append([]byte{}, iv...)
		} else {
			d.mac = hmac.New(st.mac, macKey)
		}
		return d
	}
	if typ, data, err := s.readRecord(); err != nil || typ != 20 || !bytes.Equal(data, []byte{1}) {
		return neg, fmt.Errorf("anontlstest: expected ChangeCipherSpec (%d %v)", typ, err)
	}
	s.in = mk(cMAC, cKey, cIV)
	typ, fin, err := s.readRecord()
	if err != nil || typ != 22 || len(fin) != 16 || fin[0] != 20 {
		return neg, fmt.Errorf("anontlstest: expected Finished (%d %v)", typ, err)
	}
	if !hmac.Equal(fin[4:], prf(st.prfHash, master, "client finished", sum(), 12)) {
		_ = s.writeRecord(21, []byte{2, 51})
		return neg, errors.New("anontlstest: client Finished does not verify")
	}
	transcript.Write(fin)
	if err := s.writeRecord(20, []byte{1}); err != nil {
		return neg, err
	}
	s.out = mk(sMAC, sKey, sIV)
	return neg, s.writeRecord(22, hsMsg(20, prf(st.prfHash, master, "server finished", sum(), 12)))
}

// Read returns application data (alerts end the stream).
func (s *conn) Read(b []byte) (int, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	for len(s.pending) == 0 {
		typ, data, err := s.readRecord()
		if err != nil {
			return 0, err
		}
		switch typ {
		case 23:
			s.pending = data
		case 21:
			if len(data) == 2 && data[1] == 0 {
				return 0, io.EOF
			}
			return 0, fmt.Errorf("anontlstest: alert %v", data)
		default:
			return 0, fmt.Errorf("anontlstest: unexpected record %d", typ)
		}
	}
	n := copy(b, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// Write sends application data.
func (s *conn) Write(b []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	for off := 0; off < len(b); off += 1 << 14 {
		if err := s.writeRecord(23, b[off:min(len(b), off+1<<14)]); err != nil {
			return off, err
		}
	}
	return len(b), nil
}
