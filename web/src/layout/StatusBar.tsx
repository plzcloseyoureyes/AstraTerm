import * as React from 'react'
import { statusItems } from '@/app/registry'
import { ErrorBoundary } from '@/components/error-boundary'
import { Tooltip } from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'

/**
 * Status bar building block for registered items: a compact button (or plain text when no onClick).
 * Usage in a feature: registerStatusItem({ id, align: 'right', order: 50, component: () => <StatusBarItem .../> })
 */
export function StatusBarItem({
  icon: Icon,
  children,
  tooltip,
  onClick,
  className,
  tone,
  'aria-label': ariaLabel,
}: {
  icon?: React.ComponentType<{ className?: string }>
  children?: React.ReactNode
  tooltip?: React.ReactNode
  onClick?: () => void
  className?: string
  tone?: 'default' | 'success' | 'warning' | 'danger' | 'muted' | 'accent'
  'aria-label'?: string
}) {
  const toneClass = {
    default: '',
    success: 'text-success',
    warning: 'text-warning',
    danger: 'text-destructive',
    muted: 'text-muted-foreground',
    accent: 'text-primary',
  }[tone ?? 'default']
  const content = (
    <>
      {Icon && <Icon className="size-3.5 shrink-0" />}
      {children != null && <span className="truncate">{children}</span>}
    </>
  )
  const cls = cn('flex h-full max-w-72 items-center gap-1 px-1.5 whitespace-nowrap', toneClass, className)
  const el = onClick ? (
    <button
      type="button"
      onClick={onClick}
      aria-label={ariaLabel}
      className={cn(cls, 'rounded-sm outline-none hover:bg-foreground/8 focus-visible:ring-1 focus-visible:ring-ring')}
    >
      {content}
    </button>
  ) : (
    <div className={cls} aria-label={ariaLabel}>
      {content}
    </div>
  )
  return tooltip ? (
    <Tooltip content={tooltip} side="top">
      {el}
    </Tooltip>
  ) : (
    el
  )
}

/** Bottom status bar: registered items (left / right). */
export function StatusBar() {
  const items = statusItems.useList()
  const left = items.filter((i) => i.align === 'left')
  const right = items.filter((i) => i.align === 'right')
  return (
    <footer
      role="status"
      aria-label="Status bar"
      className="flex h-6 shrink-0 items-center justify-between gap-2 px-1.5 text-xs text-statusbar-foreground select-none"
    >
      <div className="flex h-full min-w-0 items-center gap-0.5 overflow-hidden">
        {left.map((i) => (
          <ErrorBoundary key={i.id} label={i.id} fallback={() => null}>
            <i.component />
          </ErrorBoundary>
        ))}
      </div>
      <div className="flex h-full min-w-0 items-center gap-0.5 overflow-hidden">
        {right.map((i) => (
          <ErrorBoundary key={i.id} label={i.id} fallback={() => null}>
            <i.component />
          </ErrorBoundary>
        ))}
      </div>
    </footer>
  )
}
