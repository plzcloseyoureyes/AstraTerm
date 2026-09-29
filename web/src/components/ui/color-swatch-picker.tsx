import * as React from 'react'
import { Check, Pipette, X } from 'lucide-react'
import { cn } from '@/lib/utils'

export const DEFAULT_SWATCHES = [
  '#ef4444',
  '#f97316',
  '#f59e0b',
  '#eab308',
  '#84cc16',
  '#22c55e',
  '#14b8a6',
  '#06b6d4',
  '#0ea5e9',
  '#3b82f6',
  '#6366f1',
  '#8b5cf6',
  '#a855f7',
  '#ec4899',
  '#f43f5e',
  '#64748b',
]

export interface ColorSwatchPickerProps {
  value: string | undefined | null
  onChange: (color: string | undefined) => void
  /** Preset colours (hex) or {value, label, swatch} objects for named presets. */
  swatches?: (string | { value: string; label: string; swatch: string })[]
  /** Allow a custom colour via the native picker. */
  allowCustom?: boolean
  /** Show a "none" swatch that clears the value. */
  allowNone?: boolean
  size?: 'sm' | 'md'
  className?: string
  'aria-label'?: string
}

/** Accessible swatch grid (radio group semantics, arrow-key navigation). */
export function ColorSwatchPicker({
  value,
  onChange,
  swatches = DEFAULT_SWATCHES,
  allowCustom = true,
  allowNone = false,
  size = 'md',
  className,
  'aria-label': ariaLabel = 'Colour',
}: ColorSwatchPickerProps) {
  const items = swatches.map((s) => (typeof s === 'string' ? { value: s, label: s, swatch: s } : s))
  const isCustom = !!value && !items.some((i) => i.value.toLowerCase() === value.toLowerCase())
  const dim = size === 'sm' ? 'size-5' : 'size-6'
  const refs = React.useRef<(HTMLButtonElement | null)[]>([])

  const onKey = (e: React.KeyboardEvent, i: number) => {
    const n = items.length
    let next = -1
    if (e.key === 'ArrowRight' || e.key === 'ArrowDown') next = (i + 1) % n
    if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') next = (i - 1 + n) % n
    if (next >= 0) {
      e.preventDefault()
      refs.current[next]?.focus()
      onChange(items[next].value)
    }
  }

  return (
    <div role="radiogroup" aria-label={ariaLabel} className={cn('flex flex-wrap items-center gap-1.5', className)}>
      {allowNone && (
        <button
          type="button"
          role="radio"
          aria-checked={!value}
          aria-label="None"
          title="None"
          onClick={() => onChange(undefined)}
          className={cn(
            dim,
            'flex items-center justify-center rounded-md border border-dashed text-muted-foreground outline-none',
            'focus-visible:ring-2 focus-visible:ring-ring/60',
            !value && 'ring-2 ring-foreground/60 ring-offset-1 ring-offset-background',
          )}
        >
          <X className="size-3" />
        </button>
      )}
      {items.map((item, i) => {
        const selected = !!value && value.toLowerCase() === item.value.toLowerCase()
        return (
          <button
            key={item.value}
            ref={(el) => {
              refs.current[i] = el
            }}
            type="button"
            role="radio"
            aria-checked={selected}
            aria-label={item.label}
            title={item.label}
            tabIndex={selected || (!value && i === 0) ? 0 : -1}
            onClick={() => onChange(item.value)}
            onKeyDown={(e) => onKey(e, i)}
            className={cn(
              dim,
              'flex items-center justify-center rounded-md border border-black/10 outline-none transition-transform hover:scale-110',
              'focus-visible:ring-2 focus-visible:ring-ring/60 focus-visible:ring-offset-1 focus-visible:ring-offset-background',
              selected && 'ring-2 ring-foreground/70 ring-offset-1 ring-offset-background',
            )}
            style={{ background: item.swatch }}
          >
            {selected && <Check className="size-3 text-white drop-shadow" strokeWidth={3} />}
          </button>
        )
      })}
      {allowCustom && (
        <label
          className={cn(
            dim,
            'relative flex cursor-pointer items-center justify-center overflow-hidden rounded-md border text-muted-foreground hover:text-foreground',
            'focus-within:ring-2 focus-within:ring-ring/60',
            isCustom && 'ring-2 ring-foreground/70 ring-offset-1 ring-offset-background',
          )}
          style={isCustom ? { background: value ?? undefined } : undefined}
          title="Custom colour"
        >
          {!isCustom && <Pipette className="size-3" />}
          <input
            type="color"
            className="absolute inset-0 cursor-pointer opacity-0"
            value={value && /^#[0-9a-f]{6}$/i.test(value) ? value : '#3b82f6'}
            onChange={(e) => onChange(e.target.value)}
            aria-label="Custom colour"
          />
        </label>
      )}
    </div>
  )
}
