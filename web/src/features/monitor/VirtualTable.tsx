/*
 * Dense, virtualized, keyboard-navigable table (process / service / port lists can hold thousands of rows). Sorting
 * is owned by the caller (the header only reports clicks); one context menu serves every row.
 */
import { useEffect, useRef, type KeyboardEvent, type MouseEvent, type ReactNode } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import { ArrowDown, ArrowUp } from 'lucide-react'
import { ContextMenu, ContextMenuContent, ContextMenuTrigger } from '@/components/ui/context-menu'
import { cn } from '@/lib/utils'

export interface Column<T> {
  id: string
  header: string
  /** CSS grid track, e.g. "4.5rem" or "minmax(12rem,1fr)". */
  width: string
  align?: 'left' | 'right'
  sortable?: boolean
  /** Hidden below this container width (px) to keep narrow panes readable. */
  minWidth?: number
  cell: (row: T) => ReactNode
  title?: (row: T) => string | undefined
  className?: string
}

export interface SortState {
  id: string
  desc: boolean
}

export function VirtualTable<T>({
  rows,
  columns,
  rowKey,
  sort,
  onSort,
  selected,
  onSelect,
  onActivate,
  onKeyAction,
  menu,
  rowClassName,
  empty,
  ariaLabel,
  rowHeight = 26,
  width,
}: {
  rows: T[]
  columns: Column<T>[]
  rowKey: (row: T) => string
  sort?: SortState
  onSort?: (id: string) => void
  selected?: string | null
  onSelect?: (key: string | null) => void
  onActivate?: (row: T) => void
  /** Keys handled for the selected row (e.g. Delete). Return true when handled. */
  onKeyAction?: (e: KeyboardEvent, row: T) => boolean
  /** Context menu content for the selected row. */
  menu?: (row: T) => ReactNode
  rowClassName?: (row: T) => string | undefined
  empty?: ReactNode
  ariaLabel: string
  rowHeight?: number
  /** Container width (px) for responsive column hiding. */
  width?: number
}) {
  const scroller = useRef<HTMLDivElement>(null)
  const cols = columns.filter((c) => !c.minWidth || !width || width >= c.minWidth)
  const template = cols.map((c) => c.width).join(' ')
  const virt = useVirtualizer({ count: rows.length, getScrollElement: () => scroller.current, estimateSize: () => rowHeight, overscan: 12 })
  const index = selected != null ? rows.findIndex((r) => rowKey(r) === selected) : -1
  const selRow = index >= 0 ? rows[index] : undefined

  // Keep the selected row visible when it moves (sorting, refresh).
  const lastScrolled = useRef<string | null>(null)
  useEffect(() => {
    if (selected && index >= 0 && lastScrolled.current !== selected) {
      virt.scrollToIndex(index, { align: 'auto' })
      lastScrolled.current = selected
    }
  }, [selected, index, virt])

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (!rows.length) return
    const move = (to: number) => {
      const i = Math.max(0, Math.min(rows.length - 1, to))
      onSelect?.(rowKey(rows[i]))
      virt.scrollToIndex(i, { align: 'auto' })
      e.preventDefault()
    }
    const page = Math.max(1, Math.floor((scroller.current?.clientHeight ?? 300) / rowHeight) - 1)
    switch (e.key) {
      case 'ArrowDown':
        return move(index + 1)
      case 'ArrowUp':
        return move(index < 0 ? 0 : index - 1)
      case 'PageDown':
        return move(index + page)
      case 'PageUp':
        return move(index - page)
      case 'Home':
        return move(0)
      case 'End':
        return move(rows.length - 1)
      case 'Enter':
        if (selRow && onActivate) {
          onActivate(selRow)
          e.preventDefault()
        }
        return
    }
    if (selRow && onKeyAction?.(e, selRow)) e.preventDefault()
  }

  const pickRow = (e: MouseEvent) => {
    const el = (e.target as HTMLElement).closest<HTMLElement>('[data-row-key]')
    if (el?.dataset.rowKey) onSelect?.(el.dataset.rowKey)
  }

  const header = (
    <div role="row" className="sticky top-0 z-10 grid border-b bg-panel text-xs font-medium text-muted-foreground" style={{ gridTemplateColumns: template }}>
      {cols.map((c) => {
        const active = sort?.id === c.id
        const content = (
          <>
            <span className="truncate">{c.header}</span>
            {active && (sort?.desc ? <ArrowDown className="size-3 shrink-0" /> : <ArrowUp className="size-3 shrink-0" />)}
          </>
        )
        return (
          <div
            key={c.id}
            role="columnheader"
            aria-sort={active ? (sort?.desc ? 'descending' : 'ascending') : undefined}
            className={cn('flex h-7 min-w-0 items-center', c.align === 'right' && 'justify-end')}
          >
            {c.sortable && onSort ? (
              <button
                type="button"
                onClick={() => onSort(c.id)}
                className={cn(
                  'flex h-full w-full min-w-0 items-center gap-1 px-2 outline-none hover:text-foreground focus-visible:ring-1 focus-visible:ring-ring',
                  c.align === 'right' && 'justify-end',
                  active && 'text-foreground',
                )}
              >
                {content}
              </button>
            ) : (
              <span className="flex min-w-0 items-center gap-1 px-2">{content}</span>
            )}
          </div>
        )
      })}
    </div>
  )

  const body = (
    <div
      ref={scroller}
      role="grid"
      aria-label={ariaLabel}
      aria-rowcount={rows.length + 1}
      tabIndex={0}
      onKeyDown={onKeyDown}
      onMouseDown={pickRow}
      onContextMenu={pickRow}
      className="relative min-h-0 flex-1 overflow-auto outline-none focus-visible:ring-1 focus-visible:ring-ring/60 focus-visible:ring-inset"
    >
      {header}
      {rows.length === 0 ? (
        <div className="p-2">{empty}</div>
      ) : (
        <div role="rowgroup" style={{ height: virt.getTotalSize(), position: 'relative' }}>
          {virt.getVirtualItems().map((vi) => {
            const row = rows[vi.index]
            const key = rowKey(row)
            const isSel = key === selected
            return (
              <div
                key={key}
                role="row"
                aria-rowindex={vi.index + 2}
                aria-selected={isSel}
                data-row-key={key}
                onDoubleClick={() => onActivate?.(row)}
                className={cn(
                  'absolute inset-x-0 grid cursor-default items-center border-b border-border/40 text-sm',
                  isSel ? 'bg-primary/15' : 'hover:bg-accent/50',
                  rowClassName?.(row),
                )}
                style={{ height: rowHeight, transform: `translateY(${vi.start}px)`, gridTemplateColumns: template }}
              >
                {cols.map((c) => (
                  <div
                    key={c.id}
                    role="gridcell"
                    title={c.title?.(row)}
                    className={cn('min-w-0 truncate px-2', c.align === 'right' && 'text-right tabular', c.className)}
                  >
                    {c.cell(row)}
                  </div>
                ))}
              </div>
            )
          })}
        </div>
      )}
    </div>
  )

  if (!menu) return body
  return (
    <ContextMenu>
      <ContextMenuTrigger asChild>{body}</ContextMenuTrigger>
      {selRow && <ContextMenuContent className="min-w-52">{menu(selRow)}</ContextMenuContent>}
    </ContextMenu>
  )
}
