/*
 * trzsz (FILE-20) on top of the `trzsz` npm package (1.1.6): its TrzszFilter parses the stream, detects the
 * `::TRZSZ:TRANSFER:` magic of `trz` / `tsz` on the server and runs the protocol (MD5-checked chunks, base64 or
 * escaped binary, tmux-aware, directories). In a browser it insists on the File System Access API with pickers of
 * its own that need a fresh user gesture, so the two entry points that choose files are replaced on the instance:
 *
 *   downloads  the destination comes from NexTerm's UI: a folder (File System Access API, streamed) or the browser's
 *              download manager (per file; folders as one ZIP) — see engine/save.ts
 *   uploads    files come from NexTerm's pickers / drops (any browser, folders included)
 *
 * Everything else (protocol, progress bar in the terminal, Ctrl+C handling, drag-upload typing `trz`) is the
 * library's. Flow control: each chunk waits for the server's acknowledgement; the chunk size is capped at 1 MiB so a
 * chunk (base64 grows it by a third) always fits NexTerm's 8 MiB per-session input queue.
 *
 * Library internals used (checked at construction, pinned version): trzszTransfer, createProgressBar,
 * textProgressBar, uploadFiles* fields, handleTrzsz{Download,Upload}Files. No app imports: unit-tested under Node.
 */
import { TrzszFilter } from 'trzsz'
import { EMPTY, concatBytes, indexOfBytes, latin1 } from './bytes'
import { sanitizeFileName } from './names'
import type { SaveFile, SaveTarget } from './save'

export const TRZSZ_MAGIC = '::TRZSZ:TRANSFER:'
const MAGIC_BYTES = new TextEncoder().encode(TRZSZ_MAGIC)
/** Largest data chunk the browser sends (see header). */
export const TRZSZ_MAX_CHUNK = 1024 * 1024

// ---------------------------------------------------------------------------------------------------------------------
// library internals (runtime shape of trzsz 1.1.6)
// ---------------------------------------------------------------------------------------------------------------------

interface ProgressCallback {
  onNum(num: number): void
  onName(name: string): void
  onSize(size: number): void
  onStep(step: number): void
  onDone(): void
}

interface TextProgressBar extends ProgressCallback {
  setTerminalColumns?(columns: number): void
}

interface FileReaderLike {
  getPathId(): number
  getRelPath(): string[]
  isDir(): boolean
  getSize(): number
  readFile(buf: ArrayBuffer): Promise<Uint8Array>
  closeFile(): void
}

interface FileWriterLike {
  getFileName(): string
  getLocalName(): string
  isDir(): boolean
  writeFile(buf: Uint8Array): Promise<void>
  closeFile(): void
  deleteFile(): Promise<string>
}

type OpenSaveFile = (saveParam: unknown, fileName: string, directory: boolean, overwrite: boolean) => Promise<FileWriterLike>

interface TransferLike {
  sendAction(confirm: boolean, remoteIsWindows: boolean): Promise<void>
  recvConfig(): Promise<Record<string, unknown>>
  recvFiles(saveParam: unknown, open: OpenSaveFile, progress: ProgressCallback | null): Promise<string[]>
  sendFiles(files: FileReaderLike[], progress: ProgressCallback | null): Promise<string[]>
  clientExit(msg: string): Promise<void>
}

interface FilterInternals {
  processServerOutput(output: Uint8Array): void
  isTransferringFiles(): boolean
  stopTransferringFiles(): void
  setTerminalColumns(columns: number): void
  trzszTransfer: TransferLike | null
  textProgressBar: TextProgressBar | null
  createProgressBar(quiet?: boolean, tmuxPaneColumns?: number): void
  uploadFilesList: FileReaderLike[] | null
  uploadFilesResolve: ((v?: unknown) => void) | null
  uploadFilesReject: ((e: unknown) => void) | null
  uploadInterrupting: boolean
  uploadSkipTrzCommand: boolean
  handleTrzszDownloadFiles(version: string, remoteIsWindows: boolean): Promise<void>
  handleTrzszUploadFiles(version: string, directory: boolean, remoteIsWindows: boolean): Promise<void>
}

