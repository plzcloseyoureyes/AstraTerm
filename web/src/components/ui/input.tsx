import * as React from 'react'
import { cn } from '@/lib/utils'

export const inputBase = cn(
  'flex w-full min-w-0 rounded-md border border-input bg-background/60 px-2.5 text-base text-foreground shadow-xs',
  'placeholder:text-muted-foreground/70 selection:bg-primary/30',
  'transition-[border-color,box-shadow] duration-100 outline-none',
  'focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/25',
  'aria-invalid:border-destructive aria-invalid:ring-destructive/20',
  'disabled:cursor-not-allowed disabled:opacity-50 read-only:bg-muted/40',
  'dark:bg-input/25',
)

/** Overrides for the filled variant: tint instead of an outline; the focus ring still marks the field. */
const filled = 'border-transparent bg-foreground/6 shadow-none hover:bg-foreground/8 focus-visible:bg-transparent focus-visible:border-ring dark:bg-foreground/6'

export interface InputProps extends React.ComponentProps<'input'> {
  /** Compact height (h-7) for toolbars and dense forms. */
  inputSize?: 'sm' | 'md'
  /** 'filled': a soft tinted field without an outline (search boxes on the chrome surface). */
  variant?: 'outline' | 'filled'
  /** Element rendered inside the field on the left (icon). */
  leading?: React.ReactNode
  /** Element rendered inside the field on the right (button, unit). */
  trailing?: React.ReactNode
}

export function Input({ className, type = 'text', inputSize = 'md', variant = 'outline', leading, trailing, ...props }: InputProps) {
  const sizing = cn(inputSize === 'sm' ? 'h-7 text-sm' : 'h-8', variant === 'filled' && filled)
  if (!leading && !trailing) {
    return (
      <input
        type={type}
        data-slot="input"
        className={cn(
          inputBase,
          sizing,
          'file:mr-2 file:h-full file:border-0 file:bg-transparent file:text-sm file:font-medium',
          className,
        )}
        {...props}
      />
    )
  }
  return (
    <div className={cn('relative flex w-full items-center', className)}>
      {leading && (
        <span className="pointer-events-none absolute left-2.5 flex items-center text-muted-foreground [&_svg]:size-3.5">
          {leading}
        </span>
      )}
      <input
        type={type}
        data-slot="input"
        className={cn(inputBase, sizing, leading && 'pl-8', trailing && 'pr-8')}
        {...props}
      />
      {trailing && <span className="absolute right-1 flex items-center gap-0.5 text-muted-foreground">{trailing}</span>}
    </div>
  )
}
