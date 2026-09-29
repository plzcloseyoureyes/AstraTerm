// Package kube implements the "kube" terminal protocol and Kubernetes pickers (PROTO-31, RESEARCH §3.17) by driving
// the host's kubectl binary inside a local PTY: `kubectl exec -it` with a bash→sh fallback, or `kubectl logs -f`
// (options.kubeMode="logs"). Contexts, namespaces and pods are listed with `kubectl config view` / `kubectl get`.
// kubectl runs on the Termstead host with the host's kubeconfig, so in server mode it is restricted to administrators.
//
// Endpoints:
//
//	GET /api/kube/contexts                       list kubeconfig contexts
//	GET /api/kube/namespaces?context=            list namespaces
//	GET /api/kube/pods?context=&namespace=       list pods in a namespace
package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/xpty"
	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/core"
	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/term"
)

// Mount registers the "kube" protocol, its policy and the context/pod pickers.
func Mount(d *app.Deps, c *core.Core) error {
	m := &module{d: d}
	term.RegisterPolicy(string(model.ProtoKube), func(_ context.Context, user *model.User, _ *model.Connection) error {
		return m.allowed(user)
	})
	term.RegisterProtocol(string(model.ProtoKube), func(ctx context.Context, req term.OpenRequest) (term.Backend, error) {
		if err := m.allowed(req.User); err != nil {
			return nil, term.Permanent(err)
		}
		return m.open(ctx, req)
	})
	// Probe with the same lookup the sessions use (GUI launches often lack Homebrew & co. in PATH).
	app.RegisterFeature("kubectl", func(context.Context) bool {
		_, err := kubectlPath()
		return err == nil
	})
	api := d.Router.API()
	api.GET("/kube/contexts", m.handleContexts)
	api.GET("/kube/namespaces", m.handleNamespaces)
	api.GET("/kube/pods", m.handlePods)
	return nil
}

type module struct{ d *app.Deps }

func (m *module) allowed(user *model.User) error {
	if user == nil {
		return httpx.ErrUnauthorized
	}
	if m.d.Cfg != nil && m.d.Cfg.IsServer() && !user.IsAdmin() {
		return httpx.Forbidden("Kubernetes sessions are only available to administrators in server mode")
	}
	return nil
}

// kubectlPath finds kubectl in PATH, then in the usual install locations (a desktop app started from the Dock or a
// service manager does not see the login shell's PATH).
func kubectlPath() (string, error) {
	if p, err := exec.LookPath("kubectl"); err == nil {
		return p, nil
	}
	for _, p := range kubectlCandidates() {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && (runtime.GOOS == "windows" || fi.Mode()&0o111 != 0) {
			return p, nil
		}
	}
	return "", errors.New("kubectl was not found on the Termstead host (install it or add it to PATH)")
}

func kubectlCandidates() []string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		var out []string
		for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("LOCALAPPDATA")} {
			if base != "" {
				out = append(out,
					filepath.Join(base, "Docker", "Docker", "resources", "bin", "kubectl.exe"),
					filepath.Join(base, "Microsoft", "WinGet", "Links", "kubectl.exe"))
			}
		}
		if home != "" {
			out = append(out, filepath.Join(home, "scoop", "shims", "kubectl.exe"), filepath.Join(home, ".rd", "bin", "kubectl.exe"))
		}
		return out
	}
	out := []string{"/opt/homebrew/bin/kubectl", "/usr/local/bin/kubectl", "/snap/bin/kubectl", "/usr/bin/kubectl"}
	if home != "" {
		out = append(out, filepath.Join(home, ".rd", "bin", "kubectl"), filepath.Join(home, ".local", "bin", "kubectl"),
			filepath.Join(home, "bin", "kubectl"), filepath.Join(home, ".orbstack", "bin", "kubectl"))
	}
	return out
}

// ---- pickers ------------------------------------------------------------------------------------------------------