// ---------------------------------------------------------------------------------------------------------------------
// public types
// ---------------------------------------------------------------------------------------------------------------------

export interface TrzszUploadItem {
  /** Path from the upload root; the first component is the top-level name. */
  relPath: string[]
  isDir: boolean
  blob?: Blob
}

export interface TrzszProgress {
  file: string
  fileIndex: number
  fileCount: number | null
  fileBytes: number
  fileSize: number | null
  bytes: number
}

export interface TrzszEnd {
  ok: boolean
  canceled?: boolean
  error?: string
  /** Top-level names saved / uploaded. */
  names?: string[]
  /** Download destination label. */
  target?: string
  bytes: number
}

export interface TrzszHooks {
  /** Session arbitration: resolve false when another view answers this transfer. */
  claim(direction: 'download' | 'upload', dropped: boolean): Promise<boolean>
  /** Download destination (null = refuse the transfer). Called before anything is confirmed to the server. */
  chooseTarget(): Promise<{ create(opts: { directory: boolean }): SaveTarget } | null>
  /** Files for `trz` / `trz -d` (null or empty = refuse). */
  chooseFiles(opts: { directory: boolean }): Promise<TrzszUploadItem[] | null>
  start(info: { direction: 'download' | 'upload'; directory: boolean }): void
  progress(p: TrzszProgress): void
  end(result: TrzszEnd): void
}

export interface TrzszStageOptions {
  send(data: string | Uint8Array): void
  /** Terminal output produced outside a filter() call (progress bar, final messages). */
  writeAsync(bytes: Uint8Array): void
  columns: number
  hooks: TrzszHooks
  maxChunkBytes?: number
  /** How long a drag upload waits for `trz` to start (ms). */
  dragInitTimeoutMs?: number
  /**
   * After the magic, `trz` / `tsz` wait silently for the answer; text arriving within this window (ms) means the magic
   * was merely printed (`cat` of a log, `grep` output) and nothing is answered. Default 150; 0 disables the check.
   */
  detectGraceMs?: number
  /** How long a confirmed transfer waits for the server's configuration (ms, default 15 s). */
  negotiationTimeoutMs?: number
}

// ---------------------------------------------------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------------------------------------------------

const utf8 = new TextEncoder()

/** The same summary trzsz prints ("Saved 2 files/directories to X\r\n- a\r\n- b"). */
export function formatSaved(names: readonly string[], dest: string): string {
  let msg = `Saved ${names.length} ${names.length > 1 ? 'files/directories' : 'file/directory'}`
  if (dest) msg += ` to ${dest}`
  return [msg, ...names].join('\r\n- ')
}

/**
 * Where to split `data` so that a possibly incomplete magic line at the end is held back until the next chunk (a
 * magic split across two WebSocket frames would otherwise be missed, or its id truncated). Returns data.length when
 * nothing needs holding.
 */
export function trzszHoldIndex(data: Uint8Array): number {
  const from = Math.max(0, data.length - 80)
  const s = latin1(data, from)
  const i = s.lastIndexOf(TRZSZ_MAGIC)
  if (i >= 0 && !/[\r\n]/.test(s.slice(i))) return from + i
  for (let k = Math.min(TRZSZ_MAGIC.length - 1, s.length); k >= 2; k--) {
    if (s.endsWith(TRZSZ_MAGIC.slice(0, k))) return data.length - k
  }
  return data.length
}

/**
 * Whether output contains visible text once escape sequences, control characters and whitespace are removed (a real
 * `trz` / `tsz` prints nothing but cursor control after its magic line).
 */
export function hasVisibleText(bytes: Uint8Array): boolean {
  const s = latin1(bytes, 0)
    // oxlint-disable-next-line no-control-regex
    .replace(/\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?|\x1b[P^_][^\x1b]*(?:\x1b\\)?|\x1b[ -/]*[0-~]/g, '')
    // oxlint-disable-next-line no-control-regex
    .replace(/[\x00-\x20\x7f]/g, '')
  return s.length > 0
}

