/*
 * BrowserController: the behaviour of one file view (SFTP panel, files tab, commander pane) — navigation with
 * history, selection (click / Ctrl / Shift, keyboard cursor), inline rename and the file operations bound to the
 * current folder and selection. State lives in the view store (./viewStore); the controller holds the latest render
 * inputs (FsContext, visible rows) and is registered by view id so commands, context menus and drag & drop between
 * views can reach it.
 */
import { useEffect, useRef } from 'react'
import type { FileEntry } from '@/api/types'
import {
  checksumOf,
  compareFolders,
  compressEntries,
  copyNames,
  copyPaths,
  copyScpUrls,
  createFile,
  createFolder,
  createSymlink,
  deleteEntries,
  editEntry,
  extractHere,
  openFile,
  openTerminalHere,
  pasteInto,
  permissionsOf,
  prefetchDir,
  previewEntry,
  propertiesOf,
  refreshDir,
  renameEntry,
  searchIn,
  transferEntries,
  uploadHere,
} from '../actions'
import { setFileClipboard } from '../clipboard'
import { downloadEntries } from '../download'
import { isDirLike } from '../format'
import { dirname, isRoot, normalizePath, resolveInput } from '../paths'
import type { FsContext, ListRow } from '../types'
import { EMPTY_SELECTION, getView, patchView, type ViewState } from './viewStore'

export type ViewVariant = 'panel' | 'tab' | 'pane'

export interface ControllerEnv {
  viewId: string
  ctx: FsContext | null
  /** Rows in display order (sorted, filtered, with the ".." row first when not at the root). */
  rows: ListRow[]
  /** Every entry of the shown folder (unfiltered). */
  entries: FileEntry[]
  variant: ViewVariant
  /** The other pane in commander mode (F5 / F6 targets). */
  peer?: () => BrowserController | undefined
  /** This view became the active one (commander pane focus). */
  onActivate?: () => void
}

const controllers = new Map<string, BrowserController>()
let activeViewId: string | null = null

/** Run `focus` once the closing dialog has unmounted, when the focus fell back to the page body. */
export function refocusAfterDialog(focus: () => void): void {
  setTimeout(() => {
    const a = document.activeElement
    if (!a || a === document.body || a === document.documentElement) focus()
  }, 30)
}

export function getController(viewId: string | null | undefined): BrowserController | undefined {
  return viewId ? controllers.get(viewId) : undefined
}

/** The view the user interacted with last (commands without explicit target). */
export function getActiveController(): BrowserController | undefined {
  return getController(activeViewId)
}

export class BrowserController {
  env: ControllerEnv

  constructor(env: ControllerEnv) {
    this.env = env
  }

  get viewId(): string {
    return this.env.viewId
  }

  get ctx(): FsContext | null {
    return this.env.ctx
  }

  get view(): ViewState {
    return getView(this.env.viewId)
  }

  /** The folder shown (confirmed by a listing). */
  get dir(): string | null {
    return this.view.path
  }

  markActive(): void {
    activeViewId = this.viewId
    this.env.onActivate?.()
  }

  // --- navigation --------------------------------------------------------------------------------------------------

  /** Go to a folder. `quiet`: not the user's own action here (follow terminal folder) — no loading bar, no dimming. */
  navigate(path: string, opts: { mode?: 'push' | 'replace' | 'back' | 'forward'; select?: string | null; quiet?: boolean } = {}): void {
    const target = normalizePath(path)
    const v = this.view
    if (target === v.path && !v.pending) {
      if (opts.select) this.reveal(opts.select)
      return
    }
    patchView(this.viewId, { pending: target, pendingMode: opts.mode ?? 'push', pendingSelect: opts.select ?? null, pendingQuiet: !!opts.quiet, renaming: null })
  }

  /** Go to what the user typed in the location bar (relative, ~, absolute). */
  go(input: string): void {
    const ctx = this.ctx
    const cur = this.view.path ?? ctx?.handle.home ?? '/'
    this.navigate(resolveInput(input, cur, ctx?.handle.home ?? '/'))
  }

