import { defineSettings } from '@/stores/settings'
import type { EditorSettings } from './types'

const EDITOR_DEFAULTS: EditorSettings = {
  fontSize: 13,
  fontFamily: '',
  tabSize: 4,
  insertSpaces: true,
  detectIndentation: true,
  wordWrap: false,
  vimMode: false,
  minimap: true,
  stickyScroll: true,
  bracketPairs: true,
  autosave: 'off',
  autosaveDelay: 1000,
  showWhitespace: false,
  indentGuides: true,
  colorSwatches: true,
  lineNumbers: true,
  highlightActiveLine: true,
  closeBrackets: true,
  autocomplete: true,
  semanticValidation: false,
  watchRemote: true,
  largeFileMiB: 5,
  defaultEol: 'lf',
}

/** Settings section "editor" (Settings → Editor). */
export const editorSettings = defineSettings<EditorSettings>('editor', EDITOR_DEFAULTS)

export const FONT_MIN = 8
export const FONT_MAX = 40

/** Effective font size for a tab (setting + per-tab zoom delta), clamped. */
export function effectiveFontSize(settings: EditorSettings, zoom = 0): number {
  const base = Number.isFinite(settings.fontSize) ? settings.fontSize : EDITOR_DEFAULTS.fontSize
  return Math.min(FONT_MAX, Math.max(FONT_MIN, Math.round(base + zoom)))
}
