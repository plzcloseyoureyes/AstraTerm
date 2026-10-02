package automation

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// scriptParams describes one execution of a script program.
type scriptParams struct {
	name         string
	program      *goja.Program
	vars         map[string]string
	sessionID    string        // bind an existing session of the user
	connectionID string        // open a session for this connection (closed at the end)
	session      *term.Session // bind this session (batch runs)
	timeout      time.Duration
	logf         func(level, text string)
}

type scriptEnv struct {
	m    *Module
	ctx  context.Context
	user *model.User
	vm   *goja.Runtime
	p    scriptParams

	mu     sync.Mutex
	sess   []*jsSession
	opened int
}

type exitSignal struct{ code int }

func isExit(v any) bool {
	_, ok := v.(exitSignal)
	return ok
}

func (e exitSignal) String() string { return "exit(" + strconv.Itoa(e.code) + ")" }

// execScript runs a compiled script to completion (or timeout / cancellation) and closes the sessions it opened.
func (m *Module) execScript(ctx context.Context, user *model.User, p scriptParams) (err error) {
	if p.timeout <= 0 {
		p.timeout = defaultScriptTimeout
	}
	if p.logf == nil {
		p.logf = func(string, string) {}
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	vm := goja.New()
	vm.SetMaxCallStackSize(4096)
	defer scriptHeap.add(vm, m.log)()
	env := &scriptEnv{m: m, ctx: ctx, user: user, vm: vm, p: p}
	defer env.cleanup()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			vm.Interrupt(ctx.Err())
		case <-stop:
		}
	}()

	defer func() {
		if r := recover(); r != nil {
			m.log.Error("script runtime panicked", "script", p.name, "panic", r)
			err = errors.New("internal error in the script runtime")
		}
	}()

	env.install()
	bound := goja.Null()
	switch {
	case p.session != nil:
		bound = env.attach(p.session, false, false).object()
	case p.sessionID != "":
		s, err := m.ownSession(user, p.sessionID)
		if err != nil {
			return err
		}
		bound = env.attach(s, false, false).object()
	case p.connectionID != "":
		js, err := env.open(p.connectionID, 60*time.Second, false)
		if err != nil {
			return err
		}
		bound = js.object()
	}
	_ = vm.Set("session", bound)
	_, runErr := vm.RunProgram(p.program)
	return env.result(runErr)
}

func (e *scriptEnv) result(err error) error {
	if err == nil {
		return nil
	}
	if cerr := e.ctx.Err(); cerr != nil {
		// Stopped from outside (timeout / cancel), whatever the script was doing at that moment.
		var ie *goja.InterruptedError
		if !errors.As(err, &ie) || !isExit(ie.Value()) {
			if errors.Is(cerr, context.DeadlineExceeded) {
				return fmt.Errorf("the script timed out after %s", e.p.timeout)
			}
			return context.Canceled
		}
	}
	if ie, ok := errors.AsType[*goja.InterruptedError](err); ok {
		switch v := ie.Value().(type) {
		case exitSignal:
			if v.code == 0 {
				return nil
			}
			return fmt.Errorf("the script exited with code %d", v.code)
		case error:
			if errors.Is(v, context.DeadlineExceeded) {
				return fmt.Errorf("the script timed out after %s", e.p.timeout)
			}
			return v
		}
		return errors.New("the script was interrupted")
	}
	if _, ok := errors.AsType[*goja.StackOverflowError](err); ok {
		return errors.New("stack overflow (too much recursion)")
	}
	if ex, ok := errors.AsType[*goja.Exception](err); ok {
		msg := ex.Error()
		if len(msg) > 2000 {
			msg = msg[:2000] + "…"
		}
		return errors.New(msg)
	}
	return err
}

func (e *scriptEnv) cleanup() {
	e.mu.Lock()
	list := e.sess
	e.sess = nil
	e.mu.Unlock()
	for _, js := range list {
		js.release()
	}
}

// ---- helpers ------------------------------------------------------------------------------------------------------

// throw raises a JavaScript Error with the given name from a native function.
func (e *scriptEnv) throw(name, msg string) {
	obj, err := e.vm.New(e.vm.Get("Error"), e.vm.ToValue(msg))
	if err != nil {
		panic(e.vm.NewGoError(errors.New(msg)))
	}
	_ = obj.Set("name", name)
	panic(obj)
}

