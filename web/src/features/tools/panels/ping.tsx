/*
 * Ping (TOOL-8): ICMP echo from the AstraTerm host (unprivileged where the OS allows it, TCP-connect fallback), a
 * TCP-connect "ping" to any port, or the remote host's own ping via a saved SSH connection. Latency graph + log.
 */
import * as React from 'react'
import { Activity } from 'lucide-react'
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
import { asBool, asNumber, asString, usePrefill } from '../navigate'
import type { ToolRow } from '../types'
import { useRowsOfKind, useToolJob } from '../useToolJob'

const DEFAULTS = { host: '', mode: 'icmp' as 'icmp' | 'tcp', port: 443, count: 10, intervalMs: 1000, timeoutMs: 2000, size: 56, ipv6: false, via: '' }

/** Latency over time: a line through the replies, red ticks for lost probes (last 300 samples). */
export function LatencyChart({ replies }: { replies: ToolRow[] }) {
  const samples = replies.slice(-300)
  const rtts = samples.map((r) => num(r.rttMs))
  const vals = rtts.filter((v): v is number => v !== undefined)
  if (samples.length < 2) return null
  const max = Math.max(1, ...vals) * 1.1
  const w = 600
  const h = 72
  const x = (i: number) => (i / (samples.length - 1)) * w
  const y = (v: number) => h - (v / max) * (h - 6) - 3
  let path = ''
  rtts.forEach((v, i) => {
    if (v === undefined) return
    path += `${path && rtts[i - 1] !== undefined ? 'L' : 'M'}${x(i).toFixed(1)},${y(v).toFixed(1)}`
  })
  return (
    <figure className="mx-4 mt-3 shrink-0 rounded-md border bg-card px-3 py-2">
      <figcaption className="mb-1 flex justify-between text-2xs text-muted-foreground uppercase">
        <span>Latency (last {samples.length})</span>
        <span className="tabular">max {Math.max(...vals, 0).toFixed(1)} ms</span>
      </figcaption>
      <svg viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" className="h-16 w-full" role="img" aria-label="Latency graph">
        <line x1="0" x2={w} y1={h - 3} y2={h - 3} stroke="var(--color-border)" strokeWidth="1" vectorEffect="non-scaling-stroke" />
        {rtts.map((v, i) => (v === undefined ? <line key={i} x1={x(i)} x2={x(i)} y1={0} y2={h} stroke="var(--color-destructive)" strokeOpacity="0.55" strokeWidth="1.5" vectorEffect="non-scaling-stroke" /> : null))}
        <path d={path} fill="none" stroke="var(--color-primary)" strokeWidth="1.75" vectorEffect="non-scaling-stroke" strokeLinejoin="round" />
      </svg>
    </figure>
  )
}

const COLUMNS: Column<ToolRow>[] = [
  { key: 'seq', header: 'Seq', className: 'w-16 tabular', cell: (r) => str(r.seq), csv: (r) => str(r.seq), sortValue: (r) => num(r.seq) ?? 0 },
  { key: 'from', header: 'From', className: 'font-mono', cell: (r) => str(r.from), csv: (r) => str(r.from) },
  { key: 'ttl', header: 'TTL', className: 'w-16 tabular', cell: (r) => str(r.ttl), csv: (r) => str(r.ttl) },
  {
    key: 'time',
    header: 'Time',
    className: 'w-28 tabular',
    cell: (r) => (r.error ? <span className="text-warning">{str(r.error)}</span> : <span>{str(r.rttMs)} ms{r.duplicate ? ' (dup)' : ''}</span>),
    csv: (r) => (r.error ? str(r.error) : str(r.rttMs)),
    sortValue: (r) => num(r.rttMs) ?? Number.MAX_VALUE,
  },
]

