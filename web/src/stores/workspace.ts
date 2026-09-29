/*
 * Workspace = the dockview tab/split/float/pop-out area. This module wraps the DockviewApi so features never touch
 * dockview directly:
 *
 *   openTab({kind, params, title?, position?: 'tab'|'right'|'below'|'left'|'above'|'float'|'window', id?})  → tabId
 *   closeTab(tabId) · focusTab · renameTab · setTabTitle · updateTabParams · setTabState(tabId, {status, progress, activity})
 *   activeTab() / useActiveTab() · listTabs() / useTabs() · useTabState(tabId)
 *   splitActive('right'|'below') · arrangeLayout('single'|'columns'|'rows'|'grid') · popout · float · dockTab · maximize
 *   reopenClosed() (CC-7) · layout persistence (localStorage, per user)
 *
 * Every dock panel uses one generic host component; the real component comes from the tab-kind registry, so tabs
 * restored before their feature registered render a placeholder until it does.
 */
import { useCallback, useSyncExternalStore } from 'react'
import { create } from 'zustand'
import { toast } from 'sonner'
import type { AddPanelOptions, DockviewApi, IDockviewPanel, SerializedDockview } from 'dockview-react'
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
  /** Put the tab back where a closed tab was (ClosedTab.placement): next to its old neighbours, else at its old index
   *  in its group, else like a new tab. Ignored when `position` is not 'tab'. */
  placement?: TabPlacement
}

interface WorkspaceStore {
  ready: boolean
  tabs: TabInfo[]
  activeTabId: string | null
  tabStates: Record<string, TabState>
  closed: ClosedTab[]
  maximized: boolean
  /** Tab ids per pane (grid and floating groups, in order; pop-out windows excluded) — drives the title-bar tabs. */
  panes: string[][]
}

export const useWorkspaceStore = create<WorkspaceStore>(() => ({
  ready: false,
  tabs: [],
  activeTabId: null,
  tabStates: {},
  closed: [],
  maximized: false,
  panes: [],
}))

const MAX_CLOSED = 25
let api: DockviewApi | null = null
let userKey = 'anonymous'
/** Grid groups hide their own tab header: their tabs are shown in the title bar instead (see setTitleBarTabs). */
let titleBarTabs = false
const pendingOpens: OpenTabOptions[] = []
const windowListeners = new Set<(win: Window) => void>()

const layoutKey = () => `astraterm:layout:v1:${userKey}`
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

/**
 * Rebuild the tab list from dockview. Unchanged TabInfo objects keep their identity (and the store is not touched
 * when nothing changed), so frequent layout events (sash drags) do not re-render subscribers.
 */
function sync(): void {
  if (!api) return
  const prev = useWorkspaceStore.getState()
  const prevById = new Map(prev.tabs.map((t) => [t.id, t]))
  const tabs: TabInfo[] = []
  let changed = false
  for (const group of api.groups) {
    for (const panel of group.panels) {
      const next = infoOf(panel)
      const old = prevById.get(next.id)
      const reuse = old && old.kind === next.kind && old.title === next.title && old.params === next.params
      if (!reuse) changed = true
      tabs.push(reuse ? old : next)
    }
  }
  if (tabs.length !== prev.tabs.length || tabs.some((t, i) => t !== prev.tabs[i])) changed = true
  const activeTabId = api.activePanel?.id ?? null
  const maximized = api.hasMaximizedGroup()
  const live = new Set(tabs.map((t) => t.id))
  let tabStates = prev.tabStates
  for (const id of Object.keys(tabStates)) {
    if (!live.has(id)) {
      if (tabStates === prev.tabStates) tabStates = { ...tabStates }
      delete tabStates[id]
    }
  }
  const nextPanes: string[][] = []
  for (const group of api.groups) {
    const where = group.api.location.type
    const hidden = titleBarTabs && where === 'grid'
    if (group.header.hidden !== hidden) group.header.hidden = hidden
    if (where !== 'popout' && group.panels.length) nextPanes.push(group.panels.map((p) => p.id))
  }
  const panes = samePanes(prev.panes, nextPanes) ? prev.panes : nextPanes
  if (!changed && activeTabId === prev.activeTabId && maximized === prev.maximized && tabStates === prev.tabStates && panes === prev.panes) return
  useWorkspaceStore.setState({ tabs: changed ? tabs : prev.tabs, activeTabId, tabStates, maximized, panes })
}

