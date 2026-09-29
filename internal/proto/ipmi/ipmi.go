// Package ipmi implements the "ipmi" terminal protocol (CC-2): an IPMI v2.0 (RMCP+) Serial-over-LAN console using
// github.com/bougou/go-ipmi, streamed to xterm, plus chassis power control (status/on/off/cycle/reset/soft) over a
// REST endpoint. SOL is UDP/RMCP+ and cannot be proxied, so it connects directly to the BMC.
//
// Endpoints:
//
//	POST /api/ipmi/:connectionId/power {action}   status | on | off | cycle | reset | soft
package ipmi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	goipmi "github.com/bougou/go-ipmi/pkg/client"
	"github.com/bougou/go-ipmi/pkg/command/chassis"
	"github.com/bougou/go-ipmi/pkg/command/transport"
	"github.com/bougou/go-ipmi/pkg/types"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/core"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// Mount registers the "ipmi" terminal protocol and the power-control endpoint.
func Mount(d *app.Deps, c *core.Core) error {
	m := &module{d: d, c: c}
	term.RegisterProtocol(string(model.ProtoIPMI), func(ctx context.Context, req term.OpenRequest) (term.Backend, error) {
		return m.open(ctx, req)
	})
	d.Router.API().POST("/ipmi/:connectionId/power", m.handlePower)
	return nil
}

type module struct {
	d *app.Deps
	c *core.Core
}

// ipmiParams holds the connection parameters extracted from a connection.
type ipmiParams struct {
	host        string
	port        int
	username    string
	password    string
	iface       string
	cipherSuite int // -1 = unset (auto)
	hasCipher   bool
	privilege   types.PrivilegeLevel
}

func paramsFrom(conn *model.Connection, secrets map[string]string) ipmiParams {
	o := conn.Options
	p := ipmiParams{
		host:      conn.Host,
		port:      conn.Port,
		username:  conn.Username,
		password:  secrets[model.SecretPassword],
		iface:     strings.ToLower(strings.TrimSpace(o.String("ipmiInterface", "lanplus"))),
		privilege: mapPrivilege(o.String("privilegeLevel", "")),
	}
	if p.port == 0 {
		p.port = model.DefaultPort(model.ProtoIPMI)
	}
	if p.iface == "" {
		p.iface = "lanplus"
	}
	if o.Has("cipherSuite") {
		p.cipherSuite = o.Int("cipherSuite", 0)
		p.hasCipher = true
	}
	return p
}

func mapPrivilege(s string) types.PrivilegeLevel {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "user":
		return types.PrivilegeLevelUser
	case "operator":
		return types.PrivilegeLevelOperator
	case "administrator", "admin", "":
		return types.PrivilegeLevelAdministrator
	default:
		return types.PrivilegeLevelAdministrator
	}
}

// newClient builds and configures a go-ipmi client (not yet connected).
func newClient(p ipmiParams) (*goipmi.Client, error) {
	cl, err := goipmi.NewClient(p.host, p.port, p.username, p.password)
	if err != nil {
		return nil, err
	}
	if p.iface == "lan" {
		cl.WithInterface(goipmi.InterfaceLan)
	} else {
		cl.WithInterface(goipmi.InterfaceLanplus)
	}
	if p.privilege != types.PrivilegeLevelUnspecified {
		cl.WithMaxPrivilegeLevel(p.privilege)
	}
	if p.hasCipher && p.cipherSuite >= 0 && p.cipherSuite <= 17 {
		cl.WithCipherSuiteID(types.CipherSuiteID(p.cipherSuite))
	}
	cl.WithTimeout(5 * time.Second)
	return cl, nil
}

// ---- SOL backend --------------------------------------------------------------------------------------------------

// checkRoute refuses connections configured to go through a proxy or SSH gateway: RMCP+ is UDP and cannot follow
// them, and silently bypassing a configured route would leak traffic around it.
func checkRoute(conn *model.Connection) error {
	o := conn.Options
	if strings.TrimSpace(o.String("sshTunnelVia", "")) != "" || len(o.Strings("jumpHosts")) > 0 ||
		strings.TrimSpace(o.String("proxyCommand", "")) != "" {
		return errors.New("ipmi: IPMI (RMCP+ over UDP) cannot be routed through an SSH gateway or proxy command")
	}
	if pt := strings.ToLower(strings.TrimSpace(model.Options(o.Map("proxy")).String("type", ""))); pt != "" && pt != "none" {
		return errors.New("ipmi: IPMI (RMCP+ over UDP) cannot be routed through a proxy")
	}
	return nil
}

