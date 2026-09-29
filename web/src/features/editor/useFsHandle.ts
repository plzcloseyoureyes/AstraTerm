/*
 * The file system handle of an "editor" tab: its details (host label, origin, sudo capability), and transparent
 * re-opening when it expired (10 min idle, server restart — SPEC §9 editor). `call` runs an API call against the
 * current handle and retries it once on a fresh handle; the tab's params follow the new handle id.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useLatest } from '@/lib/hooks'
import { updateTabParams } from '@/stores/workspace'
import { getFsInfo, infoLabel, isHandleGone, openFs, rememberHandle, sourceOf, type FsHandleInfo } from './api'
import type { EditorTabParams } from './types'

export interface FsHandle {
  /** Handle details (null until known, or when the handle is gone). */
  info: FsHandleInfo | null
  /** Resolves the details request of the current handle (backup keys wait for it). */
  infoReady: () => Promise<FsHandleInfo | null>
  /** The current handle id (changes when an expired handle is re-opened). */
  id: () => string
  call: <T>(fn: (fsId: string) => Promise<T>) => Promise<T>
  /**
   * Re-open the handle from its origin (session / connection / server files) and switch the tab to it. `keepBuffer`:
   * the tab keeps its document (no reload for the new id). Resolves the new id, null when it cannot be re-opened.
   */
  reconnect: (keepBuffer?: boolean) => Promise<string | null>
  /** True once for the fsId a reconnect switched the tab to (the tab must not reload the file for it). */
  takeReconnected: (fsId: string) => boolean
}

export function useFsHandle(tabId: string, params: EditorTabParams): FsHandle {
  const { fsId } = params
  const [info, setInfo] = useState<FsHandleInfo | null>(null)
  const idRef = useRef(fsId)
  idRef.current = fsId
  const latestParams = useLatest(params)
  const reconnectedTo = useRef<string | null>(null)
  const reconnecting = useRef<Promise<string | null> | null>(null)
  const infoPromise = useRef<Promise<FsHandleInfo | null> | null>(null)

  // Handle details: host label for the status bar, how to reconnect, sudo capability.
  useEffect(() => {
    const ctl = new AbortController()
    infoPromise.current = getFsInfo(fsId, ctl.signal)
      .then((h) => {
        rememberHandle(h)
        setInfo(h)
        const patch: Partial<EditorTabParams> = {}
        const src = sourceOf(h)
        const cur = latestParams.current
        if (src && JSON.stringify(src) !== JSON.stringify(cur.source)) patch.source = src
        const label = infoLabel(h)
        if (label && !cur.label) patch.label = label
        if (Object.keys(patch).length) updateTabParams<EditorTabParams>(tabId, patch)
        return h
      })
      .catch(() => null)
    return () => ctl.abort()
  }, [fsId, latestParams, tabId])

  const reconnect = useCallback(
    (keepBuffer = true): Promise<string | null> => {
      if (reconnecting.current) return reconnecting.current
      const src = latestParams.current.source
      if (!src) return Promise.resolve(null)
      const p = openFs(src)
        .then((h) => {
          rememberHandle(h)
          idRef.current = h.id
          if (keepBuffer) reconnectedTo.current = h.id
          setInfo(h)
          updateTabParams<EditorTabParams>(tabId, { fsId: h.id, label: latestParams.current.label ?? infoLabel(h), ownsFs: true })
          return h.id
        })
        .catch(() => null)
        .finally(() => {
          reconnecting.current = null
        })
      reconnecting.current = p
      return p
    },
    [latestParams, tabId],
  )

  const call = useCallback(
    async <T>(fn: (id: string) => Promise<T>): Promise<T> => {
      try {
        return await fn(idRef.current)
      } catch (err) {
        if (!isHandleGone(err)) throw err
        const id = await reconnect()
        if (!id) throw err
        return fn(id)
      }
    },
    [reconnect],
  )

  const infoReady = useCallback(() => infoPromise.current ?? Promise.resolve(null), [])
  const id = useCallback(() => idRef.current, [])
  const takeReconnected = useCallback((next: string) => {
    if (reconnectedTo.current !== next) return false
    reconnectedTo.current = null
    return true
  }, [])

  return useMemo(() => ({ info, infoReady, id, call, reconnect, takeReconnected }), [info, infoReady, id, call, reconnect, takeReconnected])
}
