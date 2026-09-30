/*
 * Editor feature — text editor and compare tool (TOOL-2, TOOL-3, FILE-10 remote editing, FILE-11 sudo save,
 * FILE-23 hex editor). The text editor and diff views are Monaco (bundled locally, workers via Vite, themed from the
 * design tokens). Everything heavy (Monaco, its languages and workers, the hex grid) is lazy-loaded with the tabs.
 *
 * Tab kinds
 *   editor  {fsId, path, readOnly?, label?, source?, mode?: 'text'|'hex', line?}   a file on a backend fs handle
 *   text    {title, content?, language?}                                           scratch / local-computer document
 *   diff    {left?, right?}  (DiffSource: {kind:'remote', fsId, path} | {kind:'text', title, docId})
 *
 * Commands (category "Editor")
 *   editor.open {fsId, path, readOnly?, label?, source?, mode?, line?}  focus the tab of that file if already open;
 *                                                                     without args: pick a remote file
 *   editor.openHex {fsId, path}          editor.new            editor.openText {title, content, language?}
 *   editor.openLocal                     editor.diff {left?, right?} (sides: {fsId, path} | {content|text, title?})
 *   editor.save ($mod+S)  editor.saveAs  editor.find  editor.replace  editor.gotoLine ($mod+G)  zoom, toggles, ...
 *   editor.findInFiles {fsId?, dir?, label?, query?}  editor.exportHtml  editor.print ($mod+P)
 *   editor.goToFile ($mod+O: quick-open a file of the current file's folder, path bar)
 */
import { lazy } from 'react'
import {
  Binary,
  Command,
  Download,
  FileCode2,
  FileDiff,
  FilePen,
  FilePlus2,
  FileSearch,
  FileOutput,
  FileText,
  FileUp,
  FolderOpen,
  FolderSearch,
  History,
  Link2,
  Map as MapIcon,
  Printer,
  RefreshCw,
  Replace,
  Save,
  SaveAll,
  Search,
  TextCursorInput,
  WandSparkles,
  WrapText,
  ZoomIn,
  ZoomOut,
} from 'lucide-react'
import { toast } from 'sonner'
import type { FileEntry } from '@/api/types'
import {
  registerCommand,
  registerContextMenu,
  registerMenu,
  registerOverlay,
  registerSettingsSection,
  registerTabKind,
  type MenuItem,
  type TabInfo,
} from '@/app/registry'
import { isDarkTheme } from '@/lib/theme'
import { copyText, errorMessage, isMac } from '@/lib/utils'
import { activeTab, focusTab, listTabs, openTab, useWorkspaceStore } from '@/stores/workspace'
import { closeFs, downloadBlob, downloadUrl, infoLabel, sourceOf, triggerDownload, type FsHandleInfo } from './api'
import { baseName, dirName } from './paths'
import { activeController, anyDirty, DIRTY_MARK, EDITOR_KINDS, getController, isEditorTabActive, type EditorController } from './controllers'
import { choose, EditorDialogHost, findInFiles, pickRemoteFile } from './dialogs'
import { currentUserId, deleteDoc, listDocs, pruneLegacy } from './docstore'
import { newScratch, openDiff, openLocalFile, openRemoteFile } from './open'
import { clearRecent, listRecent } from './recent'
import { editorSettings } from './settings'
import type { DiffSourceInput, DiffTabParams, EditorTabParams, TextTabParams } from './types'

const EditorTab = lazy(() => import('./EditorTab'))
const TextTab = lazy(() => import('./TextTab'))
const DiffTab = lazy(() => import('./DiffTab'))

const CATEGORY = 'Editor'

// ---------------------------------------------------------------------------------------------------------------------
// close guard
// ---------------------------------------------------------------------------------------------------------------------

