package anontls

import (
	"bytes"
	"crypto/ecdh"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

// RFC 5114 groups (OpenSSL's dh_2048_256 and dh_1024_160): primes that are not in the standard-group table, used as
// a legitimate non-standard group (DSA-style, like GnuTLS-generated parameters) and as a too-small group.
const (
	rfc5114p2048 = "" +
		"87A8E61DB4B6663CFFBBD19C651959998CEEF608660DD0F25D2CEED4435E3B00E00DF8F1D61957D4FAF7DF4561B2AA30" +
		"16C3D91134096FAA3BF4296D830E9A7C209E0C6497517ABD5A8A9D306BCF67ED91F9E6725B4758C022E0B1EF4275BF7B" +
		"6C5BFC11D45F9088B941F54EB1E59BB8BC39A0BF12307F5C4FDB70C581B23F76B63ACAE1CAA6B7902D52526735488A0E" +
		"F13C6D9A51BFA4AB3AD8347796524D8EF6A167B5A41825D967E144E5140564251CCACB83E6B486F6B3CA3F7971506026" +
		"C0B857F689962856DED4010ABD0BE621C3A3960A54E710C375F26375D7014103A4B54330C198AF126116D2276E11715F" +
		"693877FAD7EF09CADB094AE91E1A1597"
	rfc5114g2048 = "" +
		"3FB32C9B73134D0B2E77506660EDBD484CA7B18F21EF205407F4793A1A0BA12510DBC15077BE463FFF4FED4AAC0BB555" +
		"BE3A6C1B0C6B47B1BC3773BF7E8C6F62901228F8C28CBB18A55AE31341000A650196F931C77A57F2DDF463E5E9EC144B" +
		"777DE62AAAB8A8628AC376D282D6ED3864E67982428EBC831D14348F6F2F9193B5045AF2767164E1DFC967C1FB3F2E55" +
		"A4BD1BFFE83B9C80D052B985D182EA0ADB2A3B7313D3FE14C8484B1E052588B9B7D2BBD2DF016199ECD06E1557CD0915" +
		"B3353BBB64E0EC377FD028370DF92B52C7891428CDC67EB6184B523D1DB246C32F63078490F00EF8D647D148D4795451" +
		"5E2327CFEF98C582664B4C0F6CC41659"
	rfc5114p1024 = "" +
		"B10B8F96A080E01DDE92DE5EAE5D54EC52C99FBCFB06A3C69A6A9DCA52D23B616073E28675A23D189838EF1E2EE652C0" +
		"13ECB4AEA906112324975C3CD49B83BFACCBDD7D90C4BD7098488E9C219A73724EFFD6FAE5644738FAA31A4FF55BCCC0" +
		"A151AF5F0DC8B4BD45BF37DF365C1A65E68CFDA76D4DA708DF1FB2BC2E4A4371"
	rfc5114g1024 = "" +
		"A4D1CBD5C3FD34126765A442EFB99905F8104DD258AC507FD6406CFF14266D31266FEA1E5C41564B777E690F5504F213" +
		"160217B4B01B886A5E91547F9E2749F4D7FBD7D3B9A92EE1909D0D2263F80A76A6A24C087A091F531DBF0A0169B6A28A" +
		"D662A4D18E73AFA32D779D5918D08BC8858F4DCEF97C2A24855E6EEB22B3B2E5"
)

// roundTrip checks that application data flows both ways through an established connection.
func roundTrip(t *testing.T, c *Conn) {
	t.Helper()
	msg := bytes.Repeat([]byte("vnc-rfb-stream "), 3000) // several records
	go func() { _, _ = c.Write(msg) }()
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("echo mismatch")
	}
}

