package auth

import (
	"sync"
	"time"
)

// limiter implements per-key exponential backoff for failed authentication attempts (SEC-20). After `threshold`
// failures a key is locked for 1s, doubling with every further failure up to maxLock. A success resets the key.
type limiter struct {
	mu      sync.Mutex
	entries map[string]*limEntry
	now     func() time.Time
	maxLock time.Duration
	idleTTL time.Duration
}

type limEntry struct {
	fails       int
	lockedUntil time.Time
	last        time.Time
}

func newLimiter() *limiter {
	return &limiter{entries: map[string]*limEntry{}, now: time.Now, maxLock: 15 * time.Minute, idleTTL: time.Hour}
}

// retryAfter returns how long the most restrictive of keys is still locked (0 = allowed).
func (l *limiter) retryAfter(keys ...string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var worst time.Duration
	for _, k := range keys {
		if e := l.entries[k]; e != nil {
			if d := e.lockedUntil.Sub(now); d > worst {
				worst = d
			}
		}
	}
	return worst
}

// fail records a failure for key; once fails ≥ threshold the key is locked with exponential backoff.
func (l *limiter) fail(key string, threshold int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entries[key]
	if e == nil || now.Sub(e.last) > l.idleTTL {
		e = &limEntry{}
		l.entries[key] = e
	}
	e.fails++
	e.last = now
	if e.fails >= threshold {
		shift := min(e.fails-threshold, 20)
		d := min(time.Second<<shift, l.maxLock)
		e.lockedUntil = now.Add(d)
	}
}

// setMaxLock changes the backoff cap (login policy).
func (l *limiter) setMaxLock(d time.Duration) {
	if d <= 0 {
		return
	}
	l.mu.Lock()
	l.maxLock = d
	l.mu.Unlock()
}

// reset forgets key (after a successful attempt).
func (l *limiter) reset(key string) {
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

// sweep drops idle, unlocked entries.
func (l *limiter) sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, e := range l.entries {
		if now.After(e.lockedUntil) && now.Sub(e.last) > l.idleTTL {
			delete(l.entries, k)
		}
	}
}
