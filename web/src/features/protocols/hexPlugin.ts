/*
 * Terminal plugin (registerTerminalPlugin) for serial/raw terminals: when the session's connection has
 * `options.hexView`, the hex monitor (PROTO-12) opens automatically the first time the session is shown. Capturing
 * itself happens on the terminal bus (hexStore.ts) only while a monitor is open.
 */
import type { TerminalPluginContext } from '@/app/registry'
import type { Connection } from '@/api/types'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import { events } from '@/lib/events'
import { getTabParams } from '@/stores/workspace'
import type { TerminalTabParams } from '@/features/terminal/types'
import { forgetSession } from './hexStore'
import { closeHexMonitor, openHexMonitor, useProtocolsUI } from './store'

export function isHexProtocol(p: string | undefined | null): boolean {
  return p === 'serial' || p === 'raw'
}

const autoOpened = new Set<string>()

function wantsHexView(params: TerminalTabParams | undefined): boolean {
  if (!params) return false
  if (params.quick?.options && params.quick.options.hexView === true) return true
  if (!params.connectionId) return false
  const conns = queryClient.getQueryData<Connection[]>(queryKeys.connections)
  const conn = conns?.find((c) => c.id === params.connectionId) ?? queryClient.getQueryData<Connection>(queryKeys.connection(params.connectionId))
  return conn?.options?.hexView === true
}

export function setupHexAutoOpen(_term: unknown, ctx: TerminalPluginContext): () => void {
  const params = getTabParams<TerminalTabParams>(ctx.tabId)
  const protocol = params?.protocol ?? ctx.session()?.protocol
  if (isHexProtocol(protocol) && !autoOpened.has(ctx.sessionId) && wantsHexView(params)) {
    autoOpened.add(ctx.sessionId)
    openHexMonitor(ctx.sessionId)
  }
  return () => {}
}

// Forget captures of sessions that ended (and close their monitor).
events.on('session.closed', (ev) => {
  autoOpened.delete(ev.id)
  if (useProtocolsUI.getState().hexSession === ev.id) closeHexMonitor()
  forgetSession(ev.id)
})
