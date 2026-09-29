/*
 * Upload engine (FILE-5, FILE-6): browser files → PUT /api/fs/{id}/upload?path=<target>&offset=<n>&mtime=<unix>
 * [&final=1] with raw 8 MiB chunks. The server appends at `offset` to a partial file and answers {size}; the last
 * chunk carries final=1 (atomic rename into place, mtime applied). After a failure the upload resumes from the size
 * the server already has (GET …/upload?path= → {size}, 0 when unknown). Several files upload in parallel
 * (settings.files.uploadParallelism, default 3); progress comes from XHR upload events.
 *
 *   planUpload(ctx, destDir, files, dirs)   conflict check against the destination + start
 *   collectDrop(dataTransfer)                files and folders dropped from the OS (webkitGetAsEntry recursion)
 */
import { toast } from 'sonner'
import { ApiError, isApiError } from '@/api/client'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { RuntimeSession } from '@/api/types'
import { errorMessage, plural, uid } from '@/lib/utils'
import { markUnauthenticated } from '@/stores/auth'
import { requestVaultUnlock } from '@/stores/ui'
import { fsApi, fsKeys, isDisconnected, isHandleGone } from './api'
import { askConflict } from './dialogs/store'
// The server receives chunks into "<target>.nexterm-part" and renames it onto the target with the final chunk.
import { PART_SUFFIX } from './format'
import { openFs, useFsStore, withFs } from './fsHandles'
import { dirname, joinPath, normalizePath, uniqueName } from './paths'
import { filesSettings, uploadParallelism } from './settings'
import { openTransfers, publishLocal, removeLocal, transfersShown, type TransferState, type TransferView } from './transfers/store'
import type { ConflictPolicy, FsContext } from './types'

export const CHUNK_SIZE = 8 * 1024 * 1024
const MAX_ATTEMPTS = 6

/** A browser file and its path relative to the upload's destination folder ("sub/dir/name.txt"). */
export interface LocalFile {
  file: File
  relPath: string
}

interface Item {
  id: string
  file: File
  relPath: string
  target: string
  size: number
  /** Bytes the server confirmed. */
  confirmed: number
  /** Bytes of the chunk in flight. */
  inflight: number
  state: TransferState | 'skipped'
  error?: string
  attempts: number
  /** `confirmed` already holds the server's part size (no status query needed before resuming). */
  resumeKnown?: boolean
  /** Ask the server for its part size before the next chunk (after the transport came back). */
  resync?: boolean
  /** A chunk was sent: the server may hold a partial file. */
  sent?: boolean
  /** When the file system's transport was found gone (session reconnecting / link lost); cleared by a good chunk. */
  lostAt?: number
  /** Shown in the queue while the item waits (e.g. for the SSH session to reconnect). */
  waiting?: string
  xhr?: XMLHttpRequest
}

interface Job {
  id: string
  key: string
  ctx: FsContext
  destDir: string
  label: string
  items: Item[]
  /** Absolute folders to create first (folder uploads, including empty ones). */
  dirs: string[]
  dirsReady: boolean
  canceled: boolean
  createdAt: number
  finishedAt?: number
  /** Speed measurement. */
  lastBytes: number
  lastAt: number
  bytesPerSec: number
  notified: boolean
}

const jobs = new Map<string, Job>()
let running = 0

// ---------------------------------------------------------------------------------------------------------------------
// public API
// ---------------------------------------------------------------------------------------------------------------------

/**
 * Upload browser files into `destDir` of a file system. Top-level names that already exist are resolved with the
 * conflict policy (asking when it is "ask"). Resolves the job id, or null when cancelled / nothing to do.
 */
