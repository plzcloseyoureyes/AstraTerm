/*
 * Typed endpoint functions for SPEC §6.0 "Files" (/api/fs, /api/transfers) and react-query keys.
 *
 * Handles: POST /api/fs opens a file system (session / saved connection / local host / quick connect) and returns an
 * FsHandle whose id prefixes every other call. The backend may time handles out or drop them when their SSH transport
 * goes away; isHandleGone() recognises those errors so callers can reopen transparently.
 */
import { api, apiUrl, isApiError, seg, type RequestOptions } from '@/api/client'
import type { FileEntry, FileReadResult, FsListResult, Transfer, TransferRequest } from '@/api/types'
import type { FsHandleEx, FsOpenBody } from './types'

const base = (id: string) => `/api/fs/${seg(id)}`

export const fsKeys = {
  all: ['fs'] as const,
  fs: (fsId: string) => ['fs', fsId] as const,
  lists: (fsId: string) => ['fs', fsId, 'list'] as const,
  list: (fsId: string, path: string) => ['fs', fsId, 'list', path] as const,
  stat: (fsId: string, path: string) => ['fs', fsId, 'stat', path] as const,
  read: (fsId: string, path: string, maxBytes: number) => ['fs', fsId, 'read', path, maxBytes] as const,
}

export const transferKeys = {
  all: ['transfers'] as const,
}

export type ChecksumAlgo = 'md5' | 'sha1' | 'sha256' | 'sha512'

export interface SearchResult {
  entries: FileEntry[]
  truncated: boolean
}

/** The part of an upload already stored server-side (resume), or null when unknown. */
export interface UploadStatus {
  size: number
  exists?: boolean
}

export interface FsSpace {
  total: number
  free: number
  avail: number
}

export type CompareStatus = 'same' | 'different' | 'left-only' | 'right-only' | 'type-mismatch'

export interface CompareItem {
  path: string
  type: FileEntry['type']
  status: CompareStatus
  reason?: string
  left?: FileEntry
  right?: FileEntry
}

export interface CompareResult {
  items: CompareItem[]
  truncated: boolean
  summary: { same: number; different: number; leftOnly: number; rightOnly: number; typeMismatch: number }
}

export interface CompareRequest {
  left: { fsId: string; path: string }
  right: { fsId: string; path: string }
  recursive: boolean
  mode: 'size-mtime' | 'checksum'
  excludes?: string[]
}

/** Extra keys of POST /api/transfers (SPEC §9 files-backend). */
export interface TransferRequestEx extends TransferRequest {
  move?: boolean
  preserve?: boolean
  verify?: boolean
}

