/*
 * IronRDP engine (RESEARCH §3.10 Path A): the RDP client runs in the browser as WebAssembly
 * (@devolutions/iron-remote-desktop web component + the -rdp backend, lazy-loaded), talking RDCleanPath to the Go
 * relay at /ws/rdp/{sessionId} with the one-time ticket.
 *
 * The web component sizes its canvas from the window size, which does not fit docked tabs: it is created with an
 * unknown `scale` (so it applies none of its own sizing) and a stylesheet in its (open) shadow root makes the canvas
 * fill the host element, which the controller sizes. Its canvas focuses itself on hover; that is reduced to
 * click-to-focus so moving the pointer across the desktop does not steal the keyboard from other tabs.
 */
import type { IronError, NewSessionInfo, UserInteraction } from '@devolutions/iron-remote-desktop'
import { wsUrl } from '@/api/client'
import { saveBlob } from './api'
import { Cancelled, EngineError, sleep, type DesktopRequest, type EngineAdapter, type EngineContext } from './engine'
import type { KeyCombo, RdpTicketInfo } from './types'

type RdpModule = typeof import('@devolutions/iron-remote-desktop-rdp')

let loading: Promise<RdpModule> | null = null

/** Load the web component and the WASM backend once per page. */
function loadIronRdp(): Promise<RdpModule> {
  if (!loading) {
    loading = (async () => {
      await import('@devolutions/iron-remote-desktop') // defines <iron-remote-desktop>
      const rdp = await import('@devolutions/iron-remote-desktop-rdp')
      await initWasm(rdp.init)
      return rdp
    })()
    loading.catch(() => {
      loading = null
    })
  }
  return loading
}

/**
 * The backend embeds its WebAssembly as a data: URL and fetch()es it, which AstraTerm's Content-Security-Policy
 * (connect-src 'self') forbids. While it initializes, fetch() answers that one URL from memory instead.
 */
async function initWasm(init: (level: string) => Promise<void>): Promise<void> {
  const original = window.fetch
  const patched: typeof window.fetch = (input, initArg) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
    if (url.startsWith('data:application/wasm;base64,')) {
      const bin = atob(url.slice(url.indexOf(',') + 1))
      const bytes = new Uint8Array(bin.length)
      for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
      return Promise.resolve(new Response(bytes, { headers: { 'Content-Type': 'application/wasm' } }))
    }
    return original(input, initArg)
  }
  window.fetch = patched
  try {
    await init(import.meta.env.DEV ? 'INFO' : 'WARN')
  } finally {
    if (window.fetch === patched) window.fetch = original
  }
}

const SHADOW_CSS = `
:host { display: block; }
div { width: 100% !important; height: 100% !important; min-width: 0 !important; min-height: 0 !important;
  max-width: none !important; max-height: none !important; overflow: hidden !important; }
canvas { display: block; width: 100% !important; height: 100% !important; outline: none; image-rendering: auto; }
.capturing-inputs { outline: none !important; }
`

/** IronErrorKind (0.x API, see index.d.ts). */
const IronKind = { General: 0, WrongPassword: 1, LogonFailure: 2, AccessDenied: 3, RDCleanPath: 4, ProxyConnect: 5, NegotiationFailure: 6 }

function isIronError(e: unknown): e is IronError {
  return !!e && typeof e === 'object' && typeof (e as IronError).kind === 'function' && typeof (e as IronError).backtrace === 'function'
}

