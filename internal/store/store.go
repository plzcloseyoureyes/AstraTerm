// Package store is AstraTerm's persistence layer: SQLite via modernc.org/sqlite (pure Go), a per-module migration
// registry, and typed repositories for every core table (SPEC §5.1). Secrets are stored as opaque ciphertext
// ([]byte) produced by the vault; the store never sees plaintext secrets.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/model"

	_ "modernc.org/sqlite" // registers driver "sqlite"
)

// Store bundles the database handle and all repositories.
type Store struct {
	DB *sql.DB

	Users        *Users
	AuthSessions *AuthSessions
	APITokens    *APITokens
	Folders      *Folders
	Connections  *Connections
	Identities   *Identities
	Keys         *Keys
	KnownHosts   *KnownHosts
	Snippets     *Snippets
	Macros       *Macros
	Tunnels      *Tunnels
	Settings     *Settings
	Audit        *Audit
	Recordings   *Recordings
	ShareLinks   *ShareLinks
	VaultMeta    *VaultMeta

	migMu sync.Mutex
}

// Open opens (creating if needed) the SQLite database at path, configures WAL / busy timeout / foreign keys and
// applies all registered migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	if strings.ContainsRune(path, '?') {
		return nil, fmt.Errorf("store: database path %q must not contain '?'", path)
	}
	// Create the file owner-only (SQLite gives the -wal/-shm files the same mode); tighten an existing one.
	if f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600); err == nil {
		f.Close()
		_ = os.Chmod(path, 0o600)
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	s := New(db)
	if err := s.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenConfig opens the database inside the configured data directory.
func OpenConfig(ctx context.Context, cfg *config.Config) (*Store, error) {
	return Open(ctx, cfg.DBPath())
}

// New wraps an existing *sql.DB (no migrations are run).
func New(db *sql.DB) *Store {
	return &Store{
		DB:           db,
		Users:        &Users{db: db},
		AuthSessions: &AuthSessions{db: db},
		APITokens:    &APITokens{db: db},
		Folders:      &Folders{db: db},
		Connections:  &Connections{db: db},
		Identities:   &Identities{db: db},
		Keys:         &Keys{db: db},
		KnownHosts:   &KnownHosts{db: db},
		Snippets:     &Snippets{db: db},
		Macros:       &Macros{db: db},
		Tunnels:      &Tunnels{db: db},
		Settings:     &Settings{db: db},
		Audit:        &Audit{db: db},
		Recordings:   &Recordings{db: db},
		ShareLinks:   &ShareLinks{db: db},
		VaultMeta:    &VaultMeta{db: db},
	}
}

// Close closes the database. database/sql closes a connection that is still running a query only once that query
// finishes, after DB.Close has returned; Close waits (≤ 5 s) for those too, so the SQLite files (WAL checkpoint,
// -wal / -shm removal) are final when it returns — before a process exit or a test removing the data directory.
func (s *Store) Close() error {
	err := s.DB.Close()
	for deadline := time.Now().Add(5 * time.Second); s.DB.Stats().OpenConnections > 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	return err
}

// Tx runs fn inside a write transaction (BEGIN IMMEDIATE), committing on nil error and rolling back otherwise.
func (s *Store) Tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return withTx(ctx, s.DB, fn)
}

func withTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return mapErr(err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return mapErr(tx.Commit())
}

// ---- migrations ---------------------------------------------------------------------------------------------------

type migrationKey struct {
	module  string
	version int
}

var (
	regMu    sync.Mutex
	registry = map[migrationKey]string{}
	regErrs  []error
)

// RegisterMigration registers a schema migration for module at version. Migrations are applied by Store.Migrate in
// (module, version) order — module "core" always first — and tracked in schema_migrations. Registering the same
// (module, version) twice with different SQL is a programming error reported by Migrate.
//
// Modules usually call this from init() or at the start of Mount; a module that needs its tables during Mount should
// call d.Store.Migrate(ctx) right after registering (the server also runs Migrate after all modules are mounted).
func RegisterMigration(module string, version int, sql string) {
	regMu.Lock()
	defer regMu.Unlock()
	k := migrationKey{module, version}
	if prev, ok := registry[k]; ok && prev != sql {
		regErrs = append(regErrs, fmt.Errorf("store: conflicting migration %s/%d registered twice", module, version))
		return
	}
	registry[k] = sql
}

// Migrate applies every registered migration that has not been applied yet. It is safe to call repeatedly.
func (s *Store) Migrate(ctx context.Context) error {
	s.migMu.Lock()
	defer s.migMu.Unlock()

	regMu.Lock()
	if len(regErrs) > 0 {
		err := errors.Join(regErrs...)
		regMu.Unlock()
		return err
	}
	keys := make([]migrationKey, 0, len(registry))
	for k := range registry {
		keys = append(keys, k)
	}
	pending := make(map[migrationKey]string, len(registry))
	maps.Copy(pending, registry)
	regMu.Unlock()

	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if (a.module == "core") != (b.module == "core") {
			return a.module == "core"
		}
		if a.module != b.module {
			return a.module < b.module
		}
		return a.version < b.version
	})

	if _, err := s.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		module TEXT NOT NULL, version INTEGER NOT NULL, applied_at INTEGER NOT NULL, PRIMARY KEY (module, version))`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	applied := map[migrationKey]bool{}
	rows, err := s.DB.QueryContext(ctx, `SELECT module, version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("store: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var k migrationKey
		if err := rows.Scan(&k.module, &k.version); err != nil {
			rows.Close()
			return err
		}
		applied[k] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, k := range keys {
		if applied[k] {
			continue
		}
		stmt := pending[k]
		err := withTx(ctx, s.DB, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (module, version, applied_at) VALUES (?, ?, ?)`,
				k.module, k.version, time.Now().UnixMilli())
			return err
		})
		if err != nil {
			return fmt.Errorf("store: migration %s/%d: %w", k.module, k.version, err)
		}
	}
	return nil
}

// ---- helpers ------------------------------------------------------------------------------------------------------

type scanner interface{ Scan(dest ...any) error }

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Now returns the current time truncated to the storage precision (milliseconds, UTC).
func Now() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

func ms(t time.Time) int64 { return t.UnixMilli() }

// NullMs stores an optional time as unix milliseconds (NULL when nil or zero).
func NullMs(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// FromMs reads a unix-millisecond column.
func FromMs(v int64) time.Time { return time.UnixMilli(v).UTC() }

// FromNullMs reads an optional unix-millisecond column.
func FromNullMs(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := FromMs(v.Int64)
	return &t
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// B2I stores a bool as 0 / 1.
func B2I(b bool) int {
	if b {
		return 1
	}
	return 0
}

func jsonText(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("store: encode json: %w", err)
	}
	return string(b), nil
}

func stringsJSON(v []string) string {
	if v == nil {
		v = []string{}
	}
	s, _ := jsonText(v)
	return s
}

func parseStrings(s string) []string {
	out := []string{}
	if s != "" {
		_ = json.Unmarshal([]byte(s), &out)
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// mapErr translates driver errors into model sentinel errors.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return model.ErrNotFound
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed"):
		return fmt.Errorf("%w: already exists", model.ErrConflict)
	case strings.Contains(msg, "FOREIGN KEY constraint failed"):
		return fmt.Errorf("%w: referenced item does not exist", model.ErrConflict)
	}
	return err
}

func expectOne(res sql.Result, err error) error {
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return model.ErrNotFound
	}
	return nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func anyArgs(ids []string) []any {
	out := make([]any, len(ids))
	for i, s := range ids {
		out[i] = s
	}
	return out
}
