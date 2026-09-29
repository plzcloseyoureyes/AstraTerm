package vfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/core"
	"github.com/plzcloseyoureyes/astraterm/internal/events"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/netguard"
	"github.com/plzcloseyoureyes/astraterm/internal/sshx"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// DefaultIdleTimeout closes handles that were not used for this long (and are not pinned by a transfer).
const DefaultIdleTimeout = 10 * time.Minute

// Registry owns every open file system handle. Handles belong to the user who opened them; every lookup checks it.
type Registry struct {
	d   *app.Deps
	c   *core.Core
	log *slog.Logger
	ctx context.Context

	// IdleTimeout overrides DefaultIdleTimeout (tests).
	IdleTimeout time.Duration

	mu      sync.Mutex
	handles map[string]*Handle
}

// Handle is one open file system (the reply of POST /api/fs).
type Handle struct {
	ID           string       `json:"id"`
	Kind         string       `json:"kind"`
	Driver       string       `json:"driver"`
	Home         string       `json:"home"`
	UserHome     string       `json:"userHome"`
	Root         string       `json:"root"`
	Label        string       `json:"label"`
	Capabilities Capabilities `json:"capabilities"`
	SessionID    string       `json:"sessionId,omitempty"`
	ConnectionID string       `json:"connectionId,omitempty"`
	Protocol     string       `json:"protocol,omitempty"`
	Host         string       `json:"host,omitempty"`
	Username     string       `json:"username,omitempty"`
	Port         int          `json:"port,omitempty"`
	CreatedAt    time.Time    `json:"createdAt"`

	OwnerID string `json:"-"`
	FS      FS     `json:"-"`

	mu         sync.Mutex
	lastUsed   time.Time
	pins       int
	closing    bool
	closed     bool
	uploads    map[string]*uploadState
	stagingDir string // local staging of chunked uploads for drivers without partial writes
}

// NewRegistry creates the handle registry; handles are closed when d.Ctx ends.
func NewRegistry(d *app.Deps, c *core.Core) *Registry {
	ctx := context.Background()
	log := slog.Default()
	if d != nil && d.Ctx != nil {
		ctx = d.Ctx
	}
	if d != nil && d.Log != nil {
		log = d.Log
	}
	r := &Registry{d: d, c: c, log: log.With("module", "vfs"), ctx: ctx, IdleTimeout: DefaultIdleTimeout,
		handles: map[string]*Handle{}}
	go r.janitor()
	return r
}

func (r *Registry) janitor() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			r.closeAll()
			return
		case <-t.C:
			r.reapIdle()
		}
	}
}

func (r *Registry) reapIdle() {
	ttl := r.IdleTimeout
	if ttl <= 0 {
		ttl = DefaultIdleTimeout
	}
	r.mu.Lock()
	var stale []*Handle
	for id, h := range r.handles {
		h.mu.Lock()
		idle := h.pins == 0 && time.Since(h.lastUsed) > ttl
		h.mu.Unlock()
		if idle {
			stale = append(stale, h)
			delete(r.handles, id)
		}
	}
	r.mu.Unlock()
	for _, h := range stale {
		r.log.Debug("closing idle file system handle", "fs", h.ID, "driver", h.Driver)
		h.shutdown()
	}
}

func (r *Registry) closeAll() {
	r.mu.Lock()
	all := make([]*Handle, 0, len(r.handles))
	for _, h := range r.handles {
		all = append(all, h)
	}
	r.handles = map[string]*Handle{}
	r.mu.Unlock()
	for _, h := range all {
		h.shutdown()
	}
}

// shutdown marks the handle closing and closes its FS once no pin is left.
func (h *Handle) shutdown() {
	h.mu.Lock()
	h.closing = true
	done := h.pins == 0 && !h.closed
	if done {
		h.closed = true
	}
	h.mu.Unlock()
	if done {
		h.cleanupUploads()
		_ = h.FS.Close()
	}
}

