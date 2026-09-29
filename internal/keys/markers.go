package keys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/store"
)

// Host key markers (SSH-18/20): known_hosts "@cert-authority <patterns> <CA key>" (trust host certificates signed by
// the CA for matching hosts) and "@revoked <patterns> <key>" (always reject the key). They are global like
// known_hosts, live in the module-owned table keys_host_markers and are cached in memory because sshx consults them
// inside every SSH handshake (sshx.HostKeyMarkers).

const markersSchemaV1 = `
CREATE TABLE keys_host_markers (
	id          TEXT PRIMARY KEY,
	marker      TEXT NOT NULL CHECK (marker IN ('cert-authority', 'revoked')),
	hosts       TEXT NOT NULL,
	key_type    TEXT NOT NULL,
	public_key  TEXT NOT NULL,
	fingerprint TEXT NOT NULL DEFAULT '',
	comment     TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL
);
CREATE INDEX keys_host_markers_marker ON keys_host_markers(marker);
`

func init() {
	store.RegisterMigration("keys", 1, markersSchemaV1)
}

// HostKeyMarker is a @cert-authority or @revoked known_hosts entry (JSON contract of /api/known-hosts/markers).
type HostKeyMarker struct {
	ID          string    `json:"id"`
	Marker      string    `json:"marker"` // cert-authority | revoked
	Hosts       string    `json:"hosts"`  // comma-separated OpenSSH host patterns
	KeyType     string    `json:"keyType"`
	PublicKey   string    `json:"publicKey"` // authorized_keys format ("type base64")
	Fingerprint string    `json:"fingerprint"`
	Comment     string    `json:"comment"`
	CreatedAt   time.Time `json:"createdAt"`
}

type markerEntry struct {
	m   HostKeyMarker
	key ssh.PublicKey
}

// markerStore is the repository + cache of host key markers.
type markerStore struct {
	db *sql.DB

	mu      sync.RWMutex
	entries []markerEntry
}

func newMarkerStore(db *sql.DB) *markerStore { return &markerStore{db: db} }

const markerCols = `id, marker, hosts, key_type, public_key, fingerprint, comment, created_at`

// load (re)reads every marker into the cache.
func (s *markerStore) load(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT `+markerCols+` FROM keys_host_markers ORDER BY created_at, id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var entries []markerEntry
	for rows.Next() {
		var (
			m       HostKeyMarker
			created int64
		)
		if err := rows.Scan(&m.ID, &m.Marker, &m.Hosts, &m.KeyType, &m.PublicKey, &m.Fingerprint, &m.Comment, &created); err != nil {
			return err
		}
		m.CreatedAt = time.UnixMilli(created).UTC()
		pk, _, err := parsePublicKeyText(m.PublicKey)
		if err != nil {
			continue // unreadable row: ignored (it cannot match anything)
		}
		entries = append(entries, markerEntry{m: m, key: pk})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.entries = entries
	s.mu.Unlock()
	return nil
}

// list returns the cached markers (newest last).
func (s *markerStore) list() []HostKeyMarker {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]HostKeyMarker, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.m)
	}
	return out
}

func (s *markerStore) get(id string) (HostKeyMarker, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.entries {
		if e.m.ID == id {
			return e.m, true
		}
	}
	return HostKeyMarker{}, false
}

// find returns the marker with the same kind, patterns and key, if any.
func (s *markerStore) find(marker, hosts string, key ssh.PublicKey) (HostKeyMarker, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.entries {
		if e.m.Marker == marker && strings.EqualFold(e.m.Hosts, hosts) && sameKey(e.key, key) {
			return e.m, true
		}
	}
	return HostKeyMarker{}, false
}

var errMarkerExists = errors.New("an identical entry already exists")

// add validates and stores a marker. It returns errMarkerExists for duplicates.
func (s *markerStore) add(ctx context.Context, marker, hosts string, key ssh.PublicKey, comment string) (HostKeyMarker, error) {
	if marker != markerCertAuthority && marker != markerRevoked {
		return HostKeyMarker{}, fmt.Errorf("marker must be %q or %q", markerCertAuthority, markerRevoked)
	}
	if _, ok := key.(*ssh.Certificate); ok {
		return HostKeyMarker{}, errors.New("a certificate cannot be used here; give the plain public key")
	}
	hosts, err := validateHostPatterns(hosts)
	if err != nil {
		return HostKeyMarker{}, err
	}
	if existing, ok := s.find(marker, hosts, key); ok {
		return existing, errMarkerExists
	}
	m := HostKeyMarker{
		ID:          model.NewID(),
		Marker:      marker,
		Hosts:       hosts,
		KeyType:     key.Type(),
		PublicKey:   strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))),
		Fingerprint: ssh.FingerprintSHA256(key),
		Comment:     cleanComment(comment),
		CreatedAt:   store.Now(),
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO keys_host_markers (`+markerCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Marker, m.Hosts, m.KeyType, m.PublicKey, m.Fingerprint, m.Comment, m.CreatedAt.UnixMilli()); err != nil {
		return HostKeyMarker{}, err
	}
	s.mu.Lock()
	s.entries = append(s.entries, markerEntry{m: m, key: key})
	s.mu.Unlock()
	return m, nil
}

// remove deletes a marker (model.ErrNotFound when unknown).
func (s *markerStore) remove(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM keys_host_markers WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.ErrNotFound
	}
	s.mu.Lock()
	for i, e := range s.entries {
		if e.m.ID == id {
			s.entries = append(s.entries[:i:i], s.entries[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	return nil
}

// HostAuthorities implements sshx.HostKeyMarkers: CA keys whose @cert-authority patterns match host:port.
func (s *markerStore) HostAuthorities(host string, port int) []ssh.PublicKey {
	names := markerNames(host, port)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []ssh.PublicKey
	for _, e := range s.entries {
		if e.m.Marker == markerCertAuthority && matchHostPatterns(e.m.Hosts, names...) {
			out = append(out, e.key)
		}
	}
	return out
}

// IsRevoked implements sshx.HostKeyMarkers: key is listed by a @revoked entry whose patterns match host:port.
func (s *markerStore) IsRevoked(host string, port int, key ssh.PublicKey) bool {
	if key == nil {
		return false
	}
	names := markerNames(host, port)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.entries {
		if e.m.Marker == markerRevoked && sameKey(e.key, key) && matchHostPatterns(e.m.Hosts, names...) {
			return true
		}
	}
	return false
}