  up(): void {
    const p = this.view.path
    if (!p || isRoot(p)) return
    this.navigate(dirname(p), { select: p })
  }

  /** The login folder (the start folder may differ: sftpRoot / initialPath). */
  home(): void {
    const ctx = this.ctx
    if (ctx) this.navigate(ctx.handle.userHome || ctx.handle.home || '/')
  }

  back(): void {
    const v = this.view
    const prev = v.back[v.back.length - 1]
    if (prev) this.navigate(prev, { mode: 'back', select: v.path })
  }

  forward(): void {
    const next = this.view.forward[0]
    if (next) this.navigate(next, { mode: 'forward' })
  }

  /** A refresh the user asked for: the listing refetches and the Refresh button spins while it is slow. */
  refresh(): void {
    const ctx = this.ctx
    const p = this.view.path
    if (!ctx || !p) return
    const viewId = this.viewId
    patchView(viewId, (v) => ({ refreshing: v.refreshing + 1 }))
    void refreshDir(ctx, p).finally(() => patchView(viewId, (v) => ({ refreshing: Math.max(0, v.refreshing - 1) })))
  }

  /** Warm the listing of a folder the user is likely to open next (hover, cursor, parent). */
  prefetch(path: string): void {
    const ctx = this.ctx
    if (ctx) prefetchDir(ctx.handle.id, path)
  }

  // --- selection ---------------------------------------------------------------------------------------------------

  private indexOf(path: string | null): number {
    if (!path) return -1
    return this.env.rows.findIndex((r) => r.entry.path === path)
  }

  /** Select an item and move the cursor to it (scrolls into view; optionally starts an inline rename). */
  reveal(path: string, opts: { rename?: boolean } = {}): void {
    patchView(this.viewId, (v) => ({
      selected: new Set([path]),
      anchor: path,
      cursor: path,
      renaming: opts.rename ? path : null,
      revealTick: v.revealTick + 1,
    }))
  }

  /** Mouse selection: plain click, Ctrl/Cmd toggle, Shift range (Ctrl+Shift adds the range). */
  click(row: ListRow, mods: { toggle: boolean; range: boolean }): void {
    this.markActive()
    const path = row.entry.path
    if (row.parent) {
      patchView(this.viewId, { cursor: path, selected: EMPTY_SELECTION, anchor: null })
      return
    }
    const v = this.view
    if (mods.range && v.anchor) {
      this.selectRange(v.anchor, path, mods.toggle)
      return
    }
    if (mods.toggle) {
      const next = new Set(v.selected)
      if (next.has(path)) next.delete(path)
      else next.add(path)
      patchView(this.viewId, { selected: next, anchor: path, cursor: path })
      return
    }
    patchView(this.viewId, { selected: new Set([path]), anchor: path, cursor: path })
  }

  /** Right-click: keep a selection that contains the row, otherwise select only the row. */
  contextSelect(row: ListRow | null): void {
    this.markActive()
    // Empty space or the ".." row: the menu acts on the folder itself.
    if (!row || row.parent) {
      patchView(this.viewId, { selected: EMPTY_SELECTION, anchor: null, ...(row ? { cursor: row.entry.path } : {}) })
      return
    }
    if (!this.view.selected.has(row.entry.path)) this.click(row, { toggle: false, range: false })
  }

  private selectRange(from: string, to: string, additive: boolean): void {
    const a = this.indexOf(from)
    const b = this.indexOf(to)
    if (a < 0 || b < 0) return
    const [lo, hi] = a < b ? [a, b] : [b, a]
    const next = additive ? new Set(this.view.selected) : new Set<string>()
    for (let i = lo; i <= hi; i++) {
      const r = this.env.rows[i]
      if (!r.parent) next.add(r.entry.path)
    }
    patchView(this.viewId, { selected: next, cursor: to })
  }

  selectAll(): void {
    const next = new Set<string>()
    for (const r of this.env.rows) if (!r.parent) next.add(r.entry.path)
    patchView(this.viewId, { selected: next })
  }

  clearSelection(): void {
    patchView(this.viewId, { selected: EMPTY_SELECTION, anchor: null })
  }

