/*
 * VncController: one noVNC connection per VNC tab (PROTO-17, GFX-1..5, GFX-18). Framework-agnostic; the React view
 * subscribes to `store`.
 *
 *   - Opens WS /ws/vnc/{sessionId} itself and hands the socket to noVNC, so the backend's close code / reason (auth
 *     failure, vault locked, unknown session, …) is known when noVNC reports `disconnect`.
 *   - Auto-reconnects with backoff (1 s → 30 s) after network drops, never after permanent failures (PROTO-39).
 *   - Scaling: fit (scaleViewport), remote-resize (resizeSession) and none + zoom (10–400 %, implemented by sizing the
 *     noVNC target and letting noVNC scale to it, so pointer coordinates stay exact).
 *   - Clipboard both ways within the effective direction policy (connection / administrator policy from vnc-info,
 *     combined with the user's own setting; Termstead also enforces local → remote server-side), send-keys, held
 *     modifiers, paced typing, screenshots, view-only.
 *   - Weaker-than-offered security (close 4426) is never retried silently: the view asks, and the user's choice is sent
 *     as ?allow=weak|unencrypted for the rest of the tab's life (the backend also remembers it for the session).
 *   - HiDPI remote resize: with `hiDpi`, "resize remote" requests the tab size in device pixels and scales the result
 *     back down (1 framebuffer pixel = 1 screen pixel).
 */
import type RFB from '@novnc/novnc'
import { createStore, type StoreApi } from 'zustand/vanilla'
import { wsUrl, seg } from '@/api/client'
import { copyText } from '@/lib/utils'
import { getVncInfo } from './api'
import { COMBOS, CTRL_ALT_F_KEYS, F_KEYS, MODIFIER_KEYS, textToKeys, type Combo, type HeldModifier, type Key } from './keys'
import { vncSettings, type ClipboardAllowed } from './settings'
import { PERMANENT_CLOSE_CODES, VNC_CLOSE, type ScalingMode, type VncConfirmInfo } from './types'

type RFBCtor = typeof RFB

let noVNC: Promise<RFBCtor> | null = null

/** Lazy-load noVNC (≈60 KB gz, own chunk). */
export function loadNoVNC(): Promise<RFBCtor> {
  if (!noVNC) {
    noVNC = import('@novnc/novnc')
      .then((m) => m.default)
      .catch((err) => {
        noVNC = null
        throw err
      })
  }
  return noVNC
}

export type VncPhase =
  | 'loading'
  | 'connecting'
  | 'connected'
  | 'reconnecting'
  | 'credentials'
  | 'verify'
  | 'confirm'
  | 'disconnected'
  | 'error'
  | 'restarting'

export interface VncState {
  phase: VncPhase
  /** Human-readable reason for disconnected / error phases. */
  message?: string
  closeCode?: number
  /** Retrying cannot help (auth failure, unknown session, …). */
  permanent: boolean
  /** The session itself is gone or closed: offer "start a new session". */
  sessionGone: boolean
  /** ms epoch of the next automatic reconnect attempt. */
  retryAt?: number
  attempt: number
  connectedAt?: number
  desktopName?: string
  fbWidth: number
  fbHeight: number
  viewOnly: boolean
  scaling: ScalingMode
  zoom: number
  quality: number
  compression: number
  /** Credentials noVNC asks for (pass-through security types). */
  credentialTypes: Array<'username' | 'password' | 'target'>
  /** Server public key fingerprint awaiting approval (RA2 pass-through). */
  serverKey?: string
  /** Weaker security awaiting the user's decision (phase `confirm`); undefined while loading the details. */
  confirm?: VncConfirmInfo
  /** The user allowed weaker security for this tab (sent as ?allow=…). */
  allowed?: 'weak' | 'unencrypted'
  remoteClipboard?: string
  remoteClipboardAt?: number
  /** Effective clipboard directions; `clipboardKnown` is false until the policy of the connection arrived. */
  clipboard: ClipboardAllowed
  clipboardKnown: boolean
  /** Remote clipboard updates dropped by the policy. */
  clipboardBlocked: number
  power: boolean
  held: HeldModifier[]
  typing: boolean
  bellAt?: number
}

