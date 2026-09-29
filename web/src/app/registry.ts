/*
 * Extension points (SPEC §7, §10.2). Features self-register from src/features/<name>/index.ts; every register*
 * function returns an unregister function. Registries are reactive: components using the use* hooks re-render when
 * something registers late (e.g. a lazily imported feature), so order of feature imports never matters.
 *
 *   registerTabKind        dock tab types (component rendered inside the dockview workspace)
 *   registerSidebarPanel   left sidebar panels (icon rail)
 *   registerCommand        commands (palette, menus, ribbon, keybindings) — run via runCommand() in ./commands
 *   registerRibbonButton   large-button toolbar (ribbon) buttons
 *   registerMenu           menu bar contributions (terminal | sessions | view | tools | settings | help)
 *   registerContextMenu    context menu contributions (session-node | terminal | tab | file)
 *   registerSettingsSection  sections of the Settings tab
 *   registerProtocolEditor   per-protocol connection editors (sessions feature renders them)
 *   registerStatusItem     status bar items
 *   registerTerminalPlugin terminal add-ons (trzsz, zmodem, highlighting, triggers...)
 */
import { useSyncExternalStore, type ComponentType, type ReactNode } from 'react'
import type { LucideIcon } from 'lucide-react'
import type { Terminal } from '@xterm/xterm'
import type { Connection, FileEntry, Protocol, RuntimeSession } from '@/api/types'

// ---------------------------------------------------------------------------------------------------------------------
// Generic reactive registry
// ---------------------------------------------------------------------------------------------------------------------

export interface Registry<T> {
  register(item: T): () => void
  get(id: string): T | undefined
  /** Stable snapshot (same array identity until the registry changes), sorted by `order` when present. */
  list(): readonly T[]
  subscribe(cb: () => void): () => void
  /** React hook: the sorted list, re-rendering on change. */
  useList(): readonly T[]
  /** React hook: one item by id (undefined until registered). */
  useItem(id: string | undefined): T | undefined
}

/**
 * `replaceable`: items that exist to be replaced (the shell's placeholders for buttons a feature owns): replacing them is
 * expected and not warned about in development.
 */
export function createRegistry<T>(
  name: string,
  keyOf: (item: T) => string,
  orderOf?: (item: T) => number,
  replaceable?: (item: T) => boolean,
): Registry<T> {
  const items = new Map<string, T>()
  const listeners = new Set<() => void>()
  let snapshot: readonly T[] = []

  const rebuild = () => {
    const arr = Array.from(items.values())
    if (orderOf) arr.sort((a, b) => orderOf(a) - orderOf(b))
    snapshot = Object.freeze(arr)
    for (const l of Array.from(listeners)) l()
  }

  const reg: Registry<T> = {
    register(item) {
      const key = keyOf(item)
      const prev = items.get(key)
      if (prev !== undefined && prev !== item && !replaceable?.(prev) && import.meta.env.DEV) console.warn(`[registry:${name}] "${key}" re-registered (replacing)`)
      items.set(key, item)
      rebuild()
      return () => {
        if (items.get(key) === item) {
          items.delete(key)
          rebuild()
        }
      }
    },
    get: (id) => items.get(id),
    list: () => snapshot,
    subscribe(cb) {
      listeners.add(cb)
      return () => listeners.delete(cb)
    },
    useList() {
      return useSyncExternalStore(reg.subscribe, reg.list, reg.list)
    },
    useItem(id) {
      const get = () => (id ? items.get(id) : undefined)
      return useSyncExternalStore(reg.subscribe, get, get)
    },
  }
  return reg
}

let seq = 0
const autoId = (prefix: string) => `${prefix}#${++seq}`

// ---------------------------------------------------------------------------------------------------------------------
// Icons
// ---------------------------------------------------------------------------------------------------------------------

/** Any icon component compatible with lucide's props (className, size, strokeWidth). */
export type IconType = LucideIcon | ComponentType<{ className?: string; size?: number | string; strokeWidth?: number | string }>

// ---------------------------------------------------------------------------------------------------------------------
// Tab kinds
// ---------------------------------------------------------------------------------------------------------------------

export interface TabProps<P = any> {
  /** Dock panel id (stable for the lifetime of the tab, persisted with the layout). */
  tabId: string
  /** Tab parameters as passed to openTab (persisted with the layout: keep them JSON-serialisable). */
  params: P
}

export interface TabInfo<P = any> {
  id: string
  kind: string
  title: string
  params: P
}

