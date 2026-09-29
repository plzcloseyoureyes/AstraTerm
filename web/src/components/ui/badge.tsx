import * as React from 'react'
import { Slot } from 'radix-ui'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from '@/lib/utils'

export const badgeVariants = cva(
  "inline-flex w-fit shrink-0 items-center justify-center gap-1 overflow-hidden rounded-[4px] border px-1.5 py-px text-xs font-medium whitespace-nowrap [&>svg]:pointer-events-none [&>svg]:size-3",
  {
    variants: {
      variant: {
        default: 'border-transparent bg-primary/15 text-primary',
        secondary: 'border-transparent bg-secondary text-secondary-foreground',
        outline: 'border-border text-foreground/80',
        success: 'border-transparent bg-success/15 text-success',
        warning: 'border-transparent bg-warning/18 text-warning',
        destructive: 'border-transparent bg-destructive/15 text-destructive',
        info: 'border-transparent bg-info/15 text-info',
        solid: 'border-transparent bg-primary text-primary-foreground',
      },
    },
    defaultVariants: { variant: 'secondary' },
  },
)

export function Badge({
  className,
  variant,
  asChild = false,
  ...props
}: React.ComponentProps<'span'> & VariantProps<typeof badgeVariants> & { asChild?: boolean }) {
  const Comp = asChild ? Slot.Root : 'span'
  return <Comp data-slot="badge" className={cn(badgeVariants({ variant }), className)} {...props} />
}
