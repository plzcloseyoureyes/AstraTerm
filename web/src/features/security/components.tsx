/*
 * Small building blocks shared by the security and admin views.
 */
import { useState, type ReactNode } from 'react'
import { Check, Copy, Globe, Laptop, Monitor, Smartphone, Tablet } from 'lucide-react'
import type { IconType } from '@/app/registry'
import { Button } from '@/components/ui/button'
import { Spinner } from '@/components/ui/spinner'
import { StatusDot } from '@/components/ui/status-dot'
import { Tooltip } from '@/components/ui/tooltip'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { cn, copyText, formatDateTime, formatRelativeTime } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth'
import type { PasswordPolicyView } from './types'

// --- user agents ------------------------------------------------------------------------------------------------------

export interface DeviceInfo {
  browser: string
  os: string
  kind: 'desktop' | 'phone' | 'tablet' | 'other'
}

/** Best-effort "Firefox on macOS" style description of a User-Agent header. */
export function describeUserAgent(ua: string | undefined): DeviceInfo {
  const s = ua ?? ''
  const browser = /Edg\//.test(s)
    ? 'Edge'
    : /OPR\//.test(s)
      ? 'Opera'
      : /Firefox\//.test(s)
        ? 'Firefox'
        : /Chrome\//.test(s)
          ? 'Chrome'
          : /Safari\//.test(s) && /Version\//.test(s)
            ? 'Safari'
            : /curl\//i.test(s)
              ? 'curl'
              : /Go-http-client/.test(s)
                ? 'API client'
                : s
                  ? 'Browser'
                  : 'Unknown client'
  const os = /iPhone|iPad|iPod/.test(s)
    ? 'iOS'
    : /Android/.test(s)
      ? 'Android'
      : /Mac OS X|Macintosh/.test(s)
        ? 'macOS'
        : /Windows/.test(s)
          ? 'Windows'
          : /CrOS/.test(s)
            ? 'ChromeOS'
            : /Linux/.test(s)
              ? 'Linux'
              : ''
  const kind = /iPad|Tablet/.test(s) ? 'tablet' : /Mobile|iPhone|Android/.test(s) ? 'phone' : os ? 'desktop' : 'other'
  return { browser, os, kind }
}

export function DeviceIcon({ kind, className }: { kind: DeviceInfo['kind']; className?: string }) {
  const Icon = kind === 'phone' ? Smartphone : kind === 'tablet' ? Tablet : kind === 'desktop' ? Laptop : Monitor
  return <Icon className={cn('size-4', className)} />
}

// --- time --------------------------------------------------------------------------------------------------------------

const dayFormat = new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'short', day: 'numeric' })

/** "3 minutes ago"; under a minute "just now" (no text ticking every second). */
function relative(value: string): string {
  return Math.abs(Date.now() - Date.parse(value)) < 60_000 ? 'just now' : formatRelativeTime(value)
}

/** Relative time ("3 hours ago") or a calendar day ("Sep 28, 2026"), with the exact timestamp in a tooltip. */
export function When({ value, fallback = 'Never', day }: { value?: string | null; fallback?: string; day?: boolean }) {
  if (!value) return <span className="text-muted-foreground">{fallback}</span>
  return (
    <Tooltip content={formatDateTime(value)}>
      <time dateTime={value} className="tabular-nums">
        {day ? dayFormat.format(new Date(value)) : relative(value)}
      </time>
    </Tooltip>
  )
}

// --- copy button ----------------------------------------------------------------------------------------------------------

export function CopyButton({ text, label = 'Copy', size = 'sm', variant = 'secondary' }: { text: string; label?: string; size?: 'xs' | 'sm'; variant?: 'secondary' | 'ghost' | 'outline' }) {
  const [done, setDone] = useState(false)
  return (
    <Button
      size={size}
      variant={variant}
      onClick={async () => {
        if (await copyText(text)) {
          setDone(true)
          setTimeout(() => setDone(false), 1500)
        }
      }}
    >
      {done ? <Check className="text-success" /> : <Copy />}
      {done ? 'Copied' : label}
    </Button>
  )
}

// --- password policy ---------------------------------------------------------------------------------------------------------

const DEFAULT_POLICY: PasswordPolicyView = { minLength: 8, requireClasses: 0, disallowUsername: false }

/** The server's password rules (from /api/auth/state). */
export function usePasswordPolicy(): PasswordPolicyView {
  return useAuthStore((s) => (s.state as { passwordPolicy?: PasswordPolicyView } | null)?.passwordPolicy ?? DEFAULT_POLICY)
}

/** Validates a new password against the policy; null when acceptable. */
export function passwordProblem(pw: string, policy: PasswordPolicyView, username?: string): string | null {
  if (!pw) return null
  if ([...pw].length < policy.minLength) return `At least ${policy.minLength} characters`
  if (policy.requireClasses > 0) {
    const classes = [/\p{Ll}/u, /\p{Lu}/u, /\p{Nd}/u, /[^\p{Ll}\p{Lu}\p{Nd}]/u].filter((re) => re.test(pw)).length
    if (classes < policy.requireClasses) return `Mix at least ${policy.requireClasses} of: lowercase, uppercase, digits, symbols`
  }
  if (policy.disallowUsername && username && username.length >= 3 && pw.toLowerCase().includes(username.toLowerCase())) {
    return 'Must not contain the user name'
  }
  return null
}