  /** Invert the selection (Explorer / Total Commander "Num *"). */
  invertSelection(): void {
    const cur = this.view.selected
    const next = new Set<string>()
    for (const r of this.env.rows) if (!r.parent && !cur.has(r.entry.path)) next.add(r.entry.path)
    patchView(this.viewId, { selected: next })
  }

  /** Keyboard cursor movement; `extend` = Shift (range from the anchor), `keep` = Ctrl (move without selecting). */
  moveCursor(delta: number | 'start' | 'end', opts: { extend?: boolean; keep?: boolean } = {}): void {
    const rows = this.env.rows
    if (!rows.length) return
    const v = this.view
    const cur = this.indexOf(v.cursor)
    let next: number
    if (delta === 'start') next = 0
    else if (delta === 'end') next = rows.length - 1
    else next = cur < 0 ? (delta > 0 ? 0 : rows.length - 1) : Math.max(0, Math.min(rows.length - 1, cur + delta))
    const row = rows[next]
    const path = row.entry.path
    if (opts.extend) {
      const anchor = v.anchor ?? v.cursor ?? path
      if (!v.anchor) patchView(this.viewId, { anchor })
      this.selectRange(anchor, path, false)
      patchView(this.viewId, (s) => ({ revealTick: s.revealTick + 1 }))
      return
    }
    if (opts.keep) {
      patchView(this.viewId, (s) => ({ cursor: path, revealTick: s.revealTick + 1 }))
      return
    }
    patchView(this.viewId, (s) => ({
      cursor: path,
      anchor: row.parent ? null : path,
      selected: row.parent ? EMPTY_SELECTION : new Set([path]),
      revealTick: s.revealTick + 1,
    }))
  }

  /** Space: toggle the cursor row. */
  toggleCursor(): void {
    const v = this.view
    const row = this.env.rows[this.indexOf(v.cursor)]
    if (!row || row.parent) return
    const next = new Set(v.selected)
    if (next.has(row.entry.path)) next.delete(row.entry.path)
    else next.add(row.entry.path)
    patchView(this.viewId, { selected: next, anchor: row.entry.path })
  }

  /** Jump to the next row whose name starts with `prefix` (type-ahead). */
  typeAhead(prefix: string): void {
    const rows = this.env.rows
    if (!rows.length || !prefix) return
    const p = prefix.toLowerCase()
    const start = Math.max(0, this.indexOf(this.view.cursor))
    for (let k = 0; k < rows.length; k++) {
      const i = (start + (prefix.length === 1 ? k + 1 : k)) % rows.length
      const r = rows[i]
      if (!r.parent && r.entry.name.toLowerCase().startsWith(p)) {
        this.reveal(r.entry.path)
        return
      }
    }
  }

  /** Selected entries in display order (never the ".." row). */
  selectedEntries(): FileEntry[] {
    const sel = this.view.selected
    if (!sel.size) return []
    const out: FileEntry[] = []
    for (const r of this.env.rows) if (!r.parent && sel.has(r.entry.path)) out.push(r.entry)
    // Selected items hidden by a filter still count.
    if (out.length < sel.size) {
      const shown = new Set(out.map((e) => e.path))
      for (const e of this.env.entries) if (sel.has(e.path) && !shown.has(e.path)) out.push(e)
    }
    return out
  }

  /** The selection, or the cursor row when nothing is selected. */
  targetEntries(): FileEntry[] {
    const sel = this.selectedEntries()
    if (sel.length) return sel
    const row = this.env.rows[this.indexOf(this.view.cursor)]
    return row && !row.parent ? [row.entry] : []
  }

  cursorRow(): ListRow | undefined {
    return this.env.rows[this.indexOf(this.view.cursor)]
  }

  files(): FileEntry[] {
    return this.env.entries.filter((e) => !isDirLike(e))
  }

  // --- opening -----------------------------------------------------------------------------------------------------

