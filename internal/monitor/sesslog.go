package monitor

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/term"
)

// sessionLog keeps, per SSH runtime session, what the SSH layer does not expose (SSH-38 "reconnect count", MON-4
// "throughput"): how often the session connected, when and why it last dropped, and the bytes of its terminal stream.
// It is fed by the term hooks, so it covers the session's whole life even while nobody watches the monitor. Traffic
// counts the terminal channel only: SFTP, port forwards and monitoring on the same transport are not included.
type sessionLog struct {
	mu           sync.Mutex
	connects     int
	connected    bool
	firstConnect time.Time
	lastDrop     time.Time
	lastDropMsg  string

	in  atomic.Int64 // bytes received from the server (terminal output)
	out atomic.Int64 // bytes sent to the server (keystrokes, pastes)
}

// sessionLogFor returns (creating) the log of a session.
func (s *Service) sessionLogFor(id string) *sessionLog {
	if v, ok := s.sessLogs.Load(id); ok {
		return v.(*sessionLog)
	}
	v, _ := s.sessLogs.LoadOrStore(id, &sessionLog{})
	return v.(*sessionLog)
}

// trackState records connects and drops (called from the OnState hook, without session locks). Only state changes
// create a session's log: output can still be delivered after OnClose deleted it (the pump runs hooks outside the
// session lock), and must not bring it back.
func (s *Service) trackState(sess *term.Session, st model.SessionState) {
	if sess.Protocol != model.ProtoSSH {
		return
	}
	switch st {
	case model.StateConnecting, model.StateAuthenticating:
		s.sessionLogFor(sess.ID)
	case model.StateConnected:
		l := s.sessionLogFor(sess.ID)
		l.mu.Lock()
		if l.connects == 0 {
			l.firstConnect = time.Now()
		}
		l.connects++
		l.connected = true
		l.mu.Unlock()
	case model.StateDisconnected, model.StateError:
		l := s.sessionLogFor(sess.ID)
		_, msg := sess.State()
		l.mu.Lock()
		if l.connected {
			l.lastDrop, l.lastDropMsg = time.Now(), clip(msg, 300)
		}
		l.connected = false
		l.mu.Unlock()
	}
	if sess.Closed() { // a state change racing with the close must not leave an entry behind
		s.sessLogs.Delete(sess.ID)
	}
}

// onOutput / onInput count terminal bytes: the pump calls them synchronously, so they only do a lookup and an
// atomic add (never create an entry, see trackState).
func (s *Service) onOutput(sess *term.Session, data []byte) {
	if v, ok := s.sessLogs.Load(sess.ID); ok {
		v.(*sessionLog).in.Add(int64(len(data)))
	}
}

func (s *Service) onInput(sess *term.Session, data []byte) {
	if v, ok := s.sessLogs.Load(sess.ID); ok {
		v.(*sessionLog).out.Add(int64(len(data)))
	}
}

// sessionHistory is the part of SSHInfo the session log provides.
type sessionHistory struct {
	Reconnects       int        `json:"reconnects"`
	FirstConnectedAt *time.Time `json:"firstConnectedAt,omitempty"`
	LastDropAt       *time.Time `json:"lastDisconnectAt,omitempty"`
	LastDrop         string     `json:"lastDisconnect,omitempty"`
	TermBytesIn      int64      `json:"termBytesIn"`
	TermBytesOut     int64      `json:"termBytesOut"`
}

func (s *Service) sessionHistory(id string) sessionHistory {
	v, ok := s.sessLogs.Load(id)
	if !ok {
		return sessionHistory{}
	}
	l := v.(*sessionLog)
	h := sessionHistory{TermBytesIn: l.in.Load(), TermBytesOut: l.out.Load()}
	l.mu.Lock()
	defer l.mu.Unlock()
	h.Reconnects = max(l.connects-1, 0)
	if !l.firstConnect.IsZero() {
		t := l.firstConnect.UTC()
		h.FirstConnectedAt = &t
	}
	if !l.lastDrop.IsZero() {
		t := l.lastDrop.UTC()
		h.LastDropAt, h.LastDrop = &t, l.lastDropMsg
	}
	return h
}
