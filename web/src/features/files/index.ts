/*
 * Files feature (files-ui; SPEC §10.3, RESEARCH FILE-1..5, 8, 9, 12, 13, 15..17, GFX-16):
 *
 *   sidebar panel "sftp"   the SFTP side panel: the files of the SSH session in the active terminal tab, following
 *                          the shell's folder; auto-shown when an SSH session connects (settings.files.autoShowPanel)
 *   tab kind "files"       {fsId?|connectionId?|sessionId?|local?|quick?, protocol?, path?, dual?, right?}: the same
 *                          browser in a tab, optional dual-pane commander (F5 copy, F6 move, F7 mkdir, F8 delete)
 *   transfer queue         bottom drawer (overlay) + status bar item + sidebar badge; uploads (chunked, resumable,
 *                          parallel) and server transfers
 *   dialogs                preview (images / PDF / media / text / Markdown), permissions, properties, checksum,
 *                          compress, search, folder compare, conflicts
 *
 * Commands: files.openForSession {sessionId, path?}, files.openForConnection {connectionId, path?},
 * files.openLocal {path?}, files.commander, files.transfers, files.toggleFollow, files.revealInPanel {sessionId, path?},
 * files.search. Context menus: `file` (built-in items, order 0), `terminal` ("Show in SFTP browser"), `session-node`
 * ("Open SFTP browser" for connections with a running session), `tab` (files tabs: dual pane).
 */
import { lazy } from 'react'
import { ArrowUpDown, Columns2, FolderOpen, FolderSearch, FolderSync, HardDrive } from 'lucide-react'
import { toast } from 'sonner'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { Connection, Protocol, RuntimeSession } from '@/api/types'
import { protocolIcon } from '@/app/protocols'
import {
  registerCommand,
  registerContextMenu,
  registerMenu,
  registerOverlay,
  registerSettingsSection,
  registerSidebarPanel,
  registerStatusItem,
  registerTabKind,
} from '@/app/registry'
import { useTerminalInfoStore } from '@/features/terminal/bus'
import type { TerminalTabParams } from '@/features/terminal/types'
import { isPlainObject } from '@/lib/utils'
import { activeTab, findTabs, focusTab, listTabs, updateTabParams } from '@/stores/workspace'
import { getActiveController } from './browser/controller'
import { builtinFileItems } from './browser/menu'
import { dropViewsWithPrefix } from './browser/viewStore'
import { FilesOverlay } from './dialogs/FilesOverlay'
import { installFsLifecycle, openFs, sourceKey } from './fsHandles'
import { FILES_TAB, openFilesTab, revealInPanel, SFTP_PANEL } from './open'
import { describePlace } from './place'
import { collectDrop, filesFromInput, planUpload, withSlowToast } from './upload'
import { installDropGuard } from './dnd'
import { installTerminalProbe } from './terminalProbe'
import { installAutoShow } from './sidebar/autoshow'
import { followDefault, installPanelTargetTracking, setFollow, usePanelState } from './sidebar/state'
import { installTransfersLifecycle, toggleTransfers } from './transfers/store'
import { TransfersBadge, TransfersStatusItem } from './transfers/TransferDrawer'
import type { FilesTabParams, FsSource } from './types'

installFsLifecycle()
installTransfersLifecycle()
installPanelTargetTracking()
installAutoShow()
installDropGuard()
installTerminalProbe()

// ---------------------------------------------------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------------------------------------------------

function cachedSessions(): RuntimeSession[] {
  return queryClient.getQueryData<RuntimeSession[]>(queryKeys.sessions) ?? []
}

function cachedConnections(): Connection[] {
  return queryClient.getQueryData<Connection[]>(queryKeys.connections) ?? []
}

function argString(args: unknown, key: string): string | undefined {
  if (typeof args === 'string') return key === 'path' ? undefined : args
  if (isPlainObject(args) && typeof args[key] === 'string' && args[key]) return args[key] as string
  return undefined
}

