/*
 * Controller of one RDP tab: asks the backend for a ticket, runs the engine it names (IronRDP or guacd), keeps the
 * desktop sized to the tab (dynamic resolution or scaling), handles the viewer's host keys, fullscreen with Keyboard
 * Lock, clipboard and screenshots, and reports the state to the tab strip and — for IronRDP, whose protocol runs in
 * the browser — to the backend session.
 */
import { toast } from 'sonner'
import { isApiError } from '@/api/client'
import { getSession } from '@/api/sessions'
import { prompt } from '@/components/ui/dialog-host'
import { errorMessage } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth'
import { getTabParams, setTabState, updateTabParams } from '@/stores/workspace'
import { getTicket, reportState, saveBlob } from './api'
import {
  Cancelled,
  computeLayout,
  desktopRequest,
  EngineError,
  type DesktopRequest,
  type EndInfo,
  type EngineAdapter,
  type EngineCallbacks,
} from './engine'
import { CTRL_ALT_DEL, MAX_TYPED_TEXT } from './keys'
import { rdpSettings } from './settings'
import { getViewer, patchViewer } from './store'
import type { FileTransfer, KeyCombo, RdpController, RdpEngine, RdpScaling, RdpTabParams, RdpTicketInfo, ViewerErrorCode } from './types'

const RESIZE_DEBOUNCE = 350
/** A size request is checked after this long and repeated (a fresh session's display channel may not be open yet). */
const RESIZE_VERIFY = 1500
const RESIZE_ATTEMPTS = 4
/** Differences below this many pixels are not worth a resize. */
const RESIZE_TOLERANCE = 8
const RECONNECT_RESIZE_DEBOUNCE = 1500
/** Smaller viewports belong to hidden or collapsed tabs: never size the remote desktop after them. */
const MIN_VIEWPORT = 50
const MAX_AUTO_RECONNECT = 20

export interface ConnectionElements {
  /** Root of the tab (fullscreen target). */
  root: HTMLElement
  /** Focusable scrolling container. */
  viewport: HTMLElement
  /** Element the engine renders into. */
  stage: HTMLElement
}

export class RdpConnection implements RdpController {
  readonly tabId: string
  readonly sessionId: string
  private readonly els: ConnectionElements
  private adapter?: EngineAdapter
  private ticket?: RdpTicketInfo
  private attempt = 0
  private disposed = false
  private lastRequested?: DesktopRequest
  private resizeTimer?: ReturnType<typeof setTimeout>
  private reconnectTimer?: ReturnType<typeof setTimeout>
  private autoTimer?: ReturnType<typeof setTimeout>
  private autoAttempts = 0
  private everConnected = false
  private typing?: AbortController
  private cleanups: Array<() => void> = []
  private swallowKeyUp = new Set<string>()
  /** The dock tab is shown (hidden tabs stay mounted with a meaningless size). */
  private tabVisible = true

  constructor(tabId: string, sessionId: string, els: ConnectionElements) {
    this.tabId = tabId
    this.sessionId = sessionId
    this.els = els
    this.installListeners()
  }

  // ---- lifecycle ---------------------------------------------------------------------------------------------------

  start(): void {
    void this.connect()
  }

  dispose(): void {
    this.disposed = true
    this.attempt++
    clearTimeout(this.resizeTimer)
    clearTimeout(this.reconnectTimer)
    clearTimeout(this.autoTimer)
    this.typing?.abort()
    for (const c of this.cleanups.splice(0)) c()
    this.teardown()
    if (document.fullscreenElement === this.els.root) void document.exitFullscreen().catch(() => undefined)
  }

  private params(): RdpTabParams | undefined {
    return getTabParams<RdpTabParams>(this.tabId)
  }

  private teardown(): void {
    const a = this.adapter
    this.adapter = undefined
    a?.destroy()
    this.els.stage.replaceChildren()
  }

  private stale(my: number): boolean {
    return this.disposed || my !== this.attempt
  }

