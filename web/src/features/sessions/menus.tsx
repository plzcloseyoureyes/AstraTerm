/*
 * Menu contributions: the built-in `session-node` context menu (UI-11), the ribbon "Sessions" drop-down (folders as
 * sub-menus) and the menu bar "Sessions" additions.
 */
import {
  AppWindow,
  Clipboard,
  Copy,
  Eye,
  FolderInput,
  FolderOpen,
  FolderPlus,
  Import,
  Palette,
  PanelBottom,
  PanelRight,
  Pencil,
  Play,
  Plus,
  Search,
  SlidersHorizontal,
  Star,
  StarOff,
  Tag,
  Terminal,
  Trash,
  UserRoundCog,
  Zap,
  ChevronsDownUp,
  ChevronsUpDown,
} from 'lucide-react'
import { listConnections } from '@/api/connections'
import { listFolders } from '@/api/folders'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { Connection, Folder } from '@/api/types'
import type { IconType, MenuItem, SessionNodeContext } from '@/app/registry'
import { useAuthStore } from '@/stores/auth'
import {
  addTag,
  addTagInteractive,
  cachedFolders,
  commandLineFor,
  connectFolder,
  copyToClipboard,
  currentIndex,
  deleteConnections,
  deleteFolder,
  duplicateConnection,
  moveItems,
  newFolderIn,
  newSessionIn,
  removeTag,
  renameNodeInteractive,
  setConnectionColor,
  setConnectionsColor,
  setFolderColor,
  toggleFavorite,
} from './actions'
import { connectMany, connectSafely } from './connect'
import { openFolderDialog, openSessionEditor } from './dialogs/store'
import { ConnectionIcon, FolderIcon, NAMED_COLORS, colorDotIcon } from './icons'
import { canModify, collectTags, connectionTarget, flattenFolders, folderNodeId, isFolderInSubtree, subtreeConnections, type TreeIndex } from './model'
import { getTreeController } from './panel/controller'
import { getQuickConnectHistory } from './settings'

/** Context passed by the session tree (SessionNodeContext + extras). */
export interface SessionMenuContext extends SessionNodeContext {
  /** Selected folder ids (multi-selection). */
  folderIds?: string[]
  /** Right-click on the empty area of the tree. */
  area?: 'root'
}

const MAX_MENU_ITEMS = 150

// Menus mute every icon without a `text-*` class; coloured icons opt out with text-inherit (colour set by a wrapper).
function iconForConnection(c: Connection): IconType {
  return function Icon() {
    return <ConnectionIcon protocol={c.protocol} icon={c.icon} color={c.color} className={c.color ? 'size-3.5 text-inherit' : 'size-3.5'} />
  }
}

function iconForFolder(f: Folder): IconType {
  return function Icon() {
    return <FolderIcon icon={f.icon} color={f.color} className={f.color ? 'size-3.5 text-inherit' : 'size-3.5'} />
  }
}

const currentUser = () => useAuthStore.getState().user

// ---------------------------------------------------------------------------------------------------------------------
// Context menu
// ---------------------------------------------------------------------------------------------------------------------

function moveToItems(folderIds: string[], connectionIds: string[]): MenuItem[] {
  const folders = cachedFolders()
  const byId = new Map(folders.map((f) => [f.id, f]))
  const targets = flattenFolders(folders).filter((t) => !folderIds.some((id) => isFolderInSubtree(byId, id, t.folder.id)))
  const items: MenuItem[] = [{ label: 'Top level', icon: FolderInput, run: () => void moveItems(null, folderIds, connectionIds) }]
  for (const t of targets.slice(0, MAX_MENU_ITEMS)) {
    items.push({ label: t.path, icon: iconForFolder(t.folder), run: () => void moveItems(t.folder.id, folderIds, connectionIds) })
  }
  if (targets.length > MAX_MENU_ITEMS) items.push({ type: 'label', label: `…${targets.length - MAX_MENU_ITEMS} more (drag in the tree)` })
  return items
}

/** Colour choices; `current` null = a selection with different colours (nothing checked). */
function colorItems(current: string | undefined | null, apply: (c: string | undefined) => void): MenuItem[] {
  return [
    { label: 'None', checked: current === undefined || current === '', run: () => apply(undefined) },
    { type: 'separator' },
    ...NAMED_COLORS.map((c) => ({ label: c.name, icon: colorDotIcon(c.value), run: () => apply(c.value) })),
  ]
}

/** Tags submenu for one or more sessions: every known tag as a check item (checked = all of them have it; a mixed tag
 *  shows as unchecked and adds to the rest), plus "New tag…". */
