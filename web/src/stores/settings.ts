/*
 * User settings: loaded from GET /api/settings (user merged over global), written back with a debounced PUT.
 *
 * Settings are namespaced by top-level section key; each feature owns its section and declares defaults:
 *
 *   export const terminalSettings = defineSettings('terminal', { fontSize: 13, cursorStyle: 'block' as const })
 *   const s = terminalSettings.use()          // merged {defaults, stored} — re-renders on change
 *   terminalSettings.set({ fontSize: 14 })     // optimistic; persisted after 400ms
 *   terminalSettings.get() / .replace() / .reset() / .subscribe(cb)
 *
 * The backend applies PUT bodies as RFC 7396 JSON merge patches (objects merge deeply, null deletes), so writes are
 * sent as a diff against the last value the server acknowledged: removed keys become null.
 */
import { useMemo } from 'react'
import { create } from 'zustand'
import { toast } from 'sonner'
import { getSettings, putSettings } from '@/api/settings'
import type { SettingsObject } from '@/api/types'
import { applyAppearance, DEFAULT_ACCENT, type Density, type ThemeMode } from '@/lib/theme'
import { errorMessage, isPlainObject, jsonEqual } from '@/lib/utils'

interface SettingsStore {
  values: SettingsObject
  loaded: boolean
  loading: boolean
  error?: string
}

export const useSettingsStore = create<SettingsStore>(() => ({ values: {}, loaded: false, loading: false }))

const dirty = new Set<string>()
/** Section values as last acknowledged by the server (base for merge-patch diffs). */
let serverValues: SettingsObject = {}
let flushTimer: ReturnType<typeof setTimeout> | null = null
let failureToastShown = false
const FLUSH_DELAY = 400

/**
 * RFC 7396 merge patch turning `prev` into `next`: keys missing from `next` become null, nested objects are diffed,
 * everything else is replaced. Returns undefined when nothing changed.
 */
function mergePatchDiff(prev: unknown, next: unknown): unknown {
  if (next === undefined || next === null) return prev === undefined || prev === null ? undefined : null
  if (!isPlainObject(next) || !isPlainObject(prev)) return jsonEqual(prev, next) ? undefined : next
  const out: Record<string, unknown> = {}
  for (const k of Object.keys(prev)) if (!(k in next) || next[k] === undefined) out[k] = null
  for (const [k, v] of Object.entries(next)) {
    if (v === undefined) continue
    const d = mergePatchDiff(prev[k], v)
    if (d !== undefined) out[k] = d
  }
  return Object.keys(out).length ? out : undefined
}

function clone<T>(v: T): T {
  return v === undefined ? v : (JSON.parse(JSON.stringify(v)) as T)
}

async function flush(keepalive = false): Promise<void> {
  if (flushTimer) clearTimeout(flushTimer)
  flushTimer = null
  if (!dirty.size) return
  const values = useSettingsStore.getState().values
  const patch: SettingsObject = {}
  const sent: SettingsObject = {}
  for (const key of dirty) {
    const d = mergePatchDiff(serverValues[key], values[key])
    if (d !== undefined) patch[key] = d
    sent[key] = clone(values[key])
  }
  dirty.clear()
  if (!Object.keys(patch).length) return
  try {
    if (keepalive) {
      // Page is going away: fire-and-forget with keepalive so the write survives unload.
      void fetch('/api/settings', {
        method: 'PUT',
        credentials: 'same-origin',
        keepalive: true,
        headers: { 'Content-Type': 'application/json', 'X-AstraTerm': '1' },
        body: JSON.stringify(patch),
      }).catch(() => undefined)
      return
    }
    const res = await putSettings(patch)
    // The server answers with the effective settings (user merged over global). Adopt them as the new diff base and,
    // for sections not edited again while the request was in flight, as the local value too (so a reset key shows
    // the inherited global value instead of the hard-coded default).
    const effective = isPlainObject(res) ? res : null
    const adopt: SettingsObject = {}
    for (const [k, v] of Object.entries(sent)) {
      const truth = effective ? effective[k] : v
      if (truth === undefined) delete serverValues[k]
      else serverValues[k] = clone(truth)
      if (effective && !dirty.has(k) && !jsonEqual(useSettingsStore.getState().values[k], truth)) adopt[k] = clone(truth)
    }
    if (Object.keys(adopt).length) useSettingsStore.setState((s) => ({ values: { ...s.values, ...adopt } }))
    failureToastShown = false
  } catch (err) {
    // Keep them dirty so the next change retries.
    for (const key of Object.keys(patch)) dirty.add(key)
    if (!failureToastShown) {
      failureToastShown = true
      toast.error('Could not save settings', { description: errorMessage(err) })
    }
  }
}

