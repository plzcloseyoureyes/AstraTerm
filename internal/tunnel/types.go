package tunnel

import (
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Options are the per-tunnel settings beyond the core `tunnels` columns (SPEC §5.1). They are stored as JSON in the
// module table tunnel_meta and returned as Tunnel.options (a SPEC §9 extension of the Tunnel contract).
type Options struct {
	// Reverse turns a dynamic tunnel into a *remote* SOCKS proxy (TUN-5): the proxy listens on the SSH server
	// (bindHost:bindPort there) and connections exit through the AstraTerm host's network.
	Reverse bool `json:"reverse,omitempty"`
	// BindSocket listens on a Unix socket instead of bindHost:bindPort (TUN-6): a path on the AstraTerm host for
	// local/dynamic tunnels, a path on the SSH server for remote tunnels (streamlocal-forward).
	BindSocket string `json:"bindSocket,omitempty"`
	// DestSocket connects to a Unix socket instead of destHost:destPort (TUN-6): a path on the SSH server for local
	// tunnels (direct-streamlocal, e.g. /var/run/docker.sock), a path on the AstraTerm host for remote tunnels.
	DestSocket string `json:"destSocket,omitempty"`
	// SocksUsername enables username/password authentication on a SOCKS / HTTP proxy (password: secret
	// "socksPassword").
	SocksUsername string `json:"socksUsername,omitempty"`
	// HTTPProxy also accepts HTTP CONNECT and absolute-URI proxy requests (and serves /proxy.pac) on a dynamic
	// tunnel's port. Default true.
	HTTPProxy *bool `json:"httpProxy,omitempty"`
	// AutoReconnect re-establishes the tunnel with backoff when the SSH connection drops. Default true.
	AutoReconnect *bool `json:"autoReconnect,omitempty"`
	// OnDemand (local and dynamic tunnels) opens the listener immediately but connects SSH only when the first
	// client arrives, and disconnects after IdleTimeoutSec without connections.
	OnDemand bool `json:"onDemand,omitempty"`
	// IdleTimeoutSec is the on-demand idle disconnect delay (default 300, minimum 10).
	IdleTimeoutSec int `json:"idleTimeoutSec,omitempty"`
	// MaxConns caps concurrent client connections (0 = default cap).
	MaxConns int `json:"maxConns,omitempty"`
	// AllowFrom restricts which client addresses may use a listener on the AstraTerm host (IPs or CIDRs; empty = any).
	AllowFrom []string `json:"allowFrom,omitempty"`
	// Scheme hints how "open in browser" reaches the service: "http", "https" or "" (guess from the port).
	Scheme string `json:"scheme,omitempty"`
	// Cosmetics.
	Color string `json:"color,omitempty"`
	Icon  string `json:"icon,omitempty"`
	Notes string `json:"notes,omitempty"`
}

// Status is the runtime status of a tunnel: SPEC TunnelStatus plus extensions (SPEC §9 tunnels notes).
type Status struct {
	model.TunnelStatus
	// Connected reports whether the SSH connection the tunnel runs over is up (false for on-demand tunnels waiting
	// for their first client).
	Connected bool `json:"connected"`
	// LocalAddr / RemoteAddr are the actually bound listener addresses (auto-assigned ports resolved).
	LocalAddr  string `json:"localAddr,omitempty"`
	RemoteAddr string `json:"remoteAddr,omitempty"`
	// RateIn / RateOut are the current throughput in bytes per second (in = towards the client).
	RateIn  int64 `json:"rateIn"`
	RateOut int64 `json:"rateOut"`
	// FailedConns counts client connections that could not be forwarded (destination refused, rejected client…).
	FailedConns int64 `json:"failedConns"`
	// LastError is the most recent per-connection failure while the tunnel itself keeps running.
	LastError   string     `json:"lastError,omitempty"`
	LastErrorAt *time.Time `json:"lastErrorAt,omitempty"`
	// Reconnects counts SSH re-connections since the tunnel was started; RetryAt is the next attempt while waiting.
	Reconnects int        `json:"reconnects"`
	RetryAt    *time.Time `json:"retryAt,omitempty"`
	// Waiting tells what a starting tunnel waits for: "vault" (unlock), "client" (a browser window to answer a login
	// prompt) or "demand" (on-demand tunnel idle until a client connects).
	Waiting string `json:"waiting,omitempty"`
	// Warning flags a running tunnel that works differently than configured: a remote forward that asked for a
	// non-loopback address but that sshd bound to loopback only (GatewayPorts no).
	Warning string `json:"warning,omitempty"`
}

func stoppedStatus() Status {
	return Status{TunnelStatus: model.TunnelStatus{State: model.TunnelStopped}}
}

// ConnRef summarizes the SSH connection a tunnel runs over (display only).
type ConnRef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
}

// View is the JSON representation of a saved tunnel: the SPEC §5.2 Tunnel plus the extension fields.
type View struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Type         string    `json:"type"`
	ConnectionID string    `json:"connectionId"`
	BindHost     string    `json:"bindHost"`
	BindPort     int       `json:"bindPort"`
	DestHost     string    `json:"destHost"`
	DestPort     int       `json:"destPort"`
	AutoStart    bool      `json:"autoStart"`
	Status       Status    `json:"status"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`

	Options    Options  `json:"options"`
	SortOrder  int      `json:"sortOrder"`
	SecretKeys []string `json:"secretKeys"`
	Connection *ConnRef `json:"connection,omitempty"`
}

