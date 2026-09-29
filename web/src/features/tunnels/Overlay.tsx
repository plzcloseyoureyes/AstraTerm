/*
 * Always-mounted overlay (registerOverlay) rendering the tunnel dialogs from the dialog store (lazy chunks), plus the
 * listening-port watcher subscriptions (TUN-9).
 */
import { lazy, Suspense } from 'react'
import { useSessions } from '@/api/sessions'
import { useEventsTopic } from '@/lib/events'
import { tunnelsSettings } from './settings'
import { closeDetectPorts, closeSessionForwards, closeTunnelEditor, closeTunnelImport, useTunnelDialogs } from './store'

const TunnelEditorDialog = lazy(() => import('./TunnelEditorDialog'))
const DetectPortsDialog = lazy(() => import('./DetectPortsDialog'))
const ImportDialog = lazy(() => import('./ImportDialog'))
const SessionForwardsDialog = lazy(() => import('./SessionForwardsDialog'))

export function TunnelsOverlay() {
  const { editor, detect, importer, forwards } = useTunnelDialogs()
  return (
    <Suspense fallback={null}>
      {editor && <TunnelEditorDialog key={editor.key} request={editor} onClose={closeTunnelEditor} />}
      {detect && <DetectPortsDialog key={detect.key} request={detect} onClose={closeDetectPorts} />}
      {importer && <ImportDialog key={importer.key} onClose={closeTunnelImport} />}
      {forwards && <SessionForwardsDialog key={forwards.key} connectionId={forwards.connectionId} onClose={closeSessionForwards} />}
      <PortWatcher />
    </Suspense>
  )
}

/** Subscribes to "tunnel.ports" for every connected SSH session while the watcher setting is on. */
function PortWatcher() {
  const enabled = tunnelsSettings.useValue('watchPorts')
  const { data } = useSessions(enabled)
  if (!enabled) return null
  const ssh = (data ?? []).filter((s) => s.protocol === 'ssh' && s.kind === 'terminal' && s.state === 'connected')
  return (
    <>
      {ssh.map((s) => (
        <PortSubscription key={s.id} sessionId={s.id} />
      ))}
    </>
  )
}

function PortSubscription({ sessionId }: { sessionId: string }) {
  useEventsTopic('tunnel.ports', { sessionId })
  return null
}
