// Package transfer is Termstead's server-side transfer queue (SPEC §6.0 "Files" transfers, RESEARCH FILE-8, FILE-14):
// copies and moves between any two file system handles of the vfs registry (SFTP ↔ SFTP across hosts, local ↔
// remote, S3, FTP...), recursive with folder merge, conflict policies (ask / overwrite / skip / resume / rename with
// "apply to all"), atomic "<name>.termstead-part" writes, mtime / permission preservation, optional SHA-256
// verification, retries with resume on transient network errors, a concurrency limit, throttled progress events and
// audit entries. Data never passes through the browser.
package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/events"
	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/vfs"
)

// Overwrite policies.
const (
	PolicyAsk       = "ask"
	PolicyOverwrite = "overwrite"
	PolicySkip      = "skip"
	PolicyResume    = "resume"
	PolicyRename    = "rename"
)

// Defaults.
const (
	DefaultMaxRunning = 3
	DefaultRetries    = 3
	eventInterval     = 250 * time.Millisecond // ≤ 4 progress events per second per transfer
	maxItems          = 1_000_000
	maxKeptPerUser    = 200
	keepFinished      = 24 * time.Hour
	maxErrors         = 20
)

// Request is the body of POST /api/transfers.
type Request struct {
	SrcFS     string   `json:"srcFs"`
	SrcPaths  []string `json:"srcPaths"`
	DstFS     string   `json:"dstFs"`
	DstDir    string   `json:"dstDir"`
	Overwrite string   `json:"overwrite"`
	Move      bool     `json:"move"`
	Preserve  *bool    `json:"preserve"`
	Verify    bool     `json:"verify"`
	Label     string   `json:"label"`
}

// Info is the JSON view of a transfer: the SPEC Transfer plus extras.
type Info struct {
	model.Transfer
	Move         bool     `json:"move"`
	Overwrite    string   `json:"overwrite"`
	SkippedFiles int      `json:"skippedFiles"`
	FailedFiles  int      `json:"failedFiles"`
	Errors       []string `json:"errors,omitempty"`
	// Interrupted: the transfer was cut by a server restart; Resumable: POST /api/transfers/{id}/retry can resume it.
	Interrupted bool `json:"interrupted,omitempty"`
	Resumable   bool `json:"resumable,omitempty"`
}

// Manager owns the transfers of every user.
type Manager struct {
	d   *app.Deps
	reg *vfs.Registry
	log *slog.Logger
	ctx context.Context

	// Retries of a file after transient errors (default DefaultRetries).
	Retries int

	sem chan struct{}

	mu   sync.Mutex
	jobs map[string]*job
	old  map[string]*restored // transfers recorded by an earlier run of the server (persist.go)
}

type job struct {
	m      *Manager
	user   *model.User
	req    Request
	ctx    context.Context
	cancel context.CancelFunc
	src    *vfs.Handle
	dst    *vfs.Handle
	relSrc func()
	relDst func()

	links map[string]bool // destination symlinks this transfer created (never written through)

	srcOrigin, dstOrigin origin // how the handles were opened (persist.go)
	lastSave             time.Time

	mu       sync.Mutex
	info     Info
	policy   string // current conflict decision (fixed after "apply to all")
	samples  []sample
	pubTimer *time.Timer
	lastPub  time.Time
	dirty    bool
}

type sample struct {
	t time.Time
	n int64
}

// New creates the transfer manager on top of the vfs handle registry.
func New(d *app.Deps, reg *vfs.Registry) *Manager {
	ctx := context.Background()
	log := slog.Default()
	if d != nil && d.Ctx != nil {
		ctx = d.Ctx
	}
	if d != nil && d.Log != nil {
		log = d.Log
	}
	m := &Manager{d: d, reg: reg, log: log.With("module", "transfer"), ctx: ctx, Retries: DefaultRetries,
		sem: make(chan struct{}, DefaultMaxRunning), jobs: map[string]*job{}, old: map[string]*restored{}}
	m.load()
	go m.janitor()
	return m
}

// SetMaxRunning changes the number of transfers running at the same time (others wait queued).
func (m *Manager) SetMaxRunning(n int) {
	if n < 1 {
		n = 1
	}
	m.mu.Lock()
	m.sem = make(chan struct{}, n)
	m.mu.Unlock()
}

