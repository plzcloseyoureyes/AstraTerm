package automation

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dop251/goja"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/model"
)

type logCollector struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCollector) logf(level, text string) {
	l.mu.Lock()
	l.lines = append(l.lines, level+":"+text)
	l.mu.Unlock()
}

func (l *logCollector) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func bareModule() *Module {
	return &Module{log: slog.New(slog.NewTextHandler(io.Discard, nil)), d: &app.Deps{}, scripts: newScriptLimiter(),
		taps: newTapHub(), ctx: context.Background()}
}

func runJS(t *testing.T, src string, vars map[string]string, timeout time.Duration) (string, error) {
	t.Helper()
	prg, err := compileScript("test", src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	var lc logCollector
	err = bareModule().execScript(context.Background(), &model.User{ID: "u"}, scriptParams{name: "test", program: prg, vars: vars,
		timeout: timeout, logf: lc.logf})
	return lc.joined(), err
}

func TestScriptBasics(t *testing.T) {
	out, err := runJS(t, `
		log("hello", vars.who, 1+1, {a: [1, 2]}, null, undefined);
		console.warn("careful");
		if (session !== null) throw new Error("no session expected");
		var n = 0; for (var i = 0; i < 1000; i++) n += i;
		console.log("sum=" + n);
		sleep(10);
	`, map[string]string{"who": "world"}, 5*time.Second)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{`info:hello world 2 {"a":[1,2]} null undefined`, "warn:careful", "info:sum=499500"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log %q missing %q", out, want)
		}
	}
}

func TestScriptErrorsAndExit(t *testing.T) {
	if _, err := runJS(t, `log("a"); exit(0); log("not reached")`, nil, time.Second); err != nil {
		t.Fatalf("exit(0): %v", err)
	}
	out, err := runJS(t, `log("a"); exit(3); log("not reached")`, nil, time.Second)
	if err == nil || !strings.Contains(err.Error(), "code 3") || strings.Contains(out, "not reached") {
		t.Fatalf("exit(3): %v %q", err, out)
	}
	_, err = runJS(t, `throw new Error("boom")`, nil, time.Second)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("throw: %v", err)
	}
	start := time.Now()
	_, err = runJS(t, `while (true) {}`, nil, 150*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout: %v after %s", err, time.Since(start))
	}
	// A caught interruption still stops the script.
	_, err = runJS(t, `try { while (true) {} } catch (e) { log("caught") } log("after")`, nil, 100*time.Millisecond)
	if err == nil {
		t.Fatalf("interrupt must not be catchable")
	}
	_, err = runJS(t, `function f(){ return f() } f()`, nil, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "stack") {
		t.Fatalf("recursion: %v", err)
	}
	if _, err := compileScript("x", "function ("); err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("syntax error: %v", err)
	}
	// No ambient capabilities.
	out, err = runJS(t, `log(typeof require, typeof setTimeout, typeof process, typeof fetch)`, nil, time.Second)
	if err != nil || !strings.Contains(out, "undefined undefined undefined undefined") {
		t.Fatalf("sandbox globals: %q %v", out, err)
	}
}

func TestScriptCancel(t *testing.T) {
	prg, _ := compileScript("t", `sleep(60000)`)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := bareModule().execScript(ctx, &model.User{ID: "u"}, scriptParams{name: "t", program: prg, timeout: time.Minute})
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("cancel: %v after %s", err, time.Since(start))
	}
}

func TestScriptPatterns(t *testing.T) {
	vm := goja.New()
	env := &scriptEnv{m: bareModule(), ctx: context.Background(), vm: vm}
	v, _ := vm.RunString(`/pass(word)?:\s*$/im`)
	if re := env.pattern(v); re.String() != `(?im)pass(word)?:\s*$` || !re.MatchString("line\nPassword: ") {
		t.Fatalf("regexp literal: %s", re)
	}
	if re := env.pattern(vm.ToValue(`\$ $`)); re.String() != `\$ $` {
		t.Fatalf("string pattern: %s", re)
	}
	arr, _ := vm.RunString(`[/a/, "b"]`)
	if res := env.patterns(arr); len(res) != 2 {
		t.Fatalf("pattern array: %v", res)
	}
	// Unsupported syntax becomes a JS SyntaxError.
	bad, _ := vm.RunString(`/(?=x)/`)
	func() {
		defer func() {
			r := recover()
			obj, ok := r.(*goja.Object)
			if !ok || obj.Get("name").String() != "SyntaxError" {
				t.Fatalf("look-ahead should throw a SyntaxError, got %v", r)
			}
		}()
		env.pattern(bad)
	}()
}

func TestLogBufferTruncation(t *testing.T) {
	var l logBuffer
	big := strings.Repeat("x", 1000)
	for i := 0; i < 400; i++ {
		l.add(time.Now(), "info", "h", big)
	}
	s := l.String()
	if len(s) > maxRunLog+2000 || !strings.HasPrefix(s, "…(earlier output truncated)") {
		t.Fatalf("log buffer: %d bytes, prefix %q", len(s), s[:40])
	}
}
