package anontls

import (
	"bufio"
	"bytes"
	"context"
	"crypto/des"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, ":", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Known answers computed with `openssl kdf ... TLS1-PRF` (seed = label || seed).
func TestPRF12(t *testing.T) {
	secret := mustHex(t, "000102030405060708090a0b0c0d0e0f")
	seed := mustHex(t, "a0ba9f936cda311827a6f796ffd5198c")
	got := prf12(sha256.New, secret, "test label", seed, 48)
	want := mustHex(t, "65:FA:6B:70:9F:94:7C:73:9E:F0:C2:17:81:E7:68:30:F6:DD:04:7B:A3:44:08:89:85:33:85:16:D0:DA:98:D7:"+
		"0D:56:04:6C:D6:80:42:82:F5:8B:66:F0:66:83:27:8D")
	if !bytes.Equal(got, want) {
		t.Fatalf("PRF SHA-256:\n got %x\nwant %x", got, want)
	}
	got = prf12(sha512.New384, secret, "test label", seed, 48)
	want = mustHex(t, "06:9A:33:4A:02:52:E9:80:72:D0:C4:E6:70:73:CD:63:69:4D:6F:E1:77:E2:1D:CA:07:90:60:47:F7:04:C7:"+
		"EE:AB:D6:66:31:E2:51:24:EC:E8:A5:6D:3C:A9:52:52:97")
	if !bytes.Equal(got, want) {
		t.Fatalf("PRF SHA-384:\n got %x\nwant %x", got, want)
	}
}

func TestPaddingLen(t *testing.T) {
	cases := []struct {
		in     []byte
		remove int
		good   byte
	}{
		{[]byte{1, 2, 3, 0}, 1, 0xff},
		{[]byte{9, 2, 2, 2}, 3, 0xff},
		{[]byte{9, 1, 2, 2}, 1, 0},    // wrong padding byte
		{[]byte{5, 5, 5, 5, 5}, 1, 0}, // padding longer than the payload
		{[]byte{3, 3, 3, 3}, 4, 0xff},
	}
	for i, c := range cases {
		remove, good := paddingLen(c.in)
		if good != c.good || (good == 0xff && remove != c.remove) {
			t.Errorf("case %d: got remove=%d good=%#x, want %d %#x", i, remove, good, c.remove, c.good)
		}
	}
}

func TestRecordRoundTrip(t *testing.T) {
	for _, s := range suites {
		for _, etm := range []bool{false, true} {
			if etm && s.aead {
				continue
			}
			t.Run(fmt.Sprintf("%s/etm=%v", s.name, etm), func(t *testing.T) {
				macKey := make([]byte, s.macKeyLen())
				key := make([]byte, s.keyLen)
				iv := make([]byte, s.fixedIVLen())
				for _, b := range [][]byte{macKey, key, iv} {
					_, _ = rand.Read(b)
				}
				w, err := s.newHalfConn(macKey, key, iv, etm)
				if err != nil {
					t.Fatal(err)
				}
				r, _ := s.newHalfConn(macKey, key, iv, etm)
				for _, n := range []int{0, 1, 15, 16, 17, 100, maxPlaintext} {
					msg := make([]byte, n)
					_, _ = rand.Read(msg)
					frag, err := w.seal(recordApplicationData, versionTLS12, msg, rand.Reader)
					if err != nil {
						t.Fatal(err)
					}
					if len(frag) > maxCiphertext {
						t.Fatalf("len %d: fragment of %d bytes exceeds the record limit", n, len(frag))
					}
					// Every single-bit modification must be detected (tag, MAC, IV, ciphertext, padding).
					for _, pos := range []int{0, len(frag) / 3, len(frag) / 2, len(frag) - 1} {
						bad := append([]byte(nil), frag...)
						bad[pos] ^= 0x40
						r2, _ := s.newHalfConn(macKey, key, iv, etm)
						r2.seq = r.seq
						if _, err := r2.open(recordApplicationData, versionTLS12, bad); !errors.Is(err, errBadRecordMAC) {
							t.Fatalf("len %d: record modified at %d accepted (err %v)", n, pos, err)
						}
					}
					// Header fields are authenticated too.
					r3, _ := s.newHalfConn(macKey, key, iv, etm)
					r3.seq = r.seq
					if _, err := r3.open(recordHandshake, versionTLS12, append([]byte(nil), frag...)); !errors.Is(err, errBadRecordMAC) {
						t.Fatalf("len %d: record type not authenticated (err %v)", n, err)
					}
					pt, err := r.open(recordApplicationData, versionTLS12, append([]byte(nil), frag...))
					if err != nil {
						t.Fatalf("len %d: %v", n, err)
					}
					if !bytes.Equal(pt, msg) {
						t.Fatalf("len %d: round trip mismatch", n)
					}
				}
				// Replayed / reordered records fail (sequence number in the MAC / AAD).
				frag, _ := w.seal(recordApplicationData, versionTLS12, []byte("x"), rand.Reader)
				r.seq++
				if _, err := r.open(recordApplicationData, versionTLS12, frag); err == nil {
					t.Fatal("record with a wrong sequence number accepted")
				}
				// Truncated and malformed fragments never panic and never decrypt.
				for n := 0; n < 3*aesBlock+64; n++ {
					r4, _ := s.newHalfConn(macKey, key, iv, etm)
					if _, err := r4.open(recordApplicationData, versionTLS12, make([]byte, n)); err == nil {
						t.Fatalf("%d zero bytes accepted as a record", n)
					}
				}
			})
		}
	}
}

const aesBlock = 16

// opensslName maps suites to OpenSSL cipher names.
var opensslName = map[uint16]string{
	TLS_DH_anon_WITH_AES_128_CBC_SHA:    "ADH-AES128-SHA",
	TLS_DH_anon_WITH_AES_256_CBC_SHA:    "ADH-AES256-SHA",
	TLS_DH_anon_WITH_AES_128_CBC_SHA256: "ADH-AES128-SHA256",
	TLS_DH_anon_WITH_AES_256_CBC_SHA256: "ADH-AES256-SHA256",
	TLS_DH_anon_WITH_AES_128_GCM_SHA256: "ADH-AES128-GCM-SHA256",
	TLS_DH_anon_WITH_AES_256_GCM_SHA384: "ADH-AES256-GCM-SHA384",
	TLS_ECDH_anon_WITH_AES_128_CBC_SHA:  "AECDH-AES128-SHA",
	TLS_ECDH_anon_WITH_AES_256_CBC_SHA:  "AECDH-AES256-SHA",
}

// TestOpenSSLInterop runs every suite against `openssl s_server -nocert -rev` (an echo server replying with each line
// reversed). Enabled with ASTRATERM_TESTENV=1 when an OpenSSL with anonymous cipher suites is installed.
func TestOpenSSLInterop(t *testing.T) {
	bin := opensslForTest(t)
	dhparam := filepath.Join(t.TempDir(), "ffdhe2048.pem")
	if out, err := exec.Command(bin, "genpkey", "-genparam", "-algorithm", "DH", "-pkeyopt", "group:ffdhe2048", "-out", dhparam).CombinedOutput(); err != nil {
		t.Skipf("openssl cannot export ffdhe2048: %v %s", err, out)
	}
	for _, id := range DefaultCipherSuites() {
		for _, mode := range []struct{ ems, etm bool }{{true, true}, {false, true}, {true, false}} {
			ems, etm := mode.ems, mode.etm
			if suiteByID(id).aead && !etm {
				continue // encrypt-then-MAC does not apply to AEAD suites
			}
			name := fmt.Sprintf("%s/ems=%v/etm=%v", CipherSuiteName(id), ems, etm)
			t.Run(name, func(t *testing.T) {
				// Explicit parameters: s_server's automatic DH sizing would use 1024 bits for AES-128 suites (see
				// TestOpenSSLAutoDH1024).
				addr := startOpenSSL(t, bin, opensslName[id], ems, etm, "-dhparam", dhparam)
				raw, err := net.DialTimeout("tcp", addr, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				_ = raw.SetDeadline(time.Now().Add(20 * time.Second))
				c := Client(raw, &Config{CipherSuites: []uint16{id}})
				if err := c.HandshakeContext(context.Background()); err != nil {
					t.Fatalf("handshake: %v", err)
				}
				st := c.ConnectionState()
				wantETM := etm && !suiteByID(id).aead
				if st.CipherSuite != id || st.ExtendedMasterSecret != ems || st.EncryptThenMAC != wantETM {
					t.Fatalf("state %+v", st)
				}
				if suiteByID(id).kx == kxDH && (st.DHBits != 2048 || st.Group != "ffdhe2048") {
					t.Fatalf("DH group %q (%d bits)", st.Group, st.DHBits)
				}
				t.Logf("%+v", st)
				// One large write (several records) of lines shorter than s_server's 16 KiB line buffer.
				var lines []string
				for i := 0; i < 3; i++ {
					lines = append(lines, strings.Repeat(fmt.Sprintf("line%d-abcdefghij", i), 600))
				}
				if _, err := io.WriteString(c, strings.Join(lines, "\n")+"\n"); err != nil {
					t.Fatal(err)
				}
				br := bufio.NewReader(c)
				for _, l := range lines {
					got, err := br.ReadString('\n')
					if err != nil {
						t.Fatal(err)
					}
					if got != reverse(l)+"\n" {
						t.Fatalf("echo mismatch: got %d bytes", len(got))
					}
				}
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// TestOpenSSLAutoDH1024: with automatic DH sizing s_server uses a 1024-bit group for AES-128 anonymous suites. The
// default configuration refuses it as a handshake failure (the VNC layer then asks the user), MinDHBits: 1024 (the
// explicit "weak encryption" opt-in) accepts it, and the default offer (AES-256 first) gets a 3072-bit group.
func TestOpenSSLAutoDH1024(t *testing.T) {
	bin := opensslForTest(t)
	addr := startOpenSSL(t, bin, "ADH-AES128-GCM-SHA256:ADH-AES256-GCM-SHA384", true, true)
	dial := func(cfg *Config) (*Conn, error) {
		raw, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { raw.Close() })
		_ = raw.SetDeadline(time.Now().Add(20 * time.Second))
		c := Client(raw, cfg)
		return c, c.Handshake()
	}
	aes128 := []uint16{TLS_DH_anon_WITH_AES_128_GCM_SHA256}
	if _, err := dial(&Config{CipherSuites: aes128}); !IsHandshakeFailure(err) || !strings.Contains(err.Error(), "1024 bits") {
		t.Fatalf("1024-bit group: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // s_server serves one connection at a time
	c, err := dial(&Config{CipherSuites: aes128, MinDHBits: 1024})
	if err != nil || c.ConnectionState().DHBits != 1024 {
		t.Fatalf("opt-in: %v %+v", err, c.ConnectionState())
	}
	c.Close()
	time.Sleep(100 * time.Millisecond)
	c, err = dial(nil)
	if err != nil || c.ConnectionState().DHBits < 3072 {
		t.Fatalf("default offer: %v %+v", err, c.ConnectionState())
	}
}

func opensslForTest(t *testing.T) string {
	t.Helper()
	if os.Getenv("ASTRATERM_TESTENV") != "1" {
		t.Skip("set ASTRATERM_TESTENV=1 to run the OpenSSL interoperability tests")
	}
	bin, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl not installed")
	}
	if out, err := exec.Command(bin, "ciphers", "aNULL:@SECLEVEL=0").CombinedOutput(); err != nil || !strings.Contains(string(out), "ADH") {
		t.Skip("openssl without anonymous cipher suites")
	}
	return bin
}

func reverse(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

func startOpenSSL(t *testing.T, bin, cipher string, ems, etm bool, extra ...string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	args := []string{"s_server", "-accept", addr, "-nocert", "-tls1_2", "-cipher", cipher + ":@SECLEVEL=0", "-rev", "-quiet"}
	if !ems {
		args = append(args, "-no_ems")
	}
	if !etm {
		args = append(args, "-no_etm")
	}
	args = append(args, extra...)
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// s_server serves connections one after another: a plain TCP probe tells when it listens.
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			time.Sleep(100 * time.Millisecond) // let s_server finish with the probe
			return addr
		}
		if time.Now().After(deadline) {
			t.Fatalf("openssl s_server did not start: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestGnuTLSInterop negotiates VeNCrypt TLSVnc with the testenv TigerVNC server (GnuTLS, anonymous (EC)DH) and
// completes VNC authentication inside TLS, once with the default offer and once each restricted to the ECDH-CBC and
// DH-CBC suites (skipped when GnuTLS does not enable them). Every attempt authenticates correctly: TigerVNC
// blacklists clients after repeated security failures. Enabled with ASTRATERM_TESTENV=1 (server ASTRATERM_TEST_VNC,
// default 127.0.0.1:22059, password ASTRATERM_TEST_VNC_PASSWORD, default "vncpassword").
func TestGnuTLSInterop(t *testing.T) {
	if os.Getenv("ASTRATERM_TESTENV") != "1" {
		t.Skip("set ASTRATERM_TESTENV=1 to run against the shared test environment")
	}
	addr := envOr("ASTRATERM_TEST_VNC", "127.0.0.1:22059")
	password := envOr("ASTRATERM_TEST_VNC_PASSWORD", "vncpassword")
	offers := []struct {
		name   string
		suites []uint16
	}{
		{"default", nil},
		{"ecdh-cbc", []uint16{TLS_ECDH_anon_WITH_AES_128_CBC_SHA, TLS_ECDH_anon_WITH_AES_256_CBC_SHA}},
		{"dh-cbc", []uint16{TLS_DH_anon_WITH_AES_128_CBC_SHA256, TLS_DH_anon_WITH_AES_256_CBC_SHA256,
			TLS_DH_anon_WITH_AES_128_CBC_SHA, TLS_DH_anon_WITH_AES_256_CBC_SHA}},
	}
	for _, o := range offers {
		t.Run(o.name, func(t *testing.T) {
			raw, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Skipf("VNC test server unavailable: %v", err)
			}
			defer raw.Close()
			_ = raw.SetDeadline(time.Now().Add(20 * time.Second))
			step := func(w io.ReadWriter, write []byte, read int) []byte {
				t.Helper()
				if write != nil {
					if _, err := w.Write(write); err != nil {
						t.Fatal(err)
					}
				}
				b := make([]byte, read)
				if _, err := io.ReadFull(w, b); err != nil {
					t.Fatal(err)
				}
				return b
			}
			step(raw, nil, 12)
			n := step(raw, []byte("RFB 003.008\n"), 1)
			if n[0] == 0 {
				t.Skip("server refuses connections (blacklisted?)")
			}
			types := step(raw, nil, int(n[0]))
			if !bytes.Contains(types, []byte{19}) {
				t.Skipf("server does not offer VeNCrypt (%v)", types)
			}
			step(raw, []byte{19}, 2)
			if ack := step(raw, []byte{0, 2}, 1); ack[0] != 0 {
				t.Fatalf("VeNCrypt version rejected")
			}
			cnt := step(raw, nil, 1)
			sub := step(raw, nil, 4*int(cnt[0]))
			if !bytes.Contains(sub, []byte{0, 0, 1, 2}) { // 258 TLSVnc
				t.Skipf("server does not offer TLSVnc")
			}
			if ok := step(raw, []byte{0, 0, 1, 2}, 1); ok[0] != 1 {
				t.Fatalf("TLSVnc subtype refused")
			}
			c := Client(raw, &Config{CipherSuites: o.suites})
			if err := c.Handshake(); err != nil {
				if IsHandshakeFailure(err) && o.suites != nil {
					t.Skipf("suites not enabled on the server: %v", err)
				}
				t.Fatalf("handshake: %v", err)
			}
			st := c.ConnectionState()
			t.Logf("negotiated %+v", st)
			if st.DHBits != 0 && st.DHBits < 2048 {
				t.Fatalf("weak group accepted: %+v", st)
			}
			challenge := step(c, nil, 16)
			res := step(c, desResponse(password, challenge), 4)
			if !bytes.Equal(res, []byte{0, 0, 0, 0}) {
				t.Fatalf("VNC authentication inside TLS failed: %x", res)
			}
			// ClientInit → ServerInit proves the encrypted stream keeps working.
			hdr := step(c, []byte{1}, 24)
			t.Logf("desktop %dx%d", int(hdr[0])<<8|int(hdr[1]), int(hdr[2])<<8|int(hdr[3]))
		})
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// desResponse is the VNC Authentication response (the vnc package has the production copy).
func desResponse(password string, challenge []byte) []byte {
	var key [8]byte
	copy(key[:], password)
	for i, b := range key {
		var r byte
		for j := 0; j < 8; j++ {
			r |= (b >> j & 1) << (7 - j)
		}
		key[i] = r
	}
	block, _ := des.NewCipher(key[:])
	out := make([]byte, 16)
	block.Encrypt(out[:8], challenge[:8])
	block.Encrypt(out[8:], challenge[8:])
	return out
}
