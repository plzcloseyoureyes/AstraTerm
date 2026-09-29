/*
 * Tunnels feature (MobaSSHTunnel, RESEARCH TUN-1…TUN-9): the singleton "tunnels" tab, the tunnel editor / remote
 * ports / import / session-forward dialogs (overlay), commands (+ the contract commands `tunnels.open` and
 * `tunnels.new`), the "Tunneling" ribbon menu, context-menu entries for SSH connections and terminals, a status bar
 * item, the settings section, and the events wiring (live statuses, failure toasts, new-listening-port toasts).
 *
 * Commands (category "Tunnels"):
 *   tunnels.open                                 open the tunnel manager tab
 *   tunnels.new {connectionId?, type?, name?, bindHost?, bindPort?, destHost?, destPort?}   new-tunnel editor, prefilled
 *   tunnels.edit {id} · tunnels.start|stop|toggle {id} · tunnels.startAll · tunnels.stopAll
 *   tunnels.detectPorts {connectionId?|sessionId?}   listening ports of an SSH server (defaults to the active terminal)
 *   tunnels.forwardPort {sessionId?|connectionId?, port, host?, open?}   forward a remote port to this machine
 *   tunnels.sessionForwards {connectionId}      edit a connection's session port forwards (options.forwards)
 *   tunnels.import · tunnels.export
 */
import { lazy } from 'react'
import { toast } from 'sonner'
import { Cable, Download, Play, Plus, Radar, Square, Upload, Waypoints } from 'lucide-react'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { RuntimeSession } from '@/api/types'
import {
  registerCommand,
  registerContextMenu,
  registerMenu,
  registerOverlay,
  registerRibbonButton,
  registerSettingsSection,
  registerStatusItem,
  registerTabKind,
  ribbonButtons,
  type MenuItem,
  type RibbonButtonDef,
} from '@/app/registry'
import { events } from '@/lib/events'
import { useAuthStore } from '@/stores/auth'
import { activeTab } from '@/stores/workspace'
import {
  exportTunnelsAction,
  forwardRemotePort,
  openTunnelsTab,
  startAllAction,
  startTunnelAction,
  stopAllAction,
  stopTunnelAction,
  toggleTunnelAction,
} from './actions'
import { cachedTunnels, ensureTunnels, installTunnelEvents } from './api'
import { isActive, isSSHConnection, statusTone, webScheme } from './model'
import { TunnelsOverlay } from './Overlay'
import { tunnelsSettings } from './settings'
import { TunnelsStatusItem } from './StatusItem'
import { closeAllTunnelDialogs, openDetectPorts, openSessionForwards, openTunnelEditor, openTunnelImport } from './store'
import type { NewTunnelArgs, PortsEvent, RemotePort, TunnelEvent } from './types'

const TunnelsView = lazy(() => import('./TunnelsView'))

registerTabKind({
  kind: 'tunnels',
  title: () => 'Tunnels',
  icon: Waypoints,
  singleton: true,
  component: TunnelsView,
})

registerOverlay({ id: 'tunnels', component: TunnelsOverlay })
registerStatusItem({ id: 'tunnels', align: 'right', order: 60, component: TunnelsStatusItem })

registerSettingsSection({
  id: 'tunnels',
  title: 'Tunnels',
  icon: Waypoints,
  order: 55,
  group: 'connections',
  keywords: ['port forwarding', 'ssh tunnel', 'socks', 'proxy', 'listening ports', 'forward'],
  component: lazy(() => import('./SettingsSection')),
})

// --- commands ----------------------------------------------------------------------------------------------------------

const CATEGORY = 'Tunnels'

function findTunnel(id: unknown) {
  return typeof id === 'string' ? cachedTunnels().find((t) => t.id === id) : undefined
}

