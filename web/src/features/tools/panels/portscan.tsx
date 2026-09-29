/*
 * Port scanner (TOOL-5): TCP connect (or UDP) scan of hosts / lists / CIDRs / ranges, banner grab, service names,
 * optionally through an SSH gateway. Open ports get one-click connect actions.
 */
import * as React from 'react'
import { Radar } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { CheckboxField } from '@/components/ui/checkbox'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { SimpleSelect } from '@/components/ui/select'
import { HostActionsMenu } from '../actions'
import {
  type Column,
  EmptyResults,
  ExportCsvButton,
  FormGrid,
  JobNotes,
  PanelLayout,
  ProgressBar,
  RecentRuns,
  ResultArea,
  ResultsTable,
  RunControls,
  Stat,
  StatRow,
  StateBadge,
  ViaConnectionField,
  num,
  onEnter,
  str,
} from '../components'
import { useToolForm } from '../form'
import { addRun } from '../history'
import { asBool, asString, usePrefill } from '../navigate'
import type { ToolRow } from '../types'
import { useRowsOfKind, useToolJob } from '../useToolJob'

const PORT_PRESETS = [
  { value: 'top100', label: 'Top 100 (common services)' },
  { value: '22,23,3389,5900,5985,5986', label: 'Remote access' },
  { value: '80,443,8000,8008,8080,8081,8443,8888,9443', label: 'Web' },
  { value: '1433,1521,3306,5432,6379,9042,9200,11211,27017', label: 'Databases' },
  { value: '1-1024', label: 'Well-known (1–1024)' },
  { value: 'all', label: 'All 65535 ports' },
]

const UDP_DEFAULT = '53,67,69,123,137,161,500,514,1900,5353'

const DEFAULTS = { targets: '', ports: 'top100', concurrency: 200, timeoutMs: 800, banner: true, udp: false, showClosed: false, via: '' }

const COLUMNS: Column<ToolRow>[] = [
  { key: 'host', header: 'Host', className: 'font-mono', cell: (r) => str(r.host === r.ip ? r.ip : `${str(r.host)} (${str(r.ip)})`), csv: (r) => str(r.ip || r.host), sortValue: (r) => str(r.ip) },
  { key: 'port', header: 'Port', className: 'w-16 tabular', cell: (r) => str(r.port), csv: (r) => str(r.port), sortValue: (r) => num(r.port) ?? 0 },
  { key: 'proto', header: 'Proto', className: 'w-14 text-muted-foreground', cell: (r) => str(r.proto), csv: (r) => str(r.proto) },
  { key: 'state', header: 'State', className: 'w-28', cell: (r) => <StateBadge state={str(r.state)} />, csv: (r) => str(r.state), sortValue: (r) => str(r.state) },
  { key: 'service', header: 'Service', className: 'w-32', cell: (r) => str(r.service), csv: (r) => str(r.service), sortValue: (r) => str(r.service) },
  {
    key: 'banner',
    header: 'Banner',
    className: 'max-w-0 w-full font-mono text-sm',
    cell: (r) => (
      <span className="block truncate" title={str(r.banner || r.error)}>
        {str(r.banner || r.error)}
      </span>
    ),
    csv: (r) => str(r.banner || r.error),
  },
  {
    key: 'actions',
    header: <span className="sr-only">Actions</span>,
    className: 'w-10 text-right',
    cell: (r) => (r.state === 'open' && r.proto === 'tcp' ? <HostActionsMenu host={str(r.ip || r.host)} ports={[num(r.port) ?? 0]} /> : null),
  },
]

