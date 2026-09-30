/*
 * Terminal bus (SPEC §10) — how other features talk to live terminals without importing xterm:
 *
 *   sendToSession(sessionId, data)       input to one session (open terminal tab, else REST /input)
 *   getActiveTerminal() / listTerminals() TerminalHandle(s) of open terminal tabs
 *   onTerminalInput(cb) / onTerminalOutput(cb)
 *
 * Terminal tabs register a TerminalHandle here while mounted and publish their live TerminalInfo (status bar, MultiExec
 * bar, overlays). This module is light: it never imports xterm at runtime.
 */
import { useSyncExternalStore } from 'react'
import { create } from 'zustand'
import { sendSessionInput } from '@/api/sessions'
import { activeTab, useWorkspaceStore } from '@/stores/workspace'
import type { TerminalHandle, TerminalInfo, TerminalInputEvent, TerminalOutputEvent } from './types'

const handles = new Map<string, TerminalHandle>()
const handleListeners = new Set<() => void>()
let handleSnapshot: readonly TerminalHandle[] = []
let lastFocusedTab: string | null = null

interface InfoStore {
  infos: Record<string, TerminalInfo>
}

/** Live info of every mounted terminal tab, keyed by tab id. */
export const useTerminalInfoStore = create<InfoStore>(() => ({ infos: {} }))

function notifyHandles(): void {
  handleSnapshot = Object.freeze(Array.from(handles.values()))
  for (const l of Array.from(handleListeners)) l()
}

/** Called by terminal tabs (controller). Returns an unregister function. */
export function registerTerminal(handle: TerminalHandle, info: TerminalInfo): () => void {
  handles.set(handle.tabId, handle)
  useTerminalInfoStore.setState((s) => ({ infos: { ...s.infos, [handle.tabId]: info } }))
  notifyHandles()
  return () => {
    if (handles.get(handle.tabId) !== handle) return
    handles.delete(handle.tabId)
    if (lastFocusedTab === handle.tabId) lastFocusedTab = null
    useTerminalInfoStore.setState((s) => {
      const infos = { ...s.infos }
      delete infos[handle.tabId]
      return { infos }
    })
    notifyHandles()
  }
}

/** Publish a terminal's latest info (controller → bus). */
export function publishTerminalInfo(info: TerminalInfo): void {
  if (!handles.has(info.tabId)) return
  useTerminalInfoStore.setState((s) => (s.infos[info.tabId] === info ? s : { infos: { ...s.infos, [info.tabId]: info } }))
}

/** Remember the terminal that last had keyboard focus (fallback for getActiveTerminal). */
export function markTerminalFocused(tabId: string): void {
  lastFocusedTab = tabId
}

export function getTerminalByTab(tabId: string | null | undefined): TerminalHandle | undefined {
  return tabId ? handles.get(tabId) : undefined
}

/** Terminal tabs showing a session (usually one). */
export function getTerminalsBySession(sessionId: string): TerminalHandle[] {
  return handleSnapshot.filter((h) => h.sessionId === sessionId)
}

/**
 * The terminal of the active dock tab; when the active tab is not a terminal, the terminal that last had keyboard
 * focus (so "send to current terminal" from a sidebar panel still works).
 */
export function getActiveTerminal(): TerminalHandle | undefined {
  const active = activeTab()
  if (active) {
    const h = handles.get(active.id)
    if (h) return h
  }
  return lastFocusedTab ? handles.get(lastFocusedTab) : undefined
}

/** Only the terminal of the active dock tab (for keyboard commands). */
export function getFocusedTabTerminal(): TerminalHandle | undefined {
  const id = useWorkspaceStore.getState().activeTabId
  return id ? handles.get(id) : undefined
}

/** All open terminal tabs, in registration order. */
export function listTerminals(): TerminalHandle[] {
  return handleSnapshot.slice()
}

function subscribeHandles(cb: () => void): () => void {
  handleListeners.add(cb)
  return () => handleListeners.delete(cb)
}

/** React: the list of open terminals. */
export function useTerminals(): readonly TerminalHandle[] {
  return useSyncExternalStore(subscribeHandles, () => handleSnapshot, () => handleSnapshot)
}

/** React: live info of one terminal tab. */
export function useTerminalInfo(tabId: string | null | undefined): TerminalInfo | undefined {
  return useTerminalInfoStore((s) => (tabId ? s.infos[tabId] : undefined))
}

/** React: info of the active dock tab when it is a terminal. */
export function useActiveTerminalInfo(): TerminalInfo | undefined {
  const activeId = useWorkspaceStore((s) => s.activeTabId)
  return useTerminalInfoStore((s) => (activeId ? s.infos[activeId] : undefined))
}

// ---------------------------------------------------------------------------------------------------------------------
// Input / output observers
// ---------------------------------------------------------------------------------------------------------------------

const inputListeners = new Set<(e: TerminalInputEvent) => void>()
const outputListeners = new Set<(e: TerminalOutputEvent) => void>()

/** Observe user input sent to sessions (typed, pasted, MultiExec copies). Returns unsubscribe. */
export function onTerminalInput(cb: (e: TerminalInputEvent) => void): () => void {
  inputListeners.add(cb)
  return () => inputListeners.delete(cb)
}

/** Observe output received by open terminals (before rendering). Returns unsubscribe. */
export function onTerminalOutput(cb: (e: TerminalOutputEvent) => void): () => void {
  outputListeners.add(cb)
  return () => outputListeners.delete(cb)
}

export function hasTerminalOutputListeners(): boolean {
  return outputListeners.size > 0
}

export function emitTerminalInput(e: TerminalInputEvent): void {
  for (const l of Array.from(inputListeners)) {
    try {
      l(e)
    } catch (err) {
      console.error('[terminal-bus] input listener failed', err)
    }
  }
}

export function emitTerminalOutput(e: TerminalOutputEvent): void {
  for (const l of Array.from(outputListeners)) {
    try {
      l(e)
    } catch (err) {
      console.error('[terminal-bus] output listener failed', err)
    }
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// Sending
// ---------------------------------------------------------------------------------------------------------------------

const decoder = new TextDecoder()

/**
 * Send input to a session. Uses the terminal WebSocket of an open tab when there is one (ordered with typed input),
 * otherwise POST /api/sessions/{id}/input. Resolves true when the data was handed to the server.
 */
export async function sendToSession(sessionId: string, data: string | Uint8Array): Promise<boolean> {
  if (!sessionId || (typeof data === 'string' ? !data : !data.length)) return false
  const views = getTerminalsBySession(sessionId)
  // A view holding the input lock (in-band transfer) refuses the data: never route around it through REST.
  const locked = views.find((h) => h.inputLocked())
  if (locked) {
    locked.send(data) // refused: shows the "input held back" hint
    return false
  }
  for (const h of views) {
    if (h.send(data)) return true
  }
  try {
    await sendSessionInput(sessionId, typeof data === 'string' ? data : decoder.decode(data))
    return true
  } catch (err) {
    console.warn('[terminal-bus] sendToSession failed', err)
    return false
  }
}