func TestHandshakeAllSuitesAgainstTestServer(t *testing.T) {
	for _, s := range suites {
		for _, etm := range []bool{true, false} {
			for _, ems := range []bool{true, false} {
				if s.aead && !etm {
					continue
				}
				t.Run(fmt.Sprintf("%s/etm=%v/ems=%v", s.name, etm, ems), func(t *testing.T) {
					p := runPair(t, &testServer{suite: s.id, noETM: !etm, noEMS: !ems}, nil)
					if err := p.client.Handshake(); err != nil {
						t.Fatal(err)
					}
					st := p.client.ConnectionState()
					if st.CipherSuite != s.id || st.EncryptThenMAC != (etm && !s.aead) || st.ExtendedMasterSecret != ems {
						t.Fatalf("state %+v", st)
					}
					if s.kx == kxDH && (st.Group != "ffdhe2048" || st.DHBits != 2048) {
						t.Fatalf("group %q %d", st.Group, st.DHBits)
					}
					if s.kx == kxECDH && st.Group != "X25519" {
						t.Fatalf("group %q", st.Group)
					}
					roundTrip(t, p.client)
					if err := p.client.Close(); err != nil {
						t.Fatal(err)
					}
					if err := p.wait(t); err != nil {
						t.Fatalf("server: %v", err)
					}
				})
			}
		}
	}
}

func TestHandshakeGroups(t *testing.T) {
	for _, c := range []struct {
		name   string
		ts     testServer
		group  string
		failIs string
	}{
		{"ffdhe3072", testServer{suite: TLS_DH_anon_WITH_AES_128_GCM_SHA256, dhGroup: "ffdhe3072"}, "ffdhe3072", ""},
		{"modp2048", testServer{suite: TLS_DH_anon_WITH_AES_128_GCM_SHA256, dhGroup: "modp2048"}, "modp2048", ""},
		{"rfc5114-2048 (non-standard prime)", testServer{suite: TLS_DH_anon_WITH_AES_128_GCM_SHA256,
			dhP: mustBig(rfc5114p2048), dhG: mustBig(rfc5114g2048)}, "DH-2048", ""},
		{"P-256", testServer{suite: TLS_ECDH_anon_WITH_AES_128_CBC_SHA, curve: groupP256}, "P-256", ""},
		{"P-384", testServer{suite: TLS_ECDH_anon_WITH_AES_256_CBC_SHA, curve: groupP384}, "P-384", ""},
		{"1024-bit group", testServer{suite: TLS_DH_anon_WITH_AES_128_GCM_SHA256,
			dhP: mustBig(rfc5114p1024), dhG: mustBig(rfc5114g1024)}, "", "too small"},
		{"composite modulus", testServer{suite: TLS_DH_anon_WITH_AES_128_GCM_SHA256,
			dhP: new(big.Int).Mul(mustBig(rfc5114p2048), mustBig(rfc5114p1024)), dhG: big.NewInt(2)}, "", "not prime"},
		{"even modulus", testServer{suite: TLS_DH_anon_WITH_AES_128_GCM_SHA256,
			dhP: new(big.Int).Lsh(mustBig(rfc5114p2048), 1), dhG: big.NewInt(2)}, "", "not prime"},
		{"oversized group", testServer{suite: TLS_DH_anon_WITH_AES_128_GCM_SHA256,
			dhP: new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 8200), big.NewInt(1)), dhG: big.NewInt(2)}, "", "too large"},
		{"generator 1", testServer{suite: TLS_DH_anon_WITH_AES_128_GCM_SHA256, dhP: namedGroup("ffdhe2048"),
			dhG: big.NewInt(1)}, "", "generator"},
		{"public value 1", testServer{suite: TLS_DH_anon_WITH_AES_128_GCM_SHA256,
			dhYs: func(*big.Int) *big.Int { return big.NewInt(1) }}, "", "public value"},
		{"public value p-1 (order 2)", testServer{suite: TLS_DH_anon_WITH_AES_128_GCM_SHA256,
			dhYs: func(p *big.Int) *big.Int { return new(big.Int).Sub(p, big.NewInt(1)) }}, "", "public value"},
		{"public value p", testServer{suite: TLS_DH_anon_WITH_AES_128_GCM_SHA256,
			dhYs: func(p *big.Int) *big.Int { return p }}, "", "public value"},
		{"X25519 low-order point", testServer{suite: TLS_ECDH_anon_WITH_AES_128_CBC_SHA,
			ecdhPoint: make([]byte, 32)}, "", "ECDH"},
		{"P-256 point off the curve", testServer{suite: TLS_ECDH_anon_WITH_AES_128_CBC_SHA, curve: groupP256,
			ecdhPoint: append([]byte{4}, bytes.Repeat([]byte{7}, 64)...)}, "", "invalid server ECDH public key"},
		{"P-256 compressed point", testServer{suite: TLS_ECDH_anon_WITH_AES_128_CBC_SHA, curve: groupP256,
			ecdhPoint: append([]byte{2}, bytes.Repeat([]byte{7}, 32)...)}, "", "invalid server ECDH public key"},
		{"unsupported curve", testServer{suite: TLS_ECDH_anon_WITH_AES_128_CBC_SHA, curve: 25 /* secp521r1 */}, "",
			"unsupported curve"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts := c.ts
			p := runPair(t, &ts, nil)
			err := p.client.Handshake()
			if c.failIs != "" {
				if !errContains(err, c.failIs) {
					t.Fatalf("expected an error containing %q, got %v", c.failIs, err)
				}
				_ = p.wait(t)
				if _, desc, ok := p.clientAlert(); !ok || desc == alertCloseNotify {
					t.Fatalf("no fatal alert sent (%v %d)", ok, desc)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if st := p.client.ConnectionState(); st.Group != c.group {
				t.Fatalf("group %q, want %q", st.Group, c.group)
			}
			roundTrip(t, p.client)
		})
	}
}

