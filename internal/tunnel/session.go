package tunnel

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/term"
)

// Session-integrated forwards (TUN-7): an SSH connection's options.forwards ([{type, bindHost, bindPort, destHost,
// destPort, …}]) are started over the terminal's own SSH client when the runtime session connects and stopped when
// it disconnects or closes; they come back after every reconnect. Users can also add ad-hoc forwards to a live
// session (e.g. "Forward" in the remote-ports dialog); those live until the session is closed.

const maxSessionForwards = 64

type sessionForwards struct {
	m *Manager

	mu       sync.Mutex
	sessions map[string]*sessState
}

// sessState tracks one SSH runtime session that has (or had) forwards.
type sessState struct {
	id     string
	owner  *model.User
	connID string

	opMu sync.Mutex // serializes start/stop of the session's forwards

	mu        sync.Mutex
	title     string
	gen       uint64 // bumped on every connect / disconnect
	connected bool
	adhoc     []*sfDef
	disabled  map[string]bool // configured forwards removed by the user for this session's lifetime
	runs      []*sfRun
	link      link
	linkRel   func()
}

// sfDef is one session forward definition.
type sfDef struct {
	id     string
	source string // "connection" | "adhoc"
	spec   ForwardSpec
}

// sfRun is a started session forward.
type sfRun struct {
	def *sfDef
	st  *stats
	fw  *forward

	mu      sync.Mutex
	state   string
	err     string
	warning string // see Status.Warning
	since   time.Time
}

func (r *sfRun) set(state, err string) {
	r.mu.Lock()
	r.state, r.err = state, truncate(err, 400)
	r.mu.Unlock()
	r.st.dirty.Store(true)
}

func newSessionForwards(m *Manager) *sessionForwards {
	return &sessionForwards{m: m, sessions: map[string]*sessState{}}
}

// install registers the term hooks.
func (sf *sessionForwards) install() func() {
	if sf.m.sessions == nil {
		return func() {}
	}
	return sf.m.sessions.AddHooks(term.Hooks{
		OnState: func(s *term.Session, st model.SessionState) {
			if s.Protocol != model.ProtoSSH || s.Kind != model.KindTerminal {
				return
			}
			switch st {
			case model.StateConnected:
				sf.onConnected(s)
			case model.StateConnecting, model.StateDisconnected, model.StateError:
				sf.onDisconnected(s.ID)
			}
		},
		OnClose: func(s *term.Session) { sf.onClosed(s.ID) },
	})
}

// configured parses a connection's options.forwards (invalid entries are reported and skipped).
func configured(opts model.Options) ([]ForwardSpec, error) {
	if !opts.Has("forwards") {
		return nil, nil
	}
	var list []ForwardSpec
	if err := opts.Decode("forwards", &list); err != nil {
		return nil, fmt.Errorf("options.forwards is not a list of forwards: %w", err)
	}
	return list, nil
}

func (sf *sessionForwards) onConnected(s *term.Session) {
	conn := s.Connection()
	var specs []ForwardSpec
	var parseErr error
	if conn != nil {
		specs, parseErr = configured(conn.Options)
	}
	sf.mu.Lock()
	ss := sf.sessions[s.ID]
	if ss == nil {
		if len(specs) == 0 && parseErr == nil {
			sf.mu.Unlock()
			return // nothing configured and no ad-hoc forwards yet
		}
		ss = &sessState{id: s.ID, owner: s.Owner(), disabled: map[string]bool{}}
		sf.sessions[s.ID] = ss
	}
	sf.mu.Unlock()
	if parseErr != nil {
		s.Notice("Port forwarding: " + parseErr.Error())
	}
	ss.mu.Lock()
	ss.gen++
	gen := ss.gen
	ss.title = s.Title()
	if conn != nil {
		ss.connID = conn.ID
	}
	ss.mu.Unlock()
	go sf.startAll(ss, s, specs, gen)
}

func (sf *sessionForwards) onDisconnected(id string) {
	ss := sf.get(id)
	if ss == nil {
		return
	}
	ss.mu.Lock()
	ss.gen++
	ss.connected = false
	ss.mu.Unlock()
	go func() {
		sf.stopRuns(ss)
		sf.publish(ss)
	}()
}

