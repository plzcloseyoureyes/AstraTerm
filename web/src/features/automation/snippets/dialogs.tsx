/*
 * Snippet dialogs: the variables prompt ({{name}} placeholders), the dangerous-command confirmation and the snippet
 * editor. Rendered by the feature overlay.
 */
import * as React from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Braces, CornerDownLeft, ShieldAlert, TextCursorInput } from 'lucide-react'
import { toast } from 'sonner'
import type { Snippet } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { PasswordInput } from '@/components/ui/password-input'
import { SegmentedControl } from '@/components/ui/segmented-control'
import { SimpleSelect } from '@/components/ui/select'
import { TagInput } from '@/components/ui/tag-input'
import { Textarea } from '@/components/ui/textarea'
import { errorMessage, plural } from '@/lib/utils'
import { autoKeys, createSnippet, updateSnippet, useSnippets } from '../api'
import { ShortcutInput } from '../components/pickers'
import { automationSettings } from '../settings'
import { closeSnippetEditor, finishDangerous, finishVariables, useAutomationUI, type DangerRequest, type VariablesRequest } from '../store'
import { templateVars } from '../template'

// ---------------------------------------------------------------------------------------------------------------------
// Variables
// ---------------------------------------------------------------------------------------------------------------------

function VariablesDialog({ req }: { req: VariablesRequest }) {
  const [values, setValues] = React.useState<Record<string, string>>(() => {
    const init: Record<string, string> = {}
    for (const v of req.vars) init[v.name] = req.initial[v.name] ?? v.default ?? ''
    return init
  })
  const done = React.useRef(false)
  const finish = (v: Record<string, string> | null) => {
    if (done.current) return
    done.current = true
    finishVariables(req.id, v)
  }
  return (
    <Dialog open onOpenChange={(o) => !o && finish(null)}>
      <DialogContent size="md">
        <form
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault()
            finish(values)
          }}
        >
          <DialogHeader>
            <DialogTitle>
              <Braces className="size-4 text-primary" /> {req.title}
            </DialogTitle>
            <DialogDescription>Fill in the snippet's {plural(req.vars.length, 'value')}.</DialogDescription>
          </DialogHeader>
          <DialogBody className="flex max-h-[55vh] flex-col gap-3">
            {req.vars.map((v, i) => (
              <Field key={v.name} label={<span className="font-mono">{v.name}</span>}>
                {v.choices?.length ? (
                  <SimpleSelect
                    value={values[v.name] || v.choices[0]}
                    onValueChange={(x) => setValues((s) => ({ ...s, [v.name]: x }))}
                    options={v.choices.map((c) => ({ value: c, label: c }))}
                  />
                ) : v.secret ? (
                  <PasswordInput autoFocus={i === 0} value={values[v.name]} onChange={(e) => setValues((s) => ({ ...s, [v.name]: e.target.value }))} autoComplete="off" />
                ) : (
                  <Input autoFocus={i === 0} value={values[v.name]} placeholder={v.default} onChange={(e) => setValues((s) => ({ ...s, [v.name]: e.target.value }))} spellCheck={false} autoComplete="off" />
                )}
              </Field>
            ))}
          </DialogBody>
          <DialogFooter>
            <Button type="button" variant="secondary" onClick={() => finish(null)}>
              Cancel
            </Button>
            <Button type="submit">Send</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

export function VariablesDialogs() {
  const reqs = useAutomationUI((s) => s.variables)
  const first = reqs[0]
  return first ? <VariablesDialog key={first.id} req={first} /> : null
}

// ---------------------------------------------------------------------------------------------------------------------
// Dangerous command confirmation (Cancel is the default action)
// ---------------------------------------------------------------------------------------------------------------------

