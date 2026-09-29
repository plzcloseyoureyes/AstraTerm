/*
 * Transfer queue (FILE-8): one list for
 *   - server transfers (POST /api/transfers: copies between file systems, server-side), fed by GET /api/transfers and
 *     `{type:'transfer'}` events, and
 *   - local jobs run by this page: uploads from the browser (chunked PUT, see ../upload.ts) and downloads handed to the
 *     browser.
 * Views (drawer, status bar item, sidebar badge) read `TransferView`s; progress is published at most a few times per
 * second.
 */
import { toast } from 'sonner'
import { create } from 'zustand'
import { queryClient } from '@/api/queryClient'
import type { Transfer, TransferRequest } from '@/api/types'
import { events } from '@/lib/events'
import { errorMessage } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth'
import { transfersApi } from '../api'
import { findFsById } from '../fsHandles'
import { dirname, normalizePath } from '../paths'
import { filesSettings } from '../settings'

export type TransferState = 'queued' | 'running' | 'done' | 'error' | 'canceled'
export type TransferKind = 'upload' | 'download' | 'copy' | 'move'

export interface TransferView {
  id: string
  origin: 'server' | 'local'
  kind: TransferKind
  label: string
  /** "to /tmp on test@ssh1" */
  detail: string
  state: TransferState
  error?: string
  totalBytes: number
  doneBytes: number
  totalFiles: number
  doneFiles: number
  failedFiles: number
  currentFile?: string
  bytesPerSec: number
  createdAt: number
  finishedAt?: number
  canCancel: boolean
  canRetry: boolean
  /** Per-file problems (uploads): shown in the drawer's details. */
  problems?: { name: string; error: string }[]
  /** Why a running transfer does not progress right now ("Waiting for the connection…"). */
  note?: string
}

interface TransfersStore {
  server: Record<string, Transfer>
  local: Record<string, TransferView>
  drawerOpen: boolean
  serverLoaded: boolean
}

export const useTransfersStore = create<TransfersStore>(() => ({ server: {}, local: {}, drawerOpen: false, serverLoaded: false }))

// ---------------------------------------------------------------------------------------------------------------------
// drawer
// ---------------------------------------------------------------------------------------------------------------------

export function openTransfers(): void {
  ensureServerTransfers()
  useTransfersStore.setState({ drawerOpen: true })
}

export function closeTransfers(): void {
  useTransfersStore.setState({ drawerOpen: false })
}

export function toggleTransfers(): void {
  if (useTransfersStore.getState().drawerOpen) closeTransfers()
  else openTransfers()
}

/** The queue is on screen: its rows already show progress and results, so no toasts (they would cover it). */
export function transfersShown(): boolean {
  return useTransfersStore.getState().drawerOpen
}

// ---------------------------------------------------------------------------------------------------------------------
// local jobs (published by the upload engine)
// ---------------------------------------------------------------------------------------------------------------------

export interface LocalJobHandlers {
  cancel?: () => void
  retry?: () => void
  remove?: () => void
}

const localHandlers = new Map<string, LocalJobHandlers>()

/** Publish (insert / replace) a local job's view. */
export function publishLocal(view: TransferView, handlers?: LocalJobHandlers): void {
  if (handlers) localHandlers.set(view.id, handlers)
  useTransfersStore.setState((s) => ({ local: { ...s.local, [view.id]: view } }))
}

export function removeLocal(id: string): void {
  localHandlers.delete(id)
  useTransfersStore.setState((s) => {
    if (!s.local[id]) return s
    const local = { ...s.local }
    delete local[id]
    return { local }
  })
}

// ---------------------------------------------------------------------------------------------------------------------
// server transfers
// ---------------------------------------------------------------------------------------------------------------------

interface ServerMeta {
  move?: { srcKey: string; srcPaths: string[] }
  label?: string
  /** Destination place label as the UI names it ("ssh1 lab", "Local files"). */
  dstLabel?: string
  /** Created by this page: toast on completion. */
  mine?: boolean
}

const serverMeta = new Map<string, ServerMeta>()
const toasted = new Set<string>()
let loading: Promise<void> | null = null

/** Load GET /api/transfers once (and after the events socket reconnects). */
export function ensureServerTransfers(force = false): Promise<void> {
  if (loading && !force) return loading
  if (useTransfersStore.getState().serverLoaded && !force) return Promise.resolve()
  loading = transfersApi
    .list()
    .then((list) => {
      const server: Record<string, Transfer> = {}
      for (const t of Array.isArray(list) ? list : []) {
        if (t && typeof t.id === 'string') server[t.id] = t
        // Already finished before this page saw it: no toast.
        if (t && t.state !== 'queued' && t.state !== 'running') toasted.add(t.id)
      }
      useTransfersStore.setState({ server, serverLoaded: true })
    })
    .catch(() => {
      // 404 (older server) or offline: the queue simply starts empty.
      useTransfersStore.setState({ serverLoaded: true })
    })
    .finally(() => {
      loading = null
    })
  return loading
}

