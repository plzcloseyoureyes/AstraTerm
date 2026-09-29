/*
 * Settings section `files` (Settings → Files & SFTP) and per-place bookmarks / recent folders.
 * Bookmarks are server-side settings (synced across browsers); recent folders stay in this browser (localStorage),
 * since they change on every navigation.
 */
import { defineSettings } from '@/stores/settings'
import { storage } from '@/lib/utils'
import type { Bookmark, ColumnId, ConflictPolicy, DoubleClickAction } from './types'

export interface FilesSettings {
  /** Switch the left sidebar to the SFTP panel when an SSH session connects in the active tab. */
  autoShowPanel: boolean
  /** Default of "Follow terminal folder" for SSH sessions (a connection's `followCwd: false` turns it off). */
  followTerminal: boolean
  showHidden: boolean
  /** List "<name>.termstead-part" files (uploads / transfers in progress or interrupted); hidden by default. */
  showPartialUploads: boolean
  confirmDelete: boolean
  /** Double-click on a file. */
  doubleClickAction: DoubleClickAction
  /** Files uploaded at the same time. */
  uploadParallelism: number
  foldersFirst: boolean
  /** What to do when uploaded / copied items already exist. */
  conflictPolicy: ConflictPolicy
  /** Open the transfer queue when a transfer starts. */
  openQueueOnTransfer: boolean
  /** Column visibility chosen by the user (narrow views hide more automatically). */
  columns: Partial<Record<ColumnId, boolean>>
  /** Column widths (px). */
  columnSizes: Partial<Record<ColumnId, number>>
  sortBy: ColumnId
  sortDesc: boolean
  /** Bookmarked folders per place (`conn:<id>`, `host:<user@host:port>`, `local`). */
  bookmarks: Record<string, Bookmark[]>
}

export const filesSettings = defineSettings<FilesSettings>('files', {
  autoShowPanel: true,
  followTerminal: true,
  showHidden: false,
  showPartialUploads: false,
  confirmDelete: true,
  doubleClickAction: 'edit',
  uploadParallelism: 3,
  foldersFirst: true,
  conflictPolicy: 'ask',
  openQueueOnTransfer: false,
  columns: {},
  columnSizes: {},
  sortBy: 'name',
  sortDesc: false,
  bookmarks: {},
})

export const MAX_UPLOAD_PARALLELISM = 8

export function uploadParallelism(): number {
  const n = Number(filesSettings.get().uploadParallelism)
  return Number.isFinite(n) ? Math.min(MAX_UPLOAD_PARALLELISM, Math.max(1, Math.round(n))) : 3
}

// ---------------------------------------------------------------------------------------------------------------------
// bookmarks
// ---------------------------------------------------------------------------------------------------------------------

export function getBookmarks(placeKey: string): Bookmark[] {
  const all = filesSettings.get().bookmarks
  const list = all && typeof all === 'object' ? all[placeKey] : undefined
  return Array.isArray(list) ? list.filter((b): b is Bookmark => !!b && typeof b.path === 'string') : []
}

export function isBookmarked(placeKey: string, path: string): boolean {
  return getBookmarks(placeKey).some((b) => b.path === path)
}

export function toggleBookmark(placeKey: string, path: string): boolean {
  const list = getBookmarks(placeKey)
  const has = list.some((b) => b.path === path)
  const next = has ? list.filter((b) => b.path !== path) : [...list, { path }]
  filesSettings.set((s) => ({ bookmarks: { ...s.bookmarks, [placeKey]: next } }))
  return !has
}

export function removeBookmark(placeKey: string, path: string): void {
  const next = getBookmarks(placeKey).filter((b) => b.path !== path)
  filesSettings.set((s) => ({ bookmarks: { ...s.bookmarks, [placeKey]: next } }))
}

// ---------------------------------------------------------------------------------------------------------------------
// recent folders (browser-local)
// ---------------------------------------------------------------------------------------------------------------------

const RECENT_KEY = 'termstead:files:recent:v1'
const MAX_RECENT = 12
const MAX_PLACES = 60

type RecentMap = Record<string, { paths: string[]; at: number }>

export function getRecentPaths(placeKey: string): string[] {
  const all = storage.get<RecentMap>(RECENT_KEY, {})
  const r = all[placeKey]
  return r && Array.isArray(r.paths) ? r.paths.filter((p) => typeof p === 'string') : []
}

export function pushRecentPath(placeKey: string, path: string): void {
  if (!placeKey || !path) return
  const all = storage.get<RecentMap>(RECENT_KEY, {})
  const prev = all[placeKey]?.paths ?? []
  if (prev[0] === path) return
  all[placeKey] = { paths: [path, ...prev.filter((p) => p !== path)].slice(0, MAX_RECENT), at: Date.now() }
  const keys = Object.keys(all)
  if (keys.length > MAX_PLACES) {
    keys.sort((a, b) => (all[a]?.at ?? 0) - (all[b]?.at ?? 0))
    for (const k of keys.slice(0, keys.length - MAX_PLACES)) delete all[k]
  }
  storage.set(RECENT_KEY, all)
}

export function clearRecentPaths(): void {
  storage.remove(RECENT_KEY)
}