export interface VncControllerOptions {
  tabId: string
  sessionId: string
  target: HTMLElement
  /** Admin shadow / read-only viewer: input is never sent (the backend enforces it too). */
  readOnly: boolean
  /** Incoming (reverse) connection: cannot be re-established. */
  reverse: boolean
  shared: boolean
  autoReconnect: boolean
  scaling: ScalingMode
  quality: number
  compression: number
  viewOnly: boolean
  /** Called when the backend no longer knows the session (4404) or closed it (4410) and the user asks for a new one. */
  onSessionGone: (reason: 'gone' | 'closed', automatic: boolean) => void
  /** Vault locked (4423): resolve true once unlocked. */
  onVaultLocked: () => Promise<boolean>
  onConnected?: () => void
}

const MIN_ZOOM = 0.1
const MAX_ZOOM = 4
/** Upper bound of a HiDPI remote-resize request per dimension. */
const MAX_REMOTE_SIZE = 8192

/** noVNC 1.7 internals used for HiDPI remote resize (feature-detected; see patchHiDpiResize). */
interface NoVNCResizeInternals {
  _requestRemoteResize?: () => void
  _screenSize?: () => { w: number; h: number }
  _termsteadHiDpi?: boolean
}

/**
 * Make noVNC's remote-resize request scale the tab size by `factor()` (device pixels). noVNC measures the target in
 * CSS pixels; the request is the only use of that size which should be scaled (autoscale still fits the canvas to the
 * CSS size, which makes one framebuffer pixel one device pixel). Returns false when the internals are missing.
 */
function patchHiDpiResize(rfb: RFB, factor: () => number): boolean {
  const r = rfb as unknown as NoVNCResizeInternals
  if (r._termsteadHiDpi) return true
  const request = r._requestRemoteResize
  const screenSize = r._screenSize
  if (typeof request !== 'function' || typeof screenSize !== 'function') return false
  r._termsteadHiDpi = true
  r._requestRemoteResize = function (this: NoVNCResizeInternals) {
    const k = factor()
    if (k === 1) {
      request.call(this)
      return
    }
    const own = Object.prototype.hasOwnProperty.call(this, '_screenSize')
    const previous = this._screenSize
    this._screenSize = () => {
      const size = screenSize.call(this)
      return { w: Math.min(MAX_REMOTE_SIZE, Math.floor(size.w * k)), h: Math.min(MAX_REMOTE_SIZE, Math.floor(size.h * k)) }
    }
    try {
      request.call(this)
    } finally {
      if (own) this._screenSize = previous
      else delete this._screenSize
    }
  }
  return true
}
const RETRY_MIN = 1000
const RETRY_MAX = 30_000
const STABLE_AFTER = 10_000

function initialState(o: VncControllerOptions): VncState {
  return {
    phase: 'loading',
    permanent: false,
    sessionGone: false,
    attempt: 0,
    fbWidth: 0,
    fbHeight: 0,
    viewOnly: o.readOnly || o.viewOnly,
    scaling: o.scaling,
    zoom: 1,
    quality: o.quality,
    compression: o.compression,
    credentialTypes: [],
    clipboard: { toRemote: false, fromRemote: false },
    clipboardKnown: false,
    clipboardBlocked: 0,
    power: false,
    held: [],
    typing: false,
  }
}

function closeMessage(code: number, reason: string): string {
  if (code === VNC_CLOSE.insecure) return reason || 'The connection would be less secure than the server offered'
  if (reason) return reason
  switch (code) {
    case VNC_CLOSE.serverEnded:
      return 'The connection was closed'
    case VNC_CLOSE.shutdown:
      return 'Termstead is restarting'
    case VNC_CLOSE.notFound:
      return 'The session no longer exists'
    case VNC_CLOSE.sessionClosed:
      return 'The session was closed'
    case VNC_CLOSE.locked:
      return 'The vault is locked'
    case 1006:
      return 'Connection to Termstead lost'
  }
  return `Connection closed (code ${code})`
}

