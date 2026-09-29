import { clsx, type ClassValue } from 'clsx'
import { twMerge } from 'tailwind-merge'

/** Merge Tailwind class names, resolving conflicts (later wins). */
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs))
}

/** True on Apple platforms (Cmd is the primary modifier). */
export const isMac: boolean =
  typeof navigator !== 'undefined' && /Mac|iPhone|iPad|iPod/.test(navigator.platform || navigator.userAgent)

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']

/** Human readable IEC byte size, e.g. 1536 → "1.5 KiB". */
export function formatBytes(bytes: number | null | undefined, decimals = 1): string {
  if (bytes == null || !Number.isFinite(bytes)) return '—'
  const sign = bytes < 0 ? '-' : ''
  let v = Math.abs(bytes)
  let i = 0
  while (v >= 1024 && i < BYTE_UNITS.length - 1) {
    v /= 1024
    i++
  }
  return `${sign}${i === 0 ? v.toFixed(0) : v.toFixed(decimals)} ${BYTE_UNITS[i]}`
}

/** Bits-per-second style rate from bytes/sec, e.g. "1.2 MiB/s". */
export function formatRate(bytesPerSec: number | null | undefined): string {
  if (bytesPerSec == null || !Number.isFinite(bytesPerSec)) return '—'
  return `${formatBytes(bytesPerSec)}/s`
}

/** Compact duration from milliseconds, e.g. "3d 4h", "12m 5s", "850ms". */
export function formatDuration(ms: number | null | undefined): string {
  if (ms == null || !Number.isFinite(ms)) return '—'
  if (ms < 0) ms = 0
  if (ms < 1000) return `${Math.round(ms)}ms`
  const s = Math.floor(ms / 1000)
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  if (d > 0) return h ? `${d}d ${h}h` : `${d}d`
  if (h > 0) return m ? `${h}h ${m}m` : `${h}h`
  if (m > 0) return sec ? `${m}m ${sec}s` : `${m}m`
  return `${sec}s`
}

const rtf = typeof Intl !== 'undefined' ? new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' }) : null

/** "3 minutes ago" style relative time for an ISO timestamp or Date. */
export function formatRelativeTime(value: string | number | Date | null | undefined, now = Date.now()): string {
  if (value == null || value === '') return 'never'
  const t = value instanceof Date ? value.getTime() : typeof value === 'number' ? value : Date.parse(value)
  if (!Number.isFinite(t)) return '—'
  const diff = (t - now) / 1000
  const abs = Math.abs(diff)
  if (!rtf) return new Date(t).toLocaleString()
  if (abs < 45) return rtf.format(Math.round(diff), 'second')
  if (abs < 2700) return rtf.format(Math.round(diff / 60), 'minute')
  if (abs < 64800) return rtf.format(Math.round(diff / 3600), 'hour')
  if (abs < 86400 * 26) return rtf.format(Math.round(diff / 86400), 'day')
  if (abs < 86400 * 320) return rtf.format(Math.round(diff / (86400 * 30)), 'month')
  return rtf.format(Math.round(diff / (86400 * 365)), 'year')
}

