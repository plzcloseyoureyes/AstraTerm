/*
 * Import/export REST calls. Preview/commit send the file content as base64 (safe for binary, CRLF and legacy code
 * pages — the server decodes the text). Exports and backups are POSTed so a passphrase never appears in a URL, and are
 * saved through a Blob download. Nothing here persists a passphrase.
 */
import { api } from '@/api/client'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type {
  CommitResponse,
  Dedupe,
  DiscoverEntry,
  ExportFormat,
  ImportFormat,
  PreviewOptions,
  PreviewResponse,
  SyncStatus,
} from './types'

/** Base64-encode a UTF-8 string for the `content` field. */
export function toBase64(text: string): string {
  return bytesToBase64(new TextEncoder().encode(text))
}

/** Base64-encode bytes (chunked: large files do not overflow the call stack). */
export function bytesToBase64(bytes: Uint8Array): string {
  let binary = ''
  const chunk = 0x8000
  for (let i = 0; i < bytes.length; i += chunk) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunk))
  }
  return btoa(binary)
}

/** Base64-encode an ArrayBuffer (dropped/binary files). */
export function bufferToBase64(buf: ArrayBuffer): string {
  return bytesToBase64(new Uint8Array(buf))
}

/**
 * Reads one or several dropped files into one base64 payload. Bytes are kept as they are (the server detects UTF-16,
 * UTF-8 or the legacy code page); several files are joined with a line break — e.g. many .remmina profiles — after
 * converting UTF-16 files (BOM) to UTF-8 so the joined text stays in one encoding.
 */
export async function filesToBase64(files: File[]): Promise<string> {
  const buffers = await Promise.all(files.map((f) => f.arrayBuffer()))
  if (buffers.length === 1) return bufferToBase64(buffers[0])
  const parts: Uint8Array[] = []
  const nl = new TextEncoder().encode('\r\n')
  for (const buf of buffers) {
    let bytes = new Uint8Array(buf)
    if (bytes.length >= 2 && ((bytes[0] === 0xff && bytes[1] === 0xfe) || (bytes[0] === 0xfe && bytes[1] === 0xff))) {
      const label = bytes[0] === 0xff ? 'utf-16le' : 'utf-16be'
      bytes = new TextEncoder().encode(new TextDecoder(label).decode(bytes))
    }
    parts.push(bytes, nl)
  }
  const total = parts.reduce((n, p) => n + p.length, 0)
  const joined = new Uint8Array(total)
  let off = 0
  for (const p of parts) {
    joined.set(p, off)
    off += p.length
  }
  return bytesToBase64(joined)
}

export interface SourceRef {
  format: ImportFormat
  /** base64-encoded content, or omitted when reading a discovered file by path. */
  content?: string
  path?: string
  options?: PreviewOptions
}

export function preview(src: SourceRef): Promise<PreviewResponse> {
  return api.post<PreviewResponse>('/api/import/preview', {
    format: src.format,
    content: src.content ?? '',
    base64: src.content !== undefined,
    path: src.path,
    options: src.options,
  })
}

export interface CommitArgs extends SourceRef {
  targetFolderId?: string
  /** Temp ids to import; omitted = everything, [] = nothing (keys / known hosts / snippets only). */
  selectedIds?: string[]
  dedupe?: Dedupe
  importKeys?: boolean
  importKnownHosts?: boolean
}

export async function commit(args: CommitArgs): Promise<CommitResponse> {
  const res = await api.post<CommitResponse>('/api/import/commit', {
    format: args.format,
    content: args.content ?? '',
    base64: args.content !== undefined,
    path: args.path,
    targetFolderId: args.targetFolderId || undefined,
    selectedIds: args.selectedIds,
    dedupe: args.dedupe,
    importKeys: args.importKeys,
    importKnownHosts: args.importKnownHosts,
    options: args.options,
  })
  // Imported connections/folders/keys/snippets change other features' views.
  for (const key of [queryKeys.connections, queryKeys.folders, queryKeys.keys, queryKeys.knownHosts, queryKeys.snippets, queryKeys.identities]) {
    void queryClient.invalidateQueries({ queryKey: key })
  }
  return res
}

export function discover(): Promise<{ supported: boolean; files: DiscoverEntry[] }> {
  return api.get('/api/import/discover')
}

export function syncStatus(): Promise<SyncStatus> {
  return api.get('/api/import/sync')
}

export function setSync(enabled: boolean): Promise<SyncStatus> {
  return api.post('/api/import/sync', { enabled })
}

export interface ExportRequest {
  format: ExportFormat
  includeSecrets?: boolean
  passphrase?: string
  folderId?: string
  connectionIds?: string[]
}

/** Download an export file (POST: the passphrase travels in the body), using the server's filename. */
export async function downloadExport(req: ExportRequest): Promise<void> {
  const res = await api.post<Response>(
    '/api/export',
    {
      format: req.format,
      includeSecrets: !!req.includeSecrets,
      passphrase: req.includeSecrets ? req.passphrase : undefined,
      folderId: req.folderId || undefined,
      connectionIds: req.connectionIds?.length ? req.connectionIds : undefined,
    },
    { as: 'response' },
  )
  await saveResponse(res, `astraterm-export.${req.format === 'ssh_config' ? 'txt' : req.format}`)
}

/** Download an admin backup: a raw database snapshot, or an encrypted archive (optionally with the system key). */
export async function downloadBackup(opts: { includeSystemKey: boolean; passphrase?: string }): Promise<void> {
  const res = await api.post<Response>(
    '/api/admin/backup',
    { includeSystemKey: opts.includeSystemKey, passphrase: opts.passphrase || undefined },
    { as: 'response' },
  )
  await saveResponse(res, opts.passphrase ? 'astraterm-backup.ntbak' : 'astraterm-backup.db')
}

export interface RestoreResult {
  staged: boolean
  stagedDbPath: string
  /** The next start of AstraTerm applies the staged restore. */
  applyOnRestart: boolean
  instructions: string[]
  warnings?: string[]
}

/** POST a base64 backup for a staged restore (applied by the next start). */
export function restore(contentBase64: string, passphrase?: string): Promise<RestoreResult> {
  return api.post('/api/admin/restore', { content: contentBase64, passphrase: passphrase || undefined })
}

export interface RestoreStatus {
  pending: boolean
  stagedAt?: string
  stagedBy?: string
  systemKey: boolean
}

/** A staged restore waiting for the next start. */
export function restoreStatus(): Promise<RestoreStatus> {
  return api.get('/api/admin/restore')
}

/** Discard a staged restore. */
export function discardRestore(): Promise<unknown> {
  return api.del('/api/admin/restore')
}

async function saveResponse(res: Response, fallbackName: string): Promise<void> {
  const blob = await res.blob()
  const name = filenameFromDisposition(res.headers.get('Content-Disposition')) ?? fallbackName
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = name
  a.rel = 'noopener'
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 30_000)
}

function filenameFromDisposition(header: string | null): string | null {
  if (!header) return null
  const m = /filename\*?=(?:UTF-8'')?"?([^";]+)"?/i.exec(header)
  if (!m) return null
  try {
    return decodeURIComponent(m[1])
  } catch {
    return m[1]
  }
}
