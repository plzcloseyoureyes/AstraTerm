package keys

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"golang.org/x/crypto/ssh"

	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
)

// agentStatus is the response of GET /api/agent/status.
type agentStatus struct {
	Supported  bool       `json:"supported"` // desktop mode
	Reason     string     `json:"reason,omitempty"`
	Running    bool       `json:"running"`
	Owner      bool       `json:"owner"` // running for the caller
	SocketPath string     `json:"socketPath,omitempty"`
	Platform   string     `json:"platform"` // GOOS of the Termstead host (shell snippets)
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	KeyCount   int        `json:"keyCount"`
	Locked     bool       `json:"locked"`
	// LockedByClient: locked with ssh-add -x (unlock with ssh-add -X, or from Termstead).
	LockedByClient bool       `json:"lockedByClient"`
	VaultLocked    bool       `json:"vaultLocked"`
	Autostart      bool       `json:"autostart"`
	Clients        int        `json:"clients"`
	Signatures     int64      `json:"signatures"`
	LastUsedAt     *time.Time `json:"lastUsedAt,omitempty"`
}

func (s *agentService) status(ctx context.Context, u *model.User) agentStatus {
	d := s.h.d
	st := agentStatus{Supported: d.Cfg != nil && d.Cfg.IsDesktop(), Platform: runtime.GOOS,
		VaultLocked: d.Vault == nil || d.Vault.Locked()}
	if !st.Supported {
		st.Reason = errAgentUnsupported.Error()
	}
	cfg := loadSettings(ctx, d.Store, u.ID)
	st.Autostart = cfg.AgentAutostart
	sock := s.current()
	if sock == nil {
		return st
	}
	st.Running = true
	if sock.owner.ID != u.ID {
		return st
	}
	st.Owner, st.SocketPath, st.Clients, st.Signatures = true, sock.endpoint, sock.open(), s.signs.Load()
	started := sock.started.UTC()
	st.StartedAt = &started
	r := s.ring(u.ID)
	r.mu.Lock()
	st.Locked, st.LockedByClient = r.locked, r.locked && r.lockPass != nil
	if !r.lastUse.IsZero() {
		t := r.lastUse.UTC()
		st.LastUsedAt = &t
	}
	r.mu.Unlock()
	if keys, err := r.view(ctx, agentOrigin{kind: originTermstead}, true, "").List(); err == nil {
		st.KeyCount = len(keys)
	}
	return st
}

// agentKeyView is one key of GET /api/agent/keys.
type agentKeyView struct {
	ID              string     `json:"id"`     // "k:<stored key id>" | "a:<hash>" (added with ssh-add)
	Source          string     `json:"source"` // stored | added
	KeyID           string     `json:"keyId,omitempty"`
	Name            string     `json:"name"`
	Type            string     `json:"type"`
	Bits            int        `json:"bits"`
	Fingerprint     string     `json:"fingerprint"`
	Comment         string     `json:"comment"`
	Loaded          bool       `json:"loaded"`          // offered by the agent right now
	Excluded        bool       `json:"excluded"`        // excluded in the settings
	Unloaded        bool       `json:"unloaded"`        // removed for this agent run (ssh-add -d / Termstead)
	Unlocked        bool       `json:"unlocked"`        // decrypted in memory
	NeedsPassphrase bool       `json:"needsPassphrase"` // encrypted and the passphrase is not remembered
	Confirm         bool       `json:"confirm"`         // confirmation required on each use (ssh-add -c)
	Certificate     bool       `json:"certificate"`
	ExpiresAt       *time.Time `json:"expiresAt,omitempty"`
}

func addedKeyID(pub ssh.PublicKey) string {
	sum := sha256.Sum256(pub.Marshal())
	return "a:" + hex.EncodeToString(sum[:12])
}