export default function PortScanPanel() {
  const job = useToolJob('portscan')
  const { state } = job
  const [f, set] = useToolForm('portscan', DEFAULTS)
  usePrefill('portscan', (p) => set({ targets: asString(p.targets), ports: asString(p.ports, f.ports), via: asString(p.viaConnectionId), udp: asBool(p.udp, false) }))

  const run = () => {
    const targets = f.targets.trim()
    if (!targets || job.running) return
    const params: Record<string, unknown> = {
      targets,
      ports: f.ports.trim() || 'top100',
      concurrency: f.concurrency,
      timeoutMs: f.timeoutMs,
      banner: f.banner && !f.udp,
      udp: f.udp || undefined,
      showClosed: f.showClosed || undefined,
      viaConnectionId: f.via || undefined,
    }
    void job.run(params)
    addRun('portscan', params, `${targets} (${params.ports})`)
  }

  const ports = useRowsOfKind(state, 'port')
  const hostErrors = useRowsOfKind(state, 'hosterror')
  const open = React.useMemo(() => ports.filter((r) => r.state === 'open').length, [ports])
  const progress = state.latest.progress
  const summary = state.latest.summary
  const running = job.running

  return (
    <PanelLayout
      title="Port scanner"
      description={
        <>
          TCP connect (or UDP) scan with service names and banner grab. Targets: host, list, CIDR or range (<code className="rounded bg-muted px-1">10.0.0.1-40</code>);
          ports: <code className="rounded bg-muted px-1">22,80,1000-2000,top100,all</code>.
        </>
      }
      form={
        <>
          <FormGrid>
            <Field label="Targets" hint="Up to 8192 hosts and 262,144 probes per scan" className="@lg:col-span-2">
              <Input value={f.targets} onChange={(e) => set({ targets: e.target.value })} placeholder="10.0.0.0/24, host1, 10.0.1.5-40" onKeyDown={onEnter(run)} autoFocus spellCheck={false} />
            </Field>
            <Field
              label="Ports"
              labelAside={
                <SimpleSelect
                  size="sm"
                  value={PORT_PRESETS.some((p) => p.value === f.ports) ? f.ports : undefined}
                  onValueChange={(v) => set({ ports: v })}
                  options={PORT_PRESETS}
                  placeholder="Presets"
                  aria-label="Port presets"
                  className="h-6 w-28 text-xs"
                />
              }
            >
              <Input value={f.ports} onChange={(e) => set({ ports: e.target.value })} placeholder="top100" onKeyDown={onEnter(run)} spellCheck={false} className="font-mono" />
            </Field>
            <Field label="Concurrency">
              <NumberInput value={f.concurrency} onChange={(v) => set({ concurrency: v ?? 200 })} min={1} max={1024} />
            </Field>
            <Field label="Timeout">
              <NumberInput value={f.timeoutMs} onChange={(v) => set({ timeoutMs: v ?? 800 })} min={50} max={15000} step={50} unit="ms" />
            </Field>
            <ViaConnectionField value={f.via} onChange={(v) => set({ via: v, udp: v ? false : f.udp })} label="Scan from" hint="Scan through a saved SSH host (TCP only)" />
            <Field label="Options">
              <div className="flex flex-col gap-1.5">
                <CheckboxField label="Grab banners" checked={f.banner && !f.udp} disabled={f.udp} onCheckedChange={(v) => set({ banner: !!v })} />
                <CheckboxField
                  label="UDP instead of TCP"
                  checked={f.udp}
                  disabled={!!f.via}
                  onCheckedChange={(v) => set({ udp: !!v, ports: v && f.ports === 'top100' ? UDP_DEFAULT : !v && f.ports === UDP_DEFAULT ? 'top100' : f.ports })}
                />
                <CheckboxField label="List closed / filtered ports" checked={f.showClosed} onCheckedChange={(v) => set({ showClosed: !!v })} />
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
            disabled={!f.targets.trim()}
            runLabel="Scan"
            count={open}
            countLabel="open"
            extra={<ExportCsvButton columns={COLUMNS} rows={ports} filename="portscan.csv" />}
          />
          <RecentRuns tool="portscan" onPick={(r) => set({ targets: asString(r.params.targets), ports: asString(r.params.ports, 'top100'), udp: r.params.udp === true, via: asString(r.params.viaConnectionId) })} />
        </>
      }
    >
      <JobNotes state={state} />
      {progress && (running || !summary) && <ProgressBar done={num(progress.done) ?? 0} total={num(progress.total) ?? 0} label={`${str(progress.open)} open`} />}
      {summary && (
        <StatRow>
          <Stat label="Open" value={str(summary.open)} tone={Number(summary.open) > 0 ? 'success' : 'default'} />
          <Stat label="Hosts" value={str(summary.hosts)} />
          <Stat label="Ports" value={str(summary.ports)} />
          <Stat label="Probes" value={Number(summary.probes).toLocaleString()} />
        </StatRow>
      )}
      {hostErrors.length > 0 && (
        <div className="flex shrink-0 flex-wrap gap-1.5 px-4 pt-3">
          {hostErrors.slice(0, 20).map((r, i) => (
            <Badge key={i} variant="warning" title={str(r.error)}>
              {str(r.host)}: unresolved
            </Badge>
          ))}
        </div>
      )}
      <ResultArea className="mt-3 border-t">
        <ResultsTable
          columns={COLUMNS}
          rows={ports}
          rowKey={(r, i) => `${str(r.ip)}-${str(r.port)}-${i}`}
          label="Ports"
          empty={<EmptyResults icon={Radar} hint={state.status === 'idle' ? 'Enter targets and press Scan. Only open ports are listed unless you ask for closed ones.' : running ? 'Scanning…' : 'No open ports found.'} />}
        />
      </ResultArea>
    </PanelLayout>
  )
}
