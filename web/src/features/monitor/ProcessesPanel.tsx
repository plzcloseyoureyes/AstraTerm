/*
 * Process manager (MON-3 / MON-6 "MobaTaskList"): sortable, filterable, optionally as a tree, auto-refreshing while
 * visible; end / kill / signal / renice with confirmation and optional sudo.
 */
import { useEffect, useMemo, useRef, useState } from 'react'
import {
  Copy,
  Filter,
  ListTree,
  OctagonX,
  Pause,
  Play,
  RefreshCw,
  ShieldCheck,
  SlidersHorizontal,
  Skull,
  Square,
  Zap,
} from 'lucide-react'
import { Button } from '@/components/ui/button'
import { ContextMenuItem, ContextMenuLabel, ContextMenuSeparator, ContextMenuSub, ContextMenuSubContent, ContextMenuSubTrigger } from '@/components/ui/context-menu'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { LoadingPane, Spinner } from '@/components/ui/spinner'
import { Toolbar, ToolbarSeparator, ToolbarSpacer } from '@/components/ui/toolbar'
import { useLoadingGate } from '@/lib/useDelayedFlag'
import { copyText, errorMessage, formatCalendarTime } from '@/lib/utils'
import { useManualRefresh, useNow } from '@/lib/hooks'
import { useProcesses } from './api'
import { reniceWithPrompt, SIGNALS, signalProcess } from './actions'
import { cpuTime, compactBytes, level, levelText, uptime } from './format'
import { monitorSettings } from './settings'
import type { ProcessInfo, TargetId } from './types'
import { VirtualTable, type Column, type SortState } from './VirtualTable'

const STATES: Record<string, string> = {
  R: 'Running',
  S: 'Sleeping',
  D: 'Uninterruptible sleep (I/O)',
  Z: 'Zombie',
  T: 'Stopped',
  t: 'Stopped by debugger',
  I: 'Idle kernel thread',
  X: 'Dead',
  U: 'Uninterruptible wait',
}

type Row = ProcessInfo & { depth: number; key: string }

function isKernelThread(p: ProcessInfo): boolean {
  return p.pid === 2 || p.ppid === 2 || (p.command.startsWith('[') && p.command.endsWith(']'))
}

function sortValue(p: ProcessInfo, id: string): number | string {
  switch (id) {
    case 'pid':
      return p.pid
    case 'user':
      return p.user.toLowerCase()
    case 'cpu':
      return p.cpu
    case 'mem':
      return p.mem
    case 'rss':
      return p.rss
    case 'threads':
      return p.threads ?? 0
    case 'nice':
      return p.nice
    case 'state':
      return p.state
    case 'started':
      return p.started ? -Date.parse(p.started) : 0
    case 'time':
      return p.cpuTime
    default:
      return p.command.toLowerCase()
  }
}

function compare(a: ProcessInfo, b: ProcessInfo, s: SortState): number {
  const x = sortValue(a, s.id)
  const y = sortValue(b, s.id)
  const r = typeof x === 'number' && typeof y === 'number' ? x - y : String(x).localeCompare(String(y))
  return (s.desc ? -r : r) || a.pid - b.pid
}

/** Depth-first ordering by parent, siblings sorted; orphans of the filtered set become roots. */
function asTree(list: ProcessInfo[], s: SortState): Row[] {
  const byPid = new Map(list.map((p) => [p.pid, p]))
  const children = new Map<number, ProcessInfo[]>()
  const roots: ProcessInfo[] = []
  for (const p of list) {
    if (p.ppid !== p.pid && byPid.has(p.ppid)) {
      const arr = children.get(p.ppid) ?? []
      arr.push(p)
      children.set(p.ppid, arr)
    } else roots.push(p)
  }
  const out: Row[] = []
  const seen = new Set<number>()
  const walk = (p: ProcessInfo, depth: number) => {
    if (seen.has(p.pid)) return
    seen.add(p.pid)
    out.push({ ...p, depth, key: String(p.pid) })
    for (const c of (children.get(p.pid) ?? []).sort((a, b) => compare(a, b, s))) walk(c, depth + 1)
  }
  for (const r of roots.sort((a, b) => compare(a, b, s))) walk(r, 0)
  return out
}

