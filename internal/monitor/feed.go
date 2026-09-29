package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/sshx"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// Feed states sent with errors ({type:'monitor', sessionId, error, state}).
const (
	stateWaiting     = "waiting"     // the session is not connected (yet)
	stateUnavailable = "unavailable" // monitoring cannot work on this host
	stateError       = "error"       // transient failure, retrying
	stateClosed      = "closed"      // the session ended
)

// monitorEvent is the pushed event (SPEC §6.1; `state` is an addition).
type monitorEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	Stats     *Stats `json:"stats,omitempty"`
	Error     string `json:"error,omitempty"`
	State     string `json:"state,omitempty"`
}

// ---- subscriptions --------------------------------------------------------------------------------------------------

// subscribe is the events topic handler: params {sessionId}. It validates the session and attaches the socket to the
// session's feed; the feed goroutine waits for the connection and (re)attaches to the transport's collector.
func (s *Service) subscribe(_ context.Context, user *model.User, clientID string, params json.RawMessage) (func(), error) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
		return nil, errors.New("sessionId is required")
	}
	if user == nil {
		return nil, errors.New("session not found")
	}
	if p.SessionID == "local" { // the AstraTerm host itself (System information view)
		if !s.allowLocal(user) {
			return nil, errors.New("local host monitoring is available in desktop mode or to administrators")
		}
		f := s.attachSubscriber("local", user, true, clientID)
		return func() { s.detachSubscriber(f, clientID) }, nil
	}
	if s.c == nil || s.c.Sessions == nil {
		return nil, errors.New("session not found")
	}
	sess := s.c.Sessions.Get(p.SessionID)
	if sess == nil || sess.OwnerID != user.ID {
		return nil, errors.New("session not found")
	}
	local := false
	switch sess.Protocol {
	case model.ProtoSSH:
	case model.ProtoLocal:
		if !s.allowLocal(user) {
			return nil, errors.New("local host monitoring is available in desktop mode or to administrators")
		}
		local = true
	default:
		return nil, fmt.Errorf("monitoring is not available for %s sessions", sess.Protocol)
	}
	f := s.attachSubscriber(sess.ID, user, local, clientID)
	return func() { s.detachSubscriber(f, clientID) }, nil
}

func (s *Service) attachSubscriber(sessionID string, user *model.User, local bool, clientID string) *feed {
	s.mu.Lock()
	f := s.feeds[sessionID]
	created := false
	if f == nil {
		ctx, cancel := context.WithCancel(s.ctx)
		f = &feed{s: s, id: sessionID, owner: user, local: local, ctx: ctx, cancel: cancel,
			wake: make(chan struct{}, 1), subs: map[string]int{}}
		s.feeds[sessionID] = f
		created = true
	}
	f.mu.Lock()
	f.subs[clientID]++
	last := f.last
	f.mu.Unlock()
	s.mu.Unlock()
	if created {
		go f.run()
	}
	if last != nil && s.d != nil && s.d.Events != nil {
		s.d.Events.PublishClient(clientID, last)
	}
	return f
}

func (s *Service) detachSubscriber(f *feed, clientID string) {
	s.mu.Lock()
	f.mu.Lock()
	if n := f.subs[clientID]; n > 1 {
		f.subs[clientID] = n - 1
	} else {
		delete(f.subs, clientID)
	}
	empty := len(f.subs) == 0
	f.mu.Unlock()
	if empty && s.feeds[f.id] == f {
		delete(s.feeds, f.id)
	}
	s.mu.Unlock()
	if empty {
		f.cancel()
	}
}

// ---- feeds ----------------------------------------------------------------------------------------------------------

// feed fans the samples of one session's host out to the sockets subscribed to that session.
type feed struct {
	s      *Service
	id     string
	owner  *model.User
	local  bool
	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}

	mu   sync.Mutex
	subs map[string]int // client id → subscription count
	last []byte         // last published event, replayed to new subscribers
}

