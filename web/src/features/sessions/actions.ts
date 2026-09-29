/*
 * Session-manager actions shared by the tree, context menus, ribbon and commands. Everything works from the react-query
 * caches (no React context needed) and applies optimistic updates where the list changes.
 */
import { toast } from 'sonner'
import { bulkDeleteConnections, deleteConnection, duplicateConnection as apiDuplicate, updateConnection } from '@/api/connections'
import { deleteFolder as apiDeleteFolder } from '@/api/folders'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { Connection, ConnectionPatch, Folder } from '@/api/types'
import { confirm, confirmEx, prompt } from '@/components/ui/dialog-host'
import { copyText, errorMessage, plural } from '@/lib/utils'
import { applyMovePlan, patchFolders, planMove } from './api'
import { connectMany } from './connect'
import { openFolderDialog, openSessionEditor } from './dialogs/store'
import { buildTreeIndex, connNodeId, folderNodeId, formatHostPort, subtreeConnections, subtreeFolders, type TreeIndex } from './model'
import { afterSessionMenu, getTreeController, revealNode } from './panel/controller'
import { sessionsSettings } from './settings'

// ---------------------------------------------------------------------------------------------------------------------
// Cache access
// ---------------------------------------------------------------------------------------------------------------------

export function cachedConnections(): Connection[] {
  return queryClient.getQueryData<Connection[]>(queryKeys.connections) ?? []
}

export function cachedFolders(): Folder[] {
  return queryClient.getQueryData<Folder[]>(queryKeys.folders) ?? []
}

export function currentIndex(): TreeIndex {
  return buildTreeIndex(cachedFolders(), cachedConnections(), { sortMode: sessionsSettings.get().sortMode })
}

export function findConnection(id: string): Connection | undefined {
  return cachedConnections().find((c) => c.id === id)
}

/** Optimistically transform the cached connection list around a request (rollback + refetch on failure). */
async function mutateConnections<T>(update: (list: Connection[]) => Connection[], run: () => Promise<T>): Promise<T> {
  await queryClient.cancelQueries({ queryKey: queryKeys.connections, exact: true })
  const prev = queryClient.getQueryData<Connection[]>(queryKeys.connections)
  if (prev) queryClient.setQueryData<Connection[]>(queryKeys.connections, update(prev))
  try {
    return await run()
  } catch (err) {
    queryClient.setQueryData(queryKeys.connections, prev)
    throw err
  } finally {
    void queryClient.invalidateQueries({ queryKey: queryKeys.connections })
  }
}

