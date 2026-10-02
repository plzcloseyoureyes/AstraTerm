/*
 * Built-in shell contributions: workspace/view/app commands, menu bar menus, ribbon buttons and status bar items.
 * Imported once for its side effects by AppShell. Features add their own contributions via the same registries.
 */
import {
  AppWindow,
  Bookmark,
  Columns2,
  Copy,
  FolderOpen,
  Fullscreen,
  Grid2x2,
  CircleQuestionMark,
  Info,
  Keyboard,
  KeyRound,
  LayoutDashboard,
  LayoutGrid,
  Lock,
  LockKeyhole,
  LockOpen,
  LogOut,
  Maximize2,
  Monitor,
  Moon,
  Network,
  PanelBottom,
  PanelLeft,
  PanelRight,
  PictureInPicture2,
  Plus,
  RotateCcw,
  Rows2,
  Search,
  Server,
  Settings,
  SquareSplitHorizontal,
  SquareTerminal,
  Sun,
  SunMoon,
  Terminal,
  Trash2,
  Waypoints,
  Wifi,
  WifiOff,
  Workflow,
  Wrench,
  X,
  Zap,
} from 'lucide-react'
import { toast } from 'sonner'
import {
  registerCommand,
  registerMenu,
  registerRibbonButton,
  registerStatusItem,
  sidebarPanels,
  tabKinds,
  type MenuItem,
} from '@/app/registry'
import { isCommandEnabled, runCommand } from '@/app/commands'
import { protocolIcon } from '@/app/protocols'
import { lockScreen, logout } from '@/app/session'
import { listConnections, recentConnections } from '@/api/connections'
import { isSessionRunning, useSessions } from '@/api/sessions'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import { lockVault } from '@/api/vault'
import type { Connection, RuntimeSession } from '@/api/types'
import { events, useEventsStore } from '@/lib/events'
import { toggleFullscreen } from '@/lib/hooks'
import { errorMessage, isMac } from '@/lib/utils'
import { setVaultLocked, useAppVersion, useAuthStore, useIsAdmin, useVaultLocked } from '@/stores/auth'
import { appearanceSettings, workspacesSettings } from '@/stores/settings'
import { focusQuickConnect, openPalette, requestVaultUnlock, setAboutOpen, setAppMenuOpen, setDrawerOpen, useUIStore } from '@/stores/ui'
import {
  activateTabIndex,
  arrangeLayout,
  closeTab,
  cycleTab,
  dockTab,
  duplicateTab,
  float,
  focusTab,
  listTabs,
  popout,
  deleteWorkspace,
  reopenClosed,
  restoreWorkspace,
  saveWorkspace,
  splitActive,
  toggleMaximize,
  useWorkspaceStore,
} from '@/stores/workspace'
import { StatusBarItem } from './StatusBar'
import { showSidebarPanel, toggleSidebarCollapsed } from './Sidebar'
import { promptRenameTab } from './workspace/tabMenu'
import { prompt } from '@/components/ui/dialog-host'

const hasTabs = () => useWorkspaceStore.getState().tabs.length > 0
const hasActive = () => !!useWorkspaceStore.getState().activeTabId
const isAuthenticated = () => useAuthStore.getState().status === 'authenticated'

/** tabId from command args ({tabId}) or the active tab. */
function targetTab(args: unknown): string | undefined {
  if (args && typeof args === 'object' && typeof (args as { tabId?: unknown }).tabId === 'string') return (args as { tabId: string }).tabId
  return useWorkspaceStore.getState().activeTabId ?? undefined
}

// ---------------------------------------------------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------------------------------------------------

registerCommand({
  id: 'palette.open',
  title: 'Command Palette',
  category: 'View',
  icon: Search,
  keybinding: ['$mod+k', '$mod+Shift+p'],
  essential: ['$mod+Shift+p'],
  global: true,
  run: () => openPalette('all'),
})
registerCommand({
  id: 'palette.connections',
  title: 'Go to Session…',
  category: 'Sessions',
  icon: Search,
  keybinding: '$mod+Shift+o',
  global: true,
  run: () => openPalette('connections'),
})
registerCommand({
  id: 'palette.tabs',
  title: 'Go to Tab…',
  category: 'View',
  icon: AppWindow,
  keybinding: '$mod+Shift+e',
  global: true,
  run: () => openPalette('tabs'),
})

