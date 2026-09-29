/*
 * SNMP (CC-4): get, get-next, walk and bulk-walk over v1 / v2c / v3 (USM auth + privacy). OIDs can be given by MIB
 * name (sysDescr.0, ifTable, hrStorageTable…) and results are labelled with names from the embedded core MIBs.
 */
import * as React from 'react'
import { Router } from 'lucide-react'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { PasswordInput } from '@/components/ui/password-input'
import { SimpleSelect } from '@/components/ui/select'
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
  onEnter,
  str,
} from '../components'
import { useToolForm } from '../form'
import { addRun } from '../history'
import { asString, usePrefill } from '../navigate'
import type { ToolRow } from '../types'
import { useRowsOfKind, useToolJob } from '../useToolJob'

const OID_PRESETS = [
  { value: 'system', label: 'System (sysDescr, uptime, contact…)' },
  { value: 'sysDescr.0 sysName.0 sysUpTime.0 sysLocation.0', label: 'Identity (get)' },
  { value: 'ifTable', label: 'Interfaces (ifTable)' },
  { value: 'ifXTable', label: 'Interface names & 64-bit counters' },
  { value: 'ipAddrTable', label: 'IP addresses' },
  { value: 'hrStorageTable', label: 'Storage (HOST-RESOURCES)' },
  { value: 'hrProcessorLoad', label: 'CPU load (HOST-RESOURCES)' },
  { value: 'hrSWRunName', label: 'Running processes' },
  { value: 'entPhysicalTable', label: 'Hardware inventory (ENTITY)' },
  { value: 'lldpRemTable', label: 'LLDP neighbours' },
  { value: 'laLoad', label: 'Load average (net-snmp)' },
]

const DEFAULTS = {
  host: '',
  port: 161,
  version: '2c',
  operation: 'bulkwalk',
  community: 'public',
  oid: 'system',
  timeoutMs: 3000,
  retries: 1,
  secLevel: 'authPriv',
  username: '',
  authProto: 'SHA256',
  authPass: '',
  privProto: 'AES',
  privPass: '',
  contextName: '',
}

const COLUMNS: Column<ToolRow>[] = [
  { key: 'name', header: 'Name', className: 'w-56 font-mono text-sm', cell: (r) => <span className="block truncate" title={str(r.name)}>{str(r.name)}</span>, csv: (r) => str(r.name), sortValue: (r) => str(r.name) || '~' },
  { key: 'oid', header: 'OID', className: 'w-64 font-mono text-sm text-muted-foreground', cell: (r) => <span className="block truncate" title={str(r.oid)}>{str(r.oid)}</span>, csv: (r) => str(r.oid) },
  { key: 'type', header: 'Type', className: 'w-28 text-muted-foreground', cell: (r) => str(r.type), csv: (r) => str(r.type), sortValue: (r) => str(r.type) },
  { key: 'value', header: 'Value', className: 'max-w-0 w-full font-mono text-sm', cell: (r) => <span className="break-all">{str(r.value)}</span>, csv: (r) => str(r.value) },
]

