import * as React from 'react'
import { Toolbar as ToolbarPrimitive } from 'radix-ui'
import { cn } from '@/lib/utils'

/** Horizontal tool strip with roving focus (arrow keys move between controls). */
export function Toolbar({ className, ...props }: React.ComponentProps<typeof ToolbarPrimitive.Root>) {
  return (
    <ToolbarPrimitive.Root
      data-slot="toolbar"
      className={cn('flex h-9 shrink-0 items-center gap-0.5 border-b bg-toolbar px-1.5', className)}
      {...props}
    />
  )
}

export function ToolbarSeparator({ className, ...props }: React.ComponentProps<typeof ToolbarPrimitive.Separator>) {
  return <ToolbarPrimitive.Separator className={cn('mx-1 h-4 w-px bg-border', className)} {...props} />
}

export const ToolbarButton = ToolbarPrimitive.Button
export const ToolbarToggleGroup = ToolbarPrimitive.ToggleGroup
export const ToolbarToggleItem = ToolbarPrimitive.ToggleItem
export const ToolbarLink = ToolbarPrimitive.Link

/** Flexible spacer pushing following items to the right. */
export function ToolbarSpacer() {
  return <div className="flex-1" />
}

export function ToolbarGroup({ className, ...props }: React.ComponentProps<'div'>) {
  return <div role="group" className={cn('flex items-center gap-0.5', className)} {...props} />
}