registerCommand({
  id: 'quickConnect.focus',
  title: 'Quick Connect',
  category: 'Sessions',
  icon: Zap,
  keybinding: 'Control+Shift+q',
  global: true,
  run: () => {
    const a = appearanceSettings.get()
    if (!(a.showMenuBar || a.showRibbon) || window.matchMedia('(max-width: 767px)').matches) openPalette('connections')
    else focusQuickConnect()
  },
})

registerCommand({
  id: 'app.menu',
  title: 'Open Menu',
  category: 'View',
  keybinding: 'F10',
  run: () => {
    // Title bar: the logo's app menu; classic menu bar: focus its first menu (arrow keys move between menus).
    if (document.querySelector('[aria-label="AstraTerm menu"]')) setAppMenuOpen(true)
    else document.querySelector<HTMLElement>('[role="menubar"] [role="menuitem"]')?.focus()
  },
})
registerCommand({
  id: 'app.lock',
  title: 'Lock Screen',
  category: 'Security',
  icon: Lock,
  keybinding: '$mod+Alt+KeyL',
  global: true,
  essential: true,
  when: isAuthenticated,
  run: () => lockScreen(),
})
registerCommand({
  id: 'app.logout',
  title: 'Sign Out',
  category: 'Account',
  icon: LogOut,
  when: isAuthenticated,
  run: () => logout(),
})
registerCommand({
  id: 'app.about',
  title: 'About AstraTerm',
  category: 'Help',
  icon: Info,
  run: () => setAboutOpen(true),
})
registerCommand({
  id: 'help.shortcuts',
  title: 'Keyboard Shortcuts',
  category: 'Help',
  icon: Keyboard,
  run: async () => {
    if (!(await runCommand('settings.open', { section: 'keyboard' }))) toast.info('Settings are not available')
  },
})

registerCommand({
  id: 'vault.unlock',
  title: 'Unlock Vault',
  category: 'Security',
  icon: LockOpen,
  when: () => !!useAuthStore.getState().state?.vaultLocked,
  run: () => requestVaultUnlock().then(() => undefined),
})
registerCommand({
  id: 'vault.lock',
  title: 'Lock Vault',
  category: 'Security',
  icon: LockKeyhole,
  when: () => {
    const st = useAuthStore.getState()
    return !!st.state?.vaultHasMasterPassword && !st.state.vaultLocked && st.user?.role === 'admin'
  },
  run: async () => {
    await lockVault()
    setVaultLocked(true)
    toast.success('Vault locked')
  },
})

// --- workspace ---------------------------------------------------------------------------------------------------------

