/*
 * Scripts page (AUTO-10): list, CodeMirror editor (lazy), run against nothing / a running session / a saved
 * connection with variables and a timeout, live log, the script's recent runs, API reference.
 */
import * as React from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { BookOpen, Code2, CopyPlus, Play, Plus, Save, Search, ShieldOff, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { useConnections } from '@/api/connections'
import { isSessionRunning, useSessions } from '@/api/sessions'
import { Button } from '@/components/ui/button'
import { confirm } from '@/components/ui/dialog-host'
import { EmptyState } from '@/components/ui/empty-state'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { QueryState } from '@/components/ui/query-state'
import { SimpleSelect } from '@/components/ui/select'
import { SkeletonRows } from '@/components/ui/skeleton'
import { LazyBoundary } from '@/components/ui/spinner'
import { Textarea } from '@/components/ui/textarea'
import { cn, errorMessage, formatRelativeTime } from '@/lib/utils'
import { autoKeys, createScript, deleteScript, runScript, updateScript, useCapabilities, useRuns, useScripts } from '../api'
import { RunView } from '../components/RunView'
import { StatusBadge } from '../components/pickers'
import type { Script } from '../types'

const CodeEditor = React.lazy(() => import('../components/CodeEditor'))

const TEMPLATE = `// Runs in AstraTerm's backend. Bound session: \`session\` (null when run without a target).
// Patterns are regular expressions (RE2 syntax).
session.sendLine("uname -a")
const r = session.expect(/Linux|Darwin|BSD/, 10000)
log("system:", r.match)

const out = session.run("uptime")
log("uptime:", out)
`

export const API_HELP: [string, string][] = [
  ['session.send(text) / sendLine(text)', 'type raw text / text + Enter'],
  ['session.sendSecret(key, {enter})', 'type a stored secret of the connection (never visible to the script)'],
  ['session.expect(pattern | [patterns], ms)', '→ {index, match, groups, before}; throws TimeoutError'],
  ['session.waitFor(pattern, ms)', 'like expect, null on timeout'],
  ['session.run(cmd, {timeout, prompt})', 'type a command, wait for the prompt, return its output'],
  ['session.waitPrompt(ms, pattern?) · waitIdle(ms)', 'wait for the shell prompt / for output to stop'],
  ['session.screen(lines)', 'recent output as plain text'],
  ['session.exec(cmd, ms)', '{stdout, stderr, code} on a separate SSH channel (SSH only)'],
  ['sessions.open(idOrName, {timeout, keepOpen})', 'connect a saved session (closed at the end)'],
  ['sessions.list() · sessions.get(id) · connections.list()', 'running sessions / saved connections'],
  ['log(...) · console.warn(...) · sleep(ms)', 'output and pauses'],
  ['prompt(label, {secret, default}) · confirm(message)', 'ask in the browser'],
  ['vars.name · exit(code)', 'run variables, stop'],
]

export function ApiHelp() {
  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button size="sm" variant="ghost">
          <BookOpen /> API
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-[34rem] max-w-[90vw]">
        <h3 className="mb-2 text-sm font-semibold">Script API</h3>
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
          {API_HELP.map(([k, v]) => (
            <React.Fragment key={k}>
              <dt className="font-mono whitespace-nowrap text-primary">{k}</dt>
              <dd className="text-muted-foreground">{v}</dd>
            </React.Fragment>
          ))}
        </dl>
      </PopoverContent>
    </Popover>
  )
}

function parseVars(text: string): Record<string, string> {
  const out: Record<string, string> = {}
  for (const line of text.split('\n')) {
    const i = line.indexOf('=')
    if (i <= 0) continue
    const k = line.slice(0, i).trim()
    if (k) out[k] = line.slice(i + 1)
  }
  return out
}

type TargetKind = 'none' | 'session' | 'connection'

