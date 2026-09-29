import * as React from 'react'
import { clamp, cn } from '@/lib/utils'

export interface ResizeHandleProps {
  /** Current size (px) of the panel this handle resizes. */
  size: number
  min: number
  max: number
  onResize: (size: number) => void
  /** Fired once when a drag ends (persist here). */
  onResizeEnd?: (size: number) => void
  /** Which edge of the panel the handle sits on: 'right' grows with +x, 'left' grows with −x (etc). */
  edge: 'right' | 'left' | 'top' | 'bottom'
  /** Double-click / Enter handler (e.g. collapse/expand). */
  onToggle?: () => void
  label?: string
  /** No line at rest (the handle sits in a gap between surfaces); the accent line still shows on hover / drag / focus. */
  quiet?: boolean
  className?: string
}

/**
 * Accessible splitter (role="separator"): pointer drag, arrow keys (Shift = 5×), Home/End to min/max.
 * (Replaces react-resizable-panels, which is not installed.)
 */
export function ResizeHandle({ size, min, max, onResize, onResizeEnd, edge, onToggle, label = 'Resize', quiet = false, className }: ResizeHandleProps) {
  const [dragging, setDragging] = React.useState(false)
  const start = React.useRef<{ pos: number; size: number; last: number } | null>(null)
  const horizontal = edge === 'left' || edge === 'right'
  const sign = edge === 'right' || edge === 'bottom' ? 1 : -1

  const onPointerDown = (e: React.PointerEvent<HTMLDivElement>) => {
    if (e.button !== 0) return
    e.preventDefault()
    e.currentTarget.setPointerCapture(e.pointerId)
    start.current = { pos: horizontal ? e.clientX : e.clientY, size, last: size }
    setDragging(true)
  }
  const onPointerMove = (e: React.PointerEvent<HTMLDivElement>) => {
    const s = start.current
    if (!s) return
    const delta = ((horizontal ? e.clientX : e.clientY) - s.pos) * sign
    const next = Math.round(clamp(s.size + delta, min, max))
    if (next !== s.last) {
      s.last = next
      onResize(next)
    }
  }
  const end = (e: React.PointerEvent<HTMLDivElement>) => {
    const s = start.current
    if (!s) return
    start.current = null
    setDragging(false)
    try {
      e.currentTarget.releasePointerCapture(e.pointerId)
    } catch {
      /* already released */
    }
    onResizeEnd?.(s.last)
  }
  const onKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    const step = e.shiftKey ? 80 : 16
    const growKeys = horizontal ? (sign > 0 ? 'ArrowRight' : 'ArrowLeft') : sign > 0 ? 'ArrowDown' : 'ArrowUp'
    const shrinkKeys = horizontal ? (sign > 0 ? 'ArrowLeft' : 'ArrowRight') : sign > 0 ? 'ArrowUp' : 'ArrowDown'
    let next: number | null = null
    if (e.key === growKeys) next = size + step
    else if (e.key === shrinkKeys) next = size - step
    else if (e.key === 'Home') next = min
    else if (e.key === 'End') next = max
    else if (e.key === 'Enter' && onToggle) {
      e.preventDefault()
      onToggle()
      return
    }
    if (next == null) return
    e.preventDefault()
    const v = Math.round(clamp(next, min, max))
    onResize(v)
    onResizeEnd?.(v)
  }

  return (
    <div
      role="separator"
      aria-orientation={horizontal ? 'vertical' : 'horizontal'}
      aria-label={label}
      aria-valuenow={size}
      aria-valuemin={min}
      aria-valuemax={max}
      tabIndex={0}
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={end}
      onPointerCancel={end}
      onDoubleClick={onToggle}
      onKeyDown={onKeyDown}
      data-dragging={dragging || undefined}
      className={cn(
        'group/handle relative z-20 shrink-0 touch-none outline-none select-none',
        horizontal ? 'w-px cursor-col-resize' : 'h-px cursor-row-resize',
        className,
      )}
    >
      {/* wide invisible hit area + accent line on hover/drag/focus */}
      <div className={cn('absolute', horizontal ? '-inset-x-1 inset-y-0' : '-inset-y-1 inset-x-0')} />
      <div
        className={cn(
          'pointer-events-none absolute transition-colors delay-100',
          quiet ? 'bg-transparent' : 'bg-border',
          horizontal ? 'inset-y-0 left-0 w-px' : 'inset-x-0 top-0 h-px',
          'group-hover/handle:bg-primary/70 group-focus-visible/handle:bg-primary group-data-[dragging]/handle:bg-primary',
          horizontal
            ? 'group-hover/handle:-left-px group-hover/handle:w-[3px] group-data-[dragging]/handle:-left-px group-data-[dragging]/handle:w-[3px]'
            : 'group-hover/handle:-top-px group-hover/handle:h-[3px] group-data-[dragging]/handle:-top-px group-data-[dragging]/handle:h-[3px]',
        )}
      />
    </div>
  )
}
