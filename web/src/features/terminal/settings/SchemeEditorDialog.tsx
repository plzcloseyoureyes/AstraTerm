/*
 * Custom colour scheme editor (TERM-9): 16 ANSI colours + background / foreground / cursor / selection, live preview.
 */
import { useEffect, useId, useState } from 'react'
import { Palette } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { cn } from '@/lib/utils'
import { normalizeScheme, toHex } from '../themes'
import { ANSI_KEYS, type AnsiKey, type TerminalScheme } from '../types'
import { SchemePreview, type PreviewFont } from './SchemePreview'

type ColorKey = AnsiKey | 'background' | 'foreground' | 'cursor' | 'cursorAccent' | 'selectionBackground' | 'selectionForeground'

const SPECIALS: { key: ColorKey; label: string; optional?: boolean }[] = [
  { key: 'background', label: 'Background' },
  { key: 'foreground', label: 'Foreground' },
  { key: 'cursor', label: 'Cursor' },
  { key: 'cursorAccent', label: 'Cursor text' },
  { key: 'selectionBackground', label: 'Selection' },
  { key: 'selectionForeground', label: 'Selection text', optional: true },
]

const ANSI_LABELS = ['Black', 'Red', 'Green', 'Yellow', 'Blue', 'Magenta', 'Cyan', 'White']

function ColorField({ label, value, onChange, optional }: { label: string; value: string | undefined; onChange: (v: string | undefined) => void; optional?: boolean }) {
  const id = useId()
  const [text, setText] = useState(value ?? '')
  useEffect(() => setText(value ?? ''), [value])
  const invalid = text !== '' && !toHex(text)
  return (
    <div className="grid gap-1">
      <label htmlFor={id} className="text-xs text-muted-foreground">
        {label}
      </label>
      <div className="flex items-center gap-1.5">
        <span className="relative size-7 shrink-0 overflow-hidden rounded-md border" style={{ background: value ?? 'transparent' }}>
          <input
            type="color"
            aria-label={`${label} colour picker`}
            value={value ?? '#000000'}
            onChange={(e) => onChange(e.target.value)}
            className="absolute inset-0 size-full cursor-pointer opacity-0"
          />
        </span>
        <Input
          id={id}
          inputSize="sm"
          value={text}
          placeholder={optional ? 'default' : '#rrggbb'}
          aria-invalid={invalid || undefined}
          spellCheck={false}
          className="font-mono"
          onChange={(e) => {
            setText(e.target.value)
            const hex = toHex(e.target.value)
            if (hex) onChange(hex)
            else if (optional && e.target.value.trim() === '') onChange(undefined)
          }}
        />
      </div>
    </div>
  )
}

export function SchemeEditorDialog({
  scheme,
  font,
  onSave,
  onClose,
}: {
  /** Scheme to edit (a copy is edited). */
  scheme: TerminalScheme
  font: PreviewFont
  onSave: (s: TerminalScheme) => void
  onClose: () => void
}) {
  const [draft, setDraft] = useState<TerminalScheme>(() => normalizeScheme(scheme))
  const set = (k: ColorKey, v: string | undefined) => setDraft((d) => ({ ...d, [k]: v }))
  const nameId = useId()
  const valid = !!draft.name.trim() && ANSI_KEYS.every((k) => !!toHex(draft[k])) && !!toHex(draft.background) && !!toHex(draft.foreground)
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent size="2xl">
        <DialogHeader>
          <DialogTitle>
            <Palette className="size-4 text-muted-foreground" /> Edit colour scheme
          </DialogTitle>
          <DialogDescription>Changes are previewed live below. Colours use the #rrggbb format.</DialogDescription>
        </DialogHeader>
        <DialogBody className="grid gap-5">
          <div className="grid max-w-sm gap-1">
            <label htmlFor={nameId} className="text-sm font-medium">
              Name
            </label>
            <Input id={nameId} value={draft.name} maxLength={80} onChange={(e) => setDraft((d) => ({ ...d, name: e.target.value }))} />
          </div>
          <section className="grid gap-2">
            <h3 className="text-sm font-medium">Base colours</h3>
            <div className="grid grid-cols-2 gap-3 @lg:grid-cols-3 sm:grid-cols-3">
              {SPECIALS.map((f) => (
                <ColorField key={f.key} label={f.label} optional={f.optional} value={draft[f.key]} onChange={(v) => set(f.key, v)} />
              ))}
            </div>
          </section>
          <section className="grid gap-2">
            <h3 className="text-sm font-medium">ANSI colours</h3>
            {(['normal', 'bright'] as const).map((row) => (
              <div key={row} className="grid gap-1">
                <span className="text-xs text-muted-foreground">{row === 'normal' ? 'Normal (0–7)' : 'Bright (8–15)'}</span>
                <div className="grid grid-cols-4 gap-2 sm:grid-cols-8">
                  {ANSI_KEYS.slice(row === 'normal' ? 0 : 8, row === 'normal' ? 8 : 16).map((k, i) => (
                    <div key={k} className="grid gap-1">
                      <label className="relative block h-8 cursor-pointer overflow-hidden rounded-md border" style={{ background: draft[k] }} title={`${ANSI_LABELS[i]} ${draft[k]}`}>
                        <input type="color" aria-label={`${row === 'bright' ? 'Bright ' : ''}${ANSI_LABELS[i]}`} value={toHex(draft[k]) ?? '#000000'} onChange={(e) => set(k, e.target.value)} className="absolute inset-0 size-full cursor-pointer opacity-0" />
                      </label>
                      <span className={cn('truncate text-center text-[10px] text-muted-foreground')}>{ANSI_LABELS[i]}</span>
                    </div>
                  ))}
                </div>
              </div>
            ))}
          </section>
          <SchemePreview scheme={draft} font={font} />
        </DialogBody>
        <DialogFooter>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button disabled={!valid} onClick={() => onSave(normalizeScheme({ ...draft, name: draft.name.trim() }))}>
            Save scheme
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
