/*
 * Skeleton placeholders (docs/UX.md "Loading states"): static, low-contrast shapes that stand in for content on a
 * FIRST load only — never on a refetch (keep the old content) — and only through LoadingState / QueryState, which show
 * them after 300 ms and keep them at least 600 ms. They do not animate: a calm, still shape reads as "on its way".
 */
import type { CSSProperties } from 'react'
import { cn } from '@/lib/utils'

export function Skeleton({ className, style }: { className?: string; style?: CSSProperties }) {
  return <div aria-hidden className={cn('rounded-sm bg-muted/60', className)} style={style} />
}

/** Placeholder rows of a list or table: an icon block, a text bar of varying width, a short trailing bar. */
export function SkeletonRows({ rows = 8, rowHeight = 28, icon = true, className }: { rows?: number; rowHeight?: number; icon?: boolean; className?: string }) {
  return (
    <div aria-hidden className={cn('overflow-hidden', className)}>
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="flex items-center gap-2.5 px-3" style={{ height: rowHeight }}>
          {icon && <Skeleton className="size-4 shrink-0" />}
          <Skeleton className="h-2.5 rounded-full" style={{ width: `${35 + ((i * 37) % 45)}%` }} />
          <Skeleton className="ml-auto h-2.5 w-10 rounded-full opacity-70" />
        </div>
      ))}
    </div>
  )
}

/** Placeholder paragraph lines. */
export function SkeletonText({ lines = 3, className }: { lines?: number; className?: string }) {
  return (
    <div aria-hidden className={cn('grid gap-2', className)}>
      {Array.from({ length: lines }, (_, i) => (
        <Skeleton key={i} className="h-2.5 rounded-full" style={{ width: i === lines - 1 ? '60%' : `${88 - ((i * 13) % 20)}%` }} />
      ))}
    </div>
  )
}