export const fsApi = {
  open: (body: FsOpenBody, opts?: RequestOptions) => api.post<FsHandleEx>('/api/fs', body, opts),
  close: (id: string) => api.del<void>(base(id), undefined, { noVaultPrompt: true }),
  /** Metadata of an open handle (SPEC §9 files-backend). */
  info: (id: string) => api.get<FsHandleEx>(base(id)),

  list: (id: string, path: string, signal?: AbortSignal) => api.get<FsListResult>(`${base(id)}/list`, { query: { path }, signal }),
  stat: (id: string, path: string, signal?: AbortSignal) => api.get<FileEntry>(`${base(id)}/stat`, { query: { path }, signal }),
  realpath: async (id: string, path: string): Promise<string> => {
    const r = await api.get<{ path?: string } | string>(`${base(id)}/realpath`, { query: { path } })
    if (typeof r === 'string') return r
    return r?.path || path
  },
  read: (id: string, path: string, maxBytes: number, signal?: AbortSignal) =>
    api.get<FileReadResult>(`${base(id)}/read`, { query: { path, maxBytes }, signal }),

  mkdir: (id: string, path: string, parents = false) => api.post<FileEntry | void>(`${base(id)}/mkdir`, { path, parents }),
  rename: (id: string, from: string, to: string) => api.post<FileEntry | void>(`${base(id)}/rename`, { from, to }),
  remove: (id: string, paths: string[], recursive: boolean) => api.post<void>(`${base(id)}/delete`, { paths, recursive }),
  /** `mode`: permission bits, or an octal / symbolic string ("755", "u+x,go-w", capital X). */
  chmod: (id: string, paths: string[], mode: number | string, recursive: boolean) => api.post<void>(`${base(id)}/chmod`, { paths, mode, recursive }),
  /** uid / gid numbers or owner / group names. */
  chown: (id: string, paths: string[], owner: { uid?: number; gid?: number; owner?: string; group?: string }, recursive: boolean) =>
    api.post<void>(`${base(id)}/chown`, { paths, ...owner, recursive }),
  symlink: (id: string, target: string, link: string) => api.post<FileEntry | void>(`${base(id)}/symlink`, { target, link }),
  touch: (id: string, path: string) => api.post<FileEntry | void>(`${base(id)}/touch`, { path }),
  /** Same-fs copy (into the same folder → "name (copy)"); `overwrite` replaces existing items. */
  copy: (id: string, from: string[], toDir: string, overwrite?: boolean) => api.post<{ entries?: FileEntry[] }>(`${base(id)}/copy`, { from, toDir, overwrite }),
  checksum: async (id: string, path: string, algo: ChecksumAlgo, signal?: AbortSignal): Promise<string> => {
    const r = await api.post<unknown>(`${base(id)}/checksum`, { path, algo }, { signal })
    return pickString(r, ['sum', 'checksum', 'hash', 'digest', 'value', algo]) ?? ''
  },
  /** Name glob / substring search; `content` also matches file contents (grep). */
  search: async (id: string, path: string, pattern: string, maxResults: number, opts: { content?: string; signal?: AbortSignal } = {}): Promise<SearchResult> => {
    const r = await api.post<unknown>(`${base(id)}/search`, { path, pattern, maxResults, content: opts.content || undefined }, { signal: opts.signal })
    if (Array.isArray(r)) return { entries: r as FileEntry[], truncated: r.length >= maxResults }
    const obj = (r ?? {}) as { entries?: FileEntry[]; results?: FileEntry[]; truncated?: boolean }
    const entries = obj.entries ?? obj.results ?? []
    return { entries, truncated: !!obj.truncated || entries.length >= maxResults }
  },
  archive: (id: string, paths: string[], dest: string, format: 'zip' | 'tar.gz') =>
    api.post<FileEntry | void>(`${base(id)}/archive`, { paths, dest, format }),
  extract: (id: string, path: string, destDir: string) => api.post<void>(`${base(id)}/extract`, { path, destDir }),

  /** Size of the partial upload stored for `path` (resume). */
  uploadStatus: async (id: string, path: string): Promise<UploadStatus | null> => {
    try {
      const r = await api.get<unknown>(`${base(id)}/upload`, { query: { path } })
      const size = pickNumber(r, ['size', 'offset'])
      return size == null ? null : { size, exists: !!(r as { exists?: boolean })?.exists }
    } catch (err) {
      if (isApiError(err) && (err.status === 404 || err.status === 405)) return null
      throw err
    }
  },
  /** URL of the upload endpoint for one chunk (the upload engine PUTs raw bytes to it). */
  uploadUrl: (id: string, path: string, offset: number, opts: { mtime?: number; final?: boolean; total?: number } = {}) =>
    apiUrl(`${base(id)}/upload`, { path, offset, mtime: opts.mtime, final: opts.final ? 1 : undefined, total: opts.total }),

  /** The terminal's current folder ("" when unknown) — polling fallback for "Follow terminal folder". */
  cwd: async (id: string, sessionId?: string): Promise<string> => {
    const r = await api.get<{ path?: string }>(`${base(id)}/cwd`, { query: { sessionId } })
    return typeof r?.path === 'string' ? r.path : ''
  },
  space: (id: string, path: string) => api.get<FsSpace>(`${base(id)}/space`, { query: { path } }),
  presign: (id: string, path: string, expiresSec: number) => api.post<{ url: string }>(`${base(id)}/presign`, { path, expiresSec }),
  compare: (req: CompareRequest, signal?: AbortSignal) => api.post<CompareResult>('/api/fs/compare', req, { signal }),

  /** First `maxBytes` of a file (preview): Range request on the download endpoint. */
  head: async (id: string, path: string, maxBytes: number, signal?: AbortSignal): Promise<{ bytes: Uint8Array; total: number | null; truncated: boolean }> => {
    const get = (range: boolean) =>
      api.get<Response>(`${base(id)}/download`, {
        query: { path, inline: 1 },
        headers: range ? { Range: `bytes=0-${Math.max(0, maxBytes - 1)}` } : undefined,
        as: 'response',
        signal,
      })
    let res: Response
    try {
      res = await get(true)
    } catch (err) {
      // A server whose Range support fails: read the start of the whole stream instead (cancelled below).
      if (!isApiError(err) || err.code !== 'network_error') throw err
      res = await get(false)
    }
    // Read at most maxBytes even if the server ignored the Range header.
    const chunks: Uint8Array[] = []
    let got = 0
    const reader = res.body?.getReader()
    if (reader) {
      for (;;) {
        const { done, value } = await reader.read()
        if (done || !value) break
        chunks.push(value)
        got += value.length
        if (got >= maxBytes) {
          void reader.cancel().catch(() => undefined)
          break
        }
      }
    }
    const buf = new Uint8Array(Math.min(got, maxBytes))
    let off = 0
    for (const c of chunks) {
      const n = Math.min(c.length, buf.length - off)
      if (n <= 0) break
      buf.set(c.subarray(0, n), off)
      off += n
    }
    const range = res.headers.get('Content-Range')
    const len = Number(res.headers.get('Content-Length'))
    const total = range && /\/(\d+)$/.test(range) ? Number(range.split('/').pop()) : res.status === 200 && Number.isFinite(len) && len > 0 ? len : null
    return { bytes: buf, total, truncated: total != null ? total > buf.length : got >= maxBytes }
  },

  /** Download URL: one file (Range capable), or a zip of a folder / several paths. */
  downloadUrl: (id: string, paths: string[], opts: { zip?: boolean; inline?: boolean; name?: string } = {}) => {
    if (paths.length === 1 && !opts.zip) return apiUrl(`${base(id)}/download`, { path: paths[0], inline: opts.inline ? 1 : undefined })
    if (paths.length === 1) return apiUrl(`${base(id)}/download`, { path: paths[0], zip: 1, name: opts.name })
    return apiUrl(`${base(id)}/download`, { paths, zip: 1, name: opts.name })
  },
}

