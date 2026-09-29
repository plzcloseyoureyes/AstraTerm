/*
 * Workspace = the tabs of the title bar, each with its own split layout (like a Tabby tab). This module wraps the
 * dockview instances so features never touch dockview directly:
 *
 *   openTab({kind, params, title?, position?: 'tab'|'right'|'below'|'left'|'above'|'float'|'window', id?})  → tabId
 *   closeTab(tabId) · focusTab · renameTab · setTabTitle · updateTabParams · setTabState(tabId, {status, progress, activity})
 *   activeTab() / useActiveTab() · listTabs() / useTabs() · useTabState(tabId)
 *   splitActive('right'|'below') · arrangeLayout('single'|'columns'|'rows'|'grid') · popout · float · dockTab · maximize
 *   reopenClosed() (CC-7) · layout persistence (localStorage, per user)
 *
 * Terms: a *tab* (tabId) is one piece of content — a terminal, a file browser, Settings — shown in a *pane*. A *space*
 * is a top-level tab of the title bar: its own dockview instance holding one or more panes, split any way. Opening a
 * tab creates a new space; splitting adds a pane to the current one; closing a space's last pane closes the space.
 * Every pane holds exactly one tab.
 *
 * Every dock panel uses one generic host component; the real component comes from the tab-kind registry, so tabs
 * restored before their feature registered render a placeholder until it does.
 */
import { useCallback, useSyncExternalStore } from 'react'
import { create } from 'zustand'
import { toast } from 'sonner'
import type { AddPanelOptions, DockviewApi, DockviewGroupPanel, IDockviewPanel, SerializedDockview } from 'dockview-react'
import { tabKinds, type ClosedTab, type TabInfo, type TabPlacement, type TabPosition } from '@/app/registry'
import { trackDocument } from '@/lib/theme'
import { debounce, storage, uid } from '@/lib/utils'
import { workspacesSettings, type SavedWorkspace } from './settings'

export const HOST_COMPONENT = 'nx-host'
export const TAB_COMPONENT = 'nx-tab'
export const POPOUT_URL = '/popout.html'

/** Parameters stored on each dock panel (serialised with the layout). */
export interface PanelParams<P = unknown> {
  kind: string
  params: P
  /** The user renamed the tab: automatic titles (OSC title etc.) no longer apply. */
  userTitle?: boolean
}

export type TabStatus = 'connecting' | 'connected' | 'disconnected' | 'error'

export interface TabState {
  status?: TabStatus
  /** 0..1, 'indeterminate', or null/undefined for none. */
  progress?: number | 'indeterminate' | null
  /** Unseen output in a background tab. Cleared automatically when the tab becomes active. */
  activity?: boolean
}

export interface OpenTabOptions<P = any> {
  kind: string
  params?: P
  title?: string
  position?: TabPosition
  /** Explicit panel id (re-opening an existing id focuses it). Singleton kinds use the kind as id. */
  id?: string
  /** Focus the new tab (default true). */
  activate?: boolean
  /** Tab id to position relative to (default: the active tab). */
  reference?: string
  /** Put the tab back where a closed tab was (ClosedTab.placement): next to its old neighbours' space. Ignored when
   *  `position` is not 'tab'. */
  placement?: TabPlacement
}

/** One top-level tab as the title bar shows it. */
export interface SpaceInfo {
  id: string
  /** Tab ids per pane (grid and floating groups, in order; pop-out windows excluded). */
  panes: string[][]
  /** The space's focused tab. */
  activeTabId: string | null
}

interface WorkspaceStore {
  ready: boolean
  /** Every tab, space by space. */
  tabs: TabInfo[]
  /** The focused tab: the active space's active pane. */
  activeTabId: string | null
  tabStates: Record<string, TabState>
  closed: ClosedTab[]
  /** The active space has a maximized pane. */
  maximized: boolean
  /** The spaces in title-bar order (a new space appears here before its dockview is mounted). */
  spaces: SpaceInfo[]
  activeSpaceId: string | null
}

export const useWorkspaceStore = create<WorkspaceStore>(() => ({
  ready: false,
  tabs: [],
  activeTabId: null,
  tabStates: {},
  closed: [],
  maximized: false,
  spaces: [],
  activeSpaceId: null,
}))

const MAX_CLOSED = 25

type PanelOpts = AddPanelOptions<PanelParams>
/** The part of AddPanelOptions every tab has (placement is added per call). */
type BasePanel = Pick<PanelOpts, 'id' | 'component' | 'tabComponent' | 'title' | 'params' | 'inactive'>

/** A mounted space: its dockview instance. */
interface Space {
  api: DockviewApi
  dispose: () => void
}

/** What a space shows once its dockview is ready: panels to add, or a saved layout. */
interface SpaceSeed {
  panels?: PanelOpts[]
  layout?: SerializedDockview
  /** Pop the first panel out into a window once it exists (openTab position 'window'). */
  popout?: boolean
}

const mounted = new Map<string, Space>()
const seeds = new Map<string, SpaceSeed>()
let userKey = 'anonymous'
let initialized = false
const pendingOpens: OpenTabOptions[] = []
const windowListeners = new Set<(win: Window) => void>()
const popoutDisposers = new Map<string, () => void>()
/** Grid panes hide their own tab header: the title bar shows the spaces (see setTitleBarTabs). */
let titleBarTabs = false

const layoutKey = () => `astraterm:layout:v2:${userKey}`
const legacyLayoutKey = () => `astraterm:layout:v1:${userKey}`
const closedKey = () => `astraterm:closed-tabs:v1:${userKey}`

// ---------------------------------------------------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------------------------------------------------

function wrapperOf(panel: IDockviewPanel): PanelParams {
  const p = (panel.params ?? {}) as Partial<PanelParams>
  return { kind: typeof p.kind === 'string' ? p.kind : 'unknown', params: p.params ?? {}, userTitle: !!p.userTitle }
}

function infoOf(panel: IDockviewPanel): TabInfo {
  const w = wrapperOf(panel)
  return { id: panel.id, kind: w.kind, params: w.params, title: panel.title ?? panel.api.title ?? w.kind }
}

