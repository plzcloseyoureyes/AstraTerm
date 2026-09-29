/*
 * EditorSession: one Monaco editor + its text model, plus everything the tabs configure at runtime, independent of
 * React. Tab components create it on a container element, subscribe to its status snapshot (useSyncExternalStore) and
 * call its methods from toolbars, status bars and commands.
 *
 * The model always holds "\n"-separated text; the file's line endings are kept by a BreakTracker (../eol.ts) and
 * applied again when saving. Dirty state compares against the saved content (Monaco's alternative version id first,
 * the text when that is not conclusive), so undoing back to the saved text is clean again.
 */
import type * as Monaco from 'monaco-editor/editor'
import { runCommand } from '@/app/commands'
import { EOL_SEQ, type Eol } from '../codec'
import { adoptBreaks, BreakTracker, joinWithBreaks, normalizeBreaks, scanBreaks, validBreaks } from '../eol'
import { convertIndentation, detectIndentation, visualColumn, type IndentStyle, type LineSource } from '../indent'
import { detectLanguage, findLanguage, languageName, PLAIN_ID, PLAIN_TEXT } from '../languages'
import { effectiveFontSize } from '../settings'
import { textEdits } from '../textdiff'
import type { EditorSettings } from '../types'
import { installAppKeys } from './keys'
import type { MonacoModule } from './load'
import { safeFontFamily } from './palette'
import { THEME_NAME } from './theme'
import { attachVim } from './vim'
import { followWindow, observeResize } from './windows'
import type { VimAdapter } from './vendor/monaco-vim.js'

export interface SessionStatus {
  line: number
  col: number
  selections: number
  selectedChars: number
  selectedLines: number
  lines: number
  length: number
  docDirty: boolean
  /** Display name of the language. */
  language: string
  /** Monaco language id (the "-lite" variant for very large files). */
  languageId: string
  languageLoading: boolean
  indent: IndentStyle
  wrap: boolean
  whitespace: boolean
  minimap: boolean
  vim: boolean
  readOnly: boolean
  canUndo: boolean
  canRedo: boolean
  focused: boolean
  /** Line breaks written with another sequence than the main line ending (mixed line endings, see ../eol.ts). */
  eolMarks: number
  /** Large document: heavy features (minimap, suggestions, language service) are off. */
  large: boolean
}

export interface SessionConfig {
  tabId: string
  /** Initial text as read (any line endings; they are recorded, see ../eol.ts). */
  doc: string
  /** Main line ending of `doc` (default: the most frequent one; ties → LF). */
  eol?: Eol
  /** File path / name: language detection, the model URI (JSON schemas, TSX) and the accessible label. */
  path: string
  /** Explicit language (name or Monaco id, "" = plain text); undefined = detect. */
  language?: string
  readOnly: boolean
  settings: EditorSettings
  zoom: number
  /** Minimap for this tab (default: settings). */
  minimap?: boolean
  placeholder?: string
  onDocChange?: () => void
  onFocusChange?: (focused: boolean) => void
}

/** What a save wrote: the model version and its text (see markSaved). */
export interface DocSnapshot {
  alt: number
  text: string
}

/** Text a document can be exported / printed from. */
export interface PrintableDoc {
  title: string
  lines: string[]
  tabSize: number
  /** Syntax tokens per line (Monaco's tokenizer for the document's language). */
  tokenize(): { offset: number; type: string }[][]
}

/** Above this size (characters) or line count the editor runs in "large file" mode. */
export const LARGE_CHARS = 2 * 1024 * 1024
export const LARGE_LINES = 60_000
/** Above this size worker-backed languages (JS/TS/JSON/CSS/HTML) are coloured without their language service. */
export const HUGE_CHARS = 6 * 1024 * 1024
/** Dirty checks compare the text only below this size (above: Monaco's version ids decide). */
const EXACT_DIRTY_CHARS = 1024 * 1024

let modelSeq = 0

