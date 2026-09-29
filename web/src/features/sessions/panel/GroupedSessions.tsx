/*
 * The "All sessions" list grouped by protocol or tag (Group by in the panel's sort menu): collapsible groups of the
 * (filtered) sessions, sorted by the panel's sort mode. Rows behave like the quick-access sections: ↑↓ / Enter,
 * double-click or middle-click to connect, right-click for the session menu.
 */
import { useMemo } from 'react'
import { Tag, Tags } from 'lucide-react'
import { protocolIcon } from '@/app/protocols'
import type { Connection, RuntimeSession } from '@/api/types'
import { connectionComparator, groupConnections } from '../model'
import { sessionsSettings } from '../settings'
import type { GroupBy, SortMode } from '../types'
import { SessionSection } from './Sections'

export function GroupedSessions({
  connections,
  by,
  sortMode,
  running,
  rowHeight,
}: {
  connections: readonly Connection[]
  by: Exclude<GroupBy, 'folder'>
  sortMode: SortMode
  running: Map<string, RuntimeSession[]>
  rowHeight: number
}) {
  // Manual order is a folder-tree notion: groups fall back to names.
  const compare = useMemo(() => connectionComparator(sortMode === 'manual' ? 'name' : sortMode), [sortMode])
  const groups = useMemo(() => groupConnections(connections, by, compare), [connections, by, compare])
  const collapsed = sessionsSettings.useValue('collapsedGroups')
  const closed = useMemo(() => new Set(collapsed), [collapsed])

  const setOpen = (key: string, open: boolean) =>
    sessionsSettings.set((s) => {
      const rest = (s.collapsedGroups ?? []).filter((k) => k !== key)
      return { collapsedGroups: open ? rest : [...rest, key].slice(-200) }
    })

  return (
    <div className="h-full overflow-y-auto overscroll-contain" role="group" aria-label={by === 'protocol' ? 'Sessions by protocol' : 'Sessions by tag'}>
      {groups.map((g) => {
        const Icon = g.protocol ? protocolIcon(g.protocol) : g.key === 'tag:' ? Tags : Tag
        return (
          <SessionSection
            key={g.key}
            id={`nx-sessions-group-${g.key.replace(/[^a-z0-9-]/gi, '_')}`}
            title={g.label}
            icon={<Icon className="size-3" />}
            connections={g.connections}
            open={!closed.has(g.key)}
            onOpenChange={(o) => setOpen(g.key, o)}
            running={running}
            rowHeight={rowHeight}
            scroll={false}
          />
        )
      })}
    </div>
  )
}
