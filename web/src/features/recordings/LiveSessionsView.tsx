/*
 * Tab kind "liveSessions" (admin, command admin.sessions): every user's runtime sessions (MU-19) with owner, target,
 * state, duration, traffic, viewers and share links; Shadow (read-only view), Message and Terminate.
 */
import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Eye, MessageSquare, MonitorDot, MoreHorizontal, Power, RefreshCw, Search, Share2 } from 'lucide-react'
import { protocolIcon, protocolLabel } from '@/app/protocols'
import type { TabProps } from '@/app/registry'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { QueryState } from '@/components/ui/query-state'
import { SkeletonRows } from '@/components/ui/skeleton'
import { statusDotClass } from '@/components/ui/status-dot'
import { Tooltip } from '@/components/ui/tooltip'
import { useManualRefresh, useNow } from '@/lib/hooks'
import { cn, formatBytes, formatDateTime, formatDuration, formatRelativeTime } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth'
import { useIsTabVisible } from '@/stores/workspace'
import { attachOwnSession, shadowSession, terminateAction } from './actions'
import { listAdminSessions, recKeys } from './api'
import { openMessageDialog, openShareDialog } from './store'
import type { AdminSession } from './types'

const STATE_DOT: Record<string, string> = {
  connected: statusDotClass('success'),
  connecting: statusDotClass('warning', true),
  authenticating: statusDotClass('warning', true),
  disconnected: statusDotClass('muted'),
  error: statusDotClass('destructive'),
}

export default function LiveSessionsView({ tabId }: TabProps) {
  const visible = useIsTabVisible(tabId)
  const me = useAuthStore((s) => s.user?.id)
  const q = useQuery({ queryKey: recKeys.adminSessions, queryFn: listAdminSessions, refetchInterval: visible ? 3000 : false })
  const [filter, setFilter] = useState('')
  const now = useNow(1000)
  const rows = useMemo(() => {
    const s = filter.trim().toLowerCase()
    const list = q.data ?? []
    if (!s) return list
    return list.filter((r) => [r.title, r.host, r.username, r.owner.username, r.owner.displayName, r.protocol].some((v) => v?.toLowerCase().includes(s)))
  }, [q.data, filter])
  const users = new Set((q.data ?? []).map((r) => r.ownerId)).size
  const { refreshing, refresh } = useManualRefresh(q.refetch)

  return (
    <div className="flex h-full min-h-0 flex-col bg-background @container">
      <div className="flex shrink-0 items-center gap-2 border-b bg-toolbar px-2 py-1.5">
        <MonitorDot className="mx-1 size-4 text-muted-foreground" />
        <span className="text-sm font-medium">Live sessions</span>
        <span className="text-sm text-muted-foreground tabular-nums">
          {q.data ? `${q.data.length} sessions · ${users} ${users === 1 ? 'user' : 'users'}` : ' '}
        </span>
        <div className="flex-1" />
        <Input inputSize="sm" value={filter} onChange={(e) => setFilter(e.target.value)} placeholder="Filter by user, host, title…" leading={<Search />} className="w-64" />
        <IconButton icon={RefreshCw} label="Refresh" onClick={refresh} busy={refreshing} />
      </div>
      <div className="min-h-0 flex-1 overflow-auto">
        <QueryState
          query={q}
          skeleton={<SkeletonRows rows={8} rowHeight={36} />}
          errorTitle="Cannot load sessions"
          isEmpty={() => !rows.length}
          empty={
            <EmptyState
              icon={MonitorDot}
              title={filter ? 'No matching sessions' : 'No sessions are running'}
              description={filter ? undefined : 'Sessions of every user appear here as soon as they start.'}
            />
          }
        >
          {() => (
            <table className="w-full border-collapse text-base">
              <thead className="sticky top-0 z-10 bg-panel">
                <tr className="border-b text-xs text-muted-foreground">
                  <th className="h-7 px-2 text-left font-medium">User</th>
                  <th className="h-7 px-2 text-left font-medium">Session</th>
                  <th className="hidden h-7 px-2 text-left font-medium @2xl:table-cell">State</th>
                  <th className="h-7 px-2 text-right font-medium">Duration</th>
                  <th className="hidden h-7 px-2 text-right font-medium @3xl:table-cell">Traffic ↓ / ↑</th>
                  <th className="hidden h-7 px-2 text-left font-medium @3xl:table-cell">Last output</th>
                  <th className="h-7 px-2 text-right font-medium">Viewers</th>
                  <th className="w-24" />
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <SessionRow key={r.id} r={r} now={now} mine={r.ownerId === me} />
                ))}
              </tbody>
            </table>
          )}
        </QueryState>
      </div>
    </div>
  )
}