function prefersReducedMotion(): boolean {
  try {
    return window.matchMedia('(prefers-reduced-motion: reduce)').matches
  } catch {
    return false
  }
}

/** The editor font: the user's font-family setting, else the app's monospace font (bundled JetBrains Mono). */
export function editorFont(s: EditorSettings): string {
  const custom = safeFontFamily(s.fontFamily)
  if (custom) return custom
  try {
    const v = getComputedStyle(document.documentElement).getPropertyValue('--font-mono').trim()
    if (v) return v
  } catch {
    /* no DOM */
  }
  return '"JetBrains Mono Variable", "JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, Consolas, monospace'
}

/** Editor options shared by the text editor and the diff views. */
export function sharedOptions(s: EditorSettings, zoom: number, large: boolean): Monaco.editor.IEditorOptions & Monaco.editor.IGlobalEditorOptions {
  const reduced = prefersReducedMotion()
  const suggest = s.autocomplete && !large
  return {
    fontFamily: editorFont(s),
    fontSize: effectiveFontSize(s, zoom),
    lineHeight: 1.5,
    fontLigatures: false,
    letterSpacing: 0,
    lineNumbers: s.lineNumbers ? 'on' : 'off',
    lineNumbersMinChars: 3,
    lineDecorationsWidth: 10,
    glyphMargin: false,
    renderLineHighlight: s.highlightActiveLine ? 'all' : 'none',
    folding: true,
    showFoldingControls: 'mouseover',
    foldingHighlight: true,
    guides: { indentation: s.indentGuides !== false, highlightActiveIndentation: true, bracketPairs: large ? false : 'active', bracketPairsHorizontal: false },
    bracketPairColorization: { enabled: s.bracketPairs !== false && !large, independentColorPoolPerBracketType: false },
    matchBrackets: 'always',
    stickyScroll: { enabled: s.stickyScroll !== false && !large, maxLineCount: 5 },
    scrollbar: { verticalScrollbarSize: 10, horizontalScrollbarSize: 10, useShadows: false, alwaysConsumeMouseWheel: false },
    overviewRulerBorder: false,
    hideCursorInOverviewRuler: true,
    scrollBeyondLastLine: false,
    padding: { top: 4, bottom: 8 },
    smoothScrolling: !reduced,
    cursorBlinking: 'solid', // a steady caret: nothing on screen blinks (docs/UX.md "Motion & feedback")
    cursorSmoothCaretAnimation: 'off',
    cursorWidth: 2,
    renderControlCharacters: true,
    autoClosingBrackets: s.closeBrackets ? 'languageDefined' : 'never',
    autoClosingQuotes: s.closeBrackets ? 'languageDefined' : 'never',
    autoSurround: 'languageDefined',
    quickSuggestions: suggest ? { other: true, comments: false, strings: false } : false,
    suggestOnTriggerCharacters: suggest,
    wordBasedSuggestions: suggest ? 'currentDocument' : 'off',
    parameterHints: { enabled: suggest },
    acceptSuggestionOnEnter: 'smart',
    suggest: { preview: false, showWords: true, showStatusBar: false },
    colorDecorators: s.colorSwatches !== false && !large,
    occurrencesHighlight: large ? 'off' : 'singleFile',
    selectionHighlight: !large,
    links: !large,
    hover: { enabled: 'on', delay: 450, sticky: true },
    contextmenu: true,
    mouseWheelZoom: false,
    multiCursorModifier: 'alt',
    find: { addExtraSpaceOnTop: false, seedSearchStringFromSelection: 'selection', autoFindInSelection: 'multiline', loop: true },
    unusualLineTerminators: 'off',
    copyWithSyntaxHighlighting: false,
    'semanticHighlighting.enabled': false,
    fixedOverflowWidgets: true,
    automaticLayout: true,
    // The textarea input keeps the editor a "text field" for the app's shortcut dispatcher (see keys.ts).
    editContext: false,
    theme: THEME_NAME,
  }
}

