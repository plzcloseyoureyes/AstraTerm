/*
 * Change detection of an open remote file (FILE-10): while the tab is visible and the window focused, stat the file
 * every 5 s (and when the window regains focus) and move the banner accordingly (tabstate.ts → watchBanner).
 */
import { useEffect, type Dispatch, type SetStateAction } from 'react'
import type { FileEntry } from '@/api/types'
import { useLatest } from '@/lib/hooks'
import { isHandleGone, isNotFound } from './api'
import type { FileStat } from './filestat'
import { watchBanner, type BannerState } from './tabstate'

const WATCH_INTERVAL_MS = 5000

export interface RemoteWatchOptions {
  /** Watch at all (settings.editor.watchRemote, a document is open). */
  enabled: boolean
  /** Restarts the watch when it changes (the file: handle id + path). */
  target: string
  /** May a poll run right now? (tab visible, no save running, no compare view…) */
  canPoll: () => boolean
  stat: () => Promise<FileEntry>
  /** What the editor loaded or saved last. */
  base: () => FileStat
  /** The server version the user chose to ignore ("Ignore" on the banner). */
  ignoredMtime: () => string | null
  setBanner: Dispatch<SetStateAction<BannerState | null>>
}

export function useRemoteWatch(opts: RemoteWatchOptions): void {
  const latest = useLatest(opts)
  const { enabled, target } = opts
  useEffect(() => {
    if (!enabled) return
    let stopped = false
    let busy = false
    const tick = async () => {
      const o = latest.current
      if (stopped || busy || document.visibilityState !== 'visible' || !document.hasFocus() || !o.canPoll()) return
      busy = true
      try {
        const stat = await o.stat()
        if (stopped) return
        o.setBanner((b) => watchBanner(b, { kind: 'stat', stat, base: o.base(), ignoredMtime: o.ignoredMtime() }))
      } catch (err) {
        if (!stopped && isNotFound(err)) o.setBanner((b) => watchBanner(b, { kind: 'missing', handleGone: isHandleGone(err) }))
      } finally {
        busy = false
      }
    }
    const id = window.setInterval(() => void tick(), WATCH_INTERVAL_MS)
    const onFocus = () => void tick()
    window.addEventListener('focus', onFocus)
    return () => {
      stopped = true
      window.clearInterval(id)
      window.removeEventListener('focus', onFocus)
    }
  }, [enabled, target, latest])
}
