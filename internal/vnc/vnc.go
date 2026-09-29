// Package vnc implements AstraTerm's VNC session type (RESEARCH PROTO-17, GFX-1..5, GFX-17, GFX-18, §3.12; SPEC §6.3).
//
// A VNC runtime session (kind "vnc", created by POST /api/sessions) has no terminal backend: every browser viewer
// opens WS /ws/vnc/{sessionId} and AstraTerm connects to the VNC server for it — directly or through the generic sshx
// Dialer (options.sshTunnelVia, jumpHosts, proxy) — terminating RFB security in Go: RFB 3.3/3.7/3.8, None, VNC
// Authentication, Apple Remote Desktop and VeNCrypt 0.2 (Plain, TLSNone/TLSVnc/TLSPlain over anonymous TLS,
// X509None/X509Vnc/X509Plain over crypto/tls with TOFU of the certificate fingerprint). noVNC is then offered RFB 3.8
// with security type None, so the password never reaches the browser; types only noVNC implements (RA2ne, Tight, XVP,
// MS-Logon II) are passed through. Passwords come from the vault (secrets.vncPassword, else secrets.password) or are
// prompted through the broker (allowSave for the owner's saved connections).
//
// Extra endpoints (SPEC §9 "vnc"):
//
//	GET    /api/sessions/:id/vnc-info   negotiated security, TLS details, desktop name/size, route
//	GET    /api/vnc/listen              reverse-connection listeners (GFX-17)
//	POST   /api/vnc/listen              {port, bindHost?, password?, viewOnly?} → listener
//	DELETE /api/vnc/listen/:id
//	GET    /api/vnc/certs               trusted VeNCrypt certificates (TOFU store)
//	DELETE /api/vnc/certs/:id
//	WS     /ws/vnc/:id                  binary RFB stream for noVNC (no subprotocol required; "binary" accepted)
package vnc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/core"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// Module is the VNC feature's runtime state.
type Module struct {
	d     *app.Deps
	c     *core.Core
	log   *slog.Logger
	certs *certStore

	mu        sync.Mutex
	sessions  map[string]*sessState
	listeners map[string]*listener
	reverse   map[string]*pendingReverse // parked incoming (reverse) connections by session ID
	closing   bool
}

// sessState is the module's view of one VNC runtime session.
type sessState struct {
	viewers   map[*viewer]struct{}
	connected int
	info      *Info             // last established connection
	accepted  map[string]string // host:port → certificate fingerprint accepted once in this session
	lastError string
	confirm   *ConfirmInfo     // the last viewer stopped for the user's confirmation (close 4426)
	consent   encryptionPolicy // the owner's confirmation, remembered for the session (zero: none)
}

// Info describes a session's VNC connection (GET /api/sessions/:id/vnc-info).
type Info struct {
	SessionID     string     `json:"sessionId"`
	Connected     bool       `json:"connected"`
	Viewers       int        `json:"viewers"`
	Host          string     `json:"host,omitempty"`
	Port          int        `json:"port,omitempty"`
	ServerVersion string     `json:"serverVersion,omitempty"`
	Protocol      string     `json:"protocol,omitempty"` // negotiated RFB version, e.g. "3.8"
	Security      string     `json:"security,omitempty"`
	Encrypted     bool       `json:"encrypted"`
	TLS           *tlsInfo   `json:"tls,omitempty"`
	Passthrough   bool       `json:"passthrough"`
	DesktopName   string     `json:"desktopName,omitempty"`
	Width         int        `json:"width,omitempty"`
	Height        int        `json:"height,omitempty"`
	Route         string     `json:"route,omitempty"`
	Reverse       bool       `json:"reverse"`
	ConnectedAt   *time.Time `json:"connectedAt,omitempty"`
	LastError     string     `json:"lastError,omitempty"`

	// EncryptionPolicy is the effective options.encryption policy of the connection (require, prefer, allow-weak,
	// allow-unencrypted; see policy.go), including the user's confirmations for this session.
	EncryptionPolicy string `json:"encryptionPolicy,omitempty"`
	// Downgrade explains why the connection has less protection than the server offered (e.g. anonymous TLS failed
	// and the user allowed an unencrypted connection).
	Downgrade string `json:"downgrade,omitempty"`
	// PasswordCleartext: the login (VeNCrypt Plain without TLS) crossed the network in clear text.
	PasswordCleartext bool `json:"passwordCleartext,omitempty"`
	// Confirm is set while the session waits for the user to confirm a weaker connection (WS close 4426).
	Confirm *ConfirmInfo `json:"confirm,omitempty"`
	// Clipboard is the effective clipboard direction: both, to-remote, from-remote or none.
	Clipboard string `json:"clipboard,omitempty"`
}

