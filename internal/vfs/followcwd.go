package vfs

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/core"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// "Follow terminal folder" (FILE-2), MobaXterm style: when an SSH terminal connects, detect the login shell over the
// session's own pooled transport (exec `echo "$SHELL"`), wait until the first prompt is idle, then inject one hidden
// setup line that makes the shell report its folder with OSC 7 (term.ShellIntegrationLine / Manager.InjectHidden).
// The terminal scanner turns those reports into RuntimeSession.cwd (session.updated) and {type:'cwd'} messages the
// files panel follows. Every failure is silent (debug log): the session is never disturbed.

// Tunables (tests shorten them).
var (
	cwdPromptIdle   = 500 * time.Millisecond // output and input quiet this long at a prompt
	cwdPromptWait   = 30 * time.Minute       // give up after this long without a quiet prompt
	cwdPollInterval = 100 * time.Millisecond // first minute; then cwdSlowPoll
	cwdSlowPoll     = 500 * time.Millisecond
)

type followCwd struct {
	d   *app.Deps
	c   *core.Core
	log *slog.Logger

	mu    sync.Mutex
	state map[string]*cwdState
}

type cwdState struct {
	cancel   context.CancelFunc
	inputAt  time.Time // last keystroke (terminal replies excluded)
	lineOpen bool      // the user has typed a line that was not submitted (no Enter / Ctrl-C yet)
}

// startFollowCwd registers the session hooks driving the shell integration.
func startFollowCwd(d *app.Deps, c *core.Core) {
	if c == nil || c.Sessions == nil || c.SSH == nil {
		return
	}
	log := slog.Default()
	if d != nil && d.Log != nil {
		log = d.Log
	}
	fc := &followCwd{d: d, c: c, log: log.With("module", "vfs", "feature", "follow-cwd"), state: map[string]*cwdState{}}
	c.Sessions.AddHooks(term.Hooks{OnState: fc.onState, OnInput: fc.onInput, OnClose: fc.onClose})
}

func (fc *followCwd) onState(s *term.Session, st model.SessionState) {
	if s.Protocol != model.ProtoSSH || s.Kind != model.KindTerminal {
		return
	}
	switch st {
	case model.StateConnected:
		ctx := context.Background()
		if fc.d != nil && fc.d.Ctx != nil {
			ctx = fc.d.Ctx
		}
		ctx, cancel := context.WithCancel(ctx)
		fc.mu.Lock()
		if old := fc.state[s.ID]; old != nil {
			old.cancel()
		}
		fc.state[s.ID] = &cwdState{cancel: cancel}
		fc.mu.Unlock()
		go fc.run(ctx, s)
	case model.StateDisconnected, model.StateError, model.StateClosed:
		fc.stop(s.ID)
	}
}

func (fc *followCwd) onInput(s *term.Session, data []byte) {
	if len(data) == 0 || term.IsTerminalReply(data) {
		return // automatic terminal answers (cursor position reports...) are not typing
	}
	fc.mu.Lock()
	if st := fc.state[s.ID]; st != nil {
		st.inputAt = time.Now()
		switch data[len(data)-1] {
		case '\r', '\n', 0x03, 0x04: // Enter, Ctrl-C, Ctrl-D: the line was submitted / discarded
			st.lineOpen = false
		default:
			st.lineOpen = true
		}
	}
	fc.mu.Unlock()
}

func (fc *followCwd) onClose(s *term.Session) { fc.stop(s.ID) }

func (fc *followCwd) stop(id string) {
	fc.mu.Lock()
	if st := fc.state[id]; st != nil {
		st.cancel()
		delete(fc.state, id)
	}
	fc.mu.Unlock()
}

// inputQuiet reports whether the user has no half-typed line and did not type for at least d.
func (fc *followCwd) inputQuiet(id string, d time.Duration) bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	st := fc.state[id]
	if st == nil {
		return false
	}
	return !st.lineOpen && (st.inputAt.IsZero() || time.Since(st.inputAt) >= d)
}

// enabled reports whether the integration applies: connection option followCwd (default true), else the files
// setting followTerminal of the owner (then global) when it is explicitly false.
func (fc *followCwd) enabled(ctx context.Context, s *term.Session, conn *model.Connection) bool {
	o := conn.Options
	if o.Has("followCwd") {
		return o.Bool("followCwd", true)
	}
	switch strings.ToLower(o.String("sshBrowser", "sftp")) {
	case "none", "off", "disabled", "false":
		return false
	}
	if o.String("remoteCommand", "") != "" {
		return false
	}
	if fc.d == nil || fc.d.Store == nil {
		return true
	}
	for _, scope := range []string{s.OwnerID, store.ScopeGlobal} {
		var sec map[string]any
		if ok, err := fc.d.Store.Settings.GetJSON(ctx, scope, "files", &sec); err == nil && ok {
			if v, present := sec["followTerminal"]; present {
				if b, isBool := v.(bool); isBool {
					return b
				}
			}
		}
	}
	return true
}

