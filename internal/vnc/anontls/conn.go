// Package anontls is a minimal TLS 1.2 client for the anonymous key-exchange cipher suites (TLS_DH_anon_*,
// TLS_ECDH_anon_*), which Go's crypto/tls deliberately does not implement.
//
// VNC servers use anonymous TLS for the VeNCrypt security types TLSNone, TLSVnc and TLSPlain (TigerVNC, QEMU,
// x11vnc): the channel is encrypted but the server is not authenticated, so it protects against passive
// eavesdropping only — an active attacker can run its own key exchange with both sides. NexTerm prefers the
// certificate-based X509* types (crypto/tls) whenever a server offers them.
//
// Scope: full handshakes only (no resumption, renegotiation, tickets or client certificates); AES-GCM and AES-CBC
// record protection, CBC with encrypt-then-MAC (RFC 7366) when the server supports it and otherwise
// MAC-then-encrypt hardened like crypto/tls (constant-time padding check, Lucky Thirteen mitigation); ECDH over
// X25519 / P-256 / P-384 (crypto/ecdh validates the points) and finite-field DH with RFC 7919 groups advertised,
// the standard groups recognized and other groups of 2048 to 8192 bits accepted after a primality test (see dh.go);
// extended master secret (RFC 7627) and the renegotiation_info extension (RFC 5746) are negotiated. Defensive limits
// follow crypto/tls: 16 KiB records (+2 KiB expansion), 64 KiB handshake messages, at most 16 consecutive records
// that make no progress (empty records, warning alerts, HelloRequests).
package anontls

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"time"
)

// Config configures a client connection.
type Config struct {
	// CipherSuites restricts and orders the offered suites (nil: DefaultCipherSuites()).
	CipherSuites []uint16
	// MinDHBits is the smallest finite-field DH modulus accepted (default 2048). A smaller server group fails the
	// handshake with an insufficient_security alert (IsHandshakeFailure reports it).
	MinDHBits int
	// DisableEncryptThenMAC stops offering RFC 7366 encrypt-then-MAC for the CBC suites.
	DisableEncryptThenMAC bool
	// Rand is the entropy source (default crypto/rand). Go ignores it for ECDH key generation.
	Rand io.Reader
}

// ConnectionState describes an established connection.
type ConnectionState struct {
	Version         uint16
	CipherSuite     uint16
	CipherSuiteName string
	// Group is the key-exchange group: "X25519", "P-256", "P-384", a standard finite-field group ("ffdhe2048" …
	// "ffdhe8192", "modp2048" … "modp8192"), or "DH-<bits>" for other finite-field groups.
	Group string
	// DHBits is the finite-field modulus size (0 for ECDH).
	DHBits               int
	ExtendedMasterSecret bool
	// EncryptThenMAC reports RFC 7366 record protection (CBC suites).
	EncryptThenMAC bool
}

// AlertError is a fatal alert received from (or sent to) the peer.
type AlertError struct {
	Description uint8
	Remote      bool
}

func (e *AlertError) Error() string {
	who := "local"
	if e.Remote {
		who = "remote"
	}
	return fmt.Sprintf("anontls: %s error: %s", who, alertText(e.Description))
}

// Alert descriptions (RFC 5246 section 7.2) and handshake message types.
const (
	alertCloseNotify         uint8 = 0
	alertUnexpectedMessage   uint8 = 10
	alertBadRecordMAC        uint8 = 20
	alertRecordOverflow      uint8 = 22
	alertHandshakeFailure    uint8 = 40
	alertIllegalParameter    uint8 = 47
	alertDecodeError         uint8 = 50
	alertDecryptError        uint8 = 51
	alertProtocolVersion     uint8 = 70
	alertInsufficientSec     uint8 = 71
	alertInternalError       uint8 = 80
	alertUserCanceled        uint8 = 90
	alertNoRenegotiation     uint8 = 100
	alertUnsupportedExt      uint8 = 110
	alertLevelWarning        uint8 = 1
	alertLevelFatal          uint8 = 2
	handshakeHelloRequest    uint8 = 0
	handshakeClientHello     uint8 = 1
	handshakeServerHello     uint8 = 2
	handshakeCertificate     uint8 = 11
	handshakeServerKeyExch   uint8 = 12
	handshakeCertRequest     uint8 = 13
	handshakeServerHelloDone uint8 = 14
	handshakeClientKeyExch   uint8 = 16
	handshakeFinished        uint8 = 20
	maxHandshakeMsg                = 1 << 16
	maxUselessRecords              = 16 // consecutive records that make no progress (crypto/tls uses the same bound)
)

