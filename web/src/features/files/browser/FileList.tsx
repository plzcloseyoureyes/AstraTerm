/*
 * The file list: a virtualized, keyboard-driven grid (role="grid", aria-multiselectable) with the classic file-manager columns,
 * sortable / resizable headers, multi-select (click, Shift, Ctrl/⌘, keyboard), type-ahead, inline rename (F2),
 * drag & drop (OS files in, rows between views, single files out) and the `file` context menu.
 */
import { memo, useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type CSSProperties, type ReactNode } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import { ArrowDown, ArrowUp } from 'lucide-react'
import type { FileEntry } from '@/api/types'
import { DynamicContextMenu } from '@/components/menu-items'
import { cn, formatRelativeTime } from '@/lib/utils'
import { useIsCut } from '../clipboard'
import { acceptDrag, dragKindOf, dropModeFor, endInternalDrag, performDrop, startInternalDrag } from '../dnd'
import { entryPerm, formatExactBytes, formatMtime, formatSize, isDirLike, isPartialUpload } from '../format'
import { FileIcon } from '../icons'
import { basename, splitName } from '../paths'
import { filesSettings } from '../settings'
import type { ColumnId, FsContext, ListRow } from '../types'
import type { ColumnModel } from './columns'
import type { BrowserController } from './controller'
import { buildFileMenu } from './menu'
import { useView } from './viewStore'

const HEADER_H = 24

export interface FileListProps {
  controller: BrowserController
  ctx: FsContext
  rows: ListRow[]
  columns: ColumnModel
  rowHeight: number
  /** A new folder is on its way (the previous rows stay; the location bar shows the indicator): aria-busy only. */
  busy?: boolean
  /** Identity of what the rows show (the folder): a change replaces the rows (scroll / hover state start over). */
  contentKey?: string
  /** Replaces the rows (loading skeleton, error). */
  placeholder?: ReactNode
  /** Shown below the rows (empty folder, no filter matches). */
  after?: ReactNode
  /** Label for assistive tech ("Files in /home/test"). */
  label: string
  /** Commander pane: F5..F8 and Tab bubble to the tab. */
  commander?: boolean
  active?: boolean
}

/** Dwell time on a folder row before its listing is prefetched (ignores rows the pointer only passes over). */
const HOVER_PREFETCH_MS = 120

