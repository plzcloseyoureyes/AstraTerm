package anontls

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	mathrand "math/rand/v2"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// testServer is a scriptable server side of the anonymous TLS 1.2 handshake, built from this package's primitives
// (record protection, PRF, suites). Interoperability with real implementations is covered by the OpenSSL / GnuTLS
// tests; this server exists for negative tests (malformed, hostile or weak servers) and to produce fuzz corpora.
// It uses its own deterministic randomness, never crypto/rand, so that a client driven by
// testing/cryptotest.SetGlobalRandom behaves identically when a recorded server byte stream is replayed.
type testServer struct {
	suite uint16 // selected suite (default: the first offered)

	// Finite-field group (DH suites): a named standard group ("ffdhe2048" …) or explicit p/g.
	dhGroup    string
	dhP, dhG   *big.Int
	dhYs       func(p *big.Int) *big.Int // replaces the public value (invalid-value tests)
	curve      uint16                    // ECDH curve (default X25519)
	ecdhPoint  []byte                    // replaces the ECDH public point
	noEMS      bool                      // do not negotiate extended master secret
	noETM      bool                      // do not accept encrypt_then_mac
	etmAlways  bool                      // echo encrypt_then_mac even when not appropriate
	extraExt   []byte                    // raw extensions appended to ServerHello
	version    uint16                    // ServerHello version (default TLS 1.2)
	pickSuite  uint16                    // select this suite even if not offered
	badFinish  bool                      // corrupt the server Finished
	splitFirst bool                      // send the first flight one handshake message per record
	rnd        io.Reader

	// flight, when set, replaces the whole first flight (ServerHello … ServerHelloDone) with raw bytes.
	flight []byte

	// after runs once the handshake completed (default: echo application data until the client closes).
	after func(s *serverConn) error

	// Recorded by the handshake:
	clientExts map[uint16][]byte
}

// serverConn is the server's view of an established connection.
type serverConn struct {
	conn    net.Conn
	in, out *halfConn
	rnd     io.Reader
	etm     bool
}

func (s *serverConn) writeRecord(typ uint8, data []byte) error {
	return s.writeRecordVersion(typ, versionTLS12, data)
}

func (s *serverConn) writeRecordVersion(typ uint8, version uint16, data []byte) error {
	frag := data
	if s.out != nil {
		var err error
		if frag, err = s.out.seal(typ, version, data, s.rnd); err != nil {
			return err
		}
	}
	rec := []byte{typ, byte(version >> 8), byte(version)}
	rec = binary.BigEndian.AppendUint16(rec, uint16(len(frag)))
	_, err := s.conn.Write(append(rec, frag...))
	return err
}

func (s *serverConn) readRecord() (uint8, []byte, error) {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(s.conn, hdr); err != nil {
		return 0, nil, err
	}
	body := make([]byte, binary.BigEndian.Uint16(hdr[3:]))
	if _, err := io.ReadFull(s.conn, body); err != nil {
		return 0, nil, err
	}
	if s.in != nil {
		pt, err := s.in.open(hdr[0], binary.BigEndian.Uint16(hdr[1:3]), body)
		if err != nil {
			return 0, nil, err
		}
		body = pt
	}
	return hdr[0], body, nil
}

func detRand(seed uint64) io.Reader {
	var s [32]byte
	binary.LittleEndian.PutUint64(s[:], seed)
	return mathrand.NewChaCha8(s)
}

func mustBig(hexs string) *big.Int {
	b, err := hex.DecodeString(hexs)
	if err != nil {
		panic(err)
	}
	return new(big.Int).SetBytes(b)
}

func namedGroup(name string) *big.Int {
	for _, g := range knownGroupTable {
		if g.name == name {
			return mustBig(g.hex)
		}
	}
	panic("unknown group " + name)
}