/** Turn an IronRDP failure into a readable EngineError. */
function describeIronError(e: unknown): EngineError {
  if (!isIronError(e)) return new EngineError(e instanceof Error ? e.message : String(e))
  let detail = ''
  try {
    detail = cleanDetail(firstLine(e.backtrace()))
  } catch {
    /* ignore */
  }
  switch (e.kind()) {
    case IronKind.WrongPassword:
      return new EngineError('The user name or password is incorrect.', { authFailed: true, code: 'credentials' })
    case IronKind.LogonFailure: {
      const status = /STATUS_[A-Z_]+/.exec(detail)?.[0]
      return new EngineError(`Logon failure: the server rejected the user name or password${status ? ` (${status})` : ''}.`, {
        authFailed: true,
        code: 'credentials',
      })
    }
    case IronKind.AccessDenied:
      return new EngineError('Access denied: this account may not sign in remotely.', { authFailed: true, code: 'credentials' })
    case IronKind.RDCleanPath: {
      const d = safe(() => e.rdcleanpathDetails())
      const parts: string[] = []
      if (d?.httpStatusCode) parts.push(`HTTP ${d.httpStatusCode}`)
      if (d?.wsaErrorCode) parts.push(`socket error ${d.wsaErrorCode}`)
      if (d?.tlsAlertCode) parts.push(`TLS alert ${d.tlsAlertCode}`)
      return new EngineError(`The connection relay reported an error${parts.length ? ` (${parts.join(', ')})` : ''}.`, {
        preferServerMessage: true,
        code: 'network',
      })
    }
    case IronKind.ProxyConnect:
      return new EngineError('Cannot reach the AstraTerm connection relay.', { preferServerMessage: true, code: 'network' })
    case IronKind.NegotiationFailure:
      return new EngineError('Security negotiation failed' + (detail ? `: ${detail}` : '.'), { preferServerMessage: true })
  }
  return new EngineError(detail || 'The RDP connection failed.', { preferServerMessage: true })
}

function safe<T>(fn: () => T): T | undefined {
  try {
    return fn()
  } catch {
    return undefined
  }
}

function firstLine(s: string): string {
  const line = (s || '').split('\n').map((l) => l.trim()).find(Boolean) ?? ''
  return line.length > 300 ? `${line.slice(0, 300)}…` : line
}

/** Drop IronRDP's internal source locations ("[CredSSP @ …/connector.rs:107]", "[decode error @ …]") from a message. */
function cleanDetail(s: string): string {
  return s
    .replace(/\[([^\]@]*?)\s*@\s*[^\]]*\]\s*/g, (_, what: string) => (what.trim() ? `${what.trim()}: ` : ''))
    .replace(/(\b[\w ]+): \1: /g, '$1: ')
    .replace(/\s+/g, ' ')
    .replace(/:\s*$/, '')
    .trim()
}

type IronElement = HTMLElement & { module?: unknown }

const CONNECT_TIMEOUT = 4 * 60_000 // includes the time a certificate / credential prompt may be open

export class IronRdpAdapter implements EngineAdapter {
  readonly engine = 'ironrdp' as const
  private el?: IronElement
  private ui?: UserInteraction
  private canvas?: HTMLCanvasElement
  private ticket?: RdpTicketInfo
  private disposed = false
  private allowFocus = false
  private connected = false
  private cleanups: Array<() => void> = []

  constructor(private readonly ctx: EngineContext) {}

