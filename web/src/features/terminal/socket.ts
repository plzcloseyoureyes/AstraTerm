/*
 * Terminal WebSocket client — SPEC §6.2 `/ws/terminal/{sessionId}?offset=<n>`.
 *
 *   server → client  binary: output bytes (contiguous stream)
 *                    text:   attach / attach-end / state / title / cwd / bell / readonly / resize / pong / error / prompt-mark
 *   client → server  binary: raw input bytes
 *                    text:   resize / ack / ping / reconnect / break / signal
 *
 * The socket reconnects with exponential backoff when the *transport* drops (the remote session keeps running on the
 * server); every attempt passes the offset the terminal already has (`getOffset()`), so the server replays only the
 * missing delta (or tells the client to reset). When a connection attempt fails before opening, the session is probed
 * over REST so a vanished session ("gone") stops the retry loop.
 */
import { isApiError, wsUrl } from '@/api/client'
import { getSession } from '@/api/sessions'
import type { RuntimeSession, TerminalClientMessage, TerminalServerMessage } from '@/api/types'
import type { TransportState } from './types'

export interface TerminalSocketHandlers {
  /** Binary output chunk (in stream order). */
  onData(chunk: Uint8Array): void
  /** Text control message. */
  onMessage(msg: TerminalServerMessage): void
  /** Transport state changes; `attempt` = consecutive failures. */
  onTransport(state: TransportState, attempt: number): void
  /** The session no longer exists on the server (404 on probe). */
  onGone(): void
  /** Fresh session info from the REST probe (optional). */
  onProbe?(session: RuntimeSession): void
}

export interface TerminalSocketOptions {
  /** Stream offset to resume from on (re)connect. */
  getOffset(): number
  /**
   * Consulted when an open connection drops: return false to stay disconnected (e.g. the session has ended and the
   * server closed the socket); `reconnectNow()` still works.
   */
  shouldReconnect?(): boolean
}

const PING_INTERVAL = 20_000
const DEAD_AFTER = 60_000
const BACKOFF = [250, 500, 1000, 2000, 4000, 8000, 10_000]

const encoder = new TextEncoder()

export class TerminalSocket {
  private ws: WebSocket | null = null
  private wanted = true
  private attempt = 0
  private retryTimer: ReturnType<typeof setTimeout> | null = null
  private stableTimer: ReturnType<typeof setTimeout> | null = null
  private pingTimer: ReturnType<typeof setInterval> | null = null
  private lastMessageAt = 0
  private everOpened = false
  private state: TransportState = 'connecting'
  private readonly onOnline = () => this.reconnectNow()
  private readonly onVisible = () => {
    if (document.visibilityState === 'visible' && this.state === 'reconnecting') this.reconnectNow()
  }

  constructor(
    readonly sessionId: string,
    private readonly handlers: TerminalSocketHandlers,
    private readonly opts: TerminalSocketOptions,
  ) {
    window.addEventListener('online', this.onOnline)
    document.addEventListener('visibilitychange', this.onVisible)
  }

  get transport(): TransportState {
    return this.state
  }

  get isOpen(): boolean {
    return this.ws?.readyState === WebSocket.OPEN
  }

  connect(): void {
    if (!this.wanted || this.ws) return
    this.clearRetry()
    this.setState(this.everOpened ? 'reconnecting' : 'connecting')
    let ws: WebSocket
    try {
      const offset = Math.max(0, Math.floor(this.opts.getOffset()))
      ws = new WebSocket(wsUrl(`/ws/terminal/${encodeURIComponent(this.sessionId)}`, { offset }))
    } catch (err) {
      console.warn('[terminal] websocket construction failed', err)
      this.scheduleReconnect(false)
      return
    }
    ws.binaryType = 'arraybuffer'
    this.ws = ws
    let opened = false

    ws.onopen = () => {
      if (this.ws !== ws) return
      opened = true
      this.everOpened = true
      // Only a connection that stays up resets the backoff (a server closing right away must not cause a tight loop).
      this.stableTimer = setTimeout(() => {
        this.stableTimer = null
        this.attempt = 0
      }, 5000)
      this.lastMessageAt = Date.now()
      this.startPing()
      this.setState('open')
    }

    ws.onmessage = (ev) => {
      if (this.ws !== ws) return
      this.lastMessageAt = Date.now()
      if (typeof ev.data === 'string') {
        let msg: TerminalServerMessage
        try {
          msg = JSON.parse(ev.data) as TerminalServerMessage
        } catch {
          return
        }
        if (!msg || typeof msg !== 'object' || typeof (msg as { type?: unknown }).type !== 'string') return
        if (msg.type === 'pong') return
        this.handlers.onMessage(msg)
        return
      }
      if (ev.data instanceof ArrayBuffer) {
        if (ev.data.byteLength) this.handlers.onData(new Uint8Array(ev.data))
      }
    }

    ws.onerror = () => {
      /* onclose follows */
    }

    ws.onclose = (ev) => {
      if (this.ws !== ws) return
      this.ws = null
      this.stopPing()
      this.clearStable()
      if (!this.wanted) return
      if (ev.code === 4404) {
        this.gone()
        return
      }
      if (opened && this.opts.shouldReconnect && !this.opts.shouldReconnect()) {
        this.setState('closed')
        return
      }
      this.scheduleReconnect(!opened)
    }
  }

