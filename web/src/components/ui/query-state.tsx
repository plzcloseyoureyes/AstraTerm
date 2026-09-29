/*
 * Loading / empty / error states for content (docs/UX.md "Loading states"). The rules they encode:
 *   - a skeleton (or pane spinner) only when there is NO content yet, only after 300 ms, then for at least 600 ms;
 *   - never on a refetch or background refresh: once data is there it stays (react-query keeps the previous data
 *     while a key changes, see api/queryClient.ts) — show at most `<Spinner active={q.isFetching} reserve />` inline;
 *   - errors look the same everywhere and offer a retry; an error during a refetch keeps the old data.
 *
 *   <QueryState query={q} skeleton={<SkeletonRows />} empty={<EmptyState … />} isEmpty={(d) => !d.length}>
 *     {(items) => <List items={items} />}
 *   </QueryState>
 *
 *   <LoadingState busy={!loaded} skeleton={<SkeletonText />}>{content}</LoadingState>   // non-query loads
 */
import * as React from 'react'
import { CircleAlert, RotateCw } from 'lucide-react'
import { useDelayedFlag, type DelayedFlagOptions } from '@/lib/useDelayedFlag'
import { errorMessage } from '@/lib/utils'
import { Button } from './button'
import { EmptyState } from './empty-state'
import { DelayedRegionProvider, LoadingPane } from './spinner'

export interface LoadingStateProps {
  /** There is no content to show yet (a first load). */
  busy: boolean
  /** Placeholder shown after the delay (default: a centered spinner). */
  skeleton?: React.ReactNode
  /** Label of the default spinner. */
  label?: string
  /** Class of the default spinner pane, and of a blank box held while busy before the placeholder appears (without
   *  a class nothing is rendered then — right for inline spots). */
  className?: string
  /** Timing of the placeholder: a DELAY_PRESETS entry (default EXPLICIT_WAIT; NAVIGATION for view switches). */
  timing?: DelayedFlagOptions
  children?: React.ReactNode
}

/** First-load gate: blank for 300 ms, then the skeleton for at least 600 ms, then `children`. */
export function LoadingState({ busy, skeleton, label, className, timing, children }: LoadingStateProps) {
  const shown = useDelayedFlag(busy, timing)
  if (shown) {
    return <DelayedRegionProvider>{skeleton ?? <LoadingPane immediate label={label} className={className} />}</DelayedRegionProvider>
  }
  if (busy) return className ? <div aria-busy className={className} /> : null
  return <>{children}</>
}

export interface ErrorStateProps {
  error: unknown
  title?: string
  onRetry?: () => void
  className?: string
  size?: 'sm' | 'md'
}

/** The error block: what happened (the server's message) and a retry. */
export function ErrorState({ error, title = 'Could not load this', onRetry, className, size = 'sm' }: ErrorStateProps) {
  return (
    <EmptyState
      size={size}
      className={className}
      icon={CircleAlert}
      title={title}
      description={errorMessage(error)}
      action={
        onRetry && (
          <Button size="sm" variant="secondary" onClick={onRetry}>
            <RotateCw className="size-3.5" /> Try again
          </Button>
        )
      }
    />
  )
}

/** The part of a react-query result QueryState reads (UseQueryResult and UseInfiniteQueryResult both fit). */
export interface QueryLike<T> {
  data: T | undefined
  error: unknown
  isPending: boolean
  fetchStatus: 'fetching' | 'paused' | 'idle'
  refetch: () => unknown
}

export interface QueryStateProps<T> {
  query: QueryLike<T>
  children: (data: T) => React.ReactNode
  /** First-load placeholder (default: a centered spinner). */
  skeleton?: React.ReactNode
  label?: string
  /** Shown when isEmpty(data). */
  empty?: React.ReactNode
  isEmpty?: (data: T) => boolean
  /** Custom error rendering (default: ErrorState with retry). Only used while there is no data. */
  error?: (error: unknown, retry: () => void) => React.ReactNode
  errorTitle?: string
  className?: string
  /** Timing of the first-load placeholder (see LoadingState). */
  timing?: DelayedFlagOptions
}

export function QueryState<T>({ query, children, skeleton, label, empty, isEmpty, error, errorTitle, className, timing }: QueryStateProps<T>) {
  const { data } = query
  const retry = () => void query.refetch()
  const firstLoad = data === undefined && query.isPending && query.fetchStatus !== 'idle'
  let content: React.ReactNode = null
  if (data !== undefined) content = empty !== undefined && isEmpty?.(data) ? empty : children(data)
  else if (query.error) content = error ? error(query.error, retry) : <ErrorState error={query.error} title={errorTitle} onRetry={retry} className={className} />
  return (
    <LoadingState busy={firstLoad} skeleton={skeleton} label={label} className={className} timing={timing}>
      {content}
    </LoadingState>
  )
}
