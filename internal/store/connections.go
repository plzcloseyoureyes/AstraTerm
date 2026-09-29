package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/termstead/termstead/internal/model"
)

// ---- folders ------------------------------------------------------------------------------------------------------

// Folders is the connection-tree folder repository.
type Folders struct{ db *sql.DB }

const folderCols = `id, owner_id, parent_id, name, color, icon, sort_order, shared, created_at, updated_at`

func scanFolder(sc scanner) (*model.Folder, error) {
	var (
		f                model.Folder
		parent           sql.NullString
		shared           int
		created, updated int64
	)
	if err := sc.Scan(&f.ID, &f.OwnerID, &parent, &f.Name, &f.Color, &f.Icon, &f.SortOrder, &shared, &created, &updated); err != nil {
		return nil, mapErr(err)
	}
	f.ParentID, f.Shared = parent.String, shared != 0
	f.CreatedAt, f.UpdatedAt = fromMs(created), fromMs(updated)
	return &f, nil
}

func collectFolders(rows *sql.Rows, err error) ([]*model.Folder, error) {
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*model.Folder{}
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListVisible returns folders owned by userID or shared.
func (r *Folders) ListVisible(ctx context.Context, userID string) ([]*model.Folder, error) {
	return collectFolders(r.db.QueryContext(ctx, `SELECT `+folderCols+` FROM folders WHERE owner_id = ? OR shared = 1
		ORDER BY sort_order, name COLLATE NOCASE`, userID))
}

// ListByOwner returns folders owned by ownerID.
func (r *Folders) ListByOwner(ctx context.Context, ownerID string) ([]*model.Folder, error) {
	return collectFolders(r.db.QueryContext(ctx, `SELECT `+folderCols+` FROM folders WHERE owner_id = ?
		ORDER BY sort_order, name COLLATE NOCASE`, ownerID))
}

// Get returns a folder by ID.
func (r *Folders) Get(ctx context.Context, id string) (*model.Folder, error) {
	return scanFolder(r.db.QueryRowContext(ctx, `SELECT `+folderCols+` FROM folders WHERE id = ?`, id))
}

// Create inserts f (ID/timestamps filled when empty).
func (r *Folders) Create(ctx context.Context, f *model.Folder) error {
	if f.ID == "" {
		f.ID = model.NewID()
	}
	now := Now()
	f.CreatedAt, f.UpdatedAt = now, now
	_, err := r.db.ExecContext(ctx, `INSERT INTO folders (`+folderCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ID, f.OwnerID, nullStr(f.ParentID), f.Name, f.Color, f.Icon, f.SortOrder, b2i(f.Shared), ms(now), ms(now))
	return mapErr(err)
}

// Update saves all mutable fields of f.
func (r *Folders) Update(ctx context.Context, f *model.Folder) error {
	f.UpdatedAt = Now()
	return expectOne(r.db.ExecContext(ctx, `UPDATE folders SET parent_id = ?, name = ?, color = ?, icon = ?, sort_order = ?,
		shared = ?, updated_at = ? WHERE id = ?`,
		nullStr(f.ParentID), f.Name, f.Color, f.Icon, f.SortOrder, b2i(f.Shared), ms(f.UpdatedAt), f.ID))
}

const subtreeCTE = `WITH RECURSIVE sub(id) AS (SELECT ? UNION SELECT f.id FROM folders f JOIN sub ON f.parent_id = sub.id)`

// IsInSubtree reports whether candidateID is rootID or one of its descendants (cycle check for re-parenting).
func (r *Folders) IsInSubtree(ctx context.Context, rootID, candidateID string) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx, subtreeCTE+` SELECT COUNT(*) FROM sub WHERE id = ?`, rootID, candidateID).Scan(&n)
	return n > 0, mapErr(err)
}

