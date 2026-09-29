import { useEffect, useMemo, useRef, useState, type DragEvent, type KeyboardEvent } from 'react'
import { Columns2, Maximize2, Minimize2, PanelBottom, PanelRight, Plus } from 'lucide-react'
import { ribbonButtons, type MenuItem, type TabInfo } from '@/app/registry'
import { DynamicDropdown } from '@/components/menu-items'
import { IconButton } from '@/components/ui/icon-button'
import { cn } from '@/lib/utils'
import {
  beginSpaceDrag,
  closeSpace,
  endSpaceDrag,
  focusSpace,
  moveSpace,
  splitActive,
  toggleMaximize,
  useActiveSpaceId,
  useSpaces,
  useTabs,
  useWorkspaceStore,
  type SpaceInfo,
} from '@/stores/workspace'
import { TabChip } from './DockTab'
import { buildSpaceMenu } from './tabMenu'

/** Where a space dragged within the strip would land: before or after `id`. */
type DropMark = { id: string; after: boolean } | null

/** Hovering a tab this long while dragging another one switches to it (to drop into its layout). */
const HOVER_SWITCH_MS = 600

/**
 * Title-bar tabs: one per space (a top-level tab with its own split layout, like a Tabby tab). A tab shows its
 * focused pane (icon, title, status) and, when split, how many panes it has. Click to switch; drag to reorder, or
 * drop it onto the layout below to split it in there (hovering another tab while dragging switches to it);
 * middle-click or × closes the whole tab; ←/→ Home/End move the keyboard focus, Enter/Space open, Delete closes.
 */
export function TabStrip({ className }: { className?: string }) {
  const spaces = useSpaces()
  const tabs = useTabs()
  const activeId = useActiveSpaceId()
  const maximized = useWorkspaceStore((s) => s.maximized)
  const byId = useMemo(() => new Map(tabs.map((t) => [t.id, t])), [tabs])
  const scroller = useRef<HTMLDivElement>(null)
  const dragging = useRef<string | null>(null)
  const hover = useRef<{ id: string; timer: number } | null>(null)
  const [mark, setMark] = useState<DropMark>(null)
  const clearHover = () => {
    if (hover.current) window.clearTimeout(hover.current.timer)
    hover.current = null
  }
  const visible = spaces.filter((s) => s.panes.length > 0)
  const active = visible.find((s) => s.id === activeId)
  const split = (active?.panes.length ?? 0) > 1

  // Keep the active tab in view.
  useEffect(() => {
    if (!activeId) return
    scroller.current?.querySelector<HTMLElement>(`[data-space-id="${CSS.escape(activeId)}"]`)?.scrollIntoView({ block: 'nearest', inline: 'nearest' })
  }, [activeId, spaces])

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const els = [...e.currentTarget.querySelectorAll<HTMLElement>('[role="tab"]')]
    const i = els.indexOf(document.activeElement as HTMLElement)
    if (i < 0) return
    const id = els[i].dataset.spaceId!
    let next = -1
    if (e.key === 'ArrowRight') next = (i + 1) % els.length
    else if (e.key === 'ArrowLeft') next = (i - 1 + els.length) % els.length
    else if (e.key === 'Home') next = 0
    else if (e.key === 'End') next = els.length - 1
    else if (e.key === 'Enter' || e.key === ' ') focusSpace(id)
    else if (e.key === 'Delete') {
      els[i + 1 < els.length ? i + 1 : i - 1]?.focus()
      void closeSpace(id)
    } else return
    e.preventDefault()
    if (next >= 0) els[next].focus()
  }

  const onDragOver = (id: string) => (e: DragEvent<HTMLDivElement>) => {
    if (!dragging.current) return
    e.preventDefault()
    if (id !== dragging.current && id !== activeId && hover.current?.id !== id) {
      clearHover()
      hover.current = { id, timer: window.setTimeout(() => focusSpace(id), HOVER_SWITCH_MS) }
    }
    const r = e.currentTarget.getBoundingClientRect()
    const after = e.clientX > r.left + r.width / 2
    if (mark?.id !== id || mark.after !== after) setMark({ id, after })
  }
  const onDrop = (id: string) => (e: DragEvent) => {
    const from = dragging.current
    const after = mark?.after ?? false
    dragging.current = null
    endSpaceDrag()
    clearHover()
    setMark(null)
    if (!from || from === id) return
    e.preventDefault()
    const order = spaces.map((s) => s.id)
    let to = order.indexOf(id) + (after ? 1 : 0)
    if (order.indexOf(from) < to) to--
    moveSpace(from, to)
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
          if (!e.currentTarget.contains(e.relatedTarget as Node | null)) {
            setMark(null)
            clearHover()
          }
        }}
      >
        {visible.map((s) => (
          <SpaceTab
            key={s.id}
            space={s}
            tab={byId.get(s.activeTabId ?? s.panes[0]?.[0] ?? '')}
            active={s.id === activeId}
            focusable={s.id === (activeId ?? visible[0]?.id)}
            mark={mark?.id === s.id ? (mark.after ? 'after' : 'before') : null}
            onDragStart={(e) => {
              dragging.current = s.id
              beginSpaceDrag(s.id)
              e.dataTransfer.effectAllowed = 'move'
              e.dataTransfer.setData('text/plain', s.id)
            }}
            onDragEnd={() => {
              dragging.current = null
              endSpaceDrag()
              clearHover()
              setMark(null)
            }}
            onDragOver={onDragOver(s.id)}
            onDrop={onDrop(s.id)}
          />
        ))}
      </div>
      <NewTabButton />
      <div className="flex-1" />
      {active && (
        <div className="flex shrink-0 items-center gap-0.5">
          <IconButton icon={PanelRight} label="Split right" size="xs" onClick={() => splitActive('right')} />
          <IconButton icon={PanelBottom} label="Split down" size="xs" onClick={() => splitActive('below')} />
          {(split || maximized) && (
            <IconButton icon={maximized ? Minimize2 : Maximize2} label={maximized ? 'Restore panes' : 'Maximize pane'} size="xs" onClick={() => toggleMaximize()} />
          )}
        </div>
      )}
    </div>
  )
}