// TestWeakGroupIsHandshakeFailure: a group below the minimum means "anonymous TLS unavailable" (the VNC layer then
// asks before connecting without encryption); malformed parameters do not.
func TestWeakGroupIsHandshakeFailure(t *testing.T) {
	p := runPair(t, &testServer{suite: TLS_DH_anon_WITH_AES_128_GCM_SHA256, dhP: mustBig(rfc5114p1024),
		dhG: mustBig(rfc5114g1024)}, nil)
	err := p.client.Handshake()
	if !IsHandshakeFailure(err) {
		t.Fatalf("weak group: IsHandshakeFailure(%v) = false", err)
	}
	_ = p.wait(t)
	if _, desc, _ := p.clientAlert(); desc != alertInsufficientSec {
		t.Fatalf("alert %d, want insufficient_security", desc)
	}
	p = runPair(t, &testServer{suite: TLS_DH_anon_WITH_AES_128_GCM_SHA256,
		dhYs: func(*big.Int) *big.Int { return big.NewInt(1) }}, nil)
	if err := p.client.Handshake(); err == nil || IsHandshakeFailure(err) {
		t.Fatalf("invalid public value: %v (handshake failure: %v)", err, IsHandshakeFailure(err))
	}
	// A server that refuses the offer.
	p = runPair(t, &testServer{suite: 0x1301 /* TLS 1.3 suite, never offered */}, nil)
	if err := p.client.Handshake(); !IsHandshakeFailure(err) {
		t.Fatalf("refused offer: %v", err)
	}
	// Lowering the minimum (tests / explicit configuration) accepts the 1024-bit group.
	p = runPair(t, &testServer{suite: TLS_DH_anon_WITH_AES_128_GCM_SHA256, dhP: mustBig(rfc5114p1024),
		dhG: mustBig(rfc5114g1024)}, &Config{MinDHBits: 1024})
	if err := p.client.Handshake(); err != nil {
		t.Fatal(err)
	}
	roundTrip(t, p.client)
}