function applyServerTransfer(t: Transfer): void {
  const prev = useTransfersStore.getState().server[t.id]
  useTransfersStore.setState((s) => ({ server: { ...s.server, [t.id]: t } }))
  const finished = t.state === 'done' || t.state === 'error' || t.state === 'canceled'
  if (!finished) return
  const wasRunning = !prev || prev.state === 'queued' || prev.state === 'running'
  if (!wasRunning || toasted.has(t.id)) return
  toasted.add(t.id)
  void onServerFinished(t)
}

/** Refresh every listing of `dir` (whatever handle id shows it: a view's handle may have been reopened since). */
function invalidateListings(dir: string): void {
  const path = normalizePath(dir)
  void queryClient.invalidateQueries({ predicate: (q) => q.queryKey[0] === 'fs' && q.queryKey[2] === 'list' && q.queryKey[3] === path })
}

async function onServerFinished(t: Transfer): Promise<void> {
  const meta = serverMeta.get(t.id)
  // New entries at the destination (and, for moves, gone at the source: the server deletes them after the copy).
  if (t.dstDir) invalidateListings(t.dstDir)
  if (meta?.move || (t as TransferEx).move) {
    const srcDirs = new Set((t.srcPaths ?? []).map((p) => dirname(p)))
    for (const d of srcDirs) invalidateListings(d)
  }
  if (!meta?.mine || transfersShown()) return
  const view = serverView(t)
  if (t.state === 'done') {
    toast.success(`${view.kind === 'move' ? 'Moved' : 'Copied'} ${view.label}`, { description: view.detail })
  } else if (t.state === 'error') {
    toast.error(`${view.kind === 'move' ? 'Move' : 'Copy'} failed: ${view.label}`, {
      description: t.error,
      duration: 10_000,
      action: { label: 'Details', onClick: openTransfers },
    })
  }
}

/** Start a server-side transfer (copy, or move: the server deletes the sources after copying). */
export async function startServerTransfer(
  req: TransferRequest,
  opts: { move?: { srcKey: string }; label?: string; dstLabel?: string; announce?: boolean } = {},
): Promise<Transfer> {
  ensureServerTransfers()
  const t = await transfersApi.create({ ...req, move: opts.move ? true : undefined, preserve: true })
  serverMeta.set(t.id, {
    mine: true,
    label: opts.label,
    dstLabel: opts.dstLabel,
    move: opts.move ? { srcKey: opts.move.srcKey, srcPaths: req.srcPaths } : undefined,
  })
  applyServerTransfer(t)
  if (filesSettings.get().openQueueOnTransfer) openTransfers()
  // Only announce transfers that take a while (fast ones just report completion).
  if (opts.announce !== false) {
    setTimeout(() => {
      const cur = useTransfersStore.getState().server[t.id]
      if (!cur || (cur.state !== 'queued' && cur.state !== 'running') || transfersShown()) return
      const v = serverView(cur)
      toast.info(`${v.kind === 'move' ? 'Moving' : 'Copying'} ${v.label}`, {
        description: `${v.detail} — progress in Transfers`,
        action: { label: 'Show', onClick: openTransfers },
      })
    }, 1200)
  }
  return t
}

export async function cancelTransfer(v: TransferView): Promise<void> {
  if (v.origin === 'local') {
    localHandlers.get(v.id)?.cancel?.()
    return
  }
  try {
    await transfersApi.cancel(v.id)
  } catch (err) {
    toast.error('Could not cancel the transfer', { description: errorMessage(err) })
  }
}

export async function retryTransfer(v: TransferView): Promise<void> {
  if (v.origin === 'local') {
    localHandlers.get(v.id)?.retry?.()
    return
  }
  const t = useTransfersStore.getState().server[v.id]
  if (!t) return
  const meta = serverMeta.get(v.id)
  // Same conflict policy as the first run (a "skip existing" copy must not overwrite on retry).
  const prev = (t as TransferEx).overwrite
  const overwrite: TransferRequest['overwrite'] = prev === 'ask' || prev === 'skip' || prev === 'resume' || prev === 'rename' ? prev : 'overwrite'
  try {
    await startServerTransfer(
      { srcFs: t.srcFs, srcPaths: t.srcPaths, dstFs: t.dstFs, dstDir: t.dstDir, overwrite },
      { move: meta?.move || (t as TransferEx).move ? { srcKey: meta?.move?.srcKey ?? '' } : undefined, label: meta?.label, dstLabel: meta?.dstLabel },
    )
    await removeTransfer(v, true)
  } catch (err) {
    toast.error('Could not retry the transfer', { description: errorMessage(err) })
  }
}

export async function removeTransfer(v: TransferView, quiet = false): Promise<void> {
  if (v.origin === 'local') {
    const h = localHandlers.get(v.id)
    if (h?.remove) h.remove()
    else removeLocal(v.id)
    return
  }
  useTransfersStore.setState((s) => {
    const server = { ...s.server }
    delete server[v.id]
    return { server }
  })
  serverMeta.delete(v.id)
  try {
    await transfersApi.remove(v.id)
  } catch (err) {
    if (!quiet) console.warn('[files] could not clear transfer', err)
  }
}

