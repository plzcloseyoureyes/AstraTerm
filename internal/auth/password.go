package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// PasswordParams are the argon2id parameters for new password hashes (verification uses the parameters encoded in
// each hash, so they can be raised later). Tests may lower them.
var PasswordParams = struct {
	Time    uint32
	MemKiB  uint32
	Threads uint8
}{Time: 2, MemKiB: 64 * 1024, Threads: 2}

// Password policy.
const (
	MinPasswordLen = 8
	MaxPasswordLen = 1024
)

// hashSem bounds concurrent argon2 computations (64 MiB each) to protect memory under a login flood.
var hashSem = make(chan struct{}, 4)

var (
	dummyOnce sync.Once
	dummyHash string
)

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

// ValidateUsername checks the username policy.
func ValidateUsername(u string) error {
	if !usernameRE.MatchString(u) {
		return errors.New("username must be 1-64 characters: letters, digits, '.', '_', '@', '-' (starting with a letter or digit)")
	}
	return nil
}

// ValidatePassword checks the password policy.
func ValidatePassword(p string) error {
	if utf8.RuneCountInString(p) < MinPasswordLen {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	if len(p) > MaxPasswordLen {
		return fmt.Errorf("password must be at most %d bytes", MaxPasswordLen)
	}
	return nil
}

// HashPassword returns an argon2id PHC string ($argon2id$v=19$m=…,t=…,p=…$salt$hash).
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	p := PasswordParams
	hashSem <- struct{}{}
	key := argon2.IDKey([]byte(password), salt, p.Time, p.MemKiB, p.Threads, 32)
	<-hashSem
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.MemKiB, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks password against an encoded hash in constant time. Malformed hashes never match.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || len(password) > MaxPasswordLen {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	if m == 0 || m > 1<<21 || t == 0 || t > 32 || p == 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false
	}
	hashSem <- struct{}{}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	<-hashSem
	return subtle.ConstantTimeCompare(got, want) == 1
}

// burnPasswordCheck spends the same work as a real verification (used for unknown users to avoid a timing oracle).
func burnPasswordCheck(password string) {
	dummyOnce.Do(func() {
		dummyHash, _ = HashPassword("astraterm-dummy-password")
	})
	VerifyPassword(dummyHash, password)
}

// ---- opaque tokens ------------------------------------------------------------------------------------------------

// APITokenPrefix prefixes every personal access token.
const APITokenPrefix = "nxt_"

// randomToken returns n random bytes, base64url-encoded without padding.
func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// hashToken returns the hex SHA-256 of an opaque token (session cookies, API tokens).
func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// constantTimeEqual compares two strings without leaking timing about their contents.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
