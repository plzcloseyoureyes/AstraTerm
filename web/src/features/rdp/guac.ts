/*
 * guacd engine (RESEARCH §3.11 Path B): guacamole-common-js (lazy) over the Guacamole WebSocket tunnel at
 * /ws/guac/{sessionId}. The Go side performs the guacd handshake with the vault credentials — the ticket carries
 * none. Mouse + keyboard (+ touch), display scaling, clipboard, audio in/out, and the virtual drive (GFX-7): files
 * dropped on the desktop are uploaded to it; files the remote puts into its "Download" folder, and printed PDFs,
 * are saved by the browser.
 */
import type * as Guac from 'guacamole-common-js'
import { wsUrl } from '@/api/client'
import { saveBlob } from './api'
import { Cancelled, EngineError, sleep, type DesktopRequest, type EngineAdapter, type EngineContext } from './engine'
import { keysymForChar } from './keys'
import type { FileTransfer, KeyCombo, RdpTicketInfo } from './types'

type GuacModule = typeof import('guacamole-common-js')

let loading: Promise<GuacModule> | null = null

export function loadGuacamole(): Promise<GuacModule> {
  if (!loading) {
    loading = import('guacamole-common-js').then((m) => {
      const mod = m as unknown as { default?: GuacModule } & GuacModule
      return mod.default ?? mod
    })
    loading.catch(() => {
      loading = null
    })
  }
  return loading
}

/** Guacamole.Client states. */
const ClientState = { IDLE: 0, CONNECTING: 1, WAITING: 2, CONNECTED: 3, DISCONNECTING: 4, DISCONNECTED: 5 }

/** Guacamole status codes worth a specific message. */
function describeStatus(code: number, message: string): EngineError {
  const msg = (message || '').replace(/^\d+\s+/, '').trim()
  switch (code) {
    case 0x0301: // CLIENT_UNAUTHORIZED
      return new EngineError(msg && !/^\d+$/.test(msg) ? msg : 'Authentication failed: the server rejected the credentials.', {
        authFailed: /auth|credential|password|logon/i.test(msg) || !msg,
        code: 'credentials',
      })
    case 0x0303:
      return new EngineError(msg || 'Access denied.', { code: 'credentials', authFailed: true })
    case 0x0207:
      return new EngineError(msg || 'The remote desktop server could not be reached.', { code: 'network', preferServerMessage: true })
    case 0x0208:
      return new EngineError(msg || 'The remote desktop server is unavailable or refused the connection.', { code: 'network' })
    case 0x0209:
      return new EngineError(msg || 'The session was taken over by another connection.')
    case 0x020a:
      return new EngineError(msg || 'The remote session timed out.')
    case 0x020b:
      return new EngineError(msg || 'The remote session was closed.')
    case 0x0202:
      return new EngineError(msg || 'The remote desktop server is not responding.', { code: 'network' })
  }
  return new EngineError(msg || `The connection failed (Guacamole status ${code}).`, { preferServerMessage: !msg })
}

const AUDIO_INPUT_MIMETYPE = 'audio/L16;rate=44100,channels=2'
const CONNECT_TIMEOUT = 4 * 60_000

export class GuacAdapter implements EngineAdapter {
  readonly engine = 'guacd' as const
  private G?: GuacModule
  private client?: Guac.Client
  private tunnel?: Guac.WebSocketTunnel
  private display?: Guac.Display
  private keyboard?: Guac.Keyboard
  private ticket?: RdpTicketInfo
  private disposed = false
  private connected = false
  private ended = false
  private lastError?: EngineError
  private localCursor = false
  private lastClipboardSent = ''
  private remoteText = ''
  private cleanups: Array<() => void> = []
  private transferSeq = 0

  constructor(private readonly ctx: EngineContext) {}