/**
 * Host for an editor's overflowing widgets (suggestions, hovers, parameter hints) on the page's <body>: dockview
 * renders panels inside a transformed element, which would offset `position: fixed` widgets placed inside the editor.
 * Carries the `monaco-editor` class so the widgets keep Monaco's styles and theme variables. `follow(layout)` keeps it
 * in the window the editor is shown in (dockview pop-outs, see windows.ts) and lays the editor out there.
 */
export function createOverflowHost(parent: HTMLElement): { node: HTMLElement; follow: (layout: () => void) => void; dispose: () => void } {
  const doc = parent.ownerDocument
  const node = doc.createElement('div')
  node.className = 'monaco-editor nx-monaco-overflow'
  node.style.cssText = 'position:fixed;top:0;left:0;width:0;height:0;overflow:visible;z-index:45;background:none'
  doc.body.appendChild(node)
  let offWindow: (() => void) | null = null
  let offResize: (() => void) | null = null
  return {
    node,
    follow(layout) {
      offWindow?.()
      offWindow = followWindow(parent, (d) => {
        d.body.appendChild(node)
        offResize?.()
        offResize = observeResize(parent, layout)
        layout()
      })
    },
    dispose() {
      offWindow?.()
      offResize?.()
      node.remove()
    },
  }
}

export class EditorSession {
  readonly editor: Monaco.editor.IStandaloneCodeEditor
  readonly model: Monaco.editor.ITextModel
  /** Vim mode line / command line (shown in the tab's status bar). */
  readonly vimStatusNode: HTMLElement
  private readonly api: MonacoModule
  private readonly cfg: SessionConfig
  private readonly tracker: BreakTracker
  /** Main line ending the line-break marks are relative to. */
  private mainEol: Eol = 'lf'
  private saved: DocSnapshot
  private dirtyCache: { version: number; value: boolean } | null = null
  private statusValue: SessionStatus
  private listeners = new Set<() => void>()
  private frame = 0
  private destroyed = false
  private disposers: (() => void)[] = []
  private vimAdapter: VimAdapter | null = null
  private vimToken = 0
  private readonly large: boolean
  private conf: {
    settings: EditorSettings
    zoom: number
    readOnly: boolean
    wrap: boolean
    whitespace: boolean
    minimap: boolean
    vim: boolean
  }

