package server

import (
	"fmt"
	"runtime/debug"

	"github.com/nexterm/nexterm/internal/ai"
	"github.com/nexterm/nexterm/internal/app"
	"github.com/nexterm/nexterm/internal/automation"
	"github.com/nexterm/nexterm/internal/core"
	"github.com/nexterm/nexterm/internal/importer"
	"github.com/nexterm/nexterm/internal/keys"
	"github.com/nexterm/nexterm/internal/monitor"
	"github.com/nexterm/nexterm/internal/netguard"
	"github.com/nexterm/nexterm/internal/oidc"
	"github.com/nexterm/nexterm/internal/proto/docker"
	"github.com/nexterm/nexterm/internal/proto/ipmi"
	"github.com/nexterm/nexterm/internal/proto/kube"
	"github.com/nexterm/nexterm/internal/proto/local"
	"github.com/nexterm/nexterm/internal/proto/mosh"
	"github.com/nexterm/nexterm/internal/proto/rawtcp"
	"github.com/nexterm/nexterm/internal/proto/rlogin"
	"github.com/nexterm/nexterm/internal/proto/serial"
	"github.com/nexterm/nexterm/internal/proto/telnet"
	"github.com/nexterm/nexterm/internal/proto/winrm"
	"github.com/nexterm/nexterm/internal/rdp"
	"github.com/nexterm/nexterm/internal/recording"
	"github.com/nexterm/nexterm/internal/servers"
	"github.com/nexterm/nexterm/internal/sshx"
	"github.com/nexterm/nexterm/internal/term"
	"github.com/nexterm/nexterm/internal/tools"
	"github.com/nexterm/nexterm/internal/transfer"
	"github.com/nexterm/nexterm/internal/tunnel"
	"github.com/nexterm/nexterm/internal/vfs"
	"github.com/nexterm/nexterm/internal/vnc"
	"github.com/nexterm/nexterm/internal/webauthn"
	"github.com/nexterm/nexterm/internal/webproxy"
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
