/*
 * Diff / merge pane (TOOL-3, FILE-10 conflict resolution): Monaco's diff editor, side by side or inline. Syntax
 * highlighting, change navigation, copying a change to the other side (arrows in the gutter between the sides — → and,
 * when the left side is editable, ← — plus commands for both directions), ignoring leading / trailing or all
 * whitespace, collapsed unchanged regions, editable sides. Each side keeps its file's line endings (BreakTracker, see
 * eol.ts).
 */
import { useEffect, useImperativeHandle, useLayoutEffect, useRef, type ReactNode, type Ref } from 'react'
import type * as Monaco from 'monaco-editor/editor'
import { cn } from '@/lib/utils'
import { EOL_SEQ, type Eol } from './codec'
import { EditorLoadError, useMonaco } from './CodeEditor'
import { BreakTracker, joinWithBreaks, normalizeBreaks, type BreakInfo } from './eol'
import { detectLanguage, findLanguage, PLAIN_ID } from './languages'
import { installAppKeys } from './monaco/keys'
import { createOverflowHost, HUGE_CHARS, LARGE_CHARS, sharedOptions } from './monaco/session'
import { editorSettings } from './settings'
import { LoadingOverlay } from './ui'
import './editor.css'

export type Side = 'a' | 'b'

export interface MergePaneHandle {
  getText(side: Side): string
  /** Text of a side as it is written to its file (line endings per eol.ts). */
  getTextForSave(side: Side, meta: { eol: Eol; mixedEol: boolean }): string
  /** Marked line breaks of a side (breaks whose sequence differs from its main line ending). */
  getBreaks(side: Side): BreakInfo[]
  /** Does a side differ from `baseline`? */
  isModified(side: Side, baseline: string): boolean
  setText(side: Side, text: string): void
  nextChange(dir: 1 | -1): void
  /** Copy the change under the cursor to the other side ('toB' = left → right). */
  copyChunk(dir: 'toA' | 'toB'): boolean
  chunkCount(): number
  openSearch(): void
  gotoLine(): void
  focus(side?: Side): void
  /** Side that has (or last had) keyboard focus. */
  focusedSide(): Side
}

export interface MergeSide {
  /** "\n"-joined text. */
  text: string
  /** Line breaks with another sequence than the side's main line ending (mixed line endings). */
  breaks?: BreakInfo[]
  readOnly?: boolean
  label?: ReactNode
}

export interface MergePaneProps {
  tabId: string
  a: MergeSide
  b: MergeSide
  /** Used for language detection. */
  path: string
  /** Language name or Monaco id (undefined = detect). */
  language?: string
  unified: boolean
  /** true: ignore leading / trailing whitespace; 'all': ignore every whitespace difference. */
  ignoreWhitespace: boolean | 'all'
  collapse: boolean
  /** Gutter arrows: 'a-to-b' shows them (→ always, ← when side a is editable); 'none' hides them. */
  revert: 'a-to-b' | 'none'
  zoom: number
  wrap?: boolean
  onChange?: (side: Side) => void
  onChunks?: (count: number) => void
  ref?: Ref<MergePaneHandle>
  className?: string
}

let seq = 0

interface Live {
  diff: Monaco.editor.IStandaloneDiffEditor
  models: Record<Side, Monaco.editor.ITextModel>
  trackers: Record<Side, BreakTracker>
  editors: Record<Side, Monaco.editor.ICodeEditor>
}

/** A change in the format of `getLineChanges()` (end line 0 = no lines on that side: the gap after the start line). */
interface LineChange {
  originalStartLineNumber: number
  originalEndLineNumber: number
  modifiedStartLineNumber: number
  modifiedEndLineNumber: number
}

/** A gutter block (line ranges, end exclusive) as a LineChange. */
function toLineChange(b: { original: { startLineNumber: number; endLineNumberExclusive: number }; modified: { startLineNumber: number; endLineNumberExclusive: number } }): LineChange {
  const side = (r: { startLineNumber: number; endLineNumberExclusive: number }): [number, number] =>
    r.endLineNumberExclusive > r.startLineNumber ? [r.startLineNumber, r.endLineNumberExclusive - 1] : [r.startLineNumber - 1, 0]
  const [os, oe] = side(b.original)
  const [ms, me] = side(b.modified)
  return { originalStartLineNumber: os, originalEndLineNumber: oe, modifiedStartLineNumber: ms, modifiedEndLineNumber: me }
}