// KubeContext is one entry of GET /api/kube/contexts.
type KubeContext struct {
	Name      string `json:"name"`
	Cluster   string `json:"cluster,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Current   bool   `json:"current"`
}

// KubePod is one entry of GET /api/kube/pods.
type KubePod struct {
	Name       string   `json:"name"`
	Namespace  string   `json:"namespace"`
	Status     string   `json:"status,omitempty"`
	Containers []string `json:"containers"`
}

func (m *module) handleContexts(c *echo.Context) error {
	if err := m.allowed(httpx.UserFrom(c)); err != nil {
		return err
	}
	kubectl, err := kubectlPath()
	if err != nil {
		return httpx.BadRequest(err.Error())
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Second)
	defer cancel()
	out, err := runKubectl(ctx, kubectl, "config", "view", "-o", "json")
	if err != nil {
		return httpx.BadRequest("kubectl config view failed: " + err.Error())
	}
	var cfg struct {
		CurrentContext string `json:"current-context"`
		Contexts       []struct {
			Name    string `json:"name"`
			Context struct {
				Cluster   string `json:"cluster"`
				Namespace string `json:"namespace"`
			} `json:"context"`
		} `json:"contexts"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return httpx.BadRequest("cannot parse kubectl output")
	}
	list := make([]KubeContext, 0, len(cfg.Contexts))
	for _, ct := range cfg.Contexts {
		list = append(list, KubeContext{
			Name:      ct.Name,
			Cluster:   ct.Context.Cluster,
			Namespace: ct.Context.Namespace,
			Current:   ct.Name == cfg.CurrentContext,
		})
	}
	return c.JSON(http.StatusOK, list)
}

// KubeNamespace is one entry of GET /api/kube/namespaces.
type KubeNamespace struct {
	Name   string `json:"name"`
	Status string `json:"status,omitempty"`
}

func (m *module) handleNamespaces(c *echo.Context) error {
	if err := m.allowed(httpx.UserFrom(c)); err != nil {
		return err
	}
	kubectl, err := kubectlPath()
	if err != nil {
		return httpx.BadRequest(err.Error())
	}
	kctx := c.QueryParam("context")
	if err := validateName(kctx); err != nil {
		return httpx.BadRequest("invalid context")
	}
	args := []string{"get", "namespaces", "-o", "json", "--request-timeout=10s"}
	if kctx != "" {
		args = append(args, "--context="+kctx)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 12*time.Second)
	defer cancel()
	out, err := runKubectl(ctx, kubectl, args...)
	if err != nil {
		return httpx.BadRequest("kubectl get namespaces failed: " + err.Error())
	}
	var resp struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return httpx.BadRequest("cannot parse kubectl output")
	}
	list := make([]KubeNamespace, 0, len(resp.Items))
	for _, it := range resp.Items {
		list = append(list, KubeNamespace{Name: it.Metadata.Name, Status: it.Status.Phase})
	}
	return c.JSON(http.StatusOK, list)
}

