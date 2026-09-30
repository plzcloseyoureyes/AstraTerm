/*
 * Live editor tabs register a controller here so commands, the close guard, tab menus and vim ex commands can act on
 * them without importing the (lazy) editor chunk. Also tracks which tabs have unsaved changes.
 */
import { create } from 'zustand'
import type { PrintableDoc } from './monaco/session'
import { setTabTitle, useWorkspaceStore } from '@/stores/workspace'

export const EDITOR_KINDS: ReadonlySet<string> = new Set(['editor', 'text', 'diff'])

export type ToggleName = 'wrap' | 'whitespace' | 'minimap' | 'vim' | 'readOnly'

export interface EditorController {
  tabId: string
  kind: 'editor' | 'text' | 'diff'
  /** Unsaved changes? */
  isDirty(): boolean
  /** Save (Ctrl+S). Resolves true once saved, false when cancelled or failed (errors are reported by the tab). */
  save(): Promise<boolean>
  saveAs?(): Promise<boolean>
  /** Forget unsaved changes (close without saving). */
  discard?(): void
  reload?(): void
  find?(replace?: boolean): void
  gotoLine?(): void
  /** Font zoom: +1 / -1, 0 = reset. */
  zoom?(delta: number): void
  toggle?(what: ToggleName): void
  /** Diff navigation. */
  nextChange?(dir: 1 | -1): void
  compareWithSaved?(): void
  /** Format the document (languages with a formatter). */
  format?(): void
  /** Monaco's command palette (F1). */
  editorCommands?(): void
  /** Switch between text and hex view. */
  switchMode?(): void
  /** Quick-open another file of the same folder (path bar). */
  goToFile?(): void
  /** Reveal a 1-based line. */
  revealLine?(line: number): void
  /** Select the first occurrence of a text (find in files). */
  revealText?(text: string): void
  focus?(): void
  /** Snapshot of the text (diffing an open tab). */
  getText?(): string
  /** The text document for HTML export / printing (null in hex view or while comparing). */
  printable?(): PrintableDoc | null
}

const controllers = new Map<string, EditorController>()

export function registerController(c: EditorController): () => void {
  controllers.set(c.tabId, c)
  return () => {
    if (controllers.get(c.tabId) === c) controllers.delete(c.tabId)
  }
}

export function getController(tabId: string | undefined | null): EditorController | undefined {
  return tabId ? controllers.get(tabId) : undefined
}

/** Controller of the active dock tab (when it is an editor tab). */
export function activeController(): EditorController | undefined {
  return getController(useWorkspaceStore.getState().activeTabId)
}

/** Is the active tab one of the editor kinds? */
export function isEditorTabActive(): boolean {
  const s = useWorkspaceStore.getState()
  if (!s.activeTabId) return false
  const tab = s.tabs.find((t) => t.id === s.activeTabId)
  return !!tab && EDITOR_KINDS.has(tab.kind)
}

// ---------------------------------------------------------------------------------------------------------------------
// dirty state
// ---------------------------------------------------------------------------------------------------------------------

const useDirtyStore = create<{ dirty: Record<string, true> }>(() => ({ dirty: {} }))

export function setTabDirty(tabId: string, dirty: boolean): void {
  useDirtyStore.setState((s) => {
    if (!!s.dirty[tabId] === dirty) return s
    const next = { ...s.dirty }
    if (dirty) next[tabId] = true
    else delete next[tabId]
    return { dirty: next }
  })
}

export function anyDirty(): boolean {
  for (const c of controllers.values()) {
    try {
      if (c.isDirty()) return true
    } catch {
      /* ignore */
    }
  }
  return false
}

export const DIRTY_MARK = '● '

/** Keep the "● " dirty marker of a tab title in sync (a user rename stays; only the marker changes). */
export function applyDirtyTitle(tabId: string, dirty: boolean): void {
  const tab = useWorkspaceStore.getState().tabs.find((t) => t.id === tabId)
  if (!tab) return
  const base = tab.title.startsWith(DIRTY_MARK) ? tab.title.slice(DIRTY_MARK.length) : tab.title
  const next = dirty ? DIRTY_MARK + base : base
  if (next !== tab.title) setTabTitle(tabId, next, { force: true })
}