  constructor(parent: HTMLElement, cfg: SessionConfig, api: MonacoModule) {
    this.api = api
    this.cfg = cfg
    const { monaco } = api
    const s = cfg.settings
    this.conf = {
      settings: s,
      zoom: cfg.zoom,
      readOnly: cfg.readOnly,
      wrap: s.wordWrap,
      whitespace: s.showWhitespace,
      minimap: cfg.minimap ?? s.minimap !== false,
      vim: false,
    }
    const scan = scanBreaks(cfg.doc, cfg.eol ?? 'lf', cfg.eol)
    this.mainEol = scan.main
    const text = normalizeBreaks(cfg.doc)
    let lineCount = 1
    if (text.length > LARGE_CHARS / 4) for (let i = text.indexOf('\n'); i >= 0 && lineCount <= LARGE_LINES; i = text.indexOf('\n', i + 1)) lineCount++
    this.large = text.length > LARGE_CHARS || lineCount > LARGE_LINES

    const uri = monaco.Uri.from({ scheme: 'inmemory', authority: `nx${++modelSeq}`, path: '/' + (cfg.path || 'untitled').replace(/^[\\/]+/, '').replace(/\\/g, '/') })
    this.model = monaco.editor.createModel(text, PLAIN_ID, uri)
    this.model.setEOL(monaco.editor.EndOfLineSequence.LF)
    this.tracker = new BreakTracker(scan.marks, this.model.getAlternativeVersionId())
    this.saved = { alt: this.model.getAlternativeVersionId(), text }

    // indentation: settings, or detected from the content
    let indent: IndentStyle = { useTabs: !s.insertSpaces, size: s.tabSize }
    if (s.detectIndentation) {
      const guess = detectIndentation(this.lines())
      if (guess) indent = { useTabs: guess.useTabs, size: guess.useTabs ? s.tabSize : guess.size }
    }
    this.applyIndent(indent)

    const lang = cfg.language !== undefined ? (cfg.language ? findLanguage(cfg.language) : null) : detectLanguage(cfg.path, text)
    this.applyLanguage(lang?.id ?? PLAIN_ID)

    this.vimStatusNode = document.createElement('div')
    this.vimStatusNode.className = 'nx-vim-status'

    const overflow = createOverflowHost(parent)
    this.disposers.push(overflow.dispose)
    this.editor = monaco.editor.create(parent, {
      ...sharedOptions(s, cfg.zoom, this.large),
      overflowWidgetsDomNode: overflow.node,
      model: this.model,
      readOnly: cfg.readOnly,
      readOnlyMessage: { value: 'This document is read-only. Allow editing with the lock button in the toolbar.' },
      wordWrap: this.conf.wrap ? 'on' : 'off',
      wrappingIndent: 'same',
      renderWhitespace: this.conf.whitespace ? 'all' : 'selection',
      minimap: this.minimapOptions(),
      ariaLabel: `Text editor: ${cfg.path || 'untitled'}`,
      placeholder: cfg.placeholder,
    })

    const ed = this.editor
    api.bindEditorTab(ed, cfg.tabId)
    overflow.follow(() => {
      if (!this.destroyed) ed.layout()
    })
    this.disposers.push(installAppKeys(parent, { tabId: cfg.tabId, vim: () => this.conf.vim }))
    const subs: Monaco.IDisposable[] = [
      this.model.onDidChangeContent((e) => {
        if (!e.isFlush) this.tracker.update(e.changes, this.model.getAlternativeVersionId(), e.isUndoing || e.isRedoing)
        this.dirtyCache = null
        this.cfg.onDocChange?.()
        this.scheduleStatus()
      }),
      this.model.onDidChangeOptions(() => this.scheduleStatus()),
      this.model.onDidChangeLanguage(() => this.scheduleStatus()),
      ed.onDidChangeCursorSelection(() => this.scheduleStatus()),
      ed.onDidFocusEditorWidget(() => {
        this.cfg.onFocusChange?.(true)
        this.scheduleStatus()
      }),
      ed.onDidBlurEditorWidget(() => {
        this.cfg.onFocusChange?.(false)
        this.scheduleStatus()
      }),
    ]
    this.disposers.push(() => subs.forEach((d) => d.dispose()))
    this.addActions()
    this.statusValue = this.computeStatus()
    if (s.vimMode) this.setVim(true)
  }

  // -------------------------------------------------------------------------------------------------------------------
  // helpers

  private lines(): LineSource {
    const m = this.model
    return { lineCount: m.getLineCount(), line: (n) => m.getLineContent(n) }
  }

  private minimapOptions(): Monaco.editor.IEditorMinimapOptions {
    return { enabled: this.conf.minimap && !this.large, renderCharacters: false, showSlider: 'mouseover', scale: 1, maxColumn: 100, size: 'proportional' }
  }

  private applyIndent(indent: IndentStyle): void {
    const size = Math.max(1, indent.useTabs ? indent.size || this.conf.settings.tabSize : indent.size || this.conf.settings.tabSize)
    this.model.updateOptions(indent.useTabs ? { insertSpaces: false, tabSize: size, indentSize: 'tabSize' } : { insertSpaces: true, tabSize: size, indentSize: size })
  }

  private applyLanguage(id: string): void {
    const { monaco } = this.api
    let target = id || PLAIN_ID
    if (this.model.getValueLength() > HUGE_CHARS) target = this.api.liteLanguage(target) ?? target
    if (this.model.getLanguageId() !== target) monaco.editor.setModelLanguage(this.model, target)
  }

