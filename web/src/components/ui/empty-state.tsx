import * as React from 'react'
import type { IconType } from '@/app/registry'
import { cn } from '@/lib/utils'

export interface EmptyStateProps {
  icon?: IconType
  title: React.ReactNode
  description?: React.ReactNode
  /** Buttons / links. */
  action?: React.ReactNode
  className?: string
  size?: 'sm' | 'md'
}

export function EmptyState({ icon: Icon, title, description, action, className, size = 'md' }: EmptyStateProps) {
  return (
    <div
      className={cn(
        'flex flex-col items-center justify-center text-center',
        size === 'sm' ? 'gap-1.5 px-4 py-6' : 'gap-2.5 px-6 py-12',
        className,
      )}
    >
      {Icon && (
        <div
          className={cn(
            'flex items-center justify-center rounded-xl border bg-muted/50 text-muted-foreground',
            size === 'sm' ? 'mb-1 size-9' : 'mb-1.5 size-12',
          )}
        >
          <Icon className={size === 'sm' ? 'size-4' : 'size-5'} />
        </div>
      )}
      <div className={cn('font-medium text-foreground', size === 'sm' ? 'text-base' : 'text-md')}>{title}</div>
      {description && <div className="max-w-sm text-sm text-muted-foreground text-balance">{description}</div>}
      {action && <div className="mt-2 flex flex-wrap items-center justify-center gap-2">{action}</div>}
    </div>
  )
}