async function guardClose(tab: TabInfo): Promise<boolean> {
  const c = getController(tab.id)
  if (!c) return true
  let dirty = false
  try {
    dirty = c.isDirty()
  } catch {
    dirty = false
  }
  if (!dirty) return true
  focusTab(tab.id)
  const name = tab.title.startsWith(DIRTY_MARK) ? tab.title.slice(DIRTY_MARK.length) : tab.title
  const r = await choose({
    title: `Save changes to ${name}?`,
    description: 'Your changes will be lost if you close the tab without saving them.',
    tone: 'warning',
    size: 'lg',
    choices: [
      { value: 'cancel', label: 'Cancel' },
      { value: 'discard', label: "Don't save", variant: 'destructive' },
      { value: 'save', label: 'Save', variant: 'default' },
    ],
    cancel: 'cancel',
  })
  if (r === 'cancel') return false
  if (r === 'discard') {
    c.discard?.()
    return true
  }
  return c.save()
}

function onEditorClosed(tab: TabInfo<EditorTabParams>): void {
  const p = tab.params
  if (!p?.ownsFs || !p.fsId) return
  const stillUsed = listTabs().some((t) => t.id !== tab.id && (t.params as { fsId?: string } | undefined)?.fsId === p.fsId)
  if (!stillUsed) void closeFs(p.fsId).catch(() => undefined)
}

function titleOf(p: TextTabParams | undefined): string {
  return p?.title || 'Untitled'
}

// ---------------------------------------------------------------------------------------------------------------------
// tab kinds
// ---------------------------------------------------------------------------------------------------------------------

registerTabKind<EditorTabParams>({
  kind: 'editor',
  title: (p) => baseName(p?.path) || 'Editor',
  icon: FileCode2,
  iconFor: (p) => (p?.mode === 'hex' ? Binary : undefined),
  component: EditorTab,
  canClose: guardClose,
  onClose: onEditorClosed,
  reopen: (closed) => void openRemoteFile({ ...(closed.params as EditorTabParams) }),
  duplicate: (tab) => {
    focusTab(tab.id)
    toast.info(`${baseName(tab.params?.path)} is already open`, { description: 'Each remote file is edited in a single tab.' })
  },
})

registerTabKind<TextTabParams>({
  kind: 'text',
  title: titleOf,
  icon: FileText,
  iconFor: (p) => (p?.mode === 'hex' ? Binary : p?.local ? FileText : FilePen),
  component: TextTab,
  canClose: guardClose,
  reopen: (closed) => void openTab({ kind: 'text', params: closed.params }),
  duplicate: (tab, position) => {
    const text = getController(tab.id)?.getText?.()
    newScratch({ title: `${titleOf(tab.params).replace(/^● /, '')} (copy)`, content: text ?? '', language: tab.params?.language, position })
  },
})

registerTabKind<DiffTabParams>({
  kind: 'diff',
  title: (p) => {
    const l = p?.left ? (p.left.kind === 'remote' ? baseName(p.left.path) : p.left.title) : '…'
    const r = p?.right ? (p.right.kind === 'remote' ? baseName(p.right.path) : p.right.title) : '…'
    return p?.left || p?.right ? `${l} ↔ ${r}` : 'Text diff'
  },
  icon: FileDiff,
  component: DiffTab,
  canClose: guardClose,
  reopen: (closed) => void openTab({ kind: 'diff', params: closed.params }),
})

// ---------------------------------------------------------------------------------------------------------------------
// commands
// ---------------------------------------------------------------------------------------------------------------------

function argTab(args: unknown): string | undefined {
  if (args && typeof args === 'object' && typeof (args as { tabId?: unknown }).tabId === 'string') return (args as { tabId: string }).tabId
  return undefined
}

function targetController(args: unknown): EditorController | undefined {
  const id = argTab(args)
  return id ? getController(id) : activeController()
}

const hasEditor = () => isEditorTabActive()
const can = (pick: (c: EditorController) => unknown) => () => {
  const c = activeController()
  return !!c && !!pick(c)
}

function withController(fn: (c: EditorController) => unknown) {
  return ({ args }: { args: unknown }) => {
    const c = targetController(args)
    if (c) return fn(c) as void
  }
}

function isRemoteArgs(a: unknown): a is EditorTabParams {
  return !!a && typeof a === 'object' && typeof (a as EditorTabParams).fsId === 'string' && typeof (a as EditorTabParams).path === 'string'
}