func (sf *sessionForwards) onClosed(id string) {
	sf.mu.Lock()
	ss := sf.sessions[id]
	delete(sf.sessions, id)
	sf.mu.Unlock()
	if ss == nil {
		return
	}
	ss.mu.Lock()
	ss.gen++
	ss.connected = false
	ss.adhoc = nil
	ss.mu.Unlock()
	go func() {
		sf.stopRuns(ss)
		sf.m.d.Events.Publish(ss.owner.ID, SessionEvent{Type: evSession, SessionID: ss.id, Forwards: []SessionForward{}})
	}()
}

func (sf *sessionForwards) get(id string) *sessState {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	return sf.sessions[id]
}

// startAll starts the configured and ad-hoc forwards over the session's SSH client (generation gen).
func (sf *sessionForwards) startAll(ss *sessState, s *term.Session, specs []ForwardSpec, gen uint64) {
	ss.opMu.Lock()
	defer ss.opMu.Unlock()
	ss.mu.Lock()
	if ss.gen != gen {
		ss.mu.Unlock()
		return
	}
	ss.mu.Unlock()
	sf.stopRunsLocked(ss)

	ctx, cancel := context.WithTimeout(sf.m.ctx, 15*time.Second)
	l, rel, err := sf.m.forSession(ctx, ss.owner, ss.id)
	cancel()
	if err != nil {
		s.Notice("Port forwarding unavailable: " + err.Error())
		return
	}
	ss.mu.Lock()
	if ss.gen != gen {
		ss.mu.Unlock()
		rel()
		return
	}
	ss.link, ss.linkRel, ss.connected = l, rel, true
	defs := make([]*sfDef, 0, len(specs)+len(ss.adhoc))
	for i, spec := range specs {
		if spec.Disabled {
			continue
		}
		id := fmt.Sprintf("c%d", i)
		if ss.disabled[id] {
			continue
		}
		defs = append(defs, &sfDef{id: id, source: "connection", spec: spec})
	}
	defs = append(defs, ss.adhoc...)
	ss.mu.Unlock()

	for _, d := range defs {
		sf.startOneLocked(ss, s, d, l)
	}
	sf.publish(ss)
}

// startOneLocked starts one forward (caller holds ss.opMu).
func (sf *sessionForwards) startOneLocked(ss *sessState, s *term.Session, d *sfDef, l link) {
	run := &sfRun{def: d, st: &stats{}, state: model.TunnelStarting, since: time.Now().UTC()}
	ss.mu.Lock()
	ss.runs = append(ss.runs, run)
	ss.mu.Unlock()
	notice := func(msg string) {
		if s != nil {
			s.Notice(msg)
		}
	}
	sp, err := sf.specOf(ss.owner, d.spec)
	if err != nil {
		run.set(model.TunnelError, err.Error())
		notice(fmt.Sprintf("Port forward %s failed: %s", describeForwardSpec(d.spec), err))
		return
	}
	fw := newForward(sf.m.ctx, sp, run.st, sf.m.log.With("session", ss.id), fixedLink{l})
	run.mu.Lock()
	run.fw = fw
	run.mu.Unlock()
	if sp.kind.listensLocally() {
		if err := fw.listenLocal(); err != nil {
			fw.close()
			run.set(model.TunnelError, err.Error())
			notice(fmt.Sprintf("Port forward %s failed: %s", sp.label(), err))
			return
		}
		run.set(model.TunnelRunning, "")
		msg := "Port forward active: " + sp.labelWith(loadStr(&run.st.localAddr))
		if sp.exposedLocal() {
			msg += " (reachable from other machines)"
		}
		notice(msg)
		return
	}
	ready := make(chan error, 1)
	go func() {
		err := fw.serveRemote(fw.ctx, l, func(string) { ready <- nil })
		if err != nil {
			select {
			case ready <- err:
			default:
			}
			return
		}
		// The remote listener ended (link lost or forward closed).
		run.mu.Lock()
		if run.state == model.TunnelRunning && fw.ctx.Err() == nil {
			run.mu.Unlock()
			run.set(model.TunnelStopped, "")
			return
		}
		run.mu.Unlock()
	}()
	select {
	case err := <-ready:
		if err != nil {
			fw.close()
			run.set(model.TunnelError, err.Error())
			notice(fmt.Sprintf("Port forward %s failed: %s", sp.label(), err))
			return
		}
		run.set(model.TunnelRunning, "")
		bound := loadStr(&run.st.remoteAdr)
		notice("Port forward active: " + sp.labelWith(bound))
		if sp.exposedRemote() {
			go func() {
				msg := bindWarning(fw.ctx, l, bound)
				if msg == "" || fw.ctx.Err() != nil {
					return
				}
				run.mu.Lock()
				run.warning = msg
				run.mu.Unlock()
				run.st.dirty.Store(true)
				notice(fmt.Sprintf("Port forward %s: %s", sp.labelWith(bound), msg))
			}()
		}
	case <-time.After(30 * time.Second):
		fw.close()
		run.set(model.TunnelError, "timed out waiting for the SSH server to open the remote listener")
		notice(fmt.Sprintf("Port forward %s failed: no answer from the SSH server", sp.label()))
	}
}

