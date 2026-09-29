import * as React from 'react'
import { Slot } from 'radix-ui'
import { cva, type VariantProps } from 'class-variance-authority'
import { useDelayedFlag } from '@/lib/useDelayedFlag'
import { cn } from '@/lib/utils'
import { Spinner } from './spinner'

export const buttonVariants = cva(
  [
    'inline-flex shrink-0 items-center justify-center gap-1.5 whitespace-nowrap rounded-md font-medium select-none',
    'transition-[color,background-color,border-color,box-shadow,opacity] duration-100',
    'outline-none focus-visible:ring-2 focus-visible:ring-ring/60 focus-visible:ring-offset-1 focus-visible:ring-offset-background',
    'disabled:pointer-events-none disabled:opacity-45 aria-disabled:pointer-events-none aria-disabled:opacity-45',
    "[&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
  ],
  {
    variants: {
      variant: {
        default: 'bg-primary text-primary-foreground shadow-xs hover:bg-primary/88 active:bg-primary/80',
        secondary: 'bg-secondary text-secondary-foreground border border-border/60 hover:bg-accent active:bg-accent/80',
        ghost: 'text-foreground/85 hover:bg-accent hover:text-accent-foreground active:bg-accent/80',
        outline: 'border border-input bg-transparent hover:bg-accent hover:text-accent-foreground active:bg-accent/80',
        destructive: 'bg-destructive text-destructive-foreground shadow-xs hover:bg-destructive/88 active:bg-destructive/80',
        link: 'text-primary underline-offset-4 hover:underline h-auto! px-0!',
      },
      size: {
        xs: 'h-6 px-2 text-xs [&_svg:not([class*=size-])]:size-3.5',
        sm: 'h-7 px-2.5 text-sm',
        md: 'h-8 px-3 text-base',
        lg: 'h-9 px-4 text-md',
        icon: 'size-8',
        'icon-sm': 'size-7',
        'icon-xs': 'size-6 rounded-sm [&_svg:not([class*=size-])]:size-3.5',
      },
    },
    defaultVariants: { variant: 'default', size: 'md' },
  },
)

export interface ButtonProps extends React.ComponentProps<'button'>, VariantProps<typeof buttonVariants> {
  asChild?: boolean
  /** Blocks the button while true; after 300 ms it shows a spinner and looks disabled (then ≥ 600 ms; docs/UX.md). A
   *  fast operation changes nothing on screen: no spinner, no dimming. */
  loading?: boolean
}

export function Button({ className, variant, size, asChild = false, loading, disabled, children, onClick, ...props }: ButtonProps) {
  const Comp = asChild ? Slot.Root : 'button'
  // The busy look (spinner + disabled style) follows the delayed flag; until then the button only ignores clicks.
  const busyShown = useDelayedFlag(!!loading)
  const blocked = !!loading && !busyShown
  return (
    <Comp
      data-slot="button"
      className={cn(buttonVariants({ variant, size }), className)}
      disabled={asChild ? undefined : disabled || busyShown}
      aria-busy={loading || undefined}
      onClick={
        blocked
          ? (e: React.MouseEvent<HTMLButtonElement>) => {
              e.preventDefault() // also keeps a submit button from submitting its form again
            }
          : onClick
      }
      {...(asChild ? {} : { type: props.type ?? 'button' })}
      {...props}
    >
      {loading !== undefined && !asChild ? (
        <>
          {/* Stays mounted while the prop is given; shown exactly while the busy look is. */}
          <Spinner active={busyShown} immediate className="size-3.5" />
          {children}
        </>
      ) : (
        children
      )}
    </Comp>
  )
}
