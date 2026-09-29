package servers

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Event topics (subscribe with {type:'subscribe', topic, ...params} on /ws/events).
const (
	topicStatus = "servers"     // → {type:'server', status}
	topicLogs   = "servers.log" // {kind} → {type:'server.log', kind, entries, dropped?}
	topicSyslog = "syslog"      // → {type:'syslog', messages, dropped?}

	evServerLog = "server.log"
	evSyslog    = "syslog"
)

type statusEvent struct {
	Type   string `json:"type"`
	Status Status `json:"status"`
}

type logEvent struct {
	Type    string     `json:"type"`
	Kind    Kind       `json:"kind"`
	Entries []LogEntry `json:"entries"`
	Dropped int        `json:"dropped,omitempty"`
}

type syslogEvent struct {
	Type     string          `json:"type"`
	Messages []SyslogMessage `json:"messages"`
	Dropped  int             `json:"dropped,omitempty"`
}

type subscriber struct {
	clientID string
	userID   string
	kind     Kind // topicLogs only
}

// subscribers tracks the event-socket clients subscribed to the servers topics. Access is checked when subscribing
// and, in server mode, re-checked periodically (a demoted or disabled administrator stops receiving events).
type subscribers struct {
	m *Manager

	mu     sync.Mutex
	status map[string]subscriber // key: clientID
	logs   map[string]subscriber // key: clientID + "\x00" + kind
	syslog map[string]subscriber // key: clientID
}

func newSubscribers(m *Manager) *subscribers {
	return &subscribers{m: m, status: map[string]subscriber{}, logs: map[string]subscriber{}, syslog: map[string]subscriber{}}
}

// register installs the topic handlers on the events hub.
func (s *subscribers) register() {
	hub := s.m.d.Events
	hub.RegisterTopic(topicStatus, func(_ context.Context, user *model.User, clientID string, _ json.RawMessage) (func(), error) {
		if err := s.m.allowed(user); err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.status[clientID] = subscriber{clientID: clientID, userID: user.ID}
		s.mu.Unlock()
		return func() {
			s.mu.Lock()
			delete(s.status, clientID)
			s.mu.Unlock()
		}, nil
	})
	hub.RegisterTopic(topicLogs, func(_ context.Context, user *model.User, clientID string, params json.RawMessage) (func(), error) {
		if err := s.m.allowed(user); err != nil {
			return nil, err
		}
		var p struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(params, &p)
		k, ok := parseKind(p.Kind)
		if !ok {
			return nil, errors.New("unknown server kind")
		}
		key := clientID + "\x00" + string(k)
		s.mu.Lock()
		s.logs[key] = subscriber{clientID: clientID, userID: user.ID, kind: k}
		s.mu.Unlock()
		return func() {
			s.mu.Lock()
			delete(s.logs, key)
			s.mu.Unlock()
		}, nil
	})
	hub.RegisterTopic(topicSyslog, func(_ context.Context, user *model.User, clientID string, _ json.RawMessage) (func(), error) {
		if err := s.m.allowed(user); err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.syslog[clientID] = subscriber{clientID: clientID, userID: user.ID}
		s.mu.Unlock()
		return func() {
			s.mu.Lock()
			delete(s.syslog, clientID)
			s.mu.Unlock()
		}, nil
	})
}

// send delivers msg to the subscribers of one map, dropping those whose socket is gone.
func (s *subscribers) send(set map[string]subscriber, match func(subscriber) bool, msg []byte) {
	s.mu.Lock()
	targets := make(map[string]string, len(set))
	for key, sub := range set {
		if match == nil || match(sub) {
			targets[key] = sub.clientID
		}
	}
	s.mu.Unlock()
	var gone []string
	for key, clientID := range targets {
		if !s.m.d.Events.PublishClient(clientID, msg) {
			gone = append(gone, key)
		}
	}
	if len(gone) > 0 {
		s.mu.Lock()
		for _, key := range gone {
			delete(set, key)
		}
		s.mu.Unlock()
	}
}

func (s *subscribers) publishStatus(st Status) {
	msg, err := json.Marshal(statusEvent{Type: model.EvServer, Status: st})
	if err != nil {
		return
	}
	s.send(s.status, nil, msg)
}

func (s *subscribers) publishLog(k Kind, entries []LogEntry, dropped int) {
	s.mu.Lock()
	found := false
	for _, sub := range s.logs {
		if sub.kind == k {
			found = true
			break
		}
	}
	s.mu.Unlock()
	if !found {
		return
	}
	msg, err := json.Marshal(logEvent{Type: evServerLog, Kind: k, Entries: entries, Dropped: dropped})
	if err != nil {
		return
	}
	s.send(s.logs, func(sub subscriber) bool { return sub.kind == k }, msg)
}

func (s *subscribers) hasSyslog() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.syslog) > 0
}

func (s *subscribers) publishSyslog(msgs []SyslogMessage, dropped int) {
	msg, err := json.Marshal(syslogEvent{Type: evSyslog, Messages: msgs, Dropped: dropped})
	if err != nil {
		return
	}
	s.send(s.syslog, nil, msg)
}

// revalidate drops the subscriptions of users who may no longer use the servers (server mode: role changes).
func (s *subscribers) revalidate(ctx context.Context) {
	s.mu.Lock()
	users := map[string]bool{}
	for _, set := range []map[string]subscriber{s.status, s.logs, s.syslog} {
		for _, sub := range set {
			users[sub.userID] = true
		}
	}
	s.mu.Unlock()
	denied := map[string]bool{}
	for id := range users {
		u, err := s.m.d.Store.Users.Get(ctx, id)
		if err != nil && !errors.Is(err, model.ErrNotFound) {
			continue // transient store error: keep
		}
		if err != nil || s.m.allowed(u) != nil {
			denied[id] = true
		}
	}
	if len(denied) == 0 {
		return
	}
	s.mu.Lock()
	for _, set := range []map[string]subscriber{s.status, s.logs, s.syslog} {
		for key, sub := range set {
			if denied[sub.userID] {
				delete(set, key)
			}
		}
	}
	s.mu.Unlock()
}

// revalidateLoop re-checks subscribers every interval until ctx ends (server mode only).
func (s *subscribers) revalidateLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.revalidate(ctx)
		}
	}
}