export async function clearFinished(): Promise<void> {
  for (const v of transferViews()) {
    if (v.state === 'done' || v.state === 'canceled' || v.state === 'error') await removeTransfer(v, true)
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// views
// ---------------------------------------------------------------------------------------------------------------------

function placeLabel(fsId: string): string {
  const f = findFsById(fsId)
  return f?.handle.label ?? 'another file system'
}

/** Transfer JSON extras (SPEC §9 files-backend). */
type TransferEx = Transfer & { move?: boolean; overwrite?: string; skippedFiles?: number }

function serverView(t: Transfer): TransferView {
  const meta = serverMeta.get(t.id)
  const kind: TransferKind = meta?.move || (t as TransferEx).move ? 'move' : 'copy'
  const n = t.srcPaths?.length ?? 0
  const label = meta?.label || t.label || (n === 1 ? (t.srcPaths[0].split('/').pop() ?? t.srcPaths[0]) : `${n} items`)
  return {
    id: t.id,
    origin: 'server',
    kind,
    label,
    detail: `to ${t.dstDir} on ${meta?.dstLabel ?? placeLabel(t.dstFs)}`,
    state: t.state,
    error: t.error,
    totalBytes: t.totalBytes || 0,
    doneBytes: t.doneBytes || 0,
    totalFiles: t.totalFiles || 0,
    doneFiles: t.doneFiles || 0,
    failedFiles: 0,
    currentFile: t.currentFile,
    bytesPerSec: t.bytesPerSec || 0,
    createdAt: Date.parse(t.createdAt) || Date.now(),
    finishedAt: t.finishedAt ? Date.parse(t.finishedAt) : undefined,
    canCancel: t.state === 'queued' || t.state === 'running',
    canRetry: t.state === 'error' || t.state === 'canceled',
  }
}

/** Highest progress shown per active transfer: bars never move backwards (a retried chunk, a resume). */
const peak = new Map<string, number>()

function monotonic(v: TransferView): TransferView {
  if (v.state !== 'running' && v.state !== 'queued') {
    peak.delete(v.id)
    return v
  }
  const best = Math.max(peak.get(v.id) ?? 0, Math.min(v.doneBytes, v.totalBytes || v.doneBytes))
  peak.set(v.id, best)
  return best === v.doneBytes ? v : { ...v, doneBytes: best }
}

/** Every transfer, newest first. */
export function transferViews(s: Pick<TransfersStore, 'server' | 'local'> = useTransfersStore.getState()): TransferView[] {
  const out: TransferView[] = [...Object.values(s.local), ...Object.values(s.server).map(serverView)].map(monotonic)
  out.sort((a, b) => b.createdAt - a.createdAt)
  return out
}

let cachedInput: Pick<TransfersStore, 'server' | 'local'> | null = null
let cachedViews: TransferView[] = []

/** React: all transfer views (stable identity while nothing changed). */
export function useTransferViews(): TransferView[] {
  return useTransfersStore((s) => {
    if (cachedInput && cachedInput.server === s.server && cachedInput.local === s.local) return cachedViews
    cachedInput = { server: s.server, local: s.local }
    cachedViews = transferViews(s)
    return cachedViews
  })
}

export interface TransferSummary {
  active: number
  failed: number
  finished: number
  totalBytes: number
  doneBytes: number
  bytesPerSec: number
  /** 0..1, or null when nothing is active. */
  progress: number | null
  uploading: number
}

export function summarize(views: TransferView[]): TransferSummary {
  let active = 0
  let failed = 0
  let finished = 0
  let totalBytes = 0
  let doneBytes = 0
  let bytesPerSec = 0
  let uploading = 0
  for (const v of views) {
    if (v.state === 'queued' || v.state === 'running') {
      active++
      totalBytes += v.totalBytes
      doneBytes += Math.min(v.doneBytes, v.totalBytes || v.doneBytes)
      bytesPerSec += v.bytesPerSec
      if (v.kind === 'upload') uploading++
    } else if (v.state === 'error') failed++
    else finished++
  }
  return { active, failed, finished, totalBytes, doneBytes, bytesPerSec, progress: active ? (totalBytes > 0 ? doneBytes / totalBytes : 0) : null, uploading }
}

// ---------------------------------------------------------------------------------------------------------------------
// lifecycle
// ---------------------------------------------------------------------------------------------------------------------

let installed = false

export function installTransfersLifecycle(): void {
  if (installed) return
  installed = true
  events.on('transfer', (ev) => {
    if (ev.transfer && typeof ev.transfer.id === 'string') applyServerTransfer(ev.transfer)
  })
  // After a reconnect of the events socket, transfers may have progressed without us.
  events.on('hello', () => {
    if (useTransfersStore.getState().serverLoaded) void ensureServerTransfers(true)
  })
  useAuthStore.subscribe((s, prev) => {
    if (prev.status === 'authenticated' && s.status !== 'authenticated') {
      serverMeta.clear()
      toasted.clear()
      peak.clear()
      for (const h of localHandlers.values()) h.cancel?.()
      localHandlers.clear()
      useTransfersStore.setState({ server: {}, local: {}, drawerOpen: false, serverLoaded: false })
    }
  })
}
