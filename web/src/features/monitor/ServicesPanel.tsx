/*
 * Service manager (MON-3): systemd units or Windows services — state filter, start / stop / restart / reload /
 * enable / disable (sudo by default on Linux) and the unit's journal.
 */
import { useEffect, useMemo, useRef, useState } from 'react'
import { Cog, Copy, FileText, Filter, Play, RefreshCw, RotateCw, ShieldCheck, Square, ToggleLeft, ToggleRight, Wrench } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ContextMenuItem, ContextMenuLabel, ContextMenuSeparator } from '@/components/ui/context-menu'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { LoadingPane, Spinner } from '@/components/ui/spinner'
import { Toolbar, ToolbarSpacer } from '@/components/ui/toolbar'
import { useManualRefresh } from '@/lib/hooks'
import { useLoadingGate } from '@/lib/useDelayedFlag'
import { copyText, cn, errorMessage } from '@/lib/utils'
import { getServiceLogs, useServices } from './api'
import { ACTION_LABEL, runServiceAction } from './actions'
import type { ServiceAction, ServiceUnit, TargetId } from './types'
import { VirtualTable, type Column, type SortState } from './VirtualTable'

type StateFilter = 'all' | 'running' | 'failed' | 'inactive'

function activeBadge(u: ServiceUnit) {
  const v =
    u.active === 'active' ? 'success' : u.active === 'failed' ? 'destructive' : u.active === 'activating' || u.active === 'deactivating' || u.active === 'reloading' ? 'warning' : 'secondary'
  return (
    <Badge variant={v} className="font-normal">
      {u.active}
      {u.sub && u.sub !== u.active ? ` · ${u.sub}` : ''}
    </Badge>
  )
}