async function withTunnel(args: unknown, fn: (t: NonNullable<ReturnType<typeof findTunnel>>) => Promise<unknown> | void): Promise<void> {
  const id = (args as { id?: unknown } | undefined)?.id
  let t = findTunnel(id)
  if (!t && typeof id === 'string') t = (await ensureTunnels()).find((x) => x.id === id)
  if (!t) {
    toast.error('Tunnel not found')
    return
  }
  await fn(t)
}

/** The SSH session of the active terminal tab, if any. */
function activeSSHSession(): RuntimeSession | undefined {
  const tab = activeTab()
  const sessionId = (tab?.params as { sessionId?: unknown } | undefined)?.sessionId
  if (typeof sessionId !== 'string') return undefined
  const s = queryClient.getQueryData<RuntimeSession[]>(queryKeys.sessions)?.find((x) => x.id === sessionId)
  return s?.protocol === 'ssh' ? s : undefined
}

registerCommand({
  id: 'tunnels.open',
  title: 'Open Tunnel Manager',
  category: CATEGORY,
  icon: Waypoints,
  keywords: ['port forwarding', 'ssh tunnel', 'mobasshtunnel', 'socks'],
  run: () => openTunnelsTab(),
})

registerCommand<NewTunnelArgs | undefined>({
  id: 'tunnels.new',
  title: 'New Tunnel…',
  category: CATEGORY,
  icon: Plus,
  keywords: ['port forward', 'local forward', 'remote forward', 'socks proxy'],
  run: ({ args }) => {
    const a = args && typeof args === 'object' ? args : undefined
    if (!a?.connectionId) {
      const s = activeSSHSession()
      if (s?.connectionId) {
        openTunnelEditor({ mode: 'create', initial: { ...a, connectionId: s.connectionId } })
        return
      }
    }
    openTunnelEditor({ mode: 'create', initial: a })
  },
})

registerCommand<{ id: string }>({
  id: 'tunnels.edit',
  title: 'Edit Tunnel…',
  category: CATEGORY,
  hidden: true,
  run: ({ args }) => withTunnel(args, (t) => openTunnelEditor({ mode: 'edit', id: t.id })),
})

registerCommand<{ id: string }>({ id: 'tunnels.start', title: 'Start Tunnel', category: CATEGORY, hidden: true, run: ({ args }) => withTunnel(args, startTunnelAction) })
registerCommand<{ id: string }>({ id: 'tunnels.stop', title: 'Stop Tunnel', category: CATEGORY, hidden: true, run: ({ args }) => withTunnel(args, stopTunnelAction) })
registerCommand<{ id: string }>({ id: 'tunnels.toggle', title: 'Start / Stop Tunnel', category: CATEGORY, hidden: true, run: ({ args }) => withTunnel(args, toggleTunnelAction) })

registerCommand({ id: 'tunnels.startAll', title: 'Start All Tunnels', category: CATEGORY, icon: Play, run: () => startAllAction() })
registerCommand({ id: 'tunnels.stopAll', title: 'Stop All Tunnels', category: CATEGORY, icon: Square, run: () => stopAllAction() })

registerCommand<{ connectionId?: string; sessionId?: string } | undefined>({
  id: 'tunnels.detectPorts',
  title: 'Detect Listening Ports on an SSH Server…',
  category: CATEGORY,
  icon: Radar,
  keywords: ['remote ports', 'listening', 'forward port', 'netstat'],
  run: ({ args }) => {
    if (args?.connectionId || args?.sessionId) {
      openDetectPorts(args)
      return
    }
    const s = activeSSHSession()
    openDetectPorts(s ? { sessionId: s.id } : {})
  },
})

registerCommand<{ sessionId?: string; connectionId?: string; port: number; host?: string; open?: boolean }>({
  id: 'tunnels.forwardPort',
  title: 'Forward a Remote Port',
  category: CATEGORY,
  hidden: true,
  run: async ({ args }) => {
    if (!args || !Number.isInteger(args.port) || (!args.sessionId && !args.connectionId)) throw new Error('port and sessionId or connectionId are required')
    const p: RemotePort = { address: args.host ?? '127.0.0.1', port: args.port, scope: 'loopback', connectHost: args.host ?? 'localhost' }
    await forwardRemotePort(p, { sessionId: args.sessionId, connectionId: args.connectionId }, { open: !!args.open })
  },
})