  async connect(ticket: RdpTicketInfo, size: DesktopRequest): Promise<void> {
    const cb = this.ctx.callbacks
    this.ticket = ticket
    cb.onStatus('loading', 'Loading the RDP client…')
    let rdp: RdpModule
    try {
      rdp = await loadIronRdp()
    } catch (e) {
      throw new EngineError('The RDP client could not be loaded: ' + (e instanceof Error ? e.message : String(e)), {
        code: 'unsupported_browser',
      })
    }
    if (this.disposed) throw new Cancelled()
    const ui = await this.mount(rdp, ticket)
    if (this.disposed) throw new Cancelled()

    const builder = ui
      .configBuilder()
      .withUsername(ticket.username || '')
      .withPassword(ticket.password || '')
      .withDestination(ticket.destination)
      .withServerDomain(ticket.domain || '')
      .withProxyAddress(wsUrl(`/ws/rdp/${encodeURIComponent(this.ctx.sessionId)}`))
      .withAuthToken(ticket.token)
      .withDesktopSize({ width: ticket.fixedSize ? ticket.width : size.width, height: ticket.fixedSize ? ticket.height : size.height })
      .withExtension(rdp.displayControl(!ticket.fixedSize && ticket.resizeMethod === 'display-update'))
      .withExtension(rdp.enableCredssp(ticket.enableCredssp))
    if (ticket.preConnectionBlob) builder.withExtension(rdp.preConnectionBlob(ticket.preConnectionBlob))
    if (ticket.printing) for (const ext of this.printing(rdp)) builder.withExtension(ext)
    const config = builder.build()

    cb.onStatus('connecting', `Connecting to ${ticket.destination}`)
    let info: NewSessionInfo
    try {
      info = await Promise.race([
        ui.connect(config),
        sleep(CONNECT_TIMEOUT).then(() => {
          throw new EngineError('The remote desktop did not answer in time.', { code: 'network' })
        }),
      ])
    } catch (e) {
      if (this.disposed) throw new Cancelled()
      throw e instanceof EngineError ? e : describeIronError(e)
    }
    if (this.disposed) {
      safe(() => ui.shutdown())
      throw new Cancelled()
    }
    this.connected = true
    ui.setVisibility(true)
    cb.onDesktopSize(info.initialDesktopSize.width, info.initialDesktopSize.height)
    info
      .run()
      .then((term) => {
        if (this.disposed) return
        const reason = safe(() => term.reason()) || ''
        cb.onEnded({ message: humanReason(reason) })
      })
      .catch((e) => {
        if (this.disposed) return
        const err = describeIronError(e)
        cb.onEnded({ error: true, message: err.message, authFailed: err.authFailed, code: err.code })
      })
  }

  /**
   * Printer redirection (GFX-9): the remote prints to "AstraTerm PDF" with the Microsoft Print to PDF driver; finished
   * jobs are saved by the browser.
   */
  private printing(rdp: RdpModule): unknown[] {
    const jobs = new Map<number, { parts: Uint8Array[]; size: number }>()
    const MAX_JOB = 256 << 20
    return [
      rdp.printerName('AstraTerm PDF'),
      rdp.printerDriverName(rdp.PrinterDriverName.MicrosoftPrintToPdf),
      rdp.printJobStreamCallbacks({
        onJobStart: (id) => jobs.set(id, { parts: [], size: 0 }),
        onJobData: (id, chunk) => {
          let job = jobs.get(id)
          if (!job) jobs.set(id, (job = { parts: [], size: 0 }))
          if (job.size + chunk.byteLength > MAX_JOB) return
          job.parts.push(chunk.slice()) // the chunk is a view into WASM memory
          job.size += chunk.byteLength
        },
        onJobComplete: (id) => {
          const job = jobs.get(id)
          jobs.delete(id)
          if (!job?.size) return
          const stamp = new Date().toISOString().replace(/[:T]/g, '-').slice(0, 19)
          saveBlob(new Blob(job.parts as BlobPart[], { type: 'application/pdf' }), `remote-print-${stamp}.pdf`)
        },
        onJobError: (id) => {
          jobs.delete(id)
          this.ctx.callbacks.onWarning('A print job from the remote desktop failed.')
        },
      }),
    ]
  }

  /** Create the web component and wait for its "ready" event. */
  private mount(rdp: RdpModule, ticket: RdpTicketInfo): Promise<UserInteraction> {
    return new Promise((resolve, reject) => {
      const el = document.createElement('iron-remote-desktop') as IronElement
      // Unknown scale: the component applies none of its window-based sizing (see the module comment).
      el.setAttribute('scale', 'none')
      el.setAttribute('verbose', 'false')
      el.setAttribute('flexcenter', 'false')
      el.module = rdp.Backend
      el.style.display = 'block'
      el.style.flex = 'none'
      const timer = setTimeout(() => reject(new EngineError('The RDP component did not start.', { code: 'unsupported_browser' })), 15_000)
      el.addEventListener(
        'ready',
        (e) => {
          clearTimeout(timer)
          const ui = (e as CustomEvent<{ irgUserInteraction: UserInteraction }>).detail.irgUserInteraction
          // Must be configured before the component initializes its clipboard (right after this event).
          ui.setEnableClipboard(ticket.clipboard)
          ui.setEnableAutoClipboard(ticket.clipboard && this.ctx.settings().autoClipboard)
          ui.onWarningCallback((msg) => this.ctx.callbacks.onWarning(msg))
          ui.onClipboardRemoteUpdateCallback(() => this.ctx.callbacks.onRemoteClipboard(null, true))
          this.ui = ui
          this.decorate(el)
          resolve(ui)
        },
        { once: true },
      )
      this.el = el
      this.ctx.stage.replaceChildren(el)
    })
  }