function scheduleFlush(): void {
  if (flushTimer) clearTimeout(flushTimer)
  flushTimer = setTimeout(() => void flush(), FLUSH_DELAY)
}

if (typeof window !== 'undefined') {
  window.addEventListener('pagehide', () => {
    if (dirty.size && useSettingsStore.getState().loaded) void flush(true)
  })
}

/** Load settings from the server (after login). */
export async function loadSettings(): Promise<void> {
  useSettingsStore.setState({ loading: true, error: undefined })
  try {
    const raw = await getSettings()
    const values = isPlainObject(raw) ? raw : {}
    serverValues = clone(values)
    useSettingsStore.setState({ values, loaded: true, loading: false })
  } catch (err) {
    // Fall back to defaults; the app stays usable and later writes will retry.
    useSettingsStore.setState({ loaded: true, loading: false, error: errorMessage(err) })
  }
}

/** Forget everything (after sign-out). Call flushSettings() before signing out to keep pending changes. */
export function resetSettingsStore(): void {
  if (flushTimer) clearTimeout(flushTimer)
  flushTimer = null
  dirty.clear()
  serverValues = {}
  useSettingsStore.setState({ values: {}, loaded: false, loading: false, error: undefined })
}

/** Raw write of a whole section value (prefer defineSettings(...).set). */
function setSettingsSection(section: string, value: unknown): void {
  const prev = useSettingsStore.getState().values[section]
  if (jsonEqual(prev, value)) return
  useSettingsStore.setState((s) => ({ values: { ...s.values, [section]: value } }))
  dirty.add(section)
  scheduleFlush()
}

export function flushSettings(): Promise<void> {
  return flush()
}

// ---------------------------------------------------------------------------------------------------------------------
// defineSettings — typed, namespaced sections with defaults
// ---------------------------------------------------------------------------------------------------------------------

export interface SettingsSection<T extends object> {
  readonly section: string
  readonly defaults: Readonly<T>
  /** Current merged value (defaults ← stored). */
  get(): T
  /** React hook: merged value. */
  use(): T
  /** React hook: a single key. */
  useValue<K extends keyof T>(key: K): T[K]
  /** Shallow-merge a patch into the section and persist. */
  set(patch: Partial<T> | ((prev: T) => Partial<T>)): void
  /** Replace the whole stored section (keys missing from `next` fall back to defaults). */
  replace(next: Partial<T>): void
  /** Reset the whole section, or some keys: the user's values are removed, so they inherit (global, else default). */
  reset(keys?: (keyof T)[]): void
  /** Observe changes of the merged value. */
  subscribe(cb: (value: T) => void): () => void
}

const registeredDefaults = new Map<string, object>()

function mergeSection<T extends object>(defaults: T, raw: unknown): T {
  if (!isPlainObject(raw)) return { ...defaults }
  const out = { ...defaults } as Record<string, unknown>
  for (const [k, v] of Object.entries(raw)) if (v !== undefined) out[k] = v
  return out as T
}

/** The section as stored (user values merged over admin global values by the server), without defaults. */
function rawSection(section: string): Record<string, unknown> {
  const raw = useSettingsStore.getState().values[section]
  return isPlainObject(raw) ? raw : {}
}

