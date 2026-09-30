/*
 * History restore (Settings → Terminal → Restore history): every terminal tab keeps a snapshot of its recent output in
 * localStorage, keyed by tab id (tabs keep their id across restarts). When a restored tab finds its session gone, it
 * starts a new session and paints the snapshot first, like a native terminal app after a restart.
 */

const PREFIX = 'astraterm:history:v1:'
/** Per tab (UTF-16 chars); localStorage holds ~5M chars per origin, shared with everything else. */
const MAX_CHARS = 400_000
/** Scrollback lines kept in a snapshot. */
export const HISTORY_LINES = 1000

interface Entry {
  data: string
  savedAt: number
}

function store(): Storage | null {
  try {
    return window.localStorage
  } catch {
    return null
  }
}

export function loadHistory(tabId: string): string | null {
  try {
    const v = JSON.parse(store()?.getItem(PREFIX + tabId) ?? 'null') as Partial<Entry> | null
    return typeof v?.data === 'string' && v.data ? v.data : null
  } catch {
    return null
  }
}

/** Store a snapshot, evicting the oldest snapshots of other tabs when the quota is exceeded. */
export function saveHistory(tabId: string, data: string): void {
  const st = store()
  if (!st || data.length > MAX_CHARS) return
  const value = JSON.stringify({ data, savedAt: Date.now() } satisfies Entry)
  for (let attempt = 0; attempt < 4; attempt++) {
    try {
      st.setItem(PREFIX + tabId, value)
      return
    } catch {
      if (!evictOldest(st, tabId)) return
    }
  }
}

export function dropHistory(tabId: string): void {
  try {
    store()?.removeItem(PREFIX + tabId)
  } catch {
    /* ignore */
  }
}

function evictOldest(st: Storage, keep: string): boolean {
  let oldest: { key: string; at: number } | null = null
  for (let i = 0; i < st.length; i++) {
    const key = st.key(i)
    if (!key?.startsWith(PREFIX) || key === PREFIX + keep) continue
    let at = 0
    try {
      at = (JSON.parse(st.getItem(key) ?? '{}') as Partial<Entry>).savedAt ?? 0
    } catch {
      /* corrupt: evict first */
    }
    if (!oldest || at < oldest.at) oldest = { key, at }
  }
  if (!oldest) return false
  st.removeItem(oldest.key)
  return true
}

/** The marker painted under restored output. */
export const HISTORY_BANNER = '\r\n\x1b[0m\x1b[7m * History restored \x1b[0m\r\n\r\n'

/** Tabs whose next terminal should paint their saved history first (set when a gone session is restarted). */
export const pendingRestore = new Set<string>()