export interface TabKindDef<P = any> {
  kind: string
  /** Default title computed from params (a custom/renamed title overrides it). */
  title: (params: P) => string
  icon: IconType
  /** Optional per-tab icon (e.g. protocol icon for terminals). Falls back to `icon`. */
  iconFor?: (params: P) => IconType | undefined
  component: ComponentType<TabProps<P>>
  /** Only one tab of this kind may exist; openTab focuses it (and updates its params). */
  singleton?: boolean
  /**
   * Veto/confirm closing. Return false (or resolve false) to keep the tab open, e.g. a running session with
   * "confirm before closing" enabled. Not consulted for forced closes.
   */
  canClose?: (tab: TabInfo<P>) => boolean | Promise<boolean>
  /** Called after the user closed the tab (not when a tab moves between groups/windows). */
  onClose?: (tab: TabInfo<P>) => void
  /** "Duplicate tab": open an equivalent tab (default: same kind + params unless singleton). */
  duplicate?: (tab: TabInfo<P>, position?: TabPosition) => void
  /** Reopen from the closed-tab stack (CC-7). Default: openTab({kind, params, title, placement}). Pass
   *  `placement: closed.placement` to openTab so the tab returns to where it was; resolve once the tab is open. */
  reopen?: (closed: ClosedTab<P>) => void | Promise<void>
  /** Exclude from the closed-tab stack. */
  noReopen?: boolean
  /**
   * A saved workspace layout was restored with this tab in it (its params come from the saved layout): bring it back
   * to life, e.g. start a new session when the saved one no longer runs.
   */
  revive?: (tab: TabInfo<P>) => void
}

export type TabPosition = 'tab' | 'right' | 'below' | 'left' | 'above' | 'float' | 'window'

export interface ClosedTab<P = any> {
  kind: string
  params: P
  title: string
  closedAt: number
  /** Where the tab was (openTab `placement`), so reopening puts it back there. */
  placement?: TabPlacement
}

/** A tab's place in the workspace when it closed: its group and neighbours (ids at that time) and its index. */
export interface TabPlacement {
  /** The closed tab's id. */
  tabId: string
  groupId: string
  index: number
  /** The tab before / after it in its group. */
  prevId?: string
  nextId?: string
}

export const tabKinds = createRegistry<TabKindDef>('tabKind', (t) => t.kind)

export function registerTabKind<P = any>(def: TabKindDef<P>): () => void {
  return tabKinds.register(def as TabKindDef)
}

// ---------------------------------------------------------------------------------------------------------------------
// Sidebar panels
// ---------------------------------------------------------------------------------------------------------------------

export interface SidebarPanelDef {
  id: string
  title: string
  icon: IconType
  order: number
  component: ComponentType
  /** Optional badge renderer on the rail icon (e.g. running transfers count). */
  badge?: ComponentType
}

export const sidebarPanels = createRegistry<SidebarPanelDef>('sidebarPanel', (p) => p.id, (p) => p.order)

export function registerSidebarPanel(def: SidebarPanelDef): () => void {
  return sidebarPanels.register(def)
}

// ---------------------------------------------------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------------------------------------------------

export type CommandSource = 'palette' | 'keybinding' | 'menu' | 'ribbon' | 'context-menu' | 'api' | 'home'

export interface CommandContext<A = any> {
  /** Arguments passed to runCommand (e.g. {id} for sessions.connect). */
  args: A
  source: CommandSource
  /** The active dock tab when the command ran. */
  activeTab?: TabInfo
  /** The keyboard event for keybinding-triggered commands. */
  event?: KeyboardEvent
}

export interface CommandDef<A = any> {
  id: string
  title: string
  category?: string
  icon?: IconType
  /**
   * Default keybinding(s) in tinykeys syntax, e.g. "$mod+Shift+P", "Control+Alt+t", "g i" (sequence).
   * Users can override per command in Settings → Keyboard (stored in settings.keybindings).
   */
  keybinding?: string | string[]
  /** Fire the keybinding even while focus is in a text field / terminal (e.g. the command palette). */
  global?: boolean
  /**
   * Fire the keybinding even inside a keyboard owner — an embedded widget with its own shortcut system (Monaco:
   * `.monaco-editor`, or any element marked `data-keyboard-owner`). `true` = every binding of the command; a list =
   * only those default bindings (e.g. the palette keeps ⇧⌘P but leaves ⌘K chords to the editor). Keep this to
   * workspace essentials (close / switch tabs, lock, palette); everything else stays reachable from the palette.
   */
  essential?: boolean | string[]
  /** Hide from the command palette (still runnable, bindable). */
  hidden?: boolean
  /** Extra search terms for the palette. */
  keywords?: string[]
  description?: string
  run: (ctx: CommandContext<A>) => void | Promise<unknown>
  /** Availability predicate; unavailable commands are disabled in menus and skipped by keybindings/palette. */
  when?: () => boolean
}

