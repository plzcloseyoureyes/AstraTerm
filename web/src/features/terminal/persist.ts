/*
 * Reload persistence (SPEC §6.2 delta attach): before the page unloads, every terminal stores a serialized snapshot of
 * its screen together with the stream offset it corresponds to, in sessionStorage (per browser tab). After a reload the
 * snapshot is painted immediately and the socket attaches with ?offset=<that offset>, so only the delta is replayed.
 *
 * A snapshot is only valid at a clean stream boundary (not inside an escape sequence or a UTF-8 character), otherwise
 * the delta would start mid-sequence; BoundaryScanner tracks that. Without a valid snapshot the terminal attaches with
 * offset 0 and the server replays its whole ring buffer.
 */

const PREFIX = 'astraterm:term:v1:'
/** Upper bound of one snapshot (UTF-16 chars) — sessionStorage quotas are ~5M chars per origin. */
const MAX_SNAPSHOT_CHARS = 1_500_000

export interface SavedTerminal {
  offset: number
  cols: number
  rows: number
  data: string
  savedAt: number
}

function storage(): Storage | null {
  try {
    return window.sessionStorage
  } catch {
    return null
  }
}

export function loadSavedTerminal(sessionId: string): SavedTerminal | null {
  const st = storage()
  if (!st) return null
  try {
    const raw = st.getItem(PREFIX + sessionId)
    if (!raw) return null
    const v = JSON.parse(raw) as Partial<SavedTerminal>
    if (
      typeof v.offset !== 'number' ||
      !Number.isSafeInteger(v.offset) ||
      v.offset < 0 ||
      typeof v.data !== 'string' ||
      typeof v.cols !== 'number' ||
      typeof v.rows !== 'number' ||
      v.cols < 2 ||
      v.rows < 1 ||
      v.cols > 1000 ||
      v.rows > 1000
    ) {
      st.removeItem(PREFIX + sessionId)
      return null
    }
    return { offset: v.offset, cols: Math.floor(v.cols), rows: Math.floor(v.rows), data: v.data, savedAt: Number(v.savedAt) || 0 }
  } catch {
    return null
  }
}

/** Store a snapshot; evicts older snapshots of other sessions when the quota is exceeded. */
export function saveTerminal(sessionId: string, snap: SavedTerminal): boolean {
  const st = storage()
  if (!st || snap.data.length > MAX_SNAPSHOT_CHARS) return false
  const value = JSON.stringify(snap)
  for (let attempt = 0; attempt < 4; attempt++) {
    try {
      st.setItem(PREFIX + sessionId, value)
      return true
    } catch {
      if (!evictOldest(st, sessionId)) break
    }
  }
  try {
    st.removeItem(PREFIX + sessionId)
  } catch {
    /* ignore */
  }
  return false
}

export function dropSavedTerminal(sessionId: string): void {
  try {
    storage()?.removeItem(PREFIX + sessionId)
  } catch {
    /* ignore */
  }
}

/** Remove snapshots of sessions that are not shown by any tab anymore. */
export function pruneSavedTerminals(keep: Set<string>): void {
  const st = storage()
  if (!st) return
  try {
    for (const key of allKeys(st)) {
      if (!keep.has(key.slice(PREFIX.length))) st.removeItem(key)
    }
  } catch {
    /* ignore */
  }
}

function allKeys(st: Storage): string[] {
  const keys: string[] = []
  for (let i = 0; i < st.length; i++) {
    const k = st.key(i)
    if (k && k.startsWith(PREFIX)) keys.push(k)
  }
  return keys
}

function evictOldest(st: Storage, except: string): boolean {
  let oldest: { key: string; at: number } | null = null
  for (const key of allKeys(st)) {
    if (key === PREFIX + except) continue
    let at = 0
    try {
      at = Number((JSON.parse(st.getItem(key) ?? '{}') as Partial<SavedTerminal>).savedAt) || 0
    } catch {
      /* corrupt entry: evict first */
    }
    if (!oldest || at < oldest.at) oldest = { key, at }
  }
  if (!oldest) return false
  st.removeItem(oldest.key)
  return true
}

// ---------------------------------------------------------------------------------------------------------------------
// Stream boundary scanner
// ---------------------------------------------------------------------------------------------------------------------

// Scanner states.
const Ground = 0
const Esc = 1
const EscInter = 2
const Csi = 3
const Str = 4
const StrEsc = 5
type St = 0 | 1 | 2 | 3 | 4 | 5

/**
 * Minimal VT byte-stream state machine: tells whether the stream, after the bytes fed so far, sits at a clean
 * boundary (ground state, no pending UTF-8 continuation bytes).
 */
export class BoundaryScanner {
  private st: St = Ground
  private utf8 = 0

  reset(): void {
    this.st = Ground
    this.utf8 = 0
  }

  /** Feed bytes; returns true when the stream ends at a clean boundary. */
  feed(data: Uint8Array): boolean {
    let st = this.st
    let utf8 = this.utf8
    for (let i = 0; i < data.length; i++) {
      const b = data[i]
      switch (st) {
        case Ground:
          if (utf8 > 0) {
            if ((b & 0xc0) === 0x80) {
              utf8--
              break
            }
            utf8 = 0 // invalid sequence: the decoder resynchronises on this byte
          }
          if (b === 0x1b) st = Esc
          else if (b >= 0xc2 && b <= 0xdf) utf8 = 1
          else if (b >= 0xe0 && b <= 0xef) utf8 = 2
          else if (b >= 0xf0 && b <= 0xf4) utf8 = 3
          break
        case Esc:
          if (b === 0x5b) st = Csi // [
          else if (b === 0x5d || b === 0x50 || b === 0x58 || b === 0x5e || b === 0x5f) st = Str // ] P X ^ _
          else if (b >= 0x20 && b <= 0x2f) st = EscInter
          else if (b === 0x1b) st = Esc
          else st = Ground
          break
        case EscInter:
          if (b >= 0x20 && b <= 0x2f) break
          st = b === 0x1b ? Esc : Ground
          break
        case Csi:
          if (b >= 0x40 && b <= 0x7e) st = Ground
          else if (b === 0x1b) st = Esc
          else if (b === 0x18 || b === 0x1a) st = Ground
          break
        case Str:
          if (b === 0x07 || b === 0x18 || b === 0x1a) st = Ground
          else if (b === 0x1b) st = StrEsc
          break
        case StrEsc:
          if (b === 0x5c) st = Ground // ST = ESC \
          else if (b === 0x1b) st = StrEsc
          else {
            // ESC + something else aborts the string and starts a new escape sequence.
            st = Esc
            i--
          }
          break
      }
    }
    this.st = st
    this.utf8 = utf8
    return st === Ground && utf8 === 0
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// Unload hook
// ---------------------------------------------------------------------------------------------------------------------

const persisters = new Set<() => void>()
let unloadHookInstalled = false

/** Run `fn` when the page is being unloaded (reload, navigation, close). Returns an unregister function. */
export function registerPersister(fn: () => void): () => void {
  persisters.add(fn)
  if (!unloadHookInstalled && typeof window !== 'undefined') {
    unloadHookInstalled = true
    window.addEventListener('pagehide', () => {
      for (const p of Array.from(persisters)) {
        try {
          p()
        } catch (err) {
          console.warn('[terminal] persist failed', err)
        }
      }
    })
  }
  return () => persisters.delete(fn)
}