function defaultTitle(kind: string, params: unknown): string {
  const def = tabKinds.get(kind)
  if (!def) return kind
  try {
    return def.title(params) || kind
  } catch {
    return kind
  }
}

function spaceOrder(): string[] {
  return useWorkspaceStore.getState().spaces.map((s) => s.id)
}

function activeSpace(): Space | undefined {
  const id = useWorkspaceStore.getState().activeSpaceId
  return id ? mounted.get(id) : undefined
}

/** The dockview panel of a tab, in whichever space it lives. */
export function getPanel(tabId: string | undefined): IDockviewPanel | undefined {
  if (!tabId) return undefined
  for (const s of mounted.values()) {
    const p = s.api.getPanel(tabId)
    if (p) return p
  }
  return undefined
}

/** The id of the space holding a tab. */
export function spaceOf(tabId: string): string | undefined {
  for (const [id, s] of mounted) if (s.api.getPanel(tabId)) return id
  return undefined
}

/** The active space's dockview (sizes for new terminals, pane geometry). */
export function activeSpaceApi(): DockviewApi | undefined {
  return activeSpace()?.api
}

function panelOrActive(tabId?: string): IDockviewPanel | undefined {
  return tabId ? getPanel(tabId) : activeSpace()?.api.activePanel
}

function panelOptions<P>(opts: OpenTabOptions<P>, id: string): BasePanel {
  const params = (opts.params ?? {}) as P
  return {
    id,
    component: HOST_COMPONENT,
    tabComponent: TAB_COMPONENT,
    title: opts.title || defaultTitle(opts.kind, params),
    params: { kind: opts.kind, params },
    inactive: opts.activate === false,
  }
}

/**
 * Rebuild the store from the spaces. Unchanged TabInfo objects keep their identity (and the store is not touched when
 * nothing changed), so frequent layout events (sash drags) do not re-render subscribers.
 */
function sync(): void {
  const prev = useWorkspaceStore.getState()
  const prevById = new Map(prev.tabs.map((t) => [t.id, t]))
  const tabs: TabInfo[] = []
  let changed = false
  const spaces: SpaceInfo[] = []
  for (const old of prev.spaces) {
    const s = mounted.get(old.id)
    if (!s) {
      spaces.push(old) // not mounted yet
      continue
    }
    const panes: string[][] = []
    for (const group of s.api.groups) {
      const where = group.api.location.type
      const hidden = titleBarTabs && where === 'grid'
      if (group.header.hidden !== hidden) group.header.hidden = hidden
      for (const panel of group.panels) {
        const next = infoOf(panel)
        const o = prevById.get(next.id)
        const reuse = o && o.kind === next.kind && o.title === next.title && o.params === next.params
        if (!reuse) changed = true
        tabs.push(reuse ? o : next)
      }
      if (where !== 'popout' && group.panels.length) panes.push(group.panels.map((p) => p.id))
    }
    const activeTabId = s.api.activePanel?.id ?? null
    spaces.push(samePanes(old.panes, panes) && old.activeTabId === activeTabId ? old : { id: old.id, panes, activeTabId })
  }
  if (tabs.length !== prev.tabs.length || tabs.some((t, i) => t !== prev.tabs[i])) changed = true
  const spacesChanged = spaces.length !== prev.spaces.length || spaces.some((s, i) => s !== prev.spaces[i])
  const active = activeSpace()
  const activeTabId = active?.api.activePanel?.id ?? null
  const maximized = active?.api.hasMaximizedGroup() ?? false
  const live = new Set(tabs.map((t) => t.id))
  let tabStates = prev.tabStates
  for (const id of Object.keys(tabStates)) {
    if (!live.has(id)) {
      if (tabStates === prev.tabStates) tabStates = { ...tabStates }
      delete tabStates[id]
    }
  }
  if (!changed && !spacesChanged && activeTabId === prev.activeTabId && maximized === prev.maximized && tabStates === prev.tabStates) {
    return
  }
  useWorkspaceStore.setState({ tabs: changed ? tabs : prev.tabs, spaces: spacesChanged ? spaces : prev.spaces, activeTabId, tabStates, maximized })
}

function samePanes(a: string[][], b: string[][]): boolean {
  return a.length === b.length && a.every((ids, i) => ids.length === b[i].length && ids.every((id, j) => id === b[i][j]))
}

/** Show pane tabs in the title bar (true) or in each pane's own header (false, e.g. on phones). */
export function setTitleBarTabs(on: boolean): void {
  if (titleBarTabs === on) return
  titleBarTabs = on
  sync()
  scheduleOverlayFix()
}

/**
 * Work around a dockview 8 quirk: with the `always` renderer, a panel that becomes visible again in a group resized
 * in the same frame (split, move, close of the active tab) can keep stale overlay geometry. A forced relayout two
 * frames later re-measures every overlay. Only scheduled for structural changes (not sash drags).
 */
let overlayFixFrame = 0
function scheduleOverlayFix(): void {
  if (overlayFixFrame) cancelAnimationFrame(overlayFixFrame)
  overlayFixFrame = requestAnimationFrame(() => {
    overlayFixFrame = requestAnimationFrame(() => {
      overlayFixFrame = 0
      for (const s of mounted.values()) {
        const dv = s.api
        if (dv.width <= 0 || dv.height <= 0) continue
        try {
          dv.layout(dv.width, dv.height, true)
        } catch {
          /* disposed */
        }
      }
    })
  })
}

// ---------------------------------------------------------------------------------------------------------------------
// spaces
// ---------------------------------------------------------------------------------------------------------------------

/** Add a space to the title bar (after `after`, else at the end); its dockview mounts and then shows `seed`. */
function createSpace(seed: SpaceSeed, opts: { after?: string; activate?: boolean; id?: string } = {}): string {
  const id = opts.id ?? uid('space')
  seeds.set(id, seed)
  useWorkspaceStore.setState((s) => {
    const spaces = [...s.spaces]
    const at = opts.after ? spaces.findIndex((x) => x.id === opts.after) : -1
    const info: SpaceInfo = { id, panes: seed.panels ? [seed.panels.map((p) => p.id)] : [], activeTabId: seed.panels?.[0]?.id ?? null }
    spaces.splice(at >= 0 ? at + 1 : spaces.length, 0, info)
    return { spaces, activeSpaceId: opts.activate === false && s.activeSpaceId ? s.activeSpaceId : id }
  })
  saveLayout()
  return id
}

