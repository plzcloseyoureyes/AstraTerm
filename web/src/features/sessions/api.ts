/*
 * Session-manager specific data helpers on top of src/api/*: a 404-tolerant SSH key list for the editor, and
 * optimistic batch moves (drag & drop, "Move to…") of folders and connections.
 */
import { useQuery } from '@tanstack/react-query'
import { api, isApiError } from '@/api/client'
import { reorderConnections } from '@/api/connections'
import { updateFolder } from '@/api/folders'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import type { Connection, Folder, FolderInput, ReorderItem, SSHKey } from '@/api/types'
import { isFolderInSubtree, nextSortOrder } from './model'

// ---------------------------------------------------------------------------------------------------------------------
// SSH keys (GET /api/keys lands with the keys module; until then the picker degrades gracefully)
// ---------------------------------------------------------------------------------------------------------------------

/** Query key under the `keys` prefix, so invalidations by the keys feature refresh the picker too. */
const keysPickerKey = [...queryKeys.keys, 'picker'] as const

interface KeysPickerData {
  available: boolean
  keys: SSHKey[]
}

async function loadKeysForPicker(): Promise<KeysPickerData> {
  try {
    const keys = await api.get<SSHKey[]>('/api/keys', { noVaultPrompt: true })
    return { available: true, keys: Array.isArray(keys) ? keys : [] }
  } catch (err) {
    // Not mounted yet (404/405/501): the editor shows a hint instead of an error.
    if (isApiError(err) && (err.status === 404 || err.status === 405 || err.status === 501)) return { available: false, keys: [] }
    throw err
  }
}

export function useKeysForPicker(enabled = true) {
  return useQuery({ queryKey: keysPickerKey, queryFn: loadKeysForPicker, enabled, staleTime: 30_000 })
}

// ---------------------------------------------------------------------------------------------------------------------
// Optimistic batch updates
// ---------------------------------------------------------------------------------------------------------------------

export interface FolderPatch {
  id: string
  patch: Partial<FolderInput>
}

/** PATCH several folders with one optimistic cache update; on failure the cache is restored and refetched. */
export async function patchFolders(patches: FolderPatch[]): Promise<void> {
  if (!patches.length) return
  await queryClient.cancelQueries({ queryKey: queryKeys.folders, exact: true })
  const prev = queryClient.getQueryData<Folder[]>(queryKeys.folders)
  const byId = new Map(patches.map((p) => [p.id, p.patch]))
  queryClient.setQueryData<Folder[]>(queryKeys.folders, (old) =>
    old?.map((f) => {
      const p = byId.get(f.id)
      return p ? { ...f, ...p } : f
    }),
  )
  try {
    // Sequential: keeps server-side validation (cycle checks) deterministic.
    for (const p of patches) await updateFolder(p.id, p.patch)
  } catch (err) {
    queryClient.setQueryData(queryKeys.folders, prev)
    throw err
  } finally {
    void queryClient.invalidateQueries({ queryKey: queryKeys.folders })
  }
}

/** Move/reorder connections (POST /api/connections/reorder, atomic) with an optimistic cache update. */
async function reorderConnectionsOptimistic(items: ReorderItem[]): Promise<void> {
  if (!items.length) return
  await queryClient.cancelQueries({ queryKey: queryKeys.connections, exact: true })
  const prev = queryClient.getQueryData<Connection[]>(queryKeys.connections)
  const byId = new Map(items.map((m) => [m.id, m]))
  queryClient.setQueryData<Connection[]>(queryKeys.connections, (old) =>
    old?.map((c) => {
      const m = byId.get(c.id)
      return m ? { ...c, folderId: m.folderId, sortOrder: m.sortOrder } : c
    }),
  )
  try {
    // The endpoint accepts at most 5000 items per request.
    for (let i = 0; i < items.length; i += 5000) await reorderConnections(items.slice(i, i + 5000))
  } catch (err) {
    queryClient.setQueryData(queryKeys.connections, prev)
    throw err
  } finally {
    void queryClient.invalidateQueries({ queryKey: queryKeys.connections })
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// Move planning
// ---------------------------------------------------------------------------------------------------------------------

export interface MovePlan {
  folders: FolderPatch[]
  connections: ReorderItem[]
}

/**
 * Plan moving folders/connections into `targetFolderId` (null = top level).
 *
 * `order` is the target's desired child order (folder and connection ids mixed, as displayed) when the drop happened
 * between siblings in manual sort mode: every sibling is renumbered to match it. Without `order` the moved items are
 * appended after the target's existing children and nothing else changes.
 */
export function planMove(opts: {
  folders: readonly Folder[]
  connections: readonly Connection[]
  targetFolderId: string | null
  moveFolderIds: readonly string[]
  moveConnectionIds: readonly string[]
  /** Desired order of the target's children: {kind, id} in display order (includes the moved items). */
  order?: readonly { kind: 'folder' | 'connection'; id: string }[]
}): MovePlan {
  const target = opts.targetFolderId
  const folderById = new Map(opts.folders.map((f) => [f.id, f]))
  const connById = new Map(opts.connections.map((c) => [c.id, c]))
  const plan: MovePlan = { folders: [], connections: [] }
  const sameParent = (a: string | null | undefined, b: string | null) => (a || null) === b

  if (opts.order) {
    let fi = 0
    let ci = 0
    for (const item of opts.order) {
      if (item.kind === 'folder') {
        const f = folderById.get(item.id)
        const sortOrder = fi++
        if (!f) continue
        if (!sameParent(f.parentId, target) || f.sortOrder !== sortOrder) plan.folders.push({ id: f.id, patch: { parentId: target, sortOrder } })
      } else {
        const c = connById.get(item.id)
        const sortOrder = ci++
        if (!c) continue
        if (!sameParent(c.folderId, target) || c.sortOrder !== sortOrder) plan.connections.push({ id: c.id, folderId: target, sortOrder })
      }
    }
    return plan
  }

  // Append mode: after everything already in the target (items already there stay where they are).
  let nextFolder = nextSortOrder(opts.folders.filter((f) => sameParent(f.parentId, target)))
  let nextConn = nextSortOrder(opts.connections.filter((c) => sameParent(c.folderId, target)))
  for (const id of opts.moveFolderIds) {
    const f = folderById.get(id)
    // Skip no-ops and moves into the folder itself or one of its descendants (the backend rejects those too).
    if (!f || sameParent(f.parentId, target) || (target && isFolderInSubtree(folderById, f.id, target))) continue
    plan.folders.push({ id, patch: { parentId: target, sortOrder: nextFolder++ } })
  }
  for (const id of opts.moveConnectionIds) {
    const c = connById.get(id)
    if (!c || sameParent(c.folderId, target)) continue
    plan.connections.push({ id, folderId: target, sortOrder: nextConn++ })
  }
  return plan
}

/** Apply a move plan (connections first: one atomic request; then folder patches). */
export async function applyMovePlan(plan: MovePlan): Promise<void> {
  await Promise.all([reorderConnectionsOptimistic(plan.connections), patchFolders(plan.folders)])
}