func (m *Manager) janitor() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
		m.mu.Lock()
		for id, j := range m.jobs {
			j.mu.Lock()
			old := j.info.FinishedAt != nil && time.Since(*j.info.FinishedAt) > keepFinished
			j.mu.Unlock()
			if old {
				delete(m.jobs, id)
				go m.forget(id)
			}
		}
		for id, r := range m.old {
			if r.info.FinishedAt != nil && time.Since(*r.info.FinishedAt) > keepFinished {
				delete(m.old, id)
				go m.forget(id)
			}
		}
		m.mu.Unlock()
	}
}

// ---- API ----------------------------------------------------------------------------------------------------------

// Create validates req and queues a transfer for user.
func (m *Manager) Create(ctx context.Context, user *model.User, req Request) (Info, error) {
	if user == nil {
		return Info{}, httpx.ErrUnauthorized
	}
	req.Overwrite = strings.ToLower(strings.TrimSpace(req.Overwrite))
	switch req.Overwrite {
	case "":
		req.Overwrite = PolicyAsk
	case PolicyAsk, PolicyOverwrite, PolicySkip, PolicyResume, PolicyRename:
	default:
		return Info{}, httpx.BadRequest("overwrite must be ask, overwrite, skip, resume or rename")
	}
	if len(req.SrcPaths) == 0 {
		return Info{}, httpx.BadRequest("srcPaths is required")
	}
	if len(req.SrcPaths) > 10000 {
		return Info{}, httpx.BadRequest("too many source paths")
	}
	src, relSrc, err := m.reg.Acquire(user, req.SrcFS)
	if err != nil {
		return Info{}, err
	}
	dst, relDst, err := m.reg.Acquire(user, req.DstFS)
	if err != nil {
		relSrc()
		return Info{}, err
	}
	fail := func(err error) (Info, error) {
		relSrc()
		relDst()
		return Info{}, err
	}
	dstDir, err := vfs.CleanPath(req.DstDir, dst.Home)
	if err != nil {
		return fail(httpx.BadRequest("invalid dstDir"))
	}
	if e, err := dst.FS.Stat(ctx, dstDir); err != nil {
		return fail(vfs.FSError(err, dstDir))
	} else if e.Type != model.FileTypeDir {
		return fail(httpx.BadRequest("dstDir is not a folder"))
	}
	req.DstDir = dstDir
	seen := map[string]bool{}
	var paths []string
	for _, raw := range req.SrcPaths {
		p, err := vfs.CleanPath(raw, src.Home)
		if err != nil {
			return fail(httpx.BadRequest("invalid source path"))
		}
		if p == "/" {
			return fail(httpx.BadRequest("cannot transfer the root directory"))
		}
		if src == dst && vfs.IsWithin(dstDir, p) {
			return fail(httpx.BadRequest("cannot copy a folder into itself"))
		}
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	req.SrcPaths = paths
	label := strings.TrimSpace(req.Label)
	if label == "" {
		verb := "Copy"
		if req.Move {
			verb = "Move"
		}
		what := vfs.BaseName(paths[0])
		if len(paths) > 1 {
			what = fmt.Sprintf("%d items", len(paths))
		}
		label = fmt.Sprintf("%s %s → %s:%s", verb, what, dst.Label, dstDir)
	}
	jctx, cancel := context.WithCancel(m.ctx)
	j := &job{m: m, user: user, req: req, ctx: jctx, cancel: cancel, src: src, dst: dst, relSrc: relSrc, relDst: relDst,
		policy: req.Overwrite, srcOrigin: originOf(src), dstOrigin: originOf(dst)}
	j.info = Info{
		Transfer: model.Transfer{ID: model.NewID(), SrcFS: src.ID, DstFS: dst.ID, SrcPaths: paths, DstDir: dstDir,
			Label: label, State: model.TransferQueued, OwnerID: user.ID, CreatedAt: time.Now().UTC()},
		Move: req.Move, Overwrite: req.Overwrite,
	}
	m.mu.Lock()
	m.jobs[j.info.ID] = j
	m.pruneLocked(user.ID)
	sem := m.sem
	m.mu.Unlock()
	j.publish(true)
	j.save(true)
	m.audit(ctx, user, "transfer.start", j.info.ID, map[string]any{"src": src.Label, "srcFs": src.ID, "paths": paths,
		"dst": dst.Label, "dstFs": dst.ID, "dstDir": dstDir, "move": req.Move})
	go j.run(sem)
	return j.snapshot(), nil
}

// pruneLocked drops the oldest finished transfers of a user beyond maxKeptPerUser.
func (m *Manager) pruneLocked(userID string) {
	var mine []*job
	for _, j := range m.jobs {
		if j.user.ID == userID {
			mine = append(mine, j)
		}
	}
	if len(mine) <= maxKeptPerUser {
		return
	}
	sort.Slice(mine, func(a, b int) bool { return mine[a].info.CreatedAt.Before(mine[b].info.CreatedAt) })
	for _, j := range mine[:len(mine)-maxKeptPerUser] {
		j.mu.Lock()
		finished := j.info.FinishedAt != nil
		j.mu.Unlock()
		if finished {
			delete(m.jobs, j.info.ID)
			go m.forget(j.info.ID)
		}
	}
}

// List returns user's transfers, oldest first.
func (m *Manager) List(user *model.User) []Info {
	m.mu.Lock()
	var js []*job
	for _, j := range m.jobs {
		if user != nil && j.user.ID == user.ID {
			js = append(js, j)
		}
	}
	var olds []Info
	for _, r := range m.old {
		if user != nil && r.ownerID == user.ID {
			olds = append(olds, r.info)
		}
	}
	m.mu.Unlock()
	out := make([]Info, 0, len(js)+len(olds))
	for _, j := range js {
		out = append(out, j.snapshot())
	}
	out = append(out, olds...)
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.Before(out[b].CreatedAt) })
	return out
}