registerCommand<EditorTabParams | undefined>({
  id: 'editor.open',
  title: 'Open Remote File…',
  category: CATEGORY,
  icon: FolderOpen,
  keywords: ['edit', 'sftp', 'remote', 'file', 'text editor'],
  description: 'Open a file of a connected session in the text editor',
  run: async ({ args }) => {
    if (isRemoteArgs(args)) {
      openRemoteFile(args)
      return
    }
    // Start where the active editor's file is (or the last folder used).
    const cur = activeTab()
    const p = cur?.kind === 'editor' ? (cur.params as EditorTabParams) : undefined
    const picked = await pickRemoteFile({ title: 'Open remote file', fsId: p?.fsId, dir: p ? dirName(p.path) : undefined, label: p?.label })
    if (picked) openRemoteFile({ fsId: picked.fsId, path: picked.path, label: picked.label, source: picked.source, ownsFs: picked.ownsFs })
  },
})

interface FindInFilesArgs {
  fsId?: string
  dir?: string
  label?: string
  query?: string
}

registerCommand<FindInFilesArgs | undefined>({
  id: 'editor.findInFiles',
  title: 'Find in Files…',
  category: CATEGORY,
  icon: FileSearch,
  keywords: ['search', 'grep', 'files', 'folder', 'text', 'content'],
  description: 'Search a folder on a connected server for files containing a text',
  run: async ({ args }) => {
    // Default location: the folder of the active editor's file.
    const cur = activeTab()
    const p = cur?.kind === 'editor' ? (cur.params as EditorTabParams) : undefined
    const hit = await findInFiles({
      fsId: args?.fsId ?? p?.fsId,
      dir: args?.dir ?? (p ? dirName(p.path) : undefined),
      label: args?.label ?? p?.label,
      query: args?.query,
    })
    if (hit) openRemoteFile({ fsId: hit.fsId, path: hit.path, label: hit.label, source: hit.source, ownsFs: hit.ownsFs, find: hit.find })
  },
})

registerCommand<EditorTabParams>({
  id: 'editor.openHex',
  title: 'Open in Hex Editor',
  category: CATEGORY,
  icon: Binary,
  hidden: true,
  run: ({ args }) => {
    if (!isRemoteArgs(args)) throw new Error('editor.openHex needs {fsId, path}')
    openRemoteFile({ ...args, mode: 'hex' })
  },
})

registerCommand({
  id: 'editor.new',
  title: 'New Text Document',
  category: CATEGORY,
  icon: FilePlus2,
  keywords: ['scratch', 'untitled', 'notepad', 'text editor'],
  run: () => void newScratch(),
})

registerCommand<{ title?: string; content?: string; language?: string } | undefined>({
  id: 'editor.openText',
  title: 'Open Text in Editor',
  category: CATEGORY,
  icon: FileText,
  hidden: true,
  run: ({ args }) => void newScratch({ title: args?.title, content: typeof args?.content === 'string' ? args.content : '', language: args?.language }),
})

registerCommand({
  id: 'editor.openLocal',
  title: 'Open Local File in Editor…',
  category: CATEGORY,
  icon: FileUp,
  keywords: ['computer', 'disk', 'upload', 'local file'],
  run: () => openLocalFile().then(() => undefined),
})

registerCommand<{ left?: DiffSourceInput; right?: DiffSourceInput } | undefined>({
  id: 'editor.diff',
  title: 'Compare Text / Files (Diff)…',
  category: CATEGORY,
  icon: FileDiff,
  keywords: ['diff', 'compare', 'merge', 'text compare'],
  run: ({ args }) => void openDiff(args?.left, args?.right),
})

registerCommand({
  id: 'editor.save',
  title: 'Save',
  category: CATEGORY,
  icon: Save,
  keybinding: '$mod+s',
  global: true,
  when: hasEditor,
  run: withController((c) => c.save()),
})