func (m *module) open(ctx context.Context, req term.OpenRequest) (term.Backend, error) {
	conn := req.Connection
	if conn == nil {
		return nil, term.Permanent(errors.New("ipmi: missing connection"))
	}
	if strings.TrimSpace(conn.Host) == "" {
		return nil, term.Permanent(errors.New("ipmi: BMC host is required"))
	}
	if err := checkRoute(conn); err != nil {
		return nil, term.Permanent(err)
	}
	p := paramsFrom(conn, req.Secrets)
	if p.iface == "lan" {
		return nil, term.Permanent(errors.New("ipmi: Serial-over-LAN requires IPMI v2.0 (lanplus)"))
	}
	cl, err := newClient(p)
	if err != nil {
		return nil, term.Permanent(fmt.Errorf("ipmi: %w", err))
	}
	if req.Session != nil {
		req.Session.SetStatus(model.StateConnecting, "Connecting to BMC "+p.host)
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = cl.Connect(cctx)
	cancel()
	if err != nil {
		_ = cl.Close(context.Background())
		return nil, classifyConnectError(err)
	}
	if req.Session != nil {
		req.Session.SetStatus(model.StateConnecting, "Activating SOL payload")
	}
	actx, acancel := context.WithTimeout(ctx, 15*time.Second)
	_, err = cl.ActivatePayload(actx, &transport.ActivatePayloadRequest{PayloadType: types.PayloadTypeSOL, PayloadInstance: 1})
	acancel()
	if err != nil {
		_ = cl.Close(context.Background())
		if strings.Contains(strings.ToLower(err.Error()), "already active") {
			return nil, term.Permanent(fmt.Errorf("ipmi: Serial-over-LAN is already in use on this BMC (another console is attached): %w", err))
		}
		return nil, term.Permanent(fmt.Errorf("ipmi: activate Serial-over-LAN: %w", err))
	}

	b := newSOLBackend(cl)
	return b, nil
}

func classifyConnectError(err error) error {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "auth") || strings.Contains(msg, "password") || strings.Contains(msg, "privilege") {
		return term.Permanent(fmt.Errorf("ipmi: BMC authentication failed: %w", err))
	}
	return fmt.Errorf("ipmi: connect to BMC: %w", err)
}

type solBackend struct {
	cl     *goipmi.Client
	inR    *io.PipeReader
	inW    *io.PipeWriter
	cancel context.CancelFunc

	outMu sync.Mutex
	out   []byte
	outCh chan struct{}

	closeOnce sync.Once
	done      chan struct{}
}

