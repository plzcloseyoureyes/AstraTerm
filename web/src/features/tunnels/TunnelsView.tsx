/*
 * The "tunnels" tab (MobaSSHTunnel, TUN-1): saved tunnels with live status, start/stop, traffic and autostart, drag
 * reordering, row actions (edit, duplicate, delete, copy local URL, open in browser), a toolbar (new, start/stop all,
 * detect remote ports, import/export), and the live session forwards (TUN-7) below.
 */
import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react'
import {
  DndContext,
  KeyboardSensor,
  PointerSensor,
  closestCenter,
  useSensor,
  useSensors,
  type DragEndEvent,
} from '@dnd-kit/core'
import { SortableContext, arrayMove, sortableKeyboardCoordinates, useSortable, verticalListSortingStrategy } from '@dnd-kit/sortable'
import { CSS } from '@dnd-kit/utilities'
import {
  Container,
  Copy,
  Download,
  ExternalLink,
  GripVertical,
  Link2,
  MoreHorizontal,
  Pencil,
  Play,
  Plus,
  Radar,
  RefreshCw,
  RotateCw,
  Search,
  Square,
  Trash2,
  TriangleAlert,
  Upload,
  Waypoints,
  X,
} from 'lucide-react'
import { toast } from 'sonner'
import { useConnections } from '@/api/connections'
import { queryClient } from '@/api/queryClient'
import type { TabProps } from '@/app/registry'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { ContextMenu, ContextMenuContent, ContextMenuItem, ContextMenuSeparator, ContextMenuTrigger } from '@/components/ui/context-menu'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { LoadingPane } from '@/components/ui/spinner'
import { Toolbar, ToolbarSeparator, ToolbarSpacer } from '@/components/ui/toolbar'
import { Tooltip } from '@/components/ui/tooltip'
import { useNow } from '@/lib/hooks'
import { useLoadingGate } from '@/lib/useDelayedFlag'
import { cn, errorMessage, formatBytes, formatRate } from '@/lib/utils'
import {
  canOpenInBrowser,
  copyDockerHostAction,
  copyTunnelTarget,
  deleteTunnelAction,
  duplicateTunnelAction,
  exportTunnelsAction,
  openTunnelInBrowser,
  removeSessionForwardAction,
  restartTunnelAction,
  setAutoStart,
  startAllAction,
  stopAllAction,
  toggleTunnelAction,
} from './actions'
import { reorderTunnels, tunnelKeys, useSessionForwards, useTunnels } from './api'
import { KindBadge, StatusDot, Traffic, TunnelIcon } from './components'
import { connectionLabel, connectionRoute, dockerHost, endpoints, forwardLabel, isActive, kindOf, statusText, statusTone } from './model'
import { tunnelsSettings } from './settings'
import { openDetectPorts, openTunnelEditor, openTunnelImport } from './store'
import type { SessionForward, TunnelEx } from './types'

type Filter = 'all' | 'running' | 'stopped' | 'error'

// --- row actions -------------------------------------------------------------------------------------------------------

interface RowAction {
  id: string
  label: string
  icon: typeof Play
  run: () => void
  danger?: boolean
  disabled?: boolean
  separatorBefore?: boolean
  shortcut?: string
}