export async function planUpload(ctx: FsContext, destDir: string, files: LocalFile[], dirs: string[] = []): Promise<string | null> {
  const dest = normalizePath(destDir)
  if (!files.length && !dirs.length) return null
  // Top-level names of the upload.
  const top = new Set<string>()
  for (const f of files) top.add(f.relPath.split('/')[0])
  for (const d of dirs) top.add(d.split('/')[0])

  let existing: Set<string>
  let existingDirs: Set<string>
  try {
    const listing = await withFs(ctx.key, (id) => fsApi.list(id, dest))
    existing = new Set(listing.entries.map((e) => e.name))
    existingDirs = new Set(listing.entries.filter((e) => e.type === 'dir' || (e.type === 'symlink' && e.linkType === 'dir')).map((e) => e.name))
  } catch (err) {
    toast.error(`Cannot upload to ${dest}`, { description: errorMessage(err) })
    return null
  }

  // Existing folders are merged; existing files conflict.
  const conflicts = [...top].filter((n) => existing.has(n) && !(existingDirs.has(n) && isFolderName(n, files, dirs)))
  let policy: ConflictPolicy = 'overwrite'
  if (conflicts.length) {
    const pref = filesSettings.get().conflictPolicy
    const chosen = pref === 'ask' ? await askConflict(conflicts, dest, 'upload') : pref
    if (!chosen) return null
    policy = chosen
  }

  let list = files
  let dirList = dirs
  if (conflicts.length && policy === 'skip') {
    const skip = new Set(conflicts)
    list = files.filter((f) => !skip.has(f.relPath.split('/')[0]))
    dirList = dirs.filter((d) => !skip.has(d.split('/')[0]))
  } else if (conflicts.length && policy === 'rename') {
    const taken = new Set(existing)
    const renamed = new Map<string, string>()
    for (const n of conflicts) {
      const u = uniqueName(n, taken)
      taken.add(u)
      renamed.set(n, u)
    }
    const rewrite = (rel: string) => {
      const [first, ...rest] = rel.split('/')
      const r = renamed.get(first)
      return r ? [r, ...rest].join('/') : rel
    }
    list = files.map((f) => ({ file: f.file, relPath: rewrite(f.relPath) }))
    dirList = dirs.map(rewrite)
  }
  if (!list.length && !dirList.length) {
    toast.info('Nothing to upload', { description: 'Every item already exists and was skipped.' })
    return null
  }
  return startUpload(ctx, dest, list, dirList)
}

/** Start an upload without conflict checks. */
export function startUpload(ctx: FsContext, destDir: string, files: LocalFile[], dirs: string[] = []): string {
  const dest = normalizePath(destDir)
  const items: Item[] = files.map((f) => ({
    id: uid('up'),
    file: f.file,
    relPath: f.relPath,
    target: joinPath(dest, f.relPath),
    size: f.file.size,
    confirmed: 0,
    inflight: 0,
    state: 'queued',
    attempts: 0,
  }))
  const dirSet = new Set<string>()
  for (const d of dirs) if (d) dirSet.add(joinPath(dest, d))
  for (const it of items) {
    const parent = dirname(it.target)
    if (parent !== dest) dirSet.add(parent)
  }
  const topNames = new Set(files.map((f) => f.relPath.split('/')[0]).concat(dirs.map((d) => d.split('/')[0])))
  const label = topNames.size === 1 ? [...topNames][0] : files.length ? plural(files.length, 'file') : plural(dirs.length, 'folder')
  const job: Job = {
    id: uid('upload'),
    key: ctx.key,
    ctx,
    destDir: dest,
    label,
    items,
    dirs: [...dirSet].sort((a, b) => a.split('/').length - b.split('/').length),
    dirsReady: dirSet.size === 0,
    canceled: false,
    createdAt: Date.now(),
    lastBytes: 0,
    lastAt: Date.now(),
    bytesPerSec: 0,
    notified: false,
  }
  jobs.set(job.id, job)
  publish(job, true)
  if (filesSettings.get().openQueueOnTransfer) openTransfers()
  pump()
  return job.id
}

/** Upload jobs currently running into a folder (the status line of a view shows them). */
export function activeUploadsInto(fsKey: string, dir: string): number {
  let n = 0
  for (const j of jobs.values()) if (j.key === fsKey && j.destDir === dir && jobState(j) === 'running') n++
  return n
}

