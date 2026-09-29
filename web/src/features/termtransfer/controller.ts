/*
 * TransferController — one per terminal view (created by the terminal plugin):
 *
 *   output      one output filter: replayed history passes untouched, then the trzsz stage and the ZMODEM stage
 *               (engine/mux.ts); a transfer answered by another view of the session is hidden here
 *   arbitration exactly one view answers a transfer (engine/lock.ts: active tab > focused window > visible)
 *   prompts     where downloads go / which files to upload — cards inside the terminal pane (Overlay.tsx); buttons
 *               run inside the click / Enter handler so the browser's pickers get their user gesture
 *   keyboard    while a transfer or prompt is up, keys never reach the remote (they would corrupt the protocol):
 *               Ctrl+C cancels, Enter / Escape answer a prompt
 *   drops       OS files dropped on the pane: SFTP upload to the shell's folder (SSH), copy to the host (local
 *               shells), trz, rz or "paste contents"
 *   send file   CC-8: text line by line (server pacer when available) or raw binary chunks
 */
import type { Terminal } from '@xterm/xterm'
import { toast } from 'sonner'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { Connection, ConnectionOptions, RuntimeSession } from '@/api/types'
import { isCommandEnabled, runCommand } from '@/app/commands'
import type { TerminalPluginContext } from '@/app/registry'
import { onTerminalOutput, useTerminalInfoStore } from '@/features/terminal/bus'
import type { TerminalHandle, TerminalPluginContextEx, TerminalTabParams } from '@/features/terminal/types'
import { DELAYED_FLAG_DEFAULTS, DelayedFlag } from '@/lib/useDelayedFlag'
import { errorMessage, formatBytes, plural, uid } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth'
import { getTabParams, useWorkspaceStore } from '@/stores/workspace'
import { DangerousTextError, PacerUnavailableError, SERVER_PACER_MAX, serverPacerAvailable, startPacedSend } from './automation'
import {
  asDirLike,
  collectDrop,
  deliverDownload,
  dragHasFiles,
  folderPermission,
  hasFolderAccess,
  pickDirectory,
  pickFiles,
  readTextFile,
  rememberFolder,
  rememberedFolder,
  requestFolderPermission,
  type DirectoryHandle,
  type LocalSelection,
} from './browser'
import { OutputMux } from './engine/mux'
import { displayRemotePath, quoteShellPath, quoteWindowsPath, sanitizeFileName, uniqueName } from './engine/names'
import {
  NotConnectedError,
  PromptWatcher,
  compilePromptPattern,
  DEFAULT_PROMPT_PATTERN,
  isAbortError,
  sendBinaryPaced,
  sendTextPaced,
  timeoutClock,
  workerClock,
  type Clock,
} from './engine/pacer'
import { DownloadTarget, FolderTarget, type SaveTarget } from './engine/save'
import { TransferArbiter, type LockOwner } from './engine/lock'
import { PROGRESS_PUBLISH_MS, advance, overallFraction } from './engine/progress'
import { TrzszStage, type TrzszEnd, type TrzszProgress, type TrzszUploadItem } from './engine/trzsz'
import {
  isPeerAbort,
  ZmodemStage,
  zmodemErrorMessage,
  zmodemReceive,
  zmodemSend,
  type ZmDetection,
  type ZmReceiveSession,
  type ZmSendSession,
  type ZProgress,
} from './engine/zmodem'
import { askDangerous, getTerm, mountTerm, setDrag, setFollower, setPrompt, setTransfer, unmountTerm } from './store'
import { currentSettings, transferSettings, type TermTransferSettings } from './settings'
import type { DropZone, DropZoneId, PromptAction, PromptView, TransferDirection, TransferProtocol, TransferView } from './types'
import { localFilesAllowed, registerController } from './instances'
import { uploadSelection } from './upload'
import { showUploadToast } from './ui/UploadToast'

// ---------------------------------------------------------------------------------------------------------------------
// shared state
// ---------------------------------------------------------------------------------------------------------------------

const replayFlags = new Map<string, boolean>()
let busListener: (() => void) | null = null
let arbiterInstance: TransferArbiter | null = null

function arbiter(): TransferArbiter {
  arbiterInstance ??= new TransferArbiter()
  return arbiterInstance
}

export interface SaveChoice {
  label: string
  create(opts: { directory: boolean }): SaveTarget
}

export interface SendFileOptions {
  mode: 'text' | 'binary'
  lineDelayMs: number
  charDelayMs: number
  waitPrompt: boolean
  promptPattern: string
  promptTimeoutMs: number
  eol: string
  chunkBytes: number
  delayMs: number
}

const NOT_TEXT = 'The file is not UTF-8 text. Send it in binary mode instead.'

/** Enter answers a transfer prompt only once it has been visible this long (keys typed ahead must not answer it). */
const PROMPT_ENTER_GUARD_MS = 600

/** Shown by the terminal while this feature holds its input lock (input from other sources is held back). */
const INPUT_LOCK_REASON = 'File transfer in progress'