func TestHandshakeProtocolViolations(t *testing.T) {
	hello := func(mut func(b []byte) []byte) []byte { // a minimal valid ServerHello for DH-GCM, then mutated
		body := binary.BigEndian.AppendUint16(nil, versionTLS12)
		body = append(body, make([]byte, 32)...)
		body = append(body, 0)
		body = binary.BigEndian.AppendUint16(body, TLS_DH_anon_WITH_AES_128_GCM_SHA256)
		body = append(body, 0)
		if mut != nil {
			body = mut(body)
		}
		return handshakeMessage(handshakeServerHello, body)
	}
	rec := func(typ uint8, vers uint16, data []byte) []byte {
		r := []byte{typ, byte(vers >> 8), byte(vers)}
		r = binary.BigEndian.AppendUint16(r, uint16(len(data)))
		return append(r, data...)
	}
	cases := []struct {
		name   string
		flight []byte
		want   string
	}{
		{"not TLS", []byte("RFB 003.008\n"), "not a TLS record"},
		{"empty handshake record", rec(recordHandshake, versionTLS12, nil), "empty handshake record"},
		{"application data before the handshake", rec(recordApplicationData, versionTLS12, []byte("x")), "unexpected record type"},
		{"ChangeCipherSpec first", rec(recordChangeCipherSpec, versionTLS12, []byte{1}), "unexpected record type"},
		{"oversized plaintext record", rec(recordHandshake, versionTLS12, make([]byte, maxPlaintext+1)), "record too large"},
		{"huge handshake message", rec(recordHandshake, versionTLS12, []byte{handshakeServerHello, 0xff, 0xff, 0xff}), "too large"},
		{"fatal alert", rec(recordAlert, versionTLS12, []byte{alertLevelFatal, alertHandshakeFailure}), "handshake failure"},
		{"malformed alert", rec(recordAlert, versionTLS12, []byte{2}), "malformed alert"},
		{"wrong first message", rec(recordHandshake, versionTLS12, handshakeMessage(handshakeServerHelloDone, nil)), "expected ServerHello"},
		{"TLS 1.0", rec(recordHandshake, versionTLS12, hello(func(b []byte) []byte { b[1] = 1; return b })), "TLS version"},
		{"unoffered suite", rec(recordHandshake, versionTLS12, hello(func(b []byte) []byte {
			b[35], b[36] = 0x00, 0x2f
			return b
		})), "not offered"},
		{"compression", rec(recordHandshake, versionTLS12, hello(func(b []byte) []byte { b[37] = 1; return b })), "compression"},
		{"bad extension block", rec(recordHandshake, versionTLS12, hello(func(b []byte) []byte { return append(b, 0, 9, 1) })), "malformed ServerHello extensions"},
		{"duplicate extension", rec(recordHandshake, versionTLS12, hello(func(b []byte) []byte {
			return append(b, 0, 8, 0, 23, 0, 0, 0, 23, 0, 0)
		})), "duplicate"},
		{"bad renegotiation_info", rec(recordHandshake, versionTLS12, hello(func(b []byte) []byte {
			return append(b, 0, 6, 0xff, 0x01, 0, 2, 1, 0)
		})), "renegotiation_info"},
		{"session id too long", rec(recordHandshake, versionTLS12, hello(func(b []byte) []byte {
			out := append([]byte(nil), b[:34]...)
			out = append(out, 33)
			return append(out, make([]byte, 33)...)
		})), "session id"},
		{"record version flip after ServerHello", append(rec(recordHandshake, versionTLS12, hello(nil)),
			rec(recordHandshake, 0x0301, handshakeMessage(handshakeServerHelloDone, nil))...), "version"},
		{"certificate for an anonymous suite", append(rec(recordHandshake, versionTLS12, hello(nil)),
			rec(recordHandshake, versionTLS12, handshakeMessage(handshakeCertificate, []byte{0, 0, 0}))...), "certificate"},
		{"truncated key exchange", append(rec(recordHandshake, versionTLS12, hello(nil)),
			rec(recordHandshake, versionTLS12, handshakeMessage(handshakeServerKeyExch, []byte{0, 5, 1}))...), "malformed DH"},
		{"eof mid-record", rec(recordHandshake, versionTLS12, hello(nil))[:20], "unexpected EOF"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := runPair(t, &testServer{flight: c.flight}, nil)
			err := p.client.Handshake()
			if !errContains(err, c.want) {
				t.Fatalf("expected %q, got %v", c.want, err)
			}
			p.client.Close()
			_ = p.wait(t)
		})
	}
}

// TestZeroLengthHandshakeRecordBeforeCCS is the regression test for an index-out-of-range panic: a zero-length
// handshake record where the client expected the server's ChangeCipherSpec.
func TestZeroLengthHandshakeRecordBeforeCCS(t *testing.T) {
	// Drive the server by hand: complete the first flight normally, then answer the client's Finished with an empty
	// handshake record instead of ChangeCipherSpec.
	ts := &testServer{suite: TLS_DH_anon_WITH_AES_128_GCM_SHA256}
	cc, sc := tcpPair(t)
	go func() {
		defer sc.Close()
		inject := &injectConn{Conn: sc, beforeCCS: []byte{recordHandshake, 3, 3, 0, 0}}
		_ = ts.serve(inject)
	}()
	_ = cc.SetDeadline(time.Now().Add(10 * time.Second))
	c := Client(cc, nil)
	if err := c.Handshake(); !errContains(err, "empty handshake record") {
		t.Fatalf("expected an empty-record error, got %v", err)
	}
}

