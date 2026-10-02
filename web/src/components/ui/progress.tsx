/*
 * Progress bar (docs/UX.md "Loading states"): monotonic — it never moves backwards for the same `resetKey` (chunk
 * retries, resumed uploads) — and eased (500 ms) so bursts of updates glide instead of jump. Without a value it is a
 * still, softly tinted full bar ("working, amount unknown"): nothing sweeps or loops (docs/UX.md "Motion & feedback").
 * Use that only when progress is truly unknown.
 */
import { useRef } from 'react'
import { cn } from '@/lib/utils'

const tones = {
  primary: 'bg-primary',
  success: 'bg-success',
  warning: 'bg-warning',
  destructive: 'bg-destructive',
  muted: 'bg-muted-foreground/50',
} as const

export interface ProgressBarProps {
  /** 0…1; null / undefined = indeterminate. */
  value?: number | null
  /** Progress may only grow while this stays the same (e.g. a transfer id); a new key starts from the new value. */
  resetKey?: string | number
  tone?: keyof typeof tones
  /** Accessible name. */
  label?: string
  className?: string
}

export function ProgressBar({ value, resetKey, tone = 'primary', label, className }: ProgressBarProps) {
  const last = useRef<{ key: ProgressBarProps['resetKey']; value: number }>({ key: resetKey, value: 0 })
  const indeterminate = value === null || value === undefined || Number.isNaN(value)
  let shown = 0
  if (!indeterminate) {
    const v = Math.min(1, Math.max(0, value))
    shown = last.current.key === resetKey ? Math.max(last.current.value, v) : v
    last.current = { key: resetKey, value: shown }
  }
  return (
    <div
      role="progressbar"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={indeterminate ? undefined : Math.round(shown * 100)}
      className={cn('relative h-1 w-full overflow-hidden rounded-full bg-muted', className)}
    >
      {indeterminate ? (
        // A segment sliding one way; with reduced motion the animation is off and it is a still, dim bar.
        <div className={cn('h-full w-2/5 animate-progress-slide rounded-full motion-reduce:w-full motion-reduce:opacity-35', tones[tone])} />
      ) : (
        <div className={cn('h-full rounded-full transition-[width] duration-500 ease-out', tones[tone])} style={{ width: `${shown * 100}%` }} />
      )}
    </div>
  )
}
