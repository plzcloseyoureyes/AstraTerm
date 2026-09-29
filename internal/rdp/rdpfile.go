package rdp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// CC-15: .rdp connection files for native clients (mstsc, Windows App / Microsoft Remote Desktop, FreeRDP, Remmina).
// Files never contain passwords.

// rdpFileParams are the inputs of an .rdp file.
type rdpFileParams struct {
	Host     string
	Port     int
	Username string
	Domain   string
	Opts     rdpOptions
}

// buildRDPFile renders an .rdp file (CRLF line endings, "name:type:value" lines).
func buildRDPFile(p rdpFileParams) []byte {
	var b strings.Builder
	line := func(key, typ string, value any) {
		v := fmt.Sprint(value)
		v = strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == 0 {
				return -1
			}
			return r
		}, v)
		b.WriteString(key + ":" + typ + ":" + v + "\r\n")
	}
	i := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	o := p.Opts
	addr := p.Host
	if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
		addr = "[" + addr + "]" // IPv6 literal
	}
	if p.Port != 0 && p.Port != 3389 {
		addr += ":" + strconv.Itoa(p.Port)
	}
	line("full address", "s", addr)
	if p.Username != "" {
		user := p.Username
		if p.Domain != "" && !strings.ContainsAny(user, `\@`) {
			user = p.Domain + `\` + user
		}
		line("username", "s", user)
	}
	if p.Domain != "" {
		line("domain", "s", p.Domain)
	}
	if o.fixedSize() {
		line("screen mode id", "i", 1)
		line("desktopwidth", "i", o.Width)
		line("desktopheight", "i", o.Height)
		line("smart sizing", "i", 1)
	} else {
		line("screen mode id", "i", 2)
		line("dynamic resolution", "i", 1)
	}
	if o.DPI > 0 {
		line("desktopscalefactor", "i", o.DPI*100/96)
	}
	line("session bpp", "i", o.ColorDepth)
	if o.EnableAudio {
		line("audiomode", "i", 0)
	} else {
		line("audiomode", "i", 2)
	}
	line("audiocapturemode", "i", i(o.EnableMic))
	line("redirectclipboard", "i", i(!o.DisableClipboard))
	line("redirectprinters", "i", i(o.EnablePrinting))
	if o.EnableDrive {
		line("drivestoredirect", "s", "*")
	}
	line("administrative session", "i", i(o.Console))
	if o.InitialProgram != "" && o.RemoteApp == "" {
		line("alternate shell", "s", o.InitialProgram)
	}
	if o.RemoteApp != "" {
		line("remoteapplicationmode", "i", 1)
		line("remoteapplicationprogram", "s", o.RemoteApp)
		if o.RemoteAppArgs != "" {
			line("remoteapplicationcmdline", "s", o.RemoteAppArgs)
		}
	}
	if o.GatewayHost != "" {
		gw := o.GatewayHost
		if o.GatewayPort > 0 && o.GatewayPort != 443 {
			gw += ":" + strconv.Itoa(o.GatewayPort)
		}
		line("gatewayhostname", "s", gw)
		line("gatewayusagemethod", "i", 1)
		line("gatewayprofileusagemethod", "i", 1)
		line("gatewaycredentialssource", "i", 0)
	}
	if o.LoadBalanceInfo != "" {
		line("loadbalanceinfo", "s", o.LoadBalanceInfo)
	}
	switch o.Security {
	case secTLS:
		line("enablecredsspsupport", "i", 0)
		line("negotiate security layer", "i", 1)
	case secRDP:
		line("enablecredsspsupport", "i", 0)
		line("negotiate security layer", "i", 0)
	default:
		line("enablecredsspsupport", "i", 1)
		line("negotiate security layer", "i", 1)
	}
	if o.PreconnectionBlob != "" {
		line("pcb", "s", o.PreconnectionBlob)
	}
	if o.IgnoreCert {
		line("authentication level", "i", 0)
	} else {
		line("authentication level", "i", 2)
	}
	line("prompt for credentials", "i", 0)
	line("disable wallpaper", "i", i(!o.EnableWallpaper))
	line("disable themes", "i", i(!o.EnableTheming))
	line("allow font smoothing", "i", i(o.EnableFontSmoothing))
	line("keyboardhook", "i", 2)
	line("autoreconnection enabled", "i", 1)
	line("connection type", "i", 7)
	line("networkautodetect", "i", 1)
	line("bandwidthautodetect", "i", 1)
	return []byte(b.String())
}

// rdpFileName builds a safe download name.
func rdpFileName(name, host string) string {
	base := strings.TrimSpace(name)
	if base == "" {
		base = host
	}
	base = strings.Map(func(r rune) rune {
		switch {
		case r < ' ' || r == 0x7f, strings.ContainsRune(`<>:"/\|?*`, r):
			return '_'
		}
		return r
	}, base)
	base = strings.Trim(base, ". ")
	if base == "" {
		base = "connection"
	}
	return clipRunes(base, 100) + ".rdp"
}