export function ServicesPanel({ target, platform, active, onFollowUnit }: { target: TargetId; platform?: string; active: boolean; onFollowUnit?: (unit: string) => void }) {
  const q = useServices(target, active)
  const firstLoad = useLoadingGate(q.isPending)
  const manual = useManualRefresh(q.refetch)
  const windows = platform === 'windows'
  const [filter, setFilter] = useState('')
  const [state, setState] = useState<StateFilter>('all')
  const [sort, setSort] = useState<SortState>({ id: 'name', desc: false })
  const [selected, setSelected] = useState<string | null>(null)
  const [sudo, setSudo] = useState(!windows)
  const [logsFor, setLogsFor] = useState<string | null>(null)
  const [busy, setBusy] = useState<string | null>(null)
  const box = useRef<HTMLDivElement>(null)
  const [width, setWidth] = useState(0)
  useEffect(() => {
    const el = box.current
    if (!el) return
    const ro = new ResizeObserver(() => setWidth(el.clientWidth))
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  const units = q.data?.services ?? []
  const counts = useMemo(() => {
    let running = 0
    let failed = 0
    for (const u of units) {
      if (u.active === 'active') running++
      if (u.active === 'failed') failed++
    }
    return { running, failed }
  }, [units])
  const rows = useMemo(() => {
    const f = filter.trim().toLowerCase()
    const list = units.filter((u) => {
      if (state === 'running' && u.active !== 'active') return false
      if (state === 'failed' && u.active !== 'failed') return false
      if (state === 'inactive' && u.active !== 'inactive') return false
      return !f || u.name.toLowerCase().includes(f) || u.description.toLowerCase().includes(f)
    })
    const key = (u: ServiceUnit) => (sort.id === 'state' ? u.active + u.sub : sort.id === 'enabled' ? (u.enabled ?? '') : sort.id === 'description' ? u.description : u.name).toLowerCase()
    return list.sort((a, b) => (sort.desc ? -1 : 1) * key(a).localeCompare(key(b)) || a.name.localeCompare(b.name))
  }, [units, filter, state, sort])

  if (firstLoad.hold || q.isPending) return firstLoad.show ? <LoadingPane immediate label="Loading services…" /> : null
  if (q.isError) {
    return (
      <EmptyState
        icon={Cog}
        title="Could not list services"
        description={errorMessage(q.error)}
        action={
          <Button size="sm" variant="outline" onClick={() => void q.refetch()}>
            <RefreshCw /> Retry
          </Button>
        }
      />
    )
  }
  if (!q.data.manager) {
    return <EmptyState icon={Cog} title="Service management is not available" description={q.data.message} />
  }

  const act = async (u: ServiceUnit, a: ServiceAction) => {
    setBusy(u.name)
    try {
      await runServiceAction(target, u.name, a, { sudo, windows })
    } finally {
      setBusy(null)
    }
  }
  const sel = rows.find((u) => u.name === selected)
  const running = (u: ServiceUnit) => u.active === 'active' || u.active === 'activating'
  const actions = (u: ServiceUnit): { a: ServiceAction; icon: typeof Play; enabled: boolean }[] => [
    { a: 'start', icon: Play, enabled: !running(u) },
    { a: 'stop', icon: Square, enabled: running(u) },
    { a: 'restart', icon: RotateCw, enabled: true },
    ...(!windows ? [{ a: 'reload' as ServiceAction, icon: RefreshCw, enabled: running(u) }] : []),
    { a: 'enable', icon: ToggleRight, enabled: u.enabled !== 'enabled' && u.enabled !== 'static' },
    { a: 'disable', icon: ToggleLeft, enabled: u.enabled === 'enabled' },
  ]

  const columns: Column<ServiceUnit>[] = [
    {
      id: 'name',
      header: 'Service',
      width: 'minmax(10rem,1.2fr)',
      sortable: true,
      cell: (u) => (
        <span className={cn('font-medium', busy === u.name && 'opacity-60')}>
          <Spinner active={busy === u.name} className="mr-1 inline size-3" />
          {u.name}
        </span>
      ),
      title: (u) => u.name,
    },
    { id: 'state', header: 'State', width: '10rem', sortable: true, cell: activeBadge },
    { id: 'enabled', header: windows ? 'Startup' : 'Boot', width: '6.5rem', sortable: true, minWidth: 560, cell: (u) => u.enabled || '—' },
    { id: 'description', header: 'Description', width: 'minmax(8rem,2fr)', sortable: true, minWidth: 700, cell: (u) => u.description, title: (u) => u.description },
  ]

  const menu = (u: ServiceUnit) => (
    <>
      <ContextMenuLabel className="max-w-72 truncate">{u.name}</ContextMenuLabel>
      {actions(u).map(({ a, icon: Icon, enabled }) => (
        <ContextMenuItem key={a} disabled={!enabled} variant={a === 'stop' || a === 'disable' ? 'destructive' : undefined} onSelect={() => void act(u, a)}>
          <Icon /> {ACTION_LABEL[a]}
        </ContextMenuItem>
      ))}
      {!windows && (
        <>
          <ContextMenuSeparator />
          <ContextMenuItem onSelect={() => setLogsFor(u.name)}>
            <FileText /> Recent log…
          </ContextMenuItem>
          {onFollowUnit && (
            <ContextMenuItem onSelect={() => onFollowUnit(u.name)}>
              <FileText /> Follow log
            </ContextMenuItem>
          )}
        </>
      )}
      <ContextMenuItem onSelect={() => void copyText(u.name)}>
        <Copy /> Copy name
      </ContextMenuItem>
    </>
  )

  return (
    <div ref={box} className="flex min-h-0 flex-1 flex-col">
      <Toolbar aria-label="Service tools" className="gap-1">
        <Input inputSize="sm" className="w-52" placeholder="Filter services" value={filter} onChange={(e) => setFilter(e.target.value)} leading={<Filter />} aria-label="Filter services" />
        <SegmentedControl
          size="sm"
          value={state}
          onValueChange={setState}
          aria-label="State"
          options={[
            { value: 'all', label: `All ${units.length}` },
            { value: 'running', label: `Running ${counts.running}` },
            { value: 'failed', label: `Failed ${counts.failed}` },
            { value: 'inactive', label: 'Inactive' },
          ]}
        />
        <IconButton icon={RefreshCw} label="Refresh" size="sm" onClick={manual.refresh} busy={manual.refreshing} />
        {!windows && (
          <IconButton icon={ShieldCheck} label={sudo ? 'Actions run with sudo (click to turn off)' : 'Run actions with sudo'} size="sm" active={sudo} onClick={() => setSudo((v) => !v)} />
        )}
        <ToolbarSpacer />
        {sel && (
          <div className="flex items-center gap-0.5">
            {actions(sel).map(({ a, icon: Icon, enabled }) => (
              <IconButton key={a} icon={Icon} label={`${ACTION_LABEL[a]} ${sel.name}`} size="sm" disabled={!enabled || busy === sel.name} onClick={() => void act(sel, a)} />
            ))}
            {!windows && <IconButton icon={FileText} label="Recent log" size="sm" onClick={() => setLogsFor(sel.name)} />}
          </div>
        )}
      </Toolbar>
      <VirtualTable<ServiceUnit>
        rows={rows}
        columns={columns}
        rowKey={(u) => u.name}
        sort={sort}
        onSort={(id) => setSort((s) => (s.id === id ? { id, desc: !s.desc } : { id, desc: false }))}
        selected={selected}
        onSelect={setSelected}
        onActivate={(u) => !windows && setLogsFor(u.name)}
        menu={menu}
        width={width}
        ariaLabel="Services"
        empty={<EmptyState size="sm" icon={Wrench} title="No matching services" />}
      />
      <UnitLogDialog target={target} unit={logsFor} sudo={sudo} onClose={() => setLogsFor(null)} onFollow={onFollowUnit} />
    </div>
  )
}

function UnitLogDialog({ target, unit, sudo, onClose, onFollow }: { target: TargetId; unit: string | null; sudo: boolean; onClose: () => void; onFollow?: (unit: string) => void }) {
  const [lines, setLines] = useState<string[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const load = async (name: string) => {
    setLoading(true)
    setError(null)
    try {
      setLines((await getServiceLogs(target, name, 300, sudo)).lines)
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setLoading(false)
    }
  }
  useEffect(() => {
    setLines(null)
    if (unit) void load(unit)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [unit])
  const end = useRef<HTMLDivElement>(null)
  useEffect(() => end.current?.scrollIntoView({ block: 'end' }), [lines])
  return (
    <Dialog open={!!unit} onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="2xl">
        <DialogHeader>
          <DialogTitle>
            <FileText className="size-4" /> {unit}
          </DialogTitle>
          <DialogDescription>Last 300 journal entries{sudo ? ' (read with sudo)' : ''}.</DialogDescription>
        </DialogHeader>
        <div className="flex items-center gap-2">
          <Button size="xs" variant="outline" onClick={() => unit && void load(unit)} loading={loading}>
            <RefreshCw /> Refresh
          </Button>
          {onFollow && unit && (
            <Button
              size="xs"
              variant="outline"
              onClick={() => {
                onFollow(unit)
                onClose()
              }}
            >
              <FileText /> Follow live
            </Button>
          )}
        </div>
        <div className="h-[55vh] overflow-auto rounded-md border bg-muted/30 p-2 font-mono text-xs leading-relaxed whitespace-pre-wrap">
          {error ? (
            <div className="text-destructive">{error}</div>
          ) : lines == null ? (
            <LoadingPane />
          ) : lines.length === 0 ? (
            <div className="text-muted-foreground">No entries.</div>
          ) : (
            lines.map((l, i) => <div key={i}>{l}</div>)
          )}
          <div ref={end} />
        </div>
      </DialogContent>
    </Dialog>
  )
}
