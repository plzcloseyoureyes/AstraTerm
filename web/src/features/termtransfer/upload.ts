/*
 * Drop-to-upload through the Files API (FILE-22): POST /api/fs {sessionId} (the terminal's own SSH transport — no
 * second login) or {local:true} for local shells, then chunked PUT …/upload?path=&offset=&total=&mtime=[&final=1]
 * (8 MiB raw chunks; the server writes "<target>.astraterm-part" and renames it with the last chunk). A failed chunk
 * resumes from the size the server holds (409 offset_mismatch carries it; otherwise GET …/upload?path=). Cancelling
 * removes the partial file. Progress shows in a toast (UploadToast).
 */
import { api, apiUrl, ApiError, isApiError, seg } from '@/api/client'
import { errorMessage, plural, uid } from '@/lib/utils'
import { markUnauthenticated } from '@/stores/auth'
import { requestVaultUnlock } from '@/stores/ui'
import { joinRemotePath } from './engine/names'
import { PROGRESS_PUBLISH_MS } from './engine/progress'
import type { LocalSelection } from './browser'
import { askConflict, dropUpload, putUpload } from './store'
import { currentSettings, transferSettings, type ConflictPolicy } from './settings'
import type { UploadView } from './types'

const CHUNK_SIZE = 8 * 1024 * 1024
const MAX_ATTEMPTS = 6
const STALL_MS = 60_000

interface FsHandleInfo {
  id: string
  home: string
  userHome?: string
  root: string
  kind: string
}

interface FsEntry {
  name: string
  type: 'file' | 'dir' | 'symlink' | 'other'
  linkType?: 'file' | 'dir' | 'broken'
}

const base = (id: string) => `/api/fs/${seg(id)}`

function openFsFor(src: { sessionId: string } | { local: true }): Promise<FsHandleInfo> {
  return api.post<FsHandleInfo>('/api/fs', src)
}

function closeFs(id: string): void {
  api.del(base(id)).catch(() => undefined)
}

export interface UploadRequest {
  /** SSH session (its own transport) or the AstraTerm host (local shells). */
  source: { sessionId: string } | { local: true }
  /** Destination folder; empty = the file system's start folder. */
  dir?: string
  selection: LocalSelection
  /** Short name of the terminal for messages. */
  label: string
}

export interface UploadResult {
  handle?: FsHandleInfo
  dest: string
  /** Absolute paths of the uploaded top-level items. */
  paths: string[]
  canceled: boolean
}

interface Job {
  view: UploadView
  abort: AbortController
  xhr: XMLHttpRequest | null
  lastAt: number
  lastBytes: number
  /** Trailing publish of throttled byte progress. */
  publishTimer: ReturnType<typeof setTimeout> | null
  publishedAt: number
}

const jobs = new Map<string, Job>()

export function cancelUpload(id: string): void {
  const j = jobs.get(id)
  if (!j) return
  j.abort.abort()
  j.xhr?.abort()
}

function publish(j: Job, patch: Partial<UploadView> = {}): void {
  const now = Date.now()
  const view = { ...j.view, ...patch }
  // Monotonic while running (a retried or resumed chunk reports from its start again).
  if (view.phase === 'running' && j.view.phase === 'running' && view.total === j.view.total) view.bytes = Math.max(j.view.bytes, view.bytes)
  const dt = now - j.lastAt
  if (dt >= 500) {
    const inst = ((view.bytes - j.lastBytes) * 1000) / dt
    view.rate = j.view.rate ? j.view.rate * 0.6 + inst * 0.4 : inst
    j.lastAt = now
    j.lastBytes = view.bytes
  }
  j.view = view
  // Byte progress reaches the toast at most every PROGRESS_PUBLISH_MS; phase / file changes go out at once.
  const onlyBytes = Object.keys(patch).every((k) => k === 'bytes')
  if (onlyBytes && now - j.publishedAt < PROGRESS_PUBLISH_MS) {
    j.publishTimer ??= setTimeout(() => {
      j.publishTimer = null
      j.publishedAt = Date.now()
      putUpload(j.view)
    }, PROGRESS_PUBLISH_MS - (now - j.publishedAt))
    return
  }
  if (j.publishTimer) clearTimeout(j.publishTimer)
  j.publishTimer = null
  j.publishedAt = now
  putUpload(view)
}

/**
 * Upload a local selection. Resolves the uploaded paths (empty when everything was skipped), throws on failure.
 * The toast is driven by the store entry `id` (returned through onStart so the caller can render it at once).
 */
