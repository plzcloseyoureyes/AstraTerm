/*
 * Theme-aware uPlot time-series chart (lazy: only the monitor / System info tabs import this module). Colours come
 * from the design tokens and follow light / dark switches.
 */
import { useEffect, useMemo, useRef, useState, type ReactNode, type RefObject } from 'react'
import uPlot from 'uplot'
import 'uplot/dist/uPlot.min.css'
import { onThemeChange } from '@/lib/theme'
import { cn } from '@/lib/utils'

export interface ChartSeries {
  label: string
  /** Design token (e.g. "--primary") or CSS colour. */
  color: string
  values: number[]
  fill?: boolean
  dash?: number[]
}

/** Resolve a design token / CSS colour to rgb components (via a 1×1 canvas, so oklch() tokens work everywhere). */
function rgbOf(color: string): [number, number, number] {
  const probe = document.createElement('span')
  probe.style.color = color.startsWith('--') ? `var(${color})` : color
  probe.style.display = 'none'
  document.body.appendChild(probe)
  const css = getComputedStyle(probe).color
  probe.remove()
  const c = document.createElement('canvas')
  c.width = c.height = 1
  const ctx = c.getContext('2d', { willReadFrequently: true })
  if (!ctx) return [128, 128, 128]
  ctx.fillStyle = '#808080'
  ctx.fillStyle = css
  ctx.fillRect(0, 0, 1, 1)
  const [r, g, b] = ctx.getImageData(0, 0, 1, 1).data
  return [r, g, b]
}

function rgba([r, g, b]: [number, number, number], a: number): string {
  return `rgba(${r},${g},${b},${a})`
}

function useThemeVersion(): number {
  const [v, setV] = useState(0)
  useEffect(() => onThemeChange(() => setV((x) => x + 1)), [])
  return v
}

