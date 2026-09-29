package keys

import (
	"bytes"
	"crypto"
	"crypto/dsa" //nolint:staticcheck // legacy DSA fixtures
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayrus/putty"
	"golang.org/x/crypto/ssh"

	"github.com/termstead/termstead/internal/sshx"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixturePub(t *testing.T, name string) ssh.PublicKey {
	t.Helper()
	pk, _, err := parsePublicKeyText(string(fixture(t, name)))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return pk
}

// TestImportFixtures parses real ssh-keygen / OpenSSL / puttygen outputs.
func TestImportFixtures(t *testing.T) {
	cases := []struct {
		file, pass, format, comment, pub string
		encrypted                        bool
		typ                              string
		bits                             int
	}{
		{"openssh_ed25519", "", formatOpenSSH, "alice@laptop", "openssh_ed25519.pub", false, "ed25519", 256},
		{"openssh_ed25519_enc", "testpass", formatOpenSSH, "enc ed25519", "", true, "ed25519", 256},
		{"openssh_rsa_cbc", "testpass", formatOpenSSH, "rsa cbc", "", true, "rsa", 2048},
		{"openssh_ecdsa384_aes128", "testpass", formatOpenSSH, "ecdsa aes128", "", true, "ecdsa", 384},
		{"openssh_dsa_enc", "testpass", formatOpenSSH, "old dsa", "openssh_dsa_enc.pub", true, "dsa", 1024},
		// ssh-keygen -Z: the AEAD ciphers and 3DES (x/crypto reads none of them).
		{"openssh_ed25519_chacha", "testpass", formatOpenSSH, "chacha ed25519", "openssh_ed25519_chacha.pub", true, "ed25519", 256},
		{"openssh_ecdsa521_chacha", "testpass", formatOpenSSH, "ecdsa521 chacha", "openssh_ecdsa521_chacha.pub", true, "ecdsa", 521},
		{"openssh_rsa_aes256gcm", "testpass", formatOpenSSH, "rsa gcm", "openssh_rsa_aes256gcm.pub", true, "rsa", 2048},
		{"openssh_ecdsa256_aes128gcm", "testpass", formatOpenSSH, "ecdsa gcm", "openssh_ecdsa256_aes128gcm.pub", true, "ecdsa", 256},
		{"openssh_ed25519_3des", "testpass", formatOpenSSH, "3des ed25519", "openssh_ed25519_3des.pub", true, "ed25519", 256},
		{"pem_rsa", "", formatPEM, "", "", false, "rsa", 2048},
		{"pem_rsa_enc", "testpass", formatPEM, "", "", true, "rsa", 2048},
		{"pkcs8_ed25519_enc.pem", "testpass", formatPKCS8, "", "", true, "ed25519", 256},
		{"pkcs8_rsa_des3.pem", "testpass", formatPKCS8, "", "", true, "rsa", 2048},
		{"ec_params_and_key.pem", "", formatPEM, "", "", false, "ecdsa", 384},
		{"ec_legacy_enc.pem", "testpass", formatPEM, "", "", true, "ecdsa", 384},
		{"putty_v2_ed25519_enc.ppk", "testkey", formatPPK, "a@b", "", true, "ed25519", 256},
		{"putty_v2_nistp256_enc.ppk", "testkey", formatPPK, "a@b", "", true, "ecdsa", 256},
		{"putty_v2_nistp521_enc.ppk", "testkey", formatPPK, "a@b", "", true, "ecdsa", 521},
		{"putty_v3_rsa_enc.ppk", "testkey", formatPPK, "a@b", "", true, "rsa", 2048},
		{"putty_v3_rsa_plain.ppk", "", formatPPK, "a@b", "", false, "rsa", 2048},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			text := fixture(t, tc.file)
			pk, err := parseKey(text, tc.pass)
			if err != nil {
				t.Fatal(err)
			}
			if pk.Format != tc.format || pk.Encrypted != tc.encrypted || pk.Comment != tc.comment {
				t.Fatalf("format %q encrypted %v comment %q", pk.Format, pk.Encrypted, pk.Comment)
			}
			if typ, bits := keyTypeBits(pk.Pub); typ != tc.typ || bits != tc.bits {
				t.Fatalf("type %s %d", typ, bits)
			}
			if tc.pub != "" && !sameKey(pk.Pub, fixturePub(t, tc.pub)) {
				t.Fatal("public key differs from the .pub file")
			}
			if _, err := pk.Signer(); err != nil {
				t.Fatalf("signer: %v", err)
			}
			if tc.encrypted {
				var pe *passphraseError
				if _, err := parseKey(text, ""); !errors.As(err, &pe) || pe.wrong {
					t.Fatalf("no passphrase: %v", err)
				}
				if _, err := parseKey(text, "wrong-"+tc.pass); !errors.As(err, &pe) || !pe.wrong {
					t.Fatalf("wrong passphrase: %v", err)
				}
			}
			// What gets stored must be loadable by sshx (the connection side).
			stored, err := storageText(pk, text, pk.Comment, tc.pass)
			if err != nil {
				t.Fatal(err)
			}
			s, err := sshx.ParsePrivateKey(stored, tc.pass)
			if err != nil || !sameKey(s.PublicKey(), pk.Pub) {
				t.Fatalf("sshx cannot load the stored form: %v", err)
			}
		})
	}
}

