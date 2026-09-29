/*
 * Monitor feature (MON-1..6, SSH-38 display, SEC-22): the remote monitoring bar in the status bar, the
 * per-session 'monitor' tab (live charts, processes, services, ports, disk usage, logs, SSH connection), the local
 * 'sysinfo' tab, and Caffeine.
 *
 * Commands (category Monitor):
 *   monitor.open {sessionId?|target?, panel?}   host monitor of a session (default: the active tab / terminal)
 *   monitor.toggleBar {enabled?}                remote monitoring bar on / off (definition carries `checked()`)
 *   monitor.processes | services | ports | diskUsage | logs | connectionInfo   monitor tab on that panel
 *   monitor.systemInfo, monitor.taskManager     the Termstead host (desktop mode / admins)
 *   monitor.caffeine.toggle | on {minutes?} | off
 */
import { lazy } from 'react'
import { toast } from 'sonner'
import { Activity, Cable, Coffee, Cog, HardDrive, ListTree, Monitor, MonitorCog, Plug, ScrollText } from 'lucide-react'
import {
  registerCommand,
  registerContextMenu,
  registerMenu,
  registerOverlay,
  registerSettingsSection,
  registerStatusItem,
  registerTabKind,
  type CommandDef,
  type MenuItem,
} from '@/app/registry'
import { getActiveTerminal } from '@/features/terminal/bus'
import { CaffeineKeeper, CaffeineStatusItem, CAFFEINE_DURATIONS, caffeineStatus, toggleCaffeine } from './caffeine'
import { MonitorBar } from './MonitorBar'
import { cachedSession, canMonitorLocal, isMonitorable, MONITOR_KIND, noTargetToast, openMonitor, openSystemInfo, resolveMonitorTarget, SYSINFO_KIND } from './open'
import { getMonitorBarOverride, isMonitorBarEnabled, setMonitorBarEnabled, setMonitorBarForSession } from './settings'
import type { MonitorPanel, MonitorTabParams, SysInfoTabParams } from './types'

const MonitorView = lazy(() => import('./MonitorView'))
const SystemInfoView = lazy(() => import('./SystemInfoView'))
const MonitoringSettings = lazy(() => import('./SettingsSection'))

const MONITORABLE = new Set(['ssh', 'local'])

registerTabKind<MonitorTabParams>({
  kind: MONITOR_KIND,
  title: (p) => (p?.title ? `Monitor · ${p.title}` : 'Monitor'),
  icon: Activity,
  // (the shell would otherwise show the protocol icon of params.protocol)
  iconFor: () => Activity,
  component: MonitorView,
  // One monitor tab per session: "duplicate" focuses it rather than opening a copy.
  duplicate: (tab) => void openMonitor(tab.params?.target ?? tab.params?.sessionId ?? '', tab.params?.panel),
})

registerTabKind<SysInfoTabParams>({
  kind: SYSINFO_KIND,
  title: () => 'System information',
  icon: MonitorCog,
  singleton: true,
  component: SystemInfoView,
})

registerStatusItem({ id: 'monitor.bar', align: 'left', order: 30, component: MonitorBar })
registerStatusItem({ id: 'monitor.caffeine', align: 'right', order: 80, component: CaffeineStatusItem })
registerOverlay({ id: 'monitor.caffeine', component: CaffeineKeeper })

registerSettingsSection({
  id: 'monitor',
  title: 'Monitoring',
  icon: Activity,
  order: 45,
  group: 'connections',
  keywords: ['remote monitoring', 'status bar', 'cpu', 'memory', 'ram', 'disk', 'network', 'threshold', 'processes', 'logs', 'caffeine', 'sleep'],
  component: MonitoringSettings,
})

// --- commands ---------------------------------------------------------------------------------------------------------

function openFor(panel: MonitorPanel) {
  return ({ args }: { args: unknown }) => {
    const a = args as { target?: unknown; sessionId?: unknown; panel?: unknown } | undefined
    if (a?.target === 'local' || a?.sessionId === 'local') {
      if (canMonitorLocal()) openSystemInfo(panel === 'connection' ? 'overview' : panel)
      return
    }
    const target = resolveMonitorTarget(args, () => getActiveTerminal()?.sessionId)
    if (!target) {
      noTargetToast()
      return
    }
    const s = cachedSession(target)
    if (s && !isMonitorable(s)) {
      toast.info('This session cannot be monitored', { description: `Host monitoring works for SSH sessions and local shells, not ${s.protocol}.` })
      return
    }
    const p = typeof a?.panel === 'string' ? (a.panel as MonitorPanel) : panel
    openMonitor(target, p)
  }
}