// Acquire returns user's handle id pinned against idle close; call release when done. Unknown, closed or foreign
// handles → 404 fs_not_found.
func (r *Registry) Acquire(user *model.User, id string) (*Handle, func(), error) {
	if user == nil {
		return nil, nil, httpx.ErrUnauthorized
	}
	r.mu.Lock()
	h := r.handles[id]
	r.mu.Unlock()
	if h == nil || h.OwnerID != user.ID {
		return nil, nil, errFSNotFound
	}
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return nil, nil, errFSNotFound
	}
	h.pins++
	h.lastUsed = time.Now()
	h.mu.Unlock()
	return h, sync.OnceFunc(func() {
		h.mu.Lock()
		h.pins--
		h.lastUsed = time.Now()
		done := h.closing && h.pins == 0 && !h.closed
		if done {
			h.closed = true
		}
		h.mu.Unlock()
		if done {
			h.cleanupUploads()
			_ = h.FS.Close()
		}
	}), nil
}

// Close closes user's handle (a transfer still using it keeps it alive until it finishes).
func (r *Registry) Close(user *model.User, id string) error {
	r.mu.Lock()
	h := r.handles[id]
	if h == nil || user == nil || h.OwnerID != user.ID {
		r.mu.Unlock()
		return errFSNotFound
	}
	delete(r.handles, id)
	r.mu.Unlock()
	h.shutdown()
	return nil
}

// closeSession closes the handles bound to a runtime session that ended.
func (r *Registry) closeSession(sessionID string) {
	r.mu.Lock()
	var hs []*Handle
	for id, h := range r.handles {
		if h.SessionID == sessionID {
			hs = append(hs, h)
			delete(r.handles, id)
		}
	}
	r.mu.Unlock()
	for _, h := range hs {
		h.shutdown()
	}
}

