/*
 * Monaco themes generated from the app's design tokens (src/index.css + the accent colour set on <html> by
 * src/lib/theme.ts): editor background = panel, selections / cursor / focus from the accent, muted gutter, widgets
 * (find, suggest, hover, peek, command palette, context menu) from the popover and border tokens, scrollbars from
 * the scrollbar tokens, syntax colours from monaco/palette.ts. Re-generated whenever the theme or the accent changes.
 */
import type * as Monaco from 'monaco-editor/editor'
import { isDarkTheme, onThemeChange } from '@/lib/theme'
import { luminance, mix, parseColor, toHex, type Rgba } from './color'
import { BRACKET_COLORS, PALETTE, SYNTAX_RULES } from './palette'

export const THEME_NAME = 'termstead'

const TOKENS = [
  'background',
  'foreground',
  'panel',
  'card',
  'popover',
  'popover-foreground',
  'muted',
  'muted-foreground',
  'accent',
  'accent-foreground',
  'secondary',
  'primary',
  'primary-foreground',
  'border',
  'input',
  'ring',
  'destructive',
  'success',
  'warning',
  'info',
  'toolbar',
  'scrollbar-thumb',
  'scrollbar-thumb-hover',
  'shadow-color',
] as const

type Token = (typeof TOKENS)[number]
export type TokenColors = Record<Token, Rgba>

let canvasCtx: CanvasRenderingContext2D | null | undefined

/** Any CSS colour → RGBA: parsed when possible, else painted on a 1×1 canvas and read back. */
function resolveColor(value: string): Rgba | null {
  const parsed = parseColor(value)
  if (parsed) return parsed
  if (canvasCtx === undefined) canvasCtx = document.createElement('canvas').getContext('2d', { willReadFrequently: true })
  const ctx = canvasCtx
  if (!ctx) return null
  ctx.clearRect(0, 0, 1, 1)
  ctx.fillStyle = '#00000000'
  ctx.fillStyle = value
  ctx.fillRect(0, 0, 1, 1)
  const d = ctx.getImageData(0, 0, 1, 1).data
  return { r: d[0], g: d[1], b: d[2], a: d[3] / 255 }
}

const FALLBACK: Record<'dark' | 'light', Partial<Record<Token, string>>> = {
  dark: { panel: '#1b1e24', foreground: '#e3e6ea', primary: '#4f8ff7' },
  light: { panel: '#ffffff', foreground: '#23272f', primary: '#2563eb' },
}

/**
 * Current token values: the custom properties as computed on <html> (the runtime accent and the light/dark class
 * apply; var() references are already substituted). Read as strings — never through a probe element's computed
 * `color`, which the app's colour transitions would report mid-animation.
 */
export function readTokens(doc: Document = document): TokenColors {
  const dark = doc.documentElement.classList.contains('dark') || isDarkTheme()
  const cs = getComputedStyle(doc.documentElement)
  const out = {} as TokenColors
  for (const t of TOKENS) {
    const value = cs.getPropertyValue(`--${t}`).trim()
    out[t] =
      (value ? resolveColor(value) : null) ??
      resolveColor(FALLBACK[dark ? 'dark' : 'light'][t] ?? (dark ? '#888888' : '#666666')) ??
      { r: 128, g: 128, b: 128, a: 1 }
  }
  return out
}

