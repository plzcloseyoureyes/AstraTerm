import { Menubar as MB } from 'radix-ui'
import { Lock, LogOut, Menu, Search, Settings, UserRound } from 'lucide-react'
import { getMenuItems, menus, type MenuId } from '@/app/registry'
import { getKeybindings, runCommand } from '@/app/commands'
import { MenuItems } from '@/components/menu-items'
import { Badge } from '@/components/ui/badge'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuShortcut,
  DropdownMenuTrigger,
  menuContentClass,
} from '@/components/ui/dropdown-menu'
import { Kbd } from '@/components/ui/kbd'
import { useIsMobile } from '@/lib/hooks'
import { formatKeybinding } from '@/lib/keys'
import { cn } from '@/lib/utils'
import { useCurrentUser, useRunMode } from '@/stores/auth'
import { openPalette, setDrawerOpen } from '@/stores/ui'

const MENUS: { id: MenuId; label: string }[] = [
  { id: 'terminal', label: 'Terminal' },
  { id: 'sessions', label: 'Sessions' },
  { id: 'view', label: 'View' },
  { id: 'tools', label: 'Tools' },
  { id: 'settings', label: 'Settings' },
  { id: 'help', label: 'Help' },
]

function MenuContent({ id }: { id: MenuId }) {
  // Computed on open (Radix mounts content only while open), so toggles/check states are fresh.
  const items = getMenuItems(id)
  return (
    <MB.Portal>
      <MB.Content className={cn(menuContentClass, 'min-w-[14rem]')} align="start" sideOffset={2} loop>
        {items.length ? (
          <MenuItems items={items} flavor="menubar" source="menu" />
        ) : (
          <div className="px-2 py-1.5 text-sm text-muted-foreground">No items</div>
        )}
      </MB.Content>
    </MB.Portal>
  )
}

function Logo() {
  return (
    <div className="flex items-center gap-1.5 pr-2 pl-1 select-none" aria-hidden>
      <svg viewBox="0 0 64 64" className="size-4">
        <rect width="64" height="64" rx="14" fill="var(--primary)" />
        <path d="M16 22l12 10-12 10" stroke="var(--primary-foreground)" strokeWidth="6" fill="none" strokeLinecap="round" strokeLinejoin="round" />
        <path d="M32 44h16" stroke="var(--primary-foreground)" strokeWidth="6" strokeLinecap="round" />
      </svg>
      <span className="text-sm font-semibold tracking-tight">NexTerm</span>
    </div>
  )
}

function UserMenu() {
  const user = useCurrentUser()
  const mode = useRunMode()
  if (!user) return null
  const initial = (user.displayName || user.username || '?').trim().charAt(0).toUpperCase()
  const lockKey = getKeybindings('app.lock')[0]
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <button
          type="button"
          className="flex h-6 items-center gap-1.5 rounded-md px-1.5 text-sm text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60 data-[state=open]:bg-accent"
          aria-label="Account menu"
        >
          <span className="flex size-4.5 items-center justify-center rounded-full bg-primary/20 text-2xs font-semibold text-primary">{initial}</span>
          <span className="hidden max-w-32 truncate sm:inline">{user.displayName || user.username}</span>
        </button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="min-w-56">
        <DropdownMenuLabel className="flex items-center gap-2 py-2">
          <UserRound className="size-4" />
          <div className="grid min-w-0 flex-1">
            <span className="truncate text-sm font-medium text-foreground">{user.displayName || user.username}</span>
            <span className="truncate text-xs">@{user.username}</span>
          </div>
          <Badge variant={user.role === 'admin' ? 'default' : 'secondary'}>{user.role}</Badge>
        </DropdownMenuLabel>
        <DropdownMenuLabel className="pt-0 text-2xs">{mode === 'desktop' ? 'Desktop mode' : 'Server mode'}</DropdownMenuLabel>
        <DropdownMenuSeparator />
        <DropdownMenuItem onSelect={() => void runCommand('settings.open', undefined, { source: 'menu' })}>
          <Settings /> Settings
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => void runCommand('app.lock', undefined, { source: 'menu' })}>
          <Lock /> Lock screen
          {lockKey && <DropdownMenuShortcut>{formatKeybinding(lockKey)}</DropdownMenuShortcut>}
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem variant="destructive" onSelect={() => void runCommand('app.logout', undefined, { source: 'menu' })}>
          <LogOut /> Sign out
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

/** Top menu bar: logo, registry-driven menus, command palette launcher, account menu. */
export function MenuBar() {
  menus.useList() // re-render when menu contributions change
  const mobile = useIsMobile()
  return (
    <header className="flex h-8 shrink-0 items-center gap-1 border-b bg-toolbar px-1.5">
      {mobile && (
        <button
          type="button"
          aria-label="Open sidebar"
          onClick={() => setDrawerOpen(true)}
          className="flex size-7 items-center justify-center rounded-md text-muted-foreground hover:bg-accent hover:text-foreground"
        >
          <Menu className="size-4" />
        </button>
      )}
      <Logo />
      <MB.Root className="scrollbar-none flex min-w-0 items-center gap-0.5 overflow-x-auto" loop>
        {MENUS.map((m) => (
          <MB.Menu key={m.id}>
            <MB.Trigger className="flex h-6 items-center rounded-sm px-2 text-sm text-foreground/80 outline-none select-none hover:bg-accent hover:text-foreground focus-visible:bg-accent data-[state=open]:bg-accent data-[state=open]:text-foreground">
              {m.label}
            </MB.Trigger>
            <MenuContent id={m.id} />
          </MB.Menu>
        ))}
      </MB.Root>
      <div className="flex-1" />
      <button
        type="button"
        onClick={() => openPalette('all')}
        className="hidden h-6 w-60 items-center gap-2 rounded-md border bg-background/60 px-2 text-sm text-muted-foreground outline-none hover:border-ring/50 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60 md:flex dark:bg-input/20"
        aria-label="Search commands, sessions and tabs"
      >
        <Search className="size-3.5" />
        <span className="flex-1 text-left">Search…</span>
        <Kbd keys={getKeybindings('palette.open')[0] ?? '$mod+k'} />
      </button>
      <UserMenu />
    </header>
  )
}