export async function uploadSelection(req: UploadRequest, onStart: (id: string) => void): Promise<UploadResult> {
  const id = uid('ttup')
  const abort = new AbortController()
  const top = new Set<string>()
  for (const f of req.selection.files) top.add(f.relPath.split('/')[0])
  for (const d of req.selection.dirs) top.add(d.split('/')[0])
  const total = req.selection.files.reduce((n, f) => n + f.file.size, 0)
  const job: Job = {
    view: {
      id,
      label: top.size === 1 ? [...top][0] : plural(req.selection.files.length || req.selection.dirs.length, req.selection.files.length ? 'file' : 'folder'),
      dest: req.dir || '',
      phase: 'preparing',
      files: req.selection.files.length,
      doneFiles: 0,
      bytes: 0,
      total,
      rate: 0,
    },
    abort,
    xhr: null,
    lastAt: Date.now(),
    lastBytes: 0,
    publishTimer: null,
    publishedAt: 0,
  }
  jobs.set(id, job)
  putUpload(job.view)
  onStart(id)

  let handle: FsHandleInfo | null = null
  try {
    handle = await openFsFor(req.source)
    const dest = normalize(req.dir || handle.home || handle.root || '/')
    publish(job, { dest })

    // Conflicts with existing top-level names.
    const listing = await api.get<{ entries: FsEntry[] }>(`${base(handle.id)}/list`, { query: { path: dest } })
    const existing = new Map(listing.entries.map((e) => [e.name, e]))
    const isDirEntry = (e: FsEntry) => e.type === 'dir' || (e.type === 'symlink' && e.linkType === 'dir')
    const dirTops = new Set(req.selection.dirs.map((d) => d.split('/')[0]))
    // An existing folder receiving a dropped folder of the same name merges; everything else conflicts.
    const conflicts = [...top].filter((n) => existing.has(n) && !(isDirEntry(existing.get(n)!) && dirTops.has(n)))
    let policy: Exclude<ConflictPolicy, 'ask'> = 'overwrite'
    if (conflicts.length) {
      const pref = currentSettings().dropConflict
      if (pref === 'ask') {
        const r = await askConflict(dest, conflicts)
        if (!r.choice) throw new DOMException('Upload canceled', 'AbortError')
        policy = r.choice
        if (r.remember) transferSettings.set({ dropConflict: r.choice })
      } else policy = pref
    }
    let files = req.selection.files
    let dirs = req.selection.dirs
    const rename = new Map<string, string>()
    if (conflicts.length && policy === 'skip') {
      const skip = new Set(conflicts)
      files = files.filter((f) => !skip.has(f.relPath.split('/')[0]))
      dirs = dirs.filter((d) => !skip.has(d.split('/')[0]))
    } else if (conflicts.length && policy === 'rename') {
      const taken = new Set(existing.keys())
      for (const n of conflicts) {
        const u = uniqueRemoteName(n, taken, dirTops.has(n))
        taken.add(u)
        rename.set(n, u)
      }
    }
    const mapRel = (rel: string) => {
      const [first, ...rest] = rel.split('/')
      const r = rename.get(first)
      return r ? [r, ...rest].join('/') : rel
    }
    const tops = new Set<string>()
    for (const f of files) tops.add(mapRel(f.relPath).split('/')[0])
    for (const d of dirs) tops.add(mapRel(d).split('/')[0])
    publish(job, { phase: 'running', files: files.length, total: files.reduce((n, f) => n + f.file.size, 0) })

    // Folders first (parents before children), including empty ones and the parents of files.
    const folderSet = new Set<string>()
    for (const d of dirs) folderSet.add(mapRel(d))
    for (const f of files) {
      const parts = mapRel(f.relPath).split('/')
      for (let i = 1; i < parts.length; i++) folderSet.add(parts.slice(0, i).join('/'))
    }
    const folders = [...folderSet].sort((a, b) => a.split('/').length - b.split('/').length)
    for (const d of folders) {
      if (abort.signal.aborted) throw new DOMException('Upload canceled', 'AbortError')
      await api.post(`${base(handle.id)}/mkdir`, { path: joinRemotePath(dest, d), parents: true }).catch((err) => {
        if (!(isApiError(err) && err.code === 'exists')) throw err
      })
    }

    let doneBytes = 0
    let doneFiles = 0
    for (const f of files) {
      if (abort.signal.aborted) throw new DOMException('Upload canceled', 'AbortError')
      const target = joinRemotePath(dest, mapRel(f.relPath))
      publish(job, { current: mapRel(f.relPath) })
      await uploadFile(job, handle.id, target, f.file, (n) => publish(job, { bytes: doneBytes + n }))
      doneBytes += f.file.size
      doneFiles++
      publish(job, { bytes: doneBytes, doneFiles })
    }
    publish(job, { phase: 'done', current: undefined })
    return { handle, dest, paths: [...tops].map((t) => joinRemotePath(dest, t)), canceled: false }
  } catch (err) {
    const canceled = abort.signal.aborted || (err as { name?: string })?.name === 'AbortError'
    publish(job, { phase: canceled ? 'canceled' : 'error', error: canceled ? undefined : errorMessage(err) })
    if (canceled) return { handle: handle ?? undefined, dest: job.view.dest, paths: [], canceled: true }
    throw err
  } finally {
    jobs.delete(id)
    if (handle) closeFs(handle.id)
    // The toast owns the entry from here (it removes it when dismissed).
    setTimeout(() => {
      const v = job.view
      if (v.phase === 'done' || v.phase === 'canceled') dropUpload(id)
    }, 60_000)
  }
}

function normalize(p: string): string {
  if (!p) return '/'
  const s = p.replace(/\/+/g, '/')
  return s.length > 1 ? s.replace(/\/$/, '') : s
}