  async connect(ticket: RdpTicketInfo, size: DesktopRequest): Promise<void> {
    const cb = this.ctx.callbacks
    this.ticket = ticket
    cb.onStatus('loading', 'Loading the Guacamole client…')
    let G: GuacModule
    try {
      G = await loadGuacamole()
    } catch (e) {
      throw new EngineError('The Guacamole client could not be loaded: ' + (e instanceof Error ? e.message : String(e)), {
        code: 'unsupported_browser',
      })
    }
    if (this.disposed) throw new Cancelled()
    this.G = G

    const tunnel = new G.WebSocketTunnel(wsUrl(`/ws/guac/${encodeURIComponent(this.ctx.sessionId)}`))
    const client = new G.Client(tunnel)
    const display = client.getDisplay()
    this.tunnel = tunnel
    this.client = client
    this.display = display
    const el = display.getElement()
    el.style.flex = 'none'
    this.ctx.stage.replaceChildren(el)

    this.setupMouse(G, client, display, el)
    if (!this.ctx.readOnly) this.setupKeyboard(G, client)
    client.onclipboard = (stream, mimetype) => this.receiveClipboard(G, stream, mimetype)
    client.onfile = (stream, mimetype, name) => this.receiveFile(G, stream, mimetype, name)
    display.onresize = (w, h) => {
      if (w > 0 && h > 0) cb.onDesktopSize(w, h)
    }

    const data = new URLSearchParams({
      token: ticket.token,
      width: String(ticket.fixedSize ? ticket.width : size.width),
      height: String(ticket.fixedSize ? ticket.height : size.height),
      dpi: String(size.dpi),
    })
    try {
      const tz = Intl.DateTimeFormat().resolvedOptions().timeZone
      if (tz) data.set('timezone', tz)
    } catch {
      /* no timezone support */
    }
    if (ticket.audio) for (const t of G.AudioPlayer.getSupportedTypes()) data.append('audio', t)
    for (const t of ['image/png', 'image/jpeg', 'image/webp']) data.append('image', t)

    cb.onStatus('connecting', `Connecting to ${ticket.destination} through guacd`)
    const up = new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => reject(new EngineError('The remote desktop did not answer in time.', { code: 'network' })), CONNECT_TIMEOUT)
      client.onstatechange = (state) => {
        if (state === ClientState.CONNECTED) {
          clearTimeout(timer)
          this.connected = true
          if (ticket.microphone && !this.ctx.readOnly) this.startMicrophone(G, client)
          resolve()
        } else if (state === ClientState.DISCONNECTED) {
          clearTimeout(timer)
          if (!this.connected) reject(this.lastError ?? new EngineError('The connection was closed.', { preferServerMessage: true }))
          else this.finish()
        }
      }
      const onError = (status: Guac.Status) => {
        this.lastError = describeStatus(status.code, status.message ?? '')
        if (!this.connected) {
          clearTimeout(timer)
          reject(this.lastError)
        } else {
          this.finish()
        }
      }
      client.onerror = onError
      tunnel.onerror = onError
    })
    client.connect(data.toString())
    try {
      await up
    } catch (e) {
      if (this.disposed) throw new Cancelled()
      safeCall(() => client.disconnect())
      throw e
    }
    if (this.disposed) throw new Cancelled()
    cb.onDesktopSize(display.getWidth(), display.getHeight())
  }

  /** The connection ended after being established. */
  private finish(): void {
    if (this.ended || this.disposed) return
    this.ended = true
    const err = this.lastError
    this.ctx.callbacks.onEnded(
      err
        ? { error: true, message: err.message, authFailed: err.authFailed, code: err.code, reported: true }
        : { message: 'The remote desktop session ended.', reported: true },
    )
  }

  private setupMouse(G: GuacModule, client: Guac.Client, display: Guac.Display, el: HTMLElement): void {
    const mouse = new G.Mouse(el)
    const send = (e: unknown) => {
      if (!this.connected || this.ctx.readOnly) return // a read-only view never moves the remote pointer
      client.sendMouseState((e as Guac.Mouse.Event).state, true)
      if (!this.localCursor) display.showCursor(true)
    }
    mouse.onEach(['mousedown', 'mouseup', 'mousemove'], send as never)
    mouse.on('mouseout', (() => display.showCursor(false)) as never)
    display.oncursor = (canvas, x, y) => {
      this.localCursor = this.ctx.settings().localCursor && mouse.setCursor(canvas, x, y)
      display.showCursor(!this.localCursor)
    }
    if (window.matchMedia?.('(pointer: coarse)').matches) {
      const touch = new G.Mouse.Touchscreen(el)
      touch.onEach(['mousedown', 'mouseup', 'mousemove'], send as never)
    }
    // Clicking the desktop gives the viewer the keyboard (Guacamole prevents the default focus change).
    const onDown = () => this.ctx.viewport.focus({ preventScroll: true })
    el.addEventListener('mousedown', onDown)
    el.addEventListener('touchstart', onDown, { passive: true })
    this.cleanups.push(() => {
      el.removeEventListener('mousedown', onDown)
      el.removeEventListener('touchstart', onDown)
    })
  }

  private setupKeyboard(G: GuacModule, client: Guac.Client): void {
    const kb = new G.Keyboard(this.ctx.viewport)
    kb.onkeydown = (keysym) => {
      if (this.connected) client.sendKeyEvent(1, keysym)
      return false
    }
    kb.onkeyup = (keysym) => {
      if (this.connected) client.sendKeyEvent(0, keysym)
    }
    this.keyboard = kb
    const onBlur = () => kb.reset()
    this.ctx.viewport.addEventListener('blur', onBlur)
    window.addEventListener('blur', onBlur)
    this.cleanups.push(() => {
      this.ctx.viewport.removeEventListener('blur', onBlur)
      window.removeEventListener('blur', onBlur)
      kb.onkeydown = null
      kb.onkeyup = null
    })
  }

  private startMicrophone(G: GuacModule, client: Guac.Client): void {
    const request = () => {
      if (this.disposed || !this.connected) return
      const stream = client.createAudioStream(AUDIO_INPUT_MIMETYPE)
      const recorder = G.AudioRecorder.getInstance(stream, AUDIO_INPUT_MIMETYPE)
      if (!recorder) {
        stream.sendEnd()
        this.ctx.callbacks.onWarning('The microphone is not available in this browser.')
        return
      }
      // A normal close (the remote stopped recording) reopens the stream for the next time; errors (access denied)
      // are reported once.
      recorder.onclose = request
      recorder.onerror = () => this.ctx.callbacks.onWarning('The microphone could not be used (access denied or no device).')
    }
    request()
  }

  private receiveClipboard(G: GuacModule, stream: Guac.InputStream, mimetype: string): void {
    if (!/^text\//i.test(mimetype)) {
      stream.sendAck('Unsupported clipboard type', G.Status.Code.UNSUPPORTED)
      return
    }
    const reader = new G.StringReader(stream)
    let text = ''
    reader.ontext = (t) => {
      if (text.length < 4 << 20) text += t
    }
    reader.onend = () => {
      this.remoteText = text
      this.lastClipboardSent = text // do not echo it back
      const auto = this.ctx.settings().autoClipboard && document.hasFocus() && !!navigator.clipboard?.writeText
      if (auto) {
        navigator.clipboard.writeText(text).then(
          () => this.ctx.callbacks.onRemoteClipboard(text, false),
          () => this.ctx.callbacks.onRemoteClipboard(text, true),
        )
      } else {
        this.ctx.callbacks.onRemoteClipboard(text, true)
      }
    }
  }

  private receiveFile(G: GuacModule, stream: Guac.InputStream, mimetype: string, name: string): void {
    // oxlint-disable-next-line no-control-regex -- file names from the remote must not carry control characters
    const safeName = (name || 'download').replace(/[\\/:*?"<>|\u0000-\u001f]/g, '_').slice(0, 200) || 'download'
    const t: FileTransfer = { id: `d${++this.transferSeq}`, name: safeName, direction: 'download', size: 0, done: 0, state: 'running' }
    this.ctx.callbacks.onTransfer({ ...t })
    const reader = new G.BlobReader(stream, mimetype || 'application/octet-stream')
    reader.onprogress = (length) => {
      t.done += length
      this.ctx.callbacks.onTransfer({ ...t })
    }
    reader.onend = () => {
      t.state = 'done'
      this.ctx.callbacks.onTransfer({ ...t })
      saveBlob(reader.getBlob(), safeName)
    }
    stream.sendAck('Ready', G.Status.Code.SUCCESS)
  }

  requestSize(size: DesktopRequest): void {
    if (!this.client || !this.connected || this.ticket?.fixedSize) return
    this.client.sendSize(size.width, size.height)
  }

  layout(scale: number): void {
    if (this.display && scale > 0 && Number.isFinite(scale)) this.display.scale(scale)
  }

  focus(): void {
    this.ctx.viewport.focus({ preventScroll: true })
  }

  releaseKeys(): void {
    this.keyboard?.reset()
  }

  sendCombo(combo: KeyCombo): void {
    const client = this.client
    if (!client || !this.connected) return
    for (const k of combo.keysyms) client.sendKeyEvent(1, k)
    for (const k of [...combo.keysyms].reverse()) client.sendKeyEvent(0, k)
    this.focus()
  }

  async typeText(text: string, signal: AbortSignal): Promise<void> {
    const client = this.client
    if (!client) return
    for (const ch of text) {
      if (signal.aborted || !this.connected) throw new Cancelled()
      const keysym = keysymForChar(ch)
      if (keysym === null) continue
      client.sendKeyEvent(1, keysym)
      client.sendKeyEvent(0, keysym)
      await sleep(8, signal)
    }
  }

  async sendClipboardText(text: string): Promise<void> {
    const G = this.G
    const client = this.client
    if (!G || !client || !this.connected) throw new EngineError('Not connected.')
    const stream = client.createClipboardStream('text/plain')
    const writer = new G.StringWriter(stream)
    // StringWriter chunks the text into blobs.
    writer.sendText(text)
    writer.sendEnd()
    this.lastClipboardSent = text
  }

  async copyRemoteClipboard(): Promise<void> {
    await navigator.clipboard.writeText(this.remoteText)
    this.ctx.callbacks.onRemoteClipboard(this.remoteText, false)
  }

  onViewerFocus(): void {
    if (this.ctx.readOnly || !this.ticket?.clipboard || !this.ctx.settings().autoClipboard || !this.connected) return
    if (!navigator.clipboard?.readText || !window.isSecureContext) return
    navigator.clipboard.readText().then(
      (text) => {
        if (text && text !== this.lastClipboardSent && text !== this.remoteText) void this.sendClipboardText(text)
      },
      () => undefined, // permission denied / not focused: the clipboard panel still works
    )
  }

  screenshot(): Promise<Blob | null> {
    const display = this.display
    if (!display || !this.connected) return Promise.resolve(null)
    const canvas = display.flatten()
    return new Promise((resolve) => canvas.toBlob((b) => resolve(b), 'image/png'))
  }

  canUpload(): boolean {
    return !this.ctx.readOnly && !!this.ticket?.drive && this.connected
  }

  uploadFiles(files: File[]): void {
    const G = this.G
    const client = this.client
    if (!G || !client || !this.canUpload()) return
    for (const file of files) {
      const t: FileTransfer = { id: `u${++this.transferSeq}`, name: file.name, direction: 'upload', size: file.size, done: 0, state: 'running' }
      this.ctx.callbacks.onTransfer({ ...t })
      const stream = client.createFileStream(file.type || 'application/octet-stream', file.name)
      const writer = new G.BlobWriter(stream)
      writer.onprogress = (_blob, offset) => {
        t.done = offset
        this.ctx.callbacks.onTransfer({ ...t })
      }
      writer.oncomplete = () => {
        writer.sendEnd()
        t.done = file.size
        t.state = 'done'
        this.ctx.callbacks.onTransfer({ ...t })
      }
      writer.onerror = (_blob, _offset, error) => {
        t.state = 'error'
        t.error = error?.message || 'Upload failed'
        this.ctx.callbacks.onTransfer({ ...t })
      }
      writer.onack = (status) => {
        if (status.isError()) {
          t.state = 'error'
          t.error = status.message || 'The remote drive refused the file'
          this.ctx.callbacks.onTransfer({ ...t })
        }
      }
      writer.sendBlob(file)
    }
  }

  destroy(): void {
    this.disposed = true
    for (const c of this.cleanups.splice(0)) c()
    const client = this.client
    if (client) {
      client.onstatechange = null
      client.onerror = null
      safeCall(() => client.disconnect())
    }
    this.display?.getElement().remove()
    this.client = undefined
    this.display = undefined
    this.keyboard = undefined
  }
}

function safeCall(fn: () => void): void {
  try {
    fn()
  } catch {
    /* ignore */
  }
}
