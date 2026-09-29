package model

import (
	"encoding/json"
	"slices"
	"sort"
	"time"
)

// User is a NexTerm account (SPEC §5.2). Credentials never live on this struct.
type User struct {
	ID          string     `json:"id"`
	Username    string     `json:"username"`
	DisplayName string     `json:"displayName"`
	Role        Role       `json:"role"`
	TOTPEnabled bool       `json:"totpEnabled"`
	Disabled    bool       `json:"disabled"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"-"`
	LastLoginAt *time.Time `json:"lastLoginAt,omitempty"`
}

// IsAdmin reports whether u is a (non-nil) admin.
func (u *User) IsAdmin() bool { return u != nil && u.Role == RoleAdmin }

// Folder is a node of the connection tree. ParentID "" means root (serialized as absent/null).
type Folder struct {
	ID        string    `json:"id"`
	ParentID  string    `json:"parentId,omitempty"`
	Name      string    `json:"name"`
	Color     string    `json:"color,omitempty"`
	Icon      string    `json:"icon,omitempty"`
	SortOrder int       `json:"sortOrder"`
	Shared    bool      `json:"shared"`
	OwnerID   string    `json:"ownerId"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Connection is a saved connection (bookmark). Secrets is WRITE-ONLY: it is only ever populated from client input and
// never filled by the store; responses list the names of stored secrets in SecretKeys. Decrypted secrets are obtained
// through app.Deps.ResolveConnection.
type Connection struct {
	ID         string            `json:"id"`
	FolderID   string            `json:"folderId,omitempty"`
	Name       string            `json:"name"`
	Protocol   Protocol          `json:"protocol"`
	Host       string            `json:"host"`
	Port       int               `json:"port"`
	Username   string            `json:"username"`
	IdentityID string            `json:"identityId,omitempty"`
	KeyID      string            `json:"keyId,omitempty"`
	AuthMethod string            `json:"authMethod"`
	Color      string            `json:"color,omitempty"`
	Icon       string            `json:"icon,omitempty"`
	Tags       []string          `json:"tags"`
	Notes      string            `json:"notes"`
	Favorite   bool              `json:"favorite"`
	SortOrder  int               `json:"sortOrder"`
	Options    Options           `json:"options"`
	Secrets    map[string]string `json:"secrets,omitempty"`
	SecretKeys []string          `json:"secretKeys"`
	Shared     bool              `json:"shared"`
	OwnerID    string            `json:"ownerId"`
	LastUsedAt *time.Time        `json:"lastUsedAt,omitempty"`
	CreatedAt  time.Time         `json:"createdAt"`
	UpdatedAt  time.Time         `json:"updatedAt"`

	// SecretsEnc is the vault-sealed JSON map of secrets (nil when there are none). Never serialized.
	SecretsEnc []byte `json:"-"`
}

// Normalize replaces nil collections with empty ones so JSON renders [] / {} instead of null, and fills defaults.
func (c *Connection) Normalize() {
	if c.Tags == nil {
		c.Tags = []string{}
	}
	if c.Options == nil {
		c.Options = Options{}
	}
	if c.SecretKeys == nil {
		c.SecretKeys = []string{}
	}
	if c.AuthMethod == "" {
		c.AuthMethod = AuthAuto
	}
}

// Clone returns a deep copy of c (including Secrets and SecretsEnc).
func (c *Connection) Clone() *Connection {
	if c == nil {
		return nil
	}
	cp := *c
	cp.Tags = slices.Clone(c.Tags)
	cp.SecretKeys = slices.Clone(c.SecretKeys)
	cp.Options = c.Options.Clone()
	cp.SecretsEnc = slices.Clone(c.SecretsEnc)
	if c.Secrets != nil {
		cp.Secrets = make(map[string]string, len(c.Secrets))
		for k, v := range c.Secrets {
			cp.Secrets[k] = v
		}
	}
	if c.LastUsedAt != nil {
		t := *c.LastUsedAt
		cp.LastUsedAt = &t
	}
	return &cp
}

// Address returns host:port (IPv6-safe) for network protocols.
func (c *Connection) Address() string {
	port := c.Port
	if port == 0 {
		port = DefaultPort(c.Protocol)
	}
	return joinHostPort(c.Host, port)
}

// Identity is a reusable credential set (username + key + secrets) linked from connections.
type Identity struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Username   string            `json:"username"`
	KeyID      string            `json:"keyId,omitempty"`
	Secrets    map[string]string `json:"secrets,omitempty"` // WRITE-ONLY, see Connection.Secrets
	SecretKeys []string          `json:"secretKeys"`
	OwnerID    string            `json:"-"`
	CreatedAt  time.Time         `json:"createdAt"`
	UpdatedAt  time.Time         `json:"updatedAt"`

	SecretsEnc []byte `json:"-"`
}

// Normalize replaces nil collections with empty ones.
func (i *Identity) Normalize() {
	if i.SecretKeys == nil {
		i.SecretKeys = []string{}
	}
}

// SSHKey is a stored key pair. Private material is sealed by the vault and never serialized.
type SSHKey struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Type          string    `json:"type"`
	Bits          int       `json:"bits"`
	PublicKey     string    `json:"publicKey"`
	Fingerprint   string    `json:"fingerprint"`
	Comment       string    `json:"comment"`
	HasPassphrase bool      `json:"hasPassphrase"`
	Certificate   string    `json:"certificate,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	OwnerID       string    `json:"-"`

	PrivateKeyEnc []byte `json:"-"`
	PassphraseEnc []byte `json:"-"`
}

