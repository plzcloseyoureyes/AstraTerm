/*
 * Sessions feature (F1b): saved-session manager — sidebar tree, session editor, folders, quick connect, and the
 * sessions.* commands / menus. Heavy UI (panel, editor, dialogs) is lazy-loaded; this module only registers.
 *
 * Commands: sessions.new {folderId?, protocol?, initial?} · sessions.edit {id, tab?} · sessions.connect {id, position?}
 *   · sessions.quickConnect (raw text or {text}) · sessions.focusSearch · sessions.newFolder {parentId?}
 *   · sessions.duplicate {id} · sessions.delete {id | ids} · sessions.reveal {id} · sessions.connectFolder {folderId}
 *   · sessions.expandAll · sessions.collapseAll
 */
import { lazy } from 'react'
import { ChevronsDownUp, ChevronsUpDown, FolderOpen, FolderPlus, FolderTree, Pencil, Plus, Search } from 'lucide-react'
import {
  registerCommand,
  registerContextMenu,
  registerMenu,
  registerRibbonButton,
  registerSettingsSection,
  registerSidebarPanel,
  ribbonButtons,
  type RibbonButtonDef,
} from '@/app/registry'
import { runCommand } from '@/app/commands'
import { isPlainObject } from '@/lib/utils'
import './editors'
import { cachedConnections, connectFolder, currentIndex, deleteConnections, duplicateConnection, findConnection } from './actions'
import { connectTo } from './connect'
import { installDialogHost } from './dialogs/host'
import { openFolderDialog, openSessionEditor } from './dialogs/store'
import { menubarSessionsItems, ribbonSessionsMenu, sessionNodeItems } from './menus'
import { connNodeId, folderNodeId, parseNodeId } from './model'
import { focusSessionSearch, getTreeController, revealNode, useTreeFocus } from './panel/controller'
import { setPanelFilters } from './panel/state'
import { runQuickConnect } from './quickconnect'
import { sessionsSettings } from './settings'
import type { ConnectPosition, NewSessionArgs } from './types'

installDialogHost()

// ---------------------------------------------------------------------------------------------------------------------
// Sidebar panel
// ---------------------------------------------------------------------------------------------------------------------

registerSidebarPanel({
  id: 'sessions',
  title: 'Sessions',
  icon: FolderTree,
  order: 1,
  component: lazy(() => import('./panel/SessionsPanel')),
})

// ---------------------------------------------------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------------------------------------------------

function argString(args: unknown, key: string): string | undefined {
  if (typeof args === 'string') return args || undefined
  if (isPlainObject(args) && typeof args[key] === 'string' && args[key]) return args[key] as string
  return undefined
}

/** Folder of the tree's focused node (new items are created next to it). */
function focusedFolder(): string | null {
  const nodeId = useTreeFocus.getState().nodeId
  if (!nodeId) return null
  const index = currentIndex()
  const p = parseNodeId(nodeId)
  if (p.kind === 'folder') return index.folders.has(p.id) ? p.id : null
  if (p.kind === 'connection') {
    const folderId = index.connections.get(p.id)?.folderId
    return folderId && index.folders.has(folderId) ? folderId : null
  }
  return null
}

function focusedConnectionId(): string | undefined {
  const nodeId = useTreeFocus.getState().nodeId
  const p = nodeId ? parseNodeId(nodeId) : undefined
  return p?.kind === 'connection' ? p.id : undefined
}

const POSITIONS: ConnectPosition[] = ['tab', 'right', 'below', 'window']

// ---------------------------------------------------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------------------------------------------------

registerCommand<NewSessionArgs | undefined>({
  id: 'sessions.new',
  title: 'New Session…',
  category: 'Sessions',
  icon: Plus,
  keybinding: ['$mod+Shift+n', 'Alt+Shift+KeyN'],
  global: true,
  keywords: ['create', 'add', 'connection', 'bookmark', 'ssh', 'rdp', 'vnc'],
  run: ({ args }) => {
    const a = isPlainObject(args) ? (args as NewSessionArgs) : undefined
    openSessionEditor({
      mode: 'create',
      folderId: a?.folderId !== undefined ? a.folderId : focusedFolder(),
      protocol: a?.protocol,
      initial: a?.initial,
    })
  },
})

registerCommand<{ id?: string; tab?: string } | string | undefined>({
  id: 'sessions.edit',
  title: 'Edit Session…',
  category: 'Sessions',
  icon: Pencil,
  hidden: true,
  run: ({ args }) => {
    const id = argString(args, 'id') ?? focusedConnectionId()
    if (!id) throw new Error('Select a saved session first')
    const tab = isPlainObject(args) && typeof args.tab === 'string' ? args.tab : undefined
    openSessionEditor({ mode: 'edit', connectionId: id, tab })
  },
})

registerCommand<{ id: string; position?: ConnectPosition } | string>({
  id: 'sessions.connect',
  title: 'Connect to Session',
  category: 'Sessions',
  hidden: true,
  run: async ({ args }) => {
    const id = argString(args, 'id')
    if (!id) throw new Error('No session was specified')
    const pos = isPlainObject(args) && POSITIONS.includes(args.position as ConnectPosition) ? (args.position as ConnectPosition) : 'tab'
    await connectTo(findConnection(id) ?? id, pos)
  },
})

registerCommand({
  id: 'sessions.quickConnect',
  title: 'Quick Connect',
  category: 'Sessions',
  hidden: true,
  run: ({ args }) => runQuickConnect(args),
})

