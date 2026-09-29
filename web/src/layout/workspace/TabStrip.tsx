import { Fragment, useEffect, useMemo, useRef, useState, type DragEvent, type KeyboardEvent } from 'react'
import { LocalSelectionTransfer, PanelTransfer } from 'dockview-react'
import { Maximize2, Minimize2, PanelRight, Plus } from 'lucide-react'
import { ribbonButtons, type MenuItem } from '@/app/registry'
import { DynamicDropdown } from '@/components/menu-items'
import { IconButton } from '@/components/ui/icon-button'
import { cn } from '@/lib/utils'
import { closeTab, focusTab, getDockviewApi, splitActive, toggleMaximize, useActiveTabId, usePanes, useTabs, useWorkspaceStore } from '@/stores/workspace'
import { TabChip } from './DockTab'

const transfer = LocalSelectionTransfer.getInstance<PanelTransfer>()

/** Where a tab dragged within the strip would land: before or after `id`. */
type DropMark = { id: string; after: boolean } | null

/** The tab being dragged from the strip (dockview's own drags are left to dockview). */
let dragging: string | null = null

/**
 * Title-bar tabs: every tab of the grid and of floating groups, one pane after another (a thin rule between panes).
 * Click to focus, drag to reorder — or drag onto the workspace to split / move it into another pane, exactly like a
 * dockview tab (the drag carries dockview's own panel transfer, so its drop overlays do the rest).
 */
