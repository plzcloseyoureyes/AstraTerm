package importer

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// SSH-36: optional live sync of ~/.ssh/config (desktop mode). When enabled, the importer re-reads the file (with its
// Include files) every few seconds and, when the content changed, mirrors it into a dedicated folder "~/.ssh/config"
// of the first admin's library: new hosts are added, changed ones updated and hosts that disappeared are removed.
// Connections it manages carry options._managedBy = "ssh-config" and options._sshConfigAlias = <alias>, so the user's
// own connections are never touched and renaming a synced connection does not re-create it. Fields that come from the
// file (address, login, protocol options) are authoritative; the user's name, colour, icon, favourite, tags, notes,
// key and secrets are kept. Private keys are never copied: an IdentityFile whose public key (<file>.pub) matches a
// key already stored in AstraTerm is linked, otherwise the connection uses automatic authentication (agent, password).

const (
	syncFolderName    = "~/.ssh/config"
	syncManagedMarker = "ssh-config"
	syncManagedKey    = "_managedBy"
	syncAliasKey      = "_sshConfigAlias"
	syncSettingKey    = "importerSyncSSHConfig"
	syncFolderSetting = "importerSyncFolderId"
	syncPollInterval  = 5 * time.Second
)

// syncStatus is the JSON status of the watcher.
type syncStatus struct {
	Supported bool       `json:"supported"`
	Enabled   bool       `json:"enabled"`
	Path      string     `json:"path"`
	Exists    bool       `json:"exists"`
	FolderID  string     `json:"folderId,omitempty"`
	LastSync  *time.Time `json:"lastSync,omitempty"`
	LastError string     `json:"lastError,omitempty"`
	Synced    int        `json:"synced"`
}

type syncManager struct {
	d    *app.Deps
	path string

	runMu sync.Mutex // serializes syncs (poll loop vs. the enable request)

	mu       sync.Mutex
	enabled  bool
	lastHash [32]byte
	hashed   bool
	lastSync *time.Time
	lastErr  string
	synced   int
	folderID string
}

func newSyncManager(d *app.Deps) *syncManager {
	home := userHomeDir()
	path := ""
	if home != "" {
		path = filepath.Join(home, ".ssh", "config")
	}
	return &syncManager{d: d, path: path}
}

func (m *syncManager) supported() bool {
	return m.d.Cfg != nil && m.d.Cfg.IsDesktop() && m.path != ""
}

// start loads the persisted enabled flag and, in desktop mode, runs the poll loop until the deps context is done.
func (m *syncManager) start() {
	if !m.supported() {
		return
	}
	var enabled bool
	if _, err := m.d.Store.Settings.GetJSON(m.d.Ctx, "global", syncSettingKey, &enabled); err == nil {
		m.mu.Lock()
		m.enabled = enabled
		m.mu.Unlock()
	}
	go m.loop()
}

func (m *syncManager) loop() {
	t := time.NewTicker(syncPollInterval)
	defer t.Stop()
	m.tick()
	for {
		select {
		case <-m.d.Ctx.Done():
			return
		case <-t.C:
			m.tick()
		}
	}
}

// tick syncs when enabled and the (Include-expanded) content changed since the last sync.
func (m *syncManager) tick() {
	m.mu.Lock()
	enabled := m.enabled
	m.mu.Unlock()
	if !enabled || m.d.Ctx.Err() != nil {
		return
	}
	m.syncIfChanged(false)
}

