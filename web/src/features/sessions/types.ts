/*
 * Feature-local types of the session manager (SPEC §10: feature types live beside the feature, not in api/types.ts).
 */
import type { Connection, ConnectionOptions, Folder, Protocol } from '@/api/types'

/**
 * Protocols the session editor knows: SPEC §5.2 plus the §10.1 extension protocols served by internal/proto/winrm and
 * internal/proto/ipmi (the backend's model.KnownProtocols). api/types.ts is owned by the foundation, so the two extra
 * ids are added here.
 */
export type EditorProtocol = Protocol | 'winrm' | 'ipmi'

/** Tree node ids: the root, `f:<folderId>` and `c:<connectionId>` (namespaced so folder/connection ids never clash). */
export type NodeId = string
export const ROOT_ID = 'root'

export type TreeNode =
  | { kind: 'root'; id: NodeId }
  | { kind: 'folder'; id: NodeId; folder: Folder }
  | { kind: 'connection'; id: NodeId; connection: Connection }
  /** An id that is no longer in the data (e.g. a selection that outlived a delete) — never rendered. */
  | { kind: 'missing'; id: NodeId }

export type SortMode = 'manual' | 'name' | 'recent' | 'protocol' | 'host'

/** How the "All sessions" list is organised: the folder tree, or flat groups by protocol / tag. */
export type GroupBy = 'folder' | 'protocol' | 'tag'

export interface SessionFilters {
  protocols: string[]
  tags: string[]
  favorites: boolean
  shared: boolean
  /** Only connections with a live runtime session. */
  running: boolean
}

export const EMPTY_FILTERS: SessionFilters = Object.freeze({ protocols: [], tags: [], favorites: false, shared: false, running: false }) as SessionFilters

/** Where a connection opens (F1a's openConnection positions). */
export type ConnectPosition = 'tab' | 'right' | 'below' | 'window'

/** Partial connection used to pre-fill the editor (quick connect "Save as session…", duplicates, imports). */
export type ConnectionDraft = Partial<Omit<Connection, 'protocol'>> & { protocol?: EditorProtocol | string; password?: string; options?: ConnectionOptions }

/** Arguments of the `sessions.new` command. */
export interface NewSessionArgs {
  folderId?: string | null
  protocol?: string
  initial?: ConnectionDraft
}
