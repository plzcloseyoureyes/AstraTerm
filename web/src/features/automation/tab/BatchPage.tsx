/*
 * Batch runs (AUTO-11): a command, snippet or script on many saved connections with a parallelism limit, per-host
 * timeout and stop-on-error; live per-host results, CSV export. Also shared by the schedule editor (ActionFields).
 */
import * as React from 'react'
import { Layers, Play } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SimpleSelect } from '@/components/ui/select'
import { Textarea } from '@/components/ui/textarea'
import { errorMessage, plural } from '@/lib/utils'
import { dangerousMatches, startBatch, useCapabilities, useScripts, useSnippets } from '../api'
import { ConnectionMultiSelect } from '../components/pickers'
import { RunView } from '../components/RunView'
import { checkDangerous } from '../guard'
import { askDangerous } from '../store'
import { promptVars, renderTemplate } from '../template'
import type { ExecMode, JobAction, JobKind } from '../types'

export function ActionFields({ value, onChange }: { value: JobAction; onChange: (a: JobAction) => void }) {
  const caps = useCapabilities()
  const { data: snippets } = useSnippets(value.kind === 'snippet')
  const { data: scripts } = useScripts(value.kind === 'script')
  const snippet = snippets?.find((s) => s.id === value.snippetId)
  const needVars = value.kind === 'snippet' && snippet ? promptVars(snippet.content) : value.kind === 'command' ? promptVars(value.command ?? '') : []
  return (
    <div className="flex flex-col gap-3">
      <Field label="Run">
        <SegmentedControl<JobKind>
          value={value.kind}
          onValueChange={(kind) => onChange({ ...value, kind })}
          options={[
            { value: 'command', label: 'Command' },
            { value: 'snippet', label: 'Snippet' },
            { value: 'script', label: 'Script', disabled: caps.data ? !caps.data.scripts : false },
          ]}
          aria-label="What to run"
        />
      </Field>
      {value.kind === 'command' && (
        <Field label="Command" hint="Placeholders such as {{host}} and {{name|default}} work here too">
          <Textarea mono rows={4} value={value.command ?? ''} onChange={(e) => onChange({ ...value, command: e.target.value })} placeholder="uptime; df -h /" spellCheck={false} />
        </Field>
      )}
      {value.kind === 'snippet' && (
        <Field label="Snippet">
          <SimpleSelect value={value.snippetId} onValueChange={(snippetId) => onChange({ ...value, snippetId })} placeholder="Choose a snippet" options={(snippets ?? []).map((s) => ({ value: s.id, label: s.folder ? `${s.folder} / ${s.name}` : s.name }))} />
        </Field>
      )}
      {value.kind === 'script' && (
        <Field label="Script" hint="Each connection's session is bound as `session`; vars.host / vars.connectionName are set">
          <SimpleSelect value={value.scriptId} onValueChange={(scriptId) => onChange({ ...value, scriptId })} placeholder="Choose a script" options={(scripts ?? []).map((s) => ({ value: s.id, label: s.name }))} />
        </Field>
      )}
      {needVars.length > 0 && (
        <div className="grid gap-2 sm:grid-cols-2">
          {needVars.map((v) => (
            <Field key={v.name} label={<span className="font-mono">{v.name}</span>}>
              <Input
                value={value.variables?.[v.name] ?? ''}
                placeholder={v.default}
                type={v.secret ? 'password' : 'text'}
                onChange={(e) => onChange({ ...value, variables: { ...value.variables, [v.name]: e.target.value } })}
                autoComplete="off"
              />
            </Field>
          ))}
        </div>
      )}
      <div className="flex flex-wrap items-end gap-3">
        {value.kind !== 'script' && (
          <Field label="Mode" className="w-44" hint="exec: SSH exec channel · session: typed in a terminal">
            <SimpleSelect<ExecMode>
              size="sm"
              value={value.mode ?? 'auto'}
              onValueChange={(mode) => onChange({ ...value, mode })}
              options={[
                { value: 'auto', label: 'Automatic' },
                { value: 'exec', label: 'SSH exec' },
                { value: 'session', label: 'Terminal session' },
              ]}
            />
          </Field>
        )}
        <Field label="In parallel" className="w-28">
          <NumberInput inputSize="sm" value={value.parallel ?? 4} min={1} max={caps.data?.maxParallel ?? 32} onChange={(v) => onChange({ ...value, parallel: v ?? 4 })} />
        </Field>
        <Field label="Timeout per host" className="w-36">
          <NumberInput inputSize="sm" value={value.timeoutSec ?? 60} min={1} max={21600} unit="s" onChange={(v) => onChange({ ...value, timeoutSec: v ?? 60 })} />
        </Field>
        <CheckboxField className="pb-1.5" label="Stop on the first error" checked={!!value.stopOnError} onCheckedChange={(v) => onChange({ ...value, stopOnError: v === true })} />
      </div>
    </div>
  )
}