export function ProcessesPanel({ target, platform, active, isLocal }: { target: TargetId; platform?: string; active: boolean; isLocal?: boolean }) {
  const settings = monitorSettings.use()
  const [filter, setFilter] = useState('')
  const [sort, setSort] = useState<SortState>({ id: 'cpu', desc: true })
  const [tree, setTree] = useState(false)
  const [auto, setAuto] = useState(settings.processRefreshSec > 0)
  const [sudo, setSudo] = useState(false)
  const [selected, setSelected] = useState<string | null>(null)
  const box = useRef<HTMLDivElement>(null)
  const [width, setWidth] = useState(0)
  const now = useNow(10_000)
  const windows = platform === 'windows'
  const linux = platform === 'linux'
  useEffect(() => {
    const el = box.current
    if (!el) return
    const ro = new ResizeObserver(() => setWidth(el.clientWidth))
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  const q = useProcesses(target, { enabled: active, refetchMs: active && auto && settings.processRefreshSec > 0 ? settings.processRefreshSec * 1000 : false })
  const firstLoad = useLoadingGate(q.isPending && !q.data)

  const manual = useManualRefresh(q.refetch)
  const all = q.data ?? []

  const rows = useMemo<Row[]>(() => {
    const f = filter.trim().toLowerCase()
    let list = all
    if (linux && !settings.showKernelThreads) list = list.filter((p) => !isKernelThread(p))
    if (f) {
      list = list.filter((p) => String(p.pid) === f || p.user.toLowerCase().includes(f) || p.command.toLowerCase().includes(f) || (p.name ?? '').toLowerCase().includes(f))
    }
    if (tree && !f) return asTree(list, sort)
    return [...list].sort((a, b) => compare(a, b, sort)).map((p) => ({ ...p, depth: 0, key: String(p.pid) }))
  }, [all, filter, sort, tree, linux, settings.showKernelThreads])

  const sel = rows.find((r) => r.key === selected)
  const totals = useMemo(() => {
    let cpu = 0
    let rss = 0
    for (const p of all) {
      cpu += p.cpu
      rss += p.rss
    }
    return { cpu, rss }
  }, [all])

  const onSort = (id: string) => setSort((s) => (s.id === id ? { id, desc: !s.desc } : { id, desc: id !== 'command' && id !== 'user' && id !== 'state' }))
  const signal = (p: ProcessInfo, sig: string) => void signalProcess(target, p, sig, { sudo, windows })

  const columns: Column<Row>[] = [
    { id: 'pid', header: 'PID', width: '4.5rem', align: 'right', sortable: true, cell: (p) => p.pid },
    { id: 'user', header: 'User', width: 'minmax(4.5rem,8rem)', sortable: true, minWidth: 520, cell: (p) => p.user || '—', title: (p) => p.user },
    {
      id: 'cpu',
      header: 'CPU %',
      width: '4.5rem',
      align: 'right',
      sortable: true,
      cell: (p) => <span className={levelText[level(p.cpu, settings.warnPct, settings.critPct)]}>{p.cpu.toFixed(1)}</span>,
    },
    { id: 'mem', header: 'Mem %', width: '4.5rem', align: 'right', sortable: true, cell: (p) => p.mem.toFixed(1) },
    { id: 'rss', header: 'RSS', width: '4.5rem', align: 'right', sortable: true, minWidth: 600, cell: (p) => compactBytes(p.rss) },
    { id: 'threads', header: 'Thr', width: '3.5rem', align: 'right', sortable: true, minWidth: 760, cell: (p) => p.threads || '—' },
    { id: 'nice', header: 'Nice', width: '3.5rem', align: 'right', sortable: true, minWidth: 820, cell: (p) => p.nice },
    { id: 'state', header: 'S', width: '2.5rem', sortable: true, minWidth: 700, cell: (p) => p.state, title: (p) => STATES[p.state?.[0] ?? ''] ?? p.state },
    {
      id: 'started',
      header: 'Age',
      width: '5rem',
      align: 'right',
      sortable: true,
      minWidth: 900,
      cell: (p) => (p.started ? uptime(Math.max(0, (now - Date.parse(p.started)) / 1000)) : '—'),
      title: (p) => (p.started ? new Date(p.started).toLocaleString() : undefined),
    },
    { id: 'time', header: 'Time', width: '5.5rem', align: 'right', sortable: true, minWidth: 980, cell: (p) => cpuTime(p.cpuTime) },
    {
      id: 'command',
      header: 'Command',
      width: 'minmax(10rem,1fr)',
      sortable: true,
      cell: (p) => (
        <span className="font-mono text-xs" style={{ paddingLeft: tree ? p.depth * 14 : 0 }}>
          {tree && p.depth > 0 && <span className="text-muted-foreground">└ </span>}
          {p.command}
        </span>
      ),
      title: (p) => p.command,
    },
  ]

  const menu = (p: Row) => (
    <>
      <ContextMenuLabel className="max-w-72 truncate">
        {p.name || p.command.split(/\s+/)[0]} · PID {p.pid}
      </ContextMenuLabel>
      <ContextMenuItem onSelect={() => signal(p, 'TERM')}>
        <Square /> End process
      </ContextMenuItem>
      <ContextMenuItem variant="destructive" onSelect={() => signal(p, 'KILL')}>
        <Skull /> Kill process
      </ContextMenuItem>
      {!windows && (
        <ContextMenuSub>
          <ContextMenuSubTrigger>
            <Zap /> Send signal
          </ContextMenuSubTrigger>
          <ContextMenuSubContent>
            {SIGNALS.map((s) => (
              <ContextMenuItem key={s.id} onSelect={() => signal(p, s.id)}>
                <span className="w-16 font-mono text-xs">{s.label}</span>
                <span className="text-muted-foreground">{s.hint}</span>
              </ContextMenuItem>
            ))}
          </ContextMenuSubContent>
        </ContextMenuSub>
      )}
      <ContextMenuItem onSelect={() => void reniceWithPrompt(target, p, { sudo, windows })}>
        <SlidersHorizontal /> Change priority…
      </ContextMenuItem>
      <ContextMenuSeparator />
      <ContextMenuItem onSelect={() => setFilter(p.user)} disabled={!p.user}>
        <Filter /> Show only {p.user || 'this user'}'s processes
      </ContextMenuItem>
      <ContextMenuItem onSelect={() => void copyText(String(p.pid))}>
        <Copy /> Copy PID
      </ContextMenuItem>
      <ContextMenuItem onSelect={() => void copyText(p.command)}>
        <Copy /> Copy command line
      </ContextMenuItem>
    </>
  )

  if (firstLoad.hold) return firstLoad.show ? <LoadingPane immediate label="Loading processes…" /> : null
  if (q.isError && !q.data) {
    return (
      <EmptyState
        icon={OctagonX}
        title="Could not list processes"
        description={errorMessage(q.error)}
        action={
          <Button size="sm" variant="outline" onClick={() => void q.refetch()}>
            <RefreshCw /> Retry
          </Button>
        }
      />
    )
  }

  return (
    <div ref={box} className="flex min-h-0 flex-1 flex-col">
      <Toolbar aria-label="Process list tools" className="gap-1">
        <Input
          inputSize="sm"
          className="w-56"
          placeholder="Filter by PID, user or command"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          leading={<Filter />}
          aria-label="Filter processes"
        />
        <IconButton icon={ListTree} label={tree ? 'Show as list' : 'Show as tree'} size="sm" active={tree} onClick={() => setTree((t) => !t)} />
        <IconButton
          icon={auto ? Pause : Play}
          label={auto ? `Pause auto-refresh (every ${settings.processRefreshSec}s)` : 'Resume auto-refresh'}
          size="sm"
          active={auto}
          disabled={settings.processRefreshSec <= 0}
          onClick={() => setAuto((a) => !a)}
        />
        <IconButton icon={RefreshCw} label="Refresh now" size="sm" onClick={manual.refresh} busy={manual.refreshing} />
        {!windows && (
          <IconButton
            icon={ShieldCheck}
            label={sudo ? 'Actions run with sudo (click to turn off)' : 'Run actions with sudo'}
            size="sm"
            active={sudo}
            onClick={() => setSudo((v) => !v)}
          />
        )}
        {linux && (
          <Button size="xs" variant={settings.showKernelThreads ? 'secondary' : 'ghost'} onClick={() => monitorSettings.set({ showKernelThreads: !settings.showKernelThreads })}>
            Kernel threads
          </Button>
        )}
        <ToolbarSeparator />
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button size="xs" variant="outline" disabled={!sel}>
              <Square /> End process
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start" className="min-w-56">
            {sel && (
              <>
                <DropdownMenuLabel className="max-w-72 truncate">PID {sel.pid}</DropdownMenuLabel>
                <DropdownMenuItem onSelect={() => signal(sel, 'TERM')}>
                  <Square /> End (SIGTERM)
                </DropdownMenuItem>
                <DropdownMenuItem variant="destructive" onSelect={() => signal(sel, 'KILL')}>
                  <Skull /> Kill (SIGKILL)
                </DropdownMenuItem>
                {!windows && (
                  <>
                    <DropdownMenuSeparator />
                    {SIGNALS.filter((s) => s.id !== 'TERM' && s.id !== 'KILL').map((s) => (
                      <DropdownMenuItem key={s.id} onSelect={() => signal(sel, s.id)}>
                        <span className="w-16 font-mono text-xs">{s.label}</span>
                        <span className="text-muted-foreground">{s.hint}</span>
                      </DropdownMenuItem>
                    ))}
                  </>
                )}
                <DropdownMenuSeparator />
                <DropdownMenuItem onSelect={() => void reniceWithPrompt(target, sel, { sudo, windows })}>
                  <SlidersHorizontal /> Change priority…
                </DropdownMenuItem>
              </>
            )}
          </DropdownMenuContent>
        </DropdownMenu>
        <ToolbarSpacer />
        <span className="flex items-center gap-2 text-xs text-muted-foreground tabular">
          <Spinner active={q.isFetching} reserve className="size-3" />
          {rows.length === all.length ? `${all.length} processes` : `${rows.length} of ${all.length} processes`}
          {' · '}
          {compactBytes(totals.rss)} resident
        </span>
      </Toolbar>
      {q.isError && <div className="border-b bg-warning/10 px-3 py-1 text-xs text-warning">Refresh failed: {errorMessage(q.error)} (showing the last list)</div>}
      <VirtualTable<Row>
        rows={rows}
        columns={columns}
        rowKey={(r) => r.key}
        sort={sort}
        onSort={onSort}
        selected={selected}
        onSelect={setSelected}
        menu={menu}
        width={width}
        ariaLabel="Processes"
        rowClassName={(p) => (p.state?.[0] === 'Z' ? 'text-muted-foreground line-through' : p.state?.[0] === 'T' ? 'text-muted-foreground' : undefined)}
        onKeyAction={(e, p) => {
          if (e.key === 'Delete') {
            signal(p, e.shiftKey ? 'KILL' : 'TERM')
            return true
          }
          return false
        }}
        empty={<EmptyState size="sm" icon={Filter} title="No matching processes" description={isLocal ? undefined : 'Change the filter to see more.'} />}
      />
      {sel && (
        <div className="grid shrink-0 gap-0.5 border-t bg-panel px-3 py-1.5 text-xs">
          <div className="font-mono break-all text-foreground">{sel.command}</div>
          <div className="flex flex-wrap gap-x-4 text-muted-foreground tabular">
            <span>PID {sel.pid}</span>
            <span>Parent {sel.ppid}</span>
            {sel.user && <span>User {sel.user}</span>}
            <span>{STATES[sel.state?.[0] ?? ''] ?? sel.state}</span>
            {sel.threads ? <span>{sel.threads} threads</span> : null}
            <span>RSS {compactBytes(sel.rss)}</span>
            {sel.vsz ? <span>Virtual {compactBytes(sel.vsz)}</span> : null}
            <span>CPU time {cpuTime(sel.cpuTime)}</span>
            {sel.started && <span>Started {formatCalendarTime(sel.started)}</span>}
            <span className="ml-auto">Del: end · Shift+Del: kill · right-click: more</span>
          </div>
        </div>
      )}
    </div>
  )
}