function tagItems(conns: readonly Connection[]): MenuItem[] {
  const all = collectTags(cachedConnectionsForMenu())
  const items: MenuItem[] = all.map((t) => {
    const n = conns.filter((c) => (c.tags ?? []).some((x) => x.toLowerCase() === t.toLowerCase())).length
    const every = n === conns.length
    return {
      label: n > 0 && !every ? `${t} (${n} of ${conns.length})` : t,
      checked: every,
      run: () => void (every ? removeTag(conns, t) : addTag(conns, t)),
    }
  })
  if (items.length) items.push({ type: 'separator' })
  items.push({ label: 'New tag…', icon: Tag, run: () => void addTagInteractive(conns) })
  return items
}

function cachedConnectionsForMenu(): Connection[] {
  return queryClient.getQueryData<Connection[]>(queryKeys.connections) ?? []
}

function connectionItems(c: Connection): MenuItem[] {
  const user = currentUser()
  const writable = canModify(c, user)
  const target = connectionTarget(c)
  const cmdLine = commandLineFor(c)
  const hasFiles = c.protocol === 'ssh' || c.protocol === 'mosh'
  const copyItems: MenuItem[] = []
  if (c.host) copyItems.push({ label: 'Host', run: () => void copyToClipboard(c.host, 'Host') })
  if (c.host && c.username) copyItems.push({ label: 'user@host', run: () => void copyToClipboard(`${c.username}@${c.host}`, 'user@host') })
  if (target && target !== c.host && !(c.username && target === `${c.username}@${c.host}`)) {
    copyItems.push({ label: `Target (${target})`, run: () => void copyToClipboard(target, 'Target') })
  }
  if (cmdLine) copyItems.push({ label: `As ${c.protocol} command`, run: () => void copyToClipboard(cmdLine, 'Command') })
  copyItems.push({ label: 'Name', run: () => void copyToClipboard(c.name, 'Name') })

  return [
    { label: 'Connect', icon: Play, shortcut: 'Enter', run: () => void connectSafely(c) },
    { label: 'Connect in split right', icon: PanelRight, run: () => void connectSafely(c, 'right') },
    { label: 'Connect in split below', icon: PanelBottom, run: () => void connectSafely(c, 'below') },
    { label: 'Connect in new window', icon: AppWindow, run: () => void connectSafely(c, 'window') },
    ...(hasFiles ? [{ label: 'Open files (SFTP)', icon: FolderOpen, command: 'files.openForConnection', args: { connectionId: c.id } } as MenuItem] : []),
    { type: 'separator' },
    writable
      ? { label: 'Edit…', icon: Pencil, shortcut: 'Alt+Enter', run: () => openSessionEditor({ mode: 'edit', connectionId: c.id }) }
      : { label: 'View settings…', icon: Eye, run: () => openSessionEditor({ mode: 'edit', connectionId: c.id }) },
    { label: 'Duplicate', icon: Copy, run: () => void duplicateConnection(c) },
    { label: 'Rename', shortcut: 'F2', disabled: !writable, run: () => void renameNodeInteractive({ connection: c }) },
    {
      label: c.favorite ? 'Remove from favorites' : 'Add to favorites',
      icon: c.favorite ? StarOff : Star,
      disabled: !writable,
      run: () => void toggleFavorite([c]),
    },
    { type: 'submenu', label: 'Colour', icon: Palette, disabled: !writable, items: () => colorItems(c.color, (col) => void setConnectionColor(c, col)) },
    { type: 'submenu', label: 'Tags', icon: Tag, disabled: !writable, items: () => tagItems([c]) },
    { type: 'submenu', label: 'Copy', icon: Clipboard, items: copyItems },
    { type: 'separator' },
    { label: 'New session here…', icon: Plus, run: () => newSessionIn(c.folderId || null) },
    { label: 'New folder here…', icon: FolderPlus, run: () => void newFolderIn(c.folderId || null) },
    { type: 'submenu', label: 'Move to', icon: FolderInput, disabled: !writable, items: () => moveToItems([], [c.id]) },
    { type: 'separator' },
    { label: 'Delete…', icon: Trash, shortcut: 'Delete', danger: true, disabled: !writable, run: () => void deleteConnections([c]) },
  ]
}

