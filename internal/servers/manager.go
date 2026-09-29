package servers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nexterm/nexterm/internal/app"
	cfgpkg "github.com/nexterm/nexterm/internal/config"
	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
)

// Server states.
const (
	stateStopped  = "stopped"
	stateStarting = "starting"
	stateRunning  = "running"
	stateStopping = "stopping"
	stateError    = "error"
)

const (
	statusThrottle = 250 * time.Millisecond
	statsInterval  = 2 * time.Second
	stopTimeout    = 5 * time.Second
)

// Status is the state of one embedded server (superset of SPEC ServerStatus).
type Status struct {
	Kind      Kind           `json:"kind"`
	Name      string         `json:"name"`
	Running   bool           `json:"running"`
	State     string         `json:"state"`
	Config    map[string]any `json:"config"`
	Error     string         `json:"error,omitempty"`
	ErrorCode string         `json:"errorCode,omitempty"`
	Addr      string         `json:"addr,omitempty"`
	Addrs     []string       `json:"addrs,omitempty"`
	URL       string         `json:"url,omitempty"`
	Clients   int            `json:"clients"`
	StartedAt *time.Time     `json:"startedAt,omitempty"`
	StopAt    *time.Time     `json:"stopAt,omitempty"`
	Stats     Stats          `json:"stats"`
	Warnings  []string       `json:"warnings"`
	// Fingerprint identifies the server to clients: SSH host key (SHA256:…) or TLS certificate (SHA-256).
	Fingerprint string `json:"fingerprint,omitempty"`
}

// service is one running server implementation.
type service interface {
	// start binds every listener synchronously (bind errors are returned) and serves in background goroutines.
	start() error
	// stop closes the listeners and every client connection and waits (bounded) for the goroutines to end.
	stop()
}

// clientLister lets a service report clients itself (syslog: recent senders).
type clientLister interface {
	clientCount() int
	clientList() []ClientInfo
}

// Manager owns the embedded servers of this NexTerm instance.
type Manager struct {
	d       *app.Deps
	log     *slog.Logger
	env     *env
	secrets *secretStore
	subs    *subscribers
	syslog  *syslogStore
	slots   map[Kind]*slot
	osUser  string
	closing atomic.Bool
	done    chan struct{}
}

func newManager(d *app.Deps) *Manager {
	m := &Manager{d: d, log: d.Log.With("module", "servers"), env: buildEnv(d.Cfg), slots: map[Kind]*slot{},
		done: make(chan struct{})}
	m.secrets = &secretStore{d: d}
	m.subs = newSubscribers(m)
	m.syslog = newSyslogStore(m)
	m.osUser = currentOSUser()
	for _, k := range Kinds {
		s := &slot{m: m, kind: k, state: stateStopped}
		k := k
		s.logs = newLogRing(func(entries []LogEntry, dropped int) { m.subs.publishLog(k, entries, dropped) })
		s.cfg = defaultConfig(k, m.env)
		m.slots[k] = s
	}
	return m
}

func buildEnv(cfg *cfgpkg.Config) *env {
	e := &env{}
	e.home, _ = os.UserHomeDir()
	if cfg != nil {
		e.dataDir = cfg.DataDir
		if h, p, err := net.SplitHostPort(cfg.Listen); err == nil {
			if n, err := strconv.Atoi(p); err == nil {
				e.nexPort = n
			}
			if hh, err := normalizeBind(h); err == nil && h != "" {
				e.nexHost = hh
			} else {
				e.nexHost = "0.0.0.0"
			}
		}
	}
	base := e.home
	if base == "" {
		base, _ = os.Getwd()
	}
	if base != "" {
		e.defaultRoot = filepath.Join(base, "NexTermShare")
	}
	return e
}

func currentOSUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if n := os.Getenv("USER"); n != "" {
		return n
	}
	return os.Getenv("USERNAME")
}

// allowed reports whether u may manage the servers: anyone signed in on a desktop, administrators in server mode.
func (m *Manager) allowed(u *model.User) error {
	if u == nil {
		return httpx.ErrUnauthorized
	}
	if u.Disabled {
		return httpx.ErrForbidden
	}
	if m.d.Cfg != nil && m.d.Cfg.IsServer() && !u.IsAdmin() {
		return httpx.Forbidden("embedded servers are only available to administrators in server mode")
	}
	return nil
}