func TestImportRejects(t *testing.T) {
	cases := []struct {
		name, text, pass, code string
	}{
		{"weak rsa", string(fixture(t, "putty_v2_rsa_enc.ppk")), "testkey", "unsupported_key"},
		{"2048-bit dsa", string(fixture(t, "putty_v2_dss_enc.ppk")), "testkey", "unsupported_key"},
		{"public key", string(fixture(t, "openssh_ed25519.pub")), "", "public_key_only"},
		{"certificate", string(fixture(t, "openssh_ed25519-cert.pub")), "", "public_key_only"},
		{"garbage", "hello world", "", "invalid_key"},
		{"empty", "  \n", "", "invalid_key"},
		{"truncated pem", "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----\n", "", "invalid_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseKey([]byte(tc.text), tc.pass)
			var ke *keyError
			if !errors.As(err, &ke) || ke.code != tc.code {
				t.Fatalf("got %v, want code %s", err, tc.code)
			}
		})
	}
	// Excessive PPK v3 Argon2 parameters are refused before any key derivation happens.
	evil := strings.Replace(string(fixture(t, "putty_v3_rsa_enc.ppk")), "Argon2-Memory: 8192", "Argon2-Memory: 4194304", 1)
	if _, err := parseKey([]byte(evil), "testkey"); err == nil || !strings.Contains(err.Error(), "excessive") {
		t.Fatalf("argon2 bound: %v", err)
	}
}

// opensshBody returns the decoded body of an OpenSSH private key fixture and a re-encoder.
func opensshBody(t *testing.T, name string) ([]byte, func([]byte) []byte) {
	t.Helper()
	block, _ := pem.Decode(fixture(t, name))
	if block == nil || block.Type != "OPENSSH PRIVATE KEY" {
		t.Fatalf("%s is not an OpenSSH private key", name)
	}
	return block.Bytes, func(b []byte) []byte {
		return pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: b})
	}
}

