/*
 * Loading indicators that are calm by default (docs/UX.md "Loading states"): nothing appears for operations under
 * 300 ms, and whatever appears stays at least 600 ms. Built on lib/useDelayedFlag.
 *
 *   <Spinner active={saving} />                  inline; renders nothing until saving has lasted 300 ms
 *   <Spinner active={q.isFetching} reserve />    same, but keeps its box while hidden (no layout shift)
 *   <Delayed active={busy}>…</Delayed>           any indicator (spinner + text, overlay card) under the same rule
 *   <LoadingPane active={busy} label="…" />      centered pane spinner (lazy tabs, first loads)
 *   <BusyIcon icon={RefreshCw} busy={refreshing} />  an icon that spins only while a slow operation runs
 *   <LazyBoundary label="…">{lazy content}</LazyBoundary>   Suspense for lazy chunks under the same rule
 *
 * Inside <Delayed>, <LoadingState> and <QueryState> the delay has already been applied, so nested Spinners show at once.
 */
import * as React from 'react'
import { Suspense, useLayoutEffect, useState } from 'react'
import { LoaderCircle } from 'lucide-react'
import type { IconType } from '@/app/registry'
import { useDelayedFlag, type DelayedFlagOptions } from '@/lib/useDelayedFlag'
import { cn } from '@/lib/utils'

/** True inside a region whose visibility was already delayed (Delayed, LoadingState, QueryState). */
const DelayedRegion = React.createContext(false)

/** Marks children as already delayed: nested Spinners render at once. */
export function DelayedRegionProvider({ children }: { children: React.ReactNode }) {
  return <DelayedRegion.Provider value={true}>{children}</DelayedRegion.Provider>
}

export interface SpinnerProps extends DelayedFlagOptions {
  className?: string
  /** Accessible name (role="status"). */
  label?: string
  /** Whether the operation is running (default true: "while mounted"). Keep the Spinner mounted and toggle this so
   *  the minimum visible time can apply. */
  active?: boolean
  /** Show at once, without the 300 ms delay (the caller already delayed it, or the state is known to be long). */
  immediate?: boolean
  /** Keep an invisible box of the same size while hidden, so nothing moves when it appears. */
  reserve?: boolean
}

export function Spinner({ className, label = 'Loading', active = true, immediate, reserve, delay, minVisible }: SpinnerProps) {
  const inRegion = React.useContext(DelayedRegion)
  const direct = immediate || inRegion
  const delayed = useDelayedFlag(active && !direct, { delay, minVisible })
  const shown = direct ? active : delayed
  if (!shown) return reserve ? <span aria-hidden className={cn('inline-block size-4 shrink-0', className)} /> : null
  return <LoaderCircle role="status" aria-label={label} className={cn('size-4 shrink-0 animate-spin text-muted-foreground', className)} />
}

export interface DelayedProps extends DelayedFlagOptions {
  /** The operation is running. */
  active?: boolean
  children: React.ReactNode
  /** Rendered while the indicator is hidden (default: nothing). */
  fallback?: React.ReactNode
}

/** Renders `children` only per useDelayedFlag(active): after 300 ms, for at least 600 ms. */
export function Delayed({ active = true, children, fallback = null, delay, minVisible }: DelayedProps) {
  const shown = useDelayedFlag(active, { delay, minVisible })
  if (!shown) return <>{fallback}</>
  return <DelayedRegionProvider>{children}</DelayedRegionProvider>
}

/** Centered spinner filling its container (lazy-loaded tabs, first loads). The box is always there; its content
 *  appears only after the delay (`timing`: a DELAY_PRESETS entry, default EXPLICIT_WAIT). */
export function LoadingPane({
  label,
  className,
  active = true,
  immediate,
  timing,
}: {
  label?: string
  className?: string
  active?: boolean
  immediate?: boolean
  timing?: DelayedFlagOptions
}) {
  const inRegion = React.useContext(DelayedRegion)
  const delayed = useDelayedFlag(active && !immediate && !inRegion, timing)
  const shown = immediate || inRegion ? active : delayed
  return (
    <div aria-busy={active || undefined} className={cn('flex h-full min-h-24 w-full items-center justify-center gap-2 text-sm text-muted-foreground', className)}>
      {shown && (
        <>
          <Spinner immediate label={label} />
          {label && <span>{label}</span>}
        </>
      )}
    </div>
  )
}

/** An icon (e.g. RefreshCw) that spins only while `busy` has lasted 300 ms, and then for at least 600 ms (`timing`: a
 *  DELAY_PRESETS entry). Pass user-initiated work only (see useManualRefresh): background polling must not make icons
 *  spin. */
export function BusyIcon({ icon: Icon, busy, className, timing }: { icon: IconType; busy: boolean; className?: string; timing?: DelayedFlagOptions }) {
  const spin = useDelayedFlag(busy, timing)
  return <Icon aria-hidden className={cn(className, spin && 'animate-spin')} />
}

/**
 * Suspense for lazy chunks that follows the loading rules: nothing for 300 ms, then a pane spinner that stays at
 * least 600 ms — if the chunk arrives in the meantime the spinner stays over it for the rest of that time instead of
 * vanishing after a few ms. `timing` takes a DELAY_PRESETS entry (NAVIGATION for view switches). Needs a positioned
 * parent (the overlay is `absolute inset-0`).
 */
export function LazyBoundary({
  children,
  label,
  className = 'bg-panel',
  timing,
}: {
  children: React.ReactNode
  label?: string
  className?: string
  timing?: DelayedFlagOptions
}) {
  const [pending, setPending] = useState(false)
  const shown = useDelayedFlag(pending, timing)
  return (
    <>
      <Suspense fallback={<SuspendSignal onChange={setPending} />}>{children}</Suspense>
      {shown && (
        <div className={cn('absolute inset-0 z-10', className)}>
          <LoadingPane immediate label={label} />
        </div>
      )}
    </>
  )
}

/** Suspense fallback reporting "suspended" while mounted. */
function SuspendSignal({ onChange }: { onChange: (pending: boolean) => void }) {
  useLayoutEffect(() => {
    onChange(true)
    return () => onChange(false)
  }, [onChange])
  return null
}
