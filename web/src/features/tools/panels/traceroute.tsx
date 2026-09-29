/*
 * Traceroute (TOOL-8): classic mode (probes per hop, TTL windows) and an mtr-style continuous mode with live per-hop
 * loss / latency statistics. Runs from the Termstead host or from a saved SSH host.
 */
import * as React from 'react'
import { Waypoints } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { CheckboxField } from '@/components/ui/checkbox'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { cn } from '@/lib/utils'
import {
  type Column,
  EmptyResults,
  ExportCsvButton,
  FormGrid,
  JobNotes,
  PanelLayout,
  RecentRuns,
  ResultArea,
  ResultsTable,
  RunControls,
  Stat,
  StatRow,
  ViaConnectionField,
  num,
  onEnter,
  str,
} from '../components'
import { useToolForm } from '../form'
import { addRun } from '../history'
import { asNumber, asString, usePrefill } from '../navigate'
import type { ToolRow } from '../types'
import { useRowsOfKind, useToolJob } from '../useToolJob'

const DEFAULTS = {
  host: '',
  mode: 'trace' as 'trace' | 'mtr',
  protocol: 'icmp' as 'icmp' | 'udp',
  maxHops: 30,
  probes: 3,
  timeoutMs: 2000,
  rounds: 0,
  intervalMs: 1000,
  resolve: true,
  ipv6: false,
  via: '',
  /** Mode of the last run (decides how results render). */
  runMode: 'trace' as 'trace' | 'mtr',
}

function froms(r: ToolRow): string {
  const f = r.from
  return Array.isArray(f) ? (f as string[]).join(', ') : str(f)
}

function rttClass(v: number | undefined): string {
  if (v === undefined) return 'text-muted-foreground'
  return v < 50 ? 'text-foreground' : v < 150 ? 'text-warning' : 'text-destructive'
}

const TRACE_COLUMNS: Column<ToolRow>[] = [
  { key: 'ttl', header: '#', className: 'w-10 tabular text-muted-foreground', cell: (r) => str(r.ttl), csv: (r) => str(r.ttl), sortValue: (r) => num(r.ttl) ?? 0 },
  {
    key: 'host',
    header: 'Host',
    className: 'min-w-40',
    cell: (r) => (r.timeout ? <span className="text-muted-foreground">* * *</span> : <span className="font-medium">{str(r.host) || froms(r)}</span>),
    csv: (r) => str(r.host) || froms(r) || '*',
  },
  { key: 'from', header: 'Address', className: 'font-mono', cell: (r) => froms(r), csv: (r) => froms(r) },
  {
    key: 'times',
    header: 'RTT',
    className: 'tabular whitespace-nowrap',
    cell: (r) => (
      <span className="inline-flex gap-3">
        {((r.timesMs as (number | null)[]) || []).map((v, i) => (
          <span key={i} className={cn('min-w-14', rttClass(v ?? undefined))}>{v == null ? '*' : `${v} ms`}</span>
        ))}
        {r.annotation ? <Badge variant="destructive">{str(r.annotation)}</Badge> : null}
      </span>
    ),
    csv: (r) => ((r.timesMs as (number | null)[]) || []).map((v) => (v == null ? '*' : String(v))).join(' ') + (r.annotation ? ` ${str(r.annotation)}` : ''),
  },
]

function LossCell({ loss }: { loss: number }) {
  return (
    <span className="inline-flex items-center gap-2">
      <span className={cn('w-12 text-right tabular', loss === 0 ? 'text-success' : loss < 20 ? 'text-warning' : 'text-destructive')}>{loss.toFixed(1)}%</span>
      <span className="h-1.5 w-12 overflow-hidden rounded-full bg-muted" aria-hidden>
        <span className={cn('block h-full', loss < 20 ? 'bg-warning' : 'bg-destructive')} style={{ width: `${Math.min(100, loss)}%` }} />
      </span>
    </span>
  )
}

const ms = (v: unknown) => (num(v) === undefined ? '—' : (v as number).toFixed(1))