registerCommand({
  id: 'editor.saveAs',
  title: 'Save As…',
  category: CATEGORY,
  icon: SaveAll,
  keybinding: '$mod+Shift+s',
  global: true,
  when: can((c) => c.saveAs),
  run: withController((c) => c.saveAs?.()),
})

registerCommand({
  id: 'editor.find',
  title: 'Find',
  category: CATEGORY,
  icon: Search,
  keybinding: '$mod+f',
  when: can((c) => c.find),
  run: withController((c) => c.find?.(false)),
})

registerCommand({
  id: 'editor.replace',
  title: 'Find and Replace',
  category: CATEGORY,
  icon: Replace,
  // Option+F types "ƒ" on macOS: match the physical key.
  keybinding: isMac ? '$mod+Alt+KeyF' : 'Control+h',
  when: can((c) => c.find),
  run: withController((c) => c.find?.(true)),
})

registerCommand({
  id: 'editor.gotoLine',
  title: 'Go to Line…',
  category: CATEGORY,
  icon: TextCursorInput,
  keybinding: '$mod+g',
  when: can((c) => c.gotoLine),
  run: withController((c) => c.gotoLine?.()),
})

registerCommand({
  id: 'editor.zoomIn',
  title: 'Editor: Zoom In',
  category: CATEGORY,
  icon: ZoomIn,
  keybinding: ['$mod+Equal', '$mod+Shift+Equal', '$mod+NumpadAdd'],
  when: can((c) => c.zoom),
  run: withController((c) => c.zoom?.(1)),
})

registerCommand({
  id: 'editor.zoomOut',
  title: 'Editor: Zoom Out',
  category: CATEGORY,
  icon: ZoomOut,
  keybinding: ['$mod+Minus', '$mod+NumpadSubtract'],
  when: can((c) => c.zoom),
  run: withController((c) => c.zoom?.(-1)),
})

registerCommand({
  id: 'editor.zoomReset',
  title: 'Editor: Reset Zoom',
  category: CATEGORY,
  keybinding: ['$mod+Digit0', '$mod+Numpad0'],
  when: can((c) => c.zoom),
  run: withController((c) => c.zoom?.(0)),
})

registerCommand({
  id: 'editor.toggleWordWrap',
  title: 'Toggle Word Wrap',
  category: CATEGORY,
  icon: WrapText,
  // Option+Z types "Ω" on macOS: match the physical key.
  keybinding: 'Alt+KeyZ',
  when: can((c) => c.toggle),
  run: withController((c) => c.toggle?.('wrap')),
})

registerCommand({
  id: 'editor.toggleWhitespace',
  title: 'Toggle Show Whitespace',
  category: CATEGORY,
  when: can((c) => c.toggle),
  run: withController((c) => c.toggle?.('whitespace')),
})

registerCommand({
  id: 'editor.toggleMinimap',
  title: 'Toggle Minimap',
  category: CATEGORY,
  icon: MapIcon,
  keywords: ['overview', 'code map'],
  when: can((c) => c.toggle),
  run: withController((c) => c.toggle?.('minimap')),
})

registerCommand({
  id: 'editor.formatDocument',
  title: 'Format Document',
  category: CATEGORY,
  icon: WandSparkles,
  keywords: ['prettify', 'beautify', 'indent', 'json', 'html', 'css', 'javascript'],
  when: can((c) => c.format),
  run: withController((c) => c.format?.()),
})

registerCommand({
  id: 'editor.editorCommands',
  title: 'Show Editor Commands (F1)',
  category: CATEGORY,
  icon: Command,
  keywords: ['monaco', 'command palette', 'actions', 'fold', 'multi-cursor', 'sort lines', 'transform'],
  when: can((c) => c.editorCommands),
  run: withController((c) => c.editorCommands?.()),
})

registerCommand({
  id: 'editor.toggleVim',
  title: 'Toggle Vim Mode',
  category: CATEGORY,
  keywords: ['vi', 'modal'],
  when: can((c) => c.toggle),
  run: withController((c) => c.toggle?.('vim')),
})