/** Remove a space whose panes are all gone, focusing its right (else left) neighbour when it was active. */
function removeSpace(id: string): void {
  seeds.delete(id)
  useWorkspaceStore.setState((s) => {
    const i = s.spaces.findIndex((x) => x.id === id)
    if (i < 0) return {}
    const spaces = s.spaces.filter((x) => x.id !== id)
    const activeSpaceId = s.activeSpaceId === id ? (spaces[i]?.id ?? spaces[i - 1]?.id ?? null) : s.activeSpaceId
    return { spaces, activeSpaceId }
  })
  sync()
  saveLayout()
}

/** Remove mounted spaces left without panels (after a close or a move). Deferred: never inside a dockview event. */
let emptyCheck = 0
function scheduleEmptyCheck(): void {
  if (emptyCheck) return
  emptyCheck = window.setTimeout(() => {
    emptyCheck = 0
    for (const [id, s] of mounted) if (s.api.panels.length === 0 && !seeds.has(id)) removeSpace(id)
  })
}

/** Called by the space's dockview when it is ready (layout/workspace/Workspace.tsx). Returns the disposer. */
export function attachSpace(id: string, dv: DockviewApi): () => void {
  const structural = () => {
    sync()
    scheduleOverlayFix()
    scheduleEmptyCheck()
    saveLayout()
  }
  const disposables = [
    dv.onDidAddPanel(structural),
    dv.onDidRemovePanel(structural),
    dv.onDidMovePanel(structural),
    dv.onDidAddGroup(structural),
    dv.onDidRemoveGroup(structural),
    dv.onDidActivePanelChange((e) => {
      const tabId = e.panel?.id
      if (tabId && useWorkspaceStore.getState().activeSpaceId === id) clearActivity(tabId)
      structural()
    }),
    dv.onDidLayoutChange(() => {
      sync()
      saveLayout()
    }),
    dv.onDidMaximizedGroupChange(structural),
    dv.onDidLayoutFromJSON(structural),
    dv.onDidAddPopoutGroup((pg) => {
      const untrack = trackDocument(pg.window.document)
      popoutDisposers.set(pg.id, untrack)
      for (const l of Array.from(windowListeners)) l(pg.window)
    }),
    dv.onDidRemovePopoutGroup((pg) => {
      popoutDisposers.get(pg.id)?.()
      popoutDisposers.delete(pg.id)
    }),
    // A title-bar tab dragged over this space: dockview's drop zones show, and a drop merges its panes here.
    dv.onUnhandledDragOver((e) => {
      if (draggingSpace && draggingSpace !== id) e.accept()
    }),
    dv.onDidDrop((e) => {
      const from = draggingSpace
      if (!from || from === id) return
      draggingSpace = null
      mergeSpace(from, id, e.group, e.position)
    }),
  ]
  const space: Space = {
    api: dv,
    dispose: () => {
      for (const d of disposables) d.dispose()
    },
  }
  mounted.set(id, space)

  // The seed is kept until this mount survives a tick: a mount that is immediately undone (React StrictMode mounts
  // twice in development) must leave it for the next one.
  const seed = seeds.get(id)
  setTimeout(() => {
    if (mounted.get(id) === space) seeds.delete(id)
  })
  try {
    if (seed?.layout) dv.fromJSON(sanitizeLayout(seed.layout))
    for (const p of seed?.panels ?? []) dv.addPanel(p)
    if (seed?.popout && dv.panels[0]) void popout(dv.panels[0].id)
  } catch (err) {
    console.warn('[workspace] a tab could not be restored', err)
  }
  structural()
  return () => {
    space.dispose()
    if (mounted.get(id) === space) mounted.delete(id)
  }
}

/** Focus a space (title-bar click). */
export function focusSpace(id: string): void {
  if (!useWorkspaceStore.getState().spaces.some((s) => s.id === id)) return
  useWorkspaceStore.setState({ activeSpaceId: id })
  const s = mounted.get(id)
  const panel = s?.api.activePanel
  if (panel) {
    clearActivity(panel.id)
    panel.api.setActive()
  }
  sync()
  saveLayout()
}

/** Close a whole space: every pane (each may veto). */
export async function closeSpace(id: string): Promise<void> {
  const s = mounted.get(id)
  if (!s) return
  await closeTabs(s.api.panels.map((p) => p.id))
}

/** Move a space to another position of the title bar. */
export function moveSpace(id: string, toIndex: number): void {
  useWorkspaceStore.setState((s) => {
    const from = s.spaces.findIndex((x) => x.id === id)
    if (from < 0) return {}
    const spaces = [...s.spaces]
    const [moved] = spaces.splice(from, 1)
    spaces.splice(Math.max(0, Math.min(toIndex, spaces.length)), 0, moved)
    return { spaces }
  })
  saveLayout()
}

type Direction = 'left' | 'right' | 'above' | 'below'

/** Where a tab goes in another space: beside `group` (default: its focused pane), or at the layout's edge. */
interface MoveTarget {
  spaceId: string
  group?: DockviewGroupPanel
  direction?: Direction
  /** Beside the whole layout instead of a pane (a drop on the layout's outer edge). */
  edge?: boolean
}

/**
 * Move a tab into another pane arrangement without closing it: into a new space (`to` omitted), or beside a pane of
 * another space. The tab is re-created there (its session re-attaches), never closed.
 */
