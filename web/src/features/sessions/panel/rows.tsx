/*
 * Row visuals shared by the tree, the Favorites/Recent sections and search results.
 */
import type { ReactNode } from 'react'
import { Star, Users } from 'lucide-react'
import type { Connection, RuntimeSession } from '@/api/types'
import { statusDotClass, useSteadyStatus } from '@/components/ui/status-dot'
import { Tooltip } from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'
import { jumpToSession } from '../connect'
import { ConnectionIcon, safeColor } from '../icons'
import { connectionTarget, runningStatus, type RunningStatus } from '../model'
import { setPanelFilters, usePanelState } from './state'

/** Wrap matched character ranges (inclusive, Fuse.js style) in <mark>. */
export function highlight(text: string, ranges?: readonly (readonly [number, number])[]): ReactNode {
  if (!ranges?.length || !text) return text
  const sorted = [...ranges].filter(([a, b]) => b >= a).sort((x, y) => x[0] - y[0])
  const merged: [number, number][] = []
  for (const [a, b] of sorted) {
    const last = merged[merged.length - 1]
    if (last && a <= last[1] + 1) last[1] = Math.max(last[1], b)
    else merged.push([a, b])
  }
  const out: ReactNode[] = []
  let pos = 0
  merged.forEach(([a, b], i) => {
    if (a > pos) out.push(text.slice(pos, a))
    out.push(
      <mark key={i} className="rounded-[2px] bg-primary/25 text-inherit">
        {text.slice(a, b + 1)}
      </mark>,
    )
    pos = b + 1
  })
  if (pos < text.length) out.push(text.slice(pos))
  return out
}

const STATUS_STYLE: Record<RunningStatus, { dot: string; label: string }> = {
  connected: { dot: 'bg-success', label: 'connected' },
  connecting: { dot: statusDotClass('warning', true), label: 'connecting' },
  error: { dot: 'bg-destructive', label: 'error' },
  disconnected: { dot: 'bg-muted-foreground/60', label: 'disconnected' },
}

/** Live-session dot (SM-1 "open-session green dot"): click jumps to the session's tab. */
function RunningIndicator({ sessions, className }: { sessions: readonly RuntimeSession[]; className?: string }) {
  const live = sessions.length ? runningStatus(sessions) : 'disconnected'
  // A session that connects quickly goes straight to green; a short reconnect keeps the previous colour.
  const status = useSteadyStatus(live, (x) => x === 'connecting')
  if (!sessions.length || !status) return null
  const style = STATUS_STYLE[status]
  const target = sessions.find((s) => s.state === 'connected') ?? sessions[0]
  const label = `${sessions.length} open session${sessions.length === 1 ? '' : 's'} (${style.label}) — click to show`
  return (
    <Tooltip content={label} side="right">
      <button
        type="button"
        tabIndex={-1}
        aria-label={label}
        className={cn('flex h-4 min-w-4 shrink-0 items-center justify-center gap-0.5 rounded-full px-0.5 hover:bg-accent', className)}
        onMouseDown={(e) => e.stopPropagation()}
        onDoubleClick={(e) => e.stopPropagation()}
        onClick={(e) => {
          e.stopPropagation()
          jumpToSession(target)
        }}
      >
        <span className={cn('size-2 rounded-full', style.dot)} />
        {sessions.length > 1 && <span className="text-2xs leading-none text-muted-foreground tabular-nums">{sessions.length}</span>}
      </button>
    </Tooltip>
  )
}

/** Icon + name + target + badges of a connection row. */
export function ConnectionLabel({
  conn,
  running,
  name,
  target,
  showColorDot = true,
  tags,
  children,
}: {
  conn: Connection
  running?: readonly RuntimeSession[]
  /** Custom name node (highlighted search match). */
  name?: ReactNode
  target?: ReactNode
  showColorDot?: boolean
  /** Show the session's tags as small chips: on the row's hover / keyboard focus, or always (a selected row). Needs a
   *  `group/row` ancestor for 'hover'. */
  tags?: 'hover' | 'always'
  /** Extra trailing content (e.g. folder path in search results). */
  children?: ReactNode
}) {
  const color = safeColor(conn.color)
  // A session named after its address ("root@10.0.0.5") would show it twice.
  const auto = connectionTarget(conn)
  const tgt = target ?? (auto === conn.name ? '' : auto)
  return (
    <>
      <span className="relative flex shrink-0">
        <ConnectionIcon protocol={conn.protocol} icon={conn.icon} className="text-muted-foreground" />
        {/* Session colour: a small badge on the icon, distinct from the round status dot on the right. */}
        {showColorDot && color && (
          <span aria-hidden className="absolute -right-0.5 -bottom-0.5 size-2 rounded-full border border-sidebar" style={{ background: color }} />
        )}
      </span>
      {/* The name keeps its natural width (ellipsised only when longer than the row); the target gets what is left. */}
      <span className="flex min-w-0 flex-1 items-baseline gap-1.5">
        <span className="max-w-full shrink-0 truncate">{name ?? conn.name}</span>
        {tgt ? <span className="min-w-0 flex-1 truncate font-mono text-xs text-muted-foreground">{tgt}</span> : null}
      </span>
      {children}
      {tags && conn.tags && conn.tags.length > 0 && <TagChips tags={conn.tags} always={tags === 'always'} />}
      <span className="ml-auto flex shrink-0 items-center gap-1 pl-1">
        {conn.favorite && <Star aria-label="Favorite" className="size-3 fill-warning text-warning" />}
        {conn.shared && <Users aria-label="Shared" className="size-3 text-muted-foreground" />}
        {running && running.length > 0 && <RunningIndicator sessions={running} />}
      </span>
    </>
  )
}

const MAX_CHIPS = 2

/** A session's tags as quiet chips; clicking one filters the tree by that tag (again: removes the filter). */
function TagChips({ tags, always }: { tags: readonly string[]; always: boolean }) {
  const active = usePanelState((st) => st.filters.tags)
  const shown = tags.slice(0, MAX_CHIPS)
  const more = tags.length - shown.length
  return (
    <span className={cn('shrink-0 items-center gap-0.5', always ? 'flex' : 'hidden group-hover/row:flex group-focus-visible/row:flex')}>
      {shown.map((t) => {
        const on = active.some((x) => x.toLowerCase() === t.toLowerCase())
        return (
          <button
            key={t}
            type="button"
            tabIndex={-1}
            title={on ? `Showing sessions tagged “${t}” — click to show all` : `Show sessions tagged “${t}”`}
            className={cn(
              'max-w-20 truncate rounded-sm px-1 text-2xs leading-4 outline-none transition-colors',
              on ? 'bg-primary/20 text-primary' : 'bg-muted text-muted-foreground hover:bg-accent hover:text-foreground',
            )}
            onMouseDown={(e) => e.stopPropagation()}
            onDoubleClick={(e) => e.stopPropagation()}
            onClick={(e) => {
              e.stopPropagation()
              setPanelFilters({ tags: on ? active.filter((x) => x.toLowerCase() !== t.toLowerCase()) : [t] })
            }}
          >
            {t}
          </button>
        )
      })}
      {more > 0 && (
        <span className="text-2xs text-muted-foreground tabular-nums" title={tags.slice(MAX_CHIPS).join(', ')}>
          +{more}
        </span>
      )}
    </span>
  )
}
