package rdp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/rdp/guac"
)

// guacdManager resolves the guacd address (admin setting rdp.guacdAddress, else --guacd), reports its status and
// manages the optional Docker sidecar.
type guacdManager struct {
	h       *handler
	sidecar *sidecar

	mu       sync.Mutex
	cache    *guacdStatus
	cacheAt  time.Time
	cacheFor string
	// The protocol version needs a handshake (guacd forks a process and logs an aborted handshake for each), so
	// it is probed rarely; reachability is a plain TCP connect.
	version    string
	versionFor string
	versionAt  time.Time
}

const guacdVersionTTL = 10 * time.Minute

func newGuacdManager(h *handler) *guacdManager {
	return &guacdManager{h: h, sidecar: newSidecar(h)}
}

const guacdStatusTTL = 3 * time.Second

// address returns the effective guacd address, or "" when guacd is disabled / not configured.
func (m *guacdManager) address(ctx context.Context) string {
	addr, _ := m.addressSource(ctx)
	return addr
}

func (m *guacdManager) addressSource(ctx context.Context) (string, string) {
	g := m.h.globalSettings(ctx)
	switch {
	case strings.EqualFold(g.GuacdAddress, "off") || strings.EqualFold(g.GuacdAddress, "none"):
		return "", "settings"
	case g.GuacdAddress != "":
		if validHostPort(g.GuacdAddress) {
			if g.GuacdSidecar {
				return g.GuacdAddress, "sidecar"
			}
			return g.GuacdAddress, "settings"
		}
		return "", "settings"
	}
	if m.h.d.Cfg != nil && m.h.d.Cfg.Guacd != "" && validHostPort(m.h.d.Cfg.Guacd) {
		return m.h.d.Cfg.Guacd, "flag"
	}
	return "", ""
}

func validHostPort(s string) bool {
	host, port, err := net.SplitHostPort(s)
	if err != nil || host == "" || len(host) > 255 {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n <= 65535
}

// dial opens a TCP connection to guacd.
func (m *guacdManager) dial(ctx context.Context) (net.Conn, error) {
	addr := m.address(ctx)
	if addr == "" {
		return nil, errGuacdUnavailable
	}
	d := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, "tcp", addr)
}

// probe is the "guacd" feature probe (GET /api/auth/state → features.guacd).
func (m *guacdManager) probe(ctx context.Context) bool {
	addr := m.address(ctx)
	if addr == "" {
		return false
	}
	dctx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(dctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// guacdStatus is the answer of GET /api/guacd/status (SPEC §6.0 Graphical: {configured, reachable, version?}; the
// other fields are extensions, the admin-only ones omitted for other users).
type guacdStatus struct {
	Configured    bool           `json:"configured"`
	Reachable     bool           `json:"reachable"`
	Version       string         `json:"version,omitempty"`
	DefaultEngine string         `json:"defaultEngine"`
	Address       string         `json:"address,omitempty"`
	Source        string         `json:"source,omitempty"`
	Error         string         `json:"error,omitempty"`
	Sidecar       *sidecarStatus `json:"sidecar,omitempty"`
}

func (m *guacdManager) status(ctx context.Context) guacdStatus {
	addr, source := m.addressSource(ctx)
	m.mu.Lock()
	if m.cache != nil && m.cacheFor == addr && time.Since(m.cacheAt) < guacdStatusTTL {
		st := *m.cache
		m.mu.Unlock()
		return st
	}
	m.mu.Unlock()

	g := m.h.globalSettings(ctx)
	st := guacdStatus{Configured: addr != "", Address: addr, Source: source, DefaultEngine: engineIronRDP}
	if g.DefaultEngine == engineGuacd {
		st.DefaultEngine = engineGuacd
	}
	if addr != "" {
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		c, err := m.dial(dctx)
		cancel()
		if err != nil {
			st.Error = guacdErrorText(err)
		} else {
			_ = c.Close()
			st.Reachable = true
			st.Version = m.protocolVersion(ctx, addr)
		}
	}
	m.mu.Lock()
	m.cache, m.cacheAt, m.cacheFor = &st, time.Now(), addr
	m.mu.Unlock()
	return st
}

// protocolVersion returns guacd's protocol version ("1.5.0"), probed at most every guacdVersionTTL per address.
func (m *guacdManager) protocolVersion(ctx context.Context, addr string) string {
	m.mu.Lock()
	if m.versionFor == addr && m.version != "" && time.Since(m.versionAt) < guacdVersionTTL {
		v := m.version
		m.mu.Unlock()
		return v
	}
	m.mu.Unlock()
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	version, _, err := guac.ProbeVersion(pctx, m.dial, "rdp")
	cancel()
	if err != nil {
		return ""
	}
	v := strings.ReplaceAll(strings.TrimPrefix(version, "VERSION_"), "_", ".")
	m.mu.Lock()
	m.version, m.versionFor, m.versionAt = v, addr, time.Now()
	m.mu.Unlock()
	return v
}

// invalidate forgets the cached status (after sidecar or settings changes).
func (m *guacdManager) invalidate() {
	m.mu.Lock()
	m.cache, m.version = nil, ""
	m.mu.Unlock()
}

func guacdErrorText(err error) string {
	var ge *guac.Error
	switch {
	case errors.As(err, &ge):
		return ge.Error()
	case errors.Is(err, guac.ErrNotReady):
		return "guacd closed the connection"
	case isTimeout(err):
		return "guacd did not answer"
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return "guacd is not reachable (" + rootMessage(op.Err) + ")"
	}
	return rootMessage(err)
}

// handleGuacdStatus serves GET /api/guacd/status. Administrators also get the address and the sidecar state.
func (h *handler) handleGuacdStatus(c *echo.Context) error {
	u := httpx.UserFrom(c)
	ctx := c.Request().Context()
	st := h.guacd.status(ctx)
	if u.IsAdmin() {
		sc := h.guacd.sidecar.status(ctx)
		st.Sidecar = &sc
	} else {
		st.Address, st.Source, st.Error = "", "", ""
	}
	return c.JSON(http.StatusOK, st)
}

// sidecarRequest is the body of POST /api/guacd/sidecar.
type sidecarRequest struct {
	Action string `json:"action"` // start | stop
}

// handleSidecar serves POST /api/guacd/sidecar {action}: starts or stops the guacamole/guacd container through the
// Docker CLI. The work runs as a job (progress streamed as job events); the answer is 202 {jobId}.
func (h *handler) handleSidecar(c *echo.Context) error {
	var req sidecarRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	if action != "start" && action != "stop" {
		return httpx.BadRequest(`action must be "start" or "stop"`)
	}
	if !h.guacd.sidecar.dockerInstalled() {
		return httpx.NewError(http.StatusConflict, "docker_unavailable", "the docker command is not installed on the NexTerm host")
	}
	u := httpx.UserFrom(c)
	if h.d.Jobs == nil {
		return httpx.Internal(errors.New("jobs are not available"))
	}
	h.auditUser(c.Request().Context(), u, "guacd.sidecar."+action, h.guacd.sidecar.cfg.Name, map[string]any{"image": h.guacd.sidecar.cfg.Image})
	jobID := h.d.Jobs.Start(u, "guacd-sidecar-"+action, func(ctx context.Context, emit func(any)) error {
		defer h.guacd.invalidate()
		if action == "start" {
			return h.guacd.sidecar.start(ctx, emit)
		}
		return h.guacd.sidecar.stop(ctx, emit)
	})
	return c.JSON(http.StatusAccepted, map[string]string{"jobId": jobID})
}
