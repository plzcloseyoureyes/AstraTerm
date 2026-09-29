// Package vault encrypts AstraTerm's secrets (SPEC §4 "Vault keys", RESEARCH SEC-1).
//
// Two keys exist:
//   - the system key: 32 random bytes in <data>/system.key (0600), always available; it protects server-internal
//     secrets that must work before anyone unlocks anything (TOTP seeds, share / launch tokens) — SystemSeal/SystemOpen;
//   - the data key (DEK): 32 random bytes that encrypt user secrets (connection / identity secrets, private keys) —
//     Seal/Open, SealJSON/OpenJSON. The DEK is stored wrapped either by the system key (no master password) or by a
//     KEK derived from the master password with argon2id (t=3, m=64 MiB, p=4). With a master password the vault starts
//     locked after every restart until an admin unlocks it.
//
// Ciphertext format: version byte 0x01 || 24-byte random nonce || XChaCha20-Poly1305(ciphertext+tag).
package vault

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
)

// Errors.
var (
	// ErrLocked is returned when the DEK is needed but the vault is locked (maps to HTTP 423).
	ErrLocked = model.ErrLocked
	// ErrWrongPassword is returned for a wrong master password.
	ErrWrongPassword = &model.Error{Code: model.CodeForbidden, Msg: "wrong master password"}
	// ErrNoMasterPassword is returned by Lock when no master password is set (the vault cannot be locked).
	ErrNoMasterPassword = &model.Error{Code: model.CodeConflict, Msg: "no master password is set"}
	// ErrCorrupt is returned for malformed ciphertext or authentication failures.
	ErrCorrupt = errors.New("vault: ciphertext is corrupt or was sealed with another key")
)

const (
	keyLen         = 32
	formatV1       = 0x01
	systemKeyFile  = "system.key"
	metaDEKSystem  = "dek.system" // DEK sealed with the system key
	metaDEKMaster  = "dek.master" // DEK sealed with the master-password KEK
	metaKDF        = "kdf"        // JSON kdfParams
	metaVerifier   = "verifier"   // constant sealed with the KEK (fast wrong-password detection)
	verifierPlain  = "astraterm-vault-verifier-v1"
	minPasswordLen = 8
	maxPasswordLen = 1024
)

// KDFParams are the argon2id parameters stored in vault_meta (so they can be upgraded later).
type KDFParams struct {
	Alg     string `json:"alg"`
	Time    uint32 `json:"t"`
	MemKiB  uint32 `json:"m"`
	Threads uint8  `json:"p"`
	Salt    []byte `json:"salt"`
}

// DefaultKDF is used for new master passwords. Tests may lower it.
var DefaultKDF = KDFParams{Alg: "argon2id", Time: 3, MemKiB: 64 * 1024, Threads: 4}

// Vault holds the key material. It is safe for concurrent use.
type Vault struct {
	meta *store.VaultMeta

	mu        sync.RWMutex
	sysKey    []byte
	dek       []byte // nil while locked
	hasMaster bool

	kdfMu    sync.Mutex // serializes argon2 derivations (64 MiB each)
	onChange func(locked bool)
}

// Open loads (or creates) the system key in dataDir and the DEK from the store. On first run a fresh DEK is generated
// and wrapped with the system key.
func Open(ctx context.Context, st *store.Store, dataDir string) (*Vault, error) {
	sys, err := loadOrCreateSystemKey(filepath.Join(dataDir, systemKeyFile))
	if err != nil {
		return nil, err
	}
	v := &Vault{meta: st.VaultMeta, sysKey: sys}
	all, err := st.VaultMeta.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("vault: load meta: %w", err)
	}
	switch {
	case all[metaDEKMaster] != nil:
		v.hasMaster = true // locked until Unlock
	case all[metaDEKSystem] != nil:
		dek, err := open(sys, all[metaDEKSystem])
		if err != nil {
			return nil, fmt.Errorf("vault: cannot unwrap data key with %s (wrong or replaced system key?): %w", systemKeyFile, err)
		}
		if len(dek) != keyLen {
			return nil, fmt.Errorf("vault: invalid data key length")
		}
		v.dek = dek
	default:
		dek := randomBytes(keyLen)
		wrapped, err := seal(sys, dek)
		if err != nil {
			return nil, err
		}
		if err := st.VaultMeta.Apply(ctx, map[string][]byte{metaDEKSystem: wrapped}, nil); err != nil {
			return nil, fmt.Errorf("vault: store data key: %w", err)
		}
		v.dek = dek
	}
	return v, nil
}

func loadOrCreateSystemKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		if len(b) != keyLen {
			return nil, fmt.Errorf("vault: %s has invalid length %d (expected %d)", path, len(b), keyLen)
		}
		return b, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("vault: read system key: %w", err)
	}
	key := randomBytes(keyLen)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) { // lost a creation race: read the winner's key
			return loadOrCreateSystemKey(path)
		}
		return nil, fmt.Errorf("vault: create system key: %w", err)
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("vault: write system key: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, fmt.Errorf("vault: sync system key: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return key, nil
}

// OnChange registers a callback invoked (outside locks) whenever the lock state changes.
func (v *Vault) OnChange(fn func(locked bool)) {
	v.mu.Lock()
	v.onChange = fn
	v.mu.Unlock()
}

func (v *Vault) notify(locked bool) {
	v.mu.RLock()
	fn := v.onChange
	v.mu.RUnlock()
	if fn != nil {
		fn(locked)
	}
}

// Locked reports whether the DEK is unavailable.
func (v *Vault) Locked() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.dek == nil
}

// HasMasterPassword reports whether the DEK is protected by a master password.
func (v *Vault) HasMasterPassword() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.hasMaster
}

// ---- data key operations ------------------------------------------------------------------------------------------

// Seal encrypts plaintext with the DEK.
func (v *Vault) Seal(plaintext []byte) ([]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dek == nil {
		return nil, ErrLocked
	}
	return seal(v.dek, plaintext)
}

// Open decrypts ciphertext produced by Seal.
func (v *Vault) Open(ciphertext []byte) ([]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dek == nil {
		return nil, ErrLocked
	}
	return open(v.dek, ciphertext)
}

// SealJSON encrypts a secrets map. Empty values are dropped; an empty map yields nil (no ciphertext, and no vault
// access needed).
func (v *Vault) SealJSON(m map[string]string) ([]byte, error) {
	clean := make(map[string]string, len(m))
	for k, val := range m {
		if val != "" {
			clean[k] = val
		}
	}
	if len(clean) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return nil, err
	}
	defer clear(b)
	return v.Seal(b)
}

// OpenJSON decrypts a secrets map. Empty ciphertext yields an empty map without touching the DEK, so connections
// without secrets keep working while the vault is locked.
func (v *Vault) OpenJSON(ciphertext []byte) (map[string]string, error) {
	out := map[string]string{}
	if len(ciphertext) == 0 {
		return out, nil
	}
	b, err := v.Open(ciphertext)
	if err != nil {
		return nil, err
	}
	defer clear(b)
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("%w: invalid secrets payload", ErrCorrupt)
	}
	return out, nil
}

// ---- system key operations ----------------------------------------------------------------------------------------

// SystemSeal encrypts with the system key (always available).
func (v *Vault) SystemSeal(plaintext []byte) ([]byte, error) { return seal(v.sysKey, plaintext) }

// SystemOpen decrypts ciphertext produced by SystemSeal.
func (v *Vault) SystemOpen(ciphertext []byte) ([]byte, error) { return open(v.sysKey, ciphertext) }

// ---- master password ----------------------------------------------------------------------------------------------

// Unlock derives the KEK from password and unwraps the DEK. It is a no-op when already unlocked or when no master
// password is set.
func (v *Vault) Unlock(ctx context.Context, password string) error {
	if !v.HasMasterPassword() || !v.Locked() {
		return nil
	}
	dek, err := v.unwrapWithPassword(ctx, password)
	if err != nil {
		return err
	}
	v.mu.Lock()
	if v.dek != nil { // unlocked concurrently
		v.mu.Unlock()
		clear(dek)
		return nil
	}
	v.dek = dek
	v.mu.Unlock()
	v.notify(false)
	return nil
}

// Lock discards the DEK from memory. It requires a master password (otherwise the vault could not be reopened
// without restarting and would unlock itself anyway).
func (v *Vault) Lock() error {
	v.mu.Lock()
	if !v.hasMaster {
		v.mu.Unlock()
		return ErrNoMasterPassword
	}
	if v.dek == nil {
		v.mu.Unlock()
		return nil
	}
	clear(v.dek)
	v.dek = nil
	v.mu.Unlock()
	v.notify(true)
	return nil
}

// VerifyMasterPassword checks password against the stored master password without changing the lock state.
func (v *Vault) VerifyMasterPassword(ctx context.Context, password string) error {
	if !v.HasMasterPassword() {
		return ErrNoMasterPassword
	}
	dek, err := v.unwrapWithPassword(ctx, password)
	if err != nil {
		return err
	}
	clear(dek)
	return nil
}