// injectConn inserts raw bytes before the server's ChangeCipherSpec record.
type injectConn struct {
	net.Conn
	beforeCCS []byte
	done      bool
}

func (c *injectConn) Write(b []byte) (int, error) {
	if !c.done && len(b) > 0 && b[0] == recordChangeCipherSpec {
		c.done = true
		if _, err := c.Conn.Write(c.beforeCCS); err != nil {
			return 0, err
		}
	}
	return c.Conn.Write(b)
}

func TestBadServerFinished(t *testing.T) {
	p := runPair(t, &testServer{badFinish: true}, nil)
	if err := p.client.Handshake(); !errContains(err, "Finished verification failed") {
		t.Fatalf("got %v", err)
	}
}

func TestUnsolicitedEncryptThenMAC(t *testing.T) {
	p := runPair(t, &testServer{suite: TLS_ECDH_anon_WITH_AES_128_CBC_SHA, etmAlways: true},
		&Config{DisableEncryptThenMAC: true})
	if err := p.client.Handshake(); !errContains(err, "unsolicited encrypt_then_mac") {
		t.Fatalf("got %v", err)
	}
	// With an AEAD suite the echo is ignored.
	p = runPair(t, &testServer{suite: TLS_DH_anon_WITH_AES_128_GCM_SHA256, etmAlways: true}, nil)
	if err := p.client.Handshake(); err != nil {
		t.Fatal(err)
	}
	if p.client.ConnectionState().EncryptThenMAC {
		t.Fatal("EtM reported for an AEAD suite")
	}
	roundTrip(t, p.client)
}

// postHandshake runs a handshake whose server then executes script, and returns the client.
func postHandshake(t *testing.T, suite uint16, script func(s *serverConn) error) (*Conn, *pair) {
	t.Helper()
	p := runPair(t, &testServer{suite: suite, after: script}, nil)
	if err := p.client.Handshake(); err != nil {
		t.Fatal(err)
	}
	return p.client, p
}

