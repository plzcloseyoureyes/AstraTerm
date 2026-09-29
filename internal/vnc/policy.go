package vnc

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/store"
)

// ---- encryption policy ----------------------------------------------------------------------------------------------
//
// VNC servers negotiate security types in the clear, so neither a failed TLS negotiation nor a server offering no
// encryption can be told apart from an attacker stripping or breaking the encrypted options. Termstead therefore never
// silently connects with less protection than the server offered: connection option `encryption` (and the viewer's
// explicit confirmation, remembered for the runtime session) decides.
//
//	require            only VeNCrypt with TLS (certificate or anonymous, strong key exchange); nothing else connects
//	prefer (default)   TLS whenever the server offers it; when the TLS negotiation fails, the server's anonymous TLS
//	                   only has a weak key exchange, or the only way on would send the password in clear text
//	                   (VeNCrypt Plain without TLS), the viewer is asked (WS close 4426) instead of connecting
//	allow-weak         additionally accept anonymous TLS with a 1024–2047-bit Diffie-Hellman group without asking
//	allow-unencrypted  additionally fall back to unencrypted security types without asking (the pre-policy behavior)
//
// Servers that offer no encryption at all connect under every policy but `require` (the viewer shows "Not
// encrypted"). Certificate (X509*) failures never fall back to anything.

// encryptionPolicy levels are ordered from strictest to most permissive; the zero value is the default (prefer).
type encryptionPolicy int

const (
	encRequire encryptionPolicy = iota - 1
	encPrefer
	encAllowWeak
	encAllowUnencrypted
)

// weakDHFloor is the smallest Diffie-Hellman group Termstead accepts even with the user's consent.
const weakDHFloor = 1024

func (p encryptionPolicy) String() string {
	switch p {
	case encRequire:
		return "require"
	case encAllowWeak:
		return "allow-weak"
	case encAllowUnencrypted:
		return "allow-unencrypted"
	}
	return "prefer"
}

func parseEncryptionPolicy(s string) (encryptionPolicy, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "require", "required":
		return encRequire, true
	case "", "prefer":
		return encPrefer, true
	case "allow-weak":
		return encAllowWeak, true
	case "allow-unencrypted":
		return encAllowUnencrypted, true
	}
	return encPrefer, false
}

// connectionPolicy reads options.encryption (unknown values mean the default).
func connectionPolicy(o model.Options) encryptionPolicy {
	p, _ := parseEncryptionPolicy(o.String("encryption", ""))
	return p
}

// parseConsent reads the viewer's confirmation (/ws/vnc/:id?allow=weak|unencrypted).
func parseConsent(s string) encryptionPolicy {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "weak":
		return encAllowWeak
	case "unencrypted":
		return encAllowUnencrypted
	}
	return encPrefer
}

// effectivePolicy combines the connection's policy with confirmations; `require` cannot be overridden.
func effectivePolicy(conn encryptionPolicy, consents ...encryptionPolicy) encryptionPolicy {
	if conn == encRequire {
		return encRequire
	}
	p := conn
	for _, c := range consents {
		p = max(p, c)
	}
	return p
}

// insecureError: going on would give less protection than the server offered (or send the password in clear
// text); the viewer must confirm (close 4426, details in vnc-info `confirm`).
type insecureError struct {
	reason      string
	weakTLS     bool // anonymous TLS with a weak group is possible (?allow=weak)
	dhBits      int
	unencrypted bool // an unencrypted security type is possible (?allow=unencrypted)
	cleartext   bool // that type sends the password in clear text
}

func (e *insecureError) Error() string { return e.reason }

// ConfirmInfo describes a connection waiting for the user's confirmation (Info.Confirm).
type ConfirmInfo struct {
	Reason string `json:"reason"`
	// WeakTLS: the server's anonymous TLS works with a weak key exchange (DHBits) — retry with ?allow=weak.
	WeakTLS bool `json:"weakTls"`
	DHBits  int  `json:"dhBits,omitempty"`
	// Unencrypted: an unencrypted security type is available — retry with ?allow=unencrypted.
	Unencrypted bool `json:"unencrypted"`
	// Cleartext: that type sends the password in clear text (VeNCrypt Plain without TLS).
	Cleartext bool `json:"cleartext"`
}

func (e *insecureError) info() *ConfirmInfo {
	return &ConfirmInfo{Reason: e.reason, WeakTLS: e.weakTLS, DHBits: e.dhBits, Unencrypted: e.unencrypted, Cleartext: e.cleartext}
}

// encryptionRequiredError: policy `require` and the server cannot provide acceptable encryption.
type encryptionRequiredError struct{ reason string }

func (e *encryptionRequiredError) Error() string {
	return "encryption is required for this connection, but " + e.reason
}

// ---- clipboard direction ----------------------------------------------------------------------------------------------
//
// RESEARCH GFX-2 / SEC-15: the clipboard can be limited to one direction or turned off, per connection
// (options.clipboardDirection) and server-wide by an administrator (global settings section "vncPolicy",
// clipboardDirection); the stricter one wins. Termstead enforces local→remote itself (ClientCutText is dropped and the
// Extended Clipboard pseudo-encoding removed from SetEncodings, so the server cannot request the viewer's clipboard);
// remote→local cannot be filtered without decoding every framebuffer update, so the viewer enforces it (it ignores
// the server's clipboard when vnc-info reports `clipboard` without that direction).

type clipboardDirection struct{ toRemote, fromRemote bool }

func parseClipboardDirection(s string) clipboardDirection {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "to-remote":
		return clipboardDirection{toRemote: true}
	case "from-remote":
		return clipboardDirection{fromRemote: true}
	case "none", "off", "disabled":
		return clipboardDirection{}
	}
	return clipboardDirection{toRemote: true, fromRemote: true}
}

func (d clipboardDirection) and(o clipboardDirection) clipboardDirection {
	return clipboardDirection{toRemote: d.toRemote && o.toRemote, fromRemote: d.fromRemote && o.fromRemote}
}

func (d clipboardDirection) String() string {
	switch {
	case d.toRemote && d.fromRemote:
		return "both"
	case d.toRemote:
		return "to-remote"
	case d.fromRemote:
		return "from-remote"
	}
	return "none"
}

// policySettingsKey is the admin-only global settings section of the module.
const policySettingsKey = "vncPolicy"

// globalPolicy is the administrator's server-wide VNC policy (global settings scope only; users cannot override it).
type globalPolicy struct {
	ClipboardDirection string `json:"clipboardDirection,omitempty"`
}

func (m *Module) globalPolicy(ctx context.Context) globalPolicy {
	var g globalPolicy
	if m.d == nil || m.d.Store == nil {
		return g
	}
	var raw json.RawMessage
	if ok, err := m.d.Store.Settings.GetJSON(ctx, store.ScopeGlobal, policySettingsKey, &raw); err != nil || !ok {
		return g
	}
	_ = json.Unmarshal(raw, &g) // unknown keys / malformed values: defaults
	return g
}

// clipboardPolicy is the effective clipboard direction for a connection.
func (m *Module) clipboardPolicy(ctx context.Context, conn *model.Connection) clipboardDirection {
	d := parseClipboardDirection(conn.Options.String("clipboardDirection", ""))
	return d.and(parseClipboardDirection(m.globalPolicy(ctx).ClipboardDirection))
}
