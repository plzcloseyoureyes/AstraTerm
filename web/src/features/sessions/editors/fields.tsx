/*
 * Building blocks for protocol editors. Every control reads/writes `value.options[name]` of the draft connection and
 * reports validation errors registered under `options.<name>` (see editors/context.ts). Empty values delete the key,
 * so untouched options keep their backend defaults (SPEC §5.3).
 */
import { useEffect, useId, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react'
import { Popover as PopoverPrimitive } from 'radix-ui'
import { ChevronDown, KeyRound, Pencil, Plus, Undo2, X } from 'lucide-react'
import type { Connection, ConnectionOptions } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Field } from '@/components/ui/field'
import { Input, inputBase } from '@/components/ui/input'
import { NumberInput } from '@/components/ui/number-input'
import { PasswordInput } from '@/components/ui/password-input'
import { SimpleSelect } from '@/components/ui/select'
import { SwitchField } from '@/components/ui/switch'
import { TagInput } from '@/components/ui/tag-input'
import { Tooltip } from '@/components/ui/tooltip'
import { cn, isPlainObject } from '@/lib/utils'
import { connectionTarget } from '../model'
import { useEditorEnv, useFieldError } from './context'

// ---------------------------------------------------------------------------------------------------------------------
// Option helpers
// ---------------------------------------------------------------------------------------------------------------------

function isEmptyValue(v: unknown): boolean {
  return v === undefined || v === null || v === '' || (Array.isArray(v) && v.length === 0) || (isPlainObject(v) && Object.keys(v).length === 0)
}

/** Immutably set option keys; empty values (undefined, null, '', [], {}) delete the key. */
export function withOptions(value: Connection, patch: Record<string, unknown>): Connection {
  const options: ConnectionOptions = { ...value.options }
  for (const [k, v] of Object.entries(patch)) {
    if (isEmptyValue(v)) delete options[k]
    else options[k] = v
  }
  return { ...value, options }
}

export function optString(value: Connection, key: string): string {
  const v = value.options?.[key]
  return typeof v === 'string' ? v : typeof v === 'number' ? String(v) : ''
}

export function optNumber(value: Connection, key: string): number | null {
  const v = value.options?.[key]
  if (typeof v === 'number' && Number.isFinite(v)) return v
  if (typeof v === 'string' && v.trim() !== '' && Number.isFinite(Number(v))) return Number(v)
  return null
}

export function optBool(value: Connection, key: string, def = false): boolean {
  const v = value.options?.[key]
  return typeof v === 'boolean' ? v : def
}

export function optStrings(value: Connection, key: string): string[] {
  const v = value.options?.[key]
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : []
}

/** Set / clear a write-only secret on the draft (undefined = unchanged, '' = delete the stored value). */
function withSecret(value: Connection, key: string, v: string | undefined): Connection {
  const secrets = { ...value.secrets }
  if (v === undefined) delete secrets[key]
  else secrets[key] = v
  return { ...value, secrets }
}

export interface OptionProps {
  value: Connection
  onChange: (next: Connection) => void
  /** Option key in `options`. */
  name: string
  label: ReactNode
  hint?: ReactNode
  className?: string
}

// ---------------------------------------------------------------------------------------------------------------------
// Layout
// ---------------------------------------------------------------------------------------------------------------------

/** A titled group of options inside a tab. */
export function OptionSection({
  title,
  description,
  children,
  className,
  columns = 2,
}: {
  title?: ReactNode
  description?: ReactNode
  children: ReactNode
  className?: string
  columns?: 1 | 2
}) {
  return (
    <section className={cn('grid gap-3', className)}>
      {(title || description) && (
        <header className="grid gap-0.5 border-b pb-1.5">
          {title && <h3 className="text-xs font-semibold tracking-wide text-muted-foreground uppercase">{title}</h3>}
          {description && <p className="text-sm text-muted-foreground">{description}</p>}
        </header>
      )}
      <div className={cn('grid gap-x-4 gap-y-3', columns === 2 && '@lg:grid-cols-2')}>{children}</div>
    </section>
  )
}

