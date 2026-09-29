package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/nexterm/nexterm/internal/model"
)

// Users is the users repository.
type Users struct{ db *sql.DB }

// UserAuth carries a user's credential material (password hash, sealed TOTP secret, hashed recovery codes).
type UserAuth struct {
	User           *model.User
	PasswordHash   string
	TOTPSecretEnc  []byte
	RecoveryHashes []string
	TOTPLastStep   int64
}

const userCols = `id, username, display_name, role, totp_enabled, disabled, created_at, updated_at, last_login_at`

func scanUser(sc scanner) (*model.User, error) {
	var (
		u                model.User
		totp, disabled   int
		created, updated int64
		lastLogin        sql.NullInt64
		role             string
	)
	if err := sc.Scan(&u.ID, &u.Username, &u.DisplayName, &role, &totp, &disabled, &created, &updated, &lastLogin); err != nil {
		return nil, mapErr(err)
	}
	u.Role = model.Role(role)
	u.TOTPEnabled, u.Disabled = totp != 0, disabled != 0
	u.CreatedAt, u.UpdatedAt, u.LastLoginAt = fromMs(created), fromMs(updated), fromNullMs(lastLogin)
	return &u, nil
}

// Count returns the number of users.
func (r *Users) Count(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, mapErr(err)
}

// CountActiveAdmins returns the number of enabled admins.
func (r *Users) CountActiveAdmins(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0`).Scan(&n)
	return n, mapErr(err)
}

// Create inserts u with the given password hash. ID and timestamps are filled when empty. A duplicate username
// yields model.ErrConflict.
func (r *Users) Create(ctx context.Context, u *model.User, passwordHash string) error {
	return r.create(ctx, r.db, u, passwordHash)
}

// CreateFirstAdmin atomically creates u only if no user exists yet (initial setup). It returns model.ErrConflict when
// a user already exists.
func (r *Users) CreateFirstAdmin(ctx context.Context, u *model.User, passwordHash string) error {
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return model.ErrConflict
		}
		u.Role = model.RoleAdmin
		return r.create(ctx, tx, u, passwordHash)
	})
}

func (r *Users) create(ctx context.Context, q execer, u *model.User, passwordHash string) error {
	if u.ID == "" {
		u.ID = model.NewID()
	}
	now := Now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	if !u.Role.Valid() {
		u.Role = model.RoleUser
	}
	_, err := q.ExecContext(ctx, `INSERT INTO users (id, username, display_name, password_hash, role, disabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Username, u.DisplayName, passwordHash, string(u.Role), b2i(u.Disabled), ms(u.CreatedAt), ms(u.UpdatedAt))
	return mapErr(err)
}

// Get returns a user by ID.
func (r *Users) Get(ctx context.Context, id string) (*model.User, error) {
	return scanUser(r.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// GetByUsername returns a user by (case-insensitive) username.
func (r *Users) GetByUsername(ctx context.Context, username string) (*model.User, error) {
	return scanUser(r.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE username = ?`, username))
}

// FirstAdmin returns the oldest enabled admin (the desktop-mode owner).
func (r *Users) FirstAdmin(ctx context.Context) (*model.User, error) {
	return scanUser(r.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE role = 'admin' AND disabled = 0 ORDER BY created_at, id LIMIT 1`))
}

// List returns all users ordered by username.
func (r *Users) List(ctx context.Context) ([]*model.User, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY username COLLATE NOCASE`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*model.User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (r *Users) getAuth(ctx context.Context, where string, arg any) (*UserAuth, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+userCols+`, password_hash, totp_secret_enc, totp_recovery, totp_last_step
		FROM users WHERE `+where, arg)
	var (
		u                model.User
		totp, disabled   int
		created, updated int64
		lastLogin        sql.NullInt64
		role, hash, rec  string
		secret           []byte
		lastStep         int64
	)
	if err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &role, &totp, &disabled, &created, &updated, &lastLogin,
		&hash, &secret, &rec, &lastStep); err != nil {
		return nil, mapErr(err)
	}
	u.Role = model.Role(role)
	u.TOTPEnabled, u.Disabled = totp != 0, disabled != 0
	u.CreatedAt, u.UpdatedAt, u.LastLoginAt = fromMs(created), fromMs(updated), fromNullMs(lastLogin)
	return &UserAuth{User: &u, PasswordHash: hash, TOTPSecretEnc: secret, RecoveryHashes: parseStrings(rec), TOTPLastStep: lastStep}, nil
}

// GetAuthByUsername returns the user and credential material by username.
func (r *Users) GetAuthByUsername(ctx context.Context, username string) (*UserAuth, error) {
	return r.getAuth(ctx, "username = ?", username)
}

// GetAuth returns the user and credential material by ID.
func (r *Users) GetAuth(ctx context.Context, id string) (*UserAuth, error) {
	return r.getAuth(ctx, "id = ?", id)
}

// Update saves display name, role and disabled flag.
func (r *Users) Update(ctx context.Context, u *model.User) error {
	u.UpdatedAt = Now()
	return expectOne(r.db.ExecContext(ctx, `UPDATE users SET display_name = ?, role = ?, disabled = ?, updated_at = ? WHERE id = ?`,
		u.DisplayName, string(u.Role), b2i(u.Disabled), ms(u.UpdatedAt), u.ID))
}

// SetPassword replaces the password hash.
func (r *Users) SetPassword(ctx context.Context, id, hash string) error {
	return expectOne(r.db.ExecContext(ctx, `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, hash, ms(Now()), id))
}