export class VncController {
  readonly store: StoreApi<VncState>
  private opts: VncControllerOptions
  private rfb: RFB | null = null
  private ws: WebSocket | null = null
  private gen = 0
  private retryTimer: ReturnType<typeof setTimeout> | null = null
  private retryDelay = RETRY_MIN
  private lastClose: { code: number; reason: string } | null = null
  private securityReason = ''
  private userDisconnected = false
  private disposed = false
  private canvasObserver: MutationObserver | null = null
  private typingAbort: AbortController | null = null
  private lastSentClipboard = ''
  private autoRestarted = false
  private visible = true
  private hiDpi = false
  /** Remote clipboard text received before the clipboard policy was known. */
  private pendingRemoteClipboard: string | null = null

  constructor(opts: VncControllerOptions) {
    this.opts = opts
    this.store = createStore<VncState>(() => initialState(opts))
    this.hiDpi = vncSettings.get().hiDpi
  }

  get state(): VncState {
    return this.store.getState()
  }

  private set(patch: Partial<VncState>): void {
    if (!this.disposed) this.store.setState(patch)
  }

  get sessionId(): string {
    return this.opts.sessionId
  }

  get tabId(): string {
    return this.opts.tabId
  }

  get readOnly(): boolean {
    return this.opts.readOnly
  }

  get reverse(): boolean {
    return this.opts.reverse
  }

  get canvas(): HTMLCanvasElement | null {
    return this.opts.target.querySelector('canvas')
  }

  // ---- lifecycle ------------------------------------------------------------------------------------------------

  async start(): Promise<void> {
    this.set({ phase: 'loading' })
    try {
      await loadNoVNC()
    } catch (err) {
      this.set({ phase: 'error', permanent: true, message: `Could not load the VNC viewer: ${String(err)}` })
      return
    }
    if (!this.disposed) void this.connect()
  }

  dispose(): void {
    if (this.disposed) return
    this.releaseHeld()
    this.disposed = true
    this.clearRetry()
    this.typingAbort?.abort()
    this.teardown()
  }

  /** Detach the current RFB/socket (their late events are ignored through the generation counter). */
  private teardown(): void {
    this.gen++
    this.canvasObserver?.disconnect()
    this.canvasObserver = null
    const rfb = this.rfb
    const ws = this.ws
    this.rfb = null
    this.ws = null
    if (rfb) {
      try {
        rfb.disconnect()
      } catch {
        /* already disconnected */
      }
    }
    if (ws && ws.readyState <= WebSocket.OPEN) {
      try {
        ws.close(1000)
      } catch {
        /* ignore */
      }
    }
  }

  private clearRetry(): void {
    if (this.retryTimer) clearTimeout(this.retryTimer)
    this.retryTimer = null
  }

