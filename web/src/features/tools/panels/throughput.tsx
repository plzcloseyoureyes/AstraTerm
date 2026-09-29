/*
 * TCP throughput (TOOL-8 "iperf client"). Two clearly separated modes:
 *   - iperf3 server: Termstead speaks the iperf3 protocol (TCP) to any `iperf3 -s` (port 5201).
 *   - SSH host: the throughput of an SSH channel to a saved connection (no server software needed).
 */
import * as React from 'react'
import { Gauge } from 'lucide-react'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { formatBytes } from '@/lib/utils'
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
  formatBits,
  num,
  onEnter,
  str,
} from '../components'
import { useToolForm } from '../form'
import { addRun } from '../history'
import { asNumber, asString, usePrefill } from '../navigate'
import type { ToolRow } from '../types'
import { useRowsOfKind, useToolJob } from '../useToolJob'

const DEFAULTS = { mode: 'iperf3' as 'iperf3' | 'ssh', host: '', port: 5201, via: '', durationSec: 10, parallel: 1, reverse: false }

const COLUMNS: Column<ToolRow>[] = [
  { key: 'interval', header: 'Interval', className: 'w-40 tabular', cell: (r) => `${(num(r.startSec) ?? 0).toFixed(1)} – ${(num(r.endSec) ?? 0).toFixed(1)} s`, csv: (r) => `${str(r.startSec)}-${str(r.endSec)}` },
  { key: 'bytes', header: 'Transfer', className: 'w-32 tabular', cell: (r) => formatBytes(num(r.bytes) ?? 0), csv: (r) => str(r.bytes) },
  { key: 'rate', header: 'Bitrate', className: 'tabular font-medium', cell: (r) => formatBits(num(r.bitsPerSec)), csv: (r) => str(Math.round(num(r.bitsPerSec) ?? 0)), sortValue: (r) => num(r.bitsPerSec) ?? 0 },
]

/** One bar per second. */
function RateChart({ intervals }: { intervals: ToolRow[] }) {
  if (intervals.length === 0) return null
  const rates = intervals.map((r) => num(r.bitsPerSec) ?? 0)
  const max = Math.max(1, ...rates)
  return (
    <figure className="mx-4 mt-3 shrink-0 rounded-md border bg-card px-3 py-2">
      <figcaption className="mb-1 flex justify-between text-2xs text-muted-foreground uppercase">
        <span>Bitrate per second</span>
        <span className="tabular">peak {formatBits(max)}</span>
      </figcaption>
      <div className="flex h-20 items-end gap-0.5" role="img" aria-label="Bitrate chart">
        {rates.map((v, i) => (
          <div key={i} className="min-w-1 flex-1 rounded-t-sm bg-primary/80" style={{ height: `${Math.max(2, (v / max) * 100)}%` }} title={`${i + 1}s: ${formatBits(v)}`} />
        ))}
      </div>
    </figure>
  )
}

