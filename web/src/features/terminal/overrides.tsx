/*
 * Per-connection terminal overrides editor (TERM-10, SM-2) for the sessions feature's connection editor:
 *
 *   <TerminalOverridesEditor value={conn.options.terminal} onChange={(terminal) => setOptions({...options, terminal})} />
 *
 * Empty fields inherit the global Settings → Terminal values. `onChange(undefined)` means "no overrides".
 */
import { useMemo, type ReactNode } from 'react'
import type { TerminalOverrides } from '@/api/types'
import { Input } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { SimpleSelect } from '@/components/ui/select'
import { LIMITS, terminalSettings } from './settings'
import { listSchemes } from './themes'

const INHERIT = '__inherit__'

function clean(o: TerminalOverrides): TerminalOverrides | undefined {
  const out: TerminalOverrides = {}
  for (const [k, v] of Object.entries(o)) if (v !== undefined && v !== null && v !== '') out[k] = v
  return Object.keys(out).length ? out : undefined
}

function Row({ label, hint, children }: { label: string; hint?: string; children: ReactNode }) {
  return (
    <div className="grid gap-1">
      <span className="text-sm font-medium">{label}</span>
      {children}
      {hint && <span className="text-xs text-muted-foreground">{hint}</span>}
    </div>
  )
}

export function TerminalOverridesEditor({
  value,
  onChange,
  disabled,
}: {
  value: TerminalOverrides | undefined
  onChange: (next: TerminalOverrides | undefined) => void
  disabled?: boolean
}) {
  const global = terminalSettings.use()
  const v = value ?? {}
  const schemes = useMemo(() => listSchemes(global.customSchemes), [global.customSchemes])
  const set = (patch: Partial<TerminalOverrides>) => onChange(clean({ ...v, ...patch }))
  return (
    <div className="grid gap-4 @md:grid-cols-2">
      <Row label="Colour scheme" hint="Overrides the scheme in both light and dark UI themes.">
        <SimpleSelect
          aria-label="Colour scheme"
          size="sm"
          disabled={disabled}
          value={typeof v.theme === 'string' && v.theme ? v.theme : INHERIT}
          onValueChange={(t) => set({ theme: t === INHERIT ? undefined : t })}
          options={[{ value: INHERIT, label: 'Default (from settings)' }, ...schemes.map((s) => ({ value: s.id, label: s.name }))]}
        />
      </Row>
      <Row label="Cursor style">
        <SimpleSelect
          aria-label="Cursor style"
          size="sm"
          disabled={disabled}
          value={v.cursorStyle ?? INHERIT}
          onValueChange={(c) => set({ cursorStyle: c === INHERIT ? undefined : (c as TerminalOverrides['cursorStyle']) })}
          options={[
            { value: INHERIT, label: `Default (${global.cursorStyle})` },
            { value: 'block', label: 'Block' },
            { value: 'underline', label: 'Underline' },
            { value: 'bar', label: 'Bar' },
          ]}
        />
      </Row>
      <Row label="Font family" hint="CSS font-family list; empty uses the default.">
        <Input
          inputSize="sm"
          className="font-mono"
          disabled={disabled}
          placeholder="Default"
          defaultValue={typeof v.fontFamily === 'string' ? v.fontFamily : ''}
          onBlur={(e) => set({ fontFamily: e.target.value.trim() || undefined })}
          aria-label="Font family"
        />
      </Row>
      <Row label="Font size">
        <NumberInput
          inputSize="sm"
          disabled={disabled}
          unit="px"
          allowEmpty
          placeholder={String(global.fontSize)}
          value={typeof v.fontSize === 'number' ? v.fontSize : null}
          min={LIMITS.fontSize[0]}
          max={LIMITS.fontSize[1]}
          onChange={(n) => set({ fontSize: n ?? undefined })}
          aria-label="Font size"
        />
      </Row>
      <Row label="Scrollback lines">
        <NumberInput
          inputSize="sm"
          disabled={disabled}
          allowEmpty
          placeholder={String(global.scrollback)}
          value={typeof v.scrollback === 'number' ? v.scrollback : null}
          min={LIMITS.scrollback[0]}
          max={LIMITS.scrollback[1]}
          step={1000}
          onChange={(n) => set({ scrollback: n ?? undefined })}
          aria-label="Scrollback lines"
        />
      </Row>
    </div>
  )
}