function sessionLabel(s: RuntimeSession | undefined): string {
  if (!s) return 'SSH session'
  const c = s.connectionId ? cachedConnections().find((x) => x.id === s.connectionId) : undefined
  return c?.name || s.title || `${s.username ? `${s.username}@` : ''}${s.host ?? ''}`
}

function tabTitle(p: FilesTabParams | undefined): string {
  if (!p) return 'Files'
  if (p.dual) return 'Commander'
  if (p.local) return 'Local files'
  if (p.connectionId) return cachedConnections().find((c) => c.id === p.connectionId)?.name ?? 'Files'
  if (p.sessionId) return `SFTP · ${sessionLabel(cachedSessions().find((s) => s.id === p.sessionId))}`
  if (p.quick) return p.quick.name || `${p.quick.username ? `${p.quick.username}@` : ''}${p.quick.host ?? ''}` || 'Files'
  return 'Files'
}

/** The SSH session shown by the SFTP panel (or the active terminal's). */
function panelSessionId(): string | undefined {
  const tabId = usePanelState.getState().tabId
  const tab = tabId ? listTabs().find((t) => t.id === tabId) : activeTab()
  const p = tab?.params as TerminalTabParams | undefined
  return tab?.kind === 'terminal' ? p?.sessionId : undefined
}

// ---------------------------------------------------------------------------------------------------------------------
// panel, tab kind, overlay, status bar, settings
// ---------------------------------------------------------------------------------------------------------------------

registerSidebarPanel({
  id: SFTP_PANEL,
  title: 'SFTP',
  icon: FolderSync,
  order: 2,
  component: lazy(() => import('./sidebar/SftpPanel')),
  badge: TransfersBadge,
})

registerTabKind<FilesTabParams>({
  kind: FILES_TAB,
  title: tabTitle,
  icon: FolderOpen,
  iconFor: (p) => (p?.dual ? Columns2 : p?.local ? HardDrive : p?.protocol ? protocolIcon(p.protocol) : undefined),
  component: lazy(() => import('./tab/FilesTab')),
  onClose: (tab) => dropViewsWithPrefix(`tab:${tab.id}:`),
})

registerOverlay({ id: 'files', component: FilesOverlay, order: 50 })

registerStatusItem({ id: 'transfers', align: 'right', order: 80, component: TransfersStatusItem })

registerSettingsSection({
  id: 'files',
  title: 'Files & SFTP',
  icon: FolderSync,
  order: 20,
  group: 'files',
  keywords: ['sftp', 'ssh-browser', 'files', 'upload', 'download', 'transfer', 'hidden files', 'partial uploads', 'astraterm-part', 'follow', 'folder', 'double-click', 'conflict', 'overwrite'],
  component: lazy(() => import('./SettingsSection')),
})

// ---------------------------------------------------------------------------------------------------------------------
// commands
// ---------------------------------------------------------------------------------------------------------------------

registerCommand<{ sessionId: string; path?: string } | string>({
  id: 'files.openForSession',
  title: 'Open Files of Session',
  category: 'Files',
  icon: FolderOpen,
  hidden: true,
  run: ({ args }) => {
    const sessionId = argString(args, 'sessionId')
    if (!sessionId) throw new Error('No session given')
    const s = cachedSessions().find((x) => x.id === sessionId)
    if (s && s.protocol !== 'ssh') {
      toast.info('Files can only be browsed for SSH sessions')
      return
    }
    openFilesTab({ kind: 'session', sessionId }, { path: argString(args, 'path'), protocol: 'ssh', title: `SFTP · ${sessionLabel(s)}` })
  },
})

registerCommand<{ connectionId: string; path?: string } | string>({
  id: 'files.openForConnection',
  title: 'Open Files of Saved Session',
  category: 'Files',
  icon: FolderOpen,
  hidden: true,
  run: ({ args }) => {
    const connectionId = argString(args, 'connectionId') ?? argString(args, 'id')
    if (!connectionId) throw new Error('No session given')
    const c = cachedConnections().find((x) => x.id === connectionId)
    openFilesTab({ kind: 'connection', connectionId }, { path: argString(args, 'path'), protocol: c?.protocol, title: c ? `${c.protocol === 'ssh' ? 'SFTP · ' : ''}${c.name}` : undefined })
  },
})

