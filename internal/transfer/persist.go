package transfer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/store"
	"github.com/termstead/termstead/internal/vfs"
)

// Durability of the queue: every transfer is recorded in the module table "transfers" (created, state changes,
// progress every few seconds, finish). A server restart cannot continue a transfer (its file system handles and
// runtime sessions are gone), but it never loses one silently: transfers that were queued or running are loaded
// as state "error" with interrupted:true ("Interrupted by a server restart") and, when both sides can be reopened
// without the old handles (saved connection, the local host, a still running session), resumable:true.
// POST /api/transfers/{id}/retry reopens both sides and starts the transfer again with overwrite "resume" (finished
// files are skipped, partial "<name>.termstead-part" files continue); the old record is replaced by the new one.

func init() {
	store.RegisterMigration("transfer", 1, `CREATE TABLE IF NOT EXISTS transfers (
		id TEXT PRIMARY KEY,
		owner_id TEXT NOT NULL,
		state TEXT NOT NULL,
		info TEXT NOT NULL,
		src TEXT NOT NULL,
		dst TEXT NOT NULL,
		request TEXT NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS transfers_owner ON transfers(owner_id);`)
}

// origin describes how a file system handle was opened, so it can be opened again after a restart.
type origin struct {
	Kind         string `json:"kind"`
	Driver       string `json:"driver"`
	ConnectionID string `json:"connectionId,omitempty"`
	SessionID    string `json:"sessionId,omitempty"`
	Label        string `json:"label"`
}

func originOf(h *vfs.Handle) origin {
	return origin{Kind: h.Kind, Driver: h.Driver, ConnectionID: h.ConnectionID, SessionID: h.SessionID, Label: h.Label}
}

// reopenable reports whether the handle can be opened again without its original (quick-connect secrets, ...).
func (o origin) reopenable() bool {
	return o.Kind == "local" || o.ConnectionID != "" || o.SessionID != ""
}

// persistInterval throttles progress writes.
const persistInterval = 5 * time.Second

func (m *Manager) db() *sql.DB {
	if m.d == nil || m.d.Store == nil {
		return nil
	}
	return m.d.Store.DB
}

// save records the job (force: state changes; else at most every persistInterval). Never during shutdown: a
// transfer cut by the shutdown stays "running" in the table and is reported as interrupted on the next start.
func (j *job) save(force bool) {
	db := j.m.db()
	if db == nil || j.m.ctx.Err() != nil {
		return
	}
	j.mu.Lock()
	if !force && time.Since(j.lastSave) < persistInterval {
		j.mu.Unlock()
		return
	}
	j.lastSave = time.Now()
	in := j.snapshotLocked()
	j.mu.Unlock()
	info, _ := json.Marshal(in)
	src, _ := json.Marshal(j.srcOrigin)
	dst, _ := json.Marshal(j.dstOrigin)
	req, _ := json.Marshal(j.req)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `INSERT INTO transfers (id, owner_id, state, info, src, dst, request, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET state = excluded.state, info = excluded.info,
		updated_at = excluded.updated_at`, in.ID, j.user.ID, in.State, string(info), string(src), string(dst), string(req),
		time.Now().UnixMilli()); err != nil {
		j.m.log.Debug("transfer: cannot record", "transfer", in.ID, "err", err)
	}
}

// forget deletes records.
func (m *Manager) forget(ids ...string) {
	db := m.db()
	if db == nil || len(ids) == 0 || m.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, id := range ids {
		_, _ = db.ExecContext(ctx, `DELETE FROM transfers WHERE id = ?`, id)
	}
}

// restored is a transfer of an earlier run of the server (finished, or interrupted by the restart).
type restored struct {
	info     Info
	ownerID  string
	src, dst origin
	req      Request
}