  async connect(): Promise<void> {
    if (this.disposed) return
    this.clearRetry()
    this.teardown()
    const gen = this.gen
    let RFBClass: RFBCtor
    try {
      RFBClass = await loadNoVNC()
    } catch (err) {
      this.set({ phase: 'error', permanent: true, message: `Could not load the VNC viewer: ${String(err)}` })
      return
    }
    if (this.disposed || gen !== this.gen) return
    this.userDisconnected = false
    this.lastClose = null
    this.securityReason = ''
    this.set({
      phase: this.state.attempt > 0 ? 'reconnecting' : 'connecting',
      message: undefined,
      closeCode: undefined,
      permanent: false,
      sessionGone: false,
      retryAt: undefined,
      credentialTypes: [],
      serverKey: undefined,
      confirm: undefined,
      held: [],
      clipboardKnown: false,
    })
    this.pendingRemoteClipboard = null

    const allow = this.state.allowed
    const ws = new WebSocket(wsUrl(`/ws/vnc/${seg(this.opts.sessionId)}`, allow ? { allow } : undefined))
    ws.binaryType = 'arraybuffer'
    ws.addEventListener('close', (e) => {
      if (gen === this.gen) this.lastClose = { code: e.code, reason: e.reason }
    })
    let rfb: RFB
    try {
      rfb = new RFBClass(this.opts.target, ws, { shared: this.opts.shared, credentials: {} })
    } catch (err) {
      ws.close()
      this.set({ phase: 'error', permanent: true, message: `The VNC viewer failed to start: ${String(err)}` })
      return
    }
    this.ws = ws
    this.rfb = rfb
    this.applyViewSettings(rfb)

    const live = () => gen === this.gen && !this.disposed
    rfb.addEventListener('connect', () => {
      if (!live()) return
      const now = Date.now()
      this.set({ phase: 'connected', connectedAt: now, message: undefined, credentialTypes: [], serverKey: undefined })
      this.observeCanvas()
      setTimeout(() => {
        // A connection that stays up resets the backoff.
        if (live() && this.state.phase === 'connected' && Date.now() - now >= STABLE_AFTER - 50) {
          this.retryDelay = RETRY_MIN
          this.set({ attempt: 0 })
        }
      }, STABLE_AFTER)
      this.opts.onConnected?.()
      if (!this.state.viewOnly) rfb.focus({ preventScroll: true })
    })
    rfb.addEventListener('disconnect', (e) => {
      if (!live()) return
      this.onDisconnect(e.detail.clean)
    })
    rfb.addEventListener('credentialsrequired', (e) => {
      if (!live()) return
      this.set({ phase: 'credentials', credentialTypes: e.detail.types })
    })
    rfb.addEventListener('serververification', (e) => {
      if (!live()) return
      void fingerprint(e.detail.publickey).then((fp) => live() && this.set({ phase: 'verify', serverKey: fp }))
    })
    rfb.addEventListener('securityfailure', (e) => {
      if (!live()) return
      this.securityReason = e.detail.reason || `security failure (status ${e.detail.status})`
    })
    rfb.addEventListener('clipboard', (e) => {
      if (!live()) return
      this.onRemoteClipboard(e.detail.text)
    })
    rfb.addEventListener('bell', () => live() && this.set({ bellAt: Date.now() }))
    rfb.addEventListener('desktopname', (e) => live() && this.set({ desktopName: e.detail.name }))
    rfb.addEventListener('capabilities', (e) => live() && this.set({ power: !!e.detail.capabilities.power }))
  }

  private onDisconnect(clean: boolean): void {
    // noVNC may report the disconnect before the socket's close event (it fails first, then closes the socket): wait
    // briefly for the backend's close code / reason.
    const ws = this.ws
    const gen = this.gen
    let done = false
    const decide = () => {
      if (done) return
      done = true
      // A reconnect (or dispose) while waiting started a new generation: this disconnect no longer matters.
      if (gen !== this.gen || this.disposed) return
      this.decideAfterDisconnect(clean)
    }
    if (!this.lastClose && ws && ws.readyState !== WebSocket.CLOSED) {
      const timer = setTimeout(decide, 1000)
      ws.addEventListener(
        'close',
        () => {
          clearTimeout(timer)
          decide()
        },
        { once: true },
      )
      return
    }
    decide()
  }