// ForeignItemsInSubtree counts folders and connections inside the subtree of rootID not owned by ownerID.
func (r *Folders) ForeignItemsInSubtree(ctx context.Context, rootID, ownerID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, subtreeCTE+` SELECT
		(SELECT COUNT(*) FROM folders WHERE id IN (SELECT id FROM sub) AND owner_id <> ?) +
		(SELECT COUNT(*) FROM connections WHERE folder_id IN (SELECT id FROM sub) AND owner_id <> ?)`,
		rootID, ownerID, ownerID).Scan(&n)
	return n, mapErr(err)
}

// Delete removes a folder. When recursive, the whole subtree and every connection in it are deleted; otherwise its
// child folders and connections move to the folder's parent. It returns the IDs of deleted connections.
func (r *Folders) Delete(ctx context.Context, id string, recursive bool) (deletedConns []string, err error) {
	err = withTx(ctx, r.db, func(tx *sql.Tx) error {
		var parent sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT parent_id FROM folders WHERE id = ?`, id).Scan(&parent); err != nil {
			return mapErr(err)
		}
		now := ms(Now())
		if !recursive {
			if _, err := tx.ExecContext(ctx, `UPDATE folders SET parent_id = ?, updated_at = ? WHERE parent_id = ?`, parent, now, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE connections SET folder_id = ?, updated_at = ? WHERE folder_id = ?`, parent, now, id); err != nil {
				return err
			}
			return expectOne(tx.ExecContext(ctx, `DELETE FROM folders WHERE id = ?`, id))
		}
		rows, err := tx.QueryContext(ctx, subtreeCTE+` SELECT id FROM connections WHERE folder_id IN (SELECT id FROM sub)`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var cid string
			if err := rows.Scan(&cid); err != nil {
				rows.Close()
				return err
			}
			deletedConns = append(deletedConns, cid)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, subtreeCTE+` DELETE FROM connections WHERE folder_id IN (SELECT id FROM sub)`, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, subtreeCTE+` DELETE FROM folders WHERE id IN (SELECT id FROM sub)`, id)
		return err
	})
	return deletedConns, err
}

// ---- connections --------------------------------------------------------------------------------------------------

// Connections is the saved-connection repository.
type Connections struct{ db *sql.DB }

const connCols = `id, owner_id, folder_id, name, protocol, host, port, username, identity_id, key_id, auth_method, color, icon,
	tags, notes, favorite, sort_order, options, secrets_enc, secret_keys, shared, last_used_at, created_at, updated_at`

func scanConnection(sc scanner) (*model.Connection, error) {
	var (
		c                          model.Connection
		folder, identity, key      sql.NullString
		protocol, tags, opts, keys string
		favorite, shared           int
		lastUsed                   sql.NullInt64
		created, updated           int64
	)
	if err := sc.Scan(&c.ID, &c.OwnerID, &folder, &c.Name, &protocol, &c.Host, &c.Port, &c.Username, &identity, &key,
		&c.AuthMethod, &c.Color, &c.Icon, &tags, &c.Notes, &favorite, &c.SortOrder, &opts, &c.SecretsEnc, &keys, &shared,
		&lastUsed, &created, &updated); err != nil {
		return nil, mapErr(err)
	}
	c.Protocol = model.Protocol(protocol)
	c.FolderID, c.IdentityID, c.KeyID = folder.String, identity.String, key.String
	c.Tags, c.SecretKeys = parseStrings(tags), parseStrings(keys)
	c.Options = model.Options{}
	if opts != "" {
		_ = json.Unmarshal([]byte(opts), &c.Options)
		if c.Options == nil {
			c.Options = model.Options{}
		}
	}
	if len(c.SecretsEnc) == 0 {
		c.SecretsEnc = nil
	}
	c.Favorite, c.Shared = favorite != 0, shared != 0
	c.LastUsedAt, c.CreatedAt, c.UpdatedAt = fromNullMs(lastUsed), fromMs(created), fromMs(updated)
	c.Normalize()
	return &c, nil
}