// ---------------------------------------------------------------------------------------------------------------------
// OS drag & drop / pickers
// ---------------------------------------------------------------------------------------------------------------------

/**
 * Snapshot the dropped entries synchronously (a DataTransfer is only readable inside the drop handler), then walk
 * folders asynchronously. Returns files with relative paths plus every folder (so empty folders are created too).
 */
export function collectDrop(dt: DataTransfer): Promise<{ files: LocalFile[]; dirs: string[] }> {
  const entries: FileSystemEntry[] = []
  const loose: File[] = []
  for (const item of Array.from(dt.items ?? [])) {
    if (item.kind !== 'file') continue
    const entry = typeof item.webkitGetAsEntry === 'function' ? item.webkitGetAsEntry() : null
    if (entry) entries.push(entry)
    else {
      const f = item.getAsFile()
      if (f) loose.push(f)
    }
  }
  if (!entries.length && !loose.length) loose.push(...Array.from(dt.files ?? []))
  return (async () => {
    const files: LocalFile[] = loose.map((f) => ({ file: f, relPath: f.name }))
    const dirs: string[] = []
    const walk = async (entry: FileSystemEntry, prefix: string): Promise<void> => {
      const rel = prefix ? `${prefix}/${entry.name}` : entry.name
      if (entry.isFile) {
        const file = await new Promise<File | null>((resolve) => (entry as FileSystemFileEntry).file(resolve, () => resolve(null)))
        if (file) files.push({ file, relPath: rel })
        return
      }
      if (!entry.isDirectory) return
      dirs.push(rel)
      const reader = (entry as FileSystemDirectoryEntry).createReader()
      // readEntries returns batches (Chromium: 100) until an empty batch.
      for (;;) {
        const batch = await new Promise<FileSystemEntry[]>((resolve) => reader.readEntries(resolve, () => resolve([])))
        if (!batch.length) break
        for (const child of batch) await walk(child, rel)
      }
    }
    for (const e of entries) await walk(e, '')
    return { files, dirs }
  })()
}

/** Await `p`, showing a loading toast when it takes a noticeable while (walking a dropped folder tree…). */
export async function withSlowToast<T>(p: Promise<T>, message: string, delayMs = 400): Promise<T> {
  let id: string | number | undefined
  const timer = setTimeout(() => {
    id = toast.loading(message)
  }, delayMs)
  try {
    return await p
  } finally {
    clearTimeout(timer)
    if (id !== undefined) toast.dismiss(id)
  }
}

/** Files chosen with <input type=file> (webkitdirectory inputs carry webkitRelativePath). */
export function filesFromInput(list: FileList | null): { files: LocalFile[]; dirs: string[] } {
  const files: LocalFile[] = []
  const dirs = new Set<string>()
  for (const f of Array.from(list ?? [])) {
    const rel = (f.webkitRelativePath || f.name).replace(/^\/+/, '')
    files.push({ file: f, relPath: rel })
    const parts = rel.split('/')
    for (let i = 1; i < parts.length; i++) dirs.add(parts.slice(0, i).join('/'))
  }
  return { files, dirs: [...dirs] }
}

/** Open the browser's file (or folder) picker and resolve the choice (empty when cancelled). */
export function pickLocalFiles(folder: boolean): Promise<{ files: LocalFile[]; dirs: string[] }> {
  return new Promise((resolve) => {
    const input = document.createElement('input')
    input.type = 'file'
    input.multiple = true
    if (folder) input.setAttribute('webkitdirectory', '')
    input.style.display = 'none'
    let done = false
    const finish = (r: { files: LocalFile[]; dirs: string[] }) => {
      if (done) return
      done = true
      input.remove()
      resolve(r)
    }
    input.addEventListener('change', () => finish(filesFromInput(input.files)))
    input.addEventListener('cancel', () => finish({ files: [], dirs: [] }))
    document.body.appendChild(input)
    input.click()
  })
}

function isFolderName(name: string, files: LocalFile[], dirs: string[]): boolean {
  return dirs.some((d) => d === name || d.startsWith(`${name}/`)) || files.some((f) => f.relPath.startsWith(`${name}/`))
}

