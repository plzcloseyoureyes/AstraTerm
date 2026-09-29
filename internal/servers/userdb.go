package servers

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/nexterm/nexterm/internal/auth"
)

var (
	errBadCredentials = errors.New("authentication failed")
	errTooManyFails   = errors.New("too many failed logins, try again later")
)

const (
	authCacheTTL   = 10 * time.Minute
	authCacheMax   = 1024
	failWindow     = 10 * time.Minute
	failMax        = 10
	failBlock      = 10 * time.Minute
	failDelay      = 400 * time.Millisecond
	limiterMaxKeys = 10_000
)

// userDB authenticates the users of one server run. Successful password checks are cached (keyed by an HMAC of the
// credentials) so HTTP basic auth does not pay argon2 on every request; failures are rate limited per client IP.
type userDB struct {
	users   map[string]*dbUser
	key     [32]byte
	lim     *failLimiter
	stats   *counters
	mu      sync.Mutex
	cache   map[[32]byte]time.Time
	sleepFn func(time.Duration)
}

type dbUser struct {
	User
	keys [][]byte // marshaled public keys
}

func newUserDB(list []User, stats *counters) *userDB {
	db := &userDB{users: map[string]*dbUser{}, lim: newFailLimiter(), stats: stats, cache: map[[32]byte]time.Time{},
		sleepFn: time.Sleep}
	_, _ = rand.Read(db.key[:])
	for _, u := range list {
		du := &dbUser{User: u}
		for _, line := range u.PublicKeys {
			if pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err == nil {
				du.keys = append(du.keys, pk.Marshal())
			}
		}
		db.users[u.Username] = du
	}
	return db
}

func (db *userDB) lookup(name string) *dbUser { return db.users[name] }

var (
	dummyOnce sync.Once
	dummyHash string
)

// burn runs one password verification against a throw-away hash (unknown users cost as much as known ones).
func burn(password string) {
	dummyOnce.Do(func() {
		var b [16]byte
		_, _ = rand.Read(b[:])
		dummyHash, _ = auth.HashPassword(string(b[:]))
	})
	if dummyHash != "" {
		auth.VerifyPassword(dummyHash, password)
	}
}

// checkPassword authenticates username/password from ip.
func (db *userDB) checkPassword(ip, username, password string) (*dbUser, error) {
	if db.lim.blocked(ip) {
		return nil, errTooManyFails
	}
	u := db.users[username]
	if u == nil || u.PasswordHash == "" {
		burn(password)
		return nil, db.fail(ip)
	}
	ck := db.cacheKey(u, password)
	db.mu.Lock()
	exp, ok := db.cache[ck]
	db.mu.Unlock()
	if ok && time.Now().Before(exp) {
		return u, nil
	}
	if !auth.VerifyPassword(u.PasswordHash, password) {
		return nil, db.fail(ip)
	}
	// A success does not reset the failure count: with one valid (e.g. guest) account an attacker could otherwise
	// interleave logins to brute-force other accounts without ever being blocked.
	db.mu.Lock()
	if len(db.cache) >= authCacheMax {
		db.cache = map[[32]byte]time.Time{}
	}
	db.cache[ck] = time.Now().Add(authCacheTTL)
	db.mu.Unlock()
	return u, nil
}

// checkKey reports whether key is one of username's authorized keys. It does not count failures: SSH clients offer
// several keys before one is accepted.
func (db *userDB) checkKey(ip, username string, key ssh.PublicKey) (*dbUser, error) {
	if db.lim.blocked(ip) {
		return nil, errTooManyFails
	}
	u := db.users[username]
	if u == nil {
		return nil, errBadCredentials
	}
	m := key.Marshal()
	for _, k := range u.keys {
		if len(k) == len(m) && subtle.ConstantTimeCompare(k, m) == 1 {
			return u, nil
		}
	}
	return nil, errBadCredentials
}

func (db *userDB) fail(ip string) error {
	if db.stats != nil {
		db.stats.authFailures.Add(1)
	}
	blocked := db.lim.fail(ip)
	if db.sleepFn != nil {
		db.sleepFn(failDelay)
	}
	if blocked {
		return errTooManyFails
	}
	return errBadCredentials
}

func (db *userDB) cacheKey(u *dbUser, password string) [32]byte {
	m := hmac.New(sha256.New, db.key[:])
	var buf bytes.Buffer
	buf.WriteString(u.Username)
	buf.WriteByte(0)
	buf.WriteString(u.PasswordHash)
	buf.WriteByte(0)
	buf.WriteString(password)
	m.Write(buf.Bytes())
	var out [32]byte
	copy(out[:], m.Sum(nil))
	return out
}

// limitKey groups client addresses for throttling: IPv4 (also IPv4-mapped IPv6) per address, IPv6 per /64 prefix —
// a single host or site controls a whole /64, so counting per address would let it rotate addresses forever.
func limitKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.WithZone("").Unmap()
	if a.Is6() {
		if p, err := a.Prefix(64); err == nil {
			return p.String()
		}
	}
	return a.String()
}

// failLimiter blocks a client (see limitKey) for failBlock after failMax failed logins within failWindow.
type failLimiter struct {
	mu  sync.Mutex
	m   map[string]*failEntry
	now func() time.Time
}

type failEntry struct {
	count        int
	first        time.Time
	blockedUntil time.Time
}

func newFailLimiter() *failLimiter {
	return &failLimiter{m: map[string]*failEntry{}, now: time.Now}
}

func (l *failLimiter) blocked(ip string) bool {
	ip = limitKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[ip]
	return e != nil && l.now().Before(e.blockedUntil)
}

// fail records a failure and reports whether ip is now blocked.
func (l *failLimiter) fail(ip string) bool {
	ip = limitKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.m) >= limiterMaxKeys {
		for k, e := range l.m {
			if now.Sub(e.first) > failWindow && now.After(e.blockedUntil) {
				delete(l.m, k)
			}
		}
	}
	e := l.m[ip]
	if e == nil || now.Sub(e.first) > failWindow && now.After(e.blockedUntil) {
		if e == nil && len(l.m) >= limiterMaxKeys {
			return false // table full of active entries: fail open rather than grow without bound
		}
		e = &failEntry{first: now}
		l.m[ip] = e
	}
	e.count++
	if e.count >= failMax {
		e.blockedUntil = now.Add(failBlock)
		e.count = 0
		e.first = now
		return true
	}
	return now.Before(e.blockedUntil)
}