// specOf validates a session forward for its owner.
func (sf *sessionForwards) specOf(owner *model.User, fs ForwardSpec) (*spec, error) {
	d := def{Type: fs.Type, BindHost: fs.BindHost, BindPort: fs.BindPort, DestHost: fs.DestHost, DestPort: fs.DestPort,
		Reverse: fs.Reverse, BindSocket: fs.BindSocket, DestSocket: fs.DestSocket}
	if err := d.normalize(); err != nil {
		return nil, err
	}
	var o Options
	if err := normalizeOptions(&o, d.kind()); err != nil {
		return nil, err
	}
	sp, err := buildSpec(d, o, nil)
	if err != nil {
		return nil, err
	}
	sp.autoReconnect, sp.onDemand = false, false
	if err := sf.m.policy(owner, sp); err != nil {
		return nil, err
	}
	sf.m.finishSpec(owner, sp)
	return sp, nil
}

// normalizeForwardSpec validates and canonicalizes a forward definition (API input and connection options).
func normalizeForwardSpec(fs ForwardSpec) (ForwardSpec, error) {
	d := def{Type: fs.Type, BindHost: fs.BindHost, BindPort: fs.BindPort, DestHost: fs.DestHost, DestPort: fs.DestPort,
		Reverse: fs.Reverse, BindSocket: fs.BindSocket, DestSocket: fs.DestSocket}
	if err := d.normalize(); err != nil {
		return fs, err
	}
	if len(fs.Name) > maxNameLen {
		return fs, httpx.BadRequest("name is too long")
	}
	return ForwardSpec{Type: d.Type, BindHost: d.BindHost, BindPort: d.BindPort, DestHost: d.DestHost, DestPort: d.DestPort,
		Reverse: d.Reverse, BindSocket: d.BindSocket, DestSocket: d.DestSocket, Name: fs.Name, Disabled: fs.Disabled}, nil
}

func describeForwardSpec(fs ForwardSpec) string {
	if n, err := normalizeForwardSpec(fs); err == nil {
		d := def{Type: n.Type, BindHost: n.BindHost, BindPort: n.BindPort, DestHost: n.DestHost, DestPort: n.DestPort,
			Reverse: n.Reverse, BindSocket: n.BindSocket, DestSocket: n.DestSocket}
		sp := &spec{kind: d.kind(), bindHost: d.BindHost, bindPort: d.BindPort, bindSocket: d.BindSocket,
			destHost: d.DestHost, destPort: d.DestPort, destSocket: d.DestSocket}
		return sp.label()
	}
	return fmt.Sprintf("%s %s:%d", fs.Type, fs.BindHost, fs.BindPort)
}

// stopRuns stops the session's running forwards and releases its link.
func (sf *sessionForwards) stopRuns(ss *sessState) {
	ss.opMu.Lock()
	defer ss.opMu.Unlock()
	sf.stopRunsLocked(ss)
}

func (sf *sessionForwards) stopRunsLocked(ss *sessState) {
	ss.mu.Lock()
	runs := ss.runs
	ss.runs = nil
	rel := ss.linkRel
	ss.link, ss.linkRel = nil, nil
	ss.mu.Unlock()
	for _, r := range runs {
		r.mu.Lock()
		fw := r.fw
		r.mu.Unlock()
		if fw != nil {
			fw.close()
		}
	}
	if rel != nil {
		rel()
	}
}