// TestOpenSSHAEADIntegrity: a modified ciphertext or tag of an AEAD-encrypted key is reported like a wrong
// passphrase (the tag cannot verify), a missing tag or trailing data makes the file invalid, and an unsupported
// cipher is reported before any passphrase is asked for.
func TestOpenSSHAEADIntegrity(t *testing.T) {
	for _, name := range []string{"openssh_ed25519_chacha", "openssh_rsa_aes256gcm"} {
		body, encode := opensshBody(t, name)
		for _, pos := range []int{len(body) - 1, len(body) - 20} { // in the tag, in the ciphertext
			bad := bytes.Clone(body)
			bad[pos] ^= 0x01
			var pe *passphraseError
			if _, err := parseKey(encode(bad), "testpass"); !errors.As(err, &pe) || !pe.wrong {
				t.Fatalf("%s tampered at %d: %v", name, pos, err)
			}
		}
		var ke *keyError
		if _, err := parseKey(encode(body[:len(body)-16]), "testpass"); !errors.As(err, &ke) || ke.code != "invalid_key" {
			t.Fatalf("%s without its tag: %v", name, err)
		}
		if _, err := parseKey(encode(append(bytes.Clone(body), 0, 0, 0, 0)), "testpass"); !errors.As(err, &ke) || ke.code != "invalid_key" {
			t.Fatalf("%s with trailing data: %v", name, err)
		}
	}
	// Trailing data after a non-AEAD key is invalid too (OpenSSH and x/crypto refuse it).
	body, encode := opensshBody(t, "openssh_ed25519")
	var ke *keyError
	if _, err := parseKey(encode(append(bytes.Clone(body), 1, 2, 3, 4)), ""); !errors.As(err, &ke) || ke.code != "invalid_key" {
		t.Fatalf("trailing data: %v", err)
	}
	// An unknown cipher: unsupported_key without a passphrase (the UI must not ask for one first).
	body, encode = opensshBody(t, "openssh_ed25519_chacha")
	unknown := bytes.Replace(body, []byte("chacha20-poly1305@openssh.com"), []byte("chacha20-poly1306@openssh.com"), 1)
	if _, err := parseKey(encode(unknown), ""); !errors.As(err, &ke) || ke.code != "unsupported_key" {
		t.Fatalf("unknown cipher: %v", err)
	}
}

func generated(t *testing.T) map[string]crypto.PrivateKey {
	t.Helper()
	out := map[string]crypto.PrivateKey{}
	for _, tc := range []struct {
		typ  string
		bits int
	}{{"ed25519", 0}, {"rsa", 2048}, {"ecdsa", 256}, {"ecdsa", 384}, {"ecdsa", 521}} {
		k, err := generateKey(tc.typ, tc.bits)
		if err != nil {
			t.Fatal(err)
		}
		out[fmt.Sprintf("%s%d", tc.typ, tc.bits)] = k
	}
	dk, _ := parseKey(fixture(t, "openssh_dsa_enc"), "testpass")
	out["dsa"] = dk.Raw
	return out
}

