/*
 * Tab kind "recordings" (singleton, command recordings.open): the recordings browser (terminal casts, text logs and —
 * when the RDP module provides them — remote desktop recordings), content search across recordings, the command audit
 * (session.command entries) and storage / retention.
 */
import { lazy, useDeferredValue, useEffect, useMemo, useRef, useState } from 'react'
import {
  Clapperboard,
  Download,
  FileText,
  Film,
  Filter,
  History,
  MonitorPlay,
  MoreHorizontal,
  Play,
  RefreshCw,
  Search,
  SquareTerminal,
  Trash2,
  Users,
  X,
} from 'lucide-react'
import type { TabProps } from '@/app/registry'
import { useConnections } from '@/api/connections'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SimpleSelect } from '@/components/ui/select'
import { ErrorState, LoadingState } from '@/components/ui/query-state'
import { SkeletonRows } from '@/components/ui/skeleton'
import { LazyBoundary, Spinner } from '@/components/ui/spinner'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Tooltip } from '@/components/ui/tooltip'
import { useManualRefresh } from '@/lib/hooks'
import { cn, errorMessage, formatBytes, formatDateTime, formatDuration, formatRelativeTime } from '@/lib/utils'
import { useIsAdmin } from '@/stores/auth'
import { updateTabParams } from '@/stores/workspace'
import { deleteRecordingsAction, playRecording } from './actions'
import { downloadUrl, rdpRecordingFileUrl, recordingFileUrl, searchAll, useRdpRecordings, useRecordings } from './api'
import { clock } from './clock'
import { recordingsSettings } from './settings'
import type { RecordingFilters, RecordingItem, SearchAllResponse } from './types'

const CommandsPanel = lazy(() => import('./CommandsPanel'))
const StoragePanel = lazy(() => import('./StoragePanel'))

export interface RecordingsTabParams {
  tab?: 'recordings' | 'commands' | 'storage'
  connectionId?: string
  q?: string
}

export default function RecordingsView({ tabId, params }: TabProps<RecordingsTabParams>) {
  const tab = params?.tab ?? 'recordings'
  return (
    <Tabs value={tab} onValueChange={(v) => updateTabParams(tabId, { tab: v as RecordingsTabParams['tab'] })} className="h-full min-h-0 gap-0 bg-background">
      <TabsList className="shrink-0 px-2">
        <TabsTrigger value="recordings">
          <Clapperboard /> Recordings
        </TabsTrigger>
        <TabsTrigger value="commands">
          <History /> Command history
        </TabsTrigger>
        <TabsTrigger value="storage">
          <Film /> Storage & retention
        </TabsTrigger>
      </TabsList>
      <TabsContent value="recordings" className="min-h-0 flex-1">
        <BrowsePanel initialConnectionId={params?.connectionId} initialQ={params?.q} />
      </TabsContent>
      <TabsContent value="commands" className="relative min-h-0 flex-1">
        <LazyBoundary className="bg-background">
          <CommandsPanel />
        </LazyBoundary>
      </TabsContent>
      <TabsContent value="storage" className="relative min-h-0 flex-1 overflow-y-auto">
        <LazyBoundary className="bg-background">
          <StoragePanel />
        </LazyBoundary>
      </TabsContent>
    </Tabs>
  )
}

type KindFilter = 'all' | 'asciicast' | 'log' | 'guac'

/** Radix Select items cannot use '' as a value. */
const ALL = '__all'

const KIND_OPTIONS = [
  { value: 'asciicast', title: 'Recordings', icon: SquareTerminal },
  { value: 'log', title: 'Logs', icon: FileText },
  { value: 'guac', title: 'Remote desktop', icon: MonitorPlay },
] as const

function KindIcon({ kind, className }: { kind: RecordingItem['kind']; className?: string }) {
  const Icon = kind === 'log' ? FileText : kind === 'guac' ? MonitorPlay : SquareTerminal
  return <Icon className={cn('size-4 text-muted-foreground', className)} />
}

