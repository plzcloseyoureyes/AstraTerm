import { useEffect, useRef } from 'react'
import { DockviewReact, type DockviewReadyEvent, type DockviewTheme } from 'dockview-react'
import { generalSettings } from '@/stores/settings'
import { useCurrentUser } from '@/stores/auth'
import { attachDockview, HOST_COMPONENT, POPOUT_URL, TAB_COMPONENT } from '@/stores/workspace'
import { DockTab } from './DockTab'
import { GroupActions } from './GroupActions'
import { PanelHost } from './PanelHost'
import { Watermark } from './Watermark'

const theme: DockviewTheme = {
  name: 'termstead',
  className: 'dockview-theme-termstead',
  gap: 0,
  dndOverlayMounting: 'absolute',
  dndPanelOverlay: 'content',
  dndTabIndicator: 'line',
  tabAnimation: 'smooth',
}

const components = { [HOST_COMPONENT]: PanelHost }
const tabComponents = { [TAB_COMPONENT]: DockTab }

/** The dockview workspace: tabs, splits, floating groups and pop-out windows. */
export function Workspace() {
  const user = useCurrentUser()
  const dispose = useRef<(() => void) | null>(null)

  useEffect(
    () => () => {
      dispose.current?.()
      dispose.current = null
    },
    [],
  )

  const onReady = (e: DockviewReadyEvent) => {
    dispose.current?.()
    dispose.current = attachDockview(e.api, {
      userId: user?.id ?? 'anonymous',
      restore: generalSettings.get().restoreWorkspace,
    })
  }

  return (
    <div className="relative h-full w-full" data-workspace>
      <DockviewReact
        className="h-full w-full"
        theme={theme}
        components={components}
        tabComponents={tabComponents}
        defaultTabComponent={DockTab}
        rightHeaderActionsComponent={GroupActions}
        watermarkComponent={Watermark}
        onReady={onReady}
        defaultRenderer="always"
        popoutUrl={POPOUT_URL}
        floatingGroupBounds="boundedWithinViewport"
        scrollbars="native"
      />
    </div>
  )
}
