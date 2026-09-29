/*
 * Embedded servers feature (RESEARCH SRV-1…6 and CC-5): the singleton "servers" tab (server
 * cards with start / stop toggles), the "syslog" viewer tab, the configuration dialog and activity drawer (overlay),
 * commands (+ the contract command `servers.open`), the "Servers" ribbon drop-down, a status bar item, a settings
 * section and the live status events.
 *
 * Commands (category "Servers"):
 *   servers.open                      open the server manager tab
 *   servers.syslog                    open the syslog viewer
 *   servers.start|stop|toggle|restart {kind}   control one server (kind: http | ftp | sftp | tftp | telnet | syslog)
 *   servers.configure {kind, tab?}    open the configuration dialog
 *   servers.activity {kind, tab?}     open the activity drawer (tab: log | clients | connect)
 *   servers.stopAll                   stop every running server
 *   servers.toggle.<kind>             start / stop one server (palette, bindable)
 */
import { lazy } from 'react'
import { toast } from 'sonner'
import { CircleStop, Logs, Play, RotateCw, ScrollText, Server, Settings2 } from 'lucide-react'
import {
  registerCommand,
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
import {
  openServersTab,
  openSyslogTab,
  restartServerAction,
  startServerAction,
  stopAllAction,
  stopServerAction,
  toggleServerAction,
} from './actions'
import { cachedServers, ensureServers, installServerEvents, serversAllowed } from './api'
import { KINDS, KIND_ORDER, isKind } from './model'
import { ServersOverlay } from './Overlay'
import { ServersStatusItem } from './StatusItem'
import { closeServerConfig, closeServerDrawer, openServerConfig, openServerDrawer, type DrawerTab } from './store'
import type { ServerKindEx, ServerStatusEvent } from './types'

const ServersView = lazy(() => import('./ServersView'))
const SyslogView = lazy(() => import('./SyslogView'))

registerTabKind({
  kind: 'servers',
  title: () => 'Servers',
  icon: Server,
  singleton: true,
  component: ServersView,
})

registerTabKind({
  kind: 'syslog',
  title: () => 'Syslog',
  icon: ScrollText,
  singleton: true,
  component: SyslogView,
})

registerOverlay({ id: 'servers', component: ServersOverlay })
registerStatusItem({ id: 'servers', align: 'right', order: 58, component: ServersStatusItem })

registerSettingsSection({
  id: 'servers',
  title: 'Servers',
  icon: Server,
  order: 57,
  group: 'integrations',
  keywords: ['http server', 'ftp server', 'sftp server', 'tftp', 'telnet server', 'syslog', 'embedded servers'],
  component: lazy(() => import('./SettingsSection')),
})

// --- commands ----------------------------------------------------------------------------------------------------------

const CATEGORY = 'Servers'

interface KindArgs {
  kind?: unknown
  tab?: unknown
}

function kindArg(args: unknown): ServerKindEx {
  const k = (args as KindArgs | undefined)?.kind
  if (!isKind(k)) throw new Error('Unknown server (expected http, ftp, sftp, tftp, telnet or syslog)')
  return k
}

registerCommand({
  id: 'servers.open',
  title: 'Open Servers',
  category: CATEGORY,
  icon: Server,
  keywords: ['http server', 'ftp server', 'sftp server', 'tftp', 'telnet server', 'syslog'],
  run: () => openServersTab(),
})

registerCommand({
  id: 'servers.syslog',
  title: 'Open Syslog Viewer',
  category: CATEGORY,
  icon: ScrollText,
  keywords: ['syslog', 'logs', 'network devices', 'tftpd64'],
  when: serversAllowed,
  run: () => openSyslogTab(),
})

registerCommand<KindArgs>({
  id: 'servers.start',
  title: 'Start Server',
  category: CATEGORY,
  icon: Play,
  hidden: true,
  when: serversAllowed,
  run: ({ args }) => startServerAction(kindArg(args)),
})

registerCommand<KindArgs>({
  id: 'servers.stop',
  title: 'Stop Server',
  category: CATEGORY,
  icon: CircleStop,
  hidden: true,
  when: serversAllowed,
  run: ({ args }) => stopServerAction(kindArg(args)),
})

registerCommand<KindArgs>({
  id: 'servers.toggle',
  title: 'Start / Stop Server',
  category: CATEGORY,
  hidden: true,
  when: serversAllowed,
  run: ({ args }) => toggleServerAction(kindArg(args)),
})

registerCommand<KindArgs>({
  id: 'servers.restart',
  title: 'Restart Server',
  category: CATEGORY,
  icon: RotateCw,
  hidden: true,
  when: serversAllowed,
  run: ({ args }) => restartServerAction(kindArg(args)),
})

registerCommand<KindArgs>({
  id: 'servers.configure',
  title: 'Configure Server…',
  category: CATEGORY,
  icon: Settings2,
  hidden: true,
  when: serversAllowed,
  run: ({ args }) => openServerConfig(kindArg(args), typeof args?.tab === 'string' ? args.tab : undefined),
})

registerCommand<KindArgs>({
  id: 'servers.activity',
  title: 'Server Activity',
  category: CATEGORY,
  icon: Logs,
  hidden: true,
  when: serversAllowed,
  run: ({ args }) => {
    const tab = args?.tab === 'clients' || args?.tab === 'connect' ? (args.tab as DrawerTab) : 'log'
    openServerDrawer(kindArg(args), tab)
  },
})

registerCommand({
  id: 'servers.stopAll',
  title: 'Stop All Servers',
  category: CATEGORY,
  icon: CircleStop,
  when: serversAllowed,
  run: () => stopAllAction(),
})

for (const kind of KIND_ORDER) {
  const info = KINDS[kind]
  registerCommand({
    id: `servers.toggle.${kind}`,
    title: `Start / Stop ${info.label}`,
    category: CATEGORY,
    icon: info.icon,
    keywords: [info.short, 'server', 'start', 'stop'],
    when: serversAllowed,
    run: () => toggleServerAction(kind),
  })
  registerCommand({
    id: `servers.configure.${kind}`,
    title: `Configure ${info.label}…`,
    category: CATEGORY,
    icon: Settings2,
    keywords: [info.short, 'server', 'settings'],
    when: serversAllowed,
    run: () => openServerConfig(kind),
  })
}

// --- ribbon & menus ------------------------------------------------------------------------------------------------------

function serverItems(): MenuItem[] {
  const list = cachedServers()
  if (!list.length) {
    if (serversAllowed()) void ensureServers().catch(() => undefined)
    return [{ type: 'label', label: 'Loading servers…' }]
  }
  return KIND_ORDER.map((k) => {
    const st = list.find((s) => s.kind === k)
    const info = KINDS[k]
    const port = Number(st?.config.port ?? info.defaultPort)
    return {
      id: `server-${k}`,
      label: `${info.label} (${port})`,
      icon: info.icon,
      checked: !!st?.running,
      disabled: st?.state === 'starting' || st?.state === 'stopping',
      run: () => void toggleServerAction(k),
    }
  })
}

// The shell registers a placeholder "Servers" ribbon button (layout/builtins.tsx). Keep ours (same command, plus a
// drop-down with the per-server toggles) in place whenever the ribbon registry changes.
const serversButton: RibbonButtonDef = {
  id: 'servers',
  label: 'Servers',
  icon: Server,
  order: 20,
  command: 'servers.open',
  tooltip: 'Embedded servers (HTTP, FTP, SFTP, TFTP, Telnet, Syslog)',
  menu: () => {
    if (!serversAllowed()) return [{ label: 'Servers (administrators only)', icon: Server, command: 'servers.open' }]
    const running = cachedServers().some((s) => s.running)
    return [
      { label: 'Server manager', icon: Server, command: 'servers.open' },
      { label: 'Syslog viewer', icon: ScrollText, command: 'servers.syslog' },
      { type: 'separator' },
      { type: 'label', label: 'Start / stop' },
      ...serverItems(),
      { type: 'separator' },
      { label: 'Stop all servers', icon: CircleStop, command: 'servers.stopAll', disabled: !running },
    ]
  },
}

function ensureRibbonButton(): void {
  if (ribbonButtons.get('servers') !== serversButton) registerRibbonButton(serversButton)
}
ensureRibbonButton()
ribbonButtons.subscribe(ensureRibbonButton)

registerMenu({
  menu: 'tools',
  order: 120,
  items: () => (serversAllowed() ? [{ label: 'Syslog viewer', icon: ScrollText, command: 'servers.syslog' }] : []),
})

// --- events --------------------------------------------------------------------------------------------------------------

installServerEvents()

// A running server that fails on its own (listener error) is worth a toast; start failures are reported by the action.
const lastState = new Map<ServerKindEx, string>()
events.on('server', (raw) => {
  const st = (raw as unknown as ServerStatusEvent).status
  if (!st || !isKind(st.kind)) return
  const prev = lastState.get(st.kind)
  lastState.set(st.kind, st.state)
  if (st.state === 'error' && st.errorCode === 'failed' && prev && prev !== 'error' && prev !== 'starting') {
    toast.error(`${KINDS[st.kind].label} stopped`, {
      description: st.error,
      duration: 10_000,
      action: { label: 'Open', onClick: openServersTab },
    })
  }
})

// Dialogs never survive a sign-out.
useAuthStore.subscribe((s, prev) => {
  if (prev.user && !s.user) {
    closeServerConfig()
    closeServerDrawer()
    lastState.clear()
  }
})