function BrowsePanel({ initialConnectionId, initialQ }: { initialConnectionId?: string; initialQ?: string }) {
  const isAdmin = useIsAdmin()
  const pageSize = recordingsSettings.use().pageSize
  const [q, setQ] = useState(initialQ ?? '')
  const dq = useDeferredValue(q)
  const [kindSel, setKind] = useState<KindFilter>('all')
  const kind = kindSel === 'all' ? '' : kindSel
  const [connectionId, setConnectionId] = useState(initialConnectionId ?? '')
  const [from, setFrom] = useState('')
  const [to, setTo] = useState('')
  const [all, setAll] = useState(false)
  const [sort, setSort] = useState<NonNullable<RecordingFilters['sort']>>('started')
  const [limit, setLimit] = useState(pageSize)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [contentQ, setContentQ] = useState<string | null>(null)
  const [showFilters, setShowFilters] = useState(!!initialConnectionId)

  const filters: RecordingFilters = { q: dq.trim(), kind, connectionId, from, to, all, sort }
  const list = useRecordings(filters, limit)
  const rdpEnabled = (kind === '' || kind === 'guac') && !connectionId && !dq.trim() && !from && !to
  const rdp = useRdpRecordings(all, rdpEnabled)
  const connections = useConnections().data

  const items = useMemo(() => {
    let out: RecordingItem[] = kind === 'guac' ? [] : (list.data?.items ?? [])
    if (rdpEnabled && rdp.data?.length) {
      out = [...out, ...rdp.data]
      if (sort === 'started') out.sort((a, b) => Date.parse(b.startedAt) - Date.parse(a.startedAt))
    }
    return out
  }, [list.data, rdp.data, rdpEnabled, kind, sort])

  const total = (kind === 'guac' ? 0 : (list.data?.total ?? 0)) + (rdpEnabled ? (rdp.data?.length ?? 0) : 0)
  const firstLoad = !list.data && list.isPending
  const { refreshing, refresh } = useManualRefresh(list.refetch)
  const selectedItems = items.filter((i) => selected.has(i.id))
  const allChecked = items.length > 0 && selectedItems.length === items.length

  // Forget selections that are no longer listed.
  useEffect(() => {
    setSelected((old) => {
      const ids = new Set(items.map((i) => i.id))
      const next = new Set([...old].filter((id) => ids.has(id)))
      return next.size === old.size ? old : next
    })
  }, [items])

  const connectionOptions = useMemo(
    () => [{ value: ALL, label: 'All connections' }, ...(connections ?? []).map((c) => ({ value: c.id, label: c.name }))],
    [connections],
  )
  const activeFilters = (connectionId ? 1 : 0) + (from ? 1 : 0) + (to ? 1 : 0)

  return (
    <div className="flex h-full min-h-0 flex-col @container">
      {/* Labels give way to icons (tooltips keep the names) before anything wraps; on a very narrow tab the right-hand
          group (selection, sort, refresh) wraps as one piece. */}
      <div className="flex shrink-0 flex-wrap items-center gap-2 border-b bg-toolbar px-2 py-1.5">
        <Input
          inputSize="sm"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder="Filter by title or connection…"
          leading={<Search />}
          className="w-64 min-w-36 shrink"
          trailing={q ? <IconButton icon={X} label="Clear" size="xs" onClick={() => setQ('')} /> : undefined}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && e.altKey && q.trim()) setContentQ(q.trim())
          }}
        />
        <Tooltip content="Search the output of recordings and logs (Alt+Enter in the filter)">
          <Button size="sm" variant={contentQ ? 'secondary' : 'ghost'} disabled={!q.trim()} onClick={() => setContentQ(q.trim())} aria-label="Search inside">
            <Search /> <span className="hidden @5xl:inline">Search inside</span>
          </Button>
        </Tooltip>
        <SegmentedControl<KindFilter>
          size="sm"
          className="shrink-0"
          aria-label="Kind"
          value={kindSel}
          onValueChange={setKind}
          options={[
            { value: 'all', label: 'All' },
            ...KIND_OPTIONS.map((o) => ({ ...o, label: <span className="hidden @6xl:inline">{o.title}</span> })),
          ]}
        />
        <Button size="sm" variant={showFilters || activeFilters ? 'secondary' : 'ghost'} onClick={() => setShowFilters((v) => !v)} aria-label="Filters" className="shrink-0">
          <Filter /> <span className="hidden @4xl:inline">Filters</span>
          {activeFilters ? <span className="tabular-nums">· {activeFilters}</span> : null}
        </Button>
        {isAdmin && (
          <Button size="sm" variant={all ? 'secondary' : 'ghost'} aria-pressed={all} onClick={() => setAll((v) => !v)} aria-label="All users" className="shrink-0">
            <Users /> <span className="hidden @4xl:inline">All users</span>
          </Button>
        )}
        <div className="ml-auto flex items-center gap-2">
          {selectedItems.length > 0 && (
            <div className="flex items-center gap-1 animate-in fade-in-0">
              <span className="text-sm text-muted-foreground tabular-nums">{selectedItems.length} selected</span>
              <Button size="sm" variant="ghost" className="text-destructive" onClick={() => void deleteRecordingsAction(selectedItems)}>
                <Trash2 /> Delete
              </Button>
            </div>
          )}
          <SimpleSelect
            size="sm"
            value={sort}
            onValueChange={setSort}
            aria-label="Sort"
            className="w-36 shrink-0"
            options={[
              { value: 'started', label: 'Newest first' },
              { value: 'duration', label: 'Longest first' },
              { value: 'size', label: 'Largest first' },
              { value: 'title', label: 'Title' },
            ]}
          />
          <IconButton icon={RefreshCw} label="Refresh" onClick={refresh} busy={refreshing} />
        </div>
      </div>
      {showFilters && (
        <div className="flex shrink-0 flex-wrap items-center gap-2 border-b px-2 py-1.5 animate-in fade-in-0 slide-in-from-top-1 duration-150">
          <SimpleSelect size="sm" value={connectionId || ALL} onValueChange={(v) => setConnectionId(v === ALL ? '' : v)} options={connectionOptions} aria-label="Connection" className="w-56" />
          <label className="flex items-center gap-1.5 text-sm text-muted-foreground">
            From
            <Input inputSize="sm" type="date" value={from} onChange={(e) => setFrom(e.target.value)} className="w-36" />
          </label>
          <label className="flex items-center gap-1.5 text-sm text-muted-foreground">
            to
            <Input inputSize="sm" type="date" value={to} onChange={(e) => setTo(e.target.value)} className="w-36" />
          </label>
          {activeFilters > 0 && (
            <Button
              size="xs"
              variant="ghost"
              onClick={() => {
                setConnectionId('')
                setFrom('')
                setTo('')
              }}
            >
              Clear filters
            </Button>
          )}
        </div>
      )}
      {contentQ ? (
        <ContentSearch q={contentQ} filters={filters} onClose={() => setContentQ(null)} />
      ) : (
        <div className="relative min-h-0 flex-1 overflow-auto">
          <LoadingState busy={firstLoad} skeleton={<SkeletonRows rows={10} rowHeight={36} />}>
            {list.isError && !list.data ? (
              <ErrorState error={list.error} title="Cannot load recordings" onRetry={() => void list.refetch()} size="md" />
            ) : !items.length ? (
              <EmptyState
                icon={Clapperboard}
                title={dq || kind || activeFilters ? 'No matching recordings' : 'No recordings yet'}
                description={
                  dq || kind || activeFilters
                    ? 'Try other filters.'
                    : 'Turn on “Record session” or “Log to file” in a connection, or start recording from a terminal’s context menu. Recordings appear here while they are written.'
                }
              />
            ) : (
              <table className="w-full border-collapse text-base">
                <thead className="sticky top-0 z-10 bg-panel">
                  <tr className="border-b">
                    <th className="w-8 px-2">
                      <Checkbox
                        aria-label="Select all"
                        checked={allChecked ? true : selectedItems.length ? 'indeterminate' : false}
                        onCheckedChange={(v) => setSelected(v ? new Set(items.map((i) => i.id)) : new Set())}
                      />
                    </th>
                    <th className="h-7 w-full px-2 text-left text-xs font-medium text-muted-foreground">Title</th>
                    <th className="hidden h-7 px-2 text-left text-xs font-medium text-muted-foreground @2xl:table-cell">Connection</th>
                    {all && <th className="hidden h-7 px-2 text-left text-xs font-medium text-muted-foreground @3xl:table-cell">Owner</th>}
                    <th className="h-7 px-2 text-left text-xs font-medium text-muted-foreground">Started</th>
                    <th className="h-7 px-2 text-right text-xs font-medium text-muted-foreground">Duration</th>
                    <th className="h-7 px-2 text-right text-xs font-medium text-muted-foreground">Size</th>
                    <th className="w-20" />
                  </tr>
                </thead>
                <tbody>
                  {items.map((it) => (
                    <RecordingRow
                      key={it.id}
                      item={it}
                      showOwner={all}
                      selected={selected.has(it.id)}
                      onSelect={(v) =>
                        setSelected((old) => {
                          const n = new Set(old)
                          if (v) n.add(it.id)
                          else n.delete(it.id)
                          return n
                        })
                      }
                    />
                  ))}
                </tbody>
              </table>
            )}
          </LoadingState>
        </div>
      )}
      {!contentQ && (
        <div className="flex h-7 shrink-0 items-center gap-3 border-t px-3 text-xs text-muted-foreground tabular-nums">
          {/* A changed filter keeps the previous list on screen while the new one loads (not for polling). */}
          <Spinner active={list.isFetching && list.isPlaceholderData} reserve className="size-3.5" label="Updating" />
          <span>
            {items.length} of {total} {total === 1 ? 'item' : 'items'} · {formatBytes(items.reduce((n, i) => n + (i.size || 0), 0))} shown
          </span>
          {kind !== 'guac' && (list.data?.total ?? 0) > limit && (
            <Button size="xs" variant="ghost" onClick={() => setLimit((l) => l + pageSize)}>
              Load more
            </Button>
          )}
        </div>
      )}
    </div>
  )
}