// serve runs the server handshake on conn and then s.after.
func (ts *testServer) serve(conn net.Conn) error {
	rnd := ts.rnd
	if rnd == nil {
		rnd = detRand(99)
	}
	sc := &serverConn{conn: conn, rnd: rnd}
	var transcript bytes.Buffer

	// ClientHello (may span records)
	var hsBuf []byte
	for len(hsBuf) < 4 || len(hsBuf) < 4+(int(hsBuf[1])<<16|int(hsBuf[2])<<8|int(hsBuf[3])) {
		typ, data, err := sc.readRecord()
		if err != nil {
			return err
		}
		if typ != recordHandshake {
			return fmt.Errorf("server: expected handshake record, got %d", typ)
		}
		hsBuf = append(hsBuf, data...)
	}
	if hsBuf[0] != handshakeClientHello {
		return errors.New("server: expected ClientHello")
	}
	transcript.Write(hsBuf)
	r := reader(hsBuf[4:])
	r.u16()
	clientRandom, _ := r.bytes(32)
	sid, _ := r.u8()
	r.bytes(int(sid))
	csLen, _ := r.u16()
	csRaw, _ := r.bytes(int(csLen))
	compLen, _ := r.u8()
	r.bytes(int(compLen))
	ts.clientExts = map[uint16][]byte{}
	if extLen, ok := r.u16(); ok {
		exts, _ := r.bytes(int(extLen))
		er := reader(exts)
		for len(er) > 0 {
			t, _ := er.u16()
			n, _ := er.u16()
			d, _ := er.bytes(int(n))
			ts.clientExts[t] = d
		}
	}
	var offered []uint16
	for i := 0; i+1 < len(csRaw); i += 2 {
		offered = append(offered, binary.BigEndian.Uint16(csRaw[i:]))
	}
	id := ts.suite
	if id == 0 {
		id = offered[0]
	}
	if ts.pickSuite != 0 {
		id = ts.pickSuite
	} else if !containsU16(offered, id) {
		_ = sc.writeRecord(recordAlert, []byte{alertLevelFatal, alertHandshakeFailure})
		return errors.New("server: no common cipher suite")
	}
	st := suiteByID(id)
	if st == nil {
		st = suites[0] // pickSuite with an unknown ID: continue as if it were the first one
	}
	_, ems := ts.clientExts[extExtendedMasterSec]
	ems = ems && !ts.noEMS
	_, etmOffered := ts.clientExts[extEncryptThenMAC]
	etm := etmOffered && !ts.noETM && !st.aead
	sendETM := etm || ts.etmAlways

	serverRandom := make([]byte, 32)
	_, _ = io.ReadFull(rnd, serverRandom)
	var flight [][]byte
	if ts.flight == nil {
		// ServerHello
		ver := ts.version
		if ver == 0 {
			ver = versionTLS12
		}
		body := binary.BigEndian.AppendUint16(nil, ver)
		body = append(body, serverRandom...)
		body = append(body, 0)
		body = binary.BigEndian.AppendUint16(body, id)
		body = append(body, 0)
		var ext []byte
		add := func(t uint16, d []byte) {
			ext = binary.BigEndian.AppendUint16(ext, t)
			ext = binary.BigEndian.AppendUint16(ext, uint16(len(d)))
			ext = append(ext, d...)
		}
		add(extRenegotiationInfo, []byte{0})
		if ems {
			add(extExtendedMasterSec, nil)
		}
		if sendETM {
			add(extEncryptThenMAC, nil)
		}
		if st.kx == kxECDH {
			add(extECPointFormats, []byte{1, 0})
		}
		ext = append(ext, ts.extraExt...)
		body = binary.BigEndian.AppendUint16(body, uint16(len(ext)))
		body = append(body, ext...)
		flight = append(flight, handshakeMessage(handshakeServerHello, body))
	}

	// ServerKeyExchange
	var premasterFn func(ckx []byte) ([]byte, error)
	if st.kx == kxECDH {
		curveID := ts.curve
		if curveID == 0 {
			curveID = groupX25519
		}
		var curve ecdh.Curve
		var scalarLen int
		switch curveID {
		case groupP256:
			curve, scalarLen = ecdh.P256(), 32
		case groupP384:
			curve, scalarLen = ecdh.P384(), 48
		default:
			curve, scalarLen = ecdh.X25519(), 32
		}
		var priv *ecdh.PrivateKey
		for priv == nil {
			k := make([]byte, scalarLen)
			_, _ = io.ReadFull(rnd, k)
			priv, _ = curve.NewPrivateKey(k)
		}
		point := priv.PublicKey().Bytes()
		if ts.ecdhPoint != nil {
			point = ts.ecdhPoint
		}
		body := []byte{curveTypeNamedCurve}
		body = binary.BigEndian.AppendUint16(body, curveID)
		body = append(body, byte(len(point)))
		body = append(body, point...)
		flight = append(flight, handshakeMessage(handshakeServerKeyExch, body))
		premasterFn = func(ckx []byte) ([]byte, error) {
			if len(ckx) < 1 || int(ckx[0]) != len(ckx)-1 {
				return nil, errors.New("server: malformed ECDH ClientKeyExchange")
			}
			peer, err := curve.NewPublicKey(ckx[1:])
			if err != nil {
				return nil, err
			}
			return priv.ECDH(peer)
		}
	} else {
		p, g := ts.dhP, ts.dhG
		if p == nil {
			name := ts.dhGroup
			if name == "" {
				name = "ffdhe2048"
			}
			p, g = namedGroup(name), big.NewInt(2)
		}
		y, _ := rand.Int(rnd, new(big.Int).Sub(p, big.NewInt(3)))
		y.Add(y, big.NewInt(2))
		ys := new(big.Int).Exp(g, y, p)
		if ts.dhYs != nil {
			ys = ts.dhYs(p)
		}
		var body []byte
		for _, v := range []*big.Int{p, g, ys} {
			b := v.Bytes()
			body = binary.BigEndian.AppendUint16(body, uint16(len(b)))
			body = append(body, b...)
		}
		flight = append(flight, handshakeMessage(handshakeServerKeyExch, body))
		premasterFn = func(ckx []byte) ([]byte, error) {
			if len(ckx) < 2 || int(binary.BigEndian.Uint16(ckx)) != len(ckx)-2 {
				return nil, errors.New("server: malformed DH ClientKeyExchange")
			}
			yc := new(big.Int).SetBytes(ckx[2:])
			return new(big.Int).Exp(yc, y, p).Bytes(), nil
		}
	}
	flight = append(flight, handshakeMessage(handshakeServerHelloDone, nil))

	if ts.flight != nil {
		if _, err := conn.Write(ts.flight); err != nil {
			return err
		}
		// Nothing follows the scripted bytes: half-close, then read until the client gives up.
		if rc, ok := conn.(recordingConn); ok {
			if cw, ok := rc.Conn.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
		}
		_, _ = io.Copy(io.Discard, conn)
		return nil
	}
	for _, m := range flight {
		transcript.Write(m)
	}
	if ts.splitFirst {
		for _, m := range flight {
			if err := sc.writeRecordVersion(recordHandshake, versionTLS12, m); err != nil {
				return err
			}
		}
	} else if err := sc.writeRecordVersion(recordHandshake, versionTLS12, bytes.Join(flight, nil)); err != nil {
		return err
	}

	// ClientKeyExchange
	typ, data, err := sc.readRecord()
	if err != nil {
		return err
	}
	if typ != recordHandshake || len(data) < 4 || data[0] != handshakeClientKeyExch {
		return fmt.Errorf("server: expected ClientKeyExchange, got record %d", typ)
	}
	transcript.Write(data)
	premaster, err := premasterFn(data[4:])
	if err != nil {
		return err
	}
	var master []byte
	if ems {
		master = prf12(st.prfHash, premaster, "extended master secret", hashOf(st, transcript.Bytes()), 48)
	} else {
		master = prf12(st.prfHash, premaster, "master secret", append(append([]byte{}, clientRandom...), serverRandom...), 48)
	}
	macLen, keyLen, ivLen := st.macKeyLen(), st.keyLen, st.fixedIVLen()
	kb := prf12(st.prfHash, master, "key expansion", append(append([]byte{}, serverRandom...), clientRandom...),
		2*macLen+2*keyLen+2*ivLen)
	cMAC, sMAC := kb[:macLen], kb[macLen:2*macLen]
	cKey, sKey := kb[2*macLen:2*macLen+keyLen], kb[2*macLen+keyLen:2*macLen+2*keyLen]
	cIV, sIV := kb[2*macLen+2*keyLen:2*macLen+2*keyLen+ivLen], kb[2*macLen+2*keyLen+ivLen:]

	// Client ChangeCipherSpec + Finished
	if typ, data, err = sc.readRecord(); err != nil || typ != recordChangeCipherSpec || !bytes.Equal(data, []byte{1}) {
		return fmt.Errorf("server: expected ChangeCipherSpec (%d %x %v)", typ, data, err)
	}
	sc.in, _ = st.newHalfConn(cMAC, cKey, cIV, etm)
	if typ, data, err = sc.readRecord(); err != nil || typ != recordHandshake || len(data) != 16 || data[0] != handshakeFinished {
		return fmt.Errorf("server: expected client Finished (%d %v)", typ, err)
	}
	want := prf12(st.prfHash, master, "client finished", hashOf(st, transcript.Bytes()), 12)
	if !bytes.Equal(data[4:], want) {
		return errors.New("server: client Finished does not verify")
	}
	transcript.Write(data)

	// Server ChangeCipherSpec + Finished
	if err := sc.writeRecord(recordChangeCipherSpec, []byte{1}); err != nil {
		return err
	}
	sc.out, _ = st.newHalfConn(sMAC, sKey, sIV, etm)
	sc.etm = etm
	verify := prf12(st.prfHash, master, "server finished", hashOf(st, transcript.Bytes()), 12)
	if ts.badFinish {
		verify[0] ^= 1
	}
	if err := sc.writeRecord(recordHandshake, handshakeMessage(handshakeFinished, verify)); err != nil {
		return err
	}
	if ts.after != nil {
		return ts.after(sc)
	}
	return echo(sc)
}

