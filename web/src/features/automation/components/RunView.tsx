/*
 * A run of the history (script or batch): live log / per-host results from job events while it runs, the stored
 * record afterwards. Cancel, copy the log, export batch results as CSV.
 */
import * as React from 'react'
import { ChevronRight, Copy, Download, Square } from 'lucide-react'
import { toast } from 'sonner'
import { cancelJob } from '@/api/jobs'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { ErrorState } from '@/components/ui/query-state'
import { LoadingPane } from '@/components/ui/spinner'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { useLoadingGate } from '@/lib/useDelayedFlag'
import { cn, copyText, errorMessage, formatDateTime, formatDuration } from '@/lib/utils'
import { useRun, watchJobEvents } from '../api'
import type { BatchEvent, HostResult, Run, ScriptLogEvent } from '../types'
import { StatusBadge } from './pickers'

interface LogLine {
  ts: string
  level: string
  text: string
  host?: string
}

const ORIGIN: Record<Run['origin'], string> = { manual: 'Started manually', schedule: 'Scheduled', trigger: 'By a trigger' }

function levelClass(level: string): string {
  switch (level) {
    case 'error':
      return 'text-destructive'
    case 'warn':
    case 'warning':
      return 'text-warning'
    case 'debug':
      return 'text-muted-foreground'
    default:
      return ''
  }
}

function parseStoredLog(log: string): LogLine[] {
  return log
    .split('\n')
    .filter(Boolean)
    .map((l) => {
      const m = /^(\d\d:\d\d:\d\d\.\d{3}) (?:(ERROR|WARN|DEBUG) )?(?:\[([^\]]+)\] )?(.*)$/.exec(l)
      if (!m) return { ts: '', level: 'info', text: l }
      return { ts: m[1], level: (m[2] ?? 'info').toLowerCase(), host: m[3], text: m[4] }
    })
}

