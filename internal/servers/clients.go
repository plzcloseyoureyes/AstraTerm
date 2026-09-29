package servers

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Connection limits of one server run (resource exhaustion by connection floods, slowloris). Connections over a limit
// are closed at once; the refusal is logged at most every limitLogEvery.
const (
	maxClients      = 256
	maxClientsPerIP = 32 // per address, per /64 for IPv6 (limitKey)
	limitLogEvery   = 10 * time.Second
)

var (
	errClientsClosed  = errors.New("the server is stopping")
	errTooManyClients = errors.New("too many connections")
)

// ClientInfo is one connected client (GET /api/servers/{kind}/clients).
type ClientInfo struct {
	ID       string    `json:"id"`
	Addr     string    `json:"addr"`
	User     string    `json:"user,omitempty"`
	Since    time.Time `json:"since"`
	Activity string    `json:"activity,omitempty"`
	BytesIn  int64     `json:"bytesIn"`
	BytesOut int64     `json:"bytesOut"`
}

// Stats are the counters of one server run.
type Stats struct {
	Connections  int64 `json:"connections"`
	BytesIn      int64 `json:"bytesIn"`
	BytesOut     int64 `json:"bytesOut"`
	Transfers    int64 `json:"transfers"`
	AuthFailures int64 `json:"authFailures"`
	Messages     int64 `json:"messages,omitempty"`
}

type counters struct {
	conns, bytesIn, bytesOut, transfers, authFailures, messages atomic.Int64
}

func (c *counters) snapshot() Stats {
	return Stats{Connections: c.conns.Load(), BytesIn: c.bytesIn.Load(), BytesOut: c.bytesOut.Load(),
		Transfers: c.transfers.Load(), AuthFailures: c.authFailures.Load(), Messages: c.messages.Load()}
}

// client is a tracked connection.
type client struct {
	id    string
	addr  string
	key   string // limitKey of the remote address
	since time.Time
	close func()

	mu       sync.Mutex
	user     string
	activity string
	bytesIn  atomic.Int64
	bytesOut atomic.Int64
}

func (c *client) setUser(u string) {
	c.mu.Lock()
	c.user = u
	c.mu.Unlock()
}

func (c *client) getUser() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.user
}

func (c *client) setActivity(a string) {
	c.mu.Lock()
	c.activity = sanitizeLog(a, 200)
	c.mu.Unlock()
}

func (c *client) info() ClientInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ClientInfo{ID: c.id, Addr: c.addr, User: c.user, Since: c.since, Activity: c.activity,
		BytesIn: c.bytesIn.Load(), BytesOut: c.bytesOut.Load()}
}

// clientSet tracks the connected clients of one server run. changed is called (outside the lock) after every add
// or removal.
type clientSet struct {
	mu       sync.Mutex
	seq      uint64
	m        map[string]*client
	perIP    map[string]int
	closed   bool
	changed  func()
	stats    *counters
	max      int
	maxPerIP int
	// onLimit reports a refused connection (called outside the lock, rate limited).
	onLimit   func(addr, why string)
	lastLimit time.Time
}

func newClientSet(stats *counters, changed func()) *clientSet {
	return &clientSet{m: map[string]*client{}, perIP: map[string]int{}, changed: changed, stats: stats,
		max: maxClients, maxPerIP: maxClientsPerIP}
}

// add tracks a new client; close disconnects it. It fails when the set is closed (server stopping) or a connection
// limit is reached: the caller must then drop the connection.
func (s *clientSet) add(addr string, close func()) (*client, error) {
	key := limitKey(remoteIP(addr))
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errClientsClosed
	}
	why := ""
	switch {
	case s.max > 0 && len(s.m) >= s.max:
		why = fmt.Sprintf("the server already serves %d connections", len(s.m))
	case s.maxPerIP > 0 && s.perIP[key] >= s.maxPerIP:
		why = fmt.Sprintf("%d connections from this address already", s.perIP[key])
	}
	if why != "" {
		report := time.Since(s.lastLimit) >= limitLogEvery
		if report {
			s.lastLimit = time.Now()
		}
		s.mu.Unlock()
		if report && s.onLimit != nil {
			s.onLimit(addr, why)
		}
		return nil, errTooManyClients
	}
	s.seq++
	c := &client{id: strconv.FormatUint(s.seq, 10), addr: addr, key: key, since: time.Now().UTC(), close: close}
	s.m[c.id] = c
	s.perIP[key]++
	s.mu.Unlock()
	s.stats.conns.Add(1)
	if s.changed != nil {
		s.changed()
	}
	return c, nil
}

