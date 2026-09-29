/*
 * UI-19 active sessions manager: every runtime session of the signed-in user (running, detached, ended) with live
 * state from the events socket, filters, and single / bulk actions (open, reconnect, close, close all detached).
 * Double-click (or Enter) opens a session; Delete closes the selection.
 */
import { useMemo, useState } from 'react'
import { CircleDot, MonitorPlay, PlugZap, RefreshCw, Search, SquareArrowOutUpRight, Unplug, X } from 'lucide-react'
import { toast } from 'sonner'
import { useQueryClient } from '@tanstack/react-query'
import { queryKeys } from '@/api/queryKeys'
import { closeSession, reconnectSession, useSessions } from '@/api/sessions'
import type { RuntimeSession, SessionState } from '@/api/types'
import { protocolIcon, protocolLabel } from '@/app/protocols'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { confirm } from '@/components/ui/dialog-host'
import { EmptyState } from '@/components/ui/empty-state'
import { Input } from '@/components/ui/input'
import { QueryState } from '@/components/ui/query-state'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SkeletonRows } from '@/components/ui/skeleton'
import { StatusDot, type StatusTone } from '@/components/ui/status-dot'
import { Tooltip } from '@/components/ui/tooltip'
import { attachSession, findSessionTabs, openLocalShell } from '@/features/terminal/open'
import { useNow } from '@/lib/hooks'
import { cn, errorMessage, formatDateTime, formatDuration, plural } from '@/lib/utils'
import { useTabs } from '@/stores/workspace'

type Filter = 'all' | 'running' | 'detached' | 'ended'

const ENDED: SessionState[] = ['closed', 'error', 'disconnected']
const PENDING: SessionState[] = ['connecting', 'authenticating']

function stateLabel(s: RuntimeSession): string {
  switch (s.state) {
    case 'connecting':
      return 'Connecting'
    case 'authenticating':
      return 'Signing in'
    case 'connected':
      return 'Connected'
    case 'disconnected':
      return 'Disconnected'
    case 'error':
      return 'Error'
    case 'closed':
      return 'Closed'
  }
}

const STATE_TONE: Record<SessionState, StatusTone> = {
  connecting: 'warning',
  authenticating: 'warning',
  connected: 'success',
  error: 'destructive',
  disconnected: 'muted',
  closed: 'muted',
}

/** Steady dot; connecting / signing in breathes slowly (docs/UX.md "Status dots"). */
function StateDot({ state }: { state: SessionState }) {
  return <StatusDot tone={STATE_TONE[state]} pending={PENDING.includes(state)} className="size-2" />
}