// ---------------------------------------------------------------------------------------------------------------------
// scheduler
// ---------------------------------------------------------------------------------------------------------------------

function nextItem(): { job: Job; item: Item } | null {
  const ordered = [...jobs.values()].sort((a, b) => a.createdAt - b.createdAt)
  for (const job of ordered) {
    if (job.canceled || !job.dirsReady) continue
    const item = job.items.find((i) => i.state === 'queued')
    if (item) return { job, item }
  }
  return null
}

function pump(): void {
  // Folder creation first (one job at a time, sequentially).
  for (const job of jobs.values()) {
    if (!job.dirsReady && !job.canceled && !creatingDirs.has(job.id)) void createDirs(job)
  }
  const limit = uploadParallelism()
  while (running < limit) {
    const next = nextItem()
    if (!next) break
    running++
    next.item.state = 'running'
    publish(next.job, true)
    void runItem(next.job, next.item).finally(() => {
      running--
      publish(next.job, true)
      finishIfDone(next.job)
      pump()
    })
  }
}

const creatingDirs = new Set<string>()

async function createDirs(job: Job): Promise<void> {
  creatingDirs.add(job.id)
  try {
    for (const d of job.dirs) {
      if (job.canceled) return
      await withFs(job.key, (id) => fsApi.mkdir(id, d, true)).catch((err) => {
        // "Already exists" is fine (folder merge).
        if (isApiError(err) && err.status === 409) return
        throw err
      })
    }
    job.dirsReady = true
  } catch (err) {
    const msg = errorMessage(err)
    for (const it of job.items) if (it.state === 'queued') Object.assign(it, { state: 'error', error: `Folder could not be created: ${msg}` })
    job.dirsReady = true
    publish(job, true)
    finishIfDone(job)
  } finally {
    creatingDirs.delete(job.id)
    pump()
  }
}

function currentFsId(job: Job): string | undefined {
  const e = useFsStore.getState().entries[job.key]
  return e?.status === 'ready' ? e.handle?.id : undefined
}

