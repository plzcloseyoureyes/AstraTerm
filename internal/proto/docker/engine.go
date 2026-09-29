package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// engine is a minimal Docker Engine API client (RESEARCH §3.16). It talks HTTP over a caller-supplied transport
// (unix socket, Windows named pipe, TCP, or an SSH-forwarded socket) and supports the container list, exec
// create/start(hijack)/resize, logs streaming and start/stop/restart actions Termstead needs. Unversioned API paths
// are used so the daemon maps them to its current version (Docker and Podman both accept this).
type engine struct {
	client *http.Client
	tr     *http.Transport
	// dial opens a fresh raw connection to the daemon, used for the hijacked exec-attach stream.
	dial   func(ctx context.Context) (net.Conn, error)
	closer func() // releases any held resource (e.g. an SSH client) when the engine is done
	once   sync.Once
}

// close releases resources held by the engine: idle API connections and, for a remote engine, the SSH client.
func (e *engine) close() {
	e.once.Do(func() {
		if e.tr != nil {
			e.tr.CloseIdleConnections()
		}
		if e.closer != nil {
			e.closer()
		}
	})
}

// apiTimeout bounds short API calls made outside a request context (resize, exit status).
const apiTimeout = 10 * time.Second

// dialer produces raw connections to a docker daemon endpoint.
type dialer func(ctx context.Context) (net.Conn, error)

// newEngineDialer resolves options.dockerHost (or the default socket) into a raw dialer plus a human label.
func newEngineDialer(host string) (dialer, string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		host = os.Getenv("DOCKER_HOST")
	}
	if host == "" {
		if runtime.GOOS == "windows" {
			host = defaultWindowsEngine
		} else {
			p := defaultSocketPath()
			if p == "" {
				return nil, "", errors.New("no Docker engine found on this host (set a Docker host or connect through SSH)")
			}
			host = "unix://" + p
		}
	}
	switch {
	case strings.HasPrefix(host, "unix://"):
		path := strings.TrimPrefix(host, "unix://")
		return func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}, "unix://" + path, nil
	case strings.HasPrefix(host, "npipe://"):
		pipe := strings.TrimPrefix(host, "npipe://")
		pipe = strings.ReplaceAll(pipe, "/", `\`)
		d, err := npipeDialer(pipe)
		if err != nil {
			return nil, "", err
		}
		return d, "npipe://" + pipe, nil
	case strings.HasPrefix(host, "tcp://"), strings.HasPrefix(host, "http://"):
		addr := host
		addr = strings.TrimPrefix(addr, "tcp://")
		addr = strings.TrimPrefix(addr, "http://")
		addr = strings.TrimRight(addr, "/")
		return func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}, "tcp://" + addr, nil
	default:
		return nil, "", fmt.Errorf("unsupported Docker host %q (use unix://, npipe:// or tcp://)", host)
	}
}

// newEngine builds an engine from a raw dialer.
func newEngine(dial dialer) *engine {
	tr := &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		DisableCompression:  true,
		MaxIdleConns:        4,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &engine{
		client: &http.Client{Transport: tr, Timeout: 0},
		tr:     tr,
		dial:   dial,
	}
}

// defaultWindowsEngine is Docker Desktop's engine pipe.
const defaultWindowsEngine = "npipe:////./pipe/docker_engine"

// defaultSocketPath finds the local engine socket (Docker, Colima, OrbStack, Rancher Desktop, rootless).
func defaultSocketPath() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	candidates := []string{"/var/run/docker.sock", "/run/docker.sock"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".docker", "run", "docker.sock"),
			filepath.Join(home, ".colima", "default", "docker.sock"),
			filepath.Join(home, ".orbstack", "run", "docker.sock"),
			filepath.Join(home, ".rd", "docker.sock"))
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "docker.sock"))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// ---- API types ----------------------------------------------------------------------------------------------------

// Container is one entry of the container list (a subset of the Docker API response).
type Container struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Image  string `json:"image"`
	State  string `json:"state"`
	Status string `json:"status"`
}

type apiContainer struct {
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	Image  string   `json:"Image"`
	State  string   `json:"State"`
	Status string   `json:"Status"`
}

type apiInspect struct {
	Config struct {
		Tty bool `json:"Tty"`
	} `json:"Config"`
}

// ---- API operations -----------------------------------------------------------------------------------------------

const apiBase = "http://docker"

func (e *engine) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, friendlyDialError(err)
	}
	return resp, nil
}

func friendlyDialError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such file"):
		return errors.New("cannot reach the Docker engine (is it running and is the socket accessible?)")
	case strings.Contains(msg, "permission denied"):
		return errors.New("permission denied on the Docker socket (the user needs to be in the docker group)")
	case strings.Contains(msg, "open failed") || strings.Contains(msg, "administratively prohibited"):
		return errors.New("the SSH server refused to open the Docker socket (is Docker running there, is the SSH user in the docker group, and is stream-local forwarding allowed?)")
	}
	return err
}

func decodeError(resp *http.Response) error {
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var m struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &m)
	if m.Message != "" {
		return fmt.Errorf("docker: %s (%d)", m.Message, resp.StatusCode)
	}
	return fmt.Errorf("docker: HTTP %d", resp.StatusCode)
}

func (e *engine) listContainers(ctx context.Context, all bool) ([]Container, error) {
	q := url.Values{}
	if all {
		q.Set("all", "1")
	}
	resp, err := e.do(ctx, http.MethodGet, "/containers/json?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	defer resp.Body.Close()
	var raw []apiContainer
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(raw))
	for _, c := range raw {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, Container{ID: shortID(c.ID), Name: name, Image: c.Image, State: c.State, Status: c.Status})
	}
	return out, nil
}

func (e *engine) inspect(ctx context.Context, id string) (*apiInspect, error) {
	resp, err := e.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	defer resp.Body.Close()
	var ins apiInspect
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&ins); err != nil {
		return nil, err
	}
	return &ins, nil
}

func (e *engine) containerAction(ctx context.Context, id, action string) error {
	path := "/containers/" + url.PathEscape(id) + "/" + action
	resp, err := e.do(ctx, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 204 = success, 304 = already in the desired state.
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return nil
	}
	return decodeError(resp)
}

type execConfig struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Cmd          []string `json:"Cmd"`
	User         string   `json:"User,omitempty"`
	Env          []string `json:"Env,omitempty"`
}

func (e *engine) execCreate(ctx context.Context, id string, cfg execConfig) (string, error) {
	resp, err := e.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/exec", cfg)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", decodeError(resp)
	}
	defer resp.Body.Close()
	var r struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if r.ID == "" {
		return "", errors.New("docker: exec create returned no id")
	}
	return r.ID, nil
}

// execStart starts an exec instance and hijacks the connection, returning the raw bidirectional stream.
func (e *engine) execStart(ctx context.Context, execID string, tty bool) (net.Conn, *bufio.Reader, error) {
	conn, err := e.dial(ctx)
	if err != nil {
		return nil, nil, friendlyDialError(err)
	}
	body, _ := json.Marshal(map[string]any{"Detach": false, "Tty": tty})
	req := "POST /exec/" + url.PathEscape(execID) + "/start HTTP/1.1\r\n" +
		"Host: docker\r\n" +
		"Content-Type: application/json\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: tcp\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
	if _, err := conn.Write(append([]byte(req), body...)); err != nil {
		conn.Close()
		return nil, nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("docker: exec start: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		defer conn.Close()
		return nil, nil, decodeError(resp)
	}
	// After the (possibly 200) response headers, br + conn carry the raw stream.
	return conn, br, nil
}

func (e *engine) execResize(ctx context.Context, execID string, h, w int) error {
	q := url.Values{}
	q.Set("h", strconv.Itoa(h))
	q.Set("w", strconv.Itoa(w))
	resp, err := e.do(ctx, http.MethodPost, "/exec/"+url.PathEscape(execID)+"/resize?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return decodeError(resp)
	}
	return nil
}

// execExitCode returns the exit status of a finished exec (-1 when unknown). The attach stream can end a moment
// before the daemon records the exit, so a still-running exec is polled briefly.
func (e *engine) execExitCode(ctx context.Context, execID string) int {
	for attempt := 0; attempt < 10; attempt++ {
		code, running, ok := e.execInspect(ctx, execID)
		if !ok {
			return -1
		}
		if !running {
			return code
		}
		select {
		case <-ctx.Done():
			return -1
		case <-time.After(100 * time.Millisecond):
		}
	}
	return -1
}

func (e *engine) execInspect(ctx context.Context, execID string) (code int, running, ok bool) {
	resp, err := e.do(ctx, http.MethodGet, "/exec/"+url.PathEscape(execID)+"/json", nil)
	if err != nil {
		return -1, false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return -1, false, false
	}
	var r struct {
		ExitCode *int `json:"ExitCode"`
		Running  bool `json:"Running"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return -1, false, false
	}
	if r.Running {
		return -1, true, true
	}
	if r.ExitCode == nil {
		return -1, false, true
	}
	return *r.ExitCode, false, true
}

