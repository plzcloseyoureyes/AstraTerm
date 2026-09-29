/*
 * SSH server audit (CC-17, ssh-audit style): offered key exchange / host key / cipher / MAC algorithms rated against
 * a built-in policy, Terrapin (CVE-2023-48795) exposure, host key fingerprints and a hardened sshd_config suggestion.
 */
import * as React from 'react'
import { ShieldCheck, TriangleAlert } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { cn } from '@/lib/utils'
import { CopyButton, EmptyResults, FormGrid, JobNotes, PanelLayout, RatingBadge, RecentRuns, ResultArea, RunControls, onEnter, str } from '../components'
import { useToolForm } from '../form'
import { addRun } from '../history'
import { asNumber, asString, usePrefill } from '../navigate'
import type { ToolRow } from '../types'
import { useRowsOfKind, useToolJob } from '../useToolJob'

const GRADE_TONE: Record<string, string> = { A: 'text-success', B: 'text-success', C: 'text-warning', D: 'text-destructive', F: 'text-destructive' }
const SEVERITY_VARIANT: Record<string, React.ComponentProps<typeof Badge>['variant']> = { critical: 'destructive', high: 'destructive', medium: 'warning', low: 'info', info: 'secondary' }
const DEFAULTS = { host: '', port: 22 }

export default function SshAuditPanel() {
  const job = useToolJob('sshaudit')
  const { state } = job
  const [f, set] = useToolForm('sshaudit', DEFAULTS)
  usePrefill('sshaudit', (p) => set({ host: asString(p.host), port: asNumber(p.port, 22) }))

  const run = () => {
    const host = f.host.trim()
    if (!host || job.running) return
    const params = { host, port: f.port }
    void job.run(params)
    addRun('sshaudit', params, `${host}:${f.port}`)
  }

  const server = state.latest.server
  const terrapin = state.latest.terrapin
  const summary = state.latest.summary
  const categories = useRowsOfKind(state, 'category')
  const findings = useRowsOfKind(state, 'finding')
  const hostkeys = useRowsOfKind(state, 'hostkey')

  return (
    <PanelLayout
      title="SSH server audit"
      description="Enumerate the algorithms an SSH server offers, flag weak or legacy ones and Terrapin exposure, and get a hardened sshd_config."
      form={
        <>
          <FormGrid>
            <Field label="Host">
              <Input value={f.host} onChange={(e) => set({ host: e.target.value })} placeholder="example.com or host:2222" onKeyDown={onEnter(run)} autoFocus spellCheck={false} />
            </Field>
            <Field label="Port">
              <NumberInput value={f.port} onChange={(v) => set({ port: v ?? 22 })} min={1} max={65535} />
            </Field>
          </FormGrid>
        </>
      }
      controls={
        <>
          <RunControls state={state} onRun={run} onCancel={job.cancel} onClear={job.reset} disabled={!f.host.trim()} runLabel="Audit" />
          <RecentRuns tool="sshaudit" onPick={(r) => set({ host: asString(r.params.host), port: asNumber(r.params.port, 22) })} />
        </>
      }
    >
      <JobNotes state={state} />
      <ResultArea className="overflow-auto">
        {server || summary ? (
          <div className="flex flex-col gap-3 p-4">
            <div className="flex flex-wrap items-center gap-4 rounded-lg border bg-card p-3">
              {summary?.grade != null && (
                <div className={cn('text-4xl leading-none font-bold', GRADE_TONE[str(summary.grade)] || 'text-foreground')} aria-label={`Grade ${str(summary.grade)}`}>
                  {str(summary.grade)}
                </div>
              )}
              <div className="min-w-0 flex-1">
                {server && <div className="font-mono text-sm break-all">{str(server.banner)}</div>}
                {terrapin && (
                  <div className="mt-1.5 flex flex-wrap items-center gap-2 text-sm">
                    <Badge variant={terrapin.vulnerable ? 'destructive' : 'success'}>{terrapin.vulnerable ? 'Terrapin exposed' : 'Terrapin safe'}</Badge>
                    {terrapin.strictKex ? <Badge variant="success">strict-kex</Badge> : <Badge variant="outline">no strict-kex</Badge>}
                    <span className="text-muted-foreground">{str(terrapin.reason)}</span>
                  </div>
                )}
              </div>
            </div>

            {findings.length > 0 && (
              <section className="rounded-lg border bg-card p-3" aria-label="Findings">
                <h3 className="mb-2 text-sm font-semibold">Findings</h3>
                <ul className="flex flex-col gap-1.5">
                  {findings.map((f2, i) => (
                    <li key={i} className="flex items-start gap-2 text-sm">
                      <Badge variant={SEVERITY_VARIANT[str(f2.severity)] ?? 'secondary'} className="mt-0.5 w-16 shrink-0 justify-center uppercase">
                        {str(f2.severity)}
                      </Badge>
                      <span className="min-w-0 break-words">{str(f2.message)}</span>
                    </li>
                  ))}
                </ul>
              </section>
            )}

            {hostkeys.length > 0 && (
              <section className="rounded-lg border bg-card p-3" aria-label="Host keys">
                <h3 className="mb-2 text-sm font-semibold">Host keys</h3>
                <ul className="flex flex-col gap-1.5 text-sm">
                  {hostkeys.map((k: ToolRow) => (
                    <li key={str(k.type)} className="flex flex-wrap items-center gap-2">
                      <Badge variant="outline" className="w-28 justify-center">{str(k.type)}{k.bits ? ` ${str(k.bits)}` : ''}</Badge>
                      <span className="font-mono break-all">{str(k.sha256)}</span>
                      <CopyButton value={str(k.sha256)} label="" title="Copy fingerprint" />
                      {k.warning ? (
                        <span className="inline-flex items-center gap-1 text-warning">
                          <TriangleAlert className="size-3.5" /> {str(k.warning)}
                        </span>
                      ) : k.note ? (
                        <span className="text-xs text-muted-foreground">{str(k.note)}</span>
                      ) : null}
                    </li>
                  ))}
                </ul>
              </section>
            )}

            {categories.map((cat) => (
              <section key={str(cat.id)} className="rounded-lg border bg-card p-3" aria-label={str(cat.label)}>
                <h3 className="mb-2 text-sm font-semibold">{str(cat.label)}</h3>
                <ul className="flex flex-col gap-1">
                  {((cat.algorithms as ToolRow[]) || []).map((a, i) => (
                    <li key={i} className="flex flex-wrap items-center gap-2 text-sm">
                      <RatingBadge rating={str(a.rating)} />
                      <span className="font-mono">{str(a.name)}</span>
                      {a.note ? <span className="text-xs text-muted-foreground">— {str(a.note)}</span> : null}
                    </li>
                  ))}
                </ul>
              </section>
            ))}

            {summary?.recommendedConfig != null && (
              <section className="rounded-lg border bg-card p-3" aria-label="Suggested sshd_config">
                <div className="mb-2 flex items-center justify-between">
                  <h3 className="text-sm font-semibold">Suggested sshd_config</h3>
                  <CopyButton value={str(summary.recommendedConfig)} label="Copy" />
                </div>
                <pre className="overflow-x-auto rounded bg-muted p-2 font-mono text-sm">{str(summary.recommendedConfig)}</pre>
              </section>
            )}
          </div>
        ) : (
          <EmptyResults icon={ShieldCheck} hint={state.status === 'idle' ? 'Enter an SSH server and press Audit.' : 'Connecting…'} />
        )}
      </ResultArea>
    </PanelLayout>
  )
}