function uniqueRemoteName(name: string, taken: Set<string>, dir: boolean): string {
  const dot = dir ? -1 : name.lastIndexOf('.')
  const stem = dot > 0 ? name.slice(0, dot) : name
  const ext = dot > 0 ? name.slice(dot) : ''
  for (let i = 1; i < 10_000; i++) {
    const c = `${stem} (${i})${ext}`
    if (!taken.has(c)) return c
  }
  return `${stem} (${Date.now()})${ext}`
}

async function uploadFile(job: Job, fsId: string, target: string, file: File, onBytes: (n: number) => void): Promise<void> {
  const size = file.size
  const mtime = Number.isFinite(file.lastModified) && file.lastModified > 0 ? file.lastModified : undefined
  let offset = 0
  let attempts = 0
  let sent = false
  for (;;) {
    if (job.abort.signal.aborted) {
      if (sent) await removePart(fsId, target)
      throw new DOMException('Upload canceled', 'AbortError')
    }
    try {
      for (;;) {
        const end = Math.min(size, offset + CHUNK_SIZE)
        const final = end >= size
        const url = apiUrl(`${base(fsId)}/upload`, { path: target, offset, total: size, mtime, final: final ? 1 : undefined })
        sent = true
        const res = await putChunk(job, url, file.slice(offset, end), (loaded) => onBytes(offset + loaded))
        if (final) {
          onBytes(size)
          return
        }
        const got = res && typeof res.size === 'number' && Number.isFinite(res.size) ? res.size : end
        offset = got > offset && got <= size ? got : end
        onBytes(offset)
      }
    } catch (err) {
      if (job.abort.signal.aborted || (err as { name?: string })?.name === 'AbortError') {
        if (sent) await removePart(fsId, target)
        throw new DOMException('Upload canceled', 'AbortError')
      }
      if (isApiError(err) && err.code === 'offset_mismatch') {
        const s = (err.body as { size?: unknown } | undefined)?.size
        offset = typeof s === 'number' && s >= 0 && s <= size ? s : 0
      } else if (isApiError(err) && err.status === 423) {
        if (!(await requestVaultUnlock())) throw new Error('The vault is locked')
        continue
      } else if (!retriable(err)) {
        throw err
      } else {
        // Resume from what the server holds.
        const status = await api.get<{ size?: number }>(`${base(fsId)}/upload`, { query: { path: target } }).catch(() => null)
        const s = status?.size
        offset = typeof s === 'number' && s >= 0 && s <= size ? s : 0
      }
      if (++attempts >= MAX_ATTEMPTS) throw err
      await new Promise((r) => setTimeout(r, Math.min(10_000, 700 * 2 ** (attempts - 1))))
    }
  }
}

function retriable(err: unknown): boolean {
  if (!isApiError(err)) return true
  return err.status === 0 || err.status === 408 || err.status === 429 || err.status >= 500 || err.code === 'disconnected'
}

async function removePart(fsId: string, target: string): Promise<void> {
  await api.post(`${base(fsId)}/delete`, { paths: [`${target}.astraterm-part`], recursive: false }).catch(() => undefined)
}

function putChunk(job: Job, url: string, body: Blob, onProgress: (loaded: number) => void): Promise<{ size?: number } | null> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    job.xhr = xhr
    let last = Date.now()
    const watchdog = setInterval(() => {
      if (Date.now() - last > STALL_MS) xhr.abort()
    }, 5_000)
    const done = () => {
      clearInterval(watchdog)
      if (job.xhr === xhr) job.xhr = null
    }
    xhr.open('PUT', url)
    xhr.setRequestHeader('X-AstraTerm', '1')
    xhr.setRequestHeader('Content-Type', 'application/octet-stream')
    xhr.setRequestHeader('Accept', 'application/json')
    xhr.upload.onprogress = (e) => {
      last = Date.now()
      onProgress(e.loaded)
    }
    xhr.upload.onload = () => clearInterval(watchdog)
    xhr.onload = () => {
      done()
      if (xhr.status >= 200 && xhr.status < 300) {
        try {
          resolve(xhr.responseText ? (JSON.parse(xhr.responseText) as { size?: number }) : null)
        } catch {
          resolve(null)
        }
        return
      }
      let message = xhr.statusText || `HTTP ${xhr.status}`
      let code = 'error'
      let parsed: unknown
      try {
        const b = JSON.parse(xhr.responseText) as { error?: string; code?: string }
        parsed = b
        if (b.error) message = b.error
        if (b.code) code = b.code
      } catch {
        /* not JSON */
      }
      if (xhr.status === 401) markUnauthenticated()
      reject(new ApiError(xhr.status, code, message, parsed))
    }
    xhr.onerror = () => {
      done()
      reject(new ApiError(0, 'network_error', 'Network error while uploading'))
    }
    xhr.onabort = () => {
      done()
      reject(job.abort.signal.aborted ? new DOMException('Upload canceled', 'AbortError') : new ApiError(0, 'network_error', 'The upload stalled'))
    }
    xhr.send(body)
  })
}
