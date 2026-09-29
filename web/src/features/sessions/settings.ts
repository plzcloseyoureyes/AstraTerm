/*
 * Session manager preferences (settings section `sessions`, persisted server-side per user).
 */
import { defineSettings } from '@/stores/settings'
import type { GroupBy, SortMode } from './types'

export interface SessionsSettings {
  /** Folder ids expanded in the session tree. */
  expanded: string[]
  sortMode: SortMode
  /** Folder tree (default) or groups by protocol / tag. */
  groupBy: GroupBy
  /** Collapsed groups of the protocol / tag views (`protocol:ssh`, `tag:prod`). */
  collapsedGroups: string[]
  /** Filter chip row visible. */
  showFilters: boolean
  /** Favorites / Recent sections shown above the tree. */
  showFavorites: boolean
  showRecent: boolean
  /** Section open (expanded) state. */
  favoritesOpen: boolean
  recentOpen: boolean
  /** Recent quick-connect strings (passwords stripped), newest first. */
  quickConnectHistory: string[]
  /** Protocol pre-selected in a new session editor. */
  lastProtocol: string
  /** Ask before opening more than this many sessions at once ("Connect all"). 0 = never ask. */
  connectAllConfirm: number
}

export const sessionsSettings = defineSettings<SessionsSettings>('sessions', {
  expanded: [],
  sortMode: 'manual',
  groupBy: 'folder',
  collapsedGroups: [],
  showFilters: false,
  showFavorites: true,
  showRecent: true,
  favoritesOpen: true,
  recentOpen: true,
  quickConnectHistory: [],
  lastProtocol: 'ssh',
  connectAllConfirm: 5,
})

export const MAX_QUICK_HISTORY = 30

/** Remember a (sanitised) quick-connect string. */
export function rememberQuickConnect(text: string): void {
  const t = text.trim()
  if (!t) return
  sessionsSettings.set((s) => ({
    quickConnectHistory: [t, ...(Array.isArray(s.quickConnectHistory) ? s.quickConnectHistory : []).filter((h) => h !== t)].slice(
      0,
      MAX_QUICK_HISTORY,
    ),
  }))
}

export function getQuickConnectHistory(): string[] {
  const h = sessionsSettings.get().quickConnectHistory
  return Array.isArray(h) ? h.filter((x): x is string => typeof x === 'string') : []
}