// List returns user's open handles.
func (r *Registry) List(user *model.User) []*Handle {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Handle
	for _, h := range r.handles {
		if user != nil && h.OwnerID == user.ID {
			out = append(out, h)
		}
	}
	slices.SortFunc(out, func(a, b *Handle) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

// Register adds an already opened FS as a handle of user (tests, other modules).
func (r *Registry) Register(user *model.User, h *Handle) *Handle {
	h.ID = model.NewID()
	h.OwnerID = user.ID
	h.CreatedAt = time.Now().UTC()
	h.lastUsed = time.Now()
	if h.Root == "" {
		h.Root = "/"
	}
	h.stagingDir = filepath.Join(r.tmpDir(), "uploads", h.ID)
	r.mu.Lock()
	r.handles[h.ID] = h
	r.mu.Unlock()
	return h
}

// ---- opening ------------------------------------------------------------------------------------------------------

// OpenRequest is the body of POST /api/fs.
type OpenRequest struct {
	SessionID    string                `json:"sessionId,omitempty"`
	ConnectionID string                `json:"connectionId,omitempty"`
	Local        bool                  `json:"local,omitempty"`
	Quick        *term.QuickConnection `json:"quick,omitempty"`
	Sudo         bool                  `json:"sudo,omitempty"`
}

// Open opens a file system for user.
func (r *Registry) Open(ctx context.Context, user *model.User, req OpenRequest) (*Handle, error) {
	if user == nil {
		return nil, httpx.ErrUnauthorized
	}
	var h *Handle
	var err error
	switch {
	case req.SessionID != "":
		h, err = r.openSession(ctx, user, req.SessionID, req.Sudo)
	case req.ConnectionID != "":
		if !model.ValidID(req.ConnectionID) {
			return nil, httpx.ErrNotFound
		}
		var conn *model.Connection
		var secrets map[string]string
		if conn, secrets, err = r.d.ResolveConnection(ctx, user, req.ConnectionID); err != nil {
			return nil, err
		}
		h, err = r.openConnection(ctx, user, conn, secrets, req.Sudo, false)
	case req.Quick != nil:
		var conn *model.Connection
		var secrets map[string]string
		if conn, secrets, err = r.quickConnection(ctx, user, req.Quick); err != nil {
			return nil, err
		}
		h, err = r.openConnection(ctx, user, conn, secrets, req.Sudo, true)
	case req.Local:
		h, err = r.openLocal(ctx, user)
	default:
		return nil, httpx.BadRequest("sessionId, connectionId, quick or local is required")
	}
	if err != nil {
		return nil, err
	}
	return r.Register(user, h), nil
}

func (r *Registry) openSession(ctx context.Context, user *model.User, sessionID string, sudo bool) (*Handle, error) {
	if r.c == nil || r.c.Sessions == nil || r.c.SSH == nil {
		return nil, httpx.NotFound("session not found")
	}
	s := r.c.Sessions.Get(sessionID)
	if s == nil || s.OwnerID != user.ID {
		return nil, httpx.NotFound("session not found")
	}
	if s.Protocol != model.ProtoSSH {
		return nil, httpx.BadRequest(fmt.Sprintf("the %s session has no file browser (SSH sessions only)", s.Protocol))
	}
	conn := s.Connection()
	if conn == nil {
		return nil, errNotConn
	}
	src := &sshSource{host: conn.Host, acquire: func(ctx context.Context) (*sshx.Client, func(), error) {
		return r.c.SSH.ForSession(ctx, user, sessionID)
	}}
	secrets := func(ctx context.Context) map[string]string {
		_, sec, err := s.Resolve(ctx)
		if err != nil {
			return nil
		}
		return sec
	}
	h, err := r.openSSH(ctx, user, src, conn, secrets, sudo, sessionID)
	if err != nil {
		return nil, err
	}
	h.SessionID = sessionID
	if info := s.Info(); info.ConnectionID != "" {
		h.ConnectionID = info.ConnectionID
	}
	if t := s.Title(); t != "" {
		h.Label = t
	}
	return h, nil
}

// openConnection opens a saved (or quick) connection's file system by protocol.
func (r *Registry) openConnection(ctx context.Context, user *model.User, conn *model.Connection, secrets map[string]string, sudo, quick bool) (*Handle, error) {
	if secrets == nil {
		secrets = map[string]string{}
	}
	var h *Handle
	var err error
	switch strings.ToLower(string(conn.Protocol)) {
	case string(model.ProtoSSH), string(model.ProtoSFTP), string(model.ProtoMosh):
		if r.c == nil || r.c.SSH == nil {
			return nil, httpx.BadRequest("SSH is not available")
		}
		c := conn.Clone()
		src := &sshSource{host: conn.Host}
		if quick || conn.ID == "" {
			src.acquire = func(ctx context.Context) (*sshx.Client, func(), error) {
				return r.c.SSH.Acquire(ctx, user, c, secrets)
			}
		} else {
			id := conn.ID
			src.acquire = func(ctx context.Context) (*sshx.Client, func(), error) {
				return r.c.SSH.Get(ctx, user, id)
			}
		}
		h, err = r.openSSH(ctx, user, src, conn, func(context.Context) map[string]string { return secrets }, sudo, "")
	case string(model.ProtoFTP), "ftps":
		h, err = r.openFTP(ctx, user, conn, secrets)
	case string(model.ProtoS3):
		h, err = r.openS3(ctx, user, conn, secrets)
	case "webdav", "webdavs", "dav":
		h, err = r.openWebDAV(ctx, user, conn, secrets)
	case "smb", "cifs":
		h, err = r.openSMB(ctx, user, conn, secrets)
	default:
		return nil, httpx.BadRequest(fmt.Sprintf("connections of protocol %q have no file browser", conn.Protocol))
	}
	if err != nil {
		return nil, err
	}
	if !quick {
		h.ConnectionID = conn.ID
	}
	return h, nil
}

// openSSH picks the SSH driver: sudo-sftp (browse as root), SFTP, or the SCP / shell fallback.
func (r *Registry) openSSH(ctx context.Context, user *model.User, src *sshSource, conn *model.Connection,
	secrets func(context.Context) map[string]string, sudo bool, sessionID string) (*Handle, error) {
	mode := strings.ToLower(strings.TrimSpace(conn.Options.String("sshBrowser", "sftp")))
	switch mode {
	case "none", "off", "disabled", "false":
		return nil, httpx.NewError(http.StatusConflict, codeSSHBrowserDisabled, "the SSH browser is disabled for this connection")
	}
	if _, err := src.client(ctx); err != nil {
		if errors.Is(err, term.ErrNotConnected) {
			return nil, errNotConn
		}
		return nil, err
	}
	fail := func(err error) (*Handle, error) {
		src.close()
		return nil, err
	}
	// The exec probe runs while the SFTP subsystem starts.
	execCh := make(chan bool, 1)
	go func() { execCh <- src.probeExec(ctx) }()
	var sftpErr error
	if mode != "scp" && !sudo {
		cl, err := src.client(ctx)
		if err != nil {
			return fail(err)
		}
		_, sftpErr = cl.SFTP()
	}
	execOK := <-execCh
	var sudoH *sudoHelper
	if execOK {
		sudoH = &sudoHelper{src: src, getPass: r.sudoPasswords(user, conn, secrets, sessionID),
			onSave: func(ctx context.Context, pw string) { r.saveSecret(ctx, user, conn, model.SecretSudoPassword, pw) }}
	}
	h := &Handle{Kind: "sftp", Protocol: string(conn.Protocol), Host: conn.Host, Username: conn.Username, Port: conn.Port}
	switch {
	case sudo:
		if !execOK {
			return fail(httpx.BadRequest("browsing as root needs shell access on the server"))
		}
		server, err := sudoH.findSFTPServer(ctx, conn.Options.String("sftpServerCommand", ""))
		if err != nil {
			return fail(err)
		}
		ch := &sudoChannel{h: sudoH, server: server}
		if _, err := ch.get(ctx); err != nil {
			return fail(err)
		}
		// Everything runs through the root SFTP channel: commands over exec would run as the unprivileged user.
		f := newSFTPFS(src, false, nil)
		f.root = true
		f.owners = newOwnerCache(src) // getent works without privileges
		f.getSC, f.closer = ch.get, ch.close
		h.FS, h.Driver = f, "sudo-sftp"
	case mode != "scp" && sftpErr == nil:
		h.FS, h.Driver = newSFTPFS(src, execOK, sudoH), "sftp"
	case execOK:
		if sftpErr != nil && !isSubsystemError(sftpErr) {
			r.log.Debug("sftp unavailable, using the shell fallback", "host", conn.Host, "err", sftpErr)
		}
		h.FS, h.Driver = newShellFS(src, sudoH), "scp"
	case sftpErr != nil:
		return fail(fmt.Errorf("the server offers neither SFTP nor shell access: %w", sftpErr))
	default:
		return fail(httpx.BadRequest("SCP mode needs shell access on the server"))
	}
	userHome, err := h.FS.Home(ctx)
	if err != nil || userHome == "" {
		userHome = "/"
	}
	h.UserHome = userHome
	h.Home = r.startDir(ctx, h.FS, conn, userHome)
	h.Label = connLabel(conn)
	h.Capabilities = capsFor(h.FS, h.Driver, execOK && h.Driver != "sudo-sftp")
	return h, nil
}

// startDir resolves the configured start folder (sftpRoot / initialPath) against the login folder.
func (r *Registry) startDir(ctx context.Context, fsys FS, conn *model.Connection, home string) string {
	start := strings.TrimSpace(conn.Options.String("sftpRoot", ""))
	if start == "" {
		start = strings.TrimSpace(conn.Options.String("initialPath", ""))
	}
	if start == "" {
		return home
	}
	p, err := cleanPath(start, home)
	if err != nil {
		return home
	}
	if e, err := fsys.Stat(ctx, p); err != nil || e.Type != model.FileTypeDir {
		return home
	}
	return p
}

func connLabel(conn *model.Connection) string {
	if conn.Name != "" {
		return conn.Name
	}
	h := conn.Host
	if conn.Username != "" {
		h = conn.Username + "@" + h
	}
	if h == "" {
		return string(conn.Protocol)
	}
	return h
}

// capsFor derives the capabilities of a driver.
func capsFor(fsys FS, driver string, execOK bool) Capabilities {
	c := Capabilities{Checksum: true, Search: true, Archive: true, Extract: true, Copy: true, Resume: true}
	switch driver {
	case "sftp", "sudo-sftp", "scp":
		c.Chmod, c.Chown, c.Symlink, c.Mtime = true, true, true, true
		c.Exec, c.Sudo = execOK, execOK && driver != "sudo-sftp"
		c.Space = true
		if f, ok := fsys.(*sftpFS); ok {
			c.Hardlink = f.hardlink
			c.Space = f.statvfs || execOK
		} else {
			c.Hardlink = true
		}
	case "local":
		unix := runtime.GOOS != "windows"
		c.Chmod, c.Chown, c.Symlink, c.Hardlink, c.Mtime = unix, unix, unix, unix, true
		c.Space = runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "freebsd"
	case "ftp":
		if f, ok := fsys.(*ftpFS); ok {
			c.Mtime = f.canSet
		}
	case "s3":
		c.Presign = true
	case "smb":
		c.Mtime, c.Space = true, true
	}
	return c
}

// ---- local --------------------------------------------------------------------------------------------------------

// localRootEnv jails the local file system in server mode (overrides the global setting files.localRoot).
const localRootEnv = "ASTRATERM_LOCAL_FS_ROOT"

func (r *Registry) openLocal(ctx context.Context, user *model.User) (*Handle, error) {
	server := r.d != nil && r.d.Cfg != nil && r.d.Cfg.IsServer()
	if server && !user.IsAdmin() {
		return nil, httpx.Forbidden("the local file system is only available to administrators in server mode")
	}
	root := ""
	if server {
		root = r.localRoot(ctx)
	}
	l, err := newLocalFS(root)
	if err != nil {
		return nil, err
	}
	label := "Local files"
	if hn, err := os.Hostname(); err == nil && hn != "" {
		label = hn
	}
	h := &Handle{Kind: "local", Driver: "local", FS: l, Home: l.home, UserHome: l.home, Label: label, Root: "/"}
	h.Capabilities = capsFor(l, "local", false)
	return h, nil
}

// localRoot is the jail of the local file system in server mode: $ASTRATERM_LOCAL_FS_ROOT, else the global setting
// files.localRoot, else <data dir>/files.
func (r *Registry) localRoot(ctx context.Context) string {
	if v := strings.TrimSpace(os.Getenv(localRootEnv)); v != "" {
		return v
	}
	if r.d != nil && r.d.Store != nil {
		var sec map[string]any
		if ok, err := r.d.Store.Settings.GetJSON(ctx, store.ScopeGlobal, "files", &sec); err == nil && ok {
			if v, _ := sec["localRoot"].(string); strings.TrimSpace(v) != "" {
				return filepath.Clean(strings.TrimSpace(v))
			}
		}
	}
	if r.d != nil && r.d.Cfg != nil {
		return r.d.Cfg.Path("files")
	}
	return filepath.Join(os.TempDir(), "astraterm-files")
}

// ---- helpers: dialing, prompts, secrets ---------------------------------------------------------------------------

// dialer returns a function dialing conn's host (or any address) through its proxy / jump chain. The SSH pool's
// Dialer applies the SSRF policy (internal/netguard) to direct routes and to the proxy / first hop; the fallback
// dialer applies it too.
func (r *Registry) dialer(ctx context.Context, user *model.User, conn *model.Connection, secrets map[string]string) (func(ctx context.Context, addr string) (net.Conn, error), func(), error) {
	if r.c != nil && r.c.SSH != nil {
		d, err := r.c.SSH.Dialer(ctx, user, conn, secrets)
		if err != nil {
			return nil, nil, err
		}
		return func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) },
			func() { d.Close() }, nil
	}
	nd := netguard.ForUser(r.d, user).Dialer(30 * time.Second)
	return func(ctx context.Context, addr string) (net.Conn, error) { return nd.DialContext(ctx, "tcp", addr) }, func() {}, nil
}