func settingsKey(k Kind) string { return "servers." + string(k) }

// loadConfigs reads the stored configurations (defaults for missing or unreadable ones).
func (m *Manager) loadConfigs(ctx context.Context) {
	for _, k := range Kinds {
		s := m.slots[k]
		cfg := defaultConfig(k, m.env)
		raw, err := m.d.Store.Settings.Get(ctx, settingsScope, settingsKey(k))
		switch {
		case err == nil:
			if err := json.Unmarshal(raw, cfg); err != nil {
				m.log.Warn("servers: ignoring an unreadable stored configuration", "kind", k, "err", err)
				cfg = defaultConfig(k, m.env)
			} else if err := cfg.validate(m.env); err != nil {
				// Keep it: the problem is reported when the server starts (and shown in the editor).
				m.log.Warn("servers: stored configuration is invalid", "kind", k, "err", err)
			}
		case !errors.Is(err, model.ErrNotFound):
			m.log.Warn("servers: cannot read the stored configuration", "kind", k, "err", err)
		}
		s.mu.Lock()
		s.cfg = cfg
		s.mu.Unlock()
	}
}

// autostart starts every server whose configuration asks for it.
func (m *Manager) autostart() {
	for _, k := range Kinds {
		s := m.slots[k]
		s.mu.Lock()
		auto := s.cfg.base().AutoStart
		s.mu.Unlock()
		if !auto || m.closing.Load() {
			continue
		}
		if _, err := s.start(m.d.Ctx); err != nil {
			m.log.Warn("servers: autostart failed", "kind", k, "err", err)
		} else {
			m.log.Info("servers: started automatically", "kind", k)
		}
	}
}

// run publishes changed counters of running servers and stops everything on shutdown.
func (m *Manager) run() {
	defer close(m.done)
	t := time.NewTicker(statsInterval)
	defer t.Stop()
	last := map[Kind]Stats{}
	for {
		select {
		case <-m.d.Ctx.Done():
			m.shutdown()
			return
		case <-t.C:
			for _, k := range Kinds {
				s := m.slots[k]
				s.mu.Lock()
				in := s.inst
				var st Stats
				if in != nil {
					st = in.stats.snapshot()
				}
				s.mu.Unlock()
				if in == nil {
					delete(last, k)
					continue
				}
				if prev, ok := last[k]; !ok || prev != st {
					last[k] = st
					if ok {
						s.changed()
					}
				}
			}
		}
	}
}

// shutdown stops every server (NexTerm is exiting; nothing is written to the store).
func (m *Manager) shutdown() {
	m.closing.Store(true)
	var wg sync.WaitGroup
	for _, s := range m.slots {
		wg.Add(1)
		go func(s *slot) {
			defer wg.Done()
			s.op.Lock()
			s.stopLocked(" (NexTerm is shutting down)")
			s.op.Unlock()
		}(s)
	}
	wg.Wait()
	m.syslog.closeFile()
}

// Wait blocks until the manager stopped every server after shutdown (or ctx ends).
func (m *Manager) Wait(ctx context.Context) error {
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// statuses returns the status of every server.
func (m *Manager) statuses() []Status {
	out := make([]Status, 0, len(Kinds))
	for _, k := range Kinds {
		out = append(out, m.slots[k].status())
	}
	return out
}

// ---- slot ---------------------------------------------------------------------------------------------------------

// slot is one server kind: its saved configuration, its log and its current run (instance).
type slot struct {
	m    *Manager
	kind Kind
	op   sync.Mutex // serializes start / stop / restart / configure
	logs *logRing

	mu         sync.Mutex
	cfg        config
	state      string
	err        string
	errCode    string
	inst       *instance
	lastStats  *Stats
	stopTimer  *time.Timer
	stopAt     time.Time
	pubPending bool
	lastPub    time.Time
}

func (s *slot) config() config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneConfig(s.kind, s.cfg, s.m.env)
}

func (s *slot) status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

