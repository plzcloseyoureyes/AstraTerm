/*
 * Monaco's global (standalone) services, adapted to Termstead's workspace. Installed once by monaco/setup.ts.
 *
 *   popup layer     Context menus, dropdowns (diff gutter "…"), hovers and action lists are placed by Monaco in its
 *                   "layout container" — by default the DOM node of whichever editor happens to be focused or was
 *                   created first. In a dockview workspace that node may belong to a hidden tab, be clipped by its
 *                   panel, or live in another window, and it lies outside Monaco's theme variables (menus rendered
 *                   without a background). Every window gets one body-level layer instead (`.monaco-component`, so
 *                   the theme variables apply), and Monaco's layout service answers with the layer of the window
 *                   the focused editor is in.
 *   font zoom       Monaco's own "Editor Font Zoom In / Out / Reset" (F1) would zoom every editor; they act on the
 *                   tab's zoom instead (the same as Ctrl + / Ctrl - / Ctrl 0), so there is one zoom per tab.
 *   diff gutter     The gutter between the two sides of a diff offers "Copy block to the left" next to Monaco's
 *                   "Revert block" (→), whenever the left side is editable: changes can be copied in both directions.
 *   whitespace      A diff can ignore all whitespace (not only leading / trailing): lines are compared with their
 *                   whitespace removed, while the characters highlighted inside changed lines still come from the
 *                   real text.
 *   TS diagnostics  Semantic JavaScript / TypeScript checking can be switched on (settings); syntax errors always.
 *   pop-out input   Monaco asks "is my text area focused?" through the active element of the *main* document (its
 *                   window registry cannot be extended from outside), so in a dockview pop-out window the text
 *                   area never counts as focused: its selection is not kept in sync and typing corrupts the text.
 *                   The text area checks its own document instead (identical in the main window).
 */
import type * as Monaco from 'monaco-editor/editor'
import { StandaloneServices } from 'monaco-editor/editor/standalone/browser/standaloneServices.js'
import { ILayoutService } from 'monaco-editor/platform/layout/browser/layoutService.js'
import { ICodeEditorService } from 'monaco-editor/editor/browser/services/codeEditorService.js'
import { IEditorWorkerService } from 'monaco-editor/editor/common/services/editorWorker.js'
import { MenuId, MenuRegistry } from 'monaco-editor/platform/actions/common/actions.js'
import { CommandsRegistry } from 'monaco-editor/platform/commands/common/commands.js'
import { ContextKeyExpr } from 'monaco-editor/platform/contextkey/common/contextkey.js'
import { EditorContextKeys } from 'monaco-editor/editor/common/editorContextKeys.js'
import { Codicon } from 'monaco-editor/base/common/codicons.js'
import { EditorZoom } from 'monaco-editor/editor/common/config/editorZoom.js'
import { TextAreaWrapper } from 'monaco-editor/editor/browser/controller/editContext/textArea/textAreaEditContextInput.js'
import { restoreParentsScrollTop, saveParentsScrollTop } from 'monaco-editor/base/browser/dom.js'
import { runCommand } from '@/app/commands'
import { blockInnerChanges, stripWhitespaceLines, type DiffBlock, type InnerChange } from './diffws'

type MonacoApi = typeof Monaco

// ---------------------------------------------------------------------------------------------------------------------
// editors → tabs
// ---------------------------------------------------------------------------------------------------------------------

const editorTabs = new WeakMap<Monaco.editor.ICodeEditor, string>()

/** Remember which workspace tab an editor belongs to (font zoom, commands run from Monaco's UI). */
export function bindEditorTab(editor: Monaco.editor.ICodeEditor, tabId: string): void {
  editorTabs.set(editor, tabId)
}

interface CodeEditorService {
  getFocusedCodeEditor(): Monaco.editor.ICodeEditor | null
  getActiveCodeEditor(): Monaco.editor.ICodeEditor | null
}

function codeEditorService(): CodeEditorService {
  return StandaloneServices.get<CodeEditorService>(ICodeEditorService)
}

function focusedEditor(): Monaco.editor.ICodeEditor | null {
  const s = codeEditorService()
  return s.getFocusedCodeEditor() ?? s.getActiveCodeEditor()
}

// ---------------------------------------------------------------------------------------------------------------------
// popup layer
// ---------------------------------------------------------------------------------------------------------------------

const layers = new WeakMap<Document, HTMLElement>()

