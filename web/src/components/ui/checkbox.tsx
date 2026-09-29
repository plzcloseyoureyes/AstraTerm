import * as React from 'react'
import { Checkbox as CheckboxPrimitive } from 'radix-ui'
import { Check, Minus } from 'lucide-react'
import { cn } from '@/lib/utils'

export function Checkbox({ className, ...props }: React.ComponentProps<typeof CheckboxPrimitive.Root>) {
  return (
    <CheckboxPrimitive.Root
      data-slot="checkbox"
      className={cn(
        'peer size-4 shrink-0 rounded-[4px] border border-input bg-background/60 shadow-xs outline-none dark:bg-input/30',
        'transition-colors focus-visible:ring-2 focus-visible:ring-ring/40',
        'data-[state=checked]:border-primary data-[state=checked]:bg-primary data-[state=checked]:text-primary-foreground',
        'data-[state=indeterminate]:border-primary data-[state=indeterminate]:bg-primary data-[state=indeterminate]:text-primary-foreground',
        'disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive',
        className,
      )}
      {...props}
    >
      <CheckboxPrimitive.Indicator className="flex items-center justify-center text-current">
        {props.checked === 'indeterminate' ? <Minus className="size-3" strokeWidth={3} /> : <Check className="size-3" strokeWidth={3} />}
      </CheckboxPrimitive.Indicator>
    </CheckboxPrimitive.Root>
  )
}

/** Checkbox with an inline clickable label and optional description. */
export function CheckboxField({
  label,
  description,
  className,
  id,
  ...props
}: React.ComponentProps<typeof CheckboxPrimitive.Root> & { label: React.ReactNode; description?: React.ReactNode }) {
  const autoId = React.useId()
  const cid = id ?? autoId
  return (
    <div className={cn('flex items-start gap-2', className)}>
      <Checkbox id={cid} className="mt-0.5" aria-describedby={description ? `${cid}-desc` : undefined} {...props} />
      <div className="grid gap-0.5">
        <label htmlFor={cid} className="text-base leading-snug select-none peer-disabled:opacity-50">
          {label}
        </label>
        {description && (
          <p id={`${cid}-desc`} className="text-sm text-muted-foreground">
            {description}
          </p>
        )}
      </div>
    </div>
  )
}