registerCommand({
  id: 'editor.toggleReadOnly',
  title: 'Toggle Read-Only',
  category: CATEGORY,
  when: can((c) => c.toggle),
  run: withController((c) => c.toggle?.('readOnly')),
})

registerCommand({
  id: 'editor.reload',
  title: 'Reload from Disk',
  category: CATEGORY,
  icon: RefreshCw,
  when: can((c) => c.reload),
  run: withController((c) => c.reload?.()),
})

registerCommand({
  id: 'editor.compareWithSaved',
  title: 'Compare with Saved Version',
  category: CATEGORY,
  icon: FileDiff,
  when: can((c) => c.compareWithSaved),
  run: withController((c) => c.compareWithSaved?.()),
})

registerCommand({
  id: 'editor.goToFile',
  title: 'Go to File in This Folder…',
  category: CATEGORY,
  icon: FolderSearch,
  keybinding: '$mod+o',
  keywords: ['quick open', 'sibling', 'folder', 'breadcrumbs', 'switch file'],
  description: 'Open another file from the folder of the file being edited',
  when: can((c) => c.goToFile),
  run: withController((c) => c.goToFile?.()),
})

registerCommand({
  id: 'editor.switchMode',
  title: 'Toggle Hex / Text View',
  category: CATEGORY,
  icon: Binary,
  when: can((c) => c.switchMode),
  run: withController((c) => c.switchMode?.()),
})

/** HTML export / printing with syntax colours (TOOL-2); the renderer loads on first use. */
async function exportOrPrint(c: EditorController, how: 'export' | 'print'): Promise<void> {
  const doc = c.printable?.()
  if (!doc) {
    toast.info(how === 'print' ? 'Printing is available in the text view' : 'HTML export is available in the text view')
    return
  }
  try {
    const { renderHtml, printHtml } = await import('./export')
    const s = editorSettings.get()
    let tokens: ReturnType<typeof doc.tokenize> | null = null
    try {
      tokens = doc.tokenize()
    } catch {
      /* plain text */
    }
    const html = renderHtml(doc.lines, tokens, {
      title: doc.title,
      // Paper gets dark text on white; an export keeps the look of the current theme.
      theme: how === 'print' || !isDarkTheme() ? 'light' : 'dark',
      lineNumbers: s.lineNumbers,
      tabSize: doc.tabSize,
      fontSize: how === 'print' ? 10 : s.fontSize,
    })
    if (how === 'print') await printHtml(html)
    else {
      downloadBlob(html, `${doc.title.replace(/[\\/:*?"<>|]+/g, '_') || 'document'}.html`, 'text/html;charset=utf-8')
      toast.success(`Exported ${doc.title} as HTML`)
    }
  } catch (err) {
    toast.error(how === 'print' ? 'Could not print' : 'Could not export', { description: errorMessage(err) })
  }
}

registerCommand({
  id: 'editor.exportHtml',
  title: 'Export as HTML…',
  category: CATEGORY,
  icon: FileOutput,
  keywords: ['html', 'export', 'syntax highlighting', 'colors'],
  when: can((c) => c.printable?.()),
  run: withController((c) => exportOrPrint(c, 'export')),
})

registerCommand({
  id: 'editor.print',
  title: 'Print…',
  category: CATEGORY,
  icon: Printer,
  keybinding: '$mod+p',
  keywords: ['print', 'paper', 'pdf'],
  when: can((c) => c.printable?.()),
  run: withController((c) => exportOrPrint(c, 'print')),
})

registerCommand({
  id: 'editor.nextChange',
  title: 'Diff: Next Change',
  category: CATEGORY,
  keybinding: 'F7',
  when: can((c) => c.nextChange),
  run: withController((c) => c.nextChange?.(1)),
})

registerCommand({
  id: 'editor.prevChange',
  title: 'Diff: Previous Change',
  category: CATEGORY,
  keybinding: 'Shift+F7',
  when: can((c) => c.nextChange),
  run: withController((c) => c.nextChange?.(-1)),
})

// ---------------------------------------------------------------------------------------------------------------------
// menus
// ---------------------------------------------------------------------------------------------------------------------

