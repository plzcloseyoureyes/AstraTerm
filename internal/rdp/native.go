package rdp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// CC-15 native client launch (desktop mode only: the client opens on the machine running AstraTerm). Connections that
// need AstraTerm's gateway routing (SSH gateway, jump hosts, proxy) are launched against a loopback forwarder. The
// password is injected where the client allows it without writing it to disk: Windows Credential Manager (cmdkey,
// removed again after the client started) and FreeRDP's stdin.

const (
	nativeFileTTL      = 2 * time.Minute
	nativeForwardIdle  = 2 * time.Minute
	cmdkeyRemoveDelay  = 30 * time.Second
	nativeMaxForwarder = 64
)

type nativeLauncher struct {
	h  *handler
	mu sync.Mutex
	// forwarders serving launched clients (closed after they stay idle, or on shutdown).
	forwarders map[*forwarder]struct{}
	// lookPath / start are replaced in tests.
	lookPath func(string) (string, error)
	start    func(cmd *exec.Cmd) error
	goos     string
}

func newNativeLauncher(h *handler) *nativeLauncher {
	return &nativeLauncher{h: h, forwarders: map[*forwarder]struct{}{}, lookPath: exec.LookPath,
		start: func(cmd *exec.Cmd) error { return cmd.Start() }, goos: runtime.GOOS}
}

func (n *nativeLauncher) closeAll() {
	n.mu.Lock()
	list := make([]*forwarder, 0, len(n.forwarders))
	for f := range n.forwarders {
		list = append(list, f)
	}
	n.mu.Unlock()
	for _, f := range list {
		f.Close()
	}
}

// launchResult is the answer of the launch endpoints.
type launchResult struct {
	Client           string `json:"client"`
	Forwarded        string `json:"forwarded,omitempty"`
	PasswordInjected bool   `json:"passwordInjected"`
}

var errDesktopOnly = httpx.NewError(http.StatusForbidden, "desktop_only",
	"native clients can only be launched in desktop mode (AstraTerm runs on this computer)")

// handleLaunchConnection serves POST /api/connections/{id}/launch-native.
func (h *handler) handleLaunchConnection(c *echo.Context) error {
	if !h.isDesktop() {
		return errDesktopOnly
	}
	u := httpx.UserFrom(c)
	ctx := c.Request().Context()
	if _, err := h.visibleConnection(ctx, u, c.Param("id")); err != nil {
		return err
	}
	conn, secrets, err := h.d.ResolveConnection(ctx, u, c.Param("id"))
	if err != nil {
		return err
	}
	res, err := h.native.launch(ctx, u, conn, secrets, nil)
	if err != nil {
		return err
	}
	h.auditUser(ctx, u, "rdp.native.launch", conn.ID, map[string]any{"client": res.Client, "host": conn.Host})
	return c.JSON(http.StatusOK, res)
}

// handleLaunchSession serves POST /api/sessions/{id}/launch-native (also for quick-connect sessions).
func (h *handler) handleLaunchSession(c *echo.Context) error {
	if !h.isDesktop() {
		return errDesktopOnly
	}
	s, u, err := h.session(c, false)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	conn, secrets, err := s.Resolve(ctx)
	if err != nil {
		return err
	}
	res, err := h.native.launch(ctx, u, conn, secrets, s)
	if err != nil {
		return err
	}
	h.auditUser(ctx, u, "rdp.native.launch", s.ID, map[string]any{"client": res.Client, "host": conn.Host})
	return c.JSON(http.StatusOK, res)
}

// nativeClient is a detected RDP client.
type nativeClient struct {
	name string
	path string
	kind string // mstsc | open | freerdp | remmina
}

func (n *nativeLauncher) detect() (nativeClient, error) {
	switch n.goos {
	case "windows":
		if p, err := n.lookPath("mstsc.exe"); err == nil {
			return nativeClient{name: "Remote Desktop Connection (mstsc)", path: p, kind: "mstsc"}, nil
		}
		return nativeClient{}, errors.New("mstsc.exe was not found")
	case "darwin":
		home, _ := os.UserHomeDir()
		for _, app := range []string{"Windows App", "Microsoft Remote Desktop"} {
			for _, dir := range []string{"/Applications", filepath.Join(home, "Applications")} {
				p := filepath.Join(dir, app+".app")
				if st, err := os.Stat(p); err == nil && st.IsDir() {
					return nativeClient{name: app, path: p, kind: "open"}, nil
				}
			}
		}
	}
	for _, name := range []string{"xfreerdp3", "xfreerdp", "sdl-freerdp3", "wlfreerdp"} {
		if p, err := n.lookPath(name); err == nil {
			return nativeClient{name: "FreeRDP (" + name + ")", path: p, kind: "freerdp"}, nil
		}
	}
	if p, err := n.lookPath("remmina"); err == nil {
		return nativeClient{name: "Remmina", path: p, kind: "remmina"}, nil
	}
	if n.goos == "darwin" {
		return nativeClient{}, errors.New(`no RDP client found: install "Windows App" from the App Store`)
	}
	return nativeClient{}, errors.New("no RDP client found: install FreeRDP (xfreerdp) or Remmina")
}

