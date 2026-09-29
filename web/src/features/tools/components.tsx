/*
 * Shared building blocks for tool panels: form/run layout, a virtualized sortable results table, CSV export, copy
 * buttons, status and progress, rating badges, the "run from" SSH-host picker and the recent-runs list.
 */
import * as React from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import { ArrowDown, ArrowUp, Check, Copy, Download, Eraser, Info, Play, Square, X } from 'lucide-react'
import { toast } from 'sonner'
import { useConnections } from '@/api/connections'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { Field } from '@/components/ui/field'
import { SimpleSelect } from '@/components/ui/select'
import { Spinner } from '@/components/ui/spinner'
import { copyText, cn, formatDuration } from '@/lib/utils'
import type { JobStatus, ToolJobState } from './jobs'
import { clearHistory, useToolHistory } from './history'
import type { ToolRow, ToolRun } from './types'

// ---- layout ----------------------------------------------------------------------------------------------------------

export function PanelLayout({
  title,
  description,
  form,
  controls,
  children,
}: {
  title: string
  description?: React.ReactNode
  form: React.ReactNode
  /** Run / stop / export bar: always visible between the (scrollable) form and the results. */
  controls?: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <div className="flex h-full min-h-0 flex-col">
      <header className="shrink-0 border-b px-4 py-2.5">
        <h2 className="text-md font-semibold">{title}</h2>
        {description && <p className="mt-0.5 line-clamp-2 text-sm text-muted-foreground text-balance">{description}</p>}
      </header>
      <div className="max-h-[45%] shrink-0 overflow-y-auto bg-panel/40 px-4 pt-3 pb-3">{form}</div>
      {controls && <div className="shrink-0 border-y bg-panel/40 px-4 py-2">{controls}</div>}
      {/* Scrolls as a whole on short windows; result tables keep a usable minimum height (ResultArea). */}
      <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">{children}</div>
    </div>
  )
}

/** The part of a panel's result area that takes the remaining height (and scrolls itself). */
export function ResultArea({ children, className }: { children: React.ReactNode; className?: string }) {
  return <div className={cn('min-h-60 flex-1', className)}>{children}</div>
}

/** A responsive grid of form fields (container queries: the Tools tab can be narrow). */
export function FormGrid({ children, className }: { children: React.ReactNode; className?: string }) {
  return <div className={cn('grid grid-cols-1 gap-3 @lg:grid-cols-2 @3xl:grid-cols-3 @6xl:grid-cols-4', className)}>{children}</div>
}

// ---- run controls & status -------------------------------------------------------------------------------------------

export function RunControls({
  state,
  onRun,
  onCancel,
  onClear,
  runLabel = 'Run',
  disabled,
  count,
  countLabel = 'results',
  extra,
}: {
  state: ToolJobState
  onRun: () => void
  onCancel: () => void
  onClear?: () => void
  runLabel?: string
  disabled?: boolean
  count?: number
  countLabel?: string
  extra?: React.ReactNode
}) {
  const running = state.status === 'running' || state.status === 'starting'
  return (
    <div className="flex flex-wrap items-center gap-2">
      {running ? (
        <Button variant="destructive" size="sm" onClick={onCancel} disabled={state.stopping} aria-keyshortcuts="Escape">
          <Square className="size-3.5" /> {state.stopping ? 'Stopping…' : 'Stop'}
        </Button>
      ) : (
        <Button size="sm" onClick={onRun} disabled={disabled}>
          <Play className="size-3.5" /> {runLabel}
        </Button>
      )}
      <StatusBadge status={state.status} />
      {state.startedAt && <Elapsed startedAt={state.startedAt} finishedAt={state.finishedAt} running={running} />}
      {typeof count === 'number' && count > 0 && (
        <span className="text-sm text-muted-foreground tabular">
          {count.toLocaleString()} {countLabel}
        </span>
      )}
      <div className="ml-auto flex items-center gap-1">
        {extra}
        {onClear && state.status !== 'idle' && !running && (
          <Button variant="ghost" size="xs" onClick={onClear} title="Clear results">
            <Eraser className="size-3.5" /> Clear
          </Button>
        )}
      </div>
    </div>
  )
}

