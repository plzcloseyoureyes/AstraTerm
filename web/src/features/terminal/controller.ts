/*
 * TerminalController — the engine behind one terminal tab (lazy chunk: this is where xterm and its addons load).
 *
 *   xterm 6 + addons: fit (ResizeObserver, debounced), webgl (pooled, DOM fallback on failure / context loss), unicode11,
 *   web-links + OSC 8 link handler, search, image (sixel / iTerm2), clipboard (OSC 52 policy), serialize, progress.
 *
 *   Output stream (SPEC §6.2): offsets are tracked per byte. `receivedOffset` = end of the last byte queued into xterm,
 *   `renderedOffset` = end of the last byte xterm parsed (write callback). Acks carry renderedOffset; pausing output
 *   (Scroll Lock, CC-10) holds chunks — and therefore acks — so the server throttles the remote. Chunks replayed while
 *   (re)attaching are flagged: bells, notifications, OSC 52 and automatic terminal replies generated while parsing them
 *   are suppressed (they answer queries of the past).
 *
 *   Input: keystrokes / pastes are "user input" (MultiExec fan-out, observers); xterm's automatic replies (DA, CPR…) and
 *   mouse reports only go to this session.
 */
import { Terminal, type IDisposable, type IMarker, type ITerminalOptions } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { WebglAddon } from '@xterm/addon-webgl'
import { Unicode11Addon } from '@xterm/addon-unicode11'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { SearchAddon } from '@xterm/addon-search'
import { ImageAddon } from '@xterm/addon-image'
import { ClipboardAddon, type ClipboardSelectionType, type IBase64, type IClipboardProvider } from '@xterm/addon-clipboard'
import { SerializeAddon } from '@xterm/addon-serialize'
import { ProgressAddon } from '@xterm/addon-progress'
import { toast } from 'sonner'
import { breakSession, getScrollback, reconnectSession, setSessionLogging, setSessionRecording, signalSession } from '@/api/sessions'
import type { Connection, RuntimeSession, TerminalServerMessage } from '@/api/types'
import { terminalPlugins, type TerminalPluginContext, type TerminalPluginDef } from '@/app/registry'
import { isCommandEnabled, runCommand } from '@/app/commands'
import { errorMessage, isMac } from '@/lib/utils'
import { generalSettings } from '@/stores/settings'
import { closeTab, focusTab, setTabState, setTabTitle, updateTabParams, useWorkspaceStore, type TabStatus } from '@/stores/workspace'
import { emitTerminalInput, emitTerminalOutput, hasTerminalOutputListeners, markTerminalFocused, publishTerminalInfo, registerTerminal } from './bus'
import { pushClipboardHistory, readClipboard, writeClipboard } from './clipboard'
import { ensureFontLoaded } from './fonts'
import { InputLocks } from './inputLock'
import { isParticipant, multiExecTargets } from './multiexec'
import { notify, pageHasAttention, playBell } from './notify'
import { duplicateSession, isFreshSession, restartSessionInTab } from './open'
import { analyzePaste, sanitizePaste, splitPasteLines, type PasteAnalysis } from './paste'
import { BoundaryScanner, dropSavedTerminal, loadSavedTerminal, registerPersister, saveTerminal } from './persist'
import { LIMITS, terminalSettings, type TerminalSettings } from './settings'
import { downloadBlob, downloadText, sanitizeFileName } from './schemeIO'
import { TerminalSocket } from './socket'
import { resolveScheme, searchColors, toXtermTheme } from './themes'
import type {
  TerminalHandle,
  TerminalInfo,
  TerminalPasteOptions,
  TerminalPluginContextEx,
  TerminalScheme,
  TerminalSessionState,
  TerminalTabParams,
  TransportState,
} from './types'

// ---------------------------------------------------------------------------------------------------------------------
// UI requests (rendered by TerminalView inside the terminal's own window, so they work in pop-outs)
// ---------------------------------------------------------------------------------------------------------------------

export interface PasteDecision {
  text: string
  mode: 'normal' | 'paced'
  stripHidden: boolean
  dontAskMultiline: boolean
}

export type UiRequest =
  | {
      kind: 'paste'
      text: string
      analysis: PasteAnalysis
      /** Number of terminals receiving the paste (MultiExec). */
      targets: number
      bracketed: boolean
      resolve: (d: PasteDecision | null) => void
    }
  | { kind: 'link'; uri: string; scheme: string; action: string; resolve: (ok: boolean) => void }
  | { kind: 'osc52-read'; resolve: (ok: boolean) => void }
  | { kind: 'osc52-copy'; text: string; resolve: (ok: boolean) => void }
  | { kind: 'history'; resolve: (ok: boolean) => void }

type DistributiveOmit<T, K extends PropertyKey> = T extends unknown ? Omit<T, K> : never
type UiRequestInput = DistributiveOmit<UiRequest, 'resolve'>

// ---------------------------------------------------------------------------------------------------------------------
// WebGL context pool (TERM-4): browsers allow ~16 live WebGL contexts; visible terminals get priority.
// ---------------------------------------------------------------------------------------------------------------------

const MAX_WEBGL = 10
const webglHolders = new Set<TerminalController>()

function reserveWebgl(c: TerminalController): void {
  webglHolders.add(c)
  if (webglHolders.size <= MAX_WEBGL) return
  let victim: TerminalController | null = null
  for (const h of webglHolders) {
    if (h === c || h.isVisible) continue
    if (!victim || h.lastVisibleAt < victim.lastVisibleAt) victim = h
  }
  victim?.releaseWebgl()
}

// ---------------------------------------------------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------------------------------------------------

const EMPTY = new Uint8Array(0)
const ACK_STEP = 16 * 1024
const ENDED: ReadonlySet<TerminalSessionState> = new Set(['closed', 'disconnected', 'error', 'gone'])
const RUNNING: ReadonlySet<TerminalSessionState> = new Set(['connecting', 'authenticating', 'connected'])
const MAX_PROMPT_MARKS = 500
const OSC52_MAX_BYTES = 1 << 20
const isLinux = typeof navigator !== 'undefined' && /Linux/.test(navigator.platform) && !/Android/.test(navigator.userAgent)

