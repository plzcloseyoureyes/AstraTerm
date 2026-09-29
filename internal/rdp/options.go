package rdp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
)

// Engines.
const (
	engineIronRDP = "ironrdp"
	engineGuacd   = "guacd"
)

// Security modes of options.security (SPEC §5.3).
const (
	secAny       = "any"
	secNLA       = "nla"
	secTLS       = "tls"
	secRDP       = "rdp"
	secVMConnect = "vmconnect"
)

// Display limits.
const (
	minDesktop     = 200
	maxDesktop     = 8192
	defaultWidth   = 1280
	defaultHeight  = 800
	defaultDPI     = 96
	defaultVMPort  = 2179
	defaultDrive   = "AstraTerm"
	maxOptionValue = 4096
)

// rdpOptions are the rdp keys of a connection's options (SPEC §5.3 "rdp", plus the documented extensions in §9).
type rdpOptions struct {
	Engine              string
	Domain              string
	Security            string
	IgnoreCert          bool
	Width, Height, DPI  int
	ColorDepth          int
	ResizeMethod        string
	EnableAudio         bool
	EnableMic           bool
	EnableDrive         bool
	DriveName           string
	EnablePrinting      bool
	DisableClipboard    bool
	Console             bool
	InitialProgram      string
	ServerLayout        string
	Timezone            string
	GatewayHost         string
	GatewayPort         int
	GatewayUsername     string
	GatewayDomain       string
	EnableWallpaper     bool
	EnableTheming       bool
	EnableFontSmoothing bool
	Recording           bool
	PreconnectionBlob   string
	PreconnectionID     int
	LegacyTLS           bool
	LoadBalanceInfo     string
	RemoteApp           string
	RemoteAppDir        string
	RemoteAppArgs       string
}

// parseOptions reads the rdp options with their defaults. Invalid values fall back to defaults.
func parseOptions(o model.Options) rdpOptions {
	s := func(key string) string { return clip(strings.TrimSpace(o.String(key, ""))) }
	r := rdpOptions{
		Engine:              strings.ToLower(s("rdpEngine")),
		Domain:              s("domain"),
		Security:            strings.ToLower(s("security")),
		IgnoreCert:          o.Bool("ignoreCert", false),
		Width:               o.Int("width", 0),
		Height:              o.Int("height", 0),
		DPI:                 o.Int("dpi", 0),
		ColorDepth:          o.Int("colorDepth", 32),
		ResizeMethod:        strings.ToLower(s("resizeMethod")),
		EnableAudio:         o.Bool("enableAudio", false),
		EnableMic:           o.Bool("enableMic", false),
		EnableDrive:         o.Bool("enableDrive", false),
		DriveName:           s("driveName"),
		EnablePrinting:      o.Bool("enablePrinting", false),
		DisableClipboard:    o.Bool("disableClipboard", false),
		Console:             o.Bool("console", false),
		InitialProgram:      s("initialProgram"),
		ServerLayout:        s("serverLayout"),
		Timezone:            s("timezone"),
		GatewayHost:         s("gatewayHost"),
		GatewayPort:         o.Int("gatewayPort", 0),
		GatewayUsername:     s("gatewayUsername"),
		GatewayDomain:       s("gatewayDomain"),
		EnableWallpaper:     o.Bool("enableWallpaper", false),
		EnableTheming:       o.Bool("enableTheming", false),
		EnableFontSmoothing: o.Bool("enableFontSmoothing", false),
		Recording:           o.Bool("recording", false),
		PreconnectionBlob:   s("preconnectionBlob"),
		PreconnectionID:     o.Int("preconnectionId", 0),
		LegacyTLS:           o.Bool("legacyTls", false),
		LoadBalanceInfo:     s("loadBalanceInfo"),
		RemoteApp:           s("remoteApp"),
		RemoteAppDir:        s("remoteAppDir"),
		RemoteAppArgs:       s("remoteAppArgs"),
	}
	switch r.Engine {
	case engineIronRDP, engineGuacd:
	default:
		r.Engine = ""
	}
	switch r.Security {
	case secAny, secNLA, secTLS, secRDP, secVMConnect:
	case "nla-ext":
		r.Security = secNLA
	default:
		r.Security = secAny
	}
	if r.Width != 0 && (r.Width < minDesktop || r.Width > maxDesktop) {
		r.Width = 0
	}
	if r.Height != 0 && (r.Height < minDesktop || r.Height > maxDesktop) {
		r.Height = 0
	}
	if r.Width == 0 || r.Height == 0 {
		r.Width, r.Height = 0, 0
	}
	if r.DPI != 0 && (r.DPI < 72 || r.DPI > 480) {
		r.DPI = 0
	}
	switch r.ColorDepth {
	case 8, 16, 24, 32:
	default:
		r.ColorDepth = 32
	}
	switch r.ResizeMethod {
	case "display-update", "reconnect", "none":
	default:
		r.ResizeMethod = "display-update"
	}
	if r.GatewayPort < 0 || r.GatewayPort > 65535 {
		r.GatewayPort = 0
	}
	if r.PreconnectionID < 0 {
		r.PreconnectionID = 0
	}
	if r.DriveName == "" {
		r.DriveName = defaultDrive
	}
	return r
}