registerCommand({
  id: 'workspace.closeTab',
  title: 'Close Tab',
  category: 'Workspace',
  icon: X,
  // Ctrl+Shift+W as in other terminal apps on Windows / Linux, where the shell keeps Ctrl+W (delete word).
  keybinding: ['$mod+w', 'Alt+w', 'Control+Alt+q', ...(isMac ? [] : ['Control+Shift+w'])],
  essential: true,
  when: hasActive,
  run: ({ args }) => {
    const id = targetTab(args)
    if (id) void closeTab(id)
  },
})
registerCommand({
  id: 'workspace.reopenClosed',
  title: 'Reopen Closed Tab',
  category: 'Workspace',
  icon: RotateCcw,
  keybinding: ['$mod+Shift+t', 'Alt+Shift+t'],
  when: () => useWorkspaceStore.getState().closed.length > 0,
  run: ({ args }) => {
    const index = args && typeof args === 'object' && typeof (args as { index?: unknown }).index === 'number' ? (args as { index: number }).index : 0
    void reopenClosed(index)
  },
})
registerCommand({
  id: 'workspace.nextTab',
  title: 'Next Tab',
  category: 'Workspace',
  keybinding: ['Control+Tab', 'Control+PageDown', 'Control+Alt+ArrowRight'],
  essential: true,
  when: hasTabs,
  run: () => cycleTab(1),
})
registerCommand({
  id: 'workspace.prevTab',
  title: 'Previous Tab',
  category: 'Workspace',
  keybinding: ['Control+Shift+Tab', 'Control+PageUp', 'Control+Alt+ArrowLeft'],
  essential: true,
  when: hasTabs,
  run: () => cycleTab(-1),
})
for (let n = 1; n <= 9; n++) {
  registerCommand({
    id: `workspace.goToTab${n}`,
    title: `Go to Tab ${n}`,
    category: 'Workspace',
    hidden: true,
    keybinding: `Control+Alt+F${n}`,
    essential: true,
    run: () => activateTabIndex(n),
  })
}
registerCommand({
  id: 'workspace.split.right',
  title: 'Split Right',
  category: 'Workspace',
  icon: PanelRight,
  keybinding: '$mod+Backslash',
  when: hasActive,
  run: ({ args }) => splitActive('right', targetTab(args)),
})
registerCommand({
  id: 'workspace.split.below',
  title: 'Split Down',
  category: 'Workspace',
  icon: PanelBottom,
  keybinding: '$mod+Shift+Backslash',
  when: hasActive,
  run: ({ args }) => splitActive('below', targetTab(args)),
})
registerCommand({
  id: 'workspace.layout.single',
  title: 'Layout: Single',
  category: 'Workspace',
  icon: SquareTerminal,
  keybinding: 'Control+Alt+1',
  when: hasTabs,
  run: () => arrangeLayout('single'),
})
registerCommand({
  id: 'workspace.layout.columns',
  title: 'Layout: Two Columns',
  category: 'Workspace',
  icon: Columns2,
  keybinding: 'Control+Alt+2',
  when: hasTabs,
  run: () => arrangeLayout('columns'),
})
registerCommand({
  id: 'workspace.layout.rows',
  title: 'Layout: Two Rows',
  category: 'Workspace',
  icon: Rows2,
  keybinding: 'Control+Alt+3',
  when: hasTabs,
  run: () => arrangeLayout('rows'),
})
registerCommand({
  id: 'workspace.layout.grid',
  title: 'Layout: 2×2 Grid',
  category: 'Workspace',
  icon: Grid2x2,
  keybinding: 'Control+Alt+4',
  when: hasTabs,
  run: () => arrangeLayout('grid'),
})
registerCommand({
  id: 'workspace.duplicateTab',
  title: 'Duplicate Tab',
  category: 'Workspace',
  icon: Copy,
  keybinding: 'Control+Shift+u',
  when: hasActive,
  run: ({ args }) => duplicateTab(targetTab(args)),
})
registerCommand({
  id: 'workspace.renameTab',
  title: 'Rename Tab…',
  category: 'Workspace',
  when: hasActive,
  run: ({ args }) => {
    const id = targetTab(args)
    const tab = listTabs().find((t) => t.id === id)
    if (tab) void promptRenameTab(tab.id, tab.title)
  },
})
registerCommand({
  id: 'workspace.popout',
  title: 'Move Tab to New Window',
  category: 'Workspace',
  icon: AppWindow,
  keybinding: 'Control+Shift+d',
  when: hasActive,
  run: ({ args }) => popout(targetTab(args)).then(() => undefined),
})
registerCommand({
  id: 'workspace.float',
  title: 'Float Tab',
  category: 'Workspace',
  icon: PictureInPicture2,
  when: hasActive,
  run: ({ args }) => float(targetTab(args)),
})
registerCommand({
  id: 'workspace.dock',
  title: 'Dock Tab into Grid',
  category: 'Workspace',
  when: hasActive,
  run: ({ args }) => dockTab(targetTab(args)),
})
registerCommand({
  id: 'workspace.maximize',
  title: 'Toggle Maximize Tab Group',
  category: 'Workspace',
  icon: Maximize2,
  keybinding: '$mod+Shift+Enter',
  when: hasActive,
  run: ({ args }) => toggleMaximize(targetTab(args)),
})

// --- saved workspaces (named layouts) ------------------------------------------------------------------------------------

