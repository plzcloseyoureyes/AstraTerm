// Package keys is AstraTerm's SSH key module (MobaKeyGen + MobAgent; RESEARCH TOOL-1, SSH-5, SSH-11, SSH-12, SSH-19,
// SSH-20): stored keys (generate, import OpenSSH / PEM / PKCS#8 / PuTTY PPK, export OpenSSH / PPK / PEM / public,
// OpenSSH user certificates, install on a server, sign certificates), the known hosts manager (entries,
// @cert-authority and @revoked markers, OpenSSH and PuTTY import, export) and the built-in SSH agent (local socket /
// named pipe, forwarding keyring, confirm-before-use). Identities (SM-7) use the core /api/identities endpoints.
//
// Integration with sshx (see internal/sshx/cert_hostca.go and agent_builtin.go): the module registers its host key
// markers and its agent with the SSH pool, so host certificates signed by trusted CAs are accepted, revoked keys are
// refused, the running agent's keys are offered for authentication and forwarded agents follow the user's settings.
package keys

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/core"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/sshx"
)

type handler struct {
	d       *app.Deps
	c       *core.Core
	log     *slog.Logger
	markers *markerStore
	agent   *agentService
	drafts  *draftStore
}

// Mount registers the module's routes and wires its host key markers and built-in agent into the SSH pool.
func Mount(d *app.Deps, c *core.Core) error {
	log := slog.Default()
	if d.Log != nil {
		log = d.Log
	}
	ctx := d.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	h := &handler{d: d, c: c, log: log.With("module", "keys"), drafts: newDraftStore()}
	h.markers = newMarkerStore(d.Store.DB)
	h.agent = newAgentService(h)

	// The migration is registered in init(), so it normally ran when the store opened; Migrate is idempotent.
	if err := d.Store.Migrate(ctx); err != nil {
		return fmt.Errorf("keys: migrate: %w", err)
	}
	if err := h.markers.load(ctx); err != nil {
		h.log.Error("loading host key markers failed", "err", err)
	}
	if c != nil && c.SSH != nil {
		c.SSH.SetHostKeyMarkers(h.markers)
		c.SSH.SetBuiltinAgent(h.agent)
	}
	go h.agent.run(ctx)
	go h.drafts.janitor(ctx)

	api := d.Router.API()
	// Keys (SPEC §6.0 "Keys / known hosts / agent" + module extensions, §9).
	api.GET("/keys", h.listKeys)
	api.POST("/keys/generate", h.generate)
	api.POST("/keys/import", h.importKey)
	api.POST("/keys/inspect", h.inspect)
	api.POST("/keys/convert", h.convert)
	api.POST("/keys/drafts/:id/store", h.storeDraft)
	api.POST("/keys/drafts/:id/export", h.exportDraft)
	api.DELETE("/keys/drafts/:id", h.discardDraft)
	api.GET("/keys/:id", h.getKey)
	api.PATCH("/keys/:id", h.updateKey)
	api.DELETE("/keys/:id", h.deleteKey)
	api.GET("/keys/:id/export", h.exportKeyGET)
	api.POST("/keys/:id/export", h.exportKeyPOST)
	api.POST("/keys/:id/install", h.install)
	api.POST("/keys/:id/sign", h.signCertificate)
	// Known hosts.
	api.GET("/known-hosts", h.listKnownHosts)
	api.POST("/known-hosts", h.addKnownHost)
	api.POST("/known-hosts/import", h.importKnownHosts)
	api.GET("/known-hosts/export", h.exportKnownHosts)
	api.POST("/known-hosts/bulk-delete", h.bulkDeleteKnownHosts)
	api.GET("/known-hosts/markers", h.listMarkers)
	api.POST("/known-hosts/markers", h.addMarker)
	api.DELETE("/known-hosts/markers/:id", h.deleteMarker)
	api.DELETE("/known-hosts/:id", h.deleteKnownHost)
	// Built-in agent.
	api.GET("/agent/status", h.agentStatus)
	api.POST("/agent/start", h.agentStart)
	api.POST("/agent/stop", h.agentStop)
	api.GET("/agent/keys", h.agentKeys)
	api.DELETE("/agent/keys/:id", h.agentRemoveKey)
	api.POST("/agent/reload", h.agentReload)
	api.POST("/agent/lock", h.agentLock)
	api.POST("/agent/unlock", h.agentUnlock)

	app.RegisterFeature("sshAgent", func(context.Context) bool { return d.Cfg != nil && d.Cfg.IsDesktop() })
	mounted.Store(d, h)
	context.AfterFunc(ctx, func() { mounted.Delete(d) })
	h.agent.autostart(ctx)
	return nil
}

