import { useEffect, useRef } from 'react'
import { DockviewReact, type DockviewReadyEvent, type DockviewTheme } from 'dockview-react'
import { cn } from '@/lib/utils'
import { generalSettings } from '@/stores/settings'
import { useCurrentUser } from '@/stores/auth'
import { attachSpace, HOST_COMPONENT, initWorkspace, POPOUT_URL, TAB_COMPONENT, useActiveSpaceId, useSpaces } from '@/stores/workspace'
import { DockTab } from './DockTab'
import { GroupActions } from './GroupActions'
import { PanelHost } from './PanelHost'
import { Watermark } from './Watermark'

const theme: DockviewTheme = {
  name: 'astraterm',
  className: 'dockview-theme-astraterm',
  gap: 0,
  dndOverlayMounting: 'absolute',
  dndPanelOverlay: 'content',
  dndTabIndicator: 'line',
  tabAnimation: 'smooth',
}

const components = { [HOST_COMPONENT]: PanelHost }
const tabComponents = { [TAB_COMPONENT]: DockTab }

/**
 * The workspace: one dockview per space (a top-level tab of the title bar, with its own split layout). Every space
 * stays mounted, so background sessions keep running; only the active one is shown. With no tabs, Home shows.
 */
export function Workspace() {
  const user = useCurrentUser()
  const spaces = useSpaces()
  const activeId = useActiveSpaceId()

  useEffect(() => initWorkspace({ userId: user?.id ?? 'anonymous', restore: generalSettings.get().restoreWorkspace }), [user?.id])

  return (
    <div className="relative h-full w-full" data-workspace>
      {spaces.length === 0 && <Watermark />}
      {spaces.map((s) => (
        <SpaceView key={s.id} id={s.id} hidden={s.id !== activeId} />
      ))}
    </div>
  )
}

function SpaceView({ id, hidden }: { id: string; hidden: boolean }) {
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
    dispose.current = attachSpace(id, e.api)
  }
  return (
    // Hidden spaces stay laid out (not display:none): their panes keep their sizes, so switching tabs never
    // re-measures or flashes. opacity-0 as well as invisible: some dockview parts set their own visibility.
    <div className={cn('absolute inset-0', hidden && 'invisible pointer-events-none opacity-0')} inert={hidden} aria-hidden={hidden || undefined} data-space={id}>
      <DockviewReact
        className="h-full w-full"
        theme={theme}
        components={components}
        tabComponents={tabComponents}
        defaultTabComponent={DockTab}
        rightHeaderActionsComponent={GroupActions}
        onReady={onReady}
        defaultRenderer="always"
        popoutUrl={POPOUT_URL}
        floatingGroupBounds="boundedWithinViewport"
        scrollbars="native"
      />
    </div>
  )
}
