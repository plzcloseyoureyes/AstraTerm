import * as React from 'react'
import { Label as LabelPrimitive } from 'radix-ui'
import { cn } from '@/lib/utils'

export function Label({ className, ...props }: React.ComponentProps<typeof LabelPrimitive.Root>) {
  return (
    <LabelPrimitive.Root
      data-slot="label"
      className={cn(
        'flex items-center gap-1.5 text-sm font-medium leading-none text-foreground/90 select-none',
        'peer-disabled:cursor-not-allowed peer-disabled:opacity-50 group-data-[disabled=true]:opacity-50',
        className,
      )}
      {...props}
    />
  )
}