registerCommand<{ connectionId: string }>({
  id: 'tunnels.sessionForwards',
  title: 'Session Port Forwards…',
  category: CATEGORY,
  icon: Cable,
  hidden: true,
  run: ({ args }) => {
    const id = args?.connectionId ?? activeSSHSession()?.connectionId
    if (!id) throw new Error('choose an SSH connection')
    openSessionForwards(id)
  },
})

registerCommand({ id: 'tunnels.import', title: 'Import Tunnels…', category: CATEGORY, icon: Upload, run: () => openTunnelImport() })
registerCommand({ id: 'tunnels.export', title: 'Export Tunnels…', category: CATEGORY, icon: Download, run: () => exportTunnelsAction() })

// --- ribbon, menus, context menus --------------------------------------------------------------------------------------

function tunnelToggleItems(): MenuItem[] {
  const list = cachedTunnels()
  if (!list.length) {
    void ensureTunnels().catch(() => undefined)
    return []
  }
  const items: MenuItem[] = [{ type: 'label', label: 'Tunnels' }]
  for (const t of list.slice(0, 30)) {
    items.push({ id: `tunnel-${t.id}`, label: t.name, checked: isActive(t.status), run: () => void toggleTunnelAction(t) })
  }
  if (list.length > 30) items.push({ label: `All ${list.length} tunnels…`, run: openTunnelsTab })
  return items
}

// The shell registers a placeholder "Tunneling" ribbon button (layout/builtins.tsx, loaded with the app shell — after
// the features). Keep ours (same command, plus a drop-down) in place whenever the ribbon registry changes.
const tunnelingButton: RibbonButtonDef = {
  id: 'tunneling',
  label: 'Tunneling',
  icon: Waypoints,
  order: 80,
  command: 'tunnels.open',
  tooltip: 'SSH tunnels (port forwarding)',
  menu: () => {
    const list = cachedTunnels()
    const running = list.filter((t) => isActive(t.status)).length
    return [
      { label: 'Tunnel manager', icon: Waypoints, command: 'tunnels.open' },
      { label: 'New tunnel…', icon: Plus, command: 'tunnels.new' },
      { label: 'Detect listening ports…', icon: Radar, command: 'tunnels.detectPorts' },
      { type: 'separator' },
      { label: 'Start all', icon: Play, command: 'tunnels.startAll', disabled: !list.length || running === list.length },
      { label: 'Stop all', icon: Square, command: 'tunnels.stopAll', disabled: !running },
      { type: 'separator' },
      ...tunnelToggleItems(),
    ]
  },
}
function ensureRibbonButton(): void {
  if (ribbonButtons.get('tunneling') !== tunnelingButton) registerRibbonButton(tunnelingButton)
}
ensureRibbonButton()
ribbonButtons.subscribe(ensureRibbonButton)

registerMenu({
  menu: 'tools',
  order: 110,
  items: () => [
    { label: 'New tunnel…', icon: Plus, command: 'tunnels.new' },
    { label: 'Detect listening ports…', icon: Radar, command: 'tunnels.detectPorts' },
  ],
})

function forwardingSubmenu(connectionId: string | undefined, sessionId: string | undefined): MenuItem {
  return {
    type: 'submenu',
    label: 'Port forwarding',
    icon: Waypoints,
    items: () => [
      { label: 'New tunnel through this server…', icon: Plus, disabled: !connectionId, run: () => openTunnelEditor({ mode: 'create', initial: { connectionId } }) },
      {
        label: 'Listening ports…',
        icon: Radar,
        run: () => openDetectPorts(sessionId ? { sessionId } : { connectionId }),
      },
      {
        label: 'Session port forwards…',
        icon: Cable,
        disabled: !connectionId,
        run: () => connectionId && openSessionForwards(connectionId),
      },
      { type: 'separator' },
      { label: 'Tunnel manager', icon: Waypoints, command: 'tunnels.open' },
    ],
  }
}