// syncIfChanged reads the config and syncs when its content differs from the last synced content (or force).
func (m *syncManager) syncIfChanged(force bool) {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	data, err := m.readConfig()
	var sum [32]byte
	if err == nil {
		sum = sha256.Sum256(data)
	} else if errors.Is(err, os.ErrNotExist) {
		data, err = nil, nil // a removed file syncs as an empty config
	}
	m.mu.Lock()
	unchanged := err == nil && m.hashed && sum == m.lastHash && !force
	m.mu.Unlock()
	if unchanged {
		return
	}
	var n int
	if err == nil {
		n, err = m.doSync(data)
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSync = &now
	if err != nil {
		m.lastErr = truncate(err.Error(), 300)
		m.hashed = false // retry on the next tick
		return
	}
	m.lastErr, m.lastHash, m.hashed, m.synced = "", sum, true, n
}

// readConfig returns ~/.ssh/config with its Include files inlined.
func (m *syncManager) readConfig() ([]byte, error) {
	data, err := readFileLimited(m.path, maxDiscoverFileBytes)
	if err != nil {
		return nil, err
	}
	return []byte(expandSSHConfigIncludes(string(data), filepath.Dir(m.path), 0)), nil
}

// setEnabled turns the watcher on/off and persists the flag; enabling triggers an immediate sync.
func (m *syncManager) setEnabled(enabled bool) error {
	if err := m.d.Store.Settings.SetJSON(m.d.Ctx, "global", syncSettingKey, enabled); err != nil {
		return err
	}
	m.mu.Lock()
	m.enabled = enabled
	m.mu.Unlock()
	if enabled {
		m.syncIfChanged(true)
	}
	return nil
}

func (m *syncManager) status() syncStatus {
	m.mu.Lock()
	st := syncStatus{
		Supported: m.supported(), Enabled: m.enabled, Path: m.path, FolderID: m.folderID,
		LastSync: m.lastSync, LastError: m.lastErr, Synced: m.synced,
	}
	m.mu.Unlock()
	if m.path != "" {
		if _, err := os.Stat(m.path); err == nil {
			st.Exists = true
		}
	}
	return st
}

// doSync mirrors the config into the admin's "~/.ssh/config" folder and returns the number of synced hosts.
func (m *syncManager) doSync(data []byte) (int, error) {
	ctx := m.d.Ctx
	admin, err := m.d.Store.Users.FirstAdmin(ctx)
	if err != nil {
		return 0, err
	}
	var conns []*pconn
	if strings.TrimSpace(string(data)) != "" {
		p, err := parseSSHConfig(data)
		if err != nil && !strings.Contains(err.Error(), "no concrete Host entries") {
			return 0, err
		}
		if p != nil {
			conns = p.conns
		}
	}
	folderID, err := m.ensureFolder(ctx, admin)
	if err != nil {
		return 0, err
	}
	existing, err := m.d.Store.Connections.ListByOwner(ctx, admin.ID)
	if err != nil {
		return 0, err
	}
	managed := map[string]*model.Connection{} // alias → managed connection
	for _, c := range existing {
		if c.Options.String(syncManagedKey, "") != syncManagedMarker {
			continue
		}
		alias := c.Options.String(syncAliasKey, "")
		if alias == "" && c.FolderID == folderID {
			alias = c.Name // synced before aliases were recorded
		}
		if alias != "" {
			managed[strings.ToLower(alias)] = c
		}
	}
	keys, _ := m.d.Store.Keys.ListByOwner(ctx, admin.ID)
	keyByFP := map[string]string{}
	for _, k := range keys {
		keyByFP[k.Fingerprint] = k.ID
	}

	idByTemp := map[string]string{}
	result := map[string]*model.Connection{}
	seen := map[string]bool{}
	for _, pc := range conns {
		alias := pc.conn.Name
		want := pc.conn
		want.Options = pc.conn.Options.Clone()
		want.Options[syncManagedKey] = syncManagedMarker
		want.Options[syncAliasKey] = alias
		wantKey := pc.keyPath != "" // an IdentityFile is configured
		keyID := ""
		if wantKey {
			keyID = keyByFP[publicKeyFingerprint(pc.keyPath+".pub")]
		}
		want.AuthMethod = model.AuthAuto
		if keyID != "" {
			want.AuthMethod = model.AuthKey
		}
		seen[strings.ToLower(alias)] = true
		if old, ok := managed[strings.ToLower(alias)]; ok {
			upd := old.Clone()
			upd.Protocol, upd.Host, upd.Port, upd.Username = want.Protocol, want.Host, want.Port, want.Username
			if keyID != "" {
				upd.KeyID = keyID // else keep a key the user attached
			}
			switch {
			case wantKey && upd.KeyID != "":
				upd.AuthMethod = model.AuthKey
			case upd.AuthMethod == model.AuthKey && upd.KeyID == "":
				upd.AuthMethod = model.AuthAuto
			}
			upd.Options = mergeSyncedOptions(old.Options, want.Options)
			if err := sanitizeConn(upd); err != nil {
				continue
			}
			if !syncedEqual(old, upd) {
				if err := m.d.Store.Connections.Update(ctx, upd); err != nil {
					return 0, err
				}
			}
			idByTemp[pc.tempID], result[pc.tempID] = upd.ID, upd
			continue
		}
		c := want
		c.ID, c.OwnerID, c.FolderID, c.KeyID = "", admin.ID, folderID, keyID
		if err := sanitizeConn(&c); err != nil {
			continue
		}
		if err := m.d.Store.Connections.Create(ctx, &c); err != nil {
			return 0, err
		}
		idByTemp[pc.tempID], result[pc.tempID] = c.ID, &c
	}
	// Jump chains: aliases of the file become the synced connections' ids.
	for _, pc := range conns {
		c := result[pc.tempID]
		if c == nil {
			continue
		}
		var chain []string
		for _, h := range pc.hops {
			if h.ref != "" {
				if id := idByTemp[h.ref]; id != "" {
					chain = append(chain, id)
				}
				continue
			}
			chain = append(chain, h.spec())
		}
		if slices.Equal(chain, c.Options.Strings("jumpHosts")) {
			continue
		}
		upd := c.Clone()
		if len(chain) > 0 {
			upd.Options["jumpHosts"] = chain
		} else {
			delete(upd.Options, "jumpHosts")
		}
		if err := m.d.Store.Connections.Update(ctx, upd); err != nil {
			return 0, err
		}
	}
	// Prune managed connections whose alias vanished from the config.
	pruned := 0
	for alias, c := range managed {
		if !seen[alias] {
			if err := m.d.Store.Connections.Delete(ctx, c.ID); err == nil {
				pruned++
			}
		}
	}
	m.d.Audit.Log(ctx, "import.ssh_config.sync", folderID, map[string]any{"synced": len(result), "removed": pruned})
	return len(result), nil
}

// syncOwnedKeys are the option keys the sync derives from ssh_config (replaced on every sync; others are kept).
var syncOwnedKeys = []string{"jumpHosts", "proxyCommand", "keepAliveSec", "compression", "agentForwarding",
	"x11Forwarding", "remoteCommand", "connectTimeoutSec", "ciphers", "kex", "macs", "hostKeyAlgorithms", "env",
	"forwards", syncManagedKey, syncAliasKey}

// mergeSyncedOptions keeps the user's own option keys and takes every ssh_config-derived key from want.
func mergeSyncedOptions(old, want model.Options) model.Options {
	out := old.Clone()
	if out == nil {
		out = model.Options{}
	}
	for _, k := range syncOwnedKeys {
		if k == "jumpHosts" {
			continue // resolved in the second pass
		}
		if v, ok := want[k]; ok {
			out[k] = v
		} else {
			delete(out, k)
		}
	}
	return out
}

// syncedEqual reports whether an update would change nothing the sync manages.
func syncedEqual(a, b *model.Connection) bool {
	if a.Protocol != b.Protocol || a.Host != b.Host || a.Port != b.Port || a.Username != b.Username ||
		a.KeyID != b.KeyID || a.AuthMethod != b.AuthMethod {
		return false
	}
	ja, _ := a.Options.MarshalJSON()
	jb, _ := b.Options.MarshalJSON()
	return string(ja) == string(jb)
}

// publicKeyFingerprint returns the SHA256 fingerprint of an OpenSSH public key file ("" when unreadable).
func publicKeyFingerprint(path string) string {
	data, err := readFileLimited(expandTilde(path), 16<<10)
	if err != nil {
		return ""
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(pub)
}

// ensureFolder returns the sync folder: the remembered one when it still exists, else a root folder named
// "~/.ssh/config", else a new one.
func (m *syncManager) ensureFolder(ctx context.Context, admin *model.User) (string, error) {
	var remembered string
	_, _ = m.d.Store.Settings.GetJSON(ctx, "global", syncFolderSetting, &remembered)
	folders, err := m.d.Store.Folders.ListByOwner(ctx, admin.ID)
	if err != nil {
		return "", err
	}
	id := ""
	for _, f := range folders {
		if remembered != "" && f.ID == remembered {
			id = f.ID
			break
		}
	}
	if id == "" {
		for _, f := range folders {
			if f.ParentID == "" && f.Name == syncFolderName {
				id = f.ID
				break
			}
		}
	}
	if id == "" {
		f := &model.Folder{OwnerID: admin.ID, Name: syncFolderName, Icon: "lucide:Cog"}
		if err := m.d.Store.Folders.Create(ctx, f); err != nil {
			return "", err
		}
		id = f.ID
	}
	if id != remembered {
		_ = m.d.Store.Settings.SetJSON(ctx, "global", syncFolderSetting, id)
	}
	m.mu.Lock()
	m.folderID = id
	m.mu.Unlock()
	return id, nil
}
