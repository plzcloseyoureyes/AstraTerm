/*
 * Tiny IndexedDB store for editor documents that are not files on a server: scratch buffers ("text" tabs), local files
 * opened from this computer (with their File System Access handle when the browser supports it), diff sides and
 * crash/unsaved-change backups of remote buffers ("hot exit"). Falls back to memory when IndexedDB is unavailable.
 *
 * One database per signed-in user ("astraterm-editor:<userId>"): people sharing a browser profile (server mode) never
 * see — or garbage-collect — each other's documents and backups. Documents of the former shared database
 * ("astraterm-editor") move to the first user whose tab asks for them; its stale backups are dropped.
 */
import { useAuthStore } from '@/stores/auth'

export interface DocRecord {
  id: string
  /** Text content (text documents). */
  content?: string
  /** Raw bytes (hex documents). */
  bytes?: Uint8Array
  /** File System Access API handle of a local file (structured-cloneable). */
  handle?: unknown
  /** Free-form metadata (encoding, eol, base mtime, ...). */
  meta?: Record<string, unknown>
  updatedAt: number
}

const LEGACY_DB = 'astraterm-editor'
const STORE = 'docs'

const dbs = new Map<string, Promise<IDBDatabase | null>>()
const memory = new Map<string, DocRecord>()
/** Documents created in this page but maybe not yet committed (tabs can mount before the write lands). */
const pending = new Map<string, DocRecord>()

/** The signed-in user's id ("anonymous" before sign-in). */
export function currentUserId(): string {
  return useAuthStore.getState().user?.id ?? 'anonymous'
}

function dbName(user = currentUserId()): string {
  return `${LEGACY_DB}:${user}`
}

/** Keys of the in-memory maps are scoped like the databases. */
const key = (id: string) => `${currentUserId()}\u0000${id}`

function openDb(name = dbName()): Promise<IDBDatabase | null> {
  let p = dbs.get(name)
  if (p) return p
  p = new Promise((resolve) => {
    try {
      if (typeof indexedDB === 'undefined') return resolve(null)
      const req = indexedDB.open(name, 1)
      req.onupgradeneeded = () => {
        const db = req.result
        if (!db.objectStoreNames.contains(STORE)) db.createObjectStore(STORE, { keyPath: 'id' })
      }
      req.onsuccess = () => resolve(req.result)
      req.onerror = () => resolve(null)
      req.onblocked = () => resolve(null)
    } catch {
      resolve(null)
    }
  })
  dbs.set(name, p)
  return p
}

function run<T>(db: IDBDatabase, mode: IDBTransactionMode, fn: (store: IDBObjectStore) => IDBRequest<T>): Promise<T | undefined> {
  return new Promise<T | undefined>((resolve, reject) => {
    try {
      const tx = db.transaction(STORE, mode)
      const req = fn(tx.objectStore(STORE))
      req.onsuccess = () => resolve(req.result)
      req.onerror = () => reject(req.error ?? new Error('IndexedDB request failed'))
    } catch (err) {
      reject(err instanceof Error ? err : new Error(String(err)))
    }
  })
}

let legacyKnown: Promise<boolean> | null = null

/** Does the former shared database exist? (Opening it would create it.) */
function legacyExists(): Promise<boolean> {
  if (!legacyKnown) {
    legacyKnown =
      typeof indexedDB !== 'undefined' && typeof indexedDB.databases === 'function'
        ? indexedDB.databases().then(
            (list) => list.some((d) => d.name === LEGACY_DB),
            () => true,
          )
        : Promise.resolve(typeof indexedDB !== 'undefined')
  }
  return legacyKnown
}

