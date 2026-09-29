// Package monitor implements NexTerm's host monitoring (RESEARCH MON-1..6, SSH-38 display, SEC-22; SPEC §6.0
// "Monitoring"):
//
//   - the MobaXterm-style remote monitoring bar: an events topic "monitor" ({type:'subscribe', topic:'monitor',
//     sessionId}) backed by one shared collector per SSH transport — a single long-lived, non-PTY exec channel running a
//     per-OS sampler script (Linux /proc loop of RESEARCH §3.21; macOS, BSD and Windows PowerShell variants) — that
//     pushes {type:'monitor', sessionId, stats} every 2 s; collectors are ref-counted by subscribers and stop 10 s
//     after the last one leaves; local shell sessions are sampled in-process with gopsutil;
//   - REST: snapshot, process list / kill / renice (optional sudo), systemd / Windows services, listening ports, disk
//     usage drill-down, SSH connection details with a live latency probe, the local System info view;
//   - a WebSocket log follower (tail -F / journalctl -f, MON-5);
//   - Caffeine: keep the NexTerm host awake (desktop mode or administrators).
package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/nexterm/nexterm/internal/app"
	"github.com/nexterm/nexterm/internal/core"
	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/sshx"
	"github.com/nexterm/nexterm/internal/term"
)

// Topic is the events topic of the monitoring feed.
const Topic = "monitor"

// Defaults.
const (
	DefaultInterval = 2 * time.Second
	DefaultLinger   = 10 * time.Second
	maxExecPerHost  = 3 // concurrent ad-hoc commands per transport (on top of the sampler channel)
)

// Service is the monitoring module.
type Service struct {
	d   *app.Deps
	c   *core.Core
	log *slog.Logger
	ctx context.Context

	// Interval is the sampling period; Linger how long a collector outlives its last subscriber.
	Interval time.Duration
	Linger   time.Duration

	mu         sync.Mutex
	feeds      map[string]*feed      // by session id
	collectors map[any]*collector    // by source: *sshx.Client, localKey{}
	clients    map[any]*clientState  // per transport: probe result + exec gate
	procHist   map[string]*procState // previous process CPU counters, by target id
	sessLogs   sync.Map              // session id → *sessionLog (connect history, terminal traffic of SSH sessions)

	sf       singleflight.Group
	local    localState
	caffeine *caffeine
}

// localKey identifies the NexTerm host in the per-source maps.
type localKey struct{}

// clientState caches what the monitor learned about one transport.
type clientState struct {
	host *hostInfo
	gate chan struct{}
}

// New creates the service without registering anything (tests); Mount wires it into the application.
func New(d *app.Deps, c *core.Core) *Service {
	ctx := context.Background()
	log := slog.Default()
	if d != nil && d.Ctx != nil {
		ctx = d.Ctx
	}
	if d != nil && d.Log != nil {
		log = d.Log
	}
	s := &Service{
		d:          d,
		c:          c,
		log:        log.With("module", "monitor"),
		ctx:        ctx,
		Interval:   DefaultInterval,
		Linger:     DefaultLinger,
		feeds:      map[string]*feed{},
		collectors: map[any]*collector{},
		clients:    map[any]*clientState{},
		procHist:   map[string]*procState{},
	}
	s.caffeine = newCaffeine(ctx, s.log)
	return s
}

// Mount registers the monitoring topic, REST endpoints and WebSocket, session hooks and the caffeine feature probe.
func Mount(d *app.Deps, c *core.Core) error {
	if d == nil || c == nil || c.Sessions == nil || c.SSH == nil {
		return errors.New("monitor: missing dependencies")
	}
	s := New(d, c)
	s.caffeine.onChange = s.broadcastCaffeine
	d.Events.RegisterTopic(Topic, s.subscribe)
	c.Sessions.AddHooks(term.Hooks{OnState: s.onSessionState, OnClose: s.onSessionClose, OnOutput: s.onOutput, OnInput: s.onInput})
	app.RegisterFeature("caffeine", func(context.Context) bool { return s.caffeine.supported() })
	s.routes()
	return nil
}

