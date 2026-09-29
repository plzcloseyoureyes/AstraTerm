/*
 * Tools feature: the 'tools' tab (network diagnostics, scanners, SNMP, SSH audit, throughput),
 * the `tools.open {tool?, params?}` command (cross-module contract; `params` optionally prefills the tool's form) and
 * one palette command per tool, the "Tools" toolbar dropdown, and "Network tools" context submenus on
 * saved sessions and terminals (ping / trace / scan / audit the host).
 */
import { lazy } from 'react'
import { Wrench } from 'lucide-react'
import {
  registerCommand,
  registerContextMenu,
  registerRibbonButton,
  registerTabKind,
  ribbonButtons,
  type MenuItem,
  type RibbonButtonDef,
} from '@/app/registry'
import { getTool, TOOL_CATEGORIES, TOOLS } from './catalog'
import { cancelAllTools } from './jobs'
import { openTool } from './navigate'
import type { ToolsTabParams } from './types'

const ToolsView = lazy(() => import('./ToolsView'))

registerTabKind<ToolsTabParams>({
  kind: 'tools',
  singleton: true,
  title: (p) => {
    const t = getTool(p?.tool)
    return t ? `Tools · ${t.label}` : 'Tools'
  },
  icon: Wrench,
  iconFor: (p) => getTool(p?.tool)?.icon,
  component: ToolsView,
  // Closing the Tools tab stops every running tool job (scans, mtr, pings) instead of leaving them orphaned.
  onClose: () => cancelAllTools(),
})

// Cross-module contract command: tools.open {tool?, params?}.
registerCommand<{ tool?: string; params?: Record<string, unknown> } | string | undefined>({
  id: 'tools.open',
  title: 'Tools',
  category: 'Tools',
  icon: Wrench,
  keywords: ['network', 'ping', 'scan', 'dns'],
  run: ({ args }) => {
    if (typeof args === 'string') return openTool(getTool(args) ? args : undefined)
    const tool = args && typeof args === 'object' && getTool(args.tool) ? args.tool : undefined
    const params = args && typeof args === 'object' && args.params && typeof args.params === 'object' ? args.params : undefined
    openTool(tool, params)
  },
})

// One palette command per tool so every tool is directly searchable.
for (const t of TOOLS) {
  registerCommand({
    id: `tools.${t.id}`,
    title: t.label,
    category: 'Tools',
    icon: t.icon,
    keywords: ['tool', ...(t.keywords ?? [])],
    run: () => openTool(t.id),
  })
}

// ---------------------------------------------------------------------------------------------------------------------
// Ribbon: replace the shell's placeholder "Tools" button with a dropdown listing every tool by category.
// ---------------------------------------------------------------------------------------------------------------------

function ribbonToolsMenu(): MenuItem[] {
  const items: MenuItem[] = [{ label: 'Open Tools', icon: Wrench, command: 'tools.open' }]
  for (const cat of TOOL_CATEGORIES) {
    const tools = TOOLS.filter((t) => t.category === cat)
    if (tools.length === 0) continue
    items.push({ type: 'separator' }, { type: 'label', label: cat })
    for (const t of tools) items.push({ label: t.label, icon: t.icon, command: `tools.${t.id}` })
  }
  return items
}

const toolsRibbon: RibbonButtonDef = {
  id: 'tools',
  label: 'Tools',
  icon: Wrench,
  order: 30,
  tooltip: 'Network tools',
  command: 'tools.open',
  menu: ribbonToolsMenu,
}

// The shell registers a simpler placeholder under the same id; keep ours registered if it gets replaced.
function ensureToolsRibbon(): void {
  if (ribbonButtons.get('tools') !== toolsRibbon) registerRibbonButton(toolsRibbon)
}
ribbonButtons.subscribe(ensureToolsRibbon)
ensureToolsRibbon()

// ---------------------------------------------------------------------------------------------------------------------
// Context menus: network tools for the host of a saved session or of a terminal.
// ---------------------------------------------------------------------------------------------------------------------

const NETWORK_PROTOCOLS = new Set(['ssh', 'sftp', 'telnet', 'rlogin', 'raw', 'mosh', 'ftp', 'vnc', 'rdp', 'winrm', 'ipmi', 'web'])

function hostTools(host: string, port: number | undefined, protocol: string | undefined): MenuItem[] {
  const items: MenuItem[] = [
    { label: 'Ping', icon: getTool('ping')!.icon, run: () => openTool('ping', { host }) },
    { label: 'Traceroute', icon: getTool('traceroute')!.icon, run: () => openTool('traceroute', { host }) },
    { label: 'mtr (continuous trace)', icon: getTool('traceroute')!.icon, run: () => openTool('traceroute', { host, mode: 'mtr' }) },
    { label: 'Scan ports…', icon: getTool('portscan')!.icon, run: () => openTool('portscan', { targets: host }) },
    { label: 'DNS lookup', icon: getTool('dns')!.icon, run: () => openTool('dns', { name: host }) },
  ]
  if (protocol === 'ssh' || protocol === 'sftp' || protocol === 'mosh') {
    items.push({ label: 'Audit SSH server', icon: getTool('sshaudit')!.icon, run: () => openTool('sshaudit', { host, port: port || 22 }) })
  }
  if (protocol === 'web' || protocol === 'winrm' || protocol === 'rdp' || protocol === 'ftp') {
    const tlsPort = protocol === 'web' ? port || 443 : protocol === 'winrm' ? 5986 : protocol === 'rdp' ? 3389 : 21
    items.push({ label: 'Inspect TLS certificate', icon: getTool('tlscert')!.icon, run: () => openTool('tlscert', { host, port: tlsPort, startTls: protocol === 'ftp' ? 'ftp' : undefined }) })
  }
  return [{ type: 'submenu', label: 'Network tools', icon: Wrench, items }]
}

registerContextMenu({
  id: 'tools.ctx.session',
  target: 'session-node',
  order: 180,
  items: (ctx) => {
    const c = ctx.connection
    if (!c || !c.host || !NETWORK_PROTOCOLS.has(c.protocol) || (ctx.selection?.length ?? 1) > 1) return []
    return hostTools(c.host, c.port, c.protocol)
  },
})

registerContextMenu({
  id: 'tools.ctx.terminal',
  target: 'terminal',
  order: 180,
  items: (ctx) => {
    const s = ctx.session
    if (!s?.host || !NETWORK_PROTOCOLS.has(s.protocol)) return []
    return hostTools(s.host, undefined, s.protocol)
  },
})