/** Visible text after the line of the last magic in `data` (a real trz / tsz prints nothing after it). */
export function textAfterMagicLine(data: Uint8Array): boolean {
  const s = latin1(data, 0)
  const at = s.lastIndexOf(TRZSZ_MAGIC)
  if (at < 0) return false
  const nl = s.indexOf('\n', at)
  return nl >= 0 && hasVisibleText(data.subarray(nl + 1))
}

/** Seconds a confirmed transfer waits for trz / tsz to send its configuration (they answer at once). */
const NEGOTIATION_TIMEOUT_MS = 15_000

function withTimeout<T>(p: Promise<T>, ms: number, onTimeout: () => void, message: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined
  return Promise.race([
    p,
    new Promise<T>((_, reject) => {
      timer = setTimeout(() => {
        onTimeout()
        reject(new Error(message))
      }, ms)
    }),
  ]).finally(() => clearTimeout(timer))
}

export function isTrzszStopped(err: unknown): boolean {
  const m = (err as { message?: string } | null)?.message ?? String(err)
  return m === 'Stopped' || /\bStopped\b/.test(m)
}

function errorText(err: unknown): string {
  if (err instanceof Error) return err.message.replace(/^\[TrzszError\]\s*/, '')
  return String(err)
}

/** A Blob as a trzsz file reader. */
class BlobReader implements FileReaderLike {
  private pos = 0
  private blob: Blob | null
  private readonly pathId: number
  private readonly relPath: string[]
  private readonly dir: boolean
  private readonly size: number

  constructor(pathId: number, relPath: string[], blob: Blob | null, dir: boolean) {
    this.pathId = pathId
    this.relPath = relPath
    this.blob = blob
    this.dir = dir
    this.size = blob?.size ?? 0
  }

  getPathId(): number {
    return this.pathId
  }

  getRelPath(): string[] {
    return this.relPath
  }

  isDir(): boolean {
    return this.dir
  }

  getSize(): number {
    return this.size
  }

  async readFile(buf: ArrayBuffer): Promise<Uint8Array> {
    const blob = this.blob
    if (!blob || this.pos >= this.size) return new Uint8Array(0)
    const len = Math.min(buf.byteLength, this.size - this.pos)
    const chunk = new Uint8Array(await blob.slice(this.pos, this.pos + len).arrayBuffer())
    this.pos += chunk.length
    return chunk
  }

  closeFile(): void {
    this.blob = null
  }
}

/**
 * trzsz readers for upload items, ordered like the library's own drop parser (a folder before its contents), with a
 * path id per top-level item. `directory` false keeps files only, flattened to their names (plain `trz`).
 */
export function uploadReaders(items: readonly TrzszUploadItem[], directory: boolean): FileReaderLike[] {
  const clean = items
    .map((it) => ({ ...it, relPath: it.relPath.map((c) => sanitizeFileName(c)).filter(Boolean) }))
    .filter((it) => it.relPath.length > 0 && (it.isDir || it.blob))
  if (!directory) {
    return clean.filter((it) => !it.isDir).map((it, i) => new BlobReader(i, [it.relPath[it.relPath.length - 1]], it.blob ?? null, false))
  }
  // Group by top-level name, add implied parent folders, sort parents first (stable).
  const tops = new Map<string, number>()
  const seen = new Set<string>()
  const out: { id: number; path: string[]; dir: boolean; blob: Blob | null }[] = []
  const add = (path: string[], dir: boolean, blob: Blob | null) => {
    const key = path.join('/')
    if (seen.has(key)) return
    seen.add(key)
    let id = tops.get(path[0])
    if (id === undefined) {
      id = tops.size
      tops.set(path[0], id)
    }
    out.push({ id, path, dir, blob })
  }
  for (const it of clean) {
    for (let i = 1; i < it.relPath.length; i++) add(it.relPath.slice(0, i), true, null)
    add(it.relPath, it.isDir, it.isDir ? null : (it.blob ?? null))
  }
  out.sort((a, b) => a.id - b.id || cmpPath(a.path, b.path))
  return out.map((e) => new BlobReader(e.id, e.path, e.blob, e.dir))
}