// TestExportRoundTrip exports every key type in every private format (with and without passphrase) and reads it back
// with our parser, x/crypto (OpenSSH / PEM) and kayrus/putty (PPK).
func TestExportRoundTrip(t *testing.T) {
	for name, raw := range generated(t) {
		pk, err := newParsedKey(raw, "", formatOpenSSH, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, format := range []string{exportOpenSSH, exportPPK, exportPEM, exportPKCS8} {
			for _, ppk := range []int{2, 3} {
				if format != exportPPK && ppk == 2 {
					continue
				}
				for _, pass := range []string{"", "s3cret pass"} {
					res, err := exportPrivateKey(pk, format, "my comment", pass, ppk, "base")
					if format == exportPKCS8 && name == "dsa" {
						if err == nil {
							t.Fatal("DSA PKCS#8 export should fail")
						}
						continue
					}
					if err != nil {
						t.Fatalf("%s %s %q: %v", name, format, pass, err)
					}
					back, err := parseKey([]byte(res.Content), pass)
					if err != nil {
						t.Fatalf("%s %s v%d %q: re-parse: %v\n%s", name, format, ppk, pass, err, res.Content)
					}
					if !sameKey(back.Pub, pk.Pub) || back.Encrypted != (pass != "") {
						t.Fatalf("%s %s: key or encryption differs", name, format)
					}
					if (format == exportOpenSSH || format == exportPPK) && back.Comment != "my comment" {
						t.Fatalf("%s %s: comment %q", name, format, back.Comment)
					}
					// The same file must be accepted by the connection side (sshx: x/crypto + kayrus/putty), except
					// DSA in OpenSSH format and encrypted PKCS#8, which x/crypto cannot read (never stored that way).
					if !(name == "dsa" && format == exportOpenSSH) && !(format == exportPKCS8 && pass != "") &&
						!(strings.HasPrefix(name, "ed25519") && format == exportPEM && pass != "") {
						s, err := sshx.ParsePrivateKey([]byte(res.Content), pass)
						if err != nil || !sameKey(s.PublicKey(), pk.Pub) {
							t.Fatalf("%s %s v%d %q: sshx: %v", name, format, ppk, pass, err)
						}
					}
				}
			}
		}
	}
}

// TestPPKAlignedBlob covers private blobs whose length is a multiple of the AES block size (a full padding block is
// then required by kayrus/putty).
func TestPPKAlignedBlob(t *testing.T) {
	found := false
	for i := 0; i < 40 && !found; i++ {
		k, _ := generateKey("rsa", 2048)
		blob, err := ppkPrivateBlob(k)
		if err != nil {
			t.Fatal(err)
		}
		if len(blob)%16 != 0 {
			continue
		}
		found = true
		for _, v := range []int{2, 3} {
			out, err := marshalPPK(k, "aligned", []byte("pw"), v)
			if err != nil {
				t.Fatal(err)
			}
			pk, err := putty.New(out)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pk.ParseRawPrivateKey([]byte("pw")); err != nil {
				t.Fatalf("v%d aligned blob: %v", v, err)
			}
		}
	}
	if !found {
		t.Skip("no block-aligned RSA blob generated")
	}
}

func TestBcryptPBKDFMatchesXCrypto(t *testing.T) {
	// x/crypto encrypts with its internal bcrypt_pbkdf; our parser decrypts with ours.
	k, _ := generateKey("ed25519", 0)
	block, err := ssh.MarshalPrivateKeyWithPassphrase(k, "c", []byte("pass phrase"))
	if err != nil {
		t.Fatal(err)
	}
	pk, err := parseOpenSSHPrivateKey(block.Bytes, []byte("pass phrase"))
	if err != nil || pk.Comment != "c" {
		t.Fatalf("%v %+v", err, pk)
	}
}

func TestPublicEncodings(t *testing.T) {
	pub := fixturePub(t, "openssh_ed25519.pub")
	line := authorizedKeyLine(pub, "  multi\tline\ncomment ")
	if !strings.HasSuffix(line, " multi line comment") {
		t.Fatalf("line %q", line)
	}
	for _, text := range []string{line, strings.Join(strings.Fields(line)[:2], " "), strings.Fields(line)[1],
		rfc4716(pub, strings.Repeat("long comment ", 10)), `command="echo hi",no-pty ` + line} {
		back, _, err := parsePublicKeyText(text)
		if err != nil || !sameKey(back, pub) {
			t.Fatalf("%q: %v", text, err)
		}
	}
	_, comment, _ := parsePublicKeyText(rfc4716(pub, "hello \"world\""))
	if comment != "hello 'world'" {
		t.Fatalf("rfc4716 comment %q", comment)
	}
	if got, want := fingerprintMD5(pub), "MD5:"+ssh.FingerprintLegacyMD5(pub); got != want {
		t.Fatalf("md5 %s %s", got, want)
	}
	if fileBase("Work Laptop (2026)!", "ed25519") != "work-laptop-2026" || fileBase("  ", "rsa") != "id_rsa" {
		t.Fatal(fileBase("Work Laptop (2026)!", "ed25519"))
	}
}

func TestGenerateValidation(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		bits int
		ok   bool
	}{{"ed25519", 0, true}, {"ed25519", 512, false}, {"rsa", 1024, false}, {"rsa", 3072, true}, {"ecdsa", 384, true},
		{"ecdsa", 512, false}, {"dsa", 1024, false}, {"x448", 0, false}} {
		_, err := generateKey(tc.typ, tc.bits)
		if (err == nil) != tc.ok {
			t.Fatalf("%s %d: %v", tc.typ, tc.bits, err)
		}
	}
	// The ECDSA curves map to the right SSH types.
	k, _ := generateKey("ecdsa", 521)
	if k.(*ecdsa.PrivateKey).Curve != elliptic.P521() {
		t.Fatal("curve")
	}
}

