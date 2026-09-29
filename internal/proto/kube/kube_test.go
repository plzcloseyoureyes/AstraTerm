package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"", "default", "arn:aws:eks:eu-west-1:1:cluster/prod", "my_ctx@user"} {
		if err := validateName(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"--all-namespaces", "-n", "bad\x00name", "a\nb"} {
		if err := validateName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestKubectlArgs(t *testing.T) {
	got := kubectlArgs("web-0", "prod", "shop", "app", model.Options{})
	want := []string{"--context=prod", "--namespace=shop", "exec", "-it", "web-0", "--container=app", "--", "/bin/sh", "-c", "command -v bash >/dev/null 2>&1 && exec bash || exec sh"}
	if !slices.Equal(got, want) {
		t.Fatalf("exec args = %q", got)
	}
	got = kubectlArgs("web-0", "", "", "", model.Options{"kubeMode": "logs", "logTail": 50})
	if !slices.Equal(got, []string{"logs", "--follow", "--tail=50", "web-0"}) {
		t.Fatalf("logs args = %q", got)
	}
	got = kubectlArgs("p", "", "", "", model.Options{"shell": "bash -l"})
	if !slices.Equal(got[len(got)-3:], []string{"/bin/sh", "-c", "bash -l"}) {
		t.Fatalf("shell command line = %q", got)
	}
}

// fakeKubectl installs a kubectl stand-in first in PATH. It answers the list commands with canned JSON, records its
// arguments, and for exec behaves like an interactive shell on its tty.
func fakeKubectl(t *testing.T) (argsFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script kubectl stand-in needs a unix shell")
	}
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := `#!/bin/sh
printf '%s\n' "$@" > "` + argsFile + `"
case "$*" in
  "config view -o json")
    echo '{"current-context":"kind","contexts":[{"name":"kind","context":{"cluster":"kind","namespace":"dev"}},{"name":"prod","context":{"cluster":"eks"}}]}' ;;
  "get namespaces"*)
    echo '{"items":[{"metadata":{"name":"default"},"status":{"phase":"Active"}},{"metadata":{"name":"dev"},"status":{"phase":"Active"}}]}' ;;
  "get pods"*)
    echo '{"items":[{"metadata":{"name":"web-0","namespace":"dev"},"status":{"phase":"Running"},"spec":{"containers":[{"name":"app"},{"name":"sidecar"}]}}]}' ;;
  *" exec -it "*)
    printf 'TTY=%s\r\n' "$(test -t 0 && echo yes || echo no)"
    IFS= read -r line
    printf 'GOT:%s\r\n' "$line"
    exit 7 ;;
  *" logs "*|"logs "*)
    printf 'log line 1\r\nlog line 2\r\n' ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if p, err := exec.LookPath("kubectl"); err != nil || filepath.Dir(p) != dir {
		t.Fatalf("fake kubectl not first in PATH: %s %v", p, err)
	}
	return argsFile
}

func call(t *testing.T, h echo.HandlerFunc, target string) (int, []byte) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req = req.WithContext(httpx.WithUser(req.Context(), &model.User{ID: "u1", Role: model.RoleAdmin}))
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h(c); err != nil {
		var he *httpx.HTTPError
		if errors.As(err, &he) {
			return he.Status, []byte(he.Error())
		}
		t.Fatalf("handler error: %v", err)
	}
	return rec.Code, rec.Body.Bytes()
}

func TestPickersWithFakeKubectl(t *testing.T) {
	argsFile := fakeKubectl(t)
	m := &module{d: &app.Deps{}}

	code, body := call(t, m.handleContexts, "/api/kube/contexts")
	var ctxs []KubeContext
	if code != 200 || json.Unmarshal(body, &ctxs) != nil || len(ctxs) != 2 || !ctxs[0].Current || ctxs[0].Namespace != "dev" {
		t.Fatalf("contexts %d %s", code, body)
	}

	code, body = call(t, m.handleNamespaces, "/api/kube/namespaces?context=prod")
	var nss []KubeNamespace
	if code != 200 || json.Unmarshal(body, &nss) != nil || len(nss) != 2 || nss[1].Name != "dev" {
		t.Fatalf("namespaces %d %s", code, body)
	}
	if args, _ := os.ReadFile(argsFile); !strings.Contains(string(args), "--context=prod") {
		t.Fatalf("namespaces args: %q", args)
	}

	code, body = call(t, m.handlePods, "/api/kube/pods?context=kind&namespace=dev")
	var pods []KubePod
	if code != 200 || json.Unmarshal(body, &pods) != nil || len(pods) != 1 || len(pods[0].Containers) != 2 {
		t.Fatalf("pods %d %s", code, body)
	}
	if args, _ := os.ReadFile(argsFile); !strings.Contains(string(args), "--namespace=dev") {
		t.Fatalf("pods args: %q", args)
	}

	if code, _ := call(t, m.handlePods, "/api/kube/pods?namespace=--all-namespaces"); code != http.StatusBadRequest {
		t.Fatalf("flag injection accepted: %d", code)
	}
}

func TestExecSessionWithFakeKubectl(t *testing.T) {
	argsFile := fakeKubectl(t)
	m := &module{d: &app.Deps{}}
	conn := &model.Connection{Protocol: model.ProtoKube, Options: model.Options{"pod": "web-0", "namespace": "dev", "container": "app"}}
	be, err := m.open(context.Background(), term.OpenRequest{Connection: conn, User: &model.User{ID: "u", Role: model.RoleAdmin}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer be.Close()
	col := newCollector(be)
	if out := col.until(5*time.Second, func(b []byte) bool { return bytes.Contains(b, []byte("TTY=")) }); !bytes.Contains(out, []byte("TTY=yes")) {
		t.Fatalf("kubectl did not get a tty: %q", out)
	}
	if _, err := be.Write([]byte("hello\r")); err != nil {
		t.Fatal(err)
	}
	if out := col.until(5*time.Second, func(b []byte) bool { return bytes.Contains(b, []byte("GOT:")) }); !bytes.Contains(out, []byte("GOT:hello")) {
		t.Fatalf("input not delivered: %q", out)
	}
	col.until(5*time.Second, func([]byte) bool { return false })
	if err := col.error(); err != io.EOF {
		t.Fatalf("end = %v, want EOF", err)
	}
	if ec := be.(term.ExitCoder).ExitCode(); ec != 7 {
		t.Fatalf("exit code = %d, want 7", ec)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--namespace=dev\nexec\n-it\nweb-0\n--container=app\n--\n") {
		t.Fatalf("exec args: %q", args)
	}
}

func TestOpenMissingKubectl(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := kubectlPath(); err == nil {
		t.Skip("kubectl found in a standard location; cannot test the missing-binary path")
	}
	m := &module{d: &app.Deps{}}
	_, err := m.open(context.Background(), term.OpenRequest{
		Connection: &model.Connection{Protocol: model.ProtoKube, Options: model.Options{"pod": "web"}},
		User:       &model.User{ID: "u", Role: model.RoleAdmin},
	})
	if err == nil || !term.IsPermanent(err) {
		t.Fatalf("missing kubectl: %v", err)
	}
}

type collector struct {
	mu  sync.Mutex
	buf []byte
	err error
}

func newCollector(r io.Reader) *collector {
	c := &collector{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			c.mu.Lock()
			c.buf = append(c.buf, buf[:n]...)
			if err != nil {
				c.err = err
			}
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return c
}

func (c *collector) until(d time.Duration, pred func([]byte) bool) []byte {
	deadline := time.Now().Add(d)
	for {
		c.mu.Lock()
		if pred(c.buf) || c.err != nil || time.Now().After(deadline) {
			out := c.buf
			c.buf = nil
			c.mu.Unlock()
			return out
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
}

func (c *collector) error() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}
