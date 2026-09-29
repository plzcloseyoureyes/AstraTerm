package model

import (
	"net"
	"strconv"
	"time"
)

// SessionKind distinguishes terminal sessions from graphical ones.
type SessionKind string

const (
	KindTerminal SessionKind = "terminal"
	KindVNC      SessionKind = "vnc"
	KindRDP      SessionKind = "rdp"
)

// SessionState is the lifecycle state of a runtime session.
type SessionState string

const (
	StateConnecting     SessionState = "connecting"
	StateAuthenticating SessionState = "authenticating"
	StateConnected      SessionState = "connected"
	StateDisconnected   SessionState = "disconnected"
	StateClosed         SessionState = "closed"
	StateError          SessionState = "error"
)

// RuntimeSession is the JSON view of a live session owned by the backend (SPEC §5.2).
type RuntimeSession struct {
	ID           string       `json:"id"`
	Kind         SessionKind  `json:"kind"`
	Protocol     Protocol     `json:"protocol"`
	ConnectionID string       `json:"connectionId,omitempty"`
	Title        string       `json:"title"`
	Host         string       `json:"host,omitempty"`
	Username     string       `json:"username,omitempty"`
	State        SessionState `json:"state"`
	StateMessage string       `json:"stateMessage,omitempty"`
	ExitCode     *int         `json:"exitCode,omitempty"`
	Cols         int          `json:"cols"`
	Rows         int          `json:"rows"`
	Clients      int          `json:"clients"`
	Cwd          string       `json:"cwd,omitempty"`
	Recording    bool         `json:"recording"`
	RecordingID  string       `json:"recordingId,omitempty"`
	Logging      bool         `json:"logging"`
	OwnerID      string       `json:"ownerId"`
	CreatedAt    time.Time    `json:"createdAt"`
	ConnectedAt  *time.Time   `json:"connectedAt,omitempty"`
}

// Prompt kinds.
const (
	PromptHostKey             = "hostkey"
	PromptPassword            = "password"
	PromptPassphrase          = "passphrase"
	PromptKeyboardInteractive = "keyboard-interactive"
	PromptConfirm             = "confirm"
)

// PromptField is one input of an interactive prompt.
type PromptField struct {
	Label string `json:"label"`
	Echo  bool   `json:"echo"`
	Value string `json:"value,omitempty"`
}

// Host key statuses.
const (
	HostKeyUnknown  = "unknown"
	HostKeyMismatch = "mismatch"
)

// HostKeyInfo describes a host key awaiting user verification.
type HostKeyInfo struct {
	Host             string `json:"host"`
	Port             int    `json:"port"`
	KeyType          string `json:"keyType"`
	Fingerprint      string `json:"fingerprint"`
	FingerprintMD5   string `json:"fingerprintMd5"`
	Status           string `json:"status"`
	KnownFingerprint string `json:"knownFingerprint,omitempty"`
}

// Prompt is an interactive question relayed to the user's browser(s) through the events socket (SPEC §4, §6.1).
type Prompt struct {
	ID           string        `json:"id"`
	Kind         string        `json:"kind"`
	Title        string        `json:"title"`
	Message      string        `json:"message,omitempty"`
	SessionID    string        `json:"sessionId,omitempty"`
	ConnectionID string        `json:"connectionId,omitempty"`
	Fields       []PromptField `json:"fields"`
	HostKey      *HostKeyInfo  `json:"hostKey,omitempty"`
	AllowSave    bool          `json:"allowSave"`
}

// PromptResponse is the user's answer to a Prompt.
type PromptResponse struct {
	Accept bool     `json:"accept"`
	Values []string `json:"values,omitempty"`
	Save   bool     `json:"save,omitempty"`
}

// File entry types.
const (
	FileTypeFile    = "file"
	FileTypeDir     = "dir"
	FileTypeSymlink = "symlink"
	FileTypeOther   = "other"
)