export function moveTab(tabId: string, to?: MoveTarget): void {
  const panel = getPanel(tabId)
  const from = spaceOf(tabId)
  if (!panel || !from) return
  const target = to ? mounted.get(to.spaceId) : undefined
  if (to && (!target || to.spaceId === from)) return
  const opts: BasePanel = {
    id: panel.id,
    component: HOST_COMPONENT,
    tabComponent: TAB_COMPONENT,
    title: panel.title ?? '',
    params: panel.params as PanelParams,
  }
  panel.api.close() // not closeTab: the tab lives on in its new place
  if (target && to) {
    const direction = to.direction ?? 'right'
    const group = to.edge ? undefined : (to.group ?? target.api.activePanel?.group)
    const position = group ? { referenceGroup: group, direction } : target.api.panels.length ? { direction } : undefined
    target.api.addPanel({ ...opts, position } as PanelOpts)
    focusSpace(to.spaceId)
  } else {
    createSpace({ panels: [opts as PanelOpts] }, { after: from })
  }
  scheduleEmptyCheck()
}

/** The space being dragged from the title bar (its panes can be dropped into another space's layout). */
let draggingSpace: string | null = null

export function beginSpaceDrag(id: string): void {
  draggingSpace = id
}

export function endSpaceDrag(): void {
  draggingSpace = null
}

const DROP_DIRECTION: Record<string, Direction> = { left: 'left', right: 'right', top: 'above', bottom: 'below', center: 'right' }

