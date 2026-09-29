/*
 * Context menu of the Sessions panel: like the shell's DynamicContextMenu, plus focus coordination so an action that
 * focuses something itself (inline rename) runs after the menu restored focus instead of being undone by it.
 */
import { useState, type ReactNode } from 'react'
import { ContextMenu as CM } from 'radix-ui'
import type { MenuItem } from '@/app/registry'
import { MenuItems } from '@/components/menu-items'
import { menuContentClass } from '@/components/ui/dropdown-menu'
import { cn } from '@/lib/utils'
import { runAfterSessionMenuClose, setSessionMenuOpen } from './controller'

export function SessionContextMenu({ items, children }: { items: () => MenuItem[]; children: ReactNode }) {
  const [list, setList] = useState<MenuItem[]>([])
  return (
    <CM.Root
      onOpenChange={(open) => {
        if (open) {
          try {
            setList(items())
          } catch (err) {
            console.error('[sessions] context menu items failed', err)
            setList([])
          }
        }
        setSessionMenuOpen(open)
      }}
    >
      <CM.Trigger asChild>{children}</CM.Trigger>
      <CM.Portal>
        <CM.Content
          className={cn(menuContentClass, 'max-h-(--radix-context-menu-content-available-height)')}
          onCloseAutoFocus={(e) => {
            if (runAfterSessionMenuClose()) e.preventDefault()
          }}
        >
          {list.length ? (
            <MenuItems items={list} flavor="context" source="context-menu" />
          ) : (
            <div className="px-2 py-1.5 text-sm text-muted-foreground">No actions</div>
          )}
        </CM.Content>
      </CM.Portal>
    </CM.Root>
  )
}