function SpaceTab({
  space,
  tab,
  active,
  focusable,
  mark,
  ...drag
}: {
  space: SpaceInfo
  tab: TabInfo | undefined
  active: boolean
  focusable: boolean
  mark: 'before' | 'after' | null
  onDragStart: (e: DragEvent) => void
  onDragEnd: () => void
  onDragOver: (e: DragEvent<HTMLDivElement>) => void
  onDrop: (e: DragEvent) => void
}) {
  if (!tab) return null
  const panes = space.panes.length
  return (
    <TabChip
      tabId={tab.id}
      title={tab.title}
      kind={tab.kind}
      params={tab.params}
      variant="bar"
      active={active}
      badge={panes > 1 ? <SplitBadge count={panes} /> : undefined}
      menu={() => buildSpaceMenu(space.id, tab.id)}
      onClose={() => void closeSpace(space.id)}
      role="tab"
      aria-selected={active}
      data-space-id={space.id}
      // Roving focus: Tab reaches the strip once (on the active tab); arrows move between tabs.
      tabIndex={focusable ? 0 : -1}
      className={cn(mark === 'after' && 'shadow-[2px_0_0_var(--primary)]', mark === 'before' && 'shadow-[-2px_0_0_var(--primary)]')}
      draggable
      onDragStart={drag.onDragStart}
      onDragEnd={drag.onDragEnd}
      onDragOver={drag.onDragOver}
      onDrop={drag.onDrop}
      // On click, not mouse-down: starting a drag must not switch away from the layout it will be dropped on.
      onClick={() => focusSpace(space.id)}
    />
  )
}

/** "This tab is split into N panes." */
function SplitBadge({ count }: { count: number }) {
  return (
    <span className="flex shrink-0 items-center gap-0.5 text-2xs text-muted-foreground tabular-nums" title={`${count} panes`} aria-label={`${count} panes`}>
      <Columns2 className="size-3" aria-hidden />
      {count}
    </span>
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