  private async connect(engine?: RdpEngine): Promise<void> {
    const my = ++this.attempt
    clearTimeout(this.reconnectTimer)
    clearTimeout(this.autoTimer)
    this.teardown()
    patchViewer(this.tabId, {
      status: 'loading',
      message: 'Preparing the connection…',
      errorCode: undefined,
      authFailed: false,
      remoteClipboardPending: false,
      desktop: undefined,
      reconnectAt: undefined,
    })
    setTabState(this.tabId, { status: 'connecting', progress: 'indeterminate' })

    const size = this.currentRequest()
    const params = this.params()
    let ticket: RdpTicketInfo
    try {
      const shadow = await this.isShadow()
      if (this.stale(my)) return
      ticket = await this.fetchTicket(size, engine ?? params?.engine, params?.vmId, shadow)
    } catch (err) {
      if (this.stale(my) || err instanceof Cancelled) return
      this.failTicket(err)
      return
    }
    if (this.stale(my)) return
    this.ticket = ticket
    this.lastRequested = size
    const readOnly = !!ticket.readOnly
    patchViewer(this.tabId, {
      engine: ticket.engine,
      clipboardEnabled: ticket.clipboard && !readOnly,
      driveEnabled: ticket.drive && ticket.engine === 'guacd' && !readOnly,
      driveName: ticket.driveName,
      canResize: !ticket.fixedSize && ticket.resizeMethod !== 'none',
      via: ticket.via,
      destination: ticket.destination,
      readOnly,
      recording: !!ticket.recording && ticket.engine === 'guacd',
    })
    if (ticket.recording && ticket.engine === 'ironrdp') {
      this.warnOnce('This connection records sessions, which needs the guacd engine: this IronRDP session is not recorded.')
    }

    const adapter = await this.createAdapter(ticket.engine)
    if (this.stale(my)) {
      adapter.destroy()
      return
    }
    this.adapter = adapter
    try {
      await adapter.connect(ticket, size)
    } catch (err) {
      if (this.stale(my) || err instanceof Cancelled) return
      ticket.password = ''
      await this.failConnect(my, ticket, err)
      return
    }
    ticket.password = '' // not needed any more (CredSSP ran in the WASM client)
    if (this.stale(my)) return
    this.everConnected = true
    this.autoAttempts = 0
    patchViewer(this.tabId, { status: 'connected', message: undefined, reconnectAttempt: undefined })
    setTabState(this.tabId, { status: 'connected', progress: null })
    if (ticket.engine === 'ironrdp') void reportState(this.sessionId, 'connected').catch(() => undefined)
    this.layout()
    // The tab may have changed size while connecting (its layout settles after the ticket was requested).
    this.syncSize(0)
    if (this.els.viewport.contains(document.activeElement) || this.isVisible()) adapter.focus()
  }

  /**
   * Whether this tab views another user's session (administrators shadowing a guacd session): the tab params say so
   * once known; otherwise the session's owner decides.
   */
  private async isShadow(): Promise<boolean> {
    const params = this.params()
    if (params?.shadow !== undefined) return params.shadow
    const me = useAuthStore.getState().user?.id
    if (!me) return false
    try {
      const s = await getSession(this.sessionId)
      const shadow = s.ownerId !== me
      if (shadow) updateTabParams<RdpTabParams>(this.tabId, { shadow: true })
      return shadow
    } catch {
      return false // the ticket request reports what is wrong
    }
  }