func TestPostHandshakeRecords(t *testing.T) {
	for _, suite := range []uint16{TLS_DH_anon_WITH_AES_128_GCM_SHA256, TLS_ECDH_anon_WITH_AES_128_CBC_SHA, TLS_DH_anon_WITH_AES_256_CBC_SHA256} {
		name := CipherSuiteName(suite)
		t.Run(name+"/empty records and warnings are tolerated", func(t *testing.T) {
			c, _ := postHandshake(t, suite, func(s *serverConn) error {
				for range maxUselessRecords / 2 {
					_ = s.writeRecord(recordApplicationData, nil)
					_ = s.writeRecord(recordAlert, []byte{alertLevelWarning, alertUserCanceled})
				}
				_ = s.writeRecord(recordApplicationData, []byte("hello"))
				return echo(s)
			})
			buf := make([]byte, 5)
			if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello" {
				t.Fatalf("%q %v", buf, err)
			}
		})
		t.Run(name+"/too many useless records", func(t *testing.T) {
			c, _ := postHandshake(t, suite, func(s *serverConn) error {
				for i := 0; i <= maxUselessRecords; i++ {
					if err := s.writeRecord(recordApplicationData, nil); err != nil {
						return err
					}
				}
				_, _, err := s.readRecord() // the client's alert
				return err
			})
			if _, err := c.Read(make([]byte, 1)); !errContains(err, "too many ignored records") {
				t.Fatalf("got %v", err)
			}
		})
		t.Run(name+"/HelloRequest is declined", func(t *testing.T) {
			got := make(chan []byte, 1)
			c, _ := postHandshake(t, suite, func(s *serverConn) error {
				if err := s.writeRecord(recordHandshake, []byte{handshakeHelloRequest, 0, 0, 0}); err != nil {
					return err
				}
				typ, data, err := s.readRecord()
				if err != nil {
					return err
				}
				if typ == recordAlert {
					got <- data
				}
				_ = s.writeRecord(recordApplicationData, []byte("ok"))
				return echo(s)
			})
			buf := make([]byte, 2)
			if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ok" {
				t.Fatalf("%q %v", buf, err)
			}
			if a := <-got; !bytes.Equal(a, []byte{alertLevelWarning, alertNoRenegotiation}) {
				t.Fatalf("alert %v, want no_renegotiation warning", a)
			}
		})
		t.Run(name+"/other post-handshake messages are fatal", func(t *testing.T) {
			c, _ := postHandshake(t, suite, func(s *serverConn) error {
				_ = s.writeRecord(recordHandshake, handshakeMessage(4 /* NewSessionTicket */, []byte{0, 0, 0, 0, 0, 0}))
				_, _, _ = s.readRecord()
				return nil
			})
			if _, err := c.Read(make([]byte, 1)); !errContains(err, "unexpected post-handshake message") {
				t.Fatalf("got %v", err)
			}
			// After a fatal alert nothing more is written.
			if _, err := c.Write([]byte("x")); err == nil {
				t.Fatal("write after a fatal alert succeeded")
			}
		})
		t.Run(name+"/tampered record", func(t *testing.T) {
			c, _ := postHandshake(t, suite, func(s *serverConn) error {
				frag, _ := s.out.seal(recordApplicationData, versionTLS12, []byte("secret"), s.rnd)
				frag[len(frag)-1] ^= 1
				rec := append([]byte{recordApplicationData, 3, 3}, binary.BigEndian.AppendUint16(nil, uint16(len(frag)))...)
				_, err := s.conn.Write(append(rec, frag...))
				_, _, _ = s.readRecord()
				return err
			})
			if _, err := c.Read(make([]byte, 1)); !errors.Is(err, errBadRecordMAC) {
				t.Fatalf("got %v", err)
			}
		})
		t.Run(name+"/record version change", func(t *testing.T) {
			c, _ := postHandshake(t, suite, func(s *serverConn) error {
				_ = s.writeRecordVersion(recordApplicationData, 0x0301, []byte("x"))
				_, _, _ = s.readRecord()
				return nil
			})
			if _, err := c.Read(make([]byte, 1)); !errContains(err, "version") {
				t.Fatalf("got %v", err)
			}
		})
		t.Run(name+"/remote fatal alert and close_notify", func(t *testing.T) {
			c, _ := postHandshake(t, suite, func(s *serverConn) error {
				return s.writeRecord(recordAlert, []byte{alertLevelFatal, alertInternalError})
			})
			var ae *AlertError
			if _, err := c.Read(make([]byte, 1)); !errors.As(err, &ae) || !ae.Remote || ae.Description != alertInternalError {
				t.Fatalf("got %v", err)
			}
			c2, _ := postHandshake(t, suite, func(s *serverConn) error {
				return s.writeRecord(recordAlert, []byte{alertLevelWarning, alertCloseNotify})
			})
			if _, err := c2.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Fatalf("close_notify: got %v", err)
			}
		})
		t.Run(name+"/ChangeCipherSpec after the handshake", func(t *testing.T) {
			c, _ := postHandshake(t, suite, func(s *serverConn) error {
				_ = s.writeRecord(recordChangeCipherSpec, []byte{1})
				_, _, _ = s.readRecord()
				return nil
			})
			if _, err := c.Read(make([]byte, 1)); !errContains(err, "unexpected record type") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestCloseWriteStopsWrites(t *testing.T) {
	c, _ := postHandshake(t, TLS_DH_anon_WITH_AES_128_GCM_SHA256, func(s *serverConn) error { return echo(s) })
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("x")); !errors.Is(err, errShutdown) {
		t.Fatalf("write after CloseWrite: %v", err)
	}
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("read after the peer's close_notify: %v", err)
	}
}

// TestPaddingLenMatchesReference compares the constant-time padding check with a straightforward implementation.
func TestPaddingLenMatchesReference(t *testing.T) {
	for n := 1; n <= 300; n++ {
		for _, pad := range []int{0, 1, 2, n / 2, n - 1, n, 255} {
			if pad > 255 {
				continue
			}
			buf := bytes.Repeat([]byte{byte(pad)}, n)
			checkPadding(t, buf)
			if n > 1 {
				buf[n-2] ^= 1 // break the padding
				checkPadding(t, buf)
			}
		}
	}
}

