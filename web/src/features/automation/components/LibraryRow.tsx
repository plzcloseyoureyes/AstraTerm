/*
 * One row of the Snippets / Macros sidebar libraries: click = primary action (Shift+click = broadcast), drag onto any
 * terminal, right-click or "…" for the menu. Fixed heights (one line, or two with `detail`) keep long lists scannable;
 * the trailing `meta` gives way to the "…" button on hover / focus instead of both fighting for the space.
 */
import { useState, type DragEvent, type MouseEvent, type ReactNode } from 'react'
import { MoreHorizontal } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { ContextMenu, ContextMenuContent, ContextMenuTrigger } from '@/components/ui/context-menu'
import { DropdownMenu, DropdownMenuContent, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { cn } from '@/lib/utils'

export type MenuKind = 'dropdown' | 'context'

interface Props {
  name: string
  /** Leading glyph (what a click does). */
  icon: ReactNode
  /** Next to the name (e.g. a warning mark). */
  badge?: ReactNode
  /** Trailing information on the first line (shortcut, tags, step count). */
  meta?: ReactNode
  /** Second line (e.g. the command preview). */
  detail?: ReactNode
  /** Native tooltip with the full content and the gestures. */
  hint: string
  onActivate: (e: MouseEvent) => void
  onDragStart: (e: DragEvent) => void
  /** Menu items, rendered for the "…" dropdown and for the context menu. */
  menu: (kind: MenuKind) => ReactNode
}

export function LibraryRow({ name, icon, badge, meta, detail, hint, onActivate, onDragStart, menu }: Props) {
  const [dragging, setDragging] = useState(false)
  return (
    <ContextMenu>
      <ContextMenuTrigger asChild>
        <li className="group/row relative">
          <button
            type="button"
            className={cn(
              'grid w-full min-w-0 grid-cols-[1rem_minmax(0,1fr)_auto] items-center gap-x-1.5 rounded-md px-2 text-left outline-none transition-[background-color,opacity] duration-150',
              'hover:bg-sidebar-accent focus-visible:ring-2 focus-visible:ring-ring/60 active:bg-sidebar-accent/70',
              detail ? 'h-11' : 'h-8',
              dragging && 'opacity-50',
            )}
            onClick={onActivate}
            draggable
            onDragStart={(e) => {
              setDragging(true)
              onDragStart(e)
            }}
            onDragEnd={() => setDragging(false)}
            title={hint}
          >
            <span className="flex size-4 items-center justify-center [&>svg]:size-3.5">{icon}</span>
            <span className="flex min-w-0 items-center gap-1.5">
              <span className="truncate font-medium">{name}</span>
              {badge}
            </span>
            <span className="flex min-w-7 items-center justify-end gap-1 text-2xs text-muted-foreground tabular-nums transition-opacity duration-150 group-focus-within/row:opacity-0 group-hover/row:opacity-0">
              {meta}
            </span>
            {detail && <span className="col-start-2 col-end-4 -mt-0.5 flex min-w-0 items-baseline gap-1.5 font-mono text-xs text-muted-foreground">{detail}</span>}
          </button>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button
                variant="ghost"
                size="icon-xs"
                className="absolute top-1 right-1 opacity-0 transition-opacity duration-150 group-focus-within/row:opacity-100 group-hover/row:opacity-100 data-[state=open]:opacity-100"
                aria-label={`Actions for ${name}`}
              >
                <MoreHorizontal />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">{menu('dropdown')}</DropdownMenuContent>
          </DropdownMenu>
        </li>
      </ContextMenuTrigger>
      <ContextMenuContent>{menu('context')}</ContextMenuContent>
    </ContextMenu>
  )
}
