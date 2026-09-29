package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/nexterm/nexterm/internal/model"
)

// ---- SSH keys -----------------------------------------------------------------------------------------------------

// Keys is the stored SSH key repository.
type Keys struct{ db *sql.DB }

const keyCols = `id, owner_id, name, type, bits, public_key, private_key_enc, passphrase_enc, has_passphrase, fingerprint,
	comment, certificate, created_at`

func scanKey(sc scanner) (*model.SSHKey, error) {
	var (
		k       model.SSHKey
		hasPass int
		created int64
	)
	if err := sc.Scan(&k.ID, &k.OwnerID, &k.Name, &k.Type, &k.Bits, &k.PublicKey, &k.PrivateKeyEnc, &k.PassphraseEnc,
		&hasPass, &k.Fingerprint, &k.Comment, &k.Certificate, &created); err != nil {
		return nil, mapErr(err)
	}
	k.HasPassphrase, k.CreatedAt = hasPass != 0, fromMs(created)
	return &k, nil
}

// ListByOwner returns a user's keys ordered by name.
func (r *Keys) ListByOwner(ctx context.Context, ownerID string) ([]*model.SSHKey, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+keyCols+` FROM ssh_keys WHERE owner_id = ? ORDER BY name COLLATE NOCASE`, ownerID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*model.SSHKey{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Get returns a key by ID (no ownership check).
func (r *Keys) Get(ctx context.Context, id string) (*model.SSHKey, error) {
	return scanKey(r.db.QueryRowContext(ctx, `SELECT `+keyCols+` FROM ssh_keys WHERE id = ?`, id))
}

// Create inserts k (PrivateKeyEnc / PassphraseEnc must already be sealed).
func (r *Keys) Create(ctx context.Context, k *model.SSHKey) error {
	if k.ID == "" {
		k.ID = model.NewID()
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = Now()
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO ssh_keys (`+keyCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.OwnerID, k.Name, k.Type, k.Bits, k.PublicKey, encArg(k.PrivateKeyEnc), encArg(k.PassphraseEnc),
		b2i(k.HasPassphrase), k.Fingerprint, k.Comment, k.Certificate, ms(k.CreatedAt))
	return mapErr(err)
}

// Update saves the mutable fields of k (name, comment, certificate, sealed material, passphrase flag).
func (r *Keys) Update(ctx context.Context, k *model.SSHKey) error {
	return expectOne(r.db.ExecContext(ctx, `UPDATE ssh_keys SET name = ?, comment = ?, certificate = ?, private_key_enc = ?,
		passphrase_enc = ?, has_passphrase = ?, public_key = ?, fingerprint = ?, type = ?, bits = ? WHERE id = ?`,
		k.Name, k.Comment, k.Certificate, encArg(k.PrivateKeyEnc), encArg(k.PassphraseEnc), b2i(k.HasPassphrase),
		k.PublicKey, k.Fingerprint, k.Type, k.Bits, k.ID))
}

// Delete removes a key (connections / identities referencing it are unlinked).
func (r *Keys) Delete(ctx context.Context, id string) error {
	return expectOne(r.db.ExecContext(ctx, `DELETE FROM ssh_keys WHERE id = ?`, id))
}

// UsedBySharedConnection reports whether keyID is used (directly or through its owner's identity) by a shared
// connection of the key's owner — i.e. other users may legitimately use it server-side.
func (r *Keys) UsedBySharedConnection(ctx context.Context, keyID string) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM connections c
		JOIN ssh_keys k ON k.id = ?
		LEFT JOIN identities i ON i.id = c.identity_id AND i.owner_id = c.owner_id
		WHERE c.shared = 1 AND c.owner_id = k.owner_id AND (c.key_id = k.id OR (c.key_id IS NULL AND i.key_id = k.id))`,
		keyID).Scan(&n)
	return n > 0, mapErr(err)
}

// ---- known hosts --------------------------------------------------------------------------------------------------

// KnownHosts is the global trusted host key repository.
type KnownHosts struct{ db *sql.DB }

const knownHostCols = `id, host, port, key_type, public_key, fingerprint, comment, created_at`

func scanKnownHost(sc scanner) (*model.KnownHost, error) {
	var (
		h       model.KnownHost
		created int64
	)
	if err := sc.Scan(&h.ID, &h.Host, &h.Port, &h.KeyType, &h.PublicKey, &h.Fingerprint, &h.Comment, &created); err != nil {
		return nil, mapErr(err)
	}
	h.CreatedAt = fromMs(created)
	return &h, nil
}

func collectKnownHosts(rows *sql.Rows, err error) ([]*model.KnownHost, error) {
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*model.KnownHost{}
	for rows.Next() {
		h, err := scanKnownHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// List returns all known hosts.
func (r *KnownHosts) List(ctx context.Context) ([]*model.KnownHost, error) {
	return collectKnownHosts(r.db.QueryContext(ctx, `SELECT `+knownHostCols+` FROM known_hosts ORDER BY host, port, key_type`))
}

// Find returns the known keys of host:port.
func (r *KnownHosts) Find(ctx context.Context, host string, port int) ([]*model.KnownHost, error) {
	return collectKnownHosts(r.db.QueryContext(ctx, `SELECT `+knownHostCols+` FROM known_hosts WHERE host = ? COLLATE NOCASE AND port = ?
		ORDER BY key_type`, host, port))
}

// Get returns a known host by ID.
func (r *KnownHosts) Get(ctx context.Context, id string) (*model.KnownHost, error) {
	return scanKnownHost(r.db.QueryRowContext(ctx, `SELECT `+knownHostCols+` FROM known_hosts WHERE id = ?`, id))
}

// Add inserts h.
func (r *KnownHosts) Add(ctx context.Context, h *model.KnownHost) error { return r.add(ctx, r.db, h) }

func (r *KnownHosts) add(ctx context.Context, q execer, h *model.KnownHost) error {
	if h.ID == "" {
		h.ID = model.NewID()
	}
	if h.CreatedAt.IsZero() {
		h.CreatedAt = Now()
	}
	_, err := q.ExecContext(ctx, `INSERT INTO known_hosts (`+knownHostCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		h.ID, h.Host, h.Port, h.KeyType, h.PublicKey, h.Fingerprint, h.Comment, ms(h.CreatedAt))
	return mapErr(err)
}

// Replace atomically removes existing keys of the same host, port and key type, then inserts h (used when the user
// accepts a changed host key).
func (r *KnownHosts) Replace(ctx context.Context, h *model.KnownHost) error {
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM known_hosts WHERE host = ? COLLATE NOCASE AND port = ? AND key_type = ?`,
			h.Host, h.Port, h.KeyType); err != nil {
			return err
		}
		return r.add(ctx, tx, h)
	})
}

// Delete removes a known host by ID.
func (r *KnownHosts) Delete(ctx context.Context, id string) error {
	return expectOne(r.db.ExecContext(ctx, `DELETE FROM known_hosts WHERE id = ?`, id))
}

// DeleteHost removes every key of host:port.
func (r *KnownHosts) DeleteHost(ctx context.Context, host string, port int) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM known_hosts WHERE host = ? COLLATE NOCASE AND port = ?`, host, port)
	return mapErr(err)
}

// ---- snippets -----------------------------------------------------------------------------------------------------

// Snippets is the snippet repository.
type Snippets struct{ db *sql.DB }

const snippetCols = `id, owner_id, name, folder, description, content, tags, send_mode, shortcut, created_at, updated_at`

func scanSnippet(sc scanner) (*model.Snippet, error) {
	var (
		s                model.Snippet
		tags             string
		created, updated int64
	)
	if err := sc.Scan(&s.ID, &s.OwnerID, &s.Name, &s.Folder, &s.Description, &s.Content, &tags, &s.SendMode, &s.Shortcut,
		&created, &updated); err != nil {
		return nil, mapErr(err)
	}
	s.Tags, s.CreatedAt, s.UpdatedAt = parseStrings(tags), fromMs(created), fromMs(updated)
	return &s, nil
}

// ListByOwner returns a user's snippets.
func (r *Snippets) ListByOwner(ctx context.Context, ownerID string) ([]*model.Snippet, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+snippetCols+` FROM snippets WHERE owner_id = ? ORDER BY folder, name COLLATE NOCASE`, ownerID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*model.Snippet{}
	for rows.Next() {
		s, err := scanSnippet(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Get returns a snippet by ID.
func (r *Snippets) Get(ctx context.Context, id string) (*model.Snippet, error) {
	return scanSnippet(r.db.QueryRowContext(ctx, `SELECT `+snippetCols+` FROM snippets WHERE id = ?`, id))
}

// Create inserts s.
func (r *Snippets) Create(ctx context.Context, s *model.Snippet) error {
	if s.ID == "" {
		s.ID = model.NewID()
	}
	if s.SendMode == "" {
		s.SendMode = model.SendModePaste
	}
	now := Now()
	s.CreatedAt, s.UpdatedAt = now, now
	_, err := r.db.ExecContext(ctx, `INSERT INTO snippets (`+snippetCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.OwnerID, s.Name, s.Folder, s.Description, s.Content, stringsJSON(s.Tags), s.SendMode, s.Shortcut, ms(now), ms(now))
	return mapErr(err)
}

// Update saves s.
func (r *Snippets) Update(ctx context.Context, s *model.Snippet) error {
	s.UpdatedAt = Now()
	return expectOne(r.db.ExecContext(ctx, `UPDATE snippets SET name = ?, folder = ?, description = ?, content = ?, tags = ?,
		send_mode = ?, shortcut = ?, updated_at = ? WHERE id = ?`,
		s.Name, s.Folder, s.Description, s.Content, stringsJSON(s.Tags), s.SendMode, s.Shortcut, ms(s.UpdatedAt), s.ID))
}

// Delete removes a snippet.
func (r *Snippets) Delete(ctx context.Context, id string) error {
	return expectOne(r.db.ExecContext(ctx, `DELETE FROM snippets WHERE id = ?`, id))
}

// ---- macros -------------------------------------------------------------------------------------------------------

// Macros is the macro repository.
type Macros struct{ db *sql.DB }

const macroCols = `id, owner_id, name, steps, created_at, updated_at`

func scanMacro(sc scanner) (*model.Macro, error) {
	var (
		m                model.Macro
		steps            string
		created, updated int64
	)
	if err := sc.Scan(&m.ID, &m.OwnerID, &m.Name, &steps, &created, &updated); err != nil {
		return nil, mapErr(err)
	}
	m.Steps = []model.MacroStep{}
	_ = json.Unmarshal([]byte(steps), &m.Steps)
	if m.Steps == nil {
		m.Steps = []model.MacroStep{}
	}
	m.CreatedAt, m.UpdatedAt = fromMs(created), fromMs(updated)
	return &m, nil
}

// ListByOwner returns a user's macros.
func (r *Macros) ListByOwner(ctx context.Context, ownerID string) ([]*model.Macro, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+macroCols+` FROM macros WHERE owner_id = ? ORDER BY name COLLATE NOCASE`, ownerID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*model.Macro{}
	for rows.Next() {
		m, err := scanMacro(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Get returns a macro by ID.
func (r *Macros) Get(ctx context.Context, id string) (*model.Macro, error) {
	return scanMacro(r.db.QueryRowContext(ctx, `SELECT `+macroCols+` FROM macros WHERE id = ?`, id))
}

// Create inserts m.
func (r *Macros) Create(ctx context.Context, m *model.Macro) error {
	if m.ID == "" {
		m.ID = model.NewID()
	}
	if m.Steps == nil {
		m.Steps = []model.MacroStep{}
	}
	steps, err := jsonText(m.Steps)
	if err != nil {
		return err
	}
	now := Now()
	m.CreatedAt, m.UpdatedAt = now, now
	_, err = r.db.ExecContext(ctx, `INSERT INTO macros (`+macroCols+`) VALUES (?, ?, ?, ?, ?, ?)`,
		m.ID, m.OwnerID, m.Name, steps, ms(now), ms(now))
	return mapErr(err)
}

// Update saves m.
func (r *Macros) Update(ctx context.Context, m *model.Macro) error {
	if m.Steps == nil {
		m.Steps = []model.MacroStep{}
	}
	steps, err := jsonText(m.Steps)
	if err != nil {
		return err
	}
	m.UpdatedAt = Now()
	return expectOne(r.db.ExecContext(ctx, `UPDATE macros SET name = ?, steps = ?, updated_at = ? WHERE id = ?`,
		m.Name, steps, ms(m.UpdatedAt), m.ID))
}

// Delete removes a macro.
func (r *Macros) Delete(ctx context.Context, id string) error {
	return expectOne(r.db.ExecContext(ctx, `DELETE FROM macros WHERE id = ?`, id))
}

// ---- tunnels ------------------------------------------------------------------------------------------------------

// Tunnels is the saved port-forward repository.
type Tunnels struct{ db *sql.DB }

const tunnelCols = `id, owner_id, name, type, connection_id, bind_host, bind_port, dest_host, dest_port, auto_start, created_at, updated_at`

func scanTunnel(sc scanner) (*model.Tunnel, error) {
	var (
		t                model.Tunnel
		auto             int
		created, updated int64
	)
	if err := sc.Scan(&t.ID, &t.OwnerID, &t.Name, &t.Type, &t.ConnectionID, &t.BindHost, &t.BindPort, &t.DestHost, &t.DestPort,
		&auto, &created, &updated); err != nil {
		return nil, mapErr(err)
	}
	t.AutoStart, t.CreatedAt, t.UpdatedAt = auto != 0, fromMs(created), fromMs(updated)
	t.Status = model.TunnelStatus{State: model.TunnelStopped}
	return &t, nil
}

func collectTunnels(rows *sql.Rows, err error) ([]*model.Tunnel, error) {
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*model.Tunnel{}
	for rows.Next() {
		t, err := scanTunnel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListByOwner returns a user's tunnels.
func (r *Tunnels) ListByOwner(ctx context.Context, ownerID string) ([]*model.Tunnel, error) {
	return collectTunnels(r.db.QueryContext(ctx, `SELECT `+tunnelCols+` FROM tunnels WHERE owner_id = ? ORDER BY name COLLATE NOCASE`, ownerID))
}

// ListAutoStart returns every tunnel (all users) flagged autoStart.
func (r *Tunnels) ListAutoStart(ctx context.Context) ([]*model.Tunnel, error) {
	return collectTunnels(r.db.QueryContext(ctx, `SELECT `+tunnelCols+` FROM tunnels WHERE auto_start = 1 ORDER BY created_at`))
}

// Get returns a tunnel by ID.
func (r *Tunnels) Get(ctx context.Context, id string) (*model.Tunnel, error) {
	return scanTunnel(r.db.QueryRowContext(ctx, `SELECT `+tunnelCols+` FROM tunnels WHERE id = ?`, id))
}

// Create inserts t.
func (r *Tunnels) Create(ctx context.Context, t *model.Tunnel) error {
	if t.ID == "" {
		t.ID = model.NewID()
	}
	now := Now()
	t.CreatedAt, t.UpdatedAt = now, now
	_, err := r.db.ExecContext(ctx, `INSERT INTO tunnels (`+tunnelCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.OwnerID, t.Name, t.Type, t.ConnectionID, t.BindHost, t.BindPort, t.DestHost, t.DestPort, b2i(t.AutoStart), ms(now), ms(now))
	return mapErr(err)
}

// Update saves t.
func (r *Tunnels) Update(ctx context.Context, t *model.Tunnel) error {
	t.UpdatedAt = Now()
	return expectOne(r.db.ExecContext(ctx, `UPDATE tunnels SET name = ?, type = ?, connection_id = ?, bind_host = ?, bind_port = ?,
		dest_host = ?, dest_port = ?, auto_start = ?, updated_at = ? WHERE id = ?`,
		t.Name, t.Type, t.ConnectionID, t.BindHost, t.BindPort, t.DestHost, t.DestPort, b2i(t.AutoStart), ms(t.UpdatedAt), t.ID))
}

// Delete removes a tunnel.
func (r *Tunnels) Delete(ctx context.Context, id string) error {
	return expectOne(r.db.ExecContext(ctx, `DELETE FROM tunnels WHERE id = ?`, id))
}