  /** Commands in Monaco's command palette (F1) and context menu. */
  private addActions(): void {
    const tab = { tabId: this.cfg.tabId }
    const run = (id: string) => () => void runCommand(id, tab)
    const ed = this.editor
    const actions: Monaco.editor.IActionDescriptor[] = [
      { id: 'termstead.save', label: 'File: Save', run: run('editor.save'), contextMenuGroupId: '9_termstead', contextMenuOrder: 1 },
      { id: 'termstead.saveAs', label: 'File: Save As…', run: run('editor.saveAs') },
      { id: 'termstead.reload', label: 'File: Reload from Disk', run: run('editor.reload') },
      { id: 'termstead.compare', label: 'File: Compare with Saved Version', run: run('editor.compareWithSaved') },
      { id: 'termstead.exportHtml', label: 'File: Export as HTML…', run: run('editor.exportHtml') },
      { id: 'termstead.toggleWordWrap', label: 'View: Toggle Word Wrap', run: run('editor.toggleWordWrap') },
      { id: 'termstead.toggleWhitespace', label: 'View: Toggle Render Whitespace', run: run('editor.toggleWhitespace') },
      { id: 'termstead.toggleMinimap', label: 'View: Toggle Minimap', run: run('editor.toggleMinimap') },
      { id: 'termstead.toggleVim', label: 'Preferences: Toggle Vim Mode', run: run('editor.toggleVim') },
      { id: 'termstead.toggleReadOnly', label: 'File: Toggle Read-Only', run: run('editor.toggleReadOnly') },
      { id: 'termstead.detectIndentation', label: 'Indentation: Detect from Content', run: () => void this.detectIndentation() },
      { id: 'termstead.palette', label: 'Termstead: Show All Commands', run: () => void runCommand('palette.open') },
    ]
    const subs = actions.map((a) => ed.addAction(a))
    this.disposers.push(() => subs.forEach((d) => d.dispose()))
  }

  // -------------------------------------------------------------------------------------------------------------------
  // status

  subscribe = (cb: () => void): (() => void) => {
    this.listeners.add(cb)
    return () => this.listeners.delete(cb)
  }

  getStatus = (): SessionStatus => this.statusValue

  private scheduleStatus(): void {
    if (this.frame || this.destroyed) return
    this.frame = requestAnimationFrame(() => {
      this.frame = 0
      this.refreshStatus()
    })
  }

  private refreshStatus(): void {
    if (this.destroyed) return
    this.statusValue = this.computeStatus()
    for (const l of Array.from(this.listeners)) l()
  }

  private computeStatus(): SessionStatus {
    const m = this.model
    const ed = this.editor
    const opts = m.getOptions()
    const sels = ed.getSelections() ?? []
    const pos = ed.getPosition() ?? { lineNumber: 1, column: 1 }
    let chars = 0
    let lines = 0
    for (const r of sels) {
      if (r.isEmpty()) continue
      chars += m.getValueLengthInRange(r)
      lines += r.endLineNumber - r.startLineNumber + 1
    }
    const languageId = m.getLanguageId()
    return {
      line: pos.lineNumber,
      col: visualColumn(m.getLineContent(pos.lineNumber), pos.column - 1, opts.tabSize),
      selections: sels.length || 1,
      selectedChars: chars,
      selectedLines: lines,
      lines: m.getLineCount(),
      length: m.getValueLength(),
      docDirty: this.isDocDirty(),
      language: languageId === PLAIN_ID ? PLAIN_TEXT : languageName(this.api.baseLanguage(languageId)),
      languageId,
      languageLoading: false,
      indent: { useTabs: !opts.insertSpaces, size: opts.insertSpaces ? opts.indentSize : opts.tabSize },
      wrap: this.conf.wrap,
      whitespace: this.conf.whitespace,
      minimap: this.conf.minimap && !this.large,
      vim: this.conf.vim,
      readOnly: this.conf.readOnly,
      canUndo: m.canUndo(),
      canRedo: m.canRedo(),
      focused: ed.hasWidgetFocus(),
      eolMarks: this.tracker.count(),
      large: this.large,
    }
  }

