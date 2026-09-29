/*
 * DNS lookup (TOOL-8): any record type against the system resolver or a custom one, over UDP (TCP retry on
 * truncation), TCP or DNS-over-TLS, optionally with DNSSEC records.
 */
import * as React from 'react'
import { Globe } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { CheckboxField } from '@/components/ui/checkbox'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SimpleSelect } from '@/components/ui/select'
import {
  type Column,
  CopyButton,
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
  num,
  onEnter,
  str,
} from '../components'
import { useToolForm } from '../form'
import { addRun } from '../history'
import { asString, usePrefill } from '../navigate'
import type { ToolRow } from '../types'
import { useRowsOfKind, useToolJob } from '../useToolJob'

const DNS_TYPES = ['A', 'AAAA', 'CNAME', 'MX', 'NS', 'TXT', 'SOA', 'SRV', 'CAA', 'PTR', 'ANY', 'DNSKEY', 'DS', 'HTTPS', 'SVCB', 'TLSA', 'SSHFP', 'NAPTR']
const RESOLVERS = [
  { value: 'system', label: 'System resolver' },
  { value: '1.1.1.1', label: 'Cloudflare 1.1.1.1' },
  { value: '8.8.8.8', label: 'Google 8.8.8.8' },
  { value: '9.9.9.9', label: 'Quad9 9.9.9.9' },
]

const DEFAULTS = { name: '', type: 'A', resolver: '', transport: 'udp' as 'udp' | 'tcp' | 'tls', dnssec: false }

const COLUMNS: Column<ToolRow>[] = [
  { key: 'section', header: 'Section', className: 'w-24 text-muted-foreground', cell: (r) => str(r.section), csv: (r) => str(r.section), sortValue: (r) => str(r.section) },
  { key: 'name', header: 'Name', className: 'font-mono', cell: (r) => str(r.name), csv: (r) => str(r.name), sortValue: (r) => str(r.name) },
  { key: 'ttl', header: 'TTL', className: 'w-16 tabular', cell: (r) => str(r.ttl), csv: (r) => str(r.ttl), sortValue: (r) => num(r.ttl) ?? 0 },
  { key: 'type', header: 'Type', className: 'w-20', cell: (r) => <Badge variant="outline">{str(r.type)}</Badge>, csv: (r) => str(r.type), sortValue: (r) => str(r.type) },
  {
    key: 'data',
    header: 'Data',
    className: 'max-w-0 w-full font-mono text-sm',
    cell: (r) => (
      <span className="group flex items-center gap-1">
        <span className="min-w-0 break-all">{str(r.data)}</span>
        <span className="opacity-0 transition-opacity group-hover:opacity-100">
          <CopyButton value={str(r.data)} label="" title="Copy value" />
        </span>
      </span>
    ),
    csv: (r) => str(r.data),
  },
]