// Mount registers the VNC endpoints and session hooks.
func Mount(d *app.Deps, c *core.Core) error {
	if c == nil || c.Sessions == nil || c.SSH == nil {
		return errors.New("vnc: session manager and SSH pool are required")
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	m := &Module{
		d:         d,
		c:         c,
		log:       log.With("module", "vnc"),
		certs:     &certStore{db: d.Store.DB},
		sessions:  map[string]*sessState{},
		listeners: map[string]*listener{},
		reverse:   map[string]*pendingReverse{},
	}
	api := d.Router.API()
	api.GET("/sessions/:id/vnc-info", m.handleInfo)
	api.GET("/vnc/listen", m.handleListListeners)
	api.POST("/vnc/listen", m.handleStartListener)
	api.DELETE("/vnc/listen/:id", m.handleStopListener)
	api.GET("/vnc/certs", m.handleListCerts)
	api.DELETE("/vnc/certs/:id", m.handleDeleteCert)
	d.Router.WS("/ws/vnc/:id", m.handleWS)
	c.Sessions.AddHooks(term.Hooks{OnClose: m.onSessionClosed})
	if d.Ctx != nil {
		go func() {
			<-d.Ctx.Done()
			m.shutdown()
		}()
	}
	return nil
}

// ---- session bookkeeping ------------------------------------------------------------------------------------------

// attach registers a viewer; false when the module is shutting down.
func (m *Module) attach(v *viewer) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return false
	}
	st := m.stateLocked(v.s.ID)
	st.viewers[v] = struct{}{}
	return true
}

func (m *Module) stateLocked(id string) *sessState {
	st := m.sessions[id]
	if st == nil {
		st = &sessState{viewers: map[*viewer]struct{}{}, accepted: map[string]string{}}
		m.sessions[id] = st
	}
	return st
}

// report updates the session state for a viewer still establishing its connection: connecting / authenticating
// messages are only shown while no other viewer of the session is connected.
func (m *Module) report(v *viewer, st model.SessionState, msg string) {
	m.mu.Lock()
	s := m.sessions[v.s.ID]
	busy := s != nil && s.connected > 0
	m.mu.Unlock()
	if !busy {
		m.c.Sessions.SetState(v.s.ID, st, msg)
	}
}

// connected marks a viewer as established.
func (m *Module) connected(v *viewer, info *Info) {
	m.mu.Lock()
	st := m.stateLocked(v.s.ID)
	st.connected++
	now := time.Now().UTC()
	info.ConnectedAt = &now
	st.info = info
	st.lastError = ""
	st.confirm = nil
	m.mu.Unlock()
	msg := ""
	if info.Passthrough {
		msg = "Authentication handled by the viewer"
	}
	m.c.Sessions.SetState(v.s.ID, model.StateConnected, msg)
}

// detach unregisters a viewer and derives the session state from the remaining ones.
func (m *Module) detach(v *viewer, wasConnected bool, endState model.SessionState, endMsg string) {
	m.mu.Lock()
	st := m.sessions[v.s.ID]
	remaining, stillConnected := 0, 0
	if st != nil {
		delete(st.viewers, v)
		if wasConnected && st.connected > 0 {
			st.connected--
		}
		remaining, stillConnected = len(st.viewers), st.connected
		if endState == model.StateError {
			st.lastError = endMsg
		}
		if remaining == 0 && v.s.Closed() {
			delete(m.sessions, v.s.ID) // attached after the session closed: drop the stale entry
		}
	}
	m.mu.Unlock()
	if st == nil || stillConnected > 0 || v.s.Closed() {
		return
	}
	if remaining > 0 && endState != model.StateError {
		return // another viewer is still connecting and reports its own progress
	}
	m.c.Sessions.SetState(v.s.ID, endState, endMsg)
}

// setConfirm records that a viewer stopped for the user's confirmation.
func (m *Module) setConfirm(sessionID string, c *ConfirmInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.sessions[sessionID]; st != nil {
		st.confirm = c
	}
}

// rememberConsent keeps the owner's confirmation of a weaker connection for the rest of the session.
func (m *Module) rememberConsent(sessionID string, p encryptionPolicy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.sessions[sessionID]; st != nil && p > st.consent {
		st.consent = p
	}
}

func (m *Module) rememberedConsent(sessionID string) encryptionPolicy {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.sessions[sessionID]; st != nil {
		return st.consent
	}
	return encPrefer
}

