package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

func TestPasswordHash(t *testing.T) {
	PasswordParams.Time, PasswordParams.MemKiB, PasswordParams.Threads = 1, 1024, 1
	h, err := HashPassword("s3cret password")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=1024,t=1,p=1$") {
		t.Fatalf("format: %s", h)
	}
	if !VerifyPassword(h, "s3cret password") || VerifyPassword(h, "s3cret passwore") {
		t.Fatal("verify mismatch")
	}
	h2, _ := HashPassword("s3cret password")
	if h == h2 {
		t.Fatal("salt not random")
	}
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$AA$AA", "$argon2id$v=19$m=0,t=1,p=1$AAAA$AAAAAAAAAAAAAAAAAAAAAA"} {
		if VerifyPassword(bad, "x") {
			t.Fatalf("malformed hash %q verified", bad)
		}
	}
	if ValidatePassword("1234567") == nil || ValidatePassword("12345678") != nil {
		t.Fatal("password policy")
	}
	if ValidateUsername("ok.user@x") != nil || ValidateUsername("-bad") == nil || ValidateUsername("") == nil {
		t.Fatal("username policy")
	}
}

func TestLimiterBackoff(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLimiter()
	l.now = func() time.Time { return now }
	for range 4 {
		l.fail("k", 5)
	}
	if l.retryAfter("k") != 0 {
		t.Fatal("locked before threshold")
	}
	l.fail("k", 5)
	if d := l.retryAfter("k"); d != time.Second {
		t.Fatalf("first lock = %v", d)
	}
	l.fail("k", 5)
	if d := l.retryAfter("k", "other"); d != 2*time.Second {
		t.Fatalf("second lock = %v", d)
	}
	for range 30 {
		l.fail("k", 5)
	}
	if d := l.retryAfter("k"); d != l.maxLock {
		t.Fatalf("capped lock = %v", d)
	}
	l.reset("k")
	if l.retryAfter("k") != 0 {
		t.Fatal("reset did not unlock")
	}
	l.fail("idle", 1)
	now = now.Add(2 * time.Hour)
	l.sweep()
	if len(l.entries) != 0 {
		t.Fatalf("sweep left %d entries", len(l.entries))
	}
}

func TestTOTPVerifyAndRecovery(t *testing.T) {
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "x", AccountName: "y"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	code, _ := totp.GenerateCodeCustom(key.Secret(), now.Add(-30*time.Second), totpOpts)
	step, ok := verifyTOTPCode(key.Secret(), code, now)
	if !ok || step != now.Unix()/30-1 {
		t.Fatalf("skewed code: ok=%v step=%d", ok, step)
	}
	old, _ := totp.GenerateCodeCustom(key.Secret(), now.Add(-5*time.Minute), totpOpts)
	if _, ok := verifyTOTPCode(key.Secret(), old, now); ok && old != code {
		t.Fatal("stale code accepted")
	}
	if _, ok := verifyTOTPCode(key.Secret(), "12a456", now); ok {
		t.Fatal("non-digit code accepted")
	}
	codes, hashes := newRecoveryCodes()
	if len(codes) != 10 || len(hashes) != 10 {
		t.Fatal("recovery code count")
	}
	for i, c := range codes {
		if len(c) != 11 || c[5] != '-' || hashRecoveryCode(normalizeCode(" "+strings.ToUpper(c)+" ")) != hashes[i] {
			t.Fatalf("recovery code %q", c)
		}
	}
}

func TestCSVCell(t *testing.T) {
	for in, want := range map[string]string{
		"":                  "",
		"auth.login":        "auth.login",
		"=HYPERLINK(\"x\")": "'=HYPERLINK(\"x\")",
		"+1+1":              "'+1+1",
		"-2+3":              "'-2+3",
		"@SUM(A1)":          "'@SUM(A1)",
		"  =1+1":            "'  =1+1",
		"\t=1":              "'\t=1",
		"\r\n":              "'\r\n",
		"＝cmd":              "'＝cmd",
		"192.0.2.1":         "192.0.2.1",
		`{"username":"=x"}`: `{"username":"=x"}`,
	} {
		if got := csvCell(in); got != want {
			t.Errorf("csvCell(%q) = %q, want %q", in, got, want)
		}
	}
}
