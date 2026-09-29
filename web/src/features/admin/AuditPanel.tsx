/*
 * REC-4 audit log viewer: text search, user / category / time filters, infinite paging, expandable details and CSV
 * export of the filtered log (server-side, same filters).
 */
import { Fragment, useEffect, useMemo, useRef, useState } from 'react'
import { ChevronRight, Download, RefreshCw, ScrollText, Search } from 'lucide-react'
import { apiUrl } from '@/api/client'
import type { AuditEntry } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { QueryState } from '@/components/ui/query-state'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SimpleSelect } from '@/components/ui/select'
import { SkeletonRows } from '@/components/ui/skeleton'
import { Spinner } from '@/components/ui/spinner'
import { Tooltip } from '@/components/ui/tooltip'
import { useManualRefresh } from '@/lib/hooks'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { cn, formatDateTime, formatRelativeTime, plural } from '@/lib/utils'
import { useAdminUsers, useAuditSearch } from '@/features/security/api'
import { PageHeader } from '@/features/security/components'
import type { AuditFilter } from '@/features/security/types'

const CATEGORIES = [
  { value: '', label: 'All actions' },
  { value: 'auth.', label: 'Sign-in & account' },
  { value: 'auth.login.failed', label: 'Failed sign-ins' },
  { value: 'admin.', label: 'Administration' },
  { value: 'session.', label: 'Sessions' },
  { value: 'connection.', label: 'Connections' },
  { value: 'fs.', label: 'Files' },
  { value: 'transfer.', label: 'Transfers' },
  { value: 'tunnel.', label: 'Tunnels' },
  { value: 'key.', label: 'SSH keys' },
  { value: 'known_host.', label: 'Known hosts' },
  { value: 'vault.', label: 'Vault' },
  { value: 'rdp.', label: 'Remote desktop' },
  { value: 'vnc.', label: 'VNC' },
  { value: 'server.', label: 'Embedded servers' },
  { value: 'recording.', label: 'Recordings' },
]

type Range = 'all' | '24h' | '7d' | '30d'
const RANGE_MS: Record<Range, number> = { all: 0, '24h': 86_400_000, '7d': 7 * 86_400_000, '30d': 30 * 86_400_000 }

function tone(action: string): 'destructive' | 'warning' | 'default' | 'secondary' {
  if (/failed|denied|locked|mismatch|clone|revoked/.test(action)) return 'destructive'
  if (/delete|reset|disable|revoke|unlink|kill/.test(action)) return 'warning'
  if (action.startsWith('admin.')) return 'default'
  return 'secondary'
}

function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = useState(value)
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms)
    return () => clearTimeout(t)
  }, [value, ms])
  return v
}

