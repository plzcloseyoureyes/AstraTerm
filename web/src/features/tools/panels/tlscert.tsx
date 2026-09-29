/*
 * TLS certificate inspector (TOOL-8): chain details, expiry, SANs, fingerprints, key strength, hostname match,
 * supported protocol versions, direct TLS or STARTTLS (SMTP, IMAP, POP3, FTP, PostgreSQL, LDAP).
 */
import * as React from 'react'
import { ShieldCheck, TriangleAlert } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { CheckboxField } from '@/components/ui/checkbox'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { SimpleSelect } from '@/components/ui/select'
import { cn } from '@/lib/utils'
import { CopyButton, EmptyResults, FormGrid, JobNotes, PanelLayout, RecentRuns, ResultArea, RunControls, Stat, StatRow, onEnter, str } from '../components'
import { useToolForm } from '../form'
import { addRun } from '../history'
import { asNumber, asString, usePrefill } from '../navigate'
import type { ToolRow } from '../types'
import { useRowsOfKind, useToolJob } from '../useToolJob'

const STARTTLS = [
  { value: 'none', label: 'None (direct TLS)', port: 443 },
  { value: 'smtp', label: 'SMTP', port: 25 },
  { value: 'imap', label: 'IMAP', port: 143 },
  { value: 'pop3', label: 'POP3', port: 110 },
  { value: 'ftp', label: 'FTP', port: 21 },
  { value: 'postgres', label: 'PostgreSQL', port: 5432 },
  { value: 'ldap', label: 'LDAP', port: 389 },
]

const DEFAULTS = { host: '', port: 443, startTls: 'none', sni: '', versions: true }

function CertRow({ label, value, mono, tone, copy }: { label: string; value: React.ReactNode; mono?: boolean; tone?: 'danger' | 'warning'; copy?: string }) {
  return (
    <div className="flex items-baseline gap-2">
      <dt className="w-24 shrink-0 text-muted-foreground">{label}</dt>
      <dd className={cn('flex min-w-0 items-center gap-1 break-all', mono && 'font-mono text-sm', tone === 'danger' && 'text-destructive', tone === 'warning' && 'text-warning')}>
        <span className="min-w-0">{value}</span>
        {copy && <CopyButton value={copy} label="" title={`Copy ${label}`} />}
      </dd>
    </div>
  )
}

function daysUntil(iso: string): number {
  return Math.floor((new Date(iso).getTime() - Date.now()) / 86_400_000)
}

