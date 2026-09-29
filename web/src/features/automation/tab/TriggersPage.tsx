/*
 * Triggers (AUTO-7): a regular expression over the plain-text output of your sessions — or a session event (connected,
 * disconnected, a command finished via OSC 133 shell integration) — fires actions: highlight (in the browser),
 * notify, sound, send text or a stored secret, log the line, run a snippet or a script. Evaluated by the backend
 * (RE2), so they also work with no browser attached. Rate-limited per rule (cooldown, once per connection) with a
 * loop guard that pauses a rule whose own input keeps re-triggering it.
 */
import * as React from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Bell, Eraser, FlaskConical, MoreHorizontal, Pencil, Plug, PlugZap, Plus, ScrollText, SquareTerminal, TextSearch, Trash2, Zap } from 'lucide-react'
import { toast } from 'sonner'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { confirm } from '@/components/ui/dialog-host'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { EmptyState } from '@/components/ui/empty-state'
import { Field } from '@/components/ui/field'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { QueryState } from '@/components/ui/query-state'
import { SimpleSelect } from '@/components/ui/select'
import { SkeletonRows, SkeletonText } from '@/components/ui/skeleton'
import { Switch } from '@/components/ui/switch'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { TagInput } from '@/components/ui/tag-input'
import { Textarea } from '@/components/ui/textarea'
import { cn, errorMessage, formatDateTime, formatRelativeTime } from '@/lib/utils'
import {
  autoKeys,
  clearTriggerLog,
  dangerousMatches,
  createTrigger,
  deleteTrigger,
  regexTest,
  updateTrigger,
  useCapabilities,
  useScripts,
  useSnippets,
  useTriggerLog,
  useTriggers,
} from '../api'
import { ColorSelect, ConnectionMultiSelect, cssColor } from '../components/pickers'
import { playSound, SOUNDS } from '../sound'
import { askDangerous } from '../store'
import type { RegexTestResult, Trigger, TriggerAction, TriggerActionType, TriggerEventKind, TriggerInput } from '../types'

const EVENTS: { value: TriggerEventKind; label: string; icon: typeof Zap; hint: string }[] = [
  { value: 'output', label: 'Output matches', icon: TextSearch, hint: 'Every line of output (prompts without a newline too), without colours.' },
  { value: 'command', label: 'A command finished', icon: SquareTerminal, hint: 'Needs shell integration (OSC 133 prompt marks, e.g. from your shell or prompt theme).' },
  { value: 'connect', label: 'Session connected', icon: PlugZap, hint: 'After every connect and reconnect.' },
  { value: 'disconnect', label: 'Connection dropped', icon: Plug, hint: 'When a connected session ends or loses its connection (not when you close it).' },
]

const INPUT_ACTIONS: TriggerActionType[] = ['send', 'runSnippet', 'runScript']

function eventLabel(t: Trigger): string {
  switch (t.event) {
    case 'command':
      return t.exit === 'error' ? 'command failed' : t.exit === 'ok' ? 'command succeeded' : 'command finished'
    case 'connect':
      return 'connected'
    case 'disconnect':
      return 'connection dropped'
  }
  return ''
}

const ACTION_TYPES: { value: TriggerActionType; label: string }[] = [
  { value: 'highlight', label: 'Highlight the text' },
  { value: 'notify', label: 'Show a notification' },
  { value: 'sound', label: 'Play a sound' },
  { value: 'send', label: 'Send text / a secret' },
  { value: 'log', label: 'Log the line' },
  { value: 'runSnippet', label: 'Run a snippet' },
  { value: 'runScript', label: 'Run a script' },
]

function describeAction(a: TriggerAction): string {
  switch (a.type) {
    case 'highlight':
      return 'highlight'
    case 'notify':
      return 'notify'
    case 'sound':
      return `sound (${a.sound ?? 'beep'})`
    case 'send':
      return a.secret ? `send secret ${a.secret}` : 'send text'
    case 'log':
      return 'log'
    case 'runSnippet':
      return 'snippet'
    case 'runScript':
      return 'script'
  }
  return a.type
}

function actionTypesFor(event: TriggerEventKind) {
  return ACTION_TYPES.filter((t) => (t.value !== 'highlight' || event === 'output') && (event !== 'disconnect' || !INPUT_ACTIONS.includes(t.value)))
}

