/*
 * Per-view browser state (current folder, history, filter, selection, cursor, inline rename), keyed by a view id:
 *   panel:<sessionId>        the SFTP side panel for one SSH session (restored when switching terminal tabs)
 *   tab:<tabId>:left|right   a files tab / commander pane
 * Kept outside React so the sidebar panel (unmounted while another sidebar panel is shown) resumes where it was.
 */
import { create } from 'zustand'

export interface ViewState {
  /** File system (handle registry key) the state belongs to: a view shown on another one starts over. */
  fsKey: string | null
  /** Current folder (canonical, as returned by the server), null until the first listing resolved. */
  path: string | null
  /** Folder requested but not yet confirmed by a listing (optimistic location bar). */
  pending: string | null
  /** How the pending navigation updates the history once it succeeds. */
  pendingMode: 'push' | 'replace' | 'back' | 'forward' | null
  /** Item to select once the pending folder is shown (e.g. the folder we came from after "Up"). */
  pendingSelect: string | null
  /** The pending navigation was not asked for by the user here ("Follow terminal folder"). */
  pendingQuiet: boolean
  /** Refreshes the user asked for that are still running (the Refresh button spins; polling never shows). */
  refreshing: number
  back: string[]
  forward: string[]
  filter: string
  selected: ReadonlySet<string>
  anchor: string | null
  cursor: string | null
  /** Path being renamed inline. */
  renaming: string | null
  /** Incremented to ask the list to scroll the cursor into view. */
  revealTick: number
  /** Incremented to ask the list to take keyboard focus. */
  focusTick: number
  /** Incremented to open the location bar for typing (Ctrl+L, "/" in the list); `editSeed` prefills it. */
  editTick: number
  editSeed: string | null
}

const EMPTY: ReadonlySet<string> = new Set()

function initialView(): ViewState {
  return {
    fsKey: null,
    path: null,
    pending: null,
    pendingMode: null,
    pendingSelect: null,
    pendingQuiet: false,
    refreshing: 0,
    back: [],
    forward: [],
    filter: '',
    selected: EMPTY,
    anchor: null,
    cursor: null,
    renaming: null,
    revealTick: 0,
    focusTick: 0,
    editTick: 0,
    editSeed: null,
  }
}

interface ViewStore {
  views: Record<string, ViewState>
}

const useViewStore = create<ViewStore>(() => ({ views: {} }))

const FALLBACK = initialView()

export function getView(viewId: string): ViewState {
  return useViewStore.getState().views[viewId] ?? FALLBACK
}

export function hasView(viewId: string): boolean {
  return !!useViewStore.getState().views[viewId]
}

export function useView<T>(viewId: string, select: (v: ViewState) => T): T {
  return useViewStore((s) => select(s.views[viewId] ?? FALLBACK))
}

export function patchView(viewId: string, patch: Partial<ViewState> | ((v: ViewState) => Partial<ViewState>)): void {
  useViewStore.setState((s) => {
    const prev = s.views[viewId] ?? initialView()
    const p = typeof patch === 'function' ? patch(prev) : patch
    return { views: { ...s.views, [viewId]: { ...prev, ...p } } }
  })
}

/** Start a view over on another file system (keeps nothing but the id). */
export function resetView(viewId: string, fsKey: string, pending: string): void {
  useViewStore.setState((s) => ({
    views: { ...s.views, [viewId]: { ...initialView(), fsKey, pending, pendingMode: 'replace' } },
  }))
}

/** Drop every view whose id starts with a prefix (e.g. all panes of a closed tab). */
export function dropViewsWithPrefix(prefix: string): void {
  useViewStore.setState((s) => {
    const views: Record<string, ViewState> = {}
    let changed = false
    for (const [k, v] of Object.entries(s.views)) {
      if (k.startsWith(prefix)) changed = true
      else views[k] = v
    }
    return changed ? { views } : s
  })
}

export const EMPTY_SELECTION = EMPTY
