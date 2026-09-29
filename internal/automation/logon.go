package automation

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/term"
)

// Logon actions (AUTO-8): connection.options.logonActions is an ordered list of expect/send steps executed by the
// backend after every (re)connect — also for telnet and serial sessions, with or without a browser attached:
//
//	[{"expect": "login:", "send": "admin"}, {"expect": "[Pp]assword:", "secret": "password"},
//	 {"expect": "[>#]\\s*$", "send": "terminal length 0"}]
//
// expect is a regular expression (RE2, case-sensitive unless it starts with (?i)) matched against the ANSI-stripped
// output received since the previous step; send is typed with C escapes decoded (\r \t \x03 …); secret names a stored
// secret of the connection; enter (default true) presses Enter after the text; timeoutSec (default 20) bounds the
// wait; optional steps are skipped on timeout instead of stopping the sequence; delayMs pauses before typing.
// Failures are reported as dim notices in the terminal.
//
// Ordering: options.startupCommand is typed after the last logon step. The module registers term's startup handler
// (see claimStartup), which takes the command over from term for connections with logon actions; it is typed only
// when every step succeeded (a stopped sequence may have left the session at a login prompt).

const (
	defaultLogonTimeout = 20 * time.Second
	maxLogonTimeout     = 10 * time.Minute
)

type logonManager struct {
	m  *Module
	mu sync.Mutex
	// per session: the reader prepared at "connecting", the running sequence and the start-up command claimed
	// from term for this connect (sent after the last step)
	prep    map[string]*logonPrep
	runs    map[string]context.CancelFunc
	startup map[string]func()
}

type logonPrep struct {
	t   *tap
	r   *reader
	gen int64
}

func newLogonManager(m *Module) *logonManager {
	return &logonManager{m: m, prep: map[string]*logonPrep{}, runs: map[string]context.CancelFunc{},
		startup: map[string]func(){}}
}

// claimStartup is term's StartupHandler: for a connection with logon actions it keeps the start-up command's send
// function, so the command is typed after the last step instead of before the first one. term calls it on every
// (re)connect before the "connected" hooks, which then pick the function up (see onState).
func (lm *logonManager) claimStartup(s *term.Session, conn *model.Connection, send func()) bool {
	if s.Kind != model.KindTerminal || conn == nil {
		return false
	}
	if steps, err := parseLogonActions(conn.Options); err != nil || len(steps) == 0 {
		return false
	}
	lm.mu.Lock()
	lm.startup[s.ID] = send
	lm.mu.Unlock()
	return true
}

// takeStartup returns (and forgets) the start-up command claimed for the session's current connect, or nil.
func (lm *logonManager) takeStartup(id string) func() {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	send := lm.startup[id]
	delete(lm.startup, id)
	return send
}

// parseLogonActions decodes and validates connection.options.logonActions.
func parseLogonActions(opts model.Options) ([]LogonAction, error) {
	if !opts.Has("logonActions") {
		return nil, nil
	}
	var steps []LogonAction
	if err := opts.Decode("logonActions", &steps); err != nil {
		return nil, fmt.Errorf("logon actions are invalid: %w", err)
	}
	if len(steps) > maxLogonActions {
		return nil, fmt.Errorf("too many logon actions (max %d)", maxLogonActions)
	}
	for i, st := range steps {
		if st.Expect != "" {
			if _, err := regexp.Compile(st.Expect); err != nil {
				return nil, fmt.Errorf("logon action %d: invalid pattern: %v", i+1, err)
			}
		}
		if st.Secret != "" && !validSecretKey(st.Secret) {
			return nil, fmt.Errorf("logon action %d: invalid secret name", i+1)
		}
	}
	return steps, nil
}

