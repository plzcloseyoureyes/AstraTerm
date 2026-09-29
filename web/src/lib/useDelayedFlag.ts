/*
 * Calm loading feedback (docs/UX.md "Loading states"): the single implementation of the "show it only if it takes a
 * while, then keep it long enough to read" rule. Every loading primitive in components/ui (Spinner, Delayed,
 * QueryState, LoadingState, BusyIcon, Button `loading`) is built on it; features should use those primitives and only
 * reach for the hook for custom indicators.
 *
 *   const show = useDelayedFlag(isSaving)            // false → true after 300 ms busy; stays true ≥ 600 ms
 *
 * The timing lives in DelayedFlag (plain class, no React, unit-tested with fake timers in lib/__tests__); the hook
 * only mirrors it into React state. SSR: the first render is always `false` (no timers run). StrictMode: the machine
 * is kept in a ref, the effect cleanup only cancels its timers, and the re-run re-arms them — so the double effect
 * invocation neither loses the shown state nor leaves a stuck indicator.
 */
import { useEffect, useRef, useState } from 'react'

export interface DelayedFlagOptions {
  /** Busy this long before the flag turns on (ms). Default 300. 0 = at once. */
  delay?: number
  /** Once on, the flag stays on at least this long (ms). Default 600. */
  minVisible?: number
}

/** Operations the user explicitly waits on (connect, upload, save, a refresh they asked for): the default. */
export const DELAYED_FLAG_DEFAULTS = { delay: 300, minVisible: 600 } as const

/**
 * Timing presets (docs/UX.md "Motion & feedback"):
 *   EXPLICIT_WAIT  300 ms / ≥ 600 ms   connect, upload, save, a refresh the user asked for (the default)
 *   NAVIGATION     1000 ms / ≥ 800 ms  opening a folder, switching views — with caches and prefetch it rarely shows
 */
export const DELAY_PRESETS = {
  EXPLICIT_WAIT: DELAYED_FLAG_DEFAULTS,
  NAVIGATION: { delay: 1000, minVisible: 800 },
} as const satisfies Record<string, Required<DelayedFlagOptions>>

export interface Clock {
  now(): number
  setTimeout(fn: () => void, ms: number): unknown
  clearTimeout(handle: unknown): void
  /** Calls fn once the frame showing the latest state change has been painted (optional: without it, a change
   *  counts as visible at once). */
  afterPaint?(fn: () => void): void
}

const realClock: Clock = {
  now: () => Date.now(),
  setTimeout: (fn, ms) => setTimeout(fn, ms),
  clearTimeout: (h) => clearTimeout(h as ReturnType<typeof setTimeout>),
  // The first rAF runs before the frame that carries the new state is painted, the second one after it.
  afterPaint: typeof requestAnimationFrame === 'function' ? (fn) => requestAnimationFrame(() => requestAnimationFrame(fn)) : undefined,
}

/**
 * The timing state machine behind useDelayedFlag. `set(busy)` may be called any number of times; `onChange` fires on
 * every transition of `shown`. `cancel()` drops pending timers without changing `shown` (a later `set` re-arms them).
 * The minimum visible time counts from the first painted frame showing the indicator (Clock.afterPaint), not from the
 * state change, so rendering time cannot eat into it.
 */
export class DelayedFlag {
  shown = false
  private busy = false
  private shownAt = 0
  /** The frame showing the indicator has been painted (shownAt is its time). */
  private painted = false
  private timer: unknown = null
  private opts: Required<DelayedFlagOptions>
  private readonly onChange: (shown: boolean) => void
  private readonly clock: Clock

  constructor(onChange: (shown: boolean) => void, opts: DelayedFlagOptions = {}, clock: Clock = realClock) {
    this.onChange = onChange
    this.clock = clock
    this.opts = { ...DELAYED_FLAG_DEFAULTS, ...stripUndefined(opts) }
  }

  configure(opts: DelayedFlagOptions): void {
    this.opts = { ...DELAYED_FLAG_DEFAULTS, ...stripUndefined(opts) }
  }

  set(busy: boolean): void {
    const was = this.busy
    this.busy = busy
    if (busy) {
      if (this.shown) {
        this.cancel() // a pending hide
        return
      }
      if (this.timer !== null && was) return // already waiting to show
      this.cancel()
      if (this.opts.delay <= 0) {
        this.show()
        return
      }
      this.timer = this.clock.setTimeout(() => {
        this.timer = null
        if (this.busy) this.show()
      }, this.opts.delay)
      return
    }
    if (!this.shown) {
      this.cancel() // a pending show
      return
    }
    if (this.timer !== null && !was) return // already waiting to hide
    this.cancel()
    // Not painted yet: the whole minimum is still ahead.
    const left = this.painted ? this.opts.minVisible - (this.clock.now() - this.shownAt) : this.opts.minVisible
    if (left <= 0) {
      this.hide()
      return
    }
    this.timer = this.clock.setTimeout(() => {
      this.timer = null
      if (!this.busy) this.hide()
    }, left)
  }

  cancel(): void {
    if (this.timer !== null) {
      this.clock.clearTimeout(this.timer)
      this.timer = null
    }
  }

  private show(): void {
    this.shown = true
    this.shownAt = this.clock.now()
    const afterPaint = this.clock.afterPaint
    this.painted = !afterPaint
    this.onChange(true)
    afterPaint?.call(this.clock, () => {
      if (!this.shown || this.painted) return
      this.painted = true
      this.shownAt = this.clock.now()
    })
  }

  private hide(): void {
    this.shown = false
    this.onChange(false)
  }
}

function stripUndefined(o: DelayedFlagOptions): DelayedFlagOptions {
  const out: DelayedFlagOptions = {}
  if (o.delay !== undefined) out.delay = o.delay
  if (o.minVisible !== undefined) out.minVisible = o.minVisible
  return out
}

/** `busy`, debounced for display: on after `delay` ms of busy (300), then on for at least `minVisible` ms (600). */
export function useDelayedFlag(busy: boolean, { delay, minVisible }: DelayedFlagOptions = {}): boolean {
  const [shown, setShown] = useState(false)
  const ref = useRef<DelayedFlag | null>(null)
  if (ref.current === null) ref.current = new DelayedFlag(setShown, { delay, minVisible })
  useEffect(() => {
    const flag = ref.current!
    flag.configure({ delay, minVisible })
    flag.set(busy)
    return () => flag.cancel()
  }, [busy, delay, minVisible])
  return shown
}

/**
 * First-load gate for components that return early while loading:
 *
 *   const gate = useLoadingGate(q.isPending)
 *   if (gate.hold) return gate.show ? <LoadingPane immediate label="Loading…" /> : null
 *
 * `show` = the placeholder is due (busy ≥ 300 ms); `hold` = keep showing it (or the blank) — true while busy and while
 * a shown placeholder still has part of its 600 ms minimum left, so it never vanishes a few ms after appearing.
 * Components that render the placeholder inline can use <LoadingState> / <QueryState> instead.
 */
export function useLoadingGate(busy: boolean, opts?: DelayedFlagOptions): { hold: boolean; show: boolean } {
  const show = useDelayedFlag(busy, opts)
  return { hold: busy || show, show }
}