function ActionEditor({ a, event, onChange, onRemove }: { a: TriggerAction; event: TriggerEventKind; onChange: (a: TriggerAction) => void; onRemove: () => void }) {
  const { data: snippets } = useSnippets(a.type === 'runSnippet')
  const { data: scripts } = useScripts(a.type === 'runScript')
  const invalid = !actionTypesFor(event).some((t) => t.value === a.type)
  return (
    <li className={cn('flex flex-col gap-2 rounded-md border bg-card p-2', invalid && 'border-destructive/50')}>
      <div className="flex items-center gap-2">
        <SimpleSelect<TriggerActionType> size="sm" className="w-56" value={a.type} onValueChange={(type) => onChange({ type })} options={actionTypesFor(event)} aria-label="Action" />
        {invalid && <span className="text-xs text-destructive">Not available for this event</span>}
        <IconButton icon={Trash2} label="Remove action" size="xs" className="ml-auto" onClick={onRemove} />
      </div>
      {a.type === 'highlight' && (
        <div className="flex flex-wrap items-center gap-3 text-sm">
          <span className="flex items-center gap-1.5">
            Text <ColorSelect value={a.color} onChange={(color) => onChange({ ...a, color })} allowNone aria-label="Text colour" />
          </span>
          <span className="flex items-center gap-1.5">
            Background <ColorSelect value={a.background} onChange={(background) => onChange({ ...a, background })} allowNone aria-label="Background colour" />
          </span>
          <CheckboxField label="Underline" checked={!!a.underline} onCheckedChange={(v) => onChange({ ...a, underline: v === true || undefined })} />
        </div>
      )}
      {a.type === 'notify' && (
        <div className="grid gap-2 sm:grid-cols-2">
          <Input inputSize="sm" value={a.title ?? ''} onChange={(e) => onChange({ ...a, title: e.target.value })} placeholder="Title ($0 = match, $1… = groups)" aria-label="Notification title" />
          <Input inputSize="sm" value={a.message ?? ''} onChange={(e) => onChange({ ...a, message: e.target.value })} placeholder="Message (optional)" aria-label="Notification message" />
          <SimpleSelect
            size="sm"
            value={a.level ?? 'info'}
            onValueChange={(level) => onChange({ ...a, level })}
            options={[
              { value: 'info', label: 'Info' },
              { value: 'success', label: 'Success' },
              { value: 'warning', label: 'Warning' },
              { value: 'error', label: 'Error' },
            ]}
            aria-label="Level"
          />
          <CheckboxField label="Desktop notification when Termstead is in the background" checked={!!a.desktop} onCheckedChange={(v) => onChange({ ...a, desktop: v === true })} />
        </div>
      )}
      {a.type === 'sound' && (
        <div className="flex items-center gap-2">
          <SimpleSelect size="sm" className="w-40" value={a.sound ?? 'beep'} onValueChange={(sound) => onChange({ ...a, sound })} options={SOUNDS.map((s) => ({ value: s.id, label: s.label }))} aria-label="Sound" />
          <Button size="xs" variant="ghost" onClick={() => playSound(a.sound ?? 'beep')}>
            Test
          </Button>
        </div>
      )}
      {a.type === 'send' && (
        <div className="flex flex-wrap items-center gap-2">
          <SimpleSelect<'text' | 'secret'>
            size="sm"
            className="w-36"
            value={a.secret !== undefined ? 'secret' : 'text'}
            onValueChange={(m) => onChange(m === 'secret' ? { ...a, secret: 'password', text: undefined } : { ...a, secret: undefined, text: '' })}
            options={[
              { value: 'text', label: 'Text' },
              { value: 'secret', label: 'Stored secret' },
            ]}
            aria-label="Send what"
          />
          {a.secret !== undefined ? (
            <Input inputSize="sm" className="w-48 font-mono" value={a.secret} onChange={(e) => onChange({ ...a, secret: e.target.value })} placeholder="password" aria-label="Secret name" />
          ) : (
            <Input inputSize="sm" className="min-w-48 flex-1 font-mono" value={a.text ?? ''} onChange={(e) => onChange({ ...a, text: e.target.value })} placeholder="y   (escapes: \r \t \x03)" aria-label="Text" spellCheck={false} />
          )}
          <CheckboxField label="Press Enter" checked={!!a.enter} onCheckedChange={(v) => onChange({ ...a, enter: v === true })} />
        </div>
      )}
      {a.type === 'runSnippet' && (
        <SimpleSelect size="sm" value={a.snippetId} onValueChange={(snippetId) => onChange({ ...a, snippetId })} placeholder="Choose a snippet" options={(snippets ?? []).map((s) => ({ value: s.id, label: s.name }))} aria-label="Snippet" />
      )}
      {a.type === 'runScript' && (
        <SimpleSelect size="sm" value={a.scriptId} onValueChange={(scriptId) => onChange({ ...a, scriptId })} placeholder="Choose a script" options={(scripts ?? []).map((s) => ({ value: s.id, label: s.name }))} aria-label="Script" />
      )}
      {a.type === 'log' && <p className="text-xs text-muted-foreground">The matching line is stored in the trigger log (last 1000 lines).</p>}
    </li>
  )
}