  private decorate(el: IronElement): void {
    const root = el.shadowRoot
    if (!root) return
    const style = document.createElement('style')
    style.textContent = SHADOW_CSS
    root.appendChild(style) // appended: the component checks that its root div is the shadow root's first element
    const canvas = root.querySelector('canvas')
    if (!canvas) return
    this.canvas = canvas
    // Click-to-focus: the component focuses the canvas on mouseenter; only allow that when the viewer already has
    // the keyboard or AstraTerm focuses it on purpose (mouse clicks focus the canvas natively).
    const nativeFocus = HTMLElement.prototype.focus
    const viewport = this.ctx.viewport
    canvas.focus = (opts?: FocusOptions) => {
      if (this.allowFocus || viewport.contains(document.activeElement)) nativeFocus.call(canvas, opts)
    }
    // The component cancels mousedown (so clicks never focus the canvas natively): focus it on press, and when the
    // viewport itself gets the focus (Tab key, click on the letterbox).
    const onPointerDown = () => this.focus()
    const onViewportFocus = (e: FocusEvent) => {
      if (e.target === viewport) this.focus()
    }
    viewport.addEventListener('pointerdown', onPointerDown, true)
    viewport.addEventListener('focus', onViewportFocus)
    this.cleanups.push(() => {
      viewport.removeEventListener('pointerdown', onPointerDown, true)
      viewport.removeEventListener('focus', onViewportFocus)
    })
    const track = new MutationObserver(() => {
      if (this.connected && canvas.width > 0 && canvas.height > 0) this.ctx.callbacks.onDesktopSize(canvas.width, canvas.height)
    })
    track.observe(canvas, { attributes: true, attributeFilter: ['width', 'height'] })
    this.cleanups.push(() => track.disconnect())
  }

  requestSize(size: DesktopRequest): void {
    if (!this.ui || !this.connected || this.ticket?.fixedSize) return
    this.ui.resize(size.width, size.height, size.scaleFactor)
  }

  layout(_scale: number, cssWidth: number, cssHeight: number): void {
    if (!this.el) return
    this.el.style.width = `${Math.max(1, Math.round(cssWidth))}px`
    this.el.style.height = `${Math.max(1, Math.round(cssHeight))}px`
  }

  focus(): void {
    if (!this.canvas) return
    this.allowFocus = true
    try {
      this.canvas.focus({ preventScroll: true })
    } finally {
      this.allowFocus = false
    }
  }

  releaseKeys(): void {
    // The component releases every key and button when the canvas loses focus or the pointer leaves it; a blur
    // is the public way to trigger that.
    if (this.canvas && this.el?.shadowRoot?.activeElement === this.canvas) this.canvas.blur()
  }

  ctrlAltDel(): void {
    this.ui?.ctrlAltDel()
  }

  sendCombo(combo: KeyCombo): void {
    if (combo.id === 'ctrl-alt-del') {
      this.ctrlAltDel()
      return
    }
    if (combo.id === 'win') {
      this.ui?.metaKey()
      return
    }
    this.focus()
    const mods = { ctrlKey: false, altKey: false, shiftKey: false, metaKey: false }
    for (const code of combo.codes) {
      setModifier(mods, code, true)
      this.key('keydown', code, keyName(code), mods)
    }
    for (const code of [...combo.codes].reverse()) {
      this.key('keyup', code, keyName(code), mods)
      setModifier(mods, code, false)
    }
  }