// mounted maps *app.Deps to the module's handler (white-box tests).
var mounted sync.Map

// ---- shared helpers -----------------------------------------------------------------------------------------------

// ownKey loads a key owned by u (404 otherwise: keys are private).
func (h *handler) ownKey(ctx context.Context, u *model.User, id string) (*model.SSHKey, error) {
	if !model.ValidID(id) {
		return nil, httpx.ErrNotFound
	}
	k, err := h.d.Store.Keys.Get(ctx, id)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, httpx.ErrNotFound
		}
		return nil, err
	}
	if k.OwnerID != u.ID {
		return nil, httpx.ErrNotFound
	}
	return k, nil
}

// keyPublic parses the stored public key of k.
func keyPublic(k *model.SSHKey) (ssh.PublicKey, error) {
	pub, _, err := parsePublicKeyText(k.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("stored public key of %s: %w", k.ID, err)
	}
	return pub, nil
}

// material returns the decrypted private key of k with passphrase (the remembered one when passphrase is empty).
func (h *handler) material(ctx context.Context, u *model.User, k *model.SSHKey, passphrase string) (*parsedKey, error) {
	if len(k.PrivateKeyEnc) == 0 {
		return nil, httpx.BadRequest("this key has no private part")
	}
	text, remembered, err := h.d.KeyMaterial(ctx, u, k.ID)
	if err != nil {
		return nil, err
	}
	defer clear(text)
	if passphrase == "" {
		passphrase = remembered
	}
	pk, err := parseKey(text, passphrase)
	if err != nil {
		return nil, httpKeyError(err)
	}
	return pk, nil
}

// canManageKnownHosts: the known hosts are global, so in server mode only administrators may change them.
func (h *handler) canManageKnownHosts(u *model.User) bool {
	return (h.d.Cfg != nil && h.d.Cfg.IsDesktop()) || u.IsAdmin()
}

func (h *handler) requireKnownHostsAdmin(u *model.User) error {
	if !h.canManageKnownHosts(u) {
		return httpx.Forbidden("only administrators can change the trusted host keys in server mode")
	}
	return nil
}

// knownHostNames returns up to three host names whose trusted key is key (for agent prompts).
func (h *handler) knownHostNames(ctx context.Context, key ssh.PublicKey) []string {
	list, err := h.d.Store.KnownHosts.List(ctx)
	if err != nil {
		return nil
	}
	want := key.Marshal()
	var out []string
	for _, kh := range list {
		if pk, err := sshx.ParseKnownHostKey(kh.PublicKey); err == nil && bytes.Equal(pk.Marshal(), want) {
			out = append(out, knownHostName(kh.Host, kh.Port))
			if len(out) == 3 {
				break
			}
		}
	}
	return out
}

// rememberPassphrase stores a verified passphrase of the key in the vault.
func (h *handler) rememberPassphrase(ctx context.Context, userID, keyID, passphrase string) error {
	ctx = context.WithoutCancel(ctx)
	k, err := h.d.Store.Keys.Get(ctx, keyID)
	if err != nil {
		return err
	}
	if k.OwnerID != userID {
		return errors.New("not the owner of the key")
	}
	enc, err := h.d.Vault.Seal([]byte(passphrase))
	if err != nil {
		return err
	}
	k.PassphraseEnc = enc
	if err := h.d.Store.Keys.Update(ctx, k); err != nil {
		return err
	}
	h.agent.audit(ctx, userID, "key.passphrase.remember", keyID, map[string]any{"name": k.Name})
	return nil
}

// cleanName validates a display name (key names).
func cleanName(s string) (string, error) {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) }), " ")
	if strings.ContainsFunc(s, isControl) {
		return "", httpx.BadRequest("the name contains control characters")
	}
	if utf8.RuneCountInString(s) > 200 {
		return "", httpx.BadRequest("the name is too long (at most 200 characters)")
	}
	return s, nil
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func now() time.Time { return time.Now().UTC() }