/** Locale date-time string for an ISO timestamp. */
export function formatDateTime(value: string | number | Date | null | undefined): string {
  if (value == null || value === '') return '—'
  const d = value instanceof Date ? value : new Date(value)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

const timeFmt = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' })
const weekdayFmt = new Intl.DateTimeFormat(undefined, { weekday: 'short' })
const dayFmt = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short' })
const dayYearFmt = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short', year: 'numeric' })
const DAY_MS = 86_400_000

/**
 * Calendar-style time, the way people say it: "Today 14:10", "Tomorrow 03:00", "Yesterday 09:30", a weekday within
 * a week either way ("Mon 09:30"), else the date ("28 Sep 14:10", with the year when it is another year). Local time,
 * the locale's formats. '—' for a missing or invalid value.
 */
export function formatCalendarTime(value: string | number | Date | null | undefined, now: number | Date = Date.now()): string {
  if (value == null || value === '') return '—'
  const d = value instanceof Date ? value : new Date(value)
  if (Number.isNaN(d.getTime())) return '—'
  const n = now instanceof Date ? now : new Date(now)
  // Whole calendar days between the two local dates (UTC midnights avoid daylight-saving hours).
  const days = Math.round((Date.UTC(d.getFullYear(), d.getMonth(), d.getDate()) - Date.UTC(n.getFullYear(), n.getMonth(), n.getDate())) / DAY_MS)
  const time = timeFmt.format(d)
  if (days === 0) return `Today ${time}`
  if (days === 1) return `Tomorrow ${time}`
  if (days === -1) return `Yesterday ${time}`
  if (Math.abs(days) < 7) return `${weekdayFmt.format(d)} ${time}`
  return `${(d.getFullYear() === n.getFullYear() ? dayFmt : dayYearFmt).format(d)} ${time}`
}

/** Random URL-safe id (not for secrets). */
export function uid(prefix = ''): string {
  const bytes = new Uint8Array(8)
  crypto.getRandomValues(bytes)
  let s = ''
  for (const b of bytes) s += b.toString(36).padStart(2, '0')
  return prefix ? `${prefix}-${s.slice(0, 12)}` : s.slice(0, 12)
}

/** Trailing-edge debounce with flush/cancel. */
export function debounce<A extends unknown[]>(fn: (...args: A) => void, wait: number) {
  let timer: ReturnType<typeof setTimeout> | null = null
  let pending: A | null = null
  const debounced = (...args: A) => {
    pending = args
    if (timer) clearTimeout(timer)
    timer = setTimeout(() => {
      timer = null
      const a = pending
      pending = null
      if (a) fn(...a)
    }, wait)
  }
  debounced.flush = () => {
    if (timer) clearTimeout(timer)
    timer = null
    const a = pending
    pending = null
    if (a) fn(...a)
  }
  debounced.cancel = () => {
    if (timer) clearTimeout(timer)
    timer = null
    pending = null
  }
  return debounced
}

/** Clamp a number into [min, max]. */
export function clamp(v: number, min: number, max: number): number {
  return Math.min(max, Math.max(min, v))
}

/** Safe localStorage access (private mode / disabled storage must never crash the app). */
export const storage = {
  get<T>(key: string, fallback: T): T {
    try {
      const raw = localStorage.getItem(key)
      return raw == null ? fallback : (JSON.parse(raw) as T)
    } catch {
      return fallback
    }
  },
  set(key: string, value: unknown): void {
    try {
      localStorage.setItem(key, JSON.stringify(value))
    } catch {
      /* quota exceeded or storage disabled — non-fatal */
    }
  },
  remove(key: string): void {
    try {
      localStorage.removeItem(key)
    } catch {
      /* ignore */
    }
  },
}

/** Deep-ish equality for plain JSON values (used to skip no-op settings writes). */
export function jsonEqual(a: unknown, b: unknown): boolean {
  if (a === b) return true
  try {
    return JSON.stringify(a) === JSON.stringify(b)
  } catch {
    return false
  }
}

/** Plain-object check (not arrays / null / class instances). */
export function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v) && Object.getPrototypeOf(v) === Object.prototype
}

/** Error → human message (ApiError, Error, string, unknown). */
export function errorMessage(err: unknown, fallback = 'Something went wrong'): string {
  if (!err) return fallback
  if (typeof err === 'string') return err
  if (err instanceof Error) return err.message || fallback
  if (typeof err === 'object' && 'message' in err && typeof (err as { message: unknown }).message === 'string') {
    return (err as { message: string }).message
  }
  return fallback
}

/** Is the keyboard event target an editable control (input, textarea, select, contenteditable)? */
export function isEditableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof Element)) return false
  if (target.closest('[contenteditable=""],[contenteditable="true"]')) return true
  const tag = target.tagName
  if (tag === 'TEXTAREA' || tag === 'SELECT') return true
  if (tag === 'INPUT') {
    const type = (target as HTMLInputElement).type
    return !['checkbox', 'radio', 'button', 'submit', 'reset', 'range', 'color', 'file'].includes(type)
  }
  return false
}

/** Copy text to the clipboard; resolves false when the browser refuses. */
export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    try {
      const ta = document.createElement('textarea')
      ta.value = text
      ta.setAttribute('readonly', '')
      ta.style.position = 'fixed'
      ta.style.opacity = '0'
      document.body.appendChild(ta)
      ta.select()
      const ok = document.execCommand('copy')
      ta.remove()
      return ok
    } catch {
      return false
    }
  }
}

/** Pluralize a count: plural(3, 'session') → "3 sessions". */
export function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`
}
