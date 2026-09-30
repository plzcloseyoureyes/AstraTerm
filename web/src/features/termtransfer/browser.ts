/*
 * Browser plumbing for in-terminal transfers: the File System Access folder picker and a remembered download folder
 * (IndexedDB, per user), browser downloads, file pickers and OS drops (folders walked with webkitGetAsEntry).
 *
 * Pickers need a user gesture: call pickDirectory / pickFiles synchronously from a click / key handler.
 */
import type { DirHandleLike } from './engine/save'

// ---------------------------------------------------------------------------------------------------------------------
// File System Access API (Chromium; not in the TypeScript DOM lib)
// ---------------------------------------------------------------------------------------------------------------------

type PermissionMode = { mode: 'read' | 'readwrite' }

export interface DirectoryHandle extends FileSystemDirectoryHandle {
  queryPermission?(desc: PermissionMode): Promise<PermissionState>
  requestPermission?(desc: PermissionMode): Promise<PermissionState>
}

type PickerWindow = Window & {
  showDirectoryPicker?: (opts?: { id?: string; mode?: 'read' | 'readwrite'; startIn?: string }) => Promise<DirectoryHandle>
}

/** The folder picker is available (Chromium on a secure context: https or localhost). */
export function hasFolderAccess(win: Window = window): boolean {
  return typeof (win as PickerWindow).showDirectoryPicker === 'function' && win.isSecureContext !== false
}

/** Folder picker (must run inside a user gesture). Resolves null when the user cancels. */
export async function pickDirectory(win: Window = window): Promise<DirectoryHandle | null> {
  const fn = (win as PickerWindow).showDirectoryPicker
  if (typeof fn !== 'function') throw new Error('This browser cannot save into a folder')
  try {
    return await fn.call(win, { id: 'astraterm-terminal-transfer', mode: 'readwrite', startIn: 'downloads' })
  } catch (err) {
    if ((err as { name?: string })?.name === 'AbortError') return null
    throw err
  }
}

export async function folderPermission(h: DirectoryHandle): Promise<PermissionState> {
  try {
    return (await h.queryPermission?.({ mode: 'readwrite' })) ?? 'granted'
  } catch {
    return 'denied'
  }
}

/** Ask for write access again (a remembered handle after a reload is "prompt"); needs a user gesture. */
export async function requestFolderPermission(h: DirectoryHandle): Promise<boolean> {
  try {
    return ((await h.requestPermission?.({ mode: 'readwrite' })) ?? 'granted') === 'granted'
  } catch {
    return false
  }
}

export function asDirLike(h: DirectoryHandle): DirHandleLike {
  return h as unknown as DirHandleLike
}

// --- remembered folder (IndexedDB: handles are structured-cloneable, not JSON) --------------------------------------

const DB = 'astraterm-termtransfer'
const STORE = 'handles'

function openDb(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(DB, 1)
    req.onupgradeneeded = () => req.result.createObjectStore(STORE)
    req.onsuccess = () => resolve(req.result)
    req.onerror = () => reject(req.error ?? new Error('IndexedDB unavailable'))
  })
}

async function idb<T>(mode: IDBTransactionMode, fn: (s: IDBObjectStore) => IDBRequest): Promise<T | undefined> {
  if (typeof indexedDB === 'undefined') return undefined
  const db = await openDb()
  try {
    return await new Promise<T | undefined>((resolve, reject) => {
      const tx = db.transaction(STORE, mode)
      const req = fn(tx.objectStore(STORE))
      req.onsuccess = () => resolve(req.result as T | undefined)
      req.onerror = () => reject(req.error)
    })
  } finally {
    db.close()
  }
}

const folderKey = (userId: string) => `download-folder:${userId || 'default'}`

/** In-memory copy (the handle keeps its granted permission for this page's lifetime). */
const memo = new Map<string, DirectoryHandle | null>()

