import * as React from 'react'
import { ContextMenu as ContextMenuPrimitive } from 'radix-ui'
import { Check, ChevronRight } from 'lucide-react'
import { cn } from '@/lib/utils'
import { usePortalContainer } from './portal'
import { menuContentClass, menuItemClass, menuLabelClass, menuSeparatorClass, menuShortcutClass } from './dropdown-menu'

export const ContextMenu = ContextMenuPrimitive.Root
export const ContextMenuTrigger = ContextMenuPrimitive.Trigger
export const ContextMenuGroup = ContextMenuPrimitive.Group
export const ContextMenuSub = ContextMenuPrimitive.Sub

export function ContextMenuContent({ className, ...props }: React.ComponentProps<typeof ContextMenuPrimitive.Content>) {
  return (
    <ContextMenuPrimitive.Portal container={usePortalContainer()}>
      <ContextMenuPrimitive.Content
        data-slot="context-menu-content"
        className={cn(menuContentClass, 'max-h-(--radix-context-menu-content-available-height)', className)}
        {...props}
      />
    </ContextMenuPrimitive.Portal>
  )
}

export function ContextMenuItem({
  className,
  inset,
  variant,
  ...props
}: React.ComponentProps<typeof ContextMenuPrimitive.Item> & { inset?: boolean; variant?: 'default' | 'destructive' }) {
  return <ContextMenuPrimitive.Item data-variant={variant} className={cn(menuItemClass, inset && 'pl-7', className)} {...props} />
}

export function ContextMenuCheckboxItem({ className, children, ...props }: React.ComponentProps<typeof ContextMenuPrimitive.CheckboxItem>) {
  return (
    <ContextMenuPrimitive.CheckboxItem className={cn(menuItemClass, 'pl-7', className)} {...props}>
      <span className="absolute left-2 flex size-3.5 items-center justify-center">
        <ContextMenuPrimitive.ItemIndicator>
          <Check className="size-3.5 text-foreground!" />
        </ContextMenuPrimitive.ItemIndicator>
      </span>
      {children}
    </ContextMenuPrimitive.CheckboxItem>
  )
}

export function ContextMenuLabel({ className, ...props }: React.ComponentProps<typeof ContextMenuPrimitive.Label>) {
  return <ContextMenuPrimitive.Label className={cn(menuLabelClass, className)} {...props} />
}

export function ContextMenuSeparator({ className, ...props }: React.ComponentProps<typeof ContextMenuPrimitive.Separator>) {
  return <ContextMenuPrimitive.Separator className={cn(menuSeparatorClass, className)} {...props} />
}

export function ContextMenuShortcut({ className, ...props }: React.ComponentProps<'span'>) {
  return <span className={cn(menuShortcutClass, className)} {...props} />
}

export function ContextMenuSubTrigger({ className, children, ...props }: React.ComponentProps<typeof ContextMenuPrimitive.SubTrigger>) {
  return (
    <ContextMenuPrimitive.SubTrigger className={cn(menuItemClass, 'data-[state=open]:bg-accent', className)} {...props}>
      {children}
      <ChevronRight className="ml-auto size-3.5" />
    </ContextMenuPrimitive.SubTrigger>
  )
}

export function ContextMenuSubContent({ className, ...props }: React.ComponentProps<typeof ContextMenuPrimitive.SubContent>) {
  return (
    <ContextMenuPrimitive.Portal container={usePortalContainer()}>
      <ContextMenuPrimitive.SubContent className={cn(menuContentClass, className)} {...props} />
    </ContextMenuPrimitive.Portal>
  )
}
