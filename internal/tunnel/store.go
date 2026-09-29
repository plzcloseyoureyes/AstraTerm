package tunnel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/store"
)

// The core `tunnels` table (SPEC §5.1) holds the SPEC fields; everything else lives in the module table
// tunnel_meta (options JSON, sealed secrets, manual sort order), deleted with its tunnel by cascade.
func init() {
	store.RegisterMigration("tunnel", 1, `
CREATE TABLE tunnel_meta (
	tunnel_id   TEXT PRIMARY KEY REFERENCES tunnels(id) ON DELETE CASCADE,
	options     TEXT NOT NULL DEFAULT '{}',
	secrets_enc BLOB,
	secret_keys TEXT NOT NULL DEFAULT '[]',
	sort_order  INTEGER NOT NULL DEFAULT 0
);`)
}

// meta is one tunnel_meta row.
type meta struct {
	Options    Options
	SecretsEnc []byte
	SecretKeys []string
	SortOrder  int
}

func (m *meta) decode(opts, keys string) error {
	if opts != "" {
		if err := json.Unmarshal([]byte(opts), &m.Options); err != nil {
			return fmt.Errorf("invalid options: %w", err)
		}
	}
	if keys != "" {
		_ = json.Unmarshal([]byte(keys), &m.SecretKeys)
	}
	if m.SecretKeys == nil {
		m.SecretKeys = []string{}
	}
	return nil
}

// record is a saved tunnel with its module metadata.
type record struct {
	*model.Tunnel
	meta
}

// def returns the forwarding definition of a record.
func (r *record) def() def {
	return def{
		Type: r.Type, BindHost: r.BindHost, BindPort: r.BindPort, DestHost: r.DestHost, DestPort: r.DestPort,
		Reverse: r.Options.Reverse, BindSocket: r.Options.BindSocket, DestSocket: r.Options.DestSocket,
	}
}

// repo wraps the core Tunnels repository and the module table.
type repo struct {
	st *store.Store
}

func (r *repo) getMeta(ctx context.Context, id string) (meta, error) {
	var (
		m          meta
		opts, keys string
	)
	err := r.st.DB.QueryRowContext(ctx, `SELECT options, secrets_enc, secret_keys, sort_order FROM tunnel_meta WHERE tunnel_id = ?`, id).
		Scan(&opts, &m.SecretsEnc, &keys, &m.SortOrder)
	if errors.Is(err, sql.ErrNoRows) {
		return meta{SecretKeys: []string{}}, nil // created by another tool: defaults
	}
	if err != nil {
		return meta{}, err
	}
	if err := m.decode(opts, keys); err != nil {
		return meta{}, fmt.Errorf("tunnel %s: %w", id, err)
	}
	return m, nil
}

func (r *repo) putMeta(ctx context.Context, id string, m meta) error {
	b, err := json.Marshal(m.Options)
	if err != nil {
		return err
	}
	var enc any
	if len(m.SecretsEnc) > 0 {
		enc = m.SecretsEnc
	}
	keys := m.SecretKeys
	if keys == nil {
		keys = []string{}
	}
	kb, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	_, err = r.st.DB.ExecContext(ctx, `INSERT INTO tunnel_meta (tunnel_id, options, secrets_enc, secret_keys, sort_order) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (tunnel_id) DO UPDATE SET options = excluded.options, secrets_enc = excluded.secrets_enc,
		secret_keys = excluded.secret_keys, sort_order = excluded.sort_order`,
		id, string(b), enc, string(kb), m.SortOrder)
	return err
}

// get loads one tunnel with its metadata (model.ErrNotFound when missing).
func (r *repo) get(ctx context.Context, id string) (*record, error) {
	t, err := r.st.Tunnels.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	m, err := r.getMeta(ctx, id)
	if err != nil {
		return nil, err
	}
	return &record{Tunnel: t, meta: m}, nil
}

