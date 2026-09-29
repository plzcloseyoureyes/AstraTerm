package auth

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
)

// Module table (migration auth/1): per-user security metadata the core users table has no columns for — the
// account-lockout counter (SEC-20), whether the account has a usable local password (SSO-provisioned accounts do not)
// and when it was last changed.
func init() {
	store.RegisterMigration("auth", 1, `
CREATE TABLE auth_user_meta (
	user_id             TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	failed_logins       INTEGER NOT NULL DEFAULT 0,
	locked_until        INTEGER NOT NULL DEFAULT 0,
	password_set        INTEGER NOT NULL DEFAULT 1,
	password_changed_at INTEGER NOT NULL DEFAULT 0
);`)
}

// services maps each application instance (tests run several in one process) to its auth service, so companion
// modules (webauthn, oidc) mounted later can reach it with ServiceFor.
var services sync.Map // *app.Deps → *Service

// ServiceFor returns the auth service mounted for d (nil when auth was not mounted).
func ServiceFor(d *app.Deps) *Service {
	if v, ok := services.Load(d); ok {
		return v.(*Service)
	}
	return nil
}

// userMeta is a row of auth_user_meta (defaults when the user has none).
type userMeta struct {
	FailedLogins      int
	LockedUntil       time.Time
	PasswordSet       bool
	PasswordChangedAt time.Time
}

func (m userMeta) locked(now time.Time) bool {
	return !m.LockedUntil.IsZero() && now.Before(m.LockedUntil)
}

func msTime(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

func (s *Service) userMeta(ctx context.Context, userID string) (userMeta, error) {
	var failed int
	var until, changed int64
	var set int
	err := s.d.Store.DB.QueryRowContext(ctx, `SELECT failed_logins, locked_until, password_set, password_changed_at
		FROM auth_user_meta WHERE user_id = ?`, userID).Scan(&failed, &until, &set, &changed)
	if errors.Is(err, sql.ErrNoRows) {
		return userMeta{PasswordSet: true}, nil
	}
	if err != nil {
		return userMeta{PasswordSet: true}, err
	}
	return userMeta{FailedLogins: failed, LockedUntil: msTime(until), PasswordSet: set != 0, PasswordChangedAt: msTime(changed)}, nil
}

func (s *Service) allUserMeta(ctx context.Context) (map[string]userMeta, error) {
	rows, err := s.d.Store.DB.QueryContext(ctx, `SELECT user_id, failed_logins, locked_until, password_set, password_changed_at FROM auth_user_meta`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]userMeta{}
	for rows.Next() {
		var id string
		var failed, set int
		var until, changed int64
		if err := rows.Scan(&id, &failed, &until, &set, &changed); err != nil {
			return nil, err
		}
		out[id] = userMeta{FailedLogins: failed, LockedUntil: msTime(until), PasswordSet: set != 0, PasswordChangedAt: msTime(changed)}
	}
	return out, rows.Err()
}

// recordLoginFailure counts a failed login of userID; once threshold (> 0) consecutive failures are reached the
// account is locked for lockFor and the counter restarts. It reports the lock end when this failure locked it.
func (s *Service) recordLoginFailure(ctx context.Context, userID string, threshold int, lockFor time.Duration) (time.Time, error) {
	var lockedUntil time.Time
	err := s.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_user_meta (user_id, failed_logins) VALUES (?, 1)
			ON CONFLICT(user_id) DO UPDATE SET failed_logins = failed_logins + 1`, userID); err != nil {
			return err
		}
		if threshold <= 0 {
			return nil
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT failed_logins FROM auth_user_meta WHERE user_id = ?`, userID).Scan(&n); err != nil {
			return err
		}
		if n < threshold {
			return nil
		}
		lockedUntil = store.Now().Add(lockFor)
		_, err := tx.ExecContext(ctx, `UPDATE auth_user_meta SET failed_logins = 0, locked_until = ? WHERE user_id = ?`,
			lockedUntil.UnixMilli(), userID)
		return err
	})
	return lockedUntil, err
}

// clearLoginFailures resets the failure counter (successful login); unlock also lifts an active lock.
func (s *Service) clearLoginFailures(ctx context.Context, userID string, unlock bool) error {
	q := `UPDATE auth_user_meta SET failed_logins = 0 WHERE user_id = ?`
	if unlock {
		q = `UPDATE auth_user_meta SET failed_logins = 0, locked_until = 0 WHERE user_id = ?`
	}
	_, err := s.d.Store.DB.ExecContext(ctx, q, userID)
	return err
}

// setPasswordState records whether userID has a usable local password (and stamps the change time).
func (s *Service) setPasswordState(ctx context.Context, userID string, set bool) error {
	now := store.Now().UnixMilli()
	_, err := s.d.Store.DB.ExecContext(ctx, `INSERT INTO auth_user_meta (user_id, password_set, password_changed_at) VALUES (?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET password_set = excluded.password_set, password_changed_at = excluded.password_changed_at`,
		userID, store.B2I(set), now)
	return err
}