// load reads the recorded transfers at start: finished ones are listed again for keepFinished, unfinished ones are
// marked interrupted.
func (m *Manager) load() {
	db := m.db()
	if db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	if err := m.d.Store.Migrate(ctx); err != nil {
		m.log.Warn("transfer: migration failed; transfers are not recorded", "err", err)
		return
	}
	rows, err := db.QueryContext(ctx, `SELECT owner_id, info, src, dst, request FROM transfers`)
	if err != nil {
		m.log.Warn("transfer: cannot load recorded transfers", "err", err)
		return
	}
	var list []*restored
	for rows.Next() {
		var owner, info, src, dst, req string
		if rows.Scan(&owner, &info, &src, &dst, &req) != nil {
			continue
		}
		r := &restored{ownerID: owner}
		if json.Unmarshal([]byte(info), &r.info) != nil {
			continue
		}
		_ = json.Unmarshal([]byte(src), &r.src)
		_ = json.Unmarshal([]byte(dst), &r.dst)
		_ = json.Unmarshal([]byte(req), &r.req)
		list = append(list, r)
	}
	rows.Close()
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range list {
		in := &r.info
		in.OwnerID = r.ownerID
		if in.FinishedAt == nil {
			in.State, in.Error, in.Interrupted = model.TransferError, "Interrupted by a server restart", true
			in.CurrentFile, in.BytesPerSec, in.FinishedAt = "", 0, &now
			in.Resumable = r.src.reopenable() && r.dst.reopenable()
			info, _ := json.Marshal(in)
			_, _ = db.ExecContext(ctx, `UPDATE transfers SET state = ?, info = ? WHERE id = ?`, in.State, string(info), in.ID)
		} else if time.Since(*in.FinishedAt) > keepFinished {
			_, _ = db.ExecContext(ctx, `DELETE FROM transfers WHERE id = ?`, in.ID)
			continue
		}
		m.old[in.ID] = r
	}
}

// Retry starts an interrupted (or failed / canceled) transfer of an earlier server run again: both sides are
// reopened and the copy resumes (overwrite "resume").
func (m *Manager) Retry(ctx context.Context, user *model.User, id string) (Info, error) {
	if user == nil {
		return Info{}, httpx.ErrUnauthorized
	}
	m.mu.Lock()
	r := m.old[id]
	m.mu.Unlock()
	if r == nil || r.ownerID != user.ID {
		if j := m.get(user, id); j != nil {
			return m.retryLive(ctx, user, j)
		}
		return Info{}, httpx.ErrNotFound
	}
	if !r.src.reopenable() || !r.dst.reopenable() {
		return Info{}, httpx.Conflict("this transfer used a quick connection: open both sides again and start it anew")
	}
	src, err := m.reopen(ctx, user, r.src)
	if err != nil {
		return Info{}, fmt.Errorf("reopen the source (%s): %w", r.src.Label, err)
	}
	dst, err := m.reopen(ctx, user, r.dst)
	if err != nil {
		_ = m.reg.Close(user, src)
		return Info{}, fmt.Errorf("reopen the destination (%s): %w", r.dst.Label, err)
	}
	req := r.req
	req.SrcFS, req.DstFS, req.Overwrite = src, dst, PolicyResume
	in, err := m.Create(ctx, user, req)
	if err != nil {
		return Info{}, err
	}
	m.mu.Lock()
	delete(m.old, id)
	m.mu.Unlock()
	m.forget(id)
	return in, nil
}

// retryLive restarts a finished transfer of this run (its handles may still be open).
func (m *Manager) retryLive(ctx context.Context, user *model.User, j *job) (Info, error) {
	j.mu.Lock()
	finished := j.info.FinishedAt != nil
	req := j.req
	j.mu.Unlock()
	if !finished {
		return Info{}, httpx.Conflict("the transfer is still running")
	}
	req.SrcFS, req.DstFS, req.Overwrite = j.src.ID, j.dst.ID, PolicyResume
	in, err := m.Create(ctx, user, req)
	if err != nil {
		return Info{}, err
	}
	_ = m.Remove(user, j.info.ID)
	return in, nil
}

// reopen opens a file system like the recorded handle was (a gone session falls back to its saved connection).
func (m *Manager) reopen(ctx context.Context, user *model.User, o origin) (string, error) {
	sudo := o.Driver == "sudo-sftp"
	var reqs []vfs.OpenRequest
	switch {
	case o.Kind == "local":
		reqs = append(reqs, vfs.OpenRequest{Local: true})
	default:
		if o.SessionID != "" {
			reqs = append(reqs, vfs.OpenRequest{SessionID: o.SessionID, Sudo: sudo})
		}
		if o.ConnectionID != "" {
			reqs = append(reqs, vfs.OpenRequest{ConnectionID: o.ConnectionID, Sudo: sudo})
		}
	}
	var last error = errors.New("cannot reopen")
	for _, rq := range reqs {
		h, err := m.reg.Open(ctx, user, rq)
		if err == nil {
			return h.ID, nil
		}
		last = err
	}
	return "", last
}
