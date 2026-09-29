import * as React from 'react'
import { Slot, Tooltip as TooltipPrimitive } from 'radix-ui'
import { cn } from '@/lib/utils'
import { usePortalContainer } from './portal'
import { Kbd } from './kbd'

export function TooltipProvider({ delayDuration = 400, skipDelayDuration = 200, ...props }: React.ComponentProps<typeof TooltipPrimitive.Provider>) {
  return <TooltipPrimitive.Provider delayDuration={delayDuration} skipDelayDuration={skipDelayDuration} {...props} />
}

export const TooltipRoot = TooltipPrimitive.Root
export const TooltipTrigger = TooltipPrimitive.Trigger

export function TooltipContent({ className, sideOffset = 5, children, ...props }: React.ComponentProps<typeof TooltipPrimitive.Content>) {
  return (
    <TooltipPrimitive.Portal container={usePortalContainer()}>
      <TooltipPrimitive.Content
        data-slot="tooltip-content"
        sideOffset={sideOffset}
        className={cn(
          'z-[60] flex max-w-xs items-center gap-2 rounded-md border bg-popover px-2 py-1 text-sm text-popover-foreground shadow-popover',
          'animate-in fade-in-0 zoom-in-95 data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=closed]:zoom-out-95',
          className,
        )}
        {...props}
      >
        {children}
      </TooltipPrimitive.Content>
    </TooltipPrimitive.Portal>
  )
}

export interface TooltipProps {
  content: React.ReactNode
  /** Keybinding shown on the right of the tooltip (already formatted or raw tinykeys string). */
  shortcut?: string
  side?: 'top' | 'right' | 'bottom' | 'left'
  align?: 'start' | 'center' | 'end'
  children: React.ReactElement
  disabled?: boolean
  delayDuration?: number
}

/**
 * Passes the tooltip trigger's props (events, aria, ref) to the child but drops the tooltip's `data-state`: wrapped
 * around another trigger (a dropdown / popover `…Trigger asChild`), it would overwrite that trigger's own
 * `data-state="open"` and break its open styling.
 */
function TooltipTriggerSlot({ 'data-state': _tooltipState, ...props }: React.ComponentProps<typeof Slot.Root> & { 'data-state'?: string }) {
  return <Slot.Root {...props} />
}

/** Simple tooltip wrapper: `<Tooltip content="Split right" shortcut="Ctrl+Alt+2"><Button/></Tooltip>`. */
export function Tooltip({ content, shortcut, side = 'bottom', align, children, disabled, delayDuration }: TooltipProps) {
  if (disabled || (!content && !shortcut)) return children
  return (
    <TooltipPrimitive.Root delayDuration={delayDuration}>
      <TooltipPrimitive.Trigger asChild>
        <TooltipTriggerSlot>{children}</TooltipTriggerSlot>
      </TooltipPrimitive.Trigger>
      <TooltipContent side={side} align={align}>
        <span>{content}</span>
        {shortcut && <Kbd keys={shortcut} className="-mr-0.5" />}
      </TooltipContent>
    </TooltipPrimitive.Root>
  )
}