// askSecret prompts the user for a missing secret (password, secret key) through the events broker.
func (r *Registry) askSecret(ctx context.Context, user *model.User, conn *model.Connection, key, title, label string) (string, bool, error) {
	if r.d == nil || r.d.Events == nil {
		return "", false, fmt.Errorf("%s required: %w", strings.ToLower(title), fs.ErrPermission)
	}
	p := model.Prompt{Kind: model.PromptPassword, Title: title, Message: connLabel(conn),
		Fields: []model.PromptField{{Label: label, Echo: false}}, AllowSave: r.canSave(user, conn)}
	if conn.ID != "" {
		p.ConnectionID = conn.ID
	}
	resp, err := r.d.Events.Prompt(ctx, user.ID, p)
	if err != nil {
		if errors.Is(err, events.ErrNoInteractiveClient) || errors.Is(err, events.ErrPromptTimeout) {
			return "", false, fmt.Errorf("%s required but no answer was given: %w", strings.ToLower(title), fs.ErrPermission)
		}
		return "", false, err
	}
	if !resp.Accept || len(resp.Values) == 0 {
		return "", false, fmt.Errorf("canceled: %w", fs.ErrPermission)
	}
	if resp.Save {
		r.saveSecret(ctx, user, conn, key, resp.Values[0])
	}
	return resp.Values[0], resp.Save, nil
}