/** Move every pane of space `from` into space `to`, beside `group` on side `position` (dockview drop position). */
function mergeSpace(from: string, to: string, group: DockviewGroupPanel | undefined, position: string): void {
  const src = mounted.get(from)
  if (!src || from === to) return
  const direction = DROP_DIRECTION[position] ?? 'right'
  let anchor = group
  const panels = src.api.panels // a snapshot: panes leave the source as they move
  for (const panel of panels) {
    const id = panel.id
    moveTab(id, { spaceId: to, group: anchor, direction, edge: !anchor })
    // The next panes line up after the one just placed (keeps them together).
    anchor = getPanel(id)?.group
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// lifecycle (called by layout/Workspace.tsx)
// ---------------------------------------------------------------------------------------------------------------------

interface SavedLayout {
  spaces: { id: string; layout?: SerializedDockview; panels?: PanelOpts[] }[]
  active?: string
}

const saveLayout = debounce(() => {
  if (!initialized) return
  try {
    storage.set(layoutKey(), serialize())
  } catch (err) {
    console.warn('[workspace] could not serialise layout', err)
  }
}, 400)

function serialize(): SavedLayout {
  const { spaces, activeSpaceId } = useWorkspaceStore.getState()
  return {
    active: activeSpaceId ?? undefined,
    spaces: spaces.map((s) => {
      const m = mounted.get(s.id)
      if (m) return { id: s.id, layout: m.api.toJSON() }
      const seed = seeds.get(s.id)
      return { id: s.id, layout: seed?.layout, panels: seed?.panels }
    }),
  }
}

/** The panels of a dockview layout saved before spaces existed: each becomes a space of its own. */
function legacyPanels(data: SerializedDockview | null | undefined): PanelOpts[] {
  const panels = (data as { panels?: Record<string, { id: string; params?: PanelParams; title?: string }> } | null)?.panels
  if (!panels) return []
  return Object.values(panels)
    .filter((p) => p && typeof p.id === 'string' && p.params && typeof p.params.kind === 'string')
    .map((p) => ({ id: p.id, component: HOST_COMPONENT, tabComponent: TAB_COMPONENT, title: p.title ?? p.params!.kind, params: p.params! }))
}

/** Replace the spaces by a saved layout (restore on start, saved workspaces). */
function loadSpaces(saved: SavedLayout | null, legacy: SerializedDockview | null): boolean {
  const list: SpaceInfo[] = []
  if (saved?.spaces?.length) {
    for (const s of saved.spaces) {
      if (!s?.id || (!s.layout && !s.panels?.length)) continue
      seeds.set(s.id, { layout: s.layout, panels: s.panels })
      list.push({ id: s.id, panes: [], activeTabId: null })
    }
  } else if (legacy) {
    for (const p of legacyPanels(legacy)) {
      const id = uid('space')
      seeds.set(id, { panels: [p] })
      list.push({ id, panes: [[p.id]], activeTabId: p.id })
    }
  }
  if (!list.length) return false
  const active = list.find((s) => s.id === saved?.active)?.id ?? list[0].id
  useWorkspaceStore.setState({ spaces: list, activeSpaceId: active })
  return true
}

/**
 * Start the workspace for a user: restore the saved spaces (when `restore`), open Home on the first run, and flush
 * openTab calls made before the workspace was ready. Returns a disposer.
 */
export function initWorkspace(opts: { userId: string; restore: boolean }): () => void {
  userKey = opts.userId || 'anonymous'
  useWorkspaceStore.setState({ closed: storage.get<ClosedTab[]>(closedKey(), []).slice(0, MAX_CLOSED) })
  const saved = storage.get<SavedLayout | null>(layoutKey(), null)
  const legacy = storage.get<SerializedDockview | null>(legacyLayoutKey(), null)
  const hadLayout = !!saved || !!legacy
  if (opts.restore) loadSpaces(saved, legacy)
  if (legacy) storage.remove(legacyLayoutKey())
  initialized = true
  useWorkspaceStore.setState({ ready: true })
  if (!hadLayout) openTab({ kind: 'home' }) // first run for this user on this browser
  for (const o of pendingOpens.splice(0)) openTab(o)

  // A reload or close right after a change (within the save debounce) must not lose it.
  const flushLayout = () => saveLayout.flush()
  window.addEventListener('pagehide', flushLayout)
  return () => {
    window.removeEventListener('pagehide', flushLayout)
    if (overlayFixFrame) cancelAnimationFrame(overlayFixFrame)
    overlayFixFrame = 0
    saveLayout.flush()
    initialized = false
    for (const un of popoutDisposers.values()) un()
    popoutDisposers.clear()
    seeds.clear()
    useWorkspaceStore.setState({ ready: false, tabs: [], activeTabId: null, tabStates: {}, maximized: false, spaces: [], activeSpaceId: null })
  }
}

/**
 * Pop-out windows cannot be re-opened on load (popup blockers), so saved pop-outs are restored as floating groups.
 */
function sanitizeLayout(data: SerializedDockview): SerializedDockview {
  const popouts = data.popoutGroups ?? []
  if (!popouts.length) return data
  const floating = [...(data.floatingGroups ?? [])]
  popouts.forEach((p, i) => {
    const width = Math.max(360, Math.min(p.position?.width ?? 720, window.innerWidth - 80))
    const height = Math.max(240, Math.min(p.position?.height ?? 480, window.innerHeight - 120))
    floating.push({
      data: p.data,
      grid: p.grid,
      position: { top: 60 + i * 28, left: 80 + i * 28, width, height },
    })
  })
  return { ...data, popoutGroups: [], floatingGroups: floating }
}

/** Observe pop-out windows as they open (the shell uses it to install keybindings there). */
export function onWorkspaceWindow(cb: (win: Window) => void): () => void {
  windowListeners.add(cb)
  return () => windowListeners.delete(cb)
}

/** Forget the saved layout for the current user (Settings → General → Reset layout). */
export function resetSavedLayout(): void {
  storage.remove(layoutKey())
  storage.remove(legacyLayoutKey())
}

/** Windows currently hosting popped-out tabs. */
export function getPopoutWindows(): Window[] {
  const out: Window[] = []
  for (const s of mounted.values()) {
    for (const p of s.api.getPopouts()) {
      try {
        if (p.window && !p.window.closed) out.push(p.window)
      } catch {
        /* window gone */
      }
    }
  }
  return out
}

// ---------------------------------------------------------------------------------------------------------------------
// tabs
// ---------------------------------------------------------------------------------------------------------------------

const DIRECTION: Record<string, 'left' | 'right' | 'above' | 'below'> = {
  right: 'right',
  below: 'below',
  left: 'left',
  above: 'above',
}

/**
 * Open (or focus) a tab. Returns its id. 'tab' (the default) opens it in a new space of the title bar; 'right',
 * 'below', 'left' and 'above' split the reference tab's space (default: the active one). Safe to call before the
 * workspace is mounted (queued).
 */
export function openTab<P = any>(opts: OpenTabOptions<P>): string {
  const def = tabKinds.get(opts.kind)
  const id = opts.id ?? (def?.singleton ? opts.kind : uid(opts.kind))
  if (!initialized) {
    pendingOpens.push({ ...opts, id })
    return id
  }
  const existing = getPanel(id)
  if (existing) {
    if (opts.params !== undefined) {
      existing.update({ params: { params: opts.params } })
      if (opts.title) existing.api.setTitle(opts.title)
      sync()
    }
    if (opts.activate !== false) focusTab(id)
    return id
  }

  const base = panelOptions(opts, id)
  const position = opts.position ?? 'tab'
  const ref = opts.reference ? getPanel(opts.reference) : activeSpace()?.api.activePanel
  const refSpace = ref ? mounted.get(spaceOf(ref.id)!) : activeSpace()
  const activate = opts.activate !== false

  try {
    if (DIRECTION[position] && refSpace) {
      refSpace.api.addPanel({ ...base, position: ref ? { referencePanel: ref, direction: DIRECTION[position] } : { direction: DIRECTION[position] } } as PanelOpts)
    } else if (position === 'float' && refSpace) {
      const dv = refSpace.api
      const w = Math.min(820, Math.max(360, dv.width - 120))
      const h = Math.min(520, Math.max(240, dv.height - 120))
      dv.addPanel({ ...base, floating: { width: w, height: h, x: Math.max(20, (dv.width - w) / 2), y: Math.max(20, (dv.height - h) / 3) } } as PanelOpts)
    } else if (position === 'window' && refSpace) {
      refSpace.api.addPanel({ ...base, position: ref ? { referencePanel: ref, direction: 'right' } : undefined } as PanelOpts)
      void popout(id)
    } else {
      // A new space: after the space of a reopened tab's old neighbour, else after the active one.
      const near = opts.placement ? (livePanel(opts.placement.prevId) ?? livePanel(opts.placement.nextId)) : undefined
      const after = (near && spaceOf(near.id)) ?? (ref ? spaceOf(ref.id) : undefined) ?? useWorkspaceStore.getState().activeSpaceId ?? undefined
      createSpace({ panels: [base as PanelOpts], popout: position === 'window' }, { after, activate })
    }
  } catch (err) {
    console.error('[workspace] openTab failed', err)
    toast.error('Could not open tab', { description: err instanceof Error ? err.message : String(err) })
    return id
  }
  if (opts.placement && opts.placement.tabId !== id) {
    reopenedIds.set(opts.placement.tabId, id)
    if (reopenedIds.size > MAX_CLOSED * 4) reopenedIds.delete(reopenedIds.keys().next().value!)
  }
  if (activate && refSpace && position !== 'tab') focusTab(id)
  sync()
  return id
}

/** Old id → new id of reopened tabs, so a later reopen can still find a neighbour that came back under a new id. */
const reopenedIds = new Map<string, string>()

function livePanel(id: string | undefined): IDockviewPanel | undefined {
  for (let i = 0, cur: string | undefined = id; cur && i < 8; i++, cur = reopenedIds.get(cur)) {
    const p = getPanel(cur)
    if (p) return p
  }
  return undefined
}

/** Where a tab was when it closed: its neighbours in the title bar (the tabs of the spaces before and after). */
function placementOf(panel: IDockviewPanel): TabPlacement {
  const order = spaceOrder()
  const spaceId = spaceOf(panel.id) ?? ''
  const i = order.indexOf(spaceId)
  const firstOf = (sid: string | undefined) => (sid ? mounted.get(sid)?.api.panels[0]?.id : undefined)
  return { tabId: panel.id, groupId: spaceId, index: Math.max(0, i), prevId: firstOf(order[i - 1]), nextId: firstOf(order[i + 1]) }
}

/** Close a tab (asks the tab kind's canClose unless forced). Resolves true when closed. */
export async function closeTab(tabId: string, opts: { force?: boolean } = {}): Promise<boolean> {
  const panel = getPanel(tabId)
  if (!panel) return false
  const info = infoOf(panel)
  const def = tabKinds.get(info.kind)
  if (!opts.force && def?.canClose) {
    try {
      if (!(await def.canClose(info))) return false
    } catch (err) {
      console.error('[workspace] canClose failed', err)
    }
  }
  // The panel may have been removed while we waited.
  const still = getPanel(tabId)
  if (!still) return false
  if (!def?.noReopen) pushClosed(info, placementOf(still))
  still.api.close()
  try {
    def?.onClose?.(info)
  } catch (err) {
    console.error('[workspace] onClose failed', err)
  }
  sync()
  scheduleEmptyCheck()
  return true
}

/** Close several tabs sequentially (each may veto). */
export async function closeTabs(ids: string[], opts: { force?: boolean } = {}): Promise<void> {
  for (const id of ids) await closeTab(id, opts)
}

function tabsOfSpaces(ids: string[]): string[] {
  return ids.flatMap((sid) => mounted.get(sid)?.api.panels.map((p) => p.id) ?? [])
}

/** Close every space except the one holding `tabId`. */
export function closeOtherTabs(tabId: string): Promise<void> {
  const own = spaceOf(tabId)
  return closeTabs(tabsOfSpaces(spaceOrder().filter((id) => id !== own)))
}

/** Close the spaces to the right of the one holding `tabId`. */
export function closeTabsToRight(tabId: string): Promise<void> {
  const order = spaceOrder()
  const i = order.indexOf(spaceOf(tabId) ?? '')
  return i < 0 ? Promise.resolve() : closeTabs(tabsOfSpaces(order.slice(i + 1)))
}

export function focusTab(tabId: string): boolean {
  const panel = getPanel(tabId)
  const sid = spaceOf(tabId)
  if (!panel || !sid) return false
  if (useWorkspaceStore.getState().activeSpaceId !== sid) useWorkspaceStore.setState({ activeSpaceId: sid })
  panel.api.setActive()
  clearActivity(tabId)
  if (panel.api.location.type === 'popout') {
    try {
      panel.api.getWindow().focus()
    } catch {
      /* window gone */
    }
  }
  sync()
  saveLayout()
  return true
}

/** User rename: the title sticks (automatic titles are ignored from now on). Empty title restores automatic titles. */
export function renameTab(tabId: string, title: string): void {
  const panel = getPanel(tabId)
  if (!panel) return
  const t = title.trim()
  if (!t) {
    const w = wrapperOf(panel)
    panel.update({ params: { userTitle: false } })
    panel.api.setTitle(defaultTitle(w.kind, w.params))
  } else {
    panel.update({ params: { userTitle: true } })
    panel.api.setTitle(t)
  }
  sync()
  saveLayout()
}

/** Programmatic title (e.g. OSC 0/2 window title). Ignored when the user renamed the tab unless `force`. */
export function setTabTitle(tabId: string, title: string, opts: { force?: boolean } = {}): void {
  const panel = getPanel(tabId)
  if (!panel || !title) return
  if (wrapperOf(panel).userTitle && !opts.force) return
  if (panel.title === title) return
  panel.api.setTitle(title)
  sync()
}

/** Merge into a tab's params (persisted with the layout), e.g. the terminal stores its sessionId. */
export function updateTabParams<P extends object>(tabId: string, patch: Partial<P>): void {
  const panel = getPanel(tabId)
  if (!panel) return
  const w = wrapperOf(panel)
  const prev = (w.params && typeof w.params === 'object' ? w.params : {}) as P
  panel.update({ params: { params: { ...prev, ...patch } } })
  sync()
  saveLayout()
}

/** Get a tab's params (undefined if the tab does not exist). */
export function getTabParams<P = any>(tabId: string): P | undefined {
  const panel = getPanel(tabId)
  return panel ? (wrapperOf(panel).params as P) : undefined
}

/** Tab status indicators rendered by the tab strip. Pass `undefined` for a field to clear it. */
export function setTabState(tabId: string, state: TabState): void {
  useWorkspaceStore.setState((s) => {
    const prev = s.tabStates[tabId] ?? {}
    const next: TabState = { ...prev, ...state }
    if (state.activity && s.activeTabId === tabId && document.visibilityState === 'visible') next.activity = false
    if (next.status === prev.status && next.progress === prev.progress && next.activity === prev.activity) return {}
    return { tabStates: { ...s.tabStates, [tabId]: next } }
  })
}

function clearActivity(tabId: string): void {
  const st = useWorkspaceStore.getState().tabStates[tabId]
  if (st?.activity) setTabState(tabId, { activity: false })
}

export function activeTab(): TabInfo | undefined {
  const p = activeSpace()?.api.activePanel
  return p ? infoOf(p) : undefined
}

export function listTabs(): TabInfo[] {
  return useWorkspaceStore.getState().tabs
}

export function findTabs(pred: (t: TabInfo) => boolean): TabInfo[] {
  return listTabs().filter(pred)
}

// ---------------------------------------------------------------------------------------------------------------------
// layout operations
// ---------------------------------------------------------------------------------------------------------------------

/** Duplicate a tab (kind.duplicate, else same kind+params unless singleton). */
export function duplicateTab(tabId?: string, position: TabPosition = 'tab'): void {
  const panel = panelOrActive(tabId)
  if (!panel) return
  const info = infoOf(panel)
  const def = tabKinds.get(info.kind)
  if (def?.duplicate) {
    def.duplicate(info, position)
    return
  }
  if (def?.singleton) {
    toast.info(`Only one ${def.title(info.params)} tab can be open`)
    return
  }
  openTab({ kind: info.kind, params: info.params, position, reference: panel.id })
}

/**
 * Split the tab's space: a new pane beside it with a duplicate of the tab (e.g. a second shell on the same host).
 * A pane that somehow holds several tabs moves the tab into the new pane instead.
 */
export function splitActive(direction: 'right' | 'below', tabId?: string): void {
  const panel = panelOrActive(tabId)
  if (!panel) {
    toast.info('Open a tab first to split it')
    return
  }
  if (panel.group.panels.length > 1) {
    panel.api.moveTo({ group: panel.group, position: direction === 'right' ? 'right' : 'bottom' })
    sync()
    return
  }
  duplicateTab(panel.id, direction)
}

export type LayoutPreset = 'single' | 'columns' | 'rows' | 'grid'

/**
 * Re-tile the active space's docked panes: two columns, two rows or a 2×2 grid; 'single' moves every pane but the
 * focused one into a space of its own.
 */
export function arrangeLayout(preset: LayoutPreset): void {
  const s = activeSpace()
  if (!s) return
  const dv = s.api
  const panels = dv.panels.filter((p) => p.group.api.location.type === 'grid')
  if (!panels.length) return
  const active = dv.activePanel
  if (dv.hasMaximizedGroup()) dv.exitMaximizedGroup()
  if (preset === 'single') {
    for (const p of panels) if (p.id !== active?.id) moveTab(p.id)
    return
  }
  if (panels.length < 2) return
  const base = panels[0].group
  for (const p of panels) if (p.group !== base) p.api.moveTo({ group: base, position: 'center', skipSetActive: true })
  const groups = [base]
  if (preset === 'columns') groups.push(dv.addGroup({ referenceGroup: base, direction: 'right' }))
  else if (preset === 'rows') groups.push(dv.addGroup({ referenceGroup: base, direction: 'below' }))
  else {
    const topRight = dv.addGroup({ referenceGroup: base, direction: 'right' })
    const bottomLeft = dv.addGroup({ referenceGroup: base, direction: 'below' })
    const bottomRight = dv.addGroup({ referenceGroup: topRight, direction: 'below' })
    groups.push(topRight, bottomLeft, bottomRight)
  }
  // One tab per pane: spread them, then split any pane left with several (more tabs than preset panes).
  panels.forEach((p, i) => {
    const target = groups[i % groups.length]
    if (p.group !== target) p.api.moveTo({ group: target, position: 'center', skipSetActive: true })
  })
  for (const g of dv.groups) {
    while (g.panels.length > 1) g.panels[g.panels.length - 1].api.moveTo({ group: g, position: 'right', skipSetActive: true })
  }
  for (const g of groups) if (g.panels.length === 0) dv.removeGroup(g)
  if (active && dv.getPanel(active.id)) active.api.setActive()
  sync()
}

/** Move a tab into its own browser window (dockview pop-out group). */
export async function popout(tabId?: string): Promise<boolean> {
  const panel = panelOrActive(tabId)
  const s = panel ? mounted.get(spaceOf(panel.id)!) : undefined
  if (!s || !panel) return false
  if (panel.api.location.type === 'popout') return true
  const ok = await s.api.addPopoutGroup(panel, { popoutUrl: POPOUT_URL })
  if (!ok) toast.error('Could not open a new window', { description: 'Allow pop-ups for this site and try again.' })
  sync()
  return ok
}

/** Float a tab above its space's panes. */
export function float(tabId?: string): void {
  const panel = panelOrActive(tabId)
  const s = panel ? mounted.get(spaceOf(panel.id)!) : undefined
  if (!s || !panel || panel.api.location.type === 'floating') return
  const dv = s.api
  const w = Math.min(820, Math.max(360, dv.width - 160))
  const h = Math.min(520, Math.max(240, dv.height - 160))
  dv.addFloatingGroup(panel, { width: w, height: h, x: Math.max(20, (dv.width - w) / 2), y: Math.max(20, (dv.height - h) / 3) })
  sync()
}

/** Bring a floating / popped-out tab back into its space's panes (as a pane of its own). */
export function dockTab(tabId?: string): void {
  const panel = panelOrActive(tabId)
  const s = panel ? mounted.get(spaceOf(panel.id)!) : undefined
  if (!s || !panel || panel.api.location.type === 'grid') return
  const dv = s.api
  // A pop-out leaves a hidden, empty "reference" group in the grid, and moving the last panel out of the pop-out
  // removes that pop-out group together with an EMPTY reference group — before the panel is added to the target. So
  // never target an empty group: with no other pane in the grid, a fresh group is created.
  const target = dv.groups.find((g) => g.api.location.type === 'grid' && g !== panel.group && g.api.isVisible && g.panels.length > 0)
  if (target) panel.api.moveTo({ group: target, position: 'right' })
  else panel.api.moveTo({ group: dv.addGroup(), position: 'center' })
  panel.api.setActive()
  sync()
}

/** Toggle maximize of the tab's pane (docked panes only). */
export function toggleMaximize(tabId?: string): void {
  const panel = panelOrActive(tabId)
  if (!panel || panel.api.location.type !== 'grid') return
  if (panel.api.isMaximized()) panel.api.exitMaximized()
  else panel.api.maximize()
  sync()
}

/** Activate the next/previous space of the title bar (wraps). */
export function cycleTab(delta: 1 | -1): void {
  const { spaces, activeSpaceId } = useWorkspaceStore.getState()
  if (spaces.length < 2) return
  const cur = spaces.findIndex((s) => s.id === activeSpaceId)
  focusSpace(spaces[(cur + delta + spaces.length) % spaces.length].id)
}

/** Activate the n-th space (1-based) of the title bar. */
export function activateTabIndex(n: number): void {
  const s = useWorkspaceStore.getState().spaces[n - 1]
  if (s) focusSpace(s.id)
}

// ---------------------------------------------------------------------------------------------------------------------
// closed-tab stack (CC-7)
// ---------------------------------------------------------------------------------------------------------------------

function pushClosed(info: TabInfo, placement: TabPlacement): void {
  const entry: ClosedTab = { kind: info.kind, params: info.params, title: info.title, closedAt: Date.now(), placement }
  useWorkspaceStore.setState((s) => {
    const closed = [entry, ...s.closed].slice(0, MAX_CLOSED)
    storage.set(closedKey(), closed)
    return { closed }
  })
}

/**
 * Reopen the most recently closed tab (or the one at `index` of the history) where it was: next to its old
 * neighbours in the title bar. Resolves false when there is no such entry, true once the tab kind reopened it.
 */
export async function reopenClosed(index = 0): Promise<boolean> {
  const { closed } = useWorkspaceStore.getState()
  const entry = closed[index]
  if (!entry) return false
  const rest = closed.filter((_, i) => i !== index)
  storage.set(closedKey(), rest)
  useWorkspaceStore.setState({ closed: rest })
  const def = tabKinds.get(entry.kind)
  try {
    if (def?.reopen) await def.reopen(entry)
    else openTab({ kind: entry.kind, params: entry.params, title: entry.title, placement: entry.placement })
  } catch (err) {
    console.error('[workspace] reopen failed', err)
  }
  return true
}

export function clearClosedTabs(): void {
  storage.remove(closedKey())
  useWorkspaceStore.setState({ closed: [] })
}

// ---------------------------------------------------------------------------------------------------------------------
// saved workspaces (named layouts)
// ---------------------------------------------------------------------------------------------------------------------

const MAX_SAVED_WORKSPACES = 20

/** Save the open tabs and their splits under a name (replaces a saved workspace of the same name). */
export function saveWorkspace(name: string): SavedWorkspace | null {
  const n = name.trim()
  if (!initialized || !n) return null
  const tabs = listTabs().map((t) => ({ kind: t.kind, title: t.title }))
  if (!tabs.length) {
    toast.info('Open some tabs first', { description: 'A workspace saves the open tabs and how they are arranged.' })
    return null
  }
  const entry: SavedWorkspace = { id: uid('ws'), name: n, savedAt: new Date().toISOString(), tabs, layout: serialize() }
  workspacesSettings.set((w) => ({ saved: [entry, ...w.saved.filter((x) => x.name.toLowerCase() !== n.toLowerCase())].slice(0, MAX_SAVED_WORKSPACES) }))
  return entry
}

export function deleteWorkspace(id: string): void {
  workspacesSettings.set((w) => ({ saved: w.saved.filter((x) => x.id !== id) }))
}

/**
 * Replace the open tabs by a saved workspace: the current tabs close (running sessions get the usual Undo), the saved
 * arrangement is restored, and each tab kind revives its tabs (e.g. terminals re-attach their session, or start a new
 * one when it no longer runs). Resolves false when a tab refused to close.
 */
export async function restoreWorkspace(id: string): Promise<boolean> {
  const saved = workspacesSettings.get().saved.find((x) => x.id === id)
  if (!initialized || !saved) return false
  await closeTabs(listTabs().map((t) => t.id))
  if (listTabs().length) return false
  // Saved before spaces existed: one dockview layout (each of its tabs becomes a space).
  const layout = saved.layout as SavedLayout | SerializedDockview
  const ok = 'spaces' in (layout as object) ? loadSpaces(layout as SavedLayout, null) : loadSpaces(null, layout as SerializedDockview)
  if (!ok) {
    toast.error(`Could not restore “${saved.name}”`, { description: 'The saved layout is not readable any more.' })
    return false
  }
  // Revive once the spaces have mounted and their tabs exist (bounded: a space that never mounts is skipped).
  for (let i = 0; i < 100 && useWorkspaceStore.getState().spaces.some((x) => seeds.has(x.id)); i++) {
    await new Promise((r) => setTimeout(r, 50))
  }
  for (const tab of listTabs()) {
    try {
      tabKinds.get(tab.kind)?.revive?.(tab)
    } catch (err) {
      console.error('[workspace] revive failed', err)
    }
  }
  return true
}

export const useSavedWorkspaces = (): SavedWorkspace[] => workspacesSettings.useValue('saved')

// ---------------------------------------------------------------------------------------------------------------------
// hooks
// ---------------------------------------------------------------------------------------------------------------------

export const useTabs = () => useWorkspaceStore((s) => s.tabs)
export const useActiveTabId = () => useWorkspaceStore((s) => s.activeTabId)
export const useClosedTabs = () => useWorkspaceStore((s) => s.closed)
export const useWorkspaceReady = () => useWorkspaceStore((s) => s.ready)
export const useSpaces = () => useWorkspaceStore((s) => s.spaces)
export const useActiveSpaceId = () => useWorkspaceStore((s) => s.activeSpaceId)

export function useActiveTab(): TabInfo | undefined {
  return useWorkspaceStore((s) => (s.activeTabId ? s.tabs.find((t) => t.id === s.activeTabId) : undefined))
}

const EMPTY_STATE: TabState = Object.freeze({})
export function useTabState(tabId: string): TabState {
  return useWorkspaceStore((s) => s.tabStates[tabId] ?? EMPTY_STATE)
}

/** Subscribe to a dock panel's title (for components rendered inside the tab). */
export function usePanelTitle(tabId: string): string | undefined {
  return useWorkspaceStore((s) => s.tabs.find((t) => t.id === tabId)?.title)
}

/** Whether a tab is on screen: the visible pane content of the active space (or floating / popped out there). */
export function isTabVisible(tabId: string): boolean {
  const panel = getPanel(tabId)
  if (!panel?.api.isVisible) return false
  return panel.api.location.type === 'popout' || spaceOf(tabId) === useWorkspaceStore.getState().activeSpaceId
}

/** True while the tab is on screen (useful to pause rendering work). */
export function useIsTabVisible(tabId: string): boolean {
  const subscribe = useCallback(
    (cb: () => void) => {
      const panel = getPanel(tabId)
      const d = panel?.api.onDidVisibilityChange(cb)
      const unsub = useWorkspaceStore.subscribe((s, prev) => {
        if (s.activeSpaceId !== prev.activeSpaceId) cb()
      })
      return () => {
        d?.dispose()
        unsub()
      }
    },
    [tabId],
  )
  const get = () => isTabVisible(tabId)
  return useSyncExternalStore(subscribe, get, get)
}

/** Convenience namespace. */
export const workspace = {
  openTab,
  closeTab,
  closeTabs,
  closeOtherTabs,
  closeTabsToRight,
  focusTab,
  renameTab,
  setTabTitle,
  updateTabParams,
  getTabParams,
  setTabState,
  activeTab,
  listTabs,
  findTabs,
  duplicateTab,
  splitActive,
  arrangeLayout,
  popout,
  float,
  dockTab,
  maximize: toggleMaximize,
  cycleTab,
  activateTabIndex,
  reopenClosed,
  saveWorkspace,
  restoreWorkspace,
  deleteWorkspace,
}
