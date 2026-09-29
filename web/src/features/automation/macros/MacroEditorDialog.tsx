/*
 * Macro editor (AUTO-1): name, keyboard shortcut and the steps — reorderable with the mouse or the keyboard (dnd-kit).
 * Three kinds of steps, each after its delay:
 *   type     keystrokes with C escapes (\r Enter, \t Tab, \x03 Ctrl+C, \e[A arrow up…)
 *   wait     wait until a pattern (RE2) appears in the session output, then type (optional) text — replays adapt to
 *            slow hosts instead of relying on recorded timing
 *   secret   type a stored secret of the session's connection (never saved in the macro), optionally Enter
 */
import * as React from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { closestCenter, DndContext, KeyboardSensor, PointerSensor, useSensor, useSensors, type DragEndEvent } from '@dnd-kit/core'
import { arrayMove, SortableContext, sortableKeyboardCoordinates, useSortable, verticalListSortingStrategy } from '@dnd-kit/sortable'
import { CSS } from '@dnd-kit/utilities'
import { ChevronDown, Clapperboard, Combine, GripVertical, Hourglass, KeyRound, Keyboard, Plus, Timer, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { prompt } from '@/components/ui/dialog-host'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { Field } from '@/components/ui/field'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { LoadingState } from '@/components/ui/query-state'
import { SimpleSelect } from '@/components/ui/select'
import { errorMessage, formatDuration, plural, uid } from '@/lib/utils'
import { autoKeys, createMacro, updateMacro, useMacros } from '../api'
import { ShortcutInput } from '../components/pickers'
import { describeKeys, escapeText, unescapeText } from '../escapes'
import { secretLabel } from '../plugins/passwordChip'
import { automationSettings } from '../settings'
import { closeMacroEditor, useAutomationUI } from '../store'
import type { MacroStep } from '../types'

type StepKind = 'type' | 'wait' | 'secret'

interface Row {
  key: string
  kind: StepKind
  text: string // escaped
  delayMs: number
  waitFor: string
  timeoutMs: number | null
  secret: string
  enter: boolean
}

const SECRET_KEYS = ['password', 'sudoPassword', 'passphrase', 'enablePassword']
const DEFAULT_WAIT_MS = 30_000

function toRow(s: MacroStep): Row {
  const kind: StepKind = s.secret ? 'secret' : s.waitFor ? 'wait' : 'type'
  const data = kind === 'secret' ? s.data.replace(/\r$/, '') : s.data
  return {
    key: uid('st'),
    kind,
    text: escapeText(data),
    delayMs: s.delayMs,
    waitFor: s.waitFor ?? '',
    timeoutMs: s.timeoutMs ?? null,
    secret: s.secret ?? 'password',
    enter: kind === 'secret' ? s.data.endsWith('\r') : true,
  }
}

function toStep(r: Row): MacroStep {
  const step: MacroStep = { data: '', delayMs: Math.max(0, Math.round(r.delayMs || 0)) }
  if (r.kind === 'secret') {
    step.secret = r.secret || 'password'
    step.data = unescapeText(r.text) + (r.enter ? '\r' : '')
    return step
  }
  step.data = unescapeText(r.text)
  if (r.kind === 'wait') {
    step.waitFor = r.waitFor
    if (r.timeoutMs && r.timeoutMs !== DEFAULT_WAIT_MS) step.timeoutMs = r.timeoutMs
  }
  return step
}

const newRow = (kind: StepKind, delayMs: number): Row => ({
  key: uid('st'),
  kind,
  text: '',
  delayMs,
  waitFor: '',
  timeoutMs: null,
  secret: 'password',
  enter: true,
})

const KIND_OPTIONS: { value: StepKind; label: React.ReactNode }[] = [
  {
    value: 'type',
    label: (
      <span className="flex items-center gap-1.5">
        <Keyboard className="size-3.5" /> Type
      </span>
    ),
  },
  {
    value: 'wait',
    label: (
      <span className="flex items-center gap-1.5">
        <Hourglass className="size-3.5" /> Wait for
      </span>
    ),
  },
  {
    value: 'secret',
    label: (
      <span className="flex items-center gap-1.5">
        <KeyRound className="size-3.5" /> Secret
      </span>
    ),
  },
]

function StepRow({ row, index, onChange, onRemove }: { row: Row; index: number; onChange: (r: Row) => void; onRemove: () => void }) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({ id: row.key })
  const style: React.CSSProperties = { transform: CSS.Transform.toString(transform), transition, zIndex: isDragging ? 10 : undefined }
  const keys = describeKeys(unescapeText(row.text))
  const n = index + 1
  return (
    <li ref={setNodeRef} style={style} className={`flex items-start gap-1.5 rounded-md border bg-card px-1.5 py-1 ${isDragging ? 'shadow-popover' : ''}`}>
      <button
        type="button"
        className="flex h-7 w-5 shrink-0 cursor-grab items-center justify-center rounded text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60 active:cursor-grabbing"
        aria-label={`Move step ${n}`}
        {...attributes}
        {...listeners}
      >
        <GripVertical className="size-3.5" />
      </button>
      <span className="flex h-7 w-6 shrink-0 items-center justify-end text-2xs text-muted-foreground tabular-nums">{n}</span>
      <SimpleSelect<StepKind> size="sm" className="w-28 shrink-0" value={row.kind} onValueChange={(kind) => onChange({ ...row, kind })} options={KIND_OPTIONS} aria-label={`Step ${n} kind`} />
      <div className="flex min-w-0 flex-1 flex-col gap-1">
        {row.kind === 'wait' && (
          <div className="flex items-center gap-1.5">
            <Input
              inputSize="sm"
              className="min-w-0 flex-1 font-mono"
              value={row.waitFor}
              onChange={(e) => onChange({ ...row, waitFor: e.target.value })}
              placeholder="pattern, e.g. [$#]\s*$ or Password:"
              aria-label={`Step ${n} pattern to wait for`}
              spellCheck={false}
            />
            <span className="w-24 shrink-0">
              <NumberInput inputSize="sm" value={row.timeoutMs ?? DEFAULT_WAIT_MS / 1000} min={1} max={600} unit="s" onChange={(v) => onChange({ ...row, timeoutMs: v ? v * 1000 : null })} aria-label={`Step ${n} wait timeout`} />
            </span>
          </div>
        )}
        {row.kind === 'secret' ? (
          <div className="flex items-center gap-2">
            <SimpleSelect
              size="sm"
              className="min-w-40 flex-1"
              value={row.secret}
              onValueChange={(secret) => onChange({ ...row, secret })}
              options={Array.from(new Set([...SECRET_KEYS, row.secret])).map((k) => ({ value: k, label: `Stored ${secretLabel(k)}` }))}
              aria-label={`Step ${n} secret`}
            />
            <label className="flex shrink-0 items-center gap-1.5 text-sm">
              <Checkbox checked={row.enter} onCheckedChange={(v) => onChange({ ...row, enter: v === true })} /> Enter
            </label>
          </div>
        ) : (
          <>
            <Input
              inputSize="sm"
              className="font-mono"
              value={row.text}
              onChange={(e) => onChange({ ...row, text: e.target.value })}
              placeholder={row.kind === 'wait' ? String.raw`then type (optional), e.g. show version\r` : String.raw`text, e.g. ls -la\r`}
              aria-label={row.kind === 'wait' ? `Step ${n} text typed after the wait` : `Step ${n} text`}
              spellCheck={false}
            />
            {keys !== row.text && <span className="truncate px-1 font-mono text-2xs text-muted-foreground">{keys}</span>}
          </>
        )}
      </div>
      <div className="w-28 shrink-0">
        <NumberInput inputSize="sm" value={row.delayMs} min={0} max={600000} step={50} unit="ms" onChange={(v) => onChange({ ...row, delayMs: v ?? 0 })} aria-label={`Delay before step ${n}`} />
      </div>
      <IconButton icon={Trash2} label={`Delete step ${n}`} size="xs" className="mt-0.5" onClick={onRemove} />
    </li>
  )
}

