package ai

import (
	"fmt"
	"sync"
	"time"

	"github.com/termstead/termstead/internal/httpx"
)

// limiter enforces per-user request limits: a sliding one-minute window and a calendar-day (UTC) counter. Limits
// ≤ 0 disable the respective check. Memory is bounded by the number of active users.
type limiter struct {
	mu     sync.Mutex
	users  map[string]*userLimit
	active map[string]int // streams in flight per user
	now    func() time.Time
}

// maxConcurrentStreams bounds the answers one user can have in flight (each holds a provider connection).
const maxConcurrentStreams = 4

type userLimit struct {
	recent []time.Time // requests within the last minute (oldest first)
	day    string
	count  int
}

func newLimiter() *limiter {
	return &limiter{users: map[string]*userLimit{}, active: map[string]int{}, now: time.Now}
}

// allow records a request for userID or returns a 429 error with Retry-After.
func (l *limiter) allow(userID string, perMinute, perDay int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	u := l.users[userID]
	if u == nil {
		u = &userLimit{}
		l.users[userID] = u
	}
	cutoff := now.Add(-time.Minute)
	i := 0
	for i < len(u.recent) && !u.recent[i].After(cutoff) {
		i++
	}
	u.recent = u.recent[i:]
	day := now.UTC().Format("2006-01-02")
	if u.day != day {
		u.day, u.count = day, 0
	}
	if perMinute > 0 && len(u.recent) >= perMinute {
		wait := int(u.recent[0].Add(time.Minute).Sub(now).Seconds()) + 1
		return httpx.TooManyRequests(fmt.Sprintf("AI request limit reached (%d per minute) — try again in %d s", perMinute, wait), wait)
	}
	if perDay > 0 && u.count >= perDay {
		next := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day()+1, 0, 0, 0, 0, time.UTC)
		wait := int(next.Sub(now).Seconds()) + 1
		return httpx.TooManyRequests(fmt.Sprintf("daily AI request limit reached (%d per day)", perDay), wait)
	}
	u.recent = append(u.recent, now)
	u.count++
	// Opportunistic cleanup of idle users.
	if len(l.users) > 256 {
		for id, x := range l.users {
			if id != userID && len(x.recent) == 0 && x.day != day {
				delete(l.users, id)
			}
		}
	}
	return nil
}

// acquire reserves one of the user's concurrent stream slots; release it when the answer ends.
func (l *limiter) acquire(userID string) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[userID] >= maxConcurrentStreams {
		return nil, httpx.TooManyRequests(fmt.Sprintf("%d answers are already in progress — wait for one to finish", maxConcurrentStreams), 5)
	}
	l.active[userID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			if l.active[userID]--; l.active[userID] <= 0 {
				delete(l.active, userID)
			}
			l.mu.Unlock()
		})
	}, nil
}