func collectConnections(rows *sql.Rows, err error) ([]*model.Connection, error) {
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*model.Connection{}
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListVisible returns the connections owned by userID or shared.
func (r *Connections) ListVisible(ctx context.Context, userID string) ([]*model.Connection, error) {
	return collectConnections(r.db.QueryContext(ctx, `SELECT `+connCols+` FROM connections WHERE owner_id = ? OR shared = 1
		ORDER BY sort_order, name COLLATE NOCASE`, userID))
}

// ListByOwner returns the connections owned by ownerID.
func (r *Connections) ListByOwner(ctx context.Context, ownerID string) ([]*model.Connection, error) {
	return collectConnections(r.db.QueryContext(ctx, `SELECT `+connCols+` FROM connections WHERE owner_id = ?
		ORDER BY sort_order, name COLLATE NOCASE`, ownerID))
}

// Get returns a connection by ID (no visibility check).
func (r *Connections) Get(ctx context.Context, id string) (*model.Connection, error) {
	return scanConnection(r.db.QueryRowContext(ctx, `SELECT `+connCols+` FROM connections WHERE id = ?`, id))
}

func connArgs(c *model.Connection) ([]any, error) {
	c.Normalize()
	opts, err := jsonText(c.Options)
	if err != nil {
		return nil, err
	}
	var enc any
	if len(c.SecretsEnc) > 0 {
		enc = c.SecretsEnc
	}
	return []any{c.ID, c.OwnerID, nullStr(c.FolderID), c.Name, string(c.Protocol), c.Host, c.Port, c.Username,
		nullStr(c.IdentityID), nullStr(c.KeyID), c.AuthMethod, c.Color, c.Icon, stringsJSON(c.Tags), c.Notes,
		b2i(c.Favorite), c.SortOrder, opts, enc, stringsJSON(c.SecretKeys), b2i(c.Shared), nullMs(c.LastUsedAt),
		ms(c.CreatedAt), ms(c.UpdatedAt)}, nil
}

// Create inserts c (ID/timestamps filled when empty). c.Secrets is ignored — set SecretsEnc/SecretKeys instead.
func (r *Connections) Create(ctx context.Context, c *model.Connection) error {
	if c.ID == "" {
		c.ID = model.NewID()
	}
	now := Now()
	c.CreatedAt, c.UpdatedAt = now, now
	args, err := connArgs(c)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO connections (`+connCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, args...)
	return mapErr(err)
}

// Update saves every mutable column of c (owner and creation time are immutable).
func (r *Connections) Update(ctx context.Context, c *model.Connection) error {
	c.UpdatedAt = Now()
	c.Normalize()
	opts, err := jsonText(c.Options)
	if err != nil {
		return err
	}
	var enc any
	if len(c.SecretsEnc) > 0 {
		enc = c.SecretsEnc
	}
	return expectOne(r.db.ExecContext(ctx, `UPDATE connections SET folder_id = ?, name = ?, protocol = ?, host = ?, port = ?,
		username = ?, identity_id = ?, key_id = ?, auth_method = ?, color = ?, icon = ?, tags = ?, notes = ?, favorite = ?,
		sort_order = ?, options = ?, secrets_enc = ?, secret_keys = ?, shared = ?, updated_at = ? WHERE id = ?`,
		nullStr(c.FolderID), c.Name, string(c.Protocol), c.Host, c.Port, c.Username, nullStr(c.IdentityID),
		nullStr(c.KeyID), c.AuthMethod, c.Color, c.Icon, stringsJSON(c.Tags), c.Notes, b2i(c.Favorite), c.SortOrder,
		opts, enc, stringsJSON(c.SecretKeys), b2i(c.Shared), ms(c.UpdatedAt), c.ID))
}

// TouchUsed records that a connection was opened.
func (r *Connections) TouchUsed(ctx context.Context, id string, t time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE connections SET last_used_at = ? WHERE id = ?`, ms(t), id)
	return mapErr(err)
}

// Delete removes a connection.
func (r *Connections) Delete(ctx context.Context, id string) error {
	return expectOne(r.db.ExecContext(ctx, `DELETE FROM connections WHERE id = ?`, id))
}

// DeleteMany removes the given connections in one transaction and returns how many were deleted.
func (r *Connections) DeleteMany(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var n int64
	err := withTx(ctx, r.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM connections WHERE id IN (`+placeholders(len(ids))+`)`, anyArgs(ids)...)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return int(n), mapErr(err)
}

// ReorderItem moves a connection to FolderID ("" = root) at SortOrder.
type ReorderItem struct {
	ID        string
	FolderID  string
	SortOrder int
}

// Reorder applies all items atomically.
func (r *Connections) Reorder(ctx context.Context, items []ReorderItem) error {
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		now := ms(Now())
		for _, it := range items {
			if err := expectOne(tx.ExecContext(ctx, `UPDATE connections SET folder_id = ?, sort_order = ?, updated_at = ? WHERE id = ?`,
				nullStr(it.FolderID), it.SortOrder, now, it.ID)); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- identities ---------------------------------------------------------------------------------------------------

// Identities is the reusable-credential repository.
type Identities struct{ db *sql.DB }

const identityCols = `id, owner_id, name, username, key_id, secrets_enc, secret_keys, created_at, updated_at`

func scanIdentity(sc scanner) (*model.Identity, error) {
	var (
		i                model.Identity
		key              sql.NullString
		keys             string
		created, updated int64
	)
	if err := sc.Scan(&i.ID, &i.OwnerID, &i.Name, &i.Username, &key, &i.SecretsEnc, &keys, &created, &updated); err != nil {
		return nil, mapErr(err)
	}
	i.KeyID, i.SecretKeys = key.String, parseStrings(keys)
	if len(i.SecretsEnc) == 0 {
		i.SecretsEnc = nil
	}
	i.CreatedAt, i.UpdatedAt = fromMs(created), fromMs(updated)
	return &i, nil
}

// ListByOwner returns a user's identities ordered by name.
func (r *Identities) ListByOwner(ctx context.Context, ownerID string) ([]*model.Identity, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+identityCols+` FROM identities WHERE owner_id = ? ORDER BY name COLLATE NOCASE`, ownerID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*model.Identity{}
	for rows.Next() {
		i, err := scanIdentity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// Get returns an identity by ID (no ownership check).
func (r *Identities) Get(ctx context.Context, id string) (*model.Identity, error) {
	return scanIdentity(r.db.QueryRowContext(ctx, `SELECT `+identityCols+` FROM identities WHERE id = ?`, id))
}

func encArg(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// Create inserts i.
func (r *Identities) Create(ctx context.Context, i *model.Identity) error {
	if i.ID == "" {
		i.ID = model.NewID()
	}
	now := Now()
	i.CreatedAt, i.UpdatedAt = now, now
	i.Normalize()
	_, err := r.db.ExecContext(ctx, `INSERT INTO identities (`+identityCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		i.ID, i.OwnerID, i.Name, i.Username, nullStr(i.KeyID), encArg(i.SecretsEnc), stringsJSON(i.SecretKeys), ms(now), ms(now))
	return mapErr(err)
}

// Update saves the mutable fields of i.
func (r *Identities) Update(ctx context.Context, i *model.Identity) error {
	i.UpdatedAt = Now()
	i.Normalize()
	return expectOne(r.db.ExecContext(ctx, `UPDATE identities SET name = ?, username = ?, key_id = ?, secrets_enc = ?,
		secret_keys = ?, updated_at = ? WHERE id = ?`,
		i.Name, i.Username, nullStr(i.KeyID), encArg(i.SecretsEnc), stringsJSON(i.SecretKeys), ms(i.UpdatedAt), i.ID))
}

// Delete removes an identity (connections referencing it are unlinked).
func (r *Identities) Delete(ctx context.Context, id string) error {
	return expectOne(r.db.ExecContext(ctx, `DELETE FROM identities WHERE id = ?`, id))
}
