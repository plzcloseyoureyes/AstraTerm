/*
 * "Favorites" and "Recent" sections above the tree (SM-6): collapsible, keyboard navigable (↑↓ Home End, Enter to
 * connect), double-click / middle-click to connect.
 */
import { useRef, useState, type KeyboardEvent, type ReactNode } from 'react'
import { ChevronRight } from 'lucide-react'
import type { Connection, RuntimeSession } from '@/api/types'
import { useNow } from '@/lib/hooks'
import { cn, formatRelativeTime } from '@/lib/utils'
import { connectSafely } from '../connect'
import { openSessionEditor } from '../dialogs/store'
import { ConnectionLabel } from './rows'

export function SessionSection({
  id,
  title,
  icon,
  connections,
  open,
  onOpenChange,
  running,
  rowHeight,
  showLastUsed,
  scroll = true,
}: {
  id: string
  title: string
  icon: ReactNode
  connections: readonly Connection[]
  open: boolean
  onOpenChange: (open: boolean) => void
  running: Map<string, RuntimeSession[]>
  rowHeight: number
  showLastUsed?: boolean
  /** Cap the list's height with its own scroll (quick-access sections); false inside an already scrolling list. */
  scroll?: boolean
}) {
  const [active, setActive] = useState(0)
  const listRef = useRef<HTMLUListElement>(null)
  const now = useNow(60_000)
  if (!connections.length) return null
  const current = Math.min(active, connections.length - 1)

  const focusRow = (i: number) => {
    setActive(i)
    listRef.current?.querySelector<HTMLElement>(`[data-index="${i}"]`)?.focus()
  }

  const onKeyDown = (e: KeyboardEvent<HTMLLIElement>, i: number, c: Connection) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      focusRow(Math.min(connections.length - 1, i + 1))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      focusRow(Math.max(0, i - 1))
    } else if (e.key === 'Home') {
      e.preventDefault()
      focusRow(0)
    } else if (e.key === 'End') {
      e.preventDefault()
      focusRow(connections.length - 1)
    } else if (e.key === 'Enter') {
      e.preventDefault()
      if (e.altKey) openSessionEditor({ mode: 'edit', connectionId: c.id })
      else void connectSafely(c)
    }
  }

  return (
    <section className="shrink-0 border-b" aria-labelledby={`${id}-title`}>
      <button
        type="button"
        id={`${id}-title`}
        aria-expanded={open}
        aria-controls={`${id}-list`}
        onClick={() => onOpenChange(!open)}
        className="flex h-6 w-full items-center gap-1 px-1.5 text-2xs font-semibold tracking-wider text-muted-foreground uppercase outline-none hover:text-foreground focus-visible:bg-accent"
      >
        <ChevronRight className={cn('size-3 transition-transform duration-100', open && 'rotate-90')} />
        {icon}
        <span>{title}</span>
        <span className="ml-auto font-normal tabular-nums">{connections.length}</span>
      </button>
      {open && (
        <ul ref={listRef} id={`${id}-list`} role="listbox" aria-label={title} className={cn('pb-1', scroll && 'max-h-44 overflow-y-auto overscroll-contain')}>
          {connections.map((c, i) => (
            <li
              key={c.id}
              data-index={i}
              data-conn-id={c.id}
              role="option"
              aria-selected={i === current}
              tabIndex={i === current ? 0 : -1}
              style={{ height: rowHeight }}
              className="group/row flex cursor-default items-center gap-1.5 pr-2 pl-5 text-base outline-none select-none hover:bg-sidebar-accent/70 focus-visible:bg-primary/15"
              onFocus={() => setActive(i)}
              onKeyDown={(e) => onKeyDown(e, i, c)}
              onDoubleClick={() => void connectSafely(c)}
              onMouseDown={(e) => {
                if (e.button === 1) e.preventDefault()
              }}
              onAuxClick={(e) => {
                if (e.button === 1) void connectSafely(c)
              }}
            >
              <ConnectionLabel conn={c} running={running.get(c.id)} tags="hover">
                {showLastUsed && c.lastUsedAt && (
                  <span className="hidden shrink-0 text-2xs text-muted-foreground @[20rem]:inline">{formatRelativeTime(c.lastUsedAt, now)}</span>
                )}
              </ConnectionLabel>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}