func refPadding(p []byte) (int, bool) {
	if len(p) == 0 {
		return 0, false
	}
	pad := int(p[len(p)-1])
	if pad+1 > len(p) {
		return 0, false
	}
	for _, b := range p[len(p)-1-pad : len(p)-1] {
		if int(b) != pad {
			return 0, false
		}
	}
	return pad + 1, true
}

func checkPadding(t testing.TB, p []byte) {
	t.Helper()
	remove, good := paddingLen(p)
	wantRemove, wantGood := refPadding(p)
	if (good == 0xff) != wantGood || (good != 0xff && good != 0) || (wantGood && remove != wantRemove) {
		t.Fatalf("paddingLen(%x) = %d %#x, reference %d %v", p, remove, good, wantRemove, wantGood)
	}
	if !wantGood && remove != 1 {
		t.Fatalf("bad padding must strip only the length byte, got %d", remove)
	}
}

func TestKnownGroups(t *testing.T) {
	names := map[string]bool{}
	for _, g := range knownGroupTable {
		p := mustBig(g.hex)
		if names[g.name] {
			t.Fatalf("duplicate %s", g.name)
		}
		names[g.name] = true
		bits := p.BitLen()
		// Both families fix the top and bottom 64 bits to one (RFC 7919 section A, RFC 3526).
		mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(1))
		top := new(big.Int).Rsh(p, uint(bits-64))
		if top.Cmp(mask) != 0 || new(big.Int).And(p, mask).Cmp(mask) != 0 || bits%1024 != 0 {
			t.Fatalf("%s: not a well-formed %d-bit group", g.name, bits)
		}
		if name, ok := knownGroupName(p); !ok || name != g.name {
			t.Fatalf("%s not recognized", g.name)
		}
		if bits <= 3072 || !testing.Short() {
			q := new(big.Int).Rsh(p, 1)
			if !p.ProbablyPrime(0) || !q.ProbablyPrime(0) {
				t.Fatalf("%s is not a safe prime", g.name)
			}
		}
	}
	if len(names) != 10 {
		t.Fatalf("%d groups", len(names))
	}
	if _, ok := knownGroupName(mustBig(rfc5114p2048)); ok {
		t.Fatal("RFC 5114 group recognized as standard")
	}
}

// TestShortExponentOnlyForStandardGroups: the known safe-prime groups use 512-bit exponents, others full length.
func TestShortExponentOnlyForStandardGroups(t *testing.T) {
	var rec bytes.Buffer
	for _, c := range []struct {
		p, g  *big.Int
		known bool
	}{{namedGroup("ffdhe2048"), big.NewInt(2), true}, {mustBig(rfc5114p2048), mustBig(rfc5114g2048), false}} {
		rec.Reset()
		d := &dhGroup{p: c.p, g: c.g, known: c.known}
		ys := new(big.Int).Exp(c.g, big.NewInt(12345), c.p)
		if _, _, err := d.exchange(io.TeeReader(detRand(1), &rec), ys); err != nil {
			t.Fatal(err)
		}
		want := knownGroupExpBits / 8
		if !c.known {
			want = (c.p.BitLen() + 7) / 8
		}
		if rec.Len() < want || (c.known && rec.Len() > want+8) {
			t.Fatalf("known=%v: read %d random bytes for the exponent, want about %d", c.known, rec.Len(), want)
		}
	}
}

func TestECDHPointValidation(t *testing.T) {
	// Sanity: crypto/ecdh rejects the classic invalid inputs this package relies on it for.
	if _, err := ecdh.P256().NewPublicKey([]byte{0}); err == nil {
		t.Fatal("P-256 point at infinity accepted")
	}
	priv, _ := ecdh.X25519().GenerateKey(nil)
	low, _ := ecdh.X25519().NewPublicKey(make([]byte, 32))
	if _, err := priv.ECDH(low); err == nil {
		t.Fatal("X25519 low-order point accepted")
	}
}