func (s *agentService) keysOf(ctx context.Context, u *model.User) ([]agentKeyView, error) {
	d := s.h.d
	keys, err := d.Store.Keys.ListByOwner(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	cfg := loadSettings(ctx, d.Store, u.ID)
	vaultLocked := d.Vault == nil || d.Vault.Locked()
	r := s.ring(u.ID)
	running := s.running(u.ID) != nil
	r.mu.Lock()
	locked := r.locked
	unloaded := map[string]bool{}
	for k, v := range r.unloaded {
		unloaded[k] = v
	}
	unlocked := map[string]*cachedSigner{}
	for k, v := range r.cache {
		unlocked[k] = v
	}
	r.mu.Unlock()
	t := now()
	out := []agentKeyView{}
	for _, k := range keys {
		if len(k.PrivateKeyEnc) == 0 {
			continue
		}
		v := agentKeyView{ID: "k:" + k.ID, Source: "stored", KeyID: k.ID, Name: k.Name, Type: k.Type, Bits: k.Bits,
			Fingerprint: k.Fingerprint, Comment: k.Comment, Excluded: cfg.excluded(k.ID), Unloaded: unloaded[k.ID],
			NeedsPassphrase: k.HasPassphrase && len(k.PassphraseEnc) == 0}
		if c := unlocked[k.ID]; c != nil && c.stamp == materialStamp(k) {
			v.Unlocked = true
			if !c.expires.IsZero() {
				e := c.expires.UTC()
				v.ExpiresAt = &e
			}
		}
		if pub, err := keyPublic(k); err == nil {
			if cert := storedCertificate(k.Certificate, pub); cert != nil && describeCert(cert, t).Status == certValid {
				v.Certificate = true
			}
		}
		v.Loaded = !v.Excluded && !v.Unloaded && !vaultLocked && !locked
		out = append(out, v)
	}
	if running {
		for _, ak := range r.addedKeys() {
			base := ak.pub
			cert, isCert := base.(*ssh.Certificate)
			if isCert {
				base = cert.Key
			}
			typ, bits := keyTypeBits(base)
			v := agentKeyView{ID: addedKeyID(ak.pub), Source: "added", Name: ak.comment, Type: typ, Bits: bits,
				Fingerprint: ssh.FingerprintSHA256(base), Comment: ak.comment, Loaded: !locked, Unlocked: true,
				Confirm: ak.confirm, Certificate: isCert}
			if v.Name == "" {
				v.Name = "(added with ssh-add)"
			}
			if !ak.expires.IsZero() {
				e := ak.expires.UTC()
				v.ExpiresAt = &e
			}
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Source > out[j].Source }) // stored first
	return out, nil
}

// autostart starts the agent at startup when the desktop user enabled "start automatically".
func (s *agentService) autostart(ctx context.Context) {
	d := s.h.d
	if d.Cfg == nil || !d.Cfg.IsDesktop() {
		return
	}
	u, err := d.Store.Users.FirstAdmin(ctx)
	if err != nil {
		return // not set up yet
	}
	if !loadSettings(ctx, d.Store, u.ID).AgentAutostart {
		return
	}
	if err := s.start(u); err != nil {
		s.log.Warn("the built-in SSH agent could not be started automatically", "err", err)
	}
}

// ---- handlers -----------------------------------------------------------------------------------------------------

func (h *handler) agentStatus(c *echo.Context) error {
	return c.JSON(http.StatusOK, h.agent.status(c.Request().Context(), httpx.UserFrom(c)))
}

func (h *handler) agentStart(c *echo.Context) error {
	u := httpx.UserFrom(c)
	if err := h.agent.start(u); err != nil {
		return err
	}
	h.d.Audit.Log(c, "agent.start", "", nil)
	return c.JSON(http.StatusOK, h.agent.status(c.Request().Context(), u))
}

func (h *handler) agentStop(c *echo.Context) error {
	u := httpx.UserFrom(c)
	sock := h.agent.current()
	if sock == nil {
		return httpx.OK(c)
	}
	if sock.owner.ID != u.ID && !u.IsAdmin() {
		return httpx.Forbidden("the built-in agent belongs to another user")
	}
	if h.agent.stop() {
		h.d.Audit.Log(c, "agent.stop", "", nil)
	}
	return httpx.OK(c)
}

func (h *handler) agentKeys(c *echo.Context) error {
	list, err := h.agent.keysOf(c.Request().Context(), httpx.UserFrom(c))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, list)
}

// agentRemoveKey removes a key added with ssh-add, or unloads a stored key until the agent restarts / reloads.
func (h *handler) agentRemoveKey(c *echo.Context) error {
	u := httpx.UserFrom(c)
	id := c.Param("id")
	r := h.agent.ring(u.ID)
	switch {
	case strings.HasPrefix(id, "k:"):
		k, err := h.ownKey(c.Request().Context(), u, strings.TrimPrefix(id, "k:"))
		if err != nil {
			return err
		}
		r.unload(k.ID)
	case strings.HasPrefix(id, "a:"):
		r.mu.Lock()
		found := false
		for i, ak := range r.added {
			if addedKeyID(ak.pub) == id {
				r.added = append(r.added[:i:i], r.added[i+1:]...)
				found = true
				break
			}
		}
		r.mu.Unlock()
		if !found {
			return httpx.ErrNotFound
		}
	default:
		return httpx.ErrNotFound
	}
	return httpx.OK(c)
}

// agentReload reloads the stored keys: unloaded keys come back, decrypted keys and remembered decisions are
// forgotten.
func (h *handler) agentReload(c *echo.Context) error {
	r := h.agent.ring(httpx.UserFrom(c).ID)
	r.mu.Lock()
	clear(r.unloaded)
	clear(r.cache)
	clear(r.approvals)
	r.mu.Unlock()
	return httpx.OK(c)
}

func (h *handler) agentLock(c *echo.Context) error {
	r := h.agent.ring(httpx.UserFrom(c).ID)
	r.mu.Lock()
	if !r.locked {
		r.locked, r.lockPass = true, nil
		clear(r.cache)
	}
	r.mu.Unlock()
	h.d.Audit.Log(c, "agent.lock", "", nil)
	return httpx.OK(c)
}

func (h *handler) agentUnlock(c *echo.Context) error {
	r := h.agent.ring(httpx.UserFrom(c).ID)
	r.mu.Lock()
	wipe(r.lockPass)
	r.locked, r.lockPass, r.failedUnlocks = false, nil, 0
	r.lastUse = time.Now()
	r.mu.Unlock()
	h.d.Audit.Log(c, "agent.unlock", "", nil)
	return httpx.OK(c)
}
