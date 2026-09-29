import { useEffect, useState } from 'react'
import { PanelLeftClose, Settings } from 'lucide-react'
import { sidebarPanels, type SidebarPanelDef } from '@/app/registry'
import { runCommand, useCommand } from '@/app/commands'
import { ErrorBoundary } from '@/components/error-boundary'
import { IconButton } from '@/components/ui/icon-button'
import { ResizeHandle } from '@/components/ui/resizable'
import { LazyBoundary } from '@/components/ui/spinner'
import { Tooltip } from '@/components/ui/tooltip'
import { useIsMobile } from '@/lib/hooks'
import { cn, storage } from '@/lib/utils'
import { appearanceSettings } from '@/stores/settings'
import { setDrawerOpen, useUIStore } from '@/stores/ui'

const PREFS_KEY = 'termstead:sidebar'
const MIN_W = 200
const MAX_W = 640
const DEFAULT_W = 280

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
  return (
    <nav aria-label="Sidebar" className="flex w-11 shrink-0 flex-col items-center gap-0.5 border-r bg-rail py-1.5">
      {panels.map((p) => {
        const Icon = p.icon
        const Badge = p.badge
        const selected = p.id === activeId && !collapsed
        return (
          <Tooltip key={p.id} content={p.title} side="right">
            <button
              type="button"
              onClick={() => onSelect(p.id)}
              aria-pressed={selected}
              aria-label={p.title}
              className={cn(
                'relative flex size-9 items-center justify-center rounded-md text-muted-foreground outline-none transition-colors',
                'hover:bg-sidebar-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60',
                selected && 'text-foreground',
              )}
            >
              {selected && <span className="absolute top-1.5 bottom-1.5 -left-1 w-0.5 rounded-full bg-primary" aria-hidden />}
              <Icon className="size-[18px]" strokeWidth={1.75} />
              {Badge && (
                <span className="absolute -top-0.5 -right-0.5">
                  <Badge />
                </span>
              )}
            </button>
          </Tooltip>
        )
      })}
      <div className="flex-1" />
      <Tooltip content="Settings" side="right">
        <button
          type="button"
          disabled={!settings.enabled}
          onClick={() => void runCommand('settings.open', undefined, { source: 'menu' })}
          aria-label="Settings"
          className="flex size-9 items-center justify-center rounded-md text-muted-foreground outline-none hover:bg-sidebar-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60 disabled:opacity-40"
        >
          <Settings className="size-[18px]" strokeWidth={1.75} />
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
          <div className="flex min-w-0 flex-col border-r border-sidebar-border" style={{ width }}>
            {header}
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
            className="-ml-px"
          />
        </>
      )}
    </aside>
  )
}