func (m *Manager) get(user *model.User, id string) *job {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil || user == nil || j.user.ID != user.ID {
		return nil
	}
	return j
}

// Get returns one transfer of user.
func (m *Manager) Get(user *model.User, id string) (Info, error) {
	j := m.get(user, id)
	if j == nil {
		if r := m.oldOf(user, id); r != nil {
			return r.info, nil
		}
		return Info{}, httpx.ErrNotFound
	}
	return j.snapshot(), nil
}

// oldOf returns user's recorded transfer of an earlier run.
func (m *Manager) oldOf(user *model.User, id string) *restored {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.old[id]; r != nil && user != nil && r.ownerID == user.ID {
		return r
	}
	return nil
}

// Cancel stops a queued or running transfer.
func (m *Manager) Cancel(user *model.User, id string) error {
	j := m.get(user, id)
	if j == nil {
		if m.oldOf(user, id) != nil {
			return nil // already finished
		}
		return httpx.ErrNotFound
	}
	j.cancel()
	return nil
}

// Remove cancels (if needed) and forgets a transfer.
func (m *Manager) Remove(user *model.User, id string) error {
	j := m.get(user, id)
	if j == nil {
		if m.oldOf(user, id) == nil {
			return httpx.ErrNotFound
		}
		m.mu.Lock()
		delete(m.old, id)
		m.mu.Unlock()
		m.forget(id)
		return nil
	}
	j.cancel()
	m.mu.Lock()
	delete(m.jobs, id)
	m.mu.Unlock()
	m.forget(id)
	return nil
}

// RemoveFinished forgets every finished transfer of user and returns how many were removed.
func (m *Manager) RemoveFinished(user *model.User) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, j := range m.jobs {
		if user == nil || j.user.ID != user.ID {
			continue
		}
		j.mu.Lock()
		finished := j.info.FinishedAt != nil
		j.mu.Unlock()
		if finished {
			delete(m.jobs, id)
			go m.forget(id)
			n++
		}
	}
	for id, r := range m.old {
		if user != nil && r.ownerID == user.ID {
			delete(m.old, id)
			go m.forget(id)
			n++
		}
	}
	return n
}

func (m *Manager) audit(ctx context.Context, user *model.User, action, target string, details any) {
	if m.d == nil || m.d.Audit == nil || m.ctx.Err() != nil {
		return
	}
	m.d.Audit.LogUser(context.WithoutCancel(ctx), user, action, target, details)
}

// ---- state & events -----------------------------------------------------------------------------------------------

func (j *job) snapshot() Info {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snapshotLocked()
}

func (j *job) snapshotLocked() Info {
	in := j.info
	in.SrcPaths = append([]string(nil), j.info.SrcPaths...)
	in.Errors = append([]string(nil), j.info.Errors...)
	if j.info.FinishedAt != nil {
		t := *j.info.FinishedAt
		in.FinishedAt = &t
	}
	return in
}

type transferEvent struct {
	Type     string `json:"type"`
	Transfer Info   `json:"transfer"`
}

