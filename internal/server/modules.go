package server

import (
	"fmt"
	"runtime/debug"

	"github.com/termstead/termstead/internal/ai"
	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/automation"
	"github.com/termstead/termstead/internal/core"
	"github.com/termstead/termstead/internal/importer"
	"github.com/termstead/termstead/internal/keys"
	"github.com/termstead/termstead/internal/monitor"
	"github.com/termstead/termstead/internal/netguard"
	"github.com/termstead/termstead/internal/oidc"
	"github.com/termstead/termstead/internal/proto/docker"
	"github.com/termstead/termstead/internal/proto/ipmi"
	"github.com/termstead/termstead/internal/proto/kube"
	"github.com/termstead/termstead/internal/proto/local"
	"github.com/termstead/termstead/internal/proto/mosh"
	"github.com/termstead/termstead/internal/proto/rawtcp"
	"github.com/termstead/termstead/internal/proto/rlogin"
	"github.com/termstead/termstead/internal/proto/serial"
	"github.com/termstead/termstead/internal/proto/telnet"
	"github.com/termstead/termstead/internal/proto/winrm"
	"github.com/termstead/termstead/internal/rdp"
	"github.com/termstead/termstead/internal/recording"
	"github.com/termstead/termstead/internal/servers"
	"github.com/termstead/termstead/internal/sshx"
	"github.com/termstead/termstead/internal/term"
	"github.com/termstead/termstead/internal/tools"
	"github.com/termstead/termstead/internal/transfer"
	"github.com/termstead/termstead/internal/tunnel"
	"github.com/termstead/termstead/internal/vfs"
	"github.com/termstead/termstead/internal/vnc"
	"github.com/termstead/termstead/internal/webauthn"
	"github.com/termstead/termstead/internal/webproxy"
)

// module is one entry of the fixed mount order. Module owners edit only their own line (SPEC §10).
type module struct {
	name  string
	mount func(d *app.Deps, c *core.Core) error
}

var modules = []module{
	{"term", func(d *app.Deps, c *core.Core) error { return term.Mount(d, c.Sessions) }},
	{"sshx", func(d *app.Deps, c *core.Core) error { return sshx.Mount(d, c.SSH) }},
	{"netguard", func(d *app.Deps, _ *core.Core) error { return netguard.Mount(d) }}, // SEC-7 destination policy API
	{"proto/local", local.Mount},
	{"proto/telnet", telnet.Mount},
	{"proto/rlogin", rlogin.Mount},
	{"proto/rawtcp", rawtcp.Mount},
	{"proto/serial", serial.Mount},
	{"proto/docker", docker.Mount},
	{"proto/kube", kube.Mount},
	{"proto/mosh", mosh.Mount},
	{"proto/winrm", winrm.Mount},
	{"proto/ipmi", ipmi.Mount},
	{"vfs", func(d *app.Deps, c *core.Core) error { // files API + transfers (SPEC §10.1: vfs also mounts transfers)
		reg, err := vfs.MountRegistry(d, c)
		if err != nil {
			return err
		}
		_, err = transfer.Mount(d, reg)
		return err
	}},
	{"tunnel", tunnel.Mount},
	{"keys", keys.Mount},
	{"vnc", vnc.Mount},
	{"rdp", rdp.Mount},
	{"monitor", monitor.Mount},
	{"tools", tools.Mount},
	{"servers", servers.Mount},
	{"automation", automation.Mount},
	{"recording", recording.Mount},
	{"importer", importer.Mount},
	{"ai", ai.Mount},
	{"webauthn", webauthn.Mount},
	{"oidc", oidc.Mount},
	{"webproxy", webproxy.Mount},
}

// mountModules mounts every module in order. A failing (or panicking) module is logged and skipped so the rest of
// the application stays usable.
func mountModules(d *app.Deps, c *core.Core) {
	for _, m := range modules {
		if err := safeMount(m, d, c); err != nil {
			d.Log.Error("module failed to mount", "module", m.name, "err", err)
		}
	}
}

func safeMount(m module, d *app.Deps, c *core.Core) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return m.mount(d, c)
}