  private async fetchTicket(size: DesktopRequest, engine?: RdpEngine, vmId?: string, shadow = false): Promise<RdpTicketInfo> {
    if (shadow) return getTicket(this.sessionId, { width: size.width, height: size.height, dpi: size.dpi, shadow: true })
    for (;;) {
      try {
        return await getTicket(this.sessionId, {
          width: size.width,
          height: size.height,
          dpi: size.dpi,
          engine,
          preconnectionBlob: vmId,
        })
      } catch (err) {
        if (isApiError(err) && err.code === 'vm_id_required') {
          const id = await prompt({
            title: 'Hyper-V virtual machine',
            description: 'The Hyper-V console (vmconnect) needs the id (GUID) of the virtual machine to connect to.',
            label: 'VM id',
            placeholder: 'e.g. 4a3e1b2c-…',
            confirmLabel: 'Connect',
            validate: (v) => (v.trim() ? null : 'Enter the VM id'),
          })
          if (!id) throw new EngineError('Connecting was canceled: no VM id.')
          vmId = id.trim()
          updateTabParams<RdpTabParams>(this.tabId, { vmId })
          continue
        }
        throw err
      }
    }
  }

  private async createAdapter(engine: RdpEngine): Promise<EngineAdapter> {
    const ctx = {
      tabId: this.tabId,
      sessionId: this.sessionId,
      stage: this.els.stage,
      viewport: this.els.viewport,
      callbacks: this.callbacks(),
      settings: () => rdpSettings.get(),
      readOnly: !!this.ticket?.readOnly,
    }
    if (engine === 'guacd') {
      const { GuacAdapter } = await import('./guac')
      return new GuacAdapter(ctx)
    }
    const { IronRdpAdapter } = await import('./ironrdp')
    return new IronRdpAdapter(ctx)
  }

  private callbacks(): EngineCallbacks {
    const my = this.attempt
    const live = () => !this.stale(my)
    return {
      onStatus: (status, message) => {
        if (live()) patchViewer(this.tabId, { status, message })
      },
      onDesktopSize: (width, height) => {
        if (!live()) return
        const cur = getViewer(this.tabId)?.desktop
        if (cur?.width === width && cur?.height === height) return
        patchViewer(this.tabId, { desktop: { width, height } })
        this.layout()
      },
      onRemoteClipboard: (text, pending) => {
        if (!live()) return
        patchViewer(this.tabId, (s) => ({ remoteClipboardPending: pending, remoteClipboardText: text ?? s.remoteClipboardText }))
      },
      onEnded: (info) => {
        if (live()) this.ended(info)
      },
      onWarning: (message) => {
        if (live()) this.warnOnce(message)
      },
      onTransfer: (t) => {
        if (live()) this.transfer(t)
      },
    }
  }

  private failTicket(err: unknown): void {
    let code: ViewerErrorCode = 'other'
    let status: 'error' | 'gone' = 'error'
    let message = errorMessage(err)
    if (isApiError(err)) {
      if (err.status === 404 || err.status === 410) {
        status = 'gone'
        code = 'gone'
        message = 'The server may have restarted. Start a new session with the same settings.'
      } else if (err.status === 423) {
        code = 'locked'
        message = 'The vault is locked: unlock it to use the stored credentials.'
      } else if (err.code === 'guacd_unavailable') {
        code = 'guacd_unavailable'
      } else if (err.code === 'unsupported_security') {
        code = 'unsupported_security'
      } else if (err.code === 'credentials_required' || err.status === 408) {
        code = 'credentials'
      } else if (err.code === 'shadow_unavailable') {
        code = 'shadow_unavailable'
        message = 'This session can only be viewed while its owner is connected through guacd. IronRDP sessions run in the owner\'s browser and cannot be viewed.'
      }
    }
    patchViewer(this.tabId, { status, message, errorCode: code })
    setTabState(this.tabId, { status: 'error', progress: null })
  }

  private async failConnect(my: number, ticket: RdpTicketInfo, err: unknown): Promise<void> {
    const e = err instanceof EngineError ? err : new EngineError(errorMessage(err))
    let message = e.message
    let serverKnows = false
    if (e.preferServerMessage) {
      // The relay / tunnel recorded the precise reason in the session state.
      try {
        const s = await getSession(this.sessionId)
        if (s.state === 'error' && s.stateMessage) {
          message = s.stateMessage
          serverKnows = true
        }
      } catch {
        /* keep the engine's message */
      }
    }
    // guacd sessions are tracked by the tunnel; IronRDP failures past the relay are only known here.
    if (this.stale(my)) return // a newer attempt (reconnect) owns the tab now
    if (ticket.engine === 'ironrdp' && !serverKnows) {
      void reportState(this.sessionId, 'error', message, e.authFailed).catch(() => undefined)
    }
    this.teardown()
    patchViewer(this.tabId, { status: 'error', message, errorCode: e.code ?? 'other', authFailed: e.authFailed })
    setTabState(this.tabId, { status: 'error', progress: null })
    if (!e.authFailed && e.code !== 'credentials') this.scheduleAutoReconnect()
  }

