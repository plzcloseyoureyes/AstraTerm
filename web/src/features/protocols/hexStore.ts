/*
 * Hex-monitor capture store (PROTO-12). While a session's hex monitor is open, its traffic is captured from the
 * terminal bus: RX = bytes received from the session (replayed history excluded, one tab per session so a session
 * shown in two tabs is not counted twice), TX = what the user typed plus bytes sent from the monitor's hex box.
 * Nothing is captured (and no bus listener is installed) while no monitor is open.
 *
 * This is a light module-level store (not zustand) so the hot append path never triggers React work directly; the
 * HexView subscribes and reads throttled snapshots through useSyncExternalStore.
 */
import { getTerminalsBySession, onTerminalInput, onTerminalOutput } from '@/features/terminal/bus'
import type { HexEvent } from './types'

const MAX_EVENTS = 4000
const MAX_BYTES = 128 * 1024

interface Buffer {
  events: HexEvent[]
  totalBytes: number
  rxBytes: number
  txBytes: number
}

const buffers = new Map<string, Buffer>()
const active = new Set<string>()
const listeners = new Set<() => void>()
const encoder = new TextEncoder()

let version = 0
let notifyTimer: ReturnType<typeof setTimeout> | null = null
let unsubscribeBus: (() => void) | null = null

function scheduleNotify(): void {
  if (notifyTimer) return
  // Coalesce bursts to at most ~15 renders per second.
  notifyTimer = setTimeout(() => {
    notifyTimer = null
    version++
    for (const l of Array.from(listeners)) l()
  }, 66)
}

function ensure(sessionId: string): Buffer {
  let b = buffers.get(sessionId)
  if (!b) {
    b = { events: [], totalBytes: 0, rxBytes: 0, txBytes: 0 }
    buffers.set(sessionId, b)
  }
  return b
}

/** Install the bus listeners while at least one monitor captures, remove them otherwise. */
function syncBus(): void {
  if (active.size > 0 && !unsubscribeBus) {
    const offOut = onTerminalOutput((e) => {
      if (e.replay || !active.has(e.sessionId)) return
      // A session can be shown in several tabs: count its output once (from its first tab).
      if (getTerminalsBySession(e.sessionId)[0]?.tabId !== e.tabId) return
      appendHex(e.sessionId, true, e.data.slice())
    })
    const offIn = onTerminalInput((e) => {
      if (active.has(e.sessionId)) appendHex(e.sessionId, false, encoder.encode(e.data))
    })
    unsubscribeBus = () => {
      offOut()
      offIn()
    }
  } else if (active.size === 0 && unsubscribeBus) {
    unsubscribeBus()
    unsubscribeBus = null
  }
}

/** True when the hex monitor is capturing this session. */
export function isCapturing(sessionId: string): boolean {
  return active.has(sessionId)
}

/** Append captured bytes. No-op unless capture is active for the session. */
export function appendHex(sessionId: string, rx: boolean, data: Uint8Array): void {
  if (data.length === 0 || !active.has(sessionId)) return
  const b = ensure(sessionId)
  b.events.push({ rx, ts: Date.now(), data })
  b.totalBytes += data.length
  if (rx) b.rxBytes += data.length
  else b.txBytes += data.length
  // Trim from the front when over the caps (the counters keep the totals).
  while (b.events.length > MAX_EVENTS || b.totalBytes > MAX_BYTES) {
    const ev = b.events.shift()
    if (!ev) break
    b.totalBytes -= ev.data.length
  }
  scheduleNotify()
}

/** Turn capture on/off for a session. */
export function setCapturing(sessionId: string, on: boolean): void {
  if (on) active.add(sessionId)
  else active.delete(sessionId)
  syncBus()
  scheduleNotify()
}

/** Clear a session's captured buffer and counters. */
export function clearHex(sessionId: string): void {
  buffers.delete(sessionId)
  scheduleNotify()
}

/** Drop everything about a session (it closed). */
export function forgetSession(sessionId: string): void {
  buffers.delete(sessionId)
  active.delete(sessionId)
  syncBus()
  scheduleNotify()
}

export function getBuffer(sessionId: string): Buffer | undefined {
  return buffers.get(sessionId)
}

export function subscribeHex(cb: () => void): () => void {
  listeners.add(cb)
  return () => listeners.delete(cb)
}

export function hexVersion(): number {
  return version
}
