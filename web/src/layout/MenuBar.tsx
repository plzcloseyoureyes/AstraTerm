import { Menubar as MB } from 'radix-ui'
import { useEffect, useRef, useState } from 'react'
import { ChevronDown, Lock, LogOut, Menu, Search, Settings, UserRound, Zap } from 'lucide-react'
import { getMenuItems, menus, type MenuId, type MenuItem } from '@/app/registry'
import { getKeybindings, runCommand } from '@/app/commands'
import { BrandMark } from '@/components/brand-mark'
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
  keepMovedFocus,
  menuContentClass,
} from '@/components/ui/dropdown-menu'
import { Kbd } from '@/components/ui/kbd'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { Tooltip } from '@/components/ui/tooltip'
import { TITLE_BAR } from '@/lib/desktop'
import { useIsMobile } from '@/lib/hooks'
import { formatKeybinding } from '@/lib/keys'
import { cn } from '@/lib/utils'
import { useCurrentUser, useRunMode } from '@/stores/auth'
import { openPalette, setAppMenuOpen, setDrawerOpen, useUIStore } from '@/stores/ui'
import { QuickConnect } from './QuickConnect'
import { WindowControls } from './WindowControls'
import { TabStrip } from './workspace/TabStrip'

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
      <MB.Content className={cn(menuContentClass, 'min-w-[14rem]')} align="start" sideOffset={2} loop onCloseAutoFocus={keepMovedFocus}>
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
      <BrandMark className="size-4" />
      <span className="text-sm font-semibold tracking-tight">AstraTerm</span>
    </div>
  )
}

const barButton =
  'flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground outline-none hover:bg-foreground/5 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60 data-[state=open]:bg-foreground/5 data-[state=open]:text-foreground'

/**
 * Quick connect as a title-bar button, so the tabs keep the row: it opens the field (with suggestions) in a popover.
 * Ctrl+Shift+Q (quickConnect.focus) opens it too.
 */
function QuickConnectButton() {
  const request = useUIStore((s) => s.quickConnectFocus)
  const [open, setOpen] = useState(false)
  const seen = useRef(request) // only requests made while mounted
  useEffect(() => {
    if (request === seen.current) return
    seen.current = request
    setOpen(true)
  }, [request])
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <Tooltip content="Quick connect" shortcut={getKeybindings('quickConnect.focus')[0]}>
        <PopoverTrigger asChild>
          <button type="button" aria-label="Quick connect" className={barButton}>
            <Zap className="size-4" />
          </button>
        </PopoverTrigger>
      </Tooltip>
      <PopoverContent align="end" className="w-[min(28rem,calc(100vw-2rem))] p-1.5">
        <QuickConnect autoFocus inline onDone={() => setOpen(false)} />
      </PopoverContent>
    </Popover>
  )
}

/** Logo button: every menu as a submenu. F10 opens it (command app.menu); arrow keys navigate like a menu bar. */
function AppMenu() {
  const open = useUIStore((s) => s.appMenuOpen)
  const key = getKeybindings('app.menu')[0]
  return (
    <DropdownMenu open={open} onOpenChange={setAppMenuOpen}>
      <Tooltip content="Menu" shortcut={key}>
        <DropdownMenuTrigger asChild>
          <button
            type="button"
            aria-label="AstraTerm menu"
            className="group flex h-7 shrink-0 items-center gap-1 rounded-md pr-1 pl-1.5 outline-none hover:bg-foreground/5 focus-visible:ring-2 focus-visible:ring-ring/60 data-[state=open]:bg-foreground/5"
          >
            <BrandMark className="size-4.5" />
            <ChevronDown className="size-3 text-muted-foreground transition-transform group-data-[state=open]:rotate-180 group-hover:text-foreground" />
          </button>
        </DropdownMenuTrigger>
      </Tooltip>
      <DropdownMenuContent align="start" className="min-w-44">
        {/* Items are read on open (the content mounts only while open), so check states are fresh. */}
        <MenuItems items={MENUS.map((m): MenuItem => ({ type: 'submenu', label: m.label, items: () => getMenuItems(m.id) }))} flavor="dropdown" source="menu" />
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

function UserMenu({ compact = false }: { compact?: boolean }) {
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
          className="flex h-7 items-center gap-1.5 rounded-md px-1 text-sm text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60 data-[state=open]:bg-accent"
          aria-label="Account menu"
        >
          <span className={cn('flex items-center justify-center rounded-full bg-primary/20 font-semibold text-primary', compact ? 'size-6 text-xs' : 'size-4.5 text-2xs')}>
            {initial}
          </span>
          {!compact && <span className="hidden max-w-32 truncate sm:inline">{user.displayName || user.username}</span>}
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

/**
 * Top bar. With `tabs` (desktop): the app menu behind the logo, then the open tabs, quick connect, search and account
 * — the whole chrome in one row. Otherwise the classic menu bar (logo, menus, search, account).
 */
export function MenuBar({ tabs = false }: { tabs?: boolean }) {
  menus.useList() // re-render when menu contributions change
  const mobile = useIsMobile()
  const paletteKey = getKeybindings('palette.open')[0] ?? '$mod+k'

  if (tabs) {
    return (
      // Desktop app: the bar's empty space drags the window; the window buttons sit on the left (macOS) or right (Windows).
      <header
        className={cn('flex h-10 shrink-0 items-center gap-1 px-1.5', TITLE_BAR === 'macos' && 'pl-[76px]', TITLE_BAR === 'windows' && 'pr-0')}
        data-tauri-drag-region={TITLE_BAR ? 'deep' : undefined}
      >
        <AppMenu />
        <TabStrip className="min-w-0 flex-1" />
        <QuickConnectButton />
        <Tooltip content="Search commands, sessions and tabs" shortcut={paletteKey}>
          <button
            type="button"
            onClick={() => openPalette('all')}
            className={barButton}
            aria-label="Search commands, sessions and tabs"
          >
            <Search className="size-4" />
          </button>
        </Tooltip>
        <UserMenu compact />
        {TITLE_BAR === 'windows' && <WindowControls />}
      </header>
    )
  }

  return (
    <header className="flex h-8 shrink-0 items-center gap-1 px-1.5">
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
        <Kbd keys={paletteKey} />
      </button>
      <UserMenu />
    </header>
  )
}