  // -------------------------------------------------------------------------------------------------------------------
  // content

  /** Text with the given line separator (default "\n"). */
  getText(lineSep = '\n'): string {
    const t = this.model.getValue()
    return lineSep === '\n' ? t : t.replace(/\n/g, lineSep)
  }

  /**
   * The text as it is written to the file: with `mixedEol` every existing line break keeps its original sequence and
   * new ones get `eol`; otherwise every break is `eol` (the user chose a line ending = convert).
   */
  textForSave(meta: { eol: Eol; mixedEol: boolean }): string {
    const text = this.model.getValue()
    if (meta.mixedEol) return joinWithBreaks(text, this.tracker.get(), meta.eol)
    return meta.eol === 'lf' ? text : text.replace(/\n/g, EOL_SEQ[meta.eol])
  }

  /** The current content as a save snapshot (pass it to markSaved once written). */
  snapshot(): DocSnapshot {
    return { alt: this.model.getAlternativeVersionId(), text: this.model.getValue() }
  }

  isDocDirty(): boolean {
    const m = this.model
    const alt = m.getAlternativeVersionId()
    if (alt === this.saved.alt) return false
    const version = m.getVersionId()
    if (this.dirtyCache?.version === version) return this.dirtyCache.value
    let value: boolean
    if (m.getValueLength() !== this.saved.text.length) value = true
    else if (this.saved.text.length > EXACT_DIRTY_CHARS && this.saved.alt >= 0) value = true
    else value = m.getValue() !== this.saved.text
    this.dirtyCache = { version, value }
    return value
  }

  /** `snap` (default: the current content) is what is saved on disk now. */
  markSaved(snap: DocSnapshot = this.snapshot()): void {
    this.saved = snap
    this.dirtyCache = null
    this.refreshStatus()
  }

  /** Treat `text` as the saved content (e.g. the server version after a merge). */
  markSavedText(text: string): void {
    this.saved = { alt: -1, text: normalizeBreaks(text) }
    this.dirtyCache = null
    this.refreshStatus()
  }

  /**
   * Replace the whole document with raw text (any line endings).
   *   reset       a fresh model state (history cleared, e.g. reload), line endings scanned again (main: `eol`),
   *               cursor and scroll position kept where possible;
   *   otherwise   one undoable edit touching only the parts that differ, so the cursor, folds and the line endings of
   *               untouched lines survive; `exactBreaks` also takes the line endings of `text` (backup restore).
   */
  setText(text: string, opts: { reset?: boolean; markSaved?: boolean; eol?: Eol; exactBreaks?: boolean } = {}): void {
    if (this.destroyed) return
    const m = this.model
    const next = normalizeBreaks(text)
    if (opts.reset) {
      const view = this.editor.saveViewState()
      const scan = scanBreaks(text, opts.eol ?? 'lf', opts.eol)
      this.mainEol = scan.main
      m.setValue(next)
      this.tracker.reset(scan.marks, m.getAlternativeVersionId(), true)
      if (view) this.editor.restoreViewState(view)
    } else {
      const edits = textEdits(m.getValue(), next)
      if (edits.length) {
        this.editor.pushUndoStop()
        m.pushEditOperations(
          this.editor.getSelections(),
          edits.map((e) => {
            const a = m.getPositionAt(e.offset)
            const b = m.getPositionAt(e.offset + e.length)
            return { range: { startLineNumber: a.lineNumber, startColumn: a.column, endLineNumber: b.lineNumber, endColumn: b.column }, text: e.text }
          }),
          () => null,
        )
        this.editor.pushUndoStop()
      }
      if (opts.exactBreaks) {
        const marks = validBreaks(scanBreaks(text, this.mainEol, this.mainEol).marks, m.getValue())
        this.tracker.reset(marks, m.getAlternativeVersionId())
      }
    }
    if (opts.markSaved) this.saved = this.snapshot()
    this.dirtyCache = null
    this.refreshStatus()
  }