function samePanes(a: string[][], b: string[][]): boolean {
  return a.length === b.length && a.every((ids, i) => ids.length === b[i].length && ids.every((id, j) => id === b[i][j]))
}

/** Show grid tabs in the title bar (true) or in each group's own header (false, e.g. on phones). */
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
      const dv = api
      if (!dv || dv.width <= 0 || dv.height <= 0) return
      try {
        dv.layout(dv.width, dv.height, true)
      } catch {
        /* disposed */
      }
    })
  })
}

const saveLayout = debounce(() => {
  if (!api) return
  try {
    storage.set(layoutKey(), api.toJSON())
  } catch (err) {
    console.warn('[workspace] could not serialise layout', err)
  }
}, 400)

function requireApi(): DockviewApi | null {
  if (!api) console.warn('[workspace] dockview is not ready yet')
  return api
}

function panelOrActive(tabId?: string): IDockviewPanel | undefined {
  if (!api) return undefined
  return tabId ? api.getPanel(tabId) : api.activePanel
}

// ---------------------------------------------------------------------------------------------------------------------
// lifecycle (called by layout/Workspace.tsx)
// ---------------------------------------------------------------------------------------------------------------------

/**
 * Bind the dockview instance. Restores the saved layout (when `restore`), opens `home` on first run, flushes openTab
 * calls made before the workspace was ready, and returns a disposer.
 */