func (f *feed) poke() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *feed) publish(ev monitorEvent) {
	ev.Type, ev.SessionID = model.EvMonitor, f.id
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.last = b
	ids := make([]string, 0, len(f.subs))
	for id := range f.subs {
		ids = append(ids, id)
	}
	f.mu.Unlock()
	if f.s.d == nil || f.s.d.Events == nil {
		return
	}
	for _, id := range ids {
		f.s.d.Events.PublishClient(id, b)
	}
}

func (f *feed) publishStats(st *Stats) { f.publish(monitorEvent{Stats: st}) }

func (f *feed) publishError(state, msg string) { f.publish(monitorEvent{State: state, Error: msg}) }

func (f *feed) sessionClosed() {
	f.publishError(stateClosed, "The session has ended")
	f.cancel()
}

// run keeps the feed attached to the collector of the session's current transport, across reconnects.
func (f *feed) run() {
	if f.local {
		c := f.s.attachCollector(localKey{}, func() source { return &localSource{s: f.s} }, f)
		<-f.ctx.Done()
		c.removeFeed(f)
		return
	}
	for f.ctx.Err() == nil {
		sess := f.s.c.Sessions.Get(f.id)
		if sess == nil {
			f.publishError(stateClosed, "The session has ended")
			return
		}
		client, release, err := f.s.c.SSH.ForSession(f.ctx, f.owner, f.id)
		if err != nil {
			if f.ctx.Err() != nil {
				return
			}
			if errors.Is(err, httpx.ErrNotFound) {
				f.publishError(stateClosed, "The session has ended")
				return
			}
			f.publishError(stateWaiting, waitingMessage(sess))
			select {
			case <-f.ctx.Done():
				return
			case <-f.wake:
			case <-time.After(5 * time.Second):
			}
			continue
		}
		c := f.s.attachCollector(client, func() source { return &remoteSource{s: f.s, client: client} }, f)
		select {
		case <-f.ctx.Done():
		case <-client.Done():
		}
		c.removeFeed(f)
		release()
	}
}

func waitingMessage(sess *term.Session) string {
	st, _ := sess.State()
	switch st {
	case model.StateConnecting, model.StateAuthenticating:
		return "Waiting for the SSH connection"
	case model.StateDisconnected, model.StateError:
		return "The session is disconnected"
	case model.StateClosed:
		return "The session has ended"
	}
	return "Waiting for the SSH connection"
}

// ---- collectors -----------------------------------------------------------------------------------------------------

// source produces samples until ctx ends, reporting problems through fail (it retries by itself).
type source interface {
	run(ctx context.Context, emit func(*Stats), fail func(state, msg string))
}

// collector runs one source (one exec channel per SSH transport, or the local sampler) and fans its samples out to the
// attached feeds. It stops Linger after the last feed detached.
type collector struct {
	s      *Service
	key    any
	cancel context.CancelFunc

	mu      sync.Mutex
	feeds   map[*feed]struct{}
	linger  *time.Timer
	last    *Stats
	lastErr *monitorEvent
	done    bool
}

// attachCollector attaches f to the collector of key (starting one with mk when needed) and replays its last sample.
func (s *Service) attachCollector(key any, mk func() source, f *feed) *collector {
	s.mu.Lock()
	c := s.collectors[key]
	if c != nil {
		c.mu.Lock()
		if c.done {
			c.mu.Unlock()
			c = nil
		}
	}
	var runCtx context.Context
	if c == nil {
		ctx, cancel := context.WithCancel(s.ctx)
		c = &collector{s: s, key: key, cancel: cancel, feeds: map[*feed]struct{}{}}
		s.collectors[key] = c
		c.mu.Lock()
		runCtx = ctx
	}
	if c.linger != nil {
		c.linger.Stop()
		c.linger = nil
	}
	c.feeds[f] = struct{}{}
	last, lastErr := c.last, c.lastErr
	c.mu.Unlock()
	s.mu.Unlock()
	if runCtx != nil {
		go func() {
			defer s.collectorDone(c)
			mk().run(runCtx, c.emit, c.fail)
		}()
	}
	switch {
	case lastErr != nil:
		f.publishError(lastErr.State, lastErr.Error)
	case last != nil:
		f.publishStats(last)
	}
	return c
}