// echo returns application data until the client closes.
func echo(s *serverConn) error {
	for {
		typ, data, err := s.readRecord()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch typ {
		case recordApplicationData:
			if err := s.writeRecord(recordApplicationData, data); err != nil {
				return err
			}
		case recordAlert:
			if len(data) == 2 && data[1] == alertCloseNotify {
				_ = s.writeRecord(recordAlert, []byte{alertLevelWarning, alertCloseNotify})
				return nil
			}
			return fmt.Errorf("server: received alert %v", data)
		default:
			return fmt.Errorf("server: unexpected record %d", typ)
		}
	}
}

func containsU16(list []uint16, v uint16) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// recordingConn records what the server writes (fromServer) and reads (fromClient).
type recordingConn struct {
	net.Conn
	fromServer, fromClient *syncBuffer
}

func (r recordingConn) Write(b []byte) (int, error) {
	r.fromServer.Write(b)
	return r.Conn.Write(b)
}

func (r recordingConn) Read(b []byte) (int, error) {
	n, err := r.Conn.Read(b)
	r.fromClient.Write(b[:n])
	return n, err
}

// pair is a client connected to a testServer over an in-memory pipe.
type pair struct {
	client                 *Conn
	serverErr              chan error
	fromServer, fromClient *syncBuffer
}

