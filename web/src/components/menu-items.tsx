/*
 * Renders registry MenuItem[] (src/app/registry.ts) into Radix dropdown / context / menubar menus. Items bound to a
 * command take their label, keybinding and enabled state from the command registry.
 */
import * as React from 'react'
import { ContextMenu as CM, DropdownMenu as DM, Menubar as MB } from 'radix-ui'
import { Check, ChevronRight } from 'lucide-react'
import { commands, type CommandSource, type MenuItem } from '@/app/registry'
import { getKeybindings, isCommandEnabled, runCommand } from '@/app/commands'
import { formatKeybinding } from '@/lib/keys'
import { cn } from '@/lib/utils'
import { keepMovedFocus, menuContentClass, menuItemClass, menuLabelClass, menuSeparatorClass, menuShortcutClass } from './ui/dropdown-menu'

type Flavor = 'dropdown' | 'context' | 'menubar'

const parts = {
  dropdown: DM,
  context: CM,
  menubar: MB,
} as const

interface ResolvedItem {
  label: string
  disabled: boolean
  shortcut?: string
  onSelect: () => void
}

function resolve(item: Extract<MenuItem, { label: string; type?: 'item' }>, source: CommandSource): ResolvedItem {
  const cmd = item.command ? commands.get(item.command) : undefined
  const label = item.label || cmd?.title || item.command || ''
  const enabled = item.command ? isCommandEnabled(item.command) : !!item.run
  const binding = item.shortcut ?? (item.command ? getKeybindings(item.command)[0] : undefined)
  return {
    label,
    disabled: !!item.disabled || !enabled,
    shortcut: binding ? formatKeybinding(binding) : undefined,
    onSelect: () => {
      if (item.command) void runCommand(item.command, item.args, { source })
      else item.run?.()
    },
  }
}

export function MenuItems({ items, flavor, source = 'menu' }: { items: MenuItem[]; flavor: Flavor; source?: CommandSource }) {
  const P = parts[flavor] as typeof DM
  return (
    <>
      {items.map((item, i) => {
        if (item.type === 'separator') return <P.Separator key={`sep-${i}`} className={menuSeparatorClass} />
        if (item.type === 'label')
          return (
            <P.Label key={`lbl-${i}`} className={menuLabelClass}>
              {item.label}
            </P.Label>
          )
        if (item.type === 'submenu') {
          const sub = typeof item.items === 'function' ? safe(item.items) : item.items
          const Icon = item.icon
          return (
            <P.Sub key={`sub-${i}-${item.label}`}>
              <P.SubTrigger disabled={item.disabled || sub.length === 0} className={cn(menuItemClass, 'data-[state=open]:bg-accent')}>
                {Icon ? <Icon /> : <span className="size-3.5" />}
                <span className="truncate">{item.label}</span>
                <ChevronRight className="ml-auto size-3.5" />
              </P.SubTrigger>
              <P.Portal>
                <P.SubContent className={menuContentClass} sideOffset={2} alignOffset={-4}>
                  <MenuItems items={sub} flavor={flavor} source={source} />
                </P.SubContent>
              </P.Portal>
            </P.Sub>
          )
        }
        const r = resolve(item, source)
        const Icon = item.icon ?? (item.command ? commands.get(item.command)?.icon : undefined)
        return (
          <P.Item
            key={item.id ?? `${item.command ?? 'item'}-${i}`}
            disabled={r.disabled}
            data-variant={item.danger ? 'destructive' : undefined}
            className={menuItemClass}
            onSelect={r.onSelect}
          >
            {item.checked !== undefined ? (
              <span className="flex size-3.5 items-center justify-center">{item.checked && <Check className="size-3.5 text-foreground!" />}</span>
            ) : Icon ? (
              <Icon />
            ) : (
              <span className="size-3.5" />
            )}
            <span className="truncate">{r.label}</span>
            {r.shortcut && <span className={menuShortcutClass}>{r.shortcut}</span>}
          </P.Item>
        )
      })}
    </>
  )
}

function safe(fn: () => MenuItem[]): MenuItem[] {
  try {
    return fn() ?? []
  } catch (err) {
    console.error('[menu] submenu items failed', err)
    return []
  }
}

/** Dropdown menu whose items are computed when it opens. */
export function DynamicDropdown({
  items,
  children,
  align = 'start',
  side = 'bottom',
  source = 'menu',
  contentClassName,
  onOpenChange,
}: {
  items: () => MenuItem[]
  children: React.ReactNode
  align?: 'start' | 'center' | 'end'
  side?: 'top' | 'right' | 'bottom' | 'left'
  source?: CommandSource
  contentClassName?: string
  onOpenChange?: (open: boolean) => void
}) {
  const [open, setOpen] = React.useState(false)
  const [list, setList] = React.useState<MenuItem[]>([])
  return (
    <DM.Root
      open={open}
      onOpenChange={(o) => {
        if (o) setList(safe(items))
        setOpen(o)
        onOpenChange?.(o)
      }}
    >
      <DM.Trigger asChild>{children}</DM.Trigger>
      <DM.Portal>
        <DM.Content align={align} side={side} sideOffset={4} onCloseAutoFocus={keepMovedFocus} className={cn(menuContentClass, 'max-h-(--radix-dropdown-menu-content-available-height)', contentClassName)}>
          {list.length ? (
            <MenuItems items={list} flavor="dropdown" source={source} />
          ) : (
            <div className="px-2 py-1.5 text-sm text-muted-foreground">Nothing here yet</div>
          )}
        </DM.Content>
      </DM.Portal>
    </DM.Root>
  )
}

/** Context menu whose items are computed on right-click. Renders nothing extra when there are no items. */
export function DynamicContextMenu({
  items,
  children,
  source = 'context-menu',
  disabled,
}: {
  items: () => MenuItem[]
  children: React.ReactNode
  source?: CommandSource
  disabled?: boolean
}) {
  const [list, setList] = React.useState<MenuItem[]>([])
  return (
    <CM.Root
      onOpenChange={(o) => {
        if (o) setList(safe(items))
      }}
    >
      <CM.Trigger asChild disabled={disabled}>
        {children}
      </CM.Trigger>
      <CM.Portal>
        <CM.Content onCloseAutoFocus={keepMovedFocus} className={cn(menuContentClass, 'max-h-(--radix-context-menu-content-available-height)')}>
          {list.length ? (
            <MenuItems items={list} flavor="context" source={source} />
          ) : (
            <div className="px-2 py-1.5 text-sm text-muted-foreground">No actions</div>
          )}
        </CM.Content>
      </CM.Portal>
    </CM.Root>
  )
}
