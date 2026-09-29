/*
 * UI state for the protocols module: which session's hex monitor is open, the Docker "Containers" dialog and the
 * IPMI power dialog. Kept in a zustand store so commands, context menus and toolbars can all drive the overlays.
 */
import { create } from 'zustand'
import { setCapturing } from './hexStore'
import type { DockerScope } from './api'

interface ProtocolsUI {
  /** Session id whose hex monitor drawer is open (null = closed). */
  hexSession: string | null
  /** Docker containers dialog. */
  containers: { open: boolean; scope: DockerScope }
  /** IPMI power dialog for a saved connection (null = closed). */
  ipmiPower: { connectionId: string; name: string } | null
}

export const useProtocolsUI = create<ProtocolsUI>(() => ({
  hexSession: null,
  containers: { open: false, scope: {} },
  ipmiPower: null,
}))

/** Open the hex monitor for a session (capture starts now). */
export function openHexMonitor(sessionId: string): void {
  const cur = useProtocolsUI.getState().hexSession
  if (cur === sessionId) return
  if (cur) setCapturing(cur, false)
  setCapturing(sessionId, true)
  useProtocolsUI.setState({ hexSession: sessionId })
}

/** Open the hex monitor for a session; passing the session that is already shown closes it. */
export function toggleHexMonitor(sessionId: string | null): void {
  const cur = useProtocolsUI.getState().hexSession
  if (!sessionId || cur === sessionId) {
    closeHexMonitor()
    return
  }
  openHexMonitor(sessionId)
}

export function closeHexMonitor(): void {
  const cur = useProtocolsUI.getState().hexSession
  if (cur) setCapturing(cur, false)
  useProtocolsUI.setState({ hexSession: null })
}

export function openContainersDialog(scope: DockerScope = {}): void {
  useProtocolsUI.setState({ containers: { open: true, scope } })
}

export function closeContainersDialog(): void {
  useProtocolsUI.setState((s) => ({ containers: { ...s.containers, open: false } }))
}

export function openIpmiPower(connectionId: string, name: string): void {
  useProtocolsUI.setState({ ipmiPower: { connectionId, name } })
}

export function closeIpmiPower(): void {
  useProtocolsUI.setState({ ipmiPower: null })
}