  private decideAfterDisconnect(clean: boolean): void {
    if (this.disposed) return
    this.canvasObserver?.disconnect()
    this.canvasObserver = null
    const close = this.lastClose
    const code = close?.code ?? 1006
    let reason = this.securityReason || closeMessage(code, close?.reason ?? '')
    if (!this.securityReason && (code === 1005 || (code === 1000 && !close?.reason))) {
      reason = 'The VNC handshake failed in the browser (see the browser console for details)'
    }
    this.rfb = null
    this.ws = null
    this.gen++
    this.typingAbort?.abort()

    if (this.userDisconnected) {
      this.set({ phase: 'disconnected', message: 'You disconnected. The remote desktop keeps running on the server.', closeCode: code, permanent: false, held: [] })
      return
    }
    if (code === VNC_CLOSE.locked) {
      this.set({ phase: 'error', message: reason, closeCode: code, permanent: false, held: [] })
      void this.opts.onVaultLocked().then((ok) => {
        if (ok && !this.disposed) void this.connect()
      })
      return
    }
    if (code === VNC_CLOSE.insecure) {
      // Never retried automatically: the user decides (see confirmInsecure).
      this.set({ phase: 'confirm', message: reason, closeCode: code, permanent: true, retryAt: undefined, held: [], confirm: undefined })
      const gen = this.gen
      const unavailable = () => {
        if (gen === this.gen && this.state.phase === 'confirm') this.set({ phase: 'error', permanent: true })
      }
      void getVncInfo(this.opts.sessionId)
        .then((info) => {
          if (gen !== this.gen || this.state.phase !== 'confirm') return
          // No pending confirmation (e.g. confirmed meanwhile in another tab): offer a plain reconnect instead.
          if (info.confirm) this.set({ confirm: info.confirm })
          else unavailable()
        })
        .catch(unavailable)
      return
    }
    if (code === VNC_CLOSE.notFound || code === VNC_CLOSE.sessionClosed) {
      const gone = code === VNC_CLOSE.notFound
      this.set({ phase: 'error', message: reason, closeCode: code, permanent: true, sessionGone: !this.opts.reverse, held: [] })
      if (gone && !this.opts.reverse && !this.autoRestarted) {
        // Typically after a Termstead restart: start an equivalent session once, transparently.
        this.autoRestarted = true
        this.set({ phase: 'restarting', message: 'Starting a new session…' })
        this.opts.onSessionGone('gone', true)
      }
      return
    }
    const permanent = this.opts.reverse || PERMANENT_CLOSE_CODES.has(code) || !!this.securityReason
    const message =
      this.opts.reverse && code === VNC_CLOSE.serverEnded ? 'The remote server ended the incoming connection' : reason
    // A normal close by the VNC server (logout, server stopped) or a failure inside noVNC is not retried automatically.
    const retry = !permanent && this.opts.autoReconnect && code !== VNC_CLOSE.serverEnded && code !== 1005
    if (!retry) {
      this.set({
        phase: clean && code === VNC_CLOSE.serverEnded ? 'disconnected' : 'error',
        message,
        closeCode: code,
        permanent,
        retryAt: undefined,
        held: [],
      })
      return
    }
    const delay = this.retryDelay
    this.retryDelay = Math.min(RETRY_MAX, this.retryDelay * 2)
    this.set({
      phase: 'error',
      message,
      closeCode: code,
      permanent: false,
      attempt: this.state.attempt + 1,
      retryAt: Date.now() + delay,
      held: [],
    })
    this.retryTimer = setTimeout(() => void this.connect(), delay)
  }

  /** User reconnect (also resets the backoff). */
  reconnect(): void {
    this.retryDelay = RETRY_MIN
    // A user-initiated attempt starts afresh: a failure reads "Could not connect", not "Connection lost".
    this.set({ attempt: 0, connectedAt: undefined })
    void this.connect()
  }

  /** Retry now while a backoff timer is pending (network came back). */
  retryNow(): void {
    if (this.retryTimer) void this.connect()
  }

  disconnect(): void {
    this.clearRetry()
    this.releaseHeld()
    this.userDisconnected = true
    if (this.rfb) {
      try {
        this.rfb.disconnect()
      } catch {
        this.onDisconnect(true)
      }
    } else {
      this.set({ phase: 'disconnected', message: 'You disconnected. The remote desktop keeps running on the server.', retryAt: undefined })
    }
  }

  /** The viewer turned out to be a read-only (shadow) view of another user's session. */
  setReadOnly(): void {
    if (this.opts.readOnly) return
    this.opts.readOnly = true
    this.setViewOnly(true)
  }

  /** Start an equivalent session (the old one is gone or closed); the view swaps in a new controller. */
  restartSession(): void {
    if (this.opts.reverse) return
    this.clearRetry()
    this.set({ phase: 'restarting', message: 'Starting a new session…', retryAt: undefined })
    this.opts.onSessionGone('gone', false)
  }

  restartFailed(automatic: boolean): void {
    this.set({
      phase: 'error',
      permanent: true,
      sessionGone: true,
      message: automatic ? 'The session no longer exists (was Termstead restarted?)' : 'Could not start a new session',
    })
  }

  /**
   * The user accepts weaker security for this tab: `weak` = anonymous TLS with a weak key exchange, `unencrypted` =
   * an unencrypted security type (which implies weak). Reconnects immediately.
   */
  confirmInsecure(level: 'weak' | 'unencrypted'): void {
    const cur = this.state.allowed
    this.set({ allowed: cur === 'unencrypted' ? cur : level })
    this.reconnect()
  }

