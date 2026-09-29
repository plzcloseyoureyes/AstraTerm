package importer

import (
	"bytes"
	"testing"
)

func init() {
	// Keep argon2 cheap in tests.
	exportKDF.Time, exportKDF.MemKiB, exportKDF.Threads = 1, 8, 1
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	plain := []byte(`{"format":"termstead-export","secret":"top"}`)
	env, err := encrypt(plain, "correct horse", payloadExport)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !isEncryptedEnvelope(env) {
		t.Fatalf("output is not recognised as an encrypted envelope")
	}
	if bytes.Contains(env, []byte("top")) {
		t.Fatalf("ciphertext leaks plaintext")
	}
	pt, payload, err := decrypt(env, "correct horse")
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if payload != payloadExport {
		t.Fatalf("payload = %q", payload)
	}
	if !bytes.Equal(pt, plain) {
		t.Fatalf("round-trip mismatch: %q", pt)
	}
}

func TestDecryptWrongPassphrase(t *testing.T) {
	env, err := encrypt([]byte("data"), "right-passphrase", payloadExport)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := decrypt(env, "wrong-passphrase"); err == nil {
		t.Fatalf("expected an error with the wrong passphrase")
	}
}

func TestEncryptRequiresPassphrase(t *testing.T) {
	if _, err := encrypt([]byte("x"), "", payloadExport); err == nil {
		t.Fatalf("expected an error with an empty passphrase")
	}
}