function Editor({ script, runId: initialRun }: { script: Script; runId?: string }) {
  const qc = useQueryClient()
  const [name, setName] = React.useState(script.name)
  const [description, setDescription] = React.useState(script.description)
  const [content, setContent] = React.useState(script.content)
  const [targetKind, setTargetKind] = React.useState<TargetKind>('none')
  const [sessionId, setSessionId] = React.useState<string>()
  const [connectionId, setConnectionId] = React.useState<string>()
  const [varsText, setVarsText] = React.useState('')
  const [timeout, setTimeoutSec] = React.useState<number | null>(600)
  const [runId, setRunId] = React.useState<string | undefined>(initialRun)
  const [jobId, setJobId] = React.useState<string>()
  const [saving, setSaving] = React.useState(false)
  const { data: sessions } = useSessions(targetKind === 'session')
  const { data: conns } = useConnections(targetKind === 'connection')
  const { data: runs } = useRuns({ kind: 'script', refId: script.id })
  const dirty = name !== script.name || description !== script.description || content !== script.content

  const save = React.useCallback(async () => {
    if (!name.trim()) {
      toast.error('The script needs a name')
      return
    }
    setSaving(true)
    try {
      await updateScript(script.id, { name: name.trim(), description, content })
      await qc.invalidateQueries({ queryKey: autoKeys.scripts })
    } catch (err) {
      toast.error('Could not save the script', { description: errorMessage(err) })
    } finally {
      setSaving(false)
    }
  }, [qc, script.id, name, description, content])

  const run = async () => {
    try {
      const r = await runScript(script.id, {
        content: dirty ? content : undefined,
        sessionId: targetKind === 'session' ? sessionId : undefined,
        connectionId: targetKind === 'connection' ? connectionId : undefined,
        variables: parseVars(varsText),
        timeoutSec: timeout ?? undefined,
      })
      setRunId(r.runId)
      setJobId(r.jobId)
    } catch (err) {
      toast.error('The script could not start', { description: errorMessage(err) })
    }
  }

  const remove = async () => {
    if (!(await confirm({ title: `Delete “${script.name}”?`, destructive: true, confirmLabel: 'Delete' }))) return
    try {
      await deleteScript(script.id)
      await qc.invalidateQueries({ queryKey: autoKeys.scripts })
    } catch (err) {
      toast.error('Could not delete the script', { description: errorMessage(err) })
    }
  }

  const duplicate = async () => {
    try {
      await createScript({ name: `${name} (copy)`.slice(0, 200), description, content })
      await qc.invalidateQueries({ queryKey: autoKeys.scripts })
    } catch (err) {
      toast.error('Could not duplicate the script', { description: errorMessage(err) })
    }
  }

  const liveSessions = (sessions ?? []).filter((s) => s.kind === 'terminal' && isSessionRunning(s))
  const termConns = (conns ?? []).filter((c) => !['sftp', 'ftp', 's3', 'vnc', 'rdp', 'web'].includes(c.protocol))
  const canRun = targetKind === 'none' || (targetKind === 'session' && !!sessionId) || (targetKind === 'connection' && !!connectionId)

  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col gap-2 overflow-y-auto p-3">
      <div className="flex flex-wrap items-center gap-2">
        <Input className="max-w-72 min-w-32 flex-1 font-medium" value={name} onChange={(e) => setName(e.target.value)} aria-label="Script name" maxLength={200} />
        <Input className="max-w-md min-w-40 flex-1" value={description} onChange={(e) => setDescription(e.target.value)} placeholder="Description" aria-label="Description" />
        <span className="ml-auto flex items-center gap-1">
          <ApiHelp />
          <Button size="sm" variant="ghost" onClick={() => void duplicate()}>
            <CopyPlus /> Duplicate
          </Button>
          <Button size="sm" variant="ghost" onClick={() => void remove()}>
            <Trash2 /> Delete
          </Button>
          <Button size="sm" variant="secondary" onClick={() => void save()} loading={saving} disabled={!dirty}>
            <Save /> Save
          </Button>
        </span>
      </div>
      <div className="relative min-h-40 flex-[3] overflow-hidden rounded-md border">
        <LazyBoundary label="Loading editor">
          <CodeEditor value={content} onChange={setContent} onSave={() => void save()} className="h-full [&_.cm-editor]:h-full" />
        </LazyBoundary>
      </div>
      <div className="flex flex-wrap items-end gap-3">
        <Field label="Run against" className="w-44">
          <SimpleSelect<TargetKind>
            size="sm"
            value={targetKind}
            onValueChange={setTargetKind}
            options={[
              { value: 'none', label: 'No session' },
              { value: 'session', label: 'A running session' },
              { value: 'connection', label: 'A saved connection' },
            ]}
          />
        </Field>
        {targetKind === 'session' && (
          <Field label="Session" className="w-64">
            <SimpleSelect size="sm" value={sessionId} onValueChange={setSessionId} placeholder={liveSessions.length ? 'Choose…' : 'No running sessions'} options={liveSessions.map((s) => ({ value: s.id, label: s.title }))} />
          </Field>
        )}
        {targetKind === 'connection' && (
          <Field label="Connection" className="w-64">
            <SimpleSelect size="sm" value={connectionId} onValueChange={setConnectionId} placeholder="Choose…" options={termConns.map((c) => ({ value: c.id, label: c.name }))} />
          </Field>
        )}
        <Field label="Timeout" className="w-28">
          <NumberInput inputSize="sm" value={timeout} min={1} max={86400} unit="s" onChange={setTimeoutSec} />
        </Field>
        <Field label="Variables (name=value per line)" className="min-w-48 flex-1">
          <Textarea mono rows={1} value={varsText} onChange={(e) => setVarsText(e.target.value)} className="min-h-7 py-1" placeholder="host=10.0.0.1" />
        </Field>
        <Button onClick={() => void run()} disabled={!canRun || !content.trim()}>
          <Play /> Run{dirty ? ' (unsaved)' : ''}
        </Button>
      </div>
      <div className="flex min-h-0 flex-[2] gap-3">
        <div className="flex min-h-0 min-w-0 flex-1 flex-col">
          {runId ? <RunView runId={runId} jobId={jobId} className="min-h-0 flex-1" /> : <p className="text-sm text-muted-foreground">Run the script to see its output here.</p>}
        </div>
        {!!runs?.length && (
          <aside className="hidden w-56 shrink-0 flex-col gap-1 overflow-y-auto border-l pl-3 lg:flex" aria-label="Recent runs">
            <h3 className="text-xs font-semibold text-muted-foreground uppercase">Recent runs</h3>
            {runs.slice(0, 30).map((r) => (
              <button
                key={r.id}
                type="button"
                onClick={() => {
                  setRunId(r.id)
                  setJobId(undefined)
                }}
                className={cn('flex items-center gap-2 rounded-md px-1.5 py-1 text-left text-xs outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/60', r.id === runId && 'bg-accent')}
              >
                <StatusBadge status={r.status} />
                <span className="truncate text-muted-foreground">{formatRelativeTime(r.startedAt)}</span>
              </button>
            ))}
          </aside>
        )}
      </div>
    </div>
  )
}

