import * as React from 'react'
import { ChevronDown, ChevronUp } from 'lucide-react'
import { clamp, cn } from '@/lib/utils'
import { inputBase } from './input'

export interface NumberInputProps extends Omit<React.ComponentProps<'input'>, 'value' | 'onChange' | 'type' | 'min' | 'max' | 'step'> {
  value: number | null | undefined
  onChange: (value: number | null) => void
  min?: number
  max?: number
  step?: number
  /** Allow clearing to null (otherwise an empty field reverts on blur). */
  allowEmpty?: boolean
  /** Unit label shown inside the field (e.g. "ms", "px"). */
  unit?: string
  inputSize?: 'sm' | 'md'
  /** Integer only (default true). */
  integer?: boolean
}

/** Numeric field with stepper buttons, clamping, arrow-key stepping (Shift = ×10). */
export function NumberInput({
  value,
  onChange,
  min = Number.NEGATIVE_INFINITY,
  max = Number.POSITIVE_INFINITY,
  step = 1,
  allowEmpty = false,
  unit,
  inputSize = 'md',
  integer = true,
  className,
  disabled,
  onBlur,
  ...props
}: NumberInputProps) {
  const [text, setText] = React.useState(value == null ? '' : String(value))
  React.useEffect(() => {
    setText(value == null ? '' : String(value))
  }, [value])

  const commit = (raw: string) => {
    const trimmed = raw.trim()
    if (trimmed === '') {
      if (allowEmpty) onChange(null)
      else setText(value == null ? '' : String(value))
      return
    }
    let n = Number(trimmed)
    if (!Number.isFinite(n)) {
      setText(value == null ? '' : String(value))
      return
    }
    if (integer) n = Math.round(n)
    n = clamp(n, min, max)
    setText(String(n))
    if (n !== value) onChange(n)
  }

  const bump = (dir: 1 | -1, big = false) => {
    const base = value ?? (Number.isFinite(min) ? min : 0)
    const n = clamp(base + dir * step * (big ? 10 : 1), min, max)
    onChange(integer ? Math.round(n) : Number(n.toFixed(6)))
  }

  return (
    <div className={cn('relative flex w-full items-center', className)}>
      <input
        type="text"
        inputMode={integer ? 'numeric' : 'decimal'}
        value={text}
        disabled={disabled}
        className={cn(inputBase, 'tabular pr-7', inputSize === 'sm' ? 'h-7 text-sm' : 'h-8', unit && 'pr-12')}
        onChange={(e) => setText(e.target.value)}
        onBlur={(e) => {
          commit(e.target.value)
          onBlur?.(e)
        }}
        onKeyDown={(e) => {
          if (e.key === 'ArrowUp') {
            e.preventDefault()
            bump(1, e.shiftKey)
          } else if (e.key === 'ArrowDown') {
            e.preventDefault()
            bump(-1, e.shiftKey)
          } else if (e.key === 'Enter') {
            commit((e.target as HTMLInputElement).value)
          }
        }}
        aria-valuemin={Number.isFinite(min) ? min : undefined}
        aria-valuemax={Number.isFinite(max) ? max : undefined}
        aria-valuenow={value ?? undefined}
        role="spinbutton"
        {...props}
      />
      {unit && <span className="pointer-events-none absolute right-7 text-sm text-muted-foreground">{unit}</span>}
      <div className="absolute right-0.5 flex h-[calc(100%-4px)] flex-col">
        <button
          type="button"
          tabIndex={-1}
          disabled={disabled || (value != null && value >= max)}
          onClick={() => bump(1)}
          className="flex flex-1 items-center justify-center rounded-t-sm px-1 text-muted-foreground hover:bg-accent hover:text-foreground disabled:opacity-40"
          aria-label="Increase"
        >
          <ChevronUp className="size-3" />
        </button>
        <button
          type="button"
          tabIndex={-1}
          disabled={disabled || (value != null && value <= min)}
          onClick={() => bump(-1)}
          className="flex flex-1 items-center justify-center rounded-b-sm px-1 text-muted-foreground hover:bg-accent hover:text-foreground disabled:opacity-40"
          aria-label="Decrease"
        >
          <ChevronDown className="size-3" />
        </button>
      </div>
    </div>
  )
}