function recentItems(): MenuItem[] {
  const list = listRecent()
  if (!list.length) return [{ label: 'No recent files', disabled: true, run: () => undefined }]
  return [
    ...list.map<MenuItem>((r, i) => ({
      id: `recent-${i}`,
      label: `${baseName(r.path)} — ${r.label ? `${r.label}:` : ''}${r.path}`,
      icon: FileCode2,
      run: () => void openRemoteFile({ fsId: r.fsId, path: r.path, label: r.label, source: r.source }),
    })),
    { type: 'separator' },
    { label: 'Clear recent files', run: () => clearRecent() },
  ]
}

registerMenu({
  menu: 'tools',
  order: 150,
  items: () => [
    { label: 'Text editor', icon: FilePen, command: 'editor.new' },
    { label: 'Text diff', icon: FileDiff, command: 'editor.diff' },
    { label: 'Open remote file…', icon: FolderOpen, command: 'editor.open' },
    { label: 'Open local file in editor…', icon: FileUp, command: 'editor.openLocal' },
    { label: 'Find in files…', icon: FileSearch, command: 'editor.findInFiles' },
    { type: 'submenu', label: 'Recent files', icon: History, items: recentItems },
  ],
})

registerContextMenu({
  target: 'tab',
  order: 40,
  items: (ctx) => {
    if (!EDITOR_KINDS.has(ctx.kind)) return []
    const c = getController(ctx.tabId)
    const items: MenuItem[] = [{ label: 'Save', icon: Save, command: 'editor.save', args: { tabId: ctx.tabId } }]
    if (c?.saveAs) items.push({ label: 'Save as…', icon: SaveAll, command: 'editor.saveAs', args: { tabId: ctx.tabId } })
    if (c?.reload) items.push({ label: 'Reload from disk', icon: RefreshCw, run: () => c.reload?.() })
    if (c?.compareWithSaved) items.push({ label: 'Compare with saved', icon: FileDiff, run: () => c.compareWithSaved?.() })
    if (c?.printable?.()) {
      items.push(
        { label: 'Export as HTML…', icon: FileOutput, command: 'editor.exportHtml', args: { tabId: ctx.tabId } },
        { label: 'Print…', icon: Printer, command: 'editor.print', args: { tabId: ctx.tabId } },
      )
    }
    if (c?.switchMode) {
      const hex = (ctx.params as { mode?: string } | undefined)?.mode === 'hex'
      items.push({ label: hex ? 'Open as text' : 'Open in hex editor', icon: hex ? FileText : Binary, run: () => c.switchMode?.() })
    }
    if (ctx.kind === 'editor') {
      const p = ctx.params as EditorTabParams
      items.push(
        { label: 'Copy path', icon: Link2, run: () => void copyText(p.path).then((ok) => ok && toast.success('Path copied')) },
        { label: 'Download', icon: Download, run: () => triggerDownload(downloadUrl(p.fsId, p.path), baseName(p.path)) },
      )
    }
    return items
  },
})