// fail converts a Go error from a blocking call into a JS exception (cancellation is left to the interrupt).
func (e *scriptEnv) fail(err error) {
	switch {
	case errors.Is(err, errExpectTimeout):
		e.throw("TimeoutError", err.Error())
	case errors.Is(err, errSessionClosed), errors.Is(err, errSessionDown), errors.Is(err, errSessionRestart):
		e.throw("SessionError", err.Error())
	case e.ctx.Err() != nil:
		// The watchdog interrupts the VM; make this call fail in the meantime.
		e.throw("InterruptedError", "the script was stopped")
	default:
		e.throw("Error", errorText(err))
	}
}

func isNullish(v goja.Value) bool { return v == nil || goja.IsUndefined(v) || goja.IsNull(v) }

func argString(call goja.FunctionCall, i int) string {
	v := call.Argument(i)
	if isNullish(v) {
		return ""
	}
	return v.String()
}

func argMillis(call goja.FunctionCall, i int, def time.Duration) time.Duration {
	v := call.Argument(i)
	if isNullish(v) {
		return def
	}
	ms := min(max(v.ToInteger(), 0), int64(maxScriptTimeout/time.Millisecond))
	return time.Duration(ms) * time.Millisecond
}

// optObject returns argument i as an object (nil when absent / not an object).
func (e *scriptEnv) optObject(call goja.FunctionCall, i int) *goja.Object {
	v := call.Argument(i)
	if isNullish(v) {
		return nil
	}
	if o, ok := v.(*goja.Object); ok {
		return o
	}
	return nil
}

func optBool(o *goja.Object, key string, def bool) bool {
	if o == nil {
		return def
	}
	v := o.Get(key)
	if isNullish(v) {
		return def
	}
	return v.ToBoolean()
}

func optMillis(o *goja.Object, key string, def time.Duration) time.Duration {
	if o == nil {
		return def
	}
	v := o.Get(key)
	if isNullish(v) {
		return def
	}
	ms := max(v.ToInteger(), 0)
	return min(time.Duration(ms)*time.Millisecond, maxScriptTimeout)
}

// pattern converts a JS RegExp (flags i, m, s honoured) or a string into a Go regular expression.
func (e *scriptEnv) pattern(v goja.Value) *regexp.Regexp {
	var src string
	if obj, ok := v.(*goja.Object); ok && obj.ClassName() == "RegExp" {
		src = obj.Get("source").String()
		var flags strings.Builder
		for _, f := range obj.Get("flags").String() {
			if f == 'i' || f == 'm' || f == 's' {
				flags.WriteRune(f)
			}
		}
		if flags.Len() > 0 {
			src = "(?" + flags.String() + ")" + src
		}
	} else if isNullish(v) {
		e.throw("TypeError", "a pattern is required")
	} else {
		src = v.String()
	}
	re, err := compilePattern(src)
	if err != nil {
		e.throw("SyntaxError", "invalid pattern: "+err.Error())
	}
	return re
}

func (e *scriptEnv) patterns(v goja.Value) []*regexp.Regexp {
	if obj, ok := v.(*goja.Object); ok && obj.ClassName() == "Array" {
		n := int(obj.Get("length").ToInteger())
		if n == 0 || n > 64 {
			e.throw("TypeError", "expect() takes 1 to 64 patterns")
		}
		out := make([]*regexp.Regexp, 0, n)
		for i := range n {
			out = append(out, e.pattern(obj.Get(strconv.Itoa(i))))
		}
		return out
	}
	return []*regexp.Regexp{e.pattern(v)}
}

func (e *scriptEnv) format(args []goja.Value) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		switch {
		case goja.IsUndefined(a):
			parts = append(parts, "undefined")
		case goja.IsNull(a):
			parts = append(parts, "null")
		default:
			if o, ok := a.(*goja.Object); ok && o.ClassName() != "Error" && o.ClassName() != "Function" {
				if b, err := o.MarshalJSON(); err == nil {
					parts = append(parts, string(b))
					continue
				}
			}
			parts = append(parts, a.String())
		}
	}
	return truncateUTF8(strings.Join(parts, " "), 64<<10)
}

// ---- globals ------------------------------------------------------------------------------------------------------

