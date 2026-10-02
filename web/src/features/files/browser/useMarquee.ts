/*
 * Rubber-band selection for the file list (Explorer's details view): press on empty space or on the blank part of a
 * row and drag; the rows the rectangle covers are selected (added to the selection with Ctrl / ⌘ / Shift). The list
 * scrolls while the pointer is near its top or bottom edge. Pressing on a file's icon or name drags the file instead.
 */
import { useCallback, useEffect, useRef, useState, type RefObject } from 'react'
import type { ListRow } from '../types'
import type { BrowserController } from './controller'

/** Pointer travel before a press becomes a rectangle (a click stays a click). */
const THRESHOLD = 5
/** Distance from the top / bottom edge where the list starts scrolling, and the fastest step per frame. */
const EDGE = 28
const MAX_STEP = 18

export interface MarqueeBox {
  left: number
  top: number
  width: number
  height: number
}

interface Drag {
  /** Press point in scroll-content coordinates. */
  x: number
  y: number
  /** Last pointer position (viewport). */
  cx: number
  cy: number
  base: ReadonlySet<string>
  active: boolean
  frame: number
}

export function useMarquee(opts: {
  scrollRef: RefObject<HTMLDivElement | null>
  rows: ListRow[]
  rowHeight: number
  /** Height of the sticky header above the first row. */
  headerHeight: number
  controller: BrowserController
}) {
  const [box, setBox] = useState<MarqueeBox | null>(null)
  const drag = useRef<Drag | null>(null)
  const ended = useRef(false)
  const latest = useRef(opts)
  useEffect(() => {
    latest.current = opts
  })

  const update = useCallback(() => {
    const d = drag.current
    const { scrollRef, rows, rowHeight, headerHeight, controller } = latest.current
    const el = scrollRef.current
    if (!d || !el) return
    const r = el.getBoundingClientRect()
    const x = Math.min(Math.max(d.cx - r.left, 0), el.clientWidth) + el.scrollLeft
    const y = Math.min(Math.max(d.cy - r.top, headerHeight), el.clientHeight) + el.scrollTop
    if (!d.active) {
      if (Math.hypot(x - d.x, y - d.y) < THRESHOLD) return
      d.active = true
    }
    const top = Math.min(y, d.y)
    const bottom = Math.max(y, d.y)
    setBox({ left: Math.min(x, d.x), top, width: Math.abs(x - d.x), height: bottom - top })
    const first = Math.max(0, Math.floor((top - headerHeight) / rowHeight))
    const last = Math.min(rows.length - 1, Math.floor((bottom - headerHeight) / rowHeight))
    const next = new Set(d.base)
    for (let i = first; i <= last; i++) if (!rows[i].parent) next.add(rows[i].entry.path)
    controller.setSelection(next)
  }, [])

  const stop = useCallback(() => {
    const d = drag.current
    if (!d) return
    cancelAnimationFrame(d.frame)
    drag.current = null
    setBox(null)
    if (d.active) {
      // The click that follows the release must not collapse the selection to the row under the pointer.
      ended.current = true
      setTimeout(() => (ended.current = false))
    }
  }, [])

  useEffect(() => {
    const move = (e: MouseEvent) => {
      const d = drag.current
      if (!d) return
      d.cx = e.clientX
      d.cy = e.clientY
      update()
    }
    window.addEventListener('mousemove', move)
    window.addEventListener('mouseup', stop)
    window.addEventListener('blur', stop)
    return () => {
      window.removeEventListener('mousemove', move)
      window.removeEventListener('mouseup', stop)
      window.removeEventListener('blur', stop)
      stop()
    }
  }, [update, stop])

  const start = useCallback(
    (e: React.MouseEvent) => {
      const { scrollRef, headerHeight, controller } = latest.current
      const el = scrollRef.current
      if (!el) return
      const r = el.getBoundingClientRect()
      const additive = e.ctrlKey || e.metaKey || e.shiftKey
      const d: Drag = {
        x: e.clientX - r.left + el.scrollLeft,
        y: e.clientY - r.top + el.scrollTop,
        cx: e.clientX,
        cy: e.clientY,
        base: additive ? new Set(controller.view.selected) : new Set(),
        active: false,
        frame: 0,
      }
      drag.current = d
      // Scroll while the pointer rests near (or beyond) the top / bottom edge.
      const tick = () => {
        if (drag.current !== d) return
        const box = el.getBoundingClientRect()
        const over = d.cy < box.top + headerHeight + EDGE ? d.cy - (box.top + headerHeight + EDGE) : d.cy > box.bottom - EDGE ? d.cy - (box.bottom - EDGE) : 0
        if (d.active && over) {
          const before = el.scrollTop
          el.scrollTop += Math.sign(over) * Math.min(MAX_STEP, Math.ceil(Math.abs(over) / 3))
          if (el.scrollTop !== before) update()
        }
        d.frame = requestAnimationFrame(tick)
      }
      d.frame = requestAnimationFrame(tick)
    },
    [update],
  )

  /** True right after a rectangle ended: the browser's click for that release should be ignored. */
  const justEnded = useCallback(() => ended.current, [])
  return { box, start, justEnded }
}
