/*
 * Small visual building blocks: SVG sparklines, usage meters and labelled stat tiles.
 */
import type * as React from 'react'
import { cn } from '@/lib/utils'
import { levelBg, levelText, type Level } from './format'

/** Tiny line chart of the last values (NaN gaps skipped). `max` fixes the scale (e.g. 100 for percentages). */
export function Sparkline({
  values,
  max,
  width = 36,
  height = 12,
  className,
  fill = true,
}: {
  values: number[]
  max?: number
  width?: number
  height?: number
  className?: string
  fill?: boolean
}) {
  const pts = values.filter((v) => Number.isFinite(v))
  if (pts.length < 2) return <svg width={width} height={height} className={className} aria-hidden />
  const top = Math.max(max ?? 0, ...pts, 1e-9)
  const step = width / (values.length - 1 || 1)
  let d = ''
  let first = -1
  let lastX = 0
  values.forEach((v, i) => {
    if (!Number.isFinite(v)) return
    const x = i * step
    const y = height - 1 - (Math.min(v, top) / top) * (height - 2)
    d += `${d ? 'L' : 'M'}${x.toFixed(1)},${y.toFixed(1)}`
    if (first < 0) first = x
    lastX = x
  })
  return (
    <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`} className={cn('shrink-0 overflow-visible', className)} aria-hidden>
      {fill && <path d={`${d}L${lastX.toFixed(1)},${height}L${first.toFixed(1)},${height}Z`} fill="currentColor" opacity={0.12} />}
      <path d={d} fill="none" stroke="currentColor" strokeWidth={1.2} strokeLinejoin="round" strokeLinecap="round" />
    </svg>
  )
}

/** Horizontal usage bar coloured by threshold level. */
export function Meter({ value, lvl = 'ok', className, label }: { value: number; lvl?: Level; className?: string; label?: string }) {
  const v = Math.min(100, Math.max(0, Number.isFinite(value) ? value : 0))
  return (
    <div
      role="meter"
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(v)}
      aria-label={label}
      className={cn('h-1.5 w-full overflow-hidden rounded-full bg-muted', className)}
    >
      <div className={cn('h-full rounded-full transition-[width] duration-500', levelBg[lvl])} style={{ width: `${v}%` }} />
    </div>
  )
}

/** Labelled value tile for dashboards. */
export function StatTile({
  icon: Icon,
  label,
  value,
  sub,
  lvl = 'ok',
  children,
  className,
}: {
  icon?: React.ComponentType<{ className?: string }>
  label: string
  value: React.ReactNode
  sub?: React.ReactNode
  lvl?: Level
  children?: React.ReactNode
  className?: string
}) {
  return (
    <div className={cn('flex min-w-0 flex-col gap-1 rounded-lg border bg-card px-3 py-2.5', className)}>
      <div className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
        {Icon && <Icon className="size-3.5" />}
        <span className="truncate">{label}</span>
      </div>
      <div className={cn('truncate text-lg font-semibold tabular', levelText[lvl])}>{value}</div>
      {sub != null && <div className="truncate text-xs text-muted-foreground tabular">{sub}</div>}
      {children}
    </div>
  )
}

/** Key / value rows for info cards. */
export function InfoRows({ rows, className }: { rows: [React.ReactNode, React.ReactNode][]; className?: string }) {
  return (
    <dl className={cn('grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1.5 text-sm', className)}>
      {rows.map(([k, v], i) => (
        <div key={i} className="contents">
          <dt className="text-muted-foreground">{k}</dt>
          <dd className="min-w-0 break-words">{v ?? '—'}</dd>
        </div>
      ))}
    </dl>
  )
}

/** Titled panel section. */
export function Section({ title, actions, children, className }: { title: React.ReactNode; actions?: React.ReactNode; children: React.ReactNode; className?: string }) {
  return (
    <section className={cn('flex min-w-0 flex-col gap-2 rounded-lg border bg-card p-3', className)}>
      <header className="flex items-center justify-between gap-2">
        <h3 className="text-sm font-semibold">{title}</h3>
        {actions}
      </header>
      {children}
    </section>
  )
}