func (r *Registry) canSave(user *model.User, conn *model.Connection) bool {
	return conn != nil && conn.ID != "" && user != nil && conn.OwnerID == user.ID && r.d != nil && r.d.Vault != nil &&
		!r.d.Vault.Locked()
}

// sudoPasswords yields sudo password candidates: the sudoPassword secret, the login password, then prompts.
func (r *Registry) sudoPasswords(user *model.User, conn *model.Connection, secrets func(context.Context) map[string]string, sessionID string) passwordSource {
	return func(ctx context.Context, attempt int, lastFailed bool) (string, bool, error) {
		switch attempt {
		case 0, 1:
			key := model.SecretSudoPassword
			if attempt == 1 {
				key = model.SecretPassword
			}
			if secrets == nil {
				return "", false, nil
			}
			return secrets(ctx)[key], false, nil
		}
		if attempt > 5 || r.d == nil || r.d.Events == nil {
			return "", false, errNoMorePasswords
		}
		msg := "Password for sudo on " + connLabel(conn)
		if lastFailed && attempt > 2 {
			msg = "Sorry, try again. " + msg
		}
		p := model.Prompt{Kind: model.PromptPassword, Title: "sudo password", Message: msg, SessionID: sessionID,
			Fields: []model.PromptField{{Label: "Password", Echo: false}}, AllowSave: r.canSave(user, conn)}
		if conn.ID != "" {
			p.ConnectionID = conn.ID
		}
		resp, err := r.d.Events.Prompt(ctx, user.ID, p)
		if err != nil {
			if errors.Is(err, events.ErrNoInteractiveClient) || errors.Is(err, events.ErrPromptTimeout) {
				return "", false, fmt.Errorf("sudo password required: %w", fs.ErrPermission)
			}
			return "", false, err
		}
		if !resp.Accept || len(resp.Values) == 0 {
			return "", false, fmt.Errorf("sudo canceled: %w", fs.ErrPermission)
		}
		return resp.Values[0], resp.Save, nil
	}
}

