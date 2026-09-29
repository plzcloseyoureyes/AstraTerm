/* Small UI pieces shared by the editor, text and diff tabs (lazy chunk). */
import { useEffect, useId, useMemo, useRef, useState, type ComponentProps, type ReactNode } from 'react'
import { Check, ChevronDown, CircleAlert, Info, TriangleAlert, X } from 'lucide-react'
import type { IconType } from '@/app/registry'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuSub,
  DropdownMenuSubContent,
  DropdownMenuSubTrigger,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { Delayed, LoadingPane, Spinner } from '@/components/ui/spinner'
import { Tooltip } from '@/components/ui/tooltip'
import { DELAY_PRESETS } from '@/lib/useDelayedFlag'
import { cn } from '@/lib/utils'
import { ENCODINGS, EOL_LABEL, type Eol } from './codec'

// ---------------------------------------------------------------------------------------------------------------------
// banner
// ---------------------------------------------------------------------------------------------------------------------

export interface BannerAction {
  label: string
  onClick: () => void
  primary?: boolean
  disabled?: boolean
}

const BANNER_TONE = {
  info: { icon: Info, box: 'border-info/35 bg-info/10', icon_: 'text-info' },
  warning: { icon: TriangleAlert, box: 'border-warning/40 bg-warning/12', icon_: 'text-warning' },
  danger: { icon: CircleAlert, box: 'border-destructive/40 bg-destructive/10', icon_: 'text-destructive' },
} as const