func (s *slot) statusLocked() Status {
	st := Status{Kind: s.kind, Name: kindNames[s.kind], State: s.state, Error: s.err, ErrorCode: s.errCode,
		Config: publicConfig(s.cfg), Warnings: s.m.warnings(s.cfg)}
	if in := s.inst; in != nil {
		st.Running = s.state == stateRunning
		if len(in.addrs) > 0 {
			st.Addr = in.addrs[0]
			st.Addrs = append([]string(nil), in.addrs...)
		}
		st.URL = in.url
		if cl, ok := in.svc.(clientLister); ok {
			st.Clients = cl.clientCount()
		} else {
			st.Clients = in.clients.count()
		}
		started := in.started
		st.StartedAt = &started
		st.Stats = in.stats.snapshot()
		st.Fingerprint = in.fp
		if !s.stopAt.IsZero() {
			t := s.stopAt
			st.StopAt = &t
		}
	} else if s.lastStats != nil {
		st.Stats = *s.lastStats
	}
	return st
}

// changed schedules a (throttled) status event.
func (s *slot) changed() {
	s.mu.Lock()
	if s.pubPending {
		s.mu.Unlock()
		return
	}
	s.pubPending = true
	delay := statusThrottle - time.Since(s.lastPub)
	s.mu.Unlock()
	if delay < 0 {
		delay = 0
	}
	time.AfterFunc(delay, func() {
		s.mu.Lock()
		s.pubPending = false
		s.lastPub = time.Now()
		st := s.statusLocked()
		s.mu.Unlock()
		s.m.subs.publishStatus(st)
	})
}

// publishNow sends a status event immediately (state transitions).
func (s *slot) publishNow() {
	s.mu.Lock()
	s.lastPub = time.Now()
	st := s.statusLocked()
	s.mu.Unlock()
	s.m.subs.publishStatus(st)
}

// startError is a start failure with an API error code.
type startError struct {
	status int
	code   string
	msg    string
}

func (e *startError) Error() string { return e.msg }

// httpError renders the failure for the REST API.
func (e *startError) httpError() error { return httpx.NewError(e.status, e.code, e.msg) }

var errShuttingDown = &startError{status: http.StatusServiceUnavailable, code: "shutting_down", msg: "NexTerm is shutting down"}

// start starts the server with its saved configuration (no-op when running).
func (s *slot) start(ctx context.Context) (Status, error) {
	s.op.Lock()
	defer s.op.Unlock()
	err := s.startLocked(ctx)
	return s.status(), err
}

func (s *slot) startLocked(ctx context.Context) error {
	if s.m.closing.Load() {
		return errShuttingDown
	}
	s.mu.Lock()
	if s.inst != nil {
		s.mu.Unlock()
		return nil
	}
	cfg := cloneConfig(s.kind, s.cfg, s.m.env)
	s.state, s.err, s.errCode = stateStarting, "", ""
	s.mu.Unlock()
	s.publishNow()

	in, err := s.m.launch(ctx, s, cfg)
	if err != nil {
		se := classifyStartErr(err, cfg.base())
		s.mu.Lock()
		s.state, s.err, s.errCode = stateError, se.msg, se.code
		s.mu.Unlock()
		s.logs.add(levelError, "", "", "Start failed: "+se.msg)
		s.m.log.Info("servers: start failed", "kind", s.kind, "err", se.msg)
		s.publishNow()
		return se
	}
	s.mu.Lock()
	s.inst = in
	s.state = stateRunning
	s.lastStats = nil
	if n := cfg.base().StopAfterSec; n > 0 {
		d := time.Duration(n) * time.Second
		s.stopAt = time.Now().UTC().Add(d)
		s.stopTimer = time.AfterFunc(d, func() { s.autoStop(in, d) })
	}
	s.mu.Unlock()
	msg := "Server started on " + joinAddrs(in.addrs)
	if in.fp != "" {
		msg += " (fingerprint " + in.fp + ")"
	}
	s.logs.add(levelInfo, "", "", msg)
	s.m.log.Info("servers: started", "kind", s.kind, "addr", joinAddrs(in.addrs))
	s.publishNow()
	return nil
}