export function policyHint(policy: PasswordPolicyView): string {
  const parts = [`at least ${policy.minLength} characters`]
  if (policy.requireClasses > 0) parts.push(`${policy.requireClasses} kinds of characters`)
  if (policy.disallowUsername) parts.push('not containing your user name')
  return `Use ${parts.join(', ')}.`
}

// --- layout -------------------------------------------------------------------------------------------------------------

/** A titled card section of the Security / Admin views. */
export function Section({ title, description, actions, children, className }: { title: ReactNode; description?: ReactNode; actions?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <section className={cn('grid gap-3', className)}>
      <div className="flex flex-wrap items-end justify-between gap-2">
        <div className="grid min-w-0 gap-0.5">
          <h2 className="text-md font-semibold">{title}</h2>
          {description && <p className="text-sm text-muted-foreground">{description}</p>}
        </div>
        {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
      </div>
      {children}
    </section>
  )
}

/** Status line with an icon (setting cards). */
export function StatusPill({ ok, children }: { ok: boolean; children: ReactNode }) {
  return (
    <span className={cn('inline-flex items-center gap-1.5 text-sm font-medium', ok ? 'text-success' : 'text-muted-foreground')}>
      <StatusDot tone={ok ? 'success' : 'muted'} />
      {children}
    </span>
  )
}

/** Notice box (info / warning). */
export function Notice({ tone = 'info', icon: Icon = Globe, children, action }: { tone?: 'info' | 'warning' | 'destructive'; icon?: typeof Globe; children: ReactNode; action?: ReactNode }) {
  return (
    <div
      className={cn(
        'flex items-start gap-2.5 rounded-md border px-3 py-2 text-sm',
        tone === 'warning' && 'border-warning/40 bg-warning/8',
        tone === 'info' && 'border-info/30 bg-info/6',
        tone === 'destructive' && 'border-destructive/40 bg-destructive/8',
      )}
    >
      <Icon className={cn('mt-0.5 size-4 shrink-0', tone === 'warning' ? 'text-warning' : tone === 'destructive' ? 'text-destructive' : 'text-info')} />
      <div className="min-w-0 flex-1">{children}</div>
      {action}
    </div>
  )
}

/** Heading of a section page (security / admin tabs). */
export function PageHeader({ title, description, actions }: { title: string; description?: ReactNode; actions?: ReactNode }) {
  return (
    <header className="mb-6 flex flex-wrap items-end justify-between gap-3">
      <div className="grid gap-1">
        <h1 className="text-xl font-semibold tracking-tight">{title}</h1>
        {description && <p className="text-base text-muted-foreground">{description}</p>}
      </div>
      {actions}
    </header>
  )
}

export interface SectionNavItem<T extends string> {
  id: T
  label: string
  icon: IconType
  /** Small status next to the label (count, "Required"). */
  badge?: ReactNode
}

/**
 * Frame of the Security and Administration tabs: a section list (a column on wide panes, a scrolling strip on narrow
 * ones) and the scrolling page. Every section's page starts at the same place, whatever its content width.
 */
export function SectionLayout<T extends string>({
  label,
  items,
  active,
  onSelect,
  className,
  children,
}: {
  /** Heading of the section list (also its accessible name). */
  label: string
  items: SectionNavItem<T>[]
  active: T
  onSelect: (id: T) => void
  /** Width of the page column (default max-w-3xl). */
  className?: string
  children: ReactNode
}) {
  return (
    <div className="@container flex h-full min-h-0 bg-background">
      <div className="flex min-h-0 w-full flex-col @3xl:flex-row">
        <nav
          aria-label={label}
          className="flex shrink-0 gap-0.5 overflow-x-auto border-b px-2 py-1.5 @3xl:w-52 @3xl:flex-col @3xl:overflow-visible @3xl:border-r @3xl:border-b-0 @3xl:py-3"
        >
          <div className="hidden px-2 pb-2 text-xs font-medium tracking-wide text-muted-foreground uppercase @3xl:block">{label}</div>
          {items.map((it) => {
            const current = it.id === active
            return (
              <button
                key={it.id}
                type="button"
                aria-current={current ? 'page' : undefined}
                onClick={() => onSelect(it.id)}
                className={cn(
                  'flex h-8 shrink-0 items-center gap-2 rounded-md px-2.5 text-base whitespace-nowrap outline-none transition-colors duration-150',
                  'focus-visible:ring-2 focus-visible:ring-ring/60',
                  current ? 'bg-accent font-medium text-foreground' : 'text-muted-foreground hover:bg-accent/60 hover:text-foreground',
                )}
              >
                <it.icon className="size-4" />
                <span className="flex-1 text-left">{it.label}</span>
                {it.badge}
              </button>
            )
          })}
        </nav>
        <div className="relative min-h-0 min-w-0 flex-1 overflow-y-auto">
          <div className={cn('mx-auto w-full px-4 py-6 @xl:px-6', className ?? 'max-w-3xl')}>{children}</div>
        </div>
      </div>
    </div>
  )
}

/**
 * A button's icon that turns into the spinner — in place, so the label never moves — once `busy` has lasted 300 ms
 * (then ≥ 600 ms). Buttons using it stay enabled-looking for quick operations; guard the click with `busy` instead.
 */
export function IconOrSpinner({ icon: Icon, busy, label }: { icon: IconType; busy: boolean; label?: string }) {
  const shown = useDelayedFlag(busy)
  return shown ? <Spinner immediate className="size-4" label={label} /> : <Icon />
}