func (e *scriptEnv) install() {
	vm := e.vm
	logFn := func(level string) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			e.p.logf(level, e.format(call.Arguments))
			return goja.Undefined()
		}
	}
	console := vm.NewObject()
	for _, l := range []string{"log", "info", "warn", "error", "debug"} {
		level := l
		if level == "log" {
			level = "info"
		}
		_ = console.Set(l, logFn(level))
	}
	_ = vm.Set("console", console)
	_ = vm.Set("log", logFn("info"))

	vars := vm.NewObject()
	for k, v := range e.p.vars {
		_ = vars.Set(k, v)
	}
	_ = vm.Set("vars", vars)
	_ = vm.Set("variables", vars)

	_ = vm.Set("sleep", func(call goja.FunctionCall) goja.Value {
		if err := sleepCtx(e.ctx, argMillis(call, 0, 0)); err != nil {
			e.fail(err)
		}
		return goja.Undefined()
	})
	_ = vm.Set("exit", func(call goja.FunctionCall) goja.Value {
		code := 0
		if v := call.Argument(0); !isNullish(v) {
			code = int(v.ToInteger())
		}
		vm.Interrupt(exitSignal{code: code})
		return goja.Undefined()
	})
	_ = vm.Set("prompt", e.prompt)
	_ = vm.Set("confirm", e.confirm)

	sessions := vm.NewObject()
	_ = sessions.Set("open", func(call goja.FunctionCall) goja.Value {
		target := argString(call, 0)
		if target == "" {
			e.throw("TypeError", "sessions.open() needs a connection id or name")
		}
		opts := e.optObject(call, 1)
		js, err := e.open(target, optMillis(opts, "timeout", 60*time.Second), optBool(opts, "keepOpen", false))
		if err != nil {
			e.fail(err)
		}
		return js.object()
	})
	_ = sessions.Set("get", func(call goja.FunctionCall) goja.Value {
		s, err := e.m.ownSession(e.user, argString(call, 0))
		if err != nil {
			e.throw("Error", "session not found")
		}
		return e.attach(s, false, false).object()
	})
	_ = sessions.Set("list", func(goja.FunctionCall) goja.Value {
		out := []any{}
		if mgr := e.m.sessions(); mgr != nil {
			for _, s := range mgr.List(e.user, false) {
				info := s.Info()
				if info.Kind != model.KindTerminal {
					continue
				}
				out = append(out, map[string]any{"id": info.ID, "title": info.Title, "host": info.Host, "user": info.Username,
					"protocol": string(info.Protocol), "state": string(info.State), "connectionId": info.ConnectionID})
			}
		}
		return vm.ToValue(out)
	})
	_ = vm.Set("sessions", sessions)

	connections := vm.NewObject()
	_ = connections.Set("list", func(goja.FunctionCall) goja.Value {
		list, err := e.m.d.Store.Connections.ListVisible(e.ctx, e.user.ID)
		if err != nil {
			e.fail(err)
		}
		out := make([]any, 0, len(list))
		for _, c := range list {
			tags := make([]any, 0, len(c.Tags))
			for _, t := range c.Tags {
				tags = append(tags, t)
			}
			out = append(out, map[string]any{"id": c.ID, "name": c.Name, "host": c.Host, "port": c.Port, "user": c.Username,
				"protocol": string(c.Protocol), "tags": tags, "folderId": c.FolderID})
		}
		return vm.ToValue(out)
	})
	_ = vm.Set("connections", connections)
}

func (e *scriptEnv) prompt(call goja.FunctionCall) goja.Value {
	label := argString(call, 0)
	if label == "" {
		label = "Value"
	}
	opts := e.optObject(call, 1)
	def := ""
	if opts != nil {
		if v := opts.Get("default"); !isNullish(v) {
			def = v.String()
		}
	}
	if e.m.d.Events == nil {
		return goja.Null()
	}
	resp, err := e.m.d.Events.Prompt(e.ctx, e.user.ID, model.Prompt{
		Kind:    model.PromptKeyboardInteractive,
		Title:   "Script: " + e.p.name,
		Message: truncateUTF8(label, 1000),
		Fields:  []model.PromptField{{Label: truncateUTF8(label, 200), Echo: !optBool(opts, "secret", false), Value: def}},
	})
	if err != nil {
		if e.ctx.Err() != nil {
			e.fail(e.ctx.Err())
		}
		e.p.logf("warn", "prompt(): "+errorText(err))
		return goja.Null()
	}
	if !resp.Accept || len(resp.Values) == 0 {
		return goja.Null()
	}
	return e.vm.ToValue(resp.Values[0])
}

