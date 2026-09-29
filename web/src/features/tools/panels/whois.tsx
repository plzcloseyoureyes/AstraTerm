/*
 * Whois (TOOL-8): domain, IP or ASN registration lookup following the registry referral chain.
 */
import * as React from 'react'
import { Building2 } from 'lucide-react'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { CopyButton, EmptyResults, FormGrid, JobNotes, PanelLayout, RecentRuns, ResultArea, RunControls, onEnter, str } from '../components'
import { useToolForm } from '../form'
import { addRun } from '../history'
import { asString, usePrefill } from '../navigate'
import { useToolJob } from '../useToolJob'

const DEFAULTS = { query: '', server: '' }

export default function WhoisPanel() {
  const job = useToolJob('whois')
  const { state } = job
  const [f, set] = useToolForm('whois', DEFAULTS)
  usePrefill('whois', (p) => set({ query: asString(p.query) }))
  const [filter, setFilter] = React.useState('')

  const run = () => {
    const query = f.query.trim()
    if (!query || job.running) return
    const params = { query, server: f.server.trim() || undefined }
    void job.run(params)
    addRun('whois', params, query)
  }

  const text = str(state.latest.text?.text)
  const warning = str(state.latest.summary?.warning)
  const shown = React.useMemo(() => {
    const q = filter.trim().toLowerCase()
    if (!q) return text
    return text
      .split('\n')
      .filter((l) => l.toLowerCase().includes(q))
      .join('\n')
  }, [text, filter])

  return (
    <PanelLayout
      title="Whois"
      description="Domain, IP or ASN registration lookup (follows the registry referral chain)."
      form={
        <>
          <FormGrid>
            <Field label="Query" className="@lg:col-span-2">
              <Input value={f.query} onChange={(e) => set({ query: e.target.value })} placeholder="example.com, 8.8.8.8 or AS15169" onKeyDown={onEnter(run)} autoFocus spellCheck={false} />
            </Field>
            <Field label="Server" hint="Optional: host[:port] to ask directly">
              <Input value={f.server} onChange={(e) => set({ server: e.target.value })} placeholder="whois.iana.org" onKeyDown={onEnter(run)} spellCheck={false} />
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
            disabled={!f.query.trim()}
            runLabel="Look up"
            extra={
              text ? (
                <>
                  <Input inputSize="sm" value={filter} onChange={(e) => setFilter(e.target.value)} placeholder="Filter lines…" className="w-40" aria-label="Filter lines" />
                  <CopyButton value={text} label="Copy" />
                </>
              ) : undefined
            }
          />
          <RecentRuns tool="whois" onPick={(r) => set({ query: asString(r.params.query), server: asString(r.params.server) })} />
        </>
      }
    >
      <JobNotes state={state} />
      {warning && <div className="shrink-0 px-4 pt-2 text-sm text-warning">{warning}</div>}
      <ResultArea className="mt-3 overflow-auto border-t">
        {text ? (
          <pre className="px-4 py-3 font-mono text-sm break-words whitespace-pre-wrap select-text">{shown || 'No line matches the filter.'}</pre>
        ) : (
          <EmptyResults icon={Building2} hint={state.status === 'idle' ? 'Enter a domain, IP or AS number and press Look up.' : 'Looking up…'} />
        )}
      </ResultArea>
    </PanelLayout>
  )
}
