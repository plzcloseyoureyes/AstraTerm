/*
 * Ranked fuzzy-search results (SM-5): highlighted name / user@host, folder path, keyboard driven from the search box.
 */
import { useEffect, useRef } from 'react'
import type { FuseResultMatch } from 'fuse.js'
import { Zap } from 'lucide-react'
import type { Connection, RuntimeSession } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { cn } from '@/lib/utils'
import { connectSafely } from '../connect'
import { connectionTarget } from '../model'
import { ConnectionLabel, highlight } from './rows'

export interface SearchHit {
  conn: Connection
  matches: readonly FuseResultMatch[]
  folderPath: string
}

function rangesFor(matches: readonly FuseResultMatch[], key: string): readonly (readonly [number, number])[] {
  return matches.filter((m) => m.key === key).flatMap((m) => m.indices)
}

/** Shift host / username match ranges into the rendered "user@host:port" text. */
function targetRanges(conn: Connection, target: string, matches: readonly FuseResultMatch[]): [number, number][] {
  const out: [number, number][] = []
  const hostAt = conn.host ? target.indexOf(conn.host) : -1
  if (hostAt >= 0) for (const [a, b] of rangesFor(matches, 'host')) out.push([a + hostAt, b + hostAt])
  if (conn.username && target.startsWith(`${conn.username}@`)) for (const [a, b] of rangesFor(matches, 'username')) out.push([a, b])
  return out
}

export function SearchResults({
  id,
  hits,
  query,
  active,
  onActiveChange,
  running,
  rowHeight,
  onQuickConnect,
}: {
  id: string
  hits: readonly SearchHit[]
  query: string
  active: number
  onActiveChange: (i: number) => void
  running: Map<string, RuntimeSession[]>
  rowHeight: number
  onQuickConnect: () => void
}) {
  const listRef = useRef<HTMLUListElement>(null)

  useEffect(() => {
    if (active < 0) return
    listRef.current?.querySelector<HTMLElement>(`[data-index="${active}"]`)?.scrollIntoView({ block: 'nearest' })
  }, [active])

  if (!hits.length) {
    return (
      <EmptyState
        size="sm"
        title="No saved session matches"
        description={
          <>
            Press <kbd className="font-sans font-medium">Enter</kbd> to quick connect to <span className="font-mono">{query}</span>.
          </>
        }
        action={
          <Button size="sm" variant="secondary" onClick={onQuickConnect}>
            <Zap /> Quick connect
          </Button>
        }
      />
    )
  }

  return (
    <ul ref={listRef} id={id} role="listbox" aria-label="Matching sessions" className="h-full overflow-y-auto overscroll-contain py-1">
      {hits.map((h, i) => {
        const target = connectionTarget(h.conn)
        const tagHits = h.matches.filter((m) => m.key === 'tags' && typeof m.value === 'string').map((m) => m.value as string)
        const inNotes = h.matches.some((m) => m.key === 'notes')
        return (
          <li
            key={h.conn.id}
            id={`${id}-${i}`}
            data-index={i}
            data-conn-id={h.conn.id}
            role="option"
            aria-selected={i === active}
            style={{ minHeight: rowHeight }}
            className={cn(
              'flex cursor-default flex-col justify-center px-2 py-0.5 select-none hover:bg-sidebar-accent/70',
              i === active && 'bg-primary/15 hover:bg-primary/20',
            )}
            onMouseDown={(e) => e.preventDefault() /* keep focus in the search box */}
            onClick={() => onActiveChange(i)}
            onDoubleClick={() => void connectSafely(h.conn)}
            onAuxClick={(e) => {
              if (e.button === 1) void connectSafely(h.conn)
            }}
          >
            <div className="flex min-w-0 items-center gap-1.5 text-base">
              <ConnectionLabel
                conn={h.conn}
                running={running.get(h.conn.id)}
                name={highlight(h.conn.name, rangesFor(h.matches, 'name'))}
                target={target ? highlight(target, targetRanges(h.conn, target, h.matches)) : null}
              />
            </div>
            {(h.folderPath || tagHits.length > 0 || inNotes) && (
              <div className="flex min-w-0 items-center gap-1 pl-5.5 text-xs text-muted-foreground">
                {h.folderPath && <span className="truncate">{h.folderPath}</span>}
                {tagHits.slice(0, 3).map((t) => (
                  <Badge key={t} variant="outline" className="h-4 px-1 text-2xs">
                    {t}
                  </Badge>
                ))}
                {inNotes && <span className="shrink-0 italic">in notes</span>}
              </div>
            )}
          </li>
        )
      })}
    </ul>
  )
}