func (m *Module) acceptedCert(sessionID, hostport string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.sessions[sessionID]; st != nil {
		return st.accepted[hostport]
	}
	return ""
}

func (m *Module) rememberCert(sessionID, hostport, fp string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stateLocked(sessionID).accepted[hostport] = fp
}

// onSessionClosed ends every viewer of a closed VNC session (DELETE /api/sessions/:id, reaper, shutdown). It runs
// synchronously inside the session manager's close, so the viewers' disconnect audit entries are written before the
// manager reports its shutdown complete (and the database closes).
func (m *Module) onSessionClosed(s *term.Session) {
	if s.Kind != model.KindVNC {
		return
	}
	m.mu.Lock()
	st := m.sessions[s.ID]
	delete(m.sessions, s.ID)
	pr := m.reverse[s.ID]
	delete(m.reverse, s.ID)
	var viewers []*viewer
	if st != nil {
		for v := range st.viewers {
			viewers = append(viewers, v)
		}
	}
	m.mu.Unlock()
	if pr != nil {
		pr.discard()
	}
	for _, v := range viewers {
		v.sessionClosed()
	}
}

func (m *Module) shutdown() {
	m.mu.Lock()
	m.closing = true
	ls := make([]*listener, 0, len(m.listeners))
	for _, l := range m.listeners {
		ls = append(ls, l)
	}
	m.mu.Unlock()
	for _, l := range ls {
		m.stopListener(l, "server shutting down")
	}
}

func (m *Module) audit(ctx context.Context, user *model.User, action, target string, details any) {
	if m.d.Audit == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.d.Audit.LogUser(context.WithoutCancel(ctx), user, action, target, details)
}

// ---- GET /api/sessions/:id/vnc-info -------------------------------------------------------------------------------

func (m *Module) lookupSession(c *echo.Context) (*term.Session, error) {
	user := httpx.UserFrom(c)
	id := c.Param("id")
	s := m.c.Sessions.Get(id)
	if s == nil || user == nil || (s.OwnerID != user.ID && !user.IsAdmin()) {
		return nil, httpx.ErrNotFound
	}
	if s.Kind != model.KindVNC {
		return nil, httpx.BadRequest("not a VNC session")
	}
	return s, nil
}

func (m *Module) handleInfo(c *echo.Context) error {
	s, err := m.lookupSession(c)
	if err != nil {
		return err
	}
	conn := s.Connection()
	info := Info{SessionID: s.ID}
	m.mu.Lock()
	if st := m.sessions[s.ID]; st != nil {
		if st.info != nil {
			info = *st.info
		}
		info.Viewers = len(st.viewers)
		info.Connected = st.connected > 0
		info.LastError = st.lastError
		info.Confirm = st.confirm
		if info.EncryptionPolicy == "" && conn != nil {
			info.EncryptionPolicy = effectivePolicy(connectionPolicy(conn.Options), st.consent).String()
		}
	}
	m.mu.Unlock()
	if conn != nil {
		info.Host, info.Port = conn.Host, portOf(conn)
		info.Reverse = conn.Options.Bool("reverse")
		if info.Route == "" {
			info.Route = describeRoute(conn)
		}
		if info.EncryptionPolicy == "" {
			info.EncryptionPolicy = connectionPolicy(conn.Options).String()
		}
		if info.Clipboard == "" {
			info.Clipboard = m.clipboardPolicy(c.Request().Context(), conn).String()
		}
	}
	info.SessionID = s.ID
	return c.JSON(http.StatusOK, info)
}

func portOf(c *model.Connection) int {
	if c.Port > 0 {
		return c.Port
	}
	return model.DefaultPort(model.ProtoVNC)
}

// describeRoute summarizes how the server is reached (for the UI).
func describeRoute(c *model.Connection) string {
	o := c.Options
	if o.Bool("reverse") {
		return "incoming connection"
	}
	if v := strings.TrimSpace(o.String("sshTunnelVia", "")); v != "" {
		return "SSH gateway"
	}
	if hops := o.Strings("jumpHosts"); len(hops) > 0 {
		return fmt.Sprintf("SSH jump hosts (%d)", len(hops))
	}
	if cmd := strings.TrimSpace(o.String("proxyCommand", "")); cmd != "" {
		return "ProxyCommand"
	}
	var p struct {
		Type string `json:"type"`
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if err := o.Decode("proxy", &p); err == nil && p.Type != "" && p.Type != "none" {
		return p.Type + " proxy " + net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
	}
	return "direct"
}
