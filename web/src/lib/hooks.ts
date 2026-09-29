import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from 'react'

/** Reactive CSS media query. */
export function useMediaQuery(query: string): boolean {
  const subscribe = useCallback(
    (cb: () => void) => {
      const mql = window.matchMedia(query)
      mql.addEventListener('change', cb)
      return () => mql.removeEventListener('change', cb)
    },
    [query],
  )
  return useSyncExternalStore(
    subscribe,
    () => window.matchMedia(query).matches,
    () => false,
  )
}

/** < 768px: phone-sized layout (drawer sidebar, compact ribbon). */
export function useIsMobile(): boolean {
  return useMediaQuery('(max-width: 767px)')
}

/** Current time, re-rendering every `intervalMs` (for relative timestamps / durations). */
export function useNow(intervalMs = 30_000): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), intervalMs)
    return () => clearInterval(t)
  }, [intervalMs])
  return now
}

/** Keep a ref pointing at the latest value (stable callbacks reading fresh props). */
export function useLatest<T>(value: T) {
  const ref = useRef(value)
  useEffect(() => {
    ref.current = value
  })
  return ref
}

/** Is the browser fullscreen? */
export function useFullscreen(): boolean {
  return useSyncExternalStore(
    (cb) => {
      document.addEventListener('fullscreenchange', cb)
      return () => document.removeEventListener('fullscreenchange', cb)
    },
    () => !!document.fullscreenElement,
    () => false,
  )
}

export async function toggleFullscreen(): Promise<void> {
  try {
    if (document.fullscreenElement) await document.exitFullscreen()
    else await document.documentElement.requestFullscreen({ navigationUI: 'hide' })
  } catch {
    /* denied (e.g. not triggered by a user gesture) */
  }
}

/**
 * Tracks a user-initiated refresh (a Refresh button), as opposed to background polling: `refreshing` is true from the
 * click until `refetch` settles. Feed it to `<IconButton busy>` / `<BusyIcon busy>` so icons never spin for polls.
 */
export function useManualRefresh(refetch: () => Promise<unknown> | unknown): { refreshing: boolean; refresh: () => void } {
  const [pending, setPending] = useState(0)
  const latest = useLatest(refetch)
  const refresh = useCallback(() => {
    setPending((n) => n + 1)
    void Promise.resolve()
      .then(() => latest.current())
      .catch(() => undefined)
      .finally(() => setPending((n) => n - 1))
  }, [latest])
  return { refreshing: pending > 0, refresh }
}
