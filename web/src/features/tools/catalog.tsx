/*
 * The tool catalog: every tool's id, label, icon, category and (lazily loaded) panel. Drives the left tool list, the
 * command palette entries and the ribbon menu. Panels are code-split so the app shell only carries this metadata.
 */
import { lazy, type ComponentType, type LazyExoticComponent } from 'react'
import {
  Activity,
  Binary,
  Braces,
  Building2,
  Clock,
  EthernetPort,
  FileLock2,
  Fingerprint,
  Gauge,
  Globe,
  Hash,
  KeyRound,
  ListTree,
  type LucideIcon,
  Network,
  Power,
  Radar,
  Router,
  ScanSearch,
  ShieldCheck,
  SquareTerminal,
  Waypoints,
} from 'lucide-react'

export type ToolCategory = 'Diagnostics' | 'Discovery' | 'Security' | 'Host' | 'Utilities'

export interface ToolDef {
  id: string
  label: string
  icon: LucideIcon
  category: ToolCategory
  keywords?: string[]
  component: LazyExoticComponent<ComponentType>
  /** Admin-only in server mode (scanners, host details, throughput); shown as a hint in the UI. */
  serverAdminOnly?: boolean
  /** Runs entirely in the browser. */
  offline?: boolean
}

const utility = (name: string) => lazy(() => import('./utilities').then((m) => ({ default: m.UTILITY_PANELS[name] })))
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
  // Utilities (offline)
  { id: 'password', label: 'Password generator', icon: KeyRound, category: 'Utilities', keywords: ['passphrase', 'random', 'diceware'], component: utility('password'), offline: true },
  { id: 'subnet', label: 'Subnet calculator', icon: Network, category: 'Utilities', keywords: ['cidr', 'netmask', 'ipv4', 'ipv6', 'ip'], component: utility('subnet'), offline: true },
  { id: 'hash', label: 'Hash calculator', icon: Hash, category: 'Utilities', keywords: ['md5', 'sha256', 'sha512', 'blake3', 'checksum'], component: utility('hash'), offline: true },
  { id: 'encode', label: 'Encode / decode', icon: Binary, category: 'Utilities', keywords: ['base64', 'hex', 'url', 'encode', 'decode'], component: utility('encode'), offline: true },
  { id: 'jwt', label: 'JWT decoder', icon: FileLock2, category: 'Utilities', keywords: ['token', 'json web token', 'claims'], component: utility('jwt'), offline: true },
  { id: 'json', label: 'JSON formatter', icon: Braces, category: 'Utilities', keywords: ['pretty', 'minify', 'format', 'validate'], component: utility('json'), offline: true },
  { id: 'timestamp', label: 'Timestamp converter', icon: Clock, category: 'Utilities', keywords: ['unix', 'epoch', 'date', 'iso'], component: utility('timestamp'), offline: true },
  { id: 'chmod', label: 'chmod calculator', icon: FileLock2, category: 'Utilities', keywords: ['permissions', 'octal', 'rwx'], component: utility('chmod'), offline: true },
  { id: 'uuid', label: 'UUID generator', icon: Fingerprint, category: 'Utilities', keywords: ['guid', 'v4', 'v7', 'unique'], component: utility('uuid'), offline: true },
]

export const TOOL_CATEGORIES: ToolCategory[] = ['Diagnostics', 'Discovery', 'Security', 'Host', 'Utilities']

export const DEFAULT_TOOL = 'ping'

const byId = new Map(TOOLS.map((t) => [t.id, t]))

export function getTool(id: string | undefined): ToolDef | undefined {
  return id ? byId.get(id) : undefined
}