  /** The user declined the weaker connection. */
  declineInsecure(): void {
    this.set({
      phase: 'error',
      permanent: true,
      confirm: undefined,
      message: `Not connected: ${this.state.message ?? 'the connection would be less secure than the server offered'}`,
    })
  }

  // ---- credentials (pass-through security types) ------------------------------------------------------------------

  sendCredentials(creds: { username?: string; password?: string; target?: string }): void {
    if (!this.rfb) return
    this.set({ phase: 'connecting', credentialTypes: [] })
    this.rfb.sendCredentials(creds)
  }

  approveServer(approve: boolean): void {
    if (!this.rfb) return
    if (approve) {
      this.set({ phase: 'connecting', serverKey: undefined })
      this.rfb.approveServer()
    } else {
      this.disconnect()
    }
  }

  // ---- view settings ----------------------------------------------------------------------------------------------

  private applyViewSettings(rfb: RFB): void {
    const s = this.state
    const settings = vncSettings.get()
    rfb.focusOnClick = true
    rfb.clipViewport = false
    rfb.dragViewport = false
    rfb.viewOnly = s.viewOnly
    rfb.showDotCursor = settings.dotCursor
    rfb.qualityLevel = s.quality
    rfb.compressionLevel = s.compression
    rfb.background = 'var(--vnc-surround)'
    this.applyScaling(rfb)
  }

  private applyScaling(rfb = this.rfb): void {
    if (!rfb) return
    const { scaling } = this.state
    const hiDpi = scaling === 'remote-resize' && this.hiDpi && patchHiDpiResize(rfb, () => this.pixelRatio())
    // While the tab is hidden its size is meaningless (possibly 0×0): never ask the server to resize to it.
    rfb.resizeSession = scaling === 'remote-resize' && this.visible
    // 'none' also uses noVNC's scaling: the view sizes the target to framebuffer × zoom (see targetSize). HiDPI
    // remote resize scales the device-pixel framebuffer back to the tab.
    rfb.scaleViewport = scaling === 'fit' || scaling === 'none' || hiDpi
  }

  /** Device pixels per CSS pixel for remote-resize requests (1 unless HiDPI remote resize is on). */
  private pixelRatio(): number {
    if (!this.hiDpi || this.state.scaling !== 'remote-resize') return 1
    const dpr = this.opts.target.ownerDocument.defaultView?.devicePixelRatio ?? 1
    return Number.isFinite(dpr) && dpr > 1 ? Math.min(dpr, 4) : 1
  }

  /** HiDPI remote resize on/off (settings). */
  setHiDpi(on: boolean): void {
    if (this.hiDpi === on) return
    this.hiDpi = on
    this.applyScaling()
    this.requestRemoteResize()
  }

  /** Ask the server for the current tab size again (e.g. the window moved to a screen with another pixel ratio). */
  requestRemoteResize(): void {
    const r = this.rfb as unknown as NoVNCResizeInternals | null
    if (r && this.state.scaling === 'remote-resize' && this.visible && this.state.phase === 'connected') {
      try {
        r._requestRemoteResize?.()
      } catch {
        /* not connected yet */
      }
    }
  }

  /** The tab became hidden / visible (dockview keeps hidden panels mounted). */
  setVisible(visible: boolean): void {
    if (this.visible === visible) return
    this.visible = visible
    this.applyScaling()
  }

  setScaling(scaling: ScalingMode): void {
    this.set({ scaling, zoom: scaling === 'none' ? this.state.zoom : 1 })
    this.applyScaling()
  }

  /** Effective scale of the canvas on screen (1 = 100 %). */
  effectiveScale(): number {
    const c = this.canvas
    const { fbWidth } = this.state
    if (!c || !fbWidth) return 1
    const w = c.getBoundingClientRect().width
    return w > 0 ? w / fbWidth : 1
  }

  setZoom(zoom: number): void {
    const z = Math.min(MAX_ZOOM, Math.max(MIN_ZOOM, Math.round(zoom * 10) / 10))
    this.set({ scaling: 'none', zoom: z })
    this.applyScaling()
  }

  zoomBy(steps: number): void {
    const base = this.state.scaling === 'none' ? this.state.zoom : Math.round(this.effectiveScale() * 10) / 10
    this.setZoom(base + steps * 0.1)
  }