export const commands = createRegistry<CommandDef>('command', (c) => c.id)

export function registerCommand<A = any>(def: CommandDef<A>): () => void {
  return commands.register(def as CommandDef)
}

// ---------------------------------------------------------------------------------------------------------------------
// Menus (menu bar, ribbon drop-downs, context menus)
// ---------------------------------------------------------------------------------------------------------------------

export type MenuItem =
  | {
      type?: 'item'
      id?: string
      label: string
      icon?: IconType
      /** Command to run (label/shortcut/disabled state derived from it). */
      command?: string
      args?: unknown
      /** Direct action (used when no command). */
      run?: () => void
      /** Display-only shortcut (defaults to the command's effective keybinding). */
      shortcut?: string
      disabled?: boolean
      /** Renders a checkmark (toggle items). */
      checked?: boolean
      danger?: boolean
    }
  | { type: 'separator' }
  | { type: 'label'; label: string }
  | { type: 'submenu'; label: string; icon?: IconType; items: MenuItem[] | (() => MenuItem[]); disabled?: boolean }

export type MenuId = 'terminal' | 'sessions' | 'view' | 'tools' | 'settings' | 'help'

export interface MenuContribution {
  id?: string
  menu: MenuId
  items: () => MenuItem[]
  order: number
}

export const menus = createRegistry<MenuContribution & { id: string }>('menu', (m) => m.id, (m) => m.order)

export function registerMenu(def: MenuContribution): () => void {
  return menus.register({ ...def, id: def.id ?? autoId(`menu:${def.menu}`) })
}

/** Items for one menu bar menu, contributions in order separated by separators. */
export function getMenuItems(menu: MenuId): MenuItem[] {
  return joinSections(
    menus
      .list()
      .filter((m) => m.menu === menu)
      .map((m) => safeItems(() => m.items(), `menu:${menu}`)),
  )
}

// --- context menus ---------------------------------------------------------------------------------------------------

export interface SessionNodeContext {
  /** Selected connection (for connection nodes). */
  connection?: Connection
  /** Folder id (for folder nodes). */
  folderId?: string
  /** All selected connection ids (multi-select). */
  selection?: string[]
  [key: string]: unknown
}

export interface TerminalContext {
  tabId: string
  sessionId?: string
  session?: RuntimeSession
  selection?: string
  [key: string]: unknown
}

export interface TabContext {
  tabId: string
  kind: string
  params: any
  title: string
  [key: string]: unknown
}

export interface FileContext {
  fsId: string
  path: string
  entries: FileEntry[]
  [key: string]: unknown
}

export interface ContextMenuContextMap {
  'session-node': SessionNodeContext
  terminal: TerminalContext
  tab: TabContext
  file: FileContext
}

export type ContextMenuTarget = keyof ContextMenuContextMap

export interface ContextMenuContribution<K extends ContextMenuTarget = ContextMenuTarget> {
  id?: string
  target: K
  items: (ctx: ContextMenuContextMap[K]) => MenuItem[]
  /** Order among the contributions of the same target (and group). */
  order: number
  /**
   * Built-in section of the target's menu to join (appended to it, in `order`). Without a group (or with one the
   * target does not know) the items form their own section after the built-in ones. Known groups:
   * - `terminal`: `edit` (copy / paste), `selection` (actions on the selected text, right after edit), `view`,
   *   `layout` (split / duplicate), `session` (reconnect, signals, recording), `files`, `close`.
   * Other targets have no built-in sections yet (group is ignored).
   */
  group?: string
}

export const contextMenus = createRegistry<ContextMenuContribution & { id: string }>(
  'contextMenu',
  (m) => m.id,
  (m) => m.order,
)

export function registerContextMenu<K extends ContextMenuTarget>(def: ContextMenuContribution<K>): () => void {
  return contextMenus.register({ ...(def as unknown as ContextMenuContribution), id: def.id ?? autoId(`ctx:${def.target}`) })
}

/** Contribution sections of a target in order (empty ones dropped), with the group each asked for. */
export function getContextMenuSections<K extends ContextMenuTarget>(target: K, ctx: ContextMenuContextMap[K]): { group?: string; items: MenuItem[] }[] {
  return contextMenus
    .list()
    .filter((m) => m.target === target)
    .map((m) => ({ group: m.group, items: safeItems(() => (m.items as (c: ContextMenuContextMap[K]) => MenuItem[])(ctx), `ctx:${target}`) }))
    .filter((sec) => sec.items.length > 0)
}