  /** Reconnect with exponential backoff (1 s … 30 s) after an unexpected disconnection, if the connection asks for it. */
  private scheduleAutoReconnect(): boolean {
    const t = this.ticket
    if (!t?.autoReconnect || !this.everConnected || this.disposed || this.autoAttempts >= MAX_AUTO_RECONNECT) return false
    this.autoAttempts++
    const delay = Math.min(30_000, 1000 * 2 ** (this.autoAttempts - 1))
    patchViewer(this.tabId, { reconnectAt: Date.now() + delay, reconnectAttempt: this.autoAttempts })
    clearTimeout(this.autoTimer)
    this.autoTimer = setTimeout(() => void this.connect(), delay)
    return true
  }

  cancelAutoReconnect(): void {
    clearTimeout(this.autoTimer)
    this.autoTimer = undefined
    patchViewer(this.tabId, { reconnectAt: undefined, reconnectAttempt: undefined })
  }

  private ended(info: EndInfo): void {
    const engine = this.ticket?.engine
    const status = info.error ? 'error' : 'disconnected'
    if (engine === 'ironrdp' && !info.reported) {
      void reportState(this.sessionId, info.error ? 'error' : 'disconnected', info.message, info.authFailed).catch(() => undefined)
    }
    this.teardown()
    patchViewer(this.tabId, {
      status,
      message: info.message,
      errorCode: info.code,
      authFailed: info.authFailed,
      remoteClipboardPending: false,
      keyboardLocked: false,
    })
    setTabState(this.tabId, { status: info.error ? 'error' : 'disconnected', progress: null })
    if (info.error && !info.authFailed && info.code !== 'credentials') this.scheduleAutoReconnect()
  }

  // ---- sizing ------------------------------------------------------------------------------------------------------

  private viewportSize(): { width: number; height: number } {
    const r = this.els.viewport.getBoundingClientRect()
    return { width: r.width, height: r.height }
  }

  private currentRequest(): DesktopRequest {
    const vp = this.viewportSize()
    const usable = vp.width >= MIN_VIEWPORT && vp.height >= MIN_VIEWPORT ? vp : { width: 1280, height: 800 }
    return desktopRequest(usable, window.devicePixelRatio || 1, rdpSettings.get().hiDpi)
  }

  private isVisible(): boolean {
    const vp = this.viewportSize()
    return this.tabVisible && vp.width >= MIN_VIEWPORT && vp.height >= MIN_VIEWPORT
  }

  /** The dock tab was shown or hidden. */
  setVisible(visible: boolean): void {
    this.tabVisible = visible
    if (visible) requestAnimationFrame(() => this.onResize())
  }

  /** Apply the scaling mode to the current desktop. */
  layout(): void {
    const v = getViewer(this.tabId)
    const a = this.adapter
    if (!v?.desktop || !a) return
    const vp = this.viewportSize()
    const box = computeLayout(v.scaling, vp, v.desktop, window.devicePixelRatio || 1, rdpSettings.get().hiDpi)
    a.layout(box.scale, box.width, box.height)
  }