// SetTOTP stores the sealed TOTP secret (nil clears it), the enabled flag and recovery code hashes, and resets the
// replay guard.
func (r *Users) SetTOTP(ctx context.Context, id string, secretEnc []byte, enabled bool, recoveryHashes []string) error {
	return expectOne(r.db.ExecContext(ctx, `UPDATE users SET totp_secret_enc = ?, totp_enabled = ?, totp_recovery = ?,
		totp_last_step = 0, updated_at = ? WHERE id = ?`, secretEnc, b2i(enabled), stringsJSON(recoveryHashes), ms(Now()), id))
}

// SetRecoveryCodes replaces the hashed recovery codes.
func (r *Users) SetRecoveryCodes(ctx context.Context, id string, hashes []string) error {
	return expectOne(r.db.ExecContext(ctx, `UPDATE users SET totp_recovery = ?, updated_at = ? WHERE id = ?`,
		stringsJSON(hashes), ms(Now()), id))
}

// AdvanceTOTPStep records the last accepted TOTP time step; it only succeeds when step is newer than the stored one
// (replay guard) and returns false otherwise.
func (r *Users) AdvanceTOTPStep(ctx context.Context, id string, step int64) (bool, error) {
	res, err := r.db.ExecContext(ctx, `UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_last_step < ?`, step, id, step)
	if err != nil {
		return false, mapErr(err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ConsumeRecoveryCode atomically removes hash from the user's recovery codes; it returns false if not present.
func (r *Users) ConsumeRecoveryCode(ctx context.Context, id, hash string) (bool, error) {
	ok := false
	err := withTx(ctx, r.db, func(tx *sql.Tx) error {
		var rec string
		if err := tx.QueryRowContext(ctx, `SELECT totp_recovery FROM users WHERE id = ?`, id).Scan(&rec); err != nil {
			return mapErr(err)
		}
		codes := parseStrings(rec)
		kept := codes[:0]
		for _, c := range codes {
			if c == hash && !ok {
				ok = true
				continue
			}
			kept = append(kept, c)
		}
		if !ok {
			return nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE users SET totp_recovery = ? WHERE id = ?`, stringsJSON(kept), id)
		return err
	})
	return ok, err
}

// TouchLogin records a successful login.
func (r *Users) TouchLogin(ctx context.Context, id string, t time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, ms(t), id)
	return mapErr(err)
}

// Delete removes a user (cascading to their sessions, tokens and owned rows) and their settings.
func (r *Users) Delete(ctx context.Context, id string) error {
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE scope = ?`, id); err != nil {
			return err
		}
		return expectOne(tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id))
	})
}

// ---- auth sessions ------------------------------------------------------------------------------------------------

// AuthSessions is the browser login session repository.
type AuthSessions struct{ db *sql.DB }

const authSessionCols = `id, user_id, created_at, expires_at, last_seen_at, ip, user_agent, remember`

func scanAuthSession(sc scanner) (*model.AuthSession, error) {
	var (
		s                      model.AuthSession
		created, expires, seen int64
		remember               int
	)
	if err := sc.Scan(&s.ID, &s.UserID, &created, &expires, &seen, &s.IP, &s.UserAgent, &remember); err != nil {
		return nil, mapErr(err)
	}
	s.CreatedAt, s.ExpiresAt, s.LastSeenAt, s.Remember = fromMs(created), fromMs(expires), fromMs(seen), remember != 0
	return &s, nil
}

// Create inserts a session (ID must be the token hash).
func (r *AuthSessions) Create(ctx context.Context, s *model.AuthSession) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO auth_sessions (`+authSessionCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.UserID, ms(s.CreatedAt), ms(s.ExpiresAt), ms(s.LastSeenAt), s.IP, s.UserAgent, b2i(s.Remember))
	return mapErr(err)
}

// Get returns a session by ID (token hash).
func (r *AuthSessions) Get(ctx context.Context, id string) (*model.AuthSession, error) {
	return scanAuthSession(r.db.QueryRowContext(ctx, `SELECT `+authSessionCols+` FROM auth_sessions WHERE id = ?`, id))
}

// ListByUser returns a user's non-expired sessions, most recently used first.
func (r *AuthSessions) ListByUser(ctx context.Context, userID string) ([]*model.AuthSession, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+authSessionCols+` FROM auth_sessions WHERE user_id = ? AND expires_at > ?
		ORDER BY last_seen_at DESC`, userID, ms(Now()))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*model.AuthSession{}
	for rows.Next() {
		s, err := scanAuthSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Touch slides a session's expiry.
func (r *AuthSessions) Touch(ctx context.Context, id string, lastSeen, expires time.Time, ip string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE auth_sessions SET last_seen_at = ?, expires_at = ?, ip = ? WHERE id = ?`,
		ms(lastSeen), ms(expires), ip, id)
	return mapErr(err)
}

// Delete removes a session.
func (r *AuthSessions) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE id = ?`, id)
	return mapErr(err)
}

// DeleteForUser removes a session only if it belongs to userID (model.ErrNotFound otherwise).
func (r *AuthSessions) DeleteForUser(ctx context.Context, userID, id string) error {
	return expectOne(r.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE id = ? AND user_id = ?`, id, userID))
}