  open(row: ListRow): void {
    const ctx = this.ctx
    if (!ctx) return
    if (row.parent) {
      this.up()
      return
    }
    const e = row.entry
    if (isDirLike(e)) {
      this.navigate(e.path)
      return
    }
    openFile(ctx, e, this.files())
  }

  /** Enter: open the cursor row (a single folder / file), or edit / download a multi-selection. */
  openCursor(): void {
    const row = this.cursorRow()
    if (row) this.open(row)
  }

  edit(entry?: FileEntry): void {
    const ctx = this.ctx
    const e = entry ?? this.targetEntries()[0]
    if (ctx && e && !isDirLike(e)) void editEntry(ctx, e, this.files())
  }

  preview(entry?: FileEntry): void {
    const ctx = this.ctx
    const e = entry ?? this.targetEntries()[0]
    if (!ctx || !e) return
    if (isDirLike(e)) this.navigate(e.path)
    else previewEntry(ctx, e, this.files())
  }

  download(zip = false): void {
    const ctx = this.ctx
    const list = this.targetEntries()
    if (ctx && list.length && this.dir) downloadEntries(ctx, list, this.dir, { zip })
  }

  // --- rename ------------------------------------------------------------------------------------------------------

  startRename(path?: string): void {
    const target = path ?? this.targetEntries()[0]?.path
    if (!target) return
    this.reveal(target, { rename: true })
  }

  cancelRename(): void {
    patchView(this.viewId, (v) => ({ renaming: null, focusTick: v.focusTick + 1 }))
  }

  async commitRename(entry: FileEntry, name: string): Promise<void> {
    const ctx = this.ctx
    patchView(this.viewId, (v) => ({ renaming: null, focusTick: v.focusTick + 1 }))
    if (!ctx) return
    const to = await renameEntry(ctx, entry, name)
    if (to) this.reveal(to)
    this.refocusSoon()
  }

  // --- file operations ---------------------------------------------------------------------------------------------

  private revealFn = (path: string, opts?: { rename?: boolean }) => this.reveal(path, opts)

  newFolder(): void {
    const ctx = this.ctx
    if (ctx && this.dir) void createFolder(ctx, this.dir, this.revealFn).finally(() => this.refocusSoon())
  }

  newFile(): void {
    const ctx = this.ctx
    if (ctx && this.dir) void createFile(ctx, this.dir, this.revealFn).finally(() => this.refocusSoon())
  }

  async deleteSelection(): Promise<void> {
    const ctx = this.ctx
    const list = this.targetEntries()
    if (!ctx || !list.length) return
    // Move the cursor next to the deleted block so keyboard users can continue.
    const rows = this.env.rows
    const idx = rows.findIndex((r) => r.entry.path === list[list.length - 1].path)
    const removed = new Set(list.map((e) => e.path))
    let after: string | null = null
    for (let i = idx + 1; i < rows.length; i++) if (!removed.has(rows[i].entry.path) && !rows[i].parent) (after ??= rows[i].entry.path)
    if (!after) for (let i = idx - 1; i >= 0; i--) if (!removed.has(rows[i].entry.path) && !rows[i].parent) (after ??= rows[i].entry.path)
    const deleted = await deleteEntries(ctx, list)
    if (deleted) {
      if (after) this.reveal(after)
      else this.clearSelection()
    }
    this.refocusSoon()
  }

  upload(folder = false): void {
    const ctx = this.ctx
    if (ctx && this.dir) void uploadHere(ctx, this.dir, folder).finally(() => this.refocusSoon())
  }

  copyToClipboard(op: 'copy' | 'cut'): void {
    const ctx = this.ctx
    const list = this.targetEntries()
    if (ctx && list.length && this.dir) setFileClipboard(op, ctx, list, this.dir)
  }

  paste(intoDir?: string): void {
    const ctx = this.ctx
    const dir = intoDir ?? this.dir
    if (ctx && dir) void pasteInto(ctx, dir).finally(() => this.refocusSoon())
  }