func (e *scriptEnv) confirm(call goja.FunctionCall) goja.Value {
	msg := argString(call, 0)
	if e.m.d.Events == nil {
		return e.vm.ToValue(false)
	}
	resp, err := e.m.d.Events.Prompt(e.ctx, e.user.ID, model.Prompt{
		Kind: model.PromptConfirm, Title: "Script: " + e.p.name, Message: truncateUTF8(msg, 2000), Fields: []model.PromptField{},
	})
	if err != nil {
		if e.ctx.Err() != nil {
			e.fail(e.ctx.Err())
		}
		e.p.logf("warn", "confirm(): "+errorText(err))
		return e.vm.ToValue(false)
	}
	return e.vm.ToValue(resp.Accept)
}

// ---- sessions -----------------------------------------------------------------------------------------------------

type jsSession struct {
	env    *scriptEnv
	s      *term.Session
	t      *tap
	r      *reader
	opened bool
	keep   bool

	once sync.Once
	obj  *goja.Object
}

// attach binds a live session to the script (its tap starts at the current line, so a displayed prompt matches).
func (e *scriptEnv) attach(s *term.Session, opened, keep bool) *jsSession {
	e.mu.Lock()
	for _, js := range e.sess {
		if js.s == s {
			e.mu.Unlock()
			return js
		}
	}
	e.mu.Unlock()
	t := e.m.acquireTap(s)
	js := &jsSession{env: e, s: s, t: t, r: t.newReader(true), opened: opened, keep: keep}
	e.mu.Lock()
	e.sess = append(e.sess, js)
	e.mu.Unlock()
	return js
}

// open creates a session for a saved connection (by id or name) and waits until it is connected.
func (e *scriptEnv) open(target string, timeout time.Duration, keep bool) (*jsSession, error) {
	mgr := e.m.sessions()
	if mgr == nil {
		return nil, errors.New("sessions are not available")
	}
	e.mu.Lock()
	if e.opened >= maxScriptSessions {
		e.mu.Unlock()
		return nil, fmt.Errorf("a script may open at most %d sessions", maxScriptSessions)
	}
	e.opened++
	e.mu.Unlock()
	connID, err := e.m.findConnection(e.ctx, e.user, target)
	if err != nil {
		return nil, err
	}
	s, err := mgr.Create(e.ctx, e.user, term.CreateRequest{ConnectionID: connID, Cols: 160, Rows: 48,
		Title: "Script · " + e.p.name})
	if err != nil {
		return nil, err
	}
	js := e.attach(s, true, keep)
	if err := waitConnected(e.ctx, js.t, s, timeout); err != nil {
		return nil, err
	}
	return js, nil
}

// findConnection resolves a connection id or (case-insensitive) name visible to user.
func (m *Module) findConnection(ctx context.Context, user *model.User, target string) (string, error) {
	if model.ValidID(target) {
		if _, _, err := m.d.ResolveConnection(ctx, user, target); err == nil {
			return target, nil
		}
	}
	list, err := m.d.Store.Connections.ListVisible(ctx, user.ID)
	if err != nil {
		return "", err
	}
	var found []*model.Connection
	for _, c := range list {
		if strings.EqualFold(c.Name, target) {
			found = append(found, c)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no saved session named %q", target)
	case 1:
		return found[0].ID, nil
	}
	return "", fmt.Errorf("several saved sessions are named %q; use the id", target)
}

// waitConnected waits for a session created by automation to connect, reporting its state message on failure.
func waitConnected(ctx context.Context, t *tap, s *term.Session, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	err := t.waitState(ctx, timeout, func(st model.SessionState) bool { return st == model.StateConnected },
		func(st model.SessionState) bool { return st == model.StateError || st == model.StateDisconnected })
	if err == nil {
		return nil
	}
	if st, msg := s.State(); st == model.StateConnected {
		return nil
	} else if msg != "" && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("could not connect: %s", msg)
	}
	if errors.Is(err, errExpectTimeout) {
		return errors.New("could not connect: timed out")
	}
	return err
}