export function TabStrip({ className }: { className?: string }) {
  const panes = usePanes()
  const tabs = useTabs()
  const activeId = useActiveTabId()
  const maximized = useWorkspaceStore((s) => s.maximized)
  const byId = useMemo(() => new Map(tabs.map((t) => [t.id, t])), [tabs])
  const scroller = useRef<HTMLDivElement>(null)
  const [mark, setMark] = useState<DropMark>(null)
  const dv = getDockviewApi()
  const split = panes.length > 1

  // Keep the active tab in view.
  useEffect(() => {
    if (!activeId) return
    scroller.current?.querySelector<HTMLElement>(`[data-tab-id="${CSS.escape(activeId)}"]`)?.scrollIntoView({ block: 'nearest', inline: 'nearest' })
  }, [activeId, panes])

  const firstId = panes[0]?.[0]

  // Keyboard (WAI-ARIA tabs): ←/→ Home/End move focus, Enter/Space open, Delete closes.
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const els = [...e.currentTarget.querySelectorAll<HTMLElement>('[role="tab"]')]
    const i = els.indexOf(document.activeElement as HTMLElement)
    if (i < 0) return
    const id = els[i].dataset.tabId!
    let next = -1
    if (e.key === 'ArrowRight') next = (i + 1) % els.length
    else if (e.key === 'ArrowLeft') next = (i - 1 + els.length) % els.length
    else if (e.key === 'Home') next = 0
    else if (e.key === 'End') next = els.length - 1
    else if (e.key === 'Enter' || e.key === ' ') focusTab(id)
    else if (e.key === 'Delete') {
      els[i + 1 < els.length ? i + 1 : i - 1]?.focus()
      void closeTab(id)
    } else return
    e.preventDefault()
    if (next >= 0) els[next].focus()
  }

  const onDragStart = (id: string) => (e: DragEvent) => {
    const panel = dv?.getPanel(id)
    if (!dv || !panel) return
    dragging = id
    transfer.setData([new PanelTransfer(dv.id, panel.group.id, id)], PanelTransfer.prototype)
    e.dataTransfer.effectAllowed = 'move'
    e.dataTransfer.setData('text/plain', panel.title ?? id)
  }
  const onDragEnd = () => {
    dragging = null
    transfer.clearData(PanelTransfer.prototype)
    setMark(null)
  }
  const onDragOver = (id: string) => (e: DragEvent<HTMLDivElement>) => {
    if (!dragging) return
    e.preventDefault()
    const r = e.currentTarget.getBoundingClientRect()
    const after = e.clientX > r.left + r.width / 2
    if (mark?.id !== id || mark.after !== after) setMark({ id, after })
  }
  const onDrop = (id: string) => (e: DragEvent) => {
    const from = dragging
    const target = dv?.getPanel(id)
    const moving = from ? dv?.getPanel(from) : undefined
    const after = mark?.after ?? false
    onDragEnd()
    if (!target || !moving || from === id) return
    e.preventDefault()
    const group = target.group
    let index = group.panels.indexOf(target) + (after ? 1 : 0)
    // Within the same group, removing the tab first shifts the later ones left.
    const cur = group.panels.indexOf(moving)
    if (cur >= 0 && cur < index) index--
    moving.api.moveTo({ group, position: 'center', index })
  }

  return (
    <div className={cn('flex h-full min-w-0 items-center gap-1', className)}>
      <div
        ref={scroller}
        role="tablist"
        aria-label="Open tabs"
        className="scrollbar-none flex h-full min-w-0 items-center gap-0.5 overflow-x-auto"
        onKeyDown={onKeyDown}
        onWheel={(e) => {
          if (e.deltaY && !e.deltaX) e.currentTarget.scrollLeft += e.deltaY
        }}
        onDragLeave={(e) => {
          if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setMark(null)
        }}
      >
        {panes.map((ids, gi) => (
          <Fragment key={ids[0]}>
            {gi > 0 && <span className="mx-1 h-4 w-px shrink-0 bg-border" aria-hidden />}
            {ids.map((id) => {
              const t = byId.get(id)
              if (!t) return null
              const active = id === activeId
              // In a split, each pane's visible tab stays marked (dimmer than the focused one).
              const visible = split && !active && dv?.getPanel(id)?.group.activePanel?.id === id
              return (
                <TabChip
                  key={id}
                  tabId={id}
                  title={t.title}
                  kind={t.kind}
                  params={t.params}
                  variant="bar"
                  active={active}
                  role="tab"
                  aria-selected={active}
                  // Roving focus: Tab reaches the strip once (on the active tab); arrows move between tabs.
                  tabIndex={id === (activeId ?? firstId) ? 0 : -1}
                  className={cn(
                    visible && 'bg-foreground/5 text-foreground/85',
                    mark?.id === id && (mark.after ? 'shadow-[2px_0_0_var(--primary)]' : 'shadow-[-2px_0_0_var(--primary)]'),
                  )}
                  draggable
                  onDragStart={onDragStart(id)}
                  onDragEnd={onDragEnd}
                  onDragOver={onDragOver(id)}
                  onDrop={onDrop(id)}
                  onMouseDown={(e) => {
                    if (e.button === 0) focusTab(id)
                  }}
                />
              )
            })}
          </Fragment>
        ))}
      </div>
      <NewTabButton />
      <div className="flex-1" />
      {tabs.length > 0 && (
        <div className="flex shrink-0 items-center gap-0.5">
          <IconButton icon={PanelRight} label="Split right" size="xs" onClick={() => splitActive('right')} />
          {(split || maximized) && (
            <IconButton icon={maximized ? Minimize2 : Maximize2} label={maximized ? 'Restore panes' : 'Maximize pane'} size="xs" onClick={() => toggleMaximize()} />
          )}
        </div>
      )}
    </div>
  )
}

/** "+": the saved-sessions menu (new session, quick connect, favorites / recent). */
function NewTabButton() {
  const items = (): MenuItem[] => {
    const saved = ribbonButtons.get('sessions')?.menu?.() ?? []
    return saved.length ? saved : [{ label: 'New session…', icon: Plus, command: 'sessions.new' }]
  }
  return (
    <DynamicDropdown items={items} source="ribbon">
      <button
        type="button"
        aria-label="New tab"
        title="New tab"
        className="flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground outline-none hover:bg-foreground/5 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60 data-[state=open]:bg-foreground/5"
      >
        <Plus className="size-4" />
      </button>
    </DynamicDropdown>
  )
}
