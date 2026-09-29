import type { ReactNode } from 'react'
import { cn } from '@/lib/utils'

/** Page scaffold for a settings section. */
export function SettingsPage({ title, description, actions, children }: { title: string; description?: ReactNode; actions?: ReactNode; children: ReactNode }) {
  return (
    <div className="mx-auto grid w-full max-w-3xl gap-8 px-4 py-6 @xl:px-6">
      <header className="flex flex-wrap items-end justify-between gap-3">
        <div className="grid gap-1">
          <h1 className="text-xl font-semibold tracking-tight">{title}</h1>
          {description && <p className="text-base text-muted-foreground">{description}</p>}
        </div>
        {actions}
      </header>
      {children}
    </div>
  )
}

/** A titled group of settings rows. */
export function SettingsGroup({ title, description, children, className }: { title: string; description?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <section className={cn('grid gap-3', className)}>
      <div className="grid gap-0.5">
        <h2 className="text-md font-semibold">{title}</h2>
        {description && <p className="text-sm text-muted-foreground">{description}</p>}
      </div>
      <div className="divide-y rounded-lg border bg-card">{children}</div>
    </section>
  )
}

/** One setting: label + description on the left, control on the right (stacks on narrow widths). */
export function SettingRow({
  label,
  description,
  htmlFor,
  children,
  stacked,
}: {
  label: ReactNode
  description?: ReactNode
  htmlFor?: string
  children: ReactNode
  /** Put the control below the label (wide controls). */
  stacked?: boolean
}) {
  return (
    <div className={cn('flex gap-x-6 gap-y-2.5 px-4 py-3', stacked ? 'flex-col' : 'flex-col @xl:flex-row @xl:items-center @xl:justify-between')}>
      <div className="grid min-w-0 gap-0.5">
        <label htmlFor={htmlFor} className="text-base font-medium">
          {label}
        </label>
        {description && <p className="text-sm text-muted-foreground">{description}</p>}
      </div>
      <div className={cn('flex shrink-0 items-center gap-2', stacked && 'w-full')}>{children}</div>
    </div>
  )
}