  /**
   * After merging with another version of the file (`otherRaw`, raw line endings: the server's copy), line breaks
   * that come from that version take its line endings; the others keep theirs (see adoptBreaks). Returns whether the
   * document now has mixed line endings.
   */
  adoptLineEndings(otherRaw: string): boolean {
    if (this.destroyed) return false
    const marks = adoptBreaks(this.model.getValue(), this.tracker.get(), otherRaw, this.mainEol)
    this.tracker.reset(marks, this.model.getAlternativeVersionId())
    this.refreshStatus()
    return marks.length > 0
  }

  // -------------------------------------------------------------------------------------------------------------------
  // configuration

  setLanguageByName(name: string | null): void {
    const info = name && name !== PLAIN_TEXT ? findLanguage(name) : null
    this.applyLanguage(info?.id ?? PLAIN_ID)
    this.refreshStatus()
  }

  setReadOnly(ro: boolean): void {
    this.conf.readOnly = ro
    this.editor.updateOptions({ readOnly: ro })
    this.refreshStatus()
  }

  setWrap(on: boolean): void {
    this.conf.wrap = on
    this.editor.updateOptions({ wordWrap: on ? 'on' : 'off' })
    this.refreshStatus()
  }

  setWhitespace(on: boolean): void {
    this.conf.whitespace = on
    this.editor.updateOptions({ renderWhitespace: on ? 'all' : 'selection' })
    this.refreshStatus()
  }

  setMinimap(on: boolean): void {
    this.conf.minimap = on
    this.editor.updateOptions({ minimap: this.minimapOptions() })
    this.refreshStatus()
  }

  setVim(on: boolean): void {
    this.conf.vim = on
    const token = ++this.vimToken
    if (!on) {
      this.vimAdapter?.dispose()
      this.vimAdapter = null
      this.vimStatusNode.replaceChildren()
    } else if (!this.vimAdapter) {
      void attachVim(this.editor, this.cfg.tabId, this.vimStatusNode).then(
        (adapter) => {
          if (this.destroyed || token !== this.vimToken || !this.conf.vim) adapter.dispose()
          else this.vimAdapter = adapter
          this.refreshStatus()
        },
        (err: unknown) => {
          console.warn('[editor] vim mode failed to load', err)
          this.conf.vim = false
          this.refreshStatus()
        },
      )
    }
    this.refreshStatus()
  }

  setIndent(indent: IndentStyle): void {
    this.applyIndent({ useTabs: indent.useTabs, size: indent.useTabs ? indent.size || this.conf.settings.tabSize : indent.size })
    this.refreshStatus()
  }

  /** Re-indent the whole document with the current indentation style (one undoable change). */
  convertIndentation(): void {
    const o = this.model.getOptions()
    const useTabs = !o.insertSpaces
    const edits = convertIndentation(this.lines(), useTabs, (useTabs ? o.tabSize : o.indentSize) || 4)
    if (!edits.length) return
    this.editor.pushUndoStop()
    this.editor.executeEdits(
      'termstead.convertIndentation',
      edits.map((e) => ({ range: { startLineNumber: e.line, startColumn: 1, endLineNumber: e.line, endColumn: e.length + 1 }, text: e.insert })),
    )
    this.editor.pushUndoStop()
  }

  detectIndentation(): IndentStyle | null {
    const guess = detectIndentation(this.lines())
    if (guess) this.setIndent(guess)
    return guess
  }

  setZoom(zoom: number): void {
    this.conf.zoom = zoom
    this.editor.updateOptions({ fontSize: effectiveFontSize(this.conf.settings, zoom) })
  }