/** Files browser context menu ("file" target): edit / hex / compare selected files. */
registerContextMenu({
  target: 'file',
  order: 20,
  items: (ctx) => {
    if (!ctx.fsId) return []
    const selection: FileEntry[] = ctx.entries ?? []
    const files = selection.filter((e: FileEntry) => e.type === 'file' || (e.type === 'symlink' && e.linkType !== 'dir' && e.linkType !== 'broken'))
    // The files browser adds its FsContext ({handle, label}); use it for labels / reconnecting when present.
    const extra = (ctx as { ctx?: { label?: unknown; handle?: Partial<FsHandleInfo> } }).ctx
    const label = typeof extra?.label === 'string' ? extra.label : extra?.handle ? infoLabel(extra.handle) : undefined
    const source = extra?.handle ? sourceOf(extra.handle) : undefined
    const base = { fsId: ctx.fsId, label, source }
    // Find in files: in the folder shown (nothing selected) or the one selected folder.
    const dirs = selection.filter((e: FileEntry) => e.type === 'dir' || (e.type === 'symlink' && e.linkType === 'dir'))
    const searchDir = selection.length === 0 ? ctx.path : selection.length === 1 && dirs.length === 1 ? dirs[0].path : undefined
    if (!files.length) {
      return searchDir
        ? [{ label: 'Find in files…', icon: FileSearch, command: 'editor.findInFiles', args: { fsId: ctx.fsId, dir: searchDir, label } }]
        : []
    }
    const items: MenuItem[] = []
    if (files.length === 1) {
      const f = files[0]
      items.push(
        { label: 'Open in text editor', icon: FilePen, run: () => void openRemoteFile({ ...base, path: f.path }) },
        { label: 'Open in hex editor', icon: Binary, run: () => void openRemoteFile({ ...base, path: f.path, mode: 'hex' }) },
        { label: 'Compare with…', icon: FileDiff, run: () => void openDiff({ fsId: ctx.fsId, path: f.path, label, source }) },
      )
    } else if (files.length === 2) {
      items.push({
        label: 'Compare files',
        icon: FileDiff,
        run: () => void openDiff({ fsId: ctx.fsId, path: files[0].path, label, source }, { fsId: ctx.fsId, path: files[1].path, label, source }),
      })
    }
    return items
  },
})

// ---------------------------------------------------------------------------------------------------------------------
// settings, dialogs, unload guard, housekeeping
// ---------------------------------------------------------------------------------------------------------------------

registerSettingsSection({
  id: 'editor',
  title: 'Editor',
  icon: FileSearch,
  order: 35,
  keywords: ['text editor', 'monaco', 'font', 'tab size', 'type check', 'diagnostics', 'typescript', 'indent', 'spaces', 'tabs', 'word wrap', 'vim', 'autosave', 'whitespace', 'indent guides', 'minimap', 'sticky scroll', 'bracket', 'colour', 'color', 'swatch', 'diff', 'hex'],
  component: lazy(() => import('./SettingsSection')),
})

registerOverlay({ id: 'editor-dialogs', component: EditorDialogHost, keepMountedWhileLocked: true, order: 60 })

if (typeof window !== 'undefined') {
  window.addEventListener('beforeunload', (e) => {
    if (!anyDirty()) return
    e.preventDefault()
    // Legacy browsers need returnValue set to show the prompt.
    e.returnValue = ''
  })
}

/** Drop stored scratch documents no open or recently closed tab refers to (and stale unsaved-change backups). */
async function collectGarbage(): Promise<void> {
  const refs = new Set<string>()
  const add = (p: unknown) => {
    const o = (p ?? {}) as { docId?: unknown; left?: { docId?: unknown }; right?: { docId?: unknown } }
    if (typeof o.docId === 'string') refs.add(o.docId)
    if (typeof o.left?.docId === 'string') refs.add(o.left.docId)
    if (typeof o.right?.docId === 'string') refs.add(o.right.docId)
  }
  const ws = useWorkspaceStore.getState()
  for (const t of ws.tabs) add(t.params)
  for (const c of ws.closed) add(c.params)
  const now = Date.now()
  const DAY = 86_400_000
  for (const d of await listDocs()) {
    if (refs.has(d.id)) continue
    const maxAge = d.id.startsWith('backup:') ? 14 * DAY : 3 * DAY
    if (now - (d.updatedAt || 0) > maxAge) await deleteDoc(d.id)
  }
  await pruneLegacy(30 * DAY)
}

/** Once per signed-in user and page: the user's own store only (their workspace decides what is referenced). */
let gcDoneFor: string | null = null
useWorkspaceStore.subscribe((s) => {
  const user = currentUserId()
  if (!s.ready || user === 'anonymous' || gcDoneFor === user) return
  gcDoneFor = user
  setTimeout(() => {
    // Signed out / switched user meanwhile: that workspace is not loaded, so nothing can be judged unused.
    if (currentUserId() === user) void collectGarbage().catch(() => undefined)
    else gcDoneFor = null
  }, 15_000)
})