function report(what: string) {
  return (err: unknown) => {
    toast.error(what, { description: errorMessage(err) })
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// Connections
// ---------------------------------------------------------------------------------------------------------------------

/** Patch one connection with an optimistic cache update (secrets are never merged into the cache). */
export async function patchConnection(id: string, patch: ConnectionPatch): Promise<Connection | undefined> {
  const { secrets: _secrets, ...visible } = patch
  try {
    const updated = await mutateConnections(
      (list) => list.map((c) => (c.id === id ? { ...c, ...visible } : c)),
      () => updateConnection(id, patch),
    )
    queryClient.setQueryData(queryKeys.connection(id), updated)
    return updated
  } catch (err) {
    report('Could not update the session')(err)
    return undefined
  }
}

export async function renameConnection(c: Connection, name: string): Promise<void> {
  const n = name.trim()
  if (!n || n === c.name) return
  await patchConnection(c.id, { name: n.slice(0, 200) })
}

export async function toggleFavorite(conns: readonly Connection[], favorite?: boolean): Promise<void> {
  if (!conns.length) return
  const target = favorite ?? !conns.every((c) => c.favorite)
  await Promise.all(conns.filter((c) => c.favorite !== target).map((c) => patchConnection(c.id, { favorite: target })))
}

export async function setConnectionColor(c: Connection, color: string | undefined): Promise<void> {
  await patchConnection(c.id, { color: color ?? '' })
}

/** Colour several sessions at once (multi-selection). */
export async function setConnectionsColor(conns: readonly Connection[], color: string | undefined): Promise<void> {
  await Promise.all(conns.filter((c) => (c.color || undefined) !== color).map((c) => setConnectionColor(c, color)))
}

const hasTag = (c: Connection, tag: string) => (c.tags ?? []).some((t) => t.toLowerCase() === tag.toLowerCase())

/** Add a tag to sessions that do not have it yet (case-insensitive). */
export async function addTag(conns: readonly Connection[], tag: string): Promise<void> {
  const t = tag.trim()
  if (!t) return
  await Promise.all(conns.filter((c) => !hasTag(c, t)).map((c) => patchConnection(c.id, { tags: [...(c.tags ?? []), t] })))
}

/** Remove a tag from the sessions that have it. */
export async function removeTag(conns: readonly Connection[], tag: string): Promise<void> {
  await Promise.all(
    conns.filter((c) => hasTag(c, tag)).map((c) => patchConnection(c.id, { tags: (c.tags ?? []).filter((x) => x.toLowerCase() !== tag.toLowerCase()) })),
  )
}

/** Ask for a tag name and add it to the sessions. */
export async function addTagInteractive(conns: readonly Connection[]): Promise<void> {
  if (!conns.length) return
  const tag = await prompt({
    title: conns.length === 1 ? `Tag “${conns[0].name}”` : `Tag ${plural(conns.length, 'session')}`,
    label: 'Tag',
    placeholder: 'e.g. prod, web, eu-west',
    confirmLabel: 'Add tag',
    validate: (v) => (!v.trim() ? 'Enter a tag' : v.trim().length > 64 ? 'Keep tags under 64 characters' : null),
  })
  if (tag) await addTag(conns, tag)
}

export async function duplicateConnection(c: Connection): Promise<void> {
  try {
    const copy = await apiDuplicate(c.id)
    queryClient.setQueryData<Connection[]>(queryKeys.connections, (old) => (old ? [...old.filter((x) => x.id !== copy.id), copy] : old))
    void queryClient.invalidateQueries({ queryKey: queryKeys.connections })
    revealNode(connNodeId(copy.id))
    toast.success(`Duplicated as "${copy.name}"`, {
      action: { label: 'Edit', onClick: () => openSessionEditor({ mode: 'edit', connectionId: copy.id }) },
    })
  } catch (err) {
    report('Could not duplicate the session')(err)
  }
}

/** Confirm, then delete one or more connections. */
export async function deleteConnections(conns: readonly Connection[]): Promise<boolean> {
  if (!conns.length) return false
  const one = conns.length === 1
  const ok = await confirm({
    title: one ? `Delete session “${conns[0].name}”?` : `Delete ${plural(conns.length, 'session')}?`,
    description: one
      ? 'The saved session and its stored credentials are removed. Open terminals stay connected.'
      : `${conns
          .slice(0, 5)
          .map((c) => c.name)
          .join(', ')}${conns.length > 5 ? `, and ${conns.length - 5} more` : ''} will be removed with their stored credentials.`,
    confirmLabel: 'Delete',
    destructive: true,
  })
  if (!ok) return false
  const ids = conns.map((c) => c.id)
  const idSet = new Set(ids)
  try {
    await mutateConnections(
      (list) => list.filter((c) => !idSet.has(c.id)),
      () => (one ? deleteConnection(ids[0]) : bulkDeleteConnections(ids)),
    )
    for (const id of ids) queryClient.removeQueries({ queryKey: queryKeys.connection(id), exact: true })
    toast.success(one ? `Deleted "${conns[0].name}"` : `Deleted ${plural(conns.length, 'session')}`)
    return true
  } catch (err) {
    report('Could not delete')(err)
    return false
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// Folders
// ---------------------------------------------------------------------------------------------------------------------

export async function renameFolder(f: Folder, name: string): Promise<void> {
  const n = name.trim()
  if (!n || n === f.name) return
  await patchFolders([{ id: f.id, patch: { name: n.slice(0, 200) } }]).catch(report('Could not rename the folder'))
}

export async function setFolderColor(f: Folder, color: string | undefined): Promise<void> {
  await patchFolders([{ id: f.id, patch: { color: color ?? '' } }]).catch(report('Could not change the folder colour'))
}

/** Delete a folder: empty folders directly; otherwise ask whether to delete the contents or move them up. */
export async function deleteFolder(f: Folder): Promise<boolean> {
  const index = currentIndex()
  const nid = folderNodeId(f.id)
  const conns = subtreeConnections(index, nid)
  const subs = subtreeFolders(index, nid)
  let recursive = false
  if (!conns.length && !subs.length) {
    if (!(await confirm({ title: `Delete folder “${f.name}”?`, description: 'The folder is empty.', confirmLabel: 'Delete', destructive: true }))) return false
  } else {
    const parts = [conns.length ? plural(conns.length, 'session') : '', subs.length ? plural(subs.length, 'subfolder') : ''].filter(Boolean).join(' and ')
    const r = await confirmEx({
      title: `Delete folder “${f.name}”?`,
      description: `It contains ${parts}. Unless you also delete them, they move up to ${f.parentId ? 'the parent folder' : 'the top level'}.`,
      checkboxLabel: `Also delete the ${parts} inside`,
      confirmLabel: 'Delete',
      destructive: true,
    })
    if (!r.ok) return false
    recursive = r.checked
  }
  await queryClient.cancelQueries({ queryKey: queryKeys.folders, exact: true })
  const prev = queryClient.getQueryData<Folder[]>(queryKeys.folders)
  const removed = new Set([f.id, ...(recursive ? subs.map((s) => s.id) : [])])
  queryClient.setQueryData<Folder[]>(queryKeys.folders, (old) =>
    old?.filter((x) => !removed.has(x.id)).map((x) => (!recursive && x.parentId === f.id ? { ...x, parentId: f.parentId ?? null } : x)),
  )
  try {
    await apiDeleteFolder(f.id, recursive)
    toast.success(`Deleted folder "${f.name}"`)
    return true
  } catch (err) {
    queryClient.setQueryData(queryKeys.folders, prev)
    report('Could not delete the folder')(err)
    return false
  } finally {
    void queryClient.invalidateQueries({ queryKey: queryKeys.folders })
    void queryClient.invalidateQueries({ queryKey: queryKeys.connections })
  }
}

/** Open every connection below a folder (asks first above the configured threshold). */
export async function connectFolder(folderId: string): Promise<void> {
  const index = currentIndex()
  const f = index.folders.get(folderId)
  const conns = subtreeConnections(index, folderNodeId(folderId))
  if (!conns.length) {
    toast.info(`“${f?.name ?? 'Folder'}” has no sessions`)
    return
  }
  const threshold = sessionsSettings.get().connectAllConfirm
  if (threshold > 0 && conns.length > threshold) {
    const ok = await confirm({ title: `Open ${plural(conns.length, 'session')}?`, description: `Every session in “${f?.name}” opens in its own tab.`, confirmLabel: 'Open all' })
    if (!ok) return
  }
  await connectMany(conns)
}

// ---------------------------------------------------------------------------------------------------------------------
// Moving
// ---------------------------------------------------------------------------------------------------------------------

/** Move folders/connections into a folder (null = top level), appended after its existing children. */
export async function moveItems(targetFolderId: string | null, folderIds: readonly string[], connectionIds: readonly string[]): Promise<void> {
  const plan = planMove({
    folders: cachedFolders(),
    connections: cachedConnections(),
    targetFolderId,
    moveFolderIds: folderIds,
    moveConnectionIds: connectionIds,
  })
  if (!plan.folders.length && !plan.connections.length) return
  try {
    await applyMovePlan(plan)
    if (targetFolderId) revealNode(folderNodeId(targetFolderId))
  } catch (err) {
    report('Could not move')(err)
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// Rename (inline when the tree shows the node, else a prompt)
// ---------------------------------------------------------------------------------------------------------------------

export async function renameNodeInteractive(target: { connection?: Connection; folder?: Folder }): Promise<void> {
  const nodeId = target.connection ? connNodeId(target.connection.id) : target.folder ? folderNodeId(target.folder.id) : null
  if (!nodeId) return
  // From a context menu: start once the menu has restored focus, or the new input would lose it immediately.
  const started = await new Promise<boolean>((resolve) => afterSessionMenu(() => resolve(!!getTreeController()?.startRename(nodeId))))
  if (started) return
  const current = target.connection?.name ?? target.folder?.name ?? ''
  const name = await prompt({
    title: target.connection ? 'Rename session' : 'Rename folder',
    defaultValue: current,
    confirmLabel: 'Rename',
    validate: (v) => (!v.trim() ? 'Enter a name' : v.trim().length > 200 ? 'At most 200 characters' : null),
  })
  if (name === null) return
  if (target.connection) await renameConnection(target.connection, name)
  else if (target.folder) await renameFolder(target.folder, name)
}

// ---------------------------------------------------------------------------------------------------------------------
// New items
// ---------------------------------------------------------------------------------------------------------------------

export function newSessionIn(folderId: string | null): void {
  openSessionEditor({ mode: 'create', folderId })
}

export async function newFolderIn(parentId: string | null): Promise<void> {
  await openFolderDialog({ mode: 'create', parentId })
}

// ---------------------------------------------------------------------------------------------------------------------
// Clipboard
// ---------------------------------------------------------------------------------------------------------------------

/** POSIX shell quoting for copy-pasteable commands. */
export function shellQuote(s: string): string {
  if (s === '') return "''"
  return /^[A-Za-z0-9_@%+=:,./-]+$/.test(s) ? s : `'${s.replace(/'/g, `'\\''`)}'`
}

function jumpSpec(c: Connection): string {
  const hp = formatHostPort(c.host, c.port, 'ssh')
  return c.username ? `${c.username}@${hp}` : hp
}

/** Equivalent command line (ssh / sftp / mosh / telnet), or null when there is none. */
export function commandLineFor(c: Connection, all: readonly Connection[] = cachedConnections()): string | null {
  if (!c.host) return null
  const byId = new Map(all.map((x) => [x.id, x]))
  const dest = c.username ? `${c.username}@${c.host}` : c.host
  // Hops are saved connection ids or ad-hoc "[user@]host[:port]" strings; unresolvable ids (deleted) are skipped.
  const jumps = (Array.isArray(c.options?.jumpHosts) ? (c.options.jumpHosts as unknown[]) : [])
    .filter((x): x is string => typeof x === 'string' && x !== '')
    .map((hop) => {
      const saved = byId.get(hop)
      if (saved) return jumpSpec(saved)
      return /^[a-z2-7]{20}$/.test(hop) ? null : hop
    })
    .filter((x): x is string => !!x)
  const jumpArgs = jumps.length ? ['-J', jumps.join(',')] : []
  switch (c.protocol) {
    case 'ssh': {
      const args = ['ssh']
      if (c.port && c.port !== 22) args.push('-p', String(c.port))
      args.push(...jumpArgs)
      if (c.options?.agentForwarding === true) args.push('-A')
      if (c.options?.x11Forwarding === true) args.push('-X')
      args.push(dest)
      const cmd = typeof c.options?.remoteCommand === 'string' ? c.options.remoteCommand.trim() : ''
      return args.map(shellQuote).join(' ') + (cmd ? ` ${shellQuote(cmd)}` : '')
    }
    case 'sftp': {
      const args = ['sftp']
      if (c.port && c.port !== 22) args.push('-P', String(c.port))
      args.push(...jumpArgs, dest)
      return args.map(shellQuote).join(' ')
    }
    case 'mosh': {
      const args = ['mosh']
      if (c.port && c.port !== 22) args.push(`--ssh=ssh -p ${c.port}`)
      args.push(dest)
      return args.map(shellQuote).join(' ')
    }
    case 'telnet':
      return ['telnet', c.host, ...(c.port && c.port !== 23 ? [String(c.port)] : [])].map(shellQuote).join(' ')
    default:
      return null
  }
}

export async function copyToClipboard(text: string, what: string): Promise<void> {
  if (await copyText(text)) toast.success(`${what} copied`)
  else toast.error('The browser blocked clipboard access')
}