// stop stops the server (no-op when stopped; an error state is cleared).
func (s *slot) stop(reason string) Status {
	s.op.Lock()
	s.stopLocked(reason)
	s.op.Unlock()
	return s.status()
}

func (s *slot) stopLocked(reason string) {
	s.mu.Lock()
	in := s.inst
	if in == nil {
		cleared := s.state == stateError
		if cleared {
			s.state, s.err, s.errCode = stateStopped, "", ""
		}
		s.mu.Unlock()
		if cleared {
			s.publishNow()
		}
		return
	}
	s.state = stateStopping
	if s.stopTimer != nil {
		s.stopTimer.Stop()
		s.stopTimer = nil
	}
	s.stopAt = time.Time{}
	s.mu.Unlock()
	s.publishNow()

	in.cancel()
	done := make(chan struct{})
	go func() {
		in.svc.stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stopTimeout):
		s.m.log.Warn("servers: stopping took too long; continuing", "kind", s.kind)
	}
	in.closeRoot()

	s.mu.Lock()
	st := in.stats.snapshot()
	s.lastStats = &st
	s.inst = nil
	s.state, s.err, s.errCode = stateStopped, "", ""
	s.mu.Unlock()
	s.logs.add(levelInfo, "", "", "Server stopped"+reason)
	s.m.log.Info("servers: stopped", "kind", s.kind)
	s.publishNow()
}

// restart stops and starts the server with its saved configuration.
func (s *slot) restart(ctx context.Context) (Status, error) {
	s.op.Lock()
	defer s.op.Unlock()
	s.stopLocked(" (restart)")
	err := s.startLocked(ctx)
	return s.status(), err
}

func (s *slot) autoStop(in *instance, d time.Duration) {
	s.op.Lock()
	defer s.op.Unlock()
	s.mu.Lock()
	same := s.inst == in
	s.mu.Unlock()
	if same {
		s.stopLocked(" (stopped automatically after " + d.String() + ")")
	}
}

// fail handles a server that stopped serving on its own (listener error).
func (s *slot) fail(in *instance, cause error) {
	s.op.Lock()
	defer s.op.Unlock()
	s.mu.Lock()
	same := s.inst == in
	s.mu.Unlock()
	if !same || s.m.closing.Load() {
		return
	}
	s.stopLocked(" (error)")
	msg := cause.Error()
	s.mu.Lock()
	s.state, s.err, s.errCode = stateError, msg, "failed"
	s.mu.Unlock()
	s.logs.add(levelError, "", "", "Server failed: "+msg)
	s.m.log.Warn("servers: server failed", "kind", s.kind, "err", msg)
	s.publishNow()
}

// configure applies a configuration update (JSON object; omitted keys keep their value, "users" replaces the list with
// write-only passwords), stores it and restarts the server when it is running. restarted reports a restart; a failed
// restart leaves the server in the error state (the configuration is saved anyway).
func (s *slot) configure(ctx context.Context, body []byte) (st Status, changed []string, err error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil || keys == nil {
		return s.status(), nil, invalidf("the configuration must be a JSON object")
	}
	s.op.Lock()
	defer s.op.Unlock()

	cur := s.config()
	next := cloneConfig(s.kind, cur, s.m.env)
	if _, ok := keys["users"]; ok && next.users() != nil {
		*next.users() = nil // decode the new list into fresh elements (json reuses slice elements otherwise)
	}
	if err := json.Unmarshal(body, next); err != nil {
		return s.status(), nil, invalidf("invalid configuration: %v", jsonErrorText(err))
	}
	if ul := next.users(); ul != nil {
		if err := applyUserInput(*ul, *cur.users()); err != nil {
			return s.status(), nil, err
		}
	}
	if err := next.validate(s.m.env); err != nil {
		return s.status(), nil, err
	}
	changed = diffKeys(storedMap(cur), storedMap(next)) // includes password hashes: a new password restarts the server
	raw, err := json.Marshal(next)
	if err != nil {
		return s.status(), nil, err
	}
	if err := s.m.d.Store.Settings.Set(ctx, settingsScope, settingsKey(s.kind), raw); err != nil {
		return s.status(), nil, err
	}
	s.mu.Lock()
	s.cfg = next
	running := s.inst != nil
	if !running && s.state == stateError {
		// The failure was about the previous configuration.
		s.state, s.err, s.errCode = stateStopped, "", ""
	}
	s.mu.Unlock()
	s.logs.add(levelInfo, "", "", "Configuration saved")
	if running && len(changed) > 0 && !onlyKeys(changed, "autoStart") {
		s.stopLocked(" (configuration changed)")
		_ = s.startLocked(ctx) // failures are reported in the status
	} else {
		s.publishNow()
	}
	return s.status(), changed, nil
}

