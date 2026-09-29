import type { ReactNode } from 'react'
import { cn } from '@/lib/utils'

export function BrandMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 64 64" className={cn('size-10', className)} aria-hidden>
      <rect width="64" height="64" rx="14" fill="var(--primary)" />
      <path d="M16 22l12 10-12 10" stroke="var(--primary-foreground)" strokeWidth="5" fill="none" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M32 44h16" stroke="var(--primary-foreground)" strokeWidth="5" strokeLinecap="round" />
    </svg>
  )
}

/**
 * Card layout for the setup / login / unreachable screens. The column is anchored at a fixed height (about a third
 * down a tall window, 2.5rem on a short one) instead of being centred: content that grows — an error message, the
 * next wizard step, the second-factor step — extends downwards and nothing above it moves.
 */
export function AuthLayout({ children, footer, wide }: { children: ReactNode; footer?: ReactNode; wide?: boolean }) {
  return (
    <div className="relative flex h-full w-full flex-col items-center overflow-auto bg-background px-4 pt-[max(2.5rem,calc(50dvh-17rem))] pb-10">
      {/* subtle backdrop */}
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0 opacity-70"
        style={{
          background:
            'radial-gradient(60rem 30rem at 50% -10%, color-mix(in oklab, var(--primary) 16%, transparent), transparent 70%), radial-gradient(40rem 24rem at 100% 110%, color-mix(in oklab, var(--primary) 8%, transparent), transparent 70%)',
        }}
      />
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0 opacity-[0.35] [background-image:linear-gradient(var(--border)_1px,transparent_1px),linear-gradient(90deg,var(--border)_1px,transparent_1px)] [background-size:48px_48px] [mask-image:radial-gradient(ellipse_at_center,black_20%,transparent_70%)]"
      />
      <main className={cn('relative w-full', wide ? 'max-w-lg' : 'max-w-sm')}>{children}</main>
      {footer && <footer className="relative mt-6 text-center text-xs text-muted-foreground">{footer}</footer>}
    </div>
  )
}

export function AuthCard({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={cn('rounded-xl border bg-card/95 p-6 shadow-popover backdrop-blur-sm', className)}>{children}</div>
}
