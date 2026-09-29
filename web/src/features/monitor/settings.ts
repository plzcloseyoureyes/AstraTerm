/*
 * Settings section `monitor` (defineSettings: only changed keys are stored) and the small public state other features
 * read — e.g. the SFTP panel's "Remote monitoring" checkbox:
 *
 *   import { useMonitorBarShown, setMonitorBarEnabled } from '@/features/monitor/settings'
 *   runCommand('monitor.toggleBar')                          // global on / off (or {enabled: boolean})
 *   runCommand('monitor.toggleBar', {sessionId, enabled})    // per session, like the SFTP panel's checkbox
 *   useMonitorBarShown(sessionId)                            // effective state for one session
 *   commands.get('monitor.toggleBar')?.checked?.()           // global state (extra `checked` on the definition)
 */
import { create } from 'zustand'
import { events } from '@/lib/events'
import { defineSettings } from '@/stores/settings'

export type BarItem = 'host' | 'cpu' | 'mem' | 'swap' | 'disk' | 'net' | 'load' | 'users' | 'uptime' | 'procs'

export const BAR_ITEMS: { id: BarItem; label: string }[] = [
  { id: 'host', label: 'Host name' },
  { id: 'cpu', label: 'CPU' },
  { id: 'mem', label: 'Memory' },
  { id: 'swap', label: 'Swap' },
  { id: 'disk', label: 'Disk (/)' },
  { id: 'net', label: 'Network ↓↑' },
  { id: 'load', label: 'Load average' },
  { id: 'users', label: 'Logged-in users' },
  { id: 'uptime', label: 'Uptime' },
  { id: 'procs', label: 'Processes' },
]

export interface MonitorSettings {
  /** Remote monitoring bar in the status bar (the bottom bar of SSH tabs). */
  showBar: boolean
  /** Also monitor local shell tabs (the NexTerm host). */
  barForLocal: boolean
  /** Items shown by the bar. */
  barItems: BarItem[]
  /** Tiny history sparklines next to CPU / memory / network. */
  sparklines: boolean
  /** Usage (%) turning an item orange / red. */
  warnPct: number
  critPct: number
  /** Stop sampling while the browser tab is hidden (the exec channel counts toward the server's MaxSessions). */
  pauseHidden: boolean
  /** Process list auto-refresh period (seconds, 0 = off). */
  processRefreshSec: number
  /** Show kernel threads in process lists (Linux). */
  showKernelThreads: boolean
  /** Lines of history when following logs. */
  logLines: number
  /** Log files suggested in the log viewer. */
  logFiles: string[]
  /** Caffeine toggle in the status bar. */
  caffeineStatusItem: boolean
}

export const MONITOR_DEFAULTS: MonitorSettings = {
  showBar: true,
  barForLocal: true,
  barItems: ['host', 'cpu', 'mem', 'disk', 'net', 'users', 'uptime', 'load'],
  sparklines: true,
  warnPct: 80,
  critPct: 90,
  pauseHidden: true,
  processRefreshSec: 3,
  showKernelThreads: false,
  logLines: 200,
  logFiles: ['/var/log/syslog', '/var/log/messages', '/var/log/auth.log', '/var/log/secure', '/var/log/kern.log', '/var/log/nginx/error.log'],
  caffeineStatusItem: true,
}

export const monitorSettings = defineSettings<MonitorSettings>('monitor', MONITOR_DEFAULTS)

/** Is the remote monitoring bar enabled? */
export function isMonitorBarEnabled(): boolean {
  return monitorSettings.get().showBar
}

/** React: is the remote monitoring bar enabled? */
export function useMonitorBarEnabled(): boolean {
  return monitorSettings.useValue('showBar')
}

export function setMonitorBarEnabled(enabled: boolean): void {
  monitorSettings.set({ showBar: enabled })
}

/*
 * Per-session choices: true / false override the global setting for one runtime session. Sessions outlive page
 * reloads (the backend owns them), so the choice is kept in localStorage too — pruned when the session closes, after
 * 30 days, and beyond 200 entries.
 */
const OVERRIDES_KEY = 'nexterm:monitor:bar-sessions:v1'
const OVERRIDE_TTL_MS = 30 * 24 * 3_600_000
const MAX_OVERRIDES = 200

type StoredOverrides = Record<string, { on: boolean; at: number }>

function readStoredOverrides(): StoredOverrides {
  try {
    const raw = localStorage.getItem(OVERRIDES_KEY)
    const parsed: unknown = raw ? JSON.parse(raw) : null
    if (!parsed || typeof parsed !== 'object') return {}
    const now = Date.now()
    const out: StoredOverrides = {}
    for (const [id, v] of Object.entries(parsed as Record<string, unknown>)) {
      const e = v as { on?: unknown; at?: unknown } | null
      if (e && typeof e.on === 'boolean' && typeof e.at === 'number' && now - e.at < OVERRIDE_TTL_MS) out[id] = { on: e.on, at: e.at }
    }
    return out
  } catch {
    return {} // storage unavailable (private mode, blocked site data)
  }
}

let stored: StoredOverrides = readStoredOverrides()

function writeStoredOverrides(): void {
  const entries = Object.entries(stored).sort((a, b) => b[1].at - a[1].at)
  if (entries.length > MAX_OVERRIDES) stored = Object.fromEntries(entries.slice(0, MAX_OVERRIDES))
  try {
    if (Object.keys(stored).length) localStorage.setItem(OVERRIDES_KEY, JSON.stringify(stored))
    else localStorage.removeItem(OVERRIDES_KEY)
  } catch {
    /* storage unavailable: the choice lasts for this page only */
  }
}

const useBarOverrides = create<{ bySession: Record<string, boolean> }>(() => ({
  bySession: Object.fromEntries(Object.entries(stored).map(([id, v]) => [id, v.on])),
}))

export function setMonitorBarForSession(sessionId: string, enabled: boolean): void {
  stored = { ...stored, [sessionId]: { on: enabled, at: Date.now() } }
  writeStoredOverrides()
  useBarOverrides.setState((s) => ({ bySession: { ...s.bySession, [sessionId]: enabled } }))
}

/** Forget the choice of a session (it closed). */
export function forgetMonitorBarForSession(sessionId: string): void {
  if (sessionId in stored) {
    const { [sessionId]: _drop, ...rest } = stored
    stored = rest
    writeStoredOverrides()
  }
  useBarOverrides.setState((s) => {
    if (!(sessionId in s.bySession)) return s
    const { [sessionId]: _gone, ...bySession } = s.bySession
    return { bySession }
  })
}

events.on('session.closed', (ev) => {
  if (typeof ev.id === 'string') forgetMonitorBarForSession(ev.id)
})

export function getMonitorBarOverride(sessionId: string): boolean | undefined {
  return useBarOverrides.getState().bySession[sessionId]
}

export function useMonitorBarOverride(sessionId: string | undefined): boolean | undefined {
  return useBarOverrides((s) => (sessionId ? s.bySession[sessionId] : undefined))
}

/**
 * React: is the bar shown for a session? A per-session choice wins, else the global setting and the connection's
 * `monitoring` option (evaluated by the bar itself).
 */
export function useMonitorBarShown(sessionId: string | undefined): boolean {
  const override = useMonitorBarOverride(sessionId)
  const global = useMonitorBarEnabled()
  return override ?? global
}