/** Text of a command / snippet action (for the client-side guard). */
export function actionText(a: JobAction, snippets: { id: string; content: string }[] | undefined): string {
  if (a.kind === 'command') return renderTemplate(a.command ?? '', a.variables ?? {}).text
  if (a.kind === 'snippet') {
    const s = snippets?.find((x) => x.id === a.snippetId)
    return s ? renderTemplate(s.content, a.variables ?? {}).text : ''
  }
  return ''
}

export function actionReady(a: JobAction): boolean {
  if (a.kind === 'command') return !!a.command?.trim()
  if (a.kind === 'snippet') return !!a.snippetId
  return !!a.scriptId
}

export default function BatchPage({ connectionIds }: { connectionIds?: string[] }) {
  const { data: snippets } = useSnippets()
  const [action, setAction] = React.useState<JobAction>({ kind: 'command', command: '', mode: 'auto', parallel: 4, timeoutSec: 60 })
  const [conns, setConns] = React.useState<string[]>(connectionIds ?? [])
  const [name, setName] = React.useState('')
  const [run, setRun] = React.useState<{ runId?: string; jobId: string } | null>(null)
  const [starting, setStarting] = React.useState(false)

  const start = async () => {
    setStarting(true)
    try {
      let confirm = false
      const pre = checkDangerous(actionText(action, snippets))
      if (pre.length) {
        if (!(await askDangerous(pre, [plural(conns.length, 'connection')]))) return
        confirm = true
      }
      for (;;) {
        try {
          const r = await startBatch({ ...action, name: name.trim() || undefined, connectionIds: conns, confirmDangerous: confirm })
          setRun(r)
          return
        } catch (err) {
          const m = dangerousMatches(err)
          if (m && !confirm) {
            if (!(await askDangerous(m, [plural(conns.length, 'connection')]))) return
            confirm = true
            continue
          }
          throw err
        }
      }
    } catch (err) {
      toast.error('The batch could not start', { description: errorMessage(err) })
    } finally {
      setStarting(false)
    }
  }

  return (
    <div className="flex h-full min-h-0 flex-col gap-3 overflow-y-auto p-3 @4xl:flex-row">
      <section className="flex w-full shrink-0 flex-col gap-3 @4xl:w-[26rem]" aria-label="Batch run">
        <h2 className="flex items-center gap-2 text-md font-semibold">
          <Layers className="size-4 text-primary" /> Run on many hosts
        </h2>
        <ActionFields value={action} onChange={setAction} />
        <Field label="Connections">
          <ConnectionMultiSelect value={conns} onChange={setConns} />
        </Field>
        <Field label="Name (optional)">
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="Shown in the run history" maxLength={200} />
        </Field>
        <Button onClick={() => void start()} loading={starting} disabled={!conns.length || !actionReady(action)} className="self-start">
          <Play /> Run on {plural(conns.length, 'connection')}
        </Button>
      </section>
      <section className="flex min-h-80 min-w-0 flex-1 flex-col" aria-label="Results">
        {run ? (
          <RunView runId={run.runId} jobId={run.jobId} className="min-h-0 flex-1" />
        ) : (
          <div className="flex flex-1 items-center justify-center rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
            Results appear here, one row per connection. SSH connections run on an exec channel (clean output, exit codes);
            other protocols open a terminal session, type the command and capture the output until the prompt returns.
          </div>
        )}
      </section>
    </div>
  )
}