/** Inline notice (info / warning) inside an editor. */
export function EditorNote({ children, tone = 'info', className }: { children: ReactNode; tone?: 'info' | 'warning'; className?: string }) {
  return (
    <p
      className={cn(
        'rounded-md border px-2.5 py-1.5 text-sm',
        tone === 'warning' ? 'border-warning/40 bg-warning/10 text-foreground' : 'border-border bg-muted/40 text-muted-foreground',
        className,
      )}
    >
      {children}
    </p>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// Simple controls
// ---------------------------------------------------------------------------------------------------------------------

export function TextOption({
  value,
  onChange,
  name,
  label,
  hint,
  className,
  placeholder,
  mono,
  required,
  trim,
}: OptionProps & { placeholder?: string; mono?: boolean; required?: boolean; trim?: boolean }) {
  const error = useFieldError(`options.${name}`)
  return (
    <Field label={label} hint={hint} error={error} className={className} required={required}>
      <Input
        value={optString(value, name)}
        placeholder={placeholder}
        spellCheck={false}
        autoComplete="off"
        autoCapitalize="off"
        className={mono ? 'font-mono' : undefined}
        onChange={(e) => onChange(withOptions(value, { [name]: e.target.value }))}
        onBlur={(e) => {
          if (trim && e.target.value !== e.target.value.trim()) onChange(withOptions(value, { [name]: e.target.value.trim() }))
        }}
      />
    </Field>
  )
}

export function NumberOption({
  value,
  onChange,
  name,
  label,
  hint,
  className,
  min,
  max,
  step,
  unit,
  placeholder,
}: OptionProps & { min?: number; max?: number; step?: number; unit?: string; placeholder?: string }) {
  const error = useFieldError(`options.${name}`)
  return (
    <Field label={label} hint={hint} error={error} className={className}>
      <NumberInput
        value={optNumber(value, name)}
        onChange={(n) => onChange(withOptions(value, { [name]: n ?? undefined }))}
        min={min}
        max={max}
        step={step}
        unit={unit}
        allowEmpty
        placeholder={placeholder}
      />
    </Field>
  )
}

export function SwitchOption({
  value,
  onChange,
  name,
  label,
  hint,
  className,
  defaultValue = false,
  disabled,
}: OptionProps & { defaultValue?: boolean; disabled?: boolean }) {
  return (
    <SwitchField
      label={label}
      description={hint}
      className={cn('py-0.5', className)}
      checked={optBool(value, name, defaultValue)}
      disabled={disabled}
      onCheckedChange={(v) => onChange(withOptions(value, { [name]: v }))}
    />
  )
}

const DEFAULT_SENTINEL = '__default__'

export interface ChoiceOption {
  value: string
  label: ReactNode
  disabled?: boolean
}

/** Select bound to an option; `defaultLabel` adds a first entry that clears the key (backend default). */
export function SelectOption({
  value,
  onChange,
  name,
  label,
  hint,
  className,
  options,
  defaultLabel,
  numeric,
}: OptionProps & { options: ChoiceOption[]; defaultLabel?: string; numeric?: boolean }) {
  const error = useFieldError(`options.${name}`)
  const raw = value.options?.[name]
  const current = raw === undefined || raw === null || raw === '' ? (defaultLabel ? DEFAULT_SENTINEL : undefined) : String(raw)
  const all = defaultLabel ? [{ value: DEFAULT_SENTINEL, label: defaultLabel }, ...options] : options
  // A stored value outside the list (imported or set by a newer version) stays selectable.
  const list = current && !all.some((o) => o.value === current) ? [...all, { value: current, label: current }] : all
  const id = useId()
  return (
    <Field label={label} hint={hint} error={error} className={className} htmlFor={id}>
      <SimpleSelect
        id={id}
        value={current}
        options={list}
        onValueChange={(v) => {
          if (v === DEFAULT_SENTINEL) onChange(withOptions(value, { [name]: undefined }))
          else onChange(withOptions(value, { [name]: numeric ? Number(v) : v }))
        }}
      />
    </Field>
  )
}

export function TagsOption({
  value,
  onChange,
  name,
  label,
  hint,
  className,
  placeholder,
  suggestions,
}: OptionProps & { placeholder?: string; suggestions?: string[] }) {
  const error = useFieldError(`options.${name}`)
  return (
    <Field label={label} hint={hint} error={error} className={className}>
      <TagInput
        value={optStrings(value, name)}
        onChange={(tags) => onChange(withOptions(value, { [name]: tags }))}
        placeholder={placeholder}
        suggestions={suggestions}
      />
    </Field>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// Combobox (free text + suggestions)
// ---------------------------------------------------------------------------------------------------------------------

export interface ComboSuggestion {
  value: string
  label?: string
  description?: string
}

export interface ComboInputProps {
  id?: string
  value: string
  onValueChange: (v: string) => void
  suggestions: readonly ComboSuggestion[]
  loading?: boolean
  placeholder?: string
  mono?: boolean
  disabled?: boolean
  className?: string
  /** Called when the list opens (lazy loading of suggestions). */
  onOpen?: () => void
  /** Shown when there are no suggestions (e.g. why a list is unavailable). */
  emptyText?: string
  onBlur?: () => void
  /** Enter without a highlighted suggestion; return true when handled (the form is then not submitted). */
  onEnter?: (text: string) => boolean
  'aria-describedby'?: string
  'aria-invalid'?: boolean
}

/**
 * Text input with a filterable suggestion list (↑↓ Enter Esc). Free text is always allowed. The list is rendered in
 * place (not portalled) so it scrolls inside modal dialogs.
 */
export function ComboInput({
  id,
  value,
  onValueChange,
  suggestions,
  loading,
  placeholder,
  mono,
  disabled,
  className,
  onOpen,
  emptyText,
  onBlur,
  onEnter,
  ...aria
}: ComboInputProps) {
  const [open, setOpen] = useState(false)
  const [active, setActive] = useState(-1)
  /** null: show everything (just opened); otherwise filter by the typed text. */
  const [filter, setFilter] = useState<string | null>(null)
  const wrapRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  const listId = useId()

  const matches = useMemo(() => {
    const q = (filter ?? '').trim().toLowerCase()
    const list = q
      ? suggestions.filter(
          (s) => s.value.toLowerCase().includes(q) || s.label?.toLowerCase().includes(q) || s.description?.toLowerCase().includes(q),
        )
      : suggestions
    return list.slice(0, 200)
  }, [suggestions, filter])

  const showEmpty = !loading && matches.length === 0 && !!emptyText && filter === null
  const visible = open && (matches.length > 0 || !!loading || showEmpty)

  const openList = () => {
    if (!open) onOpen?.()
    setOpen(true)
  }
  const pick = (s: ComboSuggestion) => {
    onValueChange(s.value)
    setOpen(false)
    setActive(-1)
  }

  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      openList()
      setActive((a) => Math.min(matches.length - 1, a + 1))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setActive((a) => Math.max(-1, a - 1))
    } else if (e.key === 'Enter') {
      if (visible && active >= 0 && matches[active]) {
        e.preventDefault()
        pick(matches[active])
      } else if (onEnter?.(value)) {
        e.preventDefault()
        setOpen(false)
      } else if (visible) {
        // Enter while choosing closes the list instead of submitting the surrounding form.
        e.preventDefault()
        setOpen(false)
      }
    } else if (e.key === 'Escape' && visible) {
      // Close the list only (the dialog stays open).
      e.preventDefault()
      e.stopPropagation()
      setOpen(false)
    }
  }

  return (
    <PopoverPrimitive.Root open={visible} onOpenChange={(o) => !o && setOpen(false)}>
      <PopoverPrimitive.Anchor asChild>
        <div ref={wrapRef} className={cn('relative flex w-full items-center', className)}>
          <input
            ref={inputRef}
            id={id}
            role="combobox"
            aria-expanded={visible}
            aria-controls={listId}
            aria-autocomplete="list"
            aria-activedescendant={visible && active >= 0 ? `${listId}-${active}` : undefined}
            value={value}
            placeholder={placeholder}
            disabled={disabled}
            spellCheck={false}
            autoComplete="off"
            autoCapitalize="off"
            className={cn(inputBase, 'h-8 pr-7', mono && 'font-mono')}
            onChange={(e) => {
              onValueChange(e.target.value)
              setFilter(e.target.value)
              setActive(-1)
              openList()
            }}
            onFocus={() => {
              setFilter(null)
              openList()
            }}
            onBlur={() => {
              setOpen(false)
              onBlur?.()
            }}
            onKeyDown={onKeyDown}
            {...aria}
          />
          <button
            type="button"
            tabIndex={-1}
            disabled={disabled}
            aria-label="Show suggestions"
            className="absolute right-1 flex size-6 items-center justify-center rounded-sm text-muted-foreground hover:bg-accent hover:text-foreground disabled:opacity-40"
            onMouseDown={(e) => {
              e.preventDefault()
              inputRef.current?.focus()
              setFilter(null)
              if (open) setOpen(false)
              else openList()
            }}
          >
            <ChevronDown className="size-3.5" />
          </button>
        </div>
      </PopoverPrimitive.Anchor>
      <PopoverPrimitive.Content
        align="start"
        sideOffset={4}
        onOpenAutoFocus={(e) => e.preventDefault()}
        onCloseAutoFocus={(e) => e.preventDefault()}
        onInteractOutside={(e) => {
          if (wrapRef.current?.contains(e.target as Node)) e.preventDefault()
        }}
        className="z-50 max-h-64 w-(--radix-popover-trigger-width) min-w-48 overflow-y-auto overscroll-contain rounded-md border bg-popover p-1 text-popover-foreground shadow-popover"
      >
        <ul id={listId} role="listbox" aria-label="Suggestions">
          {loading && <li className="px-2 py-1.5 text-sm text-muted-foreground">Loading…</li>}
          {showEmpty && <li className="px-2 py-1.5 text-sm text-muted-foreground">{emptyText}</li>}
          {matches.map((s, i) => (
            <li
              key={`${s.value}-${i}`}
              id={`${listId}-${i}`}
              role="option"
              aria-selected={i === active}
              onMouseDown={(e) => {
                e.preventDefault()
                pick(s)
              }}
              onMouseEnter={() => setActive(i)}
              className={cn('flex h-7 cursor-default items-center gap-2 rounded-sm px-2 text-base', i === active && 'bg-accent text-accent-foreground')}
            >
              <span className={cn('truncate', mono && 'font-mono text-sm')}>{s.label ?? s.value}</span>
              {s.description && <span className="ml-auto truncate pl-3 text-xs text-muted-foreground">{s.description}</span>}
            </li>
          ))}
        </ul>
      </PopoverPrimitive.Content>
    </PopoverPrimitive.Root>
  )
}

/** Text option with suggestions (serial ports, containers, pods, shells, term types…). */
export function ComboOption({
  value,
  onChange,
  name,
  label,
  hint,
  className,
  placeholder,
  suggestions,
  loading,
  mono,
  numeric,
  required,
  onOpen,
  emptyText,
}: OptionProps & {
  placeholder?: string
  suggestions: readonly ComboSuggestion[]
  loading?: boolean
  mono?: boolean
  /** Store as a number when the text is numeric. */
  numeric?: boolean
  required?: boolean
  onOpen?: () => void
  emptyText?: string
}) {
  const error = useFieldError(`options.${name}`)
  const text = optString(value, name)
  return (
    <Field label={label} hint={hint} error={error} className={className} required={required}>
      <ComboInput
        value={text}
        suggestions={suggestions}
        loading={loading}
        placeholder={placeholder}
        mono={mono}
        onOpen={onOpen}
        emptyText={emptyText}
        onValueChange={(v) => {
          const t = numeric && /^\d+$/.test(v.trim()) ? Number(v.trim()) : v
          onChange(withOptions(value, { [name]: t }))
        }}
      />
    </Field>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// Secrets
// ---------------------------------------------------------------------------------------------------------------------

/**
 * Write-only secret: shows "stored" when the vault holds a value (secretKeys) with Change / Remove actions; typing sets
 * a new value; removing sends "" on save. Never displays stored values (they never reach the browser).
 */
export function SecretInput({
  value,
  onChange,
  secret,
  placeholder,
  id,
  generate,
  'aria-describedby': describedBy,
  'aria-invalid': invalid,
}: {
  value: Connection
  onChange: (next: Connection) => void
  secret: string
  placeholder?: string
  id?: string
  generate?: boolean
  'aria-describedby'?: string
  'aria-invalid'?: boolean
}) {
  const pending = value.secrets?.[secret]
  const stored = (value.secretKeys ?? []).includes(secret)
  const [editing, setEditing] = useState(false)

  if (stored && pending === '') {
    return (
      <div className="flex h-8 items-center gap-2 rounded-md border border-dashed px-2.5 text-sm text-muted-foreground">
        <span className="flex-1 truncate">Stored value will be removed when you save</span>
        <Button variant="ghost" size="xs" onClick={() => onChange(withSecret(value, secret, undefined))}>
          <Undo2 /> Undo
        </Button>
      </div>
    )
  }

  if (stored && pending === undefined && !editing) {
    return (
      <div className="flex h-8 items-center gap-2 rounded-md border bg-muted/30 px-2.5">
        <KeyRound className="size-3.5 text-muted-foreground" />
        <span className="font-mono text-sm tracking-widest text-muted-foreground" aria-hidden>
          ••••••••
        </span>
        <Badge variant="success" className="ml-1">
          Stored
        </Badge>
        <span className="flex-1" />
        <Tooltip content="Replace the stored value">
          <Button variant="ghost" size="icon-xs" aria-label="Change stored secret" onClick={() => setEditing(true)} id={id}>
            <Pencil />
          </Button>
        </Tooltip>
        <Tooltip content="Remove the stored value">
          <Button variant="ghost" size="icon-xs" aria-label="Remove stored secret" onClick={() => onChange(withSecret(value, secret, ''))}>
            <X />
          </Button>
        </Tooltip>
      </div>
    )
  }

  return (
    <div className="flex items-center gap-1.5">
      <PasswordInput
        id={id}
        value={pending ?? ''}
        placeholder={placeholder ?? (stored ? 'New value' : 'Not stored — you will be asked when connecting')}
        generate={generate}
        autoFocus={editing}
        aria-describedby={describedBy}
        aria-invalid={invalid}
        data-lpignore="true"
        data-1p-ignore=""
        data-bwignore=""
        onChange={(e) => onChange(withSecret(value, secret, e.target.value === '' ? undefined : e.target.value))}
      />
      {stored && (
        <Tooltip content="Keep the stored value">
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label="Keep the stored value"
            onClick={() => {
              setEditing(false)
              onChange(withSecret(value, secret, undefined))
            }}
          >
            <Undo2 />
          </Button>
        </Tooltip>
      )}
    </div>
  )
}

export function SecretOption({
  value,
  onChange,
  secret,
  label,
  hint,
  className,
  placeholder,
}: {
  value: Connection
  onChange: (next: Connection) => void
  secret: string
  label: ReactNode
  hint?: ReactNode
  className?: string
  placeholder?: string
}) {
  const error = useFieldError(`secrets.${secret}`)
  return (
    <Field label={label} hint={hint} error={error} className={className}>
      <SecretInput value={value} onChange={onChange} secret={secret} placeholder={placeholder} />
    </Field>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// Key / value (environment variables)
// ---------------------------------------------------------------------------------------------------------------------

interface KvRow {
  id: number
  key: string
  value: string
}

const ENV_NAME_RE = /^[A-Za-z_][A-Za-z0-9_]*$/

function toRecord(rows: KvRow[]): Record<string, string> {
  const out: Record<string, string> = {}
  for (const r of rows) {
    const k = r.key.trim()
    if (k) out[k] = r.value
  }
  return out
}

function recordOf(value: Connection, name: string): Record<string, string> {
  const v = value.options?.[name]
  if (!isPlainObject(v)) return {}
  const out: Record<string, string> = {}
  for (const [k, x] of Object.entries(v)) if (typeof x === 'string') out[k] = x
  return out
}

let kvSeq = 0

/** Environment-variable editor bound to `options[name]` (Record<string, string>). */
export function KeyValueOption({ value, onChange, name, label, hint, className }: OptionProps) {
  const env = recordOf(value, name)
  const envKey = JSON.stringify(env)
  const [rows, setRows] = useState<KvRow[]>(() => Object.entries(env).map(([key, v]) => ({ id: ++kvSeq, key, value: v })))
  const lastCommitted = useRef(envKey)

  // External changes (not caused by our own edits) reset the rows.
  useEffect(() => {
    if (envKey !== lastCommitted.current) {
      lastCommitted.current = envKey
      setRows(Object.entries(JSON.parse(envKey) as Record<string, string>).map(([key, v]) => ({ id: ++kvSeq, key, value: v })))
    }
  }, [envKey])

  const commit = (next: KvRow[]) => {
    setRows(next)
    const rec = toRecord(next)
    lastCommitted.current = JSON.stringify(rec)
    onChange(withOptions(value, { [name]: rec }))
  }

  const dupes = new Set<string>()
  const seen = new Set<string>()
  for (const r of rows) {
    const k = r.key.trim()
    if (k && seen.has(k)) dupes.add(k)
    seen.add(k)
  }

  return (
    <div className={cn('grid gap-1.5', className)}>
      <div className="flex items-center justify-between gap-2">
        <span className="text-sm font-medium text-foreground/90">{label}</span>
        <Button variant="ghost" size="xs" onClick={() => commit([...rows, { id: ++kvSeq, key: '', value: '' }])}>
          <Plus /> Add
        </Button>
      </div>
      {rows.length === 0 ? (
        <p className="text-sm text-muted-foreground">{hint ?? 'No variables.'}</p>
      ) : (
        <div className="grid gap-1.5">
          {rows.map((r, i) => {
            const k = r.key.trim()
            const bad = k !== '' && (!ENV_NAME_RE.test(k) || dupes.has(k))
            return (
              <div key={r.id} className="flex items-center gap-1.5">
                <Input
                  aria-label={`Variable ${i + 1} name`}
                  aria-invalid={bad || undefined}
                  value={r.key}
                  placeholder="NAME"
                  spellCheck={false}
                  autoComplete="off"
                  className="w-2/5 font-mono"
                  onChange={(e) => commit(rows.map((x) => (x.id === r.id ? { ...x, key: e.target.value } : x)))}
                />
                <span className="text-muted-foreground">=</span>
                <Input
                  aria-label={`Variable ${i + 1} value`}
                  value={r.value}
                  placeholder="value"
                  spellCheck={false}
                  autoComplete="off"
                  className="flex-1 font-mono"
                  onChange={(e) => commit(rows.map((x) => (x.id === r.id ? { ...x, value: e.target.value } : x)))}
                />
                <Button variant="ghost" size="icon-sm" aria-label="Remove variable" onClick={() => commit(rows.filter((x) => x.id !== r.id))}>
                  <X />
                </Button>
              </div>
            )
          })}
          {dupes.size > 0 && <p className="text-sm text-destructive">Duplicate names: {[...dupes].join(', ')} (the last one wins)</p>}
        </div>
      )}
    </div>
  )
}

/** Validation of a Record<string,string> option holding environment variables. */
export function validateEnv(value: Connection, name = 'env'): string | undefined {
  const bad = Object.keys(recordOf(value, name)).filter((k) => !ENV_NAME_RE.test(k))
  return bad.length ? `Invalid variable name${bad.length > 1 ? 's' : ''}: ${bad.join(', ')}` : undefined
}

// ---------------------------------------------------------------------------------------------------------------------
// Shared choices
// ---------------------------------------------------------------------------------------------------------------------

/** What Enter sends (rlogin / raw / serial `lineEnding`). */
export const LINE_ENDING_CHOICES: ChoiceOption[] = [
  { value: 'crlf', label: 'CR LF (\\r\\n)' },
  { value: 'cr', label: 'CR (\\r)' },
  { value: 'lf', label: 'LF (\\n)' },
]

// ---------------------------------------------------------------------------------------------------------------------
// Saved-connection picker
// ---------------------------------------------------------------------------------------------------------------------

const NONE = '__none__'

/** Label of a saved connection in pickers: "name — user@host". */
function connectionChoiceLabel(c: Connection): string {
  const target = connectionTarget(c)
  return target && target !== c.name ? `${c.name} — ${target}` : c.name
}

/**
 * Pick another saved connection (gateway, "Docker via SSH"); stores its id in `options[name]`. Searchable, so it stays
 * usable with thousands of saved sessions.
 */
export function ConnectionSelectOption({
  value,
  onChange,
  name,
  label,
  hint,
  className,
  filter,
  noneLabel = 'None',
}: OptionProps & { filter: (c: Connection) => boolean; noneLabel?: string }) {
  const env = useEditorEnv()
  const error = useFieldError(`options.${name}`)
  const current = optString(value, name)
  /** Text being typed (null: show the current selection). */
  const [text, setText] = useState<string | null>(null)
  const list = useMemo(() => env.connections.filter((c) => c.id !== env.selfId && filter(c)), [env.connections, env.selfId, filter])
  const suggestions = useMemo<ComboSuggestion[]>(
    () => [{ value: NONE, label: noneLabel }, ...list.map((c) => ({ value: c.id, label: c.name, description: connectionTarget(c) }))],
    [list, noneLabel],
  )
  const known = current ? env.connections.find((c) => c.id === current) : undefined
  const shown = text ?? (current ? (known ? connectionChoiceLabel(known) : 'Unknown session (deleted?)') : '')
  return (
    <Field label={label} hint={hint} error={error} className={className}>
      <ComboInput
        value={shown}
        placeholder={noneLabel}
        suggestions={suggestions}
        emptyText="No matching session"
        onBlur={() => setText(null)}
        onValueChange={(v) => {
          if (v === NONE) {
            setText(null)
            onChange(withOptions(value, { [name]: undefined }))
          } else if (list.some((c) => c.id === v)) {
            setText(null)
            onChange(withOptions(value, { [name]: v }))
          } else setText(v)
        }}
      />
    </Field>
  )
}