/** Collect context menu items for a target, contributions in order separated by separators (groups ignored). */
export function getContextMenuItems<K extends ContextMenuTarget>(target: K, ctx: ContextMenuContextMap[K]): MenuItem[] {
  return joinSections(
    contextMenus
      .list()
      .filter((m) => m.target === target)
      .map((m) => safeItems(() => (m.items as (c: ContextMenuContextMap[K]) => MenuItem[])(ctx), `ctx:${target}`)),
  )
}

function safeItems(fn: () => MenuItem[], where: string): MenuItem[] {
  try {
    return fn() ?? []
  } catch (err) {
    console.error(`[registry] ${where} items() failed`, err)
    return []
  }
}

/** Join item sections with single separators, trimming leading/trailing/double separators. */
export function joinSections(sections: MenuItem[][]): MenuItem[] {
  const out: MenuItem[] = []
  for (const sec of sections) {
    if (!sec.length) continue
    if (out.length) out.push({ type: 'separator' })
    out.push(...sec)
  }
  // normalise separators
  const norm: MenuItem[] = []
  for (const it of out) {
    if (it.type === 'separator' && (norm.length === 0 || norm[norm.length - 1].type === 'separator')) continue
    norm.push(it)
  }
  while (norm.length && norm[norm.length - 1].type === 'separator') norm.pop()
  return norm
}

// ---------------------------------------------------------------------------------------------------------------------
// Ribbon
// ---------------------------------------------------------------------------------------------------------------------

export interface RibbonButtonDef {
  id: string
  label: string
  icon: IconType
  order: number
  /** Command run on click (button renders disabled while the command is not registered). */
  command?: string
  args?: unknown
  /** Drop-down menu (instead of, or in addition to, a command: with both, the arrow opens the menu). */
  menu?: () => MenuItem[]
  /** Tooltip (defaults to the command title). */
  tooltip?: string
  /**
   * Toolbar group the button sits in (separated from its neighbours): 'connect' | 'workspace' | 'network' | 'tools' |
   * 'app' (any other name forms its own group after 'tools'). Defaults to the shell's grouping of known ids, else 'tools'.
   */
  group?: string
  /** A shell placeholder the owning feature replaces (no "re-registered" warning). */
  placeholder?: boolean
}

export const ribbonButtons = createRegistry<RibbonButtonDef>('ribbon', (b) => b.id, (b) => b.order, (b) => !!b.placeholder)

export function registerRibbonButton(def: RibbonButtonDef): () => void {
  return ribbonButtons.register(def)
}

// ---------------------------------------------------------------------------------------------------------------------
// Settings sections
// ---------------------------------------------------------------------------------------------------------------------

/** The groups of the Settings navigation, in display order. */
export const SETTINGS_GROUPS = [
  { id: 'general', title: 'General' },
  { id: 'appearance', title: 'Appearance' },
  { id: 'terminal', title: 'Terminal & Editor' },
  { id: 'connections', title: 'Connections' },
  { id: 'files', title: 'Files' },
  { id: 'security', title: 'Security' },
  { id: 'integrations', title: 'Integrations' },
  { id: 'about', title: 'About' },
] as const

export type SettingsGroupId = (typeof SETTINGS_GROUPS)[number]['id']

export interface SettingsSectionDef {
  id: string
  title: string
  icon: IconType
  /** Position inside its group. */
  order: number
  component: ComponentType
  /** Navigation group (default: by section id for the built-in features, else 'integrations'). */
  group?: SettingsGroupId
  /** Extra words matched by the settings search box. */
  keywords?: string[]
  /** Only show to admins. */
  adminOnly?: boolean
}

export const settingsSections = createRegistry<SettingsSectionDef>('settingsSection', (s) => s.id, (s) => s.order)

export function registerSettingsSection(def: SettingsSectionDef): () => void {
  return settingsSections.register(def)
}

// ---------------------------------------------------------------------------------------------------------------------
// Protocol editors
// ---------------------------------------------------------------------------------------------------------------------

export interface ProtocolEditorProps {
  /** Draft connection being edited (protocol-specific keys live in value.options). */
  value: Connection
  /** Replace the draft (immutably: pass a new object). */
  onChange: (next: Connection) => void
  mode: 'create' | 'edit'
}

export interface ProtocolEditorDef {
  protocol: Protocol
  label: string
  icon: IconType
  defaultPort: number
  group: 'terminal' | 'files' | 'graphical' | 'other'
  component: ComponentType<ProtocolEditorProps>
  description?: string
  order?: number
}