// asciiFileName is the plain-ASCII fallback of a download name (Content-Disposition filename=).
func asciiFileName(name string) string {
	return strings.Map(func(r rune) rune {
		if r > 0x7e || r < 0x20 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
}

func (h *handler) sendRDPFile(c *echo.Context, name string, data []byte) error {
	hdr := c.Response().Header()
	hdr.Set(echo.HeaderContentDisposition, fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, asciiFileName(name), url.PathEscape(name)))
	hdr.Set(echo.HeaderCacheControl, "no-store")
	return c.Blob(http.StatusOK, "application/x-rdp; charset=utf-8", data)
}

// handleSessionFile serves GET /api/sessions/{id}/rdp-file.
func (h *handler) handleSessionFile(c *echo.Context) error {
	s, _, err := h.session(c, true)
	if err != nil {
		return err
	}
	conn := s.Connection()
	if conn == nil {
		return httpx.ErrNotFound
	}
	p := fileParams(conn)
	return h.sendRDPFile(c, rdpFileName(s.Title(), conn.Host), buildRDPFile(p))
}

// handleConnectionFile serves GET /api/connections/{id}/rdp-file (no secrets needed: works while the vault is locked).
func (h *handler) handleConnectionFile(c *echo.Context) error {
	conn, err := h.visibleConnection(c.Request().Context(), httpx.UserFrom(c), c.Param("id"))
	if err != nil {
		return err
	}
	return h.sendRDPFile(c, rdpFileName(conn.Name, conn.Host), buildRDPFile(fileParams(conn)))
}

// visibleConnection loads a saved RDP connection visible to user, with the identity's username merged in, without
// decrypting secrets.
func (h *handler) visibleConnection(ctx context.Context, u *model.User, id string) (*model.Connection, error) {
	if u == nil {
		return nil, httpx.ErrUnauthorized
	}
	if !model.ValidID(id) || h.d.Store == nil {
		return nil, httpx.ErrNotFound
	}
	conn, err := h.d.Store.Connections.Get(ctx, id)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, httpx.ErrNotFound
		}
		return nil, err
	}
	if !app.Visible(u, conn.OwnerID, conn.Shared) {
		return nil, httpx.ErrNotFound
	}
	if conn.Protocol != model.ProtoRDP {
		return nil, httpx.BadRequest("not an RDP connection")
	}
	c := conn.Clone()
	c.Secrets, c.SecretsEnc = nil, nil
	if c.Username == "" && c.IdentityID != "" {
		if ident, err := h.d.Store.Identities.Get(ctx, c.IdentityID); err == nil && ident.OwnerID == conn.OwnerID {
			c.Username = ident.Username
		}
	}
	return c, nil
}

func fileParams(conn *model.Connection) rdpFileParams {
	opts := parseOptions(conn.Options)
	port := conn.Port
	if port <= 0 || port > 65535 {
		port = 3389
		if opts.Security == secVMConnect {
			port = defaultVMPort
		}
	}
	user, domain := conn.Username, opts.Domain
	if d, name, ok := strings.Cut(user, `\`); ok && domain == "" && d != "" && name != "" {
		domain, user = d, name
	}
	return rdpFileParams{Host: conn.Host, Port: port, Username: user, Domain: domain, Opts: opts}
}
