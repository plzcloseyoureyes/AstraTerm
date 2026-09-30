/*
 * File system handles (POST /api/fs): opened once per source and shared by every view that browses it.
 *
 *   useFs(source)            hook: {status, handle, error, retry} — retains the handle while mounted
 *   openFs(source, {force})  open (deduplicated; `force` reopens, e.g. after the transport changed)
 *   withFs(key, fn)          run fn(fsId); a "handle gone" error reopens the handle and retries once
 *
 * Lifetimes: a session's handle lives as long as the session (the SFTP panel is cached per session — switching
 * terminal tabs back and forth must not reconnect); it is reopened when the session reconnects (new SSH transport)
 * and closed when the session closes. Other handles close a while after their last view unmounts (unless a tab
 * still refers to the handle id, e.g. an editor tab opened from it).
 */
import { useCallback, useEffect } from 'react'
import { create } from 'zustand'
import { isApiError } from '@/api/client'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { RuntimeSession } from '@/api/types'
import { events } from '@/lib/events'
import { useLatest } from '@/lib/hooks'
import { errorMessage } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth'
import { listTabs } from '@/stores/workspace'
import { fsApi, fsKeys, isHandleGone } from './api'
import type { FsHandleEx, FsOpenBody, FsSource } from './types'

type HandleStatus = 'idle' | 'opening' | 'ready' | 'error'

interface HandleError {
  message: string
  status: number
  code: string
}

interface Entry {
  key: string
  source: FsSource
  status: Exclude<HandleStatus, 'idle'>
  handle?: FsHandleEx
  error?: HandleError
  /** RuntimeSession.connectedAt when the handle was opened (session sources): a change means a new transport. */
  connectedAt?: string
}

interface FsStore {
  entries: Record<string, Entry>
}

export const useFsStore = create<FsStore>(() => ({ entries: {} }))

const inflight = new Map<string, Promise<FsHandleEx>>()
const generation = new Map<string, number>()
const users = new Map<string, number>()
const closeTimers = new Map<string, ReturnType<typeof setTimeout>>()
/** Idle handles (no view shows them) are closed after this delay. */
const CLOSE_GRACE_MS = 90_000

// ---------------------------------------------------------------------------------------------------------------------
// keys
// ---------------------------------------------------------------------------------------------------------------------

export function sourceKey(src: FsSource): string {
  switch (src.kind) {
    case 'session':
      return `session:${src.sessionId}`
    case 'connection':
      return `conn:${src.connectionId}${src.sudo ? ':sudo' : ''}`
    case 'local':
      return 'local'
    case 'handle':
      return `handle:${src.fsId}`
    case 'quick': {
      const q = src.quick
      return `quick:${q.protocol ?? 'sftp'}:${q.username ?? ''}@${(q.host ?? '').toLowerCase()}:${q.port ?? ''}:${q.identityId ?? ''}:${q.keyId ?? ''}`
    }
  }
}

function openBody(src: FsSource): FsOpenBody {
  switch (src.kind) {
    case 'session':
      return { sessionId: src.sessionId }
    case 'connection':
      return src.sudo ? { connectionId: src.connectionId, sudo: true } : { connectionId: src.connectionId }
    case 'local':
      return { local: true }
    case 'quick':
      return { quick: src.quick }
    case 'handle':
      return {}
  }
}

function toHandleError(err: unknown): HandleError {
  if (isApiError(err)) return { message: err.message, status: err.status, code: err.code }
  return { message: errorMessage(err), status: 0, code: 'error' }
}

function patchEntry(key: string, patch: Partial<Entry> | null): void {
  useFsStore.setState((s) => {
    const entries = { ...s.entries }
    if (patch === null) delete entries[key]
    else entries[key] = { ...(entries[key] as Entry), ...patch, key }
    return { entries }
  })
}

function sessionConnectedAt(sessionId: string): string | undefined {
  return queryClient.getQueryData<RuntimeSession[]>(queryKeys.sessions)?.find((s) => s.id === sessionId)?.connectedAt
}