func (sf *sessionForwards) stopAll() {
	sf.mu.Lock()
	all := make([]*sessState, 0, len(sf.sessions))
	for _, ss := range sf.sessions {
		all = append(all, ss)
	}
	sf.sessions = map[string]*sessState{}
	sf.mu.Unlock()
	for _, ss := range all {
		sf.stopRuns(ss)
	}
}

// views returns the JSON view of a session's forwards.
func (ss *sessState) views() []SessionForward {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	out := make([]SessionForward, 0, len(ss.runs)+len(ss.adhoc))
	seen := map[string]bool{}
	for _, r := range ss.runs {
		seen[r.def.id] = true
		st := Status{}
		r.mu.Lock()
		st.State, st.Error = r.state, r.err
		if r.state == model.TunnelRunning {
			t := r.since
			st.StartedAt = &t
			st.Warning = r.warning
		}
		r.mu.Unlock()
		st.Connected = ss.connected
		r.st.fill(&st)
		out = append(out, SessionForward{ID: ss.id + ":" + r.def.id, SessionID: ss.id, SessionTitle: ss.title,
			ConnectionID: ss.connID, Source: r.def.source, Spec: r.def.spec, Status: st})
	}
	// Ad-hoc forwards waiting for the session to (re)connect.
	for _, d := range ss.adhoc {
		if seen[d.id] {
			continue
		}
		out = append(out, SessionForward{ID: ss.id + ":" + d.id, SessionID: ss.id, SessionTitle: ss.title,
			ConnectionID: ss.connID, Source: d.source, Spec: d.spec, Status: stoppedStatus()})
	}
	return out
}

func (sf *sessionForwards) publish(ss *sessState) {
	if sf.m.d.Events == nil {
		return
	}
	sf.m.d.Events.Publish(ss.owner.ID, SessionEvent{Type: evSession, SessionID: ss.id, Forwards: ss.views()})
}

// tick samples rates and republishes sessions whose counters changed.
func (sf *sessionForwards) tick(now time.Time) {
	sf.mu.Lock()
	all := make([]*sessState, 0, len(sf.sessions))
	for _, ss := range sf.sessions {
		all = append(all, ss)
	}
	sf.mu.Unlock()
	for _, ss := range all {
		ss.mu.Lock()
		runs := append([]*sfRun(nil), ss.runs...)
		ss.mu.Unlock()
		changed := false
		for _, r := range runs {
			r.st.sampleRates(now)
			if r.st.dirty.Swap(false) {
				changed = true
			}
		}
		if changed {
			sf.publish(ss)
		}
	}
}

// list returns the session forwards of user (optionally of one session), newest sessions last.
func (sf *sessionForwards) list(user *model.User, sessionID string) []SessionForward {
	sf.mu.Lock()
	all := make([]*sessState, 0, len(sf.sessions))
	for _, ss := range sf.sessions {
		if ss.owner.ID == user.ID && (sessionID == "" || ss.id == sessionID) {
			all = append(all, ss)
		}
	}
	sf.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].id < all[j].id })
	out := []SessionForward{}
	for _, ss := range all {
		out = append(out, ss.views()...)
	}
	return out
}