export default function DnsPanel() {
  const job = useToolJob('dns')
  const { state } = job
  const [f, set] = useToolForm('dns', DEFAULTS)
  usePrefill('dns', (p) => set({ name: asString(p.name), type: asString(p.type, f.type) }))

  const run = () => {
    const name = f.name.trim()
    if (!name || job.running) return
    // An IP address with an address type means a reverse lookup.
    const isIP = /^\d{1,3}(\.\d{1,3}){3}$/.test(name) || (name.includes(':') && /^[0-9a-f:.]+$/i.test(name))
    const type = isIP && (f.type === 'A' || f.type === 'AAAA') ? 'PTR' : f.type
    if (type !== f.type) set({ type })
    const params: Record<string, unknown> = { name, type, resolver: f.resolver.trim() || undefined, tcp: f.transport === 'tcp' || undefined, dot: f.transport === 'tls' || undefined, dnssec: f.dnssec || undefined }
    void job.run(params)
    addRun('dns', params, `${type} ${name}${f.resolver.trim() ? ' @' + f.resolver.trim() : ''}`)
  }

  const records = useRowsOfKind(state, 'record')
  const summary = state.latest.summary
  const resolverPreset = RESOLVERS.find((r) => r.value === (f.resolver.trim() || 'system'))?.value

  return (
    <PanelLayout
      title="DNS lookup"
      description="Query any record type against the system resolver or a custom one, over UDP, TCP or DNS-over-TLS."
      form={
        <>
          <FormGrid>
            <Field label="Name" hint="An IP address looks up its PTR record" className="@lg:col-span-2">
              <Input value={f.name} onChange={(e) => set({ name: e.target.value })} placeholder="example.com or 1.1.1.1" onKeyDown={onEnter(run)} autoFocus spellCheck={false} />
            </Field>
            <Field label="Type">
              <SimpleSelect value={f.type} onValueChange={(v) => set({ type: v })} options={DNS_TYPES.map((t) => ({ value: t, label: t }))} aria-label="Record type" />
            </Field>
            <Field
              label="Resolver"
              labelAside={
                <SimpleSelect
                  size="sm"
                  value={resolverPreset}
                  onValueChange={(v) => set({ resolver: v === 'system' ? '' : v })}
                  options={RESOLVERS}
                  placeholder="Presets"
                  aria-label="Resolver presets"
                  className="h-6 w-28 text-xs"
                />
              }
            >
              <Input value={f.resolver} onChange={(e) => set({ resolver: e.target.value })} placeholder="system (or host[:port])" onKeyDown={onEnter(run)} spellCheck={false} className="font-mono" />
            </Field>
            <Field label="Transport">
              <SegmentedControl
                value={f.transport}
                onValueChange={(v) => set({ transport: v })}
                options={[
                  { value: 'udp', label: 'UDP' },
                  { value: 'tcp', label: 'TCP' },
                  { value: 'tls', label: 'TLS (DoT)' },
                ]}
                aria-label="Transport"
              />
            </Field>
            <Field label="Options">
              <CheckboxField label="DNSSEC (DO bit)" checked={f.dnssec} onCheckedChange={(v) => set({ dnssec: !!v })} />
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
            disabled={!f.name.trim()}
            runLabel="Query"
            count={records.length}
            countLabel="records"
            extra={<ExportCsvButton columns={COLUMNS} rows={records} filename={`dns-${f.name.trim() || 'query'}.csv`} />}
          />
          <RecentRuns tool="dns" onPick={(r) => set({ name: asString(r.params.name), type: asString(r.params.type, 'A'), resolver: asString(r.params.resolver) })} />
        </>
      }
    >
      <JobNotes state={state} />
      {summary && (
        <StatRow>
          <Stat label="Status" value={str(summary.rcode)} tone={summary.rcode === 'NOERROR' ? 'success' : 'warning'} />
          <Stat label="Answers" value={str(summary.answers)} />
          {summary.rttMs != null && <Stat label="Time" value={`${summary.rttMs} ms`} />}
          <Stat label="Server" value={<span className="font-mono text-base">{str(summary.server)}</span>} />
          <Stat label="Transport" value={str(summary.transport)} />
          <Stat
            label="Flags"
            value={
              <span className="text-base">
                {[summary.authoritative && 'authoritative', summary.authenticated && 'DNSSEC-validated', summary.truncated && 'truncated'].filter(Boolean).join(', ') || '—'}
              </span>
            }
          />
        </StatRow>
      )}
      <ResultArea className="mt-3 border-t">
        <ResultsTable
          columns={COLUMNS}
          rows={records}
          rowKey={(r, i) => `${str(r.name)}-${str(r.type)}-${i}`}
          label="Records"
          empty={<EmptyResults icon={Globe} hint={state.status === 'idle' ? 'Enter a name and press Query.' : state.status === 'done' ? 'The answer has no records.' : 'Querying…'} />}
        />
      </ResultArea>
    </PanelLayout>
  )
}
