/*
 * Opening monitor views: one 'monitor' tab per monitored session (id `monitor:<session>`), the singleton 'sysinfo' tab
 * for the AstraTerm host, and the target resolution shared by commands and the monitoring bar.
 */
import { toast } from 'sonner'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { RuntimeSession } from '@/api/types'
import type { TabInfo } from '@/app/registry'
import { useAuthStore } from '@/stores/auth'
import { activeTab, getTabParams, openTab } from '@/stores/workspace'
import type { MonitorPanel, MonitorTabParams, SysInfoTabParams } from './types'

export const MONITOR_KIND = 'monitor'
export const SYSINFO_KIND = 'sysinfo'

function monitorTabId(target: string): string {
  return `monitor:${target}`
}

/** The session a tab is about: `params.target` of monitor tabs, `params.sessionId` of any other tab. */
export function tabTarget(tab: Pick<TabInfo, 'kind' | 'params'> | undefined): string | undefined {
  if (!tab) return undefined
  const p = tab.params as { target?: unknown; sessionId?: unknown } | undefined
  const v = tab.kind === MONITOR_KIND ? (p?.target ?? p?.sessionId) : p?.sessionId
  return typeof v === 'string' && v ? v : undefined
}

export function cachedSession(id: string): RuntimeSession | undefined {
  return queryClient.getQueryData<RuntimeSession[]>(queryKeys.sessions)?.find((s) => s.id === id)
}

export function isMonitorable(s: RuntimeSession | undefined): boolean {
  return !!s && (s.protocol === 'ssh' || s.protocol === 'local') && s.state !== 'closed'
}

/** Local host monitoring is allowed in desktop mode or for administrators (backend enforces it too). */
export function canMonitorLocal(): boolean {
  const st = useAuthStore.getState()
  return st.state?.mode !== 'server' || st.user?.role === 'admin'
}

/** Open (or focus) the monitor tab of a session, optionally on a panel. */
export function openMonitor(target: string, panel?: MonitorPanel, extra: Partial<MonitorTabParams> = {}): string {
  const s = cachedSession(target)
  const id = monitorTabId(target)
  const prev = getTabParams<MonitorTabParams>(id)
  const params: MonitorTabParams = {
    ...prev,
    target,
    ...(panel ? { panel } : {}),
    title: s?.title || s?.host || prev?.title,
    protocol: s?.protocol ?? prev?.protocol,
    ...extra,
  }
  return openTab<MonitorTabParams>({ kind: MONITOR_KIND, id, params })
}

/** Open the System information view of the AstraTerm host. */
export function openSystemInfo(panel?: SysInfoTabParams['panel']): string {
  return openTab<SysInfoTabParams>({ kind: SYSINFO_KIND, params: panel ? { panel } : {} })
}

/** Resolve the session a monitor command should use: explicit args, the active tab, else the focused terminal. */
export function resolveMonitorTarget(args: unknown, fallback?: () => string | undefined): string | undefined {
  const a = args as { sessionId?: unknown; target?: unknown } | undefined
  const explicit = a?.target ?? a?.sessionId
  if (typeof explicit === 'string' && explicit) return explicit
  const fromTab = tabTarget(activeTab())
  if (fromTab && isMonitorable(cachedSession(fromTab))) return fromTab
  const fb = fallback?.()
  if (fb && isMonitorable(cachedSession(fb))) return fb
  return undefined
}

export function noTargetToast(): void {
  toast.info('Nothing to monitor', {
    description: 'Open an SSH session (or a local shell) first, then use Monitor host.',
    action: canMonitorLocal() ? { label: 'System info', onClick: () => openSystemInfo() } : undefined,
  })
}