function folderItems(f: Folder, index: TreeIndex): MenuItem[] {
  const user = currentUser()
  const writable = canModify(f, user)
  const count = subtreeConnections(index, folderNodeId(f.id)).length
  return [
    { label: count ? `Connect all (${count})` : 'Connect all', icon: Play, disabled: !count, run: () => void connectFolder(f.id) },
    { type: 'separator' },
    { label: 'New session here…', icon: Plus, run: () => newSessionIn(f.id) },
    { label: 'New folder here…', icon: FolderPlus, run: () => void newFolderIn(f.id) },
    { type: 'separator' },
    { label: 'Rename', shortcut: 'F2', disabled: !writable, run: () => void renameNodeInteractive({ folder: f }) },
    { label: writable ? 'Edit folder…' : 'Folder details…', icon: SlidersHorizontal, run: () => void openFolderDialog({ mode: 'edit', folderId: f.id }) },
    { type: 'submenu', label: 'Colour', icon: Palette, disabled: !writable, items: () => colorItems(f.color, (c) => void setFolderColor(f, c)) },
    { label: 'Icon…', disabled: !writable, run: () => void openFolderDialog({ mode: 'edit', folderId: f.id, focus: 'icon' }) },
    { type: 'submenu', label: 'Move to', icon: FolderInput, disabled: !writable, items: () => moveToItems([f.id], []) },
    { type: 'separator' },
    { label: 'Delete folder…', icon: Trash, shortcut: 'Delete', danger: true, disabled: !writable, run: () => void deleteFolder(f) },
  ]
}

function multiItems(connectionIds: string[], folderIds: string[], index: TreeIndex): MenuItem[] {
  const user = currentUser()
  const conns = connectionIds.map((id) => index.connections.get(id)).filter((c): c is Connection => !!c)
  const folders = folderIds.map((id) => index.folders.get(id)).filter((f): f is Folder => !!f)
  const writable = conns.every((c) => canModify(c, user)) && folders.every((f) => canModify(f, user))
  const allFav = conns.length > 0 && conns.every((c) => c.favorite)
  const total = conns.length + folders.length
  const items: MenuItem[] = []
  if (conns.length) items.push({ label: `Connect ${conns.length} session${conns.length === 1 ? '' : 's'}`, icon: Play, run: () => void connectMany(conns) })
  if (conns.length) {
    items.push({
      label: allFav ? 'Remove from favorites' : 'Add to favorites',
      icon: allFav ? StarOff : Star,
      disabled: !conns.every((c) => canModify(c, user)),
      run: () => void toggleFavorite(conns, !allFav),
    })
  }
  if (conns.length) {
    const connsWritable = conns.every((c) => canModify(c, user))
    const colors = new Set(conns.map((c) => c.color || ''))
    items.push(
      {
        type: 'submenu',
        label: 'Colour',
        icon: Palette,
        disabled: !connsWritable,
        items: () => colorItems(colors.size === 1 ? [...colors][0] || undefined : null, (col) => void setConnectionsColor(conns, col)),
      },
      { type: 'submenu', label: 'Tags', icon: Tag, disabled: !connsWritable, items: () => tagItems(conns) },
    )
  }
  items.push(
    { type: 'submenu', label: 'Move to', icon: FolderInput, disabled: !writable, items: () => moveToItems(folderIds, connectionIds) },
    { type: 'separator' },
    {
      label: `Delete ${total} item${total === 1 ? '' : 's'}…`,
      icon: Trash,
      shortcut: 'Delete',
      danger: true,
      disabled: !writable,
      run: async () => {
        if (conns.length && !(await deleteConnections(conns))) return
        for (const f of folders) if (!(await deleteFolder(f))) return
      },
    },
  )
  return items
}

function rootItems(): MenuItem[] {
  const ctl = getTreeController()
  return [
    { label: 'New session…', icon: Plus, run: () => newSessionIn(null) },
    { label: 'New folder…', icon: FolderPlus, run: () => void newFolderIn(null) },
    { type: 'separator' },
    { label: 'Expand all', icon: ChevronsUpDown, disabled: !ctl, run: () => ctl?.expandAll() },
    { label: 'Collapse all', icon: ChevronsDownUp, disabled: !ctl, run: () => ctl?.collapseAll() },
    { type: 'separator' },
    { label: 'Import sessions…', icon: Import, command: 'importer.open' },
  ]
}

/** Built-in items of the `session-node` context menu. */
export function sessionNodeItems(ctx: SessionMenuContext): MenuItem[] {
  const index = currentIndex()
  const connIds = ctx.selection ?? (ctx.connection ? [ctx.connection.id] : [])
  const folderIds = ctx.folderIds ?? (ctx.folderId ? [ctx.folderId] : [])
  if (connIds.length + folderIds.length > 1) return multiItems(connIds, folderIds, index)
  if (ctx.connection) return connectionItems(index.connections.get(ctx.connection.id) ?? ctx.connection)
  if (ctx.folderId) {
    const f = index.folders.get(ctx.folderId)
    return f ? folderItems(f, index) : []
  }
  return ctx.area === 'root' ? rootItems() : []
}

