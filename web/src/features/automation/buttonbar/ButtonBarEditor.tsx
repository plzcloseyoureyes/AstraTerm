/*
 * Button bar editor (AUTO-4): bars (name + where they show) and their buttons (label, colour, action, target).
 */
import * as React from 'react'
import { ArrowDown, ArrowUp, LayoutPanelTop, Plus, Trash2 } from 'lucide-react'
import { commands } from '@/app/registry'
import { Button } from '@/components/ui/button'
import { ColorSwatchPicker } from '@/components/ui/color-swatch-picker'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { EmptyState } from '@/components/ui/empty-state'
import { Field } from '@/components/ui/field'
import { IconButton } from '@/components/ui/icon-button'
import { Input } from '@/components/ui/input'
import { SimpleSelect } from '@/components/ui/select'
import { SwitchField } from '@/components/ui/switch'
import { TagInput } from '@/components/ui/tag-input'
import { uid } from '@/lib/utils'
import { useMacros, useScripts, useSnippets } from '../api'
import { automationSettings } from '../settings'
import { useAutomationUI } from '../store'
import type { ButtonActionType, ButtonBar, ButtonTarget, QuickButton } from '../types'

const ACTIONS: { value: ButtonActionType; label: string }[] = [
  { value: 'send', label: 'Send text' },
  { value: 'snippet', label: 'Run snippet' },
  { value: 'macro', label: 'Play macro' },
  { value: 'script', label: 'Run script' },
  { value: 'command', label: 'Run command' },
]

const TARGETS: { value: ButtonTarget; label: string }[] = [
  { value: 'active', label: 'Active terminal' },
  { value: 'multiexec', label: 'Broadcast group' },
  { value: 'all', label: 'Every terminal' },
]

function ButtonRow({
  b,
  index,
  count,
  onChange,
  onMove,
  onRemove,
}: {
  b: QuickButton
  index: number
  count: number
  onChange: (b: QuickButton) => void
  onMove: (dir: -1 | 1) => void
  onRemove: () => void
}) {
  const { data: snippets } = useSnippets(b.action === 'snippet')
  const { data: macros } = useMacros(b.action === 'macro')
  const { data: scripts } = useScripts(b.action === 'script')
  const cmds = commands.useList()
  return (
    <li className="flex flex-col gap-2 rounded-md border bg-card p-2">
      <div className="flex items-center gap-2">
        <Input inputSize="sm" className="w-40" value={b.label} onChange={(e) => onChange({ ...b, label: e.target.value })} placeholder="Label" aria-label="Button label" maxLength={40} />
        <SimpleSelect<ButtonActionType> size="sm" className="w-36" value={b.action} onValueChange={(action) => onChange({ ...b, action, refId: undefined, text: action === b.action ? b.text : '' })} options={ACTIONS} aria-label="Action" />
        {b.action !== 'command' && b.action !== 'script' && (
          <SimpleSelect<ButtonTarget> size="sm" className="w-44" value={b.target ?? 'active'} onValueChange={(target) => onChange({ ...b, target })} options={TARGETS} aria-label="Target" />
        )}
        <span className="ml-auto flex items-center gap-0.5">
          <IconButton icon={ArrowUp} label="Move up" size="xs" disabled={index === 0} onClick={() => onMove(-1)} />
          <IconButton icon={ArrowDown} label="Move down" size="xs" disabled={index === count - 1} onClick={() => onMove(1)} />
          <IconButton icon={Trash2} label="Delete button" size="xs" onClick={onRemove} />
        </span>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        {b.action === 'send' && (
          <Input inputSize="sm" className="min-w-60 flex-1 font-mono" value={b.text ?? ''} onChange={(e) => onChange({ ...b, text: e.target.value })} placeholder="uptime\r   (\r = Enter, \x03 = Ctrl+C)" aria-label="Text to send" spellCheck={false} />
        )}
        {b.action === 'snippet' && (
          <SimpleSelect size="sm" className="min-w-60 flex-1" value={b.refId} onValueChange={(refId) => onChange({ ...b, refId })} placeholder="Choose a snippet" options={(snippets ?? []).map((s) => ({ value: s.id, label: s.folder ? `${s.folder} / ${s.name}` : s.name }))} aria-label="Snippet" />
        )}
        {b.action === 'macro' && (
          <SimpleSelect size="sm" className="min-w-60 flex-1" value={b.refId} onValueChange={(refId) => onChange({ ...b, refId })} placeholder="Choose a macro" options={(macros ?? []).map((m) => ({ value: m.id, label: m.name }))} aria-label="Macro" />
        )}
        {b.action === 'script' && (
          <SimpleSelect size="sm" className="min-w-60 flex-1" value={b.refId} onValueChange={(refId) => onChange({ ...b, refId })} placeholder="Choose a script" options={(scripts ?? []).map((s) => ({ value: s.id, label: s.name }))} aria-label="Script" />
        )}
        {b.action === 'command' && (
          <>
            <select
              className="h-7 min-w-60 flex-1 rounded-md border border-input bg-background/60 px-2 text-sm outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/25 dark:bg-input/25"
              value={b.text ?? ''}
              onChange={(e) => onChange({ ...b, text: e.target.value })}
              aria-label="Command"
            >
              <option value="">Choose a command…</option>
              {cmds
                .filter((c) => !c.hidden)
                .slice()
                .sort((x, y) => `${x.category ?? ''}${x.title}`.localeCompare(`${y.category ?? ''}${y.title}`))
                .map((c) => (
                  <option key={c.id} value={c.id}>
                    {c.category ? `${c.category}: ` : ''}
                    {c.title}
                  </option>
                ))}
            </select>
            <Input inputSize="sm" className="w-48 font-mono" value={b.args ?? ''} onChange={(e) => onChange({ ...b, args: e.target.value })} placeholder='args (JSON), e.g. {"tool":"ping"}' aria-label="Command arguments" />
          </>
        )}
      </div>
      <div className="flex items-center gap-2">
        <span className="text-xs text-muted-foreground">Colour</span>
        <ColorSwatchPicker size="sm" value={b.color} onChange={(color) => onChange({ ...b, color })} allowNone aria-label="Button colour" />
      </div>
    </li>
  )
}

