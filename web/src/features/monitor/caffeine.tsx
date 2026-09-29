/*
 * Caffeine (SEC-22): keep the NexTerm host awake (backend inhibitor) and this screen on (Screen Wake Lock) during long
 * jobs. Status bar toggle, commands and a keeper overlay that holds the wake lock while Caffeine is on.
 */
import { useEffect } from 'react'
import { Coffee } from 'lucide-react'
import { toast } from 'sonner'
import { queryClient } from '@/api/queryClient'
import { StatusBarItem } from '@/layout/StatusBar'
import { events } from '@/lib/events'
import { errorMessage, formatDuration } from '@/lib/utils'
import { useNow } from '@/lib/hooks'
import { getCaffeine, monitorKeys, setCaffeine, useCaffeineStatus } from './api'
import { monitorSettings } from './settings'
import type { CaffeineStatus } from './types'

let listening = false

/** Mirror {type:'caffeine', status} broadcasts (other tabs, timer expiry) into the query cache. */
function listen(): void {
  if (listening) return
  listening = true
  events.onAny((ev) => {
    const e = ev as unknown as { type: string; status?: CaffeineStatus }
    if (e.type !== 'caffeine' || !e.status) return
    queryClient.setQueryData<CaffeineStatus>(monitorKeys.caffeine, (old) => ({ ...e.status!, allowed: old?.allowed ?? false }))
  })
}

export function caffeineStatus(): CaffeineStatus | undefined {
  return queryClient.getQueryData<CaffeineStatus>(monitorKeys.caffeine)
}

/** Switch Caffeine on (optionally for some minutes) or off. */
export async function toggleCaffeine(enabled?: boolean, minutes = 0): Promise<void> {
  listen()
  const cur = caffeineStatus() ?? (await getCaffeine())
  const next = enabled ?? !cur.enabled
  try {
    const st = await setCaffeine(next, next ? minutes : 0)
    queryClient.setQueryData(monitorKeys.caffeine, st)
    if (next) toast.success('Caffeine on', { description: minutes ? `This computer stays awake for ${minutes >= 60 ? `${minutes / 60} h` : `${minutes} min`}.` : 'This computer stays awake until you turn it off.' })
    else toast.info('Caffeine off', { description: 'Normal sleep settings apply again.' })
  } catch (err) {
    toast.error(next ? 'Could not keep the computer awake' : 'Could not turn Caffeine off', { description: errorMessage(err) })
    void queryClient.invalidateQueries({ queryKey: monitorKeys.caffeine })
  }
}

export const CAFFEINE_DURATIONS = [
  { minutes: 0, label: 'Until turned off' },
  { minutes: 30, label: 'For 30 minutes' },
  { minutes: 60, label: 'For 1 hour' },
  { minutes: 120, label: 'For 2 hours' },
  { minutes: 240, label: 'For 4 hours' },
  { minutes: 480, label: 'For 8 hours' },
]

export function CaffeineStatusItem() {
  const show = monitorSettings.useValue('caffeineStatusItem')
  const q = useCaffeineStatus(show)
  const now = useNow(30_000)
  useEffect(() => listen(), [])
  const st = q.data
  if (!show || !st || !st.allowed || !st.supported) return null
  const left = st.until ? Date.parse(st.until) - now : undefined
  const tip = st.enabled
    ? `Caffeine is on (${st.method}): this computer will not sleep${left != null && left > 0 ? ` for another ${formatDuration(left)}` : ''}. Click to turn off.`
    : st.error
      ? `Caffeine stopped: ${st.error}. Click to turn on again.`
      : 'Caffeine is off. Click to keep this computer awake.'
  return (
    <StatusBarItem
      icon={Coffee}
      tone={st.enabled ? 'accent' : st.error ? 'warning' : 'muted'}
      tooltip={tip}
      onClick={() => void toggleCaffeine(!st.enabled)}
      aria-label={st.enabled ? 'Caffeine on — turn off' : 'Caffeine off — turn on'}
    >
      {st.enabled ? (left != null && left > 0 ? formatDuration(left) : 'On') : undefined}
    </StatusBarItem>
  )
}

/** Overlay: holds a Screen Wake Lock while Caffeine is on (re-acquired when the page becomes visible again). */
export function CaffeineKeeper() {
  const q = useCaffeineStatus(true)
  const enabled = !!q.data?.enabled
  useEffect(() => listen(), [])
  useEffect(() => {
    if (!enabled || typeof navigator === 'undefined' || !('wakeLock' in navigator)) return
    let lock: WakeLockSentinel | null = null
    let disposed = false
    let pending = false
    const acquire = async () => {
      if (disposed || pending || document.visibilityState !== 'visible' || (lock && !lock.released)) return
      pending = true
      try {
        const l = await navigator.wakeLock.request('screen')
        // Caffeine was switched off (or the overlay unmounted) while the request was pending: do not keep the lock.
        if (disposed) void l.release().catch(() => undefined)
        else lock = l
      } catch {
        /* denied (battery saver, unsupported context) */
      } finally {
        pending = false
      }
    }
    const onVis = () => void acquire()
    document.addEventListener('visibilitychange', onVis)
    void acquire()
    return () => {
      disposed = true
      document.removeEventListener('visibilitychange', onVis)
      void lock?.release().catch(() => undefined)
    }
  }, [enabled])
  return null
}