func (lm *logonManager) onState(s *term.Session, st model.SessionState) {
	switch st {
	case model.StateConnecting:
		lm.stop(s.ID)
		// Start capturing output now: the backend pump starts before the "connected" hook runs, so the first
		// prompt could otherwise be missed. The session's connection is the one resolved for the previous connect
		// (or at creation); actions added since are picked up at "connected" (see below).
		if conn := s.Connection(); conn == nil || !conn.Options.Has("logonActions") {
			return
		}
		t := lm.m.acquireTap(s)
		t.setState(model.StateConnecting)
		r := t.newReader(false)
		r.bindGeneration()
		lm.mu.Lock()
		if old := lm.prep[s.ID]; old != nil {
			old.t.release()
		}
		lm.prep[s.ID] = &logonPrep{t: t, r: r, gen: r.gen}
		lm.mu.Unlock()
	case model.StateConnected:
		lm.mu.Lock()
		p := lm.prep[s.ID]
		delete(lm.prep, s.ID)
		lm.mu.Unlock()
		// Claimed by claimStartup for this connect; every path that does not run the steps types it at once (send is
		// a no-op when the session has moved on).
		startup := lm.takeStartup(s.ID)
		conn := s.Connection() // resolved for this connect
		var steps []LogonAction
		var err error
		if conn != nil {
			steps, err = parseLogonActions(conn.Options)
		}
		if err != nil || len(steps) == 0 || (p != nil && p.t.generation() != p.gen) {
			if p != nil {
				p.t.release()
			}
			if err != nil {
				s.Notice("Logon actions skipped: " + err.Error())
			}
			if startup != nil {
				startup()
			}
			return
		}
		if p == nil {
			// Logon actions were added after the previous connect: capture from the current line on.
			t := lm.m.acquireTap(s)
			r := t.newReader(true)
			r.bindGeneration()
			p = &logonPrep{t: t, r: r, gen: r.gen}
		}
		ctx, cancel := context.WithCancel(lm.m.ctx)
		lm.mu.Lock()
		if prev := lm.runs[s.ID]; prev != nil {
			prev()
		}
		lm.runs[s.ID] = cancel
		lm.mu.Unlock()
		go func() {
			defer p.t.release()
			defer cancel()
			lm.finish(s, lm.run(ctx, s, p.r, steps), startup)
			lm.mu.Lock()
			if lm.runs[s.ID] != nil {
				delete(lm.runs, s.ID)
			}
			lm.mu.Unlock()
		}()
	case model.StateDisconnected, model.StateError:
		lm.stop(s.ID)
		lm.mu.Lock()
		p := lm.prep[s.ID]
		delete(lm.prep, s.ID)
		lm.mu.Unlock()
		if p != nil {
			p.t.release()
		}
	}
}

func (lm *logonManager) onClose(s *term.Session) {
	lm.stop(s.ID)
	lm.takeStartup(s.ID)
	lm.mu.Lock()
	p := lm.prep[s.ID]
	delete(lm.prep, s.ID)
	lm.mu.Unlock()
	if p != nil {
		p.t.release()
	}
}

func (lm *logonManager) stop(id string) {
	lm.mu.Lock()
	cancel := lm.runs[id]
	delete(lm.runs, id)
	lm.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// errLogonAborted: the sequence ended because the session went away or reconnected (nothing to report).
var errLogonAborted = errors.New("logon actions aborted")

// finish reports how a sequence ended and types the claimed start-up command after a complete one.
func (lm *logonManager) finish(s *term.Session, err error, startup func()) {
	switch {
	case err == nil:
		if startup != nil {
			startup()
		}
	case errors.Is(err, errLogonAborted):
	default:
		msg := "Logon actions stopped: " + err.Error() + "."
		if startup != nil {
			msg += " The start-up command was not sent."
		}
		s.Notice(msg)
	}
}

// run types the steps; the error says why it stopped early (errLogonAborted when there is nothing to report).
func (lm *logonManager) run(ctx context.Context, s *term.Session, r *reader, steps []LogonAction) error {
	var secrets map[string]string
	for i, st := range steps {
		n := i + 1
		if st.Expect != "" {
			re, err := regexp.Compile(st.Expect)
			if err != nil {
				return fmt.Errorf("step %d: invalid pattern: %v", n, err)
			}
			timeout := defaultLogonTimeout
			if st.TimeoutSec > 0 {
				timeout = min(time.Duration(st.TimeoutSec)*time.Second, maxLogonTimeout)
			}
			if _, err := r.expect(ctx, []*regexp.Regexp{re}, timeout); err != nil {
				if ctx.Err() != nil || errors.Is(err, errSessionClosed) || errors.Is(err, errSessionDown) || errors.Is(err, errSessionRestart) {
					return errLogonAborted
				}
				if st.Optional {
					continue
				}
				return fmt.Errorf("step %d did not see %q within %s", n, st.Expect, timeout)
			}
		}
		if st.DelayMs > 0 {
			if err := sleepCtx(ctx, time.Duration(min(st.DelayMs, 60000))*time.Millisecond); err != nil {
				return errLogonAborted
			}
		}
		enter := st.Enter == nil || *st.Enter
		var err error
		switch {
		case st.Secret != "":
			// Logon actions are the connection owner's configuration (only owners and admins can edit them), so
			// they may type the connection's secrets even when an admin shared the connection with other users.
			if secrets == nil {
				_, sec, rerr := s.Resolve(ctx)
				if rerr != nil {
					return fmt.Errorf("step %d needs the stored secret %q (%s)", n, st.Secret, errorText(rerr))
				}
				secrets = sec
			}
			v := secrets[st.Secret]
			if v == "" {
				return fmt.Errorf("step %d: no stored secret %q", n, st.Secret)
			}
			err = lm.m.writeSecret(s.ID, v, enter)
		default:
			text := Unescape(st.Send)
			if enter {
				text += "\r"
			}
			if text == "" {
				continue
			}
			err = lm.m.write(s.ID, text)
		}
		if err != nil {
			if ctx.Err() != nil {
				return errLogonAborted
			}
			return fmt.Errorf("step %d could not type: %s", n, errorText(err))
		}
	}
	return nil
}