/** Pre-order: a folder sorts right before its contents. */
function cmpPath(a: string[], b: string[]): number {
  const n = Math.min(a.length, b.length)
  for (let i = 0; i < n; i++) {
    if (a[i] !== b[i]) return a[i] < b[i] ? -1 : 1
  }
  return a.length - b.length
}

function checkDuplicateNames(files: FileReaderLike[]): void {
  const names = new Set<string>()
  for (const f of files) {
    const p = f.getRelPath().join('/')
    if (names.has(p)) throw new Error(`Duplicate name: ${p}`)
    names.add(p)
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// stage
// ---------------------------------------------------------------------------------------------------------------------

export class TrzszStage {
  private readonly f: FilterInternals
  private readonly opts: TrzszStageOptions
  private collecting: Uint8Array[] | null = null
  private hold: Uint8Array = EMPTY
  private holdTimer: ReturnType<typeof setTimeout> | null = null
  private disposed = false
  /** Output collected while a detected magic is checked for being a false positive (see probe()). */
  private probing: Uint8Array[] | null = null
  /** The last magic seen was followed by visible text in the same chunk (e.g. the shell prompt after `cat`). */
  private magicTrailingText = false

  constructor(opts: TrzszStageOptions) {
    this.opts = opts
    const filter = new TrzszFilter({
      writeToTerminal: (out) => this.write(out),
      sendToServer: (input) => opts.send(input),
      // Node builds of trzsz require these; NexTerm replaces the handlers that would call them.
      chooseSendFiles: async () => undefined,
      chooseSaveDirectory: async () => undefined,
      terminalColumns: Math.max(1, opts.columns || 80),
      isWindowsShell: false,
      maxDataChunkSize: Math.max(1024, Math.min(TRZSZ_MAX_CHUNK, opts.maxChunkBytes ?? TRZSZ_MAX_CHUNK)),
      dragInitTimeout: opts.dragInitTimeoutMs ?? 5000,
    })
    this.f = filter as unknown as FilterInternals
    if (typeof this.f.handleTrzszDownloadFiles !== 'function' || typeof this.f.handleTrzszUploadFiles !== 'function' || typeof this.f.createProgressBar !== 'function') {
      throw new Error('Unsupported trzsz library version')
    }
    this.f.handleTrzszDownloadFiles = (_version, remoteIsWindows) => this.download(remoteIsWindows)
    this.f.handleTrzszUploadFiles = (_version, directory, remoteIsWindows) => this.upload(directory, remoteIsWindows)
  }

  get transferring(): boolean {
    return this.f.isTransferringFiles()
  }

  /** OutputMux: a running transfer owns every byte. */
  get busy(): boolean {
    return this.f.isTransferringFiles()
  }

  /** A drag upload typed `trz` and waits for it. */
  get uploadPending(): boolean {
    return !!this.f.uploadFilesList
  }

  setColumns(cols: number): void {
    if (cols > 0) this.f.setTerminalColumns(cols)
  }

  /** Stop the running transfer (like Ctrl+C). */
  stop(): void {
    this.f.stopTransferringFiles()
  }

  dispose(): void {
    this.disposed = true
    if (this.holdTimer) clearTimeout(this.holdTimer)
    this.holdTimer = null
    this.hold = EMPTY
    try {
      this.f.stopTransferringFiles()
    } catch {
      /* not running */
    }
  }

  private write(out: string | ArrayBuffer | Uint8Array | Blob): void {
    if (this.disposed) return
    if (typeof Blob !== 'undefined' && out instanceof Blob) {
      void out.arrayBuffer().then((b) => this.opts.writeAsync(new Uint8Array(b)))
      return
    }
    const bytes = typeof out === 'string' ? utf8.encode(out) : out instanceof Uint8Array ? out : new Uint8Array(out as ArrayBuffer)
    if (!bytes.length) return
    if (this.collecting) this.collecting.push(bytes)
    else this.opts.writeAsync(bytes)
  }

  private run(data: Uint8Array): Uint8Array {
    if (!data.length) return EMPTY
    const out: Uint8Array[] = []
    this.collecting = out
    try {
      this.f.processServerOutput(data)
    } finally {
      this.collecting = null
    }
    return concatBytes(out)
  }

  /** Feed output; returns the bytes to render now. */
  filter(bytes: Uint8Array): Uint8Array {
    if (this.probing && this.f.isTransferringFiles()) {
      this.probing.push(bytes.slice())
      return EMPTY
    }
    if (this.f.isTransferringFiles()) {
      if (this.hold.length) this.feedHeldToTransfer()
      this.f.processServerOutput(bytes)
      return EMPTY
    }
    let data = bytes
    if (this.hold.length) {
      data = concatBytes([this.hold, bytes])
      this.hold = EMPTY
      if (this.holdTimer) clearTimeout(this.holdTimer)
      this.holdTimer = null
    }
    const cut = trzszHoldIndex(data)
    const now = cut < data.length ? data.subarray(0, cut) : data
    // Fast path: without the magic (and outside a drag upload's typing phase) the library would only echo the bytes.
    const f = this.f
    const idle = !f.uploadFilesList && !f.uploadInterrupting && !f.uploadSkipTrzCommand
    const hasMagic = indexOfBytes(now, MAGIC_BYTES) >= 0
    if (hasMagic) this.magicTrailingText = textAfterMagicLine(now)
    const out = idle && !hasMagic ? now : this.run(now)
    if (cut < data.length) {
      this.hold = data.slice(cut)
      this.holdTimer = setTimeout(() => {
        this.holdTimer = null
        const h = this.hold
        this.hold = EMPTY
        if (!h.length || this.disposed) return
        const o = this.run(h)
        if (o.length) this.opts.writeAsync(o)
      }, 40)
    }
    return out
  }

  /** Bytes were held back but a transfer started meanwhile: they belong to the transfer. */
  private feedHeldToTransfer(): void {
    const h = this.hold
    this.hold = EMPTY
    if (this.holdTimer) clearTimeout(this.holdTimer)
    this.holdTimer = null
    if (h.length) this.f.processServerOutput(h)
  }

  /**
   * False-positive check right after the library saw a magic line: a real `trz` / `tsz` waits silently for our answer,
   * while a magic that was merely printed (`cat`, `grep`, a log viewer) is followed by more text or the shell's
   * prompt. Resolves true for a false positive — the collected output is rendered and nothing is sent to the server.
   */
  private async probe(): Promise<boolean> {
    const grace = this.opts.detectGraceMs ?? 150
    if (grace <= 0) return false
    if (this.magicTrailingText) {
      // The library already rendered that text; only the answer must not happen.
      this.magicTrailingText = false
      return true
    }
    this.probing = []
    await new Promise((r) => setTimeout(r, grace))
    const got = concatBytes(this.probing ?? [])
    this.probing = null
    if (this.disposed) return true
    if (hasVisibleText(got)) {
      this.opts.writeAsync(got)
      return true
    }
    if (got.length) this.f.processServerOutput(got)
    return false
  }

  /**
   * The server's transfer configuration, bounded in time: if the magic was not really from trz / tsz (or the server
   * went away), the terminal must not stay blocked waiting for it.
   */
  private recvConfig(t: TransferLike): Promise<Record<string, unknown>> {
    return withTimeout(t.recvConfig(), this.opts.negotiationTimeoutMs ?? NEGOTIATION_TIMEOUT_MS, () => this.f.stopTransferringFiles(), 'The server did not start the trzsz transfer')
  }

  // --- replaced library handlers ------------------------------------------------------------------------------------

  private progressFor(onStep: (p: TrzszProgress) => void): { cb: ProgressCallback; bytes: () => number } {
    const text = this.f.textProgressBar
    let count: number | null = null
    let index = 0
    let name = ''
    let size: number | null = null
    let step = 0
    let done = 0
    const emit = () => onStep({ file: name, fileIndex: index, fileCount: count, fileBytes: step, fileSize: size, bytes: done + step })
    const cb: ProgressCallback = {
      onNum: (n) => {
        count = n
        text?.onNum(n)
      },
      onName: (n) => {
        name = n
        index++
        size = null
        step = 0
        text?.onName(n)
        emit()
      },
      onSize: (s) => {
        size = s
        step = 0
        text?.onSize(s)
        emit()
      },
      onStep: (s) => {
        step = s
        text?.onStep(s)
        emit()
      },
      onDone: () => {
        done += size ?? step
        step = 0
        text?.onDone()
        emit()
      },
    }
    return { cb, bytes: () => done + step }
  }

  private async download(remoteIsWindows: boolean): Promise<void> {
    const f = this.f
    const t = f.trzszTransfer
    if (!t) return
    const hooks = this.opts.hooks
    if (await this.probe()) return
    if (!(await hooks.claim('download', false))) return
    const choice = await hooks.chooseTarget()
    if (!choice) {
      await t.sendAction(false, remoteIsWindows)
      hooks.end({ ok: false, canceled: true, bytes: 0 })
      return
    }
    let target: SaveTarget | null = null
    let progress: { cb: ProgressCallback; bytes: () => number } | null = null
    const closes: Promise<void>[] = []
    try {
      await t.sendAction(true, remoteIsWindows)
      const config = await this.recvConfig(t)
      const directory = config.directory === true
      target = choice.create({ directory })
      hooks.start({ direction: 'download', directory })
      f.createProgressBar(config.quiet === true, typeof config.tmux_pane_width === 'number' ? config.tmux_pane_width : undefined)
      progress = this.progressFor((p) => hooks.progress(p))
      const tgt = target
      const tops = new Map<number, string>()
      const open: OpenSaveFile = async (_param, fileName, dirMode, overwrite) => {
        if (!dirMode) {
          const local = await tgt.reserve(sanitizeFileName(fileName), { dir: false, overwrite })
          return writerFor(await tgt.openFile([local]), fileName, local, false)
        }
        let meta: { path_id?: unknown; path_name?: unknown; is_dir?: unknown }
        try {
          meta = JSON.parse(fileName)
        } catch {
          throw new Error(`Invalid name: ${fileName.slice(0, 200)}`)
        }
        const names = Array.isArray(meta.path_name) ? meta.path_name.filter((x): x is string => typeof x === 'string') : []
        if (!names.length || typeof meta.path_id !== 'number') throw new Error(`Invalid name: ${fileName.slice(0, 200)}`)
        const comps = names.map((c) => sanitizeFileName(c))
        const isDir = meta.is_dir === true
        let top = tops.get(meta.path_id)
        if (top === undefined) {
          top = await tgt.reserve(comps[0], { dir: isDir || comps.length > 1, overwrite })
          tops.set(meta.path_id, top)
        }
        const full = [top, ...comps.slice(1)]
        if (isDir) {
          await tgt.mkdir(full)
          return dirWriter(names[names.length - 1], top)
        }
        return writerFor(await tgt.openFile(full), names[names.length - 1], top, false)
      }
      const writerFor = (sf: SaveFile, name: string, local: string, dir: boolean): FileWriterLike => {
        let closed = false
        return {
          getFileName: () => name,
          getLocalName: () => local,
          isDir: () => dir,
          writeFile: (buf) => sf.write(buf),
          closeFile: () => {
            if (closed) return
            closed = true
            closes.push(sf.close())
          },
          deleteFile: async () => {
            await sf.abort()
            return local
          },
        }
      }
      const dirWriter = (name: string, local: string): FileWriterLike => ({
        getFileName: () => name,
        getLocalName: () => local,
        isDir: () => true,
        writeFile: async () => undefined,
        closeFile: () => undefined,
        deleteFile: async () => local,
      })
      const localNames = await t.recvFiles(null, open, progress.cb)
      await Promise.all(closes)
      await target.finish()
      await t.clientExit(formatSaved(localNames, target.label))
      hooks.end({ ok: true, names: localNames, target: target.label, bytes: progress.bytes() })
    } catch (err) {
      await Promise.allSettled(closes)
      await target?.abort().catch(() => undefined)
      hooks.end({ ok: false, canceled: isTrzszStopped(err), error: errorText(err), bytes: progress?.bytes() ?? 0 })
      throw err
    }
  }

  private async upload(directory: boolean, remoteIsWindows: boolean): Promise<void> {
    const f = this.f
    const t = f.trzszTransfer
    if (!t) return
    const hooks = this.opts.hooks
    // Files dropped on the terminal (startDropUpload) are consumed right away, before anything async.
    const dropped = f.uploadFilesList
    f.uploadFilesList = null
    if (!dropped && (await this.probe())) return
    if (!(await hooks.claim('upload', !!dropped))) {
      if (dropped) f.uploadFilesReject?.(new Error('The transfer is handled in another window'))
      f.uploadFilesResolve = null
      f.uploadFilesReject = null
      return
    }
    let readers = dropped
    if (!readers) {
      const items = await hooks.chooseFiles({ directory })
      readers = items?.length ? uploadReaders(items, directory) : null
    }
    if (!readers?.length) {
      await t.sendAction(false, remoteIsWindows)
      hooks.end({ ok: false, canceled: true, bytes: 0 })
      return
    }
    let progress: { cb: ProgressCallback; bytes: () => number } | null = null
    try {
      hooks.start({ direction: 'upload', directory })
      await t.sendAction(true, remoteIsWindows)
      const config = await this.recvConfig(t)
      if (config.overwrite === true) checkDuplicateNames(readers)
      f.createProgressBar(config.quiet === true, typeof config.tmux_pane_width === 'number' ? config.tmux_pane_width : undefined)
      progress = this.progressFor((p) => hooks.progress(p))
      const remoteNames = await t.sendFiles(readers, progress.cb)
      await t.clientExit(formatSaved(remoteNames, ''))
      hooks.end({ ok: true, names: remoteNames, bytes: progress.bytes() })
    } catch (err) {
      hooks.end({ ok: false, canceled: isTrzszStopped(err), error: errorText(err), bytes: progress?.bytes() ?? 0 })
      throw err
    }
  }

  /**
   * Upload dropped files with `trz` (like the library's drag upload): interrupt the foreground command with Ctrl+C,
   * type `trz` (`trz -d` with folders) and hand the files to it. Resolves when the upload finished, rejects when trz
   * does not start in time or the upload failed.
   */
  async startDropUpload(items: readonly TrzszUploadItem[]): Promise<void> {
    const f = this.f
    if (f.uploadFilesList || f.isTransferringFiles()) throw new Error('A trzsz transfer is already running')
    const hasDir = items.some((it) => it.isDir || it.relPath.length > 1)
    const readers = uploadReaders(items, hasDir)
    if (!readers.length) throw new Error('No files to upload')
    f.uploadFilesList = readers
    f.uploadInterrupting = true
    this.opts.send('\x03')
    await new Promise((r) => setTimeout(r, 200))
    f.uploadInterrupting = false
    f.uploadSkipTrzCommand = true
    this.opts.send(hasDir ? 'trz -d\r' : 'trz\r')
    const timeout = this.opts.dragInitTimeoutMs ?? 5000
    return new Promise<void>((resolve, reject) => {
      f.uploadFilesResolve = () => resolve()
      f.uploadFilesReject = (e) => reject(e instanceof Error ? e : new Error(String(e)))
      setTimeout(() => {
        if (f.uploadFilesList !== readers) return
        f.uploadFilesList = null
        f.uploadSkipTrzCommand = false
        const rej = f.uploadFilesReject
        f.uploadFilesResolve = null
        f.uploadFilesReject = null
        rej?.(new Error('trz did not start — is trzsz installed on the server?'))
      }, timeout)
    })
  }
}