function RecordingRow({ item, showOwner, selected, onSelect }: { item: RecordingItem; showOwner: boolean; selected: boolean; onSelect: (v: boolean) => void }) {
  const rowRef = useRef<HTMLTableRowElement>(null)
  const play = () => playRecording(item)
  const fileUrl = (fmt?: 'v3' | 'v2' | 'txt') => (item.source === 'rdp' ? rdpRecordingFileUrl(item.id) : recordingFileUrl(item.id, fmt, true))
  return (
    <tr
      ref={rowRef}
      tabIndex={0}
      data-state={selected ? 'selected' : undefined}
      className="group h-9 cursor-default border-b border-border/70 outline-none transition-colors hover:bg-accent/40 focus-visible:bg-accent/60 data-[state=selected]:bg-primary/10"
      onDoubleClick={play}
      onKeyDown={(e) => {
        if (e.target !== e.currentTarget) return
        if (e.key === 'Enter') play()
        else if (e.key === ' ') {
          e.preventDefault()
          onSelect(!selected)
        } else if (e.key === 'Delete') void deleteRecordingsAction([item])
        else if (e.key === 'ArrowDown') (rowRef.current?.nextElementSibling as HTMLElement | null)?.focus()
        else if (e.key === 'ArrowUp') (rowRef.current?.previousElementSibling as HTMLElement | null)?.focus()
      }}
    >
      <td className="px-2" onClick={(e) => e.stopPropagation()}>
        <Checkbox aria-label={`Select ${item.title}`} checked={selected} onCheckedChange={(v) => onSelect(!!v)} />
      </td>
      <td className="w-full max-w-0 px-2">
        <div className="flex min-w-0 items-center gap-2">
          <KindIcon kind={item.kind} className="shrink-0" />
          <span className="truncate">{item.title || 'Untitled session'}</span>
          {item.live && (
            <Badge variant="destructive" className="shrink-0">
              <span className="size-1.5 rounded-full bg-current" /> {item.kind === 'log' ? 'Logging' : 'Recording'}
            </Badge>
          )}
          {item.interrupted && (
            <Tooltip content="Termstead stopped before this session ended; the file ends where it stopped.">
              <Badge variant="warning" className="shrink-0">
                Interrupted
              </Badge>
            </Tooltip>
          )}
        </div>
      </td>
      <td className="hidden max-w-48 truncate px-2 text-sm text-muted-foreground @2xl:table-cell">{item.connectionName || (item.connectionId ? '(deleted connection)' : 'Unsaved session')}</td>
      {showOwner && <td className="hidden px-2 text-sm text-muted-foreground @3xl:table-cell">{item.ownerName || item.ownerId}</td>}
      <td className="px-2 text-sm whitespace-nowrap text-muted-foreground">
        <Tooltip content={formatDateTime(item.startedAt)}>
          <span>{formatRelativeTime(item.startedAt)}</span>
        </Tooltip>
      </td>
      <td className="px-2 text-right text-sm whitespace-nowrap tabular-nums">{item.durationMs != null ? formatDuration(item.durationMs) : '—'}</td>
      <td className="px-2 text-right text-sm whitespace-nowrap tabular-nums">{formatBytes(item.size)}</td>
      <td className="px-1 text-right whitespace-nowrap">
        <span className="inline-flex items-center opacity-70 transition-opacity group-hover:opacity-100 group-focus-within:opacity-100">
          <IconButton icon={Play} label={item.kind === 'log' ? 'Open log' : 'Play'} size="xs" onClick={play} />
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="icon-xs" aria-label="More actions" className="text-muted-foreground hover:text-foreground">
                <MoreHorizontal />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem onSelect={play}>
                <Play /> {item.kind === 'log' ? 'Open log' : 'Play'}
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              {item.kind === 'asciicast' ? (
                <>
                  <DropdownMenuItem onSelect={() => downloadUrl(fileUrl('v3'))}>
                    <Download /> Download .cast (v3)
                  </DropdownMenuItem>
                  <DropdownMenuItem onSelect={() => downloadUrl(fileUrl('v2'))}>
                    <Download /> Download .cast (v2)
                  </DropdownMenuItem>
                  <DropdownMenuItem onSelect={() => downloadUrl(fileUrl('txt'))}>
                    <FileText /> Download transcript
                  </DropdownMenuItem>
                </>
              ) : (
                <DropdownMenuItem onSelect={() => downloadUrl(fileUrl())}>
                  <Download /> Download
                </DropdownMenuItem>
              )}
              <DropdownMenuSeparator />
              <DropdownMenuItem className="text-destructive focus:text-destructive" disabled={item.live} onSelect={() => void deleteRecordingsAction([item])}>
                <Trash2 /> Delete
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </span>
      </td>
    </tr>
  )
}

