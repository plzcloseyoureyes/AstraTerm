/*
 * SFTP side panel state (outside React: the sidebar only mounts the visible panel).
 *
 *   target tab   the panel follows the active *terminal-like* tab (terminal / vnc / rdp); while another kind of tab is
 *                active (an editor opened from the panel, Home, Settings…) it keeps showing the last one, so editing a
 *                file does not blank the browser.
 *   follow       "Follow terminal folder" per session (default: settings ∧ connection option followCwd).
 *   companion    graphical sessions (GFX-16): the saved SSH connection chosen to browse the host's files.
 * ("Remote monitoring" is the monitor feature's own per-session state, see SftpPanel's footer.)
 */
import { create } from 'zustand'
import type { ConnectionOptions } from '@/api/types'
import { useWorkspaceStore } from '@/stores/workspace'
import { filesSettings } from '../settings'

const BROWSABLE_KINDS = new Set(['terminal', 'vnc', 'rdp'])

interface PanelState {
  /** Tab the panel shows (null: none). */
  tabId: string | null
  /** Per-session "Follow terminal folder" choices (absent: default). */
  follow: Record<string, boolean>
  /** Graphical tab id → companion SSH connection id. */
  companion: Record<string, string>
}

export const usePanelState = create<PanelState>(() => ({ tabId: null, follow: {}, companion: {} }))

/** Default of "Follow terminal folder" for a session: the files setting, unless the connection turns it off. */
export function followDefault(options: ConnectionOptions | undefined, followTerminal = filesSettings.get().followTerminal): boolean {
  return followTerminal && options?.followCwd !== false
}

/** Last cwd each session's panel navigated to (so a cwd change is followed once, and switching back catches up). */
export const lastFollowedCwd = new Map<string, string>()

function syncTarget(): void {
  const ws = useWorkspaceStore.getState()
  const cur = usePanelState.getState().tabId
  const active = ws.activeTabId ? ws.tabs.find((t) => t.id === ws.activeTabId) : undefined
  if (active && BROWSABLE_KINDS.has(active.kind)) {
    if (cur !== active.id) usePanelState.setState({ tabId: active.id })
    return
  }
  // The shown tab was closed: fall back to nothing (or the only remaining terminal-like tab).
  if (cur && !ws.tabs.some((t) => t.id === cur)) {
    const candidates = ws.tabs.filter((t) => BROWSABLE_KINDS.has(t.kind))
    usePanelState.setState({ tabId: candidates.length === 1 ? candidates[0].id : null })
  } else if (!cur && !active) {
    const candidates = ws.tabs.filter((t) => BROWSABLE_KINDS.has(t.kind))
    if (candidates.length === 1) usePanelState.setState({ tabId: candidates[0].id })
  }
}

let installed = false

export function installPanelTargetTracking(): void {
  if (installed) return
  installed = true
  useWorkspaceStore.subscribe((s, prev) => {
    if (s.activeTabId !== prev.activeTabId || s.tabs !== prev.tabs) syncTarget()
  })
  syncTarget()
}

export function setFollow(sessionId: string, on: boolean): void {
  if (on) lastFollowedCwd.delete(sessionId)
  usePanelState.setState((s) => ({ follow: { ...s.follow, [sessionId]: on } }))
}

export function setCompanion(tabId: string, connectionId: string | null): void {
  usePanelState.setState((s) => {
    const companion = { ...s.companion }
    if (connectionId) companion[tabId] = connectionId
    else delete companion[tabId]
    return { companion }
  })
}

/** Forget per-session choices of a closed session. */
export function forgetSession(sessionId: string): void {
  lastFollowedCwd.delete(sessionId)
  usePanelState.setState((s) => {
    if (!(sessionId in s.follow)) return s
    const follow = { ...s.follow }
    delete follow[sessionId]
    return { follow }
  })
}
