/*
 * The tab's own progress bar (in the tab header) for background work while the document stays on screen — reloading
 * it, reading the server version to compare. It follows the loading rule (docs/UX.md): shown only once the work has
 * run 300 ms, then for at least 400 ms, so quick requests never blink a bar in the tab strip.
 *
 *   const track = useTabProgress(tabId)
 *   const res = await track(readFile(…))
 */
import { useCallback, useEffect, useState } from 'react'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { setTabState } from '@/stores/workspace'

export function useTabProgress(tabId: string): <T>(work: Promise<T>) => Promise<T> {
  const [running, setRunning] = useState(0)
  const shown = useDelayedFlag(running > 0)
  useEffect(() => {
    setTabState(tabId, { progress: shown ? 'indeterminate' : null })
  }, [tabId, shown])
  useEffect(() => () => setTabState(tabId, { progress: null }), [tabId])
  return useCallback(<T>(work: Promise<T>): Promise<T> => {
    setRunning((n) => n + 1)
    return work.finally(() => setRunning((n) => n - 1))
  }, [])
}
