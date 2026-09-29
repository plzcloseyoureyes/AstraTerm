import {
  AppWindow,
  Copy,
  Maximize2,
  PanelBottom,
  PanelRight,
  PanelTop,
  Pencil,
  PictureInPicture2,
  X,
} from 'lucide-react'
import { getContextMenuItems, joinSections, type MenuItem } from '@/app/registry'
import { prompt } from '@/components/ui/dialog-host'
import {
  closeOtherTabs,
  closeTabsToRight,
  dockTab,
  float,
  getDockviewApi,
  popout,
  renameTab,
  splitActive,
  toggleMaximize,
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

/** Built-in tab context menu + contributions registered for target "tab". */
export function buildTabMenu(tabId: string, onRename?: () => void): MenuItem[] {
  const api = getDockviewApi()
  const panel = api?.getPanel(tabId)
  if (!panel) return []
  const params = (panel.params ?? {}) as { kind?: string; params?: unknown }
  const title = panel.title ?? ''
  const location = panel.api.location.type
  const inGroup = panel.group.panels
  const index = inGroup.findIndex((p) => p.id === tabId)

  const primary: MenuItem[] = [
    { label: 'Rename…', icon: Pencil, run: () => (onRename ? onRename() : void promptRenameTab(tabId, title)) },
    { label: 'Duplicate', icon: Copy, command: 'workspace.duplicateTab', args: { tabId } },
  ]
  const layout: MenuItem[] = [
    { label: 'Split right', icon: PanelRight, run: () => splitActive('right', tabId) },
    { label: 'Split down', icon: PanelBottom, run: () => splitActive('below', tabId) },
    location === 'grid'
      ? { label: panel.api.isMaximized() ? 'Restore size' : 'Maximize', icon: Maximize2, run: () => toggleMaximize(tabId) }
      : { label: 'Dock into grid', icon: PanelTop, run: () => dockTab(tabId) },
    ...(location !== 'floating' ? [{ label: 'Float', icon: PictureInPicture2, run: () => float(tabId) } as MenuItem] : []),
    ...(location !== 'popout' ? [{ label: 'Move to new window', icon: AppWindow, run: () => void popout(tabId) } as MenuItem] : []),
  ]
  const contributed = getContextMenuItems('tab', {
    tabId,
    kind: params.kind ?? 'unknown',
    params: params.params,
    title,
  })
  const close: MenuItem[] = [
    { label: 'Close', icon: X, command: 'workspace.closeTab', args: { tabId } },
    { label: 'Close others', run: () => void closeOtherTabs(tabId), disabled: inGroup.length < 2 },
    { label: 'Close to the right', run: () => void closeTabsToRight(tabId), disabled: index < 0 || index >= inGroup.length - 1 },
  ]
  return joinSections([primary, contributed, layout, close])
}