func (n *nativeLauncher) launch(ctx context.Context, u *model.User, conn *model.Connection, secrets map[string]string, sess *term.Session) (*launchResult, error) {
	h := n.h
	client, err := n.detect()
	if err != nil {
		return nil, httpx.NewError(http.StatusConflict, "no_native_client", err.Error())
	}
	p := fileParams(conn)
	if strings.TrimSpace(p.Host) == "" {
		return nil, httpx.BadRequest("the connection has no host")
	}
	res := &launchResult{Client: client.name}
	if routeDescription(conn) != "" {
		n.mu.Lock()
		count := len(n.forwarders)
		n.mu.Unlock()
		if count >= nativeMaxForwarder {
			return nil, httpx.TooManyRequests("too many native sessions are being forwarded", 60)
		}
		target := conn.Clone()
		target.Port = p.Port
		f, err := h.startForwarder(h.ctx, "127.0.0.1", u, target, secrets, sess, 0, nativeForwardIdle)
		if err != nil {
			return nil, fmt.Errorf("start the gateway forwarder: %w", err)
		}
		n.track(f)
		p.Host, p.Port = "127.0.0.1", f.Addr().Port
		res.Forwarded = net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
	}
	password := secrets[model.SecretPassword]
	switch client.kind {
	case "mstsc", "open", "remmina":
		file, err := n.writeTempFile(buildRDPFile(p))
		if err != nil {
			return nil, err
		}
		var cmd *exec.Cmd
		switch client.kind {
		case "mstsc":
			if password != "" && p.Username != "" {
				res.PasswordInjected = n.cmdkeyAdd(p, password)
			}
			cmd = exec.Command(client.path, file)
		case "open":
			cmd = exec.Command("open", "-a", client.path, file)
		default:
			cmd = exec.Command(client.path, "-c", file)
		}
		if err := n.run(cmd); err != nil {
			return nil, fmt.Errorf("start %s: %w", client.name, err)
		}
	case "freerdp":
		args := []string{"/v:" + net.JoinHostPort(p.Host, strconv.Itoa(p.Port)), "/cert:tofu", "+clipboard", "/dynamic-resolution"}
		if p.Username != "" {
			args = append(args, "/u:"+p.Username)
		}
		if p.Domain != "" {
			args = append(args, "/d:"+p.Domain)
		}
		if p.Opts.Console {
			args = append(args, "/admin")
		}
		if p.Opts.GatewayHost != "" {
			args = append(args, "/g:"+p.Opts.GatewayHost)
		}
		cmd := exec.Command(client.path, args...)
		if password != "" {
			// /from-stdin:force reads the password from stdin: it never appears in the process list or on disk.
			cmd.Args = append(cmd.Args, "/from-stdin:force")
			cmd.Stdin = strings.NewReader(password + "\n")
			res.PasswordInjected = true
		}
		if err := n.run(cmd); err != nil {
			return nil, fmt.Errorf("start %s: %w", client.name, err)
		}
	}
	return res, nil
}

func (n *nativeLauncher) track(f *forwarder) {
	n.mu.Lock()
	n.forwarders[f] = struct{}{}
	n.mu.Unlock()
	go func() {
		<-f.Done()
		n.mu.Lock()
		delete(n.forwarders, f)
		n.mu.Unlock()
	}()
}

// run starts a detached client process and reaps it in the background.
func (n *nativeLauncher) run(cmd *exec.Cmd) error {
	if err := n.start(cmd); err != nil {
		return err
	}
	if cmd.Process != nil {
		go func() { _ = cmd.Wait() }()
	}
	return nil
}

// writeTempFile writes an .rdp file readable only by the current user and removes it after nativeFileTTL.
func (n *nativeLauncher) writeTempFile(data []byte) (string, error) {
	dir := os.TempDir()
	if cfg := n.h.d.Cfg; cfg != nil && cfg.DataDir != "" {
		dir = cfg.TmpDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	path := filepath.Join(dir, "astraterm-"+hex.EncodeToString(rnd[:])+".rdp")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	time.AfterFunc(nativeFileTTL, func() { _ = os.Remove(path) })
	return path, nil
}

// cmdkeyAdd stores the credentials for mstsc in the Windows Credential Manager and removes them again after the
// client had time to read them.
func (n *nativeLauncher) cmdkeyAdd(p rdpFileParams, password string) bool {
	cmdkey, err := n.lookPath("cmdkey.exe")
	if err != nil {
		return false
	}
	target := "TERMSRV/" + p.Host
	user := p.Username
	if p.Domain != "" && !strings.ContainsAny(user, `\@`) {
		user = p.Domain + `\` + user
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, cmdkey, "/generic:"+target, "/user:"+user, "/pass:"+password).Run(); err != nil {
		n.h.log.Debug("rdp: cmdkey failed", "err", err)
		return false
	}
	time.AfterFunc(cmdkeyRemoveDelay, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, cmdkey, "/delete:"+target).Run()
	})
	return true
}