registerCommand({
  id: 'workspace.save',
  title: 'Save Workspace…',
  category: 'Workspace',
  icon: Bookmark,
  description: 'Save the open tabs and their arrangement under a name, to restore later',
  keywords: ['layout', 'save layout', 'session set'],
  when: hasTabs,
  run: async ({ args }) => {
    const given = args && typeof args === 'object' && typeof (args as { name?: unknown }).name === 'string' ? (args as { name: string }).name : null
    const name =
      given ??
      (await prompt({
        title: 'Save workspace',
        label: 'Name',
        description: 'The open tabs, splits and floating windows. Restoring reconnects the sessions.',
        placeholder: 'e.g. Production triage',
        confirmLabel: 'Save',
      }))
    if (!name?.trim()) return
    const saved = saveWorkspace(name)
    if (saved) toast.success(`Saved workspace “${saved.name}”`, { description: `${saved.tabs.length} tab${saved.tabs.length === 1 ? '' : 's'}` })
  },
})
registerCommand<{ id: string }>({
  id: 'workspace.restore',
  title: 'Restore Workspace',
  category: 'Workspace',
  hidden: true,
  run: ({ args }) => {
    if (args?.id) void restoreWorkspace(args.id)
  },
})
registerCommand<{ id: string }>({
  id: 'workspace.delete',
  title: 'Delete Saved Workspace',
  category: 'Workspace',
  hidden: true,
  run: ({ args }) => {
    const w = workspacesSettings.get().saved.find((x) => x.id === args?.id)
    if (!w) return
    deleteWorkspace(w.id)
    toast(`Deleted workspace “${w.name}”`, {
      action: { label: 'Undo', onClick: () => workspacesSettings.set((s) => ({ saved: [w, ...s.saved.filter((x) => x.id !== w.id)] })) },
    })
  },
})

// One palette entry per saved workspace ("Workspace: Production triage"), kept in sync with the saved list.
const workspaceCommands = new Map<string, () => void>()
function syncWorkspaceCommands() {
  const saved = workspacesSettings.get().saved
  const live = new Set(saved.map((w) => `${w.id}\u0000${w.name}`))
  for (const [key, unregister] of workspaceCommands) {
    if (!live.has(key)) {
      unregister()
      workspaceCommands.delete(key)
    }
  }
  for (const w of saved) {
    const key = `${w.id}\u0000${w.name}`
    if (workspaceCommands.has(key)) continue
    workspaceCommands.set(
      key,
      registerCommand({
        id: `workspace.open.${w.id}`,
        title: `Workspace: ${w.name}`,
        category: 'Workspace',
        icon: LayoutDashboard,
        description: w.tabs.map((t) => t.title).join(', '),
        run: () => void restoreWorkspace(w.id),
      }),
    )
  }
}
workspacesSettings.subscribe(syncWorkspaceCommands)
syncWorkspaceCommands()

function workspaceMenuItems(): MenuItem[] {
  const saved = workspacesSettings.get().saved
  const items: MenuItem[] = [{ label: 'Save current workspace…', icon: Bookmark, command: 'workspace.save' }]
  if (saved.length) {
    items.push({ type: 'separator' }, { type: 'label', label: 'Restore' })
    for (const w of saved) items.push({ id: `ws-${w.id}`, label: w.name, icon: LayoutDashboard, command: 'workspace.restore', args: { id: w.id } })
    items.push(
      { type: 'separator' },
      {
        type: 'submenu',
        label: 'Delete',
        icon: Trash2,
        items: () => saved.map((w) => ({ id: `wsdel-${w.id}`, label: w.name, command: 'workspace.delete', args: { id: w.id } })),
      },
    )
  }
  return items
}

// --- view --------------------------------------------------------------------------------------------------------------

const toggle = (key: 'showSidebar' | 'showRibbon' | 'showStatusBar' | 'showMenuBar' | 'ribbonCompact') => () =>
  appearanceSettings.set((a) => ({ [key]: !a[key] }))

