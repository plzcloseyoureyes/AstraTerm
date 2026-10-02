/*
 * Terminal settings (section `terminal` of the user settings, TERM-1/5/9/10/11/12/13/16/17/18/20/21/24/25).
 * Per-connection overrides live in `connection.options.terminal` (SPEC §5.3) and are merged over these values with
 * `effectiveTerminalSettings()`; unknown / ill-typed override keys are ignored.
 */
import type { TerminalOverrides } from '@/api/types'
import { isMac } from '@/lib/utils'
import { defineSettings } from '@/stores/settings'
import type { TerminalScheme } from './types'

type BellStyle = 'none' | 'visual' | 'sound' | 'both'
type RightClickAction = 'menu' | 'paste' | 'copy-or-paste'
type Osc52Policy = 'off' | 'write' | 'read-write'
type TitleMode = 'session' | 'osc' | 'both'
type CursorStyle = 'block' | 'underline' | 'bar'
type CursorInactiveStyle = 'outline' | 'block' | 'bar' | 'underline' | 'none'
type RendererPreference = 'auto' | 'dom'
type LinkModifier = 'none' | 'mod'
type CloseOnExit = 'never' | 'clean' | 'always'
type FontWeightSetting = 'normal' | 'bold' | '100' | '200' | '300' | '400' | '500' | '600' | '700' | '800' | '900'

export const BUNDLED_FONT_FAMILY = '"JetBrains Mono Variable", "JetBrains Mono", ui-monospace, Menlo, Consolas, monospace'

export interface TerminalSettings {
  // --- colours (TERM-9, TERM-11) ---
  /** Colour scheme id (built-in or `custom:<id>`); used with the dark UI theme (or always when matchAppTheme is off). */
  theme: string
  /** Scheme used while the UI theme is light (matchAppTheme). */
  lightTheme: string
  /** Switch between `theme` and `lightTheme` with the UI's light/dark theme. */
  matchAppTheme: boolean
  customSchemes: TerminalScheme[]
  minimumContrastRatio: number
  drawBoldTextInBrightColors: boolean
  /** 0.3..1 — terminal background opacity (1 = opaque). */
  backgroundOpacity: number
  // --- font (TERM-5) ---
  fontFamily: string
  fontSize: number
  fontWeight: FontWeightSetting
  fontWeightBold: FontWeightSetting
  lineHeight: number
  letterSpacing: number
  // --- cursor (TERM-12) ---
  cursorStyle: CursorStyle
  cursorBlink: boolean
  cursorInactiveStyle: CursorInactiveStyle
  cursorWidth: number
  // --- rendering / scrolling (TERM-4, TERM-13) ---
  padding: number
  renderer: RendererPreference
  scrollback: number
  smoothScrollDuration: number
  scrollSensitivity: number
  fastScrollSensitivity: number
  scrollOnUserInput: boolean
  unicodeVersion: '11' | '6'
  screenReaderMode: boolean
  /** Sixel / iTerm2 inline images (TERM-32). */
  images: boolean
  // --- title (TERM-24) ---
  titleMode: TitleMode
  // --- mouse & selection (TERM-16, TERM-23) ---
  copyOnSelect: boolean
  rightClickAction: RightClickAction
  middleClickPaste: boolean
  rightClickSelectsWord: boolean
  wordSeparator: string
  altClickMovesCursor: boolean
  /** Links open on plain click ('none') or with Ctrl/Cmd held ('mod'). */
  linkModifier: LinkModifier
  trimCopiedWhitespace: boolean
  // --- keyboard (TERM-19, TERM-25) ---
  macOptionIsMeta: boolean
  /** Alt/Option+Left/Right send word-jump sequences (ESC b / ESC f). */
  altArrowWordJump: boolean
  /** Ctrl/Cmd + =/−/0 and Ctrl+wheel zoom the terminal font. */
  ctrlZoom: boolean
  /** Alt+<key> always goes to the shell (app shortcuts using Alt are skipped while a terminal is focused). */
  altKeysToTerminal: boolean
  /** Extra keybindings (tinykeys syntax) that always go to the shell when a terminal is focused. */
  passthroughKeys: string[]
  // --- paste (TERM-17) ---
  pasteConfirmMultiline: boolean
  /** Skip the multi-line confirmation when the application enabled bracketed paste (it will not execute lines). */
  pasteSkipConfirmBracketed: boolean
  pasteConfirmDangerous: boolean
  /** Honour the application's bracketed paste mode (off = always paste raw). */
  bracketedPaste: boolean
  /** Per-line delay of "Paste line by line". */
  pasteLineDelayMs: number
  // --- clipboard (TERM-18, CC-20) ---
  osc52: Osc52Policy
  clipboardHistory: boolean
  // --- bell & notifications (TERM-20, TERM-21, TERM-22) ---
  bell: BellStyle
  /** Desktop notification for bells in background tabs. */
  bellNotify: boolean
  /** Desktop notification when a long command finishes in a background tab (OSC 133). */
  notifyLongCommands: boolean
  longCommandSeconds: number
  /** Silence threshold of "Notify on silence". */
  silenceSeconds: number
  /** OSC 9 / OSC 777 notifications from applications. */
  osc9Notifications: boolean
  // --- end of session (PROTO-39) ---
  showEndPrompt: boolean
  closeOnExit: CloseOnExit
  // --- conveniences (CC-19) ---
  /** Hide the mouse pointer over the terminal while typing. */
  hidePointerWhileTyping: boolean
  /** Moving the mouse into a terminal pane focuses it. */
  focusFollowsMouse: boolean
  /** Small per-terminal toolbar (find, split, MultiExec, logging) shown on hover. */
  showToolbar: boolean
  /** After a restart, a restored tab whose session is gone starts anew and shows its previous output (history.ts). */
  restoreHistory: boolean
}