registerCommand({
  id: 'monitor.open',
  title: 'Monitor Host',
  category: 'Monitor',
  icon: Activity,
  keywords: ['monitoring', 'cpu', 'memory', 'dashboard', 'top', 'htop', 'host'],
  description: 'Live CPU, memory, disk, network, processes, services and logs of the session host',
  run: openFor('overview'),
})

const toggleBar: CommandDef<{ enabled?: boolean; sessionId?: string } | undefined> & { checked: () => boolean } = {
  id: 'monitor.toggleBar',
  title: 'Toggle Remote Monitoring Bar',
  category: 'View',
  icon: Activity,
  keywords: ['monitoring', 'status bar', 'cpu', 'ram'],
  description: 'Show or hide the remote monitoring bar (for all sessions, or one session with {sessionId})',
  run: ({ args }) => {
    const sessionId = typeof args?.sessionId === 'string' && args.sessionId ? args.sessionId : undefined
    if (sessionId) {
      const current = getMonitorBarOverride(sessionId) ?? isMonitorBarEnabled()
      setMonitorBarForSession(sessionId, typeof args?.enabled === 'boolean' ? args.enabled : !current)
      return
    }
    setMonitorBarEnabled(typeof args?.enabled === 'boolean' ? args.enabled : !isMonitorBarEnabled())
  },
  checked: () => isMonitorBarEnabled(),
}
registerCommand(toggleBar)

const panelCommands: { id: string; title: string; panel: MonitorPanel; icon: typeof Activity; keywords: string[] }[] = [
  { id: 'monitor.processes', title: 'Monitor: Processes', panel: 'processes', icon: ListTree, keywords: ['task manager', 'kill', 'top', 'ps'] },
  { id: 'monitor.services', title: 'Monitor: Services', panel: 'services', icon: Cog, keywords: ['systemd', 'systemctl', 'restart'] },
  { id: 'monitor.ports', title: 'Monitor: Listening Ports', panel: 'ports', icon: Plug, keywords: ['netstat', 'ss', 'sockets'] },
  { id: 'monitor.diskUsage', title: 'Monitor: Disk Usage', panel: 'disk', icon: HardDrive, keywords: ['du', 'space', 'ncdu'] },
  { id: 'monitor.logs', title: 'Monitor: Follow Logs', panel: 'logs', icon: ScrollText, keywords: ['tail', 'journalctl', 'syslog'] },
  { id: 'monitor.connectionInfo', title: 'Monitor: SSH Connection Details', panel: 'connection', icon: Cable, keywords: ['latency', 'cipher', 'kex', 'fingerprint'] },
]
for (const c of panelCommands) {
  registerCommand({ id: c.id, title: c.title, category: 'Monitor', icon: c.icon, keywords: c.keywords, run: openFor(c.panel) })
}

registerCommand({
  id: 'monitor.systemInfo',
  title: 'System Information',
  category: 'Tools',
  icon: MonitorCog,
  keywords: ['swinfo', 'hwinfo', 'hardware', 'software', 'this computer', 'local host'],
  description: 'Hardware, software and live statistics of the computer Termstead runs on',
  run: () => void openSystemInfo(),
  when: canMonitorLocal,
})

registerCommand({
  id: 'monitor.taskManager',
  title: 'Task Manager (this computer)',
  category: 'Tools',
  icon: ListTree,
  keywords: ['tasklist', 'killtask', 'processes', 'kill'],
  run: () => void openSystemInfo('processes'),
  when: canMonitorLocal,
})

const caffeineAllowed = () => canMonitorLocal() && caffeineStatus()?.supported !== false