export async function rememberedFolder(userId: string): Promise<DirectoryHandle | null> {
  const k = folderKey(userId)
  if (memo.has(k)) return memo.get(k) ?? null
  try {
    const h = await idb<DirectoryHandle>('readonly', (s) => s.get(k))
    memo.set(k, h && (h as { kind?: string }).kind === 'directory' ? h : null)
  } catch {
    memo.set(k, null)
  }
  return memo.get(k) ?? null
}

export async function rememberFolder(userId: string, h: DirectoryHandle | null): Promise<void> {
  const k = folderKey(userId)
  memo.set(k, h)
  try {
    await idb('readwrite', (s) => (h ? s.put(h, k) : s.delete(k)))
  } catch {
    /* private mode: the in-memory copy still works for this page */
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// downloads
// ---------------------------------------------------------------------------------------------------------------------

/** Hand a Blob to the browser's download manager (anchor with `download`, in the terminal's own window). */
export function deliverDownload(blob: Blob, name: string, doc: Document = document): void {
  const url = URL.createObjectURL(blob)
  const a = doc.createElement('a')
  a.href = url
  a.download = name
  a.rel = 'noopener'
  a.style.display = 'none'
  doc.body.appendChild(a)
  a.click()
  a.remove()
  // Revoke later: the download manager reads the Blob asynchronously.
  setTimeout(() => URL.revokeObjectURL(url), 120_000)
}

// ---------------------------------------------------------------------------------------------------------------------
// local files
// ---------------------------------------------------------------------------------------------------------------------

/** A local file with its path relative to what the user picked / dropped ("dir/sub/name.txt"). */
interface LocalItem {
  file: File
  relPath: string
}

export interface LocalSelection {
  files: LocalItem[]
  /** Folders (relative paths), including empty ones. */
  dirs: string[]
}

/** File (or folder) picker; must run inside a user gesture. Resolves null when cancelled. */
export function pickFiles(doc: Document = document, opts: { multiple?: boolean; folder?: boolean } = {}): Promise<LocalSelection | null> {
  return new Promise((resolve) => {
    const input = doc.createElement('input')
    input.type = 'file'
    input.multiple = opts.multiple !== false
    if (opts.folder) input.webkitdirectory = true
    input.style.display = 'none'
    let done = false
    const finish = (v: LocalSelection | null) => {
      if (done) return
      done = true
      input.remove()
      resolve(v)
    }
    input.addEventListener('change', () => {
      const sel = selectionFromList(input.files)
      finish(sel.files.length || sel.dirs.length ? sel : null)
    })
    input.addEventListener('cancel', () => finish(null))
    doc.body.appendChild(input)
    input.click()
  })
}

function selectionFromList(list: FileList | readonly File[] | null): LocalSelection {
  const files: LocalItem[] = []
  const dirs = new Set<string>()
  for (const f of Array.from(list ?? [])) {
    const rel = (f.webkitRelativePath || f.name).replace(/^\/+/, '')
    files.push({ file: f, relPath: rel })
    const parts = rel.split('/')
    for (let i = 1; i < parts.length; i++) dirs.add(parts.slice(0, i).join('/'))
  }
  return { files, dirs: [...dirs] }
}

/** True when a drag carries files from the OS (not text / links / dockview tabs). */
export function dragHasFiles(dt: DataTransfer | null): boolean {
  if (!dt) return false
  const types = Array.from(dt.types ?? [])
  return types.includes('Files')
}

/**
 * Snapshot a drop synchronously (a DataTransfer is only readable inside the drop handler) and walk folders
 * afterwards. Works for DataTransfers of pop-out windows too (no instanceof checks).
 */
export function collectDrop(dt: DataTransfer): Promise<LocalSelection> {
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
    const files: LocalItem[] = loose.map((f) => ({ file: f, relPath: f.name }))
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

/** Decode a small file as UTF-8 text; null when it is not text (invalid UTF-8 or NUL bytes). */
export async function readTextFile(file: Blob, maxBytes: number): Promise<string | null> {
  if (file.size > maxBytes) return null
  const bytes = new Uint8Array(await file.arrayBuffer())
  if (bytes.includes(0)) return null
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes).replace(/^﻿/, '')
  } catch {
    return null
  }
}
