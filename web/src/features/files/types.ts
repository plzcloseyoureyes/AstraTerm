/*
 * Files feature types (SPEC §10: feature-specific types live next to the feature). The §6.0 JSON contract types
 * (FsHandle, FileEntry, Transfer, ...) come from @/api/types.
 */
import type { FileEntry, FsHandle, Protocol } from '@/api/types'
import type { QuickSpec } from '@/features/terminal/types'

/** Where a browser's files come from (POST /api/fs). */
export type FsSource =
  /** The SSH connection of a live runtime session (the SFTP side panel that follows the terminal): no second login. */
  | { kind: 'session'; sessionId: string }
  /** A saved connection (sftp / ftp / s3, or ssh opened on its own transport); `sudo`: browse as root (FILE-11). */
  | { kind: 'connection'; connectionId: string; sudo?: boolean }
  /** The file system of the machine running AstraTerm. */
  | { kind: 'local' }
  /** An unsaved quick-connect spec (never contains secrets; the backend prompts). */
  | { kind: 'quick'; quick: QuickSpec }
  /** A handle another feature already opened (only its id is known). */
  | { kind: 'handle'; fsId: string }

/**
 * POST /api/fs body: SPEC §6.0 `{sessionId?|connectionId?|local?:true}` plus `quick` (quick-connect file tabs) and
 * `sudo` (browse as root) — both documented by files-backend in SPEC §9.
 */
export interface FsOpenBody {
  sessionId?: string
  connectionId?: string
  local?: true
  quick?: QuickSpec
  sudo?: boolean
}

/** Capabilities reported by the backend (SPEC §9 files-backend adds everything after `checksum`). */
export interface FsCapabilitiesEx {
  chmod: boolean
  chown: boolean
  symlink: boolean
  exec: boolean
  checksum: boolean
  search?: boolean
  archive?: boolean
  extract?: boolean
  copy?: boolean
  presign?: boolean
  sudo?: boolean
  resume?: boolean
  mtime?: boolean
  hardlink?: boolean
  space?: boolean
}

/** FsHandle with the files-backend extras (SPEC §9): driver (sftp | scp | sudo-sftp …), the login folder. */
export type FsHandleEx = FsHandle & {
  driver?: string
  userHome?: string
  sessionId?: string
  connectionId?: string
  capabilities: FsCapabilitiesEx
}

/** Params of a dock tab of kind "files" (JSON-serialisable, persisted with the layout). */
export interface FilesTabParams {
  /** An already opened handle (from another feature). */
  fsId?: string
  connectionId?: string
  sessionId?: string
  local?: boolean
  quick?: QuickSpec
  /** Protocol (tab icon; shell convention `params.protocol`). */
  protocol?: Protocol
  /** Folder shown (kept up to date so a reload restores it). */
  path?: string
  /** Dual-pane commander mode (FILE-13). */
  dual?: boolean
  /** Source + folder of the right pane in commander mode. */
  right?: PaneParams
  /** Pane that has the focus in commander mode. */
  activePane?: 'left' | 'right'
}

/** A commander pane's source (subset of the tab params). */
export interface PaneParams {
  fsId?: string
  connectionId?: string
  sessionId?: string
  local?: boolean
  quick?: QuickSpec
  protocol?: Protocol
  path?: string
}

/** Sortable columns of the file list. */
export type ColumnId = 'name' | 'size' | 'mtime' | 'owner' | 'group' | 'perm'

export interface SortSpec {
  by: ColumnId
  desc: boolean
}

/** What double-clicking a file does. */
export type DoubleClickAction = 'edit' | 'preview' | 'download'

/** Conflict policy for uploads and copies (SPEC §6.0 TransferRequest.overwrite minus 'resume'). */
export type ConflictPolicy = 'ask' | 'overwrite' | 'skip' | 'rename'

export interface Bookmark {
  path: string
  label?: string
}

/** A row of the file list: a FileEntry, or the synthetic ".." row. */
export interface ListRow {
  entry: FileEntry
  /** The ".." row (go to the parent folder). */
  parent?: boolean
}

/** Everything a view needs to act on its file system. */
export interface FsContext {
  /** Handle registry key (fsHandles.sourceKey): withFs(key, ...) survives a reopened handle. */
  key: string
  handle: FsHandleEx
  source: FsSource
  /** Runtime session behind the file system (session sources): "Open terminal here", follow cwd. */
  sessionId?: string
  /** Bookmarks / recent paths key (`conn:<id>`, `host:<user@host:port>`, `local`...). */
  placeKey: string
  /** Human label of the place ("test@ssh1", "Local files"). */
  label: string
  /** For scp:// URLs. */
  host?: { host: string; port?: number; username?: string }
}
