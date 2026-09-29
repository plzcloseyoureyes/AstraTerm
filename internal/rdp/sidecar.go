package rdp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/termstead/termstead/internal/rdp/guac"
)

// The guacd sidecar (CORE-16): a guacamole/guacd container run through the local Docker CLI, published on the
// loopback interface only, whose address is then saved as the global guacd address. Containers are labelled so
// Termstead never touches a container it did not create.

const (
	sidecarLabel      = "termstead.managed"
	sidecarLabelValue = "guacd"
	sidecarImage      = "guacamole/guacd:1.6.0"
	sidecarName       = "termstead-guacd"
	sidecarPort       = 4822
	// sidecarDataPath is the directory inside the container used for virtual drives and recordings (writable by the
	// image's unprivileged guacd user).
	sidecarDataPath = "/tmp/termstead"
)

type sidecarConfig struct {
	Name   string
	Image  string
	BindIP string
	Port   int
}

type sidecar struct {
	h   *handler
	cfg sidecarConfig
	// docker is the docker executable ("docker" resolved through PATH by default; tests may override).
	docker string
	mu     sync.Mutex // serializes start/stop
}

func newSidecar(h *handler) *sidecar {
	return &sidecar{h: h, cfg: sidecarConfig{Name: sidecarName, Image: sidecarImage, BindIP: "127.0.0.1", Port: sidecarPort}, docker: "docker"}
}

func (s *sidecar) address() string {
	return net.JoinHostPort(s.cfg.BindIP, strconv.Itoa(s.cfg.Port))
}

func (s *sidecar) dockerInstalled() bool {
	_, err := exec.LookPath(s.docker)
	return err == nil
}

// sidecarStatus describes the sidecar container (admins only).
type sidecarStatus struct {
	DockerAvailable bool   `json:"dockerAvailable"`
	DockerVersion   string `json:"dockerVersion,omitempty"`
	Exists          bool   `json:"exists"`
	Running         bool   `json:"running"`
	Managed         bool   `json:"managed"` // the container carries Termstead's label
	Container       string `json:"container"`
	Image           string `json:"image"`
	Address         string `json:"address"`
	Active          bool   `json:"active"` // the global guacd address points to the sidecar
	Error           string `json:"error,omitempty"`
}

type containerInfo struct {
	exists  bool
	running bool
	managed bool
	image   string
}

func (s *sidecar) status(ctx context.Context) sidecarStatus {
	st := sidecarStatus{Container: s.cfg.Name, Image: s.cfg.Image, Address: s.address()}
	g := s.h.globalSettings(ctx)
	st.Active = g.GuacdSidecar && g.GuacdAddress == s.address()
	if !s.dockerInstalled() {
		st.Error = "the docker command is not installed"
		return st
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	v, err := s.run(ctx, nil, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		st.Error = "Docker is not running: " + err.Error()
		return st
	}
	st.DockerAvailable, st.DockerVersion = true, strings.TrimSpace(v)
	ci, err := s.inspect(ctx)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Exists, st.Running, st.Managed = ci.exists, ci.running, ci.managed
	return st
}

func (s *sidecar) inspect(ctx context.Context) (containerInfo, error) {
	out, err := s.run(ctx, nil, "container", "inspect", "--format",
		`{{.State.Running}}|{{index .Config.Labels "`+sidecarLabel+`"}}|{{.Config.Image}}`, s.cfg.Name)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such") {
			return containerInfo{}, nil
		}
		return containerInfo{}, err
	}
	parts := strings.SplitN(strings.TrimSpace(out), "|", 3)
	if len(parts) != 3 {
		return containerInfo{}, fmt.Errorf("unexpected docker inspect output %q", out)
	}
	return containerInfo{exists: true, running: parts[0] == "true", managed: parts[1] == sidecarLabelValue, image: parts[2]}, nil
}

// progress is the job event payload of sidecar operations.
type progress struct {
	Stage   string `json:"stage"`
	Message string `json:"message"`
}