// add starts an ad-hoc forward on a live SSH session of user.
func (sf *sessionForwards) add(ctx context.Context, user *model.User, sessionID string, fs ForwardSpec) (SessionForward, error) {
	if sf.m.sessions == nil {
		return SessionForward{}, httpx.Conflict("sessions are not available")
	}
	s := sf.m.sessions.Get(sessionID)
	if s == nil || s.OwnerID != user.ID {
		return SessionForward{}, httpx.ErrNotFound
	}
	if s.Protocol != model.ProtoSSH {
		return SessionForward{}, httpx.BadRequest("port forwarding needs an SSH session")
	}
	n, err := normalizeForwardSpec(fs)
	if err != nil {
		return SessionForward{}, err
	}
	if _, err := sf.specOf(user, n); err != nil {
		return SessionForward{}, err
	}
	if st, _ := s.State(); st != model.StateConnected {
		return SessionForward{}, httpx.Conflict("the session is not connected")
	}
	sf.mu.Lock()
	ss := sf.sessions[sessionID]
	created := ss == nil
	if created {
		ss = &sessState{id: sessionID, owner: s.Owner(), disabled: map[string]bool{}, title: s.Title()}
		if c := s.Connection(); c != nil {
			ss.connID = c.ID
		}
		sf.sessions[sessionID] = ss
	}
	sf.mu.Unlock()

	ss.opMu.Lock()
	defer ss.opMu.Unlock()
	// The session may have closed since the checks above. The term manager forgets a session before its OnClose
	// hooks run, so once it is gone onClosed has already run or will still find ss registered and stop its forwards.
	// A state created here after onClosed ran would be an orphan nobody stops: drop it (it holds nothing yet).
	gone := sf.m.sessions.Get(sessionID) == nil
	sf.mu.Lock()
	registered := sf.sessions[sessionID] == ss
	if registered && gone && created {
		delete(sf.sessions, sessionID)
	}
	sf.mu.Unlock()
	if !registered || gone {
		return SessionForward{}, httpx.Conflict("the session is not connected")
	}
	ss.mu.Lock()
	if len(ss.adhoc)+len(ss.runs) >= maxSessionForwards {
		ss.mu.Unlock()
		return SessionForward{}, httpx.Conflict(fmt.Sprintf("at most %d forwards per session", maxSessionForwards))
	}
	d := &sfDef{id: "a" + model.NewID()[:10], source: "adhoc", spec: n}
	ss.adhoc = append(ss.adhoc, d)
	l := ss.link
	ss.mu.Unlock()
	if l == nil {
		// First forward of a session without configured forwards: borrow the terminal's client now.
		lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		nl, rel, err := sf.m.forSession(lctx, user, sessionID)
		cancel()
		if err != nil {
			sf.dropAdhoc(ss, d.id)
			return SessionForward{}, err
		}
		ss.mu.Lock()
		if ss.link == nil {
			ss.link, ss.linkRel, ss.connected = nl, rel, true
			l = nl
		} else {
			l = ss.link
			rel()
		}
		ss.mu.Unlock()
	}
	sf.startOneLocked(ss, s, d, l)
	sf.publish(ss)
	for _, v := range ss.views() {
		if v.ID == ss.id+":"+d.id {
			return v, nil
		}
	}
	return SessionForward{}, errors.New("forward vanished")
}

func (sf *sessionForwards) dropAdhoc(ss *sessState, id string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for i, d := range ss.adhoc {
		if d.id == id {
			ss.adhoc = append(ss.adhoc[:i], ss.adhoc[i+1:]...)
			return
		}
	}
}

// remove stops a session forward: ad-hoc forwards are deleted, configured ones are disabled until the session
// closes.
func (sf *sessionForwards) remove(user *model.User, fullID string) error {
	sessionID, id, ok := cutLast(fullID, ':')
	if !ok {
		return httpx.ErrNotFound
	}
	ss := sf.get(sessionID)
	if ss == nil || ss.owner.ID != user.ID {
		return httpx.ErrNotFound
	}
	ss.opMu.Lock()
	ss.mu.Lock()
	found := false
	for i, d := range ss.adhoc {
		if d.id == id {
			ss.adhoc = append(ss.adhoc[:i], ss.adhoc[i+1:]...)
			found = true
			break
		}
	}
	var stop *sfRun
	for i, r := range ss.runs {
		if r.def.id == id {
			stop = r
			ss.runs = append(ss.runs[:i], ss.runs[i+1:]...)
			found = true
			if r.def.source == "connection" {
				ss.disabled[id] = true
			}
			break
		}
	}
	ss.mu.Unlock()
	ss.opMu.Unlock()
	if !found {
		return httpx.ErrNotFound
	}
	if stop != nil {
		stop.mu.Lock()
		fw := stop.fw
		stop.mu.Unlock()
		if fw != nil {
			fw.close()
		}
	}
	sf.publish(ss)
	return nil
}

func cutLast(s string, sep byte) (before, after string, ok bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// fixedLink provides a session's SSH client to its forwards.
type fixedLink struct{ l link }

func (f fixedLink) linkFor(context.Context) (link, error) {
	select {
	case <-f.l.Done():
		return nil, errors.New("the SSH session is disconnected")
	default:
		return f.l, nil
	}
}