// publish sends {type:'transfer'}: immediately when force (state changes), else at most every eventInterval.
func (j *job) publish(force bool) {
	if j.m.d == nil || j.m.d.Events == nil {
		return
	}
	j.mu.Lock()
	if !force {
		j.dirty = true
		if j.pubTimer != nil {
			j.mu.Unlock()
			return
		}
		if wait := eventInterval - time.Since(j.lastPub); wait > 0 {
			j.pubTimer = time.AfterFunc(wait, func() {
				j.mu.Lock()
				j.pubTimer = nil
				dirty := j.dirty
				j.mu.Unlock()
				if dirty {
					j.publish(false)
				}
			})
			j.mu.Unlock()
			return
		}
	}
	j.dirty = false
	j.lastPub = time.Now()
	ev := transferEvent{Type: model.EvTransfer, Transfer: j.snapshotLocked()}
	userID := j.user.ID
	j.mu.Unlock()
	j.m.d.Events.Publish(userID, ev)
}

func (j *job) setState(st string) {
	j.mu.Lock()
	j.info.State = st
	j.mu.Unlock()
	j.publish(true)
	j.save(true)
}

// addBytes records progress and refreshes the rate (bytes per second over the last 3 s).
func (j *job) addBytes(n int64) {
	if n <= 0 {
		return
	}
	now := time.Now()
	j.mu.Lock()
	j.info.DoneBytes += n
	j.samples = append(j.samples, sample{now, j.info.DoneBytes})
	cut := 0
	for cut < len(j.samples)-1 && now.Sub(j.samples[cut].t) > 3*time.Second {
		cut++
	}
	j.samples = j.samples[cut:]
	if first := j.samples[0]; now.Sub(first.t) > 200*time.Millisecond {
		j.info.BytesPerSec = int64(float64(j.info.DoneBytes-first.n) / now.Sub(first.t).Seconds())
	}
	j.mu.Unlock()
	j.publish(false)
	j.save(false)
}

func (j *job) fileError(rel string, err error) {
	j.mu.Lock()
	j.info.FailedFiles++
	if len(j.info.Errors) < maxErrors {
		msg := err.Error()
		var he *httpx.HTTPError
		if errors.As(vfs.FSError(err, ""), &he) {
			msg = he.Message
		}
		j.info.Errors = append(j.info.Errors, rel+": "+msg)
	}
	j.mu.Unlock()
	j.publish(false)
}

// countingReader reports progress as data is read from the source.
type countingReader struct {
	r io.Reader
	j *job
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.j.addBytes(int64(n))
	return n, err
}

// ---- execution ----------------------------------------------------------------------------------------------------

// item is one entry to transfer.
type item struct {
	src   *vfs.Entry
	rel   string // path relative to dstDir
	top   bool   // a selected source path (not a child)
	links bool   // recreate as symlink
}

func (j *job) run(sem chan struct{}) {
	defer func() {
		if r := recover(); r != nil {
			j.m.log.Error("transfer panicked", "transfer", j.info.ID, "panic", r)
			j.finish(fmt.Errorf("internal error"))
		}
	}()
	defer j.relSrc()
	defer j.relDst()
	select {
	case sem <- struct{}{}:
	case <-j.ctx.Done():
		j.finish(j.ctx.Err())
		return
	}
	defer func() { <-sem }()
	j.setState(model.TransferRunning)
	j.finish(j.execute())
}

func (j *job) finish(err error) {
	now := time.Now().UTC()
	j.mu.Lock()
	j.info.FinishedAt = &now
	j.info.CurrentFile = ""
	j.info.BytesPerSec = 0
	switch {
	case errors.Is(err, context.Canceled) || j.ctx.Err() != nil:
		j.info.State = model.TransferCanceled
		j.info.Error = "canceled"
	case err != nil:
		j.info.State = model.TransferError
		j.info.Error = errMessage(err)
	case j.info.FailedFiles > 0:
		j.info.State = model.TransferError
		j.info.Error = fmt.Sprintf("%d of %d files failed", j.info.FailedFiles, j.info.TotalFiles)
	default:
		j.info.State = model.TransferDone
	}
	in := j.snapshotLocked()
	if j.pubTimer != nil {
		j.pubTimer.Stop()
		j.pubTimer = nil
	}
	j.mu.Unlock()
	j.publish(true)
	j.save(true)
	action := "transfer." + in.State
	j.m.audit(context.Background(), j.user, action, in.ID, map[string]any{"src": j.src.Label, "dst": j.dst.Label,
		"paths": in.SrcPaths, "dstDir": in.DstDir, "files": in.DoneFiles, "bytes": in.DoneBytes, "skipped": in.SkippedFiles,
		"failed": in.FailedFiles, "move": in.Move, "error": in.Error})
}