export function StatusBadge({ status }: { status: JobStatus }) {
  switch (status) {
    case 'starting':
    case 'running':
      return (
        <Badge variant="info" className="gap-1">
          <Spinner reserve className="size-3 text-current" label="Running" /> {status === 'starting' ? 'Starting' : 'Running'}
        </Badge>
      )
    case 'done':
      return <Badge variant="success">Done</Badge>
    case 'error':
      return <Badge variant="destructive">Error</Badge>
    case 'canceled':
      return <Badge variant="warning">Stopped</Badge>
    default:
      return <Badge variant="outline">Idle</Badge>
  }
}

function Elapsed({ startedAt, finishedAt, running }: { startedAt: number; finishedAt?: number; running: boolean }) {
  const [now, setNow] = React.useState(() => Date.now())
  React.useEffect(() => {
    if (!running) return
    const t = window.setInterval(() => setNow(Date.now()), 250)
    return () => window.clearInterval(t)
  }, [running])
  const end = running ? now : (finishedAt ?? now)
  return <span className="text-sm text-muted-foreground tabular">{formatDuration(Math.max(0, end - startedAt))}</span>
}

/** Error / note / truncation banners for a job. */
export function JobNotes({ state }: { state: ToolJobState }) {
  return (
    <>
      {state.error && (
        <div role="alert" className="mx-4 mt-3 flex shrink-0 items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive">
          <X className="mt-0.5 size-3.5 shrink-0" />
          <span className="min-w-0 break-words">{state.error}</span>
        </div>
      )}
      {(state.note || state.truncated) && (
        <div className="mx-4 mt-3 flex shrink-0 items-start gap-2 rounded-md border border-warning/40 bg-warning/10 px-3 py-2 text-sm text-warning">
          <Info className="mt-0.5 size-3.5 shrink-0" />
          <span className="min-w-0 break-words">
            {state.note}
            {state.truncated && ' Too many results: only the first 100,000 rows are kept.'}
          </span>
        </div>
      )}
    </>
  )
}

export function ProgressBar({ done, total, label }: { done: number; total: number; label?: React.ReactNode }) {
  const pct = total > 0 ? Math.min(100, (done / total) * 100) : 0
  return (
    <div className="flex items-center gap-2 px-4 pt-3 text-xs text-muted-foreground">
      <div
        className="h-1.5 min-w-24 flex-1 overflow-hidden rounded-full bg-muted"
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={total}
        aria-valuenow={done}
      >
        <div className="h-full rounded-full bg-primary transition-[width] duration-150" style={{ width: `${pct}%` }} />
      </div>
      <span className="shrink-0 tabular">
        {done.toLocaleString()} / {total.toLocaleString()}
        {label ? <> · {label}</> : null}
      </span>
    </div>
  )
}

// ---- results table ---------------------------------------------------------------------------------------------------

export interface Column<T> {
  key: string
  header: React.ReactNode
  cell: (row: T) => React.ReactNode
  className?: string
  /** CSV value (columns without it are not exported). */
  csv?: (row: T) => string
  /** Sort key (columns without it are not sortable). */
  sortValue?: (row: T) => string | number
}

function compareValues(a: string | number, b: string | number): number {
  if (typeof a === 'number' && typeof b === 'number') return a - b
  return String(a).localeCompare(String(b), undefined, { numeric: true, sensitivity: 'base' })
}

/**
 * A virtualized table (thousands of rows stay smooth) with click-to-sort headers and a sticky header. It fills and
 * scrolls its container.
 */
