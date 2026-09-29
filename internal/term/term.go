// Package term is AstraTerm's runtime session manager (SPEC §4 "Terminal protocol registry", §6.0 runtime sessions,
// §6.2 terminal WebSocket). Sessions live in the Go process, independent of browser sockets: backend output is
// pumped into an offset-addressed ring buffer, fanned out to any number of attached WebSocket clients with ack-based
// flow control, scanned for OSC title/cwd/prompt marks, optionally recorded (asciicast v3) and logged (plain text).
// Protocol modules (ssh, local, telnet, ...) plug in through RegisterProtocol.
package term

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Backend is a live terminal connection produced by an Opener.
//
// Read returns remote output and blocks until data is available. It must return io.EOF when the remote side ended
// normally (process exited, exit status received) and any other error when the connection was lost — only the
// latter triggers options.autoReconnect. Close must unblock a pending Read.
type Backend interface {
	io.Reader
	io.Writer
	Resize(cols, rows int) error
	Close() error
}

// Breaker is implemented by backends that can send a BREAK (serial, telnet, ssh).
type Breaker interface{ SendBreak() error }

// Signaler is implemented by backends that can deliver signals (INT, TERM, KILL, HUP, QUIT, USR1, USR2).
type Signaler interface{ Signal(name string) error }

// ExitCoder is implemented by backends that know the exit status after Read returned. A negative value means
// "unknown".
type ExitCoder interface{ ExitCode() int }

// OpenRequest carries everything an Opener needs.
type OpenRequest struct {
	Session    *Session          // runtime session being opened (ID, owner, size, protocol, ...)
	Connection *model.Connection // resolved saved connection (or synthesized for quick-connect); private copy
	Secrets    map[string]string // decrypted secrets of the connection (+ identity, + remembered prompt answers)
	User       *model.User       // session owner
}

// Opener dials/spawns the backend of a session. ctx is cancelled when the session is closed or reconnected; the
// session is also available through SessionFromContext(ctx). Openers may prompt the user (events prompt broker).
type Opener func(ctx context.Context, req OpenRequest) (Backend, error)

// Policy vets a session request before it is created (e.g. "local shells are admin-only in server mode"). A non-nil
// error is returned to the API caller as is.
type Policy func(ctx context.Context, user *model.User, conn *model.Connection) error

var registry = struct {
	sync.RWMutex
	openers  map[string]Opener
	policies map[string]Policy
}{openers: map[string]Opener{}, policies: map[string]Policy{}}

// RegisterProtocol installs the opener for a protocol (registering again replaces it).
func RegisterProtocol(protocol string, open Opener) {
	registry.Lock()
	registry.openers[protocol] = open
	registry.Unlock()
}

// RegisterPolicy installs a creation policy for a protocol (extension of SPEC §4; see §9 B1 notes).
func RegisterPolicy(protocol string, p Policy) {
	registry.Lock()
	registry.policies[protocol] = p
	registry.Unlock()
}

func lookupOpener(protocol string) Opener {
	registry.RLock()
	defer registry.RUnlock()
	return registry.openers[protocol]
}

func lookupPolicy(protocol string) Policy {
	registry.RLock()
	defer registry.RUnlock()
	return registry.policies[protocol]
}

// ---- errors -------------------------------------------------------------------------------------------------------

// Errors returned by the manager.
var (
	ErrNotConnected = &model.Error{Code: model.CodeConflict, Msg: "session is not connected"}
	ErrClosed       = &model.Error{Code: model.CodeConflict, Msg: "session is closed"}
	ErrReadOnly     = &model.Error{Code: model.CodeForbidden, Msg: "read-only session view"}
	ErrUnsupported  = &model.Error{Code: model.CodeBadRequest, Msg: "not supported by this session"}
)

// permanentError marks an opener failure that must not be retried automatically (authentication failure, rejected
// host key, user cancellation).
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent wraps err so autoReconnect does not retry it (PROTO-39: never auto-retry after an auth failure or a
// host-key mismatch).
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	var pe *permanentError
	if errors.As(err, &pe) {
		return err
	}
	return &permanentError{err: err}
}

// IsPermanent reports whether err (or a wrapped error) was marked with Permanent.
func IsPermanent(err error) bool {
	var pe *permanentError
	return errors.As(err, &pe)
}

// ---- context ------------------------------------------------------------------------------------------------------

type ctxKey struct{}

// WithSession returns ctx carrying s (set by the manager for openers).
func WithSession(ctx context.Context, s *Session) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// SessionFromContext returns the session being opened, or nil.
func SessionFromContext(ctx context.Context) *Session {
	s, _ := ctx.Value(ctxKey{}).(*Session)
	return s
}

// ---- limits -------------------------------------------------------------------------------------------------------

// Flow-control and framing parameters (SPEC §6.2).
const (
	HighWater     = 1 << 20              // stop sending to a client above this many unacknowledged bytes
	LowWater      = 256 << 10            // resume below this
	MaxFrame      = 32 << 10             // largest binary output frame
	CoalesceDelay = 8 * time.Millisecond // minimum spacing of partial output frames
	MinScrollback = 2 * HighWater        // smallest ring buffer (larger than the flow-control window)
	MinSize       = 2
	MaxSize       = 1000
	readChunk     = 32 << 10
)

// ClampSize clamps a terminal dimension to [MinSize, MaxSize]; 0 means "unset" and yields def.
func ClampSize(v, def int) int {
	if v <= 0 {
		v = def
	}
	return min(max(v, MinSize), MaxSize)
}