function ContentSearch({ q, filters, onClose }: { q: string; filters: RecordingFilters; onClose: () => void }) {
  const [res, setRes] = useState<SearchAllResponse | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const key = JSON.stringify([q, filters.kind, filters.all, filters.connectionId, filters.from, filters.to])
  useEffect(() => {
    let stop = false
    setBusy(true)
    setError(null)
    searchAll(q, filters)
      .then((r) => !stop && setRes(r))
      .catch((err) => !stop && setError(errorMessage(err)))
      .finally(() => !stop && setBusy(false))
    return () => {
      stop = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key])
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex h-8 shrink-0 items-center gap-2 border-b bg-muted/30 px-3 text-sm">
        <Search className="size-3.5 text-muted-foreground" />
        <span>
          Output containing <span className="font-mono">“{q}”</span>
        </span>
        {res && (
          <span className="text-muted-foreground tabular-nums">
            · {res.results.length} recordings of {res.scanned} searched{res.truncated ? ' (limited)' : ''}
          </span>
        )}
        <Spinner active={busy && !!res} className="size-3.5" label="Searching" />
        <div className="flex-1" />
        <Button size="xs" variant="ghost" onClick={onClose}>
          <X /> Back to the list
        </Button>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto">
        <LoadingState busy={busy && !res && !error} label="Searching" className="h-full">
          {error ? (
            <EmptyState icon={Search} title="Search failed" description={error} />
          ) : res && !res.results.length ? (
            <EmptyState icon={Search} title="No matches" description="Nothing in the recorded output matches. Titles are filtered with the box above." />
          ) : (
            <ul className="divide-y">
              {res?.results.map((r) => (
                <li key={r.recording.id} className="px-3 py-2">
                  <button type="button" className="flex w-full items-center gap-2 text-left" onClick={() => playRecording(r.recording)}>
                    <KindIcon kind={r.recording.kind} />
                    <span className="truncate font-medium">{r.recording.title || 'Untitled session'}</span>
                    <span className="text-sm text-muted-foreground">{formatDateTime(r.recording.startedAt)}</span>
                    <Badge variant="secondary" className="ml-auto tabular-nums">
                      {r.count >= 1000 ? '1000+' : r.count} {r.count === 1 ? 'match' : 'matches'}
                    </Badge>
                  </button>
                  <ul className="mt-1 grid gap-0.5 pl-6">
                    {r.samples.map((m, i) => (
                      <li key={i}>
                        <button
                          type="button"
                          className="flex w-full items-baseline gap-2 rounded-sm px-1 text-left hover:bg-accent focus-visible:bg-accent focus-visible:outline-none"
                          onClick={() =>
                            playRecording(r.recording, m.time != null ? { startAt: Math.max(0, m.time - 1) } : { line: m.line })
                          }
                        >
                          <span className="w-14 shrink-0 text-xs text-muted-foreground tabular-nums">{m.time != null ? clock(m.time) : `line ${m.line}`}</span>
                          <span className="truncate font-mono text-[12px]">{m.text}</span>
                        </button>
                      </li>
                    ))}
                  </ul>
                </li>
              ))}
            </ul>
          )}
        </LoadingState>
      </div>
    </div>
  )
}