// saveSecret stores a prompt answer in the owner's saved connection (vault-encrypted).
func (r *Registry) saveSecret(ctx context.Context, user *model.User, conn *model.Connection, key, value string) {
	if !r.canSave(user, conn) || r.d.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	c, err := r.d.Store.Connections.Get(ctx, conn.ID)
	if err != nil || c.OwnerID != user.ID {
		return
	}
	secrets, err := r.d.Vault.OpenJSON(c.SecretsEnc)
	if err != nil {
		return
	}
	secrets[key] = value
	enc, err := r.d.Vault.SealJSON(secrets)
	if err != nil {
		return
	}
	c.SecretsEnc = enc
	keys := make([]string, 0, len(secrets))
	for k, v := range secrets {
		if v != "" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	c.SecretKeys = keys
	if err := r.d.Store.Connections.Update(ctx, c); err != nil {
		r.log.Warn("cannot save connection secret", "connection", c.ID, "err", err)
		return
	}
	if r.d.Audit != nil {
		r.d.Audit.LogUser(ctx, user, "connection.secret.save", c.ID, map[string]any{"key": key})
	}
}

// quickConnection validates an ad-hoc connection spec (like term's quick connect): the identity must be the
// caller's; secrets come only from the identity and explicit password / secrets fields.
func (r *Registry) quickConnection(ctx context.Context, user *model.User, q *term.QuickConnection) (*model.Connection, map[string]string, error) {
	c := &model.Connection{
		Name:       strings.TrimSpace(q.Name),
		Protocol:   q.Protocol,
		Host:       strings.TrimSpace(q.Host),
		Port:       q.Port,
		Username:   strings.TrimSpace(q.Username),
		IdentityID: q.IdentityID,
		KeyID:      q.KeyID,
		AuthMethod: q.AuthMethod,
		Options:    q.Options.Clone(),
		OwnerID:    user.ID,
	}
	if c.Protocol == "" {
		c.Protocol = model.ProtoSFTP
	}
	if !model.ValidProtocol(c.Protocol) {
		return nil, nil, httpx.BadRequest("invalid protocol")
	}
	if c.AuthMethod == "" {
		c.AuthMethod = model.AuthAuto
	}
	if !model.ValidAuthMethod(c.AuthMethod) {
		return nil, nil, httpx.BadRequest("invalid authMethod")
	}
	if c.Port < 0 || c.Port > 65535 {
		return nil, nil, httpx.BadRequest("port must be between 1 and 65535")
	}
	if c.Port == 0 {
		c.Port = model.DefaultPort(c.Protocol)
	}
	if len(c.Host) > 255 || strings.ContainsFunc(c.Host, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return nil, nil, httpx.BadRequest("invalid host")
	}
	if c.Host == "" && c.Protocol != model.ProtoS3 {
		return nil, nil, httpx.BadRequest("host is required")
	}
	if len(c.Username) > 256 || strings.ContainsFunc(c.Username, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return nil, nil, httpx.BadRequest("invalid username")
	}
	if c.KeyID != "" && !model.ValidID(c.KeyID) {
		return nil, nil, httpx.BadRequest("invalid keyId")
	}
	secrets := map[string]string{}
	for k, v := range q.Secrets {
		if k != "" && v != "" {
			secrets[k] = v
		}
	}
	if q.Password != "" {
		secrets[model.SecretPassword] = q.Password
	}
	if c.IdentityID != "" {
		if r.d == nil || r.d.Store == nil || !model.ValidID(c.IdentityID) {
			return nil, nil, httpx.NotFound("identity not found")
		}
		ident, err := r.d.Store.Identities.Get(ctx, c.IdentityID)
		if err != nil || ident.OwnerID != user.ID {
			return nil, nil, httpx.NotFound("identity not found")
		}
		if c.Username == "" {
			c.Username = ident.Username
		}
		if c.KeyID == "" {
			c.KeyID = ident.KeyID
		}
		is, err := r.d.Vault.OpenJSON(ident.SecretsEnc)
		if err != nil {
			if errors.Is(err, model.ErrLocked) {
				return nil, nil, httpx.ErrLocked
			}
			return nil, nil, fmt.Errorf("decrypt identity secrets: %w", err)
		}
		for k, v := range is {
			if _, ok := secrets[k]; !ok {
				secrets[k] = v
			}
		}
	}
	c.Normalize()
	return c, secrets, nil
}

// hostPort formats host and port for URLs.
func hostPort(host string, port, def int) string {
	if port == 0 {
		port = def
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// tmpDir is the local scratch directory (staged uploads, zip extraction).
func (r *Registry) tmpDir() string {
	if r.d != nil && r.d.Cfg != nil && r.d.Cfg.DataDir != "" {
		return r.d.Cfg.TmpDir()
	}
	return filepath.Join(os.TempDir(), "astraterm-tmp")
}
