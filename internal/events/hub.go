// Package events implements the per-user event hub behind /ws/events (SPEC §6.1): fan-out of server events to every
// socket of a user, the interactive prompt broker (SPEC §4), topic subscriptions (e.g. monitor) and background jobs.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v5"

	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
)

// Errors returned by the prompt broker.
var (
	// ErrNoInteractiveClient means the user has no connected events socket to answer a prompt (HTTP 409 via httpx).
	ErrNoInteractiveClient error = &model.Error{Code: model.CodeConflict, Msg: "no interactive client is connected to answer the prompt"}
	// ErrPromptTimeout means nobody answered within Hub.PromptTimeout.
	ErrPromptTimeout = errors.New("prompt timed out")
)

// Defaults.
const (
	DefaultPromptTimeout = 3 * time.Minute
	DefaultPingInterval  = 20 * time.Second
	sendQueueSize        = 256
	writeTimeout         = 10 * time.Second
	clientReadLimit      = 1 << 20
	maxSubsPerClient     = 256
)

// TopicHandler starts a subscription for one client. params is the subscribe message minus "type"/"topic" as a
// canonical JSON object (e.g. {"sessionId":"…"}). ctx is cancelled when the client unsubscribes or disconnects; the
// returned unsubscribe func (may be nil) is called at that point as well. Handlers push data with Hub.PublishClient.
// Handlers run in their own goroutine and may block (e.g. while dialing), but must honor ctx.
type TopicHandler func(ctx context.Context, user *model.User, clientID string, params json.RawMessage) (unsubscribe func(), err error)

// Hub fans events out to connected clients. Create it with NewHub.
type Hub struct {
	// PromptTimeout bounds how long Prompt waits for an answer.
	PromptTimeout time.Duration
	// PingInterval is the WebSocket keep-alive ping period.
	PingInterval time.Duration

	ctx context.Context
	log *slog.Logger

	mu      sync.RWMutex
	clients map[string]*client
	byUser  map[string]map[string]*client
	prompts map[string]*pendingPrompt
	topics  map[string]TopicHandler
}

// NewHub creates a hub bound to ctx (closing all sockets when ctx is cancelled).
func NewHub(ctx context.Context, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		PromptTimeout: DefaultPromptTimeout,
		PingInterval:  DefaultPingInterval,
		ctx:           ctx,
		log:           log,
		clients:       map[string]*client{},
		byUser:        map[string]map[string]*client{},
		prompts:       map[string]*pendingPrompt{},
		topics:        map[string]TopicHandler{},
	}
}

type client struct {
	id          string
	user        *model.User
	authSession string
	conn        *websocket.Conn
	send        chan []byte
	ctx         context.Context
	cancel      context.CancelFunc
	closeOnce   sync.Once

	subMu sync.Mutex
	subs  map[string]*subscription
}

// enqueue queues msg without blocking; a client whose queue is full is disconnected (slow consumer).
func (c *client) enqueue(msg []byte) bool {
	if c.ctx.Err() != nil {
		return false
	}
	select {
	case c.send <- msg:
		return true
	default:
		go c.close(websocket.StatusPolicyViolation, "client too slow")
		return false
	}
}

func (c *client) close(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		c.cancel()
		go func() {
			// Close performs the closing handshake (bounded internally); CloseNow as a fallback.
			if err := c.conn.Close(code, reason); err != nil {
				_ = c.conn.CloseNow()
			}
		}()
	})
}

// ---- websocket handler --------------------------------------------------------------------------------------------

type helloEvent struct {
	Type     string      `json:"type"`
	ClientID string      `json:"clientId"`
	User     *model.User `json:"user"`
}

// ServeWS is the /ws/events handler (mount with Router.WS so the user is authenticated and Origin checked).
func (h *Hub) ServeWS(c *echo.Context) error {
	user := httpx.UserFrom(c)
	if user == nil {
		return httpx.ErrUnauthorized
	}
	conn, err := httpx.AcceptWS(c, nil)
	if err != nil {
		h.log.Debug("events: accept failed", "err", err)
		return nil // the handshake error response has been written
	}
	conn.SetReadLimit(clientReadLimit)
	ctx, cancel := context.WithCancel(h.ctx)
	cl := &client{
		id:          model.NewID(),
		user:        user,
		authSession: httpx.AuthInfoFrom(c).SessionID,
		conn:        conn,
		send:        make(chan []byte, sendQueueSize),
		ctx:         ctx,
		cancel:      cancel,
		subs:        map[string]*subscription{},
	}
	hello, _ := json.Marshal(helloEvent{Type: model.EvHello, ClientID: cl.id, User: user})
	cl.enqueue(hello)
	h.add(cl)
	defer h.remove(cl)

	go h.writeLoop(cl)
	go h.pingLoop(cl)
	h.readLoop(cl)
	return nil
}