function TriggerEditor({ trigger, onClose }: { trigger?: Trigger; onClose: () => void }) {
  const qc = useQueryClient()
  const caps = useCapabilities()
  const [name, setName] = React.useState(trigger?.name ?? '')
  const [event, setEvent] = React.useState<TriggerEventKind>(trigger?.event ?? 'output')
  const [exit, setExit] = React.useState<'any' | 'ok' | 'error'>(trigger?.exit ?? 'any')
  const [minDuration, setMinDuration] = React.useState<number | null>(trigger?.minDurationSec ?? 0)
  const [pattern, setPattern] = React.useState(trigger?.pattern ?? '')
  const [caseSensitive, setCaseSensitive] = React.useState(trigger?.caseSensitive ?? false)
  const [actions, setActions] = React.useState<TriggerAction[]>(trigger?.actions ?? [{ type: 'notify', level: 'warning' }])
  const [cooldown, setCooldown] = React.useState<number | null>(trigger?.cooldownMs ?? 2000)
  const [once, setOnce] = React.useState(trigger?.once ?? false)
  const [conns, setConns] = React.useState<string[]>(trigger?.scope.connectionIds ?? [])
  const [protocols, setProtocols] = React.useState<string[]>(trigger?.scope.protocols ?? [])
  const [tags, setTags] = React.useState<string[]>(trigger?.scope.tags ?? [])
  const [scoped, setScoped] = React.useState(!!(conns.length || protocols.length || tags.length))
  const [sample, setSample] = React.useState('')
  const [test, setTest] = React.useState<RegexTestResult | null>(null)
  const [saving, setSaving] = React.useState(false)

  React.useEffect(() => {
    if (!pattern.trim()) {
      setTest(null)
      return
    }
    let alive = true
    const t = setTimeout(() => {
      regexTest(pattern, caseSensitive, sample)
        .then((r) => alive && setTest(r))
        .catch(() => undefined)
    }, 300)
    return () => {
      alive = false
      clearTimeout(t)
    }
  }, [pattern, caseSensitive, sample])

  const save = async () => {
    setSaving(true)
    const body: TriggerInput = {
      name: name.trim(),
      event,
      exit: event === 'command' ? exit : undefined,
      minDurationSec: event === 'command' ? (minDuration ?? 0) : undefined,
      pattern,
      caseSensitive,
      actions,
      cooldownMs: cooldown ?? 2000,
      once,
      scope: scoped ? { connectionIds: conns, protocols: protocols.map((p) => p.toLowerCase()), tags } : {},
    }
    try {
      for (;;) {
        try {
          if (trigger) await updateTrigger(trigger.id, body)
          else await createTrigger(body)
          break
        } catch (err) {
          const matches = dangerousMatches(err)
          if (!matches || body.confirmDangerous) throw err
          // A "send" action types a dangerous command: confirm it once, at save time.
          if (!(await askDangerous(matches, ['every session this trigger fires in']))) return
          body.confirmDangerous = true
        }
      }
      await qc.invalidateQueries({ queryKey: autoKeys.triggers })
      toast.success(trigger ? 'Trigger saved' : 'Trigger created')
      onClose()
    } catch (err) {
      toast.error('Could not save the trigger', { description: errorMessage(err) })
    } finally {
      setSaving(false)
    }
  }
  const scriptBlocked = actions.some((a) => a.type === 'runScript') && caps.data && !caps.data.scripts
  const actionsValid = actions.every((a) => actionTypesFor(event).some((t) => t.value === a.type))
  const needsPattern = event === 'output'
  const ready = !!name.trim() && (!needsPattern || !!pattern.trim()) && test?.valid !== false && actions.length > 0 && actionsValid && !scriptBlocked
  const eventInfo = EVENTS.find((e) => e.value === event)!

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="2xl" className="max-h-[92vh]">
        <DialogHeader>
          <DialogTitle>
            <Zap className="size-4 text-primary" /> {trigger ? 'Edit trigger' : 'New trigger'}
          </DialogTitle>
          <DialogDescription>{eventInfo.hint}</DialogDescription>
        </DialogHeader>
        <DialogBody className="flex flex-col gap-3">
          <div className="grid gap-3 sm:grid-cols-[1fr_2fr]">
            <Field label="Name" required>
              <Input autoFocus value={name} onChange={(e) => setName(e.target.value)} maxLength={200} />
            </Field>
            <Field label="When">
              <SimpleSelect<TriggerEventKind>
                value={event}
                onValueChange={setEvent}
                options={EVENTS.map((e) => ({
                  value: e.value,
                  label: (
                    <span className="flex items-center gap-1.5">
                      <e.icon className="size-3.5 text-muted-foreground" /> {e.label}
                    </span>
                  ),
                }))}
                aria-label="When the trigger fires"
              />
            </Field>
          </div>
          {event === 'command' && (
            <div className="flex flex-wrap items-center gap-3 text-sm">
              <span className="flex items-center gap-1.5">
                Exit code
                <SimpleSelect<'any' | 'ok' | 'error'>
                  size="sm"
                  className="w-40"
                  value={exit}
                  onValueChange={setExit}
                  options={[
                    { value: 'any', label: 'Any' },
                    { value: 'error', label: 'Failed (not 0)' },
                    { value: 'ok', label: 'Succeeded (0)' },
                  ]}
                  aria-label="Exit code"
                />
              </span>
              <span className="flex items-center gap-1.5">
                Ran at least
                <span className="w-24">
                  <NumberInput inputSize="sm" value={minDuration} min={0} max={604800} unit="s" onChange={setMinDuration} aria-label="Minimum duration" />
                </span>
              </span>
              <span className="text-xs text-muted-foreground">Tip: 10 s + “notify” = know when a long build is done.</span>
            </div>
          )}
          <Field
            label={needsPattern ? 'Pattern (regular expression, RE2 syntax)' : event === 'command' ? 'Command pattern (optional)' : event === 'connect' ? 'Session title pattern (optional)' : 'Message pattern (optional)'}
            required={needsPattern}
            error={test && !test.valid ? test.error : undefined}
          >
            <Input
              className="font-mono"
              value={pattern}
              onChange={(e) => setPattern(e.target.value)}
              placeholder={needsPattern ? String.raw`disk (\S+) is (\d+)% full` : event === 'command' ? String.raw`^(make|npm run build|cargo build)\b` : 'any'}
              spellCheck={false}
            />
          </Field>
          <div className="flex flex-wrap items-center gap-4">
            <CheckboxField label="Case-sensitive" checked={caseSensitive} onCheckedChange={(v) => setCaseSensitive(v === true)} />
            <CheckboxField label="Once per connection" checked={once} onCheckedChange={(v) => setOnce(v === true)} />
            <span className="flex items-center gap-1.5 text-sm">
              Cooldown
              <span className="w-28">
                <NumberInput inputSize="sm" value={cooldown} min={250} max={86400000} step={500} unit="ms" onChange={setCooldown} aria-label="Cooldown" />
              </span>
            </span>
          </div>
          <Field label="Try it" hint={test?.valid ? `${test.matches.length} matching line${test.matches.length === 1 ? '' : 's'}` : needsPattern ? 'Paste some output to test the pattern' : 'Paste sample text to test the pattern'}>
            <Textarea mono rows={3} value={sample} onChange={(e) => setSample(e.target.value)} placeholder="Paste sample output here" />
          </Field>
          {test?.valid && test.matches.length > 0 && (
            <ul className="flex max-h-28 flex-col gap-0.5 overflow-y-auto rounded-md bg-muted/40 p-2 font-mono text-xs">
              {test.matches.slice(0, 20).map((m, i) => (
                <li key={i} className="truncate">
                  <FlaskConical className="mr-1 inline size-3 text-primary" />
                  <span className="text-primary">{m.match}</span>
                  {m.groups.length > 0 && <span className="text-muted-foreground"> · {m.groups.map((g, j) => `$${j + 1}=${g}`).join(' ')}</span>}
                </li>
              ))}
            </ul>
          )}
          <div className="flex flex-col gap-2">
            <span className="text-sm font-medium">Actions</span>
            <ul className="flex flex-col gap-1.5">
              {actions.map((a, i) => (
                <ActionEditor key={i} a={a} event={event} onChange={(na) => setActions((as) => as.map((x, j) => (j === i ? na : x)))} onRemove={() => setActions((as) => as.filter((_, j) => j !== i))} />
              ))}
            </ul>
            {scriptBlocked && <p className="text-sm text-destructive">Scripts are restricted to administrators on this server.</p>}
            <Button
              size="sm"
              variant="secondary"
              className="self-start"
              disabled={actions.length >= 16}
              onClick={() => setActions((as) => [...as, event === 'output' ? { type: 'highlight', color: 'brightYellow' } : { type: 'notify', level: 'info' }])}
            >
              <Plus /> Add action
            </Button>
          </div>
          <div className="flex flex-col gap-2">
            <label className="flex items-center gap-2 text-sm font-medium">
              <Switch size="sm" checked={scoped} onCheckedChange={setScoped} /> Only for some sessions
            </label>
            {scoped && (
              <div className="grid gap-3 sm:grid-cols-2">
                <div className="flex flex-col gap-3">
                  <Field label="Protocols">
                    <TagInput value={protocols} onChange={setProtocols} placeholder="ssh, telnet…" />
                  </Field>
                  <Field label="Connection tags">
                    <TagInput value={tags} onChange={setTags} placeholder="prod…" />
                  </Field>
                </div>
                <Field label="Saved connections">
                  <ConnectionMultiSelect value={conns} onChange={setConns} height="h-40" />
                </Field>
              </div>
            )}
          </div>
        </DialogBody>
        <DialogFooter>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button onClick={() => void save()} loading={saving} disabled={!ready}>
            {trigger ? 'Save' : 'Create'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function TriggerLog() {
  const qc = useQueryClient()
  const log = useTriggerLog()
  const { data } = log
  return (
    <section className="flex min-h-48 flex-col gap-2" aria-label="Trigger log">
      <div className="flex items-center gap-2">
        <h3 className="flex items-center gap-1.5 text-sm font-semibold">
          <ScrollText className="size-4 text-muted-foreground" /> Trigger log
        </h3>
        <Button
          size="xs"
          variant="ghost"
          className="ml-auto"
          disabled={!data?.length}
          onClick={() =>
            void clearTriggerLog()
              .then(() => qc.invalidateQueries({ queryKey: ['automation', 'trigger-log'] }))
              .catch((err) => toast.error('Could not clear the log', { description: errorMessage(err) }))
          }
        >
          <Eraser /> Clear
        </Button>
      </div>
      <QueryState
        query={log}
        skeleton={<SkeletonText lines={4} className="rounded-md border p-2" />}
        errorTitle="Could not load the trigger log"
        isEmpty={(entries) => !entries.length}
        empty={<p className="text-sm text-muted-foreground">Lines captured by “log” actions appear here.</p>}
      >
        {(entries) => (
          <div className="max-h-72 overflow-y-auto rounded-md border bg-muted/30 p-2 font-mono text-xs">
            {entries.map((e) => (
              <div key={e.id} className="flex gap-2 whitespace-pre-wrap">
                <span className="shrink-0 text-muted-foreground" title={formatDateTime(e.ts)}>
                  {new Date(e.ts).toLocaleTimeString()}
                </span>
                <span className="shrink-0 text-primary">[{e.triggerName}]</span>
                <span className="shrink-0 text-muted-foreground">{e.sessionTitle}</span>
                <span className="break-all">{e.line}</span>
              </div>
            ))}
        </div>
        )}
      </QueryState>
    </section>
  )
}

export default function TriggersPage() {
  const qc = useQueryClient()
  const triggers = useTriggers()
  const [editing, setEditing] = React.useState<Trigger | 'new' | null>(null)
  const toggle = async (t: Trigger, enabled: boolean) => {
    try {
      await updateTrigger(t.id, { enabled })
      await qc.invalidateQueries({ queryKey: autoKeys.triggers })
    } catch (err) {
      toast.error('Could not update the trigger', { description: errorMessage(err) })
    }
  }
  const remove = async (t: Trigger) => {
    if (!(await confirm({ title: `Delete “${t.name}”?`, destructive: true, confirmLabel: 'Delete' }))) return
    try {
      await deleteTrigger(t.id)
      await qc.invalidateQueries({ queryKey: autoKeys.triggers })
    } catch (err) {
      toast.error('Could not delete the trigger', { description: errorMessage(err) })
    }
  }
  return (
    <div className="flex h-full min-h-0 flex-col gap-4 overflow-y-auto p-3">
      <div className="flex items-center gap-2">
        <h2 className="flex items-center gap-2 text-md font-semibold">
          <Zap className="size-4 text-primary" /> Triggers
        </h2>
        <Button size="sm" className="ml-auto" onClick={() => setEditing('new')}>
          <Plus /> New trigger
        </Button>
      </div>
      <QueryState
        query={triggers}
        skeleton={<SkeletonRows rows={4} rowHeight={44} className="rounded-md border" />}
        errorTitle="Could not load triggers"
        isEmpty={(list) => !list.length}
        empty={
          <EmptyState
            icon={Bell}
            title="No triggers"
            description="React to output or events: highlight “ERROR”, get notified when a long command fails, answer “Continue? [y/N]”, log every “segfault”, run a snippet whenever a session connects…"
            action={
              <Button onClick={() => setEditing('new')}>
                <Plus /> New trigger
              </Button>
            }
          />
        }
      >
        {(list) => (
          <Table containerClassName="rounded-md border">
            <TableHeader>
              <TableRow>
                <TableHead className="w-14">On</TableHead>
                <TableHead>Trigger</TableHead>
                <TableHead>Actions</TableHead>
                <TableHead className="w-32">Hits</TableHead>
                <TableHead className="w-12" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {list.map((t) => {
                const hl = t.actions.find((a) => a.type === 'highlight')
                return (
                  <TableRow key={t.id} className={cn(!t.enabled && 'opacity-60')}>
                    <TableCell>
                      <Switch size="sm" checked={t.enabled} onCheckedChange={(v) => void toggle(t, v)} aria-label={`Enable ${t.name}`} />
                    </TableCell>
                    <TableCell>
                      <div className="flex flex-col">
                        <span className="font-medium">{t.name}</span>
                        {t.event && t.event !== 'output' ? (
                          <span className="truncate text-xs text-muted-foreground">
                            On {eventLabel(t)}
                            {t.pattern ? (
                              <>
                                {' · '}
                                <code className="font-mono">{t.pattern}</code>
                              </>
                            ) : null}
                          </span>
                        ) : (
                          <code
                            className="truncate font-mono text-xs"
                            style={{ color: cssColor(hl?.color), background: cssColor(hl?.background), textDecoration: hl?.underline ? 'underline' : undefined }}
                          >
                            {t.pattern}
                          </code>
                        )}
                      </div>
                    </TableCell>
                    <TableCell>
                      <span className="flex flex-wrap gap-1">
                        {t.actions.map((a, i) => (
                          <Badge key={i} variant="outline">
                            {describeAction(a)}
                          </Badge>
                        ))}
                      </span>
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground tabular-nums">
                      {t.stats.hits}
                      {t.stats.lastHitAt ? ` · ${formatRelativeTime(t.stats.lastHitAt)}` : ''}
                    </TableCell>
                    <TableCell>
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button size="icon-xs" variant="ghost" aria-label={`Actions for ${t.name}`}>
                            <MoreHorizontal />
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem onSelect={() => setEditing(t)}>
                            <Pencil /> Edit…
                          </DropdownMenuItem>
                          <DropdownMenuSeparator />
                          <DropdownMenuItem variant="destructive" onSelect={() => void remove(t)}>
                            <Trash2 /> Delete
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
        </Table>
        )}
      </QueryState>
      <TriggerLog />
      {editing && <TriggerEditor trigger={editing === 'new' ? undefined : editing} onClose={() => setEditing(null)} />}
    </div>
  )
}