export function defineSettings<T extends object>(section: string, defaults: T): SettingsSection<T> {
  registeredDefaults.set(section, defaults)
  const get = () => mergeSection(defaults, useSettingsStore.getState().values[section])
  return {
    section,
    defaults,
    get,
    use() {
      const raw = useSettingsStore((s) => s.values[section])
      return useMemo(() => mergeSection(defaults, raw), [raw])
    },
    useValue<K extends keyof T>(key: K): T[K] {
      return useSettingsStore((s) => {
        const raw = s.values[section]
        if (isPlainObject(raw) && key in raw) return raw[key as string] as T[K]
        return defaults[key]
      })
    },
    set(patch) {
      const prev = get()
      const p = typeof patch === 'function' ? patch(prev) : patch
      // Persist only what is set: keys never touched stay absent, so the defaults and the admin's global settings
      // (GET /api/settings merges user over global) keep applying to them instead of being frozen into user scope.
      setSettingsSection(section, { ...rawSection(section), ...p })
    },
    replace(next) {
      setSettingsSection(section, { ...next })
    },
    reset(keys) {
      // Resetting removes the user's values, so the keys inherit again (admin global value, else the default).
      if (!keys) {
        setSettingsSection(section, undefined)
        return
      }
      const next = { ...rawSection(section) }
      for (const k of keys) delete next[k as string]
      setSettingsSection(section, next)
    },
    subscribe(cb) {
      let last = useSettingsStore.getState().values[section]
      return useSettingsStore.subscribe((s) => {
        const raw = s.values[section]
        if (raw === last) return
        last = raw
        cb(mergeSection(defaults, raw))
      })
    },
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// Core sections owned by the shell
// ---------------------------------------------------------------------------------------------------------------------

export interface AppearanceSettings {
  theme: ThemeMode
  accent: string
  /** UI zoom factor (1 = 100%). */
  uiScale: number
  density: Density
  /** Desktop app on macOS / Windows: below 1 the desktop shows through, blurred (lib/theme.ts). */
  windowOpacity: number
  showMenuBar: boolean
  showRibbon: boolean
  /** Ribbon shows icons only (compact mode). */
  ribbonCompact: boolean
  showSidebar: boolean
  showStatusBar: boolean
}

export const appearanceSettings = defineSettings<AppearanceSettings>('appearance', {
  theme: 'dark',
  accent: DEFAULT_ACCENT,
  uiScale: 1,
  density: 'comfortable',
  windowOpacity: 1,
  showMenuBar: true,
  // Tabs, quick connect and the app menu share the title bar; tool launchers sit in the sidebar rail.
  showRibbon: false,
  ribbonCompact: false,
  showSidebar: true,
  showStatusBar: true,
})

export interface GeneralSettings {
  /** Restore the previous tabs/splits on start (UI-13). */
  restoreWorkspace: boolean
  /** Ask before closing a tab with a running session. */
  confirmCloseRunning: boolean
  /** How links clicked in terminals are opened. */
  openLinks: 'newTab' | 'ask'
}

export const generalSettings = defineSettings<GeneralSettings>('general', {
  restoreWorkspace: true,
  // Closing a running session's tab offers Undo for a few seconds instead of asking first (docs/UX.md "Forgiving").
  confirmCloseRunning: false,
  openLinks: 'newTab',
})

export interface SecuritySettings {
  /** Lock the UI after this many idle minutes (0 = never). SEC-5. */
  autoLockMinutes: number
}

export const securitySettings = defineSettings<SecuritySettings>('security', {
  autoLockMinutes: 0,
})

/** A named workspace layout (tabs, splits, floating groups) the user saved to restore later. */
export interface SavedWorkspace {
  id: string
  name: string
  savedAt: string
  /** What it contains (shown before restoring). */
  tabs: { kind: string; title: string }[]
  /** Dockview's serialized layout. */
  layout: unknown
}

export const workspacesSettings = defineSettings<{ saved: SavedWorkspace[] }>('workspaces', { saved: [] })

/** User keybinding overrides: commandId → bindings (an empty array unbinds the command). */
export type KeybindingOverrides = Record<string, string[]>

export const keybindingSettings = defineSettings<KeybindingOverrides>('keybindings', {})

// Keep the document in sync with appearance settings.
appearanceSettings.subscribe((a) => applyAppearance(a))
useSettingsStore.subscribe((s, prev) => {
  if (s.loaded && !prev.loaded) applyAppearance(appearanceSettings.get())
})