function DangerDialog({ req }: { req: DangerRequest }) {
  const [disable, setDisable] = React.useState(false)
  const done = React.useRef(false)
  const cancelRef = React.useRef<HTMLButtonElement>(null)
  const finish = (ok: boolean) => {
    if (done.current) return
    done.current = true
    if (ok && disable) automationSettings.set({ guardEnabled: false })
    finishDangerous(req.id, ok)
  }
  const n = req.targets.length
  return (
    <Dialog open onOpenChange={(o) => !o && finish(false)}>
      <DialogContent
        size="md"
        onOpenAutoFocus={(e) => {
          e.preventDefault()
          cancelRef.current?.focus()
        }}
      >
        <DialogHeader>
          <DialogTitle>
            <ShieldAlert className="size-4 text-destructive" /> Potentially dangerous command
          </DialogTitle>
          <DialogDescription>{n > 1 ? `It would be sent to ${plural(n, 'session')}.` : 'Review it before it runs.'}</DialogDescription>
        </DialogHeader>
        <div className="flex flex-col gap-1.5">
          {req.matches.slice(0, 6).map((m, i) => (
            <div key={i} className="flex flex-col gap-0.5 rounded-md border border-destructive/30 bg-destructive/8 px-2.5 py-1.5">
              <span className="flex items-center gap-1.5 text-sm font-medium">
                {m.message}
                {m.severity === 'warning' && <Badge variant="warning">strict</Badge>}
              </span>
              <code className="truncate font-mono text-xs text-muted-foreground" title={m.line}>
                {m.line}
              </code>
            </div>
          ))}
        </div>
        {n > 0 && (
          <p className="text-sm text-muted-foreground">
            {n > 1 ? 'Targets: ' : 'Target: '}
            <span className="text-foreground">
              {req.targets.slice(0, 8).join(', ')}
              {n > 8 ? ` and ${n - 8} more` : ''}
            </span>
          </p>
        )}
        <CheckboxField label="Don't check commands any more" description="Turns the dangerous-command guard off (Settings → Highlighting & triggers)." checked={disable} onCheckedChange={(v) => setDisable(v === true)} />
        <DialogFooter>
          <Button ref={cancelRef} variant="secondary" onClick={() => finish(false)}>
            Cancel
          </Button>
          <Button variant="destructive" onClick={() => finish(true)}>
            {n > 1 ? `Run on ${n} sessions` : 'Run anyway'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export function DangerDialogs() {
  const reqs = useAutomationUI((s) => s.danger)
  const first = reqs[0]
  return first ? <DangerDialog key={first.id} req={first} /> : null
}

// ---------------------------------------------------------------------------------------------------------------------
// Snippet editor
// ---------------------------------------------------------------------------------------------------------------------

function SnippetEditor({ snippet, initial }: { snippet?: Snippet; initial?: Partial<Snippet> }) {
  const qc = useQueryClient()
  const { data: all } = useSnippets()
  const src = snippet ?? initial ?? {}
  const [name, setName] = React.useState(src.name ?? '')
  const [folder, setFolder] = React.useState(src.folder ?? '')
  const [description, setDescription] = React.useState(src.description ?? '')
  const [content, setContent] = React.useState(src.content ?? '')
  const [tags, setTags] = React.useState<string[]>(src.tags ?? [])
  const [sendMode, setSendMode] = React.useState<Snippet['sendMode']>(src.sendMode ?? 'execute')
  const [shortcut, setShortcut] = React.useState(src.shortcut ?? '')
  const [saving, setSaving] = React.useState(false)
  const [error, setError] = React.useState<string | null>(null)
  const folders = React.useMemo(() => Array.from(new Set((all ?? []).map((s) => s.folder).filter(Boolean))).sort(), [all])
  const tagSuggestions = React.useMemo(() => Array.from(new Set((all ?? []).flatMap((s) => s.tags))).sort(), [all])
  const vars = React.useMemo(() => templateVars(content), [content])
  const folderListId = React.useId()
  const nameMissing = !name.trim()

  const save = async () => {
    if (nameMissing) {
      setError('Give the snippet a name.')
      return
    }
    setSaving(true)
    setError(null)
    const body = { name: name.trim(), folder, description, content, tags, sendMode, shortcut }
    try {
      if (snippet) await updateSnippet(snippet.id, body)
      else await createSnippet(body)
      await qc.invalidateQueries({ queryKey: autoKeys.snippets })
      toast.success(snippet ? 'Snippet saved' : 'Snippet created')
      closeSnippetEditor()
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && closeSnippetEditor()}>
      <DialogContent size="xl">
        <form
          className="flex min-h-0 flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault()
            void save()
          }}
        >
          <DialogHeader>
            <DialogTitle>
              <TextCursorInput className="size-4 text-primary" /> {snippet ? 'Edit snippet' : 'New snippet'}
            </DialogTitle>
            <DialogDescription>
              Use <code className="font-mono">{'{{name}}'}</code>, <code className="font-mono">{'{{name|default}}'}</code>,{' '}
              <code className="font-mono">{'{{env|dev|prod}}'}</code> or <code className="font-mono">{'{{pw:secret}}'}</code> for values asked when sending;{' '}
              <code className="font-mono">{'{{host}} {{user}} {{date}} {{clipboard}}'}</code> are filled in automatically.
            </DialogDescription>
          </DialogHeader>
          <DialogBody className="flex flex-col gap-3">
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="Name" required error={error && nameMissing ? error : undefined}>
                <Input autoFocus value={name} onChange={(e) => setName(e.target.value)} maxLength={200} />
              </Field>
              <Field label="Folder" hint="Use / for sub-folders">
                <Input value={folder} onChange={(e) => setFolder(e.target.value)} list={folderListId} placeholder="e.g. linux/disks" maxLength={200} />
              </Field>
              <datalist id={folderListId}>
                {folders.map((f) => (
                  <option key={f} value={f} />
                ))}
              </datalist>
            </div>
            <Field label="Command / text">
              <Textarea mono value={content} onChange={(e) => setContent(e.target.value)} rows={8} spellCheck={false} placeholder="df -h {{path|/}}" className="min-h-40 resize-y" />
            </Field>
            {vars.length > 0 && (
              <div className="flex flex-wrap items-center gap-1.5 text-sm text-muted-foreground">
                Placeholders:
                {vars.map((v) => (
                  <Badge key={v.name} variant={v.builtin ? 'outline' : 'default'} className="font-mono">
                    {v.name}
                    {v.secret ? ' 🔒' : ''}
                    {v.choices?.length ? ` (${v.choices.length} choices)` : v.default ? ` = ${v.default}` : ''}
                  </Badge>
                ))}
              </div>
            )}
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="When sent">
                <SegmentedControl
                  value={sendMode}
                  onValueChange={setSendMode}
                  aria-label="Send mode"
                  options={[
                    { value: 'execute', label: 'Run (press Enter)', icon: CornerDownLeft },
                    { value: 'paste', label: 'Paste only' },
                  ]}
                />
              </Field>
              <Field label="Keyboard shortcut" hint="Sends to the active terminal">
                <ShortcutInput value={shortcut} onChange={setShortcut} />
              </Field>
            </div>
            <Field label="Description">
              <Input value={description} onChange={(e) => setDescription(e.target.value)} placeholder="What does it do?" />
            </Field>
            <Field label="Tags">
              <TagInput value={tags} onChange={setTags} suggestions={tagSuggestions} />
            </Field>
            {error && !nameMissing && (
              <p role="alert" className="text-sm text-destructive">
                {error}
              </p>
            )}
          </DialogBody>
          <DialogFooter>
            <Button type="button" variant="secondary" onClick={() => closeSnippetEditor()}>
              Cancel
            </Button>
            <Button type="submit" loading={saving}>
              {snippet ? 'Save' : 'Create'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

export function SnippetEditorDialog() {
  const st = useAutomationUI((s) => s.snippetEditor)
  if (!st) return null
  return <SnippetEditor key={st.snippet?.id ?? 'new'} snippet={st.snippet} initial={st.initial} />
}
