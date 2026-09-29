import * as React from 'react'
import { Switch as SwitchPrimitive } from 'radix-ui'
import { cn } from '@/lib/utils'

export function Switch({ className, size = 'md', ...props }: React.ComponentProps<typeof SwitchPrimitive.Root> & { size?: 'sm' | 'md' }) {
  return (
    <SwitchPrimitive.Root
      data-slot="switch"
      className={cn(
        'peer inline-flex shrink-0 items-center rounded-full border border-transparent shadow-xs outline-none transition-colors',
        'focus-visible:ring-2 focus-visible:ring-ring/40 disabled:cursor-not-allowed disabled:opacity-50',
        'data-[state=checked]:bg-primary data-[state=unchecked]:bg-input',
        size === 'sm' ? 'h-4 w-7' : 'h-[1.15rem] w-8',
        className,
      )}
      {...props}
    >
      <SwitchPrimitive.Thumb
        className={cn(
          'pointer-events-none block rounded-full bg-background shadow-sm ring-0 transition-transform dark:data-[state=unchecked]:bg-foreground/80 dark:data-[state=checked]:bg-primary-foreground',
          size === 'sm'
            ? 'size-3 data-[state=checked]:translate-x-[calc(100%+2px)] data-[state=unchecked]:translate-x-0.5'
            : 'size-3.5 data-[state=checked]:translate-x-[calc(100%+1px)] data-[state=unchecked]:translate-x-0.5',
        )}
      />
    </SwitchPrimitive.Root>
  )
}

/** Settings-style row: label + description on the left, switch on the right. */
export function SwitchField({
  label,
  description,
  className,
  id,
  ...props
}: React.ComponentProps<typeof SwitchPrimitive.Root> & { label: React.ReactNode; description?: React.ReactNode }) {
  const autoId = React.useId()
  const sid = id ?? autoId
  return (
    <div className={cn('flex items-start justify-between gap-4', className)}>
      <div className="grid gap-0.5">
        <label htmlFor={sid} className="text-base leading-snug select-none">
          {label}
        </label>
        {description && (
          <p id={`${sid}-desc`} className="text-sm text-muted-foreground">
            {description}
          </p>
        )}
      </div>
      <Switch id={sid} className="mt-0.5" aria-describedby={description ? `${sid}-desc` : undefined} {...props} />
    </div>
  )
}
