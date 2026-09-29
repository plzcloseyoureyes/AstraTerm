import { useEffect, useState } from 'react'
import { PanelLeftClose, Settings } from 'lucide-react'
import { commands, ribbonButtons, sidebarPanels, type RibbonButtonDef, type SidebarPanelDef } from '@/app/registry'
import { runCommand, useCommand } from '@/app/commands'
import { ErrorBoundary } from '@/components/error-boundary'
import { DynamicContextMenu, DynamicDropdown } from '@/components/menu-items'
import { IconButton } from '@/components/ui/icon-button'
import { ResizeHandle } from '@/components/ui/resizable'
import { LazyBoundary } from '@/components/ui/spinner'
import { Tooltip } from '@/components/ui/tooltip'
import { useIsMobile } from '@/lib/hooks'
import { cn, storage } from '@/lib/utils'
import { appearanceSettings } from '@/stores/settings'
import { setDrawerOpen, useUIStore } from '@/stores/ui'

const PREFS_KEY = 'astraterm:sidebar'
const MIN_W = 180
const MAX_W = 640
const DEFAULT_W = 248

interface SidebarPrefs {
  width: number
  active: string | null
  collapsed: boolean
}

function loadPrefs(): SidebarPrefs {
  const p = storage.get<Partial<SidebarPrefs>>(PREFS_KEY, {})
  return {
    width: typeof p.width === 'number' ? Math.min(MAX_W, Math.max(MIN_W, p.width)) : DEFAULT_W,
    active: typeof p.active === 'string' ? p.active : null,
    collapsed: p.collapsed === true,
  }
}

// Shared so commands (view.toggleSidebar, sidebar.show) can drive the sidebar.
let prefs = loadPrefs()
const listeners = new Set<() => void>()
function setPrefs(patch: Partial<SidebarPrefs>) {
  prefs = { ...prefs, ...patch }
  storage.set(PREFS_KEY, prefs)
  for (const l of Array.from(listeners)) l()
}
function useSidebarPrefs(): SidebarPrefs {
  const [p, setP] = useState(prefs)
  useEffect(() => {
    const l = () => setP(prefs)
    listeners.add(l)
    return () => {
      listeners.delete(l)
    }
  }, [])
  return p
}

/** Show a sidebar panel (expanding the sidebar; opens the drawer on mobile). */
export function showSidebarPanel(id: string): void {
  if (!appearanceSettings.get().showSidebar) appearanceSettings.set({ showSidebar: true })
  setPrefs({ active: id, collapsed: false })
  if (window.matchMedia('(max-width: 767px)').matches) setDrawerOpen(true)
}

/** Toggle the panel area (keeps the icon rail). */
export function toggleSidebarCollapsed(): void {
  setPrefs({ collapsed: !prefs.collapsed })
}

function PanelBody({ panel }: { panel: SidebarPanelDef }) {
  const Component = panel.component
  return (
    <ErrorBoundary label={panel.title} resetKey={panel.id}>
      <LazyBoundary className="bg-sidebar">
        <Component />
      </LazyBoundary>
    </ErrorBoundary>
  )
}

/** Toolbar buttons the rail leaves out: the title bar's "+" (new session / saved sessions), the app menu (View, Split,
 *  Help) and the rail's own Settings button cover them. */
const RAIL_SKIP = new Set(['session', 'sessions', 'view', 'split', 'settings', 'help'])

const railButton = cn(
  'relative flex size-8 shrink-0 items-center justify-center rounded-md text-muted-foreground outline-none transition-colors',
  'hover:bg-sidebar-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60 disabled:opacity-40 data-[state=open]:bg-sidebar-accent',
)

/** A tool launcher (a toolbar button) in the rail: click runs it; its menu opens on right-click (or on click when the
 *  button has no command of its own). */
function RailTool({ def }: { def: RibbonButtonDef }) {
  commands.useList() // re-render when commands (un)register
  const cmd = useCommand(def.command)
  const Icon = def.icon
  const runnable = !!def.command && cmd.enabled
  const title = def.tooltip ?? cmd.command?.title ?? def.label
  const button = (
    <button
      type="button"
      aria-label={def.label}
      disabled={!runnable && !def.menu}
      onClick={runnable ? () => void runCommand(def.command!, def.args, { source: 'ribbon' }) : undefined}
      className={railButton}
    >
      <Icon className="size-4.5" strokeWidth={1.75} />
    </button>
  )
  if (def.menu && !runnable) {
    return (
      <Tooltip content={def.label} side="right">
        <DynamicDropdown items={def.menu} side="right" source="ribbon">
          {button}
        </DynamicDropdown>
      </Tooltip>
    )
  }
  const tip = (
    <Tooltip content={def.menu ? `${title} · right-click for more` : title} shortcut={cmd.keybindings[0]} side="right">
      {button}
    </Tooltip>
  )
  return def.menu ? (
    <DynamicContextMenu items={def.menu} source="ribbon">
      {tip}
    </DynamicContextMenu>
  ) : (
    tip
  )
}