// KnownHost is a trusted host key (global, shared by all users).
type KnownHost struct {
	ID          string    `json:"id"`
	Host        string    `json:"host"`
	Port        int       `json:"port"`
	KeyType     string    `json:"keyType"`
	PublicKey   string    `json:"publicKey"`
	Fingerprint string    `json:"fingerprint"`
	Comment     string    `json:"comment"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Snippet send modes.
const (
	SendModePaste   = "paste"
	SendModeExecute = "execute"
)

// Snippet is a saved command / text block.
type Snippet struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Folder      string    `json:"folder"`
	Description string    `json:"description"`
	Content     string    `json:"content"`
	Tags        []string  `json:"tags"`
	SendMode    string    `json:"sendMode"`
	Shortcut    string    `json:"shortcut,omitempty"`
	OwnerID     string    `json:"-"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// MacroStep is one step of a macro.
type MacroStep struct {
	Data    string `json:"data"`
	DelayMs int    `json:"delayMs"`
}

// Macro is a recorded/edited sequence of inputs.
type Macro struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Steps     []MacroStep `json:"steps"`
	OwnerID   string      `json:"-"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
}

// Tunnel types.
const (
	TunnelLocal   = "local"
	TunnelRemote  = "remote"
	TunnelDynamic = "dynamic"
)

// Tunnel states.
const (
	TunnelStopped  = "stopped"
	TunnelStarting = "starting"
	TunnelRunning  = "running"
	TunnelError    = "error"
)

// TunnelStatus is the runtime status of a port forward.
type TunnelStatus struct {
	State       string     `json:"state"`
	Error       string     `json:"error,omitempty"`
	ActiveConns int        `json:"activeConns"`
	TotalConns  int64      `json:"totalConns"`
	BytesIn     int64      `json:"bytesIn"`
	BytesOut    int64      `json:"bytesOut"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
}

// Tunnel is a saved port forward. Status is runtime-only (not stored).
type Tunnel struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Type         string       `json:"type"`
	ConnectionID string       `json:"connectionId"`
	BindHost     string       `json:"bindHost"`
	BindPort     int          `json:"bindPort"`
	DestHost     string       `json:"destHost"`
	DestPort     int          `json:"destPort"`
	AutoStart    bool         `json:"autoStart"`
	Status       TunnelStatus `json:"status"`
	OwnerID      string       `json:"-"`
	CreatedAt    time.Time    `json:"createdAt"`
	UpdatedAt    time.Time    `json:"updatedAt"`
}

// Recording kinds.
const (
	RecordingAsciicast = "asciicast"
	RecordingLog       = "log"
)

// Recording is a session recording (asciicast) or text log file.
type Recording struct {
	ID           string     `json:"id"`
	OwnerID      string     `json:"ownerId"`
	SessionID    string     `json:"sessionId"`
	ConnectionID string     `json:"connectionId,omitempty"`
	Title        string     `json:"title"`
	Kind         string     `json:"kind"`
	Path         string     `json:"-"` // server-side file path; never exposed
	Size         int64      `json:"size"`
	Cols         int        `json:"cols"`
	Rows         int        `json:"rows"`
	StartedAt    time.Time  `json:"startedAt"`
	EndedAt      *time.Time `json:"endedAt,omitempty"`
}

// Share link modes.
const (
	ShareRead  = "read"
	ShareWrite = "write"
)

// ShareLink maps a (hashed) share token to a live session.
type ShareLink struct {
	ID        string    `json:"id"`
	SessionID string    `json:"sessionId"`
	OwnerID   string    `json:"ownerId"`
	Mode      string    `json:"mode"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	TokenHash string    `json:"-"`
}

// AuditEntry is one row of the append-only audit log.
type AuditEntry struct {
	ID       int64           `json:"id"`
	TS       time.Time       `json:"ts"`
	UserID   string          `json:"userId,omitempty"`
	Username string          `json:"username,omitempty"`
	Action   string          `json:"action"`
	Target   string          `json:"target,omitempty"`
	Details  json.RawMessage `json:"details,omitempty"`
	IP       string          `json:"ip,omitempty"`
}

// APIToken is a personal access token (Bearer nxt_…). Only its hash is stored.
type APIToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	UserID     string     `json:"-"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	TokenHash  string     `json:"-"`
}

// AuthSession is a browser login session. ID is the SHA-256 (hex) of the cookie token.
type AuthSession struct {
	ID         string    `json:"id"`
	UserID     string    `json:"-"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	IP         string    `json:"ip"`
	UserAgent  string    `json:"userAgent"`
	Remember   bool      `json:"remember"`
	Current    bool      `json:"current"`
}

// SSHConnInfo describes a live SSH transport (negotiated algorithms, host key, latency).
type SSHConnInfo struct {
	ServerVersion      string `json:"serverVersion"`
	ClientVersion      string `json:"clientVersion"`
	Kex                string `json:"kex"`
	HostKeyAlgo        string `json:"hostKeyAlgo"`
	Cipher             string `json:"cipher"`
	MAC                string `json:"mac"`
	HostKeyFingerprint string `json:"hostKeyFingerprint"`
	LatencyMs          int64  `json:"latencyMs"`
}

// SortedKeys returns the keys of m in sorted order (used for SecretKeys).
func SortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