  /** Apply changed settings (live). Per-tab toggles made from the toolbar are replaced by the new settings. */
  applySettings(s: EditorSettings, prev: EditorSettings): void {
    this.conf.settings = s
    this.editor.updateOptions(sharedOptions(s, this.conf.zoom, this.large))
    if (s.wordWrap !== prev.wordWrap) this.setWrap(s.wordWrap)
    if (s.showWhitespace !== prev.showWhitespace) this.setWhitespace(s.showWhitespace)
    if (s.minimap !== prev.minimap) this.setMinimap(s.minimap !== false)
    if (s.vimMode !== prev.vimMode) this.setVim(s.vimMode)
    if (s.tabSize !== prev.tabSize || s.insertSpaces !== prev.insertSpaces) this.setIndent({ useTabs: !s.insertSpaces, size: s.tabSize })
    this.refreshStatus()
  }

  // -------------------------------------------------------------------------------------------------------------------
  // actions

  focus(): void {
    if (!this.destroyed) this.editor.focus()
  }

  /**
   * Run a Monaco action. Quick-input actions (go to line, command palette) need a focused editor: an editor in a tab
   * that is just being shown may not take focus yet, so wait a frame for it.
   */
  private runAction(id: string): void {
    if (this.destroyed) return
    const run = () => {
      if (this.destroyed) return
      this.editor.focus()
      void this.editor.getAction(id)?.run()
    }
    this.editor.focus()
    if (this.editor.hasTextFocus()) run()
    else requestAnimationFrame(run)
  }

  openSearch(replace = false): void {
    this.runAction(replace && !this.conf.readOnly ? 'editor.action.startFindReplaceAction' : 'actions.find')
  }

  gotoLine(): void {
    this.runAction('editor.action.gotoLine')
  }

  formatDocument(): void {
    this.runAction('editor.action.formatDocument')
  }

  /** Can the document's language format it (JSON, CSS, HTML, JS/TS)? */
  canFormat(): boolean {
    return !this.destroyed && !this.conf.readOnly && !!this.editor.getAction('editor.action.formatDocument')?.isSupported()
  }

  commandPalette(): void {
    this.runAction('editor.action.quickCommand')
  }

  /** Select the first occurrence of `text` (case-insensitive) and show every match with the find widget open. */
  revealText(text: string): boolean {
    if (this.destroyed || !text) return false
    const match = this.model.findMatches(text, false, false, false, null, false, 1)[0]
    if (!match) return false
    this.editor.setSelection(match.range)
    this.editor.revealRangeInCenter(match.range)
    this.runAction('actions.find')
    requestAnimationFrame(() => this.editor.focus())
    return true
  }

  undo(): void {
    this.editor.focus()
    this.editor.trigger('termstead', 'undo', null)
  }

  redo(): void {
    this.editor.focus()
    this.editor.trigger('termstead', 'redo', null)
  }

  revealLine(line: number, col = 1): void {
    const n = Math.min(Math.max(1, Math.floor(line)), this.model.getLineCount())
    const column = Math.min(this.model.getLineMaxColumn(n), Math.max(1, col))
    this.editor.setPosition({ lineNumber: n, column })
    this.editor.revealLineInCenter(n)
    this.editor.focus()
  }

  /** The document for HTML export / printing. */
  printable(title: string): PrintableDoc {
    const { monaco } = this.api
    const model = this.model
    return {
      title,
      lines: model.getLinesContent(),
      tabSize: model.getOptions().tabSize,
      tokenize: () => monaco.editor.tokenize(model.getValue(), model.getLanguageId()),
    }
  }

  destroy(): void {
    if (this.destroyed) return
    this.destroyed = true
    if (this.frame) cancelAnimationFrame(this.frame)
    this.vimAdapter?.dispose()
    this.vimAdapter = null
    this.listeners.clear()
    this.editor.dispose()
    for (const d of this.disposers) d()
    this.model.dispose()
  }
}