  zoomReset(): void {
    this.setZoom(1)
  }

  /** CSS size of the noVNC target for the current mode (undefined = fill the viewport). */
  targetSize(): { width: number; height: number } | undefined {
    const { scaling, zoom, fbWidth, fbHeight } = this.state
    if (scaling !== 'none' || !fbWidth || !fbHeight) return undefined
    return { width: Math.round(fbWidth * zoom), height: Math.round(fbHeight * zoom) }
  }

  setViewOnly(viewOnly: boolean): void {
    if (this.opts.readOnly) viewOnly = true
    if (viewOnly) this.releaseHeld()
    this.set({ viewOnly })
    if (this.rfb) this.rfb.viewOnly = viewOnly
  }

  setQuality(q: number): void {
    this.set({ quality: q })
    if (this.rfb) this.rfb.qualityLevel = q
  }

  setCompression(c: number): void {
    this.set({ compression: c })
    if (this.rfb) this.rfb.compressionLevel = c
  }

  setDotCursor(show: boolean): void {
    if (this.rfb) this.rfb.showDotCursor = show
  }

  focus(): void {
    this.rfb?.focus({ preventScroll: true })
  }

  /** Framebuffer size tracking: noVNC resizes the canvas backing store on desktop size changes. */
  private observeCanvas(): void {
    const c = this.canvas
    if (!c) return
    const update = () => this.set({ fbWidth: c.width, fbHeight: c.height })
    update()
    this.canvasObserver?.disconnect()
    this.canvasObserver = new MutationObserver(update)
    this.canvasObserver.observe(c, { attributes: true, attributeFilter: ['width', 'height'] })
  }

  // ---- input ------------------------------------------------------------------------------------------------------

  private canSend(): boolean {
    return !!this.rfb && this.state.phase === 'connected' && !this.state.viewOnly
  }

  sendCtrlAltDel(): void {
    if (this.canSend()) this.rfb!.sendCtrlAltDel()
  }

  sendCombo(combo: Combo | string): void {
    const c = typeof combo === 'string' ? [...COMBOS, ...F_KEYS, ...CTRL_ALT_F_KEYS].find((x) => x.id === combo) : combo
    if (!c || !this.canSend()) return
    const rfb = this.rfb!
    for (const k of c.keys) rfb.sendKey(k.keysym, k.code, true)
    for (const k of c.keys.toReversed()) rfb.sendKey(k.keysym, k.code, false)
    this.focus()
  }

  sendKey(key: Key): void {
    if (this.canSend()) this.rfb!.sendKey(key.keysym, key.code)
  }

  toggleHeld(mod: HeldModifier): void {
    if (!this.canSend()) return
    const held = this.state.held
    const key = MODIFIER_KEYS[mod]
    if (held.includes(mod)) {
      this.rfb!.sendKey(key.keysym, key.code, false)
      this.set({ held: held.filter((m) => m !== mod) })
    } else {
      this.rfb!.sendKey(key.keysym, key.code, true)
      this.set({ held: [...held, mod] })
    }
    this.focus()
  }

  private releaseHeld(): void {
    const held = this.state.held
    if (!held.length) return
    if (this.rfb && this.state.phase === 'connected') {
      for (const m of held.toReversed()) {
        const k = MODIFIER_KEYS[m]
        try {
          this.rfb.sendKey(k.keysym, k.code, false)
        } catch {
          /* disconnected */
        }
      }
    }
    this.set({ held: [] })
  }

  /** Type text as keystrokes, paced (GFX-3 "type clipboard" for consoles without clipboard support). */
  async typeText(text: string): Promise<number> {
    if (!this.canSend()) return 0
    this.typingAbort?.abort()
    const abort = new AbortController()
    this.typingAbort = abort
    const keys = textToKeys(text)
    const delay = Math.max(0, Math.min(500, vncSettings.get().typingDelay))
    this.set({ typing: true })
    let sent = 0
    try {
      for (const k of keys) {
        if (abort.signal.aborted || !this.canSend()) break
        this.rfb!.sendKey(k.keysym, k.code)
        sent++
        if (delay > 0) await new Promise((r) => setTimeout(r, delay))
      }
    } finally {
      if (this.typingAbort === abort) {
        this.typingAbort = null
        this.set({ typing: false })
      }
    }
    return sent
  }