registerCommand({
  id: 'view.toggleSidebar',
  title: 'Toggle Sidebar',
  category: 'View',
  icon: PanelLeft,
  keybinding: '$mod+Shift+b',
  run: () => {
    if (window.matchMedia('(max-width: 767px)').matches) {
      setDrawerOpen(!useUIStore.getState().drawerOpen)
      return
    }
    const a = appearanceSettings.get()
    if (!a.showSidebar) appearanceSettings.set({ showSidebar: true })
    else toggleSidebarCollapsed()
  },
})
registerCommand({ id: 'view.toggleRibbon', title: 'Toggle Toolbar', category: 'View', run: toggle('showRibbon') })
registerCommand({ id: 'view.toggleCompact', title: 'Toggle Compact Toolbar', category: 'View', run: toggle('ribbonCompact') })
registerCommand({ id: 'view.toggleStatusBar', title: 'Toggle Status Bar', category: 'View', run: toggle('showStatusBar') })
registerCommand({ id: 'view.toggleMenuBar', title: 'Toggle Title Bar', category: 'View', run: toggle('showMenuBar') })
registerCommand({
  id: 'view.toggleFullscreen',
  title: 'Toggle Full Screen',
  category: 'View',
  icon: Fullscreen,
  keybinding: 'F11',
  run: () => toggleFullscreen(),
})
registerCommand({
  id: 'view.toggleTheme',
  title: 'Toggle Light/Dark Theme',
  category: 'View',
  icon: SunMoon,
  run: () => {
    const cur = appearanceSettings.get().theme
    const dark = cur === 'system' ? window.matchMedia('(prefers-color-scheme: dark)').matches : cur === 'dark'
    appearanceSettings.set({ theme: dark ? 'light' : 'dark' })
  },
})
for (const [theme, title, icon] of [
  ['dark', 'Theme: Dark', Moon],
  ['light', 'Theme: Light', Sun],
  ['system', 'Theme: Follow System', Monitor],
] as const) {
  registerCommand({ id: `view.theme.${theme}`, title, category: 'View', icon, run: () => appearanceSettings.set({ theme }) })
}
registerCommand({
  id: 'view.zoomIn',
  title: 'Zoom In (UI)',
  category: 'View',
  run: () => appearanceSettings.set((a) => ({ uiScale: Math.min(1.5, Math.round((a.uiScale + 0.05) * 100) / 100) })),
})
registerCommand({
  id: 'view.zoomOut',
  title: 'Zoom Out (UI)',
  category: 'View',
  run: () => appearanceSettings.set((a) => ({ uiScale: Math.max(0.8, Math.round((a.uiScale - 0.05) * 100) / 100) })),
})
registerCommand({ id: 'view.zoomReset', title: 'Reset Zoom (UI)', category: 'View', run: () => appearanceSettings.set({ uiScale: 1 }) })

// One "Show <panel>" command per registered sidebar panel (kept in sync as features register panels).
const panelCommands = new Map<string, () => void>()
function syncPanelCommands() {
  const live = new Set<string>()
  for (const p of sidebarPanels.list()) {
    live.add(p.id)
    if (panelCommands.has(p.id)) continue
    panelCommands.set(
      p.id,
      registerCommand({ id: `sidebar.show.${p.id}`, title: `Show ${p.title}`, category: 'View', icon: p.icon, run: () => showSidebarPanel(p.id) }),
    )
  }
  for (const [id, unregister] of panelCommands) {
    if (!live.has(id)) {
      unregister()
      panelCommands.delete(id)
    }
  }
}
sidebarPanels.subscribe(syncPanelCommands)
syncPanelCommands()

// ---------------------------------------------------------------------------------------------------------------------
// Menu bar
// ---------------------------------------------------------------------------------------------------------------------

function connectionItems(list: Connection[]): MenuItem[] {
  return list.map((c) => ({
    id: `conn-${c.id}`,
    label: c.name,
    icon: protocolIcon(c.protocol),
    command: 'sessions.connect',
    args: { id: c.id },
  }))
}

function runningSessionItems(): MenuItem[] {
  const sessions = (queryClient.getQueryData<RuntimeSession[]>(queryKeys.sessions) ?? []).filter(isSessionRunning)
  return sessions.map((s) => ({
    id: `sess-${s.id}`,
    label: s.title || `${s.username ? `${s.username}@` : ''}${s.host ?? s.protocol}`,
    icon: protocolIcon(s.protocol),
    run: () => {
      const tab = listTabs().find((t) => (t.params as { sessionId?: string } | undefined)?.sessionId === s.id)
      if (tab) focusTab(tab.id)
      else if (isCommandEnabled('terminal.attach')) void runCommand('terminal.attach', { sessionId: s.id }, { source: 'menu' })
    },
  }))
}

function closedTabItems(): MenuItem[] {
  return useWorkspaceStore
    .getState()
    .closed.slice(0, 10)
    .map((c, i) => ({
      id: `closed-${i}`,
      label: c.title,
      icon: tabKinds.get(c.kind)?.icon,
      command: 'workspace.reopenClosed',
      args: { index: i },
    }))
}

registerMenu({
  menu: 'terminal',
  order: 100,
  items: () => [
    { label: 'New local terminal', icon: SquareTerminal, command: 'terminal.newLocal' },
    { label: 'New session…', icon: Plus, command: 'sessions.new' },
    { label: 'Quick connect', icon: Zap, command: 'quickConnect.focus' },
    { type: 'separator' },
    { label: 'Reopen closed tab', command: 'workspace.reopenClosed' },
    { type: 'submenu', label: 'Recently closed', items: closedTabItems },
    { type: 'separator' },
    { label: 'Close tab', command: 'workspace.closeTab' },
  ],
})

