// Package servers implements Termstead's embedded servers (RESEARCH SRV-1…6, CC-5): an HTTP(S) file server, TFTP,
// FTP(S), SSH/SFTP (optional shell), Telnet and a syslog receiver. They run on the Termstead host with persisted
// configurations, autostart, automatic stop, per-server activity logs, connected-client lists and live status events.
// Desktop mode: any signed-in user; server mode: administrators only (SPEC principle 7).
//
// REST (all under /api):
//
//	GET    /servers                        → Status[]
//	GET    /servers/host                   → HostInfo (interfaces, OS user, default shared folder, privileged ports)
//	POST   /servers/stop-all               → Status[]
//	GET    /servers/:kind                  → Status
//	PUT    /servers/:kind                  → Status   (configuration; a running server restarts)
//	POST   /servers/:kind/start|stop|restart → Status
//	GET    /servers/:kind/logs?after=&limit= → {entries, lastId}
//	DELETE /servers/:kind/logs
//	GET    /servers/:kind/clients          → ClientInfo[]
//	DELETE /servers/:kind/clients/:id      → disconnect a client
//	GET    /servers/syslog/messages?q= (or filter=)&regex=&severity=&facility=&host=&app=&after=&before=&limit= → SyslogPage
//	DELETE /servers/syslog/messages
//	GET    /servers/syslog/export?…        → text/plain attachment (same filters)
//
// Events (topics on /ws/events, same access rule): "servers" → {type:'server', status}; "servers.log" {kind} →
// {type:'server.log', kind, entries, dropped?}; "syslog" → {type:'syslog', messages, dropped?}.
package servers

import (
	"context"
	"sync"
	"time"

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/core"
)

// Mount creates the servers manager, loads the stored configurations, registers the REST routes and event topics,
// and starts the servers marked for autostart.
func Mount(d *app.Deps, _ *core.Core) error {
	_, err := mount(d, true)
	return err
}

// managers maps the deps a manager was mounted with to it until its shutdown finished (see WaitShutdown).
var (
	managersMu sync.Mutex
	managers   = map[*app.Deps]*Manager{}
)

// WaitShutdown blocks until the servers mounted on d have stopped after d.Ctx was cancelled — clients disconnected,
// syslog files flushed and closed — or ctx ends. internal/server calls it from Close before the process exits.
func WaitShutdown(ctx context.Context, d *app.Deps) error {
	managersMu.Lock()
	m := managers[d]
	managersMu.Unlock()
	if m == nil {
		return nil
	}
	return m.Wait(ctx)
}

func mount(d *app.Deps, autostart bool) (*Manager, error) {
	m := newManager(d)
	managersMu.Lock()
	managers[d] = m
	managersMu.Unlock()
	go func() {
		<-m.done
		managersMu.Lock()
		if managers[d] == m {
			delete(managers, d)
		}
		managersMu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(d.Ctx, 10*time.Second)
	m.loadConfigs(ctx)
	cancel()
	h := &handler{d: d, m: m}
	h.mount()
	m.subs.register()
	go m.run()
	if d.Cfg != nil && d.Cfg.IsServer() {
		go m.subs.revalidateLoop(d.Ctx, 30*time.Second)
	}
	if autostart {
		go m.autostart()
	}
	return m, nil
}