async function runItem(job: Job, item: Item): Promise<void> {
  const mtime = item.file.lastModified ? Math.floor(item.file.lastModified / 1000) : undefined
  for (;;) {
    try {
      let offset = item.confirmed
      if ((item.attempts > 0 || item.resync) && !item.resumeKnown) {
        // Resume: ask how much of the partial file the server has (unknown → start over at 0, which truncates the
        // part; a transport that is still gone waits again instead).
        const st = await withFs(job.key, (id) => fsApi.uploadStatus(id, item.target)).catch((err: unknown) => {
          if (isDisconnected(err)) throw err
          return null
        })
        offset = st && st.size >= 0 && st.size <= item.size ? st.size : 0
        item.confirmed = offset
      }
      item.resumeKnown = false
      item.resync = false
      for (;;) {
        if (job.canceled || item.state === 'canceled') throw new DOMException('Upload canceled', 'AbortError')
        const end = Math.min(item.size, offset + CHUNK_SIZE)
        const final = end >= item.size
        const fsId = currentFsId(job) ?? (await openFs(job.ctx.source)).id
        const url = fsApi.uploadUrl(fsId, item.target, offset, { mtime, final, total: item.size })
        item.sent = true
        const res = await putChunk(item, url, item.file.slice(offset, end))
        const size = res && typeof res.size === 'number' && Number.isFinite(res.size) ? res.size : end
        item.inflight = 0
        item.lostAt = undefined
        if (final) {
          item.confirmed = item.size
          break
        }
        // The server's size wins (it may already hold more from an earlier attempt).
        offset = size > offset && size <= item.size ? size : end
        item.confirmed = offset
        publish(job)
      }
      item.state = 'done'
      item.error = undefined
      return
    } catch (err) {
      item.inflight = 0
      item.xhr = undefined
      if (err instanceof DOMException && err.name === 'AbortError') {
        item.state = 'canceled'
        return
      }
      if (isApiError(err) && err.code === 'offset_mismatch') {
        // The server holds a different part size (another attempt got further): continue from there.
        const size = (err.body as { size?: unknown } | undefined)?.size
        item.confirmed = typeof size === 'number' && size >= 0 && size <= item.size ? size : 0
        item.resumeKnown = true
        if (item.attempts + 1 >= MAX_ATTEMPTS) {
          item.state = 'error'
          item.error = errorMessage(err)
          return
        }
        item.attempts++
        continue
      }
      if (isApiError(err) && err.status === 423) {
        if (!(await requestVaultUnlock())) {
          item.state = 'error'
          item.error = 'The vault is locked'
          return
        }
        continue
      }
      if (isDisconnected(err)) {
        // The file system's transport dropped (the terminal's SSH connection is reconnecting, or waits for the user
        // to reconnect it): wait for it — cancellable — and resume from the server's part size, instead of failing.
        item.lostAt ??= Date.now()
        if (Date.now() - item.lostAt > RECONNECT_WAIT_MS) {
          item.state = 'error'
          item.error = `The connection was lost: ${errorMessage(err)}`
          return
        }
        item.waiting = 'Waiting for the connection…'
        publish(job, true)
        const goOn = await waitForTransport(job, item)
        item.waiting = undefined
        if (!goOn) {
          item.state = 'canceled'
          return
        }
        item.resync = true
        publish(job, true)
        continue
      }
      if (isHandleGone(err)) {
        if (item.attempts + 1 >= MAX_ATTEMPTS) {
          item.state = 'error'
          item.error = errorMessage(err)
          return
        }
        await openFs(job.ctx.source, { force: true }).catch(() => undefined)
      } else if (!retriable(err) || item.attempts + 1 >= MAX_ATTEMPTS) {
        item.state = 'error'
        item.error = errorMessage(err)
        return
      }
      item.attempts++
      publish(job, true)
      await sleep(Math.min(15_000, 800 * 2 ** (item.attempts - 1)))
      if (job.canceled || item.state === 'canceled') {
        item.state = 'canceled'
        return
      }
    }
  }
}

/** How long an upload waits for a lost transport (reconnecting session) before it fails. */
const RECONNECT_WAIT_MS = 3 * 60_000

/**
 * Wait until the transport may be back: a session source is `connected` again (its terminal reconnected); other
 * sources simply back off (the backend re-dials pooled connections). False when the upload was canceled meanwhile.
 */
async function waitForTransport(job: Job, item: Item): Promise<boolean> {
  const src = job.ctx.source
  const deadline = (item.lostAt ?? Date.now()) + RECONNECT_WAIT_MS
  const pause = src.kind === 'session' ? 1000 : Math.min(8000, 1000 * 2 ** Math.min(3, item.attempts))
  for (;;) {
    await sleep(pause)
    if (job.canceled || item.state === 'canceled') return false
    if (src.kind !== 'session' || Date.now() > deadline) return true
    const s = queryClient.getQueryData<RuntimeSession[]>(queryKeys.sessions)?.find((x) => x.id === src.sessionId)
    // Connected again, or gone for good (the next attempt reports the real error).
    if (!s || s.state === 'connected' || s.state === 'closed') return true
  }
}

function retriable(err: unknown): boolean {
  if (!isApiError(err)) return true
  return err.status === 0 || err.status === 408 || err.status === 429 || err.status >= 500
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms))
}

/** A chunk whose bytes stop leaving the browser for this long is aborted and retried (dead link, stuck proxy). */
const STALL_MS = 60_000