// SetMasterPassword sets, changes or (newPassword == "") removes the master password. The vault must be unlocked;
// when a master password exists, current must match it. The DEK itself is unchanged (only re-wrapped), so no
// stored secret needs re-encryption. The vault stays unlocked afterwards.
func (v *Vault) SetMasterPassword(ctx context.Context, current, newPassword string) error {
	if newPassword != "" && (len(newPassword) < minPasswordLen || len(newPassword) > maxPasswordLen) {
		return &model.Error{Code: model.CodeBadRequest, Msg: fmt.Sprintf("master password must be %d-%d characters", minPasswordLen, maxPasswordLen)}
	}
	v.mu.RLock()
	has, dekPresent := v.hasMaster, v.dek != nil
	v.mu.RUnlock()
	if !dekPresent {
		return ErrLocked
	}
	if has {
		if err := v.VerifyMasterPassword(ctx, current); err != nil {
			return err
		}
	} else if newPassword == "" {
		return nil // nothing to remove
	}

	v.mu.RLock()
	if v.dek == nil {
		v.mu.RUnlock()
		return ErrLocked
	}
	dek := bytes.Clone(v.dek)
	v.mu.RUnlock()
	defer clear(dek)

	if newPassword == "" {
		wrapped, err := seal(v.sysKey, dek)
		if err != nil {
			return err
		}
		if err := v.meta.Apply(ctx, map[string][]byte{metaDEKSystem: wrapped}, []string{metaDEKMaster, metaKDF, metaVerifier}); err != nil {
			return fmt.Errorf("vault: remove master password: %w", err)
		}
		v.mu.Lock()
		v.hasMaster = false
		v.mu.Unlock()
		return nil
	}

	params := DefaultKDF
	params.Salt = randomBytes(16)
	kek, err := v.derive(ctx, newPassword, params)
	if err != nil {
		return err
	}
	defer clear(kek)
	wrapped, err := seal(kek, dek)
	if err != nil {
		return err
	}
	verifier, err := seal(kek, []byte(verifierPlain))
	if err != nil {
		return err
	}
	pj, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if err := v.meta.Apply(ctx, map[string][]byte{metaDEKMaster: wrapped, metaKDF: pj, metaVerifier: verifier},
		[]string{metaDEKSystem}); err != nil {
		return fmt.Errorf("vault: store master password: %w", err)
	}
	v.mu.Lock()
	v.hasMaster = true
	v.mu.Unlock()
	return nil
}

func (v *Vault) unwrapWithPassword(ctx context.Context, password string) ([]byte, error) {
	if password == "" || len(password) > maxPasswordLen {
		return nil, ErrWrongPassword
	}
	all, err := v.meta.All(ctx)
	if err != nil {
		return nil, err
	}
	var params KDFParams
	if err := json.Unmarshal(all[metaKDF], &params); err != nil || params.Alg != "argon2id" || len(params.Salt) == 0 {
		return nil, fmt.Errorf("vault: invalid KDF parameters in store")
	}
	kek, err := v.derive(ctx, password, params)
	if err != nil {
		return nil, err
	}
	defer clear(kek)
	if ver := all[metaVerifier]; ver != nil {
		pt, err := open(kek, ver)
		if err != nil || subtle.ConstantTimeCompare(pt, []byte(verifierPlain)) != 1 {
			return nil, ErrWrongPassword
		}
	}
	dek, err := open(kek, all[metaDEKMaster])
	if err != nil {
		return nil, ErrWrongPassword
	}
	if len(dek) != keyLen {
		return nil, fmt.Errorf("vault: invalid data key length")
	}
	return dek, nil
}

// derive runs argon2id; derivations are serialized to bound memory use (64 MiB each).
func (v *Vault) derive(ctx context.Context, password string, p KDFParams) ([]byte, error) {
	if p.Time == 0 || p.MemKiB == 0 || p.Threads == 0 || p.MemKiB > 4<<20 || p.Time > 64 {
		return nil, fmt.Errorf("vault: unsupported KDF parameters")
	}
	v.kdfMu.Lock()
	defer v.kdfMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return argon2.IDKey([]byte(password), p.Salt, p.Time, p.MemKiB, p.Threads, keyLen), nil
}

// ---- primitives ---------------------------------------------------------------------------------------------------

func seal(key, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 1+aead.NonceSize(), 1+aead.NonceSize()+len(plaintext)+aead.Overhead())
	out[0] = formatV1
	if _, err := rand.Read(out[1:]); err != nil {
		return nil, err
	}
	return aead.Seal(out, out[1:1+aead.NonceSize()], plaintext, []byte{formatV1}), nil
}

func open(key, ct []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	ns := aead.NonceSize()
	if len(ct) < 1+ns+aead.Overhead() || ct[0] != formatV1 {
		return nil, ErrCorrupt
	}
	pt, err := aead.Open(nil, ct[1:1+ns], ct[1+ns:], []byte{formatV1})
	if err != nil {
		return nil, ErrCorrupt
	}
	return pt, nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("vault: crypto/rand failed: " + err.Error())
	}
	return b
}