// wait returns the server's result.
func (p *pair) wait(t testing.TB) error {
	t.Helper()
	select {
	case err := <-p.serverErr:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("test server did not finish")
		return nil
	}
}

// clientAlert returns the first alert record the client sent (plaintext alerts only: before encryption started).
func (p *pair) clientAlert() (level, desc uint8, ok bool) {
	b := p.fromClient.Bytes()
	for len(b) >= 5 {
		n := int(binary.BigEndian.Uint16(b[3:5]))
		if len(b) < 5+n {
			return 0, 0, false
		}
		if b[0] == recordAlert && n == 2 {
			return b[5], b[6], true
		}
		b = b[5+n:]
	}
	return 0, 0, false
}

// tcpPair returns both ends of a loopback TCP connection (kernel-buffered, unlike net.Pipe, so a client alert can be
// written while the server is still writing).
func tcpPair(t testing.TB) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	return client, server
}

func runPair(t testing.TB, ts *testServer, cfg *Config) *pair {
	t.Helper()
	cc, sc := tcpPair(t)
	p := &pair{serverErr: make(chan error, 1), fromServer: &syncBuffer{}, fromClient: &syncBuffer{}}
	go func() {
		err := ts.serve(recordingConn{Conn: sc, fromServer: p.fromServer, fromClient: p.fromClient})
		sc.Close()
		p.serverErr <- err
	}()
	_ = cc.SetDeadline(time.Now().Add(20 * time.Second))
	p.client = Client(cc, cfg)
	return p
}

func errContains(err error, sub string) bool {
	return err != nil && strings.Contains(err.Error(), sub)
}