function rowActions(t: TunnelEx): RowAction[] {
  const active = isActive(t.status)
  const docker = dockerHost(t)
  const actions: RowAction[] = [
    { id: 'toggle', label: active ? 'Stop' : 'Start', icon: active ? Square : Play, run: () => void toggleTunnelAction(t), shortcut: 'Space' },
    { id: 'restart', label: 'Restart', icon: RotateCw, run: () => void restartTunnelAction(t), disabled: !active },
    { id: 'edit', label: 'Edit…', icon: Pencil, run: () => openTunnelEditor({ mode: 'edit', id: t.id }), separatorBefore: true, shortcut: 'Enter' },
    { id: 'duplicate', label: 'Duplicate', icon: Copy, run: () => void duplicateTunnelAction(t) },
    {
      id: 'copy',
      label: kindOf(t) === 'dynamic' ? 'Copy proxy URL' : kindOf(t) === 'local' ? 'Copy local address' : 'Copy server address',
      icon: Link2,
      run: () => void copyTunnelTarget(t),
      separatorBefore: true,
    },
  ]
  if (docker) actions.push({ id: 'docker', label: 'Copy DOCKER_HOST', icon: Container, run: () => void copyDockerHostAction(t), disabled: !docker.value })
  actions.push(
    { id: 'open', label: 'Open in browser', icon: ExternalLink, run: () => void openTunnelInBrowser(t), disabled: !canOpenInBrowser(t) },
    { id: 'delete', label: 'Delete…', icon: Trash2, run: () => void deleteTunnelAction(t), danger: true, separatorBefore: true, shortcut: 'Del' },
  )
  return actions
}