export function TimeChart({
  title,
  xs,
  series,
  yMax,
  format,
  height = 150,
  windowMs,
  className,
  headline,
}: {
  title: string
  /** Sample times (ms epoch). */
  xs: number[]
  series: ChartSeries[]
  /** Fixed upper bound (e.g. 100 for percentages); auto-scaled otherwise. */
  yMax?: number
  format: (v: number) => string
  height?: number
  /** Visible time window ending at the last sample. */
  windowMs?: number
  className?: string
  /** Current value shown next to the title. */
  headline?: ReactNode
}) {
  const box = useRef<HTMLDivElement>(null)
  const plot = useRef<uPlot | null>(null)
  const theme = useThemeVersion()
  const fmt = useRef(format)
  fmt.current = format
  const win = useRef(windowMs)
  win.current = windowMs
  const [width, setWidth] = useState(0)

  useEffect(() => {
    const el = box.current
    if (!el) return
    const ro = new ResizeObserver(() => setWidth(Math.floor(el.clientWidth)))
    ro.observe(el)
    setWidth(Math.floor(el.clientWidth))
    return () => ro.disconnect()
  }, [])

  const shape = series.map((s) => `${s.label}|${s.color}|${s.fill ? 1 : 0}`).join(',')
  const data = useMemo<uPlot.AlignedData>(
    () => [xs.map((t) => t / 1000), ...series.map((s) => s.values.map((v) => (Number.isFinite(v) ? v : null)))],
    [xs, series],
  )

  // (Re)create on shape / theme / size changes.
  useEffect(() => {
    const el = box.current
    if (!el || width < 50) return
    const axis = rgbOf('--muted-foreground')
    const grid = rgbOf('--border')
    // Canvas fonts cannot contain var(): such a font string is rejected and the canvas falls back to 10 device px
    // (half size on HiDPI screens). Resolve the UI font family instead.
    const font = `10px ${getComputedStyle(el).fontFamily || 'sans-serif'}`
    const opts: uPlot.Options = {
      width,
      height,
      padding: [6, 6, 0, 0],
      legend: { show: false },
      cursor: { drag: { x: false, y: false }, points: { size: 5 } },
      scales: {
        // The visible window always ends at the newest sample (also right after the chart is re-created).
        x: {
          time: true,
          range: (_u, min, max) => {
            const w = win.current
            if (w) return [max - w / 1000, max]
            return min === max ? [min - 60, max] : [min, max]
          },
        },
        y: yMax != null ? { range: [0, yMax] } : { range: (_u, _min, max) => [0, max > 0 ? max * 1.15 : 1] },
      },
      axes: [
        {
          stroke: rgba(axis, 1),
          grid: { stroke: rgba(grid, 0.7), width: 1 },
          ticks: { stroke: rgba(grid, 0.7), width: 1, size: 3 },
          font,
          space: 60,
        },
        {
          stroke: rgba(axis, 1),
          grid: { stroke: rgba(grid, 0.7), width: 1 },
          ticks: { show: false },
          font,
          size: 52,
          values: (_u, vals) => vals.map((v) => (v == null ? '' : fmt.current(v))),
        },
      ],
      series: [
        {},
        ...series.map((s) => {
          const c = rgbOf(s.color)
          return {
            label: s.label,
            stroke: rgba(c, 1),
            width: 1.4,
            fill: s.fill ? rgba(c, 0.14) : undefined,
            dash: s.dash,
            points: { show: false },
            value: (_u: uPlot, v: number | null) => (v == null ? '—' : fmt.current(v)),
          } satisfies uPlot.Series
        }),
      ],
    }
    plot.current?.destroy()
    plot.current = new uPlot(opts, data, el)
    return () => {
      plot.current?.destroy()
      plot.current = null
    }
    // data is pushed by the effect below; recreate only when the structure changes
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [shape, theme, width, height, yMax])

  useEffect(() => {
    plot.current?.setData(data)
  }, [data, windowMs])

  const cursor = useCursorValues(plot, series)

  return (
    <div className={cn('flex min-w-0 flex-col gap-1 rounded-lg border bg-card p-3', className)}>
      <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-0.5">
        <h3 className="text-sm font-semibold">{title}</h3>
        <div className="text-sm font-semibold tabular">{headline}</div>
      </div>
      <div className="flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-muted-foreground" aria-hidden>
        {series.map((s, i) => (
          <span key={s.label} className="flex items-center gap-1">
            <span className="inline-block h-0.5 w-3 rounded" style={{ background: s.color.startsWith('--') ? `var(${s.color})` : s.color }} />
            {s.label}
            {cursor?.[i] != null && <span className="text-foreground tabular">{cursor[i]}</span>}
          </span>
        ))}
      </div>
      <div className="relative w-full" style={{ height }}>
        <div ref={box} className="absolute inset-0" role="img" aria-label={`${title} chart`} />
        {xs.length < 2 && (
          <div className="pointer-events-none absolute inset-0 flex items-center justify-center text-xs text-muted-foreground">Collecting samples…</div>
        )}
      </div>
    </div>
  )
}

/** Values under the cursor (legend) while hovering the chart. */
function useCursorValues(plot: RefObject<uPlot | null>, series: ChartSeries[]): (string | null)[] | null {
  const [vals, setVals] = useState<(string | null)[] | null>(null)
  useEffect(() => {
    const u = plot.current
    if (!u) return
    const over = u.over
    const onMove = () => {
      const idx = u.cursor.idx
      if (idx == null) {
        setVals(null)
        return
      }
      setVals(
        series.map((_, i) => {
          const v = u.data[i + 1]?.[idx]
          const s = u.series[i + 1]
          return v == null ? null : typeof s.value === 'function' ? String(s.value(u, v, i + 1, idx)) : String(v)
        }),
      )
    }
    const onLeave = () => setVals(null)
    over.addEventListener('mousemove', onMove)
    over.addEventListener('mouseleave', onLeave)
    return () => {
      over.removeEventListener('mousemove', onMove)
      over.removeEventListener('mouseleave', onLeave)
    }
  })
  return vals
}