/** Placeholder metadata for handles opened elsewhere (only the id is known). */
function syntheticHandle(fsId: string): FsHandleEx {
  return {
    id: fsId,
    kind: 'sftp',
    home: '/',
    root: '/',
    label: 'Files',
    capabilities: { chmod: true, chown: false, symlink: true, exec: false, checksum: true },
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// open / close
// ---------------------------------------------------------------------------------------------------------------------

/** Open (or return the open) handle of a source. `force` reopens (the previous handle is closed afterwards). */
export function openFs(src: FsSource, opts: { force?: boolean } = {}): Promise<FsHandleEx> {
  const key = sourceKey(src)
  if (src.kind === 'handle') {
    const known = useFsStore.getState().entries[key]?.handle ?? findFsById(src.fsId)?.handle
    const h = known ?? syntheticHandle(src.fsId)
    patchEntry(key, { source: src, status: 'ready', handle: h, error: undefined })
    // Fill in the real metadata (home, capabilities, label) when only the id was known.
    if (!known) {
      void fsApi
        .info(src.fsId)
        .then((info) => {
          if (info && typeof info.id === 'string') patchEntry(key, { handle: { ...h, ...info } })
        })
        .catch(() => undefined)
    }
    return Promise.resolve(h)
  }
  const cur = useFsStore.getState().entries[key]
  if (!opts.force) {
    if (cur?.status === 'ready' && cur.handle) return Promise.resolve(cur.handle)
    const pending = inflight.get(key)
    if (pending) return pending
  }
  const gen = (generation.get(key) ?? 0) + 1
  generation.set(key, gen)
  const previous = cur?.handle
  patchEntry(key, { source: src, status: 'opening', error: undefined })
  const connectedAt = src.kind === 'session' ? sessionConnectedAt(src.sessionId) : undefined
  const p = fsApi
    .open(openBody(src))
    .then((h) => {
      if (generation.get(key) !== gen) {
        // Superseded by a newer open: drop this handle.
        void fsApi.close(h.id).catch(() => undefined)
        return useFsStore.getState().entries[key]?.handle ?? h
      }
      patchEntry(key, { status: 'ready', handle: h, error: undefined, connectedAt })
      if (previous && previous.id !== h.id) {
        void fsApi.close(previous.id).catch(() => undefined)
        queryClient.removeQueries({ queryKey: fsKeys.fs(previous.id) })
      }
      return h
    })
    .catch((err) => {
      if (generation.get(key) === gen) patchEntry(key, { status: 'error', error: toHandleError(err), handle: undefined })
      throw err
    })
    .finally(() => {
      if (inflight.get(key) === p) inflight.delete(key)
    })
  inflight.set(key, p)
  return p
}

/** Close a handle now (server-side DELETE) and forget it. */
function closeFs(key: string): void {
  const t = closeTimers.get(key)
  if (t) clearTimeout(t)
  closeTimers.delete(key)
  generation.set(key, (generation.get(key) ?? 0) + 1)
  inflight.delete(key)
  const e = useFsStore.getState().entries[key]
  if (!e) return
  patchEntry(key, null)
  if (e.handle && e.source.kind !== 'handle') {
    const id = e.handle.id
    void fsApi.close(id).catch(() => undefined)
    queryClient.removeQueries({ queryKey: fsKeys.fs(id) })
  }
}

function tabUsesHandle(fsId: string): boolean {
  return listTabs().some((t) => {
    const p = t.params as { fsId?: unknown; right?: { fsId?: unknown } } | undefined
    return p?.fsId === fsId || p?.right?.fsId === fsId
  })
}

function scheduleClose(key: string): void {
  if (closeTimers.has(key)) return
  closeTimers.set(
    key,
    setTimeout(() => {
      closeTimers.delete(key)
      if ((users.get(key) ?? 0) > 0) return
      const e = useFsStore.getState().entries[key]
      // A tab (editor, another files tab) still works with this handle id: keep it a while longer.
      if (e?.handle && tabUsesHandle(e.handle.id)) {
        scheduleClose(key)
        return
      }
      closeFs(key)
    }, CLOSE_GRACE_MS),
  )
}

function retainFs(key: string): void {
  users.set(key, (users.get(key) ?? 0) + 1)
  const t = closeTimers.get(key)
  if (t) {
    clearTimeout(t)
    closeTimers.delete(key)
  }
}

function releaseFs(key: string): void {
  const n = Math.max(0, (users.get(key) ?? 0) - 1)
  users.set(key, n)
  if (n > 0) return
  const e = useFsStore.getState().entries[key]
  // Session handles live with their session (cached SSH-browser); everything else closes when idle.
  if (!e || e.source.kind === 'session' || e.source.kind === 'handle') return
  scheduleClose(key)
}

/** Current handle of a key (ready only). */
export function getFsHandle(key: string): FsHandleEx | undefined {
  const e = useFsStore.getState().entries[key]
  return e?.status === 'ready' ? e.handle : undefined
}

/** Find an open handle by its id. */
export function findFsById(fsId: string): { key: string; source: FsSource; handle: FsHandleEx } | undefined {
  for (const e of Object.values(useFsStore.getState().entries)) {
    if (e.handle?.id === fsId) return { key: e.key, source: e.source, handle: e.handle }
  }
  return undefined
}

/**
 * Run an operation against a key's handle. When the server no longer knows the handle (timed out, transport
 * replaced), reopen it and retry once.
 */
export async function withFs<T>(key: string, fn: (fsId: string) => Promise<T>): Promise<T> {
  const e = useFsStore.getState().entries[key]
  if (!e) throw new Error('The file system is not open')
  const handle = e.status === 'ready' && e.handle ? e.handle : await openFs(e.source)
  try {
    return await fn(handle.id)
  } catch (err) {
    if (!isHandleGone(err) || e.source.kind === 'handle') throw err
    const fresh = await openFs(e.source, { force: true })
    return fn(fresh.id)
  }
}

/** A request failed because the handle is gone: reopen it in the background (views re-render with the new id). */
export function reportFsError(key: string, err: unknown): void {
  if (!isHandleGone(err)) return
  const e = useFsStore.getState().entries[key]
  if (!e || e.source.kind === 'handle' || inflight.has(key)) return
  void openFs(e.source, { force: true }).catch(() => undefined)
}

// ---------------------------------------------------------------------------------------------------------------------
// React
// ---------------------------------------------------------------------------------------------------------------------

export interface UseFsResult {
  key: string | null
  status: HandleStatus
  handle?: FsHandleEx
  error?: HandleError
  retry: () => void
}

/** Open and hold a source's handle while mounted (`enabled` false: hold nothing, status idle). */
export function useFs(source: FsSource | null, enabled = true): UseFsResult {
  const key = source ? sourceKey(source) : null
  const entry = useFsStore((s) => (key ? s.entries[key] : undefined))
  const srcRef = useLatest(source)
  useEffect(() => {
    if (!key || !enabled || !srcRef.current) return
    retainFs(key)
    const e = useFsStore.getState().entries[key]
    if (!e || (e.status === 'ready' && !e.handle)) void openFs(srcRef.current).catch(() => undefined)
    return () => releaseFs(key)
  }, [key, enabled, srcRef])
  const retry = useCallback(() => {
    if (srcRef.current) void openFs(srcRef.current, { force: true }).catch(() => undefined)
  }, [srcRef])
  if (!key || !enabled) return { key, status: 'idle', retry }
  return { key, status: entry?.status ?? 'opening', handle: entry?.handle, error: entry?.error, retry }
}

// ---------------------------------------------------------------------------------------------------------------------
// lifecycle (installed once by the feature)
// ---------------------------------------------------------------------------------------------------------------------

let installed = false

export function installFsLifecycle(): void {
  if (installed) return
  installed = true
  events.on('session.closed', (ev) => {
    if (typeof ev.id === 'string') closeFs(`session:${ev.id}`)
  })
  events.on('session.updated', (ev) => {
    const s = ev.session
    if (!s || typeof s.id !== 'string') return
    const key = `session:${s.id}`
    const e = useFsStore.getState().entries[key]
    if (!e) return
    if (s.state === 'closed') {
      closeFs(key)
      return
    }
    if (s.state !== 'connected') return
    const inUse = (users.get(key) ?? 0) > 0
    const replaced = e.status === 'ready' && !!e.connectedAt && !!s.connectedAt && e.connectedAt !== s.connectedAt
    if (replaced || e.status === 'error') {
      // New SSH transport (reconnect): the old handle is bound to the previous one.
      if (inUse) void openFs(e.source, { force: true }).catch(() => undefined)
      else closeFs(key)
    } else if (e.status === 'ready' && !e.connectedAt && s.connectedAt) {
      patchEntry(key, { connectedAt: s.connectedAt })
    }
  })
  // Signing out: the server dropped every handle with the login session.
  useAuthStore.subscribe((s, prev) => {
    if (prev.status === 'authenticated' && s.status !== 'authenticated') {
      for (const t of closeTimers.values()) clearTimeout(t)
      closeTimers.clear()
      inflight.clear()
      users.clear()
      useFsStore.setState({ entries: {} })
      queryClient.removeQueries({ queryKey: fsKeys.all })
    }
  })
}
