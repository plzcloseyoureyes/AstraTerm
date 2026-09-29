package vault

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/termstead/termstead/internal/store"
)

func init() {
	DefaultKDF.Time, DefaultKDF.MemKiB, DefaultKDF.Threads = 1, 1024, 1
}

func setup(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, dir
}

func TestSealOpen(t *testing.T) {
	ctx := context.Background()
	st, dir := setup(t)
	v, err := Open(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	if v.Locked() || v.HasMasterPassword() {
		t.Fatal("fresh vault should be unlocked without master password")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, systemKeyFile))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("system key perms: %v %v", fi.Mode(), err)
		}
	}
	ct, err := v.Seal([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	ct2, _ := v.Seal([]byte("hello"))
	if bytes.Equal(ct, ct2) {
		t.Fatal("nonce reuse: identical ciphertexts")
	}
	pt, err := v.Open(ct)
	if err != nil || string(pt) != "hello" {
		t.Fatalf("open: %q %v", pt, err)
	}
	ct[len(ct)-1] ^= 1
	if _, err := v.Open(ct); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("tampered: %v", err)
	}
	if _, err := v.Open([]byte{1, 2}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("short: %v", err)
	}

	enc, err := v.SealJSON(map[string]string{"password": "p", "empty": ""})
	if err != nil {
		t.Fatal(err)
	}
	m, err := v.OpenJSON(enc)
	if err != nil || len(m) != 1 || m["password"] != "p" {
		t.Fatalf("json roundtrip: %v %v", m, err)
	}
	if enc, _ := v.SealJSON(map[string]string{"a": ""}); enc != nil {
		t.Fatal("empty secrets should seal to nil")
	}

	// A second Open with the same store/dir unwraps the same DEK.
	v2, err := Open(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := v2.OpenJSON(enc); err != nil || m["password"] != "p" {
		t.Fatalf("reopen: %v %v", m, err)
	}
	// System key sealing is independent of the DEK.
	sct, _ := v.SystemSeal([]byte("totp"))
	if _, err := v.Open(sct); err == nil {
		t.Fatal("system ciphertext opened with DEK")
	}
	if pt, err := v2.SystemOpen(sct); err != nil || string(pt) != "totp" {
		t.Fatalf("system open: %q %v", pt, err)
	}
	if err := v.Lock(); !errors.Is(err, ErrNoMasterPassword) {
		t.Fatalf("lock without master: %v", err)
	}
}

func TestMasterPassword(t *testing.T) {
	ctx := context.Background()
	st, dir := setup(t)
	v, err := Open(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	var changes atomic.Int32
	v.OnChange(func(bool) { changes.Add(1) })
	secret, _ := v.SealJSON(map[string]string{"password": "s3cret"})

	if err := v.SetMasterPassword(ctx, "", "short"); err == nil {
		t.Fatal("short master password accepted")
	}
	if err := v.SetMasterPassword(ctx, "", "correct horse"); err != nil {
		t.Fatal(err)
	}
	if !v.HasMasterPassword() || v.Locked() {
		t.Fatal("after set: should have master password and stay unlocked")
	}

	// Restart: locked until unlocked; secrets inaccessible, empty ciphertext still fine.
	v, err = Open(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	v.OnChange(func(bool) { changes.Add(1) })
	if !v.Locked() || !v.HasMasterPassword() {
		t.Fatal("restarted vault should be locked")
	}
	if _, err := v.OpenJSON(secret); !errors.Is(err, ErrLocked) {
		t.Fatalf("open while locked: %v", err)
	}
	if _, err := v.Seal([]byte("x")); !errors.Is(err, ErrLocked) {
		t.Fatalf("seal while locked: %v", err)
	}
	if m, err := v.OpenJSON(nil); err != nil || len(m) != 0 {
		t.Fatalf("empty secrets while locked: %v %v", m, err)
	}
	if err := v.SetMasterPassword(ctx, "correct horse", "other password"); !errors.Is(err, ErrLocked) {
		t.Fatalf("change while locked: %v", err)
	}
	if err := v.Unlock(ctx, "wrong password"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong unlock: %v", err)
	}
	if err := v.Unlock(ctx, "correct horse"); err != nil {
		t.Fatal(err)
	}
	if m, err := v.OpenJSON(secret); err != nil || m["password"] != "s3cret" {
		t.Fatalf("after unlock: %v %v", m, err)
	}
	if err := v.Lock(); err != nil || !v.Locked() {
		t.Fatalf("lock: %v", err)
	}
	if err := v.Unlock(ctx, "correct horse"); err != nil {
		t.Fatal(err)
	}
	if changes.Load() != 3 { // unlock, lock, unlock
		t.Fatalf("OnChange calls = %d", changes.Load())
	}

	// Change requires the current password.
	if err := v.SetMasterPassword(ctx, "nope", "new password!"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("change with wrong current: %v", err)
	}
	if err := v.SetMasterPassword(ctx, "correct horse", "new password!"); err != nil {
		t.Fatal(err)
	}
	v, _ = Open(ctx, st, dir)
	if err := v.Unlock(ctx, "correct horse"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("old password still works: %v", err)
	}
	if err := v.Unlock(ctx, "new password!"); err != nil {
		t.Fatal(err)
	}

	// Remove: vault opens unlocked again with the same DEK.
	if err := v.SetMasterPassword(ctx, "new password!", ""); err != nil {
		t.Fatal(err)
	}
	v, _ = Open(ctx, st, dir)
	if v.Locked() || v.HasMasterPassword() {
		t.Fatal("after removal: should be unlocked without master password")
	}
	if m, err := v.OpenJSON(secret); err != nil || m["password"] != "s3cret" {
		t.Fatalf("after removal: %v %v", m, err)
	}
}

func TestWrongSystemKey(t *testing.T) {
	ctx := context.Background()
	st, dir := setup(t)
	if _, err := Open(ctx, st, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, systemKeyFile), bytes.Repeat([]byte{7}, keyLen), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, st, dir); err == nil {
		t.Fatal("expected failure with a replaced system key")
	}
	if err := os.WriteFile(filepath.Join(dir, systemKeyFile), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, st, dir); err == nil {
		t.Fatal("expected failure with a truncated system key")
	}
}
