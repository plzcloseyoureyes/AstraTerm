/*
 * File endpoints used by the editor (SPEC §6.0 "Files"). Kept inside the feature (web/src/api is foundation-owned).
 *
 *   GET  /api/fs/{id}/read?path=&maxBytes=  → {content, encoding:'utf-8'|'base64', size, mtime, mode}
 *   PUT  /api/fs/{id}/write {path, content, encoding, expectMtime?, sudo?} → FileEntry | 409 conflict
 *   GET  /api/fs/{id}/stat?path=            → FileEntry
 *   GET  /api/fs/{id}/list?path=            → {path, parent, entries}
 *   POST /api/fs {sessionId?|connectionId?|local?} → FsHandle ; DELETE /api/fs/{id}
 */
import { api, apiUrl, isApiError, seg, type ApiError } from '@/api/client'
import type { FileEntry, FileReadResult, FsHandle, FsListResult, FsOpenRequest } from '@/api/types'
import { queryClient } from '@/api/queryClient'

export interface WriteFileRequest {
  path: string
  content: string
  encoding: 'utf-8' | 'base64'
  /** Last mtime we read/wrote: the server answers 409 when the file changed since (second precision). */
  expectMtime?: string
  /** Size we last read/wrote (files-backend extra: catches same-second changes). */
  expectSize?: number
  /** Write through sudo (FILE-11) after a permission error. */
  sudo?: boolean
}

/** GET /api/fs/{id} (files-backend): the handle with its origin. */
export interface FsHandleInfo extends FsHandle {
  driver?: string
  userHome?: string
  sessionId?: string
  connectionId?: string
  protocol?: string
  host?: string
  username?: string
  port?: number
  capabilities: FsHandle['capabilities'] & { sudo?: boolean; mtime?: boolean }
}

const fsPath = (fsId: string, op: string) => `/api/fs/${seg(fsId)}/${op}`

export function readFile(fsId: string, path: string, maxBytes?: number, signal?: AbortSignal): Promise<FileReadResult> {
  return api.get<FileReadResult>(fsPath(fsId, 'read'), { query: { path, maxBytes }, signal })
}

export function writeFile(fsId: string, req: WriteFileRequest): Promise<FileEntry | undefined> {
  const body: WriteFileRequest = { path: req.path, content: req.content, encoding: req.encoding }
  if (req.expectMtime) body.expectMtime = req.expectMtime
  if (req.expectMtime && typeof req.expectSize === 'number') body.expectSize = req.expectSize
  if (req.sudo) body.sudo = true
  return api.put<FileEntry | undefined>(fsPath(fsId, 'write'), body)
}

export function statFile(fsId: string, path: string, signal?: AbortSignal): Promise<FileEntry> {
  return api.get<FileEntry>(fsPath(fsId, 'stat'), { query: { path }, signal })
}

export function listDir(fsId: string, path: string, signal?: AbortSignal): Promise<FsListResult> {
  return api.get<FsListResult>(fsPath(fsId, 'list'), { query: { path }, signal })
}

export function openFs(req: FsOpenRequest): Promise<FsHandleInfo> {
  return api.post<FsHandleInfo>('/api/fs', req)
}

/** Handle details (label, origin, capabilities); 404 fs_not_found when it expired. */
export function getFsInfo(fsId: string, signal?: AbortSignal): Promise<FsHandleInfo> {
  return api.get<FsHandleInfo>(`/api/fs/${seg(fsId)}`, { signal })
}

/** The caller's open handles. */
export function listFsHandles(): Promise<FsHandleInfo[]> {
  return api.get<FsHandleInfo[]>('/api/fs')
}

/** Files under `dir` containing `content` (case-insensitive, fixed string) whose name matches `pattern` (glob). */
export function searchFiles(
  fsId: string,
  req: { path: string; content: string; pattern?: string; maxResults?: number },
  signal?: AbortSignal,
): Promise<{ entries: FileEntry[]; truncated: boolean }> {
  return api.post<{ entries: FileEntry[]; truncated: boolean }>(fsPath(fsId, 'search'), { pattern: '', maxResults: 500, ...req }, { signal })
}

export function realpath(fsId: string, path: string): Promise<{ path: string }> {
  return api.get<{ path: string }>(fsPath(fsId, 'realpath'), { query: { path } })
}

/** How to re-open a handle like this one (null for quick-connect handles). */
export function sourceOf(h: Partial<FsHandleInfo>): FsOpenRequest | undefined {
  if (h.sessionId) return { sessionId: h.sessionId }
  if (h.connectionId) return { connectionId: h.connectionId }
  if (h.kind === 'local') return { local: true }
  return undefined
}

/** Short label for status bars: the handle label, else user@host. */
export function infoLabel(h: Partial<FsHandleInfo>): string | undefined {
  if (h.label) return h.label
  if (h.host) return h.username ? `${h.username}@${h.host}` : h.host
  return undefined
}

