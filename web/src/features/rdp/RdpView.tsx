/*
 * The "rdp" tab: toolbar + remote desktop viewport. The engine (IronRDP or guacd) is chosen by the backend ticket;
 * see connection.ts for the lifecycle.
 */
import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { MonitorX } from 'lucide-react'
import type { TabProps } from '@/app/registry'
import { useRuntimeSession } from '@/api/sessions'
import { EmptyState } from '@/components/ui/empty-state'
import { cn } from '@/lib/utils'
import { useIsTabVisible } from '@/stores/workspace'
import { RdpConnection } from './connection'
import { RdpOverlays } from './Overlays'
import { rdpSettings } from './settings'
import { dropViewer, getViewer, initialViewerState, registerController, setViewer, useViewer } from './store'
import { RdpToolbar } from './Toolbar'
import type { RdpTabParams } from './types'

export default function RdpView({ tabId, params }: TabProps<RdpTabParams>) {
  const rootRef = useRef<HTMLDivElement>(null)
  const viewportRef = useRef<HTMLDivElement>(null)
  const stageRef = useRef<HTMLDivElement>(null)
  const sessionId = params?.sessionId
  const [ctrl, setCtrl] = useState<RdpConnection | null>(null)
  const viewer = useViewer(tabId)
  const session = useRuntimeSession(sessionId)
  const visible = useIsTabVisible(tabId)
  const { hiDpi } = rdpSettings.use()

  useLayoutEffect(() => {
    if (!getViewer(tabId)) setViewer(tabId, initialViewerState(params?.scaling ?? rdpSettings.get().scaling))
    return () => dropViewer(tabId)
    // The scaling param only seeds the state; later changes go through the controller.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tabId])

  useEffect(() => {
    const root = rootRef.current
    const viewport = viewportRef.current
    const stage = stageRef.current
    if (!sessionId || !root || !viewport || !stage) return
    const c = new RdpConnection(tabId, sessionId, { root, viewport, stage })
    const unregister = registerController(c)
    setCtrl(c)
    c.start()
    return () => {
      unregister()
      c.dispose()
      setCtrl(null)
    }
  }, [tabId, sessionId])

  // Hidden tabs keep a meaningless size: the controller ignores it and re-applies sizing when the tab shows again.
  useEffect(() => {
    ctrl?.setVisible(visible)
  }, [visible, ctrl])

  useEffect(() => {
    ctrl?.onResize()
  }, [hiDpi, ctrl])

  if (!sessionId) {
    return <EmptyState icon={MonitorX} title="No remote desktop session" description="This tab is not linked to a session." className="h-full" />
  }

  return (
    <div ref={rootRef} className="@container flex h-full min-h-0 flex-col bg-background" data-rdp-root>
      <RdpToolbar tabId={tabId} params={params} ctrl={ctrl} viewer={viewer} session={session} />
      <div
        ref={viewportRef}
        tabIndex={0}
        role="application"
        aria-label="Remote desktop"
        aria-roledescription="remote desktop"
        data-terminal
        data-rdp-viewport={tabId}
        className={cn(
          'relative min-h-0 flex-1 bg-remote-backdrop outline-none',
          viewer?.scaling === 'none' ? 'overflow-auto' : 'overflow-hidden',
        )}
      >
        <div ref={stageRef} className="flex min-h-full w-max min-w-full items-center justify-center" />
        <RdpOverlays tabId={tabId} params={params} ctrl={ctrl} viewer={viewer} session={session} />
      </div>
    </div>
  )
}