const MTR_COLUMNS: Column<ToolRow>[] = [
  { key: 'ttl', header: '#', className: 'w-10 tabular text-muted-foreground', cell: (r) => str(r.ttl), csv: (r) => str(r.ttl), sortValue: (r) => num(r.ttl) ?? 0 },
  {
    key: 'host',
    header: 'Host',
    className: 'min-w-44',
    cell: (r) =>
      r.from ? (
        <span className="flex min-w-0 flex-col">
          <span className="truncate font-medium">{str(r.host) || str(r.from)}</span>
          {r.host || Array.isArray(r.hosts) ? (
            <span className="truncate font-mono text-xs text-muted-foreground">{Array.isArray(r.hosts) ? (r.hosts as string[]).join(', ') : str(r.from)}</span>
          ) : null}
        </span>
      ) : (
        <span className="text-muted-foreground">???</span>
      ),
    csv: (r) => (Array.isArray(r.hosts) ? (r.hosts as string[]).join(' ') : str(r.from) || '???'),
  },
  { key: 'loss', header: 'Loss', className: 'w-32', cell: (r) => <LossCell loss={num(r.loss) ?? 0} />, csv: (r) => str(r.loss), sortValue: (r) => num(r.loss) ?? 0 },
  { key: 'sent', header: 'Sent', className: 'w-14 text-right tabular', cell: (r) => str(r.sent), csv: (r) => str(r.sent) },
  { key: 'last', header: 'Last', className: 'w-16 text-right tabular', cell: (r) => ms(r.lastMs), csv: (r) => str(r.lastMs), sortValue: (r) => num(r.lastMs) ?? 1e9 },
  { key: 'avg', header: 'Avg', className: 'w-16 text-right tabular font-medium', cell: (r) => ms(r.avgMs), csv: (r) => str(r.avgMs), sortValue: (r) => num(r.avgMs) ?? 1e9 },
  { key: 'best', header: 'Best', className: 'w-16 text-right tabular', cell: (r) => ms(r.bestMs), csv: (r) => str(r.bestMs) },
  { key: 'worst', header: 'Wrst', className: 'w-16 text-right tabular', cell: (r) => ms(r.worstMs), csv: (r) => str(r.worstMs), sortValue: (r) => num(r.worstMs) ?? 1e9 },
  { key: 'stdev', header: 'StDev', className: 'w-16 text-right tabular', cell: (r) => ms(r.stdevMs), csv: (r) => str(r.stdevMs), sortValue: (r) => num(r.stdevMs) ?? 1e9 },
]

