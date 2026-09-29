// Package tunnel is Termstead's SSH port-forward manager (MobaSSHTunnel, RESEARCH TUN-1…TUN-9):
//
//   - saved tunnels (core `tunnels` table + module table tunnel_meta): local (-L), remote (-R), dynamic SOCKS/HTTP
//     proxies (-D) and reverse dynamic proxies (-R SOCKS), each optionally on Unix sockets;
//   - one supervised runner per started tunnel that holds its own pooled SSH client (so tunnels outlive terminals),
//     reconnects with backoff when the link drops, waits for the vault / an interactive window when needed, and
//     supports on-demand connections; autoStart tunnels start with the server;
//   - live status (state, connections, traffic, rates, bound addresses) pushed as {type:'tunnel'} events;
//   - session-integrated forwards (connection.options.forwards, ad-hoc forwards on live sessions) that follow an SSH
//     runtime session's connections;
//   - remote listening-port detection and a polling watcher topic ("tunnel.ports").
//
// REST (all authenticated, own tunnels only): see handlers.go and SPEC §9 "tunnels".
package tunnel

import (
	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/core"
)

// Mount wires the tunnel manager: REST routes, the session hooks for session-integrated forwards, the
// "tunnel.ports" events topic, the status publisher and the autostart of saved tunnels.
func Mount(d *app.Deps, c *core.Core) error {
	_, err := mount(d, c, true)
	return err
}

func mount(d *app.Deps, c *core.Core, autostart bool) (*Manager, error) {
	if err := d.Store.Migrate(d.Ctx); err != nil { // the module table is needed by the autostart below
		return nil, err
	}
	m := newManager(d, c.SSH, c.Sessions)
	h := &handler{d: d, m: m}
	h.mount()
	m.sf.install()
	d.Events.RegisterTopic(evPorts, m.portsTopic)
	go m.run()
	if autostart {
		go m.autostartAll()
	}
	return m, nil
}
