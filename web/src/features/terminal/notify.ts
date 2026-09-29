/*
 * Bell sound and desktop notifications (TERM-20, TERM-21, TERM-22). Notifications are rate-limited per key and only
 * shown with permission; without it they degrade to in-app toasts.
 */
import { toast } from 'sonner'

let audio: AudioContext | null = null

/** Short beep through Web Audio (browsers allow it after the first user gesture in the page). */
export function playBell(volume = 0.15): void {
  try {
    const Ctx = window.AudioContext ?? (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext
    if (!Ctx) return
    audio ??= new Ctx()
    const ctx = audio
    if (ctx.state === 'suspended') void ctx.resume().catch(() => undefined)
    const osc = ctx.createOscillator()
    const gain = ctx.createGain()
    osc.type = 'sine'
    osc.frequency.value = 880
    const t = ctx.currentTime
    gain.gain.setValueAtTime(0, t)
    gain.gain.linearRampToValueAtTime(volume, t + 0.005)
    gain.gain.exponentialRampToValueAtTime(0.0001, t + 0.12)
    osc.connect(gain).connect(ctx.destination)
    osc.start(t)
    osc.stop(t + 0.13)
  } catch {
    /* audio unavailable */
  }
}

export function notificationsSupported(): boolean {
  return typeof window !== 'undefined' && 'Notification' in window
}

export function notificationPermission(): NotificationPermission | 'unsupported' {
  return notificationsSupported() ? Notification.permission : 'unsupported'
}

/** Ask for notification permission (call from a user gesture, e.g. enabling a setting). */
export async function requestNotificationPermission(): Promise<boolean> {
  if (!notificationsSupported()) return false
  if (Notification.permission === 'granted') return true
  if (Notification.permission === 'denied') return false
  try {
    return (await Notification.requestPermission()) === 'granted'
  } catch {
    return false
  }
}

const lastShown = new Map<string, number>()

export interface NotifyOptions {
  /** Rate-limit key (e.g. `bell:<tabId>`). */
  key: string
  title: string
  body?: string
  /** Minimum interval between notifications with the same key. */
  minIntervalMs?: number
  /** Called when the user clicks the notification (focus the tab). */
  onClick?: () => void
  /** Fall back to a toast when desktop notifications are unavailable (default true). */
  toastFallback?: boolean
}

/** Show a desktop notification (or a toast), rate-limited per key. Returns whether something was shown. */
export function notify(opts: NotifyOptions): boolean {
  const now = Date.now()
  const min = opts.minIntervalMs ?? 5000
  const last = lastShown.get(opts.key) ?? 0
  if (now - last < min) return false
  lastShown.set(opts.key, now)
  if (lastShown.size > 500) {
    for (const [k, t] of lastShown) if (now - t > 60_000) lastShown.delete(k)
  }
  const title = truncate(opts.title, 120)
  const body = opts.body ? truncate(opts.body, 300) : undefined
  if (notificationsSupported() && Notification.permission === 'granted') {
    try {
      const n = new Notification(title, { body, tag: opts.key, silent: true })
      n.onclick = () => {
        try {
          window.focus()
        } catch {
          /* ignore */
        }
        opts.onClick?.()
        n.close()
      }
      return true
    } catch {
      /* fall through (e.g. Android requires a service worker) */
    }
  }
  if (opts.toastFallback === false) return false
  toast(title, { description: body, action: opts.onClick ? { label: 'Show', onClick: opts.onClick } : undefined })
  return true
}

function truncate(s: string, n: number): string {
  // oxlint-disable-next-line no-control-regex -- remote text must not carry control characters into notifications
  const clean = s.replace(/[\u0000-\u001f\u007f]/g, ' ').trim()
  return clean.length > n ? `${clean.slice(0, n - 1)}…` : clean
}

/** Is the user currently looking at this page (focused window, visible document)? */
export function pageHasAttention(doc: Document = document): boolean {
  return doc.visibilityState === 'visible' && doc.hasFocus()
}