func errMessage(err error) string {
	var he *httpx.HTTPError
	if errors.As(vfs.FSError(err, ""), &he) {
		return he.Message
	}
	return err.Error()
}

func (j *job) execute() error {
	ctx := j.ctx
	sfs, dfs := j.src.FS, j.dst.FS
	// Same file system + move: plain renames.
	if j.req.Move && j.src == j.dst {
		return j.moveInPlace(ctx)
	}
	items, err := j.scan(ctx)
	if err != nil {
		return err
	}
	var dirsToRemove []string
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		dstPath := joinRel(j.req.DstDir, it.rel)
		j.mu.Lock()
		j.info.CurrentFile = it.rel
		j.mu.Unlock()
		if err := j.safeDest(dstPath); err != nil {
			j.fileError(it.rel, err)
			continue
		}
		switch {
		case it.src.Type == model.FileTypeDir:
			if err := j.ensureDir(ctx, dfs, dstPath); err != nil {
				j.fileError(it.rel, err)
				continue
			}
			if j.req.Move {
				dirsToRemove = append(dirsToRemove, it.src.Path)
			}
		case it.links:
			if err := j.copyLink(ctx, it, dstPath); err != nil {
				j.fileError(it.rel, err)
			}
		default:
			if err := j.copyFile(ctx, it, dstPath); err != nil {
				if errors.Is(err, context.Canceled) {
					return err
				}
				j.fileError(it.rel, err)
				continue
			}
			if j.req.Move {
				if err := sfs.Remove(ctx, it.src.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
					j.fileError(it.rel, fmt.Errorf("copied, but the source could not be deleted: %w", err))
				}
			}
		}
	}
	// Move: remove the emptied source folders, deepest first.
	for i := len(dirsToRemove) - 1; i >= 0; i-- {
		_ = sfs.Remove(ctx, dirsToRemove[i])
	}
	return nil
}

// errUnsafeDest refuses destinations outside dstDir or below a symlink this transfer created (defense in depth: the
// source listings are sanitized, see vfs.ListDir).
var errUnsafeDest = errors.New("refusing to write outside the destination folder or through a symbolic link")

// safeDest checks a destination path before anything is written to it.
func (j *job) safeDest(p string) error {
	if p != vfs.JoinPath(vfs.ParentDir(p), vfs.BaseName(p)) || !vfs.IsWithin(p, j.req.DstDir) || p == j.req.DstDir {
		return errUnsafeDest
	}
	for d := vfs.ParentDir(p); d != j.req.DstDir && vfs.IsWithin(d, j.req.DstDir); d = vfs.ParentDir(d) {
		if j.links[d] {
			return errUnsafeDest
		}
	}
	return nil
}

func joinRel(dir, rel string) string {
	if rel == "" {
		return dir
	}
	if dir == "/" {
		return "/" + rel
	}
	return dir + "/" + rel
}

// scan lists everything to transfer (pre-order, Lstat based) and computes the totals.
func (j *job) scan(ctx context.Context) ([]item, error) {
	sfs, dfs := j.src.FS, j.dst.FS
	symlinks := vfs.SupportsSymlinks(dfs)
	var items []item
	var total int64
	var files int
	var walk func(e *vfs.Entry, rel string, top bool) error
	walk = func(e *vfs.Entry, rel string, top bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(items) >= maxItems {
			return fmt.Errorf("too many files (more than %d)", maxItems)
		}
		if strings.HasSuffix(e.Name, vfs.PartSuffix) && !top {
			return nil // unfinished uploads are not copied
		}
		switch e.Type {
		case model.FileTypeDir:
			items = append(items, item{src: e, rel: rel, top: top})
			children, err := vfs.ListDir(ctx, sfs, e.Path) // sanitized: no "..", "/" or duplicate names
			if err != nil {
				return err
			}
			sort.Slice(children, func(a, b int) bool { return children[a].Name < children[b].Name })
			for _, c := range children {
				if err := walk(c, rel+"/"+c.Name, false); err != nil {
					return err
				}
			}
		case model.FileTypeSymlink:
			if symlinks {
				if e.LinkTarget == "" {
					t, err := sfs.Readlink(ctx, e.Path)
					if err != nil {
						return nil
					}
					e.LinkTarget = t
				}
				items = append(items, item{src: e, rel: rel, top: top, links: true})
				files++
				return nil
			}
			t, err := sfs.Stat(ctx, e.Path)
			if err != nil || t.Type != model.FileTypeFile {
				return nil // broken or directory symlink: skipped (no loops)
			}
			t.Path, t.Name = e.Path, e.Name
			items = append(items, item{src: t, rel: rel, top: top})
			total += t.Size
			files++
		case model.FileTypeFile:
			items = append(items, item{src: e, rel: rel, top: top})
			total += e.Size
			files++
		}
		j.mu.Lock()
		j.info.TotalBytes, j.info.TotalFiles = total, files
		j.mu.Unlock()
		j.publish(false)
		return nil
	}
	for _, p := range j.req.SrcPaths {
		e, err := sfs.Lstat(ctx, p)
		if err != nil {
			j.fileError(vfs.BaseName(p), err)
			continue
		}
		if err := walk(e, vfs.BaseName(p), true); err != nil {
			return nil, err
		}
	}
	j.mu.Lock()
	j.info.TotalBytes, j.info.TotalFiles = total, files
	j.mu.Unlock()
	j.publish(true)
	return items, nil
}

