/*
 * Live monitoring feeds. Components call useMonitorFeed(sessionId, active): the store keeps one ref-counted
 * `{type:'subscribe', topic:'monitor', sessionId}` per session while at least one consumer is active and a Termstead
 * window is visible (the remote exec channel counts toward the server's MaxSessions, RESEARCH §3.21), and records the
 * samples plus a 10-minute history for charts and sparklines. 'local' is the Termstead host itself.
 */
import { useEffect } from 'react'
import { create } from 'zustand'
import { events } from '@/lib/events'
import { getPopoutWindows, onWorkspaceWindow } from '@/stores/workspace'
import { monitorSettings } from './settings'
import type { FeedState, MonitorEvent, Stats } from './types'

/** One history point (percentages 0–100, rates in bytes/s). */
export interface Sample {
  t: number
  cpu: number
  user: number
  system: number
  iowait: number
  steal: number
  memPct: number
  cachePct: number
  swapPct: number
  rx: number
  tx: number
  load1: number
  load5: number
  load15: number
  read: number
  write: number
}

export interface FeedEntry {
  stats?: Stats
  error?: string
  state?: FeedState
  history: Sample[]
  /** ms epoch of the last stats. */
  receivedAt?: number
}

interface MonitorStore {
  feeds: Record<string, FeedEntry>
}

export const useMonitorStore = create<MonitorStore>(() => ({ feeds: {} }))

const HISTORY_MS = 10 * 60_000

function toSample(st: Stats, t: number): Sample {
  const pct = (a: number, b: number) => (b > 0 ? Math.min(100, Math.max(0, (a / b) * 100)) : 0)
  return {
    t,
    cpu: st.cpu?.usage ?? 0,
    user: st.cpu?.user ?? 0,
    system: st.cpu?.system ?? 0,
    iowait: st.cpu?.iowait ?? 0,
    steal: st.cpu?.steal ?? 0,
    memPct: pct(st.mem?.used ?? 0, st.mem?.total ?? 0),
    cachePct: pct(st.mem?.cached ?? 0, st.mem?.total ?? 0),
    swapPct: pct(st.mem?.swapUsed ?? 0, st.mem?.swapTotal ?? 0),
    rx: st.warmup ? NaN : (st.netTotal?.rxBps ?? 0),
    tx: st.warmup ? NaN : (st.netTotal?.txBps ?? 0),
    load1: st.load?.[0] ?? 0,
    load5: st.load?.[1] ?? 0,
    load15: st.load?.[2] ?? 0,
    read: st.warmup ? NaN : (st.diskIo?.readBps ?? 0),
    write: st.warmup ? NaN : (st.diskIo?.writeBps ?? 0),
  }
}

function gapSample(t: number): Sample {
  return { t, cpu: NaN, user: NaN, system: NaN, iowait: NaN, steal: NaN, memPct: NaN, cachePct: NaN, swapPct: NaN, rx: NaN, tx: NaN, load1: NaN, load5: NaN, load15: NaN, read: NaN, write: NaN }
}

function applyEvent(ev: MonitorEvent): void {
  if (!ev || typeof ev.sessionId !== 'string') return
  useMonitorStore.setState((s) => {
    const prev = s.feeds[ev.sessionId] ?? { history: [] }
    let next: FeedEntry
    if (ev.stats) {
      const t = Date.parse(ev.stats.ts) || Date.now()
      const last = prev.history[prev.history.length - 1]
      let history = prev.history
      if (!last || t > last.t) {
        const cut = t - HISTORY_MS
        history = prev.history.filter((p) => p.t >= cut)
        // Sampling paused (hidden window, other tab, reconnect): break chart lines instead of bridging the gap.
        if (last && t - last.t > Math.max(10_000, (ev.stats.intervalSec || 2) * 4_000)) history.push(gapSample(last.t + 1))
        history.push(toSample(ev.stats, t))
      }
      next = { stats: ev.stats, history, receivedAt: Date.now() }
    } else {
      next = { ...prev, error: ev.error || 'Monitoring unavailable', state: ev.state ?? 'error' }
    }
    return { feeds: { ...s.feeds, [ev.sessionId]: next } }
  })
}

function setFeedError(sessionId: string, error: string, state: FeedState): void {
  useMonitorStore.setState((s) => {
    const prev = s.feeds[sessionId] ?? { history: [] }
    return { feeds: { ...s.feeds, [sessionId]: { ...prev, error, state } } }
  })
}

let installed = false

function install(): void {
  if (installed) return
  installed = true
  events.on('monitor', (ev) => applyEvent(ev as unknown as MonitorEvent))
  events.on('subscribe.error', (ev) => {
    const id = ev.params?.sessionId
    if (ev.topic === 'monitor' && typeof id === 'string') setFeedError(id, ev.error, 'unavailable')
  })
  events.on('session.closed', (ev) => {
    if (typeof ev.id !== 'string') return
    // Keep the entry briefly so open views can show the last numbers, then forget the history.
    setTimeout(() => {
      if (refs.has(ev.id)) return
      useMonitorStore.setState((s) => {
        if (!s.feeds[ev.id]) return s
        const feeds = { ...s.feeds }
        delete feeds[ev.id]
        return { feeds }
      })
    }, 30_000)
  })
  const onVisibility = () => reconcile()
  document.addEventListener('visibilitychange', onVisibility)
  onWorkspaceWindow((w) => w.document.addEventListener('visibilitychange', onVisibility))
  monitorSettings.subscribe(() => reconcile())
}

// --- ref-counted subscriptions --------------------------------------------------------------------------------------

const refs = new Map<string, number>()
const active = new Map<string, () => void>()

/** Visible if the main window or any pop-out workspace window is. */
function anyWindowVisible(): boolean {
  if (typeof document === 'undefined') return true
  if (document.visibilityState !== 'hidden') return true
  for (const w of getPopoutWindows()) {
    try {
      if (w.document.visibilityState !== 'hidden') return true
    } catch {
      /* closed */
    }
  }
  return false
}

function reconcile(): void {
  const paused = monitorSettings.get().pauseHidden && !anyWindowVisible()
  for (const [id, n] of refs) {
    if (n > 0 && !paused && !active.has(id)) active.set(id, events.subscribe('monitor', { sessionId: id }))
  }
  for (const [id, unsubscribe] of Array.from(active)) {
    if (paused || !((refs.get(id) ?? 0) > 0)) {
      unsubscribe()
      active.delete(id)
    }
  }
}

/** Hold a monitoring subscription for a session (or 'local'); returns the release function. */
export function acquireFeed(sessionId: string): () => void {
  install()
  refs.set(sessionId, (refs.get(sessionId) ?? 0) + 1)
  reconcile()
  let released = false
  return () => {
    if (released) return
    released = true
    const n = (refs.get(sessionId) ?? 1) - 1
    if (n <= 0) refs.delete(sessionId)
    else refs.set(sessionId, n)
    reconcile()
  }
}

/** React: subscribe to a session's monitoring feed while `active`, returning its live entry. */
export function useMonitorFeed(sessionId: string | undefined | null, active = true): FeedEntry | undefined {
  useEffect(() => {
    if (!sessionId || !active) return
    return acquireFeed(sessionId)
  }, [sessionId, active])
  return useMonitorStore((s) => (sessionId ? s.feeds[sessionId] : undefined))
}

/** Latest entry without subscribing (menus, commands). */
export function getFeed(sessionId: string): FeedEntry | undefined {
  return useMonitorStore.getState().feeds[sessionId]
}
