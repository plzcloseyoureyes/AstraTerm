import * as React from 'react'
import { RadioGroup as RadioGroupPrimitive } from 'radix-ui'
import { cn } from '@/lib/utils'

export function RadioGroup({ className, ...props }: React.ComponentProps<typeof RadioGroupPrimitive.Root>) {
  return <RadioGroupPrimitive.Root data-slot="radio-group" className={cn('grid gap-2', className)} {...props} />
}

export function RadioGroupItem({ className, ...props }: React.ComponentProps<typeof RadioGroupPrimitive.Item>) {
  return (
    <RadioGroupPrimitive.Item
      data-slot="radio-group-item"
      className={cn(
        'peer aspect-square size-4 shrink-0 rounded-full border border-input bg-background/60 shadow-xs outline-none dark:bg-input/30',
        'focus-visible:ring-2 focus-visible:ring-ring/40 disabled:cursor-not-allowed disabled:opacity-50',
        'data-[state=checked]:border-primary',
        className,
      )}
      {...props}
    >
      <RadioGroupPrimitive.Indicator className="flex items-center justify-center">
        <span className="size-2 rounded-full bg-primary" />
      </RadioGroupPrimitive.Indicator>
    </RadioGroupPrimitive.Item>
  )
}

/** Radio item with a label and optional description. */
export function RadioField({
  value,
  label,
  description,
  disabled,
}: {
  value: string
  label: React.ReactNode
  description?: React.ReactNode
  disabled?: boolean
}) {
  const id = React.useId()
  return (
    <div className="flex items-start gap-2">
      <RadioGroupItem id={id} value={value} disabled={disabled} className="mt-0.5" />
      <div className="grid gap-0.5">
        <label htmlFor={id} className="text-base leading-snug select-none peer-disabled:opacity-50">
          {label}
        </label>
        {description && <p className="text-sm text-muted-foreground">{description}</p>}
      </div>
    </div>
  )
}
