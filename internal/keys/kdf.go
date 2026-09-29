package keys

import (
	"errors"
	"runtime"
	"time"

	"github.com/nexterm/nexterm/internal/httpx"
)

// Password-based key derivations of imported key files (bcrypt_pbkdf of OpenSSH keys, Argon2 of PPK v3 files,
// PBKDF2 of encrypted PKCS#8) have a cost chosen by whoever wrote the file: within the accepted bounds one derivation
// can take seconds of CPU or 128 MiB of memory, and the import dialog inspects the key as the passphrase is typed.
// They therefore run in a few process-wide slots — like the other expensive operations (encrypting exports, RSA key
// generation); a request that cannot get one in time is answered 429.

var kdfSlots = make(chan struct{}, min(max(runtime.NumCPU()/2, 2), 4))

const kdfWait = 20 * time.Second

// errKDFBusy is returned when every derivation slot stayed busy (429 {code:'too_many_requests'} + Retry-After).
var errKDFBusy = httpx.TooManyRequests("too many keys are being decrypted at the moment; try again", 5)

// withKDF runs fn in a derivation slot.
func withKDF(fn func() error) error {
	t := time.NewTimer(kdfWait)
	defer t.Stop()
	select {
	case kdfSlots <- struct{}{}:
	case <-t.C:
		return errKDFBusy
	}
	defer func() { <-kdfSlots }()
	return fn()
}

// isKDFBusy reports whether err is errKDFBusy.
func isKDFBusy(err error) bool { return errors.Is(err, errKDFBusy) }