func (fc *followCwd) run(ctx context.Context, s *term.Session) {
	defer func() {
		if r := recover(); r != nil {
			fc.log.Error("follow-cwd panicked", "session", s.ID, "panic", r)
		}
	}()
	start := time.Now()
	conn := s.Connection()
	if conn == nil || conn.Options.String("remoteCommand", "") != "" || !fc.enabled(ctx, s, conn) {
		return
	}

	// 1. Detect the login shell (and the host name, see ShellIntegrationLineFor) over the session's own transport
	//    (no second login).
	family, host, rtt := fc.detectShell(ctx, s)
	if family == "" {
		return
	}
	line := term.ShellIntegrationLineFor(family, host)
	// Slow links: the echo of the line needs a few round trips (the filter also extends the hold while it arrives).
	timeout := max(4*time.Second, 4*rtt+2*time.Second)

	// 2. Wait for a quiet prompt — output idle, no keystroke for a moment, no half-typed line, the cursor line looks
	//    like a shell prompt (InjectHidden checks that and full-screen programs) — then 3. inject the hidden line.
	//    Typing does not abandon the integration: it is retried at the next quiet prompt. Keystrokes arriving while
	//    the hidden line runs are deferred by term and delivered right after it.
	_, startHead := s.Offsets()
	lastHead, lastChange := int64(-1), time.Now()
	deadline := start.Add(cwdPromptWait)
	for {
		poll := cwdPollInterval
		if time.Since(start) > time.Minute {
			poll = cwdSlowPoll
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
		if time.Now().After(deadline) || s.Closed() {
			fc.log.Debug("no quiet prompt; shell integration skipped", "session", s.ID)
			return
		}
		_, head := s.Offsets()
		// The shell reports its folder itself (fish, vte.sh...). Only a report of this connection counts: the
		// folder of the previous connection is dropped when the new shell's first output arrives (term), so a
		// reconnect always gets the integration again.
		if head > startHead && s.Cwd() != "" {
			return
		}
		if st, _ := s.State(); st != model.StateConnected {
			return
		}
		if head != lastHead {
			lastHead, lastChange = head, time.Now()
			continue
		}
		if head == 0 || time.Since(lastChange) < cwdPromptIdle || !fc.inputQuiet(s.ID, cwdPromptIdle) {
			continue
		}
		err := fc.c.Sessions.InjectHidden(s.ID, line, term.InjectOptions{MarkerOSC: "7", Timeout: timeout, Precheck: func() bool {
			_, h := s.Offsets()
			return h == lastHead && fc.inputQuiet(s.ID, cwdPromptIdle)
		}})
		switch {
		case errors.Is(err, term.ErrNotAtPrompt), errors.Is(err, term.ErrUserActive), errors.Is(err, term.ErrInjectPending):
			lastChange = time.Now() // not now: wait for the next quiet prompt
			continue
		case err != nil:
			fc.log.Debug("shell integration not injected", "session", s.ID, "err", err)
			return
		}
		fc.log.Debug("shell integration injected", "session", s.ID, "shell", family)
		return
	}
}

// detectShell asks the server for the login shell ($SHELL, else the name of the shell running the command) and
// its host name (uname -n), and measures a round trip of the transport.
func (fc *followCwd) detectShell(ctx context.Context, s *term.Session) (family, host string, rtt time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cl, release, err := fc.c.SSH.ForSession(ctx, s.Owner(), s.ID)
	if err != nil {
		fc.log.Debug("shell detection: no transport", "session", s.ID, "err", err)
		return "", "", 0
	}
	defer release()
	start := time.Now()
	out, _, code, err := cl.Exec(ctx, `echo "$SHELL"; uname -n 2>/dev/null`)
	rtt = time.Since(start)
	if ms := cl.Info().LatencyMs; ms > 0 {
		rtt = max(rtt/2, time.Duration(ms)*time.Millisecond)
	}
	var sh string
	if err == nil && code == 0 {
		lines := strings.Split(strings.TrimRight(string(out), "\r\n"), "\n") // line 1 may be empty ($SHELL unset)
		sh = strings.TrimSpace(lines[0])
		if len(lines) > 1 {
			host = strings.TrimSpace(lines[1])
		}
	}
	if sh == "" {
		if out, _, code, err := cl.Exec(ctx, `ps -p $$ -o comm=`); err == nil && code == 0 {
			sh = strings.TrimSpace(string(out))
		}
	}
	if sh == "" {
		return "", "", 0
	}
	if k := term.ShellKind(sh); k != "" {
		return k, host, rtt
	}
	fc.log.Debug("unsupported login shell; shell integration skipped", "session", s.ID, "shell", sh)
	return "", "", 0
}