func (m *module) handlePods(c *echo.Context) error {
	if err := m.allowed(httpx.UserFrom(c)); err != nil {
		return err
	}
	kubectl, err := kubectlPath()
	if err != nil {
		return httpx.BadRequest(err.Error())
	}
	kctx := c.QueryParam("context")
	ns := c.QueryParam("namespace")
	if err := validateName(kctx); err != nil {
		return httpx.BadRequest("invalid context")
	}
	if err := validateName(ns); err != nil {
		return httpx.BadRequest("invalid namespace")
	}
	args := []string{"get", "pods", "-o", "json", "--request-timeout=10s"}
	if kctx != "" {
		args = append(args, "--context="+kctx)
	}
	if ns != "" {
		args = append(args, "--namespace="+ns)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 12*time.Second)
	defer cancel()
	out, err := runKubectl(ctx, kubectl, args...)
	if err != nil {
		return httpx.BadRequest("kubectl get pods failed: " + err.Error())
	}
	var resp struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
			Spec struct {
				Containers []struct {
					Name string `json:"name"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return httpx.BadRequest("cannot parse kubectl output")
	}
	pods := make([]KubePod, 0, len(resp.Items))
	for _, it := range resp.Items {
		names := make([]string, 0, len(it.Spec.Containers))
		for _, ct := range it.Spec.Containers {
			names = append(names, ct.Name)
		}
		pods = append(pods, KubePod{Name: it.Metadata.Name, Namespace: it.Metadata.Namespace, Status: it.Status.Phase, Containers: names})
	}
	return c.JSON(http.StatusOK, pods)
}

func runKubectl(ctx context.Context, kubectl string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, kubectl, args...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, errors.New(strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

// ---- opener -------------------------------------------------------------------------------------------------------

func (m *module) open(_ context.Context, req term.OpenRequest) (term.Backend, error) {
	conn := req.Connection
	if conn == nil {
		return nil, term.Permanent(errors.New("kube: missing connection"))
	}
	kubectl, err := kubectlPath()
	if err != nil {
		return nil, term.Permanent(err)
	}
	o := conn.Options
	pod := strings.TrimSpace(o.String("pod", ""))
	if pod == "" {
		return nil, term.Permanent(errors.New("kube: no pod configured"))
	}
	kctx := strings.TrimSpace(o.String("context", ""))
	ns := strings.TrimSpace(o.String("namespace", ""))
	container := strings.TrimSpace(o.String("container", ""))
	for _, v := range []string{pod, kctx, ns, container} {
		if err := validateName(v); err != nil {
			return nil, term.Permanent(fmt.Errorf("kube: invalid argument %q", v))
		}
	}

	args := kubectlArgs(pod, kctx, ns, container, o)
	logsMode := strings.EqualFold(o.String("kubeMode", ""), "logs")

	cols, rows := 80, 24
	if req.Session != nil {
		cols, rows = req.Session.Size()
	}
	p, err := xpty.NewPty(cols, rows)
	if err != nil {
		return nil, fmt.Errorf("kube: create pseudo-terminal: %w", err)
	}
	cmd := exec.Command(kubectl, args...)
	configureCmd(cmd)
	if err := p.Start(cmd); err != nil {
		p.Close()
		return nil, fmt.Errorf("kube: start kubectl: %w", err)
	}
	b := &backend{pty: p, cmd: cmd, done: make(chan struct{}), logs: logsMode}
	b.code.Store(-1)
	go b.wait()
	return b, nil
}

// kubectlArgs builds the kubectl command line. Values are passed as single arguments (no shell) in --flag=value form
// and validated by validateName, so nothing can be read as an extra flag.
func kubectlArgs(pod, kctx, ns, container string, o model.Options) []string {
	var args []string
	if kctx != "" {
		args = append(args, "--context="+kctx)
	}
	if ns != "" {
		args = append(args, "--namespace="+ns)
	}
	if strings.EqualFold(o.String("kubeMode", ""), "logs") {
		args = append(args, "logs", "--follow", "--tail="+strconv.Itoa(min(max(o.Int("logTail", 200), 0), 100000)), pod)
		if container != "" {
			args = append(args, "--container="+container)
		}
		return args
	}
	args = append(args, "exec", "-it", pod)
	if container != "" {
		args = append(args, "--container="+container)
	}
	args = append(args, "--")
	switch shell := strings.TrimSpace(o.String("shell", "")); {
	case shell == "":
		args = append(args, "/bin/sh", "-c", "command -v bash >/dev/null 2>&1 && exec bash || exec sh")
	case strings.ContainsAny(shell, " \t"):
		args = append(args, "/bin/sh", "-c", shell) // a command line, run inside the container
	default:
		args = append(args, shell)
	}
	return args
}

// validateName rejects empty-safe kubectl arguments that could be interpreted as flags or contain shell/control
// characters (kubectl is exec'd without a shell, so this only guards against flag injection and stray control bytes).
func validateName(s string) error {
	if s == "" {
		return nil
	}
	if strings.HasPrefix(s, "-") {
		return errors.New("must not start with '-'")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return errors.New("control character")
		}
	}
	return nil
}

// ---- backend ------------------------------------------------------------------------------------------------------

type backend struct {
	pty  xpty.Pty
	cmd  *exec.Cmd
	logs bool

	closeOnce sync.Once
	ptyOnce   sync.Once
	done      chan struct{}
	code      atomic.Int64
}

const drainGrace = 300 * time.Millisecond

func (b *backend) wait() {
	err := xpty.WaitProcess(context.Background(), b.cmd)
	b.code.Store(int64(exitCode(b.cmd, err)))
	close(b.done)
	time.Sleep(drainGrace)
	b.closePty()
}

func (b *backend) closePty() { b.ptyOnce.Do(func() { _ = b.pty.Close() }) }

func (b *backend) Read(p []byte) (int, error) {
	n, err := b.pty.Read(p)
	if err == nil {
		return n, nil
	}
	select {
	case <-b.done:
	case <-time.After(2 * time.Second):
	}
	return n, io.EOF
}

func (b *backend) Write(p []byte) (int, error) {
	if b.logs {
		return len(p), nil // logs view is read-only
	}
	return b.pty.Write(p)
}

func (b *backend) Resize(cols, rows int) error { return b.pty.Resize(cols, rows) }

func (b *backend) ExitCode() int { return int(b.code.Load()) }

func (b *backend) Close() error {
	b.closeOnce.Do(func() {
		select {
		case <-b.done:
		default:
			killCmd(b.cmd)
			go func() {
				select {
				case <-b.done:
				case <-time.After(3 * time.Second):
					if b.cmd.Process != nil {
						_ = b.cmd.Process.Kill()
					}
				}
			}()
		}
		b.closePty()
	})
	return nil
}