func (s *clientSet) remove(c *client) {
	if c == nil {
		return
	}
	s.mu.Lock()
	_, ok := s.m[c.id]
	if ok {
		delete(s.m, c.id)
		if s.perIP[c.key]--; s.perIP[c.key] <= 0 {
			delete(s.perIP, c.key)
		}
	}
	s.mu.Unlock()
	if ok && s.changed != nil {
		s.changed()
	}
}

func (s *clientSet) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

func (s *clientSet) list() []ClientInfo {
	s.mu.Lock()
	cs := make([]*client, 0, len(s.m))
	for _, c := range s.m {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	out := make([]ClientInfo, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// kick disconnects one client; false when unknown.
func (s *clientSet) kick(id string) bool {
	s.mu.Lock()
	c := s.m[id]
	s.mu.Unlock()
	if c == nil {
		return false
	}
	if c.close != nil {
		c.close()
	}
	return true
}

// closeAll marks the set closed (later adds fail) and disconnects every client.
func (s *clientSet) closeAll() {
	s.mu.Lock()
	s.closed = true
	cs := make([]*client, 0, len(s.m))
	for _, c := range s.m {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	for _, c := range cs {
		if c.close != nil {
			c.close()
		}
	}
}

// countingConn counts bytes in both directions into the server stats and the client.
type countingConn struct {
	net.Conn
	stats *counters
	cl    *client
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.stats.bytesIn.Add(int64(n))
		if c.cl != nil {
			c.cl.bytesIn.Add(int64(n))
		}
	}
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.stats.bytesOut.Add(int64(n))
		if c.cl != nil {
			c.cl.bytesOut.Add(int64(n))
		}
	}
	return n, err
}

// trackedConn removes its client from the set when closed (once).
type trackedConn struct {
	countingConn
	set  *clientSet
	once sync.Once
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.set.remove(c.cl) })
	return err
}

// track wraps conn so it is counted and listed until closed. It returns nil (after closing conn) when the server is
// stopping or a connection limit is reached.
func (s *clientSet) track(conn net.Conn) (*trackedConn, *client) {
	tc := &trackedConn{countingConn: countingConn{Conn: conn, stats: s.stats}, set: s}
	cl, err := s.add(conn.RemoteAddr().String(), func() { _ = conn.Close() })
	if err != nil {
		_ = conn.Close()
		return nil, nil
	}
	tc.cl = cl
	return tc, cl
}

// remoteIP returns the IP part of a host:port address.
func remoteIP(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// trackingListener tracks (and counts) every accepted connection in a clientSet.
type trackingListener struct {
	net.Listener
	set *clientSet
}

func (l *trackingListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if tc, _ := l.set.track(c); tc != nil {
			return tc, nil
		}
	}
}

// clientOf returns the tracked client behind conn (unwrapping TLS), or nil.
func clientOf(conn net.Conn) *client {
	for i := 0; i < 4 && conn != nil; i++ {
		switch c := conn.(type) {
		case *trackedConn:
			return c.cl
		case interface{ NetConn() net.Conn }:
			conn = c.NetConn()
		default:
			return nil
		}
	}
	return nil
}

// byAddr returns the client connected from addr (host:port), or nil.
func (s *clientSet) byAddr(addr string) *client {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.m {
		if c.addr == addr {
			return c
		}
	}
	return nil
}