/** Does a 1-based line lie in a change's range of one side ("end 0" = no lines: the gap after `start`)? */
function inChange(line: number, start: number, end: number): boolean {
  return end === 0 ? line === start || line === start + 1 : line >= start && line <= end
}

export function MergePane(props: MergePaneProps) {
  const { tabId, unified, ignoreWhitespace, collapse, revert, zoom, ref } = props
  const hostRef = useRef<HTMLDivElement>(null)
  const live = useRef<Live | null>(null)
  const lastFocused = useRef<Side>('b')
  const settings = editorSettings.use()
  const latest = useRef(props)
  latest.current = props
  const { api, error, retry } = useMonaco()
  const chunkFrame = useRef(0)

  const options = (modified?: Monaco.Uri): Monaco.editor.IDiffEditorOptions => {
    const p = latest.current
    const s = editorSettings.get()
    const large = Math.max(p.a.text.length, p.b.text.length) > LARGE_CHARS
    const uri = modified ?? live.current?.models.b.uri
    return {
      ...sharedOptions(s, p.zoom, large),
      renderSideBySide: !p.unified,
      useInlineViewWhenSpaceIsLimited: false,
      ignoreTrimWhitespace: !!p.ignoreWhitespace,
      // (the "ignore all whitespace" mode is a different diff computation: see monaco/services.ts)
      maxComputationTime: api && uri ? api.setIgnoreAllWhitespace(uri, p.ignoreWhitespace === 'all') : 5000,
      hideUnchangedRegions: { enabled: p.collapse, contextLineCount: 3, minimumLineCount: 6, revealLineCount: 20 },
      renderGutterMenu: p.revert !== 'none',
      renderMarginRevertIcon: p.revert === 'a-to-b' && !p.b.readOnly,
      originalEditable: !p.a.readOnly,
      readOnly: !!p.b.readOnly,
      enableSplitViewResizing: false,
      renderOverviewRuler: true,
      renderIndicators: true,
      diffAlgorithm: 'advanced',
      maxFileSize: 64,
      wordWrap: p.wrap ? 'on' : 'off',
      minimap: { enabled: false },
      stickyScroll: { enabled: false },
    }
  }

  const reportChunks = () => {
    if (chunkFrame.current) return
    chunkFrame.current = requestAnimationFrame(() => {
      chunkFrame.current = 0
      latest.current.onChunks?.(live.current?.diff.getLineChanges()?.length ?? 0)
    })
  }

  const languageOf = (): string => {
    const p = latest.current
    const info = p.language !== undefined ? (p.language ? findLanguage(p.language) : null) : detectLanguage(p.path, p.b.text || p.a.text)
    return info?.id ?? PLAIN_ID
  }

  useLayoutEffect(() => {
    const host = hostRef.current
    if (!host || !api) return
    const { monaco } = api
    const p = latest.current
    const n = ++seq
    const path = '/' + (p.path || 'compare').replace(/^[\\/]+/, '').replace(/\\/g, '/')
    const mk = (side: Side, text: string) => {
      const m = monaco.editor.createModel(normalizeBreaks(text), PLAIN_ID, monaco.Uri.from({ scheme: 'inmemory', authority: `nx-diff${n}${side}`, path }))
      m.setEOL(monaco.editor.EndOfLineSequence.LF)
      return m
    }
    const models = { a: mk('a', p.a.text), b: mk('b', p.b.text) }
    const trackers = {
      a: new BreakTracker(p.a.breaks ?? [], models.a.getAlternativeVersionId()),
      b: new BreakTracker(p.b.breaks ?? [], models.b.getAlternativeVersionId()),
    }
    const overflow = createOverflowHost(host)
    const diff = monaco.editor.createDiffEditor(host, {
      ...options(models.b.uri),
      overflowWidgetsDomNode: overflow.node,
      ariaLabel: 'Comparison',
      originalAriaLabel: 'Left side of the comparison',
      modifiedAriaLabel: 'Right side of the comparison',
    })
    diff.setModel({ original: models.a, modified: models.b })
    const editors = { a: diff.getOriginalEditor(), b: diff.getModifiedEditor() }
    live.current = { diff, models, trackers, editors }
    applyLanguage()
    api.bindEditorTab(editors.a, tabId)
    api.bindEditorTab(editors.b, tabId)
    overflow.follow(() => {
      if (live.current?.diff === diff) diff.layout()
    })
    // Until the first comparison is computed, the count is unknown.
    latest.current.onChunks?.(-1)
    const offCopyLeft = api.onCopyToLeft(models.b.uri, (block) => {
      copyChange('toA', toLineChange(block))
    })

    let revealed = false
    const subs: Monaco.IDisposable[] = [
      diff.onDidUpdateDiff(() => {
        reportChunks()
        if (!revealed) {
          revealed = true
          if (diff.getLineChanges()?.length) diff.revealFirstDiff()
        }
      }),
    ]
    for (const side of ['a', 'b'] as Side[]) {
      subs.push(
        models[side].onDidChangeContent((e) => {
          if (!e.isFlush) trackers[side].update(e.changes, models[side].getAlternativeVersionId(), e.isUndoing || e.isRedoing)
          latest.current.onChange?.(side)
        }),
        editors[side].onDidFocusEditorWidget(() => {
          lastFocused.current = side
        }),
      )
    }
    const offKeys = installAppKeys(host, { tabId, vim: () => false })
    return () => {
      offKeys()
      offCopyLeft()
      api.forgetDiff(models.b.uri)
      subs.forEach((d) => d.dispose())
      if (chunkFrame.current) cancelAnimationFrame(chunkFrame.current)
      chunkFrame.current = 0
      live.current = null
      diff.dispose()
      overflow.dispose()
      models.a.dispose()
      models.b.dispose()
    }
    // Created once per mount (and Monaco load); options are updated in place below.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [api])

  function applyLanguage() {
    const l = live.current
    if (!l || !api) return
    let id = languageOf()
    if (Math.max(l.models.a.getValueLength(), l.models.b.getValueLength()) > HUGE_CHARS) id = api.liteLanguage(id) ?? id
    for (const m of [l.models.a, l.models.b]) if (m.getLanguageId() !== id) api.monaco.editor.setModelLanguage(m, id)
  }

  useEffect(() => {
    live.current?.diff.updateOptions(options())
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [unified, ignoreWhitespace, collapse, revert, zoom, props.wrap, props.a.readOnly, props.b.readOnly, settings])

  useEffect(() => {
    applyLanguage()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [props.path, props.language])

  const focusedSide = (): Side => {
    const l = live.current
    if (!l || latest.current.unified) return 'b'
    if (l.editors.a.hasWidgetFocus()) return 'a'
    if (l.editors.b.hasWidgetFocus()) return 'b'
    return lastFocused.current
  }

  const run = (side: Side, action: string) => {
    const ed = live.current?.editors[side]
    if (!ed) return
    ed.focus()
    void ed.getAction(action)?.run()
  }

  /** Replace one side's lines of a change with the other side's ('toB' = left → right). */
  function copyChange(dir: 'toA' | 'toB', change: LineChange): boolean {
    const l = live.current
    if (!l || !api) return false
    const from: Side = dir === 'toB' ? 'a' : 'b'
    const to: Side = dir === 'toB' ? 'b' : 'a'
    const dest = l.editors[to]
    if (dest.getOption(api.monaco.editor.EditorOption.readOnly)) return false
    const src = l.models[from]
    const dst = l.models[to]
    const [sStart, sEnd] = from === 'a' ? [change.originalStartLineNumber, change.originalEndLineNumber] : [change.modifiedStartLineNumber, change.modifiedEndLineNumber]
    const [dStart, dEnd] = to === 'a' ? [change.originalStartLineNumber, change.originalEndLineNumber] : [change.modifiedStartLineNumber, change.modifiedEndLineNumber]
    const text = sEnd === 0 ? '' : src.getValueInRange({ startLineNumber: sStart, startColumn: 1, endLineNumber: sEnd, endColumn: src.getLineMaxColumn(sEnd) })
    const count = dst.getLineCount()
    let range: Monaco.IRange
    let insert: string
    if (dEnd === 0) {
      // insert the lines after line dStart (0 = at the top)
      if (dStart === 0) {
        range = { startLineNumber: 1, startColumn: 1, endLineNumber: 1, endColumn: 1 }
        insert = text + '\n'
      } else {
        const col = dst.getLineMaxColumn(dStart)
        range = { startLineNumber: dStart, startColumn: col, endLineNumber: dStart, endColumn: col }
        insert = '\n' + text
      }
    } else if (sEnd === 0) {
      // delete the destination's lines with one of their line breaks
      if (dEnd < count) range = { startLineNumber: dStart, startColumn: 1, endLineNumber: dEnd + 1, endColumn: 1 }
      else if (dStart > 1) range = { startLineNumber: dStart - 1, startColumn: dst.getLineMaxColumn(dStart - 1), endLineNumber: dEnd, endColumn: dst.getLineMaxColumn(dEnd) }
      else range = dst.getFullModelRange()
      insert = ''
    } else {
      range = { startLineNumber: dStart, startColumn: 1, endLineNumber: dEnd, endColumn: dst.getLineMaxColumn(dEnd) }
      insert = text
    }
    dest.pushUndoStop()
    dest.executeEdits('nexterm.copyChange', [{ range, text: insert }])
    dest.pushUndoStop()
    return true
  }

  useImperativeHandle(
    ref,
    (): MergePaneHandle => ({
      getText: (side) => live.current?.models[side].getValue() ?? normalizeBreaks(latest.current[side].text),
      getTextForSave: (side, meta) => {
        const l = live.current
        const text = l ? l.models[side].getValue() : normalizeBreaks(latest.current[side].text)
        if (meta.mixedEol) return joinWithBreaks(text, l ? l.trackers[side].get() : (latest.current[side].breaks ?? []), meta.eol)
        return meta.eol === 'lf' ? text : text.replace(/\n/g, EOL_SEQ[meta.eol])
      },
      getBreaks: (side) => {
        const l = live.current
        return l ? [...l.trackers[side].get()] : (latest.current[side].breaks ?? [])
      },
      isModified: (side, baseline) => {
        const m = live.current?.models[side]
        if (!m) return false
        const base = normalizeBreaks(baseline)
        return m.getValueLength() !== base.length || m.getValue() !== base
      },
      setText: (side, text) => {
        const l = live.current
        if (!l) return
        const m = l.models[side]
        const ed = l.editors[side]
        const next = normalizeBreaks(text)
        if (m.getValue() === next) return
        // One undoable edit; a replaced side starts without marks (its breaks get the main line ending).
        ed.pushUndoStop()
        ed.executeEdits('nexterm.replace', [{ range: m.getFullModelRange(), text: next }])
        ed.pushUndoStop()
        l.trackers[side].reset([], m.getAlternativeVersionId())
        reportChunks()
      },
      nextChange: (dir) => {
        const l = live.current
        if (!l) return
        l.editors[focusedSide()].focus()
        l.diff.goToDiff(dir > 0 ? 'next' : 'previous')
      },
      copyChunk: (dir) => {
        const l = live.current
        if (!l) return false
        const side = focusedSide()
        const line = l.editors[side].getPosition()?.lineNumber ?? 1
        const change = (l.diff.getLineChanges() ?? []).find((c) =>
          side === 'a' ? inChange(line, c.originalStartLineNumber, c.originalEndLineNumber) : inChange(line, c.modifiedStartLineNumber, c.modifiedEndLineNumber),
        )
        return !!change && copyChange(dir, change)
      },
      chunkCount: () => live.current?.diff.getLineChanges()?.length ?? 0,
      openSearch: () => run(focusedSide(), 'actions.find'),
      gotoLine: () => run(focusedSide(), 'editor.action.gotoLine'),
      focus: (side) => live.current?.editors[latest.current.unified ? 'b' : (side ?? lastFocused.current)].focus(),
      focusedSide,
    }),
  )

  return (
    <div className={cn('nx-merge', props.className)}>
      {!unified && (props.a.label || props.b.label) && (
        <div className="grid shrink-0 grid-cols-2 border-b bg-muted/40 text-xs text-muted-foreground">
          <div className="min-w-0 truncate px-3 py-1">{props.a.label}</div>
          <div className="min-w-0 truncate border-l px-3 py-1">{props.b.label}</div>
        </div>
      )}
      {unified && (props.a.label || props.b.label) && (
        <div className="grid shrink-0 grid-cols-[auto_minmax(0,1fr)] gap-x-2 border-b bg-muted/40 px-3 py-1 text-xs text-muted-foreground">
          <span className="font-mono text-destructive" aria-label="Original">−</span>
          <span className="min-w-0 truncate">{props.a.label}</span>
          <span className="font-mono text-success" aria-label="Modified">+</span>
          <span className="min-w-0 truncate">{props.b.label}</span>
        </div>
      )}
      <div className="relative min-h-0 flex-1">
        {/* Keyboard owner (app/keybindings.ts): Monaco keeps its keys; monaco/keys.ts, installed here, adds the editor's. */}
        <div ref={hostRef} className="absolute inset-0" data-keyboard-owner="editor" />
        <LoadingOverlay active={!api && !error} label="Loading the editor…" />
        {!!error && !api && <EditorLoadError error={error} onRetry={retry} />}
      </div>
    </div>
  )
}
