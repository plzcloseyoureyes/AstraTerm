/*
 * Live state of the RDP tabs (toolbar, overlays, status bar) and the registry of their controllers, so commands,
 * menus and the toolbar can act on a tab without owning its React tree.
 */
import { create } from 'zustand'
import type { RdpController, RdpScaling, ViewerState } from './types'

interface RdpStore {
  tabs: Record<string, ViewerState>
  /** The recordings dialog is open. */
  recordingsOpen: boolean
}

export const useRdpStore = create<RdpStore>(() => ({ tabs: {}, recordingsOpen: false }))

export const openRecordings = () => useRdpStore.setState({ recordingsOpen: true })
export const closeRecordings = () => useRdpStore.setState({ recordingsOpen: false })

export function initialViewerState(scaling: RdpScaling): ViewerState {
  return {
    status: 'idle',
    scaling,
    clipboardEnabled: true,
    fullscreen: false,
    keyboardLocked: false,
    focused: false,
    transfers: [],
    driveEnabled: false,
    canResize: false,
  }
}

export function patchViewer(tabId: string, patch: Partial<ViewerState> | ((s: ViewerState) => Partial<ViewerState>)): void {
  useRdpStore.setState((st) => {
    const cur = st.tabs[tabId]
    if (!cur) return st
    const p = typeof patch === 'function' ? patch(cur) : patch
    return { tabs: { ...st.tabs, [tabId]: { ...cur, ...p } } }
  })
}

export function setViewer(tabId: string, state: ViewerState): void {
  useRdpStore.setState((st) => ({ tabs: { ...st.tabs, [tabId]: state } }))
}

export function dropViewer(tabId: string): void {
  useRdpStore.setState((st) => {
    if (!(tabId in st.tabs)) return st
    const tabs = { ...st.tabs }
    delete tabs[tabId]
    return { tabs }
  })
}

export function getViewer(tabId: string): ViewerState | undefined {
  return useRdpStore.getState().tabs[tabId]
}

export function useViewer(tabId: string | undefined): ViewerState | undefined {
  return useRdpStore((s) => (tabId ? s.tabs[tabId] : undefined))
}

// ---- controllers --------------------------------------------------------------------------------------------------

const controllers = new Map<string, RdpController>()

export function registerController(c: RdpController): () => void {
  controllers.set(c.tabId, c)
  return () => {
    if (controllers.get(c.tabId) === c) controllers.delete(c.tabId)
  }
}

export function getController(tabId: string | undefined): RdpController | undefined {
  return tabId ? controllers.get(tabId) : undefined
}
