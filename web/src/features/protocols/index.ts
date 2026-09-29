/*
 * The 'protocols' Stage-2 feature module (SPEC §10.2): extra UI for the protocol backends in internal/proto/*.
 *
 * It registers:
 *   - a terminal plugin that opens the hex monitor for serial/raw sessions whose connection has `hexView` (PROTO-12;
 *     capture runs on the terminal bus while a monitor is open, see hexStore.ts);
 *   - an overlay hosting the serial line-status toolbar (PROTO-10/CC-18), the hex monitor drawer, the Docker
 *     "Containers" dialog (PROTO-30) and the IPMI power dialog (CC-2);
 *   - commands + terminal / session-tree context menus and a Tools-menu entry to open them.
 *
 * The connection editors for these protocols live in src/features/sessions/editors/* and register themselves through
 * defineProtocol.
 */
import { Binary, Container, Power } from 'lucide-react'
import { toast } from 'sonner'
import { registerCommand, registerContextMenu, registerMenu, registerOverlay, registerTerminalPlugin, type MenuItem } from '@/app/registry'
import type { Connection } from '@/api/types'
import { queryClient } from '@/api/queryClient'
import { queryKeys } from '@/api/queryKeys'
import { getActiveTerminal, getTerminalByTab } from '@/features/terminal/bus'
import { isHexProtocol, setupHexAutoOpen } from './hexPlugin'
import { ProtocolsOverlay } from './Overlay'
import { openContainersDialog, openIpmiPower, toggleHexMonitor } from './store'

registerTerminalPlugin({ id: 'protocols.hex', setup: setupHexAutoOpen })

registerOverlay({ id: 'protocols', component: ProtocolsOverlay })

function connectionName(id: string): string {
  const conns = queryClient.getQueryData<Connection[]>(queryKeys.connections)
  return conns?.find((c) => c.id === id)?.name ?? 'IPMI connection'
}

registerCommand<{ sessionId?: string } | undefined>({
  id: 'protocols.hexMonitor.toggle',
  title: 'Toggle Hex Monitor',
  category: 'Terminal',
  icon: Binary,
  keywords: ['hex', 'serial', 'raw', 'bytes', 'dump', 'monitor'],
  // No `when`: menus pass the session explicitly (the right-clicked tab need not be the active one).
  run: ({ args }) => {
    if (args?.sessionId) {
      toggleHexMonitor(args.sessionId)
      return
    }
    const t = getActiveTerminal()
    if (t && isHexProtocol(t.info().protocol)) toggleHexMonitor(t.sessionId)
    else toast.info('The hex monitor is available for serial and raw socket sessions')
  },
})

registerCommand<{ connectionId?: string; host?: string } | undefined>({
  id: 'protocols.docker.containers',
  title: 'Docker Containers…',
  category: 'Tools',
  icon: Container,
  keywords: ['docker', 'container', 'podman', 'exec', 'logs'],
  run: ({ args }) => openContainersDialog({ connectionId: args?.connectionId, host: args?.host }),
})

registerCommand<{ connectionId?: string } | undefined>({
  id: 'protocols.ipmi.power',
  title: 'BMC Power Control…',
  category: 'Tools',
  icon: Power,
  keywords: ['ipmi', 'bmc', 'power', 'reboot', 'reset', 'idrac', 'ilo'],
  run: ({ args }) => {
    const t = getActiveTerminal()
    const id = args?.connectionId ?? (t?.info().protocol === 'ipmi' ? t.session()?.connectionId : undefined)
    if (!id) {
      toast.info('Open the power control from a saved IPMI connection (session tree context menu) or its console tab')
      return
    }
    openIpmiPower(id, connectionName(id))
  },
})

registerMenu({
  menu: 'tools',
  order: 60,
  items: (): MenuItem[] => [{ label: 'Docker containers…', icon: Container, command: 'protocols.docker.containers' }],
})

// Terminal context menu: hex monitor for serial/raw sessions, power control for IPMI consoles.
registerContextMenu({
  target: 'terminal',
  group: 'session',
  order: 40,
  items: (ctx): MenuItem[] => {
    if (!ctx.sessionId) return []
    const protocol = getTerminalByTab(ctx.tabId)?.info().protocol ?? ctx.session?.protocol
    if (isHexProtocol(protocol)) {
      return [{ label: 'Hex monitor', icon: Binary, command: 'protocols.hexMonitor.toggle', args: { sessionId: ctx.sessionId } }]
    }
    const connectionId = ctx.session?.connectionId ?? getTerminalByTab(ctx.tabId)?.session()?.connectionId
    if (protocol === 'ipmi' && connectionId) {
      return [{ label: 'BMC power control…', icon: Power, command: 'protocols.ipmi.power', args: { connectionId } }]
    }
    return []
  },
})

// Session tree: power control for IPMI connections, Docker containers on SSH hosts.
registerContextMenu({
  target: 'session-node',
  order: 170,
  items: (ctx): MenuItem[] => {
    const c = ctx.connection
    if (!c || (ctx.selection && ctx.selection.length > 1)) return []
    if (c.protocol === 'ipmi') {
      return [{ label: 'BMC power control…', icon: Power, command: 'protocols.ipmi.power', args: { connectionId: c.id } }]
    }
    if (c.protocol === 'ssh') {
      return [{ label: 'Docker containers on this host…', icon: Container, command: 'protocols.docker.containers', args: { connectionId: c.id } }]
    }
    return []
  },
})
