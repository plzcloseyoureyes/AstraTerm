/*
 * /ws/events singleton (SPEC §6.1).
 *
 *   events.start() / events.stop()                 lifecycle (App starts it once authenticated)
 *   events.on('session.updated', ev => ...)        typed listener → unsubscribe fn  ('*' via onAny)
 *   events.send({type: 'ping'})                    returns false when the socket is not open
 *   events.subscribe('monitor', {sessionId})       ref-counted topic subscription, re-sent after every reconnect
 *
 * Reconnects with exponential backoff + jitter (0.5s → 15s), immediately on `online` / tab becoming visible, and
 * detects dead connections with an application-level ping every 25s (no traffic for 70s → reconnect).
 */
import { useEffect } from 'react'
import { create } from 'zustand'
import { wsUrl } from '@/api/client'
import type { ClientEvent, ServerEvent, ServerEventOf, ServerEventType } from '@/api/types'

type EventsStatus = 'idle' | 'connecting' | 'open' | 'closed'

interface EventsState {
  status: EventsStatus
  clientId?: string
  /** Consecutive failed connection attempts since the last successful open. */
  failures: number
  /** ms epoch of the last successful open. */
  connectedAt?: number
  /** ms epoch of the next scheduled reconnect (status 'closed'). */
  retryAt?: number
}

export const useEventsStore = create<EventsState>(() => ({ status: 'idle', failures: 0 }))

type Listener = (ev: ServerEvent) => void

interface EventsHooks {
  /** Called after several consecutive failed connects (lets the app re-check auth: the cookie may have expired). */
  onRepeatedFailure?: () => void
}

const PING_INTERVAL = 25_000
const DEAD_AFTER = 70_000
const BACKOFF_MIN = 500
const BACKOFF_MAX = 15_000

class EventsSocket {
  private ws: WebSocket | null = null
  private wanted = false
  private listeners = new Map<string, Set<Listener>>()
  private subs = new Map<string, { msg: ClientEvent; count: number }>()
  private retryTimer: ReturnType<typeof setTimeout> | null = null
  private pingTimer: ReturnType<typeof setInterval> | null = null
  private lastMessageAt = 0
  private backoff = BACKOFF_MIN
  private hooks: EventsHooks = {}
  private windowListenersInstalled = false

  configure(hooks: EventsHooks) {
    this.hooks = { ...this.hooks, ...hooks }
  }

  get status(): EventsStatus {
    return useEventsStore.getState().status
  }

  get isOpen(): boolean {
    return this.ws?.readyState === WebSocket.OPEN
  }

  start(): void {
    if (this.wanted) return
    this.wanted = true
    this.installWindowListeners()
    this.connect()
  }

  stop(): void {
    this.wanted = false
    this.clearTimers()
    const ws = this.ws
    this.ws = null
    if (ws) {
      ws.onopen = ws.onclose = ws.onerror = ws.onmessage = null
      try {
        ws.close(1000, 'client stop')
      } catch {
        /* ignore */
      }
    }
    useEventsStore.setState({ status: 'idle', clientId: undefined, failures: 0, retryAt: undefined })
  }

