/*
 * Status dots (docs/UX.md "Motion & feedback"): always steady — status is colour + shape + a short text, never motion.
 *   settled states   a solid dot (connected = green, error = red, stopped = grey)
 *   pending states   (connecting, reconnecting, starting) a steady ring outline in the tone's colour
 * A state change swaps the dot once, without animation. `useSteadyStatus` keeps short pending blips (a reconnect that
 * succeeds at once) from swapping the dot at all.
 */
import { useRef } from 'react'
import { DELAY_PRESETS, useDelayedFlag } from '@/lib/useDelayedFlag'
import { cn } from '@/lib/utils'

export type StatusTone = 'success' | 'warning' | 'destructive' | 'info' | 'primary' | 'muted'

const toneBg: Record<StatusTone, string> = {
  success: 'bg-success',
  warning: 'bg-warning',
  destructive: 'bg-destructive',
  info: 'bg-info',
  primary: 'bg-primary',
  muted: 'bg-muted-foreground/60',
}

const toneRing: Record<StatusTone, string> = {
  success: 'border-success',
  warning: 'border-warning',
  destructive: 'border-destructive',
  info: 'border-info',
  primary: 'border-primary',
  muted: 'border-muted-foreground/60',
}

/**
 * Formerly the pending "breathe" for elements other than dots. Nothing animates any more: kept (empty) so existing
 * callers still compile; pending text is plain text.
 * @deprecated steady by design — convey pending with text / `statusDotClass(tone, true)`.
 */
export const pendingPulseClass = ''

/** Class names of a dot of `tone` (for status maps that store class strings); `pending` = a steady ring outline. */
export function statusDotClass(tone: StatusTone, pending = false): string {
  return pending ? cn('border-[1.5px] bg-transparent', toneRing[tone]) : toneBg[tone]
}

export function StatusDot({ tone, pending, className, label }: { tone: StatusTone; pending?: boolean; className?: string; label?: string }) {
  return (
    <span
      role={label ? 'img' : undefined}
      aria-label={label}
      aria-hidden={label ? undefined : true}
      className={cn('inline-block size-1.5 shrink-0 rounded-full', statusDotClass(tone, pending), className)}
    />
  )
}

/**
 * The status to *display* for a live `value`: a pending state shows only once it has lasted 300 ms (then for at least
 * 600 ms, like every indicator); until then the previous settled state stays on screen (or nothing, when there was
 * none). A reconnect that succeeds at once therefore never swaps the dot green → amber → green.
 */
export function useSteadyStatus<T>(value: T, isPending: (v: T) => boolean): T | undefined {
  const pending = isPending(value)
  const show = useDelayedFlag(pending, DELAY_PRESETS.EXPLICIT_WAIT)
  const settled = useRef<T | undefined>(pending ? undefined : value)
  const lastPending = useRef<T>(value)
  if (pending) lastPending.current = value
  else settled.current = value
  if (pending) return show ? value : settled.current
  return show ? lastPending.current : value
}