func newSOLBackend(cl *goipmi.Client) *solBackend {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	b := &solBackend{
		cl:     cl,
		inR:    pr,
		inW:    pw,
		cancel: cancel,
		outCh:  make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	go func() {
		// SOLStream reads console input from inR and writes BMC output to solWriter until ctx is cancelled or the
		// stream ends.
		_ = cl.SOLStream(ctx, pr, solWriter{b}, nil)
		b.finish()
	}()
	return b
}

type solWriter struct{ b *solBackend }

func (w solWriter) Write(p []byte) (int, error) {
	w.b.appendOutput(p)
	return len(p), nil
}

func (b *solBackend) appendOutput(data []byte) {
	if len(data) == 0 {
		return
	}
	b.outMu.Lock()
	b.out = append(b.out, data...)
	b.outMu.Unlock()
	select {
	case b.outCh <- struct{}{}:
	default:
	}
}

func (b *solBackend) Read(p []byte) (int, error) {
	for {
		b.outMu.Lock()
		if len(b.out) > 0 {
			n := copy(p, b.out)
			b.out = b.out[n:]
			b.outMu.Unlock()
			return n, nil
		}
		b.outMu.Unlock()
		select {
		case <-b.done:
			b.outMu.Lock()
			if len(b.out) > 0 {
				n := copy(p, b.out)
				b.out = b.out[n:]
				b.outMu.Unlock()
				return n, nil
			}
			b.outMu.Unlock()
			return 0, io.EOF
		case <-b.outCh:
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (b *solBackend) Write(p []byte) (int, error) {
	select {
	case <-b.done:
		return 0, io.ErrClosedPipe
	default:
	}
	return b.inW.Write(p)
}

func (b *solBackend) Resize(int, int) error { return nil }

func (b *solBackend) finish() {
	select {
	case <-b.done:
	default:
		close(b.done)
	}
	select {
	case b.outCh <- struct{}{}:
	default:
	}
}

func (b *solBackend) Close() error {
	b.closeOnce.Do(func() {
		b.cancel()
		_ = b.inW.Close()
		_ = b.inR.Close()
		// Best-effort deactivate + close on a fresh context.
		dctx, dcancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, _ = b.cl.DeactivatePayload(dctx, &transport.DeactivatePayloadRequest{PayloadType: types.PayloadTypeSOL, PayloadInstance: 1})
		_ = b.cl.Close(dctx)
		dcancel()
		b.finish()
	})
	return nil
}

// ---- power control ------------------------------------------------------------------------------------------------

// PowerResult is the JSON of POST /api/ipmi/:connectionId/power.
type PowerResult struct {
	Action  string `json:"action"`
	PowerOn bool   `json:"powerOn"`
}

func (m *module) handlePower(c *echo.Context) error {
	user := httpx.UserFrom(c)
	if user == nil {
		return httpx.ErrUnauthorized
	}
	connID := c.Param("connectionId")
	if !model.ValidID(connID) {
		return httpx.ErrNotFound
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := httpx.Bind(c, &body); err != nil {
		return err
	}
	action := strings.ToLower(strings.TrimSpace(body.Action))
	if action == "" {
		return httpx.BadRequest("action is required")
	}
	conn, secrets, err := m.d.ResolveConnection(c.Request().Context(), user, connID)
	if err != nil {
		return err
	}
	if conn.Protocol != model.ProtoIPMI {
		return httpx.BadRequest("connection is not an IPMI connection")
	}
	if err := checkRoute(conn); err != nil {
		return httpx.BadRequest(err.Error())
	}
	if _, ok := mapPowerAction(action); !ok && action != "status" {
		return httpx.BadRequest("unsupported power action " + action + " (status, on, off, cycle, reset, soft)")
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 20*time.Second)
	defer cancel()
	res, err := power(ctx, paramsFrom(conn, secrets), action)
	if err != nil {
		return httpx.BadRequest(err.Error())
	}
	if action != "status" && m.d.Audit != nil {
		m.d.Audit.Log(c, "ipmi.power", connID, map[string]any{"action": action})
	}
	return c.JSON(http.StatusOK, res)
}

// power runs one chassis power action (or reads the status) over its own short IPMI session.
func power(ctx context.Context, p ipmiParams, action string) (PowerResult, error) {
	cl, err := newClient(p)
	if err != nil {
		return PowerResult{}, fmt.Errorf("ipmi: %w", err)
	}
	if err := cl.Connect(ctx); err != nil {
		return PowerResult{}, classifyConnectError(err)
	}
	defer cl.Close(context.Background())
	if action == "status" {
		st, err := cl.GetChassisStatus(ctx)
		if err != nil {
			return PowerResult{}, fmt.Errorf("ipmi: chassis status: %w", err)
		}
		return PowerResult{Action: "status", PowerOn: st.PowerIsOn}, nil
	}
	control, ok := mapPowerAction(action)
	if !ok {
		return PowerResult{}, fmt.Errorf("ipmi: unsupported power action %q", action)
	}
	if _, err := cl.ChassisControl(ctx, control); err != nil {
		return PowerResult{}, fmt.Errorf("ipmi: power %s: %w", action, err)
	}
	// Report the resulting power state where the BMC updates quickly.
	on := action == "on" || action == "cycle" || action == "reset"
	if st, err := cl.GetChassisStatus(ctx); err == nil {
		on = st.PowerIsOn
	}
	return PowerResult{Action: action, PowerOn: on}, nil
}

// mapPowerAction maps a REST action to a chassis control command.
func mapPowerAction(action string) (chassis.ChassisControl, bool) {
	switch action {
	case "on":
		return chassis.ChassisControlPowerUp, true
	case "off":
		return chassis.ChassisControlPowerDown, true
	case "cycle":
		return chassis.ChassisControlPowerCycle, true
	case "reset":
		return chassis.ChassisControlHardReset, true
	case "soft":
		return chassis.ChassisControlSoftShutdown, true
	default:
		return 0, false
	}
}