export default function ThroughputPanel() {
  const job = useToolJob('throughput')
  const { state } = job
  const [f, set] = useToolForm('throughput', DEFAULTS)
  usePrefill('throughput', (p) => set({ host: asString(p.host), port: asNumber(p.port, 5201), via: asString(p.viaConnectionId), mode: p.viaConnectionId ? 'ssh' : 'iperf3' }))

  const ready = f.mode === 'iperf3' ? !!f.host.trim() : !!f.via
  const run = () => {
    if (!ready || job.running) return
    const params: Record<string, unknown> =
      f.mode === 'iperf3'
        ? { mode: 'iperf3', host: f.host.trim(), port: f.port, durationSec: f.durationSec, parallel: f.parallel, reverse: f.reverse || undefined }
        : { mode: 'ssh', viaConnectionId: f.via, durationSec: f.durationSec, parallel: f.parallel, reverse: f.reverse || undefined }
    void job.run(params)
    addRun('throughput', params, f.mode === 'iperf3' ? `iperf3 ${f.host.trim()}:${f.port}${f.reverse ? ' ↓' : ' ↑'}` : `SSH${f.reverse ? ' ↓' : ' ↑'}`)
  }

  const intervals = useRowsOfKind(state, 'interval')
  const summary = state.latest.summary

  return (
    <PanelLayout
      title="Throughput test"
      description={
        f.mode === 'iperf3'
          ? 'Measure TCP throughput against an iperf3 server (iperf3 -s, port 5201) — Termstead acts as the iperf3 client.'
          : 'Measure how fast data moves through an SSH channel to a saved host (no server software needed; includes SSH encryption cost).'
      }
      form={
        <>
          <FormGrid>
            <Field label="Measure against">
              <SegmentedControl
                value={f.mode}
                onValueChange={(v) => set({ mode: v, parallel: v === 'ssh' ? Math.min(f.parallel, 4) : f.parallel })}
                options={[
                  { value: 'iperf3', label: 'iperf3 server' },
                  { value: 'ssh', label: 'SSH host' },
                ]}
                aria-label="Measurement mode"
              />
            </Field>
            {f.mode === 'iperf3' ? (
              <>
                <Field label="iperf3 server">
                  <Input value={f.host} onChange={(e) => set({ host: e.target.value })} placeholder="iperf.example.net" onKeyDown={onEnter(run)} autoFocus spellCheck={false} />
                </Field>
                <Field label="Port">
                  <NumberInput value={f.port} onChange={(v) => set({ port: v ?? 5201 })} min={1} max={65535} />
                </Field>
              </>
            ) : (
              <ViaConnectionField value={f.via} onChange={(v) => set({ via: v })} label="SSH host" hint="The saved SSH connection to measure" required />
            )}
            <Field label="Direction">
              <SegmentedControl
                value={f.reverse ? 'down' : 'up'}
                onValueChange={(v) => set({ reverse: v === 'down' })}
                options={[
                  { value: 'up', label: 'Upload ↑' },
                  { value: 'down', label: 'Download ↓' },
                ]}
                aria-label="Direction"
              />
            </Field>
            <Field label="Duration">
              <NumberInput value={f.durationSec} onChange={(v) => set({ durationSec: v ?? 10 })} min={1} max={60} unit="s" />
            </Field>
            <Field label={f.mode === 'iperf3' ? 'Parallel streams' : 'Parallel channels'}>
              <NumberInput value={f.parallel} onChange={(v) => set({ parallel: v ?? 1 })} min={1} max={f.mode === 'iperf3' ? 8 : 4} />
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
            disabled={!ready}
            runLabel="Start test"
            extra={<ExportCsvButton columns={COLUMNS} rows={intervals} filename="throughput.csv" />}
          />
          <RecentRuns
            tool="throughput"
            onPick={(r) =>
              set({
                mode: r.params.mode === 'ssh' ? 'ssh' : 'iperf3',
                host: asString(r.params.host),
                port: asNumber(r.params.port, 5201),
                via: asString(r.params.viaConnectionId),
                reverse: r.params.reverse === true,
              })
            }
          />
        </>
      }
    >
      <JobNotes state={state} />
      {summary && (
        <StatRow>
          <Stat label={summary.reverse ? 'Download' : 'Upload'} value={formatBits(num(summary.receiverBitsPerSec))} tone="success" title="Measured at the receiver" />
          <Stat label="Sender" value={formatBits(num(summary.senderBitsPerSec))} />
          <Stat label="Transferred" value={formatBytes(num(summary.receiverBytes) ?? 0)} />
          <Stat label="Duration" value={`${(num(summary.durationSec) ?? 0).toFixed(1)} s`} />
          <Stat label={summary.mode === 'ssh' ? 'Channels' : 'Streams'} value={str(summary.streams)} />
          {summary.retransmits != null && <Stat label="Retransmits" value={str(summary.retransmits)} tone={Number(summary.retransmits) > 0 ? 'warning' : 'default'} />}
          {summary.congestion ? <Stat label="Congestion" value={str(summary.congestion)} /> : null}
        </StatRow>
      )}
      <RateChart intervals={intervals} />
      <ResultArea className={intervals.length ? 'mt-3 border-t' : undefined}>
        <ResultsTable
          columns={COLUMNS}
          rows={intervals}
          rowKey={(r, i) => `${str(r.startSec)}-${i}`}
          label="Intervals"
          empty={<EmptyResults icon={Gauge} hint={state.status === 'idle' ? (f.mode === 'iperf3' ? 'Enter an iperf3 server and press Start test.' : 'Pick an SSH host and press Start test.') : 'Connecting…'} />}
        />
      </ResultArea>
    </PanelLayout>
  )
}