registerCommand({
  id: 'monitor.caffeine.toggle',
  title: 'Caffeine: Keep This Computer Awake',
  category: 'Tools',
  icon: Coffee,
  keywords: ['sleep', 'awake', 'screen saver', 'caffeinate', 'prevent sleep'],
  run: () => toggleCaffeine(),
  when: caffeineAllowed,
})
registerCommand<{ minutes?: number } | undefined>({
  id: 'monitor.caffeine.on',
  title: 'Caffeine: On',
  category: 'Tools',
  icon: Coffee,
  hidden: true,
  run: ({ args }) => toggleCaffeine(true, typeof args?.minutes === 'number' ? args.minutes : 0),
  when: caffeineAllowed,
})
registerCommand({
  id: 'monitor.caffeine.off',
  title: 'Caffeine: Off',
  category: 'Tools',
  icon: Coffee,
  hidden: true,
  run: () => toggleCaffeine(false),
  when: caffeineAllowed,
})

// --- menus -------------------------------------------------------------------------------------------------------------

registerMenu({
  id: 'monitor.view',
  menu: 'view',
  order: 320,
  items: () => [{ label: 'Remote monitoring bar', command: 'monitor.toggleBar', checked: isMonitorBarEnabled() }],
})

registerMenu({
  id: 'monitor.terminal',
  menu: 'terminal',
  order: 400,
  items: () => [{ label: 'Monitor host', icon: Activity, command: 'monitor.open' }],
})

registerMenu({
  id: 'monitor.tools',
  menu: 'tools',
  order: 400,
  items: (): MenuItem[] => {
    const caf = caffeineStatus()
    return [
      { label: 'Monitor host', icon: Activity, command: 'monitor.open' },
      { label: 'System information', icon: MonitorCog, command: 'monitor.systemInfo' },
      { label: 'Task manager (this computer)', icon: ListTree, command: 'monitor.taskManager' },
      {
        type: 'submenu',
        label: caf?.enabled ? 'Caffeine (on)' : 'Caffeine',
        icon: Coffee,
        disabled: !caffeineAllowed(),
        items: () => [
          ...CAFFEINE_DURATIONS.map((d) => ({ label: d.label, command: 'monitor.caffeine.on', args: { minutes: d.minutes } }) satisfies MenuItem),
          { type: 'separator' as const },
          { label: 'Turn off', command: 'monitor.caffeine.off', disabled: !caf?.enabled },
        ],
      },
    ]
  },
})

registerContextMenu({
  id: 'monitor.terminal-context',
  target: 'terminal',
  order: 600,
  items: (ctx) => {
    if (!ctx.sessionId || !MONITORABLE.has(ctx.session?.protocol ?? '')) return []
    return [
      {
        type: 'submenu',
        label: 'Monitor host',
        icon: Monitor,
        items: [
          { label: 'Overview', icon: Activity, command: 'monitor.open', args: { sessionId: ctx.sessionId } },
          { label: 'Processes', icon: ListTree, command: 'monitor.open', args: { sessionId: ctx.sessionId, panel: 'processes' } },
          { label: 'Services', icon: Cog, command: 'monitor.open', args: { sessionId: ctx.sessionId, panel: 'services' } },
          { label: 'Listening ports', icon: Plug, command: 'monitor.open', args: { sessionId: ctx.sessionId, panel: 'ports' } },
          { label: 'Disk usage', icon: HardDrive, command: 'monitor.open', args: { sessionId: ctx.sessionId, panel: 'disk' } },
          { label: 'Follow logs', icon: ScrollText, command: 'monitor.open', args: { sessionId: ctx.sessionId, panel: 'logs' } },
          ...(ctx.session?.protocol === 'ssh'
            ? [{ label: 'SSH connection details', icon: Cable, command: 'monitor.open', args: { sessionId: ctx.sessionId, panel: 'connection' } } satisfies MenuItem]
            : []),
        ],
      },
    ]
  },
})

registerContextMenu({
  id: 'monitor.tab-context',
  target: 'tab',
  order: 600,
  items: (ctx) => {
    const p = ctx.params as { sessionId?: unknown; protocol?: unknown } | undefined
    if (ctx.kind !== 'terminal' || typeof p?.sessionId !== 'string' || !MONITORABLE.has(String(p.protocol ?? ''))) return []
    return [{ label: 'Monitor host', icon: Activity, command: 'monitor.open', args: { sessionId: p.sessionId } }]
  },
})