export const protocolEditors = createRegistry<ProtocolEditorDef>('protocolEditor', (p) => p.protocol, (p) => p.order ?? 100)

export function registerProtocolEditor(def: ProtocolEditorDef): () => void {
  return protocolEditors.register(def)
}

// ---------------------------------------------------------------------------------------------------------------------
// Status bar items
// ---------------------------------------------------------------------------------------------------------------------

export interface StatusItemDef {
  id: string
  align: 'left' | 'right'
  order: number
  component: ComponentType
}

export const statusItems = createRegistry<StatusItemDef>('statusItem', (s) => s.id, (s) => s.order)

export function registerStatusItem(def: StatusItemDef): () => void {
  return statusItems.register(def)
}

// ---------------------------------------------------------------------------------------------------------------------
// Terminal plugins (SPEC §10: implemented by the terminal feature, consumed through this registry)
// ---------------------------------------------------------------------------------------------------------------------

export interface TerminalPluginContext {
  tabId: string
  sessionId: string
  /** Current runtime session (live getter). */
  session: () => RuntimeSession | undefined
  /** Send input to the session (as if typed). */
  send: (data: string | Uint8Array) => void
  /** Observe user input going to the session. Returns unsubscribe. */
  onInput: (cb: (data: string) => void) => () => void
  /** Observe output bytes coming from the session (before they are written to xterm). Returns unsubscribe. */
  onOutput: (cb: (data: Uint8Array) => void) => () => void
  /** Effective terminal settings for this tab (settings.terminal merged with connection overrides). */
  settings: Readonly<Record<string, unknown>>
}

export interface TerminalPluginDef {
  id: string
  /** Called once per terminal instance; return a disposer. */
  setup: (term: Terminal, ctx: TerminalPluginContext) => (() => void) | void
  order?: number
}

export const terminalPlugins = createRegistry<TerminalPluginDef>('terminalPlugin', (p) => p.id, (p) => p.order ?? 100)

export function registerTerminalPlugin(def: TerminalPluginDef): () => void {
  return terminalPlugins.register(def)
}

// ---------------------------------------------------------------------------------------------------------------------
// Protocol openers: override how connections of a protocol open (default: runtime session → terminal/vnc/rdp tab, and
// file protocols → files tab). Used by features that own a protocol without a terminal backend (e.g. 'web').
// ---------------------------------------------------------------------------------------------------------------------

export interface ProtocolOpenOptions {
  position?: TabPosition
  reference?: string
  activate?: boolean
  title?: string
}

export interface ProtocolOpenerDef {
  protocol: string
  /** Open a saved (resolved) connection. Resolve to the new/focused tab id, or null after reporting the problem. */
  open: (conn: Connection, opts: ProtocolOpenOptions) => Promise<string | null>
  /** Open an unsaved quick-connect spec (optional; without it quick connect falls back to the default path). */
  openQuick?: (spec: Partial<Connection> & { password?: string }, opts: ProtocolOpenOptions) => Promise<string | null>
}

export const protocolOpeners = createRegistry<ProtocolOpenerDef>('protocolOpener', (p) => p.protocol)

export function registerProtocolOpener(def: ProtocolOpenerDef): () => void {
  return protocolOpeners.register(def)
}

// ---------------------------------------------------------------------------------------------------------------------
// Overlays: always-mounted app-wide React subtrees for feature dialogs, floating panels and drawers
// ---------------------------------------------------------------------------------------------------------------------

export interface OverlayDef {
  id: string
  /**
   * Rendered once inside the authenticated shell (under the query client, tooltip provider and an error boundary), so
   * it exists even when the sidebar/status bar/tabs are hidden. Keep dialog state in a store so any part of the app
   * (commands, menus, other features) can open it. Unmounted on sign-out.
   */
  component: ComponentType<{ locked: boolean }>
  order?: number
  /**
   * By default an overlay is unmounted while the lock screen is up (it cannot fight the lock screen for focus). Set
   * true to stay mounted — the component then MUST hide its dialogs while `locked` is true (so unsaved input survives).
   */
  keepMountedWhileLocked?: boolean
}

export const overlays = createRegistry<OverlayDef>('overlay', (o) => o.id, (o) => o.order ?? 100)

export function registerOverlay(def: OverlayDef): () => void {
  return overlays.register(def)
}

// ---------------------------------------------------------------------------------------------------------------------
// Misc
// ---------------------------------------------------------------------------------------------------------------------

/** Render helper type for features that contribute small React fragments. */
export type Renderable = ReactNode | (() => ReactNode)