func (s *Service) routes() {
	h := &handlers{s: s}
	api := s.d.Router.API()
	api.GET("/monitor/local", h.systemInfo)
	api.GET("/monitor/:id/host", h.host)
	api.GET("/monitor/:id/snapshot", h.snapshot)
	api.GET("/monitor/:id/processes", h.processes)
	api.POST("/monitor/:id/kill", h.kill)
	api.POST("/monitor/:id/renice", h.renice)
	api.GET("/monitor/:id/services", h.services)
	api.GET("/monitor/:id/services/:name/logs", h.serviceLogs)
	api.POST("/monitor/:id/services/:name/:action", h.serviceAction)
	api.GET("/monitor/:id/ports", h.ports)
	api.GET("/monitor/:id/du", h.diskUsage)
	api.GET("/monitor/:id/ssh-info", h.sshInfo)
	api.GET("/system/caffeine", h.caffeineStatus)
	api.POST("/system/caffeine", h.caffeineSet)
	s.d.Router.WS("/ws/monitor/:id/tail", h.tail)
}

// allowLocal reports whether user may monitor and act on the NexTerm host itself (principle 7: desktop mode or admins).
func (s *Service) allowLocal(user *model.User) bool {
	if user == nil {
		return false
	}
	return user.IsAdmin() || s.d == nil || s.d.Cfg == nil || s.d.Cfg.IsDesktop()
}

var errLocalForbidden = httpx.Forbidden("the local host monitor is available in desktop mode or to administrators")

// ---- targets --------------------------------------------------------------------------------------------------------

// target is a monitored host resolved for one request: a live SSH session's transport or the NexTerm host.
type target struct {
	id      string // session id, or "local"
	local   bool
	sess    *term.Session
	user    *model.User
	r       runner
	host    *hostInfo
	client  *sshx.Client
	key     any
	release func()
}

// label is "user@host" for prompts and audit details.
func (t *target) label() string {
	if t.local || t.sess == nil {
		return "the NexTerm host"
	}
	info := t.sess.Info()
	switch {
	case info.Username != "" && info.Host != "":
		return info.Username + "@" + info.Host
	case info.Host != "":
		return info.Host
	}
	return info.Title
}

// resolveTarget resolves a target id from a URL: "local" (the NexTerm host) or a runtime session the user owns.
func (s *Service) resolveTarget(ctx context.Context, user *model.User, id string) (*target, error) {
	if user == nil {
		return nil, httpx.ErrUnauthorized
	}
	if id == "local" {
		if !s.allowLocal(user) {
			return nil, errLocalForbidden
		}
		return s.localTarget(user, nil), nil
	}
	if s.c == nil || s.c.Sessions == nil {
		return nil, httpx.ErrNotFound
	}
	sess := s.c.Sessions.Get(id)
	if sess == nil || sess.OwnerID != user.ID {
		return nil, httpx.ErrNotFound
	}
	switch sess.Protocol {
	case model.ProtoLocal:
		if !s.allowLocal(user) {
			return nil, errLocalForbidden
		}
		return s.localTarget(user, sess), nil
	case model.ProtoSSH:
		client, release, err := s.c.SSH.ForSession(ctx, user, id)
		if err != nil {
			return nil, err
		}
		r := newSSHRunner(client)
		host, err := s.hostFor(ctx, client, r)
		if err != nil {
			release()
			var ue *unavailableError
			if errors.As(err, &ue) {
				return nil, httpx.NewError(http.StatusUnprocessableEntity, "monitor_unavailable", ue.msg)
			}
			return nil, err
		}
		return &target{id: id, sess: sess, user: user, r: r, host: host, client: client, key: client, release: release}, nil
	}
	return nil, httpx.BadRequest(fmt.Sprintf("monitoring is not available for %s sessions (SSH and local shells only)", sess.Protocol))
}