/** Terminal reports that user input never produces (only relevant to the IME / insertText input path). */
// oxlint-disable-next-line no-control-regex -- terminal reports are control sequences
const REPORT_RE = /^(\x1b\[[?>]?[\d;]*[cnRty]|\x1b\[\d+;\d+R|\x1b\[[IO]|\x1b\][\s\S]*(\x07|\x1b\\)|\x1bP[\s\S]*\x1b\\|\x1b\[\?[\d;]*\$y)$/

function sanitizeTitle(t: string): string {
  // oxlint-disable-next-line no-control-regex -- strip control characters from remote titles
  return t.replace(/[\u0000-\u001f\u007f-\u009f]/g, '').trim().slice(0, 200)
}

function statusFor(state: TerminalSessionState): TabStatus | undefined {
  switch (state) {
    case 'connecting':
    case 'authenticating':
      return 'connecting'
    case 'connected':
      return 'connected'
    case 'error':
      return 'error'
    case 'disconnected':
    case 'closed':
    case 'gone':
      return 'disconnected'
    default:
      return undefined
  }
}

function safeDecode(s: string): string {
  try {
    return decodeURIComponent(s)
  } catch {
    return s
  }
}

function timestamp(): string {
  const d = new Date()
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}`
}

/** Base64 for OSC 52 with a size cap (the default addon codec has none). */
const osc52Codec: IBase64 = {
  encodeText(data: string): string {
    const bytes = new TextEncoder().encode(data)
    let bin = ''
    for (let i = 0; i < bytes.length; i += 0x8000) bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000))
    return btoa(bin)
  },
  decodeText(data: string): string {
    if (!data || data.length > Math.ceil((OSC52_MAX_BYTES * 4) / 3) + 4) return ''
    try {
      const bin = atob(data)
      const bytes = new Uint8Array(bin.length)
      for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
      return new TextDecoder('utf-8', { fatal: false }).decode(bytes)
    } catch {
      return ''
    }
  },
}

interface Pending {
  /** Stream offset after this chunk; null for local writes. */
  end: number | null
  replay: boolean
  ground: boolean
}

type OutputItem =
  | { kind: 'data'; bytes: Uint8Array; end: number; ground: boolean; replay: boolean }
  | { kind: 'reset'; offset: number }
  | { kind: 'local'; data: string | Uint8Array }

export interface ControllerInit {
  tabId: string
  params: TerminalTabParams
  settings: TerminalSettings
  uiDark: boolean
  connection?: Connection
  session?: RuntimeSession
  /** Size to create the terminal with before it can be fitted. */
  initialSize?: { cols: number; rows: number }
}

// ---------------------------------------------------------------------------------------------------------------------
// controller
// ---------------------------------------------------------------------------------------------------------------------

// Fitting (TERM-3): a proposed size below MIN_SETTLED_* is retried every FIT_RETRY_MS (≤ FIT_SETTLE_RETRIES times) before
// it is applied; SETTLE_FIT_DELAYS re-check the size after the terminal was opened or shown.
const MIN_SETTLED_ROWS = 5
const MIN_SETTLED_COLS = 20
const FIT_RETRY_MS = 100
const FIT_SETTLE_RETRIES = 15
const SETTLE_FIT_DELAYS = [150, 600, 1500]

export class TerminalController implements TerminalHandle {
  readonly tabId: string
  readonly sessionId: string
  readonly term: Terminal

  readonly fitAddon = new FitAddon()
  readonly searchAddon = new SearchAddon({ highlightLimit: 2000 })
  readonly serializeAddon = new SerializeAddon()
  private readonly progressAddon = new ProgressAddon()
  private webgl: WebglAddon | null = null
  private webglLossListener: IDisposable | null = null
  private webglRetryAt = 0
  private imageAddon: ImageAddon | null = null
  private clipboardAddon: ClipboardAddon | null = null

  private params: TerminalTabParams
  private settings: TerminalSettings
  private uiDark: boolean
  private scheme: TerminalScheme
  private connection?: Connection
  private runtime?: RuntimeSession
  private host: HTMLElement | null = null
  private opened = false
  private disposed = false
  private readonly socket: TerminalSocket
  private readonly disposables: IDisposable[] = []
  private readonly cleanups: (() => void)[] = []
  private resizeObserver: ResizeObserver | null = null

  // stream state
  private receivedOffset = 0
  private renderedOffset = 0
  private ackedOffset = 0
  private replayHead = 0
  private skipBytes = 0
  private readonly pending: Pending[] = []
  private pendingReplay = 0
  private readonly scanner = new BoundaryScanner()
  private groundAtRendered = true
  private paused = false
  private pausedQueue: OutputItem[] = []
  private ackTimer: ReturnType<typeof setTimeout> | null = null

  // input classification
  private keyData: string | null = null
  private programmatic = 0
  private imeActive = false
  private textInput = false
  private swallowKeypress = false
  private droppedInputAt = 0

  // info & UI
  private live: TerminalInfo
  private readonly infoListeners = new Set<() => void>()
  private ui: UiRequest | null = null
  private readonly uiQueue: UiRequest[] = []
  private readonly uiListeners = new Set<() => void>()
  private readonly bellListeners = new Set<() => void>()
  private readonly searchListeners = new Set<() => void>()
  private currentWin: Window | null = null

  // plugins & observers
  private readonly inputObservers = new Set<(data: string) => void>()
  private readonly outputObservers = new Set<(data: Uint8Array) => void>()
  private readonly outputFilters: ((data: Uint8Array) => Uint8Array | null)[] = []
  private readonly plugins = new Map<string, { def: TerminalPluginDef; dispose?: () => void }>()

  // timers & misc
  private fitTimer: ReturnType<typeof setTimeout> | null = null
  /** Input gate held by plugins (in-band transfers): refuses input from other sources, see inputLock.ts. */
  private readonly inputLocks = new InputLocks((reason) => this.setInfo({ inputLock: reason ?? undefined }))
  private blockedInputAt = 0
  /** Consecutive fits that saw a tiny (probably transient) pane and retried instead of applying it. */
  private fitRetries = 0
  private settleTimers: ReturnType<typeof setTimeout>[] = []
  private unwatchWindow: (() => void) | null = null
  private resizeSendTimer: ReturnType<typeof setTimeout> | null = null
  private copyOnSelectTimer: ReturnType<typeof setTimeout> | null = null
  private silenceTimer: ReturnType<typeof setTimeout> | null = null
  private closeOnExitTimer: ReturnType<typeof setTimeout> | null = null
  private infoPublishFrame = 0
  private lastBellAt = 0
  private commandStartAt: number | null = null
  private readonly promptMarks: IMarker[] = []
  private pasteAbort: { aborted: boolean } | null = null
  private linkTooltip: HTMLElement | null = null
  private unregisterBus: () => void = () => undefined
  private sentSize: { cols: number; rows: number } | null = null
  private attachResizePending = false
  /** Offset passed on the latest (re)connect. */
  private requestedOffset = 0
  visibleNow = false
  lastVisibleAt = 0

  constructor(init: ControllerInit) {
    this.tabId = init.tabId
    this.sessionId = init.params.sessionId
    this.params = init.params
    this.settings = init.settings
    this.uiDark = init.uiDark
    this.connection = init.connection
    this.runtime = init.session
    this.scheme = resolveScheme(this.settings, this.uiDark)

    const saved = loadSavedTerminal(this.sessionId)
    const size = saved ?? init.initialSize ?? (init.session ? { cols: init.session.cols, rows: init.session.rows } : { cols: 80, rows: 24 })
    const fontSize = this.fontSize()

    this.term = new Terminal({
      ...this.xtermOptions(fontSize),
      cols: Math.max(2, Math.min(1000, size.cols || 80)),
      rows: Math.max(1, Math.min(1000, size.rows || 24)),
      allowProposedApi: true,
      allowTransparency: this.settings.backgroundOpacity < 1,
      overviewRuler: { width: 10 },
      logLevel: 'warn',
      windowOptions: { getWinSizeChars: true, getCellSizePixels: true, getWinSizePixels: true, pushTitle: true, popTitle: true },
      linkHandler: {
        allowNonHttpProtocols: true,
        activate: (event, text) => this.activateLink(event, text),
        hover: (event, text) => this.showLinkTooltip(event, text),
        leave: () => this.hideLinkTooltip(),
      },
    })

    this.live = {
      tabId: this.tabId,
      sessionId: this.sessionId,
      protocol: init.params.protocol ?? init.session?.protocol,
      title: this.baseTitle(),
      oscTitle: '',
      cols: this.term.cols,
      rows: this.term.rows,
      state: init.session?.state ?? 'unknown',
      stateMessage: init.session?.stateMessage,
      exitCode: init.session?.exitCode,
      transport: 'connecting',
      transportAttempts: 0,
      cwd: init.session?.cwd,
      readOnly: false,
      paused: false,
      pausedBytes: 0,
      encoding: this.encoding(),
      renderer: 'dom',
      fontSize,
      hasOutput: false,
      replaying: false,
      searchOpen: false,
      pasting: null,
      recording: !!init.session?.recording,
      logging: !!init.session?.logging,
    }

    this.loadAddons()
    this.installTerminalListeners()

    if (saved) {
      // Paint the pre-reload screen immediately; the socket then attaches with ?offset=saved.offset (delta).
      this.receivedOffset = this.renderedOffset = this.ackedOffset = saved.offset
      this.writeLocalInternal(saved.data, true)
      this.live.hasOutput = true
    }

    this.socket = new TerminalSocket(
      this.sessionId,
      {
        onData: (chunk) => this.onSocketData(chunk),
        onMessage: (msg) => this.onSocketMessage(msg),
        onTransport: (state, attempt) => this.onTransport(state, attempt),
        onGone: () => this.onGone(),
        onProbe: (s) => this.setSession(s),
      },
      {
        getOffset: () => (this.requestedOffset = this.receivedOffset),
        shouldReconnect: () => this.live.state !== 'closed' && this.live.state !== 'gone',
      },
    )

    this.unregisterBus = registerTerminal(this, this.live)
    this.cleanups.push(registerPersister(() => this.persist()))
    this.applyTabStatus()
  }

  // ===================================================================================================================
  // lifecycle
  // ===================================================================================================================

  /** Attach to the DOM (after the terminal font is loaded) and connect the socket. */
  async mount(host: HTMLElement): Promise<void> {
    if (this.disposed) return
    this.host = host
    this.installHostListeners(host)
    this.socket.connect()
    const doc = host.ownerDocument
    await ensureFontLoaded(this.settings.fontFamily, this.fontSize(), [this.settings.fontWeight, this.settings.fontWeightBold], doc)
    if (this.disposed || this.host !== host) return
    this.term.open(host)
    this.opened = true
    this.currentWin = host.ownerDocument.defaultView
    this.observeResize(host)
    if (this.visibleNow) this.enableWebgl()
    this.fitNow()
    this.settleFit()
    this.setupPlugins()
    // Re-measure once web fonts finished loading (metrics may change after the first paint).
    void doc.fonts?.ready.then(() => {
      if (this.disposed || !this.opened) return
      this.term.options.fontFamily = this.settings.fontFamily
      this.scheduleFit(0)
      this.settleFit()
    })
    if (this.isActiveTab() && this.shouldTakeFocus()) this.focus()
  }

  dispose(): void {
    if (this.disposed) return
    this.disposed = true
    for (const t of [this.fitTimer, this.resizeSendTimer, this.copyOnSelectTimer, this.silenceTimer, this.closeOnExitTimer, this.ackTimer, ...this.settleTimers]) {
      if (t) clearTimeout(t)
    }
    this.unwatchWindow?.()
    if (this.infoPublishFrame) cancelAnimationFrame(this.infoPublishFrame)
    if (this.pasteAbort) this.pasteAbort.aborted = true
    // Resolve pending UI requests (as cancelled) so awaiting callers finish.
    const pendingUi = [this.ui, ...this.uiQueue]
    this.ui = null
    this.uiQueue.length = 0
    for (const r of pendingUi) if (r) (r.resolve as (v: unknown) => void)(r.kind === 'paste' ? null : false)
    this.socket.close()
    this.inputLocks.clear()
    for (const [, p] of this.plugins) this.safeDispose(p.dispose)
    this.plugins.clear()
    for (const c of this.cleanups.splice(0)) this.safeDispose(c)
    for (const d of this.disposables.splice(0)) this.safeDispose(() => d.dispose())
    this.resizeObserver?.disconnect()
    this.resizeObserver = null
    this.hideLinkTooltip()
    webglHolders.delete(this)
    this.webglLossListener?.dispose()
    this.webglLossListener = null
    this.webgl = null
    this.unregisterBus()
    this.inputObservers.clear()
    this.outputObservers.clear()
    this.infoListeners.clear()
    this.uiListeners.clear()
    this.bellListeners.clear()
    this.searchListeners.clear()
    try {
      this.term.dispose()
    } catch {
      /* already disposed */
    }
  }

  private safeDispose(fn?: () => void): void {
    try {
      fn?.()
    } catch (err) {
      console.error('[terminal] dispose failed', err)
    }
  }

  get isVisible(): boolean {
    return this.visibleNow
  }

  /** The window that hosts this terminal (the main window or a dockview pop-out). */
  win(): Window {
    return this.host?.ownerDocument.defaultView ?? window
  }

  // ===================================================================================================================
  // info store (React: useSyncExternalStore)
  // ===================================================================================================================

  getInfo = (): TerminalInfo => this.live

  subscribeInfo = (cb: () => void): (() => void) => {
    this.infoListeners.add(cb)
    return () => this.infoListeners.delete(cb)
  }

  private setInfo(patch: Partial<TerminalInfo>): void {
    if (this.disposed) return
    let changed = false
    for (const k of Object.keys(patch) as (keyof TerminalInfo)[]) {
      if (this.live[k] !== patch[k]) {
        changed = true
        break
      }
    }
    if (!changed) return
    this.live = { ...this.live, ...patch }
    for (const l of Array.from(this.infoListeners)) l()
    // The bus (status bar, MultiExec bar) is updated once per frame at most.
    if (!this.infoPublishFrame) {
      this.infoPublishFrame = requestAnimationFrame(() => {
        this.infoPublishFrame = 0
        if (!this.disposed) publishTerminalInfo(this.live)
      })
    }
  }

  getUi = (): UiRequest | null => this.ui

  subscribeUi = (cb: () => void): (() => void) => {
    this.uiListeners.add(cb)
    return () => this.uiListeners.delete(cb)
  }

  /** Queue a UI request (one dialog at a time) and await its answer. */
  private requestUi<T>(req: UiRequestInput): Promise<T> {
    return new Promise<T>((resolve) => {
      const full = { ...req, resolve } as unknown as UiRequest
      if (this.disposed) {
        ;(resolve as (v: unknown) => void)(req.kind === 'paste' ? null : false)
        return
      }
      if (this.ui) this.uiQueue.push(full)
      else {
        this.ui = full
        this.emitUi()
      }
    })
  }

  /** Withdraw pending UI requests of a kind (answered with their "cancel" value). */
  private dropUi(kind: UiRequest['kind']): void {
    const cancel = (r: UiRequest) => (r.resolve as (v: unknown) => void)(r.kind === 'paste' ? null : false)
    for (let i = this.uiQueue.length - 1; i >= 0; i--) {
      if (this.uiQueue[i].kind === kind) cancel(this.uiQueue.splice(i, 1)[0])
    }
    if (this.ui?.kind === kind) this.resolveUi(this.ui.kind === 'paste' ? null : false)
  }

  /** Answer the current UI request (called by the view). */
  resolveUi(value: unknown): void {
    const cur = this.ui
    if (!cur) return
    this.ui = this.uiQueue.shift() ?? null
    this.emitUi()
    ;(cur.resolve as (v: unknown) => void)(value)
    // Give the keyboard back to the terminal once the dialog is gone (after the dialog's own focus restoration).
    if (!this.ui) {
      const win = this.win()
      win.requestAnimationFrame(() => win.requestAnimationFrame(() => !this.ui && this.focus()))
    }
  }

  private emitUi(): void {
    for (const l of Array.from(this.uiListeners)) l()
  }

  /** Visual bell subscribers (the view flashes). */
  onBell(cb: () => void): () => void {
    this.bellListeners.add(cb)
    return () => this.bellListeners.delete(cb)
  }

  // ===================================================================================================================
  // settings / params / connection / session updates from the view
  // ===================================================================================================================

  private fontSize(): number {
    const [min, max] = LIMITS.fontSize
    return Math.min(max, Math.max(min, this.settings.fontSize + (this.params.zoom ?? 0)))
  }

  private xtermOptions(fontSize: number): ITerminalOptions {
    const s = this.settings
    return {
      fontFamily: s.fontFamily,
      fontSize,
      fontWeight: s.fontWeight,
      fontWeightBold: s.fontWeightBold,
      lineHeight: s.lineHeight,
      letterSpacing: s.letterSpacing,
      cursorStyle: s.cursorStyle,
      cursorBlink: s.cursorBlink,
      cursorInactiveStyle: s.cursorInactiveStyle,
      cursorWidth: s.cursorWidth,
      scrollback: s.scrollback,
      smoothScrollDuration: s.smoothScrollDuration,
      scrollSensitivity: s.scrollSensitivity,
      fastScrollSensitivity: s.fastScrollSensitivity,
      scrollOnUserInput: s.scrollOnUserInput,
      minimumContrastRatio: s.minimumContrastRatio,
      drawBoldTextInBrightColors: s.drawBoldTextInBrightColors,
      macOptionIsMeta: s.macOptionIsMeta,
      macOptionClickForcesSelection: true,
      altClickMovesCursor: s.altClickMovesCursor,
      rightClickSelectsWord: s.rightClickSelectsWord,
      wordSeparator: s.wordSeparator,
      screenReaderMode: s.screenReaderMode,
      ignoreBracketedPasteMode: !s.bracketedPaste,
      theme: toXtermTheme(this.scheme, s.backgroundOpacity),
    }
  }

  /** Effective settings changed (global settings, connection overrides, UI theme). */
  applySettings(settings: TerminalSettings, uiDark: boolean): void {
    if (this.disposed) return
    const prev = this.settings
    this.settings = settings
    this.uiDark = uiDark
    this.scheme = resolveScheme(settings, uiDark)
    const fontSize = this.fontSize()
    const opts = this.xtermOptions(fontSize)
    const cur = this.term.options as Record<string, unknown>
    const next: Record<string, unknown> = {}
    for (const [k, v] of Object.entries(opts)) {
      if (k === 'theme' ? JSON.stringify(cur.theme) !== JSON.stringify(v) : cur[k] !== v) next[k] = v
    }
    if (Object.keys(next).length) this.term.options = next as ITerminalOptions
    if (prev.images !== settings.images) this.setImages(settings.images)
    if (prev.osc52 !== settings.osc52) this.setClipboardPolicy()
    if (prev.unicodeVersion !== settings.unicodeVersion) this.setUnicode()
    if (prev.renderer !== settings.renderer) {
      if (settings.renderer === 'dom') this.releaseWebgl()
      else if (this.visibleNow) this.enableWebgl()
    }
    if (prev.titleMode !== settings.titleMode) this.applyTitle()
    this.setInfo({ fontSize })
    if (next.fontFamily !== undefined || next.fontSize !== undefined || next.lineHeight !== undefined || next.letterSpacing !== undefined) {
      if (next.fontFamily !== undefined && this.host) {
        void ensureFontLoaded(settings.fontFamily, fontSize, [settings.fontWeight, settings.fontWeightBold], this.host.ownerDocument).then(() => {
          if (this.disposed) return
          this.term.options.fontFamily = settings.fontFamily
          this.scheduleFit(0)
        })
      }
      this.scheduleFit(0)
    }
  }

  /** Tab params changed (zoom, monitors, title, color...). */
  setParams(params: TerminalTabParams): void {
    const prev = this.params
    this.params = params
    if ((prev.zoom ?? 0) !== (params.zoom ?? 0)) {
      const fontSize = this.fontSize()
      if (this.term.options.fontSize !== fontSize) {
        this.term.options.fontSize = fontSize
        this.setInfo({ fontSize })
        this.scheduleFit(0)
      }
    }
    if (prev.title !== params.title) this.applyTitle()
    if (prev.monitorSilence !== params.monitorSilence) this.resetSilenceTimer()
    if (JSON.stringify(prev.fixedSize ?? null) !== JSON.stringify(params.fixedSize ?? null)) this.scheduleFit(0)
  }

  /** Toggle a fixed 80×24 terminal (CC-19); pane resizes no longer change the remote size while it is on. */
  toggleFixedSize(size: { cols: number; rows: number } = { cols: 80, rows: 24 }): void {
    const next = this.params.fixedSize ? undefined : size
    this.params = { ...this.params, fixedSize: next }
    updateTabParams<TerminalTabParams>(this.tabId, { fixedSize: next })
    this.scheduleFit(0)
  }

  isFixedSize(): boolean {
    return !!this.params.fixedSize
  }

  setConnection(conn: Connection | undefined): void {
    this.connection = conn
    this.setInfo({ encoding: this.encoding() })
    this.applyTitle()
  }

  /** Runtime session from the sessions cache (events socket). The socket's own state messages win while it is open. */
  setSession(s: RuntimeSession | undefined): void {
    this.runtime = s
    if (!s) return
    const patch: Partial<TerminalInfo> = { recording: !!s.recording, logging: !!s.logging }
    if (s.cwd && !this.live.cwd) patch.cwd = s.cwd
    if (this.live.transport !== 'open' && this.live.state !== 'gone') {
      this.applyState(s.state, s.stateMessage, s.exitCode)
    }
    this.setInfo(patch)
    this.applyTitle()
  }

  private encoding(): string {
    const e = this.connection?.options?.encoding ?? this.params.quick?.options?.encoding
    return typeof e === 'string' && e ? e.toUpperCase() : 'UTF-8'
  }

  private backspaceMode(): 'del' | 'ctrl-h' {
    const b = this.connection?.options?.backspace ?? this.params.quick?.options?.backspace
    return b === 'ctrl-h' ? 'ctrl-h' : 'del'
  }

  // ===================================================================================================================
  // addons
  // ===================================================================================================================

  private loadAddons(): void {
    const t = this.term
    t.loadAddon(this.fitAddon)
    t.loadAddon(this.searchAddon)
    t.loadAddon(this.serializeAddon)
    t.loadAddon(this.progressAddon)
    t.loadAddon(new Unicode11Addon())
    this.setUnicode()
    t.loadAddon(
      new WebLinksAddon((event, uri) => this.activateLink(event, uri), {
        hover: (event, text) => this.showLinkTooltip(event, text),
        leave: () => this.hideLinkTooltip(),
      }),
    )
    this.setImages(this.settings.images)
    this.setClipboardPolicy()
    this.disposables.push(
      this.progressAddon.onChange(({ state, value }) => {
        const v = Math.max(0, Math.min(100, value)) / 100
        setTabState(this.tabId, { progress: state === 0 ? null : state === 3 ? 'indeterminate' : v })
      }),
    )
  }

  private setUnicode(): void {
    try {
      this.term.unicode.activeVersion = this.settings.unicodeVersion === '6' ? '6' : '11'
    } catch {
      /* unsupported version */
    }
  }

  private setImages(on: boolean): void {
    if (on && !this.imageAddon) {
      try {
        this.imageAddon = new ImageAddon({
          enableSizeReports: true,
          pixelLimit: 4096 * 2160,
          storageLimit: 48,
          showPlaceholder: true,
          sixelSizeLimit: 12_000_000,
          iipSizeLimit: 12_000_000,
        })
        this.term.loadAddon(this.imageAddon)
      } catch (err) {
        console.warn('[terminal] image addon unavailable', err)
        this.imageAddon = null
      }
    } else if (!on && this.imageAddon) {
      this.safeDispose(() => this.imageAddon?.dispose())
      this.imageAddon = null
    }
  }

  private setClipboardPolicy(): void {
    this.safeDispose(() => this.clipboardAddon?.dispose())
    this.clipboardAddon = null
    if (this.settings.osc52 === 'off') return
    const provider: IClipboardProvider = {
      readText: (sel: ClipboardSelectionType) => this.osc52Read(sel),
      writeText: (sel: ClipboardSelectionType, text: string) => this.osc52Write(sel, text),
    }
    this.clipboardAddon = new ClipboardAddon(osc52Codec, provider)
    this.term.loadAddon(this.clipboardAddon)
  }

  /** OSC 52 query: denied unless the policy is read-write and the user agrees (the parser waits for the answer). */
  private async osc52Read(sel: ClipboardSelectionType): Promise<string> {
    if (sel !== 'c' || this.settings.osc52 !== 'read-write' || this.isReplaying() || this.live.readOnly) return ''
    // The parser waits for the answer: give up after 30 s (the dialog is withdrawn).
    const timer = setTimeout(() => this.dropUi('osc52-read'), 30_000)
    const ok = await this.requestUi<boolean>({ kind: 'osc52-read' })
    clearTimeout(timer)
    if (!ok || this.disposed) return ''
    const r = await readClipboard(this.win())
    return r.ok ? r.text.slice(0, OSC52_MAX_BYTES) : ''
  }

  /** OSC 52 copy from the remote application (TERM-18). Never blocks the parser. */
  private osc52Write(sel: ClipboardSelectionType, text: string): void {
    if (sel !== 'c' || this.settings.osc52 === 'off' || this.isReplaying() || !text) return
    const clipped = text.slice(0, OSC52_MAX_BYTES)
    if (this.settings.clipboardHistory) pushClipboardHistory(clipped, 'osc52', this.live.title)
    void writeClipboard(clipped, this.win()).then((ok) => {
      if (ok || this.disposed) return
      // The browser wants a user gesture: let the user click to finish the copy (latest copy only).
      this.dropUi('osc52-copy')
      void this.requestUi<boolean>({ kind: 'osc52-copy', text: clipped })
    })
  }

  // --- WebGL ---------------------------------------------------------------------------------------------------------

  private enableWebgl(): void {
    if (this.webgl || !this.opened || this.disposed || this.settings.renderer === 'dom') return
    if (Date.now() < this.webglRetryAt) return
    try {
      const addon = new WebglAddon()
      this.term.loadAddon(addon)
      this.webgl = addon
      reserveWebgl(this)
      this.webglLossListener = addon.onContextLoss(() => {
        // Context lost (GPU reset, too many contexts): fall back to the DOM renderer, retry later.
        this.webglRetryAt = Date.now() + 30_000
        this.releaseWebgl()
      })
      this.setInfo({ renderer: 'webgl' })
    } catch (err) {
      console.warn('[terminal] WebGL renderer unavailable, using the DOM renderer', err)
      this.webglRetryAt = Date.now() + 5 * 60_000
      this.webgl = null
      this.setInfo({ renderer: 'dom' })
    }
  }

  /** Drop the WebGL renderer (pool eviction, context loss, settings). */
  releaseWebgl(): void {
    const w = this.webgl
    this.webgl = null
    webglHolders.delete(this)
    this.webglLossListener?.dispose()
    this.webglLossListener = null
    if (w) this.safeDispose(() => w.dispose())
    if (!this.disposed) this.setInfo({ renderer: 'dom' })
  }

  // ===================================================================================================================
  // xterm listeners
  // ===================================================================================================================

  private installTerminalListeners(): void {
    const t = this.term
    this.disposables.push(
      t.onKey(({ key }) => {
        this.keyData = key
        queueMicrotask(() => {
          if (this.keyData === key) this.keyData = null
        })
      }),
      t.onData((data) => this.onXtermData(data)),
      t.onBinary((data) => {
        if (this.isReplaying() || this.live.readOnly) return
        const bytes = new Uint8Array(data.length)
        for (let i = 0; i < data.length; i++) bytes[i] = data.charCodeAt(i) & 0xff
        this.sendRaw(bytes)
      }),
      t.onTitleChange((title) => this.onOscTitle(title)),
      t.onBell(() => this.ringBell()),
      t.onResize(({ cols, rows }) => {
        this.setInfo({ cols, rows })
        this.scheduleResizeSend()
      }),
      t.onSelectionChange(() => this.onSelectionChange()),
      t.parser.registerOscHandler(133, (data) => this.onPromptMark(data)),
      // Registered after the progress addon: OSC 9;4 falls through to it.
      t.parser.registerOscHandler(9, (data) => this.onOsc9(data)),
      t.parser.registerOscHandler(777, (data) => this.onOsc777(data)),
    )
    t.attachCustomKeyEventHandler((e) => this.onKeyEvent(e))
  }

  private installHostListeners(host: HTMLElement): void {
    const on = <K extends keyof HTMLElementEventMap>(type: K, fn: (e: HTMLElementEventMap[K]) => void, opts: AddEventListenerOptions = { capture: true }) => {
      host.addEventListener(type, fn as EventListener, opts)
      this.cleanups.push(() => host.removeEventListener(type, fn as EventListener, opts))
    }
    on('paste', (e) => {
      // Every paste goes through the safety pipeline (xterm's own handler never sees it).
      e.preventDefault()
      e.stopPropagation()
      const text = e.clipboardData?.getData('text/plain') ?? ''
      if (text) void this.paste(text)
    })
    on('copy', (e) => {
      if (!this.term.hasSelection()) return
      e.preventDefault()
      e.stopPropagation()
      const text = this.selectionText()
      e.clipboardData?.setData('text/plain', text)
      if (this.settings.clipboardHistory) pushClipboardHistory(text, 'copy', this.live.title)
    })
    on('contextmenu', (e) => this.onContextMenuCapture(e))
    on('mousedown', (e) => {
      if (e.button === 1 && this.settings.middleClickPaste && !isLinux && this.term.modes.mouseTrackingMode === 'none') e.preventDefault()
    })
    on('auxclick', (e) => {
      if (e.button !== 1 || !this.settings.middleClickPaste || isLinux) return
      if (this.term.modes.mouseTrackingMode !== 'none' && !e.shiftKey) return
      e.preventDefault()
      e.stopPropagation()
      void this.pasteFromClipboard()
    })
    on(
      'wheel',
      (e) => {
        if (!this.settings.ctrlZoom || !(e.ctrlKey || e.metaKey)) return
        e.preventDefault()
        e.stopPropagation()
        this.zoomBy(e.deltaY < 0 ? 1 : -1)
      },
      { capture: true, passive: false },
    )
    on('compositionstart', () => {
      this.imeActive = true
    })
    on('compositionend', () => {
      // xterm finalises compositions in a setTimeout(0) of its own: clear the flag after that one.
      setTimeout(() => setTimeout(() => (this.imeActive = false), 0), 0)
    })
    on('input', (e) => {
      if ((e as InputEvent).inputType === 'insertText') {
        this.textInput = true
        setTimeout(() => (this.textInput = false), 0)
      }
    })
    on('focusin', () => {
      markTerminalFocused(this.tabId)
    })
    // CC-19: hide the pointer while typing (it reappears on the next mouse move).
    on('keydown', (e) => {
      if (!this.settings.hidePointerWhileTyping || e.metaKey || e.ctrlKey || e.altKey) return
      if (e.key.length === 1 || e.key === 'Enter' || e.key === 'Backspace') host.classList.add('nx-term-typing')
    })
    on('mousemove', () => host.classList.remove('nx-term-typing'))
    // CC-19: focus follows the mouse between terminal panes.
    on('pointerenter', (e) => {
      if (!this.settings.focusFollowsMouse || e.buttons || e.pointerType === 'touch') return
      const doc = host.ownerDocument
      if (!doc.hasFocus() || this.ui || !this.shouldTakeFocus()) return
      if (!this.isActiveTab()) focusTab(this.tabId)
      this.focus()
    })
  }

  private observeResize(host: HTMLElement): void {
    const RO = (host.ownerDocument.defaultView as (Window & typeof globalThis) | null)?.ResizeObserver ?? ResizeObserver
    this.resizeObserver?.disconnect()
    this.resizeObserver = new RO(() => {
      this.checkWindow()
      this.scheduleFit()
    })
    this.resizeObserver.observe(host)
    // A page (or pop-out) that becomes visible again, or a window resize, re-fits and re-checks once the layout
    // settled: ResizeObserver callbacks do not run in hidden / background documents.
    this.unwatchWindow?.()
    const doc = host.ownerDocument
    const win = doc.defaultView
    const onVisible = () => {
      if (doc.visibilityState === 'visible') {
        this.scheduleFit(0)
        this.settleFit()
      }
    }
    const onResize = () => this.scheduleFit()
    doc.addEventListener('visibilitychange', onVisible)
    win?.addEventListener('resize', onResize)
    this.unwatchWindow = () => {
      doc.removeEventListener('visibilitychange', onVisible)
      win?.removeEventListener('resize', onResize)
    }
  }

  /** Detect a move of the host element into another window (dockview pop-out / re-dock). */
  checkWindow(): void {
    const w = this.host?.ownerDocument.defaultView ?? null
    if (!w || w === this.currentWin) return
    this.currentWin = w
    this.onWindowChanged()
  }

  /**
   * The tab moved to another browser window (dockview pop-out) or back: re-open xterm there (window services), rebuild
   * the WebGL renderer for the new document and re-observe the size.
   */
  onWindowChanged(): void {
    if (!this.opened || !this.host || this.disposed) return
    // The WebGL context belongs to the old document, and the image addon wraps core.open()/setRenderer once per open()
    // (a second open would make its wrappers recurse): unload both around the re-open and load them again after.
    const hadWebgl = !!this.webgl
    const hadImages = !!this.imageAddon
    if (hadWebgl) this.releaseWebgl()
    if (hadImages) this.setImages(false)
    try {
      this.term.open(this.host)
    } catch (err) {
      console.warn('[terminal] re-open in new window failed', err)
    }
    if (hadImages) this.setImages(true)
    // WebGL availability can differ per window: allow a fresh attempt.
    this.webglRetryAt = 0
    if (this.visibleNow || hadWebgl) this.enableWebgl()
    this.observeResize(this.host)
    this.scheduleFit(30)
  }

  // ===================================================================================================================
  // visibility / focus / sizing
  // ===================================================================================================================

  setVisible(visible: boolean): void {
    if (this.visibleNow === visible) return
    this.visibleNow = visible
    this.lastVisibleAt = Date.now()
    if (visible) {
      this.enableWebgl()
      this.scheduleFit(0)
      this.settleFit()
    }
  }

  private isActiveTab(): boolean {
    return useWorkspaceStore.getState().activeTabId === this.tabId
  }

  /** Taking focus on activation must not steal it from inputs or dialogs. */
  private shouldTakeFocus(): boolean {
    const doc = this.host?.ownerDocument ?? document
    const a = doc.activeElement
    if (!a || a === doc.body) return true
    if (a.closest('[role="dialog"],[role="menu"],[cmdk-root]')) return false
    return !!a.closest('.dv-dockview, [data-workspace]')
  }

  /** The tab became the active one: focus the terminal. */
  onActivated(): void {
    if (this.shouldTakeFocus()) this.focus()
  }

  focus(): void {
    if (this.opened && !this.disposed) this.term.focus()
  }

  scheduleFit(delay = 50): void {
    if (this.fitTimer) clearTimeout(this.fitTimer)
    this.fitTimer = setTimeout(() => {
      this.fitTimer = null
      this.fitNow()
    }, delay)
  }

  /**
   * Extra fits shortly after the terminal was opened / shown: a restored dock layout, late fonts or a page loaded in a
   * background tab can report an intermediate pane size first (the "3 rows after a reload" bug). fitNow is idempotent.
   */
  private settleFit(): void {
    for (const t of this.settleTimers) clearTimeout(t)
    this.settleTimers = SETTLE_FIT_DELAYS.map((ms) => setTimeout(() => this.fitNow(), ms))
  }

  private fitNow(): void {
    if (!this.opened || this.disposed || !this.host) return
    const fixed = this.params.fixedSize
    if (fixed && !this.live.readOnly) {
      const cols = Math.max(2, Math.min(1000, Math.floor(fixed.cols) || 80))
      const rows = Math.max(1, Math.min(1000, Math.floor(fixed.rows) || 24))
      if (cols !== this.term.cols || rows !== this.term.rows) this.term.resize(cols, rows)
      else if (!this.sentSize) this.scheduleResizeSend()
      return
    }
    if (this.live.readOnly && this.live.ptyCols && this.live.ptyRows) {
      if (this.term.cols !== this.live.ptyCols || this.term.rows !== this.live.ptyRows) this.term.resize(this.live.ptyCols, this.live.ptyRows)
      return
    }
    const rect = this.host.getBoundingClientRect()
    // Hidden tabs have no size: never resize the PTY to 0×0 (TERM-3).
    if (rect.width < 20 || rect.height < 20) return
    let dims: { cols: number; rows: number } | undefined
    try {
      dims = this.fitAddon.proposeDimensions()
    } catch {
      dims = undefined
    }
    if (!dims || !Number.isFinite(dims.cols) || !Number.isFinite(dims.rows)) return
    const cols = Math.max(2, Math.min(1000, dims.cols))
    const rows = Math.max(1, Math.min(1000, dims.rows))
    // A tiny pane is usually a layout in progress: retry for a while before shrinking the remote PTY to it.
    if ((rows < MIN_SETTLED_ROWS || cols < MIN_SETTLED_COLS) && this.fitRetries < FIT_SETTLE_RETRIES) {
      this.fitRetries++
      this.scheduleFit(FIT_RETRY_MS)
      return
    }
    this.fitRetries = 0
    if (cols !== this.term.cols || rows !== this.term.rows) {
      try {
        this.fitAddon.fit()
      } catch {
        this.term.resize(cols, rows)
      }
    } else if (!this.sentSize) {
      this.scheduleResizeSend()
    }
  }

  private scheduleResizeSend(): void {
    if (this.resizeSendTimer) clearTimeout(this.resizeSendTimer)
    this.resizeSendTimer = setTimeout(() => {
      this.resizeSendTimer = null
      this.sendResize()
    }, 40)
  }

  private sendResize(force = false): void {
    if (this.live.readOnly || !this.opened) return
    const cols = this.term.cols
    const rows = this.term.rows
    if (!force && this.sentSize && this.sentSize.cols === cols && this.sentSize.rows === rows) return
    if (this.socket.sendControl({ type: 'resize', cols, rows })) {
      this.sentSize = { cols, rows }
      // The server applies our size (we are now the most recent writer) and only notifies the other clients.
      this.setInfo({ ptyCols: cols, ptyRows: rows })
    }
  }

  // ===================================================================================================================
  // socket events
  // ===================================================================================================================

  private onTransport(state: TransportState, attempt: number): void {
    this.setInfo({ transport: state, transportAttempts: attempt })
    if (state === 'open') {
      this.sentSize = null
      this.sendResize(true)
    }
  }

  private onGone(): void {
    this.applyState('gone', 'This session no longer exists on the server.')
    dropSavedTerminal(this.sessionId)
  }

  private onSocketMessage(msg: TerminalServerMessage): void {
    switch (msg.type) {
      case 'attach':
        this.onAttach(msg.mode, msg.from, msg.head)
        break
      case 'attach-end':
        this.attachResizePending = false
        break
      case 'state':
        this.applyState(msg.state, msg.message, msg.exitCode)
        break
      case 'title':
        if (typeof msg.title === 'string') this.onOscTitle(msg.title)
        break
      case 'cwd':
        if (typeof msg.path === 'string') this.setInfo({ cwd: msg.path })
        break
      case 'readonly':
        this.setReadOnly(!!msg.value)
        break
      case 'resize':
        this.onPtyResize(msg.cols, msg.rows)
        break
      case 'shadow':
        // Administrators viewing this session read-only (the server tells only the owner's views).
        this.setInfo({ shadowedBy: msg.viewers?.length ? msg.viewers.map(String) : undefined })
        break
      case 'error':
        if (typeof msg.message === 'string') {
          this.setInfo({ error: msg.message })
          // One toast per terminal at a time (sonner id), so a chatty server cannot flood the screen.
          toast.error(this.live.title, { id: `term-error:${this.tabId}`, description: msg.message.slice(0, 300) })
        }
        break
      case 'bell':
        // Rendered bells come from xterm's parser (in sync with the output); the server event is redundant here.
        break
      case 'prompt-mark':
        // Prompt marks are tracked by the OSC 133 parser hook (exact buffer positions).
        break
    }
  }

  private onAttach(mode: 'delta' | 'reset', from: number, head: number): void {
    this.attachResizePending = true
    const f = Number.isFinite(from) && from >= 0 ? Math.floor(from) : 0
    const h = Number.isFinite(head) && head >= f ? Math.floor(head) : f
    // Replayed bytes are history (replies / bells / notifications suppressed) when this view starts from scratch on an
    // existing session, or when the server had to reset us. Deltas after our own earlier render point, and the first
    // output of a session this page just created, are live: applications may be waiting for our replies.
    const history = mode === 'reset' || (this.requestedOffset === 0 && !isFreshSession(this.sessionId))
    this.replayHead = history ? h : f
    this.skipBytes = 0
    if (mode === 'reset') {
      this.scanner.reset()
      this.enqueue({ kind: 'reset', offset: f })
      this.receivedOffset = f
    } else if (f < this.receivedOffset) {
      // The server resumes earlier than what we already have: drop the duplicate prefix.
      this.skipBytes = this.receivedOffset - f
    } else {
      this.receivedOffset = f
    }
    // The server counts this client's acknowledgements from the attach offset.
    this.ackedOffset = f
    if (history && h > f) this.setInfo({ replaying: true })
  }

  private onSocketData(chunk: Uint8Array): void {
    let data = chunk
    if (this.skipBytes > 0) {
      if (data.length <= this.skipBytes) {
        this.skipBytes -= data.length
        return
      }
      data = data.subarray(this.skipBytes)
      this.skipBytes = 0
    }
    const start = this.receivedOffset
    const end = start + data.length
    this.receivedOffset = end
    const ground = this.scanner.feed(data)
    this.enqueue({ kind: 'data', bytes: data, end, ground, replay: start < this.replayHead })
  }

  // ===================================================================================================================
  // output pipeline
  // ===================================================================================================================

  private enqueue(item: OutputItem): void {
    if (this.paused) {
      this.pausedQueue.push(item)
      if (item.kind === 'data') this.setInfo({ pausedBytes: this.live.pausedBytes + item.bytes.length })
      return
    }
    this.process(item)
  }

  private process(item: OutputItem): void {
    if (this.disposed) return
    if (item.kind === 'reset') {
      const entry: Pending = { end: item.offset, replay: true, ground: true }
      this.pushPending(entry)
      this.term.write(EMPTY, () => {
        this.term.reset()
        this.afterReset()
        this.onParsed(entry)
      })
      return
    }
    if (item.kind === 'local') {
      const entry: Pending = { end: null, replay: false, ground: this.groundAtRendered }
      this.pushPending(entry)
      this.term.write(item.data, () => this.onParsed(entry))
      return
    }
    const raw = item.bytes
    for (const cb of Array.from(this.outputObservers)) {
      try {
        cb(raw)
      } catch (err) {
        console.error('[terminal] output observer failed', err)
      }
    }
    if (hasTerminalOutputListeners()) emitTerminalOutput({ tabId: this.tabId, sessionId: this.sessionId, data: raw, replay: item.replay })
    let bytes: Uint8Array = raw
    for (const f of this.outputFilters) {
      try {
        const r = f(bytes)
        bytes = r ?? EMPTY
      } catch (err) {
        console.error('[terminal] output filter failed', err)
      }
    }
    const entry: Pending = { end: item.end, replay: item.replay, ground: item.ground }
    this.pushPending(entry)
    this.term.write(bytes, () => this.onParsed(entry))
    if (!item.replay) this.onLiveOutput()
  }

  private pushPending(p: Pending): void {
    this.pending.push(p)
    if (p.replay) this.pendingReplay++
  }

  private onParsed(entry: Pending): void {
    const i = this.pending.indexOf(entry)
    if (i >= 0) this.pending.splice(i, 1)
    if (entry.replay) this.pendingReplay = Math.max(0, this.pendingReplay - 1)
    if (entry.end !== null) {
      this.renderedOffset = entry.end
      this.groundAtRendered = entry.ground
      this.maybeAck()
      if (!this.live.hasOutput && entry.end > 0) this.setInfo({ hasOutput: true })
    }
    if (this.live.replaying && this.pendingReplay === 0 && this.renderedOffset >= this.replayHead) this.setInfo({ replaying: false })
  }

  /** True while xterm is parsing bytes replayed from the ring buffer (or a restored snapshot). */
  isReplaying(): boolean {
    return this.pending.length > 0 && this.pending[0].replay
  }

  private maybeAck(force = false): void {
    const unacked = this.renderedOffset - this.ackedOffset
    if (unacked <= 0) return
    if (unacked >= ACK_STEP || force) {
      if (this.socket.sendControl({ type: 'ack', offset: this.renderedOffset })) this.ackedOffset = this.renderedOffset
      if (this.ackTimer) {
        clearTimeout(this.ackTimer)
        this.ackTimer = null
      }
      return
    }
    if (!this.ackTimer) {
      this.ackTimer = setTimeout(() => {
        this.ackTimer = null
        this.maybeAck(true)
      }, 100)
    }
  }

  private afterReset(): void {
    for (const m of this.promptMarks.splice(0)) this.safeDispose(() => m.dispose())
    this.commandStartAt = null
    try {
      this.searchAddon.clearDecorations()
    } catch {
      /* not opened */
    }
    try {
      this.progressAddon.progress = { state: 0, value: 0 }
    } catch {
      /* ignore */
    }
    this.imageAddon?.reset()
    this.setInfo({ oscTitle: '' })
    this.applyTitle()
  }

  private writeLocalInternal(data: string | Uint8Array, snapshot = false): void {
    if (snapshot) {
      const entry: Pending = { end: null, replay: true, ground: true }
      this.pushPending(entry)
      this.term.write(data, () => this.onParsed(entry))
      return
    }
    this.enqueue({ kind: 'local', data })
  }

  /** Write locally (not sent to the session). */
  writeLocal(data: string | Uint8Array): void {
    if (!this.disposed) this.writeLocalInternal(data)
  }

  /** Pause / resume rendering (CC-10). While paused, chunks and acks are held so the server throttles the remote. */
  togglePause(on?: boolean): void {
    const next = on ?? !this.paused
    if (next === this.paused) return
    this.paused = next
    if (!next) {
      const queued = this.pausedQueue
      this.pausedQueue = []
      for (const item of queued) this.process(item)
    }
    this.setInfo({ paused: next, pausedBytes: next ? this.live.pausedBytes : 0 })
  }

  // ===================================================================================================================
  // session state
  // ===================================================================================================================

  private applyState(state: TerminalSessionState, message?: string, exitCode?: number): void {
    const prev = this.live.state
    if (prev === 'gone' && state !== 'gone') return
    this.setInfo({ state, stateMessage: message, exitCode })
    this.applyTabStatus()
    if (prev === state) return
    // (The server prints its own in-band "reconnected" separator, so nothing is written locally here.)
    // Lost connections / errors are announced when the user is not looking at this terminal (a clean exit is not).
    if ((state === 'disconnected' || state === 'error') && RUNNING.has(prev) && (state === 'error' || !!message)) {
      const background = !this.visibleNow || !this.isActiveTab()
      if (background || !pageHasAttention(this.host?.ownerDocument)) {
        notify({
          key: `disconnect:${this.tabId}`,
          title: state === 'error' ? `Session error: ${this.live.title}` : `Disconnected: ${this.live.title}`,
          body: message,
          onClick: () => focusTab(this.tabId),
          // A visible tab already shows the end-of-session panel: only a desktop notification is useful then.
          toastFallback: background,
        })
      }
    }
    // A process that exited (EOF with an exit status) ends in 'disconnected' with an exit code and no error message;
    // 'closed' means the session was closed explicitly (e.g. from another window).
    if (state === 'disconnected' && RUNNING.has(prev) && exitCode !== undefined && exitCode !== null && !message) this.maybeCloseOnExit(exitCode)
    if (state === 'closed' && this.settings.closeOnExit === 'always') this.maybeCloseOnExit(exitCode)
    // Nothing more will arrive: show everything that was held back.
    if (ENDED.has(state) && this.paused) this.togglePause(false)
  }

  private applyTabStatus(): void {
    setTabState(this.tabId, { status: statusFor(this.live.state) })
  }

  private maybeCloseOnExit(exitCode?: number): void {
    const mode = this.settings.closeOnExit
    if (mode === 'never') return
    if (mode === 'clean' && exitCode !== 0) return
    if (this.closeOnExitTimer) clearTimeout(this.closeOnExitTimer)
    this.closeOnExitTimer = setTimeout(() => void closeTab(this.tabId, { force: true }), 400)
  }

  private setReadOnly(ro: boolean): void {
    this.term.options.disableStdin = ro
    this.setInfo({ readOnly: ro })
    this.scheduleFit(0)
  }

  private onPtyResize(cols: number, rows: number): void {
    if (!Number.isFinite(cols) || !Number.isFinite(rows) || cols < 2 || rows < 1) return
    if (this.attachResizePending) {
      // The size in the attach burst predates the resize we send when the socket opens: writers keep their own size.
      this.attachResizePending = false
      if (!this.live.readOnly && this.sentSize) return
    }
    this.setInfo({ ptyCols: Math.min(1000, cols), ptyRows: Math.min(1000, rows) })
    if (this.live.readOnly) this.scheduleFit(0)
  }

  /** The session ended / vanished and the end-of-session prompt is shown (PROTO-39). */
  endPromptActive(): boolean {
    return this.settings.showEndPrompt && ENDED.has(this.live.state)
  }

  isRunning(): boolean {
    return RUNNING.has(this.live.state)
  }

  // ===================================================================================================================
  // titles, bells, activity, notifications
  // ===================================================================================================================

  private baseTitle(): string {
    return this.connection?.name || this.params.title || this.runtime?.title || this.params.quick?.name || 'Terminal'
  }

  private onOscTitle(raw: string): void {
    const t = sanitizeTitle(raw)
    if (t === this.live.oscTitle) return
    this.setInfo({ oscTitle: t })
    this.applyTitle()
  }

  private applyTitle(): void {
    const base = this.baseTitle()
    const osc = this.live.oscTitle
    let title = base
    if (this.settings.titleMode === 'osc' && osc) title = osc
    else if (this.settings.titleMode === 'both' && osc && osc !== base) title = `${base} — ${osc}`
    this.setInfo({ title })
    setTabTitle(this.tabId, title)
  }

  private ringBell(): void {
    if (this.isReplaying()) return
    const now = Date.now()
    if (now - this.lastBellAt < 150) return
    this.lastBellAt = now
    const style = this.settings.bell
    if (style === 'visual' || style === 'both') for (const l of Array.from(this.bellListeners)) l()
    if (style === 'sound' || style === 'both') playBell()
    const background = !this.visibleNow || !this.isActiveTab()
    if (background) setTabState(this.tabId, { activity: true })
    if (this.settings.bellNotify && (background || !pageHasAttention(this.host?.ownerDocument))) {
      notify({ key: `bell:${this.tabId}`, title: `Bell in ${this.live.title}`, onClick: () => focusTab(this.tabId), minIntervalMs: 10_000, toastFallback: background })
    }
  }

  private onLiveOutput(): void {
    const background = !this.visibleNow || !this.isActiveTab()
    if (background) {
      const st = useWorkspaceStore.getState().tabStates[this.tabId]
      if (!st?.activity) setTabState(this.tabId, { activity: true })
      if (this.params.monitorActivity) {
        notify({ key: `activity:${this.tabId}`, title: `Activity in ${this.live.title}`, onClick: () => focusTab(this.tabId), minIntervalMs: 30_000 })
      }
    }
    if (this.params.monitorSilence) this.resetSilenceTimer()
  }

  private resetSilenceTimer(): void {
    if (this.silenceTimer) clearTimeout(this.silenceTimer)
    this.silenceTimer = null
    if (!this.params.monitorSilence || this.disposed) return
    const secs = this.settings.silenceSeconds
    this.silenceTimer = setTimeout(() => {
      this.silenceTimer = null
      if (!this.isRunning()) return
      notify({ key: `silence:${this.tabId}`, title: `No output for ${secs}s in ${this.live.title}`, onClick: () => focusTab(this.tabId), minIntervalMs: 1000 })
    }, secs * 1000)
  }

  /** OSC 133 shell integration marks (TERM-22, TERM-27). */
  private onPromptMark(data: string): boolean {
    const [kind, ...rest] = data.split(';')
    if (kind === 'A') {
      try {
        const m = this.term.registerMarker(0)
        if (m) {
          this.promptMarks.push(m)
          if (this.promptMarks.length > MAX_PROMPT_MARKS) this.promptMarks.shift()?.dispose()
        }
      } catch {
        /* alternate buffer */
      }
    } else if (kind === 'C') {
      this.commandStartAt = this.isReplaying() ? null : Date.now()
    } else if (kind === 'D') {
      const started = this.commandStartAt
      this.commandStartAt = null
      if (started !== null && !this.isReplaying() && this.settings.notifyLongCommands) {
        const secs = (Date.now() - started) / 1000
        const exit = Number.parseInt(rest[0] ?? '', 10)
        const background = !this.visibleNow || !this.isActiveTab()
        if (secs >= this.settings.longCommandSeconds && (background || !pageHasAttention(this.host?.ownerDocument))) {
          notify({
            toastFallback: background,
            key: `cmd:${this.tabId}`,
            title: `Command finished in ${this.live.title}`,
            body: `${Number.isFinite(exit) ? `Exit code ${exit} · ` : ''}${Math.round(secs)}s`,
            onClick: () => focusTab(this.tabId),
            minIntervalMs: 2000,
          })
        }
      }
    }
    return true
  }

  /** OSC 9 notifications (iTerm2 style); OSC 9;4 progress falls through to the progress addon, OSC 9;9 is a cwd. */
  private onOsc9(data: string): boolean {
    if (data.startsWith('4;')) return false
    if (/^\d+;/.test(data)) return true
    if (!this.isReplaying() && this.settings.osc9Notifications && data.trim()) {
      notify({ key: `osc9:${this.tabId}`, title: this.live.title, body: data, onClick: () => focusTab(this.tabId), minIntervalMs: 3000 })
    }
    return true
  }

  /** OSC 777;notify;title;body (rxvt / Ghostty style). */
  private onOsc777(data: string): boolean {
    const [cmd, title, ...body] = data.split(';')
    if (cmd !== 'notify') return true
    if (!this.isReplaying() && this.settings.osc9Notifications) {
      notify({ key: `osc777:${this.tabId}`, title: title || this.live.title, body: body.join(';'), onClick: () => focusTab(this.tabId), minIntervalMs: 3000 })
    }
    return true
  }

  /** Scroll to the previous / next shell prompt (OSC 133 A marks). */
  jumpToPrompt(dir: -1 | 1): boolean {
    const buf = this.term.buffer.active
    if (buf.type !== 'normal') return false
    const top = buf.viewportY
    const lines = this.promptMarks.filter((m) => !m.isDisposed && m.line >= 0).map((m) => m.line)
    const target = dir < 0 ? lines.filter((l) => l < top).pop() : lines.find((l) => l > top)
    if (target === undefined) return false
    this.term.scrollToLine(target)
    return true
  }

  hasPromptMarks(): boolean {
    return this.promptMarks.some((m) => !m.isDisposed)
  }

  // ===================================================================================================================
  // input
  // ===================================================================================================================

  private onXtermData(data: string): void {
    const user = this.isUserData(data)
    if (!user) {
      // Replies to queries replayed from the past must not reach the remote application.
      if (this.isReplaying() || this.live.readOnly) return
      this.sendRaw(data)
      return
    }
    this.userInput(data)
  }

  private isUserData(data: string): boolean {
    if (this.programmatic > 0) return true
    if (this.keyData !== null) {
      const k = this.keyData
      this.keyData = null
      if (k === data) return true
    }
    if (this.imeActive || this.textInput) return !REPORT_RE.test(data)
    return false
  }

  /** User input: MultiExec fan-out, observers, then the socket. */
  private userInput(data: string): void {
    if (this.live.readOnly || !data) return
    if (this.endPromptActive()) return
    const targets = isParticipant(this.tabId) ? multiExecTargets() : []
    if (targets.length > 1 || (targets.length === 1 && targets[0].tabId !== this.tabId)) {
      for (const t of targets) {
        const ok = t === this ? this.sendRaw(data) : t.send(data)
        if (ok) emitTerminalInput({ tabId: t.tabId, sessionId: t.sessionId, data, broadcast: t !== this })
      }
      if (!targets.includes(this)) {
        // A focused terminal that is excluded from MultiExec still gets its own input.
        if (this.sendRaw(data)) emitTerminalInput({ tabId: this.tabId, sessionId: this.sessionId, data, broadcast: false })
      }
    } else if (this.sendRaw(data)) {
      emitTerminalInput({ tabId: this.tabId, sessionId: this.sessionId, data, broadcast: false })
    }
    for (const cb of Array.from(this.inputObservers)) {
      try {
        cb(data)
      } catch (err) {
        console.error('[terminal] input observer failed', err)
      }
    }
  }

  private sendRaw(data: string | Uint8Array): boolean {
    if (this.disposed || this.live.readOnly) return false
    if (this.socket.sendInput(data)) {
      // Typing makes this client the most recent writer: the server switches the PTY back to our size.
      if (this.sentSize && (this.live.ptyCols !== this.sentSize.cols || this.live.ptyRows !== this.sentSize.rows)) {
        this.setInfo({ ptyCols: this.sentSize.cols, ptyRows: this.sentSize.rows })
      }
      return true
    }
    const now = Date.now()
    if (now - this.droppedInputAt > 4000) {
      this.droppedInputAt = now
      toast.warning(`${this.live.title} is not connected`, { description: 'Input was discarded while the connection is re-established.' })
    }
    return false
  }

  /** TerminalHandle.send — raw input to this session only (refused while a plugin holds the input lock). */
  send(data: string | Uint8Array): boolean {
    if (this.inputBlocked()) return false
    return this.sendRaw(data)
  }

  /** TerminalHandle.input — as if typed by the user (refused while a plugin holds the input lock). */
  input(data: string): void {
    if (this.inputBlocked()) return
    this.withProgrammatic(() => this.userInput(data))
  }

  /** TerminalHandle.inputLocked — why input from other sources is refused right now (null = it is not). */
  inputLocked(): string | null {
    return this.inputLocks.reason
  }

  /** True (and a calm, throttled hint) when input from another source must not reach the session now. */
  private inputBlocked(): boolean {
    const reason = this.inputLocks.reason
    if (!reason) return false
    const now = Date.now()
    if (now - this.blockedInputAt > 4000) {
      this.blockedInputAt = now
      toast.info(`${this.live.title}: input held back`, { id: `term-inputlock:${this.tabId}`, description: reason })
    }
    return true
  }

  private withProgrammatic<T>(fn: () => T): T {
    this.programmatic++
    try {
      return fn()
    } finally {
      this.programmatic--
    }
  }

  private swallow(e: KeyboardEvent): false {
    e.preventDefault()
    e.stopPropagation()
    this.swallowKeypress = true
    return false
  }

  /** attachCustomKeyEventHandler: end-of-session keys, zoom, backspace mode, Alt+arrows, Scroll Lock. */
  private onKeyEvent(e: KeyboardEvent): boolean {
    if (e.type === 'keypress') {
      if (this.swallowKeypress) {
        this.swallowKeypress = false
        return false
      }
      return true
    }
    if (e.type !== 'keydown') return true
    this.swallowKeypress = false
    const s = this.settings
    if (this.endPromptActive() && !e.ctrlKey && !e.metaKey && !e.altKey && !e.isComposing) {
      const k = e.key.toLowerCase()
      if (k === 'r') {
        this.reconnect()
        return this.swallow(e)
      }
      if (k === 'd') {
        void duplicateSession(this.tabId)
        return this.swallow(e)
      }
      if (k === 's') {
        this.saveOutput('text')
        return this.swallow(e)
      }
      if (k === 'q' || e.key === 'Enter' || e.key === 'Escape') {
        void closeTab(this.tabId, { force: true })
        return this.swallow(e)
      }
      if (e.key.length === 1) return this.swallow(e)
    }
    if (s.ctrlZoom && (e.ctrlKey || e.metaKey) && !e.altKey) {
      if (e.key === '=' || e.key === '+' || e.code === 'NumpadAdd') {
        this.zoomBy(1)
        return this.swallow(e)
      }
      if (e.key === '-' || e.key === '_' || e.code === 'NumpadSubtract') {
        this.zoomBy(-1)
        return this.swallow(e)
      }
      if ((e.key === '0' && !e.shiftKey) || e.code === 'Numpad0') {
        this.zoomReset()
        return this.swallow(e)
      }
    }
    if (e.key === 'ScrollLock') {
      this.togglePause()
      return this.swallow(e)
    }
    if (e.key === 'Backspace' && !e.altKey && !e.metaKey && this.backspaceMode() === 'ctrl-h') {
      this.inputKey(e.ctrlKey ? '\x7f' : '\b')
      return this.swallow(e)
    }
    if (s.altArrowWordJump && e.altKey && !e.ctrlKey && !e.metaKey && !e.shiftKey && (e.key === 'ArrowLeft' || e.key === 'ArrowRight')) {
      this.inputKey(e.key === 'ArrowLeft' ? '\x1bb' : '\x1bf')
      return this.swallow(e)
    }
    return true
  }

  private inputKey(data: string): void {
    if (this.settings.scrollOnUserInput) this.term.scrollToBottom()
    this.input(data)
  }

  // ===================================================================================================================
  // selection, copy & paste
  // ===================================================================================================================

  getSelection(): string {
    return this.term.getSelection()
  }

  private selectionText(): string {
    const text = this.term.getSelection()
    if (!this.settings.trimCopiedWhitespace) return text
    return text
      .split(/\r?\n/)
      .map((l) => l.replace(/[ \t]+$/, ''))
      .join('\n')
  }

  async copySelection(source: 'copy' | 'select' = 'copy'): Promise<boolean> {
    if (!this.term.hasSelection()) return false
    const text = this.selectionText()
    if (!text) return false
    const ok = await writeClipboard(text, this.win())
    if (ok && this.settings.clipboardHistory) pushClipboardHistory(text, source, this.live.title)
    if (!ok && source === 'copy') toast.error('Could not copy to the clipboard')
    return ok
  }

  private onSelectionChange(): void {
    if (!this.settings.copyOnSelect) return
    if (this.copyOnSelectTimer) clearTimeout(this.copyOnSelectTimer)
    this.copyOnSelectTimer = setTimeout(() => {
      this.copyOnSelectTimer = null
      if (this.term.hasSelection()) void this.copySelection('select')
    }, 180)
  }

  selectAll(): void {
    this.term.selectAll()
  }

  private onContextMenuCapture(e: MouseEvent): void {
    const modifier = e.shiftKey || e.ctrlKey || e.metaKey
    // Mouse-aware applications (mc, htop, vim mouse=a) get the right button; Shift opens our menu.
    if (!e.shiftKey && this.term.modes.mouseTrackingMode !== 'none') {
      e.preventDefault()
      e.stopPropagation()
      return
    }
    if (modifier) return
    const action = this.settings.rightClickAction
    if (action === 'menu') return
    e.preventDefault()
    e.stopPropagation()
    if (action === 'copy-or-paste' && this.term.hasSelection()) {
      void this.copySelection().then(() => this.term.clearSelection())
      return
    }
    void this.pasteFromClipboard()
  }

  /** Paste the clipboard (async clipboard API; falls back to the latest clipboard-history entry). */
  async pasteFromClipboard(opts: TerminalPasteOptions = {}): Promise<boolean> {
    const r = await readClipboard(this.win())
    if (r.ok) {
      if (!r.text) return false
      return this.paste(r.text, opts)
    }
    toast.error(r.reason === 'denied' ? 'Clipboard access was blocked' : 'The clipboard cannot be read here', {
      description: `Paste with ${isMac ? '⌘V' : 'Ctrl+Shift+V or Shift+Insert'} instead, or allow clipboard access for this site.`,
    })
    return false
  }

  /** Paste through the safety pipeline (TERM-17). */
  async paste(text: string, opts: TerminalPasteOptions = {}): Promise<boolean> {
    if (this.disposed || !text) return false
    if (this.inputBlocked()) return false
    if (this.live.readOnly) {
      toast.info('This terminal is read-only')
      return false
    }
    if (this.endPromptActive()) return false
    const s = this.settings
    const analysis = analyzePaste(text)
    const bracketed = s.bracketedPaste && this.term.modes.bracketedPasteMode
    const targets = isParticipant(this.tabId) ? Math.max(1, multiExecTargets().length) : 1
    const dangerous = analysis.severity === 'danger' || analysis.severity === 'warning'
    const needConfirm =
      !opts.skipConfirm &&
      ((s.pasteConfirmDangerous && dangerous) ||
        (s.pasteConfirmMultiline && analysis.multiline && !(s.pasteSkipConfirmBracketed && bracketed)) ||
        (targets > 1 && analysis.multiline))
    let finalText = text
    let mode = opts.mode ?? 'normal'
    let stripHidden = true
    if (needConfirm) {
      const d = await this.requestUi<PasteDecision | null>({ kind: 'paste', text, analysis, targets, bracketed })
      if (!d || this.disposed) return false
      finalText = d.text
      mode = d.mode
      stripHidden = d.stripHidden
      if (d.dontAskMultiline) terminalSettings.set({ pasteConfirmMultiline: false })
    }
    finalText = sanitizePaste(finalText, { stripHidden })
    if (!finalText) return false
    if (mode === 'paced') {
      await this.pacedPaste(finalText, opts.lineDelayMs ?? s.pasteLineDelayMs)
    } else {
      this.withProgrammatic(() => this.term.paste(finalText))
    }
    this.focus()
    return true
  }

  /** Line-by-line paste with a delay (slow devices, TERM-17). Cancelled with `cancelPaste()`. */
  private async pacedPaste(text: string, delayMs: number): Promise<void> {
    const lines = splitPasteLines(text)
    const endsWithNewline = /\r?\n$/.test(text)
    const delay = Math.max(10, Math.min(LIMITS.pasteLineDelayMs[1], delayMs || 50))
    if (this.pasteAbort) this.pasteAbort.aborted = true
    const abort = { aborted: false }
    this.pasteAbort = abort
    this.setInfo({ pasting: { done: 0, total: lines.length } })
    try {
      for (let i = 0; i < lines.length; i++) {
        if (abort.aborted || this.disposed || ENDED.has(this.live.state)) break
        const eol = i < lines.length - 1 || endsWithNewline ? '\r' : ''
        this.input(lines[i] + eol)
        this.setInfo({ pasting: { done: i + 1, total: lines.length } })
        if (i < lines.length - 1) await new Promise((r) => setTimeout(r, delay))
      }
    } finally {
      if (this.pasteAbort === abort) this.pasteAbort = null
      if (!this.disposed) this.setInfo({ pasting: null })
    }
  }

  cancelPaste(): void {
    if (this.pasteAbort) this.pasteAbort.aborted = true
  }

  /** Clipboard history picker (CC-20). */
  openClipboardHistory(): void {
    void this.requestUi<boolean>({ kind: 'history' })
  }

  // ===================================================================================================================
  // links
  // ===================================================================================================================

  private activateLink(event: MouseEvent, uri: string): void {
    if (this.settings.linkModifier === 'mod' && !(event.ctrlKey || event.metaKey)) return
    let url: URL
    try {
      url = new URL(uri)
    } catch {
      return
    }
    const scheme = url.protocol.toLowerCase()
    if (['javascript:', 'data:', 'vbscript:', 'blob:', 'about:', 'chrome:', 'view-source:'].includes(scheme)) {
      toast.error('Blocked a potentially unsafe link', { description: uri.slice(0, 200) })
      return
    }
    void this.openUrl(url, uri)
  }

  private async openUrl(url: URL, raw: string): Promise<void> {
    const scheme = url.protocol.toLowerCase()
    const web = scheme === 'http:' || scheme === 'https:'
    if (web) {
      if (generalSettings.get().openLinks === 'ask') {
        const ok = await this.requestUi<boolean>({ kind: 'link', uri: raw, scheme, action: 'open' })
        if (!ok) return
      }
      this.win().open(url.href, '_blank', 'noopener,noreferrer')
      return
    }
    if (['ssh:', 'telnet:', 'sftp:', 'rdp:', 'vnc:', 'ftp:', 'ftps:'].includes(scheme)) {
      if (!isCommandEnabled('sessions.quickConnect')) {
        toast.info('Quick connect is not available')
        return
      }
      const ok = await this.requestUi<boolean>({ kind: 'link', uri: raw, scheme, action: 'connect' })
      if (ok) void runCommand('sessions.quickConnect', raw, { source: 'api' })
      return
    }
    if (scheme === 'file:') {
      if (!isCommandEnabled('files.openForSession')) {
        toast.info('Opening files from the terminal is not available')
        return
      }
      const ok = await this.requestUi<boolean>({ kind: 'link', uri: raw, scheme, action: 'files' })
      if (ok) void runCommand('files.openForSession', { sessionId: this.sessionId, path: safeDecode(url.pathname) }, { source: 'api' })
      return
    }
    const ok = await this.requestUi<boolean>({ kind: 'link', uri: raw, scheme, action: 'open' })
    if (ok) this.win().open(url.href, '_blank', 'noopener,noreferrer')
  }

  private showLinkTooltip(event: MouseEvent, text: string): void {
    const el = this.term.element
    if (!el) return
    this.hideLinkTooltip()
    const doc = el.ownerDocument
    const tip = doc.createElement('div')
    tip.className = 'xterm-hover nx-term-link-tip'
    const hint = this.settings.linkModifier === 'mod' ? (isMac ? '⌘-click to open' : 'Ctrl+click to open') : 'Click to open'
    const uri = doc.createElement('span')
    uri.className = 'nx-term-link-uri'
    uri.textContent = text.length > 300 ? `${text.slice(0, 299)}…` : text
    const h = doc.createElement('span')
    h.className = 'nx-term-link-hint'
    h.textContent = hint
    tip.append(uri, h)
    const rect = el.getBoundingClientRect()
    const x = Math.max(4, Math.min(event.clientX - rect.left, rect.width - 260))
    const y = event.clientY - rect.top
    tip.style.left = `${x}px`
    if (y > 48) tip.style.bottom = `${rect.height - y + 14}px`
    else tip.style.top = `${y + 18}px`
    el.appendChild(tip)
    this.linkTooltip = tip
  }

  private hideLinkTooltip(): void {
    this.linkTooltip?.remove()
    this.linkTooltip = null
  }

  // ===================================================================================================================
  // actions (context menu, commands)
  // ===================================================================================================================

  clear(): void {
    this.term.clear()
  }

  reset(): void {
    this.term.reset()
    this.afterReset()
    this.scheduleFit(0)
  }

  openSearch(): void {
    this.setInfo({ searchOpen: true })
    for (const l of Array.from(this.searchListeners)) l()
  }

  /** The search bar focuses itself when search is requested again while open. */
  onSearchRequest(cb: () => void): () => void {
    this.searchListeners.add(cb)
    return () => this.searchListeners.delete(cb)
  }

  closeSearch(): void {
    this.setInfo({ searchOpen: false })
    try {
      this.searchAddon.clearDecorations()
    } catch {
      /* ignore */
    }
    this.focus()
  }

  searchColors() {
    return searchColors(this.scheme)
  }

  zoomBy(delta: number): void {
    const [min, max] = LIMITS.fontSize
    const base = this.settings.fontSize
    const zoom = Math.max(min - base, Math.min(max - base, (this.params.zoom ?? 0) + delta))
    this.setZoom(zoom)
  }

  zoomReset(): void {
    this.setZoom(0)
  }

  private setZoom(zoom: number): void {
    if ((this.params.zoom ?? 0) === zoom) return
    this.params = { ...this.params, zoom }
    const fontSize = this.fontSize()
    this.term.options.fontSize = fontSize
    this.setInfo({ fontSize })
    this.scheduleFit(0)
    updateTabParams<TerminalTabParams>(this.tabId, { zoom: zoom || undefined })
  }

  reconnect(): void {
    // A closed (or vanished) session cannot be revived: start a new one with the same parameters in this tab.
    if (this.live.state === 'gone' || this.live.state === 'closed') {
      void restartSessionInTab(this.tabId)
      return
    }
    if (this.live.transport === 'open' && this.socket.sendControl({ type: 'reconnect' })) return
    reconnectSession(this.sessionId)
      .then(() => this.socket.reconnectNow())
      .catch((err) => toast.error('Could not reconnect', { description: errorMessage(err) }))
  }

  /** Retry the WebSocket now (transport only). */
  retryTransport(): void {
    this.socket.reconnectNow()
  }

  sendSignal(name: 'INT' | 'TERM' | 'KILL' | 'HUP' | 'QUIT'): void {
    if (this.live.transport === 'open' && this.socket.sendControl({ type: 'signal', name })) return
    signalSession(this.sessionId, name).catch((err) => toast.error(`Could not send ${name}`, { description: errorMessage(err) }))
  }

  sendBreak(): void {
    if (this.live.transport === 'open' && this.socket.sendControl({ type: 'break' })) return
    breakSession(this.sessionId).catch((err) => toast.error('Could not send break', { description: errorMessage(err) }))
  }

  async setRecording(enabled: boolean): Promise<void> {
    try {
      await setSessionRecording(this.sessionId, enabled)
      this.setInfo({ recording: enabled })
      toast.success(enabled ? 'Recording started' : 'Recording stopped')
    } catch (err) {
      toast.error(enabled ? 'Could not start recording' : 'Could not stop recording', { description: errorMessage(err) })
    }
  }

  async setLogging(enabled: boolean): Promise<void> {
    try {
      await setSessionLogging(this.sessionId, enabled)
      this.setInfo({ logging: enabled })
      toast.success(enabled ? 'Session logging started' : 'Session logging stopped')
    } catch (err) {
      toast.error(enabled ? 'Could not start logging' : 'Could not stop logging', { description: errorMessage(err) })
    }
  }

  /** Plain text of the terminal buffer (wrapped lines joined). */
  bufferText(): string {
    const buf = this.term.buffer.normal
    const out: string[] = []
    for (let y = 0; y < buf.length; y++) {
      const line = buf.getLine(y)
      if (!line) continue
      const text = line.translateToString(true)
      if (line.isWrapped && out.length) out[out.length - 1] += text
      else out.push(text)
    }
    while (out.length && !out[out.length - 1].trim()) out.pop()
    return out.join('\n') + '\n'
  }

  private htmlDocument(): string {
    const body = this.serializeAddon.serializeAsHTML({ includeGlobalBackground: true })
    const title = this.live.title.replace(/[<>&"]/g, (c) => ({ '<': '&lt;', '>': '&gt;', '&': '&amp;', '"': '&quot;' })[c] ?? c)
    return `<!doctype html>\n<html><head><meta charset="utf-8"><title>${title}</title></head><body style="margin:0;background:${this.scheme.background}">${body}</body></html>\n`
  }

  /** Save the terminal output as text or coloured HTML (TERM-26). */
  saveOutput(format: 'text' | 'html'): void {
    const name = sanitizeFileName(`${this.live.title}-${timestamp()}`)
    if (format === 'html') downloadText(this.htmlDocument(), `${name}.html`, 'text/html', this.win())
    else downloadText(this.bufferText(), `${name}.txt`, 'text/plain', this.win())
  }

  /** Download the server-side scrollback (whole ring buffer, ANSI stripped). */
  async saveServerLog(): Promise<void> {
    try {
      const text = await getScrollback(this.sessionId, false)
      downloadBlob(new Blob([text], { type: 'text/plain;charset=utf-8' }), `${sanitizeFileName(`${this.live.title}-${timestamp()}`)}.log`, this.win())
    } catch (err) {
      toast.error('Could not download the session log', { description: errorMessage(err) })
    }
  }

  async copyAll(format: 'text' | 'html'): Promise<void> {
    const text = this.bufferText()
    const ok = await writeClipboard(text, this.win(), format === 'html' ? this.serializeAddon.serializeAsHTML({ includeGlobalBackground: true }) : undefined)
    if (ok) {
      if (this.settings.clipboardHistory) pushClipboardHistory(text, 'copy', this.live.title)
      toast.success('Terminal output copied')
    } else toast.error('Could not copy to the clipboard')
  }

  /** Open the file browser at the terminal's working directory (files feature command). */
  openFilesHere(): void {
    void runCommand('files.openForSession', { sessionId: this.sessionId, path: this.live.cwd }, { source: 'context-menu' })
  }

  // ===================================================================================================================
  // plugins (registerTerminalPlugin)
  // ===================================================================================================================

  private pluginContext(): TerminalPluginContext & TerminalPluginContextEx {
    const currentSettings = () => this.settings as unknown as Readonly<Record<string, unknown>>
    return {
      tabId: this.tabId,
      sessionId: this.sessionId,
      session: () => this.runtime,
      send: (data) => {
        this.sendRaw(data)
      },
      onInput: (cb) => {
        this.inputObservers.add(cb)
        return () => this.inputObservers.delete(cb)
      },
      onOutput: (cb) => {
        this.outputObservers.add(cb)
        return () => this.outputObservers.delete(cb)
      },
      get settings() {
        return currentSettings()
      },
      handle: this,
      writeLocal: (data) => this.writeLocal(data),
      addOutputFilter: (filter) => {
        this.outputFilters.push(filter)
        return () => {
          const i = this.outputFilters.indexOf(filter)
          if (i >= 0) this.outputFilters.splice(i, 1)
        }
      },
      isReplaying: () => this.isReplaying(),
      acquireInputLock: (reason) => this.inputLocks.acquire(reason),
      sendRaw: (data) => this.sendRaw(data),
    }
  }

  private setupPlugins(): void {
    const sync = () => {
      if (this.disposed) return
      const live = new Map(terminalPlugins.list().map((p) => [p.id, p]))
      for (const [id, p] of this.plugins) {
        if (live.get(id) !== p.def) {
          this.safeDispose(p.dispose)
          this.plugins.delete(id)
        }
      }
      for (const [id, def] of live) {
        if (this.plugins.has(id)) continue
        try {
          const d = def.setup(this.term, this.pluginContext())
          this.plugins.set(id, { def, dispose: typeof d === 'function' ? d : undefined })
        } catch (err) {
          console.error(`[terminal] plugin "${id}" failed to set up`, err)
          this.plugins.set(id, { def })
        }
      }
    }
    sync()
    this.cleanups.push(terminalPlugins.subscribe(sync))
  }

  // ===================================================================================================================
  // persistence
  // ===================================================================================================================

  /** Save a snapshot + offset for the next page load (called on pagehide). */
  persist(): void {
    if (this.disposed || this.live.state === 'closed' || this.live.state === 'gone' || this.live.state === 'unknown') return
    if (!this.groundAtRendered || this.renderedOffset <= 0) {
      dropSavedTerminal(this.sessionId)
      return
    }
    try {
      for (const scrollback of [Math.min(this.settings.scrollback, 3000), 300]) {
        const data = this.serializeAddon.serialize({ scrollback })
        if (saveTerminal(this.sessionId, { offset: this.renderedOffset, cols: this.term.cols, rows: this.term.rows, data, savedAt: Date.now() })) return
      }
      dropSavedTerminal(this.sessionId)
    } catch (err) {
      console.warn('[terminal] snapshot failed', err)
      dropSavedTerminal(this.sessionId)
    }
  }

  // ===================================================================================================================
  // TerminalHandle
  // ===================================================================================================================

  /** Runtime session from the sessions cache. */
  session(): RuntimeSession | undefined {
    return this.runtime
  }

  info(): TerminalInfo {
    return this.live
  }
}