registerMenu({
  menu: 'sessions',
  order: 100,
  items: () => {
    const conns = queryClient.getQueryData<Connection[]>(queryKeys.connections) ?? []
    const favorites = conns.filter((c) => c.favorite).slice(0, 20)
    return [
      { label: 'New session…', icon: Plus, command: 'sessions.new' },
      { label: 'Quick connect', icon: Zap, command: 'quickConnect.focus' },
      { label: 'Go to session…', icon: Search, command: 'palette.connections' },
      { type: 'separator' },
      { type: 'submenu', label: 'Favorites', items: () => connectionItems(favorites) },
      { type: 'submenu', label: 'Recent', items: () => connectionItems(recentConnections(conns, 12)) },
      { type: 'submenu', label: 'Running sessions', items: runningSessionItems },
    ]
  },
})

registerMenu({
  menu: 'view',
  order: 100,
  items: () => {
    const a = appearanceSettings.get()
    return [
      { label: 'Title bar', checked: a.showMenuBar, command: 'view.toggleMenuBar' },
      { label: 'Toolbar', checked: a.showRibbon, command: 'view.toggleRibbon' },
      { label: 'Compact toolbar', checked: a.ribbonCompact, command: 'view.toggleCompact' },
      { label: 'Sidebar', checked: a.showSidebar, command: 'view.toggleSidebar' },
      { label: 'Status bar', checked: a.showStatusBar, command: 'view.toggleStatusBar' },
      { label: 'Full screen', checked: !!document.fullscreenElement, command: 'view.toggleFullscreen' },
      { type: 'separator' },
      {
        type: 'submenu',
        label: 'Theme',
        icon: SunMoon,
        items: () => [
          { label: 'Dark', checked: a.theme === 'dark', command: 'view.theme.dark' },
          { label: 'Light', checked: a.theme === 'light', command: 'view.theme.light' },
          { label: 'Follow system', checked: a.theme === 'system', command: 'view.theme.system' },
        ],
      },
      {
        type: 'submenu',
        label: 'Zoom',
        items: () => [
          { label: 'Zoom in', command: 'view.zoomIn' },
          { label: 'Zoom out', command: 'view.zoomOut' },
          { label: `Reset (${Math.round(a.uiScale * 100)}%)`, command: 'view.zoomReset' },
        ],
      },
      { type: 'separator' },
      { type: 'submenu', label: 'Workspaces', icon: LayoutDashboard, items: workspaceMenuItems },
      {
        type: 'submenu',
        label: 'Layout',
        icon: LayoutGrid,
        items: () => [
          { label: 'Single', command: 'workspace.layout.single' },
          { label: 'Two columns', command: 'workspace.layout.columns' },
          { label: 'Two rows', command: 'workspace.layout.rows' },
          { label: '2×2 grid', command: 'workspace.layout.grid' },
        ],
      },
      { label: 'Split right', command: 'workspace.split.right' },
      { label: 'Split down', command: 'workspace.split.below' },
      { label: 'Maximize tab group', command: 'workspace.maximize' },
      { label: 'Float tab', command: 'workspace.float' },
      { label: 'Move tab to new window', command: 'workspace.popout' },
      { type: 'separator' },
      { label: 'Command palette', command: 'palette.open' },
    ]
  },
})

registerMenu({
  menu: 'tools',
  order: 100,
  items: () => [
    { label: 'Tools', icon: Wrench, command: 'tools.open' },
    { label: 'Tunnels', icon: Waypoints, command: 'tunnels.open' },
    { label: 'SSH keys', icon: KeyRound, command: 'keys.open' },
    { label: 'Servers', icon: Server, command: 'servers.open' },
    { label: 'Local files', icon: FolderOpen, command: 'files.openLocal' },
    { label: 'Broadcast input', icon: Workflow, command: 'terminal.multiexec.toggle' },
  ],
})

registerMenu({
  menu: 'settings',
  order: 100,
  items: () => [
    { label: 'Settings', icon: Settings, command: 'settings.open' },
    { label: 'Keyboard shortcuts', icon: Keyboard, command: 'help.shortcuts' },
    { type: 'separator' },
    { label: 'Unlock vault', icon: LockOpen, command: 'vault.unlock' },
    { label: 'Lock vault', icon: LockKeyhole, command: 'vault.lock' },
    { label: 'Lock screen', icon: Lock, command: 'app.lock' },
    { type: 'separator' },
    { label: 'Sign out', icon: LogOut, command: 'app.logout' },
  ],
})

