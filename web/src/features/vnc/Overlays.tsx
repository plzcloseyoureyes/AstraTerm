/*
 * Always-mounted VNC dialogs (registerOverlay): incoming-connection listeners and the repeater connect dialog.
 */
import { ListenersDialog } from './ListenersDialog'
import { RepeaterDialog } from './RepeaterDialog'

export function VncOverlays({ locked }: { locked: boolean }) {
  return (
    <>
      <ListenersDialog locked={locked} />
      <RepeaterDialog locked={locked} />
    </>
  )
}