export const TERMINAL_DEFAULTS: TerminalSettings = {
  theme: 'astraterm-dark',
  lightTheme: 'astraterm-light',
  matchAppTheme: true,
  customSchemes: [],
  minimumContrastRatio: 1,
  drawBoldTextInBrightColors: true,
  backgroundOpacity: 1,

  fontFamily: BUNDLED_FONT_FAMILY,
  fontSize: 13,
  fontWeight: 'normal',
  fontWeightBold: 'bold',
  lineHeight: 1.15,
  letterSpacing: 0,

  cursorStyle: 'block',
  cursorBlink: false, // steady by default (docs/UX.md: nothing blinks); users can turn it on
  cursorInactiveStyle: 'outline',
  cursorWidth: 2,

  padding: 6,
  renderer: 'auto',
  scrollback: 10000,
  smoothScrollDuration: 0,
  scrollSensitivity: 1,
  fastScrollSensitivity: 5,
  scrollOnUserInput: true,
  unicodeVersion: '11',
  screenReaderMode: false,
  images: true,

  titleMode: 'session',

  copyOnSelect: false,
  rightClickAction: 'menu',
  middleClickPaste: true,
  rightClickSelectsWord: isMac,
  wordSeparator: ' ()[]{}\',"`│<>|;',
  altClickMovesCursor: true,
  linkModifier: 'mod',
  trimCopiedWhitespace: true,

  macOptionIsMeta: false,
  altArrowWordJump: isMac,
  ctrlZoom: true,
  altKeysToTerminal: false,
  passthroughKeys: [],

  pasteConfirmMultiline: true,
  pasteSkipConfirmBracketed: true,
  pasteConfirmDangerous: true,
  bracketedPaste: true,
  pasteLineDelayMs: 50,

  osc52: 'write',
  clipboardHistory: true,

  bell: 'visual',
  bellNotify: false,
  notifyLongCommands: true,
  longCommandSeconds: 15,
  silenceSeconds: 30,
  osc9Notifications: true,

  showEndPrompt: true,
  closeOnExit: 'never',

  hidePointerWhileTyping: true,
  focusFollowsMouse: false,
  showToolbar: true,
  restoreHistory: true,
}

export const terminalSettings = defineSettings<TerminalSettings>('terminal', TERMINAL_DEFAULTS)

/** Numeric limits shared by the settings UI and the override sanitiser. */
export const LIMITS = {
  fontSize: [6, 72],
  lineHeight: [1, 2],
  letterSpacing: [-2, 10],
  scrollback: [0, 1_000_000],
  padding: [0, 32],
  cursorWidth: [1, 6],
  minimumContrastRatio: [1, 21],
  backgroundOpacity: [0.3, 1],
  smoothScrollDuration: [0, 500],
  scrollSensitivity: [0.1, 10],
  fastScrollSensitivity: [1, 50],
  pasteLineDelayMs: [0, 5000],
  longCommandSeconds: [1, 86_400],
  silenceSeconds: [2, 86_400],
} as const satisfies Partial<Record<keyof TerminalSettings, readonly [number, number]>>