export default function PingPanel() {
  const job = useToolJob('ping')
  const { state } = job
  const [f, set] = useToolForm('ping', DEFAULTS)
  usePrefill('ping', (p) => set({ host: asString(p.host), via: asString(p.viaConnectionId), mode: asString(p.mode, 'icmp') === 'tcp' ? 'tcp' : 'icmp', port: asNumber(p.port, 443), ipv6: asBool(p.ipv6, false) }))
  const remote = !!f.via
  const mode = remote ? 'icmp' : f.mode

  const run = () => {
    const host = f.host.trim()
    if (!host || job.running) return
    const params: Record<string, unknown> = { host, count: f.count, intervalMs: f.intervalMs, timeoutMs: f.timeoutMs, mode, ipv6: f.ipv6 || undefined }
    if (mode === 'tcp') params.port = f.port
    if (mode === 'icmp' && f.size !== 56) params.size = f.size
    if (remote) params.viaConnectionId = f.via
    void job.run(params)
    addRun('ping', params, mode === 'tcp' ? `${host}:${f.port}` : host)
  }

  const replies = useRowsOfKind(state, 'reply')
  const infos = useRowsOfKind(state, 'info')
  const summary = state.latest.summary
  const lossTone = (v: number) => (v === 0 ? 'success' : v < 100 ? 'warning' : 'danger')

  return (
    <PanelLayout
      title="Ping"
      description="ICMP echo (TCP-connect fallback where ICMP is unavailable), a TCP-connect ping to any port, or ping from a saved SSH host."
      form={
        <>
          <FormGrid>
            <Field label="Host">
              <Input value={f.host} onChange={(e) => set({ host: e.target.value })} placeholder="example.com or 10.0.0.1" onKeyDown={onEnter(run)} autoFocus spellCheck={false} />
            </Field>
            <Field label="Method" hint={remote ? 'Remote hosts use their own ping (ICMP)' : undefined}>
              <SegmentedControl
                value={mode}
                onValueChange={(v) => set({ mode: v })}
                options={[
                  { value: 'icmp', label: 'ICMP' },
                  { value: 'tcp', label: 'TCP connect', disabled: remote },
                ]}
                aria-label="Ping method"
              />
            </Field>
            {mode === 'tcp' ? (
              <Field label="TCP port">
                <NumberInput value={f.port} onChange={(v) => set({ port: v ?? 443 })} min={1} max={65535} />
              </Field>
            ) : (
              <Field label="Payload size">
                <NumberInput value={f.size} onChange={(v) => set({ size: v ?? 56 })} min={24} max={65000} unit="B" />
              </Field>
            )}
            <Field label="Count">
              <NumberInput value={f.count} onChange={(v) => set({ count: v ?? 10 })} min={1} max={1000} />
            </Field>
            <Field label="Interval">
              <NumberInput value={f.intervalMs} onChange={(v) => set({ intervalMs: v ?? 1000 })} min={remote ? 200 : 100} max={60000} step={100} unit="ms" />
            </Field>
            <Field label="Timeout">
              <NumberInput value={f.timeoutMs} onChange={(v) => set({ timeoutMs: v ?? 2000 })} min={100} max={60000} step={100} unit="ms" />
            </Field>
            <ViaConnectionField value={f.via} onChange={(v) => set({ via: v })} />
            <Field label="Options">
              <CheckboxField label="Prefer IPv6" checked={f.ipv6} onCheckedChange={(v) => set({ ipv6: !!v })} />
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
            runLabel="Ping"
            count={replies.length}
            countLabel="replies"
            extra={<ExportCsvButton columns={COLUMNS} rows={replies} filename={`ping-${f.host.trim() || 'host'}.csv`} />}
          />
          <RecentRuns
            tool="ping"
            onPick={(r) => set({ host: asString(r.params.host), mode: r.params.mode === 'tcp' ? 'tcp' : 'icmp', port: asNumber(r.params.port, f.port), via: asString(r.params.viaConnectionId) })}
          />
        </>
      }
    >
      <JobNotes state={state} />
      {infos.length > 0 && (
        <div className="shrink-0 px-4 pt-2 font-mono text-xs text-muted-foreground">
          {infos.slice(-3).map((r, i) => (
            <div key={i} className="truncate">{str(r.message)}</div>
          ))}
        </div>
      )}
      {summary && (
        <StatRow>
          <Stat label="Sent" value={str(summary.sent)} />
          <Stat label="Received" value={str(summary.recv)} tone={Number(summary.recv) > 0 ? 'success' : 'danger'} />
          <Stat label="Loss" value={`${summary.loss ?? 0}%`} tone={lossTone(Number(summary.loss ?? 0))} />
          {summary.avgMs != null && (
            <Stat
              label="RTT min / avg / max"
              value={`${summary.minMs} / ${summary.avgMs} / ${summary.maxMs} ms`}
              title={summary.stddevMs != null ? `Standard deviation ${summary.stddevMs} ms` : undefined}
            />
          )}
        </StatRow>
      )}
      <LatencyChart replies={replies} />
      <ResultArea className={cn(replies.length > 0 && 'mt-3 border-t')}>
        <ResultsTable
          columns={COLUMNS}
          rows={replies}
          rowKey={(r, i) => `${str(r.seq)}-${i}`}
          rowClassName={(r) => (r.error ? 'text-warning' : undefined)}
          label="Ping replies"
          empty={state.status === 'idle' ? <EmptyResults icon={Activity} hint="Enter a host and press Ping (or Enter)." /> : undefined}
        />
      </ResultArea>
    </PanelLayout>
  )
}