func TestDSAWriterReadableByOurParserOnly(t *testing.T) {
	dk, err := parseKey(fixture(t, "openssh_dsa_enc"), "testpass")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := dk.Raw.(*dsa.PrivateKey); !ok {
		t.Fatalf("%T", dk.Raw)
	}
	out, err := marshalOpenSSHPrivateKey(dk.Raw, "again", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	back, err := parseKey(out, "x")
	if err != nil || !sameKey(back.Pub, dk.Pub) || back.Comment != "again" {
		t.Fatalf("%v", err)
	}
	// Stored DSA keys use PEM, which sshx can read.
	stored, err := storageText(dk, nil, "", "pw")
	if err != nil || !bytes.Contains(stored, []byte("DSA PRIVATE KEY")) {
		t.Fatalf("%v %s", err, stored)
	}
	if _, err := sshx.ParsePrivateKey(stored, "pw"); err != nil {
		t.Fatal(err)
	}
}

func TestPKCS8RejectsBadParameters(t *testing.T) {
	k, _ := generateKey("ed25519", 0)
	block, err := encryptPKCS8(k, []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decryptPKCS8(block.Bytes, []byte("nope")); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
	if _, err := decryptPKCS8([]byte{0x30, 0x03, 0x02, 0x01, 0x00}, []byte("pw")); err == nil {
		t.Fatal("garbage accepted")
	}
	raw, err := decryptPKCS8(block.Bytes, []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newParsedKey(raw, "", formatPKCS8, true); err != nil {
		t.Fatal(err)
	}
	_ = rand.Reader
}

// TestPPKWriterMatchesPuTTYgen re-encodes genuine puttygen files (0.84, and the older ones of kayrus/putty's tests)
// and requires byte-identical output: the MAC, the encryption (v2 SHA-1 key derivation, v3 Argon2id with the file's
// parameters), the SHA-1 padding and the layout all match PuTTY's.
func TestPPKWriterMatchesPuTTYgen(t *testing.T) {
	cases := []struct{ file, pass string }{
		{"puttygen084_ed25519_plain.ppk", ""},
		{"puttygen084_ed25519_enc.ppk", "testkey"},
		{"puttygen084_rsa_v3_enc.ppk", "testkey"},
		{"puttygen084_rsa_v2_enc.ppk", "testkey"},
		{"puttygen084_ecdsa384_enc.ppk", "testkey"},
		{"puttygen084_dsa_enc.ppk", "testkey"},
		{"putty_v2_ed25519_enc.ppk", "testkey"},
		{"putty_v2_nistp256_enc.ppk", "testkey"},
		{"putty_v2_nistp521_enc.ppk", "testkey"},
		{"putty_v3_rsa_enc.ppk", "testkey"},
		{"putty_v3_rsa_plain.ppk", ""},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			text := fixture(t, tc.file)
			pk, err := parseKey(text, tc.pass)
			if err != nil {
				t.Fatal(err)
			}
			hdr, err := putty.New(cleanKeyText(text))
			if err != nil {
				t.Fatal(err)
			}
			var kdf *ppkKDF
			if hdr.Version == 3 && tc.pass != "" {
				if hdr.KeyDerivation != "Argon2id" {
					t.Skipf("key derivation %s", hdr.KeyDerivation)
				}
				kdf = &ppkKDF{memoryKiB: hdr.Argon2Memory, passes: hdr.Argon2Passes, lanes: hdr.Argon2Parallelism, salt: hdr.Argon2Salt}
			}
			out, err := marshalPPKWith(pk.Raw, pk.Comment, []byte(tc.pass), hdr.Version, kdf)
			if err != nil {
				t.Fatal(err)
			}
			want := string(cleanKeyText(text)) + "\n"
			if string(out) != want {
				t.Fatalf("differs from puttygen:\n%s\nwant\n%s", out, want)
			}
		})
	}
}