export function attachDockview(dv: DockviewApi, opts: { userId: string; restore: boolean }): () => void {
  api = dv
  userKey = opts.userId || 'anonymous'
  useWorkspaceStore.setState({ closed: storage.get<ClosedTab[]>(closedKey(), []).slice(0, MAX_CLOSED) })

  const structural = () => {
    sync()
    scheduleOverlayFix()
  }
  const disposables = [
    dv.onDidAddPanel(structural),
    dv.onDidRemovePanel(structural),
    dv.onDidMovePanel(structural),
    dv.onDidAddGroup(structural),
    dv.onDidRemoveGroup(structural),
    dv.onDidActivePanelChange((e) => {
      const id = e.panel?.id
      if (id) clearActivity(id)
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
  ]

  const saved = storage.get<SerializedDockview | null>(layoutKey(), null)
  if (opts.restore && saved) {
    try {
      dv.fromJSON(sanitizeLayout(saved))
    } catch (err) {
      console.warn('[workspace] saved layout could not be restored; starting fresh', err)
      try {
        dv.clear()
      } catch {
        /* ignore */
      }
      storage.remove(layoutKey())
    }
  }
  if (!saved) {
    // First run for this user on this browser: show the Home tab.
    pendingOpens.unshift({ kind: 'home' })
  }

  useWorkspaceStore.setState({ ready: true })
  sync()
  const queued = pendingOpens.splice(0)
  for (const o of queued) openTab(o)

  // A reload or close right after a change (within the save debounce) must not lose it.
  const flushLayout = () => saveLayout.flush()
  window.addEventListener('pagehide', flushLayout)

  return () => {
    window.removeEventListener('pagehide', flushLayout)
    if (overlayFixFrame) cancelAnimationFrame(overlayFixFrame)
    overlayFixFrame = 0
    saveLayout.flush()
    for (const d of disposables) d.dispose()
    for (const un of popoutDisposers.values()) un()
    popoutDisposers.clear()
    if (api === dv) api = null
    useWorkspaceStore.setState({ ready: false, tabs: [], activeTabId: null, tabStates: {}, maximized: false, panes: [] })
  }
}

const popoutDisposers = new Map<string, () => void>()

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
}

export function getDockviewApi(): DockviewApi | null {
  return api
}

/** Windows currently hosting popped-out tab groups. */
export function getPopoutWindows(): Window[] {
  if (!api) return []
  const out: Window[] = []
  for (const p of api.getPopouts()) {
    try {
      if (p.window && !p.window.closed) out.push(p.window)
    } catch {
      /* window gone */
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

/** Open (or focus) a tab. Returns its id. Safe to call before the workspace is mounted (queued). */
export function openTab<P = any>(opts: OpenTabOptions<P>): string {
  const def = tabKinds.get(opts.kind)
  const id = opts.id ?? (def?.singleton ? opts.kind : uid(opts.kind))
  if (!api) {
    pendingOpens.push({ ...opts, id })
    return id
  }
  const existing = api.getPanel(id)
  if (existing) {
    if (opts.params !== undefined) {
      existing.update({ params: { params: opts.params } })
      if (opts.title) existing.api.setTitle(opts.title)
      sync()
    }
    if (opts.activate !== false) focusTab(id)
    return id
  }

  const params = (opts.params ?? {}) as P
  const wrapper: PanelParams<P> = { kind: opts.kind, params }
  const title = opts.title || defaultTitle(opts.kind, params)
  const position = opts.position ?? 'tab'
  const ref = opts.reference ? api.getPanel(opts.reference) : api.activePanel

  const base: AddPanelOptions<PanelParams<P>> = {
    id,
    component: HOST_COMPONENT,
    tabComponent: TAB_COMPONENT,
    title,
    params: wrapper,
    inactive: opts.activate === false,
  }

  let panel: IDockviewPanel
  try {
    const restored = position === 'tab' ? placementTarget(opts.placement) : null
    if (restored) {
      panel = api.addPanel({ ...base, position: { referenceGroup: restored.group, index: restored.index } } as AddPanelOptions<PanelParams<P>>)
    } else if (position === 'float') {
      const w = Math.min(820, Math.max(360, api.width - 120))
      const h = Math.min(520, Math.max(240, api.height - 120))
      panel = api.addPanel({
        ...base,
        floating: { width: w, height: h, x: Math.max(20, (api.width - w) / 2), y: Math.max(20, (api.height - h) / 3) },
      } as AddPanelOptions<PanelParams<P>>)
    } else if (DIRECTION[position]) {
      const direction = DIRECTION[position]
      panel = api.addPanel({
        ...base,
        position: ref ? { referencePanel: ref, direction } : { direction },
      } as AddPanelOptions<PanelParams<P>>)
    } else {
      panel = api.addPanel({
        ...base,
        ...(ref ? { position: { referenceGroup: ref.group } } : {}),
      } as AddPanelOptions<PanelParams<P>>)
    }
  } catch (err) {
    console.error('[workspace] openTab failed', err)
    toast.error('Could not open tab', { description: err instanceof Error ? err.message : String(err) })
    return id
  }
  if (position === 'window') void popout(panel.id)
  if (opts.placement && opts.placement.tabId !== id) {
    reopenedIds.set(opts.placement.tabId, id)
    if (reopenedIds.size > MAX_CLOSED * 4) reopenedIds.delete(reopenedIds.keys().next().value!)
  }
  sync()
  return id
}

/** Old id → new id of reopened tabs, so a later reopen can still find a neighbour that came back under a new id. */
const reopenedIds = new Map<string, string>()

function livePanel(id: string | undefined): IDockviewPanel | undefined {
  if (!api || !id) return undefined
  for (let i = 0, cur: string | undefined = id; cur && i < 8; i++, cur = reopenedIds.get(cur)) {
    const p = api.getPanel(cur)
    if (p) return p
  }
  return undefined
}

/** Where a reopened tab goes: after its old left neighbour, before its old right one, or at its old index. */
function placementTarget(pl: TabPlacement | undefined): { group: IDockviewPanel['group']; index: number } | null {
  if (!api || !pl) return null
  const prev = livePanel(pl.prevId)
  if (prev) return { group: prev.group, index: prev.group.panels.indexOf(prev) + 1 }
  const next = livePanel(pl.nextId)
  if (next) return { group: next.group, index: next.group.panels.indexOf(next) }
  const group = api.groups.find((g) => g.id === pl.groupId && g.api.isVisible)
  return group ? { group, index: Math.max(0, Math.min(pl.index, group.panels.length)) } : null
}

function placementOf(panel: IDockviewPanel): TabPlacement {
  const list = panel.group.panels
  const i = list.indexOf(panel)
  return { tabId: panel.id, groupId: panel.group.id, index: Math.max(0, i), prevId: list[i - 1]?.id, nextId: list[i + 1]?.id }
}

/** Close a tab (asks the tab kind's canClose unless forced). Resolves true when closed. */
export async function closeTab(tabId: string, opts: { force?: boolean } = {}): Promise<boolean> {
  const panel = api?.getPanel(tabId)
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
  const still = api?.getPanel(tabId)
  if (!still) return false
  if (!def?.noReopen) pushClosed(info, placementOf(still))
  still.api.close()
  try {
    def?.onClose?.(info)
  } catch (err) {
    console.error('[workspace] onClose failed', err)
  }
  sync()
  return true
}

/** Close several tabs sequentially (each may veto). */
export async function closeTabs(ids: string[], opts: { force?: boolean } = {}): Promise<void> {
  for (const id of ids) await closeTab(id, opts)
}

/** Close all tabs in the same group except `tabId`. */
export function closeOtherTabs(tabId: string): Promise<void> {
  const panel = api?.getPanel(tabId)
  if (!panel) return Promise.resolve()
  return closeTabs(panel.group.panels.filter((p) => p.id !== tabId).map((p) => p.id))
}

/** Close tabs to the right of `tabId` in its group. */
export function closeTabsToRight(tabId: string): Promise<void> {
  const panel = api?.getPanel(tabId)
  if (!panel) return Promise.resolve()
  const list = panel.group.panels
  const i = list.findIndex((p) => p.id === tabId)
  return closeTabs(list.slice(i + 1).map((p) => p.id))
}

export function focusTab(tabId: string): boolean {
  const panel = api?.getPanel(tabId)
  if (!panel) return false
  panel.api.setActive()
  if (panel.api.location.type === 'popout') {
    try {
      panel.api.getWindow().focus()
    } catch {
      /* window gone */
    }
  }
  return true
}

/** User rename: the title sticks (automatic titles are ignored from now on). Empty title restores automatic titles. */
export function renameTab(tabId: string, title: string): void {
  const panel = api?.getPanel(tabId)
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
  const panel = api?.getPanel(tabId)
  if (!panel || !title) return
  if (wrapperOf(panel).userTitle && !opts.force) return
  if (panel.title === title) return
  panel.api.setTitle(title)
  sync()
}

/** Merge into a tab's params (persisted with the layout), e.g. the terminal stores its sessionId. */
export function updateTabParams<P extends object>(tabId: string, patch: Partial<P>): void {
  const panel = api?.getPanel(tabId)
  if (!panel) return
  const w = wrapperOf(panel)
  const prev = (w.params && typeof w.params === 'object' ? w.params : {}) as P
  panel.update({ params: { params: { ...prev, ...patch } } })
  sync()
  saveLayout()
}

/** Get a tab's params (undefined if the tab does not exist). */
export function getTabParams<P = any>(tabId: string): P | undefined {
  const panel = api?.getPanel(tabId)
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
  const p = api?.activePanel
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
 * Split: with several tabs in the group, move the tab into a new group beside it; with a single tab, duplicate it
 * into the new split (e.g. a second shell on the same host).
 */
export function splitActive(direction: 'right' | 'below', tabId?: string): void {
  const panel = panelOrActive(tabId)
  if (!panel) {
    toast.info('Open a tab first to split the workspace')
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

/** Tiling of all docked (non-floating) tabs: 1, 2 columns, 2 rows or 2×2 grid. */
export function arrangeLayout(preset: LayoutPreset): void {
  const dv = requireApi()
  if (!dv) return
  const panels = dv.panels.filter((p) => p.group.api.location.type === 'grid')
  if (!panels.length) return
  const active = dv.activePanel
  if (dv.hasMaximizedGroup()) dv.exitMaximizedGroup()
  const base = panels[0].group
  for (const p of panels) if (p.group !== base) p.api.moveTo({ group: base, position: 'center', skipSetActive: true })
  if (preset !== 'single' && panels.length > 1) {
    const groups = [base]
    if (preset === 'columns') groups.push(dv.addGroup({ referenceGroup: base, direction: 'right' }))
    else if (preset === 'rows') groups.push(dv.addGroup({ referenceGroup: base, direction: 'below' }))
    else {
      const topRight = dv.addGroup({ referenceGroup: base, direction: 'right' })
      const bottomLeft = dv.addGroup({ referenceGroup: base, direction: 'below' })
      const bottomRight = dv.addGroup({ referenceGroup: topRight, direction: 'below' })
      groups.push(topRight, bottomLeft, bottomRight)
    }
    panels.forEach((p, i) => {
      const target = groups[i % groups.length]
      if (p.group !== target) p.api.moveTo({ group: target, position: 'center', skipSetActive: true })
    })
    for (const g of groups) if (g.panels.length === 0) dv.removeGroup(g)
  }
  if (active && dv.getPanel(active.id)) active.api.setActive()
  sync()
}

/** Move a tab into its own browser window (dockview pop-out group). */
export async function popout(tabId?: string): Promise<boolean> {
  const dv = requireApi()
  const panel = panelOrActive(tabId)
  if (!dv || !panel) return false
  if (panel.api.location.type === 'popout') return true
  const ok = await dv.addPopoutGroup(panel, { popoutUrl: POPOUT_URL })
  if (!ok) toast.error('Could not open a new window', { description: 'Allow pop-ups for this site and try again.' })
  sync()
  return ok
}

/** Float a tab above the grid. */
export function float(tabId?: string): void {
  const dv = requireApi()
  const panel = panelOrActive(tabId)
  if (!dv || !panel || panel.api.location.type === 'floating') return
  const w = Math.min(820, Math.max(360, dv.width - 160))
  const h = Math.min(520, Math.max(240, dv.height - 160))
  dv.addFloatingGroup(panel, { width: w, height: h, x: Math.max(20, (dv.width - w) / 2), y: Math.max(20, (dv.height - h) / 3) })
  sync()
}

/** Bring a floating / popped-out tab back into the grid. */
export function dockTab(tabId?: string): void {
  const dv = requireApi()
  const panel = panelOrActive(tabId)
  if (!dv || !panel || panel.api.location.type === 'grid') return
  // A pop-out leaves a hidden, empty "reference" group in the main grid, and moving the last panel out of the pop-out
  // removes that pop-out group together with an EMPTY reference group — before the panel is added to the target. So
  // never target an empty group: with no other tab in the grid, a fresh group is created (else the panel ended up in
  // a disposed group: 0×0 and missing from the tab bar).
  const target =
    dv.groups.find((g) => g.api.location.type === 'grid' && g !== panel.group && g.api.isVisible && g.panels.length > 0) ?? dv.addGroup()
  panel.api.moveTo({ group: target, position: 'center' })
  panel.api.setActive()
  sync()
}

/** Toggle maximize of the tab's group (grid groups only). */
export function toggleMaximize(tabId?: string): void {
  const panel = panelOrActive(tabId)
  if (!panel || panel.api.location.type !== 'grid') return
  if (panel.api.isMaximized()) panel.api.exitMaximized()
  else panel.api.maximize()
  sync()
}

/** Activate the next/previous tab across all groups (wraps). */
export function cycleTab(delta: 1 | -1): void {
  const dv = api
  if (!dv) return
  const all: IDockviewPanel[] = []
  for (const g of dv.groups) all.push(...g.panels)
  if (all.length < 2) return
  const cur = dv.activePanel ? all.findIndex((p) => p.id === dv.activePanel!.id) : -1
  const next = all[(cur + delta + all.length) % all.length]
  next.api.setActive()
}

/** Activate the n-th tab (1-based) of the active group. */
export function activateTabIndex(n: number): void {
  const group = api?.activeGroup
  if (!group) return
  const p = group.panels[n - 1]
  p?.api.setActive()
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
 * neighbours in its group. Resolves false when there is no such entry, true once the tab kind reopened it. Reopening
 * several tabs most-recent-first (the stack order) rebuilds their original order.
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

/** Save the current tabs and splits under a name (replaces a saved workspace of the same name). */
export function saveWorkspace(name: string): SavedWorkspace | null {
  const dv = requireApi()
  const n = name.trim()
  if (!dv || !n) return null
  const tabs = listTabs().map((t) => ({ kind: t.kind, title: t.title }))
  if (!tabs.length) {
    toast.info('Open some tabs first', { description: 'A workspace saves the open tabs and how they are arranged.' })
    return null
  }
  const entry: SavedWorkspace = { id: uid('ws'), name: n, savedAt: new Date().toISOString(), tabs, layout: dv.toJSON() }
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
  const dv = requireApi()
  const saved = workspacesSettings.get().saved.find((x) => x.id === id)
  if (!dv || !saved) return false
  await closeTabs(listTabs().map((t) => t.id))
  if (listTabs().length) return false
  try {
    dv.fromJSON(sanitizeLayout(saved.layout as SerializedDockview))
  } catch (err) {
    console.warn('[workspace] could not restore the saved workspace', err)
    toast.error(`Could not restore “${saved.name}”`, { description: 'The saved layout is not readable any more.' })
    return false
  }
  sync()
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
export const usePanes = () => useWorkspaceStore((s) => s.panes)

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

/** True while the tab is the visible one of its group (useful to pause rendering work). */
export function useIsTabVisible(tabId: string): boolean {
  const subscribe = useCallback(
    (cb: () => void) => {
      const panel = api?.getPanel(tabId)
      if (!panel) return () => undefined
      const d = panel.api.onDidVisibilityChange(cb)
      return () => d.dispose()
    },
    [tabId],
  )
  const get = () => api?.getPanel(tabId)?.api.isVisible ?? false
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