func (c *collector) removeFeed(f *feed) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.feeds, f)
	if len(c.feeds) > 0 || c.done {
		return
	}
	if c.linger != nil {
		c.linger.Stop()
	}
	c.linger = time.AfterFunc(max(c.s.Linger, 0), c.release)
}

// release stops the collector unless a feed attached meanwhile.
func (c *collector) release() {
	s := c.s
	s.mu.Lock()
	c.mu.Lock()
	if len(c.feeds) > 0 || c.done {
		c.mu.Unlock()
		s.mu.Unlock()
		return
	}
	c.done = true
	if s.collectors[c.key] == c {
		delete(s.collectors, c.key)
	}
	c.mu.Unlock()
	s.mu.Unlock()
	c.cancel()
}

// collectorDone runs when the source returned (context cancelled or transport gone).
func (s *Service) collectorDone(c *collector) {
	s.mu.Lock()
	if s.collectors[c.key] == c {
		delete(s.collectors, c.key)
	}
	c.mu.Lock()
	c.done = true
	if c.linger != nil {
		c.linger.Stop()
		c.linger = nil
	}
	c.mu.Unlock()
	s.mu.Unlock()
	c.cancel()
}

func (c *collector) snapshot() []*feed {
	out := make([]*feed, 0, len(c.feeds))
	for f := range c.feeds {
		out = append(out, f)
	}
	return out
}

func (c *collector) emit(st *Stats) {
	c.mu.Lock()
	c.last, c.lastErr = st, nil
	feeds := c.snapshot()
	c.mu.Unlock()
	for _, f := range feeds {
		f.publishStats(st)
	}
}

func (c *collector) fail(state, msg string) {
	c.mu.Lock()
	c.lastErr = &monitorEvent{State: state, Error: msg}
	feeds := c.snapshot()
	c.mu.Unlock()
	for _, f := range feeds {
		f.publishError(state, msg)
	}
}

// latest returns the collector's last sample for key when it is fresh.
func (s *Service) latest(key any) *Stats {
	s.mu.Lock()
	c := s.collectors[key]
	s.mu.Unlock()
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil || c.last.Warmup || time.Since(c.last.TS) > 3*max(s.Interval, time.Second) {
		return nil
	}
	return c.last
}

// ---- remote source --------------------------------------------------------------------------------------------------

// remoteSource streams the per-OS sampler over one exec channel of an SSH transport, restarting it with backoff.
type remoteSource struct {
	s      *Service
	client *sshx.Client
}

var retryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 60 * time.Second}

// stallTimeout is the minimum time without a sample after which a running sampler is considered hung (a variable so
// tests can shorten it).
var stallTimeout = 30 * time.Second

