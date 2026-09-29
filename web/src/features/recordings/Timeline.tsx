/*
 * Player scrubber: played range, command / marker ticks, a hover preview (time + nearest marker) and drag-to-scrub
 * (seeks follow the pointer once per frame, the final position on release). Keyboard: ←/→ ±5 s (Shift ±30 s),
 * PageUp/PageDown ±10 %, Home/End. All times are player seconds.
 */
import { useEffect, useRef, useState, type KeyboardEvent, type PointerEvent } from 'react'
import { cn } from '@/lib/utils'
import { clock } from './clock'

export interface TimelineMarker {
  time: number
  label: string
}

interface Props {
  duration: number
  current: number
  markers: TimelineMarker[]
  onSeek: (time: number) => void
  disabled?: boolean
  className?: string
}

/** A marker within this many px of the pointer is the one the hover preview names. */
const SNAP_PX = 6

export function Timeline({ duration, current, markers, onSeek, disabled, className }: Props) {
  const ref = useRef<HTMLDivElement>(null)
  const frame = useRef(0)
  const [hover, setHover] = useState<{ x: number; w: number; time: number; marker?: TimelineMarker } | null>(null)
  const [drag, setDrag] = useState<number | null>(null)
  const span = duration > 0 ? duration : 1
  const shown = drag ?? current
  const pct = (t: number) => `${(Math.min(Math.max(t / span, 0), 1) * 100).toFixed(3)}%`

  const at = (clientX: number) => {
    const r = ref.current!.getBoundingClientRect()
    const x = Math.min(Math.max(clientX - r.left, 0), r.width)
    const time = r.width ? (x / r.width) * span : 0
    let marker: TimelineMarker | undefined
    let best = SNAP_PX
    for (const m of markers) {
      const d = Math.abs((m.time / span) * r.width - x)
      if (d <= best) {
        best = d
        marker = m
      }
    }
    return { x, w: r.width, time: marker ? marker.time : time, marker }
  }
  useEffect(() => () => cancelAnimationFrame(frame.current), [])
  const seekSoon = (t: number) => {
    cancelAnimationFrame(frame.current)
    frame.current = requestAnimationFrame(() => onSeek(t))
  }

  const onPointerDown = (e: PointerEvent<HTMLDivElement>) => {
    if (disabled || e.button !== 0) return
    e.currentTarget.setPointerCapture(e.pointerId)
    const p = at(e.clientX)
    setDrag(p.time)
    seekSoon(p.time)
  }
  const onPointerMove = (e: PointerEvent<HTMLDivElement>) => {
    if (disabled) return
    const p = at(e.clientX)
    setHover(p)
    if (drag != null) {
      setDrag(p.time)
      seekSoon(p.time)
    }
  }
  const onPointerUp = (e: PointerEvent<HTMLDivElement>) => {
    if (drag == null) return
    e.currentTarget.releasePointerCapture(e.pointerId)
    cancelAnimationFrame(frame.current)
    onSeek(at(e.clientX).time)
    setDrag(null)
  }
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const step = { ArrowLeft: e.shiftKey ? -30 : -5, ArrowRight: e.shiftKey ? 30 : 5, PageDown: -span / 10, PageUp: span / 10 }[e.key]
    const to = step != null ? current + step : e.key === 'Home' ? 0 : e.key === 'End' ? span : null
    if (to == null) return
    e.preventDefault()
    e.stopPropagation()
    onSeek(Math.min(Math.max(to, 0), span))
  }

  return (
    <div
      ref={ref}
      role="slider"
      tabIndex={disabled ? -1 : 0}
      aria-label="Seek"
      aria-valuemin={0}
      aria-valuemax={Math.round(span)}
      aria-valuenow={Math.round(shown)}
      aria-valuetext={`${clock(shown)} of ${clock(duration)}`}
      aria-disabled={disabled || undefined}
      className={cn('group/tl relative flex h-5 cursor-pointer touch-none items-center outline-none select-none aria-disabled:cursor-default aria-disabled:opacity-50', className)}
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={onPointerUp}
      onPointerCancel={() => setDrag(null)}
      onPointerLeave={() => setHover(null)}
      onKeyDown={onKeyDown}
    >
      <div className="relative h-1 w-full rounded-full bg-muted transition-[height] duration-150 ease-out group-hover/tl:h-1.5 group-focus-visible/tl:h-1.5 group-focus-visible/tl:ring-2 group-focus-visible/tl:ring-ring/60">
        {hover && drag == null && <div className="absolute inset-y-0 left-0 rounded-full bg-foreground/15" style={{ width: `${hover.x}px` }} aria-hidden />}
        <div className="absolute inset-y-0 left-0 rounded-full bg-primary" style={{ width: pct(shown) }} />
      </div>
      {markers.map((m, i) => (
        <span
          key={i}
          aria-hidden
          className={cn(
            'pointer-events-none absolute top-1/2 h-2.5 w-0.5 -translate-x-1/2 -translate-y-1/2 rounded-full',
            m.time <= shown ? 'bg-primary' : 'bg-muted-foreground/70',
          )}
          style={{ left: pct(m.time) }}
        />
      ))}
      <span
        aria-hidden
        className={cn(
          'pointer-events-none absolute top-1/2 size-3 -translate-x-1/2 -translate-y-1/2 rounded-full border-2 border-background bg-primary shadow-sm transition-transform duration-150 ease-out',
          drag != null ? 'scale-110' : 'scale-75 group-hover/tl:scale-100 group-focus-visible/tl:scale-100',
        )}
        style={{ left: pct(shown) }}
      />
      {hover && (
        <div
          aria-hidden
          className="pointer-events-none absolute bottom-full mb-1.5 flex max-w-64 items-baseline gap-1.5 rounded-md border bg-popover px-1.5 py-0.5 text-xs whitespace-nowrap text-popover-foreground shadow-popover"
          // Centred on the pointer, kept inside the track at both ends.
          style={{ left: `${hover.x}px`, translate: `${-Math.min(Math.max(hover.x / (hover.w || 1), 0.1), 0.9) * 100}% 0` }}
        >
          <span className="tabular-nums">{clock(drag ?? hover.time)}</span>
          {hover.marker?.label && drag == null && <span className="truncate font-mono text-muted-foreground">{hover.marker.label}</span>}
        </div>
      )}
    </div>
  )
}