func (js *jsSession) release() {
	js.once.Do(func() {
		js.t.release()
		if js.opened && !js.keep {
			if mgr := js.env.m.sessions(); mgr != nil {
				_ = mgr.Close(js.s.ID)
			}
		}
	})
}

func (js *jsSession) write(text string) {
	if err := js.env.m.write(js.s.ID, text); err != nil {
		js.env.fail(err)
	}
}

func (js *jsSession) resultObject(res *ExpectResult) goja.Value {
	vm := js.env.vm
	o := vm.NewObject()
	groups := make([]any, 0, len(res.Groups))
	for _, g := range res.Groups {
		groups = append(groups, g)
	}
	_ = o.Set("index", res.Index)
	_ = o.Set("match", res.Match)
	_ = o.Set("groups", groups)
	_ = o.Set("before", res.Before)
	return o
}

func (js *jsSession) object() goja.Value {
	if js.obj != nil {
		return js.obj
	}
	e := js.env
	vm := e.vm
	info := js.s.Info()
	o := vm.NewObject()
	_ = o.Set("id", info.ID)
	_ = o.Set("title", info.Title)
	_ = o.Set("host", info.Host)
	_ = o.Set("user", info.Username)
	_ = o.Set("protocol", string(info.Protocol))
	_ = o.Set("connectionId", info.ConnectionID)

	_ = o.Set("send", func(call goja.FunctionCall) goja.Value {
		if text := argString(call, 0); text != "" {
			js.write(text)
		}
		return goja.Undefined()
	})
	_ = o.Set("sendLine", func(call goja.FunctionCall) goja.Value {
		js.write(argString(call, 0) + "\r")
		return goja.Undefined()
	})
	_ = o.Set("sendSecret", func(call goja.FunctionCall) goja.Value {
		key := argString(call, 0)
		v, err := e.m.sessionSecret(e.ctx, e.user, js.s, key)
		if err != nil {
			e.fail(err)
		}
		if err := e.m.writeSecret(js.s.ID, v, optBool(e.optObject(call, 1), "enter", true)); err != nil {
			e.fail(err)
		}
		return goja.Undefined()
	})
	expect := func(throw bool) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			res := e.patterns(call.Argument(0))
			r, err := js.r.expect(e.ctx, res, argMillis(call, 1, defaultExpectTimeout))
			if err != nil {
				if !throw && errors.Is(err, errExpectTimeout) {
					return goja.Null()
				}
				if errors.Is(err, errExpectTimeout) {
					e.throw("TimeoutError", "expect timed out waiting for "+patternList(res))
				}
				e.fail(err)
			}
			return js.resultObject(r)
		}
	}
	_ = o.Set("expect", expect(true))
	_ = o.Set("waitFor", expect(false))
	_ = o.Set("waitIdle", func(call goja.FunctionCall) goja.Value {
		if err := js.r.waitIdle(e.ctx, argMillis(call, 0, 500*time.Millisecond), argMillis(call, 1, 30*time.Second)); err != nil {
			e.fail(err)
		}
		return goja.Undefined()
	})
	_ = o.Set("waitPrompt", func(call goja.FunctionCall) goja.Value {
		re := defaultPromptRe
		if v := call.Argument(1); !isNullish(v) {
			re = e.pattern(v)
		}
		ok, err := js.r.waitPrompt(e.ctx, js.t.promptCount(), re, 200*time.Millisecond, argMillis(call, 0, 30*time.Second))
		if err != nil {
			e.fail(err)
		}
		return vm.ToValue(ok)
	})
	_ = o.Set("run", func(call goja.FunctionCall) goja.Value {
		cmd := argString(call, 0)
		opts := e.optObject(call, 1)
		re := defaultPromptRe
		if opts != nil {
			if v := opts.Get("prompt"); !isNullish(v) {
				re = e.pattern(v)
			}
		}
		out, err := js.run(cmd, re, optMillis(opts, "timeout", 60*time.Second))
		if err != nil {
			if errors.Is(err, errExpectTimeout) {
				e.throw("TimeoutError", "run(): the prompt did not come back in time")
			}
			e.fail(err)
		}
		return vm.ToValue(out)
	})
	_ = o.Set("screen", func(call goja.FunctionCall) goja.Value {
		n := 24
		if v := call.Argument(0); !isNullish(v) {
			n = min(max(int(v.ToInteger()), 1), 5000)
		}
		return vm.ToValue(js.t.screen(n))
	})
	_ = o.Set("exec", func(call goja.FunctionCall) goja.Value {
		stdout, stderr, code, err := js.exec(argString(call, 0), argMillis(call, 1, 60*time.Second))
		if err != nil {
			e.fail(err)
		}
		r := vm.NewObject()
		_ = r.Set("stdout", stdout)
		_ = r.Set("stderr", stderr)
		_ = r.Set("code", code)
		return r
	})
	_ = o.Set("close", func(goja.FunctionCall) goja.Value {
		if mgr := e.m.sessions(); mgr != nil {
			_ = mgr.Close(js.s.ID)
		}
		return goja.Undefined()
	})
	js.obj = o
	return o
}