  /** The tab's size changed. */
  onResize(): void {
    this.layout()
    const v = getViewer(this.tabId)
    const t = this.ticket
    if (!v || !t || v.status !== 'connected' || t.fixedSize || !this.isVisible()) return
    if (t.resizeMethod === 'reconnect' && t.engine === 'ironrdp') {
      clearTimeout(this.reconnectTimer)
      this.reconnectTimer = setTimeout(() => {
        if (this.sizeChanged(this.currentRequest())) void this.connect()
      }, RECONNECT_RESIZE_DEBOUNCE)
      return
    }
    if (v.scaling !== 'resize' || t.resizeMethod === 'none') return
    clearTimeout(this.resizeTimer)
    this.resizeTimer = setTimeout(() => this.syncSize(0), RESIZE_DEBOUNCE)
  }

  /**
   * Ask the server for the tab's size until the remote desktop has it. The request is verified after a while and
   * repeated a few times: right after connecting the display channel may not be open yet (the request is dropped),
   * and servers may resize to something else.
   */
  private syncSize(attempt: number): void {
    clearTimeout(this.resizeTimer)
    const v = getViewer(this.tabId)
    const t = this.ticket
    const a = this.adapter
    if (!v || !t || !a || v.status !== 'connected' || v.scaling !== 'resize' || t.fixedSize || !this.isVisible()) return
    if (t.resizeMethod !== 'display-update' && !(t.engine === 'guacd' && t.resizeMethod === 'reconnect')) return
    const req = this.currentRequest()
    const d = v.desktop
    if (d && Math.abs(d.width - req.width) < RESIZE_TOLERANCE && Math.abs(d.height - req.height) < RESIZE_TOLERANCE) {
      this.lastRequested = req
      return
    }
    if (attempt >= RESIZE_ATTEMPTS) return // the server keeps its size: scaling fits the picture
    this.lastRequested = req
    a.requestSize(req)
    this.resizeTimer = setTimeout(() => this.syncSize(attempt + 1), RESIZE_VERIFY)
  }

  private sizeChanged(req: DesktopRequest): boolean {
    const last = this.lastRequested
    return (
      !last ||
      Math.abs(last.width - req.width) >= RESIZE_TOLERANCE ||
      Math.abs(last.height - req.height) >= RESIZE_TOLERANCE ||
      last.scaleFactor !== req.scaleFactor
    )
  }

  setScaling(mode: RdpScaling): void {
    patchViewer(this.tabId, { scaling: mode })
    updateTabParams<RdpTabParams>(this.tabId, { scaling: mode })
    this.layout()
    if (mode === 'resize') {
      this.lastRequested = undefined
      this.onResize()
    }
  }

  // ---- input -------------------------------------------------------------------------------------------------------