registerMenu({
  menu: 'help',
  order: 100,
  items: () => [
    { label: 'Command palette', icon: Search, command: 'palette.open' },
    { label: 'Keyboard shortcuts', icon: Keyboard, command: 'help.shortcuts' },
    { label: 'Home', command: 'app.home' },
    { type: 'separator' },
    { label: 'About AstraTerm', icon: Info, command: 'app.about' },
  ],
})

// ---------------------------------------------------------------------------------------------------------------------
// Ribbon (large-button toolbar)
// ---------------------------------------------------------------------------------------------------------------------

registerRibbonButton({ id: 'session', label: 'Session', icon: Terminal, order: 10, command: 'sessions.new', tooltip: 'New session' })
registerRibbonButton({ id: 'servers', label: 'Servers', icon: Server, order: 20, command: 'servers.open', placeholder: true })
registerRibbonButton({ id: 'tools', label: 'Tools', icon: Wrench, order: 30, command: 'tools.open', placeholder: true })
registerRibbonButton({
  id: 'sessions',
  label: 'Sessions',
  icon: FolderOpen,
  order: 40,
  placeholder: true,
  tooltip: 'Saved sessions',
  menu: () => {
    const conns = queryClient.getQueryData<Connection[]>(queryKeys.connections) ?? []
    if (!conns.length) void queryClient.prefetchQuery({ queryKey: queryKeys.connections, queryFn: listConnections })
    const favorites = conns.filter((c) => c.favorite)
    const recent = recentConnections(conns, 10)
    const items: MenuItem[] = [
      { label: 'New session…', icon: Plus, command: 'sessions.new' },
      { label: 'Go to session…', icon: Search, command: 'palette.connections' },
    ]
    if (favorites.length) items.push({ type: 'separator' }, { type: 'label', label: 'Favorites' }, ...connectionItems(favorites.slice(0, 15)))
    if (recent.length) items.push({ type: 'separator' }, { type: 'label', label: 'Recent' }, ...connectionItems(recent))
    if (conns.length > 0) items.push({ type: 'separator' }, { type: 'submenu', label: `All sessions (${conns.length})`, items: () => connectionItems(conns.slice(0, 200)) })
    return items
  },
})
registerRibbonButton({
  id: 'view',
  label: 'View',
  icon: Monitor,
  order: 50,
  menu: () => {
    const a = appearanceSettings.get()
    return [
      { label: 'Toolbar', checked: a.showRibbon, command: 'view.toggleRibbon' },
      { label: 'Compact toolbar', checked: a.ribbonCompact, command: 'view.toggleCompact' },
      { label: 'Sidebar', checked: a.showSidebar, command: 'view.toggleSidebar' },
      { label: 'Status bar', checked: a.showStatusBar, command: 'view.toggleStatusBar' },
      { label: 'Title bar', checked: a.showMenuBar, command: 'view.toggleMenuBar' },
      { type: 'separator' },
      { label: 'Full screen', checked: !!document.fullscreenElement, command: 'view.toggleFullscreen' },
      { label: 'Toggle light/dark', command: 'view.toggleTheme' },
    ]
  },
})
registerRibbonButton({
  id: 'split',
  label: 'Split',
  icon: SquareSplitHorizontal,
  order: 60,
  menu: () => [
    { label: 'Split right', icon: PanelRight, command: 'workspace.split.right' },
    { label: 'Split down', icon: PanelBottom, command: 'workspace.split.below' },
    { type: 'separator' },
    { label: 'Single', icon: SquareTerminal, command: 'workspace.layout.single' },
    { label: 'Two columns', icon: Columns2, command: 'workspace.layout.columns' },
    { label: 'Two rows', icon: Rows2, command: 'workspace.layout.rows' },
    { label: '2×2 grid', icon: Grid2x2, command: 'workspace.layout.grid' },
    { type: 'separator' },
    { type: 'submenu', label: 'Workspaces', icon: LayoutDashboard, items: workspaceMenuItems },
  ],
})
registerRibbonButton({ id: 'multiexec', label: 'Broadcast', tooltip: 'Broadcast input to several terminals', icon: Workflow, order: 70, command: 'terminal.multiexec.toggle' })
registerRibbonButton({ id: 'tunneling', label: 'Tunneling', icon: Waypoints, order: 80, command: 'tunnels.open', placeholder: true })
registerRibbonButton({ id: 'keys', label: 'Keys', icon: KeyRound, order: 90, command: 'keys.open', placeholder: true })
registerRibbonButton({ id: 'settings', label: 'Settings', icon: Settings, order: 100, command: 'settings.open' })
registerRibbonButton({
  id: 'help',
  label: 'Help',
  icon: CircleQuestionMark,
  order: 110,
  menu: () => [
    { label: 'Command palette', icon: Search, command: 'palette.open' },
    { label: 'Keyboard shortcuts', icon: Keyboard, command: 'help.shortcuts' },
    { label: 'Home', command: 'app.home' },
    { type: 'separator' },
    { label: 'About AstraTerm', icon: Info, command: 'app.about' },
  ],
})