  cancelTyping(): void {
    this.typingAbort?.abort()
  }

  power(action: 'shutdown' | 'reboot' | 'reset'): void {
    if (!this.canSend() || !this.state.power) return
    if (action === 'shutdown') this.rfb!.machineShutdown()
    else if (action === 'reboot') this.rfb!.machineReboot()
    else this.rfb!.machineReset()
  }

  // ---- clipboard --------------------------------------------------------------------------------------------------

  /** Effective clipboard directions (the view combines vnc-info's policy with the user's setting). */
  setClipboardPolicy(allowed: ClipboardAllowed): void {
    const cur = this.state
    if (cur.clipboardKnown && cur.clipboard.toRemote === allowed.toRemote && cur.clipboard.fromRemote === allowed.fromRemote) return
    this.set({ clipboard: allowed, clipboardKnown: true })
    const pending = this.pendingRemoteClipboard
    this.pendingRemoteClipboard = null
    if (pending !== null) this.onRemoteClipboard(pending)
  }

  private onRemoteClipboard(text: string): void {
    const { clipboard, clipboardKnown } = this.state
    if (!clipboardKnown) {
      this.pendingRemoteClipboard = text // decided once the policy arrives
      return
    }
    if (!clipboard.fromRemote) {
      this.set({ clipboardBlocked: this.state.clipboardBlocked + 1 })
      return
    }
    this.lastSentClipboard = text // do not echo it back
    this.set({ remoteClipboard: text, remoteClipboardAt: Date.now() })
    if (vncSettings.get().clipboard !== 'auto') return
    const doc = this.opts.target.ownerDocument
    if (!doc.hasFocus()) return
    const nav = doc.defaultView?.navigator ?? navigator
    nav.clipboard?.writeText(text).catch(() => {
      /* not permitted without a user gesture in this browser: the clipboard panel shows it */
    })
  }

  /** Send local text to the remote clipboard (returns false when nothing was sent). */
  sendClipboard(text: string): boolean {
    if (!this.canSend() || !text || !this.state.clipboard.toRemote) return false
    this.lastSentClipboard = text
    this.rfb!.clipboardPasteFrom(text)
    return true
  }

  /**
   * Auto clipboard sync when the viewer gains focus: read the local clipboard (only where reading is already
   * permitted, so no permission prompt appears unexpectedly) and forward changes.
   */
  async syncLocalClipboard(): Promise<void> {
    if (vncSettings.get().clipboard !== 'auto' || !this.canSend() || !this.state.clipboard.toRemote) return
    const nav = this.opts.target.ownerDocument.defaultView?.navigator ?? navigator
    if (!nav.clipboard?.readText) return
    try {
      const perm = await nav.permissions?.query({ name: 'clipboard-read' as PermissionName })
      if (perm && perm.state !== 'granted') return
    } catch {
      return // permission not queryable (Firefox, Safari): manual panel only
    }
    try {
      const text = await nav.clipboard.readText()
      if (text && text !== this.lastSentClipboard) this.sendClipboard(text)
    } catch {
      /* denied */
    }
  }

  async copyRemoteClipboard(): Promise<boolean> {
    const t = this.state.remoteClipboard
    return t ? copyText(t) : false
  }

  // ---- screenshots (GFX-18) ---------------------------------------------------------------------------------------

  screenshot(): Promise<Blob | null> {
    const rfb = this.rfb
    if (!rfb || this.state.phase !== 'connected') return Promise.resolve(null)
    return new Promise((resolve) => {
      try {
        rfb.toBlob((b) => resolve(b), 'image/png')
      } catch {
        resolve(null)
      }
    })
  }
}

async function fingerprint(key: Uint8Array): Promise<string> {
  try {
    const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', key.slice().buffer as ArrayBuffer))
    return 'SHA256:' + Array.from(digest, (b) => b.toString(16).padStart(2, '0').toUpperCase()).join(':')
  } catch {
    return Array.from(key.slice(0, 32), (b) => b.toString(16).padStart(2, '0')).join('')
  }
}