  private installListeners(): void {
    const vp = this.els.viewport
    // Host keys (handled before the remote sees them): Ctrl+Alt+End = Ctrl+Alt+Del (like mstsc),
    // Ctrl+Alt+Enter / Ctrl+Alt+Break = fullscreen.
    const onKeyDown = (e: KeyboardEvent) => {
      if (!e.isTrusted || !e.ctrlKey || !e.altKey || e.metaKey) return
      let handled = true
      if (e.code === 'End') this.ctrlAltDel()
      else if (e.code === 'Enter' || e.code === 'NumpadEnter' || e.code === 'Pause' || e.code === 'Cancel') void this.toggleFullscreen()
      else handled = false
      if (handled) {
        // Immediate: the guacd keyboard listens on this same element.
        e.preventDefault()
        e.stopImmediatePropagation()
        this.swallowKeyUp.add(e.code)
      }
    }
    const onKeyUp = (e: KeyboardEvent) => {
      if (e.isTrusted && this.swallowKeyUp.delete(e.code)) {
        e.preventDefault()
        e.stopImmediatePropagation()
      }
    }
    const onFocusIn = () => {
      patchViewer(this.tabId, { focused: true })
      this.adapter?.onViewerFocus()
    }
    const onFocusOut = (e: FocusEvent) => {
      if (e.relatedTarget instanceof Node && vp.contains(e.relatedTarget)) return
      patchViewer(this.tabId, { focused: false })
    }
    const onWindowFocus = () => {
      if (vp.contains(document.activeElement)) this.adapter?.onViewerFocus()
    }
    const onDragOver = (e: DragEvent) => {
      if (this.adapter?.canUpload() && e.dataTransfer?.types.includes('Files')) {
        e.preventDefault()
        e.dataTransfer.dropEffect = 'copy'
      }
    }
    const onDrop = (e: DragEvent) => {
      if (!this.adapter?.canUpload() || !e.dataTransfer?.files.length) return
      e.preventDefault()
      this.uploadFiles(Array.from(e.dataTransfer.files))
    }
    const onFullscreen = () => {
      const fs = document.fullscreenElement === this.els.root
      patchViewer(this.tabId, { fullscreen: fs, keyboardLocked: fs && getViewer(this.tabId)?.keyboardLocked === true })
      if (!fs) this.unlockKeyboard()
      requestAnimationFrame(() => this.onResize())
    }
    vp.addEventListener('keydown', onKeyDown, true)
    vp.addEventListener('keyup', onKeyUp, true)
    vp.addEventListener('focusin', onFocusIn)
    vp.addEventListener('focusout', onFocusOut)
    vp.addEventListener('dragover', onDragOver)
    vp.addEventListener('drop', onDrop)
    window.addEventListener('focus', onWindowFocus)
    document.addEventListener('fullscreenchange', onFullscreen)
    const ro = new ResizeObserver(() => this.onResize())
    ro.observe(vp)
    this.cleanups.push(() => {
      vp.removeEventListener('keydown', onKeyDown, true)
      vp.removeEventListener('keyup', onKeyUp, true)
      vp.removeEventListener('focusin', onFocusIn)
      vp.removeEventListener('focusout', onFocusOut)
      vp.removeEventListener('dragover', onDragOver)
      vp.removeEventListener('drop', onDrop)
      window.removeEventListener('focus', onWindowFocus)
      document.removeEventListener('fullscreenchange', onFullscreen)
      ro.disconnect()
    })
  }

  private connected(): EngineAdapter | undefined {
    return getViewer(this.tabId)?.status === 'connected' ? this.adapter : undefined
  }

  /** The connected engine, when this view may send input (not a read-only shadow view). */
  private interactive(): EngineAdapter | undefined {
    return getViewer(this.tabId)?.readOnly ? undefined : this.connected()
  }

  ctrlAltDel(): void {
    this.sendCombo(CTRL_ALT_DEL)
  }

  sendCombo(combo: KeyCombo): void {
    const a = this.interactive()
    if (!a) return
    a.sendCombo(combo)
  }

  async typeText(text: string): Promise<void> {
    const a = this.interactive()
    if (!a || !text) return
    if (text.length > MAX_TYPED_TEXT) {
      toast.error('The text is too long to type', { description: `At most ${MAX_TYPED_TEXT.toLocaleString()} characters.` })
      return
    }
    this.typing?.abort()
    const ctl = new AbortController()
    this.typing = ctl
    const id = toast.loading(`Typing ${text.length.toLocaleString()} characters…`, {
      action: { label: 'Stop', onClick: () => ctl.abort() },
    })
    try {
      await a.typeText(text, ctl.signal)
      toast.success('Text typed', { id })
    } catch (err) {
      if (err instanceof Cancelled) toast.info('Typing stopped', { id })
      else toast.error('Typing failed', { id, description: errorMessage(err) })
    } finally {
      if (this.typing === ctl) this.typing = undefined
    }
  }

  async sendClipboardText(text: string): Promise<void> {
    const a = this.interactive()
    if (!a) return
    await a.sendClipboardText(text)
  }

  async copyRemoteClipboard(): Promise<void> {
    const a = this.adapter
    if (!a) return
    await a.copyRemoteClipboard()
  }

  focus(): void {
    this.adapter?.focus()
  }

  canUpload(): boolean {
    return !getViewer(this.tabId)?.readOnly && !!this.adapter?.canUpload()
  }

  uploadFiles(files: File[]): void {
    if (!files.length) return
    if (!this.canUpload() || !this.adapter) {
      toast.error('File upload is not available', {
        description: 'Enable the shared drive on the connection and use the guacd engine to transfer files.',
      })
      return
    }
    this.adapter.uploadFiles(files)
  }