  async typeText(text: string, signal: AbortSignal): Promise<void> {
    const ui = this.ui
    if (!ui) return
    this.focus()
    ui.setKeyboardUnicodeMode(true)
    const mods = { ctrlKey: false, altKey: false, shiftKey: false, metaKey: false }
    try {
      for (const ch of text) {
        if (signal.aborted) throw new Cancelled()
        let code = ''
        let key = ch
        if (ch === '\n' || ch === '\r') [code, key] = ['Enter', 'Enter']
        else if (ch === '\t') [code, key] = ['Tab', 'Tab']
        else if (ch.length !== 1 || ch < ' ') continue // characters outside the BMP / controls cannot be typed
        this.key('keydown', code, key, mods)
        this.key('keyup', code, key, mods)
        await sleep(8, signal)
      }
    } finally {
      ui.setKeyboardUnicodeMode(false)
    }
  }

  /** Dispatch a synthetic key event from the canvas (the component handles focused-canvas key events on window). */
  private key(type: 'keydown' | 'keyup', code: string, key: string, mods: KeyMods): void {
    const target = this.canvas ?? window
    target.dispatchEvent(new KeyboardEvent(type, { code, key, bubbles: true, composed: true, cancelable: true, ...mods }))
  }

  async sendClipboardText(text: string): Promise<void> {
    if (!this.ui) return
    // The component sends the local clipboard; put the text there first.
    await navigator.clipboard.writeText(text)
    await this.ui.sendClipboardData()
  }

  async copyRemoteClipboard(): Promise<void> {
    await this.ui?.saveRemoteClipboardData()
    this.ctx.callbacks.onRemoteClipboard(null, false)
  }

  onViewerFocus(): void {
    // Automatic mode is driven by the component itself (it polls the clipboard while the page has focus).
  }

  screenshot(): Promise<Blob | null> {
    const canvas = this.canvas
    if (!canvas || !this.connected) return Promise.resolve(null)
    return new Promise((resolve) => canvas.toBlob((b) => resolve(b), 'image/png'))
  }

  canUpload(): boolean {
    return false
  }

  uploadFiles(): void {
    /* Drive redirection (RDPDR) is not exposed by the IronRDP web API. */
  }

  destroy(): void {
    this.disposed = true
    for (const c of this.cleanups.splice(0)) c()
    if (this.ui) safe(() => this.ui!.shutdown())
    this.el?.remove()
    this.ui = undefined
    this.el = undefined
    this.canvas = undefined
  }
}

type KeyMods = { ctrlKey: boolean; altKey: boolean; shiftKey: boolean; metaKey: boolean }

function setModifier(m: KeyMods, code: string, down: boolean): void {
  if (code.startsWith('Control')) m.ctrlKey = down
  else if (code.startsWith('Alt')) m.altKey = down
  else if (code.startsWith('Shift')) m.shiftKey = down
  else if (code.startsWith('Meta')) m.metaKey = down
}

function keyName(code: string): string {
  if (code.startsWith('Key')) return code.slice(3).toLowerCase()
  if (code.startsWith('Control')) return 'Control'
  if (code.startsWith('Alt')) return 'Alt'
  if (code.startsWith('Shift')) return 'Shift'
  if (code.startsWith('Meta')) return 'Meta'
  return code
}

/** Session termination reasons of IronRDP are terse; make the common ones readable. */
function humanReason(reason: string): string {
  const r = reason.trim()
  if (!r) return 'The remote desktop session ended.'
  if (/logoff|logged off/i.test(r)) return 'You were signed out of the remote session.'
  if (/another (user|session)|replaced/i.test(r)) return 'The session was taken over by another connection.'
  if (/idle|timeout/i.test(r)) return 'The remote session was disconnected because it was idle.'
  return r.charAt(0).toUpperCase() + r.slice(1)
}