registerContextMenu({
  target: 'session-node',
  order: 60,
  items: (ctx) => {
    const c = ctx.connection
    if (!c || (ctx.selection && ctx.selection.length > 1) || !isSSHConnection(c)) return []
    return [forwardingSubmenu(c.id, undefined)]
  },
})

registerContextMenu({
  target: 'terminal',
  order: 60,
  items: (ctx) => {
    const s = ctx.session
    if (!s || s.protocol !== 'ssh') return []
    return [forwardingSubmenu(s.connectionId, s.state === 'connected' ? s.id : undefined)]
  },
})

// --- events ------------------------------------------------------------------------------------------------------------

installTunnelEvents()

// Failure toasts: a tunnel that turns to "error".
const lastTone = new Map<string, string>()
events.on('tunnel', (raw) => {
  const ev = raw as unknown as TunnelEvent
  if (!ev?.id || !ev.status) return
  const tone = statusTone(ev.status)
  const prev = lastTone.get(ev.id)
  lastTone.set(ev.id, tone)
  if (ev.change === 'deleted') lastTone.delete(ev.id)
  // Only a tunnel that was running / starting failing is news: a start refused right away already showed its error.
  if (tone !== 'error' || prev === undefined || prev === 'error' || prev === 'stopped' || !tunnelsSettings.get().notifyErrors) return
  const t = cachedTunnels().find((x) => x.id === ev.id)
  toast.error(`Tunnel “${t?.name ?? 'tunnel'}” failed`, {
    description: ev.status.error,
    duration: 10_000,
    action: { label: 'Open', onClick: openTunnelsTab },
  })
})
// Seed the known states so an already failed tunnel does not toast when the list first loads.
queryClient.getQueryCache().subscribe((e) => {
  if (e.type !== 'updated' || e.query.queryKey[0] !== 'tunnels' || e.query.queryKey.length !== 1) return
  for (const t of cachedTunnels()) if (!lastTone.has(t.id)) lastTone.set(t.id, statusTone(t.status))
})

// New listening ports (TUN-9 watcher): offer to forward / open them.
const recentlyToasted = new Map<string, number>()
events.onAny((raw) => {
  const ev = raw as unknown as PortsEvent
  if (ev.type !== 'tunnel.ports' || ev.initial || !ev.added?.length || !tunnelsSettings.get().watchPorts) return
  const session = queryClient.getQueryData<RuntimeSession[]>(queryKeys.sessions)?.find((s) => s.id === ev.sessionId)
  for (const p of ev.added.slice(0, 3)) {
    // sshd's own listeners are remote forwards (ours or someone else's), not services.
    if (p.process === 'sshd' || p.process?.startsWith('sshd:')) continue
    const key = `${ev.sessionId}:${p.connectHost}:${p.port}`
    const last = recentlyToasted.get(key) ?? 0
    if (Date.now() - last < 60_000) continue
    recentlyToasted.set(key, Date.now())
    const web = !!webScheme(p.port)
    toast.info(`Port ${p.port} is now listening${session ? ` on ${session.title}` : ''}`, {
      description: p.process ? `${p.process}${p.pid ? ` (pid ${p.pid})` : ''}` : undefined,
      duration: 15_000,
      action: {
        label: web ? 'Forward & open' : 'Forward',
        onClick: () => void forwardRemotePort(p, { sessionId: ev.sessionId }, { open: web }),
      },
      cancel: { label: 'Ports…', onClick: () => openDetectPorts({ sessionId: ev.sessionId }) },
    })
  }
})

// Dialogs never survive a sign-out.
useAuthStore.subscribe((s, prev) => {
  if (prev.user && !s.user) {
    closeAllTunnelDialogs()
    lastTone.clear()
  }
})