const ENUMS: Partial<Record<keyof TerminalSettings, readonly string[]>> = {
  cursorStyle: ['block', 'underline', 'bar'],
  cursorInactiveStyle: ['outline', 'block', 'bar', 'underline', 'none'],
  renderer: ['auto', 'dom'],
  unicodeVersion: ['11', '6'],
  titleMode: ['session', 'osc', 'both'],
  rightClickAction: ['menu', 'paste', 'copy-or-paste'],
  linkModifier: ['none', 'mod'],
  osc52: ['off', 'write', 'read-write'],
  bell: ['none', 'visual', 'sound', 'both'],
  closeOnExit: ['never', 'clean', 'always'],
  fontWeight: ['normal', 'bold', '100', '200', '300', '400', '500', '600', '700', '800', '900'],
  fontWeightBold: ['normal', 'bold', '100', '200', '300', '400', '500', '600', '700', '800', '900'],
}

function clampNum(v: number, key: keyof TerminalSettings): number {
  const lim = (LIMITS as Partial<Record<keyof TerminalSettings, readonly [number, number]>>)[key]
  return lim ? Math.min(lim[1], Math.max(lim[0], v)) : v
}

/**
 * Validate one value against the type of the default: wrong types are rejected (undefined), numbers are clamped,
 * enums checked. Used for connection overrides and for defensive reads of stored settings.
 */
function sanitizeValue<K extends keyof TerminalSettings>(key: K, value: unknown): TerminalSettings[K] | undefined {
  const def = TERMINAL_DEFAULTS[key]
  if (typeof def === 'number') {
    const n = typeof value === 'string' && value.trim() !== '' ? Number(value) : value
    return typeof n === 'number' && Number.isFinite(n) ? (clampNum(n, key) as TerminalSettings[K]) : undefined
  }
  if (typeof def === 'boolean') return typeof value === 'boolean' ? (value as TerminalSettings[K]) : undefined
  if (typeof def === 'string') {
    if (typeof value === 'number' && (key === 'fontWeight' || key === 'fontWeightBold')) value = String(value)
    if (typeof value !== 'string') return undefined
    const allowed = ENUMS[key]
    if (allowed && !allowed.includes(value)) return undefined
    if (key === 'fontFamily' && !value.trim()) return undefined
    return value as TerminalSettings[K]
  }
  if (Array.isArray(def)) return Array.isArray(value) ? (value as TerminalSettings[K]) : undefined
  return undefined
}

/** Clean a whole settings object (stored values may come from older versions or hand edits). */
function sanitizeTerminalSettings(raw: TerminalSettings): TerminalSettings {
  const out = { ...TERMINAL_DEFAULTS }
  for (const key of Object.keys(TERMINAL_DEFAULTS) as (keyof TerminalSettings)[]) {
    const v = sanitizeValue(key, raw[key])
    if (v !== undefined) (out as Record<string, unknown>)[key] = v
  }
  out.customSchemes = Array.isArray(raw.customSchemes) ? raw.customSchemes.filter(isSchemeLike) : []
  out.passthroughKeys = Array.isArray(raw.passthroughKeys) ? raw.passthroughKeys.filter((k) => typeof k === 'string' && k.trim() !== '') : []
  return out
}

function isSchemeLike(s: unknown): s is TerminalScheme {
  return !!s && typeof s === 'object' && typeof (s as TerminalScheme).id === 'string' && typeof (s as TerminalScheme).background === 'string'
}

/** Keys a connection may override through `options.terminal` (everything except the scheme library itself). */
const OVERRIDABLE_KEYS = (Object.keys(TERMINAL_DEFAULTS) as (keyof TerminalSettings)[]).filter(
  (k) => k !== 'customSchemes' && k !== 'passthroughKeys',
)

/**
 * Effective settings of one terminal: global settings ← per-connection overrides (`options.terminal`). An override
 * `theme` applies in both light and dark UI themes.
 */
export function effectiveTerminalSettings(global: TerminalSettings, overrides?: TerminalOverrides | null): TerminalSettings {
  const base = sanitizeTerminalSettings(global)
  if (!overrides || typeof overrides !== 'object') return base
  const out: TerminalSettings = { ...base }
  for (const key of OVERRIDABLE_KEYS) {
    if (!(key in overrides)) continue
    const v = sanitizeValue(key, (overrides as Record<string, unknown>)[key])
    if (v !== undefined) (out as unknown as Record<string, unknown>)[key] = v
  }
  if (typeof overrides.theme === 'string' && overrides.theme) {
    out.theme = overrides.theme
    out.lightTheme = overrides.theme
  }
  return out
}