func (r *remoteSource) run(ctx context.Context, emit func(*Stats), fail func(state, msg string)) {
	run := newSSHRunner(r.client)
	attempt := 0
	for ctx.Err() == nil && r.client.Alive() {
		host, err := r.s.hostFor(ctx, r.client, run)
		var delay time.Duration
		switch {
		case err == nil:
			got := 0
			err = streamSamples(ctx, run, host, r.s.Interval, func(st *Stats) {
				got++
				attempt = 0
				emit(st)
			})
			if ctx.Err() != nil || !r.client.Alive() {
				return
			}
			fail(stateError, "Monitoring stopped: "+errString(err)+" (retrying)")
			if got > 0 {
				attempt = 0
			}
			delay = retryDelays[min(attempt, len(retryDelays)-1)]
			attempt++
		default:
			if ctx.Err() != nil || !r.client.Alive() {
				return
			}
			var ue *unavailableError
			if errors.As(err, &ue) {
				fail(stateUnavailable, "Remote monitoring is unavailable: "+ue.msg)
				delay = time.Minute
			} else {
				fail(stateError, "Remote monitoring failed: "+errString(err)+" (retrying)")
				delay = retryDelays[min(attempt, len(retryDelays)-1)]
				attempt++
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-r.client.Done():
			return
		case <-time.After(delay):
		}
	}
}

func errString(err error) string {
	if err == nil {
		return "the sampler exited"
	}
	return clip(strings.TrimSpace(err.Error()), 300)
}

// streamSamples runs the host's sampler and calls emit for every sample until the command ends. It returns why it
// ended.
func streamSamples(ctx context.Context, r runner, host *hostInfo, interval time.Duration, emit func(*Stats)) error {
	cmd, err := loopCommand(host.Platform, interval, 0)
	if err != nil {
		return &unavailableError{msg: err.Error()}
	}
	st, err := r.start(ctx, cmd)
	if err != nil {
		return err
	}
	defer st.close()
	// Watchdog: a sampler stuck in a hung command (df on a dying local disk, who on a locked utmp…) produces nothing
	// and would leave the bar stale forever; end it so the collector reports the problem and restarts it.
	stall := max(8*interval, stallTimeout)
	var stalled atomic.Bool
	watchdog := time.AfterFunc(stall, func() {
		stalled.Store(true)
		st.stop()
	})
	defer watchdog.Stop()
	var prev *rawSample
	n := consumeSamples(st.Stdout, host, func(cur *rawSample) {
		watchdog.Reset(stall)
		emit(computeStats(prev, cur, host))
		prev = cur
	})
	watchdog.Stop()
	st.stop()
	res, werr := st.wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if stalled.Load() {
		return fmt.Errorf("the sampler stopped responding (no sample for %s)", stall.Round(time.Second))
	}
	switch {
	case werr != nil:
		return werr
	case res != nil && res.Code != 0 && res.errText() != "":
		return fmt.Errorf("sampler exited with status %d: %s", res.Code, res.errText())
	case n == 0 && res != nil && res.errText() != "":
		return errors.New(res.errText())
	}
	return errors.New("the sampler exited")
}

// consumeSamples parses sampler output (@@S…@@E blocks, or one JSON object per line on Windows) and reports each raw
// sample. It returns the number of samples read.
func consumeSamples(rd interface{ Read([]byte) (int, error) }, host *hostInfo, onSample func(*rawSample)) int {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	n := 0
	var block []string
	in := false
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if host.Platform == platWindows {
			if t := strings.TrimSpace(line); strings.HasPrefix(t, "{") {
				if cur, ok := parseWindowsSample([]byte(t), time.Now()); ok {
					n++
					onSample(cur)
				}
			}
			continue
		}
		switch line {
		case "@@S":
			block, in = block[:0], true
		case "@@E":
			if in {
				if cur := parseSample(host.Platform, block, time.Now()); cur != nil {
					n++
					onSample(cur)
				}
			}
			in = false
		default:
			if in {
				if len(block) >= 50000 { // runaway output: drop the block
					block, in = block[:0], false
					continue
				}
				block = append(block, line)
			}
		}
	}
	return n
}

func parseSample(platform string, lines []string, at time.Time) *rawSample {
	switch {
	case platform == platLinux:
		return parseLinuxSample(lines, at)
	case platform == platDarwin:
		return parseDarwinSample(lines, at)
	case isBSD(platform):
		return parseBSDSample(lines, at)
	}
	return nil
}

// oneShot collects a single sample (two readings one second apart, so CPU usage and rates are meaningful).
func (s *Service) oneShot(ctx context.Context, t *target) (*Stats, error) {
	cmd, err := loopCommand(t.host.Platform, time.Second, 2)
	if err != nil {
		return nil, httpx.NewError(422, "monitor_unavailable", err.Error())
	}
	res, err := s.run(ctx, t, cmd)
	if err != nil {
		return nil, err
	}
	var prev, last *rawSample
	var out *Stats
	consumeSamples(strings.NewReader(string(res.Stdout)), t.host, func(cur *rawSample) {
		out = computeStats(prev, cur, t.host)
		prev, last = cur, cur
	})
	if last == nil || out == nil {
		msg := res.errText()
		if msg == "" {
			msg = "the host returned no data"
		}
		return nil, httpx.NewError(422, "monitor_unavailable", msg)
	}
	return out, nil
}