  /** Force an immediate reconnect attempt (e.g. user clicked the status bar indicator). */
  reconnectNow(): void {
    if (!this.wanted) return
    this.backoff = BACKOFF_MIN
    if (this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING)) return
    this.clearTimers()
    this.connect()
  }

  on<T extends ServerEventType>(type: T, cb: (ev: ServerEventOf<T>) => void): () => void {
    return this.addListener(type, cb as Listener)
  }

  onAny(cb: (ev: ServerEvent) => void): () => void {
    return this.addListener('*', cb)
  }

  send(msg: ClientEvent): boolean {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return false
    try {
      this.ws.send(JSON.stringify(msg))
      return true
    } catch {
      return false
    }
  }

  /**
   * Ref-counted topic subscription (`{type:'subscribe', topic, ...params}`); automatically re-sent after reconnects.
   * The returned function releases this reference and sends `unsubscribe` when the last one goes away.
   */
  subscribe(topic: string, params: Record<string, unknown> = {}): () => void {
    const key = subKey(topic, params)
    const existing = this.subs.get(key)
    if (existing) {
      existing.count++
    } else {
      const msg = { type: 'subscribe', topic, ...params } as ClientEvent
      this.subs.set(key, { msg, count: 1 })
      this.send(msg)
    }
    let released = false
    return () => {
      if (released) return
      released = true
      const entry = this.subs.get(key)
      if (!entry) return
      entry.count--
      if (entry.count <= 0) {
        this.subs.delete(key)
        this.send({ type: 'unsubscribe', topic, ...params } as ClientEvent)
      }
    }
  }

  // --- internals ------------------------------------------------------------------------------------------------------

  private addListener(type: string, cb: Listener): () => void {
    let set = this.listeners.get(type)
    if (!set) {
      set = new Set()
      this.listeners.set(type, set)
    }
    set.add(cb)
    return () => {
      this.listeners.get(type)?.delete(cb)
    }
  }

  private emit(ev: ServerEvent): void {
    for (const key of [ev.type, '*']) {
      const set = this.listeners.get(key)
      if (!set) continue
      for (const cb of Array.from(set)) {
        try {
          cb(ev)
        } catch (err) {
          console.error(`[events] listener for "${ev.type}" failed`, err)
        }
      }
    }
  }

  private connect(): void {
    if (!this.wanted) return
    this.retryTimer = null
    useEventsStore.setState({ status: 'connecting', retryAt: undefined })
    let ws: WebSocket
    try {
      ws = new WebSocket(wsUrl('/ws/events'))
    } catch {
      this.scheduleReconnect()
      return
    }
    this.ws = ws
    let opened = false

    ws.onopen = () => {
      if (this.ws !== ws) return
      opened = true
      this.backoff = BACKOFF_MIN
      this.lastMessageAt = Date.now()
      useEventsStore.setState({ status: 'open', failures: 0, connectedAt: Date.now() })
      for (const { msg } of this.subs.values()) this.send(msg)
      this.startPing()
    }

    ws.onmessage = (e) => {
      if (this.ws !== ws) return
      this.lastMessageAt = Date.now()
      if (typeof e.data !== 'string') return
      let ev: ServerEvent
      try {
        ev = JSON.parse(e.data) as ServerEvent
      } catch {
        return
      }
      if (!ev || typeof ev !== 'object' || typeof (ev as { type?: unknown }).type !== 'string') return
      if (ev.type === 'hello') useEventsStore.setState({ clientId: ev.clientId })
      this.emit(ev)
    }

    ws.onerror = () => {
      /* onclose follows and handles reconnection */
    }

    ws.onclose = () => {
      if (this.ws !== ws) return
      this.ws = null
      this.stopPing()
      if (!this.wanted) return
      const failures = opened ? 0 : useEventsStore.getState().failures + 1
      useEventsStore.setState({ status: 'closed', clientId: undefined, failures })
      if (failures === 3 || (failures > 3 && failures % 10 === 0)) this.hooks.onRepeatedFailure?.()
      this.scheduleReconnect()
    }
  }

  private scheduleReconnect(): void {
    if (!this.wanted || this.retryTimer) return
    const jitter = Math.random() * 0.3 * this.backoff
    const delay = Math.min(BACKOFF_MAX, this.backoff + jitter)
    this.backoff = Math.min(BACKOFF_MAX, this.backoff * 2)
    useEventsStore.setState({ status: 'closed', retryAt: Date.now() + delay })
    this.retryTimer = setTimeout(() => this.connect(), delay)
  }

  private startPing(): void {
    this.stopPing()
    this.pingTimer = setInterval(() => {
      if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return
      if (Date.now() - this.lastMessageAt > DEAD_AFTER) {
        // Half-open connection: force a reconnect.
        try {
          this.ws.close(4000, 'ping timeout')
        } catch {
          /* ignore */
        }
        return
      }
      this.send({ type: 'ping' })
    }, PING_INTERVAL)
  }

  private stopPing(): void {
    if (this.pingTimer) clearInterval(this.pingTimer)
    this.pingTimer = null
  }

  private clearTimers(): void {
    if (this.retryTimer) clearTimeout(this.retryTimer)
    this.retryTimer = null
    this.stopPing()
  }

  private installWindowListeners(): void {
    if (this.windowListenersInstalled || typeof window === 'undefined') return
    this.windowListenersInstalled = true
    window.addEventListener('online', () => this.reconnectNow())
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'visible' && this.status === 'closed') this.reconnectNow()
    })
  }
}

function subKey(topic: string, params: Record<string, unknown>): string {
  const keys = Object.keys(params).sort()
  return topic + '|' + keys.map((k) => `${k}=${JSON.stringify(params[k])}`).join('&')
}

export const events = new EventsSocket()

/** Hold a ref-counted events topic subscription while mounted (e.g. `useEventsTopic('monitor', {sessionId})`). */
export function useEventsTopic(topic: string | null | undefined, params: Record<string, unknown> = {}): void {
  const key = topic ? subKey(topic, params) : ''
  useEffect(() => {
    if (!topic) return
    return events.subscribe(topic, params)
    // params are captured through `key`
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key])
}