export default function ScriptsPage({ scriptId, runId }: { scriptId?: string; runId?: string }) {
  const qc = useQueryClient()
  const caps = useCapabilities()
  const scripts = useScripts()
  const { data } = scripts
  const [selected, setSelected] = React.useState<string | undefined>(scriptId)
  const [q, setQ] = React.useState('')
  React.useEffect(() => {
    if (scriptId) setSelected(scriptId)
  }, [scriptId])
  const list = (data ?? []).filter((s) => !q.trim() || `${s.name} ${s.description}`.toLowerCase().includes(q.trim().toLowerCase()))
  const current = data?.find((s) => s.id === selected) ?? (selected ? undefined : data?.[0])

  const create = async () => {
    try {
      const s = await createScript({ name: `Script ${(data?.length ?? 0) + 1}`, content: TEMPLATE })
      await qc.invalidateQueries({ queryKey: autoKeys.scripts })
      setSelected(s.id)
    } catch (err) {
      toast.error('Could not create the script', { description: errorMessage(err) })
    }
  }

  if (caps.data && !caps.data.scripts) {
    return (
      <EmptyState
        icon={ShieldOff}
        title="Scripts are restricted to administrators"
        description="Scripts run inside the AstraTerm server. An administrator can allow them for everyone in Settings → Highlighting & triggers."
      />
    )
  }
  return (
    <div className="flex h-full min-h-0 flex-col @3xl:flex-row">
      <aside className="flex max-h-44 w-full shrink-0 flex-col border-b @3xl:max-h-none @3xl:w-60 @3xl:border-r @3xl:border-b-0" aria-label="Scripts">
        <div className="flex items-center gap-1 border-b p-2">
          <Input inputSize="sm" leading={<Search />} placeholder="Search scripts…" value={q} onChange={(e) => setQ(e.target.value)} aria-label="Search scripts" />
          <Button size="icon-sm" variant="ghost" aria-label="New script" onClick={() => void create()}>
            <Plus />
          </Button>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto p-1">
          <QueryState query={scripts} skeleton={<SkeletonRows rows={6} icon={false} />} errorTitle="Could not load the scripts">
            {(all) =>
              !list.length ? (
                <p className="p-2 text-sm text-muted-foreground">{all.length ? 'Nothing matches.' : 'No scripts yet.'}</p>
              ) : (
                list.map((s) => (
                  <button
                    key={s.id}
                    type="button"
                    onClick={() => setSelected(s.id)}
                    className={cn(
                      'flex w-full flex-col rounded-md px-2 py-1.5 text-left outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/60',
                      current?.id === s.id && 'bg-accent',
                    )}
                  >
                    <span className="truncate font-medium">{s.name}</span>
                    {s.description && <span className="truncate text-xs text-muted-foreground">{s.description}</span>}
                  </button>
                ))
              )
            }
          </QueryState>
        </div>
      </aside>
      {!data ? (
        <div className="flex-1" /> // first load: the list shows the placeholder, no empty-state flash here
      ) : current ? (
        <Editor key={current.id} script={current} runId={current.id === scriptId ? runId : undefined} />
      ) : (
        <EmptyState
          className="flex-1"
          icon={Code2}
          title="Automate with JavaScript"
          description="Scripts drive sessions with expect/send, run commands on many hosts and ask you for input — all inside the backend, so they keep running in background tabs."
          action={
            <Button onClick={() => void create()}>
              <Plus /> New script
            </Button>
          }
        />
      )}
    </div>
  )
}