// logs opens a follow stream of a container's logs, returning the response body (caller closes it) and whether the
// stream is TTY-raw (no stdcopy multiplexing).
func (e *engine) logs(ctx context.Context, id string, tail int) (io.ReadCloser, bool, error) {
	ins, err := e.inspect(ctx, id)
	if err != nil {
		return nil, false, err
	}
	q := url.Values{}
	q.Set("follow", "1")
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	q.Set("tail", strconv.Itoa(max(tail, 0)))
	resp, err := e.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs?"+q.Encode(), nil)
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, decodeError(resp)
	}
	return resp.Body, ins.Config.Tty, nil
}

// ---- stdcopy demultiplexing --------------------------------------------------------------------------------------

// demuxReader converts Docker's multiplexed stdout/stderr stream (8-byte header frames) into a single stream, with
// stderr wrapped in a red SGR so it stands out in the terminal.
type demuxReader struct {
	src     *bufio.Reader
	pending []byte
}

func newDemuxReader(r io.Reader) *demuxReader { return &demuxReader{src: bufio.NewReader(r)} }

func (d *demuxReader) Read(p []byte) (int, error) {
	if len(d.pending) > 0 {
		n := copy(p, d.pending)
		d.pending = d.pending[n:]
		return n, nil
	}
	var hdr [8]byte
	if _, err := io.ReadFull(d.src, hdr[:]); err != nil {
		return 0, err
	}
	streamType := hdr[0]
	size := int(binary.BigEndian.Uint32(hdr[4:]))
	if size < 0 || size > 16<<20 {
		return 0, errors.New("docker: invalid log frame size")
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(d.src, buf); err != nil {
		return 0, err
	}
	if streamType == 2 { // stderr
		out := append([]byte("\x1b[31m"), buf...)
		out = append(out, []byte("\x1b[0m")...)
		buf = out
	}
	n := copy(p, buf)
	if n < len(buf) {
		d.pending = append(d.pending, buf[n:]...)
	}
	return n, nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