function ActionsDropdown({ t }: { t: TunnelEx }) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="icon-xs" aria-label={`Actions for ${t.name}`} onClick={(e) => e.stopPropagation()}>
          <MoreHorizontal />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" onClick={(e) => e.stopPropagation()}>
        {rowActions(t).map((a) => (
          <MenuFragment key={a.id} separator={a.separatorBefore && <DropdownMenuSeparator />}>
            <DropdownMenuItem variant={a.danger ? 'destructive' : 'default'} disabled={a.disabled} onSelect={a.run}>
              <a.icon /> {a.label}
            </DropdownMenuItem>
          </MenuFragment>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

function MenuFragment({ separator, children }: { separator?: ReactNode; children: ReactNode }) {
  return (
    <>
      {separator}
      {children}
    </>
  )
}

// --- rows --------------------------------------------------------------------------------------------------------------

function TunnelRow({
  t,
  selected,
  tabbable,
  focusTick,
  onSelect,
  sortable,
  now,
  route,
}: {
  t: TunnelEx
  /** Jump hosts / proxy of the SSH connection ("bastion → jump2"), '' when direct. */
  route: string
  selected: boolean
  /** The row is the table's tab stop (selected, or the first row when nothing is selected). */
  tabbable: boolean
  /** Bumped when keyboard navigation selects a row: the selected row takes focus. */
  focusTick: number
  onSelect: () => void
  sortable: boolean
  now: number
}) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({ id: t.id, disabled: !sortable })
  const active = isActive(t.status)
  const tone = statusTone(t.status)
  const ep = endpoints(t)
  const detail = tone === 'error' || t.status.state === 'starting' || tone === 'idle' ? statusText(t.status, now) : ''
  const warning = active ? t.status.warning : undefined
  const lastError = active ? t.status.lastError : undefined
  const docker = dockerHost(t)?.value
  const server = connectionLabel(t.connection)
  const via = `via ${route ? `${route} → ` : ''}${server}`
  const rowRef = useRef<HTMLTableRowElement | null>(null)
  useEffect(() => {
    if (selected && focusTick) rowRef.current?.focus()
    // Only keyboard navigation (focusTick) moves the focus.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [focusTick])

  const onKeyDown = (e: KeyboardEvent<HTMLTableRowElement>) => {
    if (e.target !== e.currentTarget) return
    if (e.key === 'Enter') {
      e.preventDefault()
      openTunnelEditor({ mode: 'edit', id: t.id })
    } else if (e.key === ' ') {
      e.preventDefault()
      void toggleTunnelAction(t)
    } else if (e.key === 'Delete' || e.key === 'Backspace') {
      e.preventDefault()
      void deleteTunnelAction(t)
    }
  }

  return (
    <ContextMenu>
      <ContextMenuTrigger asChild>
        <tr
          ref={(el) => {
            setNodeRef(el)
            rowRef.current = el
          }}
          style={{ transform: CSS.Translate.toString(transform), transition }}
          data-state={selected ? 'selected' : undefined}
          tabIndex={tabbable ? 0 : -1}
          aria-selected={selected}
          onClick={onSelect}
          onFocus={onSelect}
          onDoubleClick={() => openTunnelEditor({ mode: 'edit', id: t.id })}
          onKeyDown={onKeyDown}
          className={cn(
            'group border-b border-border/70 outline-none transition-colors hover:bg-accent/40 focus-visible:bg-accent/50 data-[state=selected]:bg-primary/10',
            isDragging && 'relative z-10 bg-card shadow-popover',
          )}
        >
          <td className="relative w-6 pl-1">
            {t.options.color && <span aria-hidden className="absolute inset-y-1 left-0 w-0.5 rounded-full" style={{ background: t.options.color }} />}
            {sortable && (
              <button
                type="button"
                className="flex size-5 cursor-grab items-center justify-center rounded-sm text-muted-foreground/50 opacity-0 group-hover:opacity-100 group-focus-within:opacity-100 hover:bg-accent hover:text-foreground focus-visible:opacity-100 active:cursor-grabbing"
                aria-label={`Reorder ${t.name} (Space, then arrow keys)`}
                {...attributes}
                {...listeners}
                tabIndex={tabbable ? 0 : -1}
              >
                <GripVertical className="size-3.5" />
              </button>
            )}
          </td>
          <td className="w-8">
            <IconButton
              icon={active ? Square : Play}
              label={active ? `Stop ${t.name}` : `Start ${t.name}`}
              size="xs"
              tabIndex={-1}
              className={active ? 'text-destructive/80 hover:text-destructive' : 'text-success hover:text-success'}
              onClick={(e) => {
                e.stopPropagation()
                void toggleTunnelAction(t)
              }}
            />
          </td>
          <td className="overflow-hidden py-1">
            <div className="flex min-w-0 items-center gap-2">
              <StatusDot status={t.status} />
              <TunnelIcon name={t.options.icon} className="-mx-0.5" />
              <span className="truncate font-medium" title={t.options.notes || t.name}>
                {t.name}
              </span>
              {t.options.onDemand && <span className="shrink-0 rounded-sm bg-muted px-1 text-2xs text-muted-foreground">on demand</span>}
            </div>
            {detail ? (
              <div className={cn('truncate pl-4 text-xs', tone === 'error' ? 'text-destructive' : 'text-muted-foreground')} title={detail}>
                {detail}
              </div>
            ) : warning ? (
              <div className="flex min-w-0 items-center gap-1 pl-4 text-xs text-warning" title={warning}>
                <TriangleAlert className="size-3 shrink-0" />
                <span className="truncate">{warning}</span>
              </div>
            ) : lastError ? (
              <div className="truncate pl-4 text-xs text-warning" title={lastError}>
                {lastError}
              </div>
            ) : docker ? (
              <div className="truncate pl-4 font-mono text-xs text-muted-foreground" title={`DOCKER_HOST=${docker}`}>
                DOCKER_HOST={docker}
              </div>
            ) : (
              <div className="truncate pl-4 text-xs text-muted-foreground @6xl:hidden" title={via}>
                {via}
              </div>
            )}
          </td>
          <td className="hidden overflow-hidden whitespace-nowrap @lg:table-cell">
            <KindBadge kind={kindOf(t)} />
          </td>
          <td className="hidden overflow-hidden @6xl:table-cell">
            <span className="block truncate text-muted-foreground" title={server}>
              {server}
            </span>
            {route && (
              <span className="block truncate text-xs text-muted-foreground/80" title={`Through ${route}`}>
                through {route}
              </span>
            )}
          </td>
          <td className="overflow-hidden">
            <span className="block truncate font-mono text-sm" title={ep.local}>
              {ep.local}
            </span>
          </td>
          <td className="hidden overflow-hidden @2xl:table-cell">
            <span className="block truncate font-mono text-sm" title={ep.remote}>
              {ep.remote}
            </span>
          </td>
          <td className="hidden text-right whitespace-nowrap tabular-nums @md:table-cell">
            <Tooltip content={`${t.status.activeConns} active · ${t.status.totalConns} total${t.status.failedConns ? ` · ${t.status.failedConns} failed` : ''}`}>
              <span className={cn(t.status.activeConns > 0 ? 'text-foreground' : 'text-muted-foreground')}>
                {t.status.activeConns}
                <span className="text-muted-foreground">/{t.status.totalConns}</span>
                {t.status.failedConns > 0 && <span className="ml-1 text-destructive">!{t.status.failedConns}</span>}
              </span>
            </Tooltip>
          </td>
          <td className="hidden overflow-hidden text-sm @4xl:table-cell">
            <Traffic status={t.status} />
          </td>
          <td className="hidden text-center @xl:table-cell">
            <Checkbox
              checked={t.autoStart}
              aria-label={`Start ${t.name} with NexTerm`}
              title="Start with NexTerm"
              tabIndex={-1}
              onClick={(e) => e.stopPropagation()}
              onCheckedChange={(v) => void setAutoStart(t, v === true)}
            />
          </td>
          <td className="pr-1.5 text-right">
            <ActionsDropdown t={t} />
          </td>
        </tr>
      </ContextMenuTrigger>
      <ContextMenuContent>
        {rowActions(t).map((a) => (
          <MenuFragment key={a.id} separator={a.separatorBefore && <ContextMenuSeparator />}>
            <ContextMenuItem variant={a.danger ? 'destructive' : 'default'} disabled={a.disabled} onSelect={a.run}>
              <a.icon /> {a.label}
            </ContextMenuItem>
          </MenuFragment>
        ))}
      </ContextMenuContent>
    </ContextMenu>
  )
}

// --- session forwards --------------------------------------------------------------------------------------------------

function SessionForwardsSection({ list }: { list: SessionForward[] }) {
  const [open, setOpen] = useState(true)
  if (!list.length) return null
  return (
    <section className="border-t">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        className="flex h-8 w-full items-center gap-2 px-3 text-left text-sm font-semibold text-muted-foreground hover:text-foreground"
      >
        <span className={cn('transition-transform', open ? 'rotate-90' : '')}>›</span>
        Session forwards
        <span className="rounded-sm bg-muted px-1.5 text-xs font-normal">{list.length}</span>
        <span className="truncate text-xs font-normal">— live with an open SSH session (from the connection’s settings or added from a terminal)</span>
      </button>
      {open && (
        <table className="w-full border-collapse text-base">
          <tbody>
            {list.map((f) => (
              <tr key={f.id} className="border-t border-border/60 hover:bg-accent/30">
                <td className="w-8 pl-3">
                  <StatusDot status={f.status} />
                </td>
                <td className="max-w-0 min-w-32 py-1">
                  <div className="truncate font-medium">{f.spec.name || f.sessionTitle}</div>
                  <div className="truncate text-xs text-muted-foreground">
                    {f.source === 'adhoc' ? 'Added from the session' : 'From the connection settings'} · {f.sessionTitle}
                    {f.status.state === 'error' && f.status.error ? <span className="text-destructive"> · {f.status.error}</span> : null}
                    {f.status.state === 'running' && f.status.warning ? (
                      <span className="text-warning" title={f.status.warning}>
                        {' '}
                        · {f.status.warning}
                      </span>
                    ) : null}
                  </div>
                </td>
                <td className="max-w-0 min-w-40">
                  <span className="block truncate font-mono text-sm" title={forwardLabel(f.spec)}>
                    {forwardLabel(f.spec)}
                  </span>
                  {(f.status.localAddr || f.status.remoteAddr) && (
                    <span className="block truncate text-xs text-muted-foreground">bound {f.status.localAddr || f.status.remoteAddr}</span>
                  )}
                </td>
                <td className="w-px pr-3 text-right whitespace-nowrap tabular-nums">
                  {f.status.activeConns}
                  <span className="text-muted-foreground">/{f.status.totalConns}</span>
                </td>
                <td className="hidden w-px pr-3 text-sm @3xl:table-cell">
                  <Traffic status={f.status} compact />
                </td>
                <td className="w-8 pr-1.5">
                  <IconButton icon={X} size="xs" label="Stop this forward" onClick={() => void removeSessionForwardAction(f)} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}

// --- view ----------------------------------------------------------------------------------------------------------------

function FilterLabel({ label, count, tone }: { label: string; count: number; tone?: 'error' }) {
  return (
    <span className="flex items-center gap-1 whitespace-nowrap">
      {label}
      <span className={cn('tabular-nums', tone === 'error' ? 'text-destructive' : 'text-muted-foreground/80')}>{count}</span>
    </span>
  )
}

function matches(t: TunnelEx, q: string): boolean {
  if (!q) return true
  const ep = endpoints(t)
  const hay = `${t.name} ${ep.local} ${ep.remote} ${t.connection?.name ?? ''} ${t.connection?.host ?? ''} ${t.options.notes ?? ''}`.toLowerCase()
  return q
    .toLowerCase()
    .split(/\s+/)
    .every((w) => hay.includes(w))
}

function byFilter(t: TunnelEx, f: Filter): boolean {
  const tone = statusTone(t.status)
  switch (f) {
    case 'running':
      return isActive(t.status)
    case 'stopped':
      return tone === 'stopped'
    case 'error':
      return tone === 'error'
    default:
      return true
  }
}

export default function TunnelsView(_props: TabProps) {
  const q = useTunnels()
  const firstLoad = useLoadingGate(q.isLoading)
  const settings = tunnelsSettings.use()
  const sf = useSessionForwards(settings.showSessionForwards)
  const [filter, setFilter] = useState<Filter>('all')
  const [search, setSearch] = useState('')
  const [selected, setSelected] = useState<string | null>(null)
  const [focusTick, setFocusTick] = useState(0)
  const tunnels = useMemo(() => q.data ?? [], [q.data])
  const { data: conns } = useConnections()
  const routes = useMemo(() => {
    const byId = new Map((conns ?? []).map((c) => [c.id, c]))
    return new Map(tunnels.map((t) => [t.id, connectionRoute(byId.get(t.connectionId), conns).join(' → ')]))
  }, [tunnels, conns])
  const needsClock = tunnels.some((t) => t.status.retryAt)
  const now = useNow(needsClock ? 1000 : 60_000)

  const visible = useMemo(() => tunnels.filter((t) => byFilter(t, filter) && matches(t, search.trim())), [tunnels, filter, search])
  const sortable = filter === 'all' && !search.trim()
  const counts = useMemo(() => {
    let running = 0
    let errors = 0
    let conns = 0
    let bin = 0
    let bout = 0
    let rin = 0
    let rout = 0
    for (const t of tunnels) {
      if (isActive(t.status)) running++
      if (t.status.state === 'error') errors++
      conns += t.status.activeConns
      bin += t.status.bytesIn
      bout += t.status.bytesOut
      rin += t.status.rateIn
      rout += t.status.rateOut
    }
    return { running, errors, stopped: tunnels.length - running - errors, conns, bin, bout, rin, rout }
  }, [tunnels])

  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 4 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  )
  const onDragEnd = (e: DragEndEvent) => {
    const { active, over } = e
    if (!over || active.id === over.id) return
    const from = tunnels.findIndex((t) => t.id === active.id)
    const to = tunnels.findIndex((t) => t.id === over.id)
    if (from < 0 || to < 0) return
    const next = arrayMove(tunnels, from, to).map((t, i) => ({ ...t, sortOrder: i + 1 }))
    queryClient.setQueryData(tunnelKeys.list, next)
    reorderTunnels(next.map((t) => ({ id: t.id, sortOrder: t.sortOrder }))).catch((err) => {
      toast.error('Could not save the order', { description: errorMessage(err) })
      void queryClient.invalidateQueries({ queryKey: tunnelKeys.list, exact: true })
    })
  }

  const move = (delta: number) => {
    if (!visible.length) return
    const i = visible.findIndex((t) => t.id === selected)
    const next = visible[Math.min(visible.length - 1, Math.max(0, i < 0 ? 0 : i + delta))]
    setSelected(next.id)
    setFocusTick((n) => n + 1)
  }

  const toolbar = (
    <Toolbar aria-label="Tunnels" className="gap-1">
      <Button size="sm" aria-label="New tunnel" title="New tunnel" onClick={() => openTunnelEditor({ mode: 'create' })}>
        <Plus /> <span className="hidden @md:inline">New tunnel</span>
      </Button>
      <ToolbarSeparator />
      <Button
        variant="ghost"
        size="sm"
        aria-label="Start all tunnels"
        title="Start all tunnels"
        disabled={!tunnels.length || counts.running === tunnels.length}
        onClick={() => void startAllAction()}
      >
        <Play className="text-success" /> <span className="hidden @2xl:inline">Start all</span>
      </Button>
      <Button variant="ghost" size="sm" aria-label="Stop all tunnels" title="Stop all tunnels" disabled={!counts.running} onClick={() => void stopAllAction()}>
        <Square className="text-destructive/80" /> <span className="hidden @2xl:inline">Stop all</span>
      </Button>
      <ToolbarSeparator />
      <Button variant="ghost" size="sm" aria-label="Detect listening ports" title="Detect listening ports on an SSH server" onClick={() => openDetectPorts()}>
        <Radar /> <span className="hidden @2xl:inline">Detect ports</span>
      </Button>
      <ToolbarSeparator />
      <IconButton icon={Upload} label="Import tunnels…" onClick={() => openTunnelImport()} />
      <IconButton icon={Download} label="Export tunnels (JSON)" disabled={!tunnels.length} onClick={() => void exportTunnelsAction()} />
      <ToolbarSpacer />
      <div className="hidden @2xl:block">
        <SegmentedControl<Filter>
          size="sm"
          aria-label="Filter tunnels"
          value={filter}
          onValueChange={setFilter}
          options={[
            { value: 'all', label: <FilterLabel label="All" count={tunnels.length} /> },
            { value: 'running', label: <FilterLabel label="Running" count={counts.running} /> },
            { value: 'stopped', label: <FilterLabel label="Stopped" count={counts.stopped} /> },
            { value: 'error', label: <FilterLabel label="Errors" count={counts.errors} tone={counts.errors ? 'error' : undefined} /> },
          ]}
        />
      </div>
      <Input
        inputSize="sm"
        className="w-40 @3xl:w-56"
        placeholder="Filter…"
        aria-label="Filter tunnels"
        value={search}
        onChange={(e) => setSearch(e.target.value)}
        leading={<Search />}
        trailing={
          search ? (
            <button type="button" aria-label="Clear filter" className="flex size-5 items-center justify-center rounded-sm hover:bg-accent" onClick={() => setSearch('')}>
              <X className="size-3" />
            </button>
          ) : undefined
        }
      />
    </Toolbar>
  )

  let body: ReactNode
  if (firstLoad.hold) {
    body = firstLoad.show ? <LoadingPane immediate label="Loading tunnels…" /> : null
  } else if (q.isError) {
    body = (
      <EmptyState
        icon={Waypoints}
        title="Could not load the tunnels"
        description={errorMessage(q.error)}
        action={
          <Button size="sm" variant="secondary" onClick={() => void q.refetch()}>
            <RefreshCw /> Retry
          </Button>
        }
      />
    )
  } else if (!tunnels.length) {
    body = (
      <EmptyState
        icon={Waypoints}
        title="No tunnels yet"
        description="Forward ports through SSH: reach a database behind a bastion, expose a local dev server on a remote host, or browse through a SOCKS proxy."
        action={
          <>
            <Button size="sm" onClick={() => openTunnelEditor({ mode: 'create' })}>
              <Plus /> New tunnel
            </Button>
            <Button size="sm" variant="secondary" onClick={() => openDetectPorts()}>
              <Radar /> Detect remote ports
            </Button>
            <Button size="sm" variant="ghost" onClick={() => openTunnelImport()}>
              <Upload /> Import…
            </Button>
          </>
        }
      />
    )
  } else if (!visible.length) {
    body = (
      <EmptyState
        size="sm"
        icon={Search}
        title="No tunnel matches"
        action={
          <Button
            size="sm"
            variant="ghost"
            onClick={() => {
              setSearch('')
              setFilter('all')
            }}
          >
            Clear filters
          </Button>
        }
      />
    )
  } else {
    body = (
      <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={onDragEnd}>
        <table
          className="w-full table-fixed border-collapse text-base"
          aria-label="Tunnels"
          onKeyDown={(e) => {
            if (e.key === 'ArrowDown') {
              e.preventDefault()
              move(1)
            } else if (e.key === 'ArrowUp') {
              e.preventDefault()
              move(-1)
            }
          }}
        >
          <thead className="sticky top-0 z-10 bg-panel">
            <tr className="border-b text-left text-xs text-muted-foreground">
              <th className="w-6" aria-label="Order" />
              <th className="w-8" aria-label="Start / stop" />
              <th className="h-7 px-2.5 font-medium">Name</th>
              <th className="hidden w-26 px-2.5 font-medium @lg:table-cell">Type</th>
              <th className="hidden w-44 px-2.5 font-medium @6xl:table-cell">SSH server</th>
              <th className="w-[38%] px-2.5 font-medium @lg:w-40">This machine</th>
              <th className="hidden w-40 px-2.5 font-medium @2xl:table-cell">Server side</th>
              <th className="hidden w-14 px-2.5 text-right font-medium @md:table-cell">Conns</th>
              <th className="hidden w-36 px-2.5 font-medium @4xl:table-cell">Traffic</th>
              <th className="hidden w-12 px-1 text-center font-medium @xl:table-cell" title="Start with NexTerm">
                Auto
              </th>
              <th className="w-9" aria-label="Actions" />
            </tr>
          </thead>
          <SortableContext items={visible.map((t) => t.id)} strategy={verticalListSortingStrategy}>
            <tbody className="[&_td]:px-2.5 [&_td:first-child]:pr-0 [&_td:nth-child(2)]:px-0">
              {visible.map((t, i) => (
                <TunnelRow
                  key={t.id}
                  t={t}
                  route={routes.get(t.id) ?? ''}
                  selected={selected === t.id}
                  tabbable={selected === t.id || ((!selected || !visible.some((v) => v.id === selected)) && i === 0)}
                  focusTick={focusTick}
                  onSelect={() => setSelected(t.id)}
                  sortable={sortable}
                  now={now}
                />
              ))}
            </tbody>
          </SortableContext>
        </table>
      </DndContext>
    )
  }

  return (
    <div className="flex h-full min-h-0 flex-col bg-panel @container">
      {toolbar}
      <div className="min-h-0 flex-1 overflow-auto">
        {body}
        {settings.showSessionForwards && <SessionForwardsSection list={sf.data ?? []} />}
      </div>
      {tunnels.length > 0 && (
        <footer className="flex h-6 shrink-0 items-center gap-3 border-t bg-toolbar px-3 text-xs text-muted-foreground tabular-nums">
          <span>
            <span className="text-success">{counts.running}</span> running · {counts.stopped} stopped
            {counts.errors ? <span className="text-destructive"> · {counts.errors} failed</span> : null}
          </span>
          <span>{counts.conns} active connections</span>
          <span className="ml-auto">
            ↓ {formatBytes(counts.bin)} ↑ {formatBytes(counts.bout)}
            {counts.rin + counts.rout > 0 && (
              <span className="ml-2 text-primary">
                {formatRate(counts.rin)} / {formatRate(counts.rout)}
              </span>
            )}
          </span>
        </footer>
      )}
    </div>
  )
}