// storedMap renders c as stored (with password hashes) for change detection; only key names leave this package.
func storedMap(c config) map[string]any {
	b, err := json.Marshal(c)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// diffKeys lists the top-level keys whose values differ.
func diffKeys(a, b map[string]any) []string {
	var out []string
	seen := map[string]bool{}
	for k, v := range a {
		seen[k] = true
		if !jsonEqual(v, b[k]) {
			out = append(out, k)
		}
	}
	for k := range b {
		if !seen[k] {
			out = append(out, k)
		}
	}
	return out
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func onlyKeys(list []string, allowed ...string) bool {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	for _, k := range list {
		if !ok[k] {
			return false
		}
	}
	return true
}

func jsonErrorText(err error) string {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		if te.Field != "" {
			return fmt.Sprintf("%s must be a %s", te.Field, te.Type)
		}
		return fmt.Sprintf("expected %s", te.Type)
	}
	return "malformed JSON"
}

func joinAddrs(addrs []string) string {
	switch len(addrs) {
	case 0:
		return "?"
	case 1:
		return addrs[0]
	}
	out := addrs[0]
	for _, a := range addrs[1:] {
		out += ", " + a
	}
	return out
}

// classifyStartErr maps a start failure to an API error.
func classifyStartErr(err error, c *Common) *startError {
	var se *startError
	switch {
	case errors.As(err, &se):
		return se
	case isInvalid(err):
		return &startError{status: http.StatusBadRequest, code: "invalid_config", msg: err.Error()}
	case isAddrInUse(err):
		return &startError{status: http.StatusConflict, code: "port_in_use",
			msg: fmt.Sprintf("port %d on %s is already in use by another program", c.Port, c.BindAddress)}
	case isAccessDenied(err):
		msg := fmt.Sprintf("permission denied binding port %d on %s", c.Port, c.BindAddress)
		if c.Port < 1024 {
			msg += " (" + privilegeHint(c.Port) + ")"
		}
		return &startError{status: http.StatusConflict, code: "port_privileged", msg: msg}
	case isAddrNotAvail(err):
		return &startError{status: http.StatusConflict, code: "address_unavailable",
			msg: fmt.Sprintf("%s is not an address of this machine", c.BindAddress)}
	}
	return &startError{status: http.StatusConflict, code: "start_failed", msg: rootCause(err).Error()}
}

// ---- instance -----------------------------------------------------------------------------------------------------

// instance is one run of a server.
type instance struct {
	slot    *slot
	kind    Kind
	ctx     context.Context
	cancel  context.CancelFunc
	started time.Time
	addrs   []string
	url     string
	fp      string
	stats   counters
	clients *clientSet
	svc     service
	root    *rootFS
}

func (in *instance) logf(level, client, user, format string, args ...any) {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	in.slot.logs.add(level, client, user, msg)
}

// failed reports an unexpected serve error (asynchronously: the caller may be a goroutine stop() waits for).
func (in *instance) failed(err error) {
	if in.ctx.Err() != nil {
		return
	}
	go in.slot.fail(in, err)
}

func (in *instance) closeRoot() {
	if in.root != nil {
		_ = in.root.Close()
	}
}

// launch validates cfg, prepares and starts the service of kind.
func (m *Manager) launch(ctx context.Context, s *slot, cfg config) (*instance, error) {
	if err := cfg.validate(m.env); err != nil {
		return nil, err
	}
	ictx, cancel := context.WithCancel(m.d.Ctx)
	in := &instance{slot: s, kind: s.kind, ctx: ictx, cancel: cancel, started: time.Now().UTC()}
	in.clients = newClientSet(&in.stats, s.changed)
	in.clients.onLimit = func(addr, why string) { in.logf(levelWarn, addr, "", "Connection refused: %s", why) }
	if root := rootOf(cfg); root != "" {
		if err := checkRoot(root, m.env); err != nil {
			cancel()
			return nil, err
		}
		r, err := openRootFS(root, false)
		if err != nil {
			cancel()
			return nil, invalidf("cannot open %s: %v", root, rootCause(err))
		}
		in.root = r
	}
	var (
		svc service
		err error
	)
	switch c := cfg.(type) {
	case *HTTPConfig:
		svc, err = newHTTPService(ctx, m, in, c)
	case *FTPConfig:
		svc, err = newFTPService(ctx, m, in, c)
	case *SFTPConfig:
		svc, err = newSSHService(ctx, m, in, c)
	case *TFTPConfig:
		svc, err = newTFTPService(m, in, c)
	case *TelnetConfig:
		svc, err = newTelnetService(m, in, c)
	case *SyslogConfig:
		svc, err = newSyslogService(m, in, c)
	default:
		err = fmt.Errorf("unsupported server %s", s.kind)
	}
	if err == nil {
		in.svc = svc
		err = svc.start()
		if err != nil {
			svc.stop()
		}
	}
	if err != nil {
		cancel()
		in.closeRoot()
		return nil, err
	}
	return in, nil
}

// ---- warnings / host ----------------------------------------------------------------------------------------------

// privilegedPorts reports whether ports below 1024 need elevated rights on this host (Windows has none).
func privilegedPorts() bool {
	return runtime.GOOS != "windows" && os.Geteuid() != 0
}

// wildcardPrivilegedOK: macOS (10.14+) lets anyone bind ports below 1024, but only on the wildcard address.
func wildcardPrivilegedOK() bool { return runtime.GOOS == "darwin" }

// needsPrivilege reports whether binding port on bind needs elevated rights here.
func needsPrivilege(port int, bind string) bool {
	if port >= 1024 || !privilegedPorts() {
		return false
	}
	return !(wildcardPrivilegedOK() && isUnspecified(bind))
}

// privilegeHint explains how to use a port below 1024 on this system.
func privilegeHint(port int) string {
	if wildcardPrivilegedOK() {
		return fmt.Sprintf("on macOS port %d can only be used without administrator rights when listening on every interface (0.0.0.0 or ::); or choose a port ≥ 1024", port)
	}
	return fmt.Sprintf("port %d is below 1024: run NexTerm with administrator rights (or CAP_NET_BIND_SERVICE) or choose a port ≥ 1024", port)
}

// warnings lists security / usability caveats of a configuration (shown on the server card).
func (m *Manager) warnings(cfg config) []string {
	c := cfg.base()
	w := []string{}
	exposed := !isLoopback(c.BindAddress)
	switch {
	case isUnspecified(c.BindAddress):
		w = append(w, "Listening on every network interface: other machines on your network can connect.")
	case exposed:
		w = append(w, fmt.Sprintf("Listening on %s: other machines on that network can connect.", c.BindAddress))
	}
	if needsPrivilege(c.Port, c.BindAddress) {
		if wildcardPrivilegedOK() {
			w = append(w, fmt.Sprintf("Port %d is privileged: on macOS it only works when listening on every interface (or with administrator rights).", c.Port))
		} else {
			w = append(w, fmt.Sprintf("Port %d is privileged: binding it needs administrator (root) rights.", c.Port))
		}
	}
	switch t := cfg.(type) {
	case *HTTPConfig:
		if exposed && !t.RequireAuth {
			w = append(w, "No authentication: anyone who can reach the server can read the shared folder.")
		}
		if exposed && !t.ReadOnly && !t.RequireAuth {
			w = append(w, "Anyone who can reach the server can upload files.")
		}
		if exposed && t.RequireAuth && !t.TLS {
			w = append(w, "Passwords are sent unencrypted: enable HTTPS.")
		}
	case *FTPConfig:
		if exposed && t.TLS == "off" {
			w = append(w, "FTP without TLS sends passwords and files unencrypted.")
		}
		if t.Anonymous {
			if t.AnonymousWrite && !t.ReadOnly {
				w = append(w, "Anonymous users can upload, rename and delete files.")
			} else {
				w = append(w, "Anonymous users can download files.")
			}
		}
		if len(t.Users) == 0 && !t.Anonymous {
			w = append(w, "No users: add a user or allow anonymous access before starting.")
		}
	case *SFTPConfig:
		if t.Shell {
			w = append(w, fmt.Sprintf("Shell access runs commands as %s on this machine, outside the root folder.", m.osUserName()))
		}
		if len(t.Users) == 0 {
			w = append(w, "No users: add a user before starting.")
		}
	case *TFTPConfig:
		if !t.ReadOnly {
			w = append(w, "TFTP has no authentication: anyone who can reach it can upload files.")
		} else if exposed {
			w = append(w, "TFTP has no authentication: anyone who can reach it can download files.")
		}
	case *TelnetConfig:
		if exposed {
			w = append(w, "Telnet is unencrypted: passwords and sessions can be intercepted.")
		}
		w = append(w, fmt.Sprintf("Sessions run a shell as %s on this machine.", m.osUserName()))
		if len(t.Users) == 0 {
			w = append(w, "No users: add a user before starting.")
		}
	case *SyslogConfig:
		if isLoopback(c.BindAddress) {
			w = append(w, "Bound to loopback: only this machine can send messages (listen on every interface for network devices).")
		}
	}
	return w
}

func (m *Manager) osUserName() string {
	if m.osUser == "" {
		return "the NexTerm user"
	}
	return m.osUser
}

// HostInfo describes the machine the servers run on (GET /api/servers/host).
type HostInfo struct {
	Hostname        string `json:"hostname"`
	Platform        string `json:"platform"`
	OSUser          string `json:"osUser"`
	Home            string `json:"home"`
	DefaultRoot     string `json:"defaultRoot"`
	PrivilegedPorts bool   `json:"privilegedPorts"`
	// PrivilegedWildcardOK: ports below 1024 work without privileges on the wildcard address (macOS).
	PrivilegedWildcardOK bool            `json:"privilegedWildcardOk"`
	NexTermPort          int             `json:"nexTermPort"`
	Interfaces           []HostInterface `json:"interfaces"`
}

// HostInterface is one network interface with its addresses.
type HostInterface struct {
	Name      string   `json:"name"`
	Addresses []string `json:"addresses"`
	Up        bool     `json:"up"`
	Loopback  bool     `json:"loopback"`
}

func (m *Manager) hostInfo() HostInfo {
	h, _ := os.Hostname()
	hi := HostInfo{Hostname: h, Platform: runtime.GOOS, OSUser: m.osUser, Home: m.env.home, DefaultRoot: m.env.defaultRoot,
		PrivilegedPorts: privilegedPorts(), PrivilegedWildcardOK: wildcardPrivilegedOK(), NexTermPort: m.env.nexPort,
		Interfaces: []HostInterface{}}
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		addrs, _ := ifc.Addrs()
		hi2 := HostInterface{Name: ifc.Name, Up: ifc.Flags&net.FlagUp != 0, Loopback: ifc.Flags&net.FlagLoopback != 0,
			Addresses: []string{}}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				hi2.Addresses = append(hi2.Addresses, ipn.IP.String())
			}
		}
		if len(hi2.Addresses) > 0 {
			hi.Interfaces = append(hi.Interfaces, hi2)
		}
	}
	return hi
}

// displayHost is the host shown in URLs: a LAN address for wildcard binds.
func displayHost(bind string) string {
	if !isUnspecified(bind) {
		return bind
	}
	if ip := primaryIPv4(); ip != "" {
		return ip
	}
	return "127.0.0.1"
}

func primaryIPv4() string {
	ifs, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
				return ipn.IP.String()
			}
		}
	}
	return ""
}

// serverURL builds scheme://host:port for a bound address.
func serverURL(scheme, bind string, port int, suffix string) string {
	return scheme + "://" + hostPort(displayHost(bind), port) + suffix
}