function BarEditor({ bar, onChange, onRemove }: { bar: ButtonBar; onChange: (b: ButtonBar) => void; onRemove: () => void }) {
  const [scoped, setScoped] = React.useState(!!(bar.protocols?.length || bar.tags?.length))
  const setButtons = (buttons: QuickButton[]) => onChange({ ...bar, buttons })
  return (
    <section className="flex flex-col gap-2 rounded-lg border p-3" aria-label={`Bar ${bar.name}`}>
      <div className="flex items-center gap-2">
        <Input inputSize="sm" className="max-w-64 font-medium" value={bar.name} onChange={(e) => onChange({ ...bar, name: e.target.value })} aria-label="Bar name" />
        <SwitchField
          className="ml-2"
          label="Only for some sessions"
          checked={scoped}
          onCheckedChange={(v) => {
            setScoped(v)
            if (!v) onChange({ ...bar, protocols: [], tags: [] })
          }}
        />
        <IconButton icon={Trash2} label="Delete bar" size="xs" className="ml-auto" onClick={onRemove} />
      </div>
      {scoped && (
        <div className="grid gap-2 sm:grid-cols-2">
          <Field label="Protocols" hint="e.g. ssh, telnet, serial">
            <TagInput value={bar.protocols ?? []} onChange={(protocols) => onChange({ ...bar, protocols: protocols.map((p) => p.toLowerCase()) })} placeholder="Add protocol…" />
          </Field>
          <Field label="Connection tags" hint="Any of these tags">
            <TagInput value={bar.tags ?? []} onChange={(tags) => onChange({ ...bar, tags })} placeholder="Add tag…" />
          </Field>
        </div>
      )}
      <ul className="flex flex-col gap-1.5">
        {bar.buttons.map((b, i) => (
          <ButtonRow
            key={b.id}
            b={b}
            index={i}
            count={bar.buttons.length}
            onChange={(nb) => setButtons(bar.buttons.map((x) => (x.id === b.id ? nb : x)))}
            onMove={(dir) => {
              const next = bar.buttons.slice()
              const j = i + dir
              ;[next[i], next[j]] = [next[j], next[i]]
              setButtons(next)
            }}
            onRemove={() => setButtons(bar.buttons.filter((x) => x.id !== b.id))}
          />
        ))}
      </ul>
      <Button
        size="sm"
        variant="secondary"
        className="self-start"
        onClick={() => setButtons([...bar.buttons, { id: uid('btn'), label: 'New', action: 'send', text: '', target: 'active' }])}
      >
        <Plus /> Add button
      </Button>
    </section>
  )
}