// fixedSize reports whether the connection pins the desktop size.
func (r rdpOptions) fixedSize() bool { return r.Width > 0 && r.Height > 0 }

// wantsCredSSP reports whether the IronRDP client should offer CredSSP (NLA).
func (r rdpOptions) wantsCredSSP() bool { return r.Security != secTLS && r.Security != secRDP }

func clip(s string) string {
	if len(s) > maxOptionValue {
		return s[:maxOptionValue]
	}
	return s
}

// ---- global (admin) settings ----------------------------------------------------------------------------------------

// settingsKey is the global settings section of the module (PUT /api/admin/settings {rdp: {...}}). Only the global
// scope is read: users cannot redirect guacd through their own settings.
const settingsKey = "rdp"

type globalSettings struct {
	// GuacdAddress overrides --guacd (host:port; "off" disables guacd).
	GuacdAddress string `json:"guacdAddress,omitempty"`
	// DefaultEngine is used by connections without options.rdpEngine.
	DefaultEngine string `json:"defaultEngine,omitempty"`
	// GuacdSidecar is set while the address points to the container started by POST /api/guacd/sidecar.
	GuacdSidecar bool `json:"guacdSidecar,omitempty"`
	// GuacdDataPath is a writable directory in guacd's filesystem for virtual drives and recordings.
	GuacdDataPath string `json:"guacdDataPath,omitempty"`
	// GuacdForwardHost is the address guacd uses to reach AstraTerm's loopback forwarders (connections routed through
	// SSH gateways or proxies): "127.0.0.1" for a guacd on this host, "host.docker.internal" for a container.
	GuacdForwardHost string `json:"guacdForwardHost,omitempty"`
}

func (h *handler) globalSettings(ctx context.Context) globalSettings {
	var g globalSettings
	if h.d == nil || h.d.Store == nil {
		return g
	}
	var raw json.RawMessage
	ok, err := h.d.Store.Settings.GetJSON(ctx, store.ScopeGlobal, settingsKey, &raw)
	if err != nil || !ok {
		return g
	}
	_ = json.Unmarshal(raw, &g) // ignore unrelated keys / malformed values
	g.GuacdAddress = strings.TrimSpace(g.GuacdAddress)
	g.DefaultEngine = strings.ToLower(strings.TrimSpace(g.DefaultEngine))
	return g
}

// updateGlobalSettings merges patch into the module's global settings section (nil values delete keys).
func (h *handler) updateGlobalSettings(ctx context.Context, patch map[string]any) error {
	return h.d.Store.Settings.Update(ctx, store.ScopeGlobal, func(cur map[string]json.RawMessage) (map[string]json.RawMessage, []string, error) {
		sec := map[string]any{}
		if raw, ok := cur[settingsKey]; ok {
			_ = json.Unmarshal(raw, &sec)
		}
		for k, v := range patch {
			if v == nil {
				delete(sec, k)
			} else {
				sec[k] = v
			}
		}
		b, err := json.Marshal(sec)
		if err != nil {
			return nil, nil, err
		}
		return map[string]json.RawMessage{settingsKey: b}, nil, nil
	})
}

// chooseEngine picks the engine for a connection: explicit request, then the connection option, then the admin
// default, then IronRDP.
func chooseEngine(requested string, opts rdpOptions, g globalSettings) string {
	for _, e := range []string{strings.ToLower(strings.TrimSpace(requested)), opts.Engine, g.DefaultEngine} {
		if e == engineIronRDP || e == engineGuacd {
			return e
		}
	}
	return engineIronRDP
}