export default function ActiveSessionsView() {
  const sessions = useSessions()
  const qc = useQueryClient()
  const tabs = useTabs()
  const now = useNow(1000)
  const [filter, setFilter] = useState<Filter>('all')
  const [q, setQ] = useState('')
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [busy, setBusy] = useState(false)

  const shown = useMemo(() => {
    const open = new Set<string>()
    for (const t of tabs) {
      const sid = (t.params as { sessionId?: unknown } | undefined)?.sessionId
      if (typeof sid === 'string') open.add(sid)
    }
    return open
  }, [tabs])

  const rows = useMemo(() => {
    const list = sessions.data ?? []
    const needle = q.trim().toLowerCase()
    return list
      .filter((s) => {
        const detached = !ENDED.includes(s.state) && s.clients === 0 && !shown.has(s.id)
        if (filter === 'running' && ENDED.includes(s.state)) return false
        if (filter === 'detached' && !detached) return false
        if (filter === 'ended' && !ENDED.includes(s.state)) return false
        if (!needle) return true
        return [s.title, s.host, s.username, s.protocol, s.cwd].some((v) => v?.toLowerCase().includes(needle))
      })
      .sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt))
  }, [sessions.data, q, filter, shown])

  const counts = useMemo(() => {
    const list = sessions.data ?? []
    return {
      running: list.filter((s) => !ENDED.includes(s.state)).length,
      detached: list.filter((s) => !ENDED.includes(s.state) && s.clients === 0 && !shown.has(s.id)).length,
      ended: list.filter((s) => ENDED.includes(s.state)).length,
    }
  }, [sessions.data, shown])

  const sel = rows.filter((r) => selected.has(r.id))
  const toggle = (id: string, on: boolean) =>
    setSelected((prev) => {
      const next = new Set(prev)
      if (on) next.add(id)
      else next.delete(id)
      return next
    })

  const refresh = () => void qc.invalidateQueries({ queryKey: queryKeys.sessions })

  const closeMany = async (list: RuntimeSession[], what: string) => {
    if (!list.length) return
    const live = list.filter((s) => !ENDED.includes(s.state)).length
    if (live > 0) {
      const ok = await confirm({
        title: `Close ${what}?`,
        description: `${plural(live, 'running session')} will be disconnected. Tabs showing them close too.`,
        confirmLabel: 'Close',
        destructive: true,
      })
      if (!ok) return
    }
    setBusy(true)
    const results = await Promise.allSettled(list.map((s) => closeSession(s.id)))
    setBusy(false)
    const failed = results.filter((r) => r.status === 'rejected').length
    setSelected(new Set())
    refresh()
    if (failed) toast.error(`${failed} of ${list.length} sessions could not be closed`)
    else toast.success(`Closed ${plural(list.length, 'session')}`)
  }

  const reconnectMany = async (list: RuntimeSession[]) => {
    setBusy(true)
    const results = await Promise.allSettled(list.map((s) => reconnectSession(s.id)))
    setBusy(false)
    const failed = results.filter((r) => r.status === 'rejected')
    refresh()
    if (failed.length) toast.error(`${failed.length} of ${list.length} could not reconnect`, { description: errorMessage((failed[0] as PromiseRejectedResult).reason) })
    else toast.success(list.length === 1 ? 'Reconnecting…' : `Reconnecting ${list.length} sessions…`)
  }

  const detachedList = (sessions.data ?? []).filter((s) => !ENDED.includes(s.state) && s.clients === 0 && !shown.has(s.id))

  return (
    <div className="@container flex h-full min-h-0 flex-col bg-background">
      <div className="flex flex-wrap items-center gap-2 border-b px-3 py-2">
        <div className="relative w-56 max-w-full">
          <Search className="pointer-events-none absolute top-1/2 left-2 size-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Filter sessions" className="h-7 pl-7" aria-label="Filter sessions" />
        </div>
        <SegmentedControl<Filter>
          size="sm"
          value={filter}
          onValueChange={setFilter}
          aria-label="Show"
          options={[
            { value: 'all', label: 'All' },
            { value: 'running', label: <span className="tabular-nums">Running {counts.running}</span> },
            { value: 'detached', label: <span className="tabular-nums">Detached {counts.detached}</span> },
            { value: 'ended', label: <span className="tabular-nums">Ended {counts.ended}</span> },
          ]}
        />
        <div className="flex-1" />
        {sel.length > 0 ? (
          <>
            <span className="text-sm text-muted-foreground tabular-nums">{sel.length} selected</span>
            <Button size="sm" variant="secondary" disabled={busy} onClick={() => void reconnectMany(sel)}>
              <RefreshCw /> Reconnect
            </Button>
            <Button size="sm" variant="secondary" className="text-destructive" disabled={busy} onClick={() => void closeMany(sel, plural(sel.length, 'session'))}>
              <X /> Close
            </Button>
          </>
        ) : (
          <Button size="sm" variant="secondary" disabled={!detachedList.length || busy} onClick={() => void closeMany(detachedList, `${plural(detachedList.length, 'detached session')}`)}>
            <Unplug /> Close all detached
          </Button>
        )}
      </div>
      <div className="min-h-0 flex-1 overflow-auto">
        <QueryState
          query={sessions}
          skeleton={<SkeletonRows rows={6} rowHeight={36} />}
          errorTitle="Could not load your sessions"
          isEmpty={(d) => d.length === 0}
          empty={
            <EmptyState
              icon={MonitorPlay}
              title="No sessions"
              description="Sessions you open keep running on the server, even when you close the browser. They will show up here."
              action={
                <Button variant="secondary" onClick={() => void openLocalShell()}>
                  <PlugZap /> Open a local terminal
                </Button>
              }
            />
          }
        >
          {() =>
            rows.length ? (
              <table className="w-full border-collapse text-base">
                <thead className="sticky top-0 z-10 bg-background/95 text-left text-xs font-medium text-muted-foreground backdrop-blur">
                  <tr className="border-b">
                    <th className="w-8 px-3 py-1.5">
                      <Checkbox
                        aria-label="Select all"
                        checked={sel.length === rows.length ? true : sel.length ? 'indeterminate' : false}
                        onCheckedChange={(v) => setSelected(v === true ? new Set(rows.map((r) => r.id)) : new Set())}
                      />
                    </th>
                    <th className="px-2 py-1.5 font-medium">Session</th>
                    <th className="hidden px-2 py-1.5 font-medium @2xl:table-cell">Host</th>
                    <th className="px-2 py-1.5 font-medium">State</th>
                    <th className="hidden px-2 py-1.5 text-right font-medium @xl:table-cell">Duration</th>
                    <th className="hidden px-2 py-1.5 text-right font-medium @3xl:table-cell">Viewers</th>
                    <th className="w-px px-3 py-1.5" />
                  </tr>
                </thead>
                <tbody>
                  {rows.map((s) => {
                    const Icon = protocolIcon(s.protocol)
                    const ended = ENDED.includes(s.state)
                    const inTab = findSessionTabs(s.id).length > 0
                    const since = s.connectedAt ?? s.createdAt
                    return (
                      <tr
                        key={s.id}
                        tabIndex={0}
                        aria-selected={selected.has(s.id)}
                        onDoubleClick={() => void attachSession(s.id)}
                        onKeyDown={(e) => {
                          if (e.target !== e.currentTarget) return
                          if (e.key === 'Enter') void attachSession(s.id)
                          if (e.key === ' ') {
                            e.preventDefault()
                            toggle(s.id, !selected.has(s.id))
                          }
                          if (e.key === 'Delete') void closeMany(selected.has(s.id) ? sel : [s], selected.has(s.id) ? plural(sel.length, 'session') : `“${s.title}”`)
                        }}
                        className={cn(
                          'group h-10 border-b outline-none transition-colors duration-100 hover:bg-accent/40 focus-visible:bg-accent/60',
                          selected.has(s.id) && 'bg-accent/50',
                          ended && 'text-muted-foreground',
                        )}
                      >
                        <td className="px-3">
                          <Checkbox aria-label={`Select ${s.title}`} checked={selected.has(s.id)} onCheckedChange={(v) => toggle(s.id, v === true)} />
                        </td>
                        <td className="max-w-0 px-2">
                          <div className="flex min-w-0 items-center gap-2">
                            <Icon className="size-4 shrink-0 text-muted-foreground" />
                            <span className="truncate font-medium">{s.title}</span>
                            <Badge variant="outline" className="hidden @lg:inline-flex">
                              {protocolLabel(s.protocol)}
                            </Badge>
                            {s.recording && (
                              <Tooltip content="Recording">
                                <CircleDot className="size-3.5 shrink-0 text-destructive" />
                              </Tooltip>
                            )}
                          </div>
                        </td>
                        <td className="hidden max-w-0 truncate px-2 text-sm text-muted-foreground @2xl:table-cell">
                          {s.host ? `${s.username ? `${s.username}@` : ''}${s.host}` : '—'}
                        </td>
                        <td className="px-2">
                          <Tooltip content={s.stateMessage || undefined} disabled={!s.stateMessage}>
                            <span className="inline-flex items-center gap-1.5 text-sm whitespace-nowrap">
                              <StateDot state={s.state} />
                              {stateLabel(s)}
                              {!ended && s.clients === 0 && !inTab && <Badge variant="secondary">Detached</Badge>}
                            </span>
                          </Tooltip>
                        </td>
                        <td className="hidden px-2 text-right text-sm tabular-nums @xl:table-cell">
                          <Tooltip content={`Started ${formatDateTime(s.createdAt)}`}>
                            <span>{ended ? '—' : formatDuration(now - Date.parse(since))}</span>
                          </Tooltip>
                        </td>
                        <td className="hidden px-2 text-right text-sm tabular-nums @3xl:table-cell">{s.clients}</td>
                        <td className="px-3">
                          <div className="flex justify-end gap-0.5 opacity-70 transition-opacity duration-150 group-hover:opacity-100 group-focus-within:opacity-100">
                            <Tooltip content={inTab ? 'Show tab' : 'Open in a tab'}>
                              <Button size="icon-xs" variant="ghost" aria-label="Open" onClick={() => void attachSession(s.id)}>
                                <SquareArrowOutUpRight />
                              </Button>
                            </Tooltip>
                            {(ended || s.state === 'error') && s.kind === 'terminal' && (
                              <Tooltip content="Reconnect">
                                <Button size="icon-xs" variant="ghost" aria-label="Reconnect" onClick={() => void reconnectMany([s])}>
                                  <RefreshCw />
                                </Button>
                              </Tooltip>
                            )}
                            <Tooltip content={ended ? 'Remove' : 'Close session'}>
                              <Button size="icon-xs" variant="ghost" aria-label="Close" className="hover:text-destructive" onClick={() => void closeMany([s], `“${s.title}”`)}>
                                <X />
                              </Button>
                            </Tooltip>
                          </div>
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            ) : (
              <EmptyState size="sm" icon={Search} title="Nothing matches" description="Try another filter." />
            )
          }
        </QueryState>
      </div>
      <div className="flex items-center gap-3 border-t px-3 py-1 text-xs text-muted-foreground">
        <span className="tabular-nums">{plural(sessions.data?.length ?? 0, 'session')}</span>
        <span>Double-click or Enter opens · Space selects · Delete closes</span>
      </div>
    </div>
  )
}