// listByOwner returns a user's tunnels ordered by sort order, then name.
func (r *repo) listByOwner(ctx context.Context, ownerID string) ([]*record, error) {
	list, err := r.st.Tunnels.ListByOwner(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	metas, err := r.metasOfOwner(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	out := make([]*record, 0, len(list))
	for _, t := range list {
		m, ok := metas[t.ID]
		if !ok {
			m.SecretKeys = []string{}
		}
		out = append(out, &record{Tunnel: t, meta: m})
	}
	sortRecords(out)
	return out, nil
}

func sortRecords(list []*record) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].SortOrder != list[j].SortOrder {
			return list[i].SortOrder < list[j].SortOrder
		}
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
}

func (r *repo) metasOfOwner(ctx context.Context, ownerID string) (map[string]meta, error) {
	rows, err := r.st.DB.QueryContext(ctx, `SELECT m.tunnel_id, m.options, m.secrets_enc, m.secret_keys, m.sort_order
		FROM tunnel_meta m JOIN tunnels t ON t.id = m.tunnel_id WHERE t.owner_id = ?`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]meta{}
	for rows.Next() {
		var (
			id, opts, keys string
			m              meta
		)
		if err := rows.Scan(&id, &opts, &m.SecretsEnc, &keys, &m.SortOrder); err != nil {
			return nil, err
		}
		if err := m.decode(opts, keys); err != nil {
			return nil, fmt.Errorf("tunnel %s: %w", id, err)
		}
		out[id] = m
	}
	return out, rows.Err()
}

// create inserts a tunnel and its metadata. The core row is removed again when the metadata cannot be written.
func (r *repo) create(ctx context.Context, rec *record) error {
	if err := r.st.Tunnels.Create(ctx, rec.Tunnel); err != nil {
		return err
	}
	if err := r.putMeta(ctx, rec.ID, rec.meta); err != nil {
		_ = r.st.Tunnels.Delete(context.WithoutCancel(ctx), rec.ID)
		return err
	}
	return nil
}

// update saves a tunnel and its metadata.
func (r *repo) update(ctx context.Context, rec *record) error {
	if err := r.st.Tunnels.Update(ctx, rec.Tunnel); err != nil {
		return err
	}
	return r.putMeta(ctx, rec.ID, rec.meta)
}

func (r *repo) delete(ctx context.Context, id string) error {
	return r.st.Tunnels.Delete(ctx, id) // tunnel_meta follows by cascade
}

// setSortOrders updates the manual order of several tunnels of one owner in a transaction.
func (r *repo) setSortOrders(ctx context.Context, ownerID string, orders map[string]int) error {
	return r.st.Tx(ctx, func(tx *sql.Tx) error {
		for id, n := range orders {
			var owner string
			if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM tunnels WHERE id = ?`, id).Scan(&owner); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return model.ErrNotFound
				}
				return err
			}
			if owner != ownerID {
				return model.ErrNotFound
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO tunnel_meta (tunnel_id, sort_order) VALUES (?, ?)
				ON CONFLICT (tunnel_id) DO UPDATE SET sort_order = excluded.sort_order`, id, n); err != nil {
				return err
			}
		}
		return nil
	})
}

// countByOwner returns how many tunnels a user has.
func (r *repo) countByOwner(ctx context.Context, ownerID string) (int, error) {
	var n int
	err := r.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM tunnels WHERE owner_id = ?`, ownerID).Scan(&n)
	return n, err
}

// maxSortOrder returns the highest sort order of a user's tunnels (new tunnels go last).
func (r *repo) maxSortOrder(ctx context.Context, ownerID string) (int, error) {
	var n sql.NullInt64
	err := r.st.DB.QueryRowContext(ctx, `SELECT MAX(m.sort_order) FROM tunnel_meta m JOIN tunnels t ON t.id = m.tunnel_id
		WHERE t.owner_id = ?`, ownerID).Scan(&n)
	if err != nil {
		return 0, err
	}
	return int(n.Int64), nil
}
