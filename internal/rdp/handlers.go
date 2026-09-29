package rdp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/labstack/echo/v5"

	"github.com/nexterm/nexterm/internal/events"
	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
	"github.com/nexterm/nexterm/internal/term"
)

// ---- POST /api/sessions/{id}/rdp-ticket -----------------------------------------------------------------------------

// ticketRequest is the optional body of POST /api/sessions/{id}/rdp-ticket.
type ticketRequest struct {
	// Size (CSS pixels × devicePixelRatio as the viewer wants it) used when the connection does not pin width/height.
	Width  int `json:"width"`
	Height int `json:"height"`
	DPI    int `json:"dpi"`
	// Engine overrides the connection's engine for this connection attempt ("ironrdp" | "guacd").
	Engine string `json:"engine"`
	// PreconnectionBlob supplies the Hyper-V VM id (security "vmconnect") when the connection has none.
	PreconnectionBlob string `json:"preconnectionBlob"`
	// Shadow asks an administrator's read-only view of another user's session (guacd sessions only).
	Shadow bool `json:"shadow"`
}

// ticketResponse is the answer of POST /api/sessions/{id}/rdp-ticket (web/src/features/rdp/types.ts RdpTicketInfo,
// a superset of the shared RdpTicket type). The password is only included for the IronRDP engine, whose CredSSP runs
// in the browser; guacd tickets never carry credentials.
type ticketResponse struct {
	Engine            string `json:"engine"`
	Token             string `json:"token"`
	ExpiresIn         int    `json:"expiresIn"`
	Destination       string `json:"destination"`
	Host              string `json:"host"`
	Port              int    `json:"port"`
	Username          string `json:"username"`
	Domain            string `json:"domain"`
	Password          string `json:"password"`
	Width             int    `json:"width"`
	Height            int    `json:"height"`
	DPI               int    `json:"dpi"`
	FixedSize         bool   `json:"fixedSize"`
	ResizeMethod      string `json:"resizeMethod"`
	Security          string `json:"security"`
	EnableCredssp     bool   `json:"enableCredssp"`
	PreConnectionBlob string `json:"preConnectionBlob,omitempty"`
	Clipboard         bool   `json:"clipboard"`
	Audio             bool   `json:"audio"`
	Microphone        bool   `json:"microphone"`
	Drive             bool   `json:"drive"`
	DriveName         string `json:"driveName,omitempty"`
	Printing          bool   `json:"printing"`
	ColorDepth        int    `json:"colorDepth"`
	ServerLayout      string `json:"serverLayout,omitempty"`
	Via               string `json:"via,omitempty"`
	// AutoReconnect: the viewer reconnects after an unexpected disconnection (options.autoReconnect).
	AutoReconnect bool `json:"autoReconnect"`
	// ReadOnly: a shadow ticket (the viewer sends no input).
	ReadOnly bool `json:"readOnly"`
	// Recording: the connection asks for session recording (guacd sessions are recorded by NexTerm).
	Recording bool `json:"recording"`
}