export function ResultsTable<T>({
  columns,
  rows,
  empty,
  rowKey,
  rowClassName,
  onRowDoubleClick,
  label,
}: {
  columns: Column<T>[]
  rows: T[]
  empty?: React.ReactNode
  rowKey: (row: T, i: number) => string
  rowClassName?: (row: T) => string | undefined
  onRowDoubleClick?: (row: T) => void
  label?: string
}) {
  const [sort, setSort] = React.useState<{ key: string; desc: boolean } | null>(null)
  const sorted = React.useMemo(() => {
    const col = sort && columns.find((c) => c.key === sort.key)
    if (!sort || !col?.sortValue) return rows
    const get = col.sortValue
    const out = rows.slice().sort((a, b) => compareValues(get(a), get(b)))
    return sort.desc ? out.reverse() : out
  }, [rows, sort, columns])
  const scrollRef = React.useRef<HTMLDivElement>(null)
  // eslint-disable-next-line react-hooks/incompatible-library
  const virtualizer = useVirtualizer({ count: sorted.length, getScrollElement: () => scrollRef.current, estimateSize: () => 30, overscan: 16 })
  if (rows.length === 0 && empty) return <>{empty}</>
  const items = virtualizer.getVirtualItems()
  const padTop = items.length ? items[0].start : 0
  const padBottom = items.length ? virtualizer.getTotalSize() - items[items.length - 1].end : 0

  const toggleSort = (key: string) =>
    setSort((s) => (s?.key !== key ? { key, desc: false } : !s.desc ? { key, desc: true } : null))

  return (
    <div ref={scrollRef} className="h-full overflow-auto" role="region" aria-label={label ?? 'Results'} tabIndex={0}>
      <table className="w-full border-collapse text-base">
        <thead className="sticky top-0 z-10 bg-panel shadow-[inset_0_-1px_0_var(--color-border)]">
          <tr>
            {columns.map((c) => {
              const active = sort?.key === c.key
              return (
                <th
                  key={c.key}
                  className={cn('h-7 px-2.5 text-left align-middle text-xs font-medium whitespace-nowrap text-muted-foreground', c.className)}
                  aria-sort={active ? (sort!.desc ? 'descending' : 'ascending') : undefined}
                >
                  {c.sortValue ? (
                    <button type="button" onClick={() => toggleSort(c.key)} className="inline-flex items-center gap-1 rounded-sm outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50">
                      {c.header}
                      {active && (sort!.desc ? <ArrowDown className="size-3" /> : <ArrowUp className="size-3" />)}
                    </button>
                  ) : (
                    c.header
                  )}
                </th>
              )
            })}
          </tr>
        </thead>
        <tbody>
          {padTop > 0 && <tr aria-hidden style={{ height: padTop }} />}
          {items.map((vi) => {
            const row = sorted[vi.index]
            return (
              <tr
                key={rowKey(row, vi.index)}
                data-index={vi.index}
                ref={virtualizer.measureElement}
                className={cn('border-b border-border/70 transition-colors hover:bg-accent/40', rowClassName?.(row))}
                onDoubleClick={onRowDoubleClick ? () => onRowDoubleClick(row) : undefined}
              >
                {columns.map((c) => (
                  <td key={c.key} className={cn('h-8 px-2.5 align-middle', c.className)}>
                    {c.cell(row)}
                  </td>
                ))}
              </tr>
            )
          })}
          {padBottom > 0 && <tr aria-hidden style={{ height: padBottom }} />}
        </tbody>
      </table>
    </div>
  )
}

// ---- copy / export ---------------------------------------------------------------------------------------------------

export function CopyButton({ value, label = 'Copy', size = 'xs', title }: { value: string; label?: string; size?: 'xs' | 'sm'; title?: string }) {
  const [copied, setCopied] = React.useState(false)
  React.useEffect(() => {
    if (!copied) return
    const t = window.setTimeout(() => setCopied(false), 1200)
    return () => window.clearTimeout(t)
  }, [copied])
  return (
    <Button
      variant="ghost"
      size={label ? size : 'icon-xs'}
      onClick={async () => {
        if (await copyText(value)) setCopied(true)
        else toast.error('Could not copy to clipboard')
      }}
      aria-label={title ?? (label || 'Copy')}
      title={title ?? (label || 'Copy')}
    >
      {copied ? <Check className="size-3.5 text-success" /> : <Copy className="size-3.5" />}
      {label}
    </Button>
  )
}

