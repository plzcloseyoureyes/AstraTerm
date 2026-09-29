import * as React from 'react'
import { formatKeybinding } from '@/lib/keys'
import { cn } from '@/lib/utils'

/**
 * Keyboard shortcut display. Pass `keys` as a tinykeys binding ("$mod+Shift+p") to get platform formatting, or
 * children for literal content.
 */
export function Kbd({ keys, className, children, ...props }: React.ComponentProps<'kbd'> & { keys?: string }) {
  return (
    <kbd
      data-slot="kbd"
      className={cn(
        'inline-flex h-[1.125rem] min-w-[1.125rem] items-center justify-center gap-0.5 rounded-[4px] border border-border bg-muted px-1',
        'font-sans text-2xs font-medium tracking-wide text-muted-foreground shadow-[inset_0_-1px_0_var(--border)] select-none',
        className,
      )}
      {...props}
    >
      {keys ? formatKeybinding(keys) : children}
    </kbd>
  )
}