function SessionRow({ r, now, mine }: { r: AdminSession; now: number; mine: boolean }) {
  const Icon = protocolIcon(r.protocol)
  const terminal = r.kind === 'terminal'
  const owner = r.owner.displayName || r.owner.username
  const shadow = () => (mine ? attachOwnSession(r) : terminal && shadowSession({ ...r, owner }))
  return (
    <tr className="group h-9 border-b border-border/70 hover:bg-accent/40" onDoubleClick={shadow}>
      <td className="px-2">
        <div className="flex items-center gap-1.5">
          <span className="truncate">{owner}</span>
          {r.owner.role === 'admin' && <Badge variant="outline">admin</Badge>}
          {mine && <Badge variant="default">you</Badge>}
        </div>
      </td>
      <td className="w-full max-w-0 px-2">
        <div className="flex min-w-0 items-center gap-2">
          <Icon className="size-4 shrink-0 text-muted-foreground" />
          <span className="truncate">{r.title}</span>
          <span className="hidden truncate text-sm text-muted-foreground @xl:inline">
            {protocolLabel(r.protocol)}
            {r.host ? ` · ${r.username ? `${r.username}@` : ''}${r.host}` : ''}
          </span>
          {r.recording && <Badge variant="destructive">REC</Badge>}
        </div>
      </td>
      <td className="hidden px-2 @2xl:table-cell">
        <Tooltip content={r.stateMessage || r.state}>
          <span className="flex items-center gap-1.5 text-sm">
            <span className={cn('size-2 rounded-full', STATE_DOT[r.state] ?? statusDotClass('muted'))} />
            {r.state}
          </span>
        </Tooltip>
      </td>
      <td className="px-2 text-right text-sm whitespace-nowrap tabular-nums">
        <Tooltip content={`Started ${formatDateTime(r.createdAt)}`}>
          <span>{formatDuration(Math.max(0, now - Date.parse(r.createdAt)))}</span>
        </Tooltip>
      </td>
      <td className="hidden px-2 text-right text-sm whitespace-nowrap text-muted-foreground tabular-nums @3xl:table-cell">
        {terminal ? `${formatBytes(r.bytesOut)} / ${formatBytes(r.bytesIn)}` : '—'}
      </td>
      <td className="hidden px-2 text-sm whitespace-nowrap text-muted-foreground @3xl:table-cell">{r.lastOutputAt ? formatRelativeTime(r.lastOutputAt, now) : '—'}</td>
      <td className="px-2 text-right text-sm whitespace-nowrap tabular-nums">
        <Tooltip content={`${r.clients} attached window(s)${r.shares ? ` · ${r.shares} share link(s), ${r.shareViewers} viewer(s)` : ''}`}>
          <span className="inline-flex items-center gap-1">
            {r.clients}
            {r.shares > 0 && <Share2 className="size-3 text-warning" />}
          </span>
        </Tooltip>
      </td>
      <td className="px-1 text-right whitespace-nowrap">
        <span className="inline-flex items-center opacity-70 transition-opacity group-hover:opacity-100 group-focus-within:opacity-100">
          <IconButton icon={Eye} label={terminal ? (mine ? 'Open' : 'Shadow (read-only)') : 'Only terminal sessions can be shadowed'} size="xs" disabled={!terminal} onClick={shadow} />
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="icon-xs" aria-label="More actions" className="text-muted-foreground hover:text-foreground">
                <MoreHorizontal />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem disabled={!terminal} onSelect={shadow}>
                <Eye /> {mine ? 'Open' : 'Shadow (read-only)'}
              </DropdownMenuItem>
              <DropdownMenuItem onSelect={() => openMessageDialog(r.id, r.title)}>
                <MessageSquare /> Send a message…
              </DropdownMenuItem>
              {mine && terminal && (
                <DropdownMenuItem onSelect={() => openShareDialog(r.id, r.title)}>
                  <Share2 /> Share…
                </DropdownMenuItem>
              )}
              <DropdownMenuSeparator />
              <DropdownMenuItem className="text-destructive focus:text-destructive" onSelect={() => void terminateAction({ id: r.id, title: r.title, owner })}>
                <Power /> Terminate…
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </span>
      </td>
    </tr>
  )
}
