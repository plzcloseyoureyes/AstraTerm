package vnc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/term"
)

// Listening ("reverse") mode, GFX-17: Termstead accepts incoming RFB connections from VNC servers started with a
// "connect to viewer" option (x11vnc -connect, UltraVNC SC, TightVNC "attach listening viewer", …). Each accepted
// connection becomes a new VNC runtime session of the listener's owner (options.reverse = true) whose TCP stream is
// parked until a viewer attaches (the owner's UI opens a tab on the {type:'vnc.incoming'} event); it serves exactly
// one viewer. Listeners live in memory (not persisted); in server mode only administrators may open them, since they
// accept connections on the Termstead host.

const (
	defaultListenPort   = 5500
	maxListenersPerUser = 8
	maxListenersTotal   = 32
	maxPendingPerLn     = 8
	acceptsPerMinute    = 30
	unclaimedTimeout    = 2 * time.Minute
)

var errReverseUnavailable = errors.New("the incoming connection is no longer available (reverse sessions serve a single viewer; ask the server to connect again)")

// ListenerInfo is the JSON view of a listener.
type ListenerInfo struct {
	ID          string     `json:"id"`
	OwnerID     string     `json:"ownerId"`
	BindHost    string     `json:"bindHost"`
	Port        int        `json:"port"`
	Address     string     `json:"address"`
	HasPassword bool       `json:"hasPassword"`
	ViewOnly    bool       `json:"viewOnly"`
	Accepted    int64      `json:"accepted"`
	Pending     int        `json:"pending"`
	LastFrom    string     `json:"lastFrom,omitempty"`
	LastAt      *time.Time `json:"lastAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
}

type listener struct {
	id        string
	owner     *model.User
	bindHost  string
	port      int
	ln        net.Listener
	password  string // memory only, never returned
	viewOnly  bool
	createdAt time.Time
	accepted  atomic.Int64
	pending   atomic.Int32

	mu       sync.Mutex
	lastFrom string
	lastAt   time.Time
	window   time.Time
	inWindow int
	stopped  bool
}

func (l *listener) info() ListenerInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	li := ListenerInfo{ID: l.id, OwnerID: l.owner.ID, BindHost: l.bindHost, Port: l.port, Address: l.ln.Addr().String(),
		HasPassword: l.password != "", ViewOnly: l.viewOnly, Accepted: l.accepted.Load(), Pending: int(l.pending.Load()),
		LastFrom: l.lastFrom, CreatedAt: l.createdAt}
	if !l.lastAt.IsZero() {
		t := l.lastAt
		li.LastAt = &t
	}
	return li
}

// allow applies the per-listener accept rate limit.
func (l *listener) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.window) > time.Minute {
		l.window, l.inWindow = now, 0
	}
	l.inWindow++
	return l.inWindow <= acceptsPerMinute
}

type pendingReverse struct {
	conn     net.Conn
	listener *listener
	timer    *time.Timer
	once     sync.Once
}

func (p *pendingReverse) discard() {
	p.once.Do(func() {
		if p.timer != nil {
			p.timer.Stop()
		}
		_ = p.conn.Close()
		if p.listener != nil {
			p.listener.pending.Add(-1)
		}
	})
}

// takeReverse hands the parked incoming connection of a session to its viewer (once).
func (m *Module) takeReverse(sessionID string) (net.Conn, error) {
	m.mu.Lock()
	p := m.reverse[sessionID]
	delete(m.reverse, sessionID)
	m.mu.Unlock()
	if p == nil {
		return nil, errReverseUnavailable
	}
	claimed := false
	p.once.Do(func() {
		claimed = true
		if p.timer != nil {
			p.timer.Stop()
		}
		if p.listener != nil {
			p.listener.pending.Add(-1)
		}
	})
	if !claimed {
		return nil, errReverseUnavailable
	}
	return p.conn, nil
}

// listenAllowed: listening opens a port on the Termstead host.
func (m *Module) listenAllowed(user *model.User) bool {
	return user != nil && (user.IsAdmin() || (m.d.Cfg != nil && m.d.Cfg.IsDesktop()))
}

func (m *Module) handleListListeners(c *echo.Context) error {
	user := httpx.UserFrom(c)
	all := user.IsAdmin() && (c.QueryParam("all") == "1" || c.QueryParam("all") == "true")
	m.mu.Lock()
	out := []ListenerInfo{}
	for _, l := range m.listeners {
		if all || l.owner.ID == user.ID {
			out = append(out, l.info())
		}
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return c.JSON(http.StatusOK, out)
}

func (m *Module) handleStartListener(c *echo.Context) error {
	user := httpx.UserFrom(c)
	if !m.listenAllowed(user) {
		return httpx.Forbidden("listening for incoming VNC connections is available to administrators in server mode")
	}
	var req struct {
		Port     int    `json:"port"`
		BindHost string `json:"bindHost"`
		Password string `json:"password"`
		ViewOnly bool   `json:"viewOnly"`
	}
	if err := httpx.BindOptional(c, &req); err != nil {
		return err
	}
	if req.Port == 0 {
		req.Port = defaultListenPort
	}
	if req.Port < 1 || req.Port > 65535 {
		return httpx.BadRequest("port must be between 1 and 65535")
	}
	req.BindHost = strings.Trim(strings.TrimSpace(req.BindHost), "[]")
	switch {
	case req.BindHost == "", req.BindHost == "localhost":
	case net.ParseIP(req.BindHost) != nil:
	default:
		return httpx.BadRequest("bindHost must be an IP address (empty = all interfaces)")
	}
	if len(req.Password) > 1024 {
		return httpx.BadRequest("password is too long")
	}

	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return httpx.Conflict("the server is shutting down")
	}
	mine := 0
	for _, l := range m.listeners {
		if l.owner.ID == user.ID {
			mine++
		}
		if l.port == req.Port && (l.bindHost == req.BindHost || l.bindHost == "" || req.BindHost == "") {
			m.mu.Unlock()
			return httpx.Conflict(fmt.Sprintf("a VNC listener already uses port %d", req.Port))
		}
	}
	if mine >= maxListenersPerUser || len(m.listeners) >= maxListenersTotal {
		m.mu.Unlock()
		return httpx.Conflict("too many VNC listeners")
	}
	m.mu.Unlock()

	ln, err := net.Listen("tcp", net.JoinHostPort(req.BindHost, strconv.Itoa(req.Port)))
	if err != nil {
		switch {
		case errors.Is(err, syscall.EADDRINUSE):
			return httpx.Conflict(fmt.Sprintf("port %d is already in use on this host", req.Port))
		case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
			return httpx.Forbidden(fmt.Sprintf("not allowed to listen on port %d (ports below 1024 need privileges)", req.Port))
		}
		return httpx.BadRequest("cannot listen: " + err.Error())
	}
	owner := *user
	l := &listener{id: model.NewID(), owner: &owner, bindHost: req.BindHost, port: req.Port, ln: ln,
		password: req.Password, viewOnly: req.ViewOnly, createdAt: time.Now().UTC()}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		_ = ln.Close()
		return httpx.Conflict("the server is shutting down")
	}
	m.listeners[l.id] = l
	m.mu.Unlock()
	go m.acceptLoop(l)
	info := l.info()
	m.d.Audit.Log(c, "vnc.listen.start", info.Address, map[string]any{"port": req.Port, "bindHost": req.BindHost,
		"viewOnly": req.ViewOnly, "password": req.Password != ""})
	m.publishListeners(user.ID)
	return c.JSON(http.StatusCreated, info)
}

func (m *Module) handleStopListener(c *echo.Context) error {
	user := httpx.UserFrom(c)
	m.mu.Lock()
	l := m.listeners[c.Param("id")]
	m.mu.Unlock()
	if l == nil || (l.owner.ID != user.ID && !user.IsAdmin()) {
		return httpx.ErrNotFound
	}
	m.stopListener(l, "stopped")
	m.d.Audit.Log(c, "vnc.listen.stop", l.ln.Addr().String(), map[string]any{"accepted": l.accepted.Load()})
	return httpx.OK(c)
}

func (m *Module) stopListener(l *listener, reason string) {
	l.mu.Lock()
	already := l.stopped
	l.stopped = true
	l.mu.Unlock()
	if already {
		return
	}
	m.mu.Lock()
	delete(m.listeners, l.id)
	m.mu.Unlock()
	_ = l.ln.Close()
	m.log.Info("vnc: listener stopped", "addr", l.ln.Addr().String(), "reason", reason)
	m.publishListeners(l.owner.ID)
}

// listenersChanged is pushed to the owner whenever their listeners change.
type listenersChanged struct {
	Type string `json:"type"` // "vnc.listeners"
}

// incomingEvent announces a new reverse session to the listener's owner.
type incomingEvent struct {
	Type       string `json:"type"` // "vnc.incoming"
	SessionID  string `json:"sessionId"`
	ListenerID string `json:"listenerId"`
	From       string `json:"from"`
	Title      string `json:"title"`
	ViewOnly   bool   `json:"viewOnly"`
}

func (m *Module) publishListeners(userID string) {
	if m.d.Events != nil {
		m.d.Events.Publish(userID, listenersChanged{Type: "vnc.listeners"})
	}
}

func (m *Module) acceptLoop(l *listener) {
	var delay time.Duration
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			l.mu.Lock()
			stopped := l.stopped
			l.mu.Unlock()
			if stopped || errors.Is(err, net.ErrClosed) {
				return
			}
			// Temporary failure (e.g. too many open files): back off.
			delay = min(max(2*delay, 50*time.Millisecond), time.Second)
			m.log.Warn("vnc: accept failed", "addr", l.ln.Addr().String(), "err", err)
			time.Sleep(delay)
			continue
		}
		delay = 0
		go m.handleIncoming(l, conn)
	}
}

func (m *Module) handleIncoming(l *listener, conn net.Conn) {
	now := time.Now().UTC()
	from := conn.RemoteAddr().String()
	host, portStr, _ := net.SplitHostPort(from)
	port, _ := strconv.Atoi(portStr)
	if !l.allow(now) || l.pending.Load() >= maxPendingPerLn {
		m.log.Warn("vnc: rejecting incoming connection (rate limit)", "listener", l.id, "from", from)
		_ = conn.Close()
		return
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}
	l.pending.Add(1)
	l.accepted.Add(1)
	l.mu.Lock()
	l.lastFrom, l.lastAt = from, now
	l.mu.Unlock()

	ctx := httpx.WithUser(context.Background(), l.owner)
	title := "VNC from " + host
	secrets := map[string]string{}
	if l.password != "" {
		secrets[model.SecretVNCPassword] = l.password
	}
	s, err := m.c.Sessions.Create(ctx, l.owner, term.CreateRequest{
		Title:   title,
		Secrets: secrets,
		Connection: &model.Connection{
			Name:     title,
			Protocol: model.ProtoVNC,
			Host:     host,
			Port:     port,
			Options: model.Options{"reverse": true, "listenerId": l.id, "shared": true, "viewOnly": l.viewOnly,
				"listenAddress": l.ln.Addr().String()},
		},
	})
	if err != nil {
		l.pending.Add(-1)
		m.log.Warn("vnc: cannot create a session for an incoming connection", "from", from, "err", err)
		_ = conn.Close()
		return
	}
	p := &pendingReverse{conn: conn, listener: l}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		p.discard()
		_ = m.c.Sessions.Close(s.ID)
		return
	}
	m.reverse[s.ID] = p
	m.mu.Unlock()
	p.timer = time.AfterFunc(unclaimedTimeout, func() {
		m.mu.Lock()
		cur := m.reverse[s.ID]
		if cur == p {
			delete(m.reverse, s.ID)
		}
		m.mu.Unlock()
		if cur == p {
			p.discard()
			m.log.Info("vnc: incoming connection not claimed in time", "session", s.ID, "from", from)
			_ = m.c.Sessions.Close(s.ID)
		}
	})
	m.c.Sessions.SetState(s.ID, model.StateConnecting, "Incoming connection from "+from+": open the session to view it")
	m.audit(ctx, l.owner, "vnc.reverse.accept", s.ID, map[string]any{"from": from, "listenerId": l.id})
	if m.d.Events != nil {
		m.d.Events.Publish(l.owner.ID, incomingEvent{Type: "vnc.incoming", SessionID: s.ID, ListenerID: l.id,
			From: from, Title: title, ViewOnly: l.viewOnly})
	}
	m.publishListeners(l.owner.ID)
}
