/*
 * Where received files go (trzsz `tsz`, ZMODEM `sz`):
 *
 *   FolderTarget     a folder picked with the File System Access API: files stream to disk (createWritable), nested
 *                    folders are created, nothing outside the folder can be reached (components are sanitised by the
 *                    caller and FS Access itself rejects separators). A failed / cancelled transfer removes what it
 *                    created.
 *   DownloadTarget   the browser's download manager: each file is collected in memory and handed over as one
 *                    download when complete (never a partial file). Folder transfers become one ZIP archive.
 *
 * Both take already-sanitised path components (engine/names.ts). No app imports — unit-tested under Node with a fake
 * directory handle.
 */
import { uniqueName } from './names'
import { Crc32, ZipWriter } from './zip'

export interface SaveFile {
  /** Local name of the top-level item this file belongs to (reported back to the remote / the user). */
  readonly localName: string
  write(chunk: Uint8Array): Promise<void>
  close(): Promise<void>
  /** Discard the file (failure / skip). */
  abort(): Promise<void>
}

export interface SaveTarget {
  readonly kind: 'folder' | 'downloads'
  /** Human description of the destination ("Downloads", the folder name). */
  readonly label: string
  /** Reserve a top-level name (file or folder), made unique unless `overwrite`. */
  reserve(name: string, opts: { dir: boolean; overwrite: boolean }): Promise<string>
  /** Create a file; `path[0]` is a reserved top-level name, parents are created. */
  openFile(path: readonly string[], opts?: { size?: number | null; mtimeMs?: number | null }): Promise<SaveFile>
  mkdir(path: readonly string[], opts?: { mtimeMs?: number | null }): Promise<void>
  /** Called once after a successful transfer (e.g. delivers the ZIP of a folder download). */
  finish(): Promise<void>
  /** Remove whatever this transfer created (failure / cancel). Safe to call more than once. */
  abort(): Promise<void>
  /** Top-level names written so far. */
  readonly saved: readonly string[]
}

// ---------------------------------------------------------------------------------------------------------------------
// File System Access folder
// ---------------------------------------------------------------------------------------------------------------------

/** The parts of FileSystemDirectoryHandle / FileSystemFileHandle used here (lets tests pass a fake). */
export interface DirHandleLike {
  readonly kind: 'directory'
  readonly name: string
  getDirectoryHandle(name: string, opts?: { create?: boolean }): Promise<DirHandleLike>
  getFileHandle(name: string, opts?: { create?: boolean }): Promise<FileHandleLike>
  removeEntry(name: string, opts?: { recursive?: boolean }): Promise<void>
  keys(): AsyncIterable<string>
}

export interface FileHandleLike {
  readonly kind: 'file'
  readonly name: string
  createWritable(opts?: { keepExistingData?: boolean }): Promise<WritableLike>
}

export interface WritableLike {
  write(data: Uint8Array): Promise<void>
  close(): Promise<void>
  abort(reason?: unknown): Promise<void>
}

function fsError(err: unknown, what: string): Error {
  const name = (err as { name?: string } | null)?.name
  if (name === 'NotAllowedError' || name === 'SecurityError') return new Error(`No permission to write ${what}`)
  if (name === 'NoModificationAllowedError') return new Error(`${what} is locked or read-only`)
  if (name === 'TypeMismatchError') return new Error(`${what} exists with another type (file vs. folder)`)
  if (name === 'QuotaExceededError') return new Error('The disk is full')
  if (name === 'InvalidModificationError') return new Error(`${what} cannot be replaced`)
  const msg = err instanceof Error ? err.message : String(err)
  return new Error(`Cannot write ${what}: ${msg}`)
}

export class FolderTarget implements SaveTarget {
  readonly kind = 'folder' as const
  private existing: Set<string> | null = null
  private readonly reserved = new Set<string>()
  /** Top-level entries this transfer created (removed again by abort()). */
  private readonly created = new Set<string>()
  private readonly order: string[] = []
  private readonly open = new Set<SaveFile>()
  private readonly root: DirHandleLike

  constructor(root: DirHandleLike) {
    this.root = root
  }

  get label(): string {
    return this.root.name || 'the chosen folder'
  }

  get saved(): readonly string[] {
    return this.order
  }

  private async names(): Promise<Set<string>> {
    if (!this.existing) {
      const s = new Set<string>()
      try {
        for await (const k of this.root.keys()) s.add(k)
      } catch (err) {
        throw fsError(err, this.label)
      }
      this.existing = s
    }
    return this.existing
  }

  async reserve(name: string, opts: { dir: boolean; overwrite: boolean }): Promise<string> {
    const existing = await this.names()
    let local = name
    if (!opts.overwrite || this.reserved.has(name)) {
      local = uniqueName(name, (n) => existing.has(n) || this.reserved.has(n), opts.dir)
    }
    this.reserved.add(local)
    if (!existing.has(local)) this.created.add(local)
    if (!this.order.includes(local)) this.order.push(local)
    return local
  }

  private async dirFor(path: readonly string[]): Promise<DirHandleLike> {
    let dir = this.root
    for (const c of path) {
      try {
        dir = await dir.getDirectoryHandle(c, { create: true })
      } catch (err) {
        throw fsError(err, path.join('/'))
      }
    }
    return dir
  }

  async mkdir(path: readonly string[]): Promise<void> {
    if (path.length) await this.dirFor(path)
  }

