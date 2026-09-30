/*
 * Pure helpers of the session manager: the folder/connection tree index, ordering, labels and running-session lookup.
 * Nothing here touches React, the network or stores, so it can be shared by the panel, menus, dialogs and commands.
 */
import type { Connection, Folder, RuntimeSession, User } from '@/api/types'
import { defaultPort, protocolLabel } from '@/app/protocols'
import { ROOT_ID, type NodeId, type SortMode, type TreeNode } from './types'

// ---------------------------------------------------------------------------------------------------------------------
// Node ids
// ---------------------------------------------------------------------------------------------------------------------

export const folderNodeId = (id: string): NodeId => `f:${id}`
export const connNodeId = (id: string): NodeId => `c:${id}`

export function parseNodeId(nodeId: NodeId): { kind: 'root' | 'folder' | 'connection' | 'unknown'; id: string } {
  if (nodeId === ROOT_ID) return { kind: 'root', id: '' }
  if (nodeId.startsWith('f:')) return { kind: 'folder', id: nodeId.slice(2) }
  if (nodeId.startsWith('c:')) return { kind: 'connection', id: nodeId.slice(2) }
  return { kind: 'unknown', id: nodeId }
}

// ---------------------------------------------------------------------------------------------------------------------
// Ordering
// ---------------------------------------------------------------------------------------------------------------------

const collator = new Intl.Collator(undefined, { sensitivity: 'base', numeric: true })

/** Natural, case-insensitive name order ("web2" before "web10"). */
function compareNames(a: string, b: string): number {
  return collator.compare(a, b)
}

function time(v: string | undefined): number {
  const t = v ? Date.parse(v) : NaN
  return Number.isFinite(t) ? t : 0
}

function folderComparator(mode: SortMode): (a: Folder, b: Folder) => number {
  if (mode === 'manual') return (a, b) => a.sortOrder - b.sortOrder || compareNames(a.name, b.name)
  return (a, b) => compareNames(a.name, b.name)
}