// TLS extensions used by the client.
const (
	extSupportedGroups    uint16 = 10
	extECPointFormats     uint16 = 11
	extSignatureAlgs      uint16 = 13
	extEncryptThenMAC     uint16 = 22
	extExtendedMasterSec  uint16 = 23
	extRenegotiationInfo  uint16 = 0xff01
	curveTypeNamedCurve   uint8  = 3
	pointFormatUncompress uint8  = 0
)

func alertText(d uint8) string {
	switch d {
	case alertCloseNotify:
		return "close notify"
	case alertUnexpectedMessage:
		return "unexpected message"
	case alertBadRecordMAC:
		return "bad record MAC"
	case alertRecordOverflow:
		return "record overflow"
	case alertHandshakeFailure:
		return "handshake failure"
	case alertIllegalParameter:
		return "illegal parameter"
	case alertDecodeError:
		return "decode error"
	case alertDecryptError:
		return "decrypt error"
	case alertProtocolVersion:
		return "protocol version not supported"
	case alertInsufficientSec:
		return "insufficient security"
	case alertInternalError:
		return "internal error"
	case alertUserCanceled:
		return "user canceled"
	case alertNoRenegotiation:
		return "no renegotiation"
	case alertUnsupportedExt:
		return "unsupported extension"
	}
	return fmt.Sprintf("alert %d", d)
}

// IsHandshakeFailure reports whether err means that the server and this client share no acceptable parameters, i.e.
// anonymous TLS cannot be used with that server: the server refused the offer (handshake_failure,
// insufficient_security or protocol_version alert) or offered a Diffie-Hellman group this client rejects as too weak.
// Malformed or tampered handshakes are not handshake failures.
func IsHandshakeFailure(err error) bool {
	var ae *AlertError
	if errors.As(err, &ae) && ae.Remote {
		return ae.Description == alertHandshakeFailure || ae.Description == alertInsufficientSec ||
			ae.Description == alertProtocolVersion
	}
	var la *localAlert
	return errors.As(err, &la) && la.desc == alertInsufficientSec
}

// Conn is a TLS client connection. It implements net.Conn.
type Conn struct {
	conn net.Conn
	cfg  Config
	rnd  io.Reader

	handshakeMu  sync.Mutex
	handshakeErr error
	handshaked   bool
	state        ConnectionState

	// Read side: owned by the handshake while it runs, then guarded by readMu.
	readMu     sync.Mutex
	in         *halfConn
	vers       uint16 // record-layer version, enforced once the server chose it (0 before ServerHello)
	hsBuf      []byte // handshake bytes not yet consumed
	input      []byte // decrypted application data not yet returned
	readErr    error
	retryCount int // consecutive records without progress
	hdr        [recordHeaderLen]byte

	writeMu         sync.Mutex
	out             *halfConn
	writeErr        error
	closed          bool
	closeNotifySent bool
}

var errShutdown = errors.New("anontls: protocol is shut down (close_notify sent)")

// Client returns a TLS client over conn. The handshake runs on the first Read/Write or an explicit Handshake call.
func Client(conn net.Conn, cfg *Config) *Conn {
	c := &Conn{conn: conn}
	if cfg != nil {
		c.cfg = *cfg
	}
	c.rnd = c.cfg.Rand
	if c.rnd == nil {
		c.rnd = rand.Reader
	}
	if c.cfg.MinDHBits <= 0 {
		c.cfg.MinDHBits = defaultMinDHBits
	}
	return c
}

// ConnectionState returns the negotiated parameters (zero before the handshake).
func (c *Conn) ConnectionState() ConnectionState {
	c.handshakeMu.Lock()
	defer c.handshakeMu.Unlock()
	return c.state
}

// Handshake runs the client handshake (once).
func (c *Conn) Handshake() error { return c.HandshakeContext(context.Background()) }

// HandshakeContext runs the handshake, aborting (and failing the connection) when ctx ends first.
func (c *Conn) HandshakeContext(ctx context.Context) error {
	c.handshakeMu.Lock()
	defer c.handshakeMu.Unlock()
	if c.handshaked || c.handshakeErr != nil {
		return c.handshakeErr
	}
	if err := ctx.Err(); err != nil {
		c.handshakeErr = err
		return err
	}
	var stop func() bool
	if ctx.Done() != nil {
		stop = context.AfterFunc(ctx, func() { _ = c.conn.SetDeadline(time.Unix(1, 0)) })
	}
	err := c.clientHandshake()
	if stop != nil && !stop() {
		// ctx ended during the handshake: its deadline interrupted the I/O.
		_ = c.conn.SetDeadline(time.Time{})
		if err == nil {
			err = ctx.Err()
		} else {
			err = fmt.Errorf("%w (%v)", ctx.Err(), err)
		}
	}
	if err != nil {
		if shouldAlert(err) && ctx.Err() == nil {
			c.sendAlert(alertOf(err))
		}
		c.handshakeErr = err
		return err
	}
	c.handshaked = true
	return nil
}