registerCommand<{ path?: string } | undefined>({
  id: 'files.openLocal',
  title: 'Local Files',
  category: 'Files',
  icon: HardDrive,
  keywords: ['browse', 'file manager', 'host'],
  description: 'Browse the files of the computer running AstraTerm',
  run: ({ args }) => {
    openFilesTab({ kind: 'local' }, { path: argString(args, 'path'), title: 'Local files' })
  },
})

registerCommand({
  id: 'files.commander',
  title: 'File Commander (Dual Pane)',
  category: 'Files',
  icon: Columns2,
  keywords: ['total commander', 'norton', 'two panes', 'copy between servers'],
  run: () => {
    const sid = panelSessionId()
    const left: FsSource = sid && cachedSessions().some((s) => s.id === sid && s.protocol === 'ssh') ? { kind: 'session', sessionId: sid } : { kind: 'local' }
    // Left: the SSH session of the SFTP panel (else the local host); right: the local host.
    openFilesTab(left, { dual: true, protocol: left.kind === 'session' ? ('ssh' as Protocol) : undefined })
  },
})

registerCommand({
  id: 'files.transfers',
  title: 'File Transfers',
  category: 'Files',
  icon: ArrowUpDown,
  keywords: ['queue', 'uploads', 'downloads', 'progress'],
  run: () => toggleTransfers(),
})

registerCommand({
  id: 'files.toggleFollow',
  title: 'Toggle “Follow Terminal Folder”',
  category: 'Files',
  icon: FolderSync,
  keywords: ['sftp', 'cwd', 'follow', 'ssh-browser'],
  when: () => !!panelSessionId(),
  run: () => {
    const sid = panelSessionId()
    if (!sid) return
    const choice = usePanelState.getState().follow[sid]
    const s = cachedSessions().find((x) => x.id === sid)
    const conn = s?.connectionId ? cachedConnections().find((c) => c.id === s.connectionId) : undefined
    const tab = listTabs().find((t) => (t.params as TerminalTabParams | undefined)?.sessionId === sid)
    const options = conn?.options ?? (tab?.params as TerminalTabParams | undefined)?.quick?.options
    const current = choice ?? followDefault(options)
    setFollow(sid, !current)
    toast.info(`Follow terminal folder ${current ? 'off' : 'on'}`)
  },
})

registerCommand<{ sessionId: string; path?: string }>({
  id: 'files.revealInPanel',
  title: 'Show in SFTP Browser',
  category: 'Files',
  icon: FolderSync,
  hidden: true,
  run: ({ args }) => {
    const sessionId = argString(args, 'sessionId') ?? panelSessionId()
    if (sessionId) revealInPanel(sessionId, argString(args, 'path'))
  },
})

/**
 * Upload browser files through the SSH-browser engine (chunked, resumable, in the transfer queue). For other features,
 * e.g. dropping files on a terminal (FILE-22): `{sessionId | connectionId | fsId | local, path, files?: File[] |
 * FileList, dataTransfer?: DataTransfer}`. A DataTransfer is read synchronously, so call it from the drop handler.
 */
registerCommand<{
  sessionId?: string
  connectionId?: string
  fsId?: string
  local?: boolean
  path: string
  files?: File[] | FileList
  dataTransfer?: DataTransfer
}>({
  id: 'files.upload',
  title: 'Upload Files',
  category: 'Files',
  hidden: true,
  run: async ({ args }) => {
    if (!args || typeof args.path !== 'string') throw new Error('files.upload needs {path}')
    const source: FsSource | null = args.sessionId
      ? { kind: 'session', sessionId: args.sessionId }
      : args.connectionId
        ? { kind: 'connection', connectionId: args.connectionId }
        : args.fsId
          ? { kind: 'handle', fsId: args.fsId }
          : args.local
            ? { kind: 'local' }
            : null
    if (!source) throw new Error('files.upload needs a file system (sessionId, connectionId, fsId or local)')
    // Snapshot a DataTransfer before the first await (it is only readable during the drop event).
    const dropped = args.dataTransfer ? withSlowToast(collectDrop(args.dataTransfer), 'Reading the dropped items…') : null
    const picked = args.files ? filesFromInput(args.files instanceof FileList ? args.files : listOf(args.files)) : null
    const handle = await openFs(source)
    const key = sourceKey(source)
    const place = describePlace(source, cachedSessions(), cachedConnections())
    const ctx = { key, handle, source, sessionId: place.sessionId, placeKey: place.placeKey, label: place.label, host: place.host }
    const { files, dirs } = dropped ? await dropped : (picked ?? { files: [], dirs: [] })
    await planUpload(ctx, args.path, files, dirs)
  },
})