// FileEntry describes one file of a VFS listing (SPEC §6.0 Files).
type FileEntry struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Type       string    `json:"type"`
	Size       int64     `json:"size"`
	Mode       uint32    `json:"mode"`
	Perm       string    `json:"perm"`
	Mtime      time.Time `json:"mtime"`
	Owner      string    `json:"owner,omitempty"`
	Group      string    `json:"group,omitempty"`
	UID        *int      `json:"uid,omitempty"`
	GID        *int      `json:"gid,omitempty"`
	LinkTarget string    `json:"linkTarget,omitempty"`
	LinkType   string    `json:"linkType,omitempty"` // file | dir | broken
	Hidden     bool      `json:"hidden"`
}

// Transfer states.
const (
	TransferQueued   = "queued"
	TransferRunning  = "running"
	TransferDone     = "done"
	TransferError    = "error"
	TransferCanceled = "canceled"
)

// Transfer is a queued/running file transfer.
type Transfer struct {
	ID          string     `json:"id"`
	SrcFS       string     `json:"srcFs"`
	DstFS       string     `json:"dstFs"`
	SrcPaths    []string   `json:"srcPaths"`
	DstDir      string     `json:"dstDir"`
	Label       string     `json:"label"`
	State       string     `json:"state"`
	Error       string     `json:"error,omitempty"`
	TotalBytes  int64      `json:"totalBytes"`
	DoneBytes   int64      `json:"doneBytes"`
	TotalFiles  int        `json:"totalFiles"`
	DoneFiles   int        `json:"doneFiles"`
	CurrentFile string     `json:"currentFile,omitempty"`
	BytesPerSec int64      `json:"bytesPerSec"`
	OwnerID     string     `json:"-"`
	CreatedAt   time.Time  `json:"createdAt"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
}

// CPUStats is the CPU part of MonitorStats.
type CPUStats struct {
	Usage   float64   `json:"usage"`
	Cores   int       `json:"cores"`
	PerCore []float64 `json:"perCore"`
}

// MemStats is the memory part of MonitorStats (bytes).
type MemStats struct {
	Total     int64 `json:"total"`
	Used      int64 `json:"used"`
	Available int64 `json:"available"`
	SwapTotal int64 `json:"swapTotal"`
	SwapUsed  int64 `json:"swapUsed"`
}

// DiskStats is one mounted filesystem.
type DiskStats struct {
	Mount string `json:"mount"`
	FS    string `json:"fs"`
	Total int64  `json:"total"`
	Used  int64  `json:"used"`
}

// NetStats is one network interface.
type NetStats struct {
	Iface   string  `json:"iface"`
	RxBps   float64 `json:"rxBps"`
	TxBps   float64 `json:"txBps"`
	RxTotal int64   `json:"rxTotal"`
	TxTotal int64   `json:"txTotal"`
}

// MonitorStats is a remote host metrics snapshot (SPEC §6.0 Monitoring).
type MonitorStats struct {
	TS        time.Time   `json:"ts"`
	OS        string      `json:"os"`
	Hostname  string      `json:"hostname"`
	Kernel    string      `json:"kernel"`
	UptimeSec int64       `json:"uptimeSec"`
	Load      [3]float64  `json:"load"`
	CPU       CPUStats    `json:"cpu"`
	Mem       MemStats    `json:"mem"`
	Disks     []DiskStats `json:"disks"`
	Net       []NetStats  `json:"net"`
	Users     int         `json:"users"`
	Processes int         `json:"processes"`
}

// Process is one row of a remote process list.
type Process struct {
	PID     int     `json:"pid"`
	PPID    int     `json:"ppid"`
	User    string  `json:"user"`
	CPU     float64 `json:"cpu"`
	Mem     float64 `json:"mem"`
	RSS     int64   `json:"rss"`
	State   string  `json:"state"`
	Started string  `json:"started"`
	Command string  `json:"command"`
}

// ServerStatus is the status of an embedded server (http, tftp, sftp, ftp).
type ServerStatus struct {
	Kind      string         `json:"kind"`
	Running   bool           `json:"running"`
	Config    map[string]any `json:"config"`
	Error     string         `json:"error,omitempty"`
	Addr      string         `json:"addr,omitempty"`
	Clients   int            `json:"clients"`
	StartedAt *time.Time     `json:"startedAt,omitempty"`
}

func joinHostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}
