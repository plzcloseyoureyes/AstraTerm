/*
 * Terminal fonts (TERM-5): the bundled JetBrains Mono (offline, @fontsource-variable) plus common system monospace
 * fonts, availability detection, font loading before xterm measures cells, and cell-size estimates used to size new
 * sessions before their terminal exists.
 */
import { BUNDLED_FONT_FAMILY } from './settings'

export interface FontOption {
  label: string
  /** CSS font-family value. */
  family: string
  bundled?: boolean
}

const SYSTEM_MONO = [
  'SF Mono',
  'Menlo',
  'Monaco',
  'Consolas',
  'Cascadia Code',
  'Cascadia Mono',
  'Fira Code',
  'Fira Mono',
  'Source Code Pro',
  'Hack',
  'Iosevka',
  'IBM Plex Mono',
  'Roboto Mono',
  'Ubuntu Mono',
  'DejaVu Sans Mono',
  'Liberation Mono',
  'Noto Sans Mono',
  'Droid Sans Mono',
  'Inconsolata',
  'Lucida Console',
  'Andale Mono',
  'Courier New',
  'MesloLGS NF',
  'JetBrainsMono Nerd Font',
  'FiraCode Nerd Font',
  'Hack Nerd Font',
]

/** Quote a family name for CSS when needed. */
export function cssFamily(name: string): string {
  return /^[\w-]+$/.test(name) && !/^\d/.test(name) ? name : `"${name.replace(/["\\]/g, '')}"`
}

/** Font choices: bundled first, then installed system fonts (detected), generic monospace last. */
export function fontOptions(): FontOption[] {
  const out: FontOption[] = [{ label: 'JetBrains Mono (bundled)', family: BUNDLED_FONT_FAMILY, bundled: true }]
  for (const name of SYSTEM_MONO) {
    if (isFontAvailable(name)) out.push({ label: name, family: `${cssFamily(name)}, monospace` })
  }
  out.push({ label: 'System monospace', family: 'ui-monospace, monospace' })
  return out
}

let canvas: HTMLCanvasElement | null = null

function ctx2d(): CanvasRenderingContext2D | null {
  try {
    canvas ??= document.createElement('canvas')
    return canvas.getContext('2d')
  } catch {
    return null
  }
}

const availability = new Map<string, boolean>()

/**
 * Detect an installed font by comparing text widths against generic fallbacks (document.fonts.check() reports true
 * for any unknown local family, so it cannot be used for this).
 */
function isFontAvailable(name: string): boolean {
  const cached = availability.get(name)
  if (cached !== undefined) return cached
  const c = ctx2d()
  if (!c) return false
  const sample = 'mmmmmmmmmmlli10OO@#WwiI'
  let found = false
  for (const generic of ['monospace', 'serif', 'sans-serif']) {
    c.font = `72px ${generic}`
    const base = c.measureText(sample).width
    c.font = `72px ${cssFamily(name)}, ${generic}`
    if (c.measureText(sample).width !== base) {
      found = true
      break
    }
  }
  availability.set(name, found)
  return found
}

/** Installed monospace-looking fonts through the Local Font Access API (Chromium; asks for permission). */
export async function queryInstalledMonospaceFonts(): Promise<string[] | null> {
  const q = (window as unknown as { queryLocalFonts?: () => Promise<{ family: string }[]> }).queryLocalFonts
  if (typeof q !== 'function') return null
  try {
    const fonts = await q()
    const families = new Set<string>()
    for (const f of fonts) {
      if (/mono|code|console|courier|term|nerd|fixed|iosevka|hack|menlo|monaco|consolas/i.test(f.family)) families.add(f.family)
    }
    return Array.from(families).sort((a, b) => a.localeCompare(b))
  } catch {
    return null
  }
}

/**
 * Make sure the terminal font is loaded before xterm measures its cells (TERM-5); resolves after at most `timeoutMs`
 * (a missing font must never block the terminal).
 */
export async function ensureFontLoaded(family: string, size: number, weights: string[], doc: Document = document, timeoutMs = 1500): Promise<void> {
  const fonts = doc.fonts
  if (!fonts?.load) return
  const loads = weights.map((w) => fonts.load(`${normalizeWeight(w)} ${size}px ${family}`).catch(() => []))
  await Promise.race([Promise.all(loads), new Promise((r) => setTimeout(r, timeoutMs))])
}

function normalizeWeight(w: string): string {
  return w === 'normal' ? '400' : w === 'bold' ? '700' : w
}

/** Estimated cell size in CSS px for a font (used before a terminal exists, e.g. to size new sessions). */
export function estimateCell(family: string, size: number, lineHeight: number, letterSpacing: number): { width: number; height: number } {
  const c = ctx2d()
  let width = size * 0.6
  if (c) {
    c.font = `${size}px ${family}`
    const w = c.measureText('W'.repeat(20)).width / 20
    if (w > 0) width = w
  }
  const charHeight = Math.ceil(size * 1.32)
  return { width: width + letterSpacing, height: Math.max(1, Math.floor(charHeight * lineHeight)) }
}