/** A document of the former shared database, moved into the current user's (null when there is none). */
async function claimLegacy(id: string): Promise<DocRecord | undefined> {
  if (!(await legacyExists())) return undefined
  // Backups are per file, not per tab: another user's unsaved changes must not surface — drop them instead.
  const legacy = await openDb(LEGACY_DB)
  if (!legacy) return undefined
  try {
    const rec = (await run<DocRecord>(legacy, 'readonly', (s) => s.get(id) as IDBRequest<DocRecord>)) ?? undefined
    if (!rec) return undefined
    await run(legacy, 'readwrite', (s) => s.delete(id))
    if (id.startsWith('backup:')) return undefined
    await putDoc(rec)
    return rec
  } catch {
    return undefined
  }
}

export async function getDoc(id: string): Promise<DocRecord | undefined> {
  const p = pending.get(key(id))
  if (p) return p
  const db = await openDb()
  if (!db) return memory.get(key(id))
  try {
    const rec = (await run<DocRecord>(db, 'readonly', (s) => s.get(id) as IDBRequest<DocRecord>)) ?? undefined
    return rec ?? (await claimLegacy(id))
  } catch {
    return memory.get(key(id))
  }
}

export async function putDoc(rec: Omit<DocRecord, 'updatedAt'> & { updatedAt?: number }): Promise<void> {
  const full: DocRecord = { ...rec, updatedAt: rec.updatedAt ?? Date.now() }
  const k = key(full.id)
  pending.set(k, full)
  try {
    const db = await openDb()
    if (!db) {
      memory.set(k, full)
      return
    }
    await run(db, 'readwrite', (s) => s.put(full))
  } catch (err) {
    memory.set(k, full)
    console.warn('[editor] could not persist document', err)
  } finally {
    if (pending.get(k) === full) pending.delete(k)
  }
}

export async function deleteDoc(id: string): Promise<void> {
  pending.delete(key(id))
  memory.delete(key(id))
  const db = await openDb()
  if (!db) return
  try {
    await run(db, 'readwrite', (s) => s.delete(id))
  } catch {
    /* ignore */
  }
}

async function listIn(db: IDBDatabase): Promise<{ id: string; updatedAt: number }[]> {
  return new Promise((resolve) => {
    const out: { id: string; updatedAt: number }[] = []
    try {
      const req = db.transaction(STORE, 'readonly').objectStore(STORE).openCursor()
      req.onsuccess = () => {
        const cur = req.result
        if (!cur) return resolve(out)
        const v = cur.value as DocRecord
        out.push({ id: v.id, updatedAt: v.updatedAt ?? 0 })
        cur.continue()
      }
      req.onerror = () => resolve(out)
    } catch {
      resolve(out)
    }
  })
}

/** The current user's document ids with their update time (for garbage collection). */
export async function listDocs(): Promise<{ id: string; updatedAt: number }[]> {
  const db = await openDb()
  if (!db) {
    const prefix = `${currentUserId()}\u0000`
    return Array.from(memory.entries())
      .filter(([k]) => k.startsWith(prefix))
      .map(([, d]) => ({ id: d.id, updatedAt: d.updatedAt }))
  }
  return listIn(db)
}

/** Housekeeping of the former shared database: backups and anything unclaimed for `maxAgeMs` go away. */
export async function pruneLegacy(maxAgeMs: number): Promise<void> {
  if (!(await legacyExists())) return
  const legacy = await openDb(LEGACY_DB)
  if (!legacy) return
  const now = Date.now()
  let left = 0
  for (const d of await listIn(legacy)) {
    if (!d.id.startsWith('backup:') && now - d.updatedAt < maxAgeMs) {
      left++
      continue
    }
    try {
      await run(legacy, 'readwrite', (s) => s.delete(d.id))
    } catch {
      left++
    }
  }
  if (left === 0) {
    legacy.close()
    dbs.delete(LEGACY_DB)
    legacyKnown = Promise.resolve(false)
    try {
      indexedDB.deleteDatabase(LEGACY_DB)
    } catch {
      /* ignore */
    }
  }
}

/** Backup key of an unsaved remote buffer. */
export function backupId(fsId: string, path: string): string {
  return `backup:${fsId}:${path}`
}
