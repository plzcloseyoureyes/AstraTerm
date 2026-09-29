package rdp

import (
	"path"
	"strconv"
	"strings"

	"github.com/termstead/termstead/internal/model"
)

// defaultGuacdDataPath is the directory in guacd's filesystem used for virtual drives and recordings when the admin
// did not configure rdp.guacdDataPath (writable by guacd's user in the official image).
const defaultGuacdDataPath = "/tmp/termstead"

// guacParams maps a connection's options (SPEC §5.3 "rdp") onto guacd's RDP parameters. host/port is what guacd
// dials (a local forwarder for gateway routes). Credentials come from the vault and never reach the browser.
func (h *handler) guacParams(tk *ticket, opts rdpOptions, g globalSettings, host string, port int, ignoreCert bool) map[string]string {
	b := func(v bool) string {
		if v {
			return "true"
		}
		return ""
	}
	username := firstNonEmpty(tk.conn.Username, tk.secrets["username"])
	domain := opts.Domain
	if d, name, ok := strings.Cut(username, `\`); ok && domain == "" && d != "" && name != "" {
		domain, username = d, name
	}
	security := opts.Security
	if security == secNLA {
		security = "nla"
	}
	p := map[string]string{
		"hostname":              host,
		"port":                  strconv.Itoa(port),
		"username":              username,
		"password":              tk.secrets[model.SecretPassword],
		"domain":                domain,
		"security":              security,
		"ignore-cert":           b(ignoreCert),
		"console":               b(opts.Console),
		"server-layout":         opts.ServerLayout,
		"timezone":              opts.Timezone,
		"color-depth":           strconv.Itoa(opts.ColorDepth),
		"disable-audio":         b(!opts.EnableAudio),
		"enable-audio-input":    b(opts.EnableMic),
		"enable-printing":       b(opts.EnablePrinting),
		"disable-copy":          b(opts.DisableClipboard),
		"disable-paste":         b(opts.DisableClipboard),
		"initial-program":       opts.InitialProgram,
		"enable-wallpaper":      b(opts.EnableWallpaper),
		"enable-theming":        b(opts.EnableTheming),
		"enable-font-smoothing": b(opts.EnableFontSmoothing),
		"client-name":           "Termstead",
		"load-balance-info":     opts.LoadBalanceInfo,
	}
	if opts.ResizeMethod != "none" {
		p["resize-method"] = opts.ResizeMethod
	}
	if opts.fixedSize() {
		p["width"], p["height"] = strconv.Itoa(opts.Width), strconv.Itoa(opts.Height)
	}
	if opts.DPI > 0 {
		p["dpi"] = strconv.Itoa(opts.DPI)
	}
	if opts.EnablePrinting {
		p["printer-name"] = "Termstead PDF"
	}
	if opts.RemoteApp != "" {
		p["remote-app"] = opts.RemoteApp
		p["remote-app-dir"] = opts.RemoteAppDir
		p["remote-app-args"] = opts.RemoteAppArgs
	}
	if opts.GatewayHost != "" {
		p["gateway-hostname"] = opts.GatewayHost
		if opts.GatewayPort > 0 {
			p["gateway-port"] = strconv.Itoa(opts.GatewayPort)
		}
		p["gateway-username"] = firstNonEmpty(opts.GatewayUsername, username)
		p["gateway-domain"] = firstNonEmpty(opts.GatewayDomain, domain)
		p["gateway-password"] = firstNonEmpty(tk.secrets[model.SecretGatewayPassword], tk.secrets[model.SecretPassword])
	}
	if pcb := tk.conn.Options.String("preconnectionBlob", ""); pcb != "" {
		p["preconnection-blob"] = pcb
		if opts.PreconnectionID > 0 {
			p["preconnection-id"] = strconv.Itoa(opts.PreconnectionID)
		}
	}
	data := strings.TrimRight(g.GuacdDataPath, "/")
	if data == "" || !strings.HasPrefix(data, "/") || strings.Contains(data, "..") {
		data = defaultGuacdDataPath
	}
	if opts.EnableDrive {
		// One drive per Termstead user (user ids are [a-z0-9] only, safe as path segments).
		p["enable-drive"] = "true"
		p["drive-name"] = opts.DriveName
		p["drive-path"] = path.Join(data, "drives", tk.userID)
		p["create-drive-path"] = "true"
	}
	// options.recording is honoured by Termstead itself (guacrecord.go): guacd's recording-path would write into guacd's
	// (usually a container's) filesystem, out of reach of Termstead's recordings.
	return p
}