func (h *Hub) add(c *client) {
	h.mu.Lock()
	h.clients[c.id] = c
	m := h.byUser[c.user.ID]
	if m == nil {
		m = map[string]*client{}
		h.byUser[c.user.ID] = m
	}
	m[c.id] = c
	// Replay prompts still waiting for this user (e.g. after a page reload).
	var pending [][]byte
	for _, p := range h.prompts {
		if p.userID == c.user.ID {
			pending = append(pending, p.msg)
		}
	}
	h.mu.Unlock()
	for _, msg := range pending {
		c.enqueue(msg)
	}
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	delete(h.clients, c.id)
	if m := h.byUser[c.user.ID]; m != nil {
		delete(m, c.id)
		if len(m) == 0 {
			delete(h.byUser, c.user.ID)
		}
	}
	h.mu.Unlock()
	c.stopAllSubs()
	c.close(websocket.StatusNormalClosure, "")
}

func (h *Hub) writeLoop(c *client) {
	for {
		select {
		case <-c.ctx.Done():
			return
		case msg := <-c.send:
			ctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
			err := c.conn.Write(ctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				c.close(websocket.StatusGoingAway, "write failed")
				return
			}
		}
	}
}

func (h *Hub) pingLoop(c *client) {
	interval := h.PingInterval
	if interval <= 0 {
		interval = DefaultPingInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(c.ctx, interval)
			err := c.conn.Ping(ctx)
			cancel()
			if err != nil {
				c.close(websocket.StatusGoingAway, "ping timeout")
				return
			}
		}
	}
}

type clientMessage struct {
	Type   string   `json:"type"`
	ID     string   `json:"id"`
	Accept bool     `json:"accept"`
	Values []string `json:"values"`
	Save   bool     `json:"save"`
	Topic  string   `json:"topic"`
}

var pongMsg = []byte(`{"type":"pong"}`)

func (h *Hub) readLoop(c *client) {
	for {
		typ, data, err := c.conn.Read(c.ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var m clientMessage
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		switch m.Type {
		case "ping":
			c.enqueue(pongMsg)
		case "prompt.response":
			h.answerPrompt(c, m.ID, model.PromptResponse{Accept: m.Accept, Values: m.Values, Save: m.Save})
		case "subscribe":
			h.subscribe(c, m.Topic, data)
		case "unsubscribe":
			h.unsubscribe(c, m.Topic, data)
		}
	}
}

// ---- publishing ---------------------------------------------------------------------------------------------------

func marshal(ev any) ([]byte, bool) {
	switch t := ev.(type) {
	case []byte:
		return t, true
	case json.RawMessage:
		return t, true
	}
	b, err := json.Marshal(ev)
	if err != nil {
		slog.Error("events: cannot marshal event", "err", err)
		return nil, false
	}
	return b, true
}

// Publish sends ev (any JSON-marshalable value with a "type" field) to every socket of userID.
func (h *Hub) Publish(userID string, ev any) {
	msg, ok := marshal(ev)
	if !ok {
		return
	}
	h.mu.RLock()
	targets := make([]*client, 0, len(h.byUser[userID]))
	for _, c := range h.byUser[userID] {
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.enqueue(msg)
	}
}

// Broadcast sends ev to every connected socket.
func (h *Hub) Broadcast(ev any) {
	msg, ok := marshal(ev)
	if !ok {
		return
	}
	h.mu.RLock()
	targets := make([]*client, 0, len(h.clients))
	for _, c := range h.clients {
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.enqueue(msg)
	}
}

// PublishClient sends ev to one socket (used by topic subscriptions). It returns false if the client is gone.
func (h *Hub) PublishClient(clientID string, ev any) bool {
	msg, ok := marshal(ev)
	if !ok {
		return false
	}
	h.mu.RLock()
	c := h.clients[clientID]
	h.mu.RUnlock()
	if c == nil {
		return false
	}
	return c.enqueue(msg)
}

// HasClient reports whether userID has at least one connected socket.
func (h *Hub) HasClient(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byUser[userID]) > 0
}

// ClientCount returns the number of connected sockets.
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// CloseUser disconnects every socket of userID (e.g. when the account is disabled or deleted).
func (h *Hub) CloseUser(userID string) {
	h.closeWhere(func(c *client) bool { return c.user.ID == userID })
}