/** Inline message strip above the editor ("changed on the server", "read-only", ...). */
export function Banner({
  tone = 'info',
  children,
  actions = [],
  onDismiss,
  icon,
}: {
  tone?: keyof typeof BANNER_TONE
  children: ReactNode
  actions?: BannerAction[]
  onDismiss?: () => void
  icon?: IconType
}) {
  const t = BANNER_TONE[tone]
  const Icon = icon ?? t.icon
  return (
    <div
      role={tone === 'danger' ? 'alert' : 'status'}
      className={cn('flex shrink-0 flex-wrap items-center gap-x-3 gap-y-1.5 border-b px-3 py-1.5 text-sm', t.box)}
    >
      <Icon className={cn('size-4 shrink-0', t.icon_)} aria-hidden />
      <div className="min-w-0 flex-1 text-foreground">{children}</div>
      {actions.length > 0 && (
        <div className="flex flex-wrap items-center gap-1.5">
          {actions.map((a) => (
            <Button key={a.label} size="xs" variant={a.primary ? 'default' : 'secondary'} disabled={a.disabled} onClick={a.onClick}>
              {a.label}
            </Button>
          ))}
        </div>
      )}
      {onDismiss && (
        <button
          type="button"
          className="-mr-1 rounded-sm p-0.5 text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
          aria-label="Dismiss"
          onClick={onDismiss}
        >
          <X className="size-3.5" />
        </button>
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// status bar
// ---------------------------------------------------------------------------------------------------------------------

export function StatusBar({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <div
      className={cn(
        'flex h-6 shrink-0 items-center gap-0.5 overflow-hidden border-t bg-statusbar px-1.5 text-xs text-statusbar-foreground select-none',
        className,
      )}
    >
      {children}
    </div>
  )
}

export function StatusSpacer() {
  return <div className="min-w-2 flex-1" />
}

/** Clickable (or static) status bar segment. */
export function StatusButton({
  children,
  tooltip,
  onClick,
  className,
  ...rest
}: {
  children: ReactNode
  tooltip?: string
  onClick?: () => void
  className?: string
} & Omit<ComponentProps<'button'>, 'onClick' | 'children'>) {
  const btn = (
    <button
      type="button"
      className={cn(
        'inline-flex h-5 max-w-full shrink-0 items-center gap-1 rounded-sm px-1.5 whitespace-nowrap outline-none',
        'hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50 disabled:pointer-events-none',
        className,
      )}
      onClick={onClick}
      {...rest}
    >
      {children}
    </button>
  )
  return tooltip ? (
    <Tooltip content={tooltip} side="top">
      {btn}
    </Tooltip>
  ) : (
    btn
  )
}

export function StatusText({ children, className, title }: { children: ReactNode; className?: string; title?: string }) {
  return (
    <span className={cn('inline-flex h-5 shrink-0 items-center gap-1 px-1.5 whitespace-nowrap', className)} title={title}>
      {children}
    </span>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// pickers
// ---------------------------------------------------------------------------------------------------------------------

export function EolMenu({
  eol,
  mixed,
  onChange,
  disabled,
  className,
}: {
  eol: Eol
  mixed?: boolean
  onChange: (e: Eol) => void
  disabled?: boolean
  className?: string
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild disabled={disabled}>
        <StatusButton
          className={className}
          aria-label={`Line endings: ${EOL_LABEL[eol]}${mixed ? ', mixed' : ''}. Change`}
          title={mixed ? 'Mixed line endings: every line keeps its own when saving (new lines get ' + EOL_LABEL[eol] + '). Choose one to convert all lines.' : 'Line endings'}
        >
          {EOL_LABEL[eol]}
          {mixed && <span className="text-warning">*</span>}
        </StatusButton>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" side="top">
        <DropdownMenuLabel>{mixed ? 'Mixed line endings — convert all lines to' : 'Line endings'}</DropdownMenuLabel>
        {(['lf', 'crlf', 'cr'] as Eol[]).map((e) => (
          <DropdownMenuItem key={e} onSelect={() => onChange(e)}>
            <span className="flex size-3.5 items-center justify-center">{e === eol && !mixed && <Check className="size-3.5" />}</span>
            {EOL_LABEL[e]}
            <span className="ml-auto pl-4 text-xs text-muted-foreground">{e === 'lf' ? 'Unix, macOS' : e === 'crlf' ? 'Windows' : 'Classic Mac'}</span>
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

const ENCODING_GROUPS = Array.from(new Set(ENCODINGS.map((e) => e.group)))

export function EncodingMenu({
  label,
  encoding,
  bom,
  onReopen,
  onSaveWith,
  disabled,
  saveDisabled,
  className,
}: {
  label: string
  encoding: string
  bom: boolean
  /** Re-decode the file with another encoding (unavailable once edited). */
  onReopen?: (enc: string) => void
  onSaveWith: (enc: string, bom: boolean) => void
  disabled?: boolean
  /** Read-only documents can still be reopened with another encoding, not re-encoded. */
  saveDisabled?: boolean
  className?: string
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild disabled={disabled}>
        <StatusButton className={className} aria-label={`Encoding: ${label}. Change`}>
          {label}
        </StatusButton>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" side="top" className="min-w-56">
        {onReopen && (
          <DropdownMenuSub>
            <DropdownMenuSubTrigger>Reopen with encoding</DropdownMenuSubTrigger>
            <DropdownMenuSubContent className="max-h-96 overflow-y-auto">
              {ENCODING_GROUPS.map((g, gi) => (
                <div key={g}>
                  {gi > 0 && <DropdownMenuSeparator />}
                  <DropdownMenuLabel>{g}</DropdownMenuLabel>
                  {ENCODINGS.filter((e) => e.group === g).map((e) => (
                    <DropdownMenuItem key={e.id} onSelect={() => onReopen(e.id)}>
                      <span className="flex size-3.5 items-center justify-center">{e.id === encoding && <Check className="size-3.5" />}</span>
                      {e.label}
                    </DropdownMenuItem>
                  ))}
                </div>
              ))}
            </DropdownMenuSubContent>
          </DropdownMenuSub>
        )}
        <DropdownMenuSub>
          <DropdownMenuSubTrigger disabled={saveDisabled}>Save with encoding</DropdownMenuSubTrigger>
          <DropdownMenuSubContent className="max-h-96 overflow-y-auto">
            <DropdownMenuItem onSelect={() => onSaveWith('utf-8', false)}>
              <span className="flex size-3.5 items-center justify-center">{encoding === 'utf-8' && !bom && <Check className="size-3.5" />}</span>
              UTF-8
            </DropdownMenuItem>
            <DropdownMenuItem onSelect={() => onSaveWith('utf-8', true)}>
              <span className="flex size-3.5 items-center justify-center">{encoding === 'utf-8' && bom && <Check className="size-3.5" />}</span>
              UTF-8 with BOM
            </DropdownMenuItem>
            <DropdownMenuItem onSelect={() => onSaveWith('utf-16le', true)}>
              <span className="flex size-3.5 items-center justify-center">{encoding === 'utf-16le' && <Check className="size-3.5" />}</span>
              UTF-16 LE (BOM)
            </DropdownMenuItem>
            <DropdownMenuItem onSelect={() => onSaveWith('utf-16be', true)}>
              <span className="flex size-3.5 items-center justify-center">{encoding === 'utf-16be' && <Check className="size-3.5" />}</span>
              UTF-16 BE (BOM)
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            {ENCODINGS.filter((e) => e.canEncode && !e.id.startsWith('utf-')).map((e) => (
              <DropdownMenuItem key={e.id} onSelect={() => onSaveWith(e.id, false)}>
                <span className="flex size-3.5 items-center justify-center">{e.id === encoding && <Check className="size-3.5" />}</span>
                {e.label}
              </DropdownMenuItem>
            ))}
          </DropdownMenuSubContent>
        </DropdownMenuSub>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

export interface IndentValue {
  useTabs: boolean
  size: number
}

export function IndentMenu({
  indent,
  tabSize,
  onChange,
  onConvert,
  onDetect,
  disabled,
  className,
}: {
  indent: IndentValue
  tabSize: number
  onChange: (v: IndentValue) => void
  onConvert: () => void
  onDetect: () => void
  disabled?: boolean
  className?: string
}) {
  const label = indent.useTabs ? `Tab Size: ${tabSize}` : `Spaces: ${indent.size}`
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild disabled={disabled}>
        <StatusButton className={className} aria-label={`Indentation: ${label}. Change`}>
          {label}
        </StatusButton>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" side="top" className="min-w-52">
        <DropdownMenuLabel>Indent using</DropdownMenuLabel>
        {[2, 4, 8].map((n) => (
          <DropdownMenuItem key={`s${n}`} onSelect={() => onChange({ useTabs: false, size: n })}>
            <span className="flex size-3.5 items-center justify-center">{!indent.useTabs && indent.size === n && <Check className="size-3.5" />}</span>
            {n} spaces
          </DropdownMenuItem>
        ))}
        <DropdownMenuItem onSelect={() => onChange({ useTabs: true, size: tabSize })}>
          <span className="flex size-3.5 items-center justify-center">{indent.useTabs && <Check className="size-3.5" />}</span>
          Tabs
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem onSelect={onDetect}>Detect from content</DropdownMenuItem>
        <DropdownMenuItem onSelect={onConvert}>Convert indentation to {indent.useTabs ? 'tabs' : 'spaces'}</DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

/** Searchable language list (keyboard: type to filter, ↑/↓, Enter). */
export function LanguagePicker({
  value,
  names,
  hint,
  loading,
  onChange,
  disabled,
}: {
  value: string
  names: string[]
  hint?: (name: string) => string
  loading?: boolean
  onChange: (name: string) => void
  disabled?: boolean
}) {
  const [open, setOpen] = useState(false)
  const [q, setQ] = useState('')
  const [idx, setIdx] = useState(0)
  const listRef = useRef<HTMLDivElement>(null)
  const listId = useId()
  const filtered = useMemo(() => {
    const s = q.trim().toLowerCase()
    if (!s) return names
    const starts = names.filter((n) => n.toLowerCase().startsWith(s))
    const rest = names.filter((n) => !n.toLowerCase().startsWith(s) && (n.toLowerCase().includes(s) || (hint?.(n) ?? '').toLowerCase().includes(s)))
    return [...starts, ...rest]
  }, [q, names, hint])
  useEffect(() => setIdx(0), [q])
  useEffect(() => {
    listRef.current?.querySelector<HTMLElement>(`[data-idx="${idx}"]`)?.scrollIntoView({ block: 'nearest' })
  }, [idx, open])
  const pick = (name: string) => {
    setOpen(false)
    setQ('')
    onChange(name)
  }
  return (
    <Popover
      open={open}
      onOpenChange={(o) => {
        setOpen(o)
        if (o) setIdx(Math.max(0, names.indexOf(value)))
        else setQ('')
      }}
    >
      <PopoverTrigger asChild disabled={disabled}>
        <StatusButton aria-label={`Language: ${value}. Change`} aria-haspopup="listbox">
          {value}
          <Spinner active={!!loading} className="size-3" label="Loading the language" />
          <ChevronDown className="size-3 opacity-60" aria-hidden />
        </StatusButton>
      </PopoverTrigger>
      <PopoverContent side="top" align="end" className="w-64 p-1.5">
        <input
          autoFocus
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder="Select language mode…"
          aria-label="Filter languages"
          role="combobox"
          aria-expanded
          aria-autocomplete="list"
          aria-controls={listId}
          aria-activedescendant={filtered[idx] ? `${listId}-${idx}` : undefined}
          className="mb-1 h-7 w-full rounded-sm border border-input bg-background px-2 text-sm outline-none focus-visible:border-ring"
          onKeyDown={(e) => {
            if (e.key === 'ArrowDown') {
              e.preventDefault()
              setIdx((i) => Math.min(filtered.length - 1, i + 1))
            } else if (e.key === 'ArrowUp') {
              e.preventDefault()
              setIdx((i) => Math.max(0, i - 1))
            } else if (e.key === 'Enter') {
              e.preventDefault()
              const n = filtered[idx]
              if (n) pick(n)
            }
          }}
        />
        <div ref={listRef} id={listId} role="listbox" aria-label="Languages" className="max-h-72 overflow-y-auto">
          {filtered.length === 0 && <div className="px-2 py-1.5 text-sm text-muted-foreground">No matching language</div>}
          {filtered.map((n, i) => (
            <div
              key={n}
              id={`${listId}-${i}`}
              role="option"
              aria-selected={i === idx}
              aria-checked={n === value}
              data-idx={i}
              className={cn(
                'flex h-7 cursor-default items-center gap-2 rounded-sm px-2 text-sm',
                i === idx ? 'bg-accent text-accent-foreground' : 'hover:bg-accent/60',
              )}
              onMouseEnter={() => setIdx(i)}
              onClick={() => pick(n)}
            >
              <span className="flex size-3.5 items-center justify-center">{n === value && <Check className="size-3.5" />}</span>
              <span className="truncate">{n}</span>
              {hint && <span className="ml-auto truncate pl-2 text-xs text-muted-foreground">{hint(n)}</span>}
            </div>
          ))}
        </div>
      </PopoverContent>
    </Popover>
  )
}

// ---------------------------------------------------------------------------------------------------------------------
// full-pane states
// ---------------------------------------------------------------------------------------------------------------------

export function PaneMessage({
  icon: Icon,
  title,
  children,
  actions,
  tone = 'default',
}: {
  icon: IconType
  title: ReactNode
  children?: ReactNode
  actions?: ReactNode
  tone?: 'default' | 'warning' | 'danger'
}) {
  return (
    <div className="flex h-full w-full items-center justify-center overflow-auto p-6">
      <div className="flex max-w-lg flex-col items-center gap-2.5 text-center">
        <div
          className={cn(
            'mb-1 flex size-12 items-center justify-center rounded-xl border bg-muted/50',
            tone === 'warning' ? 'text-warning' : tone === 'danger' ? 'text-destructive' : 'text-muted-foreground',
          )}
        >
          <Icon className="size-5" />
        </div>
        <div className="text-md font-medium text-foreground">{title}</div>
        {children && <div className="text-sm text-balance text-muted-foreground">{children}</div>}
        {actions && <div className="mt-2 flex flex-wrap items-center justify-center gap-2">{actions}</div>}
      </div>
    </div>
  )
}

/**
 * Cover over an editor area that is still opening (the file, the Monaco bundle) — navigation-like: nothing for the
 * first second, then a centered spinner that stays at least 0.8 s (DELAY_PRESETS.NAVIGATION), over the content that
 * arrived meanwhile. Needs a positioned parent.
 */
export function LoadingOverlay({ active, label, className = 'bg-panel/80 backdrop-blur-[1px]' }: { active: boolean; label?: string; className?: string }) {
  return (
    <Delayed active={active} {...DELAY_PRESETS.NAVIGATION}>
      <div className={cn('absolute inset-0 z-10 animate-in fade-in-0 duration-150', className)}>
        <LoadingPane immediate label={label} />
      </div>
    </Delayed>
  )
}