function timestampName(): string {
  const d = new Date()
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}`
}

// ---------------------------------------------------------------------------------------------------------------------
// controller
// ---------------------------------------------------------------------------------------------------------------------

export class TransferController {
  readonly tabId: string
  readonly sessionId: string
  readonly term: Terminal
  private readonly ctx: TerminalPluginContext & TerminalPluginContextEx
  private readonly handle: TerminalHandle
  private readonly owner: LockOwner
  private readonly mux = new OutputMux()
  private readonly layer: HTMLElement
  private readonly dropRoot: HTMLElement
  private readonly cleanups: (() => void)[] = []
  private readonly watcher = new PromptWatcher()
  private trzszStage: TrzszStage | null = null
  private trzszUnavailable = false
  private zmodemStage: ZmodemStage | null = null
  private disposed = false

  /** ZMODEM detection waiting for its short grace period / an answer. */
  private detection: { d: ZmDetection; role: 'receive' | 'send'; timer: ReturnType<typeof setTimeout> | null } | null = null
  private detectionPrompt: { role: 'receive' | 'send'; close: () => void; retractTimer: ReturnType<typeof setTimeout> | null } | null = null
  /** Files chosen before the remote was asked to receive them (drop / menu → we typed `rz`). */
  private pendingRz: { selection: LocalSelection; timer: ReturnType<typeof setTimeout> } | null = null
  /** A drop just happened here: this view should win the arbitration for the transfer it starts. */
  private dropBoostUntil = 0
  /** The running in-band transfer. */
  private active: { protocol: TransferProtocol; cancel: () => void } | null = null
  private view: TransferView | null = null
  private viewTimer: ReturnType<typeof setTimeout> | null = null
  private hideTimer: ReturnType<typeof setTimeout> | null = null
  /** The card of a running transfer appears after 300 ms and then stays at least 400 ms (docs/UX.md). */
  private readonly reveal = new DelayedFlag((shown) => {
    if (!shown) return
    this.revealedAt = Date.now()
    if (this.view?.phase === 'running' && !this.disposed) setTransfer(this.tabId, this.view)
  })
  private revealedAt = 0
  private finishTimer: ReturnType<typeof setTimeout> | null = null
  private pendingFinish: (() => void) | null = null
  private lastRateAt = 0
  private lastRateDone = 0
  private stdinGuard = false
  /** Releases the terminal's input lock; set while a transfer runs or a transfer question is up. */
  private releaseInputLock: (() => void) | null = null
  private promptShownAt = 0
  private dragTimer: ReturnType<typeof setInterval> | null = null
  private lastDragAt = 0
  private clock: Clock | null = null
  /** Another view answered the current transfer (output hidden here). */
  private following = false
  /** Protocol error that made the ZMODEM stage abort the session (reported instead of "the server canceled"). */
  private zmodemError: unknown = null

  constructor(term: Terminal, ctx: TerminalPluginContext) {
    this.term = term
    this.ctx = ctx as TerminalPluginContext & TerminalPluginContextEx
    this.handle = this.ctx.handle
    this.tabId = ctx.tabId
    this.sessionId = ctx.sessionId
    const doc = term.element?.ownerDocument ?? document
    const host = term.element?.parentElement ?? null
    this.dropRoot = host?.parentElement ?? host ?? term.element ?? doc.body
    this.layer = doc.createElement('div')
    this.layer.className = 'nx-tt-layer'
    this.layer.setAttribute('data-termtransfer', '')
    Object.assign(this.layer.style, { position: 'absolute', inset: '0', zIndex: '30', pointerEvents: 'none', overflow: 'hidden' })
    this.dropRoot.appendChild(this.layer)

    this.owner = {
      id: `${this.tabId}:${uid('v')}`,
      sessionId: this.sessionId,
      priority: () => this.priority(),
    }

    this.cleanups.push(registerController(this))
    mountTerm(this.tabId, this.sessionId, this.layer)
    busListener ??= onTerminalOutput((e) => replayFlags.set(e.tabId, e.replay))
    this.cleanups.push(arbiter().register(this.owner))
    this.cleanups.push(arbiter().subscribe(() => this.syncFollower()))
    this.cleanups.push(this.ctx.addOutputFilter((b) => this.filter(b)))
    this.cleanups.push(this.ctx.onOutput((b) => this.watcher.feed(b)))
    const resize = term.onResize(({ cols }) => this.trzszStage?.setColumns(cols))
    this.cleanups.push(() => resize.dispose())
    this.installKeyGuard()
    this.installDrop()
    this.cleanups.push(
      useTerminalInfoStore.subscribe((s, prev) => {
        const info = s.infos[this.tabId]
        const before = prev.infos[this.tabId]
        if (!info || info === before) return
        const lost = (info.state !== 'connected' || info.transport === 'closed') && before?.state === 'connected'
        if (!lost) return
        if (this.active) this.active.cancel()
        // A question about a transfer the server can no longer run must not keep the terminal's keys blocked.
        const prompt = getTerm(this.tabId)?.prompt
        if (prompt && (prompt.kind === 'download' || prompt.kind === 'upload')) prompt.cancel()
      }),
    )
  }

  /** Arbitration priority: a view the user just dropped on > the active tab > a focused window > a visible pane. */
  private priority(): number {
    const d = this.layer.ownerDocument
    let p = 0
    if (Date.now() < this.dropBoostUntil || this.pendingRz) p += 8
    if (useWorkspaceStore.getState().activeTabId === this.tabId) p += 4
    if (d.hasFocus?.()) p += 2
    if (d.visibilityState === 'visible' && this.layer.offsetWidth > 0) p += 1
    return p
  }

  dispose(): void {
    if (this.disposed) return
    this.disposed = true
    try {
      this.active?.cancel()
    } catch {
      /* ignore */
    }
    this.active = null
    if (this.detection?.timer) clearTimeout(this.detection.timer)
    this.detection = null
    this.detectionPrompt?.close()
    if (this.pendingRz) clearTimeout(this.pendingRz.timer)
    this.pendingRz = null
    const z = this.zmodemStage?.session
    if (z && !z.has_ended()) {
      try {
        z.abort()
      } catch {
        /* ended */
      }
    }
    this.trzszStage?.dispose()
    this.zmodemStage?.dispose()
    this.mux.dispose()
    for (const t of [this.viewTimer, this.hideTimer, this.finishTimer]) if (t) clearTimeout(t)
    this.reveal.cancel()
    if (this.dragTimer) clearInterval(this.dragTimer)
    for (const c of this.cleanups.splice(0)) {
      try {
        c()
      } catch (err) {
        console.error('[termtransfer] cleanup failed', err)
      }
    }
    arbiter().release(this.owner)
    this.setStdinGuard(false)
    this.setInputLock(false)
    replayFlags.delete(this.tabId)
    unmountTerm(this.tabId)
    this.layer.remove()
  }

  // ===================================================================================================================
  // context helpers
  // ===================================================================================================================

  private win(): Window {
    return this.layer.ownerDocument.defaultView ?? window
  }

  private doc(): Document {
    return this.layer.ownerDocument
  }

  session(): RuntimeSession | undefined {
    return this.ctx.session()
  }

  get protocol(): string | undefined {
    return this.handle.info().protocol ?? this.session()?.protocol
  }

  get title(): string {
    return this.handle.info().title || this.session()?.title || 'Terminal'
  }

  /** The saved connection (or quick-connect spec) behind this terminal. */
  private connectionOptions(): ConnectionOptions | undefined {
    const params = getTabParams<TerminalTabParams>(this.tabId)
    const id = params?.connectionId ?? this.session()?.connectionId
    if (id) {
      const one = queryClient.getQueryData<Connection>(queryKeys.connection(id))
      const c = one ?? queryClient.getQueryData<Connection[]>(queryKeys.connections)?.find((x) => x.id === id)
      if (c) return c.options
    }
    return params?.quick?.options
  }

  /** Baud rate of a serial session (paced binary sends match it), else null. */
  serialBaud(): number | null {
    if (this.protocol !== 'serial') return null
    const b = Number(this.connectionOptions()?.baud ?? 9600)
    return Number.isFinite(b) && b > 0 ? b : null
  }

  /** Why raw bytes cannot cross this session unchanged (character set conversion), or null. */
  binaryProblem(): string | null {
    const enc = this.connectionOptions()?.encoding
    if (typeof enc === 'string' && enc && !/^(utf-?8|unicode-1-1-utf-8)$/i.test(enc.trim())) {
      return `This session converts its output from ${enc.toUpperCase()} to UTF-8, which corrupts binary transfers. Use a UTF-8 session (encoding setting) for ZMODEM, or trzsz in its default (base64) mode.`
    }
    return null
  }

  /** Input rewriting that corrupts binary uploads (Backspace sent as Ctrl+H rewrites DEL bytes). */
  private uploadWarning(): string | undefined {
    if (this.connectionOptions()?.backspace === 'ctrl-h') {
      return 'This session sends Backspace as Ctrl+H: DEL bytes in uploads are rewritten. Switch the Backspace option back to DEL for binary uploads.'
    }
    return undefined
  }

  isConnected(): boolean {
    const i = this.handle.info()
    return i.state === 'connected' && i.transport === 'open'
  }

  get readOnly(): boolean {
    return this.handle.info().readOnly
  }

  /** A transfer, a prompt or a detection keeps this terminal busy (keys go nowhere). */
  get busy(): boolean {
    return !!this.active || !!getTerm(this.tabId)?.prompt || !!this.detection
  }

  // ===================================================================================================================
  // output
  // ===================================================================================================================

  private trzsz(): TrzszStage | null {
    if (!this.trzszStage && !this.trzszUnavailable) {
      try {
        this.trzszStage = new TrzszStage({
          send: (d) => {
            this.ctx.sendRaw(d)
          },
          writeAsync: (b) => {
            if (!this.disposed) this.ctx.writeLocal(b)
          },
          columns: this.term.cols,
          hooks: {
            claim: (_dir, dropped) => this.claimForTrzsz(dropped),
            chooseTarget: () => this.chooseDownloadSafe('trzsz'),
            chooseFiles: ({ directory }) => this.chooseTrzszFiles(directory),
            start: ({ direction, directory }) => this.trzszStarted(direction, directory),
            progress: (p) => this.trzszProgress(p),
            end: (r) => this.trzszEnded(r),
          },
        })
      } catch (err) {
        console.error('[termtransfer] trzsz unavailable', err)
        this.trzszUnavailable = true
        return null
      }
    }
    return this.trzszStage
  }

  private zmodem(): ZmodemStage {
    this.zmodemStage ??= new ZmodemStage({
      send: (b) => {
        this.ctx.sendRaw(b)
      },
      writeAsync: (b) => {
        if (!this.disposed) this.ctx.writeLocal(b)
      },
      detect: (d) => this.onZmodemDetect(d),
      retract: () => this.onZmodemRetract(),
      failed: (err) => this.onZmodemFailed(err),
    })
    return this.zmodemStage
  }

  /** Follow the arbiter: while another view transfers, hide the stream; afterwards show the shell's lines. */
  private syncFollower(): boolean {
    const f = !this.active && arbiter().heldElsewhere(this.owner)
    if (this.following && !f) {
      const tail = this.mux.takeFollowerTail()
      if (tail.length && !this.disposed) this.ctx.writeLocal(tail)
    }
    this.following = f
    setFollower(this.tabId, f)
    return f
  }

  private filter(bytes: Uint8Array): Uint8Array | null {
    if (this.disposed) return bytes
    const replay = replayFlags.get(this.tabId) ?? this.ctx.isReplaying()
    const follower = this.syncFollower()
    if (this.readOnly) return this.mux.filter(bytes, { replay, follower }, [])
    const s = currentSettings()
    const tz = this.trzszStage?.busy ? this.trzszStage : s.trzsz ? this.trzsz() : null
    const zm = this.zmodemStage?.busy ? this.zmodemStage : s.zmodem ? this.zmodem() : null
    try {
      return this.mux.filter(bytes, { replay, follower }, [tz, zm])
    } catch (err) {
      console.error('[termtransfer] output filter failed', err)
      return bytes
    }
  }

  // ===================================================================================================================
  // keyboard guard
  // ===================================================================================================================

  private installKeyGuard(): void {
    const el = this.term.element
    if (!el) return
    const onKey = (e: KeyboardEvent) => {
      if (!this.busy || e.isComposing) return
      const prompt = getTerm(this.tabId)?.prompt
      const ctrlC = e.ctrlKey && !e.altKey && !e.metaKey && !e.shiftKey && (e.key === 'c' || e.key === 'C')
      if (ctrlC || (e.key === 'Escape' && prompt)) {
        if (prompt) prompt.cancel()
        else this.cancelActive()
      } else if (e.key === 'Enter' && prompt) {
        // Type-ahead protection: an Enter meant for the shell right as the card appears must not answer it.
        if (Date.now() - this.promptShownAt >= PROMPT_ENTER_GUARD_MS && !e.repeat) {
          const a = prompt.actions[prompt.primary]
          a?.run({ remember: !!prompt.remember })
        }
      } else if (['Shift', 'Control', 'Alt', 'Meta'].includes(e.key) || e.metaKey) {
        return // let modifier presses and app shortcuts ($mod+…) through
      }
      e.preventDefault()
      e.stopPropagation()
    }
    el.addEventListener('keydown', onKey, true)
    this.cleanups.push(() => el.removeEventListener('keydown', onKey, true))
  }

  /** While busy, xterm itself must not produce input either (IME, paste, automatic replies). */
  private setStdinGuard(on: boolean): void {
    if (this.stdinGuard === on) return
    this.stdinGuard = on
    try {
      this.term.options.disableStdin = on ? true : this.handle.info().readOnly
    } catch {
      /* disposed */
    }
  }

  /**
   * The terminal's input lock: input from other sources (broadcast, snippets, automation, pastes) is held back while a
   * transfer owns the session's input stream; the protocol bytes themselves go out through ctx.sendRaw.
   */
  private setInputLock(on: boolean): void {
    if (on === (this.releaseInputLock !== null)) return
    if (on) {
      this.releaseInputLock = this.ctx.acquireInputLock(INPUT_LOCK_REASON)
    } else {
      const release = this.releaseInputLock
      this.releaseInputLock = null
      release?.()
    }
  }

  private syncGuard(): void {
    if (this.disposed) return
    this.setStdinGuard(this.busy)
    // A ZMODEM detection still in its 150 ms grace period is often retracted (text that merely contains a header):
    // keys are held meanwhile, but the lock (and the terminal's badge) waits for a real transfer or question.
    this.setInputLock(!!this.active || !!getTerm(this.tabId)?.prompt)
  }

  /** Cancel the running in-band transfer (Ctrl+C, card button). */
  cancelActive(): void {
    this.active?.cancel()
  }

  // ===================================================================================================================
  // transfer card
  // ===================================================================================================================

  private begin(protocol: TransferProtocol, direction: TransferDirection, opts: { title: string; cancel: () => void; total?: number | null; unit?: 'bytes' | 'lines'; target?: string }): void {
    this.flushFinish()
    if (this.hideTimer) clearTimeout(this.hideTimer)
    this.hideTimer = null
    this.active = { protocol, cancel: opts.cancel }
    this.lastRateAt = Date.now()
    this.lastRateDone = 0
    this.view = {
      id: uid('tt'),
      protocol,
      direction,
      phase: 'running',
      title: opts.title,
      unit: opts.unit ?? 'bytes',
      done: 0,
      total: opts.total ?? null,
      pct: opts.total === 0 ? 1 : null,
      rate: 0,
      startedAt: Date.now(),
      target: opts.target,
      canCancel: true,
    }
    // A short transfer shows no progress card at all (only its result); a card still in its minimum-visible time from
    // the previous transfer continues with this one.
    this.reveal.set(true)
    if (this.reveal.shown) setTransfer(this.tabId, this.view)
    this.following = false
    this.mux.takeFollowerTail()
    setFollower(this.tabId, false)
    this.syncGuard()
  }

  private progress(patch: Partial<TransferView>): void {
    const v = this.view
    if (!v || v.phase !== 'running') return
    const next: TransferView = { ...v, ...patch }
    // Monotonic: a ZMODEM rewind (ZRPOS) or a retried chunk never moves the bar backwards.
    next.done = Math.max(v.done, next.done)
    next.pct = advance(v.pct, overallFraction(next))
    const now = Date.now()
    const dt = now - this.lastRateAt
    if (dt >= 400) {
      const inst = ((next.done - this.lastRateDone) * 1000) / dt
      next.rate = v.rate ? v.rate * 0.65 + inst * 0.35 : inst
      this.lastRateAt = now
      this.lastRateDone = next.done
    }
    this.view = next
    if (!this.viewTimer && this.reveal.shown) {
      this.viewTimer = setTimeout(() => {
        this.viewTimer = null
        if (this.view && !this.disposed && this.reveal.shown) setTransfer(this.tabId, this.view)
      }, PROGRESS_PUBLISH_MS)
    }
  }

  /** Show a finished card that is still waiting for the progress card's minimum-visible time. */
  private flushFinish(): void {
    if (this.finishTimer) clearTimeout(this.finishTimer)
    this.finishTimer = null
    const f = this.pendingFinish
    this.pendingFinish = null
    f?.()
  }

  private finish(phase: 'done' | 'error' | 'canceled', message: string): void {
    if (this.viewTimer) clearTimeout(this.viewTimer)
    this.viewTimer = null
    this.active = null
    const v = this.view
    if (v) {
      const title = phase === 'done' ? v.title.replace(/^Receiving/, 'Received').replace(/^Sending/, 'Sent') : v.title
      const view: TransferView = { ...v, title, phase, message, finishedAt: Date.now(), canCancel: false }
      this.view = view
      const hold = this.revealHoldMs()
      this.reveal.set(false)
      const show = () => {
        this.finishTimer = null
        this.pendingFinish = null
        if (this.disposed || this.view !== view) return
        setTransfer(this.tabId, view)
        if (this.hideTimer) clearTimeout(this.hideTimer)
        this.hideTimer = setTimeout(
          () => {
            this.hideTimer = null
            if (this.view === view) {
              this.view = null
              setTransfer(this.tabId, undefined)
            }
          },
          phase === 'error' ? 15_000 : 6_000,
        )
      }
      if (hold > 0) {
        // The progress card appeared only just now: let it complete instead of flashing to the result.
        if (phase === 'done') setTransfer(this.tabId, { ...v, pct: 1, done: v.total ?? v.done })
        this.pendingFinish = show
        this.finishTimer = setTimeout(show, hold)
      } else show()
    }
    this.syncGuard()
    if (this.layer.ownerDocument.activeElement?.closest?.('[data-termtransfer]')) this.handle.focus()
  }

  /** How much longer a shown progress card must stay (its minimum visible time); 0 when it is hidden. */
  private revealHoldMs(): number {
    return this.reveal.shown ? Math.max(0, DELAYED_FLAG_DEFAULTS.minVisible - (Date.now() - this.revealedAt)) : 0
  }

  dismissCard(): void {
    if (this.view && this.view.phase !== 'running') {
      this.view = null
      setTransfer(this.tabId, undefined)
    }
  }

  /**
   * A one-line summary in the terminal (local only: not sent to the session, not in its log). Only at the start of a
   * line: when the shell already redrew its prompt, the card alone reports the result.
   */
  private notice(text: string, tone: 'ok' | 'error' | 'info'): void {
    if (this.disposed) return
    const color = tone === 'error' ? '31' : tone === 'ok' ? '32' : '2'
    try {
      if (this.term.buffer.active.cursorX > 0) return
    } catch {
      return
    }
    this.ctx.writeLocal(`\x1b[${color}m${text.replace(/[\r\n]+/g, ' ')}\x1b[0m\r\n`)
  }

  /** Hide the sender's in-flight data after a cancel / failure; show the shell's lines that follow it. */
  private drain(): void {
    this.mux.drain(400, 3000, (tail) => {
      if (!this.disposed) this.ctx.writeLocal(tail)
    })
  }

  // ===================================================================================================================
  // prompts
  // ===================================================================================================================

  private showPrompt(p: Omit<PromptView, 'id'>): () => void {
    const view: PromptView = { ...p, id: uid('pr') }
    this.promptShownAt = Date.now()
    setPrompt(this.tabId, view)
    this.syncGuard()
    return () => {
      if (getTerm(this.tabId)?.prompt?.id === view.id) setPrompt(this.tabId, undefined)
      this.syncGuard()
      if (this.layer.ownerDocument.activeElement?.closest?.('[data-termtransfer]')) this.handle.focus()
    }
  }

  private userId(): string {
    return useAuthStore.getState().user?.id ?? ''
  }

  private downloadsChoice(): SaveChoice {
    const doc = this.doc()
    return {
      label: 'Downloads',
      create: ({ directory }) =>
        new DownloadTarget(
          (blob, name) => deliverDownload(blob, name, doc),
          directory ? (tops) => `${sanitizeFileName(tops.length === 1 ? tops[0] : `transfer-${timestampName()}`)}.zip` : undefined,
        ),
    }
  }

  private folderChoice(h: DirectoryHandle): SaveChoice {
    return { label: h.name || 'folder', create: () => new FolderTarget(asDirLike(h)) }
  }

  /**
   * Decide where downloaded files go: the settings decide silently when possible (downloads, or a remembered folder
   * whose permission is still granted); otherwise the prompt card asks.
   */
  private async chooseDownload(protocol: 'zmodem' | 'trzsz', opts: { auto: boolean }): Promise<SaveChoice | null> {
    const s = currentSettings()
    const win = this.win()
    const folderOk = hasFolderAccess(win)
    const userId = this.userId()
    const remembered = folderOk ? await rememberedFolder(userId) : null
    if (opts.auto) {
      if (s.downloadTarget === 'downloads' || (s.downloadTarget === 'folder' && !folderOk)) return this.downloadsChoice()
      if (s.downloadTarget === 'folder' && remembered && (await folderPermission(remembered)) === 'granted') return this.folderChoice(remembered)
    }
    const name = protocol === 'zmodem' ? 'ZMODEM' : 'trzsz'
    return new Promise<SaveChoice | null>((resolve) => {
      let done = false
      let close = () => undefined as void
      const settle = (v: SaveChoice | null) => {
        if (done) return
        done = true
        close()
        resolve(v)
      }
      const actions: PromptAction[] = []
      if (remembered && folderOk) {
        actions.push({
          id: 'remembered',
          label: `Save to “${remembered.name}”`,
          run: ({ remember }) => {
            void requestFolderPermission(remembered).then((ok) => {
              if (!ok) {
                toast.error(`No permission to write to “${remembered.name}”`)
                return
              }
              if (remember) transferSettings.set({ downloadTarget: 'folder' })
              settle(this.folderChoice(remembered))
            })
          },
        })
      }
      if (folderOk) {
        actions.push({
          id: 'folder',
          label: remembered ? 'Other folder…' : 'Choose folder…',
          run: ({ remember }) => {
            pickDirectory(win)
              .then(async (h) => {
                if (!h) return
                await rememberFolder(userId, h)
                if (remember) transferSettings.set({ downloadTarget: 'folder' })
                settle(this.folderChoice(h))
              })
              .catch((err) => toast.error('Cannot open the folder', { description: errorMessage(err) }))
          },
        })
      }
      actions.push({
        id: 'downloads',
        label: 'Download',
        run: ({ remember }) => {
          if (remember) transferSettings.set({ downloadTarget: 'downloads' })
          settle(this.downloadsChoice())
        },
      })
      actions.push({ id: 'cancel', label: 'Cancel', variant: 'ghost', run: () => settle(null) })
      close = this.showPrompt({
        kind: 'download',
        title: `Receive files (${name})`,
        message: folderOk
          ? 'The server is sending files. Save them into a folder (streamed to disk) or through the browser’s downloads.'
          : 'The server is sending files. They are saved through the browser’s downloads.',
        note: protocol === 'zmodem' ? this.binaryProblem() ?? undefined : undefined,
        actions,
        primary: actions.findIndex((a) => a.id === (remembered && folderOk ? 'remembered' : 'downloads')),
        cancel: () => settle(null),
        rememberLabel: 'Always do this',
        remember: false,
      })
      if (protocol === 'zmodem') {
        this.detectionPrompt = { role: 'receive', close: () => settle(null), retractTimer: null }
      }
    })
  }

  private async chooseDownloadSafe(protocol: 'zmodem' | 'trzsz'): Promise<SaveChoice | null> {
    try {
      return await this.chooseDownload(protocol, { auto: true })
    } catch (err) {
      toast.error('Cannot start the download', { description: errorMessage(err) })
      return null
    }
  }

  /** Files for an upload the server waits for (rz / trz / trz -d). */
  private chooseUpload(protocol: 'zmodem' | 'trzsz', directory: boolean): Promise<LocalSelection | null> {
    const doc = this.doc()
    const name = protocol === 'zmodem' ? 'rz' : directory ? 'trz -d' : 'trz'
    return new Promise<LocalSelection | null>((resolve) => {
      let done = false
      let close = () => undefined as void
      const settle = (v: LocalSelection | null) => {
        if (done) return
        done = true
        close()
        resolve(v)
      }
      const pick = (folder: boolean) => {
        pickFiles(doc, { multiple: true, folder })
          .then((sel) => {
            if (sel) settle(sel)
          })
          .catch((err) => toast.error('Cannot open the file picker', { description: errorMessage(err) }))
      }
      const actions: PromptAction[] = [{ id: 'files', label: 'Choose files…', run: () => pick(false) }]
      if (directory) actions.push({ id: 'folder', label: 'Choose folder…', run: () => pick(true) })
      actions.push({ id: 'cancel', label: 'Cancel', variant: 'ghost', run: () => settle(null) })
      close = this.showPrompt({
        kind: 'upload',
        title: `Upload files (${name})`,
        message: directory ? 'The server is ready to receive files and folders. Choose them or drop them here.' : 'The server is ready to receive files. Choose them or drop them here.',
        note: protocol === 'zmodem' ? (this.binaryProblem() ?? this.uploadWarning()) : undefined,
        actions,
        primary: 0,
        cancel: () => settle(null),
        acceptDrop: (dt) => {
          void collectDrop(dt).then((sel) => {
            if (sel.files.length || sel.dirs.length) settle(sel)
          })
        },
      })
      if (protocol === 'zmodem') this.detectionPrompt = { role: 'send', close: () => settle(null), retractTimer: null }
    })
  }

  // ===================================================================================================================
  // ZMODEM
  // ===================================================================================================================

  private onZmodemDetect(d: ZmDetection): void {
    const role = d.get_session_role()
    if (this.detectionPrompt && this.detectionPrompt.role === role) {
      // The sender re-announced the session while we ask: keep the prompt, answer the new detection.
      if (this.detectionPrompt.retractTimer) clearTimeout(this.detectionPrompt.retractTimer)
      this.detectionPrompt.retractTimer = null
      if (this.detection?.timer) clearTimeout(this.detection.timer)
      this.detection = { d, role, timer: null }
      return
    }
    if (this.detection?.timer) clearTimeout(this.detection.timer)
    // A short grace period: text that merely contains a ZMODEM header is followed by more text (retract).
    this.detection = { d, role, timer: setTimeout(() => void this.startZmodem(), 150) }
    this.syncGuard()
  }

  private onZmodemRetract(): void {
    if (this.detection?.timer) {
      clearTimeout(this.detection.timer)
      this.detection = null
      this.syncGuard()
      return
    }
    const p = this.detectionPrompt
    if (p && !p.retractTimer) {
      // Either replaced right away by a new header (resend), or the sender gave up.
      p.retractTimer = setTimeout(() => {
        if (this.detectionPrompt !== p) return
        const d = this.detection?.d
        if (d && d.is_valid()) return
        this.detectionPrompt = null
        this.detection = null
        p.close()
        toast.info('The ZMODEM transfer was abandoned by the server')
      }, 600)
    }
  }

  private onZmodemFailed(err: unknown): void {
    console.warn('[termtransfer] ZMODEM protocol error', err)
    this.zmodemError = err
    this.drain()
  }

  private async startZmodem(): Promise<void> {
    const det = this.detection
    if (!det) return
    det.timer = null
    const valid = () => this.detection?.d.is_valid() ?? false
    if (!valid() || this.active || this.readOnly) {
      this.detection = null
      this.syncGuard()
      return
    }
    if (!(await arbiter().claim(this.owner))) {
      this.detection = null
      this.syncGuard()
      return
    }
    this.zmodemError = null
    try {
      const problem = this.binaryProblem()
      if (problem) {
        this.detection?.d.deny()
        this.detection = null
        toast.error('ZMODEM is not possible in this session', { description: problem })
        return
      }
      if (det.role === 'receive') await this.zmodemReceiveFlow()
      else await this.zmodemSendFlow()
    } finally {
      this.detection = null
      this.detectionPrompt = null
      arbiter().release(this.owner)
      this.syncGuard()
    }
  }

  /** Confirm the current (latest valid) detection, or null when it went stale. */
  private confirmDetection(): ReturnType<ZmDetection['confirm']> | null {
    const d = this.detection?.d
    if (!d || !d.is_valid()) return null
    try {
      return d.confirm()
    } catch {
      return null
    }
  }

  private denyDetection(): void {
    const d = this.detection?.d
    if (d?.is_valid()) d.deny()
  }

  private async zmodemReceiveFlow(): Promise<void> {
    const s = currentSettings()
    const choice = await this.chooseDownload('zmodem', { auto: s.zmodemAutoReceive })
    this.detectionPrompt = null
    if (!choice) {
      this.denyDetection()
      return
    }
    const session = this.confirmDetection() as ZmReceiveSession | null
    if (!session) {
      toast.info('The ZMODEM transfer was abandoned by the server')
      return
    }
    const ac = new AbortController()
    const target = choice.create({ directory: false })
    const tops = new Map<string, string>()
    this.begin('zmodem', 'download', { title: 'Receiving with ZMODEM', cancel: () => ac.abort(), target: choice.label })
    try {
      const res = await zmodemReceive(session, {
        signal: ac.signal,
        open: async (info) => {
          let path: string[]
          if (target.kind === 'folder' && info.path.length > 1) {
            let top = tops.get(info.path[0])
            if (!top) {
              top = await target.reserve(info.path[0], { dir: true, overwrite: false })
              tops.set(info.path[0], top)
            }
            path = [top, ...info.path.slice(1)]
          } else {
            path = [await target.reserve(info.path.join('_'), { dir: false, overwrite: false })]
          }
          return target.openFile(path, { size: info.size, mtimeMs: info.mtimeMs })
        },
        progress: (p) => this.zProgress(p),
      })
      await target.finish()
      const names = res.files
      const msg = names.length
        ? `Received ${names.length === 1 ? names[0] : plural(names.length, 'file')} (${formatBytes(res.bytes)}) → ${target.label}`
        : 'Nothing was received'
      this.finish('done', msg)
      this.notice(`ZMODEM: ${msg}`, 'ok')
    } catch (err) {
      await target.abort().catch(() => undefined)
      this.zFailed(err, 'download')
    }
  }

  private async zmodemSendFlow(): Promise<void> {
    let selection: LocalSelection | null
    if (this.pendingRz) {
      selection = this.pendingRz.selection
      clearTimeout(this.pendingRz.timer)
      this.pendingRz = null
    } else {
      selection = await this.chooseUpload('zmodem', false)
      this.detectionPrompt = null
    }
    const files = selection?.files ?? []
    if (!files.length) {
      this.denyDetection()
      if (selection?.dirs.length) toast.info('ZMODEM sends files only', { description: 'Use trz -d (trzsz) or drop onto “Upload to …” for folders.' })
      return
    }
    const session = this.confirmDetection() as ZmSendSession | null
    if (!session) {
      toast.info('The ZMODEM receiver is gone (rz gave up waiting)')
      return
    }
    const taken = new Set<string>()
    const items = files.map((f) => {
      const n = uniqueName(sanitizeFileName(f.file.name), (c) => taken.has(c))
      taken.add(n)
      return { name: n, blob: f.file, mtimeMs: f.file.lastModified }
    })
    const total = items.reduce((n, it) => n + it.blob.size, 0)
    const ac = new AbortController()
    this.begin('zmodem', 'upload', { title: 'Sending with ZMODEM', cancel: () => ac.abort(), total })
    try {
      const res = await zmodemSend(session, items, { signal: ac.signal, progress: (p) => this.zProgress(p) })
      const msg = res.files.length
        ? `Sent ${res.files.length === 1 ? res.files[0] : plural(res.files.length, 'file')} (${formatBytes(res.bytes)})${res.skipped.length ? ` · ${res.skipped.length} skipped by the server` : ''}`
        : `Nothing was sent${res.skipped.length ? ` (the server skipped ${plural(res.skipped.length, 'file')})` : ''}`
      this.finish('done', msg)
      this.notice(`ZMODEM: ${msg}`, 'ok')
    } catch (err) {
      this.zFailed(err, 'upload')
    }
  }

  private zProgress(p: ZProgress): void {
    this.progress({ file: p.file, fileIndex: p.fileIndex, fileCount: p.fileCount, fileBytes: p.fileBytes, fileSize: p.fileSize, done: p.bytes, total: p.totalSize })
  }

  private zFailed(err: unknown, dir: 'download' | 'upload'): void {
    this.drain()
    if (isAbortError(err)) {
      this.finish('canceled', 'Canceled')
      this.notice(`ZMODEM: ${dir} canceled`, 'info')
      return
    }
    const local = this.zmodemError
    this.zmodemError = null
    const msg = isPeerAbort(err) ? (local ? `Protocol error: ${zmodemErrorMessage(local)}` : 'The server canceled the transfer') : zmodemErrorMessage(err)
    this.finish('error', msg)
    this.notice(`ZMODEM: ${dir} failed: ${msg}`, 'error')
  }

  // ===================================================================================================================
  // trzsz hooks
  // ===================================================================================================================

  private async claimForTrzsz(dropped: boolean): Promise<boolean> {
    if (this.readOnly || this.active) return false
    if (dropped) this.dropBoostUntil = Date.now() + 10_000
    const ok = await arbiter().claim(this.owner)
    if (ok) {
      // Until trzszStarted() or trzszEnded(): a prompt may follow; keys must not reach the server.
      this.active = { protocol: 'trzsz', cancel: () => this.trzszStage?.stop() }
      this.syncGuard()
    }
    return ok
  }

  private async chooseTrzszFiles(directory: boolean): Promise<TrzszUploadItem[] | null> {
    try {
      const sel = await this.chooseUpload('trzsz', directory)
      if (!sel) return null
      const items: TrzszUploadItem[] = sel.files.map((f) => ({ relPath: f.relPath.split('/').filter(Boolean), isDir: false, blob: f.file }))
      for (const d of sel.dirs) items.push({ relPath: d.split('/').filter(Boolean), isDir: true })
      if (!directory && sel.dirs.length) toast.info('trz receives files only', { description: 'Folders need “trz -d” on the server. Only the files were sent.' })
      return items
    } catch (err) {
      toast.error('Cannot choose files', { description: errorMessage(err) })
      return null
    }
  }

  private trzszStarted(direction: TransferDirection, directory: boolean): void {
    this.begin('trzsz', direction, {
      title: direction === 'download' ? `Receiving with trzsz${directory ? ' (folders)' : ''}` : `Sending with trzsz${directory ? ' (folders)' : ''}`,
      cancel: () => this.trzszStage?.stop(),
    })
  }

  private trzszProgress(p: TrzszProgress): void {
    this.progress({ file: p.file, fileIndex: p.fileIndex, fileCount: p.fileCount, fileBytes: p.fileBytes, fileSize: p.fileSize, done: p.bytes })
  }

  private trzszEnded(r: TrzszEnd): void {
    arbiter().release(this.owner)
    if (!this.view || this.view.protocol !== 'trzsz' || this.view.phase !== 'running') {
      // Ended before the transfer started: refused (prompt cancelled) or failed while negotiating.
      this.active = null
      this.syncGuard()
      if (!r.ok && !r.canceled && r.error) toast.error('trzsz transfer failed', { description: r.error })
      return
    }
    if (r.ok) {
      const names = r.names ?? []
      const what = names.length === 1 ? names[0] : plural(names.length, 'item')
      this.finish('done', this.view.direction === 'download' ? `Saved ${what} (${formatBytes(r.bytes)}) → ${r.target ?? 'Downloads'}` : `Sent ${what} (${formatBytes(r.bytes)})`)
    } else if (r.canceled) {
      this.finish('canceled', 'Canceled')
    } else {
      this.finish('error', r.error || 'The transfer failed')
    }
  }

  // ===================================================================================================================
  // uploads started from Termstead (drop, menu)
  // ===================================================================================================================

  /** Upload local files to the SSH session's folder (SFTP) or, for local shells, the host (and type the path). */
  async uploadToFolder(selection: Promise<LocalSelection> | LocalSelection, local: boolean): Promise<void> {
    const sel = await selection
    if (!sel.files.length && !sel.dirs.length) return
    const cwd = this.handle.info().cwd || this.session()?.cwd || undefined
    try {
      const res = await uploadSelection(
        { source: local ? { local: true } : { sessionId: this.sessionId }, dir: cwd, selection: sel, label: this.title },
        (id) =>
          showUploadToast(id, {
            reveal:
              !local && isCommandEnabled('files.revealInPanel')
                ? (path) => void runCommand('files.revealInPanel', { sessionId: this.sessionId, path }, { source: 'api' })
                : undefined,
          }),
      )
      if (res.canceled || !res.paths.length) return
      if (local || currentSettings().dropTypePath) this.typePaths(res.paths, local)
    } catch (err) {
      console.warn('[termtransfer] upload failed', err)
    }
  }

  private typePaths(paths: string[], local: boolean): void {
    if (this.disposed || !this.isConnected()) return
    const windowsHost = local && paths.every((p) => /^\/[A-Za-z]:\//.test(p))
    const text = paths
      .map((p) => (windowsHost ? quoteWindowsPath(p.slice(1).replace(/\//g, '\\')) : quoteShellPath(p)))
      .join(' ')
    this.handle.send(`${text} `)
    this.handle.focus()
  }

  /** Upload with `trz` (types it after Ctrl+C, like trzsz's own drag upload). */
  async uploadWithTrz(selection: Promise<LocalSelection> | LocalSelection): Promise<void> {
    const sel = await selection
    const tz = this.trzsz()
    if (!tz) {
      toast.error('trzsz is not available')
      return
    }
    const items: TrzszUploadItem[] = sel.files.map((f) => ({ relPath: f.relPath.split('/').filter(Boolean), isDir: false, blob: f.file }))
    for (const d of sel.dirs) items.push({ relPath: d.split('/').filter(Boolean), isDir: true })
    if (!items.length) return
    this.dropBoostUntil = Date.now() + 10_000
    try {
      await tz.startDropUpload(items)
    } catch (err) {
      toast.error('Upload with trz failed', { description: errorMessage(err) })
    }
  }

  /** Upload with `rz`: remember the files, type the command, answer the receiver when it announces itself. */
  async uploadWithRz(selection: Promise<LocalSelection> | LocalSelection): Promise<void> {
    const sel = await selection
    if (!sel.files.length) {
      if (sel.dirs.length) toast.info('ZMODEM sends files only', { description: 'Use trz -d (trzsz) or “Upload to …” for folders.' })
      return
    }
    if (sel.files.some((f) => f.relPath.includes('/'))) {
      toast.info('Folders are flattened', { description: 'ZMODEM sends files only; they all land in the remote folder.' })
    }
    if (this.pendingRz) clearTimeout(this.pendingRz.timer)
    const pending = {
      selection: { files: sel.files, dirs: [] },
      timer: setTimeout(() => {
        if (this.pendingRz !== pending) return
        this.pendingRz = null
        toast.error('rz did not start', { description: 'Is lrzsz installed on the server? The command is set in Settings → File transfer (terminal).' })
      }, 10_000),
    }
    this.pendingRz = pending
    this.dropBoostUntil = Date.now() + 10_000
    this.handle.send('\x03')
    await new Promise((r) => setTimeout(r, 200))
    this.handle.send(`${currentSettings().rzCommand}\r`)
  }

  /** Paste a small text file's contents through the terminal's paste pipeline (confirmation for multi-line text). */
  async pasteFile(selection: Promise<LocalSelection> | LocalSelection): Promise<void> {
    const sel = await selection
    if (sel.files.length !== 1) {
      toast.info(sel.files.length ? 'Drop a single file to paste its contents' : 'Nothing to paste')
      return
    }
    const f = sel.files[0].file
    const max = currentSettings().pasteMaxBytes
    if (f.size > max) {
      toast.error(`${f.name} is too large to paste`, { description: `The limit is ${formatBytes(max)} (Settings → File transfer (terminal)).` })
      return
    }
    const text = await readTextFile(f, max)
    if (text === null) {
      toast.error(`${f.name} is not a text file`, { description: 'Only UTF-8 text can be pasted. Use an upload instead.' })
      return
    }
    await this.handle.paste(text)
  }

  // ===================================================================================================================
  // send file (CC-8)
  // ===================================================================================================================

  async sendFile(file: File, o: SendFileOptions): Promise<void> {
    if (this.readOnly) throw new Error('This terminal is read-only')
    if (this.busy) throw new Error('A transfer is already running in this terminal')
    if (!this.isConnected()) throw new Error('The session is not connected')
    if (o.mode === 'binary') return this.sendBinary(file, o)
    const text = await readTextFile(file, 64 * 1024 * 1024)
    if (text === null) throw new Error(NOT_TEXT)
    if (o.promptPattern.trim()) compilePromptPattern(o.promptPattern) // validate early (throws)
    const useServer = o.eol === '\r' && new TextEncoder().encode(text).length <= SERVER_PACER_MAX && (await serverPacerAvailable())
    if (useServer) {
      try {
        await this.sendTextServer(file.name, text, o, false)
        return
      } catch (err) {
        if (!(err instanceof PacerUnavailableError) && !(err instanceof FallbackToBrowser)) throw err
      }
    }
    await this.sendTextBrowser(file.name, text, o)
  }

  private async sendTextServer(name: string, text: string, o: SendFileOptions, confirmDangerous: boolean): Promise<void> {
    let job: Awaited<ReturnType<typeof startPacedSend>>
    const lines = text === '' ? 0 : text.split(/\r\n|\r|\n/).length - (/(\r\n|\r|\n)$/.test(text) ? 1 : 0)
    try {
      job = await startPacedSend(
        {
          sessionIds: [this.sessionId],
          text,
          lineDelayMs: o.lineDelayMs,
          charDelayMs: o.charDelayMs,
          waitPrompt: o.waitPrompt,
          promptPattern: o.waitPrompt ? o.promptPattern.trim() || undefined : undefined,
          promptTimeoutMs: o.promptTimeoutMs,
          enter: false,
          confirmDangerous,
        },
        (p) => {
          if (p.kind === 'progress' || p.kind === 'done') this.progress({ done: p.done ?? 0, total: p.total ?? lines })
          if (p.kind === 'warning' && this.view) this.progress({ warnings: (this.view.warnings ?? 0) + 1 })
        },
      )
    } catch (err) {
      if (err instanceof DangerousTextError) {
        const ok = await askDangerous(`Send ${name}?`, err.matches)
        if (!ok) return
        return this.sendTextServer(name, text, o, true)
      }
      // The server rejects prompt patterns its (RE2) engine cannot compile: the browser pacer understands them.
      if ((err as { status?: number }).status === 400 && /prompt pattern/i.test(errorMessage(err))) throw new FallbackToBrowser()
      throw err
    }
    this.begin('send', 'upload', { title: `Sending ${name}`, cancel: () => void job.cancel(), unit: 'lines', total: lines })
    this.progress({ file: name })
    let canceled = false
    this.active = {
      protocol: 'send',
      cancel: () => {
        canceled = true
        void job.cancel()
      },
    }
    const r = await job.done
    if (canceled) this.finish('canceled', 'Canceled')
    else if (r.ok) this.finish('done', `Sent ${plural(lines, 'line')}${r.warnings ? ` · ${plural(r.warnings, 'prompt warning')}` : ''}`)
    else this.finish('error', r.error || 'Sending failed')
  }

  private getClock(): Clock {
    this.clock ??= workerClock() ?? timeoutClock()
    return this.clock
  }

  private async sendTextBrowser(name: string, text: string, o: SendFileOptions): Promise<void> {
    const ac = new AbortController()
    const re = o.waitPrompt ? (compilePromptPattern(o.promptPattern) ?? new RegExp(DEFAULT_PROMPT_PATTERN)) : null
    this.begin('send', 'upload', { title: `Sending ${name}`, cancel: () => ac.abort(), unit: 'lines' })
    this.progress({ file: name })
    let warnings = 0
    try {
      const r = await sendTextPaced(
        text,
        { eol: o.eol, lineDelayMs: o.lineDelayMs, charDelayMs: o.charDelayMs, prompt: re ? { re, timeoutMs: o.promptTimeoutMs } : null },
        {
          send: (s) => this.ctx.sendRaw(s),
          clock: this.getClock(),
          watcher: this.watcher,
          signal: ac.signal,
          onProgress: (done, total) => this.progress({ done, total }),
          onWarning: () => {
            warnings++
            this.progress({ warnings })
          },
        },
      )
      this.finish('done', `Sent ${plural(r.lines, 'line')}${r.warnings ? ` · ${plural(r.warnings, 'prompt warning')}` : ''}`)
    } catch (err) {
      if (isAbortError(err)) this.finish('canceled', 'Canceled')
      else this.finish('error', errorMessage(err))
    }
  }

  private async sendBinary(file: File, o: SendFileOptions): Promise<void> {
    const problem = this.binaryProblem()
    if (problem) throw new Error(problem)
    const ac = new AbortController()
    const errorAtStart = this.handle.info().error
    this.begin('send', 'upload', { title: `Sending ${file.name} (binary)`, cancel: () => ac.abort(), total: file.size })
    this.progress({ file: file.name })
    try {
      const sent = await sendBinaryPaced(
        file,
        { chunkBytes: o.chunkBytes, delayMs: o.delayMs },
        {
          send: (b) => this.ctx.sendRaw(b),
          clock: this.getClock(),
          signal: ac.signal,
          onProgress: (done, total) => this.progress({ done, total }),
          problem: () => {
            const e = this.handle.info().error
            return e && e !== errorAtStart && /input buffer/i.test(e)
              ? 'The session could not keep up (the server’s input buffer is full). Use smaller chunks or a longer delay.'
              : null
          },
        },
      )
      this.finish('done', `Sent ${formatBytes(sent)}`)
    } catch (err) {
      if (isAbortError(err)) this.finish('canceled', 'Canceled')
      else if (err instanceof NotConnectedError) this.finish('error', err.message)
      else this.finish('error', errorMessage(err))
    }
  }

  // ===================================================================================================================
  // drop target
  // ===================================================================================================================

  /** Drop zones for this terminal right now. */
  dropZones(): { zones: DropZone[]; blocked?: string } {
    if (this.readOnly) return { zones: [], blocked: 'This terminal is read-only' }
    if (this.busy) return { zones: [], blocked: 'A transfer is running in this terminal' }
    if (!this.isConnected()) return { zones: [], blocked: 'The session is not connected' }
    const s: TermTransferSettings = currentSettings()
    const info = this.handle.info()
    const cwd = info.cwd || this.session()?.cwd || ''
    const zones: DropZone[] = []
    const proto = this.protocol
    if (proto === 'ssh') {
      zones.push({ id: 'sftp', label: cwd ? `Upload to ${displayRemotePath(cwd)}` : 'Upload to the home folder', detail: 'SFTP · resumable · folders too' })
    } else if (proto === 'local' && localFilesAllowed()) {
      zones.push({ id: 'local', label: cwd ? `Copy to ${cwd}` : 'Copy to the home folder', detail: 'then type the path' })
    }
    if (s.trzsz) zones.push({ id: 'trz', label: 'Upload with trz', detail: 'trzsz on the server · folders too' })
    if (s.zmodem) zones.push({ id: 'rz', label: 'Upload with rz', detail: 'ZMODEM · files only' })
    zones.push({ id: 'paste', label: 'Paste contents', detail: `one text file · up to ${formatBytes(s.pasteMaxBytes)}` })
    return { zones }
  }

  private installDrop(): void {
    const root = this.dropRoot
    const zoneAt = (e: DragEvent): DropZoneId | null => {
      const el = (e.target as Element | null)?.closest?.('[data-tt-zone]') as HTMLElement | null
      return (el?.dataset.ttZone as DropZoneId | undefined) ?? null
    }
    const show = (e: DragEvent) => {
      const term = getTerm(this.tabId)
      const { zones, blocked } = term?.drag && this.lastDragAt && Date.now() - this.lastDragAt < 1000 ? term.drag : this.dropZones()
      this.lastDragAt = Date.now()
      setDrag(this.tabId, { zones, blocked, hover: zoneAt(e) })
      this.dragTimer ??= setInterval(() => {
        if (Date.now() - this.lastDragAt > 300) this.hideDrag()
      }, 150)
    }
    const onOver = (e: DragEvent) => {
      if (!dragHasFiles(e.dataTransfer) || this.readOnly) return
      e.preventDefault()
      // A prompt waiting for files (rz / trz) takes the drop anywhere on the pane.
      if (getTerm(this.tabId)?.prompt?.acceptDrop) {
        if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy'
        return
      }
      const blocked = getTerm(this.tabId)?.drag?.blocked
      if (e.dataTransfer) e.dataTransfer.dropEffect = blocked ? 'none' : 'copy'
      show(e)
    }
    const onDrop = (e: DragEvent) => {
      if (!dragHasFiles(e.dataTransfer) || this.readOnly) return
      e.preventDefault()
      e.stopPropagation()
      const accept = getTerm(this.tabId)?.prompt?.acceptDrop
      if (accept) {
        this.hideDrag()
        if (e.dataTransfer) accept(e.dataTransfer)
        return
      }
      const drag = getTerm(this.tabId)?.drag ?? { ...this.dropZones(), hover: null }
      this.hideDrag()
      const dt = e.dataTransfer
      if (!dt || drag.blocked || !drag.zones.length) {
        if (drag.blocked) toast.info(drag.blocked)
        return
      }
      const zone = zoneAt(e) ?? drag.zones[0].id
      this.handleDrop(zone, dt)
    }
    root.addEventListener('dragenter', onOver)
    root.addEventListener('dragover', onOver)
    root.addEventListener('drop', onDrop)
    this.cleanups.push(() => {
      root.removeEventListener('dragenter', onOver)
      root.removeEventListener('dragover', onOver)
      root.removeEventListener('drop', onDrop)
    })
  }

  private hideDrag(): void {
    if (this.dragTimer) clearInterval(this.dragTimer)
    this.dragTimer = null
    this.lastDragAt = 0
    setDrag(this.tabId, undefined)
  }

  /** Must run synchronously inside the drop event (the DataTransfer is read there). */
  handleDrop(zone: DropZoneId, dt: DataTransfer): void {
    const sel = collectDrop(dt)
    this.dropBoostUntil = Date.now() + 10_000
    switch (zone) {
      case 'sftp':
        void this.uploadToFolder(sel, false)
        break
      case 'local':
        void this.uploadToFolder(sel, true)
        break
      case 'trz':
        void this.uploadWithTrz(sel)
        break
      case 'rz':
        void this.uploadWithRz(sel)
        break
      case 'paste':
        void this.pasteFile(sel)
        break
    }
  }

  /** Menu / palette: choose local files, then upload them the given way. */
  pickAndUpload(via: DropZoneId): void {
    if (this.readOnly || !this.isConnected()) {
      toast.info(this.readOnly ? 'This terminal is read-only' : 'The session is not connected')
      return
    }
    if (via !== 'sftp' && via !== 'local' && this.busy) {
      toast.info('A transfer is running in this terminal')
      return
    }
    const sel = pickFiles(this.doc(), { multiple: true, folder: false })
    void sel.then((s) => {
      if (!s) return
      this.dropBoostUntil = Date.now() + 10_000
      if (via === 'sftp') void this.uploadToFolder(s, false)
      else if (via === 'local') void this.uploadToFolder(s, true)
      else if (via === 'trz') void this.uploadWithTrz(s)
      else if (via === 'rz') void this.uploadWithRz(s)
      else void this.pasteFile(s)
    })
  }
}

class FallbackToBrowser extends Error {}

/** Guarded against a failing plugin setup elsewhere: never throws. */
export function createController(term: Terminal, ctx: TerminalPluginContext): TransferController | null {
  try {
    return new TransferController(term, ctx)
  } catch (err) {
    console.error('[termtransfer] cannot attach to the terminal', err)
    return null
  }
}