  // ---- screenshot / fullscreen / session ---------------------------------------------------------------------------

  async screenshot(): Promise<Blob | null> {
    const a = this.connected()
    return a ? a.screenshot() : null
  }

  async saveScreenshot(): Promise<void> {
    const blob = await this.screenshot()
    if (!blob) {
      toast.error('No remote desktop to capture')
      return
    }
    const title = (this.params()?.title || 'remote-desktop').replace(/[\\/:*?"<>|]+/g, '_').slice(0, 60)
    const stamp = new Date().toISOString().replace(/[:T]/g, '-').slice(0, 19)
    saveBlob(blob, `${title}-${stamp}.png`)
  }

  async copyScreenshot(): Promise<void> {
    const blob = await this.screenshot()
    if (!blob) {
      toast.error('No remote desktop to capture')
      return
    }
    try {
      await navigator.clipboard.write([new ClipboardItem({ 'image/png': blob })])
      toast.success('Screenshot copied to the clipboard')
    } catch (err) {
      toast.error('Could not copy the screenshot', { description: errorMessage(err) })
    }
  }

  async toggleFullscreen(): Promise<void> {
    const root = this.els.root
    try {
      if (document.fullscreenElement === root) {
        await document.exitFullscreen()
        return
      }
      await root.requestFullscreen({ navigationUI: 'hide' })
      if (rdpSettings.get().keyboardLock) await this.lockKeyboard()
      this.focus()
    } catch (err) {
      toast.error('Fullscreen is not available', { description: errorMessage(err) })
    }
  }

  private async lockKeyboard(): Promise<void> {
    const kb = (navigator as Navigator & { keyboard?: { lock?: (codes?: string[]) => Promise<void> } }).keyboard
    if (!kb?.lock) return
    try {
      await kb.lock()
      patchViewer(this.tabId, { keyboardLocked: true })
    } catch {
      /* not allowed (e.g. not a user gesture) */
    }
  }

  private unlockKeyboard(): void {
    const kb = (navigator as Navigator & { keyboard?: { unlock?: () => void } }).keyboard
    try {
      kb?.unlock?.()
    } catch {
      /* ignore */
    }
    patchViewer(this.tabId, { keyboardLocked: false })
  }

  reconnect(opts: { engine?: RdpEngine } = {}): void {
    if (opts.engine) updateTabParams<RdpTabParams>(this.tabId, { engine: opts.engine })
    this.autoAttempts = 0
    void this.connect(opts.engine)
  }

  disconnect(): void {
    this.attempt++
    this.cancelAutoReconnect()
    const engine = this.ticket?.engine
    this.teardown()
    if (engine === 'ironrdp') void reportState(this.sessionId, 'disconnected', 'Disconnected by the user').catch(() => undefined)
    patchViewer(this.tabId, { status: 'disconnected', message: 'Disconnected.', errorCode: undefined, authFailed: false })
    setTabState(this.tabId, { status: 'disconnected', progress: null })
  }

  // ---- misc --------------------------------------------------------------------------------------------------------

  private warned = new Set<string>()

  private warnOnce(message: string): void {
    if (this.warned.has(message)) return
    this.warned.add(message)
    // Clipboard warnings of an insecure context are expected on http:// LAN URLs; show them once, quietly.
    toast.warning('Remote desktop', { description: message })
  }

  private transfer(t: FileTransfer): void {
    patchViewer(this.tabId, (s) => {
      const list = s.transfers.filter((x) => x.id !== t.id)
      list.push(t)
      return { transfers: list.slice(-20) }
    })
    if (t.state === 'done' && t.direction === 'upload') toast.success(`Uploaded ${t.name} to the remote drive`)
    if (t.state === 'error') toast.error(`${t.direction === 'upload' ? 'Upload' : 'Download'} of ${t.name} failed`, { description: t.error })
  }
}
