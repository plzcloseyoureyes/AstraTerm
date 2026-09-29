package rdp

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"sync"
	"time"

	"github.com/nexterm/nexterm/internal/model"
)

// Tickets authorize one viewer connection: POST /api/sessions/{id}/rdp-ticket mints a random token bound
// server-side to the session, its owner, the engine and the destination resolved at that moment; the relay
// (/ws/rdp) or tunnel (/ws/guac) consumes it exactly once within ticketTTL. The destination a client names in the
// RDCleanPath PDU is never trusted — the relay dials the ticket's destination.

const (
	ticketTTL = 60 * time.Second
	// maxTicketsPerUser bounds unconsumed tickets per user (a reconnect storm cannot grow the map unboundedly).
	maxTicketsPerUser = 32
)

type ticket struct {
	token     string
	sessionID string
	userID    string
	engine    string
	conn      *model.Connection // resolved connection (private copy, secrets stripped)
	secrets   map[string]string // decrypted secrets (proxy / gateway / remembered prompt answers)
	host      string
	port      int
	width     int
	height    int
	dpi       int
	expires   time.Time
	// autologon: the IronRDP client got a user name and a password; the relay then asks TLS-security servers to log
	// on with them (clientfilter.go).
	autologon bool
	// shadow: an administrator's read-only view joining the guacd connection join of the owner's viewer.
	shadow bool
	join   string
}

type ticketStore struct {
	mu      sync.Mutex
	tickets map[string]*ticket
	now     func() time.Time
}

func newTicketStore() *ticketStore {
	return &ticketStore{tickets: map[string]*ticket{}, now: time.Now}
}

func newToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("rdp: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// issue stores t under a fresh token and returns the token.
func (s *ticketStore) issue(t *ticket) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweepLocked(now)
	// Drop the user's oldest tickets beyond the limit.
	var mine []*ticket
	for _, x := range s.tickets {
		if x.userID == t.userID {
			mine = append(mine, x)
		}
	}
	for len(mine) >= maxTicketsPerUser {
		oldest := 0
		for i, x := range mine {
			if x.expires.Before(mine[oldest].expires) {
				oldest = i
			}
		}
		delete(s.tickets, mine[oldest].token)
		mine = append(mine[:oldest], mine[oldest+1:]...)
	}
	t.token = newToken()
	t.expires = now.Add(ticketTTL)
	s.tickets[t.token] = t
	return t.token
}

// consume returns and removes the ticket for token if it is unexpired and bound to sessionID, userID and engine.
// A token presented for the wrong session, user or engine is burnt as well (it may have leaked).
func (s *ticketStore) consume(token, sessionID, userID, engine string) (*ticket, bool) {
	if token == "" || len(token) > 256 {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweepLocked(now)
	t, ok := s.tickets[token]
	if !ok {
		return nil, false
	}
	delete(s.tickets, token)
	if subtle.ConstantTimeCompare([]byte(t.token), []byte(token)) != 1 || !now.Before(t.expires) ||
		t.sessionID != sessionID || t.userID != userID || t.engine != engine {
		return nil, false
	}
	return t, true
}

// revokeSession drops every ticket of a session (session closed).
func (s *ticketStore) revokeSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range s.tickets {
		if t.sessionID == sessionID {
			delete(s.tickets, k)
		}
	}
}

func (s *ticketStore) sweepLocked(now time.Time) {
	for k, t := range s.tickets {
		if !now.Before(t.expires) {
			delete(s.tickets, k)
		}
	}
}

// len reports the number of stored tickets (tests).
func (s *ticketStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tickets)
}
