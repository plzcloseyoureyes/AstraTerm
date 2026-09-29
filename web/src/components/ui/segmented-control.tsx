import * as React from 'react'
import { ToggleGroup } from 'radix-ui'
import type { IconType } from '@/app/registry'
import { cn } from '@/lib/utils'

export interface SegmentedOption<T extends string> {
  value: T
  label?: React.ReactNode
  icon?: IconType
  /** Accessible label when only an icon is shown. */
  title?: string
  disabled?: boolean
}

export interface SegmentedControlProps<T extends string> {
  value: T
  onValueChange: (v: T) => void
  options: SegmentedOption<T>[]
  size?: 'sm' | 'md'
  className?: string
  'aria-label'?: string
  fullWidth?: boolean
}

/** Single-choice segmented buttons (e.g. theme: Light / Dark / System). */
export function SegmentedControl<T extends string>({
  value,
  onValueChange,
  options,
  size = 'md',
  className,
  fullWidth,
  'aria-label': ariaLabel,
}: SegmentedControlProps<T>) {
  return (
    <ToggleGroup.Root
      type="single"
      value={value}
      onValueChange={(v) => v && onValueChange(v as T)}
      aria-label={ariaLabel}
      className={cn(
        'inline-flex items-center gap-0.5 rounded-md border bg-muted/60 p-0.5',
        size === 'sm' ? 'h-7' : 'h-8',
        fullWidth && 'flex w-full',
        className,
      )}
    >
      {options.map((o) => {
        const Icon = o.icon
        return (
          <ToggleGroup.Item
            key={o.value}
            value={o.value}
            disabled={o.disabled}
            title={o.title}
            aria-label={o.title ?? (typeof o.label === 'string' ? o.label : undefined)}
            className={cn(
              'inline-flex h-full items-center justify-center gap-1.5 rounded-sm px-2.5 font-medium text-muted-foreground outline-none transition-colors',
              size === 'sm' ? 'text-xs' : 'text-sm',
              'hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50 disabled:opacity-40',
              'data-[state=on]:bg-background data-[state=on]:text-foreground data-[state=on]:shadow-xs dark:data-[state=on]:bg-accent',
              fullWidth && 'flex-1',
            )}
          >
            {Icon && <Icon className="size-3.5" />}
            {o.label}
          </ToggleGroup.Item>
        )
      })}
    </ToggleGroup.Root>
  )
}
