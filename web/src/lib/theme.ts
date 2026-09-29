/*
 * Applies appearance settings to the document: light/dark/system theme class, accent colour, UI scale and density.
 * Also keeps extra documents (dockview pop-out windows) in sync.
 */
import { storage } from './utils'

export type ThemeMode = 'dark' | 'light' | 'system'
export type Density = 'comfortable' | 'compact'

export interface AppearanceLike {
  theme: ThemeMode
  accent: string
  uiScale: number
  density: Density
}

interface AccentPreset {
  label: string
  light: string
  dark: string
  /** Foreground on the accent in light / dark theme. */
  fgLight: string
  fgDark: string
  swatch: string
}

const WHITE = 'oklch(0.99 0 0)'

export const ACCENT_PRESETS: Record<string, AccentPreset> = {
  blue: { label: 'Blue', light: 'oklch(0.55 0.19 252)', dark: 'oklch(0.62 0.18 252)', fgLight: WHITE, fgDark: WHITE, swatch: '#3b82f6' },
  sky: {
    label: 'Sky',
    light: 'oklch(0.58 0.14 232)',
    dark: 'oklch(0.72 0.14 228)',
    fgLight: WHITE,
    fgDark: 'oklch(0.2 0.04 230)',
    swatch: '#0ea5e9',
  },
  violet: { label: 'Violet', light: 'oklch(0.54 0.22 292)', dark: 'oklch(0.64 0.2 292)', fgLight: WHITE, fgDark: WHITE, swatch: '#8b5cf6' },
  teal: {
    label: 'Teal',
    light: 'oklch(0.55 0.1 185)',
    dark: 'oklch(0.72 0.12 182)',
    fgLight: WHITE,
    fgDark: 'oklch(0.2 0.04 185)',
    swatch: '#14b8a6',
  },
  green: {
    label: 'Green',
    light: 'oklch(0.54 0.15 150)',
    dark: 'oklch(0.72 0.17 150)',
    fgLight: WHITE,
    fgDark: 'oklch(0.2 0.05 150)',
    swatch: '#22c55e',
  },
  amber: {
    label: 'Amber',
    light: 'oklch(0.68 0.16 70)',
    dark: 'oklch(0.8 0.16 78)',
    fgLight: 'oklch(0.2 0.04 70)',
    fgDark: 'oklch(0.2 0.04 70)',
    swatch: '#f59e0b',
  },
  orange: {
    label: 'Orange',
    light: 'oklch(0.62 0.19 45)',
    dark: 'oklch(0.72 0.18 50)',
    fgLight: WHITE,
    fgDark: 'oklch(0.2 0.05 50)',
    swatch: '#f97316',
  },
  rose: { label: 'Rose', light: 'oklch(0.57 0.21 12)', dark: 'oklch(0.66 0.2 12)', fgLight: WHITE, fgDark: WHITE, swatch: '#f43f5e' },
  slate: {
    label: 'Graphite',
    light: 'oklch(0.4 0.02 255)',
    dark: 'oklch(0.78 0.02 255)',
    fgLight: WHITE,
    fgDark: 'oklch(0.18 0.01 255)',
    swatch: '#64748b',
  },
}

export const DEFAULT_ACCENT = 'blue'

const HEX_RE = /^#([0-9a-f]{6}|[0-9a-f]{3})$/i

function hexLuminance(hex: string): number {
  let h = hex.slice(1)
  if (h.length === 3) h = h.replace(/./g, (c) => c + c)
  const n = parseInt(h, 16)
  const chan = (v: number) => {
    const c = v / 255
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4
  }
  return 0.2126 * chan((n >> 16) & 255) + 0.7152 * chan((n >> 8) & 255) + 0.0722 * chan(n & 255)
}

function resolveAccent(accent: string, dark: boolean): { bg: string; fg: string } {
  const preset = ACCENT_PRESETS[accent]
  if (preset) return { bg: dark ? preset.dark : preset.light, fg: dark ? preset.fgDark : preset.fgLight }
  if (HEX_RE.test(accent)) return { bg: accent, fg: hexLuminance(accent) > 0.45 ? 'oklch(0.18 0.01 255)' : WHITE }
  const d = ACCENT_PRESETS[DEFAULT_ACCENT]
  return { bg: dark ? d.dark : d.light, fg: dark ? d.fgDark : d.fgLight }
}

const media = typeof window !== 'undefined' ? window.matchMedia('(prefers-color-scheme: dark)') : null
const extraDocs = new Set<Document>()
let current: AppearanceLike = { theme: 'dark', accent: DEFAULT_ACCENT, uiScale: 1, density: 'comfortable' }
let resolvedDark = true
const listeners = new Set<(dark: boolean) => void>()

const CACHE_KEY = 'astraterm:appearance'

function applyTo(doc: Document): void {
  const root = doc.documentElement
  root.classList.toggle('dark', resolvedDark)
  root.style.colorScheme = resolvedDark ? 'dark' : 'light'
  const { bg, fg } = resolveAccent(current.accent, resolvedDark)
  root.style.setProperty('--primary', bg)
  root.style.setProperty('--primary-foreground', fg)
  const scale = Number.isFinite(current.uiScale) ? Math.min(2, Math.max(0.7, current.uiScale)) : 1
  root.style.setProperty('--ui-scale', String(scale))
  root.style.setProperty('--spacing', current.density === 'compact' ? '0.2rem' : '0.25rem')
  root.dataset.density = current.density
}

function refresh(): void {
  const dark = current.theme === 'system' ? (media?.matches ?? true) : current.theme === 'dark'
  const changed = dark !== resolvedDark
  resolvedDark = dark
  applyTo(document)
  for (const d of extraDocs) {
    try {
      applyTo(d)
    } catch {
      extraDocs.delete(d)
    }
  }
  const meta = document.querySelector('meta[name="theme-color"]')
  if (meta) meta.setAttribute('content', dark ? '#15181d' : '#f7f8fa')
  if (changed) for (const l of Array.from(listeners)) l(dark)
}

media?.addEventListener('change', () => {
  if (current.theme === 'system') refresh()
})

/** Apply appearance settings (idempotent) and cache them for the next page load (pre-login screens). */
export function applyAppearance(a: Partial<AppearanceLike>): void {
  current = { ...current, ...a }
  // public/boot.js reads this before the first paint (theme class, accent, scale): it gets the resolved accent colours
  // too, so it needs no copy of the presets.
  const dark = resolveAccent(current.accent, true)
  const light = resolveAccent(current.accent, false)
  storage.set(CACHE_KEY, { ...current, vars: { dark: { primary: dark.bg, fg: dark.fg }, light: { primary: light.bg, fg: light.fg } } })
  refresh()
}

/** Apply the cached appearance immediately at boot (before settings load) to avoid a theme flash. */
export function applyCachedAppearance(): void {
  const cached = storage.get<Partial<AppearanceLike> | null>(CACHE_KEY, null)
  if (cached && typeof cached === 'object') current = { ...current, ...cached }
  refresh()
}

/** Keep another document (e.g. a pop-out window) themed like the main one. Returns a disposer. */
export function trackDocument(doc: Document): () => void {
  extraDocs.add(doc)
  try {
    applyTo(doc)
  } catch {
    /* window closed */
  }
  return () => extraDocs.delete(doc)
}

export function isDarkTheme(): boolean {
  return resolvedDark
}

/** Subscribe to resolved dark/light changes (e.g. terminal themes that follow the UI). */
export function onThemeChange(cb: (dark: boolean) => void): () => void {
  listeners.add(cb)
  return () => listeners.delete(cb)
}