/**
 * One CSV field: quoted when needed, and prefixed with ' when it starts like a spreadsheet formula (=, +, -, @, tab,
 * CR) — banners and remote data are attacker-controlled and must not execute when the export is opened in Excel.
 */
export function csvField(v: string): string {
  let s = v
  if (/^[=+\-@\t\r]/.test(s) && !/^-?\d+(\.\d+)?$/.test(s)) s = "'" + s
  if (/[",\n\r]/.test(s)) s = '"' + s.replace(/"/g, '""') + '"'
  return s
}

export function toCSV<T>(columns: Column<T>[], rows: T[]): string {
  const cols = columns.filter((c) => c.csv)
  const head = cols.map((c) => csvField(typeof c.header === 'string' ? c.header : c.key)).join(',')
  const body = rows.map((r) => cols.map((c) => csvField(c.csv!(r))).join(',')).join('\r\n')
  return head + '\r\n' + body + '\r\n'
}

export function downloadText(filename: string, text: string, type = 'text/plain'): void {
  try {
    const blob = new Blob([text], { type })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = filename.replace(/[\\/:*?"<>|]+/g, '_')
    document.body.appendChild(a)
    a.click()
    a.remove()
    window.setTimeout(() => URL.revokeObjectURL(url), 1000)
  } catch {
    toast.error('Download failed')
  }
}

export function ExportCsvButton<T>({ columns, rows, filename }: { columns: Column<T>[]; rows: T[]; filename: string }) {
  return (
    <Button variant="ghost" size="xs" disabled={rows.length === 0} onClick={() => downloadText(filename, toCSV(columns, rows), 'text/csv')} title="Export as CSV">
      <Download className="size-3.5" /> CSV
    </Button>
  )
}

// ---- badges & tiles --------------------------------------------------------------------------------------------------

const RATING_VARIANT: Record<string, React.ComponentProps<typeof Badge>['variant']> = {
  pq: 'success',
  ok: 'success',
  weak: 'warning',
  legacy: 'destructive',
  unknown: 'outline',
}

export function RatingBadge({ rating }: { rating: string }) {
  const label = rating === 'pq' ? 'quantum-safe' : rating === 'ok' ? 'secure' : rating
  return (
    <Badge variant={RATING_VARIANT[rating] ?? 'outline'} className="w-24 justify-center">
      {label}
    </Badge>
  )
}

export function StateBadge({ state }: { state: string }) {
  const variant: React.ComponentProps<typeof Badge>['variant'] =
    state === 'open' ? 'success' : state === 'closed' ? 'secondary' : state === 'error' ? 'destructive' : 'warning'
  return <Badge variant={variant}>{state}</Badge>
}

export function StatRow({ children }: { children: React.ReactNode }) {
  return <div className="flex shrink-0 flex-wrap gap-2 px-4 pt-3">{children}</div>
}

export function Stat({ label, value, tone, title }: { label: string; value: React.ReactNode; tone?: 'default' | 'success' | 'warning' | 'danger'; title?: string }) {
  const toneCls = tone === 'success' ? 'text-success' : tone === 'warning' ? 'text-warning' : tone === 'danger' ? 'text-destructive' : 'text-foreground'
  return (
    <div className="min-w-24 rounded-md border bg-card px-3 py-2" title={title}>
      <div className="text-2xs font-medium tracking-wide text-muted-foreground uppercase">{label}</div>
      <div className={cn('mt-0.5 text-md font-semibold tabular', toneCls)}>{value}</div>
    </div>
  )
}

export function EmptyResults({ hint, icon }: { hint: string; icon?: React.ComponentProps<typeof EmptyState>['icon'] }) {
  return <EmptyState size="sm" icon={icon} title="No results yet" description={hint} className="h-full" />
}

// ---- "run from" SSH host ---------------------------------------------------------------------------------------------

const LOCAL = '__local__'

/** Picks a saved SSH connection to run a tool from ('' = the NexTerm host itself). */
export function ViaConnectionField({
  value,
  onChange,
  label = 'Run from',
  hint = 'Run on a saved SSH host instead of the NexTerm server',
  localLabel = 'This host (NexTerm server)',
  required,
}: {
  value: string
  onChange: (v: string) => void
  label?: string
  hint?: string
  localLabel?: string
  required?: boolean
}) {
  const { data: conns, isLoading } = useConnections()
  const ssh = (conns ?? []).filter((c) => c.protocol === 'ssh' || c.protocol === 'sftp')
  // Radix Select reserves the empty string (it means "no selection"), hence a sentinel for the local host.
  const options = [
    ...(required ? [] : [{ value: LOCAL, label: localLabel }]),
    ...ssh.map((c) => ({ value: c.id, label: c.name || `${c.username ? c.username + '@' : ''}${c.host}` })),
  ]
  // A deleted / no longer visible connection falls back to the local host.
  const current = value && ssh.some((c) => c.id === value) ? value : required ? undefined : LOCAL
  if (required && options.length === 0) {
    return (
      <Field label={label} hint={hint}>
        <div className="flex h-8 items-center text-sm text-muted-foreground">{isLoading ? 'Loading connections…' : 'No saved SSH connections'}</div>
      </Field>
    )
  }
  return (
    <Field label={label} hint={hint}>
      <SimpleSelect value={current} onValueChange={(v) => onChange(v === LOCAL ? '' : v)} options={options} placeholder="Choose an SSH connection" aria-label={label} />
    </Field>
  )
}

// ---- recent runs -----------------------------------------------------------------------------------------------------

export function RecentRuns({ tool, onPick }: { tool: string; onPick: (run: ToolRun) => void }) {
  const runs = useToolHistory(tool)
  if (runs.length === 0) return null
  return (
    <div className="mt-1.5 flex max-h-12 flex-wrap items-center gap-1.5 overflow-hidden">
      <span className="text-xs text-muted-foreground">Recent:</span>
      {runs.slice(0, 8).map((r) => (
        <button
          key={r.id}
          type="button"
          onClick={() => onPick(r)}
          className="max-w-64 truncate rounded-full border bg-card px-2 py-0.5 text-xs text-foreground/80 outline-none transition hover:border-primary/40 hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/50"
          title={`${r.label} — ${new Date(r.at).toLocaleString()}`}
        >
          {r.label}
        </button>
      ))}
      <button type="button" onClick={() => clearHistory(tool)} className="rounded-sm text-xs text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50">
        clear
      </button>
    </div>
  )
}

// ---- misc ------------------------------------------------------------------------------------------------------------

export const str = (v: unknown): string => (v == null ? '' : String(v))
export const num = (v: unknown): number | undefined => (typeof v === 'number' && Number.isFinite(v) ? v : undefined)

/** Last row of a kind in a row list. */
export function lastOfKind(rows: ToolRow[], kind: string): ToolRow | undefined {
  for (let i = rows.length - 1; i >= 0; i--) if (rows[i].kind === kind) return rows[i]
  return undefined
}

/** Format bits per second (1 Gbit/s = 10^9). */
export function formatBits(bps: number | undefined): string {
  if (bps == null || !Number.isFinite(bps)) return '—'
  const units = ['bit/s', 'kbit/s', 'Mbit/s', 'Gbit/s', 'Tbit/s']
  let v = bps
  let i = 0
  while (v >= 1000 && i < units.length - 1) {
    v /= 1000
    i++
  }
  return `${v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2)} ${units[i]}`
}

/** Submit a form with Enter from any text input inside it (keyboard-first forms). */
export function onEnter(fn: () => void) {
  return (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && !e.nativeEvent.isComposing) {
      e.preventDefault()
      fn()
    }
  }
}