function putChunk(item: Item, url: string, body: Blob): Promise<{ size?: number } | null> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    item.xhr = xhr
    let lastProgress = Date.now()
    let stalled = false
    const watchdog = setInterval(() => {
      if (Date.now() - lastProgress < STALL_MS) return
      stalled = true
      xhr.abort()
    }, 5_000)
    const done = () => {
      clearInterval(watchdog)
      item.xhr = undefined
    }
    xhr.open('PUT', url)
    xhr.setRequestHeader('X-NexTerm', '1')
    xhr.setRequestHeader('Content-Type', 'application/octet-stream')
    xhr.setRequestHeader('Accept', 'application/json')
    xhr.upload.onprogress = (e) => {
      lastProgress = Date.now()
      item.inflight = e.loaded
      const job = jobOf(item)
      if (job) publish(job)
    }
    // Every byte is out: the reply follows once the server stored them (no stall timer for that part).
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
      let code = xhr.status === 403 ? 'forbidden' : xhr.status === 409 ? 'conflict' : 'error'
      let body: unknown
      try {
        const b = JSON.parse(xhr.responseText) as { error?: string; code?: string }
        body = b
        if (b.error) message = b.error
        if (b.code) code = b.code
      } catch {
        /* not JSON */
      }
      if (xhr.status === 401) markUnauthenticated()
      reject(new ApiError(xhr.status, code, message, body))
    }
    xhr.onerror = () => {
      done()
      reject(new ApiError(0, 'network_error', 'Network error while uploading'))
    }
    xhr.onabort = () => {
      done()
      // The watchdog's abort is a retriable network failure; any other abort is the user's cancel.
      reject(stalled ? new ApiError(0, 'network_error', 'The upload stalled (no progress for a minute)') : new DOMException('Upload canceled', 'AbortError'))
    }
    xhr.send(body)
  })
}

function jobOf(item: Item): Job | undefined {
  for (const j of jobs.values()) if (j.items.includes(item)) return j
  return undefined
}

// ---------------------------------------------------------------------------------------------------------------------
// job state & publishing
// ---------------------------------------------------------------------------------------------------------------------

function jobState(job: Job): TransferState {
  let queued = 0
  let active = 0
  let errors = 0
  let canceled = 0
  for (const it of job.items) {
    if (it.state === 'queued') queued++
    else if (it.state === 'running') active++
    else if (it.state === 'error') errors++
    else if (it.state === 'canceled') canceled++
  }
  if (active || (!job.dirsReady && !job.canceled)) return 'running'
  if (queued) return job.canceled ? 'canceled' : 'queued'
  if (errors) return 'error'
  if (canceled || job.canceled) return 'canceled'
  return 'done'
}

const publishTimers = new Map<string, ReturnType<typeof setTimeout>>()

function publish(job: Job, now = false): void {
  if (!jobs.has(job.id)) return
  if (!now) {
    if (publishTimers.has(job.id)) return
    publishTimers.set(
      job.id,
      setTimeout(() => {
        publishTimers.delete(job.id)
        publish(job, true)
      }, 250),
    )
    return
  }
  const t = publishTimers.get(job.id)
  if (t) clearTimeout(t)
  publishTimers.delete(job.id)
  publishLocal(view(job), {
    cancel: () => cancelJob(job.id),
    retry: () => retryJob(job.id),
    remove: () => removeJob(job.id),
  })
}

function view(job: Job): TransferView {
  let totalBytes = 0
  let doneBytes = 0
  let doneFiles = 0
  let failedFiles = 0
  let current: string | undefined
  let note: string | undefined
  const problems: { name: string; error: string }[] = []
  for (const it of job.items) {
    totalBytes += it.size
    doneBytes += it.state === 'done' ? it.size : Math.min(it.size, it.confirmed + it.inflight)
    if (it.state === 'done') doneFiles++
    if (it.state === 'error') {
      failedFiles++
      problems.push({ name: it.relPath, error: it.error ?? 'Failed' })
    }
    if (it.state === 'running' && !current) current = it.relPath
    if (it.state === 'running' && it.waiting && !note) note = it.waiting
  }
  const now = Date.now()
  const dt = (now - job.lastAt) / 1000
  if (dt >= 0.5) {
    const inst = Math.max(0, doneBytes - job.lastBytes) / dt
    job.bytesPerSec = job.bytesPerSec ? job.bytesPerSec * 0.6 + inst * 0.4 : inst
    job.lastBytes = doneBytes
    job.lastAt = now
  }
  const state = jobState(job)
  if (state !== 'running' && state !== 'queued' && !job.finishedAt) job.finishedAt = now
  return {
    id: job.id,
    origin: 'local',
    kind: 'upload',
    label: job.label,
    detail: `to ${job.destDir} on ${job.ctx.label}`,
    state,
    error: failedFiles ? (failedFiles === 1 ? problems[0].error : `${failedFiles} files failed`) : undefined,
    totalBytes,
    doneBytes,
    totalFiles: job.items.length,
    doneFiles,
    failedFiles,
    currentFile: current,
    bytesPerSec: state === 'running' && !note ? job.bytesPerSec : 0,
    note: state === 'running' ? note : undefined,
    createdAt: job.createdAt,
    finishedAt: job.finishedAt,
    canCancel: state === 'running' || state === 'queued',
    canRetry: state === 'error' || state === 'canceled',
    problems: problems.length ? problems.slice(0, 50) : undefined,
  }
}