// CloseAuthSession disconnects the sockets opened with a given login session (logout / session revocation).
func (h *Hub) CloseAuthSession(sessionID string) {
	if sessionID == "" {
		return
	}
	h.closeWhere(func(c *client) bool { return c.authSession == sessionID })
}

// CloseUserSessionsExcept disconnects userID's sockets opened with a login session other than keep (sockets
// authenticated by API tokens are left alone). Used after a password change.
func (h *Hub) CloseUserSessionsExcept(userID, keep string) {
	h.closeWhere(func(c *client) bool { return c.user.ID == userID && c.authSession != "" && c.authSession != keep })
}

func (h *Hub) closeWhere(match func(*client) bool) {
	h.mu.RLock()
	var targets []*client
	for _, c := range h.clients {
		if match(c) {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.close(websocket.StatusPolicyViolation, "session ended")
	}
}

// ---- topics -------------------------------------------------------------------------------------------------------

type subscription struct {
	cancel context.CancelFunc
	mu     sync.Mutex
	unsub  func()
	done   bool
}

func (s *subscription) stop() {
	s.mu.Lock()
	s.done = true
	u := s.unsub
	s.unsub = nil
	s.mu.Unlock()
	s.cancel()
	if u != nil {
		safeCall(u)
	}
}

type subscribeError struct {
	Type   string          `json:"type"`
	Topic  string          `json:"topic"`
	Params json.RawMessage `json:"params,omitempty"`
	Error  string          `json:"error"`
}

// RegisterTopic installs the handler for {type:'subscribe', topic}. Registering a topic twice replaces the handler.
func (h *Hub) RegisterTopic(topic string, fn TopicHandler) {
	h.mu.Lock()
	h.topics[topic] = fn
	h.mu.Unlock()
}

// subscriptionKey returns the canonical params (message minus type/topic) and the per-client subscription key.
func subscriptionKey(topic string, data []byte) (json.RawMessage, string) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		m = map[string]json.RawMessage{}
	}
	delete(m, "type")
	delete(m, "topic")
	params, _ := json.Marshal(m) // map keys are sorted → canonical
	return params, topic + "\x00" + string(params)
}

func (h *Hub) subscribe(c *client, topic string, data []byte) {
	params, key := subscriptionKey(topic, data)
	h.mu.RLock()
	fn := h.topics[topic]
	h.mu.RUnlock()
	fail := func(msg string) {
		b, _ := json.Marshal(subscribeError{Type: model.EvSubscribeError, Topic: topic, Params: params, Error: msg})
		c.enqueue(b)
	}
	if fn == nil {
		fail("unknown topic")
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	sub := &subscription{cancel: cancel}

	c.subMu.Lock()
	if old := c.subs[key]; old != nil {
		delete(c.subs, key)
		c.subMu.Unlock()
		old.stop()
		c.subMu.Lock()
	}
	if len(c.subs) >= maxSubsPerClient {
		c.subMu.Unlock()
		cancel()
		fail("too many subscriptions")
		return
	}
	c.subs[key] = sub
	c.subMu.Unlock()

	go func() {
		var (
			unsub func()
			err   error
		)
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = errors.New("internal error")
					h.log.Error("events: topic handler panicked", "topic", topic, "panic", r)
				}
			}()
			unsub, err = fn(ctx, c.user, c.id, params)
		}()
		if err != nil {
			c.subMu.Lock()
			if c.subs[key] == sub {
				delete(c.subs, key)
			}
			c.subMu.Unlock()
			cancel()
			if unsub != nil {
				safeCall(unsub)
			}
			if !errors.Is(err, context.Canceled) {
				fail(err.Error())
			}
			return
		}
		sub.mu.Lock()
		if sub.done {
			sub.mu.Unlock()
			if unsub != nil {
				safeCall(unsub)
			}
			return
		}
		sub.unsub = unsub
		sub.mu.Unlock()
	}()
}

func (h *Hub) unsubscribe(c *client, topic string, data []byte) {
	_, key := subscriptionKey(topic, data)
	c.subMu.Lock()
	sub := c.subs[key]
	delete(c.subs, key)
	c.subMu.Unlock()
	if sub != nil {
		sub.stop()
	}
}

func (c *client) stopAllSubs() {
	c.subMu.Lock()
	subs := c.subs
	c.subs = map[string]*subscription{}
	c.subMu.Unlock()
	for _, s := range subs {
		s.stop()
	}
}

func safeCall(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("events: unsubscribe panicked", "panic", r)
		}
	}()
	fn()
}