/** The body-level popup layer of a window (created on first use). */
export function popupLayer(doc: Document = document): HTMLElement {
  let el = layers.get(doc)
  if (!el || !el.isConnected) {
    el = doc.createElement('div')
    el.className = 'monaco-component nx-monaco-layer'
    // Inline as well: the layer must work before (or without) the feature's style sheet in a pop-out window.
    el.style.cssText = 'position:fixed;inset:0;overflow:visible;pointer-events:none;z-index:45;background:none'
    doc.body.appendChild(el)
    layers.set(doc, el)
  }
  return el
}

function activeDocument(): Document {
  return focusedEditor()?.getContainerDomNode().ownerDocument ?? document
}

function patchLayoutService(): void {
  const ls = StandaloneServices.get<Record<string, unknown>>(ILayoutService)
  Object.defineProperties(ls, {
    mainContainer: { configurable: true, get: () => popupLayer(document) },
    activeContainer: {
      configurable: true,
      get: () => popupLayer(activeDocument()),
    },
    containers: { configurable: true, get: () => [popupLayer(document)] },
  })
  ls.getContainer = (win?: Window) => popupLayer(win?.document ?? activeDocument())
}

// ---------------------------------------------------------------------------------------------------------------------
// font zoom → tab zoom
// ---------------------------------------------------------------------------------------------------------------------

