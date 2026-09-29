// Package model holds the shared domain structs that make up Termstead's JSON contract (docs/SPEC.md §5, §6). It is a
// leaf package: it must not import anything from the project. Field names mirror web/src/api/types.ts.
package model

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// idEncoding is lowercase RFC 4648 base32 without padding.
var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NewID returns a 20-character lowercase base32 random identifier (100 bits of entropy).
func NewID() string {
	var b [13]byte // 13 bytes → 21 base32 chars; truncated to 20 (100 bits)
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read never returns an error on supported platforms (Go ≥1.24 crashes instead).
		panic("model: crypto/rand failed: " + err.Error())
	}
	return idEncoding.EncodeToString(b[:])[:20]
}

// ValidID reports whether s looks like an ID produced by NewID (cheap input sanity check for path parameters).
func ValidID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// Error is a typed domain error with a stable machine-readable code (e.g. "not_found"). Transport layers (httpx) map
// codes to HTTP statuses. Two errors with the same code match under errors.Is, which lets lower layers (store, vault)
// signal conditions without importing httpx.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// ErrorCode returns the machine-readable code.
func (e *Error) ErrorCode() string { return e.Code }

// Is matches any error exposing the same ErrorCode().
func (e *Error) Is(target error) bool {
	t, ok := target.(interface{ ErrorCode() string })
	return ok && t.ErrorCode() == e.Code
}

// Error codes shared by every layer.
const (
	CodeNotFound        = "not_found"
	CodeForbidden       = "forbidden"
	CodeUnauthorized    = "unauthorized"
	CodeBadRequest      = "bad_request"
	CodeConflict        = "conflict"
	CodeLocked          = "locked"
	CodeTooManyRequests = "too_many_requests"
	CodeInternal        = "internal"
)

// Sentinel domain errors. Wrap them with fmt.Errorf("...: %w", model.ErrNotFound) to add context.
var (
	ErrNotFound  = &Error{Code: CodeNotFound, Msg: "not found"}
	ErrForbidden = &Error{Code: CodeForbidden, Msg: "forbidden"}
	ErrConflict  = &Error{Code: CodeConflict, Msg: "conflict"}
	ErrLocked    = &Error{Code: CodeLocked, Msg: "vault is locked"}
)

// Role of a user account.
type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r == RoleAdmin || r == RoleUser }

// Protocol identifies a connection / session backend.
type Protocol string

const (
	ProtoSSH    Protocol = "ssh"
	ProtoTelnet Protocol = "telnet"
	ProtoRlogin Protocol = "rlogin"
	ProtoRaw    Protocol = "raw"
	ProtoSerial Protocol = "serial"
	ProtoLocal  Protocol = "local"
	ProtoMosh   Protocol = "mosh"
	ProtoDocker Protocol = "docker"
	ProtoKube   Protocol = "kube"
	ProtoSFTP   Protocol = "sftp"
	ProtoFTP    Protocol = "ftp"
	ProtoS3     Protocol = "s3"
	ProtoVNC    Protocol = "vnc"
	ProtoRDP    Protocol = "rdp"
	ProtoWinRM  Protocol = "winrm"
	ProtoIPMI   Protocol = "ipmi"
	ProtoWeb    Protocol = "web" // saved browser/web session (webproxy module); no terminal backend
)

// KnownProtocols lists the protocols of SPEC §5.2 plus the §10.1 extension protocols.
var KnownProtocols = []Protocol{ProtoSSH, ProtoTelnet, ProtoRlogin, ProtoRaw, ProtoSerial, ProtoLocal, ProtoMosh,
	ProtoDocker, ProtoKube, ProtoSFTP, ProtoFTP, ProtoS3, ProtoVNC, ProtoRDP, ProtoWinRM, ProtoIPMI, ProtoWeb}

// DefaultPort returns the conventional port for p (0 when the protocol has no network port).
func DefaultPort(p Protocol) int {
	switch p {
	case ProtoSSH, ProtoSFTP, ProtoMosh:
		return 22
	case ProtoTelnet:
		return 23
	case ProtoRlogin:
		return 513
	case ProtoFTP:
		return 21
	case ProtoVNC:
		return 5900
	case ProtoRDP:
		return 3389
	case ProtoWinRM:
		return 5985
	case ProtoIPMI:
		return 623
	case ProtoS3, ProtoWeb:
		return 443
	}
	return 0
}

// ValidProtocol reports whether p is syntactically acceptable: a known protocol or a short lowercase token (so feature
// modules can introduce new protocols without touching the core).
func ValidProtocol(p Protocol) bool {
	s := string(p)
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || (i > 0 && (c == '-' || c == '.' || c == '+'))) {
			return false
		}
	}
	return true
}

// KindForProtocol derives the runtime session kind of a protocol.
func KindForProtocol(p Protocol) SessionKind {
	switch p {
	case ProtoVNC:
		return KindVNC
	case ProtoRDP:
		return KindRDP
	}
	return KindTerminal
}

// Auth methods for Connection.AuthMethod.
const (
	AuthAuto                = "auto"
	AuthPassword            = "password"
	AuthKey                 = "key"
	AuthAgent               = "agent"
	AuthKeyboardInteractive = "keyboard-interactive"
	AuthNone                = "none"
)

// ValidAuthMethod reports whether m is a known auth method.
func ValidAuthMethod(m string) bool {
	switch m {
	case AuthAuto, AuthPassword, AuthKey, AuthAgent, AuthKeyboardInteractive, AuthNone:
		return true
	}
	return false
}

// Known secret keys (SPEC §5.3). Other keys are allowed and stored the same way.
const (
	SecretPassword        = "password"
	SecretPassphrase      = "passphrase"
	SecretProxyPassword   = "proxyPassword"
	SecretVNCPassword     = "vncPassword"
	SecretSecretAccessKey = "secretAccessKey"
	SecretGatewayPassword = "gatewayPassword"
	SecretSudoPassword    = "sudoPassword"
)

// Event types sent over /ws/events (SPEC §6.1).
const (
	EvHello          = "hello"
	EvSessionUpdated = "session.updated"
	EvSessionClosed  = "session.closed"
	EvPrompt         = "prompt"
	EvPromptCancel   = "prompt.cancel"
	EvTransfer       = "transfer"
	EvTunnel         = "tunnel"
	EvMonitor        = "monitor"
	EvJob            = "job"
	EvNotify         = "notify"
	EvServer         = "server"
	EvVault          = "vault"
	EvPong           = "pong"
	EvSubscribeError = "subscribe.error"
)

// NotifyEvent is a user-visible toast ({type:'notify'}).
type NotifyEvent struct {
	Type    string `json:"type"` // always "notify"
	Level   string `json:"level"`
	Title   string `json:"title"`
	Message string `json:"message,omitempty"`
}

// Notify builds a notify event. level is one of info|success|warning|error.
func Notify(level, title, message string) NotifyEvent {
	return NotifyEvent{Type: EvNotify, Level: level, Title: title, Message: message}
}

// Settings is a free-form settings object (user settings merged over global settings).
type Settings map[string]any

// Mode of the running server.
const (
	ModeDesktop = "desktop"
	ModeServer  = "server"
)

// TrimLower is a small helper for case-insensitive keys (usernames).
func TrimLower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