export function connectionComparator(mode: SortMode): (a: Connection, b: Connection) => number {
  const byName = (a: Connection, b: Connection) => compareNames(a.name, b.name)
  switch (mode) {
    case 'manual':
      return (a, b) => a.sortOrder - b.sortOrder || byName(a, b)
    case 'recent':
      return (a, b) => time(b.lastUsedAt) - time(a.lastUsedAt) || byName(a, b)
    case 'protocol':
      return (a, b) => compareNames(protocolLabel(a.protocol), protocolLabel(b.protocol)) || byName(a, b)
    case 'host':
      return (a, b) => {
        if (!a.host !== !b.host) return a.host ? -1 : 1
        return compareNames(a.host, b.host) || byName(a, b)
      }
    default:
      return byName
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// Tree index
// ---------------------------------------------------------------------------------------------------------------------

export interface TreeIndex {
  folders: Map<string, Folder>
  connections: Map<string, Connection>
  nodes: Map<NodeId, TreeNode>
  /** Display-ordered children (folders first) of the root and every folder node. */
  children: Map<NodeId, NodeId[]>
  /** Effective parent node (orphans and cycles are re-attached to the root). */
  parent: Map<NodeId, NodeId>
  /** Connections inside each folder's subtree (after filtering). */
  counts: Map<NodeId, number>
  /** True when built with a filter (only matching connections and their folders are present). */
  filtered: boolean
}

export interface IndexOptions {
  sortMode: SortMode
  /** Keep only connections passing this predicate (plus the folders that contain them). */
  filter?: (c: Connection) => boolean
}

const ROOT_NODE: TreeNode = { kind: 'root', id: ROOT_ID }

/** Effective parent folder id of each folder: missing parents and cycles resolve to the root (null). */
function effectiveFolderParents(folders: Map<string, Folder>): Map<string, string | null> {
  const out = new Map<string, string | null>()
  const limit = folders.size + 1
  for (const f of folders.values()) {
    const p = f.parentId
    if (!p || p === f.id || !folders.has(p)) {
      out.set(f.id, null)
      continue
    }
    // Walk up from the parent; reaching `f` again means a cycle (never produced by the backend, but be defensive).
    let cur: string | null | undefined = p
    let steps = 0
    let cycle = false
    while (cur && steps++ <= limit) {
      if (cur === f.id) {
        cycle = true
        break
      }
      const next: Folder | undefined = folders.get(cur)
      cur = next?.parentId && folders.has(next.parentId) ? next.parentId : null
    }
    out.set(f.id, cycle || steps > limit ? null : p)
  }
  return out
}

export function buildTreeIndex(folderList: readonly Folder[], connectionList: readonly Connection[], opts: IndexOptions): TreeIndex {
  const folders = new Map(folderList.map((f) => [f.id, f]))
  const connections = new Map(connectionList.map((c) => [c.id, c]))
  const folderParent = effectiveFolderParents(folders)
  const connFolder = (c: Connection): string | null => (c.folderId && folders.has(c.folderId) ? c.folderId : null)

  // Which folders take part: all, or only ancestors of matching connections.
  let includedConns: Connection[]
  let includedFolders: Set<string>
  if (opts.filter) {
    includedConns = connectionList.filter(opts.filter)
    includedFolders = new Set()
    for (const c of includedConns) {
      let f = connFolder(c)
      while (f && !includedFolders.has(f)) {
        includedFolders.add(f)
        f = folderParent.get(f) ?? null
      }
    }
  } else {
    includedConns = connectionList.slice()
    includedFolders = new Set(folders.keys())
  }

  const nodes = new Map<NodeId, TreeNode>([[ROOT_ID, ROOT_NODE]])
  const parent = new Map<NodeId, NodeId>()
  const folderKids = new Map<NodeId, Folder[]>()
  const connKids = new Map<NodeId, Connection[]>()
  const push = <T>(m: Map<NodeId, T[]>, k: NodeId, v: T) => {
    const arr = m.get(k)
    if (arr) arr.push(v)
    else m.set(k, [v])
  }

  for (const id of includedFolders) {
    const f = folders.get(id)!
    const nid = folderNodeId(id)
    const p = folderParent.get(id)
    const pid = p ? folderNodeId(p) : ROOT_ID
    nodes.set(nid, { kind: 'folder', id: nid, folder: f })
    parent.set(nid, pid)
    push(folderKids, pid, f)
  }
  for (const c of includedConns) {
    const nid = connNodeId(c.id)
    const f = connFolder(c)
    const pid = f ? folderNodeId(f) : ROOT_ID
    nodes.set(nid, { kind: 'connection', id: nid, connection: c })
    parent.set(nid, pid)
    push(connKids, pid, c)
  }

  const fcmp = folderComparator(opts.sortMode)
  const ccmp = connectionComparator(opts.sortMode)
  const children = new Map<NodeId, NodeId[]>()
  const parents = new Set<NodeId>([ROOT_ID, ...folderKids.keys(), ...connKids.keys()])
  for (const pid of parents) {
    const fs = (folderKids.get(pid) ?? []).sort(fcmp).map((f) => folderNodeId(f.id))
    const cs = (connKids.get(pid) ?? []).sort(ccmp).map((c) => connNodeId(c.id))
    children.set(pid, [...fs, ...cs])
  }

  // Subtree connection counts (iterative post-order to stay safe on deep trees).
  const counts = new Map<NodeId, number>()
  const order: NodeId[] = []
  const stack: NodeId[] = [ROOT_ID]
  while (stack.length) {
    const id = stack.pop()!
    order.push(id)
    for (const k of children.get(id) ?? []) if (k.startsWith('f:')) stack.push(k)
  }
  for (let i = order.length - 1; i >= 0; i--) {
    const id = order[i]
    let n = 0
    for (const k of children.get(id) ?? []) n += k.startsWith('c:') ? 1 : (counts.get(k) ?? 0)
    counts.set(id, n)
  }

  return { folders, connections, nodes, children, parent, counts, filtered: !!opts.filter }
}

/** Folder chain from the top level down to `folderId` (inclusive). */
function folderPath(folders: Map<string, Folder>, folderId: string | null | undefined): Folder[] {
  const out: Folder[] = []
  const seen = new Set<string>()
  let cur = folderId ? folders.get(folderId) : undefined
  while (cur && !seen.has(cur.id)) {
    seen.add(cur.id)
    out.unshift(cur)
    cur = cur.parentId ? folders.get(cur.parentId) : undefined
  }
  return out
}

export function folderPathLabel(folders: Map<string, Folder>, folderId: string | null | undefined, sep = ' / '): string {
  return folderPath(folders, folderId)
    .map((f) => f.name)
    .join(sep)
}

/** Connections below a node in display order (depth-first). */
export function subtreeConnections(index: TreeIndex, nodeId: NodeId): Connection[] {
  const out: Connection[] = []
  const walk = (id: NodeId, depth: number) => {
    if (depth > 256) return
    for (const k of index.children.get(id) ?? []) {
      const n = index.nodes.get(k)
      if (n?.kind === 'connection') out.push(n.connection)
      else if (n?.kind === 'folder') walk(k, depth + 1)
    }
  }
  walk(nodeId, 0)
  return out
}

/** Folders below a node (not including it). */
export function subtreeFolders(index: TreeIndex, nodeId: NodeId): Folder[] {
  const out: Folder[] = []
  const walk = (id: NodeId, depth: number) => {
    if (depth > 256) return
    for (const k of index.children.get(id) ?? []) {
      const n = index.nodes.get(k)
      if (n?.kind === 'folder') {
        out.push(n.folder)
        walk(k, depth + 1)
      }
    }
  }
  walk(nodeId, 0)
  return out
}

/** Is `candidate` the folder `ancestor` itself or one of its descendants? */
export function isFolderInSubtree(folders: Map<string, Folder>, ancestor: string, candidate: string | null | undefined): boolean {
  const seen = new Set<string>()
  let cur = candidate ?? null
  while (cur && !seen.has(cur)) {
    if (cur === ancestor) return true
    seen.add(cur)
    cur = folders.get(cur)?.parentId ?? null
  }
  return false
}

/** Every folder as {folder, depth} in display (name/manual) order, for folder pickers. */
export function flattenFolders(folderList: readonly Folder[], sortMode: SortMode = 'manual'): { folder: Folder; depth: number; path: string }[] {
  const index = buildTreeIndex(folderList, [], { sortMode })
  const out: { folder: Folder; depth: number; path: string }[] = []
  const walk = (id: NodeId, depth: number, prefix: string) => {
    if (depth > 64) return
    for (const k of index.children.get(id) ?? []) {
      const n = index.nodes.get(k)
      if (n?.kind !== 'folder') continue
      const path = prefix ? `${prefix} / ${n.folder.name}` : n.folder.name
      out.push({ folder: n.folder, depth, path })
      walk(k, depth + 1, path)
    }
  }
  walk(ROOT_ID, 0, '')
  return out
}

// ---------------------------------------------------------------------------------------------------------------------
// Labels
// ---------------------------------------------------------------------------------------------------------------------

/** "host:port" with IPv6 brackets; the port is omitted when it is the protocol default. */
export function formatHostPort(host: string, port: number | undefined | null, protocol?: string): string {
  if (!host) return ''
  const h = host.includes(':') && !host.startsWith('[') ? `[${host}]` : host
  if (!port || (protocol && port === defaultPort(protocol))) return h
  return `${h}:${port}`
}

/** The fields labels are computed from (protocol is a plain string: extension protocols are not in api/types). */
export interface TargetFields {
  protocol: string
  host: string
  port: number
  username: string
  options: Connection['options']
}

function opt(c: Pick<TargetFields, 'options'>, key: string): string {
  const v = c.options?.[key]
  return typeof v === 'string' ? v.trim() : typeof v === 'number' ? String(v) : ''
}

/** Secondary text for a connection row: user@host:port, device@baud, container… */
export function connectionTarget(c: TargetFields): string {
  switch (c.protocol) {
    case 'serial': {
      const dev = opt(c, 'device')
      const baud = opt(c, 'baud') || '9600'
      return dev ? `${dev} @ ${baud}` : ''
    }
    case 'local':
      return opt(c, 'shell') || 'default shell'
    case 'docker': {
      const ctr = opt(c, 'container')
      return ctr ? `${ctr}${opt(c, 'user') ? ` (${opt(c, 'user')})` : ''}` : ''
    }
    case 'kube': {
      const pod = opt(c, 'pod')
      const ns = opt(c, 'namespace')
      return pod ? `${ns ? `${ns}/` : ''}${pod}` : opt(c, 'context')
    }
    case 's3': {
      const bucket = opt(c, 'bucket')
      const endpoint = opt(c, 'endpoint').replace(/^https?:\/\//i, '')
      return bucket ? `s3://${bucket}` : endpoint || 'Amazon S3'
    }
    default: {
      const hp = formatHostPort(c.host, c.port, c.protocol)
      if (!hp) return ''
      return c.username ? `${c.username}@${hp}` : hp
    }
  }
}

/** Name suggested for a draft without one (sessions are named after their target). */
export function autoName(c: TargetFields): string {
  switch (c.protocol) {
    case 'serial':
      return opt(c, 'device')
    case 'local': {
      const sh = opt(c, 'shell')
      return sh ? sh.split(/[\\/]/).pop() || sh : 'Local shell'
    }
    case 'docker':
      return opt(c, 'container')
    case 'kube':
      return opt(c, 'pod') || opt(c, 'context')
    case 's3':
      return opt(c, 'bucket') || 'S3'
    default:
      return c.host ? (c.username ? `${c.username}@${c.host}` : c.host) : ''
  }
}

/** Sorted unique tags across connections (case-insensitive de-duplication, first spelling wins). */
export interface ConnectionGroup {
  /** `protocol:<id>`, `tag:<lower-case tag>` or `tag:` (untagged). */
  key: string
  label: string
  /** The protocol id (protocol groups). */
  protocol?: string
  connections: Connection[]
}

/**
 * Flat groups for the protocol / tag views, each sorted with `compare`: protocols by label; tags by name with the
 * untagged sessions last. A session with several tags appears in each of its tag groups.
 */
export function groupConnections(conns: readonly Connection[], by: 'protocol' | 'tag', compare: (a: Connection, b: Connection) => number): ConnectionGroup[] {
  const groups = new Map<string, ConnectionGroup>()
  const add = (key: string, label: string, c: Connection, protocol?: string) => {
    let g = groups.get(key)
    if (!g) groups.set(key, (g = { key, label, protocol, connections: [] }))
    g.connections.push(c)
  }
  for (const c of conns) {
    if (by === 'protocol') add(`protocol:${c.protocol}`, protocolLabel(c.protocol) || c.protocol, c, c.protocol)
    else if (!c.tags?.length) add('tag:', 'Untagged', c)
    else {
      const own = new Map<string, string>()
      for (const t of c.tags) if (t.trim() && !own.has(t.trim().toLowerCase())) own.set(t.trim().toLowerCase(), t.trim())
      if (!own.size) add('tag:', 'Untagged', c)
      for (const [lower, t] of own) add(`tag:${lower}`, groups.get(`tag:${lower}`)?.label ?? t, c)
    }
  }
  const list = [...groups.values()]
  for (const g of list) g.connections.sort(compare)
  return list.sort((a, b) => (a.key === 'tag:' ? 1 : b.key === 'tag:' ? -1 : compareNames(a.label, b.label)))
}

export function collectTags(conns: readonly Connection[] | undefined): string[] {
  const seen = new Map<string, string>()
  for (const c of conns ?? []) for (const t of c.tags ?? []) if (t && !seen.has(t.toLowerCase())) seen.set(t.toLowerCase(), t)
  return [...seen.values()].sort(compareNames)
}

// ---------------------------------------------------------------------------------------------------------------------
// Running sessions
// ---------------------------------------------------------------------------------------------------------------------

export type RunningStatus = 'connected' | 'connecting' | 'error' | 'disconnected'

/** Live (not closed) runtime sessions grouped by connection id. */
export function groupRunningSessions(sessions: readonly RuntimeSession[] | undefined): Map<string, RuntimeSession[]> {
  const out = new Map<string, RuntimeSession[]>()
  for (const s of sessions ?? []) {
    if (!s.connectionId || s.state === 'closed') continue
    const arr = out.get(s.connectionId)
    if (arr) arr.push(s)
    else out.set(s.connectionId, [s])
  }
  return out
}

/** Most relevant state among a connection's live sessions. */
export function runningStatus(list: readonly RuntimeSession[]): RunningStatus {
  if (list.some((s) => s.state === 'connected')) return 'connected'
  if (list.some((s) => s.state === 'connecting' || s.state === 'authenticating')) return 'connecting'
  if (list.some((s) => s.state === 'error')) return 'error'
  return 'disconnected'
}

// ---------------------------------------------------------------------------------------------------------------------
// Permissions
// ---------------------------------------------------------------------------------------------------------------------

/** Owners and admins may modify an item; shared items of other users are read-only (SPEC §3). */
export function canModify(item: { ownerId: string } | undefined | null, user: User | null | undefined): boolean {
  if (!item || !user) return false
  return user.role === 'admin' || item.ownerId === user.id
}

/** Next sortOrder to append after the existing siblings. */
export function nextSortOrder(items: readonly { sortOrder: number }[]): number {
  let max = -1
  for (const i of items) if (Number.isFinite(i.sortOrder) && i.sortOrder > max) max = i.sortOrder
  return max + 1
}