// ---------------------------------------------------------------------------------------------------------------------
// Status bar
// ---------------------------------------------------------------------------------------------------------------------

function EventsStatusItem() {
  const status = useEventsStore((s) => s.status)
  const failures = useEventsStore((s) => s.failures)
  if (status === 'open') {
    return <StatusBarItem icon={Wifi} tone="muted" tooltip="Connected to the AstraTerm server (live updates)" aria-label="Server connected" />
  }
  const connecting = status === 'connecting'
  return (
    <StatusBarItem
      icon={WifiOff}
      tone={connecting ? 'warning' : 'danger'}
      tooltip={connecting ? 'Connecting to the server…' : `Disconnected from the server${failures ? ` (${failures} failed attempts)` : ''} — click to retry`}
      onClick={() => events.reconnectNow()}
      aria-label="Reconnect to server"
    >
      {connecting ? 'Connecting…' : 'Offline'}
    </StatusBarItem>
  )
}

function SessionsCountItem() {
  const { data } = useSessions()
  const running = data?.filter(isSessionRunning) ?? []
  const connected = running.filter((s) => s.state === 'connected').length
  return (
    <StatusBarItem
      icon={Network}
      tone="muted"
      tooltip={`${running.length} running session${running.length === 1 ? '' : 's'} (${connected} connected)`}
      onClick={() => openPalette('tabs')}
      aria-label="Running sessions"
    >
      {/* The count, with a word on wider screens: "2 sessions". */}
      <span className="tabular-nums">{running.length}</span>
      <span className="hidden lg:inline"> {running.length === 1 ? 'session' : 'sessions'}</span>
    </StatusBarItem>
  )
}

function VaultStatusItem() {
  const locked = useVaultLocked()
  const isAdmin = useIsAdmin()
  const hasMaster = useAuthStore((s) => !!s.state?.vaultHasMasterPassword)
  if (!hasMaster && !locked) return null
  if (locked) {
    return (
      <StatusBarItem icon={LockKeyhole} tone="warning" tooltip="Vault locked — click to unlock" onClick={() => void requestVaultUnlock()} aria-label="Unlock vault">
        Vault locked
      </StatusBarItem>
    )
  }
  return (
    <StatusBarItem
      icon={LockOpen}
      tone="muted"
      tooltip={isAdmin ? 'Vault unlocked — click to lock' : 'Vault unlocked'}
      onClick={
        isAdmin
          ? async () => {
              try {
                await lockVault()
                setVaultLocked(true)
                toast.success('Vault locked')
              } catch (err) {
                toast.error('Could not lock the vault', { description: errorMessage(err) })
              }
            }
          : undefined
      }
      aria-label="Vault status"
    />
  )
}

function VersionItem() {
  const version = useAppVersion()
  return (
    <StatusBarItem tone="muted" tooltip="About AstraTerm" onClick={() => setAboutOpen(true)} aria-label="About AstraTerm">
      {version ? `v${version.replace(/^v/, '')}` : 'AstraTerm'}
    </StatusBarItem>
  )
}

registerStatusItem({ id: 'events', align: 'left', order: 0, component: EventsStatusItem })
registerStatusItem({ id: 'sessions', align: 'left', order: 10, component: SessionsCountItem })
registerStatusItem({ id: 'vault', align: 'right', order: 900, component: VaultStatusItem })
registerStatusItem({ id: 'version', align: 'right', order: 1000, component: VersionItem })
