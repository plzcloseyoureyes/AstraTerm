package keys

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/nexterm/nexterm/internal/store"
)

// settingsSection is the settings key of the module (frontend: defineSettings('keys', …), Settings → SSH keys &
// agent). The backend reads the agent keys; the other keys (generator defaults) are frontend-only.
const settingsSection = "keys"

// keysSettings are the backend-relevant values of the "keys" settings section (user values merged over global ones).
type keysSettings struct {
	// AgentAutostart starts the built-in agent socket when NexTerm starts (desktop mode, the desktop user's setting).
	AgentAutostart bool `json:"agentAutostart"`
	// AgentConfirm asks before local processes use a key through the agent socket.
	AgentConfirm bool `json:"agentConfirm"`
	// AgentForwardConfirm asks before a remote host uses a key through agent forwarding.
	AgentForwardConfirm bool `json:"agentForwardConfirm"`
	// AgentExclude lists stored key IDs the agent (and forwarding) must not offer.
	AgentExclude []string `json:"agentExclude"`
	// AgentAutoLockMin locks the agent after that many idle minutes (0 = never).
	AgentAutoLockMin int `json:"agentAutoLockMin"`
	// AgentKeyLifetimeMin forgets decrypted keys that many minutes after they were unlocked (0 = until the agent
	// stops); keys whose passphrase is not remembered then ask for it again.
	AgentKeyLifetimeMin int `json:"agentKeyLifetimeMin"`
}

// loadSettings returns the effective settings of userID (global values, overridden by the user's).
func loadSettings(ctx context.Context, st *store.Store, userID string) keysSettings {
	var s keysSettings
	for _, scope := range []string{store.ScopeGlobal, userID} {
		if scope == "" {
			continue
		}
		raw, err := st.Settings.Get(ctx, scope, settingsSection)
		if err != nil {
			continue
		}
		_ = json.Unmarshal(raw, &s) // present keys override; malformed values are ignored
	}
	s.AgentAutoLockMin = min(max(s.AgentAutoLockMin, 0), 7*24*60)
	s.AgentKeyLifetimeMin = min(max(s.AgentKeyLifetimeMin, 0), 7*24*60)
	return s
}

func (s keysSettings) excluded(keyID string) bool { return slices.Contains(s.AgentExclude, keyID) }
