/*
 * Command history (REC-5): the session.command audit entries — every command recognized in terminal sessions (shell
 * integration marks, else echo-confirmed typed lines), with host, directory, exit code and a jump into the recording
 * when the session was recorded.
 */
import { useMemo, useState } from 'react'
import { useInfiniteQuery } from '@tanstack/react-query'
import { Copy, History, Play, RefreshCw, Search, Users } from 'lucide-react'
import { toast } from 'sonner'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { QueryState } from '@/components/ui/query-state'
import { SkeletonRows } from '@/components/ui/skeleton'
import { Tooltip } from '@/components/ui/tooltip'
import { useManualRefresh } from '@/lib/hooks'
import { copyText, formatDateTime, formatDuration } from '@/lib/utils'
import { useIsAdmin } from '@/stores/auth'
import { playRecording } from './actions'
import { listCommandAudit, recKeys, usePolicy, type CommandAuditEntry } from './api'

const PAGE = 200

export default function CommandsPanel() {
  const isAdmin = useIsAdmin()
  const [all, setAll] = useState(false)
  const [q, setQ] = useState('')
  const policy = usePolicy().data
  const query = useInfiniteQuery({
    queryKey: recKeys.audit(all, ''),
    initialPageParam: undefined as number | undefined,
    queryFn: ({ pageParam }) => listCommandAudit(all, pageParam, PAGE),
    getNextPageParam: (last) => (last.length === PAGE ? last[last.length - 1].id : undefined),
    refetchInterval: 15_000,
  })
  const entries = useMemo(() => query.data?.pages.flat() ?? [], [query.data])
  const filtered = useMemo(() => {
    const s = q.trim().toLowerCase()
    if (!s) return entries
    return entries.filter((e) =>
      [e.details?.command, e.details?.host, e.details?.title, e.details?.cwd, e.username].some((v) => v?.toLowerCase().includes(s)),
    )
  }, [entries, q])
  const { refreshing, refresh } = useManualRefresh(query.refetch)

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex shrink-0 items-center gap-2 border-b bg-toolbar px-2 py-1.5">
        <Input inputSize="sm" value={q} onChange={(e) => setQ(e.target.value)} placeholder="Filter commands, hosts, folders…" leading={<Search />} className="w-72" />
        {isAdmin && (
          <Button size="sm" variant={all ? 'secondary' : 'ghost'} aria-pressed={all} onClick={() => setAll((v) => !v)}>
            <Users /> All users
          </Button>
        )}
        <div className="flex-1" />
        {policy && (
          <span className="text-sm text-muted-foreground">
            Command auditing is <span className={policy.commandAudit ? 'text-success' : 'text-foreground'}>{policy.commandAudit ? 'on' : 'off'}</span>
          </span>
        )}
        <IconButton icon={RefreshCw} label="Refresh" onClick={refresh} busy={refreshing} />
      </div>
      {policy && !policy.commandAudit && (
        <div className="shrink-0 border-b bg-muted/40 px-3 py-1.5 text-sm text-muted-foreground">
          New commands are not being recorded.{' '}
          {isAdmin ? 'Turn “Command audit” on under Storage & retention.' : 'An administrator can turn command auditing on.'}
        </div>
      )}
      <div className="relative min-h-0 flex-1 overflow-auto">
        <QueryState
          query={query}
          skeleton={<SkeletonRows rows={12} rowHeight={32} icon={false} />}
          errorTitle="Cannot load the command history"
          isEmpty={() => !filtered.length}
          empty={
            <EmptyState
              icon={History}
              title={q ? 'No matching commands' : 'No commands recorded'}
              description={q ? undefined : 'Commands run in terminal sessions appear here while command auditing is on.'}
            />
          }
        >
          {() => (
            <table className="w-full border-collapse text-base">
              <thead className="sticky top-0 z-10 bg-panel">
                <tr className="border-b text-xs text-muted-foreground">
                  <th className="h-7 px-2 text-left font-medium">Time</th>
                  {all && <th className="h-7 px-2 text-left font-medium">User</th>}
                  <th className="h-7 px-2 text-left font-medium">Where</th>
                  <th className="h-7 px-2 text-left font-medium">Command</th>
                  <th className="h-7 px-2 text-right font-medium">Result</th>
                  <th className="w-16" />
                </tr>
              </thead>
              <tbody>
                {filtered.map((e) => (
                  <CommandRow key={e.id} e={e} showUser={all} />
                ))}
              </tbody>
            </table>
          )}
        </QueryState>
      </div>
      <div className="flex h-7 shrink-0 items-center gap-3 border-t px-3 text-xs text-muted-foreground tabular-nums">
        <span>
          {filtered.length} {filtered.length === 1 ? 'command' : 'commands'}
          {q ? ` of ${entries.length} loaded` : ''}
        </span>
        {query.hasNextPage && (
          <Button size="xs" variant="ghost" loading={query.isFetchingNextPage} onClick={() => void query.fetchNextPage()}>
            Load older
          </Button>
        )}
      </div>
    </div>
  )
}