export default function TraceroutePanel() {
  const job = useToolJob('traceroute')
  const { state } = job
  const [f, set] = useToolForm('traceroute', DEFAULTS)
  usePrefill('traceroute', (p) => set({ host: asString(p.host), via: asString(p.viaConnectionId), mode: p.mode === 'mtr' ? 'mtr' : 'trace', maxHops: asNumber(p.maxHops, 30) }))

  const run = () => {
    const host = f.host.trim()
    if (!host || job.running) return
    const params: Record<string, unknown> = {
      host,
      mode: f.mode,
      protocol: f.protocol,
      maxHops: f.maxHops,
      timeoutMs: f.timeoutMs,
      resolveNames: f.resolve,
      ipv6: f.ipv6 || undefined,
      viaConnectionId: f.via || undefined,
    }
    if (f.mode === 'trace') params.probes = f.probes
    else Object.assign(params, { rounds: f.rounds || undefined, intervalMs: f.intervalMs })
    set({ runMode: f.mode })
    void job.run(params)
    addRun('traceroute', params, `${f.mode === 'mtr' ? 'mtr ' : ''}${host}`)
  }

  const hops = useRowsOfKind(state, 'hop')
  const round = state.latest.round
  const lastTtl = num(round?.lastTtl) ?? 0
  const mtrRows = React.useMemo(
    () =>
      Object.values(state.keyed.mtr ?? {})
        .filter((r) => (num(r.ttl) ?? 0) <= lastTtl)
        .sort((a, b) => (num(a.ttl) ?? 0) - (num(b.ttl) ?? 0)),
    [state.keyed.mtr, lastTtl],
  )
  const summary = state.latest.summary
  const info = state.latest.info
  const showMtr = state.status === 'idle' ? f.mode === 'mtr' : f.runMode === 'mtr'

  return (
    <PanelLayout
      title="Traceroute"
      description="Trace the path to a host hop by hop, or run it continuously (mtr) for live per-hop loss and latency."
      form={
        <>
          <FormGrid>
            <Field label="Host">
              <Input value={f.host} onChange={(e) => set({ host: e.target.value })} placeholder="example.com" onKeyDown={onEnter(run)} autoFocus spellCheck={false} />
            </Field>
            <Field label="Mode">
              <SegmentedControl
                value={f.mode}
                onValueChange={(v) => set({ mode: v, timeoutMs: v === 'mtr' ? 1000 : 2000 })}
                options={[
                  { value: 'trace', label: 'Trace' },
                  { value: 'mtr', label: 'Continuous (mtr)' },
                ]}
                aria-label="Traceroute mode"
              />
            </Field>
            <Field label="Probes" hint={f.via ? 'ICMP needs root on the SSH host (else UDP)' : undefined}>
              <SegmentedControl
                value={f.protocol}
                onValueChange={(v) => set({ protocol: v })}
                options={[
                  { value: 'icmp', label: 'ICMP echo' },
                  { value: 'udp', label: 'UDP' },
                ]}
                aria-label="Probe protocol"
              />
            </Field>
            <Field label="Max hops">
              <NumberInput value={f.maxHops} onChange={(v) => set({ maxHops: v ?? 30 })} min={1} max={64} />
            </Field>
            {f.mode === 'trace' ? (
              <Field label="Probes per hop">
                <NumberInput value={f.probes} onChange={(v) => set({ probes: v ?? 3 })} min={1} max={10} />
              </Field>
            ) : (
              <>
                <Field label="Rounds" hint="0 = until stopped (max 3600)">
                  <NumberInput value={f.rounds} onChange={(v) => set({ rounds: v ?? 0 })} min={0} max={3600} />
                </Field>
                <Field label="Interval">
                  <NumberInput value={f.intervalMs} onChange={(v) => set({ intervalMs: v ?? 1000 })} min={200} max={60000} step={100} unit="ms" />
                </Field>
              </>
            )}
            <Field label="Timeout">
              <NumberInput value={f.timeoutMs} onChange={(v) => set({ timeoutMs: v ?? 2000 })} min={200} max={15000} step={100} unit="ms" />
            </Field>
            <ViaConnectionField value={f.via} onChange={(v) => set({ via: v })} />
            <Field label="Options">
              <div className="flex flex-col gap-1.5">
                <CheckboxField label="Resolve host names" checked={f.resolve} onCheckedChange={(v) => set({ resolve: !!v })} />
                <CheckboxField label="Prefer IPv6" checked={f.ipv6} onCheckedChange={(v) => set({ ipv6: !!v })} />
              </div>
            </Field>
          </FormGrid>
        </>
      }
      controls={
        <>
          <RunControls
            state={state}
            onRun={run}
            onCancel={job.cancel}
            onClear={job.reset}
            disabled={!f.host.trim()}
            runLabel={f.mode === 'mtr' ? 'Start' : 'Trace'}
            count={showMtr ? num(round?.round) : hops.length}
            countLabel={showMtr ? 'rounds' : 'hops'}
            extra={
              showMtr ? (
                <ExportCsvButton columns={MTR_COLUMNS} rows={mtrRows} filename={`mtr-${f.host.trim() || 'host'}.csv`} />
              ) : (
                <ExportCsvButton columns={TRACE_COLUMNS} rows={hops} filename={`traceroute-${f.host.trim() || 'host'}.csv`} />
              )
            }
          />
          <RecentRuns tool="traceroute" onPick={(r) => set({ host: asString(r.params.host), mode: r.params.mode === 'mtr' ? 'mtr' : 'trace', via: asString(r.params.viaConnectionId) })} />
        </>
      }
    >
      <JobNotes state={state} />
      {info && state.status !== 'idle' && <div className="shrink-0 truncate px-4 pt-2 font-mono text-xs text-muted-foreground">{str(info.message)}</div>}
      {summary && (
        <StatRow>
          <Stat label="Destination" value={<span className="font-mono text-base">{str(summary.target)}</span>} />
          <Stat label="Reached" value={summary.reached ? 'Yes' : 'No'} tone={summary.reached ? 'success' : 'warning'} />
          {num(summary.hops) ? <Stat label="Hops" value={str(summary.hops)} /> : null}
          {summary.rounds != null && <Stat label="Rounds" value={str(summary.rounds)} />}
          {summary.engine ? <Stat label="Probing" value={<span className="text-base">{str(summary.engine)}</span>} /> : null}
        </StatRow>
      )}
      <ResultArea className="mt-3 border-t">
        {showMtr ? (
          <ResultsTable
            columns={MTR_COLUMNS}
            rows={mtrRows}
            rowKey={(r) => str(r.ttl)}
            label="mtr statistics"
            empty={<EmptyResults icon={Waypoints} hint={state.status === 'idle' ? 'Enter a host and press Start: every hop is probed once per interval.' : 'Waiting for the first round…'} />}
          />
        ) : (
          <ResultsTable
            columns={TRACE_COLUMNS}
            rows={hops}
            rowKey={(r, i) => `${str(r.ttl)}-${i}`}
            label="Hops"
            empty={<EmptyResults icon={Waypoints} hint={state.status === 'idle' ? 'Enter a host and press Trace (or Enter).' : 'Probing…'} />}
          />
        )}
      </ResultArea>
    </PanelLayout>
  )
}