function csvCell(v: unknown): string {
  const s = v == null ? '' : String(v)
  return /[",\n\r]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s
}

export function exportResultsCsv(run: Pick<Run, 'name' | 'startedAt'>, results: HostResult[]): void {
  const head = ['name', 'host', 'status', 'mode', 'exitCode', 'durationMs', 'error', 'output']
  const rows = results.map((r) => [r.name, r.host, r.status, r.mode, r.exitCode, r.durationMs, r.error, r.output].map(csvCell).join(','))
  const blob = new Blob([[head.join(','), ...rows].join('\r\n') + '\r\n'], { type: 'text/csv;charset=utf-8' })
  const a = document.createElement('a')
  a.href = URL.createObjectURL(blob)
  a.download = `${run.name.replace(/[^\w.-]+/g, '_').slice(0, 60) || 'batch'}-${run.startedAt.slice(0, 19).replace(/[:T]/g, '-')}.csv`
  a.click()
  setTimeout(() => URL.revokeObjectURL(a.href), 1000)
}

function ResultsTable({ results }: { results: HostResult[] }) {
  const [open, setOpen] = React.useState<Set<string>>(new Set())
  return (
    <Table containerClassName="rounded-md border">
      <TableHeader>
        <TableRow>
          <TableHead className="w-8" />
          <TableHead>Connection</TableHead>
          <TableHead className="w-28">Status</TableHead>
          <TableHead className="w-20">Mode</TableHead>
          <TableHead className="w-16 text-right">Exit</TableHead>
          <TableHead className="w-20 text-right">Time</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {results.map((r) => {
          const expanded = open.has(r.connectionId)
          const hasDetail = !!(r.output || r.error)
          const toggle = () =>
            setOpen((prev) => {
              const n = new Set(prev)
              if (n.has(r.connectionId)) n.delete(r.connectionId)
              else n.add(r.connectionId)
              return n
            })
          return (
            <React.Fragment key={r.connectionId}>
              <TableRow className={cn(hasDetail && 'cursor-pointer')} onClick={() => hasDetail && toggle()}>
                <TableCell>
                  {hasDetail && (
                    <button
                      type="button"
                      aria-expanded={expanded}
                      aria-label={`Show output of ${r.name}`}
                      className="flex items-center rounded text-muted-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring/60"
                      onClick={(e) => {
                        e.stopPropagation()
                        toggle()
                      }}
                    >
                      <ChevronRight className={cn('size-3.5 transition-transform', expanded && 'rotate-90')} />
                    </button>
                  )}
                </TableCell>
                <TableCell>
                  <div className="flex flex-col">
                    <span className="font-medium">{r.name}</span>
                    {r.host && <span className="text-xs text-muted-foreground">{r.host}</span>}
                  </div>
                </TableCell>
                <TableCell>
                  <StatusBadge status={r.status} />
                </TableCell>
                <TableCell className="text-xs text-muted-foreground">{r.mode ?? ''}</TableCell>
                <TableCell className="text-right tabular">{r.exitCode ?? ''}</TableCell>
                <TableCell className="text-right text-xs tabular text-muted-foreground">{r.durationMs ? formatDuration(r.durationMs) : ''}</TableCell>
              </TableRow>
              {expanded && (
                <TableRow className="hover:bg-transparent">
                  <TableCell />
                  <TableCell colSpan={5}>
                    {r.error && <p className="mb-1 text-sm text-destructive">{r.error}</p>}
                    {r.output && <pre className="max-h-72 overflow-auto rounded-md bg-muted/50 p-2 font-mono text-xs whitespace-pre-wrap">{r.output}</pre>}
                  </TableCell>
                </TableRow>
              )}
            </React.Fragment>
          )
        })}
      </TableBody>
    </Table>
  )
}

function LogView({ lines }: { lines: LogLine[] }) {
  const ref = React.useRef<HTMLDivElement>(null)
  const stick = React.useRef(true)
  React.useEffect(() => {
    const el = ref.current
    if (el && stick.current) el.scrollTop = el.scrollHeight
  }, [lines])
  return (
    <div
      ref={ref}
      onScroll={(e) => {
        const el = e.currentTarget
        stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24
      }}
      className="min-h-24 flex-1 overflow-auto rounded-md border bg-muted/30 p-2 font-mono text-xs"
      role="log"
      aria-live="polite"
    >
      {!lines.length ? (
        <span className="text-muted-foreground">No output.</span>
      ) : (
        lines.map((l, i) => (
          <div key={i} className={cn('whitespace-pre-wrap break-words', levelClass(l.level))}>
            {l.ts && <span className="mr-2 text-muted-foreground select-none">{l.ts.length > 12 ? new Date(l.ts).toLocaleTimeString() : l.ts}</span>}
            {l.host && <span className="mr-1.5 text-primary">[{l.host}]</span>}
            {l.text}
          </div>
        ))
      )}
    </div>
  )
}

/** Live + stored view of a run. */
export function RunView({ runId, jobId, className }: { runId?: string; jobId?: string; className?: string }) {
  const { data: run, isLoading, error, refetch } = useRun(runId)
  const gate = useLoadingGate(isLoading)
  const effectiveJob = jobId ?? run?.jobId
  const running = !run || run.status === 'running'
  const [live, setLive] = React.useState<LogLine[]>([])
  const [hosts, setHosts] = React.useState<Map<string, HostResult>>(new Map())
  React.useEffect(() => {
    setLive([])
    setHosts(new Map())
  }, [runId])
  React.useEffect(() => {
    if (!effectiveJob || !running) return
    return watchJobEvents(effectiveJob, (ev) => {
      if (ev.event !== 'data' || !ev.data) return
      const d = ev.data as ScriptLogEvent | BatchEvent
      if (d.kind === 'log') setLive((ls) => [...ls.slice(-4999), { ts: d.ts, level: d.level ?? 'info', text: d.text ?? '', host: (d as BatchEvent).host }])
      else if (d.kind === 'host' && (d as BatchEvent).result) {
        const r = (d as BatchEvent).result!
        setHosts((m) => new Map(m).set(r.connectionId, r))
      }
    })
  }, [effectiveJob, running])

  if (!runId) return <EmptyState size="sm" title="No run selected" />
  if (gate.hold) return <LoadingPane active={gate.show} immediate />
  if (error || !run) return <ErrorState error={error ?? new Error('The run was not found.')} title="Could not load the run" onRetry={() => void refetch()} />

  const stored = run.log ? parseStoredLog(run.log) : []
  const lines = running ? (live.length ? live : stored) : stored
  const results: HostResult[] = running && hosts.size ? mergeResults(run.results ?? [], hosts) : (run.results ?? [])
  const duration = run.finishedAt ? Date.parse(run.finishedAt) - Date.parse(run.startedAt) : Date.now() - Date.parse(run.startedAt)

  return (
    <div className={cn('flex min-h-0 flex-col gap-2', className)}>
      <div className="flex items-start gap-2">
        <div className="min-w-0 flex-1">
          <div className="flex min-w-0 items-center gap-2">
            <StatusBadge status={run.status} />
            <span className="truncate font-medium">{run.name}</span>
          </div>
          <div className="mt-0.5 truncate text-xs text-muted-foreground tabular-nums">
            {[
              run.target,
              ORIGIN[run.origin],
              formatDateTime(run.startedAt),
              formatDuration(duration),
              run.kind === 'batch' && run.summary.total > 0
                ? `${run.summary.ok} ok, ${run.summary.failed} failed${run.summary.skipped ? `, ${run.summary.skipped} skipped` : ''}`
                : '',
            ]
              .filter(Boolean)
              .join(' · ')}
          </div>
        </div>
        <span className="flex shrink-0 gap-1.5">
          {running && effectiveJob && (
            <Button size="xs" variant="secondary" onClick={() => void cancelJob(effectiveJob).catch((err) => toast.error('Could not stop', { description: errorMessage(err) }))}>
              <Square className="fill-current" /> Stop
            </Button>
          )}
          {lines.length > 0 && (
            <Button
              size="xs"
              variant="ghost"
              onClick={() => void copyText(lines.map((l) => `${l.host ? `[${l.host}] ` : ''}${l.text}`).join('\n')).then((ok) => ok && toast.success('Log copied'))}
            >
              <Copy /> Copy log
            </Button>
          )}
          {run.kind === 'batch' && results.length > 0 && (
            <Button size="xs" variant="ghost" onClick={() => exportResultsCsv(run, results)}>
              <Download /> CSV
            </Button>
          )}
        </span>
      </div>
      {run.error && <p className="rounded-md border border-destructive/30 bg-destructive/8 px-2.5 py-1.5 text-sm text-destructive">{run.error}</p>}
      {run.kind === 'batch' && results.length > 0 && <ResultsTable results={results} />}
      {(run.kind === 'script' || lines.length > 0) && <LogView lines={lines} />}
    </div>
  )
}

function mergeResults(base: HostResult[], live: Map<string, HostResult>): HostResult[] {
  const out = base.map((r) => live.get(r.connectionId) ?? r)
  for (const [id, r] of live) if (!base.some((b) => b.connectionId === id)) out.push(r)
  return out
}
