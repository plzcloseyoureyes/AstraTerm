import { useCallback, useEffect, useRef } from 'react'
import { useDelayedFlag } from '@/lib/useDelayedFlag'

/**
 * For screens that go away when an operation succeeds (sign-in, first-run setup): `settled()` resolves once the
 * operation's indicator — shown only when it took > 300 ms — has had its minimum visible time (docs/UX.md "Motion &
 * feedback"), so the next screen never replaces a spinner a few ms after it appeared. Call it after clearing `busy`.
 */
export function useIndicatorSettled(busy: boolean): () => Promise<void> {
  const shown = useDelayedFlag(busy)
  const shownRef = useRef(shown)
  shownRef.current = shown
  const waiter = useRef<(() => void) | null>(null)
  useEffect(() => {
    if (shown) return
    waiter.current?.()
    waiter.current = null
  }, [shown])
  return useCallback(() => (shownRef.current ? new Promise<void>((resolve) => (waiter.current = resolve)) : Promise.resolve()), [])
}