// DeleteAllForUser removes all of a user's sessions except exceptID ("" = all).
func (r *AuthSessions) DeleteAllForUser(ctx context.Context, userID, exceptID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE user_id = ? AND id <> ?`, userID, exceptID)
	return mapErr(err)
}

// DeleteExpired purges expired sessions.
func (r *AuthSessions) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE expires_at <= ?`, ms(now))
	if err != nil {
		return 0, mapErr(err)
	}
	return res.RowsAffected()
}

// ---- API tokens ---------------------------------------------------------------------------------------------------

// APITokens is the personal access token repository.
type APITokens struct{ db *sql.DB }

const apiTokenCols = `id, user_id, name, token_hash, created_at, last_used_at, expires_at`

func scanAPIToken(sc scanner) (*model.APIToken, error) {
	var (
		t             model.APIToken
		created       int64
		used, expires sql.NullInt64
	)
	if err := sc.Scan(&t.ID, &t.UserID, &t.Name, &t.TokenHash, &created, &used, &expires); err != nil {
		return nil, mapErr(err)
	}
	t.CreatedAt, t.LastUsedAt, t.ExpiresAt = fromMs(created), fromNullMs(used), fromNullMs(expires)
	return &t, nil
}

// Create inserts a token (TokenHash must be set).
func (r *APITokens) Create(ctx context.Context, t *model.APIToken) error {
	if t.ID == "" {
		t.ID = model.NewID()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = Now()
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO api_tokens (`+apiTokenCols+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.UserID, t.Name, t.TokenHash, ms(t.CreatedAt), nullMs(t.LastUsedAt), nullMs(t.ExpiresAt))
	return mapErr(err)
}

// ListByUser returns a user's tokens, newest first.
func (r *APITokens) ListByUser(ctx context.Context, userID string) ([]*model.APIToken, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+apiTokenCols+` FROM api_tokens WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*model.APIToken{}
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetByHash looks a token up by its hash.
func (r *APITokens) GetByHash(ctx context.Context, hash string) (*model.APIToken, error) {
	return scanAPIToken(r.db.QueryRowContext(ctx, `SELECT `+apiTokenCols+` FROM api_tokens WHERE token_hash = ?`, hash))
}

// Touch records token usage.
func (r *APITokens) Touch(ctx context.Context, id string, t time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, ms(t), id)
	return mapErr(err)
}

// Delete removes a token owned by userID.
func (r *APITokens) Delete(ctx context.Context, userID, id string) error {
	return expectOne(r.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ? AND user_id = ?`, id, userID))
}

// DeleteAllForUser removes all of a user's tokens.
func (r *APITokens) DeleteAllForUser(ctx context.Context, userID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE user_id = ?`, userID)
	return mapErr(err)
}
