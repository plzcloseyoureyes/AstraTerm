/*
 * The tool catalog: every tool's id, label, icon, category and (lazily loaded) panel. Drives the left tool list, the
 * command palette entries and the ribbon menu. Panels are code-split so the app shell only carries this metadata.
 */
import { lazy, type ComponentType, type LazyExoticComponent } from 'react'
import {
  Activity,
  Building2,
  EthernetPort,
  Gauge,
  Globe,
  ListTree,
  type LucideIcon,
  Power,
  Radar,
  Router,
  ScanSearch,
  ShieldCheck,
  SquareTerminal,
  Waypoints,
} from 'lucide-react'

export type ToolCategory = 'Diagnostics' | 'Discovery' | 'Security' | 'Host'

export interface ToolDef {
  id: string
  label: string
  icon: LucideIcon
  category: ToolCategory
  keywords?: string[]
  component: LazyExoticComponent<ComponentType>
  /** Admin-only in server mode (scanners, host details, throughput); shown as a hint in the UI. */
  serverAdminOnly?: boolean
}

const host = (name: 'InterfacesPanel' | 'ListeningPanel') => lazy(() => import('./panels/host').then((m) => ({ default: m[name] })))

export const TOOLS: ToolDef[] = [
  // Diagnostics
  { id: 'ping', label: 'Ping', icon: Activity, category: 'Diagnostics', keywords: ['icmp', 'latency', 'rtt', 'tcping'], component: lazy(() => import('./panels/ping')) },
  { id: 'traceroute', label: 'Traceroute / mtr', icon: Waypoints, category: 'Diagnostics', keywords: ['hops', 'route', 'mtr', 'tracert', 'path'], component: lazy(() => import('./panels/traceroute')) },
  { id: 'dns', label: 'DNS lookup', icon: Globe, category: 'Diagnostics', keywords: ['resolve', 'a', 'mx', 'txt', 'ptr', 'nslookup', 'dig', 'dot'], component: lazy(() => import('./panels/dns')) },
  { id: 'whois', label: 'Whois', icon: Building2, category: 'Diagnostics', keywords: ['domain', 'registrar', 'asn', 'ip'], component: lazy(() => import('./panels/whois')) },
  { id: 'httpcheck', label: 'HTTP check', icon: SquareTerminal, category: 'Diagnostics', keywords: ['httping', 'curl', 'status', 'timing', 'headers'], component: lazy(() => import('./panels/httpcheck')) },
  { id: 'throughput', label: 'Throughput (iperf3)', icon: Gauge, category: 'Diagnostics', keywords: ['iperf', 'bandwidth', 'speed', 'mbit'], component: lazy(() => import('./panels/throughput')), serverAdminOnly: true },
  { id: 'wol', label: 'Wake-on-LAN', icon: Power, category: 'Diagnostics', keywords: ['magic packet', 'wol', 'wake'], component: lazy(() => import('./panels/wol')) },
  // Discovery
  { id: 'portscan', label: 'Port scanner', icon: Radar, category: 'Discovery', keywords: ['ports', 'nmap', 'tcp', 'udp', 'banner'], component: lazy(() => import('./panels/portscan')), serverAdminOnly: true },
  { id: 'netscan', label: 'Network scanner', icon: ScanSearch, category: 'Discovery', keywords: ['sweep', 'cidr', 'hosts', 'arp', 'mac', 'discover', 'netbios', 'mdns'], component: lazy(() => import('./panels/netscan')), serverAdminOnly: true },
  { id: 'snmp', label: 'SNMP', icon: Router, category: 'Discovery', keywords: ['oid', 'walk', 'mib', 'get', 'bulkwalk'], component: lazy(() => import('./panels/snmp')) },
  // Security
  { id: 'tlscert', label: 'TLS certificate', icon: ShieldCheck, category: 'Security', keywords: ['ssl', 'cert', 'x509', 'expiry', 'sans', 'starttls'], component: lazy(() => import('./panels/tlscert')) },
  { id: 'sshaudit', label: 'SSH server audit', icon: ShieldCheck, category: 'Security', keywords: ['ssh-audit', 'kex', 'cipher', 'terrapin', 'algorithms', 'hardening'], component: lazy(() => import('./panels/sshaudit')) },
  // Host
  { id: 'interfaces', label: 'Interfaces', icon: EthernetPort, category: 'Host', keywords: ['nic', 'ip', 'mac', 'mtu', 'ifconfig'], component: host('InterfacesPanel'), serverAdminOnly: true },
  { id: 'listening', label: 'Listening ports', icon: ListTree, category: 'Host', keywords: ['netstat', 'ss', 'sockets', 'process', 'pid', 'listports', 'kill'], component: host('ListeningPanel'), serverAdminOnly: true },
]

export const TOOL_CATEGORIES: ToolCategory[] = ['Diagnostics', 'Discovery', 'Security', 'Host']

export const DEFAULT_TOOL = 'ping'

const byId = new Map(TOOLS.map((t) => [t.id, t]))

export function getTool(id: string | undefined): ToolDef | undefined {
  return id ? byId.get(id) : undefined
}
