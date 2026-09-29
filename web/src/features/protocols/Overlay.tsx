/*
 * Always-mounted overlay for the protocols module (registerOverlay): the serial line-status toolbar for the active
 * serial session, the hex monitor drawer, the Docker containers dialog and the IPMI power dialog. Their open state
 * lives in a store so commands, menus and toolbars can drive them.
 */
import { useActiveTerminalInfo } from '@/features/terminal/bus'
import { ContainersDialog } from './ContainersDialog'
import { HexView } from './HexView'
import { IpmiPowerDialog } from './IpmiPowerDialog'
import { SerialToolbar } from './SerialToolbar'
import { useProtocolsUI } from './store'

export function ProtocolsOverlay({ locked }: { locked: boolean }) {
  const active = useActiveTerminalInfo()
  const hexSession = useProtocolsUI((s) => s.hexSession)
  if (locked) return null

  const showSerialBar = active?.protocol === 'serial' && active.state === 'connected'

  return (
    <>
      {(showSerialBar || hexSession) && (
        <div className="pointer-events-none fixed inset-x-0 bottom-7 z-40 flex flex-col items-center gap-2 px-2">
          {showSerialBar && active && <SerialToolbar key={active.sessionId} sessionId={active.sessionId} />}
          {hexSession && (
            <div className="w-full max-w-4xl">
              <HexView sessionId={hexSession} />
            </div>
          )}
        </div>
      )}
      <ContainersDialog />
      <IpmiPowerDialog />
    </>
  )
}
