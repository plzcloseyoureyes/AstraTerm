import { useEffect, useState } from 'react'
import type { IDockviewHeaderActionsProps } from 'dockview-react'
import { AppWindow, Ellipsis, Maximize2, Minimize2, PanelBottom, PanelRight, PanelTop, PictureInPicture2, X } from 'lucide-react'
import type { MenuItem } from '@/app/registry'
import { DynamicDropdown } from '@/components/menu-items'
import { IconButton } from '@/components/ui/icon-button'
import { useIsMobile } from '@/lib/hooks'
import { closeTabs, dockTab, float, popout, splitActive } from '@/stores/workspace'
import { TabsSheetButton } from './TabsSheet'

/** Right-side header actions of every dock group: split, maximize, more (phones: all tabs, more). */
export function GroupActions({ group, containerApi, activePanel }: IDockviewHeaderActionsProps) {
  const mobile = useIsMobile()
  const [maximized, setMaximized] = useState(() => group.api.isMaximized())
  const [location, setLocation] = useState(group.api.location.type)

  useEffect(() => {
    const d1 = containerApi.onDidMaximizedGroupChange(() => setMaximized(group.api.isMaximized()))
    const d2 = group.api.onDidLocationChange((e) => setLocation(e.location.type))
    return () => {
      d1.dispose()
      d2.dispose()
    }
  }, [containerApi, group])

  const panelId = activePanel?.id
  const inGrid = location === 'grid'

  const moreItems = (): MenuItem[] => {
    const ids = group.panels.map((p) => p.id)
    const items: MenuItem[] = [
      { label: 'Split down', icon: PanelBottom, run: () => splitActive('below', panelId), disabled: !panelId },
    ]
    if (location !== 'floating') items.push({ label: 'Float', icon: PictureInPicture2, run: () => float(panelId), disabled: !panelId })
    if (location !== 'popout') items.push({ label: 'Move to new window', icon: AppWindow, run: () => void popout(panelId), disabled: !panelId })
    if (!inGrid) items.push({ label: 'Dock into grid', icon: PanelTop, run: () => panelId && dockTab(panelId), disabled: !panelId })
    items.push({ type: 'separator' }, { label: `Close group (${ids.length})`, icon: X, run: () => void closeTabs(ids), disabled: !ids.length })
    return items
  }

  return (
    <div className="flex h-full items-center gap-0.5 px-1">
      {mobile && <TabsSheetButton />}
      {!mobile && (
        <IconButton
          icon={PanelRight}
          label="Split right"
          size="xs"
          disabled={!panelId}
          onClick={() => splitActive('right', panelId)}
        />
      )}
      {inGrid && !mobile && (
        <IconButton
          icon={maximized ? Minimize2 : Maximize2}
          label={maximized ? 'Restore' : 'Maximize'}
          size="xs"
          disabled={!panelId}
          onClick={() => (maximized ? group.api.exitMaximized() : group.api.maximize())}
        />
      )}
      <DynamicDropdown items={moreItems} align="end">
        <button
          type="button"
          aria-label="More group actions"
          className="flex size-6 items-center justify-center rounded-sm text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/60"
        >
          <Ellipsis className="size-3.5" />
        </button>
      </DynamicDropdown>
    </div>
  )
}
