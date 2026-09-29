/*
 * Network scanner (TOOL-4): sweep a subnet / range / list for live hosts (ICMP sweep + TCP probes), with reverse DNS,
 * NetBIOS and mDNS names, MAC + vendor from the ARP cache, open service ports and one-click session actions.
 */
import * as React from 'react'
import { useQuery } from '@tanstack/react-query'
import { Network } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { useIsAdmin, useRunMode } from '@/stores/auth'
import { HostActionsMenu } from '../actions'
import { getInterfaces } from '../api'
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
  ViaConnectionField,
  num,
  onEnter,
  str,
} from '../components'
import { useToolForm } from '../form'
import { addRun } from '../history'
import { asString, usePrefill } from '../navigate'
import { calcSubnet } from '../subnet'
import type { ToolRow } from '../types'
import { useRowsOfKind, useToolJob } from '../useToolJob'

const DEFAULTS = { targets: '', ports: '', timeoutMs: 600, concurrency: 256, icmp: true, names: true, allHosts: false, via: '' }

function ipSortKey(ip: string): string {
  const p = ip.split('.')
  return p.length === 4 ? p.map((x) => x.padStart(3, '0')).join('.') : ip
}

function names(r: ToolRow): string[] {
  const out: string[] = []
  for (const k of ['hostname', 'netbios', 'mdns']) {
    const v = str(r[k])
    if (v && !out.some((o) => o.toLowerCase() === v.toLowerCase())) out.push(v)
  }
  return out
}

const COLUMNS: Column<ToolRow>[] = [
  { key: 'ip', header: 'Address', className: 'w-36 font-mono', cell: (r) => str(r.ip), csv: (r) => str(r.ip), sortValue: (r) => ipSortKey(str(r.ip)) },
  {
    key: 'name',
    header: 'Name',
    className: 'min-w-40',
    cell: (r) => {
      const n = names(r)
      return (
        <span className="flex min-w-0 flex-col">
          <span className="truncate">{n[0] ?? (r.target ? str(r.target) : '')}</span>
          {(n.length > 1 || !!r.workgroup) && (
            <span className="truncate text-xs text-muted-foreground">
              {n.slice(1).join(' · ')}
              {r.workgroup ? `${n.length > 1 ? ' · ' : ''}${str(r.workgroup)}` : ''}
            </span>
          )}
        </span>
      )
    },
    csv: (r) => names(r).join(' / '),
    sortValue: (r) => names(r)[0] ?? '~',
  },
  { key: 'mac', header: 'MAC', className: 'w-36 font-mono text-sm', cell: (r) => str(r.mac || r.netbiosMac), csv: (r) => str(r.mac || r.netbiosMac) },
  { key: 'vendor', header: 'Vendor', className: 'w-36', cell: (r) => <span className="block truncate">{str(r.vendor)}</span>, csv: (r) => str(r.vendor), sortValue: (r) => str(r.vendor) || '~' },
  { key: 'rtt', header: 'Ping', className: 'w-20 tabular', cell: (r) => (r.rttMs != null ? `${r.rttMs} ms` : r.alive ? '' : <span className="text-muted-foreground">down</span>), csv: (r) => str(r.rttMs), sortValue: (r) => num(r.rttMs) ?? 1e9 },
  {
    key: 'ports',
    header: 'Open ports',
    cell: (r) => (
      <div className="flex flex-wrap gap-1">
        {((r.ports as number[]) || []).map((p) => (
          <Badge key={p} variant="secondary" className="tabular">
            {p}
          </Badge>
        ))}
      </div>
    ),
    csv: (r) => ((r.ports as number[]) || []).join(' '),
    sortValue: (r) => ((r.ports as number[]) || []).length,
  },
  {
    key: 'actions',
    header: <span className="sr-only">Actions</span>,
    className: 'w-10 text-right',
    cell: (r) => (r.alive ? <HostActionsMenu host={str(r.ip)} ports={(r.ports as number[]) || []} name={names(r)[0]} /> : null),
  },
]

/** IPv4 subnets of the AstraTerm host's interfaces (sweep suggestions). */
function useLocalSubnets(enabled: boolean): string[] {
  const q = useQuery({ queryKey: ['tools', 'interfaces'], queryFn: getInterfaces, enabled, staleTime: 60_000 })
  return React.useMemo(() => {
    const out: string[] = []
    for (const ifc of q.data ?? []) {
      if (!ifc.up) continue
      for (const a of ifc.addrs ?? []) {
        const r = calcSubnet(a)
        if ('error' in r || r.family !== 4 || r.prefix < 16 || r.prefix > 30 || a.startsWith('127.') || a.startsWith('169.254.')) continue
        if (!out.includes(r.cidr)) out.push(r.cidr)
      }
    }
    return out.slice(0, 6)
  }, [q.data])
}