// ---------------------------------------------------------------------------------------------------------------------
// Ribbon "Sessions" drop-down
// ---------------------------------------------------------------------------------------------------------------------

function connectItem(c: Connection): MenuItem {
  return { id: `conn-${c.id}`, label: c.name, icon: iconForConnection(c), run: () => void connectSafely(c) }
}

function folderSubmenu(f: Folder, index: TreeIndex): MenuItem {
  return {
    type: 'submenu',
    label: f.name,
    icon: iconForFolder(f),
    items: () => {
      const nid = folderNodeId(f.id)
      const kids = index.children.get(nid) ?? []
      const items: MenuItem[] = []
      const total = index.counts.get(nid) ?? 0
      if (total > 0) items.push({ label: `Connect all (${total})`, icon: Play, run: () => void connectFolder(f.id) }, { type: 'separator' })
      for (const k of kids.slice(0, MAX_MENU_ITEMS)) {
        const n = index.nodes.get(k)
        if (n?.kind === 'folder') items.push(folderSubmenu(n.folder, index))
        else if (n?.kind === 'connection') items.push(connectItem(n.connection))
      }
      if (kids.length > MAX_MENU_ITEMS) items.push({ type: 'label', label: `…${kids.length - MAX_MENU_ITEMS} more in the Sessions panel` })
      if (!kids.length) items.push({ label: 'Empty folder', disabled: true })
      return items
    },
  }
}

export function ribbonSessionsMenu(): MenuItem[] {
  const conns = queryClient.getQueryData<Connection[]>(queryKeys.connections)
  const folders = queryClient.getQueryData<Folder[]>(queryKeys.folders)
  if (!conns) void queryClient.prefetchQuery({ queryKey: queryKeys.connections, queryFn: listConnections })
  if (!folders) void queryClient.prefetchQuery({ queryKey: queryKeys.folders, queryFn: listFolders })
  const index = currentIndex()
  const items: MenuItem[] = [
    { label: 'New session…', icon: Terminal, command: 'sessions.new' },
    { label: 'New folder…', icon: FolderPlus, command: 'sessions.newFolder' },
    { label: 'Quick connect', icon: Zap, command: 'quickConnect.focus' },
    { label: 'Find session…', icon: Search, command: 'sessions.focusSearch' },
  ]
  const favorites = [...index.connections.values()].filter((c) => c.favorite)
  if (favorites.length) {
    items.push({ type: 'separator' }, { type: 'label', label: 'Favorites' }, ...favorites.slice(0, 20).map(connectItem))
  }
  const history = getQuickConnectHistory()
  if (history.length) {
    items.push({
      type: 'submenu',
      label: 'Recent quick connects',
      icon: Zap,
      items: () => history.slice(0, 15).map((h) => ({ label: h, command: 'sessions.quickConnect', args: h })),
    })
  }
  const top = index.children.get('root') ?? []
  if (top.length) {
    items.push({ type: 'separator' }, { type: 'label', label: 'Sessions' })
    for (const k of top.slice(0, MAX_MENU_ITEMS)) {
      const n = index.nodes.get(k)
      if (n?.kind === 'folder') items.push(folderSubmenu(n.folder, index))
      else if (n?.kind === 'connection') items.push(connectItem(n.connection))
    }
    if (top.length > MAX_MENU_ITEMS) items.push({ type: 'label', label: `…${top.length - MAX_MENU_ITEMS} more in the Sessions panel` })
  } else if (conns && !conns.length) {
    items.push({ type: 'separator' }, { label: 'No saved sessions yet', disabled: true })
  }
  return items
}

// ---------------------------------------------------------------------------------------------------------------------
// Menu bar "Sessions" additions (the shell already lists new session / quick connect / favorites / recent)
// ---------------------------------------------------------------------------------------------------------------------

export function menubarSessionsItems(): MenuItem[] {
  return [
    { label: 'New folder…', icon: FolderPlus, command: 'sessions.newFolder' },
    { label: 'Find session…', icon: Search, command: 'sessions.focusSearch' },
    { type: 'separator' },
    { label: 'Import / export sessions…', icon: Import, command: 'importer.open' },
    { label: 'Manage identities…', icon: UserRoundCog, command: 'keys.identities' },
  ]
}