  /** Close for good (tab closed / unmounted). */
  close(): void {
    this.wanted = false
    this.clearRetry()
    this.clearStable()
    this.stopPing()
    window.removeEventListener('online', this.onOnline)
    document.removeEventListener('visibilitychange', this.onVisible)
    const ws = this.ws
    this.ws = null
    if (ws) {
      ws.onopen = ws.onclose = ws.onerror = ws.onmessage = null
      try {
        ws.close(1000, 'client closed')
      } catch {
        /* ignore */
      }
    }
    this.setState('closed')
  }

  /** Retry immediately (user clicked "reconnect", browser went online). */
  reconnectNow(): void {
    if (!this.wanted || this.ws) return
    this.clearRetry()
    this.connect()
  }

  /** Drop the current transport and reconnect (e.g. to re-attach from a different offset). */
  restart(): void {
    if (!this.wanted) return
    const ws = this.ws
    this.ws = null
    if (ws) {
      ws.onopen = ws.onclose = ws.onerror = ws.onmessage = null
      try {
        ws.close(1000, 'restart')
      } catch {
        /* ignore */
      }
    }
    this.stopPing()
    this.clearStable()
    this.connect()
  }

  /** Raw input bytes (binary frame). Returns false when the socket is not open. */
  sendInput(data: string | Uint8Array): boolean {
    const ws = this.ws
    if (!ws || ws.readyState !== WebSocket.OPEN) return false
    try {
      // WebSocket.send wants an ArrayBuffer-backed view (copy views over shared memory).
      const bytes = typeof data === 'string' ? encoder.encode(data) : data.buffer instanceof ArrayBuffer ? (data as Uint8Array<ArrayBuffer>) : new Uint8Array(data)
      ws.send(bytes)
      return true
    } catch {
      return false
    }
  }

  /** JSON control message (text frame). */
  sendControl(msg: TerminalClientMessage): boolean {
    const ws = this.ws
    if (!ws || ws.readyState !== WebSocket.OPEN) return false
    try {
      ws.send(JSON.stringify(msg))
      return true
    } catch {
      return false
    }
  }

  /** Bytes queued in the browser's send buffer (for paced sending). */
  get bufferedAmount(): number {
    return this.ws?.bufferedAmount ?? 0
  }

  // --- internals ------------------------------------------------------------------------------------------------------

  private setState(s: TransportState): void {
    if (this.state === s && s !== 'reconnecting') return
    this.state = s
    this.handlers.onTransport(s, this.attempt)
  }

  private gone(): void {
    this.wanted = false
    this.clearRetry()
    this.setState('closed')
    this.handlers.onGone()
  }

  private scheduleReconnect(failedBeforeOpen: boolean): void {
    if (!this.wanted || this.retryTimer) return
    this.attempt++
    const base = BACKOFF[Math.min(this.attempt - 1, BACKOFF.length - 1)]
    const delay = base + Math.random() * 0.3 * base
    this.setState('reconnecting')
    this.retryTimer = setTimeout(() => {
      this.retryTimer = null
      if (!this.wanted) return
      if (failedBeforeOpen) void this.probeThenConnect()
      else this.connect()
    }, delay)
  }

  /** A handshake failed: make sure the session still exists before hammering the endpoint. */
  private async probeThenConnect(): Promise<void> {
    try {
      const s = await getSession(this.sessionId)
      if (!this.wanted) return
      this.handlers.onProbe?.(s)
      this.connect()
    } catch (err) {
      if (!this.wanted) return
      if (isApiError(err) && (err.status === 404 || err.status === 403)) {
        this.gone()
        return
      }
      // Network / server / auth errors: keep retrying (a 401 flips the app to the login screen, which unmounts us).
      this.scheduleReconnect(true)
    }
  }

  private startPing(): void {
    this.stopPing()
    this.pingTimer = setInterval(() => {
      const ws = this.ws
      if (!ws || ws.readyState !== WebSocket.OPEN) return
      if (Date.now() - this.lastMessageAt > DEAD_AFTER) {
        try {
          ws.close(4000, 'ping timeout')
        } catch {
          /* ignore */
        }
        return
      }
      this.sendControl({ type: 'ping' })
    }, PING_INTERVAL)
  }

  private stopPing(): void {
    if (this.pingTimer) clearInterval(this.pingTimer)
    this.pingTimer = null
  }

  private clearRetry(): void {
    if (this.retryTimer) clearTimeout(this.retryTimer)
    this.retryTimer = null
  }

  private clearStable(): void {
    if (this.stableTimer) clearTimeout(this.stableTimer)
    this.stableTimer = null
  }
}