// localAlert marks a failure detected locally with the alert to send.
type localAlert struct {
	desc uint8
	err  error
}

func (e *localAlert) Error() string { return "anontls: " + e.err.Error() }
func (e *localAlert) Unwrap() error { return e.err }

func fail(desc uint8, format string, args ...any) error {
	return &localAlert{desc: desc, err: fmt.Errorf(format, args...)}
}

func alertOf(err error) uint8 {
	var la *localAlert
	if errors.As(err, &la) {
		return la.desc
	}
	if errors.Is(err, errBadRecordMAC) {
		return alertBadRecordMAC
	}
	return alertInternalError
}

// shouldAlert reports whether a failure is ours to announce with a fatal alert (not a transport error, not the
// peer's own alert, not a clean close).
func shouldAlert(err error) bool {
	var ae *AlertError
	return !errors.As(err, &ae) && !isNetErr(err) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// useless counts a record that made no progress; too many in a row end the connection.
func (c *Conn) useless() error {
	c.retryCount++
	if c.retryCount > maxUselessRecords {
		return fail(alertUnexpectedMessage, "too many ignored records")
	}
	return nil
}

// ---- handshake ----------------------------------------------------------------------------------------------------

type handshakeState struct {
	suites       []*suite
	offeredETM   bool
	clientRandom []byte
	serverRandom []byte
	suite        *suite
	ems          bool
	etm          bool
	transcript   bytes.Buffer
}

func (c *Conn) offeredSuites() ([]*suite, error) {
	ids := c.cfg.CipherSuites
	if len(ids) == 0 {
		ids = DefaultCipherSuites()
	}
	var out []*suite
	for _, id := range ids {
		s := suiteByID(id)
		if s == nil {
			return nil, fmt.Errorf("anontls: unsupported cipher suite 0x%04x", id)
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out, nil
}

// Named groups (RFC 8422, RFC 7919).
const (
	groupP256      uint16 = 23
	groupP384      uint16 = 24
	groupX25519    uint16 = 29
	groupFFDHE2048 uint16 = 256
	groupFFDHE3072 uint16 = 257
	groupFFDHE4096 uint16 = 258
)

func (c *Conn) clientHandshake() error {
	hs := &handshakeState{}
	var err error
	if hs.suites, err = c.offeredSuites(); err != nil {
		return err
	}
	hs.clientRandom = make([]byte, 32)
	if _, err := io.ReadFull(c.rnd, hs.clientRandom); err != nil {
		return err
	}
	hello := c.buildClientHello(hs)
	hs.transcript.Write(hello)
	if err := c.writeRecordLocked(recordHandshake, hello, versionTLS10); err != nil {
		return err
	}

	// ServerHello
	typ, msg, err := c.readHandshake()
	if err != nil {
		return err
	}
	if typ != handshakeServerHello {
		return fail(alertUnexpectedMessage, "expected ServerHello, got handshake message %d", typ)
	}
	hs.transcript.Write(msg)
	if err := c.parseServerHello(hs, msg[4:]); err != nil {
		return err
	}
	c.vers = versionTLS12 // every further record must carry the negotiated version

	// ServerKeyExchange (anonymous suites never send a Certificate)
	typ, msg, err = c.readHandshake()
	if err != nil {
		return err
	}
	switch typ {
	case handshakeServerKeyExch:
	case handshakeCertificate:
		return fail(alertUnexpectedMessage, "server sent a certificate for an anonymous cipher suite")
	default:
		return fail(alertUnexpectedMessage, "expected ServerKeyExchange, got handshake message %d", typ)
	}
	hs.transcript.Write(msg)
	premaster, clientKX, group, dhBits, err := c.keyExchange(hs, msg[4:])
	if err != nil {
		return err
	}
	defer clear(premaster)

	// ServerHelloDone
	typ, msg, err = c.readHandshake()
	if err != nil {
		return err
	}
	switch typ {
	case handshakeServerHelloDone:
	case handshakeCertRequest:
		return fail(alertHandshakeFailure, "anonymous server requested a client certificate")
	default:
		return fail(alertUnexpectedMessage, "expected ServerHelloDone, got handshake message %d", typ)
	}
	if len(msg) != 4 {
		return fail(alertDecodeError, "malformed ServerHelloDone")
	}
	if len(c.hsBuf) != 0 {
		return fail(alertUnexpectedMessage, "unexpected handshake data after ServerHelloDone")
	}
	hs.transcript.Write(msg)

	// ClientKeyExchange
	cke := handshakeMessage(handshakeClientKeyExch, clientKX)
	hs.transcript.Write(cke)
	if err := c.writeRecordLocked(recordHandshake, cke, versionTLS12); err != nil {
		return err
	}

	// Keys
	s := hs.suite
	var master []byte
	if hs.ems {
		master = prf12(s.prfHash, premaster, "extended master secret", hashOf(s, hs.transcript.Bytes()), 48)
	} else {
		seed := append(append([]byte{}, hs.clientRandom...), hs.serverRandom...)
		master = prf12(s.prfHash, premaster, "master secret", seed, 48)
	}
	defer clear(master)
	macLen, keyLen, ivLen := s.macKeyLen(), s.keyLen, s.fixedIVLen()
	seed := append(append([]byte{}, hs.serverRandom...), hs.clientRandom...)
	kb := prf12(s.prfHash, master, "key expansion", seed, 2*macLen+2*keyLen+2*ivLen)
	defer clear(kb)
	cMAC, rest := kb[:macLen], kb[macLen:]
	sMAC, rest := rest[:macLen], rest[macLen:]
	cKey, rest := rest[:keyLen], rest[keyLen:]
	sKey, rest := rest[:keyLen], rest[keyLen:]
	cIV, sIV := rest[:ivLen], rest[ivLen:2*ivLen]
	clientHC, err := s.newHalfConn(cMAC, cKey, cIV, hs.etm)
	if err != nil {
		return err
	}
	serverHC, err := s.newHalfConn(sMAC, sKey, sIV, hs.etm)
	if err != nil {
		return err
	}

	// ChangeCipherSpec + Finished
	if err := c.writeRecordLocked(recordChangeCipherSpec, []byte{1}, versionTLS12); err != nil {
		return err
	}
	c.writeMu.Lock()
	c.out = clientHC
	c.writeMu.Unlock()
	verify := prf12(s.prfHash, master, "client finished", hashOf(s, hs.transcript.Bytes()), 12)
	fin := handshakeMessage(handshakeFinished, verify)
	hs.transcript.Write(fin)
	if err := c.writeRecordLocked(recordHandshake, fin, versionTLS12); err != nil {
		return err
	}

	// Server ChangeCipherSpec
	if err := c.readChangeCipherSpec(); err != nil {
		return err
	}
	c.readMu.Lock()
	c.in = serverHC
	c.readMu.Unlock()

	// Server Finished
	typ, msg, err = c.readHandshake()
	if err != nil {
		return err
	}
	if typ != handshakeFinished {
		return fail(alertUnexpectedMessage, "expected Finished, got handshake message %d", typ)
	}
	expected := prf12(s.prfHash, master, "server finished", hashOf(s, hs.transcript.Bytes()), 12)
	if len(msg) != 4+12 || subtle.ConstantTimeCompare(msg[4:], expected) != 1 {
		return fail(alertDecryptError, "server Finished verification failed")
	}
	if len(c.hsBuf) != 0 {
		return fail(alertUnexpectedMessage, "unexpected handshake data after Finished")
	}
	c.retryCount = 0
	c.state = ConnectionState{
		Version:              versionTLS12,
		CipherSuite:          s.id,
		CipherSuiteName:      s.name,
		Group:                group,
		DHBits:               dhBits,
		ExtendedMasterSecret: hs.ems,
		EncryptThenMAC:       hs.etm,
	}
	return nil
}

func hashOf(s *suite, data []byte) []byte {
	h := s.prfHash()
	h.Write(data)
	return h.Sum(nil)
}

func handshakeMessage(typ uint8, body []byte) []byte {
	m := make([]byte, 4, 4+len(body))
	m[0] = typ
	m[1], m[2], m[3] = byte(len(body)>>16), byte(len(body)>>8), byte(len(body))
	return append(m, body...)
}

func (c *Conn) buildClientHello(hs *handshakeState) []byte {
	var b []byte
	b = binary.BigEndian.AppendUint16(b, versionTLS12)
	b = append(b, hs.clientRandom...)
	b = append(b, 0) // empty session id: no resumption
	var hasDH, hasEC, hasCBC bool
	cs := make([]byte, 0, 2*len(hs.suites)+2)
	for _, s := range hs.suites {
		cs = binary.BigEndian.AppendUint16(cs, s.id)
		if s.kx == kxDH {
			hasDH = true
		} else {
			hasEC = true
		}
		if !s.aead {
			hasCBC = true
		}
	}
	b = binary.BigEndian.AppendUint16(b, uint16(len(cs)))
	b = append(b, cs...)
	b = append(b, 1, 0) // compression: null only

	var ext []byte
	addExt := func(typ uint16, data []byte) {
		ext = binary.BigEndian.AppendUint16(ext, typ)
		ext = binary.BigEndian.AppendUint16(ext, uint16(len(data)))
		ext = append(ext, data...)
	}
	var groups []byte
	if hasEC {
		for _, g := range []uint16{groupX25519, groupP256, groupP384} {
			groups = binary.BigEndian.AppendUint16(groups, g)
		}
	}
	if hasDH {
		// RFC 7919: servers that support it pick one of these groups instead of their own parameters.
		for _, g := range []uint16{groupFFDHE2048, groupFFDHE3072, groupFFDHE4096} {
			groups = binary.BigEndian.AppendUint16(groups, g)
		}
	}
	addExt(extSupportedGroups, append(binary.BigEndian.AppendUint16(nil, uint16(len(groups))), groups...))
	if hasEC {
		addExt(extECPointFormats, []byte{1, pointFormatUncompress})
	}
	// signature_algorithms (not used by anonymous suites, but expected in TLS 1.2 ClientHellos)
	var sigs []byte
	for _, a := range []uint16{0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601, 0x0201} {
		sigs = binary.BigEndian.AppendUint16(sigs, a)
	}
	addExt(extSignatureAlgs, append(binary.BigEndian.AppendUint16(nil, uint16(len(sigs))), sigs...))
	if hasCBC && !c.cfg.DisableEncryptThenMAC {
		addExt(extEncryptThenMAC, nil)
		hs.offeredETM = true
	}
	addExt(extExtendedMasterSec, nil)
	addExt(extRenegotiationInfo, []byte{0}) // initial handshake: empty renegotiated_connection

	b = binary.BigEndian.AppendUint16(b, uint16(len(ext)))
	b = append(b, ext...)
	return handshakeMessage(handshakeClientHello, b)
}

func (c *Conn) parseServerHello(hs *handshakeState, body []byte) error {
	r := reader(body)
	vers, ok := r.u16()
	if !ok {
		return fail(alertDecodeError, "malformed ServerHello")
	}
	if vers != versionTLS12 {
		return fail(alertProtocolVersion, "server negotiated TLS version 0x%04x (only TLS 1.2 supports anonymous key exchange)", vers)
	}
	random, ok := r.bytes(32)
	if !ok {
		return fail(alertDecodeError, "malformed ServerHello")
	}
	hs.serverRandom = append([]byte(nil), random...)
	sidLen, ok := r.u8()
	if !ok || sidLen > 32 {
		return fail(alertDecodeError, "malformed ServerHello session id")
	}
	if _, ok := r.bytes(int(sidLen)); !ok {
		return fail(alertDecodeError, "malformed ServerHello")
	}
	id, ok := r.u16()
	if !ok {
		return fail(alertDecodeError, "malformed ServerHello")
	}
	for _, s := range hs.suites {
		if s.id == id {
			hs.suite = s
		}
	}
	if hs.suite == nil {
		return fail(alertIllegalParameter, "server selected a cipher suite that was not offered (0x%04x)", id)
	}
	comp, ok := r.u8()
	if !ok || comp != 0 {
		return fail(alertIllegalParameter, "server selected compression")
	}
	if len(r) == 0 {
		return nil
	}
	extLen, ok := r.u16()
	if !ok || int(extLen) != len(r) {
		return fail(alertDecodeError, "malformed ServerHello extensions")
	}
	seen := map[uint16]bool{}
	for len(r) > 0 {
		typ, ok1 := r.u16()
		n, ok2 := r.u16()
		data, ok3 := r.bytes(int(n))
		if !ok1 || !ok2 || !ok3 {
			return fail(alertDecodeError, "malformed ServerHello extension")
		}
		if seen[typ] {
			return fail(alertIllegalParameter, "duplicate ServerHello extension %d", typ)
		}
		seen[typ] = true
		switch typ {
		case extRenegotiationInfo: // must be empty on the initial handshake
			if len(data) != 1 || data[0] != 0 {
				return fail(alertHandshakeFailure, "invalid renegotiation_info")
			}
		case extExtendedMasterSec:
			if len(data) != 0 {
				return fail(alertDecodeError, "malformed extended_master_secret")
			}
			hs.ems = true
		case extEncryptThenMAC:
			if len(data) != 0 {
				return fail(alertDecodeError, "malformed encrypt_then_mac")
			}
			if !hs.offeredETM {
				return fail(alertUnsupportedExt, "server sent an unsolicited encrypt_then_mac extension")
			}
			// RFC 7366 applies to CBC suites only; with AEAD it is meaningless (OpenSSL ignores it too).
			hs.etm = !hs.suite.aead
		case extECPointFormats: // uncompressed must be supported
			if len(data) < 1 || int(data[0]) != len(data)-1 || !bytes.Contains(data[1:], []byte{pointFormatUncompress}) {
				return fail(alertIllegalParameter, "server does not support uncompressed EC points")
			}
		default:
			// Other extensions (e.g. session_ticket) are harmless for a full handshake without resumption.
		}
	}
	return nil
}

// keyExchange parses the ServerKeyExchange and computes the premaster secret and ClientKeyExchange body.
func (c *Conn) keyExchange(hs *handshakeState, body []byte) (premaster, clientKX []byte, group string, dhBits int, err error) {
	r := reader(body)
	if hs.suite.kx == kxECDH {
		curveType, ok1 := r.u8()
		named, ok2 := r.u16()
		n, ok3 := r.u8()
		point, ok4 := r.bytes(int(n))
		if !ok1 || !ok2 || !ok3 || !ok4 || len(r) != 0 || curveType != curveTypeNamedCurve {
			return nil, nil, "", 0, fail(alertDecodeError, "malformed ECDH ServerKeyExchange")
		}
		var curve ecdh.Curve
		switch named {
		case groupX25519:
			curve, group = ecdh.X25519(), "X25519"
		case groupP256:
			curve, group = ecdh.P256(), "P-256"
		case groupP384:
			curve, group = ecdh.P384(), "P-384"
		default:
			return nil, nil, "", 0, fail(alertIllegalParameter, "server selected an unsupported curve %d", named)
		}
		// NewPublicKey rejects points off the curve, the point at infinity and compressed encodings; X25519 rejects
		// low-order points in ECDH (all-zero shared secret).
		peer, err := curve.NewPublicKey(point)
		if err != nil {
			return nil, nil, "", 0, fail(alertIllegalParameter, "invalid server ECDH public key: %v", err)
		}
		priv, err := curve.GenerateKey(c.rnd)
		if err != nil {
			return nil, nil, "", 0, err
		}
		shared, err := priv.ECDH(peer)
		if err != nil {
			return nil, nil, "", 0, fail(alertIllegalParameter, "ECDH failed: %v", err)
		}
		pub := priv.PublicKey().Bytes()
		return shared, append([]byte{byte(len(pub))}, pub...), group, 0, nil
	}

	pLen, ok1 := r.u16()
	pBytes, ok2 := r.bytes(int(pLen))
	gLen, ok3 := r.u16()
	gBytes, ok4 := r.bytes(int(gLen))
	yLen, ok5 := r.u16()
	yBytes, ok6 := r.bytes(int(yLen))
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || len(r) != 0 {
		return nil, nil, "", 0, fail(alertDecodeError, "malformed DH ServerKeyExchange")
	}
	g, ys, err := parseDHGroup(pBytes, gBytes, yBytes, c.cfg.MinDHBits)
	if err != nil {
		return nil, nil, "", 0, err
	}
	yc, z, err := g.exchange(c.rnd, ys)
	if err != nil {
		return nil, nil, "", 0, err
	}
	kx := binary.BigEndian.AppendUint16(nil, uint16(len(yc)))
	return z, append(kx, yc...), g.name, g.p.BitLen(), nil
}

// ---- record I/O ---------------------------------------------------------------------------------------------------

// readRecord reads and deprotects one record. The handshake or c.readMu owns the read side.
func (c *Conn) readRecord() (uint8, []byte, error) {
	if _, err := io.ReadFull(c.conn, c.hdr[:]); err != nil {
		return 0, nil, err // io.EOF: clean close between records
	}
	typ := c.hdr[0]
	vers := binary.BigEndian.Uint16(c.hdr[1:3])
	n := int(binary.BigEndian.Uint16(c.hdr[3:5]))
	if typ < recordChangeCipherSpec || typ > recordApplicationData || c.hdr[1] != 3 {
		return 0, nil, fail(alertUnexpectedMessage, "not a TLS record (type %d, version 0x%04x)", typ, vers)
	}
	if c.vers != 0 && vers != c.vers {
		return 0, nil, fail(alertProtocolVersion, "received a record with version 0x%04x instead of 0x%04x", vers, c.vers)
	}
	if n > maxCiphertext || (c.in == nil && n > maxPlaintext) {
		return 0, nil, fail(alertRecordOverflow, "record too large (%d bytes)", n)
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(c.conn, data); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	if c.in != nil {
		pt, err := c.in.open(typ, vers, data)
		if err != nil {
			return 0, nil, err
		}
		data = pt
	}
	if len(data) > maxPlaintext {
		return 0, nil, fail(alertRecordOverflow, "record plaintext too large")
	}
	return typ, data, nil
}

// readHandshake returns the next handshake message (type and full bytes including the 4-byte header).
func (c *Conn) readHandshake() (uint8, []byte, error) {
	for {
		if len(c.hsBuf) >= 4 {
			n := int(c.hsBuf[1])<<16 | int(c.hsBuf[2])<<8 | int(c.hsBuf[3])
			if n > maxHandshakeMsg {
				return 0, nil, fail(alertIllegalParameter, "handshake message too large")
			}
			if len(c.hsBuf) >= 4+n {
				msg := append([]byte(nil), c.hsBuf[:4+n]...)
				c.hsBuf = c.hsBuf[4+n:]
				if msg[0] == handshakeHelloRequest && n == 0 {
					// Ignored during the handshake (RFC 5246 section 7.4.1.1), and not part of the transcript.
					if err := c.useless(); err != nil {
						return 0, nil, err
					}
					continue
				}
				c.retryCount = 0
				return msg[0], msg, nil
			}
		}
		typ, data, err := c.readRecord()
		if err != nil {
			return 0, nil, err
		}
		switch typ {
		case recordHandshake:
			if len(data) == 0 {
				return 0, nil, fail(alertUnexpectedMessage, "empty handshake record")
			}
			c.hsBuf = append(c.hsBuf, data...)
		case recordAlert:
			if err := c.handleAlert(data); err != nil {
				return 0, nil, err
			}
		default:
			return 0, nil, fail(alertUnexpectedMessage, "unexpected record type %d during the handshake", typ)
		}
	}
}

func (c *Conn) readChangeCipherSpec() error {
	for {
		// Handshake messages must not straddle the ChangeCipherSpec.
		if len(c.hsBuf) != 0 {
			return fail(alertUnexpectedMessage, "unexpected handshake data before ChangeCipherSpec")
		}
		typ, data, err := c.readRecord()
		if err != nil {
			return err
		}
		switch typ {
		case recordChangeCipherSpec:
			if len(data) != 1 || data[0] != 1 {
				return fail(alertDecodeError, "malformed ChangeCipherSpec")
			}
			return nil
		case recordAlert:
			if err := c.handleAlert(data); err != nil {
				return err
			}
		case recordHandshake:
			if len(data) == 0 {
				return fail(alertUnexpectedMessage, "empty handshake record")
			}
			c.hsBuf = append(c.hsBuf, data...)
			// Tolerate HelloRequests; anything else (e.g. an unrequested NewSessionTicket) is an error.
			for len(c.hsBuf) >= 4 && c.hsBuf[0] == handshakeHelloRequest && c.hsBuf[1]|c.hsBuf[2]|c.hsBuf[3] == 0 {
				c.hsBuf = c.hsBuf[4:]
				if err := c.useless(); err != nil {
					return err
				}
			}
			if len(c.hsBuf) != 0 {
				return fail(alertUnexpectedMessage, "unexpected handshake message %d before ChangeCipherSpec", c.hsBuf[0])
			}
		default:
			return fail(alertUnexpectedMessage, "unexpected record type %d before ChangeCipherSpec", typ)
		}
	}
}

// handleAlert processes an alert record: close_notify → io.EOF, other warnings are ignored (nil; counted as records
// without progress), fatal alerts → *AlertError.
func (c *Conn) handleAlert(data []byte) error {
	if len(data) != 2 {
		return fail(alertDecodeError, "malformed alert")
	}
	level, desc := data[0], data[1]
	if desc == alertCloseNotify {
		return io.EOF
	}
	if level == alertLevelWarning && desc != alertBadRecordMAC && desc != alertHandshakeFailure {
		return c.useless()
	}
	return &AlertError{Description: desc, Remote: true}
}

// writeRecordLocked writes data as one or more records of type typ (takes c.writeMu).
func (c *Conn) writeRecordLocked(typ uint8, data []byte, version uint16) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.writeRecord(typ, data, version)
	return err
}

// writeRecord writes data as records of at most 16 KiB (c.writeMu held) and returns the plaintext bytes written.
func (c *Conn) writeRecord(typ uint8, data []byte, version uint16) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	written := 0
	for first := true; first || len(data) > 0; first = false {
		chunk := data[:min(len(data), maxPlaintext)]
		data = data[len(chunk):]
		frag := chunk
		if c.out != nil {
			var err error
			if frag, err = c.out.seal(typ, versionTLS12, chunk, c.rnd); err != nil {
				c.writeErr = err
				return written, err
			}
			version = versionTLS12
		}
		rec := make([]byte, recordHeaderLen, recordHeaderLen+len(frag))
		rec[0] = typ
		binary.BigEndian.PutUint16(rec[1:3], version)
		binary.BigEndian.PutUint16(rec[3:5], uint16(len(frag)))
		rec = append(rec, frag...)
		if _, err := c.conn.Write(rec); err != nil {
			c.writeErr = err
			return written, err
		}
		written += len(chunk)
	}
	return written, nil
}

// sendAlert sends an alert (best effort, bounded by a 2 s write deadline). After a fatal alert the connection
// accepts no more writes.
func (c *Conn) sendAlert(desc uint8) {
	level := alertLevelFatal
	if desc == alertCloseNotify || desc == alertNoRenegotiation {
		level = alertLevelWarning
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeErr != nil {
		return
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = c.writeRecord(recordAlert, []byte{level, desc}, versionTLS12)
	_ = c.conn.SetWriteDeadline(time.Time{})
	switch {
	case desc == alertCloseNotify:
		c.closeNotifySent = true
	case level == alertLevelFatal && c.writeErr == nil:
		c.writeErr = &AlertError{Description: desc}
	}
}

// ---- net.Conn -----------------------------------------------------------------------------------------------------

// Read reads application data.
func (c *Conn) Read(b []byte) (int, error) {
	if err := c.Handshake(); err != nil {
		return 0, err
	}
	if len(b) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for len(c.input) == 0 {
		if c.readErr != nil {
			return 0, c.readErr
		}
		if err := c.readOne(); err != nil {
			c.readErr = err
			if shouldAlert(err) {
				c.sendAlert(alertOf(err))
			}
			return 0, err
		}
	}
	n := copy(b, c.input)
	c.input = c.input[n:]
	return n, nil
}

// readOne processes one record after the handshake.
func (c *Conn) readOne() error {
	typ, data, err := c.readRecord()
	if err != nil {
		return err
	}
	switch typ {
	case recordApplicationData:
		if len(data) == 0 {
			return c.useless() // some servers send empty records (CBC IV randomization)
		}
		c.retryCount = 0
		c.input = data
		return nil
	case recordAlert:
		return c.handleAlert(data)
	case recordHandshake:
		if len(data) == 0 {
			return fail(alertUnexpectedMessage, "empty handshake record")
		}
		c.hsBuf = append(c.hsBuf, data...)
		// Only HelloRequest may follow the handshake; renegotiation is not supported, so it is declined with a
		// no_renegotiation warning (RFC 5246 section 7.2.2, RFC 5746 section 4.5).
		for len(c.hsBuf) > 0 {
			if c.hsBuf[0] != handshakeHelloRequest {
				return fail(alertUnexpectedMessage, "unexpected post-handshake message %d", c.hsBuf[0])
			}
			if len(c.hsBuf) < 4 {
				return nil // rest in the next record
			}
			if c.hsBuf[1]|c.hsBuf[2]|c.hsBuf[3] != 0 {
				return fail(alertDecodeError, "malformed HelloRequest")
			}
			c.hsBuf = c.hsBuf[4:]
			if err := c.useless(); err != nil {
				return err
			}
			c.sendAlert(alertNoRenegotiation)
		}
		return nil
	}
	return fail(alertUnexpectedMessage, "unexpected record type %d", typ)
}

func isNetErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, net.ErrClosed)
}

// Write writes application data.
func (c *Conn) Write(b []byte) (int, error) {
	if err := c.Handshake(); err != nil {
		return 0, err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	if c.closeNotifySent {
		return 0, errShutdown
	}
	if len(b) == 0 {
		return 0, nil
	}
	return c.writeRecord(recordApplicationData, b, versionTLS12)
}

// CloseWrite sends close_notify without closing the underlying connection; further writes fail.
func (c *Conn) CloseWrite() error {
	c.handshakeMu.Lock()
	done := c.handshaked
	c.handshakeMu.Unlock()
	if !done {
		return errors.New("anontls: CloseWrite before the handshake completed")
	}
	c.sendAlert(alertCloseNotify)
	return nil
}

// Close sends close_notify (best effort) and closes the underlying connection.
func (c *Conn) Close() error {
	c.writeMu.Lock()
	already := c.closed
	c.closed = true
	c.writeMu.Unlock()
	if already {
		return net.ErrClosed
	}
	if c.handshakeMu.TryLock() {
		done := c.handshaked
		c.handshakeMu.Unlock()
		if done {
			c.sendAlert(alertCloseNotify)
		}
	}
	return c.conn.Close()
}

func (c *Conn) LocalAddr() net.Addr                { return c.conn.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr               { return c.conn.RemoteAddr() }
func (c *Conn) SetDeadline(t time.Time) error      { return c.conn.SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

// ---- parsing helpers ----------------------------------------------------------------------------------------------

type reader []byte

func (r *reader) u8() (uint8, bool) {
	if len(*r) < 1 {
		return 0, false
	}
	v := (*r)[0]
	*r = (*r)[1:]
	return v, true
}

func (r *reader) u16() (uint16, bool) {
	if len(*r) < 2 {
		return 0, false
	}
	v := binary.BigEndian.Uint16(*r)
	*r = (*r)[2:]
	return v, true
}

func (r *reader) bytes(n int) ([]byte, bool) {
	if n < 0 || len(*r) < n {
		return nil, false
	}
	v := (*r)[:n]
	*r = (*r)[n:]
	return v, true
}