  /** Commander: copy / move the selection into the other pane's folder (F5 / F6). */
  async toPeer(mode: 'copy' | 'move'): Promise<void> {
    const peer = this.env.peer?.()
    const ctx = this.ctx
    const list = this.targetEntries()
    if (!peer || !ctx || !peer.ctx || !peer.dir || !list.length) return
    await transferEntries(ctx, list, peer.ctx, peer.dir, mode)
    this.refocusSoon()
  }

  permissions(): void {
    const ctx = this.ctx
    const list = this.targetEntries()
    if (ctx && list.length) permissionsOf(ctx, list)
  }

  properties(): void {
    const ctx = this.ctx
    const e = this.targetEntries()[0]
    if (ctx && e) propertiesOf(ctx, e)
    else if (ctx && this.dir) {
      const cur: FileEntry = {
        name: this.dir,
        path: this.dir,
        type: 'dir',
        size: 0,
        mode: 0o40755,
        perm: '',
        mtime: '',
        hidden: false,
      }
      propertiesOf(ctx, cur)
    }
  }

  checksum(): void {
    const ctx = this.ctx
    const e = this.targetEntries().find((x) => !isDirLike(x))
    if (ctx && e) checksumOf(ctx, e)
  }

  compress(): void {
    const ctx = this.ctx
    const list = this.targetEntries()
    if (ctx && list.length && this.dir) compressEntries(ctx, list, this.dir)
  }

  extract(entry?: FileEntry): void {
    const ctx = this.ctx
    const e = entry ?? this.targetEntries()[0]
    if (ctx && e) void extractHere(ctx, e, this.dir ?? undefined)
  }

  symlink(): void {
    const ctx = this.ctx
    if (ctx && this.dir) void createSymlink(ctx, this.dir, this.targetEntries()[0]).finally(() => this.refocusSoon())
  }

  copyPath(): void {
    void copyPaths(this.targetEntries(), this.dir ?? undefined)
  }

  copyName(): void {
    void copyNames(this.targetEntries())
  }

  copyUrl(): void {
    const ctx = this.ctx
    if (ctx && this.dir) void copyScpUrls(ctx, this.targetEntries(), this.dir)
  }

  terminalHere(entry?: FileEntry): void {
    const ctx = this.ctx
    const e = entry ?? this.targetEntries()[0]
    const dir = e && isDirLike(e) ? e.path : this.dir
    if (ctx && dir) void openTerminalHere(ctx, dir)
  }

  search(): void {
    const ctx = this.ctx
    if (ctx && this.dir) searchIn(ctx, this.dir, this.viewId)
  }

  compare(entry?: FileEntry): void {
    const ctx = this.ctx
    if (!ctx) return
    const e = entry ?? this.targetEntries()[0]
    const path = e && isDirLike(e) ? e.path : this.dir
    if (!path) return
    const peer = this.env.peer?.()
    compareFolders({ ctx, path }, peer?.ctx && peer.dir ? { ctx: peer.ctx, path: peer.dir } : undefined)
  }

  focusList(): void {
    patchView(this.viewId, (v) => ({ focusTick: v.focusTick + 1 }))
  }

  /**
   * After a dialog (confirm, prompt, rename conflict…) closed: give the keyboard back to the list, unless the user
   * already put the focus somewhere else. Programmatic dialogs have no trigger to return the focus to.
   */
  refocusSoon(): void {
    refocusAfterDialog(() => this.focusList())
  }

  /** Open the location bar for typing (optionally prefilled, e.g. "/" typed in the list). */
  editLocation(seed?: string): void {
    patchView(this.viewId, (v) => ({ editTick: v.editTick + 1, editSeed: seed ?? null }))
  }
}

/** Create (once) and register the controller of a view; its env is refreshed on every render. */
export function useBrowserController(env: ControllerEnv): BrowserController {
  const ref = useRef<BrowserController | null>(null)
  if (!ref.current) ref.current = new BrowserController(env)
  ref.current.env = env
  const c = ref.current
  useEffect(() => {
    controllers.set(env.viewId, c)
    return () => {
      if (controllers.get(env.viewId) === c) controllers.delete(env.viewId)
      if (activeViewId === env.viewId) activeViewId = null
    }
  }, [env.viewId, c])
  return c
}