// start creates (pulling the image when needed) or restarts the sidecar, waits until guacd answers and saves its
// address as the global guacd address.
func (s *sidecar) start(ctx context.Context, emit func(any)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	say := func(stage, msg string) { emit(progress{Stage: stage, Message: msg}) }

	say("check", "Checking Docker")
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err := s.run(vctx, nil, "version", "--format", "{{.Server.Version}}")
	cancel()
	if err != nil {
		return fmt.Errorf("Docker is not running: %w", err)
	}
	ci, err := s.inspect(ctx)
	if err != nil {
		return err
	}
	if ci.exists && !ci.managed {
		return fmt.Errorf("a container named %q already exists and was not created by Termstead", s.cfg.Name)
	}
	if ci.exists && ci.image != s.cfg.Image {
		say("remove", "Replacing the container of "+ci.image)
		if _, err := s.run(ctx, nil, "rm", "-f", s.cfg.Name); err != nil {
			return err
		}
		ci = containerInfo{}
	}
	switch {
	case ci.running:
		say("running", "The guacd container is already running")
	case ci.exists:
		say("start", "Starting the existing container")
		if _, err := s.run(ctx, nil, "start", s.cfg.Name); err != nil {
			return err
		}
	default:
		if _, err := s.run(ctx, nil, "image", "inspect", "--format", "{{.Id}}", s.cfg.Image); err != nil {
			say("pull", "Pulling "+s.cfg.Image)
			pctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
			err := s.stream(pctx, func(line string) { say("pull", line) }, "pull", s.cfg.Image)
			cancel()
			if err != nil {
				return fmt.Errorf("pull %s: %w", s.cfg.Image, err)
			}
		}
		say("create", "Creating the guacd container")
		args := []string{"run", "-d", "--name", s.cfg.Name,
			"--label", sidecarLabel + "=" + sidecarLabelValue,
			"--restart", "unless-stopped",
			"-p", net.JoinHostPort(s.cfg.BindIP, strconv.Itoa(s.cfg.Port)) + ":4822",
		}
		if runtime.GOOS == "linux" {
			// Lets guacd reach Termstead's loopback forwarders through the bridge gateway.
			args = append(args, "--add-host", "host.docker.internal:host-gateway")
		}
		args = append(args, s.cfg.Image)
		if _, err := s.run(ctx, nil, args...); err != nil {
			return err
		}
	}

	say("wait", "Waiting for guacd to answer")
	deadline := time.Now().Add(45 * time.Second)
	for {
		dial := func(ctx context.Context) (net.Conn, error) {
			d := net.Dialer{Timeout: 2 * time.Second}
			return d.DialContext(ctx, "tcp", s.address())
		}
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		version, _, err := guac.ProbeVersion(pctx, dial, "rdp")
		cancel()
		if err == nil {
			say("ready", "guacd is ready ("+strings.ReplaceAll(strings.TrimPrefix(version, "VERSION_"), "_", ".")+" protocol)")
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("guacd did not become ready at %s: %s", s.address(), guacdErrorText(err))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}

	patch := map[string]any{"guacdAddress": s.address(), "guacdSidecar": true, "guacdDataPath": sidecarDataPath}
	if g := s.h.globalSettings(ctx); g.GuacdForwardHost == "" {
		// Connections routed through SSH gateways / proxies reach guacd through a forwarder on the host: Docker
		// Desktop maps host.docker.internal to the host's loopback; on Linux the container reaches the host through
		// the bridge gateway, where the forwarder then listens.
		fwd := "host.docker.internal"
		if runtime.GOOS == "linux" {
			gctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			out, err := s.run(gctx, nil, "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}")
			cancel()
			if ip := net.ParseIP(strings.TrimSpace(out)); err == nil && ip != nil {
				fwd = ip.String()
			}
		}
		patch["guacdForwardHost"] = fwd
	}
	if err := s.h.updateGlobalSettings(context.WithoutCancel(ctx), patch); err != nil {
		return fmt.Errorf("save the guacd address: %w", err)
	}
	say("saved", "guacd address set to "+s.address())
	return nil
}

// stop stops the sidecar container (it is kept for a fast restart). The global address stays until changed.
func (s *sidecar) stop(ctx context.Context, emit func(any)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ci, err := s.inspect(ctx)
	if err != nil {
		return err
	}
	if !ci.exists {
		emit(progress{Stage: "stopped", Message: "The guacd container does not exist"})
		return nil
	}
	if !ci.managed {
		return fmt.Errorf("the container %q was not created by Termstead", s.cfg.Name)
	}
	if ci.running {
		emit(progress{Stage: "stop", Message: "Stopping the guacd container"})
		if _, err := s.run(ctx, nil, "stop", "-t", "5", s.cfg.Name); err != nil {
			return err
		}
	}
	emit(progress{Stage: "stopped", Message: "The guacd container is stopped"})
	return nil
}

// remove deletes the sidecar container (tests).
func (s *sidecar) remove(ctx context.Context) error {
	ci, err := s.inspect(ctx)
	if err != nil || !ci.exists {
		return err
	}
	if !ci.managed {
		return fmt.Errorf("the container %q was not created by Termstead", s.cfg.Name)
	}
	_, err = s.run(ctx, nil, "rm", "-f", s.cfg.Name)
	return err
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// run executes a docker command and returns its stdout; errors carry the (trimmed) stderr.
func (s *sidecar) run(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, s.docker, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(ansiRe.ReplaceAllString(stderr.String(), ""))
		if msg == "" {
			msg = err.Error()
		}
		if len(msg) > 500 {
			msg = msg[:500]
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			msg = "docker " + args[0] + " timed out"
		}
		return stdout.String(), errors.New(msg)
	}
	return stdout.String(), nil
}

// stream runs a docker command and reports its output lines (pull progress).
func (s *sidecar) stream(ctx context.Context, line func(string), args ...string) error {
	cmd := exec.CommandContext(ctx, s.docker, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	last := time.Time{}
	for sc.Scan() {
		t := strings.TrimSpace(ansiRe.ReplaceAllString(sc.Text(), ""))
		if t == "" || time.Since(last) < 300*time.Millisecond {
			continue
		}
		last = time.Now()
		line(t)
	}
	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return errors.New(msg)
	}
	return nil
}