export default function TlsCertPanel() {
  const job = useToolJob('tlscert')
  const { state } = job
  const [f, set] = useToolForm('tlscert', DEFAULTS)
  usePrefill('tlscert', (p) => set({ host: asString(p.host), port: asNumber(p.port, 443), startTls: asString(p.startTls, 'none') }))

  const run = () => {
    const host = f.host.trim()
    if (!host || job.running) return
    const params = { host, port: f.port, startTls: f.startTls === 'none' ? undefined : f.startTls, serverName: f.sni.trim() || undefined, probeVersions: f.versions }
    void job.run(params)
    addRun('tlscert', params, `${host}:${f.port}${f.startTls !== 'none' ? ' ' + f.startTls : ''}`)
  }

  const conn = state.latest.connection
  const summary = state.latest.summary
  const certs = useRowsOfKind(state, 'cert')
  const versions = useRowsOfKind(state, 'version')
  const days = summary ? Number(summary.daysRemaining) : undefined

  return (
    <PanelLayout
      title="TLS certificate"
      description="Inspect a server's certificate chain, expiry, SANs, key and supported TLS versions (direct TLS or STARTTLS)."
      form={
        <>
          <FormGrid>
            <Field label="Host">
              <Input value={f.host} onChange={(e) => set({ host: e.target.value })} placeholder="example.com or example.com:8443" onKeyDown={onEnter(run)} autoFocus spellCheck={false} />
            </Field>
            <Field label="Port">
              <NumberInput value={f.port} onChange={(v) => set({ port: v ?? 443 })} min={1} max={65535} />
            </Field>
            <Field label="STARTTLS">
              <SimpleSelect
                value={f.startTls}
                onValueChange={(v) => set({ startTls: v, port: STARTTLS.find((s) => s.value === v)?.port ?? f.port })}
                options={STARTTLS.map((s) => ({ value: s.value, label: s.label }))}
                aria-label="STARTTLS protocol"
              />
            </Field>
            <Field label="Server name (SNI)" hint="Defaults to the host">
              <Input value={f.sni} onChange={(e) => set({ sni: e.target.value })} placeholder={f.host.trim() || 'example.com'} onKeyDown={onEnter(run)} spellCheck={false} />
            </Field>
            <Field label="Options">
              <CheckboxField label="Probe TLS 1.0–1.3 support" checked={f.versions} onCheckedChange={(v) => set({ versions: !!v })} />
            </Field>
          </FormGrid>
        </>
      }
      controls={
        <>
          <RunControls state={state} onRun={run} onCancel={job.cancel} onClear={job.reset} disabled={!f.host.trim()} runLabel="Inspect" />
          <RecentRuns tool="tlscert" onPick={(r) => set({ host: asString(r.params.host), port: asNumber(r.params.port, 443), startTls: asString(r.params.startTls, 'none') })} />
        </>
      }
    >
      <JobNotes state={state} />
      <ResultArea className="overflow-auto">
        {conn || summary ? (
          <>
            <StatRow>
              {conn && <Stat label="Protocol" value={str(conn.version)} tone={String(conn.version).startsWith('TLS 1.0') || String(conn.version).startsWith('TLS 1.1') ? 'warning' : 'default'} />}
              {conn && <Stat label="Chain" value={conn.verified ? 'Trusted' : 'Untrusted'} tone={conn.verified ? 'success' : 'danger'} />}
              {summary && <Stat label="Hostname" value={summary.hostnameMatch ? 'Matches' : 'Mismatch'} tone={summary.hostnameMatch ? 'success' : 'danger'} />}
              {days !== undefined && <Stat label={summary?.expired ? 'Expired' : 'Expires in'} value={`${Math.abs(days)} d`} tone={days < 0 ? 'danger' : days < 14 ? 'danger' : days < 45 ? 'warning' : 'success'} />}
              {summary && <Stat label="Certificates" value={str(summary.chainLength)} />}
            </StatRow>
            {conn && (
              <div className="mx-4 mt-3 space-y-1 text-sm">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="text-muted-foreground">Cipher</span>
                  <span className="font-mono">{str(conn.cipher)}</span>
                  {conn.weakCipher ? <Badge variant="warning">weak</Badge> : null}
                  {conn.alpn ? <Badge variant="outline">ALPN {str(conn.alpn)}</Badge> : null}
                  {conn.ocspStapled ? <Badge variant="outline">OCSP stapled</Badge> : null}
                  {Number(conn.scts) > 0 ? <Badge variant="outline">{str(conn.scts)} SCTs</Badge> : null}
                </div>
                {conn.verifyError ? (
                  <div className="flex items-start gap-1.5 text-warning">
                    <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
                    <span className="min-w-0 break-words">{str(conn.verifyError)}</span>
                  </div>
                ) : null}
              </div>
            )}
            {versions.length > 0 && (
              <div className="mx-4 mt-3 flex flex-wrap items-center gap-1.5 text-sm">
                <span className="mr-1 text-muted-foreground">Versions</span>
                {versions.map((v) => (
                  <Badge key={str(v.version)} variant={v.supported ? (v.deprecated ? 'warning' : 'success') : 'outline'} title={v.supported ? str(v.cipher) : str(v.error) || 'not supported'}>
                    {str(v.version)} {v.supported ? '✓' : '✗'}
                  </Badge>
                ))}
              </div>
            )}
            <div className="flex flex-col gap-3 p-4">
              {certs.map((c: ToolRow, i) => {
                const d = daysUntil(str(c.notAfter))
                return (
                  <section key={i} className="rounded-lg border bg-card p-3 text-sm" aria-label={`Certificate ${i}`}>
                    <div className="mb-2 flex flex-wrap items-center gap-2 font-semibold">
                      <Badge variant={i === 0 ? 'info' : 'outline'}>{i === 0 ? 'Leaf' : c.isCA ? `CA #${i}` : `#${i}`}</Badge>
                      <span className="min-w-0 font-mono break-all">{str(c.commonName) || str(c.subject)}</span>
                      {c.selfSigned ? <Badge variant="warning">self-signed</Badge> : null}
                      {c.expired ? <Badge variant="destructive">expired</Badge> : null}
                      {c.notYet ? <Badge variant="destructive">not yet valid</Badge> : null}
                    </div>
                    {Array.isArray(c.warnings) &&
                      (c.warnings as string[]).map((w) => (
                        <div key={w} className="mb-1 flex items-center gap-1.5 text-warning">
                          <TriangleAlert className="size-3.5" /> {w}
                        </div>
                      ))}
                    <dl className="grid grid-cols-1 gap-x-6 gap-y-1 @3xl:grid-cols-2">
                      <CertRow label="Subject" value={str(c.subject)} />
                      <CertRow label="Issuer" value={str(c.issuer)} />
                      <CertRow label="Valid from" value={new Date(str(c.notBefore)).toLocaleString()} />
                      <CertRow label="Valid until" value={`${new Date(str(c.notAfter)).toLocaleString()} (${d < 0 ? `${-d} days ago` : `${d} days`})`} tone={c.expired ? 'danger' : d < 30 ? 'warning' : undefined} />
                      <CertRow label="Key" value={`${str(c.keyAlg)}${c.keyBits ? ` ${str(c.keyBits)} bits` : ''}`} />
                      <CertRow label="Signature" value={str(c.sigAlg)} />
                      <CertRow label="Serial" value={str(c.serial)} mono copy={str(c.serial)} />
                      {Array.isArray(c.sans) && (c.sans as string[]).length > 0 && <CertRow label="DNS names" value={(c.sans as string[]).join(', ')} />}
                      {Array.isArray(c.ipSans) && (c.ipSans as string[]).length > 0 && <CertRow label="IP addresses" value={(c.ipSans as string[]).join(', ')} />}
                    </dl>
                    <dl className="mt-1 grid grid-cols-1 gap-y-1">
                      <CertRow label="SHA-256" value={str(c.sha256)} mono copy={str(c.sha256)} />
                      <CertRow label="SHA-1" value={str(c.sha1)} mono copy={str(c.sha1)} />
                    </dl>
                  </section>
                )
              })}
            </div>
          </>
        ) : (
          <EmptyResults icon={ShieldCheck} hint={state.status === 'idle' ? 'Enter a host and press Inspect.' : 'Connecting…'} />
        )}
      </ResultArea>
    </PanelLayout>
  )
}