export default function NetScanPanel() {
  const job = useToolJob('netscan')
  const { state } = job
  const [f, set] = useToolForm('netscan', DEFAULTS)
  usePrefill('netscan', (p) => set({ targets: asString(p.targets ?? p.cidr), via: asString(p.viaConnectionId) }))
  const mode = useRunMode()
  const isAdmin = useIsAdmin()
  const subnets = useLocalSubnets(mode === 'desktop' || isAdmin)

  const run = () => {
    const targets = f.targets.trim()
    if (!targets || job.running) return
    const params: Record<string, unknown> = {
      targets,
      ports: f.ports.trim() || undefined,
      timeoutMs: f.timeoutMs,
      concurrency: f.concurrency,
      noIcmp: !f.icmp || undefined,
      resolveNames: f.names,
      allHosts: f.allHosts || undefined,
      viaConnectionId: f.via || undefined,
    }
    void job.run(params)
    addRun('netscan', params, targets)
  }

  const hostRows = useRowsOfKind(state, 'host')
  const macRows = useRowsOfKind(state, 'mac')
  const hosts = React.useMemo(() => {
    const macs = new Map(macRows.map((r) => [str(r.ip), r]))
    return hostRows.map((r) => {
      const m = macs.get(str(r.ip))
      return m ? { ...r, mac: m.mac, vendor: m.vendor } : r
    })
  }, [hostRows, macRows])
  const alive = React.useMemo(() => hosts.filter((h) => h.alive).length, [hosts])
  const progress = state.latest.progress
  const summary = state.latest.summary
  const running = job.running

  return (
    <PanelLayout
      title="Network scanner"
      description="Find live hosts on a subnet or range: ping sweep + TCP probes, reverse DNS / NetBIOS / mDNS names, MAC vendors and open services."
      form={
        <>
          <FormGrid>
            <Field label="Targets" hint="CIDR, range or list — up to 8192 hosts" className="@lg:col-span-2">
              <Input value={f.targets} onChange={(e) => set({ targets: e.target.value })} placeholder="192.168.1.0/24 or 10.0.0.1-50" onKeyDown={onEnter(run)} autoFocus spellCheck={false} />
            </Field>
            <Field label="Ports" hint="Probed on each host (blank = common services)">
              <Input value={f.ports} onChange={(e) => set({ ports: e.target.value })} placeholder="21,22,23,80,443,445,3389,5900…" className="font-mono" onKeyDown={onEnter(run)} spellCheck={false} />
            </Field>
            <Field label="Timeout">
              <NumberInput value={f.timeoutMs} onChange={(v) => set({ timeoutMs: v ?? 600 })} min={50} max={10000} step={50} unit="ms" />
            </Field>
            <Field label="Concurrency">
              <NumberInput value={f.concurrency} onChange={(v) => set({ concurrency: v ?? 256 })} min={1} max={1024} />
            </Field>
            <ViaConnectionField value={f.via} onChange={(v) => set({ via: v })} label="Scan from" hint="Through a saved SSH host (TCP probes only)" />
            <Field label="Options">
              <div className="flex flex-col gap-1.5">
                <CheckboxField label="Ping sweep (ICMP)" checked={f.icmp && !f.via} disabled={!!f.via} onCheckedChange={(v) => set({ icmp: !!v })} />
                <CheckboxField label="Look up names (DNS, NetBIOS, mDNS)" checked={f.names} onCheckedChange={(v) => set({ names: !!v })} />
                <CheckboxField label="List hosts that are down" checked={f.allHosts} onCheckedChange={(v) => set({ allHosts: !!v })} />
              </div>
            </Field>
          </FormGrid>
          {subnets.length > 0 && (
            <div className="mt-2 flex flex-wrap items-center gap-1.5">
              <span className="text-xs text-muted-foreground">Local subnets:</span>
              {subnets.map((s) => (
                <Button key={s} variant="outline" size="xs" className="font-mono" onClick={() => set({ targets: s })}>
                  {s}
                </Button>
              ))}
            </div>
          )}
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
            count={alive}
            countLabel="alive"
            extra={<ExportCsvButton columns={COLUMNS} rows={hosts} filename="netscan.csv" />}
          />
          <RecentRuns tool="netscan" onPick={(r) => set({ targets: asString(r.params.targets ?? r.params.cidr), via: asString(r.params.viaConnectionId) })} />
        </>
      }
    >
      <JobNotes state={state} />
      {progress && (running || !summary) && <ProgressBar done={num(progress.done) ?? 0} total={num(progress.total) ?? 0} label={`${str(progress.alive)} alive`} />}
      {summary && (
        <StatRow>
          <Stat label="Alive" value={str(summary.alive)} tone={Number(summary.alive) > 0 ? 'success' : 'default'} />
          <Stat label="Scanned" value={Number(summary.scanned).toLocaleString()} />
        </StatRow>
      )}
      <ResultArea className="mt-3 border-t">
        <ResultsTable
          columns={COLUMNS}
          rows={hosts}
          rowKey={(r) => str(r.ip)}
          rowClassName={(r) => (r.alive ? undefined : 'text-muted-foreground')}
          label="Hosts"
          empty={<EmptyResults icon={Network} hint={state.status === 'idle' ? 'Enter a subnet and press Scan.' : running ? 'Sweeping…' : 'No live hosts found.'} />}
        />
      </ResultArea>
    </PanelLayout>
  )
}