export function AuditPanel({ visible }: { visible: boolean }) {
  const users = useAdminUsers()
  const [text, setText] = useState('')
  const [userId, setUserId] = useState('')
  const [action, setAction] = useState('')
  const [range, setRange] = useState<Range>('7d')
  const q = useDebounced(text, 300)
  // Round "since" to the minute so the query key stays stable while typing.
  const since = useMemo(() => (range === 'all' ? undefined : new Date(Math.floor((Date.now() - RANGE_MS[range]) / 60_000) * 60_000).toISOString()), [range])
  const filter: AuditFilter = { q: q.trim() || undefined, userId: userId || undefined, action: action || undefined, since }
  const audit = useAuditSearch(filter, visible)
  const entries = useMemo(() => audit.data?.pages.flatMap((p) => p.entries) ?? [], [audit.data])
  const [open, setOpen] = useState<number | null>(null)
  const { refreshing, refresh } = useManualRefresh(audit.refetch)
  const loadingMore = useDelayedFlag(audit.isFetchingNextPage)

  // Load the next page when the sentinel scrolls into view.
  const sentinel = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const el = sentinel.current
    if (!el || !audit.hasNextPage) return
    const io = new IntersectionObserver((e) => {
      if (e[0]?.isIntersecting && !audit.isFetchingNextPage) void audit.fetchNextPage()
    })
    io.observe(el)
    return () => io.disconnect()
  }, [audit.hasNextPage, audit.isFetchingNextPage, audit])

  const exportUrl = apiUrl('/api/admin/audit/export', { q: filter.q, userId: filter.userId, action: filter.action, since: filter.since })
  const userOptions = [{ value: 'all', label: 'All users' }, ...(users.data ?? []).map((u) => ({ value: u.id, label: u.displayName || u.username }))]
  // Account events target a user id: show whose account it is.
  const userNames = useMemo(() => new Map((users.data ?? []).map((u) => [u.id, u.username])), [users.data])

  return (
    <>
      <PageHeader
        title="Audit log"
        description="Security-relevant activity: sign-ins, account and admin changes, sessions, file and key operations."
        actions={
          <div className="flex items-center gap-2">
            <IconButton icon={RefreshCw} label="Refresh" busy={refreshing} onClick={refresh} />
            <Button size="sm" variant="secondary" asChild>
              <a href={exportUrl} download>
                <Download /> Export CSV
              </a>
            </Button>
          </div>
        }
      />
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <div className="relative min-w-48 flex-1">
          <Search className="pointer-events-none absolute top-1/2 left-2 size-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input value={text} onChange={(e) => setText(e.target.value)} placeholder="Search action, target, IP…" className="h-7 pl-7" aria-label="Search the audit log" />
        </div>
        <SimpleSelect size="sm" value={userId || 'all'} onValueChange={(v) => setUserId(v === 'all' ? '' : v)} options={userOptions} className="w-40" aria-label="User" />
        <SimpleSelect size="sm" value={action || 'all'} onValueChange={(v) => setAction(v === 'all' ? '' : v)} options={CATEGORIES.map((c) => ({ value: c.value || 'all', label: c.label }))} className="w-44" aria-label="Action" />
        <SegmentedControl<Range>
          size="sm"
          value={range}
          onValueChange={setRange}
          aria-label="Time range"
          options={[
            { value: '24h', label: '24 h' },
            { value: '7d', label: '7 days' },
            { value: '30d', label: '30 days' },
            { value: 'all', label: 'All' },
          ]}
        />
        {/* New filters keep the old entries on screen until the new ones are there (docs/UX.md "Loading states"); the
            spinner is for a search the user changed, background refreshes stay quiet. */}
        <Spinner active={audit.isPlaceholderData && audit.isFetching} reserve label="Searching" />
      </div>
      <QueryState
        query={audit}
        skeleton={<SkeletonRows rows={8} rowHeight={32} icon={false} className="rounded-lg border bg-card" />}
        errorTitle="Could not load the audit log"
        isEmpty={() => entries.length === 0}
        empty={
          <div className="rounded-lg border border-dashed">
            <EmptyState icon={ScrollText} title="No matching entries" description="Widen the time range or clear the filters." size="sm" />
          </div>
        }
      >
        {() => (
          <div className="overflow-x-auto rounded-lg border bg-card">
            <table className="w-full min-w-[30rem] border-collapse text-sm">
              <thead className="text-left text-xs text-muted-foreground">
                <tr className="border-b">
                  <th className="w-6" />
                  <th className="px-2 py-2 font-medium">When</th>
                  <th className="px-2 py-2 font-medium">User</th>
                  <th className="px-2 py-2 font-medium">Action</th>
                  <th className="px-2 py-2 font-medium">Target</th>
                  <th className="hidden px-2 py-2 font-medium @2xl:table-cell">Address</th>
                </tr>
              </thead>
              <tbody>
                {entries.map((e) => (
                  <Row key={e.id} e={e} targetUser={e.target ? userNames.get(e.target) : undefined} open={open === e.id} onToggle={() => setOpen(open === e.id ? null : e.id)} />
                ))}
              </tbody>
            </table>
            <div ref={sentinel} className="flex h-10 items-center justify-center text-xs text-muted-foreground">
              {loadingMore ? (
                <Spinner immediate className="size-3.5" label="Loading more entries" />
              ) : audit.hasNextPage ? (
                <Button size="xs" variant="ghost" onClick={() => !audit.isFetchingNextPage && void audit.fetchNextPage()}>
                  Load more
                </Button>
              ) : (
                <span className="tabular-nums">{plural(entries.length, 'entry', 'entries')}</span>
              )}
            </div>
          </div>
        )}
      </QueryState>
    </>
  )
}

function Row({ e, targetUser, open, onToggle }: { e: AuditEntry; targetUser?: string; open: boolean; onToggle: () => void }) {
  const hasDetails = e.details != null && JSON.stringify(e.details) !== '{}'
  return (
    <Fragment>
      <tr
        className={cn('border-b last:border-b-0 hover:bg-accent/30', hasDetails && 'cursor-pointer', open && 'bg-accent/30')}
        onClick={hasDetails ? onToggle : undefined}
        onKeyDown={(ev) => hasDetails && (ev.key === 'Enter' || ev.key === ' ') && (ev.preventDefault(), onToggle())}
        tabIndex={hasDetails ? 0 : undefined}
        aria-expanded={hasDetails ? open : undefined}
      >
        <td className="pl-2">{hasDetails && <ChevronRight className={cn('size-3.5 text-muted-foreground transition-transform duration-150', open && 'rotate-90')} />}</td>
        <td className="px-2 py-1.5 whitespace-nowrap">
          <Tooltip content={formatDateTime(e.ts)}>
            <time dateTime={e.ts} className="tabular-nums">
              {formatRelativeTime(e.ts)}
            </time>
          </Tooltip>
        </td>
        <td className="max-w-40 truncate px-2">{e.username || <span className="text-muted-foreground">—</span>}</td>
        <td className="px-2">
          <Badge variant={tone(e.action)} className="font-mono">
            {e.action}
          </Badge>
        </td>
        <td className="max-w-48 truncate px-2 font-mono text-xs text-muted-foreground" title={e.target}>
          {targetUser ? <span className="font-sans text-sm text-foreground/80">@{targetUser}</span> : e.target || '—'}
        </td>
        <td className="hidden px-2 font-mono text-xs text-muted-foreground @2xl:table-cell">{e.ip || '—'}</td>
      </tr>
      {open && hasDetails && (
        <tr className="border-b bg-muted/30">
          <td />
          <td colSpan={5} className="px-2 py-2">
            <pre className="max-h-64 overflow-auto rounded bg-background/60 p-2 font-mono text-xs leading-relaxed whitespace-pre-wrap">{JSON.stringify(e.details, null, 2)}</pre>
          </td>
        </tr>
      )}
    </Fragment>
  )
}