registerCommand({
  id: 'sessions.focusSearch',
  title: 'Find Session…',
  category: 'Sessions',
  icon: Search,
  keywords: ['search', 'filter', 'tree', 'sidebar'],
  run: ({ args }) => focusSessionSearch(argString(args, 'text')),
})

registerCommand<{ parentId?: string | null } | undefined>({
  id: 'sessions.newFolder',
  title: 'New Session Folder…',
  category: 'Sessions',
  icon: FolderPlus,
  run: ({ args }) => {
    const parentId = isPlainObject(args) && 'parentId' in args ? ((args.parentId as string | null | undefined) ?? null) : focusedFolder()
    void openFolderDialog({ mode: 'create', parentId })
  },
})

registerCommand<{ id: string } | string>({
  id: 'sessions.duplicate',
  title: 'Duplicate Session',
  category: 'Sessions',
  hidden: true,
  run: async ({ args }) => {
    const id = argString(args, 'id') ?? focusedConnectionId()
    const c = id ? findConnection(id) : undefined
    if (!c) throw new Error('Select a saved session first')
    await duplicateConnection(c)
  },
})

registerCommand<{ id?: string; ids?: string[] } | string>({
  id: 'sessions.delete',
  title: 'Delete Session…',
  category: 'Sessions',
  hidden: true,
  run: async ({ args }) => {
    const ids = isPlainObject(args) && Array.isArray(args.ids) ? (args.ids as string[]) : [argString(args, 'id') ?? focusedConnectionId()].filter(Boolean)
    const all = cachedConnections()
    const conns = all.filter((c) => ids.includes(c.id))
    if (!conns.length) throw new Error('Select a saved session first')
    await deleteConnections(conns)
  },
})

registerCommand<{ id: string } | string>({
  id: 'sessions.reveal',
  title: 'Show in Session Tree',
  category: 'Sessions',
  hidden: true,
  run: ({ args }) => {
    const id = argString(args, 'id')
    if (!id) return
    focusSessionSearch()
    revealNode(connNodeId(id))
  },
})

registerCommand<{ folderId: string } | string>({
  id: 'sessions.revealFolder',
  title: 'Show Folder in Session Tree',
  category: 'Sessions',
  hidden: true,
  run: ({ args }) => {
    const id = argString(args, 'folderId')
    if (!id) return
    void runCommand('sidebar.show.sessions', undefined, { source: 'api' })
    revealNode(folderNodeId(id))
  },
})

registerCommand<{ tag: string } | string>({
  id: 'sessions.filterByTag',
  title: 'Show Sessions with Tag',
  category: 'Sessions',
  hidden: true,
  run: ({ args }) => {
    const tag = argString(args, 'tag')
    if (!tag) return
    sessionsSettings.set({ showFilters: true })
    setPanelFilters({ tags: [tag] })
    void runCommand('sidebar.show.sessions', undefined, { source: 'api' })
  },
})

registerCommand<{ folderId: string } | string>({
  id: 'sessions.connectFolder',
  title: 'Connect All Sessions in Folder',
  category: 'Sessions',
  hidden: true,
  run: async ({ args }) => {
    const id = argString(args, 'folderId')
    if (id) await connectFolder(id)
  },
})

registerCommand({
  id: 'sessions.expandAll',
  title: 'Expand All Session Folders',
  category: 'Sessions',
  icon: ChevronsUpDown,
  when: () => !!getTreeController(),
  run: () => getTreeController()?.expandAll(),
})

registerCommand({
  id: 'sessions.collapseAll',
  title: 'Collapse All Session Folders',
  category: 'Sessions',
  icon: ChevronsDownUp,
  when: () => !!getTreeController(),
  run: () => getTreeController()?.collapseAll(),
})

// ---------------------------------------------------------------------------------------------------------------------
// Menus
// ---------------------------------------------------------------------------------------------------------------------

registerContextMenu({ id: 'sessions.builtin', target: 'session-node', order: 0, items: sessionNodeItems })

/** Tabs showing a saved connection (params.connectionId) get "Edit session" / "Show in session tree". */
registerContextMenu({
  id: 'sessions.tab',
  target: 'tab',
  order: 60,
  items: (ctx) => {
    const id = isPlainObject(ctx.params) && typeof ctx.params.connectionId === 'string' ? ctx.params.connectionId : undefined
    if (!id || !findConnection(id)) return []
    return [
      { label: 'Edit session…', icon: Pencil, command: 'sessions.edit', args: { id } },
      { label: 'Show in session tree', icon: FolderTree, command: 'sessions.reveal', args: { id } },
    ]
  },
})

registerMenu({ id: 'sessions.menubar', menu: 'sessions', order: 150, items: menubarSessionsItems })

// The "Sessions" toolbar drop-down (folders as sub-menus). The shell registers a simpler placeholder under the
// same id when it loads after this module; keep ours registered.
const sessionsRibbon: RibbonButtonDef = {
  id: 'sessions',
  label: 'Sessions',
  icon: FolderOpen,
  order: 40,
  tooltip: 'Saved sessions',
  menu: ribbonSessionsMenu,
}
function ensureSessionsRibbon(): void {
  if (ribbonButtons.get('sessions') !== sessionsRibbon) registerRibbonButton(sessionsRibbon)
}
ribbonButtons.subscribe(ensureSessionsRibbon)
ensureSessionsRibbon()

// ---------------------------------------------------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------------------------------------------------

registerSettingsSection({
  id: 'sessions',
  title: 'Sessions',
  icon: FolderTree,
  order: 25,
  group: 'connections',
  keywords: ['session', 'tree', 'folder', 'sort', 'favorites', 'recent', 'quick connect', 'history', 'connect all'],
  component: lazy(() => import('./SettingsSection')),
})