export function closeFs(fsId: string): Promise<void> {
  return api.del<void>(`/api/fs/${seg(fsId)}`)
}

/** Same-origin, cookie-authenticated download URL. */
export function downloadUrl(fsId: string, path: string): string {
  return apiUrl(fsPath(fsId, 'download'), { path })
}

// ---------------------------------------------------------------------------------------------------------------------
// error classification
// ---------------------------------------------------------------------------------------------------------------------

const PERM_RE = /permission|denied|eacces|eperm|not permitted|read-only file ?system|forbidden/i

/** 409 codes that are not "the file changed since expectMtime" (files-backend error codes). */
const NON_MTIME_409 = new Set(['disconnected', 'session_not_connected', 'exists', 'not_empty', 'fs_error', 'ssh_browser_disabled', 'offset_mismatch'])

/** 409: the file changed on the server since `expectMtime`. */
export function isConflict(err: unknown): err is ApiError {
  return isApiError(err) && err.status === 409 && !NON_MTIME_409.has(err.code)
}

/** The transport to the remote host is gone (409 disconnected / session_not_connected). */
export function isDisconnected(err: unknown): err is ApiError {
  return isApiError(err) && (err.code === 'disconnected' || err.code === 'session_not_connected')
}

/** Write refused by the remote file system (FILE-11: offer "Save with sudo"). */
export function isPermissionDenied(err: unknown): err is ApiError {
  if (!isApiError(err)) return false
  if (err.code === 'permission_denied') return true
  if (err.code === 'csrf' || err.code === 'invalid_password' || err.code === 'account_disabled') return false
  if (err.status === 403) return true
  return PERM_RE.test(err.code) || (err.status >= 400 && err.status < 500 && PERM_RE.test(err.message))
}

export function isNotFound(err: unknown): err is ApiError {
  return isApiError(err) && err.status === 404
}

export function isTooLarge(err: unknown): err is ApiError {
  return isApiError(err) && (err.status === 413 || err.code === 'too_large')
}

export function isNotSupported(err: unknown): err is ApiError {
  return isApiError(err) && err.code === 'not_supported'
}

/** The fs handle itself is gone (backend restarted, files tab closed it) rather than the file. */
export function isHandleGone(err: unknown): boolean {
  if (!isApiError(err) || err.status !== 404) return false
  return err.code === 'fs_not_found' || /^file ?system handle/i.test(err.message)
}

// ---------------------------------------------------------------------------------------------------------------------
// handle labels
// ---------------------------------------------------------------------------------------------------------------------

const handleLabels = new Map<string, string>()

/** Remember a handle's label (from openFs or a caller) for status bars and pickers. */
export function rememberHandle(h: Pick<FsHandle, 'id' | 'label'>): void {
  if (h.id && h.label) handleLabels.set(h.id, h.label)
}

/**
 * Best-effort label for a handle id: explicit label, handles we opened, or any FsHandle another feature keeps in the
 * react-query cache (the files browser).
 */
export function handleLabel(fsId: string, explicit?: string): string | undefined {
  if (explicit) return explicit
  const known = handleLabels.get(fsId)
  if (known) return known
  try {
    for (const q of queryClient.getQueryCache().getAll()) {
      const found = findHandle(q.state.data, fsId, 0)
      if (found?.label) {
        handleLabels.set(fsId, found.label)
        return found.label
      }
    }
  } catch {
    /* cache shape unknown — ignore */
  }
  return undefined
}

function findHandle(v: unknown, fsId: string, depth: number): FsHandle | undefined {
  if (!v || typeof v !== 'object' || depth > 2) return undefined
  if (Array.isArray(v)) {
    for (const item of v.slice(0, 50)) {
      const f = findHandle(item, fsId, depth + 1)
      if (f) return f
    }
    return undefined
  }
  const o = v as Partial<FsHandle>
  if (o.id === fsId && typeof o.label === 'string' && typeof o.kind === 'string') return o as FsHandle
  return undefined
}

// ---------------------------------------------------------------------------------------------------------------------
// downloads
// ---------------------------------------------------------------------------------------------------------------------

/** Start a browser download of a URL (same-origin: the session cookie authenticates it). */
export function triggerDownload(href: string, filename: string): void {
  const a = document.createElement('a')
  a.href = href
  a.download = filename
  a.rel = 'noopener'
  a.style.display = 'none'
  document.body.appendChild(a)
  a.click()
  a.remove()
}

/** Download in-memory content as a file. */
export function downloadBlob(data: BlobPart, filename: string, type = 'application/octet-stream'): void {
  const url = URL.createObjectURL(new Blob([data], { type }))
  triggerDownload(url, filename)
  setTimeout(() => URL.revokeObjectURL(url), 60_000)
}