function Editor({ id, initialSteps, initialName }: { id?: string; initialSteps?: MacroStep[]; initialName?: string }) {
  const qc = useQueryClient()
  const { data: macros, isLoading } = useMacros(!!id)
  const existing = id ? macros?.find((m) => m.id === id) : undefined
  const [name, setName] = React.useState(initialName ?? '')
  const [rows, setRows] = React.useState<Row[]>(() => (initialSteps ?? []).map(toRow))
  const [shortcut, setShortcut] = React.useState(id ? (automationSettings.get().macroShortcuts[id] ?? '') : '')
  const [saving, setSaving] = React.useState(false)
  const [error, setError] = React.useState<string | null>(null)
  // Fill the form from the stored macro during render (not in an effect), so no frame shows it empty.
  const [hydrated, setHydrated] = React.useState(!id)
  if (!hydrated && existing) {
    setHydrated(true)
    setName(existing.name)
    setRows(existing.steps.map(toRow))
  }
  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 4 } }), useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }))
  const total = rows.reduce((t, r) => t + (r.delayMs || 0), 0)

  const onDragEnd = (e: DragEndEvent) => {
    const { active, over } = e
    if (!over || active.id === over.id) return
    setRows((rs) => arrayMove(rs, rs.findIndex((r) => r.key === active.id), rs.findIndex((r) => r.key === over.id)))
  }

  const mergeTyped = () => {
    const out: Row[] = []
    for (const r of rows) {
      const prev = out[out.length - 1]
      const printable = (s: string) => !/[\x00-\x1f\x7f]/.test(unescapeText(s)) // oxlint-disable-line no-control-regex
      if (prev && prev.kind === 'type' && r.kind === 'type' && printable(prev.text) && printable(r.text)) {
        out[out.length - 1] = { ...prev, text: prev.text + r.text }
      } else {
        out.push(r)
      }
    }
    setRows(out)
  }

  const setAllDelays = async () => {
    const v = await prompt({ title: 'Delay before every step', label: 'Milliseconds', type: 'number', defaultValue: '100', validate: (x) => (/^\d{1,6}$/.test(x) ? null : 'Enter 0 – 600000') })
    if (v === null) return
    const ms = Math.min(600000, Number(v))
    setRows((rs) => rs.map((r, i) => ({ ...r, delayMs: i === 0 ? 0 : ms })))
  }

  const save = async () => {
    if (!name.trim()) {
      setError('Give the macro a name.')
      return
    }
    setSaving(true)
    setError(null)
    const bad = rows.findIndex((r) => r.kind === 'wait' && !r.waitFor.trim())
    if (bad >= 0) {
      setSaving(false)
      setError(`Step ${bad + 1}: enter the pattern to wait for.`)
      return
    }
    const steps = rows.map(toStep)
    try {
      const saved = id ? await updateMacro(id, { name: name.trim(), steps }) : await createMacro({ name: name.trim(), steps })
      const sc = { ...automationSettings.get().macroShortcuts }
      if (shortcut) sc[saved.id] = shortcut
      else delete sc[saved.id]
      automationSettings.set({ macroShortcuts: sc })
      await qc.invalidateQueries({ queryKey: autoKeys.macros })
      toast.success(id ? 'Macro saved' : 'Macro created')
      closeMacroEditor()
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && closeMacroEditor()}>
      <DialogContent size="2xl" className="h-[min(88vh,760px)]">
        <form
          className="flex min-h-0 flex-1 flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault()
            void save()
          }}
        >
          <DialogHeader>
            <DialogTitle>
              <Clapperboard className="size-4 text-primary" /> {id ? 'Edit macro' : 'New macro'}
            </DialogTitle>
            <DialogDescription>
              Each step runs after its delay. <em>Wait for</em> steps pause until the output shows a pattern, so replays keep up with slow hosts;{' '}
              <em>Secret</em> steps type a stored secret that never enters the macro. Escapes: <code className="font-mono">\r</code> Enter,{' '}
              <code className="font-mono">\t</code> Tab, <code className="font-mono">\x03</code> Ctrl+C, <code className="font-mono">\e[A</code> arrow up.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-3 sm:grid-cols-2">
            <Field label="Name" required error={error && !name.trim() ? error : undefined}>
              <Input autoFocus={!id} value={name} onChange={(e) => setName(e.target.value)} maxLength={200} />
            </Field>
            <Field label="Keyboard shortcut" hint="Plays in the active terminal">
              <ShortcutInput value={shortcut} onChange={setShortcut} />
            </Field>
          </div>
          <div className="flex items-center gap-1.5">
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button type="button" size="sm" variant="secondary">
                  <Plus /> Add step <ChevronDown className="opacity-60" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="start">
                <DropdownMenuItem onSelect={() => setRows((rs) => [...rs, newRow('type', rs.length ? 100 : 0)])}>
                  <Keyboard /> Type text
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => setRows((rs) => [...rs, newRow('wait', 0)])}>
                  <Hourglass /> Wait for output
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => setRows((rs) => [...rs, newRow('secret', 0)])}>
                  <KeyRound /> Type a stored secret
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
            <Button type="button" size="sm" variant="ghost" onClick={mergeTyped} disabled={rows.length < 2}>
              <Combine /> Merge typed text
            </Button>
            <Button type="button" size="sm" variant="ghost" onClick={() => void setAllDelays()} disabled={!rows.length}>
              <Timer /> Set all delays…
            </Button>
            <span className="ml-auto text-sm text-muted-foreground">
              {plural(rows.length, 'step')} · {formatDuration(total)}
              {rows.some((r) => r.kind === 'wait') ? ' + waits' : ''}
            </span>
          </div>
          <DialogBody className="min-h-0 flex-1">
            <LoadingState busy={!!id && isLoading && !existing} className="h-full min-h-24">
              {!rows.length ? (
                <p className="py-6 text-center text-sm text-muted-foreground">No steps yet — add one, or record a macro from a terminal.</p>
              ) : (
                <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={onDragEnd}>
                  <SortableContext items={rows.map((r) => r.key)} strategy={verticalListSortingStrategy}>
                    <ol className="flex flex-col gap-1" aria-label="Steps">
                      {rows.map((r, i) => (
                        <StepRow
                          key={r.key}
                          row={r}
                          index={i}
                          onChange={(nr) => setRows((rs) => rs.map((x) => (x.key === r.key ? nr : x)))}
                          onRemove={() => setRows((rs) => rs.filter((x) => x.key !== r.key))}
                        />
                      ))}
                    </ol>
                  </SortableContext>
                </DndContext>
              )}
            </LoadingState>
          </DialogBody>
          {error && name.trim() && (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="secondary" onClick={() => closeMacroEditor()}>
              Cancel
            </Button>
            <Button type="submit" loading={saving} disabled={!rows.length}>
              {id ? 'Save' : 'Create'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

export function MacroEditorDialog() {
  const st = useAutomationUI((s) => s.macroEditor)
  if (!st) return null
  return <Editor key={st.id ?? 'new'} id={st.id} initialSteps={st.steps} initialName={st.name} />
}