function finishIfDone(job: Job): void {
  const state = jobState(job)
  if (state === 'running' || state === 'queued') return
  // New files and folders appear in every view of the destination (and of created sub-folders' parents).
  const fsId = currentFsId(job)
  if (fsId) {
    const dirs = new Set([job.destDir, ...job.items.map((i) => dirname(i.target)), ...job.dirs.map((d) => dirname(d))])
    for (const d of dirs) void queryClient.invalidateQueries({ queryKey: fsKeys.list(fsId, d) })
  }
  if (job.notified) return
  job.notified = true
  if (transfersShown()) return
  const v = view(job)
  if (state === 'done') {
    toast.success(`Uploaded ${v.totalFiles === 1 ? v.label : plural(v.totalFiles, 'file')}`, { description: `${job.destDir} on ${job.ctx.label}` })
  } else if (state === 'error') {
    toast.error(v.failedFiles === v.totalFiles ? `Upload failed: ${job.label}` : `${v.failedFiles} of ${v.totalFiles} uploads failed`, {
      description: v.error,
      duration: 10_000,
      action: { label: 'Details', onClick: openTransfers },
    })
  }
}

function cancelJob(id: string): void {
  const job = jobs.get(id)
  if (!job) return
  job.canceled = true
  for (const it of job.items) {
    if (it.state === 'queued') it.state = 'canceled'
    else if (it.state === 'running') {
      it.state = 'canceled'
      it.xhr?.abort()
    }
  }
  job.notified = true
  publish(job, true)
}

function retryJob(id: string): void {
  const job = jobs.get(id)
  if (!job) return
  job.canceled = false
  job.notified = false
  job.finishedAt = undefined
  for (const it of job.items) {
    if (it.state === 'error' || it.state === 'canceled') {
      it.state = 'queued'
      it.error = undefined
      // attempts > 0 → resume from the server's partial size.
      it.attempts = Math.max(1, it.attempts)
    }
  }
  publish(job, true)
  pump()
}

function removeJob(id: string): void {
  const job = jobs.get(id)
  if (job) {
    const st = jobState(job)
    if (st === 'running' || st === 'queued') cancelJob(id)
    jobs.delete(id)
    discardParts(job)
  }
  removeLocal(id)
}

/**
 * The user gave up on canceled / failed items (removed them from the queue, so no retry): delete the partial files
 * they left on the server. Best effort; skips targets another upload is writing right now.
 */
function discardParts(job: Job): void {
  const busy = new Set<string>()
  for (const other of jobs.values()) {
    if (other.key !== job.key) continue
    for (const it of other.items) if (it.state === 'queued' || it.state === 'running') busy.add(it.target)
  }
  const parts = job.items.filter((it) => it.sent && (it.state === 'canceled' || it.state === 'error') && !busy.has(it.target)).map((it) => it.target + PART_SUFFIX)
  if (!parts.length) return
  void (async () => {
    // Wait for aborted requests to settle on the server (the part may still be written to for a moment).
    await sleep(1500)
    for (const part of parts) await withFs(job.key, (fsId) => fsApi.remove(fsId, [part], false)).catch(() => undefined)
  })()
}