function listOf(files: File[]): FileList {
  const dt = new DataTransfer()
  for (const f of files) dt.items.add(f)
  return dt.files
}

registerCommand({
  id: 'files.search',
  title: 'Search Files…',
  category: 'Files',
  icon: FolderSearch,
  keywords: ['find', 'mobafind', 'grep'],
  when: () => !!getActiveController()?.ctx,
  run: () => getActiveController()?.search(),
})

// ---------------------------------------------------------------------------------------------------------------------
// menus
// ---------------------------------------------------------------------------------------------------------------------

registerContextMenu({ id: 'files.builtin', target: 'file', order: 0, items: builtinFileItems })

registerContextMenu({
  id: 'files.terminal',
  target: 'terminal',
  group: 'files',
  order: 40,
  items: (ctx) => {
    const s = ctx.session ?? cachedSessions().find((x) => x.id === ctx.sessionId)
    if (!ctx.sessionId || s?.protocol !== 'ssh') return []
    const cwd = useTerminalInfoStore.getState().infos[ctx.tabId]?.cwd || s?.cwd
    return [{ label: 'Show in SFTP browser', icon: FolderSync, run: () => revealInPanel(ctx.sessionId!, cwd || undefined) }]
  },
})

registerContextMenu({
  id: 'files.session-node',
  target: 'session-node',
  order: 30,
  items: (ctx) => {
    const c = ctx.connection
    if (!c || (ctx.selection?.length ?? 0) > 1 || c.protocol !== 'ssh') return []
    // A running session of this connection shown in a tab → its SSH-browser (no second login).
    const live = cachedSessions().find((s) => s.connectionId === c.id && s.state === 'connected' && findTabs((t) => (t.params as TerminalTabParams)?.sessionId === s.id).length > 0)
    if (!live) return []
    return [{ label: 'Open SFTP browser', icon: FolderSync, run: () => revealInPanel(live.id) }]
  },
})

registerContextMenu({
  id: 'files.tab',
  target: 'tab',
  order: 55,
  items: (ctx) => {
    if (ctx.kind !== FILES_TAB) return []
    const p = (ctx.params ?? {}) as FilesTabParams
    return [
      {
        label: 'Dual-pane commander',
        icon: Columns2,
        checked: !!p.dual,
        run: () => {
          updateTabParams<FilesTabParams>(ctx.tabId, { dual: p.dual ? undefined : true })
          focusTab(ctx.tabId)
        },
      },
    ]
  },
})

registerMenu({
  id: 'files.sessions-menu',
  menu: 'sessions',
  order: 160,
  items: () => [
    { label: 'SFTP browser', icon: FolderSync, command: `sidebar.show.${SFTP_PANEL}` },
    { label: 'Local files', icon: HardDrive, command: 'files.openLocal' },
    { label: 'File commander (dual pane)', icon: Columns2, command: 'files.commander' },
    { label: 'File transfers', icon: ArrowUpDown, command: 'files.transfers' },
  ],
})

registerMenu({
  id: 'files.tools-menu',
  menu: 'tools',
  order: 150,
  items: () => [
    { label: 'File commander (dual pane)', icon: Columns2, command: 'files.commander' },
    { label: 'File transfers', icon: ArrowUpDown, command: 'files.transfers' },
  ],
})