export default function SnmpPanel() {
  const job = useToolJob('snmp')
  const { state } = job
  const [f, set] = useToolForm('snmp', DEFAULTS)
  usePrefill('snmp', (p) => set({ host: asString(p.host), oid: asString(p.oid, f.oid) }))
  const [filter, setFilter] = React.useState('')
  const v3 = f.version === '3'

  const run = () => {
    const host = f.host.trim()
    if (!host || job.running) return
    const params: Record<string, unknown> = { host, port: f.port, version: f.version, operation: f.operation, oid: f.oid.trim(), timeoutMs: f.timeoutMs, retries: f.retries }
    if (v3) {
      Object.assign(params, { secLevel: f.secLevel, username: f.username, contextName: f.contextName || undefined })
      if (f.secLevel !== 'noAuthNoPriv') Object.assign(params, { authProto: f.authProto, authPass: f.authPass })
      if (f.secLevel === 'authPriv') Object.assign(params, { privProto: f.privProto, privPass: f.privPass })
    } else {
      params.community = f.community
    }
    void job.run(params)
    addRun('snmp', params, `${f.operation} ${host} ${f.oid.trim()}`)
  }

  const vars = useRowsOfKind(state, 'var')
  const shown = React.useMemo(() => {
    const q = filter.trim().toLowerCase()
    return q ? vars.filter((r) => `${str(r.name)} ${str(r.oid)} ${str(r.value)}`.toLowerCase().includes(q)) : vars
  }, [vars, filter])
  const summary = state.latest.summary
  const needAuth = v3 && f.secLevel !== 'noAuthNoPriv'
  const needPriv = v3 && f.secLevel === 'authPriv'

  return (
    <PanelLayout
      title="SNMP"
      description="Get, get-next, walk and bulk-walk over SNMP v1 / v2c / v3. OIDs may be MIB names such as sysDescr.0 or ifTable."
      form={
        <>
          <FormGrid>
            <Field label="Host">
              <Input value={f.host} onChange={(e) => set({ host: e.target.value })} placeholder="10.0.0.1" onKeyDown={onEnter(run)} autoFocus spellCheck={false} />
            </Field>
            <Field label="Version">
              <SimpleSelect value={f.version} onValueChange={(v) => set({ version: v })} options={[{ value: '1', label: 'v1' }, { value: '2c', label: 'v2c' }, { value: '3', label: 'v3 (USM)' }]} aria-label="SNMP version" />
            </Field>
            <Field label="Operation">
              <SimpleSelect
                value={f.operation}
                onValueChange={(v) => set({ operation: v })}
                options={[
                  { value: 'get', label: 'get' },
                  { value: 'getnext', label: 'get-next' },
                  { value: 'walk', label: 'walk' },
                  { value: 'bulkwalk', label: 'bulk-walk (v2c/v3)' },
                ]}
                aria-label="Operation"
              />
            </Field>
            {!v3 && (
              <Field label="Community">
                <PasswordInput value={f.community} onChange={(e) => set({ community: e.target.value })} autoComplete="off" />
              </Field>
            )}
            <Field
              label="OIDs / MIB names"
              hint="Several separated by spaces"
              className="@lg:col-span-2"
              labelAside={
                <SimpleSelect
                  size="sm"
                  value={OID_PRESETS.some((p) => p.value === f.oid) ? f.oid : undefined}
                  onValueChange={(v) => set({ oid: v, operation: v.includes(' ') || v.endsWith('.0') ? 'get' : f.operation === 'get' ? 'bulkwalk' : f.operation })}
                  options={OID_PRESETS}
                  placeholder="Presets"
                  aria-label="OID presets"
                  className="h-6 w-28 text-xs"
                />
              }
            >
              <Input value={f.oid} onChange={(e) => set({ oid: e.target.value })} placeholder="system or 1.3.6.1.2.1.1" className="font-mono" onKeyDown={onEnter(run)} spellCheck={false} />
            </Field>
            <Field label="Port">
              <NumberInput value={f.port} onChange={(v) => set({ port: v ?? 161 })} min={1} max={65535} />
            </Field>
            <Field label="Timeout">
              <NumberInput value={f.timeoutMs} onChange={(v) => set({ timeoutMs: v ?? 3000 })} min={200} max={30000} step={100} unit="ms" />
            </Field>
            <Field label="Retries">
              <NumberInput value={f.retries} onChange={(v) => set({ retries: v ?? 1 })} min={0} max={5} />
            </Field>
          </FormGrid>
          {v3 && (
            <FormGrid className="mt-3">
              <Field label="Security level">
                <SimpleSelect
                  value={f.secLevel}
                  onValueChange={(v) => set({ secLevel: v })}
                  options={[
                    { value: 'noAuthNoPriv', label: 'noAuthNoPriv' },
                    { value: 'authNoPriv', label: 'authNoPriv' },
                    { value: 'authPriv', label: 'authPriv' },
                  ]}
                  aria-label="Security level"
                />
              </Field>
              <Field label="User name">
                <Input value={f.username} onChange={(e) => set({ username: e.target.value })} autoComplete="off" spellCheck={false} />
              </Field>
              <Field label="Context" hint="Optional">
                <Input value={f.contextName} onChange={(e) => set({ contextName: e.target.value })} autoComplete="off" spellCheck={false} />
              </Field>
              {needAuth && (
                <>
                  <Field label="Auth protocol">
                    <SimpleSelect value={f.authProto} onValueChange={(v) => set({ authProto: v })} options={['MD5', 'SHA', 'SHA224', 'SHA256', 'SHA384', 'SHA512'].map((p) => ({ value: p, label: p }))} aria-label="Auth protocol" />
                  </Field>
                  <Field label="Auth passphrase" hint="At least 8 characters" error={f.authPass && f.authPass.length < 8 ? 'Too short' : undefined}>
                    <PasswordInput value={f.authPass} onChange={(e) => set({ authPass: e.target.value })} autoComplete="off" />
                  </Field>
                </>
              )}
              {needPriv && (
                <>
                  <Field label="Privacy protocol">
                    <SimpleSelect value={f.privProto} onValueChange={(v) => set({ privProto: v })} options={['DES', 'AES', 'AES192', 'AES256', 'AES192C', 'AES256C'].map((p) => ({ value: p, label: p }))} aria-label="Privacy protocol" />
                  </Field>
                  <Field label="Privacy passphrase" hint="At least 8 characters" error={f.privPass && f.privPass.length < 8 ? 'Too short' : undefined}>
                    <PasswordInput value={f.privPass} onChange={(e) => set({ privPass: e.target.value })} autoComplete="off" />
                  </Field>
                </>
              )}
            </FormGrid>
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
            disabled={!f.host.trim() || (needAuth && f.authPass.length < 8) || (needPriv && f.privPass.length < 8) || (v3 && !f.username.trim())}
            runLabel="Query"
            count={vars.length}
            countLabel="variables"
            extra={
              <>
                {vars.length > 0 && <Input inputSize="sm" value={filter} onChange={(e) => setFilter(e.target.value)} placeholder="Filter…" className="w-40" aria-label="Filter variables" />}
                <ExportCsvButton columns={COLUMNS} rows={shown} filename={`snmp-${f.host.trim() || 'agent'}.csv`} />
              </>
            }
          />
          <RecentRuns tool="snmp" onPick={(r) => set({ host: asString(r.params.host), oid: asString(r.params.oid, 'system'), operation: asString(r.params.operation, 'bulkwalk'), version: asString(r.params.version, '2c') })} />
        </>
      }
    >
      <JobNotes state={state} />
      {summary && (
        <StatRow>
          <Stat label="Variables" value={str(summary.count)} tone={Number(summary.count) > 0 ? 'success' : 'warning'} />
          {summary.rttMs != null && <Stat label="Time" value={`${summary.rttMs} ms`} />}
          {summary.truncated ? <Stat label="Walk" value="Truncated" tone="warning" /> : null}
        </StatRow>
      )}
      <ResultArea className="mt-3 border-t">
        <ResultsTable
          columns={COLUMNS}
          rows={shown}
          rowKey={(r, i) => `${str(r.oid)}-${i}`}
          label="Variables"
          empty={<EmptyResults icon={Router} hint={state.status === 'idle' ? 'Enter an agent address and press Query.' : filter && vars.length ? 'No variable matches the filter.' : state.status === 'done' ? 'The agent returned no variables.' : 'Querying…'} />}
        />
      </ResultArea>
    </PanelLayout>
  )
}