export const transfersApi = {
  list: () => api.get<Transfer[]>('/api/transfers'),
  create: (req: TransferRequestEx) => api.post<Transfer>('/api/transfers', req),
  cancel: (id: string) => api.post<void>(`/api/transfers/${seg(id)}/cancel`),
  remove: (id: string) => api.del<void>(`/api/transfers/${seg(id)}`),
}

// ---------------------------------------------------------------------------------------------------------------------
// errors
// ---------------------------------------------------------------------------------------------------------------------

/** The fs handle itself is unknown / closed on the server (not "path not found"): reopen and retry. */
export function isHandleGone(err: unknown): boolean {
  if (!isApiError(err)) return false
  if (err.status === 410) return true
  const code = err.code.toLowerCase()
  if (['fs_not_found', 'fs_closed', 'unknown_fs', 'handle_not_found', 'fs_gone', 'gone'].includes(code)) return true
  if (err.status === 404 && /\b(file ?system|fs handle|handle)\b.*\b(not found|unknown|closed|expired)|\b(unknown|no such) (file ?system|fs|handle)/i.test(err.message)) return true
  return false
}

export function isPermissionDenied(err: unknown): boolean {
  if (!isApiError(err)) return false
  if (err.status === 403 && err.code !== 'csrf') return true
  return /permission denied|access denied|EACCES|EPERM/i.test(err.message)
}

export function isNotFound(err: unknown): boolean {
  return isApiError(err) && err.status === 404 && !isHandleGone(err)
}

export function isConflict(err: unknown): boolean {
  return isApiError(err) && err.status === 409
}

/** The transport of a session-backed fs is not usable (session disconnected / not connected yet). */
export function isDisconnected(err: unknown): boolean {
  if (!isApiError(err)) return false
  return err.code === 'session_not_connected' || err.code === 'disconnected'
}

/** The connection's "SSH-browser type" is None (409 ssh_browser_disabled). */
export function isBrowserDisabled(err: unknown): boolean {
  return isApiError(err) && err.code === 'ssh_browser_disabled'
}

function pickString(r: unknown, keys: string[]): string | null {
  if (typeof r === 'string') return r
  if (r && typeof r === 'object') {
    for (const k of keys) {
      const v = (r as Record<string, unknown>)[k]
      if (typeof v === 'string') return v
    }
  }
  return null
}

function pickNumber(r: unknown, keys: string[]): number | null {
  if (typeof r === 'number' && Number.isFinite(r)) return r
  if (r && typeof r === 'object') {
    for (const k of keys) {
      const v = (r as Record<string, unknown>)[k]
      if (typeof v === 'number' && Number.isFinite(v)) return v
    }
  }
  return null
}
