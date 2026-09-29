/*
 * HTTP check (TOOL-8 "httping"): request a URL with any method / headers / body, then break the time down (DNS,
 * connect, TLS, server wait, transfer). With a repeat count it becomes httping: one row per attempt plus statistics.
 * Headers and bodies may carry credentials: they are never written to the run history.
 */
import * as React from 'react'
import { Globe, Lock } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { CheckboxField } from '@/components/ui/checkbox'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { SimpleSelect } from '@/components/ui/select'
import { Textarea } from '@/components/ui/textarea'
import { cn, formatBytes } from '@/lib/utils'
import {
  type Column,
  CopyButton,
  EmptyResults,
  ExportCsvButton,
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
import { addRun, stripCredentials } from '../history'
import { asString, usePrefill } from '../navigate'
import type { ToolRow } from '../types'
import { useRowsOfKind, useToolJob } from '../useToolJob'

const METHODS = ['GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS']

/** Milliseconds for display: 2 decimals below 10 ms, 1 decimal above. */
const fmtMs = (v: unknown): string => {
  const n = typeof v === 'number' ? v : Number(v)
  if (!Number.isFinite(n)) return '—'
  return `${n < 10 ? n.toFixed(2) : n.toFixed(1)} ms`
}
const DEFAULTS = { url: '', method: 'GET', headers: '', body: '', follow: true, insecure: false, count: 1, intervalMs: 1000, timeoutMs: 15000, showAdvanced: false, runCount: 1 }

/** "Name: value" lines → header map (invalid lines are reported). */
function parseHeaders(text: string): { headers: Record<string, string>; error?: string } {
  const headers: Record<string, string> = {}
  for (const [i, raw] of text.split('\n').entries()) {
    const line = raw.trim()
    if (!line) continue
    const idx = line.indexOf(':')
    if (idx <= 0) return { headers, error: `Line ${i + 1}: expected "Name: value"` }
    const name = line.slice(0, idx).trim()
    if (!/^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/.test(name)) return { headers, error: `Line ${i + 1}: invalid header name` }
    headers[name] = line.slice(idx + 1).trim()
  }
  return { headers }
}

const PHASES: { key: string; label: string; color: string }[] = [
  { key: 'dnsMs', label: 'DNS', color: 'bg-info' },
  { key: 'connectMs', label: 'Connect', color: 'bg-warning' },
  { key: 'tlsMs', label: 'TLS', color: 'bg-primary' },
  { key: 'waitMs', label: 'Server', color: 'bg-success' },
  { key: 'transferMs', label: 'Transfer', color: 'bg-muted-foreground' },
]

/** Timing waterfall of the final request hop. */
function Waterfall({ timing, redirects }: { timing: Record<string, number>; redirects: number }) {
  const ttfb = timing.ttfbMs ?? 0
  const setup = (timing.dnsMs ?? 0) + (timing.connectMs ?? 0) + (timing.tlsMs ?? 0)
  const phases: Record<string, number> = {
    dnsMs: timing.dnsMs ?? 0,
    connectMs: timing.connectMs ?? 0,
    tlsMs: timing.tlsMs ?? 0,
    waitMs: Math.max(0, ttfb - setup),
    transferMs: Math.max(0, (timing.totalMs ?? 0) - (timing.redirectMs ?? 0) - ttfb),
  }
  const total = Object.values(phases).reduce((a, b) => a + b, 0) || 1
  return (
    <div className="mx-4 mt-3 shrink-0 rounded-md border bg-card px-3 py-2">
      <div className="flex h-3 overflow-hidden rounded-full bg-muted" role="img" aria-label="Timing breakdown">
        {PHASES.map((p) => (phases[p.key] > 0 ? <div key={p.key} className={p.color} style={{ width: `${(phases[p.key] / total) * 100}%` }} title={`${p.label}: ${fmtMs(phases[p.key])}`} /> : null))}
      </div>
      <div className="mt-1.5 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
        {PHASES.map((p) => (
          <span key={p.key} className="inline-flex items-center gap-1.5 tabular">
            <span className={cn('size-2 rounded-full', p.color)} />
            {p.label} {fmtMs(phases[p.key])}
          </span>
        ))}
        {redirects > 0 && <span className="tabular">redirects {fmtMs(timing.redirectMs ?? 0)}</span>}
      </div>
    </div>
  )
}

const ATTEMPT_COLUMNS: Column<ToolRow>[] = [
  { key: 'attempt', header: '#', className: 'w-10 tabular text-muted-foreground', cell: (r) => str(r.attempt), csv: (r) => str(r.attempt) },
  { key: 'status', header: 'Status', className: 'w-24', cell: (r) => (r.error ? <Badge variant="destructive">error</Badge> : <StatusCode code={num(r.status) ?? 0} />), csv: (r) => str(r.status || r.error) },
  { key: 'total', header: 'Total', className: 'w-24 tabular', cell: (r) => (r.timing ? fmtMs((r.timing as Record<string, number>).totalMs) : ''), csv: (r) => str((r.timing as Record<string, number> | undefined)?.totalMs), sortValue: (r) => (r.timing as Record<string, number> | undefined)?.totalMs ?? 1e9 },
  { key: 'ttfb', header: 'TTFB', className: 'w-24 tabular', cell: (r) => (r.timing ? fmtMs((r.timing as Record<string, number>).ttfbMs) : ''), csv: (r) => str((r.timing as Record<string, number> | undefined)?.ttfbMs) },
  { key: 'size', header: 'Size', className: 'w-24 tabular', cell: (r) => (r.error ? '' : formatBytes(num(r.bodyBytes) ?? 0)), csv: (r) => str(r.bodyBytes) },
  { key: 'detail', header: 'Detail', className: 'max-w-0 w-full', cell: (r) => <span className="block truncate text-sm text-muted-foreground" title={str(r.error || r.remoteAddr)}>{str(r.error || r.remoteAddr)}</span>, csv: (r) => str(r.error || r.remoteAddr) },
]

function StatusCode({ code }: { code: number }) {
  return <Badge variant={code < 300 ? 'success' : code < 400 ? 'info' : code < 500 ? 'warning' : 'destructive'}>{code}</Badge>
}

export default function HttpCheckPanel() {
  const job = useToolJob('httpcheck')
  const { state } = job
  const [f, set] = useToolForm('httpcheck', DEFAULTS)
  usePrefill('httpcheck', (p) => set({ url: asString(p.url), method: asString(p.method, 'GET') }))
  const parsed = React.useMemo(() => parseHeaders(f.headers), [f.headers])
  const hasBody = !['GET', 'HEAD', 'OPTIONS'].includes(f.method)

  const run = () => {
    const url = f.url.trim()
    if (!url || parsed.error || job.running) return
    const params: Record<string, unknown> = {
      url,
      method: f.method,
      followRedirects: f.follow,
      insecureTls: f.insecure || undefined,
      timeoutMs: f.timeoutMs,
      count: f.count > 1 ? f.count : undefined,
      intervalMs: f.count > 1 ? f.intervalMs : undefined,
      headers: Object.keys(parsed.headers).length ? parsed.headers : undefined,
      body: hasBody && f.body ? f.body : undefined,
    }
    set({ runCount: f.count })
    void job.run(params)
    addRun('httpcheck', { url, method: f.method }, `${f.method} ${stripCredentials(url)}`)
  }

  const results = useRowsOfKind(state, 'result')
  const redirects = useRowsOfKind(state, 'redirect')
  const result = results.filter((r) => r.error == null).at(-1)
  const summary = state.latest.summary
  const timing = (result?.timing as Record<string, number>) || {}
  const headers = (result?.headers as { name: string; value: string }[]) || []
  const tls = result?.tls as Record<string, unknown> | undefined
  const multi = state.status !== 'idle' && f.runCount > 1

  return (
    <PanelLayout
      title="HTTP check"
      description="Request a URL and break down the time (DNS, connect, TLS, server, transfer); repeat it to measure like httping."
      form={
        <>
          <div className="flex flex-wrap items-end gap-2">
            <Field label="Method" className="w-28">
              <SimpleSelect value={f.method} onValueChange={(v) => set({ method: v })} options={METHODS.map((m) => ({ value: m, label: m }))} aria-label="HTTP method" />
            </Field>
            <Field label="URL" className="min-w-60 flex-1">
              <Input value={f.url} onChange={(e) => set({ url: e.target.value })} placeholder="https://example.com/health" onKeyDown={onEnter(run)} autoFocus spellCheck={false} className="font-mono" />
            </Field>
          </div>
          <div className="mt-3 flex flex-wrap gap-x-6 gap-y-2">
            <CheckboxField label="Follow redirects" checked={f.follow} onCheckedChange={(v) => set({ follow: !!v })} />
            <CheckboxField label="Ignore TLS errors" checked={f.insecure} onCheckedChange={(v) => set({ insecure: !!v })} />
            <CheckboxField label="Headers, body & repeat…" checked={f.showAdvanced} onCheckedChange={(v) => set({ showAdvanced: !!v })} />
          </div>
          {f.showAdvanced && (
            <div className="mt-3 grid grid-cols-1 gap-3 @3xl:grid-cols-2">
              <Field label="Request headers" hint="One “Name: value” per line — not saved in the history" error={parsed.error}>
                <Textarea mono rows={4} value={f.headers} onChange={(e) => set({ headers: e.target.value })} placeholder={'Authorization: Bearer …\nAccept: application/json'} spellCheck={false} />
              </Field>
              <Field label="Request body" hint={hasBody ? 'Sent as is (max 1 MiB) — not saved in the history' : `Not sent with ${f.method}`}>
                <Textarea mono rows={4} value={f.body} onChange={(e) => set({ body: e.target.value })} disabled={!hasBody} placeholder='{"hello":"world"}' spellCheck={false} />
              </Field>
              <div className="grid grid-cols-3 gap-3 @3xl:col-span-2">
                <Field label="Repeat" hint="httping: requests to send">
                  <NumberInput value={f.count} onChange={(v) => set({ count: v ?? 1 })} min={1} max={100} />
                </Field>
                <Field label="Interval">
                  <NumberInput value={f.intervalMs} onChange={(v) => set({ intervalMs: v ?? 1000 })} min={100} max={60000} step={100} unit="ms" disabled={f.count <= 1} />
                </Field>
                <Field label="Timeout">
                  <NumberInput value={f.timeoutMs} onChange={(v) => set({ timeoutMs: v ?? 15000 })} min={500} max={120000} step={500} unit="ms" />
                </Field>
              </div>
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
            disabled={!f.url.trim() || !!parsed.error}
            runLabel={f.count > 1 ? `Send ${f.count}×` : 'Send'}
            extra={multi ? <ExportCsvButton columns={ATTEMPT_COLUMNS} rows={results} filename="httping.csv" /> : undefined}
          />
          <RecentRuns tool="httpcheck" onPick={(r) => set({ url: asString(r.params.url), method: asString(r.params.method, 'GET') })} />
        </>
      }
    >
      <JobNotes state={state} />
      {multi ? (
        <>
          {summary && (
            <StatRow>
              <Stat label="OK" value={`${str(summary.ok)}/${str(summary.count)}`} tone={summary.failed ? 'warning' : 'success'} />
              {summary.minMs != null && <Stat label="Min" value={fmtMs(summary.minMs)} />}
              {summary.avgMs != null && <Stat label="Avg" value={fmtMs(summary.avgMs)} />}
              {summary.maxMs != null && <Stat label="Max" value={fmtMs(summary.maxMs)} />}
            </StatRow>
          )}
          <ResultArea className="mt-3 border-t">
            <ResultsTable columns={ATTEMPT_COLUMNS} rows={results} rowKey={(r) => str(r.attempt)} label="Attempts" />
          </ResultArea>
        </>
      ) : result ? (
        <ResultArea className="overflow-auto">
          <StatRow>
            <Stat label="Status" value={str(result.statusText || result.status)} tone={Number(result.status) < 400 ? 'success' : Number(result.status) < 500 ? 'warning' : 'danger'} />
            <Stat label="Protocol" value={str(result.proto)} />
            <Stat label="Total" value={fmtMs(timing.totalMs)} />
            <Stat label="TTFB" value={fmtMs(timing.ttfbMs)} />
            <Stat label="Size" value={formatBytes(num(result.bodyBytes) ?? 0) + (result.truncated ? '+' : '')} />
          </StatRow>
          <Waterfall timing={timing} redirects={num(result.redirects) ?? 0} />
          <dl className="mx-4 mt-3 grid grid-cols-[8rem_1fr] gap-x-3 gap-y-1 text-sm">
            <dt className="text-muted-foreground">Final URL</dt>
            <dd className="min-w-0 font-mono break-all">{str(result.url)}</dd>
            <dt className="text-muted-foreground">Remote address</dt>
            <dd className="font-mono">{str(result.remoteAddr) || '—'}</dd>
            {redirects.length > 0 && (
              <>
                <dt className="text-muted-foreground">Redirects</dt>
                <dd className="min-w-0">
                  {redirects.map((r, i) => (
                    <div key={i} className="font-mono break-all">
                      <Badge variant="outline" className="mr-1.5">{str(r.status)}</Badge>→ {str(r.to)}
                    </div>
                  ))}
                </dd>
              </>
            )}
            {tls && (
              <>
                <dt className="text-muted-foreground">TLS</dt>
                <dd className="flex min-w-0 flex-wrap items-center gap-x-2">
                  <Lock className="size-3.5 text-success" />
                  <span>{str(tls.version)}</span>
                  <span className="font-mono text-xs text-muted-foreground">{str(tls.cipher)}</span>
                  {tls.alpn ? <Badge variant="outline">{str(tls.alpn)}</Badge> : null}
                  {tls.daysRemaining != null && <span className={cn('text-xs', Number(tls.daysRemaining) < 14 ? 'text-destructive' : 'text-muted-foreground')}>cert expires in {str(tls.daysRemaining)} days</span>}
                </dd>
              </>
            )}
          </dl>
          <h3 className="mx-4 mt-4 mb-1 text-sm font-semibold">Response headers</h3>
          <table className="mx-4 mb-3 w-[calc(100%-2rem)] text-sm">
            <tbody>
              {headers.map((hdr, i) => (
                <tr key={i} className="border-b border-border/60">
                  <td className="w-52 py-1 pr-3 align-top font-medium text-muted-foreground">{hdr.name}</td>
                  <td className="py-1 font-mono break-all">{hdr.value}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {result.bodyPreview ? (
            <>
              <div className="mx-4 mt-2 mb-1 flex items-center justify-between">
                <h3 className="text-sm font-semibold">Body (first 2 KiB)</h3>
                <CopyButton value={str(result.bodyPreview)} label="Copy" />
              </div>
              <pre className="mx-4 mb-4 max-h-80 overflow-auto rounded-md border bg-muted/40 p-2 font-mono text-sm break-words whitespace-pre-wrap">{str(result.bodyPreview)}</pre>
            </>
          ) : null}
        </ResultArea>
      ) : (
        <ResultArea>
          <EmptyResults icon={Globe} hint={state.status === 'idle' ? 'Enter a URL and press Send.' : 'Requesting…'} />
        </ResultArea>
      )}
    </PanelLayout>
  )
}