/** A starter bar with the buttons most sessions want. */
function exampleBar(): ButtonBar {
  const send = (label: string, text: string, color?: string): QuickButton => ({ id: uid('btn'), label, action: 'send', text, target: 'active', color })
  return { id: uid('bar'), name: 'Main', buttons: [send('Ctrl+C', '\\x03', '#ef4444'), send('Clear', 'clear\\r'), send('Disk', 'df -h\\r'), send('Top', 'top\\r')] }
}

function Editor({ onClose }: { onClose: () => void }) {
  const initial = automationSettings.get().buttonBars
  const [bars, setBars] = React.useState<ButtonBar[]>(() => JSON.parse(JSON.stringify(initial)) as ButtonBar[])
  const visible = automationSettings.useValue('buttonBarVisible')
  const invalid = bars.some((b) => !b.name.trim() || b.buttons.some((x) => !x.label.trim()))
  const addBar = (bar?: ButtonBar) => setBars((bs) => [...bs, bar ?? { id: uid('bar'), name: bs.length ? `Bar ${bs.length + 1}` : 'Main', buttons: [] }])
  const save = () => {
    const clean = bars.map((b) => ({ ...b, name: b.name.trim(), buttons: b.buttons.map((x) => ({ ...x, label: x.label.trim() })) }))
    automationSettings.set({ buttonBars: clean, buttonBarVisible: true })
    onClose()
  }
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="2xl" className="max-h-[min(88vh,820px)]">
        <DialogHeader>
          <DialogTitle>
            <LayoutPanelTop className="size-4 text-primary" /> Button bar
          </DialogTitle>
          <DialogDescription>Quick buttons shown in the status bar for the active terminal. {visible ? '' : 'The button bar is hidden; saving shows it again.'}</DialogDescription>
        </DialogHeader>
        <DialogBody className="flex flex-col gap-3">
          {!bars.length ? (
            <EmptyState
              icon={LayoutPanelTop}
              title="No button bars yet"
              description="A bar holds one-click buttons that send text or run snippets, macros, scripts or commands in the active terminal."
              action={
                <>
                  <Button onClick={() => addBar()}>
                    <Plus /> New bar
                  </Button>
                  <Button variant="secondary" onClick={() => addBar(exampleBar())}>
                    Start from an example
                  </Button>
                </>
              }
            />
          ) : (
            <>
              {bars.map((bar) => (
                <BarEditor key={bar.id} bar={bar} onChange={(nb) => setBars((bs) => bs.map((x) => (x.id === bar.id ? nb : x)))} onRemove={() => setBars((bs) => bs.filter((x) => x.id !== bar.id))} />
              ))}
              <Button variant="secondary" className="self-start" onClick={() => addBar()}>
                <Plus /> New bar
              </Button>
            </>
          )}
        </DialogBody>
        <DialogFooter>
          {invalid && <span className="mr-auto text-sm text-destructive">Every bar and button needs a name.</span>}
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button onClick={save} disabled={invalid}>
            Save
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export function ButtonBarEditorDialog() {
  const st = useAutomationUI((s) => s.buttonEditor)
  if (!st) return null
  return <Editor onClose={() => useAutomationUI.setState({ buttonEditor: null })} />
}