  async openFile(path: readonly string[]): Promise<SaveFile> {
    if (!path.length) throw new Error('empty path')
    const parent = await this.dirFor(path.slice(0, -1))
    const name = path[path.length - 1]
    const shown = path.join('/')
    let handle: FileHandleLike
    let w: WritableLike
    try {
      handle = await parent.getFileHandle(name, { create: true })
      w = await handle.createWritable({ keepExistingData: false })
    } catch (err) {
      throw fsError(err, shown)
    }
    const localName = path[0]
    let done = false
    const file: SaveFile = {
      localName,
      write: async (chunk) => {
        try {
          await w.write(chunk)
        } catch (err) {
          throw fsError(err, shown)
        }
      },
      close: async () => {
        if (done) return
        done = true
        this.open.delete(file)
        try {
          await w.close()
        } catch (err) {
          throw fsError(err, shown)
        }
      },
      abort: async () => {
        if (done) return
        done = true
        this.open.delete(file)
        await w.abort().catch(() => undefined)
        // A file that did not exist before this transfer is removed again (single-file downloads).
        if (path.length === 1 && this.created.has(name)) await parent.removeEntry(name).catch(() => undefined)
      },
    }
    this.open.add(file)
    return file
  }

  async finish(): Promise<void> {
    for (const f of Array.from(this.open)) await f.close()
  }

  async abort(): Promise<void> {
    for (const f of Array.from(this.open)) await f.abort()
    for (const name of Array.from(this.created)) {
      await this.root.removeEntry(name, { recursive: true }).catch(() => undefined)
      this.created.delete(name)
    }
    this.order.length = 0
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// Browser downloads
// ---------------------------------------------------------------------------------------------------------------------

/** Received data is folded into a Blob every SEGMENT_BYTES (see DownloadTarget.openFile). */
const SEGMENT_BYTES = 8 * 1024 * 1024

/** Largest single file kept in memory for a browser download. */
export const MAX_DOWNLOAD_BYTES = 2 * 1024 * 1024 * 1024

export type Deliver = (blob: Blob, name: string) => void

export class DownloadTarget implements SaveTarget {
  readonly kind = 'downloads' as const
  readonly label = 'Downloads'
  private readonly reserved = new Set<string>()
  private readonly order: string[] = []
  private zip: ZipWriter | null
  private delivered = false
  private readonly deliver: Deliver
  private readonly archive?: (topNames: readonly string[]) => string

  /**
   * @param deliver hands a finished Blob to the browser (anchor download)
   * @param archive folder transfers: collect everything into one ZIP named `archive(topNames)`
   */
  constructor(deliver: Deliver, archive?: (topNames: readonly string[]) => string) {
    this.deliver = deliver
    this.archive = archive
    this.zip = archive ? new ZipWriter() : null
  }

  get saved(): readonly string[] {
    return this.order
  }

  async reserve(name: string, opts: { dir: boolean; overwrite: boolean }): Promise<string> {
    // The browser renames on disk; only names inside this transfer must be unique (ZIP entries, reported names).
    const local = this.reserved.has(name) ? uniqueName(name, (n) => this.reserved.has(n), opts.dir) : name
    this.reserved.add(local)
    this.order.push(local)
    return local
  }

  async mkdir(path: readonly string[], opts?: { mtimeMs?: number | null }): Promise<void> {
    if (this.zip && path.length) {
      for (let i = 1; i <= path.length; i++) this.zip.addDirectory(path.slice(0, i).join('/'), opts?.mtimeMs ?? undefined)
    }
  }

  async openFile(path: readonly string[], opts?: { size?: number | null; mtimeMs?: number | null }): Promise<SaveFile> {
    if (!path.length) throw new Error('empty path')
    if (opts?.size != null && opts.size > MAX_DOWNLOAD_BYTES) {
      throw new Error('The file is too large for a browser download. Save it to a folder instead.')
    }
    // Received data is folded into Blobs every few MiB: the browser may keep Blob data outside the JS heap (on disk
    // for big downloads), so a large download does not sit in memory as thousands of small arrays.
    let parts: Uint8Array[] = []
    let pending = 0
    const segments: Blob[] = []
    const crc = new Crc32()
    let size = 0
    let done = false
    const zip = this.zip
    const localName = path[0]
    const name = zip ? path.join('/') : path.join('_')
    return {
      localName,
      write: async (chunk) => {
        if (done) return
        size += chunk.length
        if (size > MAX_DOWNLOAD_BYTES) throw new Error('The file is too large for a browser download. Save it to a folder instead.')
        // Always copy: parsers may hand out views of a buffer they reuse for the next chunk (trzsz binary mode does).
        parts.push(chunk.slice())
        if (zip) crc.update(chunk)
        pending += chunk.length
        if (pending >= SEGMENT_BYTES) {
          segments.push(new Blob(parts as Uint8Array<ArrayBuffer>[]))
          parts = []
          pending = 0
        }
      },
      close: async () => {
        if (done) return
        done = true
        const all: (Uint8Array<ArrayBuffer> | Blob)[] = [...segments, ...(parts as Uint8Array<ArrayBuffer>[])]
        if (zip) {
          for (let i = 1; i < path.length; i++) zip.addDirectory(path.slice(0, i).join('/'))
          zip.addFileParts(name, all, crc.digest(), size, opts?.mtimeMs ?? undefined)
        } else {
          this.deliver(new Blob(all, { type: 'application/octet-stream' }), name)
        }
        parts = []
        segments.length = 0
      },
      abort: async () => {
        done = true
        parts = []
        segments.length = 0
      },
    }
  }

  async finish(): Promise<void> {
    if (!this.zip || this.delivered) return
    this.delivered = true
    if (this.zip.count) this.deliver(this.zip.toBlob(), this.archive!(this.order))
    this.zip = null
  }

  async abort(): Promise<void> {
    this.zip = null
    this.delivered = true
  }
}