// Input is the body of POST /api/tunnels (and the patchable fields of PATCH /api/tunnels/:id).
type Input struct {
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	ConnectionID string            `json:"connectionId"`
	BindHost     string            `json:"bindHost"`
	BindPort     int               `json:"bindPort"`
	DestHost     string            `json:"destHost"`
	DestPort     int               `json:"destPort"`
	AutoStart    bool              `json:"autoStart"`
	Options      Options           `json:"options"`
	SortOrder    int               `json:"sortOrder"`
	Secrets      map[string]string `json:"secrets,omitempty"` // write-only; "" deletes a key
	// Start starts the tunnel right after it was created.
	Start bool `json:"start,omitempty"`
}

// Event is published to the owner's sockets ({type:'tunnel'}, SPEC §6.1) whenever a tunnel's status changes.
// Change is set for definition changes ("created" | "updated" | "deleted") so other windows refresh their list.
type Event struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Status Status `json:"status"`
	Change string `json:"change,omitempty"`
}

// Secret keys of a tunnel.
const secretSocksPassword = "socksPassword"

// ForwardSpec is one session-integrated forward (connection.options.forwards[] entries and ad-hoc session forwards,
// TUN-7). Field names mirror the Tunnel definition.
type ForwardSpec struct {
	Type       string `json:"type"`
	BindHost   string `json:"bindHost,omitempty"`
	BindPort   int    `json:"bindPort,omitempty"`
	DestHost   string `json:"destHost,omitempty"`
	DestPort   int    `json:"destPort,omitempty"`
	Reverse    bool   `json:"reverse,omitempty"`
	BindSocket string `json:"bindSocket,omitempty"`
	DestSocket string `json:"destSocket,omitempty"`
	// Name is an optional label.
	Name string `json:"name,omitempty"`
	// Disabled keeps a configured forward without starting it.
	Disabled bool `json:"disabled,omitempty"`
}

// SessionForward is the JSON view of a forward bound to a live SSH session.
type SessionForward struct {
	ID           string      `json:"id"`
	SessionID    string      `json:"sessionId"`
	SessionTitle string      `json:"sessionTitle"`
	ConnectionID string      `json:"connectionId,omitempty"`
	Source       string      `json:"source"` // "connection" (options.forwards) | "adhoc"
	Spec         ForwardSpec `json:"spec"`
	Status       Status      `json:"status"`
}

// SessionEvent is published ({type:'tunnel.session'}) with the full forward list of a session whenever it changes;
// an empty list means the session has no (more) forwards.
type SessionEvent struct {
	Type      string           `json:"type"`
	SessionID string           `json:"sessionId"`
	Forwards  []SessionForward `json:"forwards"`
}

// RemotePort is one TCP port listening on a remote host (TUN-9).
type RemotePort struct {
	// Address is the bind address as reported ("0.0.0.0", "::", "127.0.0.1", "*" …).
	Address string `json:"address"`
	Port    int    `json:"port"`
	// Scope classifies the bind address: "loopback", "all" (wildcard) or "address" (a specific interface).
	Scope string `json:"scope"`
	// ConnectHost is the destination host a local forward should use to reach the port from the SSH server.
	ConnectHost string `json:"connectHost"`
	Process     string `json:"process,omitempty"`
	PID         int    `json:"pid,omitempty"`
}

// RemotePorts is the reply of GET /api/tunnels/remote-ports.
type RemotePorts struct {
	Method string       `json:"method"`
	Ports  []RemotePort `json:"ports"`
	// Note explains partial results (e.g. process names hidden without privileges).
	Note string `json:"note,omitempty"`
}

// PortsEvent is pushed to subscribers of the "tunnel.ports" topic (TUN-9 listening-port watcher).
type PortsEvent struct {
	Type      string       `json:"type"` // "tunnel.ports"
	SessionID string       `json:"sessionId"`
	Initial   bool         `json:"initial,omitempty"`
	Ports     []RemotePort `json:"ports"`
	Added     []RemotePort `json:"added,omitempty"`
	Removed   []RemotePort `json:"removed,omitempty"`
	Error     string       `json:"error,omitempty"`
}

// Event types beyond SPEC §6.1.
const (
	evSession = "tunnel.session"
	evPorts   = "tunnel.ports"
)

// ExportFile is the JSON document of GET /api/tunnels/export and POST /api/tunnels/import.
type ExportFile struct {
	Format     string         `json:"format"` // "astraterm-tunnels"
	Version    int            `json:"version"`
	ExportedAt time.Time      `json:"exportedAt"`
	Tunnels    []ExportTunnel `json:"tunnels"`
}

// ExportTunnel is one exported tunnel definition (never includes secrets). Connection identifies the SSH connection
// so an import on another installation can map it by name / address.
type ExportTunnel struct {
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	BindHost   string  `json:"bindHost"`
	BindPort   int     `json:"bindPort"`
	DestHost   string  `json:"destHost"`
	DestPort   int     `json:"destPort"`
	AutoStart  bool    `json:"autoStart"`
	Options    Options `json:"options"`
	SortOrder  int     `json:"sortOrder"`
	Connection ConnRef `json:"connection"`
}

const exportFormat = "astraterm-tunnels"
