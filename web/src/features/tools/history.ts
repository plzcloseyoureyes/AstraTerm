/*
 * Recent tool runs, kept per browser *and user* in localStorage, used to refill a tool's form quickly. Secrets never
 * reach storage: passphrases, communities, SecureOn passwords, HTTP headers and request bodies are dropped, and
 * credentials embedded in URLs (user:password@host) are stripped from both the parameters and the label.
 */
import { useSyncExternalStore } from 'react'
import { useAuthStore } from '@/stores/auth'
import { storage, uid } from '@/lib/utils'
import type { ToolRun } from './types'

const PREFIX = 'astraterm:tools:history:v2:'
const MAX = 40
const SECRET_KEYS = new Set(['authPass', 'privPass', 'passphrase', 'password', 'community', 'secureOn', 'headers', 'body', 'contextName'])

let cacheKey = ''
let cache: ToolRun[] = []
const listeners = new Set<() => void>()

function storageKey(): string {
  return PREFIX + (useAuthStore.getState().user?.id ?? 'anonymous')
}

function load(): ToolRun[] {
  const key = storageKey()
  if (key !== cacheKey) {
    cacheKey = key
    const raw = storage.get<unknown>(key, [])
    cache = Array.isArray(raw) ? (raw as ToolRun[]).filter((r) => r && typeof r.tool === 'string') : []
  }
  return cache
}

function emit(): void {
  storage.set(cacheKey, cache)
  for (const l of Array.from(listeners)) l()
}

/** Remove user:password@ from URLs anywhere in a string. */
export function stripCredentials(s: string): string {
  return s.replace(/([a-z][a-z0-9+.-]*:\/\/)[^/@\s]*@/gi, '$1')
}

/** Record a run (secrets scrubbed). */
export function addRun(tool: string, params: Record<string, unknown>, label: string): void {
  load()
  const clean: Record<string, unknown> = {}
  for (const [k, v] of Object.entries(params)) {
    if (SECRET_KEYS.has(k) || v === undefined || v === '' || v === null) continue
    clean[k] = typeof v === 'string' ? stripCredentials(v) : v
  }
  const safeLabel = stripCredentials(label).slice(0, 120)
  const run: ToolRun = { id: uid('run'), tool, params: clean, label: safeLabel, at: Date.now() }
  cache = [run, ...cache.filter((r) => !(r.tool === tool && r.label === safeLabel))].slice(0, MAX)
  emit()
}

export function clearHistory(tool?: string): void {
  load()
  cache = tool ? cache.filter((r) => r.tool !== tool) : []
  emit()
}

function subscribe(cb: () => void): () => void {
  listeners.add(cb)
  // A different user signing in switches the storage key.
  const unsub = useAuthStore.subscribe((s, prev) => {
    if (s.user?.id !== prev.user?.id) cb()
  })
  return () => {
    listeners.delete(cb)
    unsub()
  }
}

export function useToolHistory(tool?: string): ToolRun[] {
  const all = useSyncExternalStore(subscribe, load, load)
  return tool ? all.filter((r) => r.tool === tool) : all
}