function patchFontZoom(): void {
  EditorZoom.setZoomLevel = (level: number) => {
    // Monaco's zoom level stays 0: the per-tab zoom sets the font size.
    const delta = level - EditorZoom.getZoomLevel()
    const ed = focusedEditor()
    const tabId = ed ? editorTabs.get(ed) : undefined
    void runCommand(delta > 0 ? 'editor.zoomIn' : delta < 0 ? 'editor.zoomOut' : 'editor.zoomReset', tabId ? { tabId } : undefined)
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// diff gutter: copy a block to the left side
// ---------------------------------------------------------------------------------------------------------------------

/** A diff block as the gutter hands it over (line ranges, end exclusive). */
export interface GutterBlock {
  original: { startLineNumber: number; endLineNumberExclusive: number }
  modified: { startLineNumber: number; endLineNumberExclusive: number }
}

const copyLeftHandlers = new Map<string, (block: GutterBlock) => void>()

/** Handle "Copy block to the left" for the diff editor showing `modified` on its right side. */
export function onCopyToLeft(modified: Monaco.Uri, fn: (block: GutterBlock) => void): () => void {
  const key = modified.toString()
  copyLeftHandlers.set(key, fn)
  return () => {
    if (copyLeftHandlers.get(key) === fn) copyLeftHandlers.delete(key)
  }
}

const COPY_LEFT = 'termstead.diff.copyToLeft'

function registerDiffGutter(): void {
  CommandsRegistry.registerCommand(COPY_LEFT, (_accessor, arg) => {
    const a = arg as { mapping?: GutterBlock; modifiedUri?: { toString(): string } } | undefined
    if (!a?.mapping || !a.modifiedUri) return
    copyLeftHandlers.get(a.modifiedUri.toString())?.(a.mapping)
  })
  MenuRegistry.appendMenuItem(MenuId.DiffEditorHunkToolbar, {
    command: {
      id: COPY_LEFT,
      title: 'Copy Block to the Left',
      icon: Codicon.arrowLeft,
    },
    when: ContextKeyExpr.and(EditorContextKeys.diffEditorOriginalWritable, EditorContextKeys.diffEditorInlineMode.toNegated()),
    order: 6,
    group: 'primary',
  })
}

// ---------------------------------------------------------------------------------------------------------------------
// diff: ignore all whitespace
// ---------------------------------------------------------------------------------------------------------------------

/** Diffs whose right side (modified model URI) ignores every whitespace difference. */
const ignoreAllWhitespace = new Set<string>()

/**
 * Make the diff of `modified` ignore all whitespace. Returns the `maxComputationTime` to pass with the diff options:
 * it differs between the two modes so Monaco recomputes (and does not reuse a cached result) when the mode changes.
 */
export function setIgnoreAllWhitespace(modified: Monaco.Uri, on: boolean): number {
  if (on) ignoreAllWhitespace.add(modified.toString())
  else ignoreAllWhitespace.delete(modified.toString())
  return on ? 5001 : 5000
}

export function forgetDiff(modified: Monaco.Uri): void {
  ignoreAllWhitespace.delete(modified.toString())
}

interface LineRangeLike {
  startLineNumber: number
  endLineNumberExclusive: number
}
interface RangeMappingLike {
  originalRange: Monaco.IRange
  modifiedRange: Monaco.IRange
}
interface LineMappingLike {
  original: LineRangeLike
  modified: LineRangeLike
  innerChanges?: RangeMappingLike[]
  withInnerChangesFromLineRanges(): LineMappingLike
}
interface DocumentDiff {
  identical: boolean
  quitEarly: boolean
  changes: LineMappingLike[]
  moves: unknown[]
}
type ComputeDiff = (original: Monaco.Uri, modified: Monaco.Uri, options: Record<string, unknown>, algorithm: unknown) => Promise<DocumentDiff | null>

let tempSeq = 0

function patchDiffComputation(monaco: MonacoApi): void {
  const svc = StandaloneServices.get<{ computeDiff: ComputeDiff }>(IEditorWorkerService)
  const original = svc.computeDiff.bind(svc)
  svc.computeDiff = async (a, b, options, algorithm) => {
    if (!ignoreAllWhitespace.has(b.toString())) return original(a, b, options, algorithm)
    const ma = monaco.editor.getModel(a)
    const mb = monaco.editor.getModel(b)
    if (!ma || !mb) return original(a, b, options, algorithm)
    // Same line structure, whitespace removed: the line diff of these is the whitespace-insensitive line diff.
    const n = ++tempSeq
    const ta = monaco.editor.createModel(
      stripWhitespaceLines(ma.getLinesContent()),
      'plaintext',
      monaco.Uri.from({
        scheme: 'inmemory',
        authority: 'nx-ws',
        path: `/${n}/a`,
      }),
    )
    const tb = monaco.editor.createModel(
      stripWhitespaceLines(mb.getLinesContent()),
      'plaintext',
      monaco.Uri.from({
        scheme: 'inmemory',
        authority: 'nx-ws',
        path: `/${n}/b`,
      }),
    )
    try {
      const [stripped, real] = await Promise.all([
        original(ta.uri, tb.uri, { ...options, ignoreTrimWhitespace: false, computeMoves: false }, algorithm),
        original(a, b, { ...options, ignoreTrimWhitespace: true, computeMoves: false }, algorithm),
      ])
      if (!stripped) return real
      const inner: InnerChange[] = (real?.changes ?? []).flatMap((c) => c.innerChanges ?? [])
      const text = (m: Monaco.editor.ITextModel) => (r: Monaco.IRange) => m.getValueInRange(r)
      const changes = stripped.changes.map((c) => {
        const block: DiffBlock = { original: c.original, modified: c.modified }
        const kept = blockInnerChanges(block, inner, text(ma), text(mb))
        const Mapping = c.constructor as new (o: LineRangeLike, m: LineRangeLike, inner: InnerChange[]) => LineMappingLike
        return kept.length ? new Mapping(c.original, c.modified, kept) : c.withInnerChangesFromLineRanges()
      })
      return {
        identical: changes.length === 0,
        quitEarly: stripped.quitEarly || !!real?.quitEarly,
        changes,
        moves: [],
      }
    } finally {
      ta.dispose()
      tb.dispose()
    }
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// text area focus in pop-out windows
// ---------------------------------------------------------------------------------------------------------------------

/** The focused element of the document (or shadow root) an element is in. */
function activeIn(el: Element): Element | null {
  const root = el.getRootNode() as Document | ShadowRoot
  return 'activeElement' in root ? root.activeElement : null
}

function patchTextAreaFocus(): void {
  const proto = TextAreaWrapper.prototype
  const setSelection = proto.setSelectionRange
  proto.hasFocus = function (this: TextAreaWrapper) {
    const el = this._actual
    return el.isConnected && activeIn(el) === el
  }
  proto.setSelectionRange = function (this: TextAreaWrapper, reason: string, start: number, end: number) {
    const el = this._actual
    if (el.ownerDocument === document) return setSelection.call(this, reason, start, end)
    // Monaco's logic, with the text area's own document deciding what is focused.
    const focused = activeIn(el) === el
    if (focused && el.selectionStart === start && el.selectionEnd === end) return
    this.setIgnoreSelectionChangeTime('setSelectionRange')
    if (focused) {
      el.setSelectionRange(start, end)
      return
    }
    try {
      const scroll = saveParentsScrollTop(el)
      el.focus()
      el.setSelectionRange(start, end)
      restoreParentsScrollTop(el, scroll)
    } catch {
      /* off-DOM */
    }
  }
}

// ---------------------------------------------------------------------------------------------------------------------

let installed = false

export function installServices(monaco: MonacoApi): void {
  if (installed) return
  installed = true
  patchLayoutService()
  patchTextAreaFocus()
  patchFontZoom()
  registerDiffGutter()
  patchDiffComputation(monaco)
}
