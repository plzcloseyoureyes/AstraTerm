// Package app defines the dependency bundle (Deps) handed to every module plus the shared resolution helpers that
// turn a saved connection into usable, decrypted credentials (SPEC §4.1).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/plzcloseyoureyes/astraterm/internal/audit"
	"github.com/plzcloseyoureyes/astraterm/internal/config"
	"github.com/plzcloseyoureyes/astraterm/internal/events"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
	"github.com/plzcloseyoureyes/astraterm/internal/vault"
)

// Deps is the set of core services every module receives in Mount.
type Deps struct {
	Ctx    context.Context // cancelled on shutdown
	Cfg    *config.Config
	Log    *slog.Logger
	Router *httpx.Router
	Store  *store.Store
	Vault  *vault.Vault
	Events *events.Hub
	Jobs   *events.Jobs
	Audit  *audit.Logger
}

// Visible reports whether user may see a row owned by ownerID with the given shared flag.
func Visible(user *model.User, ownerID string, shared bool) bool {
	return user != nil && (ownerID == user.ID || shared)
}

// CanModify reports whether user may modify a row owned by ownerID (owner or admin). Shared rows are read-only for
// other users.
func CanModify(user *model.User, ownerID string) bool {
	return user != nil && (ownerID == user.ID || user.IsAdmin())
}

// ResolveConnection loads a connection visible to user, merges its identity (when identityId is set and the identity
// belongs to the connection owner: the identity fills an empty username / keyId, and connection secrets override
// identity secrets) and decrypts the merged secrets. It returns httpx.ErrNotFound when the connection does not exist
// or is not visible, and httpx.ErrLocked when secrets exist but the vault is locked. The returned connection is a
// private copy (Secrets unset).
func (d *Deps) ResolveConnection(ctx context.Context, user *model.User, connID string) (*model.Connection, map[string]string, error) {
	if user == nil {
		return nil, nil, httpx.ErrUnauthorized
	}
	c, err := d.Store.Connections.Get(ctx, connID)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, nil, httpx.ErrNotFound
		}
		return nil, nil, err
	}
	if !Visible(user, c.OwnerID, c.Shared) {
		return nil, nil, httpx.ErrNotFound
	}
	conn := c.Clone()
	conn.Secrets = nil

	secrets := map[string]string{}
	if conn.IdentityID != "" {
		ident, err := d.Store.Identities.Get(ctx, conn.IdentityID)
		switch {
		case errors.Is(err, model.ErrNotFound):
			// dangling reference: ignore
		case err != nil:
			return nil, nil, err
		case ident.OwnerID == c.OwnerID:
			if conn.Username == "" {
				conn.Username = ident.Username
			}
			if conn.KeyID == "" {
				conn.KeyID = ident.KeyID
			}
			is, err := d.openSecrets(ident.SecretsEnc)
			if err != nil {
				return nil, nil, err
			}
			for k, v := range is {
				secrets[k] = v
			}
		}
	}
	cs, err := d.openSecrets(c.SecretsEnc)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range cs {
		secrets[k] = v
	}
	return conn, secrets, nil
}

func (d *Deps) openSecrets(enc []byte) (map[string]string, error) {
	m, err := d.Vault.OpenJSON(enc)
	if errors.Is(err, model.ErrLocked) {
		return nil, httpx.ErrLocked
	}
	if err != nil {
		return nil, fmt.Errorf("decrypt secrets: %w", err)
	}
	return m, nil
}

// KeyMaterial returns the decrypted private key (PEM / OpenSSH / PPK text as stored) and passphrase of a stored key.
// The key must belong to user, or be used by a shared connection of its owner (so shared connections work for
// everyone without revealing the key). Errors: httpx.ErrNotFound, httpx.ErrLocked.
func (d *Deps) KeyMaterial(ctx context.Context, user *model.User, keyID string) (pem []byte, passphrase string, err error) {
	if user == nil {
		return nil, "", httpx.ErrUnauthorized
	}
	k, err := d.Store.Keys.Get(ctx, keyID)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, "", httpx.ErrNotFound
		}
		return nil, "", err
	}
	if k.OwnerID != user.ID {
		shared, err := d.Store.Keys.UsedBySharedConnection(ctx, keyID)
		if err != nil {
			return nil, "", err
		}
		if !shared {
			return nil, "", httpx.ErrNotFound
		}
	}
	if len(k.PrivateKeyEnc) == 0 {
		return nil, "", httpx.NotFound("key has no private material")
	}
	pem, err = d.Vault.Open(k.PrivateKeyEnc)
	if err != nil {
		if errors.Is(err, model.ErrLocked) {
			return nil, "", httpx.ErrLocked
		}
		return nil, "", fmt.Errorf("decrypt private key: %w", err)
	}
	if len(k.PassphraseEnc) > 0 {
		pp, err := d.Vault.Open(k.PassphraseEnc)
		if err != nil {
			if errors.Is(err, model.ErrLocked) {
				return nil, "", httpx.ErrLocked
			}
			return nil, "", fmt.Errorf("decrypt passphrase: %w", err)
		}
		passphrase = string(pp)
	}
	return pem, passphrase, nil
}

// ---- feature probes -----------------------------------------------------------------------------------------------

// FeatureProbe reports whether an optional capability is available (e.g. guacd reachable). It must return quickly.
type FeatureProbe func(ctx context.Context) bool

var (
	featMu sync.RWMutex
	feats  = map[string]FeatureProbe{}
)

// RegisterFeature adds or overrides a probe reported in GET /api/auth/state → features.<name>. Built-in probes
// (guacd, docker, kubectl, mosh, wsl) can be overridden by the module that owns the feature.
func RegisterFeature(name string, probe FeatureProbe) {
	featMu.Lock()
	feats[name] = probe
	featMu.Unlock()
}

// FeatureProbes returns a snapshot of registered probes, sorted by name.
func FeatureProbes() []NamedProbe {
	featMu.RLock()
	defer featMu.RUnlock()
	out := make([]NamedProbe, 0, len(feats))
	for n, p := range feats {
		out = append(out, NamedProbe{Name: n, Probe: p})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NamedProbe pairs a feature name with its probe.
type NamedProbe struct {
	Name  string
	Probe FeatureProbe
}