func (h *handler) handleTicket(c *echo.Context) error {
	var req ticketRequest
	if err := httpx.BindOptional(c, &req); err != nil {
		return err
	}
	s, u, err := h.session(c, req.Shadow)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if s.OwnerID != u.ID { // an administrator viewing another user's session
		resp, err := h.issueShadowTicket(s, u)
		if err != nil {
			return err
		}
		h.auditUser(ctx, u, "session.shadow", s.ID, map[string]any{"protocol": model.ProtoRDP, "engine": engineGuacd,
			"ownerId": s.OwnerID})
		return c.JSON(http.StatusOK, resp)
	}
	resp, err := h.issueTicket(term.WithSession(ctx, s), s, u, req)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// Errors of the ticket endpoint the UI reacts to.
var (
	errGuacdUnavailable = httpx.NewError(http.StatusConflict, "guacd_unavailable",
		"guacd is not configured; start the guacd sidecar in Settings → Remote desktop, or use the built-in IronRDP engine")
	errVMIDRequired = httpx.NewError(http.StatusBadRequest, "vm_id_required",
		"the Hyper-V console (vmconnect) needs the id of the virtual machine")
	errStandardSecurity = httpx.NewError(http.StatusBadRequest, "unsupported_security",
		"standard RDP security (without TLS) is not supported by the built-in engine; use TLS / NLA or the guacd engine")
	errCredentialsRequired = httpx.NewError(http.StatusConflict, "credentials_required",
		"this server requires Network Level Authentication: a user name and password are needed")
	errShadowUnavailable = httpx.NewError(http.StatusConflict, "shadow_unavailable",
		"the session can only be viewed while its owner is connected through guacd (IronRDP sessions run in the owner's browser)")
)

// issueShadowTicket issues an administrator's read-only view of a session its owner drives through guacd: the
// tunnel joins the owner's guacd connection with read-only arguments. No credentials are involved.
func (h *handler) issueShadowTicket(s *term.Session, u *model.User) (*ticketResponse, error) {
	j, ok := h.joins.get(s.ID)
	if !ok {
		return nil, errShadowUnavailable
	}
	conn := s.Connection()
	if conn == nil {
		conn = &model.Connection{Protocol: model.ProtoRDP}
	}
	conn.Secrets, conn.SecretsEnc = nil, nil
	port := conn.Port
	if port <= 0 || port > 65535 {
		port = model.DefaultPort(model.ProtoRDP)
	}
	t := &ticket{sessionID: s.ID, userID: u.ID, engine: engineGuacd, conn: conn, secrets: map[string]string{},
		host: conn.Host, port: port, width: j.width, height: j.height, dpi: defaultDPI, shadow: true, join: j.id}
	token := h.tickets.issue(t)
	return &ticketResponse{
		Engine:       engineGuacd,
		Token:        token,
		ExpiresIn:    int(ticketTTL.Seconds()),
		Destination:  net.JoinHostPort(conn.Host, strconv.Itoa(port)),
		Host:         conn.Host,
		Port:         port,
		Username:     conn.Username,
		Width:        j.width,
		Height:       j.height,
		DPI:          defaultDPI,
		FixedSize:    true,
		ResizeMethod: "none",
		Security:     parseOptions(conn.Options).Security,
		ColorDepth:   parseOptions(conn.Options).ColorDepth,
		Via:          routeDescription(conn),
		ReadOnly:     true,
	}, nil
}

func (h *handler) issueTicket(ctx context.Context, s *term.Session, u *model.User, req ticketRequest) (*ticketResponse, error) {
	conn, secrets, err := s.Resolve(ctx)
	if err != nil {
		if term.IsPermanent(err) {
			return nil, httpx.NotFound(err.Error())
		}
		return nil, err
	}
	opts := parseOptions(conn.Options)
	g := h.globalSettings(ctx)
	engine := chooseEngine(req.Engine, opts, g)
	if engine == engineGuacd && h.guacd.address(ctx) == "" {
		return nil, errGuacdUnavailable
	}
	host := strings.TrimSpace(conn.Host)
	if host == "" || len(host) > 255 || strings.ContainsFunc(host, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return nil, httpx.BadRequest("the connection has no valid host")
	}
	port := conn.Port
	if port <= 0 || port > 65535 {
		port = model.DefaultPort(model.ProtoRDP)
		if opts.Security == secVMConnect {
			port = defaultVMPort
		}
	}
	if opts.Security == secRDP && engine == engineIronRDP {
		return nil, errStandardSecurity
	}
	// Early, DNS-free destination check for restricted users (SEC-7): a literal / localhost destination NexTerm or
	// guacd would connect to directly is refused with 403 destination_blocked before a ticket exists. Authoritative
	// checks happen when connecting (guarded dials; guacdDestination for guacd).
	if gd := h.guard(u); gd != nil {
		if routeDescription(conn) == "" {
			if err := gd.CheckLiteral(host, port); err != nil {
				return nil, err
			}
		}
		if engine == engineGuacd && strings.TrimSpace(opts.GatewayHost) != "" {
			gwPort := opts.GatewayPort
			if gwPort <= 0 {
				gwPort = 443
			}
			if err := gd.CheckLiteral(opts.GatewayHost, gwPort); err != nil {
				return nil, err
			}
		}
	}
	pcb := opts.PreconnectionBlob
	if opts.Security == secVMConnect && pcb == "" {
		pcb = strings.TrimSpace(req.PreconnectionBlob)
		if pcb == "" {
			return nil, errVMIDRequired
		}
	}
	if len(pcb) > 512 || strings.ContainsFunc(pcb, unicode.IsControl) {
		return nil, httpx.BadRequest("invalid pre-connection blob")
	}

	width, height := opts.Width, opts.Height
	if !opts.fixedSize() {
		width, height = clampDesktop(req.Width, defaultWidth), clampDesktop(req.Height, defaultHeight)
	}
	dpi := opts.DPI
	if dpi == 0 {
		dpi = req.DPI
	}
	if dpi < 72 || dpi > 480 {
		dpi = defaultDPI
	}

	username := strings.TrimSpace(conn.Username)
	if username == "" {
		username = secrets["username"]
	}
	domain := opts.Domain
	if d, name, ok := strings.Cut(username, `\`); ok && domain == "" && d != "" && name != "" {
		domain, username = d, name
	}
	password := secrets[model.SecretPassword]
	credssp := opts.wantsCredSSP()

	if engine == engineIronRDP {
		needPrompt := password == "" && (opts.Security == secNLA || opts.Security == secVMConnect ||
			(opts.Security == secAny && username != ""))
		if needPrompt {
			user, pass, perr := h.promptCredentials(ctx, s, u, conn, username, domain)
			switch {
			case perr == nil:
				username, password = user, pass
			case errors.Is(perr, errPromptCanceled) && opts.Security == secAny:
				// Without credentials the server shows its logon screen (TLS security, no NLA).
				credssp = false
			default:
				return nil, perr
			}
		}
		if username == "" && opts.Security == secAny {
			credssp = false // CredSSP needs credentials; let the server show the logon screen
		}
	}

	t := &ticket{
		sessionID: s.ID,
		userID:    u.ID,
		engine:    engine,
		conn:      conn.Clone(),
		secrets:   secrets,
		host:      host,
		port:      port,
		width:     width,
		height:    height,
		dpi:       dpi,
	}
	t.conn.Port = port
	t.autologon = engine == engineIronRDP && username != "" && password != ""
	if pcb != "" && t.conn.Options != nil {
		t.conn.Options["preconnectionBlob"] = pcb
	}
	token := h.tickets.issue(t)

	resp := &ticketResponse{
		Engine:            engine,
		Token:             token,
		ExpiresIn:         int(ticketTTL.Seconds()),
		Destination:       net.JoinHostPort(host, strconv.Itoa(port)),
		Host:              host,
		Port:              port,
		Username:          username,
		Domain:            domain,
		Width:             width,
		Height:            height,
		DPI:               dpi,
		FixedSize:         opts.fixedSize(),
		ResizeMethod:      opts.ResizeMethod,
		Security:          opts.Security,
		EnableCredssp:     credssp,
		PreConnectionBlob: pcb,
		Clipboard:         !opts.DisableClipboard,
		Audio:             opts.EnableAudio,
		Microphone:        opts.EnableMic,
		Drive:             opts.EnableDrive,
		Printing:          opts.EnablePrinting,
		ColorDepth:        opts.ColorDepth,
		ServerLayout:      opts.ServerLayout,
		Via:               routeDescription(conn),
		AutoReconnect:     conn.Options.Bool("autoReconnect", false),
		Recording:         opts.Recording,
	}
	if opts.EnableDrive {
		resp.DriveName = opts.DriveName
	}
	if engine == engineIronRDP {
		resp.Password = password
	} else {
		resp.EnableCredssp = false
	}
	return resp, nil
}

func clampDesktop(v, def int) int {
	if v <= 0 {
		return def
	}
	return min(max(v, minDesktop), maxDesktop)
}

// routeDescription names the gateway a connection is reached through (display only).
func routeDescription(c *model.Connection) string {
	o := c.Options
	switch {
	case o.String("sshTunnelVia", "") != "":
		return "ssh"
	case len(o.Strings("jumpHosts")) > 0:
		return "jump"
	case o.String("proxyCommand", "") != "":
		return "proxy-command"
	}
	if p, ok := o["proxy"].(map[string]any); ok {
		if t, _ := p["type"].(string); t != "" && t != "none" {
			return "proxy"
		}
	}
	return ""
}

var errPromptCanceled = errors.New("the credential prompt was canceled")

// promptCredentials asks for missing RDP credentials through the prompt broker. The answer is remembered for
// reconnects of this session; with "save" it is stored in the connection once the connection succeeds.
func (h *handler) promptCredentials(ctx context.Context, s *term.Session, u *model.User, conn *model.Connection, username, domain string) (string, string, error) {
	if h.d.Events == nil {
		return "", "", errCredentialsRequired
	}
	askUser := username == ""
	fields := []model.PromptField{}
	if askUser {
		fields = append(fields, model.PromptField{Label: "Username", Echo: true})
	}
	fields = append(fields, model.PromptField{Label: "Password", Echo: false})
	target := conn.Host
	if username != "" {
		who := username
		if domain != "" {
			who = domain + `\` + username
		}
		target = who + " on " + conn.Host
	}
	canSave := conn.ID != "" && conn.OwnerID == u.ID
	h.setState(s.ID, model.StateAuthenticating, "Waiting for credentials")
	resp, err := h.d.Events.Prompt(ctx, u.ID, model.Prompt{
		Kind:         model.PromptPassword,
		Title:        "Remote desktop sign-in",
		Message:      "Enter the credentials for " + target + ".",
		SessionID:    s.ID,
		ConnectionID: conn.ID,
		Fields:       fields,
		AllowSave:    canSave,
	})
	h.setState(s.ID, model.StateConnecting, "")
	switch {
	case errors.Is(err, events.ErrNoInteractiveClient):
		return "", "", errCredentialsRequired
	case errors.Is(err, events.ErrPromptTimeout):
		return "", "", httpx.NewError(http.StatusRequestTimeout, "prompt_timeout", "the credential prompt was not answered in time")
	case err != nil:
		return "", "", err
	}
	if !resp.Accept || len(resp.Values) < len(fields) {
		return "", "", errPromptCanceled
	}
	pass := resp.Values[len(resp.Values)-1]
	if askUser {
		username = strings.TrimSpace(resp.Values[0])
		if username == "" {
			return "", "", errPromptCanceled
		}
		s.RememberSecret("username", username)
	}
	s.RememberSecret(model.SecretPassword, pass)
	if resp.Save && canSave && !askUser && pass != "" {
		h.pending.put(s.ID, conn.ID, u.ID, map[string]string{model.SecretPassword: pass})
	}
	return username, pass, nil
}

// ---- POST /api/sessions/{id}/rdp-state ------------------------------------------------------------------------------

// stateRequest is the viewer's report of the RDP connection state (the browser runs the RDP protocol for IronRDP,
// so it knows best when the desktop is up or why it ended).
type stateRequest struct {
	State   string `json:"state"`
	Message string `json:"message"`
	// AuthFailed marks a rejected logon: a password remembered from a prompt is forgotten so the next attempt asks.
	AuthFailed bool `json:"authFailed"`
}

func (h *handler) handleState(c *echo.Context) error {
	s, u, err := h.session(c, false)
	if err != nil {
		return err
	}
	var req stateRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	st := model.SessionState(req.State)
	switch st {
	case model.StateConnecting, model.StateAuthenticating, model.StateConnected, model.StateDisconnected, model.StateError:
	default:
		return httpx.BadRequest(fmt.Sprintf("invalid state %q", req.State))
	}
	h.setState(s.ID, st, cleanMessage(req.Message))
	switch st {
	case model.StateConnected:
		h.commitPending(c.Request().Context(), s, u)
	case model.StateError:
		if req.AuthFailed {
			h.pending.drop(s.ID)
			s.RememberSecret(model.SecretPassword, "")
		}
	}
	return c.JSON(http.StatusOK, s.Info())
}