/** Theme data for the given tokens. */
export function buildTheme(t: TokenColors, dark: boolean): Monaco.editor.IStandaloneThemeData {
  const hx = (c: Rgba, a = 1) => toHex(c, a)
  const over = (c: Rgba, amount: number, bg: Rgba = t.panel) => toHex(mix(c, bg, amount))
  const pal = PALETTE[dark ? 'dark' : 'light']
  const brackets = BRACKET_COLORS[dark ? 'dark' : 'light']
  const strip = (s: string) => s.replace(/^#/, '')
  const rules: Monaco.editor.ITokenThemeRule[] = [
    { token: '', foreground: strip(hx(t.foreground)) },
    ...SYNTAX_RULES.map((r) => {
      const rule: Monaco.editor.ITokenThemeRule = { token: r.token }
      const c = r.color ? pal[r.color] : ''
      if (c) rule.foreground = strip(c)
      if (r.fontStyle) rule.fontStyle = r.fontStyle
      return rule
    }),
  ]
  const fg = t.foreground
  const muted = t['muted-foreground']
  const primary = t.primary
  const transparent = '#00000000'
  // Selection text stays readable: a light theme needs a lighter selection than a dark one.
  const sel = dark ? 0.34 : 0.22
  const colors: Record<string, string> = {
    // editor surface
    'editor.background': hx(t.panel),
    'editor.foreground': hx(fg),
    'editorCursor.foreground': hx(primary),
    'editorCursor.background': hx(t.panel),
    'editor.lineHighlightBackground': hx(fg, dark ? 0.045 : 0.035),
    'editor.lineHighlightBorder': transparent,
    'editor.selectionBackground': hx(primary, sel),
    'editor.inactiveSelectionBackground': hx(primary, sel * 0.55),
    'editor.selectionHighlightBackground': hx(primary, 0.14),
    'editor.selectionHighlightBorder': transparent,
    'editor.wordHighlightBackground': hx(fg, dark ? 0.09 : 0.07),
    'editor.wordHighlightStrongBackground': hx(primary, 0.18),
    'editor.wordHighlightTextBackground': hx(fg, dark ? 0.09 : 0.07),
    'editor.findMatchBackground': hx(t.warning, dark ? 0.55 : 0.5),
    'editor.findMatchBorder': hx(t.warning),
    'editor.findMatchHighlightBackground': hx(t.warning, dark ? 0.22 : 0.28),
    'editor.findRangeHighlightBackground': hx(primary, 0.08),
    'editor.rangeHighlightBackground': hx(primary, 0.1),
    'editor.hoverHighlightBackground': hx(primary, 0.1),
    'editor.foldBackground': hx(primary, 0.07),
    'editor.foldPlaceholderForeground': hx(muted),
    'editorLink.activeForeground': hx(primary),
    'editorWhitespace.foreground': hx(muted, 0.4),
    'editorIndentGuide.background1': hx(t.border),
    'editorIndentGuide.activeBackground1': hx(muted, 0.55),
    'editorRuler.foreground': hx(t.border),
    'editorCodeLens.foreground': hx(muted),
    'editorLightBulb.foreground': hx(t.warning),
    'editorBracketMatch.background': hx(t.success, 0.18),
    'editorBracketMatch.border': hx(t.success, 0.6),
    'editorBracketHighlight.foreground1': brackets[0],
    'editorBracketHighlight.foreground2': brackets[1],
    'editorBracketHighlight.foreground3': brackets[2],
    'editorBracketHighlight.foreground4': brackets[0],
    'editorBracketHighlight.foreground5': brackets[1],
    'editorBracketHighlight.foreground6': brackets[2],
    'editorBracketHighlight.unexpectedBracket.foreground': hx(t.destructive),
    'editorBracketPairGuide.activeBackground1': hx(muted, 0.6),
    'editorBracketPairGuide.activeBackground2': hx(muted, 0.6),
    'editorBracketPairGuide.activeBackground3': hx(muted, 0.6),
    'editorUnicodeHighlight.border': hx(t.warning, 0.8),
    'editorGhostText.foreground': hx(muted, 0.8),
    // gutter
    'editorGutter.background': hx(t.panel),
    'editorLineNumber.foreground': hx(muted, 0.72),
    'editorLineNumber.activeForeground': hx(fg),
    'editorLineNumber.dimmedForeground': hx(muted, 0.45),
    'editorGutter.foldingControlForeground': hx(muted),
    'editorGutter.modifiedBackground': hx(t.info),
    'editorGutter.addedBackground': hx(t.success),
    'editorGutter.deletedBackground': hx(t.destructive),
    // diagnostics
    'editorError.foreground': hx(t.destructive),
    'editorWarning.foreground': hx(t.warning),
    'editorInfo.foreground': hx(t.info),
    'editorHint.foreground': hx(muted),
    'editorOverviewRuler.border': transparent,
    'editorOverviewRuler.background': hx(t.panel),
    'editorOverviewRuler.findMatchForeground': hx(t.warning, 0.8),
    'editorOverviewRuler.selectionHighlightForeground': hx(primary, 0.6),
    'editorOverviewRuler.errorForeground': hx(t.destructive, 0.8),
    'editorOverviewRuler.warningForeground': hx(t.warning, 0.8),
    'editorOverviewRuler.infoForeground': hx(t.info, 0.8),
    'editorOverviewRuler.bracketMatchForeground': hx(t.success, 0.6),
    // sticky scroll
    'editorStickyScroll.background': hx(t.panel),
    'editorStickyScrollHover.background': hx(t.accent),
    'editorStickyScroll.border': hx(t.border),
    'editorStickyScroll.shadow': hx(t['shadow-color'], dark ? 0.6 : 0.35),
    // widgets: find, suggest, hover, parameter hints, rename, peek
    'widget.shadow': hx(t['shadow-color'], dark ? 0.55 : 0.3),
    'widget.border': hx(t.border),
    'editorWidget.background': hx(t.popover),
    'editorWidget.foreground': hx(t['popover-foreground']),
    'editorWidget.border': hx(t.border),
    'editorWidget.resizeBorder': hx(t.border),
    'editorSuggestWidget.background': hx(t.popover),
    'editorSuggestWidget.foreground': hx(t['popover-foreground']),
    'editorSuggestWidget.border': hx(t.border),
    'editorSuggestWidget.selectedBackground': hx(t.accent),
    'editorSuggestWidget.selectedForeground': hx(t['accent-foreground']),
    'editorSuggestWidget.highlightForeground': hx(primary),
    'editorSuggestWidget.focusHighlightForeground': hx(primary),
    'editorSuggestWidgetStatus.foreground': hx(muted),
    'editorHoverWidget.background': hx(t.popover),
    'editorHoverWidget.foreground': hx(t['popover-foreground']),
    'editorHoverWidget.border': hx(t.border),
    'editorHoverWidget.highlightForeground': hx(primary),
    'editorHoverWidget.statusBarBackground': hx(t.muted),
    'editorMarkerNavigation.background': hx(t.popover),
    'editorMarkerNavigationError.background': hx(t.destructive),
    'editorMarkerNavigationWarning.background': hx(t.warning),
    'editorMarkerNavigationInfo.background': hx(t.info),
    'peekView.border': hx(primary),
    'peekViewTitle.background': hx(t.toolbar),
    'peekViewTitleLabel.foreground': hx(fg),
    'peekViewTitleDescription.foreground': hx(muted),
    'peekViewEditor.background': hx(t.card),
    'peekViewEditorGutter.background': hx(t.card),
    'peekViewEditor.matchHighlightBackground': hx(t.warning, 0.3),
    'peekViewResult.background': hx(t.popover),
    'peekViewResult.fileForeground': hx(fg),
    'peekViewResult.lineForeground': hx(muted),
    'peekViewResult.selectionBackground': hx(t.accent),
    'peekViewResult.selectionForeground': hx(t['accent-foreground']),
    'peekViewResult.matchHighlightBackground': hx(t.warning, 0.3),
    'debugExceptionWidget.background': hx(t.popover),
    // inputs & buttons inside widgets (find / replace, rename)
    'input.background': hx(t.background),
    'input.foreground': hx(fg),
    'input.border': hx(t.input),
    'input.placeholderForeground': hx(muted, 0.85),
    'inputOption.activeBorder': hx(primary),
    'inputOption.activeBackground': hx(primary, 0.22),
    'inputOption.activeForeground': hx(fg),
    'inputOption.hoverBackground': hx(t.accent),
    'inputValidation.errorBackground': over(t.destructive, 0.15, t.popover),
    'inputValidation.errorBorder': hx(t.destructive),
    'inputValidation.warningBackground': over(t.warning, 0.15, t.popover),
    'inputValidation.warningBorder': hx(t.warning),
    'inputValidation.infoBackground': over(t.info, 0.15, t.popover),
    'inputValidation.infoBorder': hx(t.info),
    focusBorder: hx(t.ring, 0.9),
    contrastBorder: transparent,
    'button.background': hx(primary),
    'button.foreground': hx(t['primary-foreground']),
    'button.hoverBackground': over(primary, 0.88, t.popover),
    'button.secondaryBackground': hx(t.secondary),
    'button.secondaryForeground': hx(fg),
    'toolbar.hoverBackground': hx(t.accent),
    'toolbar.activeBackground': hx(t.accent),
    'icon.foreground': hx(muted),
    'textLink.foreground': hx(primary),
    'textLink.activeForeground': hx(primary),
    'textCodeBlock.background': hx(t.muted),
    'textBlockQuote.background': hx(t.muted),
    'textBlockQuote.border': hx(t.border),
    'textPreformat.foreground': hx(fg),
    descriptionForeground: hx(muted),
    errorForeground: hx(t.destructive),
    foreground: hx(fg),
    'sash.hoverBorder': hx(primary, 0.7),
    'progressBar.background': hx(primary),
    'badge.background': hx(t.secondary),
    'badge.foreground': hx(fg),
    // lists (suggest details, quick input / command palette, references)
    'list.hoverBackground': hx(t.accent, 0.7),
    'list.hoverForeground': hx(fg),
    'list.activeSelectionBackground': hx(t.accent),
    'list.activeSelectionForeground': hx(t['accent-foreground']),
    'list.inactiveSelectionBackground': hx(t.accent, 0.7),
    'list.focusBackground': hx(t.accent),
    'list.focusForeground': hx(t['accent-foreground']),
    'list.focusOutline': transparent,
    'list.focusAndSelectionOutline': transparent,
    'list.highlightForeground': hx(primary),
    'list.focusHighlightForeground': hx(primary),
    'list.invalidItemForeground': hx(t.destructive),
    'list.errorForeground': hx(t.destructive),
    'list.warningForeground': hx(t.warning),
    'quickInput.background': hx(t.popover),
    'quickInput.foreground': hx(t['popover-foreground']),
    'quickInputTitle.background': hx(t.toolbar),
    'quickInputList.focusBackground': hx(t.accent),
    'quickInputList.focusForeground': hx(t['accent-foreground']),
    'quickInputList.focusIconForeground': hx(t['accent-foreground']),
    'pickerGroup.foreground': hx(primary),
    'pickerGroup.border': hx(t.border),
    'keybindingLabel.background': hx(t.muted),
    'keybindingLabel.foreground': hx(muted),
    'keybindingLabel.border': hx(t.border),
    'keybindingLabel.bottomBorder': hx(t.border),
    // context menu (shadow DOM, styled from these)
    'menu.background': hx(t.popover),
    'menu.foreground': hx(t['popover-foreground']),
    'menu.selectionBackground': hx(t.accent),
    'menu.selectionForeground': hx(t['accent-foreground']),
    'menu.separatorBackground': hx(t.border),
    'menu.border': hx(t.border),
    // scrollbars & minimap
    'scrollbar.shadow': transparent,
    'scrollbarSlider.background': hx(t['scrollbar-thumb'], 0.55),
    'scrollbarSlider.hoverBackground': hx(t['scrollbar-thumb-hover'], 0.8),
    'scrollbarSlider.activeBackground': hx(t['scrollbar-thumb-hover']),
    'minimap.background': hx(t.panel),
    'minimap.selectionHighlight': hx(primary, 0.45),
    'minimap.findMatchHighlight': hx(t.warning, 0.6),
    'minimap.errorHighlight': hx(t.destructive, 0.7),
    'minimap.warningHighlight': hx(t.warning, 0.7),
    'minimapSlider.background': hx(t['scrollbar-thumb'], 0.3),
    'minimapSlider.hoverBackground': hx(t['scrollbar-thumb-hover'], 0.4),
    'minimapSlider.activeBackground': hx(t['scrollbar-thumb-hover'], 0.5),
    'minimapGutter.addedBackground': hx(t.success),
    'minimapGutter.modifiedBackground': hx(t.info),
    'minimapGutter.deletedBackground': hx(t.destructive),
    // diff editor
    'diffEditor.insertedTextBackground': hx(t.success, dark ? 0.2 : 0.18),
    'diffEditor.removedTextBackground': hx(t.destructive, dark ? 0.2 : 0.16),
    'diffEditor.insertedLineBackground': hx(t.success, dark ? 0.1 : 0.09),
    'diffEditor.removedLineBackground': hx(t.destructive, dark ? 0.1 : 0.08),
    'diffEditorGutter.insertedLineBackground': hx(t.success, dark ? 0.16 : 0.14),
    'diffEditorGutter.removedLineBackground': hx(t.destructive, dark ? 0.16 : 0.13),
    'diffEditorOverview.insertedForeground': hx(t.success, 0.7),
    'diffEditorOverview.removedForeground': hx(t.destructive, 0.7),
    'diffEditor.border': hx(t.border),
    'diffEditor.diagonalFill': hx(muted, 0.18),
    'diffEditor.unchangedRegionBackground': hx(t.muted),
    'diffEditor.unchangedRegionForeground': hx(muted),
    'diffEditor.unchangedCodeBackground': hx(fg, 0.03),
    'diffEditor.move.border': hx(t.info, 0.6),
    'diffEditor.moveActive.border': hx(t.warning),
    'multiDiffEditor.border': hx(t.border),
  }
  return { base: dark ? 'vs-dark' : 'vs', inherit: true, rules, colors }
}

/** Theme data for the current UI. */
export function currentTheme(): { data: Monaco.editor.IStandaloneThemeData; key: string; dark: boolean } {
  const dark = isDarkTheme()
  const tokens = readTokens()
  // luminance guards a stale class: the panel colour decides whether the editor is dark
  const effectiveDark = tokens.panel ? luminance(tokens.panel) < 0.35 : dark
  const data = buildTheme(tokens, effectiveDark)
  return { data, key: JSON.stringify(data.colors) + effectiveDark, dark: effectiveDark }
}

let appliedKey = ''

/** (Re)define and select the Termstead theme; no-op when nothing changed. */
export function applyTheme(monaco: typeof Monaco): void {
  const t = currentTheme()
  if (t.key === appliedKey) return
  appliedKey = t.key
  monaco.editor.defineTheme(THEME_NAME, t.data)
  monaco.editor.setTheme(THEME_NAME)
}

let watching = false

/**
 * Follow the app theme: light/dark switches (lib/theme listeners) and accent / density changes (the style and class
 * of <html>). Installed once.
 */
export function watchTheme(monaco: typeof Monaco): void {
  if (watching) return
  watching = true
  let frame = 0
  const schedule = () => {
    if (frame) return
    frame = requestAnimationFrame(() => {
      frame = 0
      applyTheme(monaco)
    })
  }
  onThemeChange(schedule)
  new MutationObserver(schedule).observe(document.documentElement, { attributes: true, attributeFilter: ['class', 'style', 'data-theme'] })
}