function CommandRow({ e, showUser }: { e: CommandAuditEntry; showUser: boolean }) {
  const d = e.details ?? {}
  const canPlay = !!d.recordingId
  const where = [d.username && d.host ? `${d.username}@${d.host}` : d.host || d.title, d.cwd].filter(Boolean).join(':')
  return (
    <tr className="group h-8 border-b border-border/70 hover:bg-accent/40">
      <td className="px-2 text-sm whitespace-nowrap text-muted-foreground tabular-nums">{formatDateTime(e.ts)}</td>
      {showUser && <td className="px-2 text-sm">{e.username || '—'}</td>}
      <td className="max-w-56 truncate px-2 text-sm text-muted-foreground" title={where}>
        {where || '—'}
      </td>
      <td className="w-full max-w-0 px-2">
        <span className="flex min-w-0 items-center gap-1.5">
          {d.guest && (
            <Tooltip
              content={`Typed by a share-link guest: ${d.guest.username || 'anonymous'}${d.guest.ip ? ` from ${d.guest.ip}` : ''}${d.guest.label ? ` (link “${d.guest.label}”)` : ''}`}
            >
              <Badge variant="warning" className="shrink-0">
                guest
              </Badge>
            </Tooltip>
          )}
          <code className="block min-w-0 truncate font-mono text-[12px]" title={d.command}>
            {d.command}
          </code>
        </span>
      </td>
      <td className="px-2 text-right whitespace-nowrap">
        {d.running ? (
          <Badge variant="info">running</Badge>
        ) : d.exitCode == null ? (
          <span className="text-xs text-muted-foreground">{d.source === 'input' ? 'typed' : '—'}</span>
        ) : (
          <Tooltip content={d.durationMs != null ? `took ${formatDuration(d.durationMs)}` : 'exit code'}>
            <Badge variant={d.exitCode === 0 ? 'success' : 'destructive'} className="tabular-nums">
              exit {d.exitCode}
            </Badge>
          </Tooltip>
        )}
      </td>
      <td className="px-1 text-right whitespace-nowrap">
        <span className="inline-flex opacity-60 transition-opacity group-hover:opacity-100">
          <IconButton
            icon={Copy}
            label="Copy command"
            size="xs"
            onClick={async () => {
              if (d.command && (await copyText(d.command))) toast.success('Command copied')
            }}
          />
          <IconButton
            icon={Play}
            label={canPlay ? 'Play from here' : 'The session was not recorded'}
            size="xs"
            disabled={!canPlay}
            onClick={() => canPlay && playRecording({ id: d.recordingId!, kind: 'asciicast', title: d.title ?? '' }, { startAt: Math.max(0, (d.recordingTime ?? 0) - 1) })}
          />
        </span>
      </td>
    </tr>
  )
}
