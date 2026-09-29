/* Recently opened remote files (Tools → Recent files). Per browser and user, newest first. */
import { storage } from '@/lib/utils'
import { currentUserId } from './docstore'
import type { RecentFile } from './types'

const LEGACY_KEY = 'termstead:editor:recent:v1'
const MAX = 15

const keyOf = () => `${LEGACY_KEY}:${currentUserId()}`

function valid(v: unknown): RecentFile[] {
  return Array.isArray(v) ? v.filter((r) => r && typeof r.fsId === 'string' && typeof r.path === 'string') : []
}

export function listRecent(): RecentFile[] {
  const own = storage.get<RecentFile[] | null>(keyOf(), null)
  if (own) return valid(own)
  // The former browser-wide list goes to the first user who looks at it.
  const legacy = valid(storage.get<RecentFile[]>(LEGACY_KEY, []))
  storage.remove(LEGACY_KEY)
  if (legacy.length) storage.set(keyOf(), legacy)
  return legacy
}

export function addRecent(entry: Omit<RecentFile, 'openedAt'>): void {
  const list = listRecent().filter((r) => !(r.path === entry.path && (r.fsId === entry.fsId || (!!r.label && r.label === entry.label))))
  list.unshift({ ...entry, openedAt: Date.now() })
  storage.set(keyOf(), list.slice(0, MAX))
}

export function clearRecent(): void {
  storage.remove(keyOf())
}
