/*
 * Formatting and threshold helpers shared by the monitoring bar, the monitor tab and System info.
 */
import { formatBytes } from '@/lib/utils'
import type { Stats } from './types'

export type Level = 'ok' | 'warn' | 'crit'

/** Threshold level of a percentage (warning / critical per settings). */
export function level(pct: number | undefined, warn: number, crit: number): Level {
  if (pct == null || !Number.isFinite(pct)) return 'ok'
  if (pct >= crit) return 'crit'
  if (pct >= warn) return 'warn'
  return 'ok'
}

/**
 * Threshold level of a load average. Load is not a usage percentage: 1.0 per core means every core is busy, so it
 * turns orange at one runnable task per core and red at two (sustained overload), like Glances' warning level. On
 * Windows the value is the instantaneous processor queue length, where a queue of two per core is the usual alarm.
 */
export function loadLevel(load: number | undefined, cores: number | undefined, platform?: string): Level {
  if (load == null || !Number.isFinite(load)) return 'ok'
  const perCore = load / Math.max(cores || 1, 1)
  const [warn, crit] = platform === 'windows' ? [2, 4] : [1, 2]
  if (perCore >= crit) return 'crit'
  if (perCore >= warn) return 'warn'
  return 'ok'
}

export const levelText: Record<Level, string> = { ok: '', warn: 'text-warning', crit: 'text-destructive' }
export const levelBg: Record<Level, string> = { ok: 'bg-primary', warn: 'bg-warning', crit: 'bg-destructive' }

export function pct(v: number | undefined, digits?: number): string {
  if (v == null || !Number.isFinite(v)) return '—'
  const d = digits ?? (v > 0 && v < 10 ? 1 : 0)
  return `${v.toFixed(d)}%`
}

/** Part of a whole in percent (0 when unknown). */
export function ratio(part: number | undefined, whole: number | undefined): number {
  if (!part || !whole || whole <= 0) return 0
  return Math.min(100, Math.max(0, (part / whole) * 100))
}

const COMPACT = ['B', 'K', 'M', 'G', 'T', 'P']

/** Compact IEC size: 1536 → "1.5K", 3.2e9 → "3.0G". */
export function compactBytes(v: number | undefined): string {
  if (v == null || !Number.isFinite(v)) return '—'
  let x = Math.abs(v)
  let i = 0
  while (x >= 1024 && i < COMPACT.length - 1) {
    x /= 1024
    i++
  }
  const s = i === 0 ? x.toFixed(0) : x < 10 ? x.toFixed(1) : x.toFixed(0)
  return `${v < 0 ? '-' : ''}${s}${COMPACT[i]}`
}

/** Compact rate: "12K/s". */
export function compactRate(v: number | undefined): string {
  if (v == null || !Number.isFinite(v)) return '—'
  return `${compactBytes(v)}/s`
}

export function bytes(v: number | undefined): string {
  return formatBytes(v ?? null)
}

export function rate(v: number | undefined): string {
  if (v == null || !Number.isFinite(v)) return '—'
  return `${formatBytes(v)}/s`
}

/** "3d 4h", "5h 12m", "12m", "45s". */
export function uptime(sec: number | undefined): string {
  if (sec == null || !Number.isFinite(sec) || sec < 0) return '—'
  const d = Math.floor(sec / 86400)
  const h = Math.floor((sec % 86400) / 3600)
  const m = Math.floor((sec % 3600) / 60)
  if (d > 0) return h ? `${d}d ${h}h` : `${d}d`
  if (h > 0) return m ? `${h}h ${m}m` : `${h}h`
  if (m > 0) return `${m}m`
  return `${Math.floor(sec)}s`
}

/** Seconds of CPU time as ps's TIME column: "1:02:03" / "12:05.33". */
export function cpuTime(sec: number | undefined): string {
  if (sec == null || !Number.isFinite(sec) || sec < 0) return '—'
  const h = Math.floor(sec / 3600)
  const m = Math.floor((sec % 3600) / 60)
  const s = sec % 60
  if (h > 0) return `${h}:${String(m).padStart(2, '0')}:${String(Math.floor(s)).padStart(2, '0')}`
  return `${m}:${s.toFixed(2).padStart(5, '0')}`
}

export function num(v: number | undefined, digits = 2): string {
  if (v == null || !Number.isFinite(v)) return '—'
  return v.toFixed(digits)
}

/** df semantics: used / (used + available) — reserved blocks do not count as free. */
export function diskPct(d: { total: number; used: number; avail?: number } | undefined): number {
  if (!d) return 0
  const denom = d.avail != null && d.used + d.avail > 0 ? d.used + d.avail : d.total
  return ratio(d.used, denom)
}

export function memPct(st: Stats | undefined): number {
  return ratio(st?.mem?.used, st?.mem?.total)
}

export function swapPct(st: Stats | undefined): number {
  return ratio(st?.mem?.swapUsed, st?.mem?.swapTotal)
}

/** The primary volume ("/", the macOS Data volume, C:). */
export function rootDisk(st: Stats | undefined) {
  return st?.disks?.[0]
}

export function platformLabel(p: string | undefined): string {
  switch (p) {
    case 'linux':
      return 'Linux'
    case 'darwin':
      return 'macOS'
    case 'freebsd':
      return 'FreeBSD'
    case 'openbsd':
      return 'OpenBSD'
    case 'netbsd':
      return 'NetBSD'
    case 'dragonfly':
      return 'DragonFly BSD'
    case 'windows':
      return 'Windows'
  }
  return p || 'Unknown'
}

/** Stats older than this are shown as stale. */
export function isStale(receivedAt: number | undefined, now: number, intervalSec = 2): boolean {
  if (!receivedAt) return false
  return now - receivedAt > Math.max(8_000, intervalSec * 4_000)
}