export function FileList({ controller, ctx, rows, columns, rowHeight, busy, contentKey, placeholder, after, label, commander, active = true }: FileListProps) {
  const viewId = controller.viewId
  const selected = useView(viewId, (v) => v.selected)
  const cursor = useView(viewId, (v) => v.cursor)
  const renaming = useView(viewId, (v) => v.renaming)
  const revealTick = useView(viewId, (v) => v.revealTick)
  const focusTick = useView(viewId, (v) => v.focusTick)
  const dir = useView(viewId, (v) => v.path)
  const sortBy = filesSettings.useValue('sortBy')
  const sortDesc = filesSettings.useValue('sortDesc')
  const scrollRef = useRef<HTMLDivElement>(null)
  const [focused, setFocused] = useState(false)
  const [dropTarget, setDropTargetState] = useState<string | null>(null)
  /** What a drop would do right now ("Upload to logs"), shown as a pill while dragging over the list. */
  const [dropHint, setDropHint] = useState<string | null>(null)
  const setDropTarget = useCallback((path: string | null, e?: React.DragEvent) => {
    setDropTargetState(path)
    if (path === null || !e) {
      setDropHint(null)
      return
    }
    const kind = dragKindOf(e)
    const verb = kind === 'os' ? 'Upload to' : dropModeFor(e, ctx) === 'move' ? 'Move to' : 'Copy to'
    const where = basename(path || dir || '/') || '/'
    const hint = `${verb} “${where}”`
    setDropHint((h) => (h === hint ? h : hint))
  }, [ctx, dir])
  const typeAhead = useRef({ text: '', at: 0 })
  const baseId = useId()

  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => rowHeight,
    overscan: 16,
    scrollMargin: HEADER_H,
    // The sticky header covers the first HEADER_H px of the viewport.
    scrollPaddingStart: HEADER_H,
    getItemKey: (i) => rows[i]?.entry.path ?? i,
  })

  useEffect(() => {
    virtualizer.measure()
  }, [rowHeight, virtualizer])

  const cursorIndex = useMemo(() => (cursor ? rows.findIndex((r) => r.entry.path === cursor) : -1), [rows, cursor])

  // Pointing at a folder for a moment warms its listing, so opening it shows the rows at once.
  const hover = useRef<{ path: string | null; timer: ReturnType<typeof setTimeout> | null }>({ path: null, timer: null })
  const hoverRow = (target: EventTarget | null) => {
    const el = target instanceof Element ? target.closest<HTMLElement>('[role="row"][data-index]') : null
    const row = el ? rows[Number(el.dataset.index)] : undefined
    const path = row && !row.parent && isDirLike(row.entry) ? row.entry.path : null
    const h = hover.current
    if (path === h.path) return
    if (h.timer) clearTimeout(h.timer)
    h.path = path
    h.timer = path ? setTimeout(() => controller.prefetch(path), HOVER_PREFETCH_MS) : null
  }
  useEffect(() => () => void (hover.current.timer && clearTimeout(hover.current.timer)), [])

  // New folder: back to the top (before the reveal below, which may scroll to the folder we came from).
  useLayoutEffect(() => {
    scrollRef.current?.scrollTo({ top: 0 })
  }, [dir])

  // Scroll the cursor into view when asked (keyboard moves, reveal after create / rename / "Up").
  const hasCursor = cursorIndex >= 0
  useLayoutEffect(() => {
    if (cursorIndex >= 0) virtualizer.scrollToIndex(cursorIndex, { align: 'auto' })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [revealTick, hasCursor])

  // The cursor ring never blinks across a navigation: it stays as it was when the folder change started (the list got
  // focus meanwhile — Enter in the location bar — adds no ring to the rows being left), and a focused list without a
  // cursor shows it on the first row right away, in the same frame as the new rows (the effect below makes it real).
  const ringAtBusyStart = useRef(false)
  const wasBusy = useRef(busy)
  if (busy && !wasBusy.current) ringAtBusyStart.current = focused && active
  wasBusy.current = !!busy
  const ringOn = focused && active && (!busy || ringAtBusyStart.current)
  const shownCursor = cursorIndex >= 0 ? cursorIndex : ringOn && rows.length ? 0 : -1

  // The list has the focus but no cursor (it was focused while a folder was loading): the new rows get it.
  useEffect(() => {
    if (!busy && focused && active && cursorIndex < 0 && rows.length) controller.moveCursor('start', { keep: true })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [contentKey, busy])

  // Focus requests (after rename, from the location bar's ↓, commander pane switch).
  useEffect(() => {
    if (focusTick > 0) scrollRef.current?.focus({ preventScroll: true })
  }, [focusTick])

  const pageSize = () => Math.max(1, Math.floor(((scrollRef.current?.clientHeight ?? 400) - HEADER_H) / rowHeight) - 1)

  // --- keyboard ------------------------------------------------------------------------------------------------------
  const onKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    if (e.target !== e.currentTarget) return // inline rename input
    const c = controller
    const mod = e.ctrlKey || e.metaKey
    const key = e.key
    const handled = () => {
      e.preventDefault()
      e.stopPropagation()
    }
    switch (key) {
      case 'ArrowDown':
        if (e.altKey) return
        handled()
        c.moveCursor(1, { extend: e.shiftKey, keep: mod && !e.shiftKey })
        return
      case 'ArrowUp':
        handled()
        if (e.altKey) c.up()
        else c.moveCursor(-1, { extend: e.shiftKey, keep: mod && !e.shiftKey })
        return
      case 'ArrowLeft':
        if (e.altKey) {
          handled()
          c.back()
        }
        return
      case 'ArrowRight':
        if (e.altKey) {
          handled()
          c.forward()
        }
        return
      case 'Home':
        handled()
        c.moveCursor('start', { extend: e.shiftKey })
        return
      case 'End':
        handled()
        c.moveCursor('end', { extend: e.shiftKey })
        return
      case 'PageDown':
        handled()
        c.moveCursor(pageSize(), { extend: e.shiftKey })
        return
      case 'PageUp':
        handled()
        c.moveCursor(-pageSize(), { extend: e.shiftKey })
        return
      case 'Enter':
        handled()
        if (e.altKey) c.properties()
        else c.openCursor()
        return
      case 'Backspace':
        handled()
        c.up()
        return
      case 'Delete':
        handled()
        void c.deleteSelection()
        return
      case 'F2':
        handled()
        c.startRename()
        return
      case 'F3':
        handled()
        c.preview()
        return
      case 'F4':
        handled()
        c.edit()
        return
      case 'F5':
        if (commander) return
        handled()
        c.refresh()
        return
      case 'F7':
        if (commander) return
        handled()
        c.newFolder()
        return
      case 'Escape':
        if (typeAhead.current.text) typeAhead.current.text = ''
        else if (selected.size) c.clearSelection()
        else return
        handled()
        return
      case ' ':
        if (typeAhead.current.text && Date.now() - typeAhead.current.at < 800) break
        handled()
        c.toggleCursor()
        return
      case 'ContextMenu':
        handled()
        openContextMenuAtCursor()
        return
    }
    if (key === 'F10' && e.shiftKey) {
      handled()
      openContextMenuAtCursor()
      return
    }
    if (mod && !e.altKey) {
      const k = key.toLowerCase()
      if (k === 'a') {
        handled()
        c.selectAll()
      } else if (k === 'c' && !e.shiftKey) {
        handled()
        c.copyToClipboard('copy')
      } else if (k === 'x') {
        handled()
        c.copyToClipboard('cut')
      } else if (k === 'r' && !e.shiftKey) {
        handled()
        c.refresh()
      } else if (k === 'i' && e.shiftKey) {
        handled()
        c.invertSelection()
      } else if (k === 'l') {
        handled()
        c.editLocation()
      }
      // Ctrl+V: the paste event below (it can carry files from the OS clipboard).
      return
    }
    // "/" or "~" starts typing a location (GTK / Finder style).
    if ((key === '/' || key === '~') && !typeAhead.current.text) {
      handled()
      c.editLocation(key)
      return
    }
    // Type-ahead: jump to the next name starting with the typed text.
    if (key.length === 1 && !e.altKey && key !== ' ') {
      const now = Date.now()
      const t = typeAhead.current
      t.text = now - t.at > 800 ? key : t.text + key
      t.at = now
      handled()
      c.typeAhead(t.text)
    }
  }

  const onPaste = (e: React.ClipboardEvent<HTMLDivElement>) => {
    if (e.target !== e.currentTarget || !dir) return
    const files = Array.from(e.clipboardData?.files ?? [])
    e.preventDefault()
    if (files.length) {
      void import('../upload').then(({ planUpload }) =>
        planUpload(
          ctx,
          dir,
          files.map((f) => ({ file: f, relPath: f.name })),
        ),
      )
      return
    }
    controller.paste()
  }

  const openContextMenuAtCursor = () => {
    const el = scrollRef.current
    if (!el) return
    const rowEl = cursorIndex >= 0 ? el.querySelector<HTMLElement>(`[data-index="${cursorIndex}"]`) : null
    const r = (rowEl ?? el).getBoundingClientRect()
    const x = r.left + Math.min(40, r.width / 2)
    const y = rowEl ? r.top + r.height / 2 : r.top + HEADER_H + 8
    ;(rowEl ?? el).dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: x, clientY: y, view: window }))
  }

  // --- mouse / drag handlers (stable) --------------------------------------------------------------------------------
  const onRowMouseDown = useCallback(
    (row: ListRow, e: React.MouseEvent) => {
      if (e.button !== 0) return
      const toggle = e.ctrlKey || e.metaKey
      // Keep a multi-selection when starting to drag it; otherwise select on press (Explorer behaviour).
      if (!toggle && !e.shiftKey && controller.view.selected.has(row.entry.path) && controller.view.selected.size > 1) return
      controller.click(row, { toggle, range: e.shiftKey })
    },
    [controller],
  )
  const onRowClick = useCallback(
    (row: ListRow, e: React.MouseEvent) => {
      const toggle = e.ctrlKey || e.metaKey
      if (!toggle && !e.shiftKey && controller.view.selected.size > 1 && controller.view.selected.has(row.entry.path)) {
        controller.click(row, { toggle: false, range: false })
      }
    },
    [controller],
  )
  const onRowDoubleClick = useCallback((row: ListRow) => controller.open(row), [controller])
  const onRowContextMenu = useCallback((row: ListRow) => controller.contextSelect(row), [controller])
  const onRowDragStart = useCallback(
    (row: ListRow, e: React.DragEvent) => {
      if (row.parent || !dir) {
        e.preventDefault()
        return
      }
      if (!controller.view.selected.has(row.entry.path)) controller.click(row, { toggle: false, range: false })
      const entries = controller.selectedEntries()
      startInternalDrag(e, { viewId, ctx, entries: entries.length ? entries : [row.entry], dir })
    },
    [controller, ctx, dir, viewId],
  )
  const onRowDragOver = useCallback(
    (row: ListRow, e: React.DragEvent) => {
      if (!isDirLike(row.entry)) return
      if (acceptDrag(e, ctx, row.entry.path)) {
        e.stopPropagation()
        setDropTarget(row.entry.path, e)
      }
    },
    [ctx, setDropTarget],
  )
  const onRowDrop = useCallback(
    (row: ListRow, e: React.DragEvent) => {
      if (!isDirLike(row.entry)) return
      setDropTarget(null)
      // The view the user dropped on is the one the conflict question / follow-up focus belongs to.
      controller.markActive()
      performDrop(e, ctx, row.entry.path)
    },
    [ctx, controller, setDropTarget],
  )
  const onRenameCommit = useCallback((entry: FileEntry, name: string) => void controller.commitRename(entry, name), [controller])
  const onRenameCancel = useCallback(() => controller.cancelRename(), [controller])

  const onListDragOver = (e: React.DragEvent) => {
    if (!dir) return
    if (acceptDrag(e, ctx, dir)) setDropTarget('', e)
    else setDropTarget(null)
  }
  const onListDragLeave = (e: React.DragEvent) => {
    if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setDropTarget(null)
  }
  const onListDrop = (e: React.DragEvent) => {
    setDropTarget(null)
    if (!dir) return
    controller.markActive()
    performDrop(e, ctx, dir)
  }

  // --- header --------------------------------------------------------------------------------------------------------
  const headers = columns.table.getHeaderGroups()[0]?.headers ?? []
  const visibleIds = new Set(columns.visible.map((c) => c.id))
  const onSort = (id: ColumnId) => {
    const spec = columns.visible.find((c) => c.id === id)?.spec
    if (sortBy === id) filesSettings.set({ sortDesc: !sortDesc })
    else filesSettings.set({ sortBy: id, sortDesc: !!spec?.descFirst })
  }

  const gridTemplate: CSSProperties = { minWidth: columns.minWidth }
  const activeDescendant = cursorIndex >= 0 && focused ? `${baseId}-r${cursorIndex}` : undefined
  const items = virtualizer.getVirtualItems()

  return (
    <div
      className={cn('relative min-h-0 flex-1', dropTarget === '' && 'after:pointer-events-none after:absolute after:inset-0 after:rounded-sm after:ring-2 after:ring-primary/70 after:ring-inset')}
      onDragOver={onListDragOver}
      onDragLeave={onListDragLeave}
      onDrop={onListDrop}
    >
      <DynamicContextMenu items={() => buildFileMenu(controller)}>
        <div
          ref={scrollRef}
          role="grid"
          tabIndex={0}
          aria-label={label}
          aria-multiselectable="true"
          aria-rowcount={rows.length + 1}
          aria-busy={busy || undefined}
          aria-activedescendant={activeDescendant}
          data-files-grid=""
          className={cn('h-full overflow-auto outline-none select-none', focused && 'nx-files-focused')}
          onKeyDown={onKeyDown}
          onPaste={onPaste}
          onPointerOver={(e) => hoverRow(e.target)}
          onPointerLeave={() => hoverRow(null)}
          onFocus={(e) => {
            if (e.target === e.currentTarget) {
              setFocused(true)
              controller.markActive()
              // Not on the rows of the folder being left: the new folder's rows get the cursor when they arrive.
              if (cursorIndex < 0 && rows.length && !busy) controller.moveCursor('start', { keep: true })
            }
          }}
          onBlur={(e) => {
            if (e.target === e.currentTarget) setFocused(false)
          }}
          onMouseDown={(e) => {
            // Empty area below the rows: clear the selection (Explorer).
            const t = e.target as Element
            if (e.button === 0 && !t.closest('[data-index]') && !t.closest('[role="columnheader"]') && !e.ctrlKey && !e.metaKey && !e.shiftKey) {
              controller.clearSelection()
            }
            controller.markActive()
          }}
          onContextMenu={(e) => {
            if (!(e.target as Element).closest('[role="row"][data-index]')) controller.contextSelect(null)
          }}
        >
          <div style={gridTemplate} className="relative w-full">
            {/* header */}
            <div
              role="row"
              aria-rowindex={1}
              className="sticky top-0 z-10 flex border-b bg-panel/95 text-xs font-medium text-muted-foreground backdrop-blur-sm"
              style={{ height: HEADER_H }}
            >
              {headers
                .filter((h) => visibleIds.has(h.column.id as ColumnId))
                .map((h) => {
                  const id = h.column.id as ColumnId
                  const spec = columns.visible.find((c) => c.id === id)!.spec
                  const sorted = sortBy === id
                  const isName = id === 'name'
                  return (
                    <div
                      key={h.id}
                      role="columnheader"
                      aria-sort={sorted ? (sortDesc ? 'descending' : 'ascending') : 'none'}
                      className={cn('group/h relative flex h-full shrink-0 items-center', isName && 'min-w-0 flex-1')}
                      style={isName ? { minWidth: spec.minSize } : { width: h.getSize() }}
                    >
                      <button
                        type="button"
                        tabIndex={-1}
                        onClick={() => onSort(id)}
                        className={cn(
                          'flex h-full min-w-0 flex-1 items-center gap-1 px-2 outline-none hover:bg-accent/60 hover:text-foreground',
                          spec.align === 'right' && 'flex-row-reverse text-right',
                          sorted && 'text-foreground',
                          isName && 'pl-7',
                        )}
                        title={`Sort by ${spec.label.toLowerCase()}`}
                      >
                        <span className="truncate">{spec.label}</span>
                        {sorted && (sortDesc ? <ArrowDown className="size-3 shrink-0" /> : <ArrowUp className="size-3 shrink-0" />)}
                      </button>
                      {h.column.getCanResize() && (
                        <div
                          role="separator"
                          aria-orientation="vertical"
                          aria-label={`Resize ${spec.label}`}
                          onMouseDown={(e) => {
                            e.stopPropagation()
                            h.getResizeHandler(e.currentTarget.ownerDocument)(e)
                          }}
                          onTouchStart={(e) => h.getResizeHandler(e.currentTarget.ownerDocument)(e)}
                          onDoubleClick={() => h.column.resetSize()}
                          className={cn(
                            'absolute top-1 -left-[3px] z-10 h-[calc(100%-8px)] w-[6px] cursor-col-resize touch-none',
                            'after:absolute after:inset-y-0 after:left-[2.5px] after:w-px after:bg-border group-hover/h:after:bg-ring/50',
                            h.column.getIsResizing() && 'after:bg-primary',
                          )}
                        />
                      )}
                    </div>
                  )
                })}
            </div>

            {/* rows */}
            {placeholder ? (
              placeholder
            ) : (
              // A new folder replaces the rows in one step: no fade (it read as a blink on every navigation).
              <div key={contentKey} className="relative w-full" style={{ height: virtualizer.getTotalSize() }}>
                {items.map((vi) => {
                  const row = rows[vi.index]
                  if (!row) return null
                  const path = row.entry.path
                  return (
                    <Row
                      key={vi.key}
                      id={`${baseId}-r${vi.index}`}
                      index={vi.index}
                      top={vi.start - HEADER_H}
                      height={rowHeight}
                      row={row}
                      fsKey={ctx.key}
                      columns={columns.visible}
                      selected={selected.has(path)}
                      isCursor={shownCursor === vi.index}
                      listFocused={ringOn}
                      renaming={renaming === path}
                      dropTarget={dropTarget === path}
                      onMouseDown={onRowMouseDown}
                      onClick={onRowClick}
                      onDoubleClick={onRowDoubleClick}
                      onContextMenu={onRowContextMenu}
                      onDragStart={onRowDragStart}
                      onDragOver={onRowDragOver}
                      onDrop={onRowDrop}
                      onRenameCommit={onRenameCommit}
                      onRenameCancel={onRenameCancel}
                    />
                  )
                })}
              </div>
            )}
            {!placeholder && after}
          </div>
        </div>
      </DynamicContextMenu>
      {dropHint && (
        <div className="pointer-events-none absolute inset-x-0 bottom-2 z-30 flex justify-center px-2" aria-live="polite">
          <span className="max-w-full truncate rounded-full border bg-popover px-2.5 py-1 text-xs font-medium text-popover-foreground shadow-popover animate-in fade-in-0 zoom-in-95 duration-150">
            {dropHint}
          </span>
        </div>
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// row
// ---------------------------------------------------------------------------------------------------------------------

interface RowProps {
  id: string
  index: number
  top: number
  height: number
  row: ListRow
  fsKey: string
  columns: ColumnModel['visible']
  selected: boolean
  isCursor: boolean
  listFocused: boolean
  renaming: boolean
  dropTarget: boolean
  onMouseDown: (row: ListRow, e: React.MouseEvent) => void
  onClick: (row: ListRow, e: React.MouseEvent) => void
  onDoubleClick: (row: ListRow) => void
  onContextMenu: (row: ListRow) => void
  onDragStart: (row: ListRow, e: React.DragEvent) => void
  onDragOver: (row: ListRow, e: React.DragEvent) => void
  onDrop: (row: ListRow, e: React.DragEvent) => void
  onRenameCommit: (entry: FileEntry, name: string) => void
  onRenameCancel: () => void
}

const Row = memo(function Row(p: RowProps) {
  const { row, columns, selected, isCursor, listFocused } = p
  const e = row.entry
  const cut = useIsCut(p.fsKey, e.path)
  const dirLike = isDirLike(e)
  const tooltip = row.parent
    ? 'Parent folder'
    : [
        e.name,
        e.type === 'symlink' && e.linkTarget ? `→ ${e.linkTarget}${e.linkType === 'broken' ? ' (broken)' : ''}` : null,
        !dirLike ? formatExactBytes(e.size) : null,
        isPartialUpload(e) ? 'Partial upload or transfer (in progress, or interrupted)' : null,
      ]
        .filter(Boolean)
        .join('\n')
  return (
    <div
      id={p.id}
      role="row"
      data-index={p.index}
      aria-rowindex={p.index + 2}
      aria-selected={row.parent ? undefined : selected}
      draggable={!row.parent && !p.renaming}
      title={tooltip}
      onMouseDown={(ev) => p.onMouseDown(row, ev)}
      onClick={(ev) => p.onClick(row, ev)}
      onDoubleClick={() => p.onDoubleClick(row)}
      onContextMenu={() => p.onContextMenu(row)}
      onDragStart={(ev) => p.onDragStart(row, ev)}
      onDragEnd={endInternalDrag}
      onDragOver={(ev) => p.onDragOver(row, ev)}
      onDrop={(ev) => p.onDrop(row, ev)}
      className={cn(
        'absolute left-0 flex w-full items-center text-base',
        !selected && 'hover:bg-accent/55',
        selected && (listFocused ? 'bg-primary/22 text-foreground' : 'bg-primary/12'),
        isCursor && listFocused && 'shadow-[inset_0_0_0_1px_var(--ring)]',
        (e.hidden || cut || isPartialUpload(e)) && !row.parent && 'text-foreground/55',
        cut && 'italic',
        p.dropTarget && 'bg-primary/15 shadow-[inset_0_0_0_2px_var(--ring)]',
      )}
      style={{ top: p.top, height: p.height }}
    >
      {columns.map((c) => (
        <Cell key={c.id} id={c.id} width={c.size} minWidth={c.spec.minSize} align={c.spec.align}>
          {c.id === 'name' ? (
            <NameCell row={row} renaming={p.renaming} onCommit={p.onRenameCommit} onCancel={p.onRenameCancel} />
          ) : row.parent ? null : (
            <CellText id={c.id} entry={e} />
          )}
        </Cell>
      ))}
    </div>
  )
})

function Cell({ id, width, minWidth, align, children }: { id: ColumnId; width: number; minWidth: number; align?: 'right'; children: ReactNode }) {
  const isName = id === 'name'
  return (
    <div
      role="gridcell"
      className={cn('flex h-full shrink-0 items-center overflow-hidden px-2', isName && 'min-w-0 flex-1 pl-1.5', align === 'right' && 'justify-end')}
      style={isName ? { minWidth } : { width }}
    >
      {children}
    </div>
  )
}

function CellText({ id, entry }: { id: ColumnId; entry: FileEntry }) {
  switch (id) {
    case 'size':
      return <span className="truncate text-sm tabular text-muted-foreground">{formatSize(entry)}</span>
    case 'mtime':
      return (
        <span className="truncate text-sm tabular text-muted-foreground" title={formatRelativeTime(entry.mtime)}>
          {formatMtime(entry.mtime)}
        </span>
      )
    case 'owner':
      return <span className="truncate text-sm text-muted-foreground">{entry.owner ?? entry.uid ?? ''}</span>
    case 'group':
      return <span className="truncate text-sm text-muted-foreground">{entry.group ?? entry.gid ?? ''}</span>
    case 'perm':
      return <span className="truncate font-mono text-xs text-muted-foreground">{entryPerm(entry)}</span>
    default:
      return null
  }
}

function NameCell({
  row,
  renaming,
  onCommit,
  onCancel,
}: {
  row: ListRow
  renaming: boolean
  onCommit: (entry: FileEntry, name: string) => void
  onCancel: () => void
}) {
  const e = row.entry
  return (
    <span className="flex min-w-0 flex-1 items-center gap-1.5">
      <FileIcon entry={e} parent={row.parent} />
      {renaming ? (
        <RenameInput entry={e} onCommit={onCommit} onCancel={onCancel} />
      ) : (
        <>
          <span className="truncate">{row.parent ? '..' : e.name}</span>
          {e.type === 'symlink' && e.linkTarget && !row.parent && (
            <span className="hidden min-w-0 truncate text-xs text-muted-foreground @md:inline">→ {e.linkTarget}</span>
          )}
        </>
      )}
    </span>
  )
}

function RenameInput({ entry, onCommit, onCancel }: { entry: FileEntry; onCommit: (entry: FileEntry, name: string) => void; onCancel: () => void }) {
  const ref = useRef<HTMLInputElement>(null)
  const done = useRef(false)
  const [value, setValue] = useState(entry.name)
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    el.focus()
    // Select the name without its extension (folders: everything).
    const stemLen = isDirLike(entry) ? entry.name.length : splitName(entry.name).stem.length
    el.setSelectionRange(0, stemLen || entry.name.length)
  }, [entry])
  const finish = (commit: boolean) => {
    if (done.current) return
    done.current = true
    if (commit && value.trim() && value !== entry.name) onCommit(entry, value)
    else onCancel()
  }
  return (
    <input
      ref={ref}
      value={value}
      aria-label={`Rename ${entry.name}`}
      spellCheck={false}
      autoComplete="off"
      onChange={(ev) => setValue(ev.target.value)}
      onKeyDown={(ev) => {
        ev.stopPropagation()
        if (ev.key === 'Enter') {
          ev.preventDefault()
          finish(true)
        } else if (ev.key === 'Escape') {
          ev.preventDefault()
          finish(false)
        }
      }}
      onBlur={() => finish(true)}
      onMouseDown={(ev) => ev.stopPropagation()}
      onDoubleClick={(ev) => ev.stopPropagation()}
      className="h-[calc(100%-4px)] min-w-0 flex-1 rounded-sm border border-ring bg-background px-1 text-base outline-none"
    />
  )
}