func patternList(res []*regexp.Regexp) string {
	parts := make([]string, 0, len(res))
	for _, r := range res {
		parts = append(parts, "/"+r.String()+"/")
	}
	return strings.Join(parts, " or ")
}

// run types a command and returns its output once the prompt is back (echo and prompt lines removed).
func (js *jsSession) run(cmd string, promptRe *regexp.Regexp, timeout time.Duration) (string, error) {
	e := js.env
	seq := js.t.promptCount()
	mark := js.r.mark()
	if err := e.m.write(js.s.ID, cmd+"\r"); err != nil {
		return "", err
	}
	// Give the echo a moment so the prompt pattern does not match the line we just typed on.
	_ = sleepCtx(e.ctx, 50*time.Millisecond)
	ok, err := js.r.waitPrompt(e.ctx, seq, promptRe, 250*time.Millisecond, timeout)
	if err != nil {
		return "", err
	}
	out := js.r.textSince(mark)
	js.r.skip()
	if !ok {
		return cleanCommandOutput(out, cmd, promptRe), errExpectTimeout
	}
	return cleanCommandOutput(out, cmd, promptRe), nil
}

// cleanCommandOutput removes the echoed command line(s) and the trailing prompt line. A command typed before the
// shell printed its previous prompt (type-ahead) is echoed first and the late prompt lands in front of the output
// ("$ result"): that prompt is removed too.
func cleanCommandOutput(out, cmd string, promptRe *regexp.Regexp) string {
	lines := strings.Split(out, "\n")
	prompt := ""
	if len(lines) > 0 && promptRe != nil && promptRe.MatchString(lines[len(lines)-1]) {
		prompt = lines[len(lines)-1]
		lines = lines[:len(lines)-1]
	}
	cmdLines := splitLines(cmd)
	for len(lines) > 0 && len(cmdLines) > 0 {
		first := strings.TrimSpace(lines[0])
		want := strings.TrimSpace(cmdLines[0])
		if first == "" && want != "" {
			lines = lines[1:]
			continue
		}
		if want == "" || !strings.HasSuffix(first, want) {
			break
		}
		lines = lines[1:]
		cmdLines = cmdLines[1:]
	}
	if prompt != "" && len(lines) > 0 && strings.HasPrefix(lines[0], prompt) {
		lines[0] = lines[0][len(prompt):]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// exec runs a command on a separate SSH exec channel of the session's transport.
func (js *jsSession) exec(cmd string, timeout time.Duration) (string, string, int, error) {
	e := js.env
	if strings.TrimSpace(cmd) == "" {
		return "", "", -1, errors.New("exec() needs a command")
	}
	if e.m.c == nil || e.m.c.SSH == nil || js.s.Protocol != model.ProtoSSH {
		return "", "", -1, errors.New("exec() is only available for SSH sessions")
	}
	cl, release, err := e.m.c.SSH.ForSession(e.ctx, e.user, js.s.ID)
	if err != nil {
		return "", "", -1, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(e.ctx, timeout)
	defer cancel()
	so, se, code, err := cl.Exec(ctx, cmd)
	if err != nil {
		return string(so), string(se), code, err
	}
	return truncateUTF8(string(so), maxHostOutput), truncateUTF8(string(se), maxHostOutput), code, nil
}