func (s *Service) localTarget(user *model.User, sess *term.Session) *target {
	id := "local"
	if sess != nil {
		id = sess.ID
	}
	return &target{id: id, local: true, sess: sess, user: user, r: localRunner{}, host: s.localHost(), key: localKey{}, release: func() {}}
}

// hostFor returns the (cached) probe result of a transport, probing once when unknown.
func (s *Service) hostFor(ctx context.Context, client *sshx.Client, r runner) (*hostInfo, error) {
	s.mu.Lock()
	if cs := s.clients[client]; cs != nil && cs.host != nil {
		h := cs.host
		s.mu.Unlock()
		return h, nil
	}
	s.mu.Unlock()
	v, err, _ := s.sf.Do(fmt.Sprintf("probe:%p", client), func() (any, error) {
		pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		h, err := probeHost(pctx, r)
		if err != nil {
			return nil, err
		}
		cs := s.clientState(client)
		s.mu.Lock()
		cs.host = h
		s.mu.Unlock()
		return h, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*hostInfo), nil
}

// clientState returns (creating) the state of a transport; entries of SSH clients are dropped when they close.
func (s *Service) clientState(key any) *clientState {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.clients[key]
	if cs == nil {
		cs = &clientState{gate: make(chan struct{}, maxExecPerHost)}
		s.clients[key] = cs
		if c, ok := key.(*sshx.Client); ok {
			go func() {
				select {
				case <-c.Done():
				case <-s.ctx.Done():
				}
				s.mu.Lock()
				if s.clients[key] == cs {
					delete(s.clients, key)
				}
				s.mu.Unlock()
			}()
		}
	}
	return cs
}

// acquire limits concurrent ad-hoc commands per host (each one is an SSH channel; MaxSessions defaults to 10).
func (t *target) acquire(ctx context.Context, s *Service) (func(), error) {
	gate := s.clientState(t.key).gate
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// run executes a command on the target within the per-host concurrency limit.
func (s *Service) run(ctx context.Context, t *target, c command) (*result, error) {
	rel, err := t.acquire(ctx, s)
	if err != nil {
		return nil, err
	}
	defer rel()
	return t.r.run(ctx, c)
}

// ---- session hooks --------------------------------------------------------------------------------------------------

func (s *Service) onSessionState(sess *term.Session, st model.SessionState) {
	s.trackState(sess, st)
	if st != model.StateConnected {
		return
	}
	s.mu.Lock()
	f := s.feeds[sess.ID]
	s.mu.Unlock()
	if f != nil {
		f.poke()
	}
}

func (s *Service) onSessionClose(sess *term.Session) {
	s.mu.Lock()
	f := s.feeds[sess.ID]
	delete(s.procHist, sess.ID)
	s.mu.Unlock()
	s.sessLogs.Delete(sess.ID)
	if f != nil {
		f.sessionClosed()
	}
}

// ---- helpers --------------------------------------------------------------------------------------------------------

func (s *Service) audit(ctx context.Context, action, target string, details any) {
	if s.d == nil || s.d.Audit == nil {
		return
	}
	s.d.Audit.Log(context.WithoutCancel(ctx), action, target, details)
}

// permissionDenied maps typical "not permitted" messages to a 403 the UI answers with "retry with sudo".
func permissionDenied(msg string) bool {
	m := strings.ToLower(msg)
	for _, p := range []string{"operation not permitted", "permission denied", "access denied", "access is denied",
		"interactive authentication required", "not authorized", "must be root", "requires root", "authentication is required",
		"insufficient permissions", "you do not have permission", "not permitted"} {
		if strings.Contains(m, p) {
			return true
		}
	}
	return false
}

func errPermission(msg string) error {
	return httpx.NewError(http.StatusForbidden, "permission_denied", msg)
}