function Rail({
  panels,
  activeId,
  collapsed,
  onSelect,
}: {
  panels: readonly SidebarPanelDef[]
  activeId: string | null
  collapsed: boolean
  onSelect: (id: string) => void
}) {
  const settings = useCommand('settings.open')
  const showRibbon = appearanceSettings.useValue('showRibbon')
  const tools = ribbonButtons.useList().filter((b) => !RAIL_SKIP.has(b.id))
  return (
    <nav aria-label="Sidebar" className="flex w-11 shrink-0 flex-col items-center gap-1 py-1">
      {panels.map((p) => {
        const Icon = p.icon
        const Badge = p.badge
        const selected = p.id === activeId && !collapsed
        return (
          <Tooltip key={p.id} content={selected ? `Hide ${p.title}` : p.title} side="right">
            <button
              type="button"
              onClick={() => onSelect(p.id)}
              aria-pressed={selected}
              aria-label={p.title}
              className={cn(
                'relative flex size-8 items-center justify-center rounded-md text-muted-foreground outline-none transition-colors',
                'hover:bg-sidebar-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60',
                selected && 'bg-sidebar-accent text-foreground',
              )}
            >
              {selected && <span className="absolute top-2 bottom-2 -left-1.5 w-[3px] rounded-full bg-primary" aria-hidden />}
              <Icon className="size-4.5" strokeWidth={1.75} />
              {Badge && (
                <span className="absolute -top-0.5 -right-0.5">
                  <Badge />
                </span>
              )}
            </button>
          </Tooltip>
        )
      })}
      {/* Tools (unless the optional toolbar row shows them). Scrolls on very short windows. */}
      {!showRibbon && tools.length > 0 && (
        <>
          <span className="h-2 shrink-0" aria-hidden />
          <div className="scrollbar-none flex min-h-0 flex-col items-center gap-0.5 overflow-y-auto">
            {tools.map((b) => (
              <RailTool key={b.id} def={b} />
            ))}
          </div>
        </>
      )}
      <div className="flex-1" />
      <Tooltip content="Settings" side="right">
        <button
          type="button"
          disabled={!settings.enabled}
          onClick={() => void runCommand('settings.open', undefined, { source: 'menu' })}
          aria-label="Settings"
          className={railButton}
        >
          <Settings className="size-4.5" strokeWidth={1.75} />
        </button>
      </Tooltip>
    </nav>
  )
}

/** Left sidebar: icon rail + resizable, collapsible panel (drawer on small screens). */
export function Sidebar() {
  const panels = sidebarPanels.useList()
  const p = useSidebarPrefs()
  const mobile = useIsMobile()
  const drawerOpen = useUIStore((s) => s.drawerOpen)
  const [width, setWidth] = useState(p.width)
  useEffect(() => setWidth(p.width), [p.width])

  if (!panels.length) return null
  const active = panels.find((x) => x.id === p.active) ?? panels[0]
  const collapsed = p.collapsed

  const onSelect = (id: string) => {
    if (mobile) {
      setPrefs({ active: id, collapsed: false })
      return
    }
    if (id === active.id && !collapsed) setPrefs({ collapsed: true })
    else setPrefs({ active: id, collapsed: false })
  }

  const header = (
    <div className="flex h-8 shrink-0 items-center justify-between gap-2 border-b pr-1 pl-3">
      <h2 className="truncate text-xs font-semibold tracking-wider text-muted-foreground uppercase">{active.title}</h2>
      <IconButton
        icon={PanelLeftClose}
        label={mobile ? 'Close' : 'Collapse sidebar'}
        size="xs"
        onClick={() => (mobile ? setDrawerOpen(false) : setPrefs({ collapsed: true }))}
      />
    </div>
  )

  if (mobile) {
    if (!drawerOpen) return null
    return (
      <div className="fixed inset-0 z-40 flex" role="dialog" aria-modal="true" aria-label="Sidebar">
        <div className="flex h-full w-[min(88vw,380px)] border-r bg-sidebar text-sidebar-foreground shadow-popover animate-in slide-in-from-left duration-150">
          <Rail panels={panels} activeId={active.id} collapsed={false} onSelect={onSelect} />
          <div className="flex min-w-0 flex-1 flex-col">
            {header}
            <div className="relative min-h-0 flex-1 overflow-hidden">
              <PanelBody panel={active} />
            </div>
          </div>
        </div>
        <button type="button" aria-label="Close sidebar" className="flex-1 bg-overlay" onClick={() => setDrawerOpen(false)} />
      </div>
    )
  }

  return (
    <aside className="flex h-full shrink-0 bg-sidebar text-sidebar-foreground" aria-label="Side panel">
      <Rail panels={panels} activeId={active.id} collapsed={collapsed} onSelect={onSelect} />
      {!collapsed && (
        <>
          {/* No title row on desktop: the rail marks the active panel (click it again to hide the panel). */}
          <div className="flex min-w-0 flex-col" style={{ width }} aria-label={active.title} role="region">
            <div className="relative min-h-0 flex-1 overflow-hidden">
              <PanelBody panel={active} />
            </div>
          </div>
          <ResizeHandle
            edge="right"
            size={width}
            min={MIN_W}
            max={MAX_W}
            label="Resize sidebar"
            onResize={setWidth}
            onResizeEnd={(w) => setPrefs({ width: w })}
            onToggle={() => setPrefs({ collapsed: true })}
            quiet
            className="mx-[3px]"
          />
        </>
      )}
    </aside>
  )
}
