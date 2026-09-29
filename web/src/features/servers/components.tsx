/*
 * Small presentational pieces shared by the servers tab, the dialogs and the syslog viewer.
 */
import type { ReactNode } from 'react'
import { TriangleAlert } from 'lucide-react'
import { Tooltip } from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'
import { STATE_LABEL, TONE_DOT, statusTone } from './model'
import type { ServerStatusEx } from './types'

export function StatusDot({ status, className }: { status: ServerStatusEx | undefined; className?: string }) {
  const tone = statusTone(status)
  const label = status ? STATE_LABEL[status.state] : 'Unknown'
  return (
    <span
      role="img"
      aria-label={label}
      className={cn('inline-block size-2.5 shrink-0 rounded-full transition-colors', TONE_DOT[tone], className)}
    />
  )
}

export function StateBadge({ status }: { status: ServerStatusEx }) {
  const tone = statusTone(status)
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1.5 rounded-full border px-2 py-px text-xs font-medium whitespace-nowrap',
        tone === 'running' && 'border-success/30 bg-success/10 text-success',
        tone === 'stopped' && 'border-border text-muted-foreground',
        tone === 'busy' && 'border-warning/30 bg-warning/10 text-warning',
        tone === 'error' && 'border-destructive/30 bg-destructive/10 text-destructive',
      )}
    >
      <StatusDot status={status} className="size-1.5 shadow-none" />
      {STATE_LABEL[status.state]}
    </span>
  )
}

/** Compact list of warnings (first `max`, the rest in a tooltip). */
export function Warnings({ items, max = 2, className }: { items: string[]; max?: number; className?: string }) {
  if (!items.length) return null
  const shown = items.slice(0, max)
  const rest = items.slice(max)
  return (
    <ul className={cn('grid gap-1', className)} aria-label="Warnings">
      {shown.map((w) => (
        <li key={w} className="flex items-start gap-1.5 text-xs leading-snug text-warning">
          <TriangleAlert className="mt-px size-3.5 shrink-0" aria-hidden />
          <span>{w}</span>
        </li>
      ))}
      {rest.length > 0 && (
        <li className="text-xs text-muted-foreground">
          <Tooltip content={<span className="whitespace-pre-line">{rest.join('\n')}</span>}>
            <button type="button" className="underline-offset-2 hover:underline">
              +{rest.length} more warning{rest.length === 1 ? '' : 's'}
            </button>
          </Tooltip>
        </li>
      )}
    </ul>
  )
}

/** Label / value row of the server card. */
export function InfoRow({ label, children, className }: { label: string; children: ReactNode; className?: string }) {
  return (
    <div className={cn('grid grid-cols-[4.75rem_minmax(0,1fr)] items-center gap-2 text-sm', className)}>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="min-w-0">{children}</dd>
    </div>
  )
}