// ensureDir creates a destination folder (merging into an existing one).
func (j *job) ensureDir(ctx context.Context, dfs vfs.FS, p string) error {
	e, err := dfs.Lstat(ctx, p)
	if err == nil {
		if e.Type == model.FileTypeDir || e.LinkType == "dir" {
			return nil
		}
		return fmt.Errorf("a file with this name exists: %w", fs.ErrExist)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return dfs.MkdirAll(ctx, p)
}

// decision resolves a conflict for dstPath according to the policy (prompting for "ask").
func (j *job) decision(ctx context.Context, rel string, src, dst *vfs.Entry) (string, error) {
	j.mu.Lock()
	policy := j.policy
	j.mu.Unlock()
	if policy != PolicyAsk {
		return policy, nil
	}
	if j.m.d == nil || j.m.d.Events == nil {
		return PolicySkip, nil
	}
	msg := fmt.Sprintf("“%s” already exists in %s.\nExisting: %s, %s\nNew: %s, %s\n\nReplace it? (No skips this file.)",
		dst.Name, vfs.ParentDir(dst.Path), sizeText(dst.Size), dst.Mtime.Local().Format("2006-01-02 15:04"),
		sizeText(src.Size), src.Mtime.Local().Format("2006-01-02 15:04"))
	resp, err := j.m.d.Events.Prompt(ctx, j.user.ID, model.Prompt{
		Kind: model.PromptConfirm, Title: "File already exists", Message: msg,
		Fields:    []model.PromptField{{Label: "action", Echo: true, Value: PolicyOverwrite}},
		AllowSave: true,
	})
	switch {
	case errors.Is(err, context.Canceled):
		return "", err
	case errors.Is(err, events.ErrNoInteractiveClient), errors.Is(err, events.ErrPromptTimeout), err != nil:
		return PolicySkip, nil
	}
	choice := PolicySkip
	if resp.Accept {
		choice = PolicyOverwrite
		if len(resp.Values) > 0 {
			switch v := strings.ToLower(strings.TrimSpace(resp.Values[0])); v {
			case PolicyOverwrite, PolicySkip, PolicyResume, PolicyRename:
				choice = v
			}
		}
	}
	if resp.Save {
		j.mu.Lock()
		j.policy = choice
		j.mu.Unlock()
	}
	return choice, nil
}

func sizeText(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func (j *job) skip() {
	j.mu.Lock()
	j.info.SkippedFiles++
	j.mu.Unlock()
	j.publish(false)
}

func (j *job) copyLink(ctx context.Context, it item, dstPath string) error {
	dfs := j.dst.FS
	if de, err := dfs.Lstat(ctx, dstPath); err == nil {
		d, err := j.decision(ctx, it.rel, it.src, de)
		if err != nil {
			return err
		}
		switch d {
		case PolicySkip, PolicyResume:
			j.skip()
			return nil
		case PolicyRename:
			name, err := vfs.NumberedName(ctx, dfs, vfs.ParentDir(dstPath), vfs.BaseName(dstPath))
			if err != nil {
				return err
			}
			dstPath = vfs.JoinPath(vfs.ParentDir(dstPath), name)
		default:
			if de.Type == model.FileTypeDir {
				return fmt.Errorf("a folder with this name exists: %w", fs.ErrExist)
			}
			if err := dfs.Remove(ctx, dstPath); err != nil {
				return err
			}
		}
	}
	if err := dfs.Symlink(ctx, it.src.LinkTarget, dstPath); err != nil {
		return err
	}
	if j.links == nil {
		j.links = map[string]bool{}
	}
	j.links[dstPath] = true
	if j.req.Move {
		_ = j.src.FS.Remove(ctx, it.src.Path)
	}
	j.mu.Lock()
	j.info.DoneFiles++
	j.mu.Unlock()
	j.publish(false)
	return nil
}

// copyFile copies one file through "<dst>.termstead-part" + rename, resuming and retrying on transient errors.
func (j *job) copyFile(ctx context.Context, it item, dstPath string) error {
	dfs := j.dst.FS
	// Object stores / WebDAV commit a PUT atomically and cannot rename cheaply: write the final name directly.
	direct := !vfs.SupportsOffsetWrites(dfs)
	part := dstPath + vfs.PartSuffix
	if direct {
		part = dstPath
	}
	var offset int64
	if de, err := dfs.Lstat(ctx, dstPath); err == nil {
		d, err := j.decision(ctx, it.rel, it.src, de)
		if err != nil {
			return err
		}
		switch d {
		case PolicySkip:
			j.skip()
			j.addBytes(it.src.Size)
			return nil
		case PolicyRename:
			name, err := vfs.NumberedName(ctx, dfs, vfs.ParentDir(dstPath), vfs.BaseName(dstPath))
			if err != nil {
				return err
			}
			dstPath = vfs.JoinPath(vfs.ParentDir(dstPath), name)
			part = dstPath + vfs.PartSuffix
			if direct {
				part = dstPath
			}
		case PolicyResume:
			switch {
			case de.Type == model.FileTypeDir:
				return fmt.Errorf("a folder with this name exists: %w", fs.ErrExist)
			case de.Size == it.src.Size:
				j.skip()
				j.addBytes(it.src.Size)
				return nil
			case de.Size < it.src.Size && vfs.SupportsOffsetWrites(dfs):
				if err := dfs.Rename(ctx, dstPath, part); err != nil {
					return err
				}
				offset = de.Size
			}
		default:
			if de.Type == model.FileTypeDir {
				return fmt.Errorf("a folder with this name exists: %w", fs.ErrExist)
			}
		}
	}
	if offset == 0 && j.policyIs(PolicyResume) && vfs.SupportsOffsetWrites(dfs) {
		if pe, err := dfs.Stat(ctx, part); err == nil && pe.Size > 0 && pe.Size <= it.src.Size {
			offset = pe.Size // continue an earlier interrupted copy
		}
	}
	j.mu.Lock()
	base := j.info.DoneBytes
	j.mu.Unlock()
	if offset > 0 {
		j.addBytes(offset)
	}
	var hasher hash.Hash
	for attempt := 0; ; attempt++ {
		err := j.copyData(ctx, it, part, offset, &hasher)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			if part != dstPath {
				_ = dfs.Remove(context.WithoutCancel(ctx), part)
			}
			return ctx.Err()
		}
		if attempt >= j.m.Retries || !vfs.IsTransient(err) {
			return err
		}
		j.m.log.Debug("transfer retry", "transfer", j.info.ID, "file", it.rel, "attempt", attempt+1, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
		// Resume from what reached the destination; progress rolls back to it.
		offset = 0
		if vfs.SupportsOffsetWrites(dfs) {
			if pe, serr := dfs.Stat(ctx, part); serr == nil && pe.Size <= it.src.Size {
				offset = pe.Size
			}
		}
		j.mu.Lock()
		j.info.DoneBytes = base + offset
		j.samples = nil
		j.mu.Unlock()
		hasher = nil
	}
	if j.req.Verify {
		if err := j.verify(ctx, it, part, hasher); err != nil {
			if part != dstPath {
				_ = dfs.Remove(ctx, part)
			}
			return err
		}
	}
	if part != dstPath {
		if te, err := dfs.Stat(ctx, dstPath); err == nil && te.Type == model.FileTypeFile && !j.preserve() {
			_ = dfs.Chmod(ctx, part, te.Mode&0o7777)
		}
		if err := dfs.Rename(ctx, part, dstPath); err != nil {
			return err
		}
	}
	if j.preserve() {
		if !it.src.Mtime.IsZero() {
			_ = dfs.Chtimes(ctx, dstPath, it.src.Mtime, it.src.Mtime)
		}
		if perm := it.src.Mode & 0o7777; perm != 0 && j.src.Capabilities.Chmod {
			_ = dfs.Chmod(ctx, dstPath, perm)
		}
	}
	j.mu.Lock()
	j.info.DoneFiles++
	j.mu.Unlock()
	j.publish(false)
	return nil
}

func (j *job) policyIs(p string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.req.Overwrite == p || j.policy == p
}

func (j *job) preserve() bool { return j.req.Preserve == nil || *j.req.Preserve }

// copyData streams the source from offset into part (created / truncated at offset). With a zero offset the data
// is also hashed for verification.
func (j *job) copyData(ctx context.Context, it item, part string, offset int64, hasher *hash.Hash) error {
	sfs, dfs := j.src.FS, j.dst.FS
	r, err := sfs.Open(ctx, it.src.Path, offset)
	if err != nil {
		return err
	}
	defer r.Close()
	var w io.WriteCloser
	if offset == 0 {
		if w, err = vfs.SizedCreate(ctx, dfs, part, it.src.Size, it.src.Mode&0o7777); err != nil {
			return err
		}
	} else {
		if w, err = dfs.Create(ctx, part, offset); err != nil {
			return err
		}
	}
	var src io.Reader = r
	if offset == 0 && j.req.Verify {
		h := sha256.New()
		*hasher = h
		src = io.TeeReader(r, h)
	}
	cr := &countingReader{r: src, j: j}
	n, err := vfs.CopyStream(ctx, w, cr, it.src.Size-offset)
	cerr := w.Close()
	if err == nil {
		err = cerr
	}
	if err == nil && n != it.src.Size-offset {
		err = fmt.Errorf("source changed while copying (%d of %d bytes): %w", n, it.src.Size-offset, io.ErrUnexpectedEOF)
	}
	return err
}

// verify compares the SHA-256 of the copied data (or of the source) with the destination part file.
func (j *job) verify(ctx context.Context, it item, part string, hasher hash.Hash) error {
	var want string
	if hasher != nil {
		want = hex.EncodeToString(hasher.Sum(nil))
	} else {
		var err error
		if want, err = vfs.Checksum(ctx, j.src.FS, it.src.Path, "sha256"); err != nil {
			return fmt.Errorf("verification: %w", err)
		}
	}
	got, err := vfs.Checksum(ctx, j.dst.FS, part, "sha256")
	if err != nil {
		return fmt.Errorf("verification: %w", err)
	}
	if got != want {
		return fmt.Errorf("verification failed: SHA-256 mismatch")
	}
	return nil
}

// moveInPlace moves the selected paths within one file system by renaming them.
func (j *job) moveInPlace(ctx context.Context) error {
	fsys := j.src.FS
	var total int64
	for _, p := range j.req.SrcPaths {
		if e, err := fsys.Lstat(ctx, p); err == nil {
			total += e.Size
		}
	}
	j.mu.Lock()
	j.info.TotalFiles, j.info.TotalBytes = len(j.req.SrcPaths), total
	j.mu.Unlock()
	for _, p := range j.req.SrcPaths {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := vfs.BaseName(p)
		dst := vfs.JoinPath(j.req.DstDir, name)
		j.mu.Lock()
		j.info.CurrentFile = name
		j.mu.Unlock()
		se, err := fsys.Lstat(ctx, p)
		if err != nil {
			j.fileError(name, err)
			continue
		}
		if dst == p {
			j.skip()
			continue
		}
		if de, err := fsys.Lstat(ctx, dst); err == nil {
			d, err := j.decision(ctx, name, se, de)
			if err != nil {
				return err
			}
			switch {
			case d == PolicySkip || d == PolicyResume:
				j.skip()
				continue
			case d == PolicyRename:
				n, err := vfs.NumberedName(ctx, fsys, j.req.DstDir, name)
				if err != nil {
					j.fileError(name, err)
					continue
				}
				dst = vfs.JoinPath(j.req.DstDir, n)
			case de.Type == model.FileTypeDir || se.Type == model.FileTypeDir:
				j.fileError(name, fmt.Errorf("cannot replace a folder: %w", fs.ErrExist))
				continue
			}
		}
		if err := fsys.Rename(ctx, p, dst); err != nil {
			j.fileError(name, err)
			continue
		}
		j.addBytes(se.Size)
		j.mu.Lock()
		j.info.DoneFiles++
		j.mu.Unlock()
	}
	return nil
}
