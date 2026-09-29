import {
  AppWindow,
  ArrowRightLeft,
  Copy,
  Maximize2,
  PanelBottom,
  PanelRight,
  PanelTop,
  Pencil,
  PictureInPicture2,
  SquareArrowOutUpRight,
  X,
} from 'lucide-react'
import { getContextMenuItems, joinSections, type MenuItem } from '@/app/registry'
import { prompt } from '@/components/ui/dialog-host'
import {
  closeOtherTabs,
  closeSpace,
  closeTab,
  closeTabsToRight,
  dockTab,
  float,
  getPanel,
  moveTab,
  popout,
  renameTab,
  spaceOf,
  splitActive,
  toggleMaximize,
  useWorkspaceStore,
} from '@/stores/workspace'

/** Ask for a new tab title (empty restores automatic titles). */
export async function promptRenameTab(tabId: string, current: string): Promise<void> {
  const v = await prompt({
    title: 'Rename tab',
    label: 'Title',
    defaultValue: current,
    description: 'Leave empty to restore the automatic title.',
    confirmLabel: 'Rename',
  })
  if (v !== null) renameTab(tabId, v)
}

/** Pane actions of a tab: split, maximize / dock, float, new window, move to another space. */
function paneItems(tabId: string): MenuItem[] {
  const panel = getPanel(tabId)
  if (!panel) return []
  const location = panel.api.location.type
  const { spaces, tabs } = useWorkspaceStore.getState()
  const own = spaceOf(tabId)
  const ownPanes = spaces.find((s) => s.id === own)?.panes.length ?? 0
  const titleOf = (id: string | null) => tabs.find((t) => t.id === id)?.title ?? 'Tab'
  const others = spaces.filter((s) => s.id !== own && s.panes.length > 0)
  const items: MenuItem[] = [
    { label: 'Split right', icon: PanelRight, run: () => splitActive('right', tabId) },
    { label: 'Split down', icon: PanelBottom, run: () => splitActive('below', tabId) },
    location === 'grid'
      ? { label: panel.api.isMaximized() ? 'Restore size' : 'Maximize pane', icon: Maximize2, run: () => toggleMaximize(tabId), disabled: ownPanes < 2 }
      : { label: 'Dock into the tab', icon: PanelTop, run: () => dockTab(tabId) },
  ]
  if (location !== 'floating') items.push({ label: 'Float', icon: PictureInPicture2, run: () => float(tabId) })
  if (location !== 'popout') items.push({ label: 'Move to new window', icon: AppWindow, run: () => void popout(tabId) })
  if (ownPanes > 1) items.push({ label: 'Move pane to a new tab', icon: SquareArrowOutUpRight, run: () => moveTab(tabId) })
  if (others.length) {
    items.push({
      type: 'submenu',
      label: 'Move pane into tab',
      icon: ArrowRightLeft,
      items: () => others.map((s): MenuItem => ({ label: titleOf(s.activeTabId ?? s.panes[0]?.[0]), run: () => moveTab(tabId, { spaceId: s.id }) })),
    })
  }
  return items
}

function contributedItems(tabId: string): MenuItem[] {
  const panel = getPanel(tabId)
  if (!panel) return []
  const params = (panel.params ?? {}) as { kind?: string; params?: unknown }
  return getContextMenuItems('tab', { tabId, kind: params.kind ?? 'unknown', params: params.params, title: panel.title ?? '' })
}

/** Menu of one tab (the header of a floating / popped-out pane, and every pane on phones). */
export function buildTabMenu(tabId: string, onRename?: () => void): MenuItem[] {
  const panel = getPanel(tabId)
  if (!panel) return []
  const title = panel.title ?? ''
  const primary: MenuItem[] = [
    { label: 'Rename…', icon: Pencil, run: () => (onRename ? onRename() : void promptRenameTab(tabId, title)) },
    { label: 'Duplicate as a new tab', icon: Copy, command: 'workspace.duplicateTab', args: { tabId } },
  ]
  const close: MenuItem[] = [{ label: 'Close', icon: X, command: 'workspace.closeTab', args: { tabId } }]
  return joinSections([primary, contributedItems(tabId), paneItems(tabId), close])
}

/** Menu of a title-bar tab (a space), acting on its focused pane `tabId` where it concerns a pane. */
export function buildSpaceMenu(spaceId: string, tabId: string): MenuItem[] {
  const panel = getPanel(tabId)
  if (!panel) return []
  const title = panel.title ?? ''
  const { spaces } = useWorkspaceStore.getState()
  const index = spaces.findIndex((s) => s.id === spaceId)
  const panes = spaces[index]?.panes.length ?? 0
  const primary: MenuItem[] = [
    { label: 'Rename…', icon: Pencil, run: () => void promptRenameTab(tabId, title) },
    { label: 'Duplicate as a new tab', icon: Copy, command: 'workspace.duplicateTab', args: { tabId } },
  ]
  const close: MenuItem[] = [
    ...(panes > 1 ? [{ label: 'Close pane', icon: X, run: () => void closeTab(tabId) } as MenuItem] : []),
    { label: panes > 1 ? `Close tab (${panes} panes)` : 'Close tab', icon: panes > 1 ? undefined : X, run: () => void closeSpace(spaceId) },
    { label: 'Close other tabs', run: () => void closeOtherTabs(tabId), disabled: spaces.length < 2 },
    { label: 'Close tabs to the right', run: () => void closeTabsToRight(tabId), disabled: index < 0 || index >= spaces.length - 1 },
  ]
  return joinSections([primary, contributedItems(tabId), paneItems(tabId), close])
}
